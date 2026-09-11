package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// RateLimitSnapshot is one rate-limit window observed for a vendor at a
// point in time: a Codex token_count event's rate_limits payload, or a
// Claude Code oauth/usage poll (internal/claude.Job). The table is
// vendor-keyed from the start (see docs/agents/storage.md): Vendor
// distinguishes rows, and fields a given vendor never populates
// (AccountID, AccountLabel, ScopeLabel, Severity, Details for Codex;
// SessionID, Machine, LimitID, ObservationKey for Claude) simply stay
// empty on that vendor's rows.
//
// A window's identity is (Vendor, Machine, AccountID, LimitID,
// WindowKind); PlanType and LimitName are display labels, not part of
// it. Codex rollouts carry no stable per-account identifier (verified
// against ~/.codex/sessions rollouts: no account id, user id, email, or
// org field appears in session_meta or token_count payloads; see
// docs/internal/session-format-sources.md), so AccountID/AccountLabel
// stay empty for Codex rows. Claude snapshots key AccountID off
// claude.Identity.AccountKey() -- the oauthAccount accountUuid plus
// organizationUuid read from ~/.claude.json -- since a rate-limit window
// belongs to a seat within an organization: accountUuid alone stays the
// same across every org a user is a member of, and logging into a
// different org changes only organizationUuid/organizationName, so
// accountUuid by itself would collapse both orgs into one
// LatestRateLimitSnapshots group. Claude rows leave SessionID and
// Machine empty, since the poller is not tied to one parsed session or
// host.
type RateLimitSnapshot struct {
	ID int64

	// Vendor is "codex" or "claude". Defaults to "codex" on write for
	// backward compatibility with callers that predate this field.
	Vendor string

	SessionID string // Codex only; empty when the source session has been deleted
	Machine   string // Codex only

	AccountID    string // Claude: claude.Identity.AccountKey() (accountUuid + organizationUuid). Empty for Codex.
	AccountLabel string // Claude: organizationName or emailAddress. Empty for Codex.

	LimitID    string // Codex only
	LimitName  string
	PlanType   string
	WindowKind string // Codex: "primary"/"secondary". Claude: "session", "weekly", or "<kind>:<scope label>" -- see docs/agents/storage.md.

	UsedPercent float64
	// WindowMinutes is 0 both for a window Codex reported with no
	// duration (parser.ParsedRateLimitSnapshot.WindowMinutes nil) and,
	// in principle, a genuine zero-minute window -- the latter never
	// occurs in practice, so 0 doubles as "unknown" without a nullable
	// column; the API layer treats it that way (see
	// service.RateLimitWindow.WindowMinutes). Claude always populates
	// this at write time (see internal/claude, CanonicalWindowMinutes).
	WindowMinutes int
	// ResetsAt is unix seconds, or nil when the vendor did not report a
	// reset time for this window. Kept nullable rather than flattened to
	// 0, so a genuinely unknown reset time is never displayed as if the
	// window resets at the unix epoch.
	ResetsAt *int64

	CreditsHas       bool // Codex only
	CreditsUnlimited bool // Codex only
	CreditsBalance   string

	RateLimitReachedType string // Codex only

	// ScopeLabel, Severity, and Details are Claude-only, sourced from one
	// entry in the oauth/usage response's top-level `limits` array.
	// ScopeLabel is the scope's model display name, else its model id,
	// else its surface (empty for an account-wide entry). Severity is
	// the server-reported severity string (e.g. "normal"). Details is a
	// free-form JSON object carrying whatever else that source needs
	// (is_active, the raw scope object, which decode path produced the
	// row, or -- for the extra_usage_monthly window -- the monetary
	// limit and currency); empty string when nothing extra applies. See
	// docs/agents/storage.md. Always "" for Codex.
	ScopeLabel string
	Severity   string
	Details    string

	ObservedAt string // RFC3339Nano
	// Ordinal is the source Codex token_count event's stable per-file
	// position (see parser.ParsedRateLimitSnapshot.Ordinal), folded into
	// DedupKey and ObservationKey so two events sharing an ObservedAt
	// second stay distinct. Always 0 for Claude, which is polled rather
	// than parsed and so has no per-event ordinal to assign.
	Ordinal  int
	DedupKey string

	// ObservationKey identifies the single source observation a row
	// came from (e.g. one Codex token_count event's rate_limits
	// payload), shared across the up-to-two window rows (primary,
	// secondary) that observation produces. Computed once from
	// SessionID+ObservedAt+LimitID+PlanType at insert time and stored
	// independently of the nullable SessionID column, so it keeps
	// sibling windows grouped together for LatestRateLimitSnapshots
	// even after the source session is deleted or excluded from a
	// resync -- unlike SessionID itself, which is exactly what goes
	// away in that case. Empty for Claude rows, which carry no
	// SessionID.
	ObservationKey string
}

// RateLimitFilter selects rate-limit snapshots by vendor, account, and
// (Codex-only) machine.
type RateLimitFilter struct {
	// Vendor narrows to one or more vendors ("codex" or "claude",
	// comma-separated); empty matches every vendor the table holds.
	Vendor    string
	AccountID string
	Machine   string
	// Agent is the same comma-separated agent selection the shared
	// session filters use (sessions.filters.agent), accepted for
	// backward compatibility with callers that filtered by agent before
	// Vendor existed. When Vendor is unset, Agent is applied directly as
	// the vendor restriction (see rateLimitEffectiveVendor): an agent
	// slug and a rate-limit vendor name are the same value.
	Agent string
}

// RateLimitHistoryFilter selects a time series of rate-limit snapshots,
// optionally narrowed to vendor, account, one limit_id, and/or window
// kind. Machine supports the same comma-separated IN semantics as
// RateLimitFilter.Machine, and Agent is resolved into the vendor
// restriction the same way as RateLimitFilter.Agent (see
// rateLimitEffectiveVendor).
type RateLimitHistoryFilter struct {
	Vendor     string
	AccountID  string
	Machine    string
	Agent      string
	LimitID    string
	WindowKind string
	Since      string // RFC3339Nano, inclusive lower bound; empty means unbounded
	Until      string // RFC3339Nano, exclusive upper bound; empty means unbounded
	// MaxPoints bounds the number of rows returned: the matching range is
	// bucketed in SQL (see RateLimitSnapshotHistory) into at most
	// MaxPoints equal-width time buckets, keeping only the most recent
	// observation in each so the series stays representative of the
	// window's end state without growing the response (and the
	// resulting chart's point count) with the query range. <= 0 uses
	// defaultRateLimitHistoryMaxPoints.
	MaxPoints int
}

// rateLimitMachineClause returns a SQL fragment (starting with " AND")
// and its bind args for filtering rate_limit_snapshots by machine, using
// the same single-value "=" / multi-value "IN" split the other usage
// filters use (see UsageFilter.appendUsageSessionFilterClauses): a lone
// value compares with "=" so the query plan can still use an equality
// index lookup, and two or more values compare with "IN". An empty filter
// returns no clause at all.
//
// The comparison only ever constrains Codex rows: Claude snapshots
// always carry an empty machine (the poller is not tied to one host),
// so applying a machine filter unconditionally hid every Claude card
// whenever any machine was selected, even though the API documents
// machine filtering as Codex-only (roborev finding). "vendor = 'claude'"
// short-circuits the OR before the machine comparison ever runs for
// those rows.
func rateLimitMachineClause(machine string) (string, []any) {
	if machine == "" {
		return "", nil
	}
	vals := strings.Split(machine, ",")
	if len(vals) == 1 {
		return " AND (vendor = 'claude' OR machine = ?)", []any{vals[0]}
	}
	placeholders := make([]string, len(vals))
	args := make([]any, len(vals))
	for i, v := range vals {
		placeholders[i] = "?"
		args[i] = v
	}
	return " AND (vendor = 'claude' OR machine IN (" + strings.Join(placeholders, ",") + "))", args
}

// rateLimitVendorClause returns a SQL fragment (starting with " AND")
// and its bind args restricting rate_limit_snapshots to one or more
// vendors, using the same single-value "=" / multi-value "IN" split as
// rateLimitMachineClause. An empty vendor returns no clause at all,
// matching every vendor the table holds.
func rateLimitVendorClause(vendor string) (string, []any) {
	if vendor == "" {
		return "", nil
	}
	vals := strings.Split(vendor, ",")
	if len(vals) == 1 {
		return " AND vendor = ?", []any{vals[0]}
	}
	placeholders := make([]string, len(vals))
	args := make([]any, len(vals))
	for i, v := range vals {
		placeholders[i] = "?"
		args[i] = v
	}
	return " AND vendor IN (" + strings.Join(placeholders, ",") + ")", args
}

// rateLimitEffectiveVendor resolves the vendor restriction a filter's
// Vendor and Agent fields together imply: an explicit Vendor takes
// precedence; otherwise Agent's comma-separated selection is applied
// directly, since an agent slug (e.g. "codex", "claude") and a
// rate-limit vendor name are the same value domain (see
// RateLimitAgentMatchesVendor). Empty when neither is set, matching
// every vendor the table holds. A selection naming neither known vendor
// (e.g. "gemini") resolves to a vendor clause that matches no stored
// row, which is the same "no rows" outcome as an explicit empty-set
// filter, without needing a separate early-return path.
func rateLimitEffectiveVendor(vendor, agent string) string {
	if vendor != "" {
		return vendor
	}
	return agent
}

// normalizeRateLimitBoundary parses an RFC3339 (or RFC3339Nano) history
// since/until bound and reformats it as a UTC RFC3339Nano string. Request
// bounds cannot be compared against the stored observed_at column as raw
// text: Go's RFC3339Nano formatting trims trailing fractional-second
// zeros to a variable width, so two otherwise-adjacent instants (e.g.
// "...T23:59:59Z" and "...T23:59:59.5Z") do not sort the way their times
// do, and a non-UTC request offset would not compare correctly against
// the always-UTC stored value either. Normalizing both sides to the same
// UTC instant here, and comparing them with SQL julianday() rather than a
// text comparison (see internal/db/search.go's identical julianday()
// rationale) makes the comparison correct regardless of either string's
// formatting or offset. julianday() resolves observed_at to roughly
// millisecond precision (SQLite's ISO8601 parsing keeps at most three
// fractional-second digits), which is well beyond Codex's own
// whole-second event timestamps and the day-boundary bounds this
// endpoint is queried with; it is not exact at sub-millisecond
// resolution. Empty input means unbounded and is returned unchanged.
func normalizeRateLimitBoundary(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return "", fmt.Errorf("parsing rate limit history bound %q: %w", s, err)
	}
	return t.UTC().Format(time.RFC3339Nano), nil
}

// RateLimitAgentMatchesVendor reports whether agent -- a comma-separated
// agent selection such as sessions.filters.agent, or "" for no filter --
// includes vendor. An empty selection (no filter active) matches
// everything, and any non-empty selection must name vendor somewhere in
// the list; a selection of one or more other agents that omits vendor
// (e.g. "claude") matches nothing. A naive exact-equality check here
// would wrongly hide a vendor's rate-limit cards whenever more than one
// agent is selected together with it (e.g. "codex,claude").
func RateLimitAgentMatchesVendor(agent, vendor string) bool {
	if agent == "" {
		return true
	}
	return slices.Contains(strings.Split(agent, ","), vendor)
}

// rateLimitObservedAtMinuteBucket truncates observedAt (RFC3339Nano) to
// the minute in UTC, for RateLimitSnapshotDedupKey's session-less-vendor
// path. Falls back to the raw string on a parse failure (should not
// happen: observed_at is always written by RFC3339Nano formatting), so a
// malformed value still participates in the key rather than panicking.
func rateLimitObservedAtMinuteBucket(observedAt string) string {
	t, err := time.Parse(time.RFC3339Nano, observedAt)
	if err != nil {
		return observedAt
	}
	return t.UTC().Truncate(time.Minute).Format(time.RFC3339)
}

// RateLimitSnapshotDedupKey returns the stable identity for one snapshot
// row. Re-parsing a Codex rollout (full or incremental) or repeating a
// Claude poll produces the same key for the same observation, so the
// unique dedup_key index makes a repeat write an upsert instead of
// duplicating rows (see insertRateLimitSnapshotsTx).
//
// A row with a session id (Codex today) keys on SessionID, ObservedAt,
// LimitID, WindowKind, and Ordinal: session id is already vendor- and
// event-specific, so Vendor, Machine, AccountID, and PlanType would be
// redundant, and Ordinal (the source token_count event's 0-based file
// position) disambiguates two distinct events that happen to share an
// ObservedAt second -- without it, the second event's row would collide
// with, and be silently dropped in favor of, the first's.
//
// A session-less row (Claude, polled rather than parsed, so it has no
// session id or per-event ordinal to key on) instead keys on Vendor,
// Machine, AccountID, LimitID, PlanType, WindowKind, ResetsAt, and
// ObservedAt bucketed to the minute (rateLimitObservedAtMinuteBucket): a
// window's full identity, since nothing else distinguishes two of an
// account's windows observed close together. Bucketing to the minute
// means two polls of the same underlying window state within one
// minute collapse onto one row; a poll in the next minute, or reporting
// a different resets_at, still gets its own row. Because the dedup key
// excludes UsedPercent, insertRateLimitSnapshotsTx follows every
// duplicate hit with an UPDATE refreshing the row's mutable fields, so a
// climbing used_percent within the same bucket is never left stale.
func RateLimitSnapshotDedupKey(s RateLimitSnapshot) string {
	if s.SessionID != "" {
		sum := sha256.Sum256([]byte(fmt.Sprintf(
			"%s|%s|%s|%s|%d",
			s.SessionID, s.ObservedAt, s.LimitID, s.WindowKind, s.Ordinal,
		)))
		return hex.EncodeToString(sum[:])
	}
	vendor := s.Vendor
	if vendor == "" {
		vendor = "codex"
	}
	var resetsAt int64
	if s.ResetsAt != nil {
		resetsAt = *s.ResetsAt
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf(
		"%s|%s|%s|%s|%s|%s|%d|%s",
		vendor, s.Machine, s.AccountID, s.LimitID, s.PlanType, s.WindowKind,
		resetsAt, rateLimitObservedAtMinuteBucket(s.ObservedAt),
	)))
	return hex.EncodeToString(sum[:])
}

// InsertRateLimitSnapshots appends new rate-limit snapshot rows and
// ignores duplicates with the same dedup key. It never deletes existing
// rows, so it is safe to call from both a full parse (which sees the
// whole transcript on every run) and an incremental parse (which only
// sees the appended tail), or a repeated Claude poll: all converge on
// the same set of rows. A write that instead replaces a session's prior
// content wholesale -- most notably an authoritative reparse superseding
// a fallback marked parser.DataVersionNeedsRetry -- must use
// InsertRateLimitSnapshotsReplacingSession instead, or that fallback's
// rows outlive the parse that superseded them.
func (db *DB) InsertRateLimitSnapshots(
	snapshots []RateLimitSnapshot,
) error {
	return db.InsertRateLimitSnapshotsContext(
		context.Background(), snapshots,
	)
}

// InsertRateLimitSnapshotsContext is InsertRateLimitSnapshots bound to
// ctx. It opens and commits its own transaction.
func (db *DB) InsertRateLimitSnapshotsContext(
	ctx context.Context, snapshots []RateLimitSnapshot,
) error {
	if len(snapshots) == 0 {
		return nil
	}
	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning rate limit snapshots tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := insertRateLimitSnapshotsTx(ctx, tx, snapshots); err != nil {
		return err
	}
	return tx.Commit()
}

// InsertRateLimitSnapshotsReplacingSession deletes every existing
// rate_limit_snapshots row for sessionID and inserts snapshots in the
// same transaction. Use this instead of InsertRateLimitSnapshots at a
// write that replaces a session's prior content wholesale (the same
// path ReplaceSessionMessages/ReplaceSessionContent use for an
// authoritative reparse): rows are otherwise append-only (see
// InsertRateLimitSnapshots), so a fallback parse marked
// parser.DataVersionNeedsRetry can leave stale observations behind once
// a later authoritative parse supersedes it -- neither dedup_key nor a
// plain append ever removes them. A normal incremental parse (the same
// data version, appending only the newly parsed tail) must keep calling
// InsertRateLimitSnapshots, which never deletes.
func (db *DB) InsertRateLimitSnapshotsReplacingSession(
	sessionID string, snapshots []RateLimitSnapshot,
) error {
	return db.InsertRateLimitSnapshotsReplacingSessionContext(
		context.Background(), sessionID, snapshots,
	)
}

// InsertRateLimitSnapshotsReplacingSessionContext is
// InsertRateLimitSnapshotsReplacingSession bound to ctx.
func (db *DB) InsertRateLimitSnapshotsReplacingSessionContext(
	ctx context.Context, sessionID string, snapshots []RateLimitSnapshot,
) error {
	if sessionID == "" {
		return db.InsertRateLimitSnapshotsContext(ctx, snapshots)
	}
	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf(
			"beginning rate limit snapshots replace tx: %w", err,
		)
	}
	defer func() { _ = tx.Rollback() }()

	if err := deleteRateLimitSnapshotsForSessionTx(tx, sessionID); err != nil {
		return err
	}
	if err := insertRateLimitSnapshotsTx(ctx, tx, snapshots); err != nil {
		return err
	}
	return tx.Commit()
}

// deleteRateLimitSnapshotsForSessionTx deletes every rate_limit_snapshots
// row belonging to sessionID. Called before inserting a fresh set from a
// parse that replaces a session's prior content wholesale, so a
// superseded fallback parse's rows (e.g. one marked
// parser.DataVersionNeedsRetry) cannot survive alongside the later
// authoritative parse's rows.
func deleteRateLimitSnapshotsForSessionTx(
	tx transactionQueries, sessionID string,
) error {
	if _, err := tx.Exec(
		"DELETE FROM rate_limit_snapshots WHERE session_id = ?", sessionID,
	); err != nil {
		return fmt.Errorf(
			"deleting rate limit snapshots for %s: %w", sessionID, err,
		)
	}
	return nil
}

// insertRateLimitSnapshotsTx inserts every row in snapshots that carries
// its required identity fields, skipping -- rather than failing the
// whole batch, and with it the session write this batch is usually part
// of -- any single entry missing one. limit_id is required only for a
// Codex row: Claude's identity has no limit_id concept at all and always
// leaves it empty. A source-format quirk or parser bug that produces one
// malformed rate_limits observation must not take down every other
// snapshot in the same batch, nor the session ingestion it rode in on;
// see docs/agents/storage.md.
//
// A dedup_key collision against an existing row whose session_id is NULL
// reattaches that row to the incoming session_id and refreshes the
// fields the INSERT's own ON CONFLICT ... DO UPDATE covers, instead of
// being ignored: a resync copy preserves dedup_key unchanged for a row
// whose session was not restored (see CopyRateLimitSnapshotsFrom), so
// that session's later reappearance (a fresh parse reproducing the
// identical dedup_key) would otherwise collide with, and lose to, the
// stale detached row forever. That reattachment is deliberately also
// conditioned on the incoming row itself carrying a session_id
// (excluded.session_id IS NOT NULL): a Claude row never carries one, so
// without this a repeat Claude poll would also take this branch, but
// unlike the unconditional pass below it applies no julianday guard at
// all, letting an out-of-order or stale replay silently regress
// used_percent and the rest of its SET list back to an older value
// (roborev finding). The condition leaves reattachment firing only for
// a genuine "session reappeared" event and leaves every Claude
// duplicate hit, and the fields this UPDATE does not cover regardless
// (observed_at, severity, ...), to the unconditional, correctly-guarded
// pass below. A collision against a row that already has a session
// attached (the common Codex case: re-parsing an unchanged event) is a
// no-op either way.
func insertRateLimitSnapshotsTx(
	ctx context.Context, tx transactionQueries, snapshots []RateLimitSnapshot,
) error {
	prepared := make([]RateLimitSnapshot, 0, len(snapshots))
	for _, snap := range snapshots {
		if snap.Vendor == "" {
			snap.Vendor = "codex"
		}
		if snap.WindowKind == "" || snap.ObservedAt == "" {
			continue
		}
		if snap.Vendor == "codex" && snap.LimitID == "" {
			continue
		}
		if snap.DedupKey == "" {
			snap.DedupKey = RateLimitSnapshotDedupKey(snap)
		}
		if snap.ObservationKey == "" {
			snap.ObservationKey = RateLimitObservationKey(snap)
		}
		prepared = append(prepared, snap)
	}

	for _, snap := range prepared {
		if err := ctx.Err(); err != nil {
			return err
		}

		var sessionID any
		if snap.SessionID != "" {
			sessionID = snap.SessionID
		}
		var resetsAt any
		if snap.ResetsAt != nil {
			resetsAt = *snap.ResetsAt
		}
		creditsHas := 0
		if snap.CreditsHas {
			creditsHas = 1
		}
		creditsUnlimited := 0
		if snap.CreditsUnlimited {
			creditsUnlimited = 1
		}

		if _, err := tx.Exec(`
			INSERT INTO rate_limit_snapshots (
				vendor, session_id, machine, account_id, account_label,
				limit_id, limit_name, plan_type, window_kind,
				used_percent, window_minutes, resets_at,
				credits_has, credits_unlimited, credits_balance,
				rate_limit_reached_type, scope_label, severity, details,
				observed_at, ordinal, dedup_key, observation_key
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(dedup_key) WHERE dedup_key != '' DO UPDATE SET
				session_id = excluded.session_id,
				machine = excluded.machine,
				account_id = excluded.account_id,
				account_label = excluded.account_label,
				limit_name = excluded.limit_name,
				plan_type = excluded.plan_type,
				used_percent = excluded.used_percent,
				window_minutes = excluded.window_minutes,
				resets_at = excluded.resets_at,
				credits_has = excluded.credits_has,
				credits_unlimited = excluded.credits_unlimited,
				credits_balance = excluded.credits_balance,
				rate_limit_reached_type = excluded.rate_limit_reached_type,
				scope_label = excluded.scope_label,
				details = excluded.details,
				observation_key = excluded.observation_key
			WHERE rate_limit_snapshots.session_id IS NULL
				AND excluded.session_id IS NOT NULL`,
			snap.Vendor, sessionID, SanitizeUTF8(snap.Machine),
			SanitizeUTF8(snap.AccountID), SanitizeUTF8(snap.AccountLabel),
			SanitizeUTF8(snap.LimitID), SanitizeUTF8(snap.LimitName),
			SanitizeUTF8(snap.PlanType), snap.WindowKind,
			snap.UsedPercent, snap.WindowMinutes, resetsAt,
			creditsHas, creditsUnlimited, SanitizeUTF8(snap.CreditsBalance),
			SanitizeUTF8(snap.RateLimitReachedType), SanitizeUTF8(snap.ScopeLabel),
			SanitizeUTF8(snap.Severity), SanitizeUTF8(snap.Details),
			snap.ObservedAt, snap.Ordinal, snap.DedupKey, snap.ObservationKey,
		); err != nil {
			return fmt.Errorf("inserting rate limit snapshot: %w", err)
		}
	}

	// A row can dedup away even though it was genuinely re-observed,
	// simply because none of its dedup-key fields (which exclude
	// used_percent) changed within the same minute bucket as before --
	// e.g. a Claude poll reports the same session/weekly windows with
	// the same resets_at as the last poll this minute, but usage climbed
	// from 90% to 100%; nothing about the row's identity moved, but its
	// value did. The INSERT above only refreshes a duplicate hit's
	// fields when its existing session_id is NULL, and does not cover
	// observed_at or severity at all (see its own doc comment), so this
	// pass runs unconditionally for every prepared row -- including one
	// this same call just inserted a moment ago, for which it is a
	// harmless no-op rewrite of identical values -- rather than trying
	// to detect which rows were freshly inserted, refreshing the
	// existing row's mutable fields to this call's incoming values,
	// guarded by julianday comparison so a replay of an older
	// observation (a resync, or an out-of-order batch) can never
	// regress a row past a newer one already stored under the same
	// dedup key.
	for _, snap := range prepared {
		if err := ctx.Err(); err != nil {
			return err
		}
		var resetsAt any
		if snap.ResetsAt != nil {
			resetsAt = *snap.ResetsAt
		}
		creditsHas := 0
		if snap.CreditsHas {
			creditsHas = 1
		}
		creditsUnlimited := 0
		if snap.CreditsUnlimited {
			creditsUnlimited = 1
		}
		observationKey := RateLimitObservationKey(snap)
		if _, err := tx.Exec(`
			UPDATE rate_limit_snapshots SET
				used_percent = ?, window_minutes = ?, resets_at = ?,
				credits_has = ?, credits_unlimited = ?, credits_balance = ?,
				rate_limit_reached_type = ?, scope_label = ?, severity = ?, details = ?,
				limit_name = ?, plan_type = ?, observed_at = ?, observation_key = ?
			WHERE dedup_key = ? AND julianday(?) >= julianday(observed_at)`,
			snap.UsedPercent, snap.WindowMinutes, resetsAt,
			creditsHas, creditsUnlimited, SanitizeUTF8(snap.CreditsBalance),
			SanitizeUTF8(snap.RateLimitReachedType), SanitizeUTF8(snap.ScopeLabel),
			SanitizeUTF8(snap.Severity), SanitizeUTF8(snap.Details),
			SanitizeUTF8(snap.LimitName), SanitizeUTF8(snap.PlanType),
			snap.ObservedAt, observationKey, snap.DedupKey, snap.ObservedAt,
		); err != nil {
			return fmt.Errorf("refreshing deduped rate limit snapshot: %w", err)
		}
	}
	return nil
}

// RateLimitObservationKey returns the stable identity of the single
// source observation s came from (e.g. one Codex token_count event's
// rate_limits payload), shared by the up-to-two window rows (primary,
// secondary) that observation produces. It is computed once at insert
// time from fields that do not change afterward -- unlike SessionID,
// which InsertRateLimitSnapshots/CopyRateLimitSnapshotsFrom can later
// leave NULL for a deleted or unrestored session -- so storing it in
// its own column keeps sibling windows grouped for
// LatestRateLimitSnapshots regardless of what later happens to the
// source session. Ordinal participates for the same reason it does in
// RateLimitSnapshotDedupKey: two distinct token_count events at the same
// SessionID+ObservedAt+LimitID+PlanType would otherwise share one
// observation_key, merging their sibling windows into a single bucket
// for LatestRateLimitSnapshots. Empty when SessionID is empty (Claude
// rows carry no SessionID); callers fall back to a per-row key in that
// case.
func RateLimitObservationKey(s RateLimitSnapshot) string {
	if s.SessionID == "" {
		return ""
	}
	return fmt.Sprintf(
		"%s|%s|%s|%s|%d",
		s.SessionID, s.ObservedAt, s.LimitID, s.PlanType, s.Ordinal,
	)
}

const rateLimitSnapshotColumns = `
	id, vendor, session_id, machine, account_id, account_label,
	limit_id, limit_name, plan_type, window_kind,
	used_percent, window_minutes, resets_at,
	credits_has, credits_unlimited, credits_balance,
	rate_limit_reached_type, scope_label, severity, details,
	observed_at, ordinal, dedup_key, observation_key`

func scanRateLimitSnapshot(row interface{ Scan(...any) error }) (RateLimitSnapshot, error) {
	var s RateLimitSnapshot
	var sessionID *string
	var resetsAt *int64
	var creditsHas, creditsUnlimited int
	if err := row.Scan(
		&s.ID, &s.Vendor, &sessionID, &s.Machine, &s.AccountID, &s.AccountLabel,
		&s.LimitID, &s.LimitName, &s.PlanType, &s.WindowKind,
		&s.UsedPercent, &s.WindowMinutes, &resetsAt,
		&creditsHas, &creditsUnlimited, &s.CreditsBalance,
		&s.RateLimitReachedType, &s.ScopeLabel, &s.Severity, &s.Details,
		&s.ObservedAt, &s.Ordinal, &s.DedupKey, &s.ObservationKey,
	); err != nil {
		return RateLimitSnapshot{}, err
	}
	if sessionID != nil {
		s.SessionID = *sessionID
	}
	s.ResetsAt = resetsAt
	s.CreditsHas = creditsHas != 0
	s.CreditsUnlimited = creditsUnlimited != 0
	return s, nil
}

// hasRateLimitSnapshotsTable reports whether this archive's
// rate_limit_snapshots table exists, probing sqlite_master once per DB
// instance and caching the result. A writable Open always creates the
// table (schema.sql's CREATE TABLE IF NOT EXISTS), but OpenReadOnly runs
// no migrations, so an older, otherwise-compatible archive opened
// read-only can legitimately predate the table (see
// docs/agents/storage.md and CopyRateLimitSnapshotsFrom's identical
// sqlite_master check). A failed probe is not cached, so a transient
// error is retried on the next call rather than permanently treated as
// "missing".
func (db *DB) hasRateLimitSnapshotsTable() bool {
	db.rateLimitSnapshotsTableMu.Lock()
	defer db.rateLimitSnapshotsTableMu.Unlock()
	if db.rateLimitSnapshotsTableProbed {
		return db.rateLimitSnapshotsTableFound
	}
	var exists bool
	if err := db.getReader().QueryRow(`SELECT EXISTS(
		SELECT 1 FROM sqlite_master
		WHERE type = 'table' AND name = 'rate_limit_snapshots'
	)`).Scan(&exists); err != nil {
		return false
	}
	db.rateLimitSnapshotsTableProbed = true
	db.rateLimitSnapshotsTableFound = exists
	return exists
}

// isMissingRateLimitSnapshotsColumnErr reports whether err is a SQLite
// "no such column" failure against rate_limit_snapshots.
// hasRateLimitSnapshotsTable only tolerates an archive that predates the
// table entirely; severity (Claude-only) is not part of every release's
// base schema, so an archive that already has the table but has never
// been opened writably since severity was added can still lack it.
// LatestRateLimitSnapshots and RateLimitSnapshotHistory treat that the
// same way as a missing table: an empty result rather than an error, so
// a read-only viewer of a not-yet-migrated archive degrades gracefully
// instead of failing outright.
func isMissingRateLimitSnapshotsColumnErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such column")
}

// rateLimitSnapshotDisabled reports whether s is explicitly marked
// disabled in its details JSON. internal/claude.Job writes such a row
// to retire a Claude window that resolves its own latest observation
// independently of any other Claude window (see
// rateLimitBucketWindowKindExpr), where nothing else would ever
// supersede a stale row on its own: an extra_usage_monthly window once
// the account turns the feature off or stops reporting it, and any
// other Claude window once a poll's `limits` array no longer reports it
// (see claude.Job.retireStaleClaudeWindows). RateLimitSnapshotHistory
// does not call this -- a chart covering the disable/retirement event
// should still show the transition, not silently truncate it.
func rateLimitSnapshotDisabled(s RateLimitSnapshot) bool {
	if s.Details == "" {
		return false
	}
	var parsed struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.Unmarshal([]byte(s.Details), &parsed); err != nil {
		return false
	}
	return parsed.Enabled != nil && !*parsed.Enabled
}

// rateLimitBucketKey identifies one LatestRateLimitSnapshots grouping
// bucket.
type rateLimitBucketKey struct {
	vendor, machine, accountID, limitID, bucketWindowKind string
}

// rateLimitBucketWindowKindExpr is the SQL expression that folds
// window_kind into the bucket key only for Claude rows, matched by every
// query below that groups or seeks rate_limit_snapshots buckets. Codex's
// window kinds (primary/secondary) are two granularities of one payload
// that arrive and vanish together, so ranking them independently would
// let a window that stops being reported (e.g. a session moving from
// primary+secondary to primary-only) keep surfacing its last-known row
// forever, since no newer row for that window_kind ever arrives to
// supersede it (roborev finding on kata ce4t) -- a constant ” folds
// them into one bucket, preserving that grouping. Claude's window kinds
// (session, weekly, the scoped/extra_usage windows) are independently
// observed facts about one account instead, and Claude's limit_id is
// always empty, so excluding window_kind from a Claude bucket would
// collapse every one of an account's windows into a single bucket
// showing only whichever was most recently polled.
const rateLimitBucketWindowKindExpr = "CASE WHEN vendor = 'claude' THEN window_kind ELSE '' END"

// rateLimitBuckets discovers every distinct (vendor, machine, account_id,
// limit_id, bucket_window_kind) bucket matching whereClause/whereArgs by
// repeatedly seeking the next key greater than the last one found, via
// the row-value comparison SQLite compiles into an index seek against
// idx_rate_limit_snapshots_bucket_observed. A single "GROUP BY ...
// MAX(observed_at)" query over the same index costs one comparison per
// matching row -- confirmed via EXPLAIN QUERY PLAN to be a full covering
// index scan, not a seek -- which dominates LatestRateLimitSnapshots' cost
// at scale even though the number of distinct buckets a real archive
// holds stays small (one per machine/limit family, or per Claude window)
// regardless of how much history has accumulated. This walk instead
// costs one O(log n) seek per bucket.
func (db *DB) rateLimitBuckets(
	ctx context.Context, whereClause string, whereArgs []any,
) ([]rateLimitBucketKey, error) {
	var out []rateLimitBucketKey
	var vendor, machine, accountID, limitID, bucketWindowKind string
	for {
		args := append([]any{vendor, machine, accountID, limitID, bucketWindowKind}, whereArgs...)
		row := db.getReader().QueryRowContext(ctx, `
			SELECT vendor, machine, account_id, limit_id, `+rateLimitBucketWindowKindExpr+`
			FROM rate_limit_snapshots
			WHERE (vendor, machine, account_id, limit_id, `+rateLimitBucketWindowKindExpr+`) > (?, ?, ?, ?, ?)`+whereClause+`
			ORDER BY vendor, machine, account_id, limit_id, `+rateLimitBucketWindowKindExpr+`
			LIMIT 1`,
			args...,
		)
		var next rateLimitBucketKey
		if err := row.Scan(
			&next.vendor, &next.machine, &next.accountID, &next.limitID, &next.bucketWindowKind,
		); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				break
			}
			return nil, fmt.Errorf("discovering rate limit buckets: %w", err)
		}
		out = append(out, next)
		vendor, machine, accountID, limitID, bucketWindowKind =
			next.vendor, next.machine, next.accountID, next.limitID, next.bucketWindowKind
	}
	return out, nil
}

// LatestRateLimitSnapshots returns the most recently observed row per
// (vendor, machine, account_id, limit_id) bucket -- and, for Claude
// rows only, per window_kind within that bucket too (see
// rateLimitBucketWindowKindExpr) -- matching f. plan_type and limit_name
// are resolved independently as the latest non-empty value ever observed
// for the (vendor, machine, account_id, limit_id) limit rather than
// taken from the winning observation, since Codex reports both as labels
// that can flip to empty on a later observation without that observation
// being a meaningful "unset" (kata k5bm), and deliberately not scoped to
// window_kind too: a primary-only observation supplying a new label,
// followed by a primary+secondary observation that omits it, would
// otherwise leave secondary showing a stale or blank label while primary
// shows the new one. "Most recently observed" is decided by
// julianday(observed_at), not a raw text comparison: see
// normalizeRateLimitBoundary's doc comment for why. Buckets are
// discovered with rateLimitBuckets, then each bucket's winning row and
// its plan_type/limit_name labels are resolved with one small indexed
// query per bucket, so cost stays close to the number of buckets rather
// than the number of rows.
//
// The winner query matches on obs_key, not observed_at alone: two
// different sessions on the same machine and account can legitimately
// report the same limit at the exact same instant (Codex's rate limit is
// account-wide), so it also matches on observation_key -- a value
// computed once at insert time (see RateLimitObservationKey) and stored
// independently of the nullable session_id column, so it keeps sibling
// windows grouped correctly even after a source session is deleted or
// excluded from a resync, unlike a key derived from session_id at query
// time. An empty observation_key (Claude rows, which carry no
// session_id) falls back to a synthetic 'row:<id>' key unique to that
// one row.
//
// A row explicitly marked disabled in its details JSON (see
// rateLimitSnapshotDisabled) is excluded from the result: Claude writes
// such a row to retire a window that would otherwise never be
// superseded on its own (extra_usage_monthly once a feature turns off,
// or any window that stops appearing in a `limits` poll).
// RateLimitSnapshotHistory does not apply this filter, so a chart
// covering the retirement event still shows the transition.
//
// An archive opened read-only from before this table existed returns an
// empty result instead of a "no such table" error (see
// hasRateLimitSnapshotsTable), and one predating a later column (e.g.
// severity) an empty result instead of a "no such column" error (see
// isMissingRateLimitSnapshotsColumnErr).
func (db *DB) LatestRateLimitSnapshots(
	ctx context.Context, f RateLimitFilter,
) ([]RateLimitSnapshot, error) {
	if !db.hasRateLimitSnapshotsTable() {
		return nil, nil
	}
	machineClause, machineArgs := rateLimitMachineClause(f.Machine)
	vendorClause, vendorArgs := rateLimitVendorClause(rateLimitEffectiveVendor(f.Vendor, f.Agent))
	accountClause := ""
	var accountArgs []any
	if f.AccountID != "" {
		// account_id scopes vendors that have accounts; a vendor whose
		// rows always carry account_id '' (Codex today) has nothing to
		// scope, so its rows must pass through an account_id filter
		// rather than being excluded by it.
		accountClause = " AND (account_id = '' OR account_id = ?)"
		accountArgs = []any{f.AccountID}
	}
	whereClause := machineClause + vendorClause + accountClause
	var whereArgs []any
	whereArgs = append(whereArgs, machineArgs...)
	whereArgs = append(whereArgs, vendorArgs...)
	whereArgs = append(whereArgs, accountArgs...)

	buckets, err := db.rateLimitBuckets(ctx, whereClause, whereArgs)
	if err != nil {
		if isMissingRateLimitSnapshotsColumnErr(err) {
			return nil, nil
		}
		return nil, err
	}

	var out []RateLimitSnapshot
	for _, bk := range buckets {
		// bkArgs (with bucketWindowKind) binds the winner CTE and the
		// final WHERE clause, both scoped to this exact bucket including
		// its window_kind dimension. bkArgsNoWindowKind (without it)
		// binds the plan_type/limit_name label subqueries, which are
		// deliberately scoped by limit_id alone -- see
		// idx_rate_limit_snapshots_bucket_jd_plan_type's schema comment.
		bkArgs := []any{bk.vendor, bk.machine, bk.accountID, bk.limitID, bk.bucketWindowKind}
		bkArgsNoWindowKind := []any{bk.vendor, bk.machine, bk.accountID, bk.limitID}
		args := append([]any{}, bkArgs...)
		args = append(args, bkArgsNoWindowKind...)
		args = append(args, bkArgsNoWindowKind...)
		args = append(args, bkArgs...)
		rows, err := db.getReader().QueryContext(ctx, `
			WITH winner AS (
				SELECT observed_at,
					CASE WHEN observation_key != '' THEN observation_key ELSE 'row:' || id END AS obs_key
				FROM rate_limit_snapshots
				WHERE vendor = ? AND machine = ? AND account_id = ? AND limit_id = ?
					AND `+rateLimitBucketWindowKindExpr+` = ?
				ORDER BY julianday(observed_at) DESC, id DESC
				LIMIT 1
			)
			SELECT r.id, r.vendor, r.session_id, r.machine, r.account_id, r.account_label,
				r.limit_id,
				COALESCE((
					SELECT ln.limit_name FROM rate_limit_snapshots ln
					WHERE ln.vendor = ? AND ln.machine = ? AND ln.account_id = ? AND ln.limit_id = ?
						AND ln.limit_name != ''
					ORDER BY julianday(ln.observed_at) DESC, ln.id DESC
					LIMIT 1
				), r.limit_name) AS limit_name,
				COALESCE((
					SELECT pt.plan_type FROM rate_limit_snapshots pt
					WHERE pt.vendor = ? AND pt.machine = ? AND pt.account_id = ? AND pt.limit_id = ?
						AND pt.plan_type != ''
					ORDER BY julianday(pt.observed_at) DESC, pt.id DESC
					LIMIT 1
				), r.plan_type) AS plan_type,
				r.window_kind,
				r.used_percent, r.window_minutes, r.resets_at,
				r.credits_has, r.credits_unlimited, r.credits_balance,
				r.rate_limit_reached_type, r.scope_label, r.severity, r.details,
				r.observed_at, r.ordinal, r.dedup_key, r.observation_key
			FROM rate_limit_snapshots r, winner w
			WHERE r.vendor = ? AND r.machine = ? AND r.account_id = ? AND r.limit_id = ?
				AND `+rateLimitBucketWindowKindExpr+` = ?
				AND r.observed_at = w.observed_at
				AND (CASE WHEN r.observation_key != '' THEN r.observation_key ELSE 'row:' || r.id END) = w.obs_key`,
			args...,
		)
		if err != nil {
			if isMissingRateLimitSnapshotsColumnErr(err) {
				return nil, nil
			}
			return nil, fmt.Errorf("querying latest rate limit snapshot: %w", err)
		}
		for rows.Next() {
			s, err := scanRateLimitSnapshot(rows)
			if err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scanning rate limit snapshot: %w", err)
			}
			if rateLimitSnapshotDisabled(s) {
				continue
			}
			out = append(out, s)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("iterating rate limit snapshots: %w", err)
		}
		_ = rows.Close()
	}

	// rateLimitBuckets already yields buckets ordered by (vendor, machine,
	// account_id, limit_id, bucket_window_kind); re-key to the documented
	// (vendor, account_id, machine, limit_id, window_kind) result order.
	slices.SortFunc(out, func(a, b RateLimitSnapshot) int {
		if c := strings.Compare(a.Vendor, b.Vendor); c != 0 {
			return c
		}
		if c := strings.Compare(a.AccountID, b.AccountID); c != 0 {
			return c
		}
		if c := strings.Compare(a.Machine, b.Machine); c != 0 {
			return c
		}
		if c := strings.Compare(a.LimitID, b.LimitID); c != 0 {
			return c
		}
		return strings.Compare(a.WindowKind, b.WindowKind)
	})
	return out, nil
}

// defaultRateLimitHistoryMaxPoints bounds RateLimitSnapshotHistory's
// result size when a caller does not set RateLimitHistoryFilter.MaxPoints.
// A wide query range on a long-lived rate limit window can otherwise
// accumulate thousands of observations, bloating the API response and
// giving RateLimitHistoryChart that many SVG points to lay out for what
// is, visually, a small sparkline-sized chart.
const defaultRateLimitHistoryMaxPoints = 500

// RateLimitSnapshotHistory returns a time-ordered series of snapshots
// matching f, bounded to at most f.MaxPoints (or
// defaultRateLimitHistoryMaxPoints) entries, for charting used_percent
// over time. A range with more matching rows than that is bucketed
// entirely in SQL: divided into that many equal-width time buckets, each
// contributing only its most recently observed row (the bucket's end
// state, not an average or first observation), so the result stays
// bounded and representative without pulling every matching row into Go
// first. The series is ordered by julianday(observed_at), not a raw text
// comparison; see normalizeRateLimitBoundary's doc comment for why. An
// archive opened read-only from before this table existed returns an
// empty result instead of a "no such table" error, and one predating a
// later column (e.g. severity) an empty result instead of a "no such
// column" error (see hasRateLimitSnapshotsTable and
// isMissingRateLimitSnapshotsColumnErr).
func (db *DB) RateLimitSnapshotHistory(
	ctx context.Context, f RateLimitHistoryFilter,
) ([]RateLimitSnapshot, error) {
	if !db.hasRateLimitSnapshotsTable() {
		return nil, nil
	}
	since, err := normalizeRateLimitBoundary(f.Since)
	if err != nil {
		return nil, err
	}
	until, err := normalizeRateLimitBoundary(f.Until)
	if err != nil {
		return nil, err
	}
	maxPoints := f.MaxPoints
	if maxPoints <= 0 {
		maxPoints = defaultRateLimitHistoryMaxPoints
	}
	machineClause, machineArgs := rateLimitMachineClause(f.Machine)
	vendorClause, vendorArgs := rateLimitVendorClause(rateLimitEffectiveVendor(f.Vendor, f.Agent))
	args := append([]any{}, machineArgs...)
	args = append(args, vendorArgs...)
	accountClause := ""
	if f.AccountID != "" {
		// See LatestRateLimitSnapshots: account_id scopes vendors that
		// have accounts, so an account-less vendor's rows (Codex today)
		// must pass through rather than being excluded.
		accountClause = " AND (account_id = '' OR account_id = ?)"
		args = append(args, f.AccountID)
	}
	args = append(args,
		f.LimitID, f.LimitID,
		f.WindowKind, f.WindowKind,
		since, since,
		until, until,
		// bucket's CASE below: the "keep everything" threshold, the
		// clip ceiling (maxPoints-1), and the bucket-width divisor.
		maxPoints, maxPoints, maxPoints,
		maxPoints, // final LIMIT
	)
	// filtered applies every predicate once. bounds computes the
	// matching range's row count and observed_at span so bucket can
	// decide, per row, which of three regimes applies: fewer rows than
	// maxPoints keeps every row (bucket = its own id, so it is the sole
	// member of its partition below); a zero-width span (every matching
	// row shares one observed_at) collapses to a single bucket, since
	// there is no time axis left to divide; otherwise each row's bucket
	// is its fractional position in the span scaled to maxPoints buckets
	// and clipped to the last one. ranked then keeps only the most
	// recent row (ORDER BY jd DESC, id DESC) per bucket, and the outer
	// query restores chronological order.
	rows, err := db.getReader().QueryContext(ctx, `
		WITH filtered AS (
			SELECT `+rateLimitSnapshotColumns+`, julianday(observed_at) AS jd
			FROM rate_limit_snapshots
			WHERE 1=1`+machineClause+vendorClause+accountClause+`
				AND (? = '' OR limit_id = ?)
				AND (? = '' OR window_kind = ?)
				AND (? = '' OR julianday(observed_at) >= julianday(?))
				AND (? = '' OR julianday(observed_at) < julianday(?))
		),
		bounds AS (
			SELECT MIN(jd) AS min_jd, MAX(jd) AS max_jd, COUNT(*) AS cnt FROM filtered
		),
		bucketed AS (
			SELECT f.*,
				CASE
					WHEN b.cnt <= ? THEN f.id
					WHEN b.max_jd <= b.min_jd THEN 0
					ELSE MIN(? - 1, CAST((f.jd - b.min_jd) * ? / (b.max_jd - b.min_jd) AS INTEGER))
				END AS bucket
			FROM filtered f, bounds b
		),
		ranked AS (
			SELECT *, ROW_NUMBER() OVER (
				PARTITION BY bucket ORDER BY jd DESC, id DESC
			) AS rn
			FROM bucketed
		)
		SELECT `+rateLimitSnapshotColumns+`
		FROM ranked
		WHERE rn = 1
		ORDER BY jd ASC, id ASC
		LIMIT ?`,
		args...,
	)
	if isMissingRateLimitSnapshotsColumnErr(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("querying rate limit snapshot history: %w", err)
	}
	defer rows.Close()

	var out []RateLimitSnapshot
	for rows.Next() {
		s, err := scanRateLimitSnapshot(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning rate limit snapshot: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating rate limit snapshot history: %w", err)
	}
	return out, nil
}

// CopyRateLimitSnapshotsFrom copies rate_limit_snapshots rows from the
// database file at sourcePath into this archive during a resync, the
// same way CopyModelPricingFrom copies model_pricing; INSERT OR IGNORE
// against the unique dedup_key index makes re-running a resync (or
// copying from a source that shares rows with the destination) safe.
// retainedSessionIDs is the union of CopyTrashedDataFrom's and
// CopyOrphanedDataFromExcluding's return values -- sessions copied here
// verbatim, without a reparse. See docs/agents/storage.md for the full
// row-eligibility and session_id-nulling rules this implements.
func (db *DB) CopyRateLimitSnapshotsFrom(sourcePath string, retainedSessionIDs []string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	// Pin a single connection: ATTACH is connection-scoped and
	// database/sql's pool doesn't guarantee the same underlying
	// connection across separate Exec calls.
	ctx := context.Background()
	conn, err := db.getWriter().Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquiring connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(
		ctx, "ATTACH DATABASE ? AS old_rate_limits_db", sourcePath,
	); err != nil {
		return fmt.Errorf("attaching source db: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(ctx, "DETACH DATABASE old_rate_limits_db")
	}()

	var hasTable bool
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM old_rate_limits_db.sqlite_master
		WHERE type = 'table' AND name = 'rate_limit_snapshots'
	)`).Scan(&hasTable); err != nil {
		return fmt.Errorf("checking rate limit snapshots storage: %w", err)
	}
	if !hasTable {
		return nil
	}

	// severity is a later column (see schemaColumnMigrations): a source
	// archive built before it existed has the table but not the column,
	// so the copy selects a literal '' in its place for copied rows
	// rather than failing the whole copy over "no such column". The
	// destination always has the column -- applySchemaColumnMigrations
	// runs on every writable open, before any copy can reach here -- so
	// only the source (SELECT) side of the INSERT varies; the
	// destination (INSERT INTO) column list always names it.
	var hasSeverity bool
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM pragma_table_info('rate_limit_snapshots', 'old_rate_limits_db')
		WHERE name = 'severity'
	)`).Scan(&hasSeverity); err != nil {
		return fmt.Errorf("checking source rate limit snapshots columns: %w", err)
	}
	severitySelect := "''"
	if hasSeverity {
		severitySelect = "o.severity"
	}

	// _retained_rate_limit_session_ids holds retainedSessionIDs, the same
	// way CopyOrphanedDataFromExcluding stages caller-supplied ids in
	// _extra_excluded_orphan_ids: a temp table so the id list can
	// participate in the SQL below without a per-id round trip or a
	// giant IN (...) literal.
	if _, err := conn.ExecContext(ctx, `
		CREATE TEMP TABLE _retained_rate_limit_session_ids (
			id TEXT PRIMARY KEY
		)`,
	); err != nil {
		return fmt.Errorf("creating retained session ids: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(ctx, "DROP TABLE IF EXISTS _retained_rate_limit_session_ids")
	}()
	if len(retainedSessionIDs) > 0 {
		idsTx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin retained session ids: %w", err)
		}
		stmt, err := idsTx.PrepareContext(ctx,
			"INSERT OR IGNORE INTO _retained_rate_limit_session_ids (id) VALUES (?)",
		)
		if err != nil {
			_ = idsTx.Rollback()
			return fmt.Errorf("prepare retained session ids: %w", err)
		}
		for _, id := range retainedSessionIDs {
			if id == "" {
				continue
			}
			if _, err := stmt.ExecContext(ctx, id); err != nil {
				_ = stmt.Close()
				_ = idsTx.Rollback()
				return fmt.Errorf("insert retained session id %s: %w", id, err)
			}
		}
		if err := stmt.Close(); err != nil {
			_ = idsTx.Rollback()
			return fmt.Errorf("close retained session ids: %w", err)
		}
		if err := idsTx.Commit(); err != nil {
			return fmt.Errorf("commit retained session ids: %w", err)
		}
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning rate limit snapshots copy: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// session_id is NULLed unless the session exists in the destination
	// (see docs/agents/storage.md); dedup_key and observation_key are
	// copied unchanged, never recomputed. The WHERE clause below is what
	// keeps a rebuilt session's own fresh rows from being overwritten by
	// its stale source rows.
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO rate_limit_snapshots (
			vendor, session_id, machine, account_id, account_label,
			limit_id, limit_name, plan_type, window_kind,
			used_percent, window_minutes, resets_at,
			credits_has, credits_unlimited, credits_balance,
			rate_limit_reached_type, scope_label, severity, details,
			observed_at, ordinal, dedup_key, observation_key
		)
		SELECT o.vendor,
			CASE
				WHEN o.session_id IS NOT NULL
					AND EXISTS (SELECT 1 FROM sessions s WHERE s.id = o.session_id)
				THEN o.session_id
				ELSE NULL
			END,
			o.machine, o.account_id, o.account_label,
			o.limit_id, o.limit_name, o.plan_type, o.window_kind,
			o.used_percent, o.window_minutes, o.resets_at,
			o.credits_has, o.credits_unlimited, o.credits_balance,
			o.rate_limit_reached_type, o.scope_label, `+severitySelect+`, o.details,
			o.observed_at, o.ordinal, o.dedup_key, o.observation_key
		FROM old_rate_limits_db.rate_limit_snapshots o
		WHERE o.session_id IS NULL
			OR NOT EXISTS (SELECT 1 FROM sessions s WHERE s.id = o.session_id)
			OR EXISTS (
				SELECT 1 FROM _retained_rate_limit_session_ids r
				WHERE r.id = o.session_id
			)`,
	); err != nil {
		return fmt.Errorf("copying rate limit snapshots: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing rate limit snapshots copy: %w", err)
	}
	return nil
}
