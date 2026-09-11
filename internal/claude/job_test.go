package claude

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/poller"
)

func writeClaudeConfig(t *testing.T, dir, accountUUID, orgName string) string {
	t.Helper()
	path := filepath.Join(dir, ".claude.json")
	body := `{"oauthAccount":{"accountUuid":"` + accountUUID + `","emailAddress":"person@example.com",` +
		`"organizationName":"` + orgName + `","organizationType":"claude_team","seatTier":"team_tier_1",` +
		`"userRateLimitTier":"default_claude_max_5x","organizationRateLimitTier":"default_raven"}}`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

// writeClaudeConfigWithOrg is writeClaudeConfig plus an explicit
// organizationUuid, for kata 1k4b's org-switch coverage: a real
// ~/.claude.json always carries organizationUuid (see the Identity doc
// comment), but writeClaudeConfig omits it since no prior test needed to
// distinguish two organizations sharing one accountUuid.
func writeClaudeConfigWithOrg(t *testing.T, dir, accountUUID, orgUUID, orgName string) string {
	t.Helper()
	path := filepath.Join(dir, ".claude.json")
	body := `{"oauthAccount":{"accountUuid":"` + accountUUID + `","emailAddress":"person@example.com",` +
		`"organizationUuid":"` + orgUUID + `","organizationName":"` + orgName + `",` +
		`"organizationType":"claude_team","seatTier":"team_tier_1",` +
		`"userRateLimitTier":"default_claude_max_5x","organizationRateLimitTier":"default_raven"}}`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func newTestJob(t *testing.T, baseURL string, database *db.DB) *Job {
	t.Helper()
	dir := t.TempDir()
	configPath := writeClaudeConfig(t, dir, "acct-uuid-1", "Acme")
	t.Setenv("AGENTSVIEW_TEST_JOB_TOKEN", "test-token")
	return NewJob(
		"personal",
		&Client{BaseURL: baseURL, HTTPClient: http.DefaultClient},
		database,
		dir,
		CredentialsSource{Kind: "env", Value: "AGENTSVIEW_TEST_JOB_TOKEN"},
		configPath,
		time.Minute,
	)
}

// TestNewJob_ClaudeConfigPath covers the roborev Medium finding on kata
// 9rs0 (filed as kata qs2f): both documented [claude.accounts.NAME]
// examples use `claude_config = "~/..."`, which reached os.ReadFile
// literally and always failed. NewJob must expand a leading "~" against
// HomeDir before storing ClaudeConfigPath, and leave an absolute path
// unchanged.
func TestNewJob_ClaudeConfigPath(t *testing.T) {
	tests := []struct {
		name             string
		claudeConfigPath string
		want             string
	}{
		{"expands tilde", "~/.claude.json", "/home/john/.claude.json"},
		{"expands tilde subdir", "~/.claude-work/.claude.json", "/home/john/.claude-work/.claude.json"},
		{"leaves absolute path unchanged", "/etc/claude/.claude.json", "/etc/claude/.claude.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := NewJob(
				"personal", NewClient(), nil, "/home/john",
				CredentialsSource{Kind: "keychain"}, tt.claudeConfigPath, time.Minute,
			)
			assert.Equal(t, tt.want, job.ClaudeConfigPath)
		})
	}
}

// TestJobRun_ResolvesTildeClaudeConfigPathEndToEnd proves the fix at the
// Run() level, not just the path-construction level: a job configured
// exactly like the documented example (a "~/..."-prefixed claude_config)
// must successfully read identity and store rows.
func TestJobRun_ResolvesTildeClaudeConfigPathEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"five_hour": {"utilization": 5, "resets_at": 1},
			"seven_day": null, "seven_day_opus": null, "seven_day_sonnet": null,
			"seven_day_oauth_apps": null, "seven_day_overage_included": null}`))
	}))
	t.Cleanup(srv.Close)

	homeDir := t.TempDir()
	writeClaudeConfig(t, homeDir, "acct-tilde-1", "Tilde Org")
	t.Setenv("AGENTSVIEW_TEST_TILDE_TOKEN", "test-token")
	database := dbtest.OpenTestDB(t)

	job := NewJob(
		"personal", &Client{BaseURL: srv.URL, HTTPClient: http.DefaultClient}, database, homeDir,
		CredentialsSource{Kind: "env", Value: "AGENTSVIEW_TEST_TILDE_TOKEN"}, "~/.claude.json", time.Minute,
	)
	require.NoError(t, job.Run(context.Background()))

	rows, err := database.RateLimitSnapshotHistory(
		context.Background(), db.RateLimitHistoryFilter{Vendor: "claude", AccountID: "acct-tilde-1"},
	)
	require.NoError(t, err)
	assert.NotEmpty(t, rows)
}

func TestJobRun_200StoresSnapshotsForConfiguredAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"five_hour": {"utilization": 55, "resets_at": 1789435448},
			"seven_day": {"utilization": 12.5, "resets_at": 1789999999},
			"seven_day_opus": null, "seven_day_sonnet": null,
			"seven_day_oauth_apps": null, "seven_day_overage_included": null
		}`))
	}))
	t.Cleanup(srv.Close)

	database := dbtest.OpenTestDB(t)
	job := newTestJob(t, srv.URL, database)

	require.NoError(t, job.Run(context.Background()))

	rows, err := database.RateLimitSnapshotHistory(
		context.Background(), db.RateLimitHistoryFilter{Vendor: "claude", AccountID: "acct-uuid-1"},
	)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	byWindow := map[string]db.RateLimitSnapshot{}
	for _, r := range rows {
		byWindow[r.WindowKind] = r
	}
	require.Contains(t, byWindow, "session")
	require.Contains(t, byWindow, "weekly")
	assert.InDelta(t, 55.0, byWindow["session"].UsedPercent, 0.001)
	assert.Equal(t, "claude", byWindow["session"].Vendor)
	assert.Equal(t, "Acme", byWindow["session"].AccountLabel)
}

// TestJobRun_CombinesWithStatuslineObservationsWithoutDuplicating covers
// roborev finding #1 on kata j5md: the oauth/usage poller (this test's
// job.Run) and the statusLine sink (cmd/agentsview's `claude
// statusline-sink`, mimicked here by inserting a row the same way that
// sink does: WindowKind "session"/"weekly", the same AccountID/
// AccountLabel, no scope/severity/details) must observe the same account
// under the identical window_kind identity, so LatestRateLimitSnapshots
// -- which is what the Usage page's cards render from -- returns exactly
// one row per window rather than one poller-sourced card and one
// stale/duplicate statusline-sourced card sitting side by side.
func TestJobRun_CombinesWithStatuslineObservationsWithoutDuplicating(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"five_hour": {"utilization": 55, "resets_at": 1789435448},
			"seven_day": {"utilization": 12.5, "resets_at": 1789999999},
			"seven_day_opus": null, "seven_day_sonnet": null,
			"seven_day_oauth_apps": null, "seven_day_overage_included": null
		}`))
	}))
	t.Cleanup(srv.Close)

	database := dbtest.OpenTestDB(t)
	job := newTestJob(t, srv.URL, database)
	// Pinned before the statusline observations below (2026-09-11):
	// this test asserts the poller's own observed_at loses to the
	// hardcoded "later" statusline timestamps, which requires the
	// poller's observed_at to actually be earlier. Using the real clock
	// here made the test flip once wall-clock time passed those
	// hardcoded timestamps (roborev finding on kata ztf4).
	job.now = func() time.Time { return time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC) }
	require.NoError(t, job.Run(context.Background()))

	// A later statusLine observation for the same account and the same
	// two windows, distinct enough (a later observed_at/resets_at) not
	// to dedup-collapse into the poller's rows, mirroring
	// runClaudeStatuslineSink's own row construction in
	// cmd/agentsview/claude.go.
	require.NoError(t, database.InsertRateLimitSnapshots([]db.RateLimitSnapshot{
		{
			Vendor:       "claude",
			AccountID:    "acct-uuid-1",
			AccountLabel: "Acme",
			WindowKind:   "session",
			UsedPercent:  60,
			ResetsAt:     new(int64(1789435460)),
			ObservedAt:   "2026-09-11T11:00:00Z",
		},
		{
			Vendor:       "claude",
			AccountID:    "acct-uuid-1",
			AccountLabel: "Acme",
			WindowKind:   "weekly",
			UsedPercent:  20,
			ResetsAt:     new(int64(1790000000)),
			ObservedAt:   "2026-09-11T11:00:00Z",
		},
	}))

	current, err := database.LatestRateLimitSnapshots(
		context.Background(), db.RateLimitFilter{Vendor: "claude", AccountID: "acct-uuid-1"},
	)
	require.NoError(t, err)
	require.Len(t, current, 2, "one card per window, not one per source")

	byWindow := map[string]db.RateLimitSnapshot{}
	for _, r := range current {
		byWindow[r.WindowKind] = r
	}
	require.Contains(t, byWindow, "session")
	require.Contains(t, byWindow, "weekly")
	assert.InDelta(t, 60.0, byWindow["session"].UsedPercent, 0.001,
		"the more recent statusline observation must win, proving the two sources share one identity")
	assert.InDelta(t, 20.0, byWindow["weekly"].UsedPercent, 0.001)
}

// TestJobRun_SwitchingOrgsWithSameAccountUUIDKeepsBothOrgsRateLimitCards
// covers kata 1k4b end to end: logging out of `claude` and back into a
// different organization on the same account rewrites ~/.claude.json in
// place with the same accountUuid and a new organizationUuid/
// organizationName. Before the fix, AccountID keyed on the bare
// accountUuid, so the second org's poll landed in the same
// LatestRateLimitSnapshots group as the first and silently overwrote its
// cards. This proves both orgs now survive as distinct account groups.
func TestJobRun_SwitchingOrgsWithSameAccountUUIDKeepsBothOrgsRateLimitCards(t *testing.T) {
	srvOrgA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"five_hour": {"utilization": 30, "resets_at": 1789435448},
			"seven_day": {"utilization": 10, "resets_at": 1789999999},
			"seven_day_opus": null, "seven_day_sonnet": null,
			"seven_day_oauth_apps": null, "seven_day_overage_included": null}`))
	}))
	t.Cleanup(srvOrgA.Close)
	srvOrgB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"five_hour": {"utilization": 70, "resets_at": 1789435500},
			"seven_day": {"utilization": 90, "resets_at": 1790000050},
			"seven_day_opus": null, "seven_day_sonnet": null,
			"seven_day_oauth_apps": null, "seven_day_overage_included": null}`))
	}))
	t.Cleanup(srvOrgB.Close)

	database := dbtest.OpenTestDB(t)
	dir := t.TempDir()
	t.Setenv("AGENTSVIEW_TEST_ORGSWITCH_TOKEN", "test-token")

	// Same accountUuid, same on-disk .claude.json path (a real org
	// switch rewrites the file in place), different organizationUuid --
	// mirroring `claude` logout/login into a different org.
	configPath := writeClaudeConfigWithOrg(t, dir, "acct-shared-1", "org-a-uuid", "Org A")
	jobOrgA := NewJob(
		"personal", &Client{BaseURL: srvOrgA.URL, HTTPClient: http.DefaultClient}, database, dir,
		CredentialsSource{Kind: "env", Value: "AGENTSVIEW_TEST_ORGSWITCH_TOKEN"}, configPath, time.Minute,
	)
	require.NoError(t, jobOrgA.Run(context.Background()))

	writeClaudeConfigWithOrg(t, dir, "acct-shared-1", "org-b-uuid", "Org B")
	jobOrgB := NewJob(
		"personal", &Client{BaseURL: srvOrgB.URL, HTTPClient: http.DefaultClient}, database, dir,
		CredentialsSource{Kind: "env", Value: "AGENTSVIEW_TEST_ORGSWITCH_TOKEN"}, configPath, time.Minute,
	)
	require.NoError(t, jobOrgB.Run(context.Background()))

	rows, err := database.LatestRateLimitSnapshots(
		context.Background(), db.RateLimitFilter{Vendor: "claude"},
	)
	require.NoError(t, err)
	require.Len(t, rows, 4, "both orgs' session and weekly windows survive as distinct groups")

	byAccountAndWindow := map[string]db.RateLimitSnapshot{}
	for _, r := range rows {
		byAccountAndWindow[r.AccountID+" "+r.WindowKind] = r
	}
	require.Contains(t, byAccountAndWindow, "acct-shared-1/org-a-uuid session")
	require.Contains(t, byAccountAndWindow, "acct-shared-1/org-b-uuid session")
	orgA := byAccountAndWindow["acct-shared-1/org-a-uuid session"]
	orgB := byAccountAndWindow["acct-shared-1/org-b-uuid session"]
	assert.Equal(t, "Org A", orgA.AccountLabel)
	assert.Equal(t, "Org B", orgB.AccountLabel)
	assert.InDelta(t, 30.0, orgA.UsedPercent, 0.001, "org A's card must not be overwritten by org B's poll")
	assert.InDelta(t, 70.0, orgB.UsedPercent, 0.001)
}

// TestJobRun_SwitchingOrgsOnSameJobDoesNotCrossContaminateLimitsArrayState
// covers a roborev finding: sawLimitsArray/usedFallback must be keyed by
// account identity, not by Job instance, since the same configured
// account (and the same running Job, reused across every poll by the
// scheduler) can poll a different org from one run to the next -- kata
// 1k4b, a real `claude` logout/login into a different org rewriting
// ~/.claude.json in place. Before this fix, a single Job-wide flag meant
// org A reporting `limits` would silently suppress org B's fixed-bucket
// windows forever, even though org B never reported `limits` at all.
func TestJobRun_SwitchingOrgsOnSameJobDoesNotCrossContaminateLimitsArrayState(t *testing.T) {
	limitsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(realOauthUsageLimitsArrayFixture))
	}))
	t.Cleanup(limitsSrv.Close)
	fixedBucketSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"five_hour": {"utilization": 25, "resets_at": 1789435448},
			"seven_day": null, "seven_day_opus": null, "seven_day_sonnet": null,
			"seven_day_oauth_apps": null, "seven_day_overage_included": null}`))
	}))
	t.Cleanup(fixedBucketSrv.Close)

	database := dbtest.OpenTestDB(t)
	dir := t.TempDir()
	t.Setenv("AGENTSVIEW_TEST_ORGSWITCH_TOKEN", "test-token")

	// One Job instance, polled twice against two different orgs -- the
	// same reuse pattern the scheduler applies to a single configured
	// account across its whole lifetime.
	configPath := writeClaudeConfigWithOrg(t, dir, "acct-shared-1", "org-a-uuid", "Org A")
	job := NewJob(
		"personal", &Client{BaseURL: limitsSrv.URL, HTTPClient: http.DefaultClient}, database, dir,
		CredentialsSource{Kind: "env", Value: "AGENTSVIEW_TEST_ORGSWITCH_TOKEN"}, configPath, time.Minute,
	)
	require.NoError(t, job.Run(context.Background()))

	writeClaudeConfigWithOrg(t, dir, "acct-shared-1", "org-b-uuid", "Org B")
	job.Client = &Client{BaseURL: fixedBucketSrv.URL, HTTPClient: http.DefaultClient}
	require.NoError(t, job.Run(context.Background()))

	rows, err := database.LatestRateLimitSnapshots(context.Background(), db.RateLimitFilter{Vendor: "claude"})
	require.NoError(t, err)

	byAccount := map[string][]db.RateLimitSnapshot{}
	for _, r := range rows {
		byAccount[r.AccountID] = append(byAccount[r.AccountID], r)
	}
	require.Contains(t, byAccount, "acct-shared-1/org-a-uuid")
	require.Contains(t, byAccount, "acct-shared-1/org-b-uuid")

	orgBWindows := map[string]bool{}
	for _, r := range byAccount["acct-shared-1/org-b-uuid"] {
		orgBWindows[r.WindowKind] = true
	}
	assert.True(t, orgBWindows["session"],
		"org B's fixed-bucket window must not be suppressed by org A's limits-array state")
}

// TestJobRun_TransitionFromFixedBucketsToLimitsRetiresFallbackOnlyWindows
// covers a roborev finding: if an account's first poll uses the
// fixed-bucket fallback and a later poll supplies the `limits` array
// instead, the fallback-only windows (claudeFixedBucketOnlyWindowKinds)
// must stop appearing in current results. Each resolves its own latest
// observation independently of every other Claude window, so nothing
// would otherwise ever supersede a fallback-only row once `limits`
// becomes the source -- the scoped cap it described now lives under a
// `weekly_scoped:<model>` window_kind instead, and both would show up as
// separate cards with the fallback one permanently stale.
func TestJobRun_TransitionFromFixedBucketsToLimitsRetiresFallbackOnlyWindows(t *testing.T) {
	var useLimits atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if useLimits.Load() {
			_, _ = w.Write([]byte(realOauthUsageLimitsArrayFixture))
			return
		}
		_, _ = w.Write([]byte(`{"five_hour": {"utilization": 15, "resets_at": 1789435448},
			"seven_day": null, "seven_day_opus": null, "seven_day_sonnet": null,
			"seven_day_oauth_apps": null,
			"seven_day_overage_included": {"utilization": 60, "resets_at": 1789600000}}`))
	}))
	t.Cleanup(srv.Close)

	database := dbtest.OpenTestDB(t)
	job := newTestJob(t, srv.URL, database)
	job.now = func() time.Time { return time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC) }
	require.NoError(t, job.Run(context.Background()))

	before, err := database.LatestRateLimitSnapshots(
		context.Background(), db.RateLimitFilter{Vendor: "claude", AccountID: "acct-uuid-1"},
	)
	require.NoError(t, err)
	windowsBefore := map[string]bool{}
	for _, r := range before {
		windowsBefore[r.WindowKind] = true
	}
	require.True(t, windowsBefore["seven_day_overage_included"],
		"the fixed-bucket-only window must show up while the fallback is the source")

	// A later poll (a different minute) supplies the `limits` array.
	useLimits.Store(true)
	job.now = func() time.Time { return time.Date(2026, 9, 9, 10, 5, 0, 0, time.UTC) }
	require.NoError(t, job.Run(context.Background()))

	after, err := database.LatestRateLimitSnapshots(
		context.Background(), db.RateLimitFilter{Vendor: "claude", AccountID: "acct-uuid-1"},
	)
	require.NoError(t, err)
	windowsAfter := map[string]bool{}
	for _, r := range after {
		windowsAfter[r.WindowKind] = true
	}
	assert.False(t, windowsAfter["seven_day_overage_included"],
		"the fallback-only window must be retired once limits takes over")
	assert.True(t, windowsAfter["weekly_scoped:Fable"],
		"the limits-array window must be present")

	history, err := database.RateLimitSnapshotHistory(
		context.Background(),
		db.RateLimitHistoryFilter{
			Vendor: "claude", AccountID: "acct-uuid-1", WindowKind: "seven_day_overage_included",
		},
	)
	require.NoError(t, err)
	assert.Len(t, history, 2, "history keeps both the active and the retirement observation")
}

// TestJobRun_RetiresFallbackOnlyWindowsAcrossRestart covers a roborev
// finding: retiring fallback-only windows must not depend on in-memory
// state that resets across a daemon restart. This test uses a second,
// freshly constructed Job sharing the same database (simulating a
// restart between the fallback poll and the limits-array poll) rather
// than reusing the first Job instance, so it would fail if retirement
// were gated on a process-lifetime flag instead of the persisted rows
// db.LatestRateLimitSnapshots already excludes once retired.
func TestJobRun_RetiresFallbackOnlyWindowsAcrossRestart(t *testing.T) {
	fallbackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"five_hour": {"utilization": 15, "resets_at": 1789435448},
			"seven_day": null, "seven_day_opus": null, "seven_day_sonnet": null,
			"seven_day_oauth_apps": null,
			"seven_day_overage_included": {"utilization": 60, "resets_at": 1789600000}}`))
	}))
	t.Cleanup(fallbackSrv.Close)
	limitsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(realOauthUsageLimitsArrayFixture))
	}))
	t.Cleanup(limitsSrv.Close)

	database := dbtest.OpenTestDB(t)
	firstRun := newTestJob(t, fallbackSrv.URL, database)
	firstRun.now = func() time.Time { return time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC) }
	require.NoError(t, firstRun.Run(context.Background()))

	before, err := database.LatestRateLimitSnapshots(
		context.Background(), db.RateLimitFilter{Vendor: "claude", AccountID: "acct-uuid-1"},
	)
	require.NoError(t, err)
	windowsBefore := map[string]bool{}
	for _, r := range before {
		windowsBefore[r.WindowKind] = true
	}
	require.True(t, windowsBefore["seven_day_overage_included"])

	// A brand-new Job instance against the same database, as if the
	// daemon had restarted between polls: its sawLimitsArray map starts
	// empty, unaware this identity ever used the fallback.
	afterRestart := newTestJob(t, limitsSrv.URL, database)
	afterRestart.now = func() time.Time { return time.Date(2026, 9, 9, 10, 5, 0, 0, time.UTC) }
	require.NoError(t, afterRestart.Run(context.Background()))

	after, err := database.LatestRateLimitSnapshots(
		context.Background(), db.RateLimitFilter{Vendor: "claude", AccountID: "acct-uuid-1"},
	)
	require.NoError(t, err)
	windowsAfter := map[string]bool{}
	for _, r := range after {
		windowsAfter[r.WindowKind] = true
	}
	assert.False(t, windowsAfter["seven_day_overage_included"],
		"the fallback-only window must still be retired despite the restart")
	assert.True(t, windowsAfter["weekly_scoped:Fable"])
}

// TestJobRun_RetiresScopedLimitsWindowRemovedFromLaterPoll covers a
// roborev finding: retirement previously handled only the four
// fixed-bucket-only window kinds, so a `limits`-sourced scoped window
// (e.g. a model-scoped weekly cap) that stops appearing in a later
// poll's `limits` array -- the cap expired, or the account no longer
// has it -- was left showing forever, since Claude windows resolve
// their own latest observation independently and nothing else would
// ever supersede it. The `limits` array is a complete snapshot on every
// poll, so anything current but missing from it must be retired.
func TestJobRun_RetiresScopedLimitsWindowRemovedFromLaterPoll(t *testing.T) {
	withFable := realOauthUsageLimitsArrayFixture
	withoutFable := strings.Replace(realOauthUsageLimitsArrayFixture,
		`,
		{"kind": "weekly_scoped", "group": "weekly", "percent": 5, "severity": "normal", "resets_at": "2026-09-16T22:00:00.010841+00:00", "scope": {"model": {"id": null, "display_name": "Fable"}, "surface": null}, "is_active": false}`,
		"", 1,
	)
	require.NotEqual(t, withFable, withoutFable, "the replacement must actually remove the scoped entry")

	var fableRemoved atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fableRemoved.Load() {
			_, _ = w.Write([]byte(withoutFable))
			return
		}
		_, _ = w.Write([]byte(withFable))
	}))
	t.Cleanup(srv.Close)

	database := dbtest.OpenTestDB(t)
	job := newTestJob(t, srv.URL, database)
	job.now = func() time.Time { return time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC) }
	require.NoError(t, job.Run(context.Background()))

	before, err := database.LatestRateLimitSnapshots(
		context.Background(), db.RateLimitFilter{Vendor: "claude", AccountID: "acct-uuid-1"},
	)
	require.NoError(t, err)
	windowsBefore := map[string]bool{}
	for _, r := range before {
		windowsBefore[r.WindowKind] = true
	}
	require.True(t, windowsBefore["weekly_scoped:Fable"])

	fableRemoved.Store(true)
	job.now = func() time.Time { return time.Date(2026, 9, 9, 10, 5, 0, 0, time.UTC) }
	require.NoError(t, job.Run(context.Background()))

	after, err := database.LatestRateLimitSnapshots(
		context.Background(), db.RateLimitFilter{Vendor: "claude", AccountID: "acct-uuid-1"},
	)
	require.NoError(t, err)
	windowsAfter := map[string]bool{}
	for _, r := range after {
		windowsAfter[r.WindowKind] = true
	}
	assert.False(t, windowsAfter["weekly_scoped:Fable"],
		"a scoped limit no longer reported by limits must be retired")
	assert.True(t, windowsAfter["session"], "an unrelated account-wide window is unaffected")
	assert.True(t, windowsAfter["weekly"], "an unrelated account-wide window is unaffected")

	history, err := database.RateLimitSnapshotHistory(
		context.Background(),
		db.RateLimitHistoryFilter{Vendor: "claude", AccountID: "acct-uuid-1", WindowKind: "weekly_scoped:Fable"},
	)
	require.NoError(t, err)
	assert.Len(t, history, 2, "history keeps both the active and the retirement observation")
}

// TestJobRun_UnparsableResetsAtDoesNotFalselyRetireItsOwnWindow covers a
// roborev finding: an entry's window_kind must be registered as "fresh"
// before parsing its resets_at, not after -- registering it only on
// successful parse meant an entry present in this poll's `limits` array
// but with an unparsable resets_at (a future, differently-shaped
// code-named entry) was invisible to retireStaleClaudeWindows, which
// then treated the window as missing from `limits` and wrote a false
// retirement for a window that was, in fact, still being reported.
func TestJobRun_UnparsableResetsAtDoesNotFalselyRetireItsOwnWindow(t *testing.T) {
	validResetsAt := `"resets_at": "2026-09-16T22:00:00.010841+00:00"`
	malformedResetsAt := `"resets_at": {}`
	malformed := strings.Replace(realOauthUsageLimitsArrayFixture, validResetsAt, malformedResetsAt, 1)
	require.NotEqual(t, realOauthUsageLimitsArrayFixture, malformed,
		"the replacement must actually corrupt the scoped entry's resets_at")

	var corrupt atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if corrupt.Load() {
			_, _ = w.Write([]byte(malformed))
			return
		}
		_, _ = w.Write([]byte(realOauthUsageLimitsArrayFixture))
	}))
	t.Cleanup(srv.Close)

	database := dbtest.OpenTestDB(t)
	job := newTestJob(t, srv.URL, database)
	job.now = func() time.Time { return time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC) }
	require.NoError(t, job.Run(context.Background()))

	before, err := database.LatestRateLimitSnapshots(
		context.Background(), db.RateLimitFilter{Vendor: "claude", AccountID: "acct-uuid-1"},
	)
	require.NoError(t, err)
	windowsBefore := map[string]bool{}
	for _, r := range before {
		windowsBefore[r.WindowKind] = true
	}
	require.True(t, windowsBefore["weekly_scoped:Fable"])

	// A later poll still reports the scoped entry, but its resets_at
	// this time fails to parse.
	corrupt.Store(true)
	job.now = func() time.Time { return time.Date(2026, 9, 9, 10, 5, 0, 0, time.UTC) }
	require.NoError(t, job.Run(context.Background()))

	after, err := database.LatestRateLimitSnapshots(
		context.Background(), db.RateLimitFilter{Vendor: "claude", AccountID: "acct-uuid-1"},
	)
	require.NoError(t, err)
	windowsAfter := map[string]bool{}
	for _, r := range after {
		windowsAfter[r.WindowKind] = true
	}
	assert.True(t, windowsAfter["weekly_scoped:Fable"],
		"a window still reported by limits must not be falsely retired just because its own resets_at failed to parse")
}

func TestJobRun_200WithNoWindowsIsANoOp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"five_hour": null, "seven_day": null, "seven_day_opus": null,
			"seven_day_sonnet": null, "seven_day_oauth_apps": null,
			"seven_day_overage_included": null
		}`))
	}))
	t.Cleanup(srv.Close)

	database := dbtest.OpenTestDB(t)
	job := newTestJob(t, srv.URL, database)
	require.NoError(t, job.Run(context.Background()))

	rows, err := database.RateLimitSnapshotHistory(
		context.Background(), db.RateLimitHistoryFilter{Vendor: "claude"},
	)
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestJobRun_DedupsOnRepeatedPoll(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"five_hour": {"utilization": 10, "resets_at": 1789435448},
			"seven_day": null, "seven_day_opus": null, "seven_day_sonnet": null,
			"seven_day_oauth_apps": null, "seven_day_overage_included": null}`))
	}))
	t.Cleanup(srv.Close)

	database := dbtest.OpenTestDB(t)
	job := newTestJob(t, srv.URL, database)
	fixedNow := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	job.now = func() time.Time { return fixedNow }

	require.NoError(t, job.Run(context.Background()))
	require.NoError(t, job.Run(context.Background()))
	assert.Equal(t, int32(2), requests.Load(), "the job polls the endpoint every run")

	rows, err := database.RateLimitSnapshotHistory(
		context.Background(), db.RateLimitHistoryFilter{Vendor: "claude"},
	)
	require.NoError(t, err)
	assert.Len(t, rows, 1, "two polls within the same minute must dedup to one row")
}

func TestJobRun_401ReturnsUnauthorizedWithoutStoring(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	database := dbtest.OpenTestDB(t)
	job := newTestJob(t, srv.URL, database)

	err := job.Run(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnauthorized)

	rows, err := database.RateLimitSnapshotHistory(
		context.Background(), db.RateLimitHistoryFilter{Vendor: "claude"},
	)
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestJobRun_429ReturnsSchedulerRetryAfterError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "45")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	database := dbtest.OpenTestDB(t)
	job := newTestJob(t, srv.URL, database)

	err := job.Run(context.Background())
	require.Error(t, err)
	var retryAfter *poller.RetryAfterError
	require.True(t, errors.As(err, &retryAfter), "expected a poller.RetryAfterError, got %T: %v", err, err)
	assert.Equal(t, 45*time.Second, retryAfter.RetryAfter)
}

func TestJobRun_MissingIdentityFailsWithoutStoring(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"five_hour": {"utilization": 10, "resets_at": 1}}`))
	}))
	t.Cleanup(srv.Close)

	database := dbtest.OpenTestDB(t)
	dir := t.TempDir()
	// A .claude.json with no oauthAccount at all (never logged in).
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(`{}`), 0o600))
	t.Setenv("AGENTSVIEW_TEST_JOB_TOKEN_2", "test-token")

	job := NewJob(
		"personal", &Client{BaseURL: srv.URL, HTTPClient: http.DefaultClient}, database, dir,
		CredentialsSource{Kind: "env", Value: "AGENTSVIEW_TEST_JOB_TOKEN_2"}, "", time.Minute,
	)
	err := job.Run(context.Background())
	require.Error(t, err)

	rows, err := database.RateLimitSnapshotHistory(
		context.Background(), db.RateLimitHistoryFilter{Vendor: "claude"},
	)
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestJobName(t *testing.T) {
	assert.Equal(t, "claude-usage:personal", JobName("personal"))
}

// TestJobRun_UsesLimitsArrayAsPrimarySourceAndPersistsExtraUsage covers
// kata eabb end to end: a live-shaped response (the fixed
// seven_day_opus/seven_day_sonnet buckets null, the model-scoped weekly
// "Fable" cap only inside `limits`, and extra_usage/spend present) must
// produce a "weekly_scoped:Fable" row with its scope label and severity,
// and an "extra_usage_monthly" row with the monetary limit in Details.
func TestJobRun_UsesLimitsArrayAsPrimarySourceAndPersistsExtraUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(realOauthUsageLimitsArrayFixture))
	}))
	t.Cleanup(srv.Close)

	database := dbtest.OpenTestDB(t)
	job := newTestJob(t, srv.URL, database)

	require.NoError(t, job.Run(context.Background()))

	rows, err := database.RateLimitSnapshotHistory(
		context.Background(), db.RateLimitHistoryFilter{Vendor: "claude", AccountID: "acct-uuid-1"},
	)
	require.NoError(t, err)

	byWindow := map[string]db.RateLimitSnapshot{}
	for _, r := range rows {
		byWindow[r.WindowKind] = r
	}

	// The fixed seven_day_opus/seven_day_sonnet buckets are null in the
	// fixture and must not appear -- their real data lives only in
	// `limits` under weekly_scoped, which must be used instead.
	assert.NotContains(t, byWindow, "seven_day_opus")
	assert.NotContains(t, byWindow, "seven_day_sonnet")

	require.Contains(t, byWindow, "session")
	require.Contains(t, byWindow, "weekly")
	require.Contains(t, byWindow, "weekly_scoped:Fable")
	scoped := byWindow["weekly_scoped:Fable"]
	assert.InDelta(t, 5, scoped.UsedPercent, 0.001)
	assert.Equal(t, "Fable", scoped.ScopeLabel)
	assert.Equal(t, "normal", scoped.Severity)
	assert.Equal(t, 10080, scoped.WindowMinutes)
	assert.Contains(t, scoped.Details, `"source":"limits"`)
	assert.Contains(t, scoped.Details, "Fable")

	require.Contains(t, byWindow, "extra_usage_monthly")
	extraUsage := byWindow["extra_usage_monthly"]
	assert.InDelta(t, 96.14, extraUsage.UsedPercent, 0.001)
	assert.Contains(t, extraUsage.Details, `"source":"extra_usage"`)
	assert.Contains(t, extraUsage.Details, `"monthly_limit_minor":110000`)
	assert.Contains(t, extraUsage.Details, `"currency":"USD"`)
}

// TestJobRun_DisablingExtraUsageHidesItFromCurrentButKeepsHistory covers
// a roborev finding: extra_usage_monthly resolves its own "latest"
// observation independently of every other Claude window (see
// bucket_window_kind), so a poll that stops reporting it because the
// account turned it off would otherwise leave its last-enabled row
// showing forever on the Usage page, with nothing to ever supersede it.
// The account turning the feature back off must retire the card from
// current results on the very next poll, while the history endpoint
// (used for the card's own chart) still returns every row so a chart
// spanning the disable event shows the transition rather than an
// unexplained gap.
func TestJobRun_DisablingExtraUsageHidesItFromCurrentButKeepsHistory(t *testing.T) {
	var enabled atomic.Bool
	enabled.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if enabled.Load() {
			_, _ = w.Write([]byte(`{
				"five_hour": {"utilization": 10, "resets_at": 1789435448},
				"seven_day": null, "seven_day_opus": null, "seven_day_sonnet": null,
				"seven_day_oauth_apps": null, "seven_day_overage_included": null,
				"extra_usage": {"is_enabled": true, "utilization": 40, "spend_limit_reached": false}
			}`))
			return
		}
		_, _ = w.Write([]byte(`{
			"five_hour": {"utilization": 15, "resets_at": 1789435448},
			"seven_day": null, "seven_day_opus": null, "seven_day_sonnet": null,
			"seven_day_oauth_apps": null, "seven_day_overage_included": null,
			"extra_usage": {"is_enabled": false, "utilization": 0, "spend_limit_reached": false}
		}`))
	}))
	t.Cleanup(srv.Close)

	database := dbtest.OpenTestDB(t)
	job := newTestJob(t, srv.URL, database)
	job.now = func() time.Time { return time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC) }
	require.NoError(t, job.Run(context.Background()))

	current, err := database.LatestRateLimitSnapshots(
		context.Background(), db.RateLimitFilter{Vendor: "claude", AccountID: "acct-uuid-1"},
	)
	require.NoError(t, err)
	byWindow := map[string]db.RateLimitSnapshot{}
	for _, r := range current {
		byWindow[r.WindowKind] = r
	}
	require.Contains(t, byWindow, "extra_usage_monthly", "an enabled extra_usage window shows up as usual")
	assert.InDelta(t, 40.0, byWindow["extra_usage_monthly"].UsedPercent, 0.001)

	// A later poll (a different minute, so its rows do not dedup away)
	// reports the account has turned extra_usage off.
	enabled.Store(false)
	job.now = func() time.Time { return time.Date(2026, 9, 9, 10, 5, 0, 0, time.UTC) }
	require.NoError(t, job.Run(context.Background()))

	current, err = database.LatestRateLimitSnapshots(
		context.Background(), db.RateLimitFilter{Vendor: "claude", AccountID: "acct-uuid-1"},
	)
	require.NoError(t, err)
	byWindow = map[string]db.RateLimitSnapshot{}
	for _, r := range current {
		byWindow[r.WindowKind] = r
	}
	assert.NotContains(t, byWindow, "extra_usage_monthly",
		"a disabled extra_usage window must not keep showing a stale monthly allowance")
	require.Contains(t, byWindow, "session")
	assert.InDelta(t, 15.0, byWindow["session"].UsedPercent, 0.001,
		"the account's other windows keep updating normally")

	history, err := database.RateLimitSnapshotHistory(
		context.Background(),
		db.RateLimitHistoryFilter{Vendor: "claude", AccountID: "acct-uuid-1", WindowKind: "extra_usage_monthly"},
	)
	require.NoError(t, err)
	require.Len(t, history, 2, "history keeps both the enabled and the disabled observation")
	assert.InDelta(t, 40.0, history[0].UsedPercent, 0.001)
	assert.InDelta(t, 0.0, history[1].UsedPercent, 0.001)
}

// TestJobRun_OmittingExtraUsageObjectEntirelyRetiresIt covers a roborev
// finding distinct from the disabled-object case above: a later poll can
// drop the `extra_usage` field from its response entirely (usage.ExtraUsage
// == nil), not just report it with is_enabled false. claudeRateLimitSnapshotsFromUsage
// only ever writes the window when the object is present, so a poll that
// omits it wrote nothing at all for extra_usage_monthly -- leaving the
// previous enabled row as "latest" forever, with nothing to ever
// supersede it, since the window resolves its own latest observation
// independently of every other Claude window.
func TestJobRun_OmittingExtraUsageObjectEntirelyRetiresIt(t *testing.T) {
	var withExtraUsage atomic.Bool
	withExtraUsage.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if withExtraUsage.Load() {
			_, _ = w.Write([]byte(`{
				"five_hour": {"utilization": 10, "resets_at": 1789435448},
				"seven_day": null, "seven_day_opus": null, "seven_day_sonnet": null,
				"seven_day_oauth_apps": null, "seven_day_overage_included": null,
				"extra_usage": {"is_enabled": true, "utilization": 40, "spend_limit_reached": false}
			}`))
			return
		}
		_, _ = w.Write([]byte(`{
			"five_hour": {"utilization": 15, "resets_at": 1789435448},
			"seven_day": null, "seven_day_opus": null, "seven_day_sonnet": null,
			"seven_day_oauth_apps": null, "seven_day_overage_included": null
		}`))
	}))
	t.Cleanup(srv.Close)

	database := dbtest.OpenTestDB(t)
	job := newTestJob(t, srv.URL, database)
	job.now = func() time.Time { return time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC) }
	require.NoError(t, job.Run(context.Background()))

	current, err := database.LatestRateLimitSnapshots(
		context.Background(), db.RateLimitFilter{Vendor: "claude", AccountID: "acct-uuid-1"},
	)
	require.NoError(t, err)
	byWindow := map[string]db.RateLimitSnapshot{}
	for _, r := range current {
		byWindow[r.WindowKind] = r
	}
	require.Contains(t, byWindow, "extra_usage_monthly", "an enabled extra_usage window shows up as usual")

	// A later poll's response drops the extra_usage field entirely,
	// rather than reporting it disabled.
	withExtraUsage.Store(false)
	job.now = func() time.Time { return time.Date(2026, 9, 9, 10, 5, 0, 0, time.UTC) }
	require.NoError(t, job.Run(context.Background()))

	current, err = database.LatestRateLimitSnapshots(
		context.Background(), db.RateLimitFilter{Vendor: "claude", AccountID: "acct-uuid-1"},
	)
	require.NoError(t, err)
	byWindow = map[string]db.RateLimitSnapshot{}
	for _, r := range current {
		byWindow[r.WindowKind] = r
	}
	assert.NotContains(t, byWindow, "extra_usage_monthly",
		"the window must retire when a later response omits extra_usage entirely, not just when it reports disabled")

	history, err := database.RateLimitSnapshotHistory(
		context.Background(),
		db.RateLimitHistoryFilter{Vendor: "claude", AccountID: "acct-uuid-1", WindowKind: "extra_usage_monthly"},
	)
	require.NoError(t, err)
	require.Len(t, history, 2, "history keeps both the enabled observation and the retirement")
	assert.InDelta(t, 40.0, history[0].UsedPercent, 0.001)
}

// TestJobRun_FallsBackToFixedBucketsWhenLimitsIsEmpty proves the
// fallback path still fires when a response has no `limits` array at
// all (matching every pre-eabb test fixture and any future response
// that drops the array again).
func TestJobRun_FallsBackToFixedBucketsWhenLimitsIsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"five_hour": {"utilization": 12, "resets_at": 1789435448},
			"seven_day": null, "seven_day_opus": null, "seven_day_sonnet": null,
			"seven_day_oauth_apps": null, "seven_day_overage_included": null,
			"limits": []
		}`))
	}))
	t.Cleanup(srv.Close)

	database := dbtest.OpenTestDB(t)
	job := newTestJob(t, srv.URL, database)
	require.NoError(t, job.Run(context.Background()))

	rows, err := database.RateLimitSnapshotHistory(
		context.Background(), db.RateLimitHistoryFilter{Vendor: "claude", AccountID: "acct-uuid-1"},
	)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "session", rows[0].WindowKind)
	assert.Contains(t, rows[0].Details, `"source":"fixed_buckets"`)
}

// TestJobRun_RetiresFixedBucketWindowThatGoesNullOnALaterPoll covers a
// roborev finding: for an account that never reports `limits` at all,
// stale-window retirement previously ran only when usage.HasLimits() was
// true, so a fixed bucket (e.g. seven_day_opus) that was populated on
// one poll and null on a later one was never retired -- Windows()
// silently omits a null bucket rather than reporting it as zero, and
// since LatestRateLimitSnapshots resolves each Claude window_kind's
// latest observation independently, nothing else would ever supersede
// the old value. It must be retired the same way a `limits`-sourced
// window already is when it disappears from a later poll.
func TestJobRun_RetiresFixedBucketWindowThatGoesNullOnALaterPoll(t *testing.T) {
	var opusNulled atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if opusNulled.Load() {
			_, _ = w.Write([]byte(`{
				"five_hour": {"utilization": 12, "resets_at": 1789435448},
				"seven_day": null, "seven_day_opus": null, "seven_day_sonnet": null,
				"seven_day_oauth_apps": null, "seven_day_overage_included": null,
				"limits": []
			}`))
			return
		}
		_, _ = w.Write([]byte(`{
			"five_hour": {"utilization": 12, "resets_at": 1789435448},
			"seven_day": null, "seven_day_opus": {"utilization": 40, "resets_at": 1789999999},
			"seven_day_sonnet": null,
			"seven_day_oauth_apps": null, "seven_day_overage_included": null,
			"limits": []
		}`))
	}))
	t.Cleanup(srv.Close)

	database := dbtest.OpenTestDB(t)
	job := newTestJob(t, srv.URL, database)
	job.now = func() time.Time { return time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC) }
	require.NoError(t, job.Run(context.Background()))

	before, err := database.LatestRateLimitSnapshots(
		context.Background(), db.RateLimitFilter{Vendor: "claude", AccountID: "acct-uuid-1"},
	)
	require.NoError(t, err)
	windowsBefore := map[string]bool{}
	for _, r := range before {
		windowsBefore[r.WindowKind] = true
	}
	require.True(t, windowsBefore["seven_day_opus"])

	opusNulled.Store(true)
	job.now = func() time.Time { return time.Date(2026, 9, 9, 10, 5, 0, 0, time.UTC) }
	require.NoError(t, job.Run(context.Background()))

	after, err := database.LatestRateLimitSnapshots(
		context.Background(), db.RateLimitFilter{Vendor: "claude", AccountID: "acct-uuid-1"},
	)
	require.NoError(t, err)
	windowsAfter := map[string]bool{}
	for _, r := range after {
		windowsAfter[r.WindowKind] = true
	}
	assert.False(t, windowsAfter["seven_day_opus"],
		"the now-null bucket must be retired from current results")

	history, err := database.RateLimitSnapshotHistory(
		context.Background(), db.RateLimitHistoryFilter{Vendor: "claude", AccountID: "acct-uuid-1"},
	)
	require.NoError(t, err)
	var sawOpusInHistory bool
	for _, r := range history {
		if r.WindowKind == "seven_day_opus" {
			sawOpusInHistory = true
		}
	}
	assert.True(t, sawOpusInHistory, "history must still show the retired bucket's past readings")
}

// TestJobRun_TransientEmptyLimitsAfterSeeingLimitsDoesNotFallBack covers a
// roborev finding: an account whose response has previously carried a
// non-empty `limits` array can still occasionally report an empty one (a
// partial response, a momentary API hiccup), and falling back to the
// fixed buckets on that poll would report the same underlying limit
// under a different window_kind than `limits` uses (e.g. a model-scoped
// weekly cap as "weekly_scoped:Fable" via `limits` but
// "seven_day_overage_included" via the fallback). Since
// LatestRateLimitSnapshots resolves "latest" independently per Claude
// window_kind, both would show up as separate cards with one
// permanently stale. Once an account is known to report `limits`, a
// later empty array must write nothing for the primary/secondary
// windows rather than switching to the fallback, leaving the
// `limits`-array windows' last-known values in place.
func TestJobRun_TransientEmptyLimitsAfterSeeingLimitsDoesNotFallBack(t *testing.T) {
	var limitsEmpty atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if limitsEmpty.Load() {
			_, _ = w.Write([]byte(`{
				"five_hour": null, "seven_day": null, "seven_day_opus": null,
				"seven_day_sonnet": null, "seven_day_oauth_apps": null,
				"seven_day_overage_included": {"utilization": 40, "resets_at": 1789435448},
				"limits": []
			}`))
			return
		}
		_, _ = w.Write([]byte(realOauthUsageLimitsArrayFixture))
	}))
	t.Cleanup(srv.Close)

	database := dbtest.OpenTestDB(t)
	job := newTestJob(t, srv.URL, database)
	job.now = func() time.Time { return time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC) }
	require.NoError(t, job.Run(context.Background()))

	before, err := database.RateLimitSnapshotHistory(
		context.Background(),
		db.RateLimitHistoryFilter{Vendor: "claude", AccountID: "acct-uuid-1", WindowKind: "weekly_scoped:Fable"},
	)
	require.NoError(t, err)
	require.Len(t, before, 1, "the limits-array poll must have written the scoped Fable window")

	// A later poll (a different minute) transiently reports an empty
	// `limits` array.
	limitsEmpty.Store(true)
	job.now = func() time.Time { return time.Date(2026, 9, 9, 10, 5, 0, 0, time.UTC) }
	require.NoError(t, job.Run(context.Background()))

	after, err := database.RateLimitSnapshotHistory(
		context.Background(),
		db.RateLimitHistoryFilter{Vendor: "claude", AccountID: "acct-uuid-1", WindowKind: "seven_day_overage_included"},
	)
	require.NoError(t, err)
	assert.Empty(t, after,
		"a transient empty limits array must not fall back to writing a fixed-bucket window")

	current, err := database.LatestRateLimitSnapshots(
		context.Background(), db.RateLimitFilter{Vendor: "claude", AccountID: "acct-uuid-1"},
	)
	require.NoError(t, err)
	byWindow := map[string]bool{}
	for _, r := range current {
		byWindow[r.WindowKind] = true
	}
	assert.True(t, byWindow["weekly_scoped:Fable"],
		"the limits-array window must still show its last-known value")
	assert.False(t, byWindow["seven_day_overage_included"],
		"no duplicate fixed-bucket card for the same limit must appear")
}
