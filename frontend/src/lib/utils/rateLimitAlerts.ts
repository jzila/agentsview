/**
 * Pure logic for the rate-limit threshold notification feature.
 *
 * Framework-free and side-effect-free: no Svelte runes, no `Notification`
 * calls, no storage access, no i18n. Stores wire this up to browser APIs,
 * persistence, and Paraglide message formatting; this module is what gets
 * unit-tested for the threshold math, once-per-cycle arming, muting, and
 * adapting an API row into the shape the rest of the feature consumes.
 *
 * The feature is vendor-agnostic: a window is identified by
 * (vendor, account, limit id, window kind). Codex is today's only source;
 * a second vendor needs no changes here beyond a `vendor` value flowing
 * through the API row.
 */

import type { ServiceRateLimitWindow } from "../api/generated/index.js";

export const DEFAULT_THRESHOLD_PERCENT = 20;
/** Full range accepted by the pure evaluator's threshold math. The
 * settings UI only ever produces values in the narrower SLIDER range
 * below, but 100 ("alert immediately") is a real, tested edge case. */
export const MIN_THRESHOLD_PERCENT = 0;
export const MAX_THRESHOLD_PERCENT = 100;

/** Range enforced by the settings panel's slider. 0 means "alert only
 * once a window is fully used"; 25 is the largest remaining-percent the
 * UI offers. Stored values are clamped into this range on load. */
export const SLIDER_MIN_THRESHOLD_PERCENT = 0;
export const SLIDER_MAX_THRESHOLD_PERCENT = 25;

/** Persisted notification-threshold configuration. Threshold resolution
 * for one window checks, in order: a window-specific override, then a
 * vendor-wide override, then the global default. */
export interface NotificationThresholdSettings {
  enabled: boolean;
  defaultThresholdPercent: number;
  /** Keyed by vendor id (e.g. "codex", "claude"). */
  vendorThresholdOverrides: Partial<Record<string, number>>;
  /** Keyed by the full window key (see RateLimitWindowInfo.key). */
  windowThresholdOverrides: Partial<Record<string, number>>;
  /** Source keys (see `sourceKey`) the user has muted. A source not in
   * this set is on by default, so a newly-observed source notifies
   * immediately rather than silently defaulting to off. */
  mutedSourceKeys: readonly string[];
  /** Master switch for the second ("exhausted") notification stage. */
  notifyOnExhausted: boolean;
}

/** One rate-limit window, normalized for the evaluator and notifier.
 * Produced by `adaptCurrentRateLimitRow`. */
export interface RateLimitWindowInfo {
  /** Stable identity for this window across polls:
   * `${vendor}:${account}:${limitId}:${windowKind}`. Used both as the
   * once-per-cycle arming key and as the notification tag. Deliberately
   * excludes planType, which the API reports as a display label rather
   * than a partition of rows (see `accountLabel`). */
  key: string;
  /** Normalized lowercase vendor id, e.g. "codex", "claude". */
  vendor: string;
  /** The raw, stable account/machine identity: accountId when the API
   * reports one, else the reporting machine, else "default". */
  account: string;
  /** Raw plan type as reported ("" when unknown). */
  planType: string;
  /** Display label for the account: the API's friendly accountLabel
   * when reported, else `account`, with the plan as parenthetical
   * context, e.g. "Watt (pro)". Never the raw id when a friendlier name
   * is available. */
  accountLabel: string;
  windowKind: string;
  /** Stable slot id for this limit within its account, e.g. "codex" or
   * a UUID. Falls back to `windowKind` when the API omits it. */
  limitId: string;
  /** Vendor-reported human name for this limit (e.g.
   * "GPT-5.3-Codex-Spark"), or "" when unknown. Used with `limitId` to
   * build a notification's named-window label the same way
   * RateLimitCard does for the Usage page. */
  limitName: string;
  usedPercent: number;
  /** Unix seconds the window resets, or null when unreported. */
  resetsAt: number | null;
  windowMinutes: number | null;
  /** True when the window is fully used: usedPercent >= 100, or a
   * window-scoped vendor exhaustion flag (currently only Claude's
   * extra_usage.spend_limit_reached — see `resolveExhausted`). Drives
   * the second ("exhausted") notification stage. */
  exhausted: boolean;
  /** When the archive recorded this observation (`row.observedAt`
   * parsed to epoch milliseconds), or null when missing/unparseable.
   * Fetches aren't ordering-guaranteed (see `evaluateRateLimitWindows`'s
   * resetsAt handling), so this is the signal used to reject a delayed
   * response that turns out to describe an OLDER snapshot than one
   * already evaluated for this window — resetsAt alone can't catch that
   * for a window with an unknown reset, since two out-of-order
   * observations can otherwise report the identical (null) resetsAt. */
  observedAtMs: number | null;
}

/** The identity of a usage source (one vendor account/machine) for
 * muting — coarser than a window key, since one source can have
 * multiple windows (e.g. a primary and a secondary). */
export function sourceKey(window: Pick<RateLimitWindowInfo, "vendor" | "account">): string {
  return `${window.vendor}:${window.account}`;
}

/** One row for the settings panel's per-source mute list: every
 * distinct (vendor, account) pair currently observed. */
export interface SourceInfo {
  key: string;
  vendor: string;
  account: string;
  /** Display label, e.g. "Codex · pro" (vendor + account/plan, no
   * window duration — a source can have more than one window). */
  label: string;
}

/** Derives the distinct sources observed across a set of windows,
 * sorted by vendor then account for stable rendering. */
export function deriveSources(windows: readonly RateLimitWindowInfo[]): SourceInfo[] {
  const byKey = new Map<string, SourceInfo>();
  for (const window of windows) {
    const key = sourceKey(window);
    if (byKey.has(key)) continue;
    byKey.set(key, {
      key,
      vendor: window.vendor,
      account: window.account,
      label: `${vendorDisplayName(window.vendor)} · ${window.accountLabel}`,
    });
  }
  return [...byKey.values()].sort(
    (a, b) => a.vendor.localeCompare(b.vendor) || a.account.localeCompare(b.account),
  );
}

/** Distinct vendors observed across a set of windows, sorted. */
export function deriveVendors(windows: readonly RateLimitWindowInfo[]): string[] {
  return [...new Set(windows.map((w) => w.vendor))].sort();
}

/** Title-cases a vendor id for display, e.g. "codex" -> "Codex". */
export function vendorDisplayName(vendor: string): string {
  return vendor.length === 0 ? vendor : vendor.charAt(0).toUpperCase() + vendor.slice(1);
}

/** The two notification stages a window can fire, at most once each per
 * reset cycle: `threshold` (remaining allowance dropped below the
 * configured threshold) and `exhausted` (the window is fully used). */
export type NotificationStage = "threshold" | "exhausted";

/** One stage having fired, and when. `recoveredAt` is set only for a
 * window whose resetsAt is unknown (null) — see
 * `evaluateRateLimitWindows`'s doc comment for why that case needs its
 * own rearm signal instead of relying on a changed resetsAt. */
export interface NotifiedStageEntry {
  notifiedAt: number;
  recoveredAt?: number;
}

/** Arming state for one window: the latest known reset cycle its stages
 * were evaluated for, and which of the two stages have already fired
 * within that cycle. A demonstrably later known resetsAt re-arms both
 * stages; see `evaluateRateLimitWindows`. */
export interface NotifiedWindowState {
  resetsAt: number | null;
  /** Epoch ms of the observation this state was last evaluated against
   * (`RateLimitWindowInfo.observedAtMs`). Optional/nullable so older
   * stored state and fixtures without it still parse -- treated as
   * "unknown," which never rejects anything as stale on its own; see
   * `evaluateRateLimitWindows`'s observedAtMs staleness check. */
  lastObservedAtMs?: number | null;
  stages: Partial<Record<NotificationStage, NotifiedStageEntry>>;
}

export type NotifiedMap = Record<string, NotifiedWindowState>;

/** One notification to send: which window, and which stage crossed. */
export interface NotificationToSend {
  window: RateLimitWindowInfo;
  stage: NotificationStage;
  /** Other stage(s) this evaluation silently marked as fired in the
   * persisted map -- without ever queuing their own notification -- as a
   * side effect of this one firing. Currently only threshold, when a
   * window is exhausted the first time it's observed: exhausted alone
   * notifies, but threshold is also marked fired so a later poll that's
   * merely no longer exhausted doesn't read as a fresh crossing (see this
   * function's doc comment). If THIS notification's delivery fails, a
   * caller should roll these back too, since they were never actually
   * delivered to the user either -- see rateLimitAlertRunner's check(). */
  silentlyAlsoMarked?: readonly NotificationStage[];
}

export interface EvaluationResult {
  /** Notifications to send right now — at most one per window per
   * stage per call. */
  toNotify: NotificationToSend[];
  /** The notified-state map to persist after this evaluation. */
  nextNotifiedMap: NotifiedMap;
}

/** Clamps and rounds a threshold value into the evaluator's full valid
 * range (0-100). Non-finite input falls back to the default rather than
 * propagating NaN into stored settings. */
export function clampThresholdPercent(value: number): number {
  if (!Number.isFinite(value)) return DEFAULT_THRESHOLD_PERCENT;
  const rounded = Math.round(value);
  return Math.min(MAX_THRESHOLD_PERCENT, Math.max(MIN_THRESHOLD_PERCENT, rounded));
}

/** Clamps and rounds a threshold value into the settings slider's range
 * (0-25), so a value stored before the slider existed can't break the
 * control on load. */
export function clampSliderThresholdPercent(value: number): number {
  if (!Number.isFinite(value)) return DEFAULT_THRESHOLD_PERCENT;
  const rounded = Math.round(value);
  return Math.min(SLIDER_MAX_THRESHOLD_PERCENT, Math.max(SLIDER_MIN_THRESHOLD_PERCENT, rounded));
}

/** Resolves the remaining-percent threshold for one window: a
 * window-specific override first, then a vendor-wide override, then the
 * global default. */
export function resolveThresholdPercent(
  settings: NotificationThresholdSettings,
  window: Pick<RateLimitWindowInfo, "key" | "vendor">,
): number {
  const windowOverride = settings.windowThresholdOverrides[window.key];
  if (typeof windowOverride === "number") return clampThresholdPercent(windowOverride);

  const vendorOverride = settings.vendorThresholdOverrides[window.vendor];
  if (typeof vendorOverride === "number") return clampThresholdPercent(vendorOverride);

  return clampThresholdPercent(settings.defaultThresholdPercent);
}

/** A threshold is expressed as remaining percent: a threshold of 20
 * fires once a window passes 80% used. usedPercent at exactly the
 * boundary (100 - threshold) counts as crossed. */
export function remainingThresholdCrossed(usedPercent: number, thresholdPercent: number): boolean {
  return usedPercent >= 100 - thresholdPercent;
}

/** The later of two known-or-unknown timestamps: unknown (null) loses to
 * any known value, and between two known values the larger (later) one
 * wins. Used for both `resetsAt` ("the latest known cycle boundary we've
 * ever observed for this window") and `observedAtMs` ("the latest
 * observation time we've ever evaluated this window against") — in both
 * cases so a stale, out-of-order value arriving after a newer one never
 * regresses what's remembered. */
function latestKnownTimestamp(a: number | null, b: number | null): number | null {
  if (a === null) return b;
  if (b === null) return a;
  return Math.max(a, b);
}

/** Reconciles carried-over stages (a poll that did not rearm) against
 * whether each stage's condition is met right now.
 *
 * Recording: while a window's remembered resetsAt is unknown (null),
 * there is no timestamp to key a fresh cycle off of, so a fired stage
 * would otherwise stay suppressed forever even after usage genuinely
 * recovers. `recordNew` (true only while resetsAt is still null) gates
 * this: the first poll where a fired stage's condition is no longer met
 * records a `recoveredAt` marker on it. A known resetsAt already has its
 * own rearm signal (a strictly later value), so a mere usage dip must
 * never record a recovery there — see `evaluateRateLimitWindows`' known-
 * resetsAt dip test.
 *
 * Consuming: once a stage already carries a `recoveredAt` marker, the
 * poll where its condition becomes true again clears the stage so it can
 * fire like a fresh cycle — unconditionally, regardless of whether
 * resetsAt is null or has since become known. The marker can only have
 * been recorded while resetsAt was null, but a window can learn its
 * resetsAt later without that being a fresh cycle (see
 * `evaluateRateLimitWindows`' null<->known doc comment); consuming the
 * marker here too is what lets a stage that already recovered still
 * refire once usage re-crosses, instead of staying suppressed forever
 * just because resetsAt is no longer null. */
function reconcileStageRecoveries(
  stages: Partial<Record<NotificationStage, NotifiedStageEntry>>,
  met: Record<NotificationStage, boolean>,
  now: number,
  recordNew: boolean,
): Partial<Record<NotificationStage, NotifiedStageEntry>> {
  const next: Partial<Record<NotificationStage, NotifiedStageEntry>> = { ...stages };
  for (const stage of ["threshold", "exhausted"] as const) {
    const entry = next[stage];
    if (!entry) continue;
    if (entry.recoveredAt === undefined) {
      if (recordNew && !met[stage]) next[stage] = { ...entry, recoveredAt: now };
    } else if (met[stage]) {
      delete next[stage];
    }
  }
  return next;
}

/** Evaluates every window against the current settings and
 * notified-state map, returning the notifications to send right now and
 * the updated map to persist.
 *
 * Each window can fire two independent stages, at most once each per
 * reset cycle: `threshold` (remaining allowance passed the configured
 * threshold) and, when `settings.notifyOnExhausted` is on, `exhausted`
 * (the window is fully used). A cycle is keyed by the *latest known*
 * resetsAt this window has ever reported (see `latestKnownResetsAt`),
 * not simply the most recent observation — fetches happen outside any
 * ordering guarantee, so an older, stale resetsAt can arrive after a
 * newer one and must not be mistaken for a fresh cycle. A demonstrably
 * later known resetsAt re-arms both stages; any other transition
 * (including to/from an unknown resetsAt, or to an equal-or-older known
 * value) preserves stage state.
 *
 * If a window is already exhausted the first time its threshold stage
 * would otherwise fire, only exhausted fires — telling the user a window
 * is fully used already implies it passed the threshold. Once an
 * unknown-reset window's exhausted stage has recorded a recovery, it
 * stops blocking threshold, so a window that drops out of exhaustion and
 * later re-crosses only the threshold boundary still gets that
 * notification.
 *
 * A muted source (see `sourceKey`) is skipped entirely: neither stage
 * notifies, and its notified-state entry is left untouched, so unmuting
 * re-arms any cycle not already notified before the mute.
 *
 * A window with a known resetsAt that has already passed (including
 * exactly 0, a real expired timestamp, not a stand-in for unknown) is
 * also skipped — the "current" endpoint returns the latest archived
 * snapshot per group without filtering out already-reset windows, so
 * without this guard, enabling alerts could immediately announce a
 * days-old exhausted window.
 *
 * Null/undefined entries in `windows` are skipped, so a caller can pass
 * an adapter result straight through even when some rows failed to
 * adapt. */
export function evaluateRateLimitWindows(
  windows: readonly (RateLimitWindowInfo | null | undefined)[],
  settings: NotificationThresholdSettings,
  notifiedMap: NotifiedMap,
  now: number = Date.now(),
): EvaluationResult {
  if (!settings.enabled) {
    return { toNotify: [], nextNotifiedMap: notifiedMap };
  }

  const muted = new Set(settings.mutedSourceKeys);
  const nextMap: NotifiedMap = { ...notifiedMap };
  const toNotify: NotificationToSend[] = [];

  for (const window of windows) {
    if (!window) continue;
    if (muted.has(sourceKey(window))) continue;
    if (window.resetsAt !== null && window.resetsAt * 1000 <= now) continue;

    const existing = nextMap[window.key];
    const priorResetsAt = existing?.resetsAt ?? null;
    // Roborev-ci finding: a stale observation (a known resetsAt strictly
    // older than the latest known cycle boundary already remembered for
    // this window) must be skipped entirely, not just "not treated as a
    // fresh cycle" -- its usedPercent describes the OLD cycle, not the
    // current one, so evaluating it against the current cycle's
    // (empty-so-far) stages can re-fire the same alert that cycle already
    // sent. Left completely untouched: no state change, no notification.
    if (priorResetsAt !== null && window.resetsAt !== null && window.resetsAt < priorResetsAt) {
      continue;
    }
    // Local-roborev finding: resetsAt alone can't catch a fetch response
    // that arrives out of order relative to one already evaluated for
    // this window when both report the same (or an unknown) resetsAt --
    // e.g. two racing tabs' polls resolving in reverse order. A window's
    // observedAtMs must also be monotonic; an older one is stale for the
    // same reason as an older resetsAt above, and is skipped the same
    // way (existing/priorObservedAtMs unknown never rejects anything).
    const priorObservedAtMs = existing?.lastObservedAtMs ?? null;
    if (
      priorObservedAtMs !== null &&
      window.observedAtMs !== null &&
      window.observedAtMs < priorObservedAtMs
    ) {
      continue;
    }
    const rearmed =
      !existing ||
      (priorResetsAt !== null && window.resetsAt !== null && window.resetsAt > priorResetsAt);
    const nextResetsAt = rearmed ? window.resetsAt : latestKnownTimestamp(priorResetsAt, window.resetsAt);
    const nextObservedAtMs = latestKnownTimestamp(priorObservedAtMs, window.observedAtMs);

    const threshold = resolveThresholdPercent(settings, window);
    const thresholdCrossed = remainingThresholdCrossed(window.usedPercent, threshold);

    let priorStages = rearmed || !existing ? {} : existing.stages;
    if (!rearmed) {
      // Local-roborev finding: gating unknown-reset recovery purely on
      // `nextResetsAt === null` meant a window that had EVER reported a
      // known resetsAt could never again record a recovery once its
      // observations went permanently unknown (null) -- nextResetsAt
      // stays pinned at that old known value forever (it only loses to a
      // newer KNOWN value, never to null), even long after that
      // remembered boundary has actually passed. Once the remembered
      // boundary is unknown OR already elapsed, there is no future
      // "later known resetsAt" transition left to rely on either, so
      // recovery-based rearming must be allowed to take over.
      const boundaryStale = nextResetsAt === null || nextResetsAt * 1000 <= now;
      priorStages = reconcileStageRecoveries(
        priorStages,
        { threshold: thresholdCrossed, exhausted: window.exhausted },
        now,
        boundaryStale,
      );
    }

    const exhaustedAlreadyFired = Boolean(priorStages.exhausted);
    const exhaustedShouldFire =
      settings.notifyOnExhausted && window.exhausted && !exhaustedAlreadyFired;
    // An exhausted stage that already fired and hasn't recovered yet
    // still blocks threshold, so a window that jumps straight to (or
    // starts at) fully-used produces exactly one notification.
    const exhaustedStillBlocksThreshold =
      exhaustedAlreadyFired && priorStages.exhausted?.recoveredAt === undefined;

    const thresholdAlreadyFired = Boolean(priorStages.threshold);
    const thresholdSuppressedByExhausted = exhaustedStillBlocksThreshold || exhaustedShouldFire;
    const thresholdShouldFire =
      thresholdCrossed && !thresholdAlreadyFired && !thresholdSuppressedByExhausted;
    // A crossing suppressed by exhausted (rather than not crossed at
    // all) is still recorded, silently, so a later poll that's simply no
    // longer exhausted (e.g. 100% -> 90%, still above the threshold
    // boundary) doesn't read as a brand-new crossing.
    const thresholdSilentlyCrossed =
      thresholdCrossed && !thresholdAlreadyFired && thresholdSuppressedByExhausted;

    if (!exhaustedShouldFire && !thresholdShouldFire) {
      if (rearmed) {
        // Nothing crosses yet on this fresh cycle, but the cycle
        // boundary itself must still be remembered -- otherwise a later,
        // stale/out-of-order observation reporting an OLDER resetsAt (see
        // the stale-observation skip above) would find no entry to
        // compare against and get evaluated as if it were the first
        // observation of a brand new window, re-firing the alert the
        // original cycle already sent (roborev-ci finding: this used to
        // delete the entry outright, discarding the boundary).
        if (nextResetsAt !== null || nextObservedAtMs !== null) {
          nextMap[window.key] = {
            resetsAt: nextResetsAt,
            lastObservedAtMs: nextObservedAtMs,
            stages: {},
          };
        } else if (existing) {
          delete nextMap[window.key];
        }
      } else {
        const stagesToKeep = thresholdSilentlyCrossed
          ? { ...priorStages, threshold: { notifiedAt: now } }
          : priorStages;
        if (Object.keys(stagesToKeep).length > 0 || nextObservedAtMs !== null) {
          nextMap[window.key] = {
            resetsAt: nextResetsAt,
            lastObservedAtMs: nextObservedAtMs,
            stages: stagesToKeep,
          };
        }
      }
      continue;
    }

    const nextStages: Partial<Record<NotificationStage, NotifiedStageEntry>> = { ...priorStages };
    if (thresholdShouldFire) {
      toNotify.push({ window, stage: "threshold" });
      nextStages.threshold = { notifiedAt: now };
    } else if (thresholdSilentlyCrossed) {
      nextStages.threshold = { notifiedAt: now };
    }
    if (exhaustedShouldFire) {
      toNotify.push({
        window,
        stage: "exhausted",
        // thresholdSilentlyCrossed was just marked fired above (in the
        // same call) without ever being queued -- if THIS notification's
        // delivery fails, that mark must roll back too, or a later poll
        // that's merely no longer exhausted (but still above the
        // threshold boundary) would find threshold already "fired" and
        // silently suppressed forever despite the user never having been
        // notified of either.
        ...(thresholdSilentlyCrossed ? { silentlyAlsoMarked: ["threshold"] as const } : {}),
      });
      nextStages.exhausted = { notifiedAt: now };
    }
    nextMap[window.key] = { resetsAt: nextResetsAt, lastObservedAtMs: nextObservedAtMs, stages: nextStages };
  }

  return { toNotify, nextNotifiedMap: nextMap };
}

/** Formats a minute duration in compact human terms, e.g. 300 -> "5h",
 * 10080 -> "7d", 90 -> "1h 30m". Shows at most two units. */
export function formatWindowDuration(minutes: number): string {
  if (!Number.isFinite(minutes) || minutes <= 0) return "0m";
  const totalMinutes = Math.round(minutes);
  const days = Math.floor(totalMinutes / 1440);
  const afterDays = totalMinutes % 1440;
  const hours = Math.floor(afterDays / 60);
  const mins = afterDays % 60;

  if (days > 0) {
    return hours > 0 ? `${days}d ${hours}h` : `${days}d`;
  }
  if (hours > 0) {
    return mins > 0 ? `${hours}h ${mins}m` : `${hours}h`;
  }
  return `${mins}m`;
}

/** Formats the time remaining until a unix-second reset timestamp, in
 * the same compact style as `formatWindowDuration`. Returns null when
 * resetsAt is unknown, and "0m" once the reset time has passed. */
export function formatTimeRemaining(resetsAt: number | null, nowMs: number): string | null {
  if (resetsAt === null || !Number.isFinite(resetsAt)) return null;
  const diffMs = resetsAt * 1000 - nowMs;
  if (diffMs <= 0) return "0m";
  return formatWindowDuration(Math.ceil(diffMs / 60_000));
}

function clampPercent(value: number): number {
  if (!Number.isFinite(value)) return 0;
  return Math.min(100, Math.max(0, value));
}

/** Shape of one row from `GET /api/v1/rate-limits/current`, as exposed
 * by the generated `ServiceRateLimitWindow` client type (see
 * frontend/src/lib/api/rateLimits.ts). Re-exported here rather than
 * imported ad hoc at each call site so the adapter has one clearly
 * documented entry point. */
export type CurrentRateLimitApiRow = ServiceRateLimitWindow;

/** The raw, stable identity used for keys and muting: accountId when
 * present, else the reporting machine, else "default". Deliberately
 * ignores accountLabel — a display nicety isn't guaranteed unique or
 * stable. */
function resolveAccount(row: CurrentRateLimitApiRow): string {
  const accountId = row.accountId?.trim() ?? "";
  if (accountId !== "") return accountId;
  const machine = row.machine?.trim() ?? "";
  if (machine !== "") return machine;
  return "default";
}

/** Probes a vendor-specific `details` blob for a known, window-scoped
 * exhaustion flag. The wire shape is a JSON-encoded string (see
 * `ServiceRateLimitWindow.details`), so this parses it defensively
 * before reading `extra_usage.spend_limit_reached` (currently the only
 * recognized flag, reported by Claude). */
function detailsExhaustedFlag(details: string | undefined): boolean {
  if (!details) return false;
  let parsed: unknown;
  try {
    parsed = JSON.parse(details);
  } catch {
    return false;
  }
  if (!parsed || typeof parsed !== "object") return false;
  const extraUsage = (parsed as Record<string, unknown>).extra_usage;
  if (!extraUsage || typeof extraUsage !== "object") return false;
  return (extraUsage as Record<string, unknown>).spend_limit_reached === true;
}

/** A window is exhausted once usedPercent reaches 100, or a vendor
 * reports its own WINDOW-SCOPED exhaustion flag (currently only
 * Claude's details.extra_usage.spend_limit_reached, which can arrive
 * slightly ahead of usedPercent reading exactly 100).
 *
 * Deliberately does NOT read Codex's rateLimitReachedType, even though
 * the generated type carries it: per the pinned Codex protocol
 * (docs/internal/session-format-sources.md), it is a snapshot-level
 * reason string with no per-window meaning — the Go parser copies the
 * same value onto both the primary and secondary row from one snapshot,
 * so honoring it here would mark whichever window isn't actually
 * exhausted as exhausted too. */
function resolveExhausted(row: CurrentRateLimitApiRow, usedPercent: number): boolean {
  if (usedPercent >= 100) return true;
  return detailsExhaustedFlag(row.details);
}

/** The identity shown in labels and notification titles: the API's
 * friendly accountLabel when present, else the same accountId/machine
 * fallback resolveAccount uses. */
function resolveDisplayIdentity(row: CurrentRateLimitApiRow): string {
  const label = row.accountLabel?.trim() ?? "";
  return label !== "" ? label : resolveAccount(row);
}

/** Adapts one raw "current" row into the normalized shape the evaluator
 * and notifier use. Returns null when the row can't be turned into a
 * usable window (missing/non-numeric usedPercent, or an empty vendor or
 * window kind — both required by the generated contract, but every
 * field is still checked defensively since JSON over the wire can't
 * enforce that) — the caller filters these out. */
export function adaptCurrentRateLimitRow(
  row: CurrentRateLimitApiRow | null | undefined,
): RateLimitWindowInfo | null {
  if (!row) return null;

  const windowKind = row.windowKind?.trim().toLowerCase() ?? "";
  if (windowKind === "") return null;
  if (typeof row.usedPercent !== "number" || !Number.isFinite(row.usedPercent)) return null;

  const vendor = row.vendor?.trim().toLowerCase() ?? "";
  if (vendor === "") return null;
  const account = resolveAccount(row);
  const planType = row.planType?.trim() ?? "";
  const limitId = row.limitId?.trim() || windowKind;
  const limitName = row.limitName?.trim() ?? "";
  const key = `${vendor}:${account}:${limitId}:${windowKind}`;

  const windowMinutes =
    typeof row.windowMinutes === "number" && Number.isFinite(row.windowMinutes) && row.windowMinutes > 0
      ? row.windowMinutes
      : null;
  const resetsAt =
    typeof row.resetsAt === "number" && Number.isFinite(row.resetsAt) ? row.resetsAt : null;
  const parsedObservedAt = typeof row.observedAt === "string" ? Date.parse(row.observedAt) : NaN;
  const observedAtMs = Number.isFinite(parsedObservedAt) ? parsedObservedAt : null;

  const displayIdentity = resolveDisplayIdentity(row);
  const accountLabel = planType ? `${displayIdentity} (${planType})` : displayIdentity;
  const usedPercent = clampPercent(row.usedPercent);

  return {
    key,
    vendor,
    account,
    planType,
    accountLabel,
    windowKind,
    limitId,
    limitName,
    usedPercent,
    resetsAt,
    exhausted: resolveExhausted(row, usedPercent),
    windowMinutes,
    observedAtMs,
  };
}
