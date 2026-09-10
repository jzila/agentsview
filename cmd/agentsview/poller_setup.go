package main

import (
	"context"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/poller"
)

// setupPollerScheduler builds and starts the internal/poller Scheduler for
// this daemon. Pricing refresh runs on it today; the Scheduler exists so
// future interval-driven background work (e.g. rate-limit tracking) can
// register alongside it instead of hand-rolling its own ticker loop.
func setupPollerScheduler(
	ctx context.Context,
	database *db.DB,
	pricingRunner pricingRefreshExclusiveRunner,
) *poller.Scheduler {
	sched := poller.New()

	pricingJob := newPricingRefreshJob(database, pricingRunner, pricingRefreshJobInterval)
	sched.Register(pricingJob, pricingRefreshJobOptions())

	sched.Start(ctx)
	return sched
}
