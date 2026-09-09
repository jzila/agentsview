package sync_test

import (
	"database/sql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	agentsync "go.kenn.io/agentsview/internal/sync"
	"os"
	"path/filepath"
	"testing"
)

func TestCopilotStoreGapAndRecoveryDoNotDoubleCount(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "session-state", "gap", "events.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(`{"type":"session.start","timestamp":"2026-09-08T12:00:00Z","data":{"sessionId":"gap"}}
{"type":"assistant.message","timestamp":"2026-09-08T12:00:01Z","data":{"content":"First","model":"gpt-5.4","outputTokens":3}}
{"type":"assistant.message","timestamp":"2026-09-08T12:00:02Z","data":{"content":"Later","model":"gpt-5.4","outputTokens":7}}
`), 0o644))
	storePath := filepath.Join(root, "session-store.db")
	store, err := sql.Open("sqlite3", storePath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	_, err = store.Exec(`PRAGMA journal_mode=WAL;
CREATE TABLE sessions(id TEXT PRIMARY KEY);
INSERT INTO sessions VALUES('gap');
CREATE TABLE assistant_usage_events(id INTEGER PRIMARY KEY AUTOINCREMENT,
session_id TEXT,model TEXT,input_tokens INTEGER,output_tokens INTEGER,
cache_read_tokens INTEGER,cache_write_tokens INTEGER,reasoning_tokens INTEGER,created_at TEXT);
CREATE INDEX idx_assistant_usage_events_session ON assistant_usage_events(session_id,id);
INSERT INTO assistant_usage_events VALUES(1,'gap','gpt-5.4',100,7,0,0,0,'2026-09-08T12:00:03Z');`)
	require.NoError(t, err)
	archive := dbtest.OpenTestDB(t)
	engine := agentsync.NewEngine(archive, agentsync.EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCopilot: {root}}, Machine: "local"})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	usage, err := archive.GetSessionUsage(t.Context(), "copilot:gap", true)
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, 10, usage.TotalOutputTokens)
	require.Len(t, usage.Breakdown, 2)
	total := 0
	for _, entry := range usage.Breakdown {
		total += entry.OutputTokens
	}
	assert.Equal(t, 10, total, "usage reports include the remainder exactly once")
	_, err = store.Exec(`INSERT INTO assistant_usage_events VALUES(2,'gap','gpt-5.4',100,3,0,0,0,'2026-09-08T12:00:01Z')`)
	require.NoError(t, err)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{storePath + "-wal"}))
	usage, err = archive.GetSessionUsage(t.Context(), "copilot:gap", true)
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, 10, usage.TotalOutputTokens)
	require.Len(t, usage.Breakdown, 2)
	for _, entry := range usage.Breakdown {
		assert.Equal(t, "session-store", entry.Source)
	}
}
