package main

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/cursorusage"
)

// doctorPollerJob is one background job `doctor pollers` knows how to
// describe, independent of whether the daemon has ever run it.
type doctorPollerJob struct {
	Name        string
	Configured  bool
	Unconfigure string // reason shown when Configured is false
}

// doctorPollerStatus mirrors poller.Status, read directly from the
// SQLite-only poller_status table (see docs/agents/storage.md) rather than
// from a live daemon: `doctor` is a short-lived, offline diagnostic.
type doctorPollerStatus struct {
	LastAttempt         string
	LastSuccess         string
	LastError           string
	ConsecutiveFailures int
	NextRun             string
}

func newDoctorPollersCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "pollers",
		Short:        "Show background poller (scheduler) status",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadReadOnly()
			if err != nil {
				return err
			}
			return runDoctorPollers(cmd.OutOrStdout(), cfg)
		},
	}
}

func runDoctorPollers(w io.Writer, cfg config.Config) error {
	fmt.Fprintln(w, "Poller Diagnostics")
	if !cfg.Poller.Enabled {
		fmt.Fprintln(w, "  disabled: [poller] enabled = false")
		return nil
	}

	statuses, err := loadDoctorPollerStatuses(cfg.DBPath)
	if err != nil {
		fmt.Fprintf(w, "  unable to read poller status: %v\n", err)
		return nil
	}
	for _, job := range doctorKnownPollerJobs(cfg) {
		writeDoctorPollerLine(w, job, statuses)
	}
	return nil
}

// doctorKnownPollerJobs lists the jobs this build can register, given
// config. It intentionally does not talk to a running daemon: a job here
// with no matching poller_status row simply has not attempted a run yet
// against this database.
func doctorKnownPollerJobs(cfg config.Config) []doctorPollerJob {
	jobs := []doctorPollerJob{{Name: pricingRefreshJobName, Configured: true}}
	if strings.TrimSpace(cfg.CursorAdminAPIKey) == "" {
		jobs = append(jobs, doctorPollerJob{
			Name:        cursorusage.JobName,
			Configured:  false,
			Unconfigure: "cursor_admin_api_key is not set",
		})
	} else {
		jobs = append(jobs, doctorPollerJob{
			Name:       cursorusage.JobName,
			Configured: true,
		})
	}
	return jobs
}

func writeDoctorPollerLine(
	w io.Writer, job doctorPollerJob, statuses map[string]doctorPollerStatus,
) {
	if !job.Configured {
		fmt.Fprintf(w, "  %s: not configured (%s)\n", job.Name, job.Unconfigure)
		return
	}
	status, ok := statuses[job.Name]
	if !ok {
		fmt.Fprintf(w, "  %s: never run\n", job.Name)
		return
	}
	fmt.Fprintf(w,
		"  %s: last success=%s, last error=%s, consecutive failures=%d, next run=%s\n",
		job.Name,
		doctorPollerTimeOrNever(status.LastSuccess),
		doctorPollerErrorOrNone(status.LastError),
		status.ConsecutiveFailures,
		doctorPollerTimeOrNever(status.NextRun),
	)
}

func doctorPollerTimeOrNever(s string) string {
	if s == "" {
		return "never"
	}
	return s
}

func doctorPollerErrorOrNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// loadDoctorPollerStatuses reads every poller_status row from the SQLite
// archive at path, read-only. A missing database file or a database that
// predates the poller_status table both report an empty map, not an error:
// neither means diagnostics failed, only that no job has run yet.
func loadDoctorPollerStatuses(path string) (map[string]doctorPollerStatus, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]doctorPollerStatus{}, nil
		}
		return nil, err
	}

	conn, err := sql.Open("sqlite3", doctorReadOnlyDSN(path))
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	rows, err := conn.Query(`
		SELECT name, last_attempt, last_success, last_error,
		       consecutive_failures, next_run
		FROM poller_status
	`)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return map[string]doctorPollerStatus{}, nil
		}
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]doctorPollerStatus)
	for rows.Next() {
		var name string
		var status doctorPollerStatus
		if err := rows.Scan(
			&name, &status.LastAttempt, &status.LastSuccess, &status.LastError,
			&status.ConsecutiveFailures, &status.NextRun,
		); err != nil {
			return nil, err
		}
		out[name] = status
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
