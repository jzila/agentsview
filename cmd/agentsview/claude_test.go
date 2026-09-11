package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/daemon"

	"go.kenn.io/agentsview/internal/db"
)

func writeTestClaudeConfig(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, ".claude.json")
	require.NoError(t, os.WriteFile(path, []byte(`{
		"oauthAccount": {
			"accountUuid": "sink-acct-1",
			"emailAddress": "sink@example.com",
			"organizationName": "Sink Org",
			"organizationType": "claude_team",
			"seatTier": "team_tier_1",
			"userRateLimitTier": "default_claude_max_5x",
			"organizationRateLimitTier": "default_raven"
		}
	}`), 0o600))
	return path
}

func TestRunClaudeStatuslineSink_StoresFiveHourAndSevenDay(t *testing.T) {
	dataDir := testDataDir(t)
	homeDir := t.TempDir()
	claudeConfigPath := writeTestClaudeConfig(t, homeDir)

	stdin := strings.NewReader(`{
		"rate_limits": {
			"five_hour": {"used_percentage": 33.3, "resets_at": 1789435448},
			"seven_day": {"used_percentage": 12, "resets_at": 1789999999}
		}
	}`)
	require.NoError(t, runClaudeStatuslineSink(
		ClaudeStatuslineSinkConfig{ClaudeConfig: claudeConfigPath}, stdin,
	))

	database, err := db.Open(sessionsDBPath(dataDir))
	require.NoError(t, err)
	defer database.Close()

	rows, err := database.RateLimitSnapshotHistory(
		context.Background(), db.RateLimitHistoryFilter{Vendor: "claude", AccountID: "sink-acct-1"},
	)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	byWindow := map[string]db.RateLimitSnapshot{}
	for _, r := range rows {
		byWindow[r.WindowKind] = r
	}
	require.Contains(t, byWindow, "session")
	require.Contains(t, byWindow, "weekly")
	assert.InDelta(t, 33.3, byWindow["session"].UsedPercent, 0.001)
	assert.Equal(t, "Sink Org", byWindow["session"].AccountLabel)
	require.NotNil(t, byWindow["session"].ResetsAt)
	assert.EqualValues(t, 1789435448, *byWindow["session"].ResetsAt)
}

// TestRunClaudeStatuslineSink_ExpandsTildeClaudeConfigPath covers the
// roborev Medium finding on kata 9rs0 (filed as kata qs2f): a
// "~/..."-prefixed --claude-config value (matching the documented
// [claude.accounts.NAME] examples) must resolve against the real home
// directory rather than failing os.ReadFile literally.
func TestRunClaudeStatuslineSink_ExpandsTildeClaudeConfigPath(t *testing.T) {
	dataDir := testDataDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeTestClaudeConfig(t, home)

	stdin := strings.NewReader(`{
		"rate_limits": {"five_hour": {"used_percentage": 20, "resets_at": 1789435448}}
	}`)
	require.NoError(t, runClaudeStatuslineSink(
		ClaudeStatuslineSinkConfig{ClaudeConfig: "~/.claude.json"}, stdin,
	))

	database, err := db.Open(sessionsDBPath(dataDir))
	require.NoError(t, err)
	defer database.Close()

	rows, err := database.RateLimitSnapshotHistory(
		context.Background(), db.RateLimitHistoryFilter{Vendor: "claude", AccountID: "sink-acct-1"},
	)
	require.NoError(t, err)
	require.Len(t, rows, 1)
}

func TestRunClaudeStatuslineSink_NoRateLimitsIsANoOp(t *testing.T) {
	dataDir := testDataDir(t)
	homeDir := t.TempDir()
	claudeConfigPath := writeTestClaudeConfig(t, homeDir)

	stdin := strings.NewReader(`{"model": {"id": "claude-sonnet"}}`)
	require.NoError(t, runClaudeStatuslineSink(
		ClaudeStatuslineSinkConfig{ClaudeConfig: claudeConfigPath}, stdin,
	))

	assertNoLocalSessionsDB(t, dataDir)
}

func TestRunClaudeStatuslineSink_NoIdentityIsANoOp(t *testing.T) {
	dataDir := testDataDir(t)
	homeDir := t.TempDir()
	claudeConfigPath := filepath.Join(homeDir, ".claude.json")
	require.NoError(t, os.WriteFile(claudeConfigPath, []byte(`{}`), 0o600))

	stdin := strings.NewReader(`{
		"rate_limits": {"five_hour": {"used_percentage": 10, "resets_at": 1}}
	}`)
	require.NoError(t, runClaudeStatuslineSink(
		ClaudeStatuslineSinkConfig{ClaudeConfig: claudeConfigPath}, stdin,
	))

	assertNoLocalSessionsDB(t, dataDir)
}

func TestRunClaudeStatuslineSink_MalformedJSONErrors(t *testing.T) {
	testDataDir(t)
	homeDir := t.TempDir()
	claudeConfigPath := writeTestClaudeConfig(t, homeDir)

	stdin := strings.NewReader(`not json`)
	err := runClaudeStatuslineSink(
		ClaudeStatuslineSinkConfig{ClaudeConfig: claudeConfigPath}, stdin,
	)
	assert.Error(t, err)
}

// startClaudeSinkTestDaemon starts an httptest server answering the kit
// daemon ping (so FindWritableDaemonRuntime's probe succeeds) plus the
// given ingest handler, and writes a live writable runtime record for it
// into dataDir (so IsLocalDaemonActive reports true), mirroring
// startEmbeddingsTestDaemon.
func startClaudeSinkTestDaemon(t *testing.T, dataDir string, ingest http.HandlerFunc) {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("GET /api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: "test",
	}))
	mux.HandleFunc("POST /api/v1/claude/rate-limit-snapshots", ingest)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	endpoint := serverEndpoint(t, srv)
	writeDaemonRuntimeForTest(t, dataDir, endpoint.Host, endpoint.Port, "test", false)
}

// TestRunClaudeStatuslineSink_ForwardsToRunningDaemon covers the roborev
// Medium finding on kata 9rs0 (filed as kata qs2f): when a writable
// daemon owns the archive, the sink must forward snapshots through the
// daemon's ingest endpoint (openWriteDB would otherwise reject the
// direct write), and it must never write sessions.db itself in that
// case.
func TestRunClaudeStatuslineSink_ForwardsToRunningDaemon(t *testing.T) {
	dataDir := testDataDir(t)
	homeDir := t.TempDir()
	claudeConfigPath := writeTestClaudeConfig(t, homeDir)

	var ingestCalled atomic.Bool
	var gotBody []byte
	startClaudeSinkTestDaemon(t, dataDir, func(w http.ResponseWriter, r *http.Request) {
		ingestCalled.Store(true)
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"inserted": 1}`))
	})

	stdin := strings.NewReader(`{
		"rate_limits": {"five_hour": {"used_percentage": 55, "resets_at": 1789435448}}
	}`)
	require.NoError(t, runClaudeStatuslineSink(
		ClaudeStatuslineSinkConfig{ClaudeConfig: claudeConfigPath}, stdin,
	))

	assert.True(t, ingestCalled.Load(), "the sink must forward to the daemon's ingest endpoint")
	assertNoLocalSessionsDB(t, dataDir)

	var payload struct {
		Snapshots []struct {
			AccountID   string  `json:"accountId"`
			WindowKind  string  `json:"windowKind"`
			UsedPercent float64 `json:"usedPercent"`
		} `json:"snapshots"`
	}
	require.NoError(t, json.Unmarshal(gotBody, &payload))
	require.Len(t, payload.Snapshots, 1)
	assert.Equal(t, "sink-acct-1", payload.Snapshots[0].AccountID)
	assert.Equal(t, "session", payload.Snapshots[0].WindowKind)
	assert.InDelta(t, 55, payload.Snapshots[0].UsedPercent, 0.001)
}

// TestRunClaudeStatuslineSink_ReportsDaemonRejection ensures a
// non-2xx response from the daemon's ingest endpoint surfaces as an
// error rather than being silently swallowed.
func TestRunClaudeStatuslineSink_ReportsDaemonRejection(t *testing.T) {
	dataDir := testDataDir(t)
	homeDir := t.TempDir()
	claudeConfigPath := writeTestClaudeConfig(t, homeDir)

	startClaudeSinkTestDaemon(t, dataDir, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error": "bad snapshot"}`))
	})

	stdin := strings.NewReader(`{
		"rate_limits": {"five_hour": {"used_percentage": 55, "resets_at": 1789435448}}
	}`)
	err := runClaudeStatuslineSink(
		ClaudeStatuslineSinkConfig{ClaudeConfig: claudeConfigPath}, stdin,
	)
	assert.Error(t, err)
}
