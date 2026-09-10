package cursorusage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestJobRunFetchesAndStoresEvents(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "admin_usage_page.json"))
	require.NoError(t, err)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	t.Cleanup(srv.Close)

	database := dbtest.OpenTestDB(t)
	client := NewClientWithBaseURL(srv.URL, "cursor-key")
	job := NewJob(client, database, time.Minute, "", "")

	require.NoError(t, job.Run(context.Background()))

	count, err := countCursorUsageEvents(database)
	require.NoError(t, err)
	assert.Equal(t, 2, count)
}

// TestJobRunDedupsOnRepeatedPoll runs the same job twice against the same
// fixture, simulating two poll cycles whose lookback windows overlap. The
// second poll must not create duplicate rows: internal/db's
// cursor_usage_events unique dedup_key index is expected to hold.
func TestJobRunDedupsOnRepeatedPoll(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "admin_usage_page.json"))
	require.NoError(t, err)

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	t.Cleanup(srv.Close)

	database := dbtest.OpenTestDB(t)
	client := NewClientWithBaseURL(srv.URL, "cursor-key")
	job := NewJob(client, database, time.Minute, "", "")

	require.NoError(t, job.Run(context.Background()))
	require.NoError(t, job.Run(context.Background()))

	assert.Equal(t, int32(2), requests.Load(), "job polled twice")
	count, err := countCursorUsageEvents(database)
	require.NoError(t, err)
	assert.Equal(t, 2, count, "second poll must dedup, not double-insert")
}

// TestJobRunRecoversAfterUnauthorized covers the 401 case: Run must surface
// an error the Scheduler can record as Status.LastError, without panicking
// or leaving the archive in a partial state, and the same Job must succeed
// on a later attempt once the credential (here, the server's behavior) is
// fixed.
func TestJobRunRecoversAfterUnauthorized(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "admin_usage_page.json"))
	require.NoError(t, err)

	var unauthorized atomic.Bool
	unauthorized.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if unauthorized.Load() {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	t.Cleanup(srv.Close)

	database := dbtest.OpenTestDB(t)
	client := NewClientWithBaseURL(srv.URL, "cursor-key")
	job := NewJob(client, database, time.Minute, "", "")

	runErr := job.Run(context.Background())
	require.Error(t, runErr)
	assert.Contains(t, runErr.Error(), "401")
	count, err := countCursorUsageEvents(database)
	require.NoError(t, err)
	assert.Zero(t, count, "a failed attempt must not leave partial rows")

	unauthorized.Store(false)
	require.NoError(t, job.Run(context.Background()))

	count, err = countCursorUsageEvents(database)
	require.NoError(t, err)
	assert.Equal(t, 2, count)
}

func TestJobNameAndInterval(t *testing.T) {
	job := NewJob(NewClient("key"), nil, 0, "", "")
	assert.Equal(t, JobName, job.Name())
	assert.Equal(t, DefaultInterval, job.Interval())

	job = NewJob(NewClient("key"), nil, 15*time.Minute, "", "")
	assert.Equal(t, 15*time.Minute, job.Interval())
}

// TestResolveLookbackCoversLongerIntervals covers the gap a fixed 24h
// lookback would otherwise leave once [poller.intervals] configures
// "cursor-usage" longer than that: consecutive on-schedule polls must always
// overlap, so lookback needs to grow with interval rather than staying
// fixed.
func TestResolveLookbackCoversLongerIntervals(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		want     time.Duration
	}{
		{name: "default interval keeps the default lookback", interval: DefaultInterval, want: defaultLookback},
		{name: "short interval keeps the default lookback", interval: time.Minute, want: defaultLookback},
		{
			name:     "interval near the default lookback still keeps it",
			interval: defaultLookback - time.Hour,
			want:     defaultLookback,
		},
		{
			name:     "interval longer than the default lookback grows it",
			interval: 48 * time.Hour,
			want:     48*time.Hour + lookbackMargin,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, resolveLookback(tt.interval))
		})
	}
}

// TestJobUsesGrownLookbackForLongInterval covers the bug directly: a Job
// configured with an interval longer than the old fixed 24h lookback must
// still fetch a window reaching back further than 24h, or every poll would
// permanently miss the events that landed in the gap between polls.
func TestJobUsesGrownLookbackForLongInterval(t *testing.T) {
	interval := 48 * time.Hour
	job := NewJob(NewClient("key"), nil, interval, "", "")
	assert.Greater(t, job.lookback, 24*time.Hour)
	assert.Equal(t, interval+lookbackMargin, job.lookback)
}

func countCursorUsageEvents(database *db.DB) (int, error) {
	events, err := database.GetCursorUsageEvents(context.Background(), 0)
	if err != nil {
		return 0, err
	}
	return len(events), nil
}
