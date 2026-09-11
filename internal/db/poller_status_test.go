package db

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/poller"
)

func TestPollerStatusRoundTrip(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	want := poller.Status{
		Name:                "pricing-refresh",
		LastAttempt:         now,
		LastSuccess:         now.Add(-time.Hour),
		LastError:           "boom",
		ConsecutiveFailures: 2,
		NextRun:             now.Add(24 * time.Hour),
		RetryAfterUntil:     now.Add(24 * time.Hour),
	}
	require.NoError(t, d.SaveStatus(ctx, want))

	statuses, err := d.LoadPollerStatuses(ctx)
	require.NoError(t, err)
	require.Contains(t, statuses, "pricing-refresh")
	got := statuses["pricing-refresh"]
	assert.Equal(t, want.Name, got.Name)
	assert.True(t, want.LastAttempt.Equal(got.LastAttempt))
	assert.True(t, want.LastSuccess.Equal(got.LastSuccess))
	assert.Equal(t, want.LastError, got.LastError)
	assert.Equal(t, want.ConsecutiveFailures, got.ConsecutiveFailures)
	assert.True(t, want.NextRun.Equal(got.NextRun))
	assert.True(t, want.RetryAfterUntil.Equal(got.RetryAfterUntil))
}

func TestPollerStatusUpsertOverwrites(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	require.NoError(t, d.SaveStatus(ctx, poller.Status{
		Name:                "cursor-usage",
		ConsecutiveFailures: 1,
		LastError:           "first",
	}))
	require.NoError(t, d.SaveStatus(ctx, poller.Status{
		Name:                "cursor-usage",
		ConsecutiveFailures: 0,
		LastError:           "",
		LastSuccess:         time.Now().UTC(),
	}))

	statuses, err := d.LoadPollerStatuses(ctx)
	require.NoError(t, err)
	got := statuses["cursor-usage"]
	assert.Zero(t, got.ConsecutiveFailures)
	assert.Empty(t, got.LastError)
	assert.False(t, got.LastSuccess.IsZero())
}

func TestPollerStatusMissingRowNotInMap(t *testing.T) {
	d := testDB(t)
	statuses, err := d.LoadPollerStatuses(context.Background())
	require.NoError(t, err)
	assert.NotContains(t, statuses, "never-run")
}

// TestCopyPollerStatusFromCopiesRows covers the resync archive-replacement
// path: scheduling diagnostics (cooldown/backoff/Retry-After deadlines
// encoded in next_run) must survive the swap, or a restart right after
// resync would lose them and every job would report as "never run".
func TestCopyPollerStatusFromCopiesRows(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	srcPath := filepath.Join(dir, "src.db")
	srcDB := testDBAtPath(t, srcPath, "src")
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	want := poller.Status{
		Name:                "pricing-refresh",
		LastAttempt:         now,
		LastSuccess:         now.Add(-time.Hour),
		LastError:           "boom",
		ConsecutiveFailures: 3,
		NextRun:             now.Add(24 * time.Hour),
	}
	require.NoError(t, srcDB.SaveStatus(ctx, want))
	srcDB.Close()

	dstPath := filepath.Join(dir, "dst.db")
	dstDB := testDBAtPath(t, dstPath, "dst")
	defer dstDB.Close()
	// A stale row for the same job must be replaced, not merged with.
	require.NoError(t, dstDB.SaveStatus(ctx, poller.Status{
		Name:                "pricing-refresh",
		ConsecutiveFailures: 0,
	}))

	require.NoError(t, dstDB.CopyPollerStatusFrom(srcPath))

	statuses, err := dstDB.LoadPollerStatuses(ctx)
	require.NoError(t, err)
	require.Contains(t, statuses, "pricing-refresh")
	got := statuses["pricing-refresh"]
	assert.True(t, want.LastAttempt.Equal(got.LastAttempt))
	assert.True(t, want.LastSuccess.Equal(got.LastSuccess))
	assert.Equal(t, want.LastError, got.LastError)
	assert.Equal(t, want.ConsecutiveFailures, got.ConsecutiveFailures)
	assert.True(t, want.NextRun.Equal(got.NextRun))
}

// TestCopyPollerStatusFromToleratesMissingTable covers an archive built
// before poller_status existed: the copy must be a silent no-op, not an
// error that aborts resync.
func TestCopyPollerStatusFromToleratesMissingTable(t *testing.T) {
	dir := t.TempDir()

	srcPath := filepath.Join(dir, "src.db")
	srcDB := testDBAtPath(t, srcPath, "src")
	_, err := srcDB.getWriter().Exec("DROP TABLE poller_status")
	require.NoError(t, err)
	srcDB.Close()

	dstPath := filepath.Join(dir, "dst.db")
	dstDB := testDBAtPath(t, dstPath, "dst")
	defer dstDB.Close()

	require.NoError(t, dstDB.CopyPollerStatusFrom(srcPath))
	statuses, err := dstDB.LoadPollerStatuses(context.Background())
	require.NoError(t, err)
	assert.Empty(t, statuses)
}

// TestCopyPollerStatusFromToleratesMissingRetryAfterUntilColumn covers a
// source archive built before retry_after_until existed: the table is
// present but the column is not, and the copy must still succeed (leaving
// retry_after_until at its default) rather than failing outright on "no
// such column" (roborev finding companion: CopyRateLimitSnapshotsFrom
// tolerates an analogous archive-generation gap the same way).
func TestCopyPollerStatusFromToleratesMissingRetryAfterUntilColumn(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	srcPath := filepath.Join(dir, "src.db")
	srcDB := testDBAtPath(t, srcPath, "src")
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	require.NoError(t, srcDB.SaveStatus(ctx, poller.Status{
		Name:                "claude-usage:personal",
		LastError:           "429",
		ConsecutiveFailures: 1,
		NextRun:             now.Add(time.Hour),
	}))
	_, err := srcDB.getWriter().Exec("ALTER TABLE poller_status DROP COLUMN retry_after_until")
	require.NoError(t, err)
	srcDB.Close()

	dstPath := filepath.Join(dir, "dst.db")
	dstDB := testDBAtPath(t, dstPath, "dst")
	defer dstDB.Close()

	require.NoError(t, dstDB.CopyPollerStatusFrom(srcPath))

	statuses, err := dstDB.LoadPollerStatuses(ctx)
	require.NoError(t, err)
	require.Contains(t, statuses, "claude-usage:personal")
	got := statuses["claude-usage:personal"]
	assert.Equal(t, "429", got.LastError)
	assert.True(t, now.Add(time.Hour).Equal(got.NextRun))
	assert.True(t, got.RetryAfterUntil.IsZero())
}

// TestSchemaColumnMigrations_AddsRetryAfterUntilToExistingPollerStatusTable
// covers the same roborev-ci finding as its rate_limit_snapshots.severity
// counterpart: poller_status is defined directly in schema.sql's CREATE
// TABLE IF NOT EXISTS, which is a no-op against a table an earlier build
// of this same branch already created without retry_after_until --
// LoadPollerStatuses and SaveStatus both assume the column always exists
// (see LoadPollerStatuses's doc comment), so a writable open against such
// an archive would otherwise fail every poller status read and write with
// "no such column: retry_after_until". schemaColumnMigrations carries an
// ALTER TABLE entry for exactly this column so a writable open picks it
// up before the scheduler ever queries it.
func TestSchemaColumnMigrations_AddsRetryAfterUntilToExistingPollerStatusTable(t *testing.T) {
	path := createClosedTestDB(t, tempDBPath(t, "sessions.db"), nil)
	execRawSQLite(t, path, "ALTER TABLE poller_status DROP COLUMN retry_after_until")

	d, err := Open(path)
	require.NoError(t, err)
	defer d.Close()

	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	require.NoError(t, d.SaveStatus(context.Background(), poller.Status{
		Name:            "claude-usage:personal",
		RetryAfterUntil: now,
	}), "a writable open must have migrated retry_after_until back in")

	statuses, err := d.LoadPollerStatuses(context.Background())
	require.NoError(t, err)
	require.Contains(t, statuses, "claude-usage:personal")
	assert.True(t, now.Equal(statuses["claude-usage:personal"].RetryAfterUntil))
}
