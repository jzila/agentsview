package server

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/poller"
)

func writeServerTestClaudeConfig(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, ".claude.json")
	require.NoError(t, os.WriteFile(path, []byte(`{
		"oauthAccount": {
			"accountUuid": "srv-acct-1",
			"emailAddress": "srv@example.com",
			"organizationName": "Srv Org",
			"organizationType": "claude_team",
			"seatTier": "team_tier_1",
			"userRateLimitTier": "default_claude_max_5x",
			"organizationRateLimitTier": "default_raven"
		}
	}`), 0o600))
	return path
}

func TestClaudeAccountsRoute_ListsConfiguredAccountsWithIdentityAndStatus(t *testing.T) {
	s := testServer(t, 0)
	dir := t.TempDir()
	claudeConfigPath := writeServerTestClaudeConfig(t, dir)

	s.cfg.Claude = config.ClaudeConfig{
		Accounts: map[string]config.ClaudeAccountConfig{
			"personal": {Credentials: "keychain", ClaudeConfig: claudeConfigPath},
		},
	}
	s.pollerStatus = func() []poller.Status {
		return []poller.Status{
			{Name: "claude-usage:personal", LastError: "401"},
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/claude/accounts", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var body ClaudeAccountsInfo
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Accounts, 1)
	acct := body.Accounts[0]
	assert.Equal(t, "personal", acct.Name)
	assert.Equal(t, "keychain", acct.CredentialsSource)
	assert.True(t, acct.IdentityAvailable)
	assert.Equal(t, "Srv Org", acct.Label)
	assert.Equal(t, "team_tier_1", acct.SeatTier)
	assert.Equal(t, "default_claude_max_5x", acct.UserRateLimitTier)
	assert.Equal(t, "401", acct.LastError)
}

// TestClaudeAccountsRoute_ExpandsTildeClaudeConfigPath covers the
// roborev Medium finding on kata 9rs0 (filed as kata qs2f): the
// documented `claude_config = "~/..."` examples must resolve against
// the real home directory in the Settings panel, not fail identity
// resolution.
func TestClaudeAccountsRoute_ExpandsTildeClaudeConfigPath(t *testing.T) {
	s := testServer(t, 0)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeServerTestClaudeConfig(t, home)

	s.cfg.Claude = config.ClaudeConfig{
		Accounts: map[string]config.ClaudeAccountConfig{
			"personal": {Credentials: "keychain", ClaudeConfig: "~/.claude.json"},
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/claude/accounts", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var body ClaudeAccountsInfo
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Accounts, 1)
	assert.True(t, body.Accounts[0].IdentityAvailable, "identity error: %s", body.Accounts[0].IdentityError)
	assert.Equal(t, "Srv Org", body.Accounts[0].Label)
}

func TestClaudeAccountsRoute_ReportsMissingIdentity(t *testing.T) {
	s := testServer(t, 0)
	dir := t.TempDir()
	claudeConfigPath := filepath.Join(dir, ".claude.json")
	require.NoError(t, os.WriteFile(claudeConfigPath, []byte(`{}`), 0o600))

	s.cfg.Claude = config.ClaudeConfig{
		Accounts: map[string]config.ClaudeAccountConfig{
			"personal": {Credentials: "keychain", ClaudeConfig: claudeConfigPath},
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/claude/accounts", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var body ClaudeAccountsInfo
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Accounts, 1)
	assert.False(t, body.Accounts[0].IdentityAvailable)
	assert.NotEmpty(t, body.Accounts[0].IdentityError)
}

func TestClaudeAccountsRoute_EmptyWithNoAccountsConfigured(t *testing.T) {
	s := testServer(t, 0)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/claude/accounts", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var body ClaudeAccountsInfo
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Empty(t, body.Accounts)
}

func TestClaudeAccountTestRoute_TriggersConfiguredAccount(t *testing.T) {
	s := testServer(t, 0)
	s.cfg.Claude = config.ClaudeConfig{
		Accounts: map[string]config.ClaudeAccountConfig{
			"personal": {Credentials: "keychain"},
		},
	}
	var triggered string
	s.pollerTrigger = func(name string) error {
		triggered = name
		return nil
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/claude/accounts/personal/test", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var body ClaudeAccountTestResult
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.True(t, body.Success)
	assert.Equal(t, "claude-usage:personal", triggered)
}

func TestClaudeAccountTestRoute_UnknownAccountReturns404(t *testing.T) {
	s := testServer(t, 0)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/claude/accounts/nope/test", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestClaudeAccountTestRoute_ReportsTriggerError(t *testing.T) {
	s := testServer(t, 0)
	s.cfg.Claude = config.ClaudeConfig{
		Accounts: map[string]config.ClaudeAccountConfig{
			"personal": {Credentials: "keychain"},
		},
	}
	s.pollerTrigger = func(name string) error {
		return errors.New("boom")
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/claude/accounts/personal/test", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var body ClaudeAccountTestResult
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.False(t, body.Success)
	assert.NotEmpty(t, body.Error)
}

// TestClaudeAccountTestRoute_ReportsRetryAfterPending covers a roborev
// finding: TriggerNow bypasses a job's Cooldown by design, so a Test click
// right after a healthy poll is expected to run immediately, but it must
// not bypass a pending Retry-After wait the same way -- retrying now would
// just repeat the same request an upstream 429 explicitly asked to wait
// out. That check now lives in poller.Scheduler.attempt itself (checked
// unconditionally, even for a TriggerNow call) rather than as a separate
// pre-check here, so a *poller.ErrRetryAfterPending from the trigger
// function is what this handler must translate into a clear message
// without ever reporting success.
func TestClaudeAccountTestRoute_ReportsRetryAfterPending(t *testing.T) {
	s := testServer(t, 0)
	s.cfg.Claude = config.ClaudeConfig{
		Accounts: map[string]config.ClaudeAccountConfig{
			"personal": {Credentials: "keychain"},
		},
	}
	s.pollerTrigger = func(name string) error {
		return &poller.ErrRetryAfterPending{Remaining: 90 * time.Second}
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/claude/accounts/personal/test", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var body ClaudeAccountTestResult
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.False(t, body.Success)
	assert.Contains(t, body.Error, "retry-after")
	assert.Contains(t, body.Error, "1m30s")
}

func TestClaudeRateLimitSnapshotsIngestRoute_InsertsRows(t *testing.T) {
	s := testServer(t, 0)

	body := `{"snapshots":[{"accountId":"acct-1","accountLabel":"Acme","windowKind":"five_hour","usedPercent":42,"resetsAt":1789435448,"observedAt":"2026-09-09T10:00:00Z"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/claude/rate-limit-snapshots", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var result ClaudeRateLimitSnapshotsIngestResult
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
	assert.Equal(t, 1, result.Inserted)

	local := s.db.(*db.DB)
	rows, err := local.RateLimitSnapshotHistory(
		context.Background(), db.RateLimitHistoryFilter{Vendor: "claude", AccountID: "acct-1"},
	)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "claude", rows[0].Vendor)
	assert.Equal(t, "Acme", rows[0].AccountLabel)
	assert.Equal(t, "five_hour", rows[0].WindowKind)
	assert.InDelta(t, 42, rows[0].UsedPercent, 0.001)
}

// TestClaudeRateLimitSnapshotsIngestRoute_ReconstructsWindowMinutes
// covers a roborev finding: the statusline sink's direct-write path
// populates WindowMinutes via claude.CanonicalWindowMinutes(WindowKind),
// but the wire shape this endpoint accepts (ClaudeRateLimitSnapshotInput)
// carries no windowMinutes field of its own, so every snapshot ingested
// through a running daemon lost its duration -- and with it the
// "Session limit"/"Weekly limit" title -- until the next oauth/usage
// poll happened to supply one. The daemon must reconstruct it the same
// way for the "session"/"weekly" kinds the sink actually sends.
func TestClaudeRateLimitSnapshotsIngestRoute_ReconstructsWindowMinutes(t *testing.T) {
	s := testServer(t, 0)

	body := `{"snapshots":[
		{"accountId":"acct-1","windowKind":"session","usedPercent":10,"observedAt":"2026-09-09T10:00:00Z"},
		{"accountId":"acct-1","windowKind":"weekly","usedPercent":20,"observedAt":"2026-09-09T10:01:00Z"}
	]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/claude/rate-limit-snapshots", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	local := s.db.(*db.DB)
	rows, err := local.RateLimitSnapshotHistory(
		context.Background(), db.RateLimitHistoryFilter{Vendor: "claude", AccountID: "acct-1"},
	)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	byWindow := map[string]db.RateLimitSnapshot{}
	for _, r := range rows {
		byWindow[r.WindowKind] = r
	}
	assert.Equal(t, 300, byWindow["session"].WindowMinutes)
	assert.Equal(t, 10080, byWindow["weekly"].WindowMinutes)
}

func TestClaudeRateLimitSnapshotsIngestRoute_RequiresWindowKindAndObservedAt(t *testing.T) {
	s := testServer(t, 0)

	body := `{"snapshots":[{"accountId":"acct-1","usedPercent":42}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/claude/rate-limit-snapshots", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
}

func TestClaudeRateLimitSnapshotsIngestRoute_EmptyBatchIsANoOp(t *testing.T) {
	s := testServer(t, 0)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/claude/rate-limit-snapshots", strings.NewReader(`{"snapshots":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var result ClaudeRateLimitSnapshotsIngestResult
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
	assert.Equal(t, 0, result.Inserted)
}
