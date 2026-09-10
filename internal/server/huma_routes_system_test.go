package server

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/poller"
)

func TestPollersRouteReturnsEmptyListWithNoStatusProvider(t *testing.T) {
	s := testServer(t, 0)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/system/pollers", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var body PollersInfo
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Empty(t, body.Pollers)
}

func TestPollersRouteReportsScheduledJobStatus(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	statuses := []poller.Status{
		{
			Name:        "pricing-refresh",
			LastAttempt: now,
			LastSuccess: now,
			NextRun:     now.Add(24 * time.Hour),
		},
		{
			Name:                "cursor-usage",
			LastAttempt:         now,
			LastError:           "401 Unauthorized",
			ConsecutiveFailures: 2,
		},
	}
	s := testServer(t, 0, WithPollerStatus(func() []poller.Status { return statuses }))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/system/pollers", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var body PollersInfo
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Pollers, 2)
	assert.Equal(t, "pricing-refresh", body.Pollers[0].Name)
	assert.NotEmpty(t, body.Pollers[0].LastSuccess)
	assert.NotEmpty(t, body.Pollers[0].NextRun)
	assert.Empty(t, body.Pollers[0].LastError)
	assert.Equal(t, "cursor-usage", body.Pollers[1].Name)
	assert.Equal(t, "401 Unauthorized", body.Pollers[1].LastError)
	assert.Equal(t, 2, body.Pollers[1].ConsecutiveFailures)
	assert.Empty(t, body.Pollers[1].LastSuccess)
}
