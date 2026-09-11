package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// DefaultBaseURL is the Anthropic API host the oauth/usage endpoint lives
// on.
const DefaultBaseURL = "https://api.anthropic.com"

// oauthBetaHeader is the anthropic-beta value Claude Code sends on every
// OAuth Bearer-authenticated first-party API request. Verified against the
// Claude Code 2.1.266 bundle (see
// docs/internal/session-format-sources.md): the minified constant (Xc =
// "oauth-2025-04-20") is attached to every `Authorization: Bearer ...`
// call across the CLI, including the closest sibling to the usage
// endpoint (a "Policy limits (bearer read)" GET using the same
// Authorization + anthropic-beta + User-Agent header set with no
// anthropic-version header).
const oauthBetaHeader = "oauth-2025-04-20"

// UserAgent mirrors Claude Code's own `claude-code/<version>` User-Agent
// string (function Ia() in the bundle). Pinned to the version this
// endpoint and header set were verified against; bump it if Anthropic
// starts gating the endpoint on a newer client version.
const UserAgent = "claude-code/2.1.266"

// Window is one nullable rate-limit bucket from the oauth/usage response:
// {utilization, resets_at}, or entirely absent (nil) when the account has
// no data for that window.
type Window struct {
	// Utilization is the percent of the window used, 0-100 (can exceed
	// 100 once a window is overspent).
	Utilization float64
	// ResetsAt is a Unix epoch seconds timestamp, or 0 when the server
	// reported no reset time.
	ResetsAt int64
}

// windowWire is the raw wire shape of one Window. resets_at is decoded
// through parseResetsAt rather than a plain int64 field: live production
// traffic returns it as an RFC3339 string with a numeric UTC offset (e.g.
// "2026-09-10T01:10:00.010657+00:00"), verified 2026-09-09 against the
// real endpoint with a real account -- not the unix-seconds integer the
// bundle's internal schema description implied. Both shapes are accepted
// since an unofficial endpoint's wire format can change without notice
// (docs/internal/session-format-sources.md).
type windowWire struct {
	Utilization float64         `json:"utilization"`
	ResetsAt    json.RawMessage `json:"resets_at"`
}

// UnmarshalJSON decodes one Window bucket, tolerating both a unix-seconds
// integer and an RFC3339 string for resets_at. See windowWire.
func (w *Window) UnmarshalJSON(data []byte) error {
	var raw windowWire
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	resetsAt, err := parseResetsAt(raw.ResetsAt)
	if err != nil {
		return fmt.Errorf("decoding resets_at: %w", err)
	}
	w.Utilization = raw.Utilization
	w.ResetsAt = resetsAt
	return nil
}

// parseResetsAt decodes a resets_at value that may be a JSON number (unix
// seconds), a JSON string holding an RFC3339 timestamp, a JSON string
// holding a numeric unix-seconds value, null, or absent.
func parseResetsAt(raw json.RawMessage) (int64, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return 0, nil
	}

	var asNumber int64
	if err := json.Unmarshal(trimmed, &asNumber); err == nil {
		return asNumber, nil
	}

	var asString string
	if err := json.Unmarshal(trimmed, &asString); err != nil {
		return 0, fmt.Errorf("unsupported JSON type %s", trimmed)
	}
	if asString == "" {
		return 0, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, asString); err == nil {
		return t.Unix(), nil
	}
	if n, err := strconv.ParseInt(asString, 10, 64); err == nil {
		return n, nil
	}
	return 0, fmt.Errorf("unrecognized format %q", asString)
}

// LimitScopeModel narrows a LimitScope to one model, by display name
// and/or id. Verified 2026-09-10 against a live response: the only
// populated field for the observed "Fable" weekly cap was DisplayName.
type LimitScopeModel struct {
	ID          *string `json:"id"`
	DisplayName *string `json:"display_name"`
}

// LimitScope narrows a `limits` array entry (LimitEntry) to one model
// and/or one surface. nil on an account-wide entry (e.g. "session",
// "weekly_all"); populated on a model- or surface-scoped entry (e.g.
// "weekly_scoped").
type LimitScope struct {
	Model   *LimitScopeModel `json:"model"`
	Surface *string          `json:"surface"`
}

// Label returns the human display label for a scope: the model's
// display name, else its id, else the surface, else "" when s is nil or
// nothing identifies the scope. Safe to call on a nil *LimitScope.
func (s *LimitScope) Label() string {
	if s == nil {
		return ""
	}
	if s.Model != nil {
		if s.Model.DisplayName != nil && *s.Model.DisplayName != "" {
			return *s.Model.DisplayName
		}
		if s.Model.ID != nil && *s.Model.ID != "" {
			return *s.Model.ID
		}
	}
	if s.Surface != nil && *s.Surface != "" {
		return *s.Surface
	}
	return ""
}

// Identity returns a stable key distinguishing this scope for
// window_kind purposes -- unlike Label(), which is display-only and
// drops the surface whenever a model is present: a scope narrowing to
// the same model on two different surfaces (or vice versa) would
// otherwise report the same Label() and collide onto one WindowKind,
// merging two distinct limits into one current card and, whenever their
// reset times also happen to match, overwriting one with the other at
// dedup time (roborev finding). Model and surface combine only when
// both are populated; with just one present (every scope observed so
// far, kata eabb) this returns exactly what Label() would, so it does
// not change WindowKind for any currently-known response shape. Safe to
// call on a nil *LimitScope.
func (s *LimitScope) Identity() string {
	if s == nil {
		return ""
	}
	var model string
	if s.Model != nil {
		if s.Model.ID != nil && *s.Model.ID != "" {
			model = *s.Model.ID
		} else if s.Model.DisplayName != nil && *s.Model.DisplayName != "" {
			model = *s.Model.DisplayName
		}
	}
	var surface string
	if s.Surface != nil && *s.Surface != "" {
		surface = *s.Surface
	}
	switch {
	case model != "" && surface != "":
		return model + ":" + surface
	case model != "":
		return model
	default:
		return surface
	}
}

// LimitEntry is one entry in the oauth/usage response's top-level
// `limits` array. Verified 2026-09-10 against a live response with a
// real account (kata eabb): this array is the primary, and as of this
// writing the *only*, source for a model-scoped weekly cap (the
// account's one "Fable" weekly limit) -- it never appears under a fixed
// seven_day_opus/seven_day_sonnet key, which were both null. See
// docs/internal/session-format-sources.md for the observed shape.
type LimitEntry struct {
	// Kind is e.g. "session", "weekly_all", "weekly_scoped". Free-form;
	// new kinds may appear without notice.
	Kind string `json:"kind"`
	// Group is e.g. "session" or "weekly"; used to derive WindowMinutes.
	Group    string          `json:"group"`
	Percent  float64         `json:"percent"`
	Severity string          `json:"severity"`
	ResetsAt json.RawMessage `json:"resets_at"`
	// Scope narrows this entry to one model/surface; nil for an
	// account-wide entry.
	Scope *LimitScope `json:"scope"`
	// IsActive marks which window is currently the binding/gating one,
	// not which windows are current or displayable: on the verified
	// live fixture (kata eabb, realOauthUsageLimitsArrayFixture) the
	// account's real, currently-relevant weekly_all AND weekly_scoped
	// ("Fable") entries both report is_active: false while only the
	// session entry reports true, so treating false as "hide this
	// window" would drop the weekly cards users most want to see (a
	// roborev-ci finding suggested exactly that; job.go and
	// rateLimitWindowFromRow deliberately store IsActive as inert
	// display metadata in Details rather than filtering on it -- see
	// TestJobRun_RetiresScopedLimitsWindowRemovedFromLaterPoll's
	// windowsBefore assertion for the regression that would catch a
	// future attempt to filter on it).
	IsActive bool `json:"is_active"`
}

// canonicalAccountWideWindowKind maps a `limits` array entry's raw Kind,
// for an account-wide (unscoped) entry only, to the shared window_kind
// identity Claude's other two ingestion paths (the statusLine
// statusline-sink, and the fixed-bucket fallback used when `limits` is
// empty) also normalize to for the same underlying window -- "session"
// for the 5-hour window, "weekly" for the 7-day all-models window.
// Without this, the same window is stored under different identities
// depending on which path observed it: current snapshots grouped by the
// exact window_kind produce a duplicate card for one, and a poller-only
// user upgrading keeps a permanently stale card alongside rows from a
// different path (roborev finding on kata eabb/qs2f, filed as kata
// j5md #1). "session" is already the shared identity, so it needs no
// entry here; a kind not in this map (a scoped entry, or a future
// code-named group with no shared equivalent) keeps its own raw Kind.
var canonicalAccountWideWindowKind = map[string]string{
	"weekly_all": "weekly",
}

// WindowKind returns this entry's rate_limit_snapshots window_kind:
// "<Kind>:<scope identity>" for a scoped entry (e.g. "weekly_scoped:Fable"),
// or the canonical shared name for an account-wide entry whose Kind has
// one (see canonicalAccountWideWindowKind), else the raw Kind. Uses
// Scope.Identity(), not Scope.Label(): two scopes narrowing to the same
// model on different surfaces (or the reverse) must not collapse onto
// one window_kind the way Label() alone would (roborev finding).
func (e LimitEntry) WindowKind() string {
	if identity := e.Scope.Identity(); identity != "" {
		return e.Kind + ":" + identity
	}
	if canonical, ok := canonicalAccountWideWindowKind[e.Kind]; ok {
		return canonical
	}
	return e.Kind
}

// WindowMinutes maps this entry's Group to a window length: 300 (5h) for
// "session", 10080 (7d) for "weekly", 0 (unknown) for anything else.
func (e LimitEntry) WindowMinutes() int {
	switch e.Group {
	case "session":
		return 300
	case "weekly":
		return 10080
	default:
		return 0
	}
}

// MoneyAmount is a minor-unit currency amount, e.g. {amount_minor:
// 110000, currency: "USD", exponent: 2} for $1,100.00.
type MoneyAmount struct {
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	Exponent    int    `json:"exponent"`
}

// Spend is the oauth/usage response's top-level `spend` object. Only
// Limit is currently read (kata eabb: the extra-usage monthly cap's
// monetary limit); the rest of the object (used, percent, severity,
// ...) is not decoded.
type Spend struct {
	Limit *MoneyAmount `json:"limit"`
}

// ExtraUsage is the oauth/usage response's top-level `extra_usage`
// object: a per-account monthly usage-credit allowance separate from
// the fixed/scoped rate-limit windows above. Verified 2026-09-10 against
// a live response at 96.14% utilization.
type ExtraUsage struct {
	IsEnabled         bool    `json:"is_enabled"`
	Utilization       float64 `json:"utilization"`
	SpendLimitReached bool    `json:"spend_limit_reached"`
}

// UsageResponse is the decoded GET /api/oauth/usage response. Every
// fixed window bucket is independently nullable: verified against the
// Claude Code 2.1.266 bundle (see
// docs/internal/session-format-sources.md). As of 2026-09-10, live
// responses put the only model-scoped weekly cap an account has under
// Limits rather than a fixed key (SevenDayOpus/SevenDaySonnet were both
// null for that account) -- internal/claude.Job treats Limits as the
// primary source and only falls back to the fixed buckets when it is
// empty. SpendLimit and Overage are the bundle-documented (but not, as
// of this writing, observed live) shape for a gateway spend cap; Spend
// and ExtraUsage are what live responses actually carry.
type UsageResponse struct {
	FiveHour                *Window `json:"five_hour"`
	SevenDay                *Window `json:"seven_day"`
	SevenDayOpus            *Window `json:"seven_day_opus"`
	SevenDaySonnet          *Window `json:"seven_day_sonnet"`
	SevenDayOAuthApps       *Window `json:"seven_day_oauth_apps"`
	SevenDayOverageIncluded *Window `json:"seven_day_overage_included"`

	Limits     []LimitEntry `json:"limits,omitempty"`
	ExtraUsage *ExtraUsage  `json:"extra_usage,omitempty"`
	Spend      *Spend       `json:"spend,omitempty"`

	SpendLimit *Window `json:"spend_limit,omitempty"`
	Overage    *Window `json:"overage,omitempty"`
}

// HasLimits reports whether the Limits array is present and non-empty,
// which internal/claude.Job treats as the signal to use it as the
// primary rate-limit source instead of the six fixed buckets.
func (r UsageResponse) HasLimits() bool {
	return len(r.Limits) > 0
}

// Windows returns the six fixed-key utilization windows as a
// (window_kind, *Window) list, skipping any that are nil, in the display
// order Claude Code uses: session, weekly (all models), Opus, Sonnet,
// OAuth apps, overage-included. This is the fallback source: prefer
// Limits (via HasLimits) when present -- see the UsageResponse doc
// comment.
//
// FiveHour/SevenDay are emitted under the shared "session"/"weekly"
// window_kind identity (not "five_hour"/"seven_day") so a window is
// stored the same way regardless of whether this fallback, the `limits`
// array (LimitEntry.WindowKind), or the statusline sink observed it --
// see canonicalAccountWideWindowKind (kata j5md #1).
func (r UsageResponse) Windows() []struct {
	Kind   string
	Window Window
} {
	var out []struct {
		Kind   string
		Window Window
	}
	add := func(kind string, w *Window) {
		if w != nil {
			out = append(out, struct {
				Kind   string
				Window Window
			}{Kind: kind, Window: *w})
		}
	}
	add("session", r.FiveHour)
	add("weekly", r.SevenDay)
	add("seven_day_opus", r.SevenDayOpus)
	add("seven_day_sonnet", r.SevenDaySonnet)
	add("seven_day_oauth_apps", r.SevenDayOAuthApps)
	add("seven_day_overage_included", r.SevenDayOverageIncluded)
	return out
}

// canonicalWindowMinutes maps a shared Claude account-wide window_kind
// identity to its known duration in minutes, for ingestion paths that
// receive no window length directly from the server: the fixed-bucket
// fallback (Windows(), used only when `limits` is absent) and the
// statusLine sink (cmd/agentsview's `claude statusline-sink`, which only
// ever sees the five_hour/seven_day pair). The `limits` array's own
// LimitEntry.WindowMinutes method needs no equivalent -- it derives the
// length from the entry's own Group field.
//
// Without this, a session/weekly card's window_minutes stayed 0 when
// written by either of these two paths, and RateLimitCard fell back to
// a duration-less generic label ("— limit") instead of "Session limit"/
// "Weekly limit" -- so a window's title flipped depending on which
// source most recently observed it (roborev finding on kata ztf4,
// following up on kata j5md #1's window-kind normalization).
var canonicalWindowMinutes = map[string]int{
	"session":                    300,
	"weekly":                     10080,
	"seven_day_opus":             10080,
	"seven_day_sonnet":           10080,
	"seven_day_oauth_apps":       10080,
	"seven_day_overage_included": 10080,
}

// CanonicalWindowMinutes returns the known window length in minutes for
// a shared Claude window_kind identity (see canonicalWindowMinutes), or
// 0 for a kind with no known fixed duration (e.g. "spend_limit", or a
// scoped/code-named `limits` entry whose length comes from
// LimitEntry.WindowMinutes instead).
func CanonicalWindowMinutes(kind string) int {
	return canonicalWindowMinutes[kind]
}

// Client talks to the unofficial Claude Code oauth/usage endpoint. This is
// not a documented, stable Anthropic API; see docs/token-usage.md for the
// "unofficial, may change" caveat this ticket requires.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
}

// NewClient builds a Client with the default base URL and a 15s timeout.
func NewClient() *Client {
	return &Client{
		BaseURL:    DefaultBaseURL,
		HTTPClient: &http.Client{Timeout: 15 * time.Second},
	}
}

func (c *Client) baseURL() string {
	if c == nil || c.BaseURL == "" {
		return DefaultBaseURL
	}
	return c.BaseURL
}

func (c *Client) httpClient() *http.Client {
	if c != nil && c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// ErrUnauthorized is returned on a 401 response: the token is invalid or
// expired. The caller must not attempt to refresh it -- only re-running
// `claude` refreshes OAuth tokens (docs/token-usage.md).
var ErrUnauthorized = fmt.Errorf("claude oauth/usage: unauthorized (re-run `claude` to refresh credentials)")

// RateLimitedError is returned on a 429 response. RetryAfter is the
// server's Retry-After hint, parsed as seconds; zero when the header was
// absent or unparsable.
type RateLimitedError struct {
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("claude oauth/usage: rate limited (retry after %s)", e.RetryAfter)
}

// FetchUsage calls GET /api/oauth/usage with accessToken and decodes the
// response. The header set (Authorization, anthropic-beta, User-Agent,
// Content-Type) mirrors exactly what Claude Code itself sends; see the
// package-level doc comment and docs/internal/session-format-sources.md.
func (c *Client) FetchUsage(ctx context.Context, accessToken string) (*UsageResponse, error) {
	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet, c.baseURL()+"/api/oauth/usage", nil,
	)
	if err != nil {
		return nil, fmt.Errorf("building oauth/usage request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("anthropic-beta", oauthBetaHeader)
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("requesting oauth/usage: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("reading oauth/usage response: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
		var out UsageResponse
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, fmt.Errorf("decoding oauth/usage response: %w", err)
		}
		return &out, nil
	case http.StatusUnauthorized:
		return nil, ErrUnauthorized
	case http.StatusTooManyRequests:
		return nil, &RateLimitedError{RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	default:
		return nil, fmt.Errorf(
			"claude oauth/usage: unexpected status %d: %s", resp.StatusCode, truncate(body, 500),
		)
	}
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
