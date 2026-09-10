package main

import (
	"context"
	"log"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/cursorusage"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/poller"
	"go.kenn.io/agentsview/internal/server"
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

// resolvePollerInterval applies a [poller.intervals] override keyed by job
// name, falling back to def when unset or non-positive.
func resolvePollerInterval(
	cfg config.Config, name string, def time.Duration,
) time.Duration {
	if d, ok := cfg.Poller.Intervals[name]; ok && d > 0 {
		return d
	}
	return def
}

// cursorUsageJobOptions is the Scheduler Options for the Cursor usage poll:
// RunAtStart gets usage data flowing shortly after daemon startup rather
// than waiting a full interval, and a short Cooldown keeps a restart or a
// manual TriggerNow from immediately re-fetching. Cursor usage is a
// background poll: it must not count toward the daemon idle-shutdown timer.
func cursorUsageJobOptions() poller.Options {
	return poller.Options{
		Jitter:           2 * time.Minute,
		Cooldown:         5 * time.Minute,
		RunAtStart:       true,
		KeepsDaemonAlive: false,
	}
}

// setupPollerScheduler builds and starts the internal/poller Scheduler for
// this daemon: pricing refresh always, plus Cursor usage polling when an
// admin API key is configured. It returns nil when the [poller] master
// switch is disabled, in which case no background jobs run at all;
// on-demand paths (CLI commands, the synchronous startup pricing seed) are
// unaffected. Background jobs never count toward the daemon idle-shutdown
// timer (see docs/agents/background-work.md), so idleTracker is wired only
// for jobs that opt into KeepsDaemonAlive.
func setupPollerScheduler(
	ctx context.Context,
	cfg config.Config,
	database *db.DB,
	pricingRunner pricingRefreshExclusiveRunner,
	idleTracker *server.IdleTracker,
) *poller.Scheduler {
	if !cfg.Poller.Enabled {
		log.Printf("poller: disabled by config ([poller] enabled = false)")
		return nil
	}

	sched := poller.New(
		dbPollerStore{db: database},
		poller.WithIdleNotifier(idleTracker),
	)

	pricingJob := newPricingRefreshJob(
		database, pricingRunner,
		resolvePollerInterval(cfg, pricingRefreshJobName, pricingRefreshJobInterval),
	)
	sched.Register(pricingJob, pricingRefreshJobOptions())

	if apiKey := strings.TrimSpace(cfg.CursorAdminAPIKey); apiKey != "" {
		cursorJob := cursorusage.NewJob(
			cursorusage.NewClient(apiKey),
			database,
			resolvePollerInterval(cfg, cursorusage.JobName, cursorusage.DefaultInterval),
			cfg.CursorAdminEmail,
			cfg.CursorAdminUserID,
		)
		sched.Register(cursorJob, cursorUsageJobOptions())
	}

	sched.Start(ctx)
	return sched
}
