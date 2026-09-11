package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/agentsview/internal/claude"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

// claudeStatuslineSinkHTTPClient bounds the single request the statusline
// sink makes when forwarding snapshots to a running daemon (see
// forwardClaudeRateLimitSnapshotsToDaemon). The sink runs on every
// statusLine refresh, so it must never hang a shell prompt.
var claudeStatuslineSinkHTTPClient = &http.Client{Timeout: 5 * time.Second}

// statuslineRateLimitWindow is one nullable {used_percentage, resets_at}
// bucket from Claude Code's statusLine JSON `rate_limits` object.
type statuslineRateLimitWindow struct {
	UsedPercentage float64 `json:"used_percentage"`
	ResetsAt       int64   `json:"resets_at"`
}

// statuslineInput is the subset of Claude Code's statusLine stdin JSON
// this sink reads. Verified against the Claude Code 2.1.266 bundle (see
// docs/internal/session-format-sources.md): the documented `rate_limits`
// object carries only five_hour, seven_day, and (gateway-only)
// spend_limit -- it never carries seven_day_opus, seven_day_sonnet, or
// seven_day_oauth_apps, so this sink is a low-latency, network-free
// complement to the oauth/usage poller (internal/claude.Job), not a
// replacement for it.
type statuslineInput struct {
	RateLimits *struct {
		FiveHour   *statuslineRateLimitWindow `json:"five_hour"`
		SevenDay   *statuslineRateLimitWindow `json:"seven_day"`
		SpendLimit *statuslineRateLimitWindow `json:"spend_limit"`
	} `json:"rate_limits"`
}

// resetsAtOrNil converts the statusline sink's own int64 "0 means the
// window carries no reset time" convention into db.RateLimitSnapshot.
// ResetsAt's *int64 "nil means unknown" convention, so a genuinely
// unknown reset time is never displayed as if the window resets at the
// unix epoch, the same way a nullable Codex or poller-sourced Claude
// resets_at is handled.
func resetsAtOrNil(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}

func newClaudeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "claude",
		Short:        "Claude Code integration commands",
		GroupID:      groupUsage,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newClaudeStatuslineSinkCommand())
	return cmd
}

// ClaudeStatuslineSinkConfig configures `agentsview claude
// statusline-sink`.
type ClaudeStatuslineSinkConfig struct {
	ClaudeConfig string
}

func newClaudeStatuslineSinkCommand() *cobra.Command {
	var cfg ClaudeStatuslineSinkConfig
	cmd := &cobra.Command{
		Use:   "statusline-sink",
		Short: "Record rate-limit snapshots from a Claude Code statusLine hook",
		Long: strings.TrimSpace(`
Reads the JSON Claude Code passes to a statusLine command on stdin and
records any rate_limits windows it carries (five_hour, seven_day, and
the gateway-only spend_limit) as Claude vendor snapshots, without any
network access of its own.

Wire it into ~/.claude/settings.json's statusLine config. Since Claude
Code renders a statusLine command's stdout as the visible status line,
compose it alongside your real statusline renderer rather than
replacing it, for example:

    "statusLine": {
      "type": "command",
      "command": "bash -c 'tee >(agentsview claude statusline-sink) | your-statusline-command'"
    }

The example above must run under bash, not a bare "sh": tee's
">(...)" process substitution is a bash/ksh/zsh extension, and parsing
it fails outright under a POSIX sh such as dash (the default /bin/sh on
several Linux distributions).

This sink only ever sees five_hour, seven_day, and spend_limit -- Claude
Code does not pass seven_day_opus, seven_day_sonnet, or
seven_day_oauth_apps to statusLine commands. Run the background poller
(configure [claude.accounts.NAME] in config.toml) for complete coverage;
this command is a low-latency, network-free complement to it, not a
replacement.
`),
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClaudeStatuslineSink(cfg, cmd.InOrStdin())
		},
	}
	cmd.Flags().StringVar(&cfg.ClaudeConfig, "claude-config", "",
		"Path to the .claude.json identity file (default ~/.claude.json)")
	return cmd
}

func runClaudeStatuslineSink(cfg ClaudeStatuslineSinkConfig, stdin io.Reader) error {
	body, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err != nil {
		return fmt.Errorf("reading statusline input: %w", err)
	}

	var in statuslineInput
	if err := json.Unmarshal(body, &in); err != nil {
		return fmt.Errorf("parsing statusline input: %w", err)
	}
	if in.RateLimits == nil {
		return nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home directory: %w", err)
	}
	claudeConfigPath := strings.TrimSpace(cfg.ClaudeConfig)
	if claudeConfigPath == "" {
		claudeConfigPath = claude.DefaultClaudeConfigPath(home)
	} else {
		claudeConfigPath = claude.ExpandHome(claudeConfigPath, home)
	}
	identity, err := claude.ReadIdentity(claudeConfigPath)
	if err != nil {
		return fmt.Errorf("reading claude identity: %w", err)
	}
	if identity.AccountUUID == "" {
		// Not logged in, or an API-key/non-subscriber install with no
		// oauthAccount block: nothing to attribute this snapshot to.
		return nil
	}

	observedAt := time.Now().UTC().Format(time.RFC3339Nano)
	var rows []db.RateLimitSnapshot
	add := func(kind string, w *statuslineRateLimitWindow) {
		if w == nil {
			return
		}
		rows = append(rows, db.RateLimitSnapshot{
			Vendor:       "claude",
			AccountID:    identity.AccountKey(),
			AccountLabel: identity.Label(),
			WindowKind:   kind,
			// CanonicalWindowMinutes(kind) is 0 for "spend_limit" (no
			// known fixed duration) and 300/10080 for "session"/
			// "weekly" -- without this, a session/weekly card's title
			// flipped to a duration-less generic label depending on
			// whether the poller or this sink last observed the window
			// (roborev finding on kata ztf4).
			WindowMinutes: claude.CanonicalWindowMinutes(kind),
			UsedPercent:   w.UsedPercentage,
			ResetsAt:      resetsAtOrNil(w.ResetsAt),
			ObservedAt:    observedAt,
		})
	}
	// "session"/"weekly", not "five_hour"/"seven_day": the same shared
	// window_kind identity the oauth/usage poller uses (both the
	// `limits` array and its own fixed-bucket fallback), so a window
	// observed by both the poller and this sink never produces a
	// duplicate card (kata j5md #1).
	add("session", in.RateLimits.FiveHour)
	add("weekly", in.RateLimits.SevenDay)
	add("spend_limit", in.RateLimits.SpendLimit)
	if len(rows) == 0 {
		return nil
	}

	appCfg, err := config.LoadMinimal()
	if err != nil {
		return err
	}

	// A writable daemon owns the SQLite archive and rejects direct
	// writes from another process (openWriteDB's write-owner check);
	// AGENTSVIEW_NO_DAEMON=1 does not change that, since the daemon
	// still holds the lock regardless of what this process's own env
	// says. Forward through the daemon's API instead so the sink keeps
	// working during normal daemon use (roborev finding on kata 9rs0,
	// filed as kata qs2f); AGENTSVIEW_NO_DAEMON=1 only skips this
	// forwarding attempt for a user who explicitly wants a direct
	// write and is prepared for it to fail if a daemon does own the
	// archive after all.
	if !daemonAutostartDisabled() && IsLocalDaemonActive(appCfg.DataDir, appCfg.AuthToken) {
		if err := forwardClaudeRateLimitSnapshotsToDaemon(context.Background(), appCfg, rows); err != nil {
			return fmt.Errorf("claude statusline-sink: %w", err)
		}
		return nil
	}

	database, writeLock, err := openWriteDB(context.Background(), appCfg)
	if err != nil {
		return fmt.Errorf("claude statusline-sink: %w", err)
	}
	defer closeWriteDB(database, writeLock)

	return database.InsertRateLimitSnapshots(rows)
}

// claudeRateLimitSnapshotWire is the wire shape POST
// /api/v1/claude/rate-limit-snapshots expects; it mirrors
// server.ClaudeRateLimitSnapshotInput's JSON tags.
type claudeRateLimitSnapshotWire struct {
	AccountID    string  `json:"accountId"`
	AccountLabel string  `json:"accountLabel,omitempty"`
	WindowKind   string  `json:"windowKind"`
	UsedPercent  float64 `json:"usedPercent"`
	ResetsAt     int64   `json:"resetsAt,omitempty"`
	ObservedAt   string  `json:"observedAt"`
}

// forwardClaudeRateLimitSnapshotsToDaemon POSTs rows to the running
// writable daemon's ingest endpoint, following the same pattern as
// resolveEmbeddingsDaemonClient/embeddingsDaemonClient.do: a bearer
// token from cfg.AuthToken and an Origin header set to the daemon's own
// base URL to satisfy its CSRF guard on mutating requests, since the CLI
// has no real browser origin. The access token itself is never included
// in this request or logged anywhere in this path.
func forwardClaudeRateLimitSnapshotsToDaemon(
	ctx context.Context, cfg config.Config, rows []db.RateLimitSnapshot,
) error {
	rt := FindWritableDaemonRuntime(cfg.DataDir, cfg.AuthToken)
	if rt == nil {
		return fmt.Errorf("no reachable writable local daemon found")
	}
	baseURL := urlFromDaemonRuntime(rt)

	wire := make([]claudeRateLimitSnapshotWire, len(rows))
	for i, r := range rows {
		var resetsAt int64
		if r.ResetsAt != nil {
			resetsAt = *r.ResetsAt
		}
		wire[i] = claudeRateLimitSnapshotWire{
			AccountID:    r.AccountID,
			AccountLabel: r.AccountLabel,
			WindowKind:   r.WindowKind,
			UsedPercent:  r.UsedPercent,
			ResetsAt:     resetsAt,
			ObservedAt:   r.ObservedAt,
		}
	}
	payload, err := json.Marshal(struct {
		Snapshots []claudeRateLimitSnapshotWire `json:"snapshots"`
	}{wire})
	if err != nil {
		return fmt.Errorf("encoding snapshots: %w", err)
	}

	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost,
		strings.TrimSuffix(baseURL, "/")+"/api/v1/claude/rate-limit-snapshots",
		bytes.NewReader(payload),
	)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", baseURL)
	if cfg.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	}

	resp, err := claudeStatuslineSinkHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("forwarding to daemon: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("daemon rejected snapshots (HTTP %d): %s", resp.StatusCode, string(respBody))
	}
	return nil
}
