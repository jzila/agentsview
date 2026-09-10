package cursorusage

import (
	"context"
	"fmt"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

// JobName is the stable internal/poller.Job name for the Cursor Admin usage
// poll.
const JobName = "cursor-usage"

// DefaultInterval is the default poll cadence when [poller.intervals] does
// not override "cursor-usage".
const DefaultInterval = 30 * time.Minute

// DefaultPageSize matches the CLI's default page size.
const DefaultPageSize = 100

// defaultLookback is the window fetched on every poll when interval is at
// or below it. It deliberately overlaps prior polls (the default interval
// is much shorter) so an event that lands or settles late is still picked
// up; the cursor_usage_events dedup key collapses the resulting re-fetches
// to no-ops.
const defaultLookback = 24 * time.Hour

// lookbackMargin is the minimum overlap a poll's lookback keeps beyond the
// configured interval, covering the Scheduler's jitter and the slack an
// occasional delayed or backed-off attempt introduces.
const lookbackMargin = 10 * time.Minute

// resolveLookback picks the fetch window for one poll. A fixed
// defaultLookback is wrong once interval exceeds it: consecutive on-schedule
// polls would leave a permanent gap between the end of one poll's window and
// the start of the next, silently dropping every event that landed in
// between. Scaling the lookback with interval keeps consecutive polls
// overlapping regardless of how [poller.intervals] configures this job.
func resolveLookback(interval time.Duration) time.Duration {
	if min := interval + lookbackMargin; min > defaultLookback {
		return min
	}
	return defaultLookback
}

// Job polls the Cursor Admin API for usage events on an interval and stores
// them in the archive, reusing the existing cursor_usage_events dedup path.
// It also backs `agentsview usage cursor`'s on-demand fetch through
// FetchAndStore, so both paths share one implementation.
type Job struct {
	client   *Client
	database *db.DB
	email    string
	userID   string
	pageSize int
	lookback time.Duration
	interval time.Duration
}

// NewJob builds the Cursor usage poll Job. interval overrides
// DefaultInterval (e.g. from [poller.intervals] config); a non-positive
// value falls back to DefaultInterval.
func NewJob(
	client *Client, database *db.DB, interval time.Duration, email, userID string,
) *Job {
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &Job{
		client:   client,
		database: database,
		email:    email,
		userID:   userID,
		pageSize: DefaultPageSize,
		lookback: resolveLookback(interval),
		interval: interval,
	}
}

func (j *Job) Name() string { return JobName }

func (j *Job) Interval() time.Duration { return j.interval }

func (j *Job) Run(ctx context.Context) error {
	now := time.Now()
	start := now.Add(-j.lookback)
	_, err := FetchAndStore(
		ctx, j.client, j.database, start, now, j.pageSize, j.email, j.userID,
	)
	return err
}

// FetchAndStore fetches Cursor Admin usage events for [start, end] and
// inserts them into the archive. Duplicate events (by the existing
// cursor_usage_events dedup key) are silently ignored by the insert path, so
// calling this repeatedly with overlapping windows is safe. It returns the
// number of events fetched from the API, not the number of new rows
// inserted.
func FetchAndStore(
	ctx context.Context,
	client *Client,
	database *db.DB,
	start, end time.Time,
	pageSize int,
	email, userID string,
) (int, error) {
	if client == nil {
		return 0, fmt.Errorf("cursorusage: nil client")
	}
	if pageSize <= 0 {
		pageSize = DefaultPageSize
	}

	events, err := client.FetchAllUsageEvents(ctx, Query{
		StartDate: start,
		EndDate:   end,
		PageSize:  pageSize,
		Email:     email,
		UserID:    userID,
	})
	if err != nil {
		return 0, err
	}

	rows := make([]db.CursorUsageEvent, 0, len(events))
	for _, ev := range events {
		rows = append(rows, db.CursorUsageEvent{
			OccurredAt:       ev.Timestamp.UTC().Format(time.RFC3339Nano),
			Model:            ev.Model,
			Kind:             ev.Kind,
			InputTokens:      ev.TokenUsage.InputTokens,
			OutputTokens:     ev.TokenUsage.OutputTokens,
			CacheWriteTokens: ev.TokenUsage.CacheWriteTokens,
			CacheReadTokens:  ev.TokenUsage.CacheReadTokens,
			Charged:          ev.Charged,
			CursorTokenFee:   ev.CursorTokenFee,
			UserID:           ev.UserID,
			UserEmail:        ev.UserEmail,
			IsHeadless:       ev.IsHeadless,
		})
	}
	if err := database.InsertCursorUsageEvents(rows); err != nil {
		return len(events), err
	}
	return len(events), nil
}
