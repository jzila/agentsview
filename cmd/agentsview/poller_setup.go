package main

import (
	"context"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/cursorusage"
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

// cursorUsageJobOptions is the Scheduler Options for the Cursor usage poll:
// RunAtStart gets usage data flowing shortly after daemon startup rather
// than waiting a full interval, and a short Cooldown keeps a restart or a
// manual TriggerNow from immediately re-fetching.
func cursorUsageJobOptions() poller.Options {
	return poller.Options{
		Jitter:     2 * time.Minute,
		Cooldown:   5 * time.Minute,
		RunAtStart: true,
	}
}

// setupPollerScheduler builds and starts the internal/poller Scheduler for
// this daemon: pricing refresh always, plus Cursor usage polling when an
// admin API key is configured.
func setupPollerScheduler(
	ctx context.Context,
	cfg config.Config,
	database *db.DB,
	pricingRunner pricingRefreshExclusiveRunner,
) *poller.Scheduler {
	sched := poller.New(dbPollerStore{db: database})

	pricingJob := newPricingRefreshJob(database, pricingRunner, pricingRefreshJobInterval)
	sched.Register(pricingJob, pricingRefreshJobOptions())

	if apiKey := strings.TrimSpace(cfg.CursorAdminAPIKey); apiKey != "" {
		cursorJob := cursorusage.NewJob(
			cursorusage.NewClient(apiKey),
			database,
			cursorusage.DefaultInterval,
			cfg.CursorAdminEmail,
			cfg.CursorAdminUserID,
		)
		sched.Register(cursorJob, cursorUsageJobOptions())
	}

	sched.Start(ctx)
	return sched
}
