package main

import (
	"context"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/poller"
)

// dbPollerStore adapts *db.DB's poller_status accessors onto
// poller.StatusStore.
type dbPollerStore struct{ db *db.DB }

func (s dbPollerStore) LoadStatuses(
	ctx context.Context,
) (map[string]poller.Status, error) {
	return s.db.LoadPollerStatuses(ctx)
}

func (s dbPollerStore) SaveStatus(ctx context.Context, status poller.Status) error {
	return s.db.SaveStatus(ctx, status)
}

// setupPollerScheduler builds and starts the internal/poller Scheduler for
// this daemon. Pricing refresh runs on it today; the Scheduler exists so
// future interval-driven background work (e.g. rate-limit tracking) can
// register alongside it instead of hand-rolling its own ticker loop.
func setupPollerScheduler(
	ctx context.Context,
	database *db.DB,
	pricingRunner pricingRefreshExclusiveRunner,
) *poller.Scheduler {
	sched := poller.New(dbPollerStore{db: database})

	pricingJob := newPricingRefreshJob(database, pricingRunner, pricingRefreshJobInterval)
	sched.Register(pricingJob, pricingRefreshJobOptions())

	sched.Start(ctx)
	return sched
}
