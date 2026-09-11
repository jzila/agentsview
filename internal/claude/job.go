package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/poller"
)

// JobNamePrefix is the stable internal/poller.Job name prefix for a Claude
// usage poll; the full name is JobNamePrefix + the configured account
// name (e.g. "claude-usage:personal").
const JobNamePrefix = "claude-usage:"

// JobName returns the internal/poller.Job name for account.
func JobName(account string) string { return JobNamePrefix + account }

// DefaultInterval is the default poll cadence when [poller.intervals]
// does not override a "claude-usage:<account>" job.
const DefaultInterval = 5 * time.Minute

// DefaultCooldown is the minimum time between attempts for a Claude usage
// job, recorded before each attempt so a failed attempt (e.g. a 401) still
// observes it.
const DefaultCooldown = 1 * time.Minute

// Job polls the oauth/usage endpoint for one configured Claude account on
// an interval and stores the resulting rate-limit windows in the archive,
// via the same rate_limit_snapshots table Codex writes to (vendor
// "claude"). Errors (a bad credentials source, ErrUnauthorized, a network
// failure) are returned from Run and surface through the scheduler's
// status (doctor, GET /api/v1/system/pollers, and the Claude accounts
// settings panel); Run itself only logs at debug level.
type Job struct {
	AccountName      string
	Client           *Client
	Database         *db.DB
	HomeDir          string
	Credentials      CredentialsSource
	ClaudeConfigPath string
	Keychain         KeychainReader

	interval time.Duration

	// now is overridable for tests; defaults to time.Now.
	now func() time.Time

	// sawLimitsArray tracks, per account identity (keyed by
	// claude.Identity.AccountKey(), not by AccountName: the same
	// configured account can poll a different org from one run to the
	// next -- kata 1k4b -- and each org has its own independent format
	// history), whether a poll for that identity has ever reported a
	// non-empty `limits` array. See claudeRateLimitSnapshotsFromUsage
	// for how this gates the fixed-bucket fallback (roborev finding).
	// This is process-lifetime state, not persisted: a restart re-learns
	// it from the next poll, and in the narrow window between a restart
	// and that poll a transiently empty `limits` response can trigger
	// one incorrect fallback-format poll -- retireStaleClaudeWindows
	// cleans that up on the very next poll that reports `limits` again,
	// since that check is driven by persisted rows rather than this map.
	sawLimitsArray map[string]bool
}

// NewJob builds the Claude usage poll Job for one configured account.
// interval overrides DefaultInterval (e.g. from [poller.intervals]
// config); a non-positive value falls back to DefaultInterval. claudeConfigPath
// defaults to DefaultClaudeConfigPath(homeDir) when empty.
func NewJob(
	accountName string,
	client *Client,
	database *db.DB,
	homeDir string,
	credentials CredentialsSource,
	claudeConfigPath string,
	interval time.Duration,
) *Job {
	if interval <= 0 {
		interval = DefaultInterval
	}
	if claudeConfigPath == "" {
		claudeConfigPath = DefaultClaudeConfigPath(homeDir)
	} else {
		claudeConfigPath = ExpandHome(claudeConfigPath, homeDir)
	}
	return &Job{
		AccountName:      accountName,
		Client:           client,
		Database:         database,
		HomeDir:          homeDir,
		Credentials:      credentials,
		ClaudeConfigPath: claudeConfigPath,
		interval:         interval,
		now:              time.Now,
		sawLimitsArray:   make(map[string]bool),
	}
}

func (j *Job) Name() string { return JobName(j.AccountName) }

func (j *Job) Interval() time.Duration { return j.interval }

func (j *Job) Run(ctx context.Context) error {
	token, err := ResolveAccessToken(ctx, j.Credentials, j.HomeDir, j.Keychain)
	if err != nil {
		return fmt.Errorf("claude-usage:%s: resolving access token: %w", j.AccountName, err)
	}

	identity, err := ReadIdentity(j.ClaudeConfigPath)
	if err != nil {
		return fmt.Errorf("claude-usage:%s: reading identity: %w", j.AccountName, err)
	}
	if identity.AccountUUID == "" {
		return fmt.Errorf(
			"claude-usage:%s: no oauthAccount identity in %s (not logged in?)",
			j.AccountName, j.ClaudeConfigPath,
		)
	}

	usage, err := j.Client.FetchUsage(ctx, token)
	if err != nil {
		if rateLimited, ok := errors.AsType[*RateLimitedError](err); ok {
			return &poller.RetryAfterError{RetryAfter: rateLimited.RetryAfter, Err: err}
		}
		return fmt.Errorf("claude-usage:%s: %w", j.AccountName, err)
	}

	now := time.Now()
	if j.now != nil {
		now = j.now()
	}
	observedAt := now.UTC().Format(time.RFC3339Nano)

	rows, freshLimitsWindowKinds, freshFixedBucketWindowKinds :=
		claudeRateLimitSnapshotsFromUsage(usage, identity, observedAt, j.sawLimitsArray)
	if usage.HasLimits() {
		retired, err := j.retireStaleClaudeWindows(ctx, identity, observedAt, freshLimitsWindowKinds)
		if err != nil {
			return fmt.Errorf("claude-usage:%s: checking for stale rate-limit windows: %w", j.AccountName, err)
		}
		rows = append(rows, retired...)
	}
	if freshFixedBucketWindowKinds != nil {
		retired, err := j.retireStaleFixedBucketWindows(ctx, identity, observedAt, freshFixedBucketWindowKinds)
		if err != nil {
			return fmt.Errorf("claude-usage:%s: checking for stale fixed-bucket windows: %w", j.AccountName, err)
		}
		rows = append(rows, retired...)
	}
	if usage.ExtraUsage == nil {
		retired, err := j.retireStaleExtraUsageWindow(ctx, identity, observedAt)
		if err != nil {
			return fmt.Errorf("claude-usage:%s: checking for a stale extra-usage window: %w", j.AccountName, err)
		}
		rows = append(rows, retired...)
	}
	if len(rows) == 0 {
		return nil
	}
	if err := j.Database.InsertRateLimitSnapshotsContext(ctx, rows); err != nil {
		return fmt.Errorf("claude-usage:%s: storing snapshots: %w", j.AccountName, err)
	}
	return nil
}

// retireStaleClaudeWindows checks this identity's currently-visible
// rate-limit windows (via db.LatestRateLimitSnapshots, which already
// excludes anything already retired) against freshWindowKinds -- every
// window_kind this poll's `limits` array produced -- and returns a
// disabled row for any current window not in that set. The `limits`
// array is a complete snapshot of the account's limits on every poll,
// not an incremental delta, so anything current but absent from it is
// no longer a real limit: a model- or surface-scoped cap that expired
// or was removed between polls, or a window that came from the older
// fixed-bucket fallback or the statusLine sink and has no equivalent in
// this poll's `limits` output (e.g. seven_day_overage_included). Each of
// these resolves its own latest observation independently of every
// other Claude window, so nothing else would ever supersede one on its
// own (roborev findings). extra_usage_monthly is a separate concern
// (see claudeRateLimitSnapshotsFromUsage) and is excluded here. Called
// whenever a poll reports `limits`, so a stale window written before
// this process started still gets retired once `limits` becomes
// authoritative again -- driving this off persisted rows rather than
// in-memory state makes it correct across a daemon restart. A poll
// where nothing needs retiring returns an empty slice and costs one
// extra read.
func (j *Job) retireStaleClaudeWindows(
	ctx context.Context, identity Identity, observedAt string, freshWindowKinds map[string]bool,
) ([]db.RateLimitSnapshot, error) {
	accountID := identity.AccountKey()
	current, err := j.Database.LatestRateLimitSnapshots(
		ctx, db.RateLimitFilter{Vendor: "claude", AccountID: accountID},
	)
	if err != nil {
		return nil, err
	}
	var rows []db.RateLimitSnapshot
	for _, r := range current {
		if r.WindowKind == "extra_usage_monthly" || freshWindowKinds[r.WindowKind] {
			continue
		}
		details, _ := json.Marshal(map[string]any{"source": "limits", "enabled": false})
		rows = append(rows, db.RateLimitSnapshot{
			Vendor:       "claude",
			AccountID:    accountID,
			AccountLabel: identity.Label(),
			WindowKind:   r.WindowKind,
			Details:      string(details),
			ObservedAt:   observedAt,
		})
	}
	return rows, nil
}

// retireStaleFixedBucketWindows is retireStaleClaudeWindows' counterpart
// for the fixed-bucket fallback path (claudeRateLimitSnapshotsFromUsage's
// `default` case): Windows() silently omits a bucket the response
// reports as null rather than including it as zero, so a bucket going
// from populated to null between polls (the account's weekly cap
// resetting to a null response, say) previously left that window's old
// value looking current forever, since nothing else supersedes a Claude
// window on its own (roborev finding; the same gap retireStaleClaude
// Windows already closes for the `limits` path). freshWindowKinds is
// non-nil only when this poll actually took the fallback path (see
// claudeRateLimitSnapshotsFromUsage), so the caller skips this entirely
// for an account on the `limits` path or mid-transient-gap. Scoped to
// the known fixed-bucket kinds (canonicalWindowMinutes' keys) rather
// than every current window, since this account's own extra_usage_
// monthly window is never a Windows() output and a `limits`-path
// account never reaches this method in the first place, so there is no
// cross-source ambiguity to guard against here the way retireStale
// ClaudeWindows must for the reverse direction.
func (j *Job) retireStaleFixedBucketWindows(
	ctx context.Context, identity Identity, observedAt string, freshWindowKinds map[string]bool,
) ([]db.RateLimitSnapshot, error) {
	accountID := identity.AccountKey()
	current, err := j.Database.LatestRateLimitSnapshots(
		ctx, db.RateLimitFilter{Vendor: "claude", AccountID: accountID},
	)
	if err != nil {
		return nil, err
	}
	var rows []db.RateLimitSnapshot
	for _, r := range current {
		if CanonicalWindowMinutes(r.WindowKind) == 0 || freshWindowKinds[r.WindowKind] {
			continue
		}
		details, _ := json.Marshal(map[string]any{"source": "fixed_buckets", "enabled": false})
		rows = append(rows, db.RateLimitSnapshot{
			Vendor:       "claude",
			AccountID:    accountID,
			AccountLabel: identity.Label(),
			WindowKind:   r.WindowKind,
			Details:      string(details),
			ObservedAt:   observedAt,
		})
	}
	return rows, nil
}

// retireStaleExtraUsageWindow checks whether this identity currently has
// a non-disabled extra_usage_monthly row (via db.LatestRateLimitSnapshots,
// which already excludes anything already retired) and, if so, returns a
// disabled row for it. Called only when this poll's response carries no
// extra_usage object at all: claudeRateLimitSnapshotsFromUsage already
// retires the window itself whenever the object is present but reports
// IsEnabled false, but a response that drops the object entirely (the
// account's extra-usage feature was turned off, or the account no longer
// has it) writes nothing for this window at all, so nothing else would
// ever supersede a previously-enabled row and the Usage page would show
// a stale allowance indefinitely (roborev finding). Driven by persisted
// rows rather than in-memory state, so it is correct across a daemon
// restart, the same as retireStaleClaudeWindows. A poll with nothing to
// retire returns an empty slice and costs one extra read.
func (j *Job) retireStaleExtraUsageWindow(
	ctx context.Context, identity Identity, observedAt string,
) ([]db.RateLimitSnapshot, error) {
	accountID := identity.AccountKey()
	current, err := j.Database.LatestRateLimitSnapshots(
		ctx, db.RateLimitFilter{Vendor: "claude", AccountID: accountID},
	)
	if err != nil {
		return nil, err
	}
	for _, r := range current {
		if r.WindowKind != "extra_usage_monthly" {
			continue
		}
		details, _ := json.Marshal(map[string]any{"source": "extra_usage", "enabled": false})
		return []db.RateLimitSnapshot{{
			Vendor:       "claude",
			AccountID:    accountID,
			AccountLabel: identity.Label(),
			WindowKind:   "extra_usage_monthly",
			Details:      string(details),
			ObservedAt:   observedAt,
		}}, nil
	}
	return nil, nil
}

// claudeRateLimitSnapshotsFromUsage converts one oauth/usage poll into
// rate_limit_snapshots rows (kata eabb).
//
// The top-level `limits` array is the primary source when present: it is
// the only place a model-scoped weekly cap (e.g. the "Fable" weekly
// limit) appears -- verified 2026-09-10 against a live response where
// SevenDayOpus and SevenDaySonnet were both null while `limits` carried
// exactly that cap under kind "weekly_scoped". The six fixed buckets
// (Windows()) are used only as a fallback when Limits is empty, in case
// a future response shape drops the array again.
//
// extra_usage is persisted independently of which of the above produced
// the primary rows, as its own "extra_usage_monthly" window, whenever
// the response carries an extra_usage object at all -- enabled or not,
// so a poll that finds it newly disabled can retire a stale card
// instead of leaving nothing to ever supersede it (see
// db.rateLimitSnapshotExtraUsageDisabled).
// resetsAtOrNil converts Claude's own int64 "0 means the server reported
// no reset time" convention (see Window.ResetsAt) into the shared
// db.RateLimitSnapshot.ResetsAt's *int64 "nil means unknown" convention,
// so a genuinely unknown Claude reset time is never displayed as if the
// window resets at the unix epoch, the same way a nullable Codex
// resets_at is handled.
func resetsAtOrNil(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}

// claudeRateLimitSnapshotsFromUsage's second return value,
// freshLimitsWindowKinds, is the set of window_kind values this poll's
// `limits` array produced (nil when usage.HasLimits() is false); the
// caller (Job.Run) uses it to retire any other Claude window for this
// account that is not in that set (see retireStaleClaudeWindows).
//
// The third return value, freshFixedBucketWindowKinds, is the analogous
// set for the fixed-bucket fallback path: non-nil only when this poll
// actually took that path (the `default` case below, an account that
// has never reported `limits`), listing whichever of the six buckets
// were non-null this time -- possibly empty, if every bucket happened
// to be null. Windows() silently omits a null bucket rather than
// reporting it as zero, so a bucket going from populated to null
// between polls would otherwise leave its last value looking current
// forever, since nothing else supersedes a Claude window on its own
// (see retireStaleClaudeWindows's doc comment); the caller uses this to
// retire any such bucket the same way.
func claudeRateLimitSnapshotsFromUsage(
	usage *UsageResponse, identity Identity, observedAt string,
	sawLimitsArray map[string]bool,
) ([]db.RateLimitSnapshot, map[string]bool, map[string]bool) {
	accountID := identity.AccountKey()
	accountLabel := identity.Label()

	var rows []db.RateLimitSnapshot
	var freshLimitsWindowKinds map[string]bool
	var freshFixedBucketWindowKinds map[string]bool
	switch {
	case usage.HasLimits():
		if sawLimitsArray != nil {
			sawLimitsArray[accountID] = true
		}
		freshLimitsWindowKinds = make(map[string]bool, len(usage.Limits))
		for _, entry := range usage.Limits {
			windowKind := entry.WindowKind()
			// Registered before parsing resets_at: this entry is still
			// part of this poll's `limits` response regardless of
			// whether its own resets_at happens to parse, so a
			// previously stored row for this window must not look
			// "missing from limits" and be retired by
			// retireStaleClaudeWindows just because this one field
			// failed to parse (roborev finding) -- only the row write
			// below is skipped for this entry.
			freshLimitsWindowKinds[windowKind] = true
			resetsAt, err := parseResetsAt(entry.ResetsAt)
			if err != nil {
				// An unparsable resets_at on one entry (e.g. a future,
				// differently-shaped code-named entry) must not drop
				// every other window in this poll; skip just this one.
				continue
			}
			details, _ := json.Marshal(map[string]any{
				"source":    "limits",
				"kind":      entry.Kind,
				"group":     entry.Group,
				"is_active": entry.IsActive,
				"scope":     entry.Scope,
			})
			rows = append(rows, db.RateLimitSnapshot{
				Vendor:        "claude",
				AccountID:     accountID,
				AccountLabel:  accountLabel,
				WindowKind:    windowKind,
				UsedPercent:   entry.Percent,
				WindowMinutes: entry.WindowMinutes(),
				ResetsAt:      resetsAtOrNil(resetsAt),
				ScopeLabel:    entry.Scope.Label(),
				Severity:      entry.Severity,
				Details:       string(details),
				ObservedAt:    observedAt,
			})
		}
	case sawLimitsArray != nil && sawLimitsArray[accountID]:
		// This identity has reported the `limits` array on a previous
		// poll, so a now-empty array is treated as a transient gap
		// (a partial response, a momentary API hiccup) rather than a
		// permanent format change: falling back to the fixed buckets
		// here would report the same underlying limit under a
		// different window_kind (e.g. a model-scoped weekly cap as
		// "weekly_scoped:<model>" via `limits` but
		// "seven_day_overage_included" via the fallback), and since
		// LatestRateLimitSnapshots resolves "latest" independently per
		// Claude window_kind, both would show up as separate cards with
		// one permanently stale (roborev finding). Writing nothing for
		// the primary/secondary windows this poll leaves the
		// `limits`-array windows' last-known values in place instead.
	default:
		windows := usage.Windows()
		freshFixedBucketWindowKinds = make(map[string]bool, len(windows))
		for _, w := range windows {
			freshFixedBucketWindowKinds[w.Kind] = true
			details, _ := json.Marshal(map[string]any{"source": "fixed_buckets"})
			rows = append(rows, db.RateLimitSnapshot{
				Vendor:        "claude",
				AccountID:     accountID,
				AccountLabel:  accountLabel,
				WindowKind:    w.Kind,
				UsedPercent:   w.Window.Utilization,
				WindowMinutes: CanonicalWindowMinutes(w.Kind),
				ResetsAt:      resetsAtOrNil(w.Window.ResetsAt),
				Details:       string(details),
				ObservedAt:    observedAt,
			})
		}
	}

	// Once an account's response has ever carried an extra_usage
	// object, the window is written on every later poll, enabled or
	// not: LatestRateLimitSnapshots resolves "latest" independently per
	// Claude window_kind (see bucket_window_kind), so a poll that
	// simply stopped writing this window because the account disabled
	// it would leave its last-enabled row as "latest" forever -- the
	// Usage page would keep showing a stale monthly allowance the
	// account no longer has. Writing an explicit disabled row instead
	// gives this window a fresh observation to resolve to, and
	// db.LatestRateLimitSnapshots excludes a disabled row from current
	// results while RateLimitSnapshotHistory still returns it, so a
	// chart covering the disable event shows the transition rather than
	// silently truncating it. An account whose response has no
	// extra_usage object at all (the common case: the feature was never
	// offered to it) gets no row and no per-poll write for a window it
	// will never report.
	if usage.ExtraUsage != nil {
		detailsMap := map[string]any{"source": "extra_usage", "enabled": usage.ExtraUsage.IsEnabled}
		usedPercent := 0.0
		if usage.ExtraUsage.IsEnabled {
			detailsMap["spend_limit_reached"] = usage.ExtraUsage.SpendLimitReached
			if usage.Spend != nil && usage.Spend.Limit != nil {
				detailsMap["monthly_limit_minor"] = usage.Spend.Limit.AmountMinor
				detailsMap["currency"] = usage.Spend.Limit.Currency
				detailsMap["exponent"] = usage.Spend.Limit.Exponent
			}
			usedPercent = usage.ExtraUsage.Utilization
		}
		details, _ := json.Marshal(detailsMap)
		rows = append(rows, db.RateLimitSnapshot{
			Vendor:       "claude",
			AccountID:    accountID,
			AccountLabel: accountLabel,
			WindowKind:   "extra_usage_monthly",
			UsedPercent:  usedPercent,
			Details:      string(details),
			ObservedAt:   observedAt,
		})
	}

	return rows, freshLimitsWindowKinds, freshFixedBucketWindowKinds
}
