# Storage Rules

Read this file before changing SQLite, PostgreSQL, CockroachDB, DuckDB, archive
resync, or storage queries.

## SQLite Archive

SQLite is the persistent archive. Never delete, drop, truncate, or recreate it
to handle a data-version change.

Use non-destructive schema migrations such as `ALTER TABLE` and `UPDATE`. A
parser change that needs a full resync must build a fresh database, sync source
files, copy orphaned sessions from the old database, and swap the files
atomically. Preserve sessions even when their source files no longer exist.

### Codex incremental import state

Four SQLite-only tables support local Codex imports: `parser_checkpoints` holds
resume metadata, `parser_checkpoint_blobs` holds cursor and hash state,
`session_signal_state` holds the incremental signal reducer, and
`tool_call_occurrence_agent_state` holds per-agent result coordinates. The last
table is populated lazily when a call receives a late result. Other providers do
not maintain Codex signal state. Full writes commit the signal seed with the
content and bind it to the stored transcript revision inside SQLite. A failed
seed rolls back the content; there is no post-commit revision read. During a
full resync, the disposable replacement archive defers the tool-call ID and
result metadata indexes until after the bulk load. Rebuilding them must succeed
before the replacement can be installed.

Codex result events also retain a raw-content digest and whether the raw event
participates in the summary. These local fields distinguish events that become
identical after sanitization and preserve whitespace and blocked-result rules.
An older session missing this metadata is reparsed from its source before a late
result is applied. Its first rewrite can advance the transcript revision;
subsequent equal parses remain no-ops. These fields are excluded from exports
and mirror fingerprints.

Large Codex imports use a disposable scratch SQLite database for result
payloads. Publication attaches it to the archive writer and commits content,
checkpoint, and enabled derived state together. Cancellation aborts publication;
cleanup detaches with a context that survives cancellation. Scratch storage is
not an archive or a mirror and is removed after the import.

Tool-result image retention uses the canonical `config.ToolResultImages` policy
on writable SQLite handles. The zero value keeps content. Drop mode projects a
valid inline `data:image/...;base64` block into an `agentsview_image`
placeholder before derived lengths, display comparisons, and persistence. Raw
event digests are captured before projection so distinct provider events remain
distinct and replayed late results stay no-ops. The projection preserves
ordinary text, metadata, block order, unsupported shapes, and future
placeholders. Combined summaries project labeled and anonymous sections using
JSON boundaries, so blank lines inside arrays do not split them. Late result
writes also project the rebuilt summary when older events predate drop mode.
`db strip --images` applies the projection to existing rows one session at a
time. The command updates `tool_calls.result_content` and
`tool_result_events.content` directly in one transaction per session,
recalculates their stored lengths, and keeps every event coordinate and metadata
column unchanged. Each changed session also gets a full secret scan of its
projected transcript inside that transaction, preserving findings with their
current offsets and rule version. A changed session gets the normal transcript
revision, Recall, signal, artifact export, usage notification, and post-commit
revocation sequence. An unchanged session gets none of those publications. Full
resync applies this same projection only to the IDs returned by its trashed and
orphaned session copies, before the replacement is published. Freshly parsed
sessions already carry the projection. Large Codex imports project events before
scratch insertion; staged summaries and signals use that projected content. The
command counts raw `tool_calls.result_content` and `tool_result_events.content`
bytes separately from decoded image bytes. `db compact` reports file-size
reclamation separately.

Transcript-only and usage-only writes omit parser checkpoints because resumable
hash state can contain raw trailing transcript bytes. They retain staged parsing
but publish projected messages and tool metadata without staged output. Late
result updates use the same projection as newly inserted messages.

### Rate-limit snapshots

`rate_limit_snapshots` is a SQLite-only vendor-data table, in the same
category as `cursor_usage_events` and the four Codex incremental-import
tables above: it is out of scope for the SQLite/PostgreSQL/DuckDB parity
rule below. It stores one row per rate-limit window observation from either
of two vendors, distinguished by a `vendor` column (`codex` or `claude`):

- **Codex**: each `rate_limits` object a Codex `token_count` event carries
  beside `info.last_token_usage` (one row per window). `session_id` is
  nullable (`ON DELETE SET NULL`) so a row survives its source session
  being deleted; `machine`, `limit_id`, and `plan_type` are Codex-specific
  and stay empty for Claude rows.
- **Claude**: each window a `internal/claude.Job` poll of the (unofficial)
  `GET /api/oauth/usage` endpoint returns. `account_id` is
  `claude.Identity.AccountKey()` -- the `oauthAccount` `accountUuid` plus
  `organizationUuid` from `~/.claude.json` -- and `account_label` is
  Claude-specific; both stay empty for Codex rows. `accountUuid` alone
  stays constant across every org a user belongs to, so `AccountKey`
  folds in `organizationUuid` too, giving each org its own
  `LatestRateLimitSnapshots` group instead of letting one org's poll
  overwrite another's cards; `session_id` and `machine` stay empty since
  the poll is not tied to one parsed session or host.

`resets_at` is nullable for both vendors (an absent field, not `0`), so a
genuinely unknown reset time never displays as "resets right now".

`window_kind` is free-form: Codex uses `primary`/`secondary`. Claude
derives it from the oauth/usage response's top-level `limits` array --
an account-wide entry's raw `kind` canonicalized to `session` or `weekly`,
or `<canonical kind>:<scope identity>` for a model- or surface-scoped
entry (e.g. `weekly_scoped:Fable`). Scope identity
(`claude.LimitScope.Identity()`) combines model and surface only when
both are populated; the separate display label
(`claude.LimitScope.Label()`, stored in `scope_label`) drops the surface
whenever a model is present. `window_kind` falls back to the fixed-bucket
source (`session`/`weekly` for the 5-hour/7-day buckets, plus
`seven_day_opus`, `seven_day_sonnet`, `seven_day_oauth_apps`,
`seven_day_overage_included`) only when `limits` is empty or absent, and
that fallback is one-way per account identity
(`internal/claude.Job.sawLimitsArray`, keyed by
`claude.Identity.AccountKey()` since one Job follows whichever org is
currently logged in): once an identity's response has ever carried a
non-empty `limits` array, a later empty one writes nothing rather than
switching sources. `Job.retireStaleClaudeWindows` treats every poll that
reports `limits` as a complete snapshot of the account's limits (except
`extra_usage_monthly`) and writes a disabled row (`{"source": "limits",
"enabled": false}`) for any current window absent from it, driven by
`LatestRateLimitSnapshots` rather than in-memory state so it is correct
across a restart. `Job.retireStaleExtraUsageWindow` does the analogous
check for `extra_usage_monthly` whenever a poll's response carries no
`extra_usage` object at all.

The statusLine sink (`agentsview claude statusline-sink`) emits the same
`session`/`weekly` identity for its own five_hour/seven_day observations,
so a window observed by more than one ingestion path converges on one
row. `extra_usage_monthly` is its own window, written on every poll once
an account's response has ever carried an `extra_usage` object, enabled
or not; `LatestRateLimitSnapshots` excludes a row `details` marks
`"enabled": false` from current results, while `RateLimitSnapshotHistory`
still returns it so a chart spanning the disable event shows the
transition.

`LatestRateLimitSnapshots`/`RateLimitSnapshotHistory` tolerate a
`rate_limit_snapshots` table or column a later release added being
absent (returning an empty result rather than erroring), matching
`OpenReadOnly`'s general tolerance for an older, otherwise-compatible
archive -- see `hasRateLimitSnapshotsTable` and
`isMissingRateLimitSnapshotsColumnErr`.

Cross-vendor label consistency ("Session limit"/"Weekly limit" in the
UI) is achieved at the display layer: the frontend derives a window's
title from `window_minutes` for both vendors (300 -> "Session limit",
10080 -> "Weekly limit", any other length -> a generic "{duration}
limit" label), with the scope/limit qualifier (Claude `scope_label`,
Codex `limit_name`) shown as a badge next to it. Codex's stored
`window_kind` stays `primary`/`secondary`.

`scope_label`, `severity`, and `details` are Claude-only. `scope_label`
is the scope's model display name, else its model id, else its surface;
`severity` is the server-reported severity string; `details` is a
free-form JSON object (the decode path that produced the row, the raw
`scope` object, or -- for `extra_usage_monthly` -- the monetary limit
and currency). These stay empty for Codex rows.

Rows are written against a unique `dedup_key` (`db.RateLimitSnapshotDedupKey`),
never a delete-then-reinsert. A row with a session id (Codex today) keys
on `(session_id, observed_at, limit_id, window_kind, ordinal)`: session
id is already vendor- and event-specific, so `ordinal` (the source
`token_count` event's 0-based file position) is what disambiguates two
events sharing an `observed_at` second. A session-less row (Claude,
polled rather than parsed) instead keys on `(vendor, machine, account_id,
limit_id, plan_type, window_kind, resets_at, observed_at bucketed to the
minute)`, since it has no session id or per-event ordinal to key on;
bucketing to the minute collapses repeat polls of unchanged state into
one row. `plan_type` and `limit_name` participate in the dedup key but
not in `LatestRateLimitSnapshots`' grouping, since they are labels that
can flip between a real value and empty for the same window (resolved
to the most recently observed non-empty value).

The write is an upsert, not a plain `INSERT OR IGNORE`: a `dedup_key`
collision against a row whose `session_id` is already NULL (a resync
copy for a session absent from the destination, or -- always, since
Claude rows never carry one -- a repeat Claude poll) reattaches that row
to the incoming session (a no-op for Claude, which has none) and
refreshes the fields `INSERT ... ON CONFLICT ... DO UPDATE` covers. A
separate pass then unconditionally refreshes every row's remaining
mutable fields (`observed_at`, `severity`, and anything else that
statement's `SET` list omits), guarded by a `julianday` comparison so an
out-of-order replay can never regress a row past a newer one already
stored under the same key -- this is what keeps a Claude window's
`used_percent` current on a poll whose other identity fields are
unchanged within the same minute bucket. A collision against a row that
already has a session attached (the common Codex case: re-parsing an
unchanged event) is a no-op either way.

An `account_id` filter (the `/current` and `/history` endpoints'
`account_id` query parameter) scopes vendors that have accounts, so
both `LatestRateLimitSnapshots` and `RateLimitSnapshotHistory` match a
nonempty `account_id` against `(account_id = '' OR account_id = ?)`
rather than a bare equality, letting an account-less vendor's rows
(Codex today) pass through instead of being excluded.

A full resync copies existing rows the same way model pricing is
copied (`CopyRateLimitSnapshotsFrom`), after orphaned sessions are
restored. It NULLs `session_id` for a row whose session does not exist
in the destination even after restoration (superseded by a reparse
under a different id, or excluded), preserving `dedup_key` unchanged,
and skips a row whose session was instead rebuilt by the resync's own
reparse (copied only when `session_id` is NULL, absent from the
destination, or explicitly retained without reparse), so a rebuilt
session's superseded rows cannot resurrect on top of its fresh ones.
Claude rows never carry a `session_id`, so this only ever applies to
Codex rows.

`LatestRateLimitSnapshots` resolves "latest" per (vendor, machine,
account_id, limit_id) bucket, excluding plan_type, and for Codex one
level above window_kind too -- Codex's window kinds are two granularities
of one payload that arrive and vanish together, while Claude's are
independently observed facts about one account, so window_kind stays
part of the bucket key only for `vendor = 'claude'` rows.
`RateLimitSnapshotHistory` still returns every row over time regardless
of this grouping, keyed by (vendor, machine, account_id, limit_id,
window_kind) -- the same fields `RateLimitCardIdentity` on the frontend
groups by.

PostgreSQL and DuckDB implement the read-side `Store` methods as no-ops
returning an empty result, so the Usage page's rate-limits section is
simply hidden when either backend is the active read store. A write
that replaces a session's messages wholesale (an authoritative reparse,
or any other full delete-and-reinsert) deletes that session's
`rate_limit_snapshots` rows in the same transaction before inserting the
new set via `InsertRateLimitSnapshotsReplacingSession`, while a normal
incremental parse and a repeated Claude poll keep appending through
`InsertRateLimitSnapshots`.

## Archive Content Policy

`archive_content` (`internal/config.ArchiveContent`) narrows what the SQLite
archive stores. The `*db.DB` handle is the single authority: `Open` variants and
`sync.NewEngine` only tighten it, never loosen it, and every write path projects
sessions and messages through `internal/db/archive_content.go` before rows are
written.

- Route any new session, message, tool call, signal, or finding write through
  the existing projection helpers instead of checking the policy inline.
- Resync copies archived rows with `ATTACH`, which bypasses the write path.
  `applyArchiveContentToCopiedSessionsTx` mirrors the Go projection in SQL for
  the orphan and trash copies. Keep the two in step when either changes.
- Copied tool renderings use exact reconstructed text where possible. When
  stored inputs cannot reconstruct a recognizable tool rendering, transcript
  projection keeps the preceding prose and tool label but discards the
  remaining message tail, whose argument boundaries are unknown.
- Usage-only rows retain normalized context/output token values and their
  presence flags as well as `token_usage`; model-mix totals use these columns.
- PostgreSQL pushes record `prompt_evidence_discarded` per session from the
  source archive policy. Automation audits preserve the stored verdict only
  when this marker explains the missing prompt evidence. Full-content rows
  remain eligible for corrections.
- Usage-only mode disables vector building, serving, and export. Opening the
  writable archive clears the local message and recall indexes under the
  vector write lock. PostgreSQL pushes clear all generations of indexed
  content for owned sessions, including sessions already deleted locally.
  Cleanup finds candidates in PostgreSQL and rechecks ownership under the
  session lock before deleting them.
- Compute derived values (signals, secret findings) from the projected messages
  so a later recompute from stored rows reproduces them.

`db migrate --images` moves retained inline payloads out of SQLite and into the
asset store. It writes each decoded payload to
`{dataDir}/assets/<sha256hex><ext>` before any UPDATE commits in the session
transaction. If that transaction fails, complete objects remain unreferenced on
disk and are reused by a matching retry; there is no automatic cleanup of
unreferenced assets. An existing object is reused only when its byte count and
SHA-256 digest match. A missing or corrupt object is replaced while the source
bytes remain available. The inline block is replaced with an `agentsview_image`
placeholder whose `image_ref` field holds `asset://<sha256hex><ext>` and whose
`text` field carries a markdown image
`![Image: <type>, <n> bytes](asset://<sha256hex><ext>)`. The discriminator for
migrated blocks is `image_ref`. A later keep-mode reparse or full resync can
restore inline bytes from provider source files. Only the four passive media
types are migrated: `image/png`, `image/jpeg`, `image/webp`, and `image/gif`.
SVG payloads stay inline. A separate serving host needs the matching
`{dataDir}/assets` directory with the copied database content. The command
otherwise follows the same transaction, revision, and publication sequence as
`db strip --images`. Back up `{dataDir}/assets` together with the archive.

## Backend Parity

- Keep observable behavior and query shape aligned between SQLite and
  PostgreSQL/CockroachDB when practical. Match queries, indexes, aggregations,
  filters, and ordering unless a documented constraint requires a difference.
- Do not fix correctness or performance in only one primary backend unless the
  user limits the task to that backend. If implementations must differ,
  explain why and preserve the same behavior.
- DuckDB is a derived mirror and is not part of this parity rule.

### Usage cache divergence

SQLite's aggregate usage APIs read timezone-specific daily rollups from a
disposable sibling database. Normalized, unpriced facts in the same database are
the exact build substrate, not the warm aggregate read path. Per-session detail
remains on the live row path, and PostgreSQL continues to aggregate its live
normalized archive rows. The live path is never a fallback for a failed or stale
SQLite aggregate read. Both implementations are co-maintained under the same
behavior contract: daily usage, top sessions, billed session counts, relaxed
matching counts, and per-session usage must remain observably equal. The
`pgtest` complete-result parity fixture is the acceptance boundary. Track the
PostgreSQL-native optimization in
[issue #1451](https://github.com/kenn-io/agentsview/issues/1451).

The usage cache filename is derived from its format version and the archive
`database_id`. A format or database-ID change selects a new generation; it does
not migrate or rewrite the archive. Facts contain only message- and
usage-event-derived data. Aggregate fingerprints additionally bake the exact
session `agent` and `started_at`, because those fields affect deduplication and
day bucketing. All other session metadata and filters come from the archive read
snapshot. Do not widen or narrow this live/baked boundary implicitly.

The cache format version is also the extractor compatibility version. Bump
`usageCacheFormatVersion` whenever fact extraction, `priceUsageFact`, web-search
fees, deduplication, rollup semantics, or query-time model canonicalization
change. Catalog and user-pricing changes are covered separately by the pricing
content digest; do not add a write-only extractor-version metadata key.

Deduplication groups are classified per group at rollup build time. A group is
finalized into daily rows only when its resolution provably cannot vary with the
query window or live filters: every member shares one source session and one
local date, general (`source:`/`usage:`) groups additionally share one model and
headless state, no member links snapshot and general dedup, no member carries a
Copilot authoritative cost, and the group's identity appears in no other cached
session (nor, for usage keys, in the Cursor fact store). Only the remaining
irreducible groups go to the timezone-specific exception tier that resolves
narrow rows at read time, preserving the window-scoped dedup semantics. Cursor
facts stay entirely on the exception tier. Because query windows are whole local
days, a single-date group is inside or outside any window as a unit.

Cross-session identity checks are conservative and served by dedicated
`usage_facts` identity indexes, not a membership table. Whenever a fill, Cursor
batch, or deletion changes the set of dedup identities a session (or the Cursor
store) contributes, it must, in the same cache transaction, delete the timezone
rollup installs of every other session holding a changed identity; rollup
installation re-verifies inside its transaction that no finalized identity
gained an outside member and, when one did, reclassifies against the newly
committed facts rather than failing the caller. A finalized daily row must never
survive gaining a sibling.

Treat a usage-cache file as identifiable only after both its SQLite
`application_id` and `usage_cache_metadata.cache_kind` match. Filename matching
alone never permits deletion or replacement. Lease-aware generations hold a
shared cross-process lease for every open SQLite pool; retirement requires the
exclusive lease plus a fresh application-ID, cache-kind, protocol-version,
format-version, and source-database-ID check against the exact filename. Keep
the lease file after retirement so a racing opener cannot lock a replacement
inode. Preserve pre-protocol generations because an older binary may hold an
idle handle without a lease, and preserve generations newer than the running
format so a downgraded binary does not force the newer one to rebuild. If
persistent cache storage is unavailable or the current generation is
incompatible, use the same schema and query path in a process-owned temporary
file and warn that the cache will rebuild after restart.

Usage reads are exact. A cold aggregate request fills facts, builds the required
timezone rollups, then reads them in one pinned cache transaction. Verify every
candidate session's facts fingerprint, exact baked metadata, canonical pricing
digest, resolved rate hashes, and Cursor high-water mark. A result is no older
than the archive snapshot captured when the read began, and may be newer for a
session whose facts were refilled meanwhile. A session confirmed deleted during
fill is dropped from the request. `cached_at` is diagnostic only.

The layers are kept apart so a live archive cannot veto a read. A fill reads one
session's facts and that session's source version inside a single archive read
transaction, installs both together, and reports the version it actually read,
which may be newer than the one the caller asked for. Rollup aggregation then
reads committed facts out of the usage cache only; it never touches the archive,
so an append landing mid-build cannot abort it. An install is stale when the
fact versions it was built from differ from the ones the cache now holds, and
only those installs are rebuilt. Sessions written during a build are refilled by
their own mutation notification and appear in the next aggregation, so staleness
of a few seconds is expected and intended. Do not reintroduce a whole-snapshot
recheck against the archive: validating a snapshot against a source that changes
one session at a time livelocks the request.

Timezone rollup identity includes both the resolved zone name and its rule
fingerprint. Cache-generation retirement cancels detached work immediately but
keeps immutable coordinator pointers and the cache database alive until active
query, backfill, fill, and rollup leases drain.

`sync_marker` is a fingerprint component, not a monotonic version: its trigger
recomputes the maximum of mutable timestamp fields, so it can decrease. A fill
must read the full source fingerprint in the same transaction as the facts it
installs. Do not compare fingerprints for ordering, and do not skip a refill
because a cached fingerprint merely looks newer.

### Activity report index

Activity session selection checks terminal tool-execution events even when a
session's `ended_at` predates the report. Keep the partial
`idx_tool_result_events_terminal` index on `(session_id, timestamp)` aligned
between SQLite and PostgreSQL. It includes completed and errored executions with
non-null timestamps, so the lookup can skip unrelated result payloads and seek
directly to the report's lower bound.

The next writable SQLite open or PostgreSQL schema setup builds the index once
for existing archives. PostgreSQL push must also detect its absence before
taking the schema-current fast path. Creating the index scans existing tool
results and can delay that first startup; it does not require a session resync.

### Usage archive indexes

The usage cache discovers bounded-window candidates through
`idx_messages_usage_timestamp` and `idx_messages_activity_timestamp`, then
extracts each selected session through the index-only
`idx_messages_usage_session_covering` scan. The global activity index is for
usage-cache candidate discovery, not the Activity report; that report continues
to avoid a global timestamp scan. Keep these indexes narrow except for the
single session-keyed covering index that carries `token_usage`.

Changing any of these index column lists rebuilds the affected archive index on
the next writable open, before HTTP readiness, and must log that startup is
waiting for the migration. Read-only opens require the current indexes and may
therefore reject an archive that has not first been opened by the matching
writable version. Treat this as executable/archive version skew, not as a reason
to mutate the archive from a read-only command.

Full resync drops these indexes in the temporary database during the bulk load
(the FTS trade: one post-load build instead of per-row B-tree maintenance) and
must rebuild them before the swap; a failed rebuild aborts the swap because
read-only opens require the indexes.

### Transcript usage identity

Token usage, Claude message/request identities, and source UUID participate in
transcript revision equality. Finalizing a streamed message can therefore bump
`transcript_revision` and `local_modified_at`, invalidate secret-scan freshness,
mark the session updated for read-progress/UI purposes, and enqueue the normal
artifact, recall, PostgreSQL, and DuckDB refreshes. Full resync reconciliation
must compare the same fields so incremental and resync paths agree. A no-op
message replacement preserves existing secret findings; changed transcript
content clears them for a fresh scan.

### Tool result summaries

`tool_calls.result_content` is a display summary derived from the call's
`tool_result_events` rows at sync time. When a call has exactly one event and
the summary equals that event's content, the summary is not stored: the column
is empty while `result_content_length` still records the summary's size. That
pair, an empty column with a non-zero length, tells a reader to take the text
from the single event. Multi-event summaries, single-event summaries that differ
from their event, calls with no events, and blocked categories store exactly
what the parser produced. Load tool calls through the message loaders, which
refill the summary once events are attached; a query that selects the column
directly must apply the same fallback, and PostgreSQL and DuckDB apply the same
write rule so their tool-call fingerprints match SQLite. Anyone reading the
archive or a mirror by hand sees the empty column and must join the events table
to recover the text.

### Background poller status

`poller_status` is a SQLite-only table (schema.sql) holding one row per
`internal/poller.Scheduler` job, keyed by the job's stable name (e.g.
`pricing-refresh`, `cursor-usage`, `claude-usage:<name>`). It records
`last_attempt`, `last_success`, `last_error`, `consecutive_failures`, and
`next_run` so `agentsview doctor` and the `/api/v1/system/pollers` status
endpoint can show a job's state across a daemon restart. It is machine-local
scheduling bookkeeping, like `parser_checkpoints`, and is never mirrored to
PostgreSQL or DuckDB. A missing row means the job has not attempted a run
against this database yet, not an error.

`retry_after_until` repeats `next_run`'s value, but only when `next_run`
was set from an explicit `*poller.RetryAfterError` (the Claude job
returns one on a 429) rather than ordinary backoff or a scheduled
interval; empty otherwise. `Scheduler.attempt` checks it unconditionally
before running a job, even for a TriggerNow call (the Claude accounts
settings panel's Test button), and returns `poller.ErrRetryAfterPending`
without running the job at all while it holds: TriggerNow bypasses
`next_run`'s ordinary Cooldown by design, but not an upstream-supplied
Retry-After wait, since retrying before it elapses would just repeat
the same request. It is a `Status` field, restored from this table the
same way `last_attempt`/`next_run` already are, so it survives a daemon
restart. `CopyPollerStatusFrom` tolerates a source archive whose table
predates this column via `pragma_table_info`.

## DuckDB Mirror

- Treat DuckDB as a disposable read mirror of SQLite, never as a system of
  record. Deleting the mirror must lose nothing.
- Do not add in-place mirror migrations. A schema or source-data version change
  must bump `internal/duckdb.SchemaVersion`, rebuild a fresh file, validate
  it, and swap it atomically. Do not add `ALTER` migrations, version-bridging
  reads, or compatibility shims for old mirrors.
- Store every DuckDB push cursor and version in the mirror's `sync_metadata`.
  Never store DuckDB sync state in SQLite.
- Replace whole sessions during incremental updates and gate them with
  per-session fingerprints. Do not add per-table, per-column, or diff-based
  updates.
- Keep Quack read-only. `duckdb push` writes the local mirror; it never writes
  to a remote DuckDB service.
- Replace a file only after identifying it as an agentsview DuckDB mirror. Fail
  closed for unknown files.

## PostgreSQL Integration Tests

Run PostgreSQL integration tests only against a dedicated test database. The
tests create and drop the `agentsview` schema.

Use `make test-postgres` to start the test container and run the suite. It
leaves the container running. If you started that container, use
`make postgres-down` when it is no longer needed.

To use an existing dedicated instance, run:

```bash
TEST_PG_URL="postgres://user:pass@host:5432/dbname?sslmode=disable" \
  CGO_ENABLED=1 go test -tags "fts5,pgtest" ./internal/postgres/... -v
```
