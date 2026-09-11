package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestStagedImportHonorsDisabledSignalRecomputation(t *testing.T) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122e01"
	database := openTestDB(t)
	root := writeCodexTranscriptRoot(t, uuid, codexParityTranscript(uuid))
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}},
		Machine:   "local", Ephemeral: true,
		StagedCodexParseMinBytes: 1, DisableSignalRecomputation: true,
		DisableFilesystemProjectDiscovery: true,
	})
	t.Cleanup(engine.Close)
	stats := engine.SyncAll(t.Context(), nil)
	require.Zero(t, stats.Failed)
	require.Equal(t, 1, stats.Synced)
	sess, err := database.GetSessionFull(t.Context(), "codex:"+uuid)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Zero(t, sess.ToolFailureSignalCount)
	assert.Zero(t, sess.SecretLeakCount)
	findings, err := database.SessionSecretFindings(t.Context(), "codex:"+uuid)
	require.NoError(t, err)
	assert.Empty(t, findings)
	_, hasState, err := database.GetSessionSignalState("codex:" + uuid)
	require.NoError(t, err)
	assert.False(t, hasState)
	msgs, err := database.GetAllMessages(t.Context(), "codex:"+uuid)
	require.NoError(t, err)
	require.NotEmpty(t, msgs, "disabling derived work still publishes transcript content")
}

func TestClaudeImportDoesNotBuildCodexSignalState(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	project := filepath.Join(root, "project-a")
	require.NoError(t, os.MkdirAll(project, 0o755))
	fixture := testjsonl.NewSessionBuilder().AddClaudeUser("2026-07-10T07:00:00Z", "hello").AddClaudeAssistant("2026-07-10T07:00:01Z", "finished")
	require.NoError(t, os.WriteFile(filepath.Join(project, "session-a.jsonl"), []byte(fixture.String()), 0o600))
	engine := NewEngine(database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}, Machine: "local", Ephemeral: true, DisableFilesystemProjectDiscovery: true})
	t.Cleanup(engine.Close)
	stats := engine.SyncAll(t.Context(), nil)
	require.Zero(t, stats.Failed)
	require.Equal(t, 1, stats.Synced)
	var sessionID string
	require.NoError(t, database.Reader().QueryRow("SELECT id FROM sessions").Scan(&sessionID))
	_, exists, err := database.GetSessionSignalState(sessionID)
	require.NoError(t, err)
	assert.False(t, exists, "only checkpoint-backed Codex appends consume compact state")
	_, err = engine.recomputeSignalsFromDB(t.Context(), sessionID)
	require.NoError(t, err)
	_, exists, err = database.GetSessionSignalState(sessionID)
	require.NoError(t, err)
	assert.False(t, exists)
	sess, err := database.GetSessionFull(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.NotEmpty(t, sess.SecretsRulesVersion, "normal derived signals are still computed")
}

// TestStagedCodexImportPersistsRateLimitSnapshots covers roborev finding
// cyt9 #1: stagedCodexParseOutcome (the streaming staging path forced by a
// low StagedCodexParseMinBytes) must forward sess.RateLimitSnapshots into
// ParseResult.RateLimitSnapshots, the same way the collecting Parse() path
// does, or a staged import silently drops every rate-limit observation it
// extracted.
func TestStagedCodexImportPersistsRateLimitSnapshots(t *testing.T) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	transcript := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/workspace/project-a", "codex_cli_rs",
			"2024-01-01T10:00:00Z",
		),
		testjsonl.CodexTurnContextJSON("gpt-5.4", "2024-01-01T10:00:01Z"),
		testjsonl.CodexMsgJSON("user", "hello", "2024-01-01T10:00:02Z"),
		testjsonl.CodexMsgJSON("assistant", "hi", "2024-01-01T10:00:03Z"),
		testjsonl.CodexTokenCountWithRateLimitsJSON(
			"2024-01-01T10:00:04Z", 10000, 500, 6000,
			"codex", "pro",
			&testjsonl.CodexRateLimitWindow{
				UsedPercent: 42, WindowMinutes: 10080, ResetsAt: 1789435448,
			},
			nil,
			"100.0",
		),
	)
	database := openTestDB(t)
	root := writeCodexTranscriptRoot(t, uuid, transcript)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}},
		Machine:   "local", Ephemeral: true,
		// A byte-sized threshold forces every Codex file through the
		// streaming staging path (stagedCodexParseOutcome) rather than
		// the provider's collecting Parse(), which is the path this
		// finding is about.
		StagedCodexParseMinBytes:          1,
		DisableFilesystemProjectDiscovery: true,
	})
	t.Cleanup(engine.Close)
	stats := engine.SyncAll(t.Context(), nil)
	require.Zero(t, stats.Failed)
	require.Equal(t, 1, stats.Synced)

	rows, err := database.RateLimitSnapshotHistory(
		t.Context(), db.RateLimitHistoryFilter{Vendor: "codex", Machine: "local"},
	)
	require.NoError(t, err)
	require.Len(t, rows, 1, "staged Codex import must persist rate-limit snapshots")
	assert.Equal(t, "codex:"+uuid, rows[0].SessionID)
	assert.Equal(t, "codex", rows[0].LimitID)
	assert.Equal(t, "pro", rows[0].PlanType)
	assert.Equal(t, "primary", rows[0].WindowKind)
	assert.InDelta(t, 42.0, rows[0].UsedPercent, 0.001)
}
