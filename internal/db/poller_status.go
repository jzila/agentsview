package db

import (
	"context"
	"fmt"
	"time"

	"go.kenn.io/agentsview/internal/poller"
)

// LoadPollerStatuses implements poller.StatusStore, reading every persisted
// job status from the SQLite-only poller_status table (see
// docs/agents/storage.md). A job with no row simply is not present in the
// returned map; the Scheduler treats that as "never attempted."
func (db *DB) LoadPollerStatuses(ctx context.Context) (map[string]poller.Status, error) {
	// No read-only-open tolerance is needed here for a missing
	// retry_after_until column (contrast isMissingRateLimitSnapshotsColumnErr):
	// LoadPollerStatuses is only ever called through poller.Scheduler's
	// StatusStore, which cmd/agentsview wires exclusively to the daemon's
	// own writable *DB -- the same connection applySchemaColumnMigrations
	// already ran against before Start can call it, so the column always
	// exists by the time this query runs.
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT name, last_attempt, last_success, last_error,
		       consecutive_failures, next_run, retry_after_until
		FROM poller_status
	`)
	if err != nil {
		return nil, fmt.Errorf("reading poller_status: %w", err)
	}
	defer rows.Close()

	out := make(map[string]poller.Status)
	for rows.Next() {
		var (
			name                string
			lastAttempt         string
			lastSuccess         string
			lastError           string
			consecutiveFailures int
			nextRun             string
			retryAfterUntil     string
		)
		if err := rows.Scan(
			&name, &lastAttempt, &lastSuccess, &lastError,
			&consecutiveFailures, &nextRun, &retryAfterUntil,
		); err != nil {
			return nil, fmt.Errorf("scanning poller_status: %w", err)
		}
		status := poller.Status{
			Name:                name,
			LastError:           lastError,
			ConsecutiveFailures: consecutiveFailures,
		}
		status.LastAttempt = parsePollerStatusTime(lastAttempt)
		status.LastSuccess = parsePollerStatusTime(lastSuccess)
		status.NextRun = parsePollerStatusTime(nextRun)
		status.RetryAfterUntil = parsePollerStatusTime(retryAfterUntil)
		out[name] = status
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading poller_status: %w", err)
	}
	return out, nil
}

// SaveStatus implements poller.StatusStore, upserting one job's status row.
func (db *DB) SaveStatus(ctx context.Context, status poller.Status) error {
	_, err := db.getWriter().ExecContext(ctx, `
		INSERT INTO poller_status
			(name, last_attempt, last_success, last_error,
			 consecutive_failures, next_run, retry_after_until, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			last_attempt = excluded.last_attempt,
			last_success = excluded.last_success,
			last_error = excluded.last_error,
			consecutive_failures = excluded.consecutive_failures,
			next_run = excluded.next_run,
			retry_after_until = excluded.retry_after_until,
			updated_at = excluded.updated_at
	`,
		status.Name,
		formatPollerStatusTime(status.LastAttempt),
		formatPollerStatusTime(status.LastSuccess),
		status.LastError,
		status.ConsecutiveFailures,
		formatPollerStatusTime(status.NextRun),
		formatPollerStatusTime(status.RetryAfterUntil),
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("saving poller_status for %q: %w", status.Name, err)
	}
	return nil
}

// CopyPollerStatusFrom copies every poller_status row from the database
// file at sourcePath into this database. Called during a resync so
// scheduling diagnostics (last success/error, cooldown, backoff, and
// Retry-After deadlines encoded in next_run) survive the archive
// replacement; without it every job reports as "never run" and the
// Scheduler restarts its cooldown/backoff clock from zero. sourcePath
// predating this table (an archive built before poller_status existed) is
// not an error: there is simply nothing to copy.
func (db *DB) CopyPollerStatusFrom(sourcePath string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	// Pin a single connection: ATTACH is connection-scoped and
	// database/sql's pool doesn't guarantee the same underlying
	// connection across separate Exec calls.
	ctx := context.Background()
	conn, err := db.getWriter().Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquiring connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(
		ctx, "ATTACH DATABASE ? AS old_db", sourcePath,
	); err != nil {
		return fmt.Errorf("attaching source db: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(ctx, "DETACH DATABASE old_db")
	}()

	var hasPollerStatus bool
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM old_db.sqlite_master
		WHERE type = 'table' AND name = 'poller_status'
	)`).Scan(&hasPollerStatus); err != nil {
		return fmt.Errorf("checking poller status storage: %w", err)
	}
	if !hasPollerStatus {
		return nil
	}

	// retry_after_until is a later column: a source archive built before
	// it existed has the table but not this column, so the copy falls
	// back to leaving it at its '' default for copied rows rather than
	// failing the whole copy over "no such column".
	var hasRetryAfterUntil bool
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM pragma_table_info('poller_status', 'old_db')
		WHERE name = 'retry_after_until'
	)`).Scan(&hasRetryAfterUntil); err != nil {
		return fmt.Errorf("checking source poller status columns: %w", err)
	}

	copyQuery := `
		INSERT OR REPLACE INTO poller_status
			(name, last_attempt, last_success, last_error,
			 consecutive_failures, next_run, updated_at)
		SELECT name, last_attempt, last_success, last_error,
			consecutive_failures, next_run, updated_at
		FROM old_db.poller_status`
	if hasRetryAfterUntil {
		copyQuery = `
			INSERT OR REPLACE INTO poller_status
				(name, last_attempt, last_success, last_error,
				 consecutive_failures, next_run, retry_after_until, updated_at)
			SELECT name, last_attempt, last_success, last_error,
				consecutive_failures, next_run, retry_after_until, updated_at
			FROM old_db.poller_status`
	}
	if _, err := conn.ExecContext(ctx, copyQuery); err != nil {
		return fmt.Errorf("copying poller status: %w", err)
	}
	return nil
}

func formatPollerStatusTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parsePollerStatusTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
