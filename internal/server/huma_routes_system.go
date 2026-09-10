package server

import (
	"context"
	"time"

	"go.kenn.io/agentsview/internal/poller"
)

// PollerStatus mirrors poller.Status over the wire. It is a distinct wire
// type (rather than the generated client casting poller.Status directly)
// so internal/poller stays free of API/schema-naming concerns; see
// docs/internal/huma-api-routes.md.
type PollerStatus struct {
	Name                string `json:"name"`
	LastAttempt         string `json:"last_attempt,omitempty"`
	LastSuccess         string `json:"last_success,omitempty"`
	LastError           string `json:"last_error,omitempty"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	NextRun             string `json:"next_run,omitempty"`
}

// PollersInfo is the response body for GET /api/v1/system/pollers.
type PollersInfo struct {
	Pollers []PollerStatus `json:"pollers"`
}

func (s *Server) registerSystemRoutes() {
	group := newRouteGroup(s.api, "/api/v1/system", "System")

	s.get(group, "/pollers", "List background poller status", s.humaPollers)
}

func (s *Server) humaPollers(
	_ context.Context, _ *emptyInput,
) (*jsonOutput[PollersInfo], error) {
	s.mu.RLock()
	statusFn := s.pollerStatus
	s.mu.RUnlock()

	pollers := make([]PollerStatus, 0)
	if statusFn != nil {
		for _, status := range statusFn() {
			pollers = append(pollers, pollerStatusToWire(status))
		}
	}
	return &jsonOutput[PollersInfo]{
		Body: PollersInfo{Pollers: pollers},
	}, nil
}

func pollerStatusToWire(status poller.Status) PollerStatus {
	return PollerStatus{
		Name:                status.Name,
		LastAttempt:         formatPollerTime(status.LastAttempt),
		LastSuccess:         formatPollerTime(status.LastSuccess),
		LastError:           status.LastError,
		ConsecutiveFailures: status.ConsecutiveFailures,
		NextRun:             formatPollerTime(status.NextRun),
	}
}

func formatPollerTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
