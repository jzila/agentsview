package main

import (
	"context"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/poller"
	"go.kenn.io/agentsview/internal/pricingrefresh"
)

// pricingRefreshJobName is the stable poller.Job name for the LiteLLM /
// GenAI / OpenRouter pricing catalog refresh.
const pricingRefreshJobName = "pricing-refresh"

// pricingRefreshJobInterval matches internal/pricingrefresh's documented
// 24-hour refresh cadence.
const pricingRefreshJobInterval = 24 * time.Hour

// pricingRefreshJobJitter spreads the daily refresh across a few minutes so
// a fleet of daemons started around the same time doesn't all hit the
// upstream catalogs at once.
const pricingRefreshJobJitter = 5 * time.Minute

type pricingRefreshExclusiveRunner interface {
	RunExclusive(func() error) error
}

// pricingRefreshJob adapts internal/pricingrefresh onto the internal/poller
// Scheduler. It preserves the original ticker loop's semantics: every
// attempt force-refreshes (pricingrefresh.RefreshCurrent), regardless of
// pricingrefresh's own internal cooldown meta-key, and the whole attempt is
// serialized against a resync swap through runner.RunExclusive.
type pricingRefreshJob struct {
	database *db.DB
	runner   pricingRefreshExclusiveRunner
	interval time.Duration
}

// newPricingRefreshJob builds the pricing refresh Job. interval overrides
// the default 24h cadence (e.g. from [poller.intervals] config); a
// non-positive value falls back to pricingRefreshJobInterval.
func newPricingRefreshJob(
	database *db.DB, runner pricingRefreshExclusiveRunner, interval time.Duration,
) *pricingRefreshJob {
	if interval <= 0 {
		interval = pricingRefreshJobInterval
	}
	return &pricingRefreshJob{database: database, runner: runner, interval: interval}
}

func (j *pricingRefreshJob) Name() string { return pricingRefreshJobName }

func (j *pricingRefreshJob) Interval() time.Duration { return j.interval }

func (j *pricingRefreshJob) Run(ctx context.Context) error {
	return runPricingExclusive(j.runner, func() error {
		return pricingrefresh.RefreshCurrent(ctx, j.database)
	})
}

// pricingRefreshJobOptions is the Scheduler Options for the pricing refresh
// job: RunAtStart preserves the previous behavior of refreshing immediately
// on daemon startup (in addition to seedPricing's synchronous fallback
// seed), and Cooldown mirrors internal/pricingrefresh.RefreshCooldown so a
// scheduled tick shortly after a refresh (including one following a
// restart, once persisted status is restored) does not immediately force
// another one. It gates only the steady-tick path: an explicit TriggerNow
// bypasses it by design. Pricing refresh is a background poll: it must not
// count toward the daemon idle-shutdown timer.
func pricingRefreshJobOptions() poller.Options {
	return poller.Options{
		Jitter:           pricingRefreshJobJitter,
		Cooldown:         pricingrefresh.RefreshCooldown,
		RunAtStart:       true,
		KeepsDaemonAlive: false,
	}
}

func runPricingExclusive(
	runner pricingRefreshExclusiveRunner,
	work func() error,
) error {
	if runner == nil {
		return work()
	}
	return runner.RunExclusive(work)
}
