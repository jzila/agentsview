package main

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/cursorusage"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/poller"
)

func TestRunDoctorPollersDisabled(t *testing.T) {
	var out bytes.Buffer
	cfg := config.Config{Poller: config.PollerConfig{Enabled: false}}
	require.NoError(t, runDoctorPollers(&out, cfg))
	assert.Contains(t, out.String(), "disabled")
}

func TestRunDoctorPollersNeverRun(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessions.db")
	dbtest.OpenTestDBAt(t, dbPath)

	var out bytes.Buffer
	cfg := config.Config{
		Poller: config.PollerConfig{Enabled: true},
		DBPath: dbPath,
	}
	require.NoError(t, runDoctorPollers(&out, cfg))
	text := out.String()
	assert.Contains(t, text, pricingRefreshJobName+": never run")
	assert.Contains(t, text, cursorusage.JobName+": not configured")
}

func TestRunDoctorPollersShowsPersistedStatus(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessions.db")
	database := dbtest.OpenTestDBAt(t, dbPath)

	now := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	require.NoError(t, database.SaveStatus(context.Background(), poller.Status{
		Name:                pricingRefreshJobName,
		LastSuccess:         now,
		ConsecutiveFailures: 0,
		NextRun:             now.Add(24 * time.Hour),
	}))
	require.NoError(t, database.SaveStatus(context.Background(), poller.Status{
		Name:                cursorusage.JobName,
		LastError:           "401 Unauthorized",
		ConsecutiveFailures: 3,
	}))

	var out bytes.Buffer
	cfg := config.Config{
		Poller:            config.PollerConfig{Enabled: true},
		DBPath:            dbPath,
		CursorAdminAPIKey: "key_xxx",
	}
	require.NoError(t, runDoctorPollers(&out, cfg))
	text := out.String()
	assert.Contains(t, text, pricingRefreshJobName+": last success=")
	assert.Contains(t, text, "consecutive failures=0")
	assert.Contains(t, text, cursorusage.JobName+": last success=never")
	assert.Contains(t, text, "last error=401 Unauthorized")
	assert.Contains(t, text, "consecutive failures=3")
}

func TestLoadDoctorPollerStatusesMissingDatabase(t *testing.T) {
	statuses, err := loadDoctorPollerStatuses(filepath.Join(t.TempDir(), "missing.db"))
	require.NoError(t, err)
	assert.Empty(t, statuses)
}
