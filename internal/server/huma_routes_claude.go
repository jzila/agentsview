package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"time"

	"go.kenn.io/agentsview/internal/claude"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/poller"
)

// ClaudeAccountInfo is the read-only, no-secrets view of one configured
// [claude.accounts.NAME] entry for the Claude accounts settings panel:
// identity, tier, and poller status. Credentials themselves are never
// included -- CredentialsSource only describes where they come from
// ("keychain", "file", or "env"), matching what is already visible in
// config.toml.
type ClaudeAccountInfo struct {
	Name              string `json:"name"`
	CredentialsSource string `json:"credentialsSource"`
	Label             string `json:"label,omitempty"`
	OrganizationName  string `json:"organizationName,omitempty"`
	SeatTier          string `json:"seatTier,omitempty"`
	UserRateLimitTier string `json:"userRateLimitTier,omitempty"`
	IdentityAvailable bool   `json:"identityAvailable"`
	IdentityError     string `json:"identityError,omitempty"`
	LastPoll          string `json:"lastPoll,omitempty"`
	LastError         string `json:"lastError,omitempty"`
	NextRun           string `json:"nextRun,omitempty"`
}

// ClaudeAccountsInfo is the response body for GET /api/v1/claude/accounts.
type ClaudeAccountsInfo struct {
	Accounts []ClaudeAccountInfo `json:"accounts"`
}

func (s *Server) registerClaudeRoutes() {
	group := newRouteGroup(s.api, "/api/v1/claude", "Claude")

	s.get(group, "/accounts", "List configured Claude accounts", s.humaClaudeAccounts)
	s.post(group, "/accounts/{name}/test", "Poll one Claude account now", s.humaClaudeAccountTest)
	s.post(group, "/rate-limit-snapshots", "Ingest Claude rate-limit snapshots", s.humaClaudeRateLimitSnapshotsIngest)
}

func (s *Server) humaClaudeAccounts(
	_ context.Context, _ *emptyInput,
) (*jsonOutput[ClaudeAccountsInfo], error) {
	// cfg is copied under the same s.mu that guards a settings update
	// writing s.cfg (roborev finding): copying it beforehand raced that
	// write, an unsynchronized struct-copy-vs-field-write race Go's race
	// detector flags even though nothing here ever mutates Claude.Accounts
	// itself once loaded.
	s.mu.RLock()
	cfg := s.cfg
	statusFn := s.pollerStatus
	s.mu.RUnlock()

	statusByName := map[string]struct {
		LastAttempt, LastSuccess, LastError, NextRun string
	}{}
	if statusFn != nil {
		for _, st := range statusFn() {
			statusByName[st.Name] = struct {
				LastAttempt, LastSuccess, LastError, NextRun string
			}{
				LastAttempt: formatPollerTime(st.LastAttempt),
				LastSuccess: formatPollerTime(st.LastSuccess),
				LastError:   st.LastError,
				NextRun:     formatPollerTime(st.NextRun),
			}
		}
	}

	homeDir, _ := os.UserHomeDir()

	names := make([]string, 0, len(cfg.Claude.Accounts))
	for name := range cfg.Claude.Accounts {
		names = append(names, name)
	}
	sort.Strings(names)

	accounts := make([]ClaudeAccountInfo, 0, len(names))
	for _, name := range names {
		acct := cfg.Claude.Accounts[name]
		info := ClaudeAccountInfo{
			Name:              name,
			CredentialsSource: acct.Credentials,
		}
		if st, ok := statusByName[claude.JobName(name)]; ok {
			info.LastPoll = st.LastSuccess
			if info.LastPoll == "" {
				info.LastPoll = st.LastAttempt
			}
			info.LastError = st.LastError
			info.NextRun = st.NextRun
		}

		claudeConfigPath := acct.ClaudeConfig
		if claudeConfigPath == "" {
			if homeDir != "" {
				claudeConfigPath = claude.DefaultClaudeConfigPath(homeDir)
			}
		} else {
			claudeConfigPath = claude.ExpandHome(claudeConfigPath, homeDir)
		}
		if claudeConfigPath == "" {
			info.IdentityError = "no home directory to resolve a default .claude.json path"
		} else if identity, err := claude.ReadIdentity(claudeConfigPath); err != nil {
			info.IdentityError = err.Error()
		} else if identity.AccountUUID == "" {
			info.IdentityError = fmt.Sprintf("no oauthAccount identity in %s (not logged in?)", claudeConfigPath)
		} else {
			info.IdentityAvailable = true
			info.Label = identity.Label()
			info.OrganizationName = identity.OrganizationName
			info.SeatTier = identity.SeatTier
			info.UserRateLimitTier = identity.UserRateLimitTier
		}

		accounts = append(accounts, info)
	}

	return &jsonOutput[ClaudeAccountsInfo]{Body: ClaudeAccountsInfo{Accounts: accounts}}, nil
}

type claudeAccountTestInput struct {
	Name string `path:"name" required:"true" doc:"Configured Claude account name"`
}

// ClaudeAccountTestResult is the response body for POST
// /api/v1/claude/accounts/{name}/test.
type ClaudeAccountTestResult struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

func (s *Server) humaClaudeAccountTest(
	_ context.Context, in *claudeAccountTestInput,
) (*jsonOutput[ClaudeAccountTestResult], error) {
	// cfg and triggerFn are both copied under the same s.mu that guards a
	// settings update writing s.cfg (roborev finding, the same one
	// humaClaudeAccounts had): copying cfg beforehand raced that write.
	s.mu.RLock()
	cfg := s.cfg
	triggerFn := s.pollerTrigger
	s.mu.RUnlock()
	if _, ok := cfg.Claude.Accounts[in.Name]; !ok {
		return nil, apiError(http.StatusNotFound, fmt.Sprintf("claude account %q is not configured", in.Name))
	}

	if triggerFn == nil {
		return &jsonOutput[ClaudeAccountTestResult]{Body: ClaudeAccountTestResult{
			Success: false,
			Error:   "background pollers are disabled ([poller] enabled = false)",
		}}, nil
	}

	// TriggerNow always bypasses the job's steady-state Cooldown, by
	// design (poller.Scheduler.attempt's doc comment), so a Test click
	// right after a healthy poll is expected to run immediately. A
	// pending Retry-After wait is different: attempt checks it
	// unconditionally, even for a TriggerNow call, and returns
	// *poller.ErrRetryAfterPending without running the job at all rather
	// than repeating a request an upstream 429 explicitly asked to wait
	// out. That check runs inside the same per-job loop goroutine that
	// would otherwise run the attempt, so it cannot race a poll that is
	// already in flight the way a separate pre-check here once did
	// (roborev finding): the Test button's trigger request and any
	// concurrently scheduled attempt are always handled one at a time.
	if err := triggerFn(claude.JobName(in.Name)); err != nil {
		message := err.Error()
		if pending, ok := errors.AsType[*poller.ErrRetryAfterPending](err); ok {
			message = fmt.Sprintf(
				"account %q is still waiting out a rate-limit retry-after from a previous poll; retrying now would repeat the same request before its wait elapses -- try again in %s",
				in.Name, pending.Remaining.Round(time.Second),
			)
		}
		return &jsonOutput[ClaudeAccountTestResult]{Body: ClaudeAccountTestResult{
			Success: false,
			Error:   message,
		}}, nil
	}
	return &jsonOutput[ClaudeAccountTestResult]{Body: ClaudeAccountTestResult{Success: true}}, nil
}

// ClaudeRateLimitSnapshotInput is one row a trusted local caller (today,
// only `agentsview claude statusline-sink`) forwards to this daemon.
// Ingestion exists because a daemon-owned SQLite archive rejects direct
// writes from another process (openWriteDB's write-owner check): the
// statusline sink calls this endpoint instead of writing directly
// whenever a writable daemon is active, so the advertised low-latency
// complement to the oauth/usage poller keeps working during normal
// daemon use (roborev finding on kata 9rs0, filed as kata qs2f). Vendor
// is always "claude" here -- this endpoint intentionally cannot write
// Codex rows.
type ClaudeRateLimitSnapshotInput struct {
	AccountID    string  `json:"accountId" required:"true"`
	AccountLabel string  `json:"accountLabel,omitempty"`
	WindowKind   string  `json:"windowKind" required:"true"`
	UsedPercent  float64 `json:"usedPercent"`
	ResetsAt     int64   `json:"resetsAt,omitempty"`
	ObservedAt   string  `json:"observedAt" required:"true"`
}

type claudeRateLimitSnapshotsIngestInput struct {
	Body struct {
		Snapshots []ClaudeRateLimitSnapshotInput `json:"snapshots"`
	}
}

// ClaudeRateLimitSnapshotsIngestResult is the response body for POST
// /api/v1/claude/rate-limit-snapshots.
type ClaudeRateLimitSnapshotsIngestResult struct {
	Inserted int `json:"inserted"`
}

func (s *Server) humaClaudeRateLimitSnapshotsIngest(
	ctx context.Context, in *claudeRateLimitSnapshotsIngestInput,
) (*jsonOutput[ClaudeRateLimitSnapshotsIngestResult], error) {
	local, ok := s.db.(*db.DB)
	if !ok {
		return nil, apiError(http.StatusNotImplemented, "claude rate-limit snapshots are SQLite-only")
	}
	rows := make([]db.RateLimitSnapshot, 0, len(in.Body.Snapshots))
	for _, snap := range in.Body.Snapshots {
		if snap.WindowKind == "" || snap.ObservedAt == "" {
			return nil, apiError(http.StatusBadRequest, "each snapshot requires windowKind and observedAt")
		}
		var resetsAt *int64
		if snap.ResetsAt != 0 {
			v := snap.ResetsAt
			resetsAt = &v
		}
		rows = append(rows, db.RateLimitSnapshot{
			Vendor:       "claude",
			AccountID:    snap.AccountID,
			AccountLabel: snap.AccountLabel,
			WindowKind:   snap.WindowKind,
			UsedPercent:  snap.UsedPercent,
			// CanonicalWindowMinutes(WindowKind) is 0 for a kind (e.g.
			// "spend_limit") with no known fixed duration, and 300/10080
			// for "session"/"weekly": the wire shape this endpoint
			// accepts (see ClaudeRateLimitSnapshotInput) has no
			// windowMinutes field of its own, so without reconstructing
			// it here, every snapshot ingested through a running daemon
			// (as opposed to a direct write) lost its duration and fell
			// back to a duration-less generic title until the next
			// oauth/usage poll supplied one (roborev finding).
			WindowMinutes: claude.CanonicalWindowMinutes(snap.WindowKind),
			ResetsAt:      resetsAt,
			ObservedAt:    snap.ObservedAt,
		})
	}

	if err := local.InsertRateLimitSnapshotsContext(ctx, rows); err != nil {
		if handled := handleHumaContextError(err); handled != nil {
			return nil, handled
		}
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("claude rate-limit snapshots ingest error", err)
	}
	return &jsonOutput[ClaudeRateLimitSnapshotsIngestResult]{
		Body: ClaudeRateLimitSnapshotsIngestResult{Inserted: len(rows)},
	}, nil
}
