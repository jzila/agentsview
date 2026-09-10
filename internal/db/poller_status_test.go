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
