import { getNotifier, type NotifierPermission } from "../utils/notifier.js";
import {
  clampSliderThresholdPercent,
  DEFAULT_THRESHOLD_PERCENT,
  type NotificationStage,
  type NotificationThresholdSettings,
  type NotifiedMap,
  type NotifiedStageEntry,
  type NotifiedWindowState,
} from "../utils/rateLimitAlerts.js";

// Rate-limit alert preferences are a purely client-side browser feature
// (the Notification API only exists in the browser, and there is no
// server-side use for "did this browser already fire this alert").
// Following the app's existing convention for display-only UI
// preferences (theme, layout, locale, yoked dates — all localStorage,
// see frontend/src/lib/stores/ui.svelte.ts and yokedDates.svelte.ts),
// this stays out of the server-backed `/api/v1/settings` surface.
const SETTINGS_KEY = "agentsview-rate-limit-alerts";
const NOTIFIED_KEY = "agentsview-rate-limit-alerts-notified";

const SETTINGS_VERSION = 2;
const NOTIFIED_VERSION = 2;

interface StoredSettings {
  version: 2;
  enabled: boolean;
  defaultThresholdPercent: number;
  vendorThresholdOverrides: Partial<Record<string, number>>;
  windowThresholdOverrides: Partial<Record<string, number>>;
  mutedSourceKeys: string[];
  notifyOnExhausted: boolean;
}

interface StoredNotifiedMap {
  version: 2;
  entries: NotifiedMap;
}

function getLocalStorage(): Storage | null {
  try {
    return globalThis.localStorage ?? null;
  } catch {
    return null;
  }
}

function defaultSettings(): StoredSettings {
  return {
    version: SETTINGS_VERSION,
    enabled: false,
    defaultThresholdPercent: DEFAULT_THRESHOLD_PERCENT,
    vendorThresholdOverrides: {},
    windowThresholdOverrides: {},
    mutedSourceKeys: [],
    notifyOnExhausted: true,
  };
}

/** Parses a `{ [key: string]: number }`-shaped override map from
 * untrusted JSON, keeping only finite-number values. Threshold values
 * are clamped to the settings-UI slider range here: every override
 * exposed by the panel (today, per-vendor) is set through the slider,
 * so a value stored before the slider existed (or corrupted by hand)
 * can't leave the control showing something out of range. */
function parseThresholdOverrides(raw: unknown): Partial<Record<string, number>> {
  if (!raw || typeof raw !== "object") return {};
  const result: Partial<Record<string, number>> = {};
  for (const [key, value] of Object.entries(raw as Record<string, unknown>)) {
    if (typeof value === "number" && Number.isFinite(value)) {
      result[key] = clampSliderThresholdPercent(value);
    }
  }
  return result;
}

function parseStoredSettings(raw: string | null): StoredSettings {
  if (!raw) return defaultSettings();
  try {
    const parsed: unknown = JSON.parse(raw);
    if (typeof parsed !== "object" || parsed === null) return defaultSettings();
    const value = parsed as Partial<StoredSettings>;
    const enabled = typeof value.enabled === "boolean" ? value.enabled : false;
    const defaultThresholdPercent = clampSliderThresholdPercent(
      Number(value.defaultThresholdPercent),
    );
    const mutedSourceKeys = Array.isArray(value.mutedSourceKeys)
      ? value.mutedSourceKeys.filter((key): key is string => typeof key === "string")
      : [];
    // Absent on settings stored before this stage existed; the shown
    // checkbox defaults on regardless of the threshold slider's value.
    const notifyOnExhausted =
      typeof value.notifyOnExhausted === "boolean" ? value.notifyOnExhausted : true;
    return {
      version: SETTINGS_VERSION,
      enabled,
      defaultThresholdPercent,
      vendorThresholdOverrides: parseThresholdOverrides(value.vendorThresholdOverrides),
      windowThresholdOverrides: parseThresholdOverrides(value.windowThresholdOverrides),
      mutedSourceKeys,
      notifyOnExhausted,
    };
  } catch {
    return defaultSettings();
  }
}

function isFiniteOrNull(value: unknown): value is number | null {
  return value === null || (typeof value === "number" && Number.isFinite(value));
}

const NOTIFICATION_STAGES: readonly NotificationStage[] = ["threshold", "exhausted"];

function parseNotifiedStages(
  raw: unknown,
): Partial<Record<NotificationStage, NotifiedStageEntry>> | null {
  if (typeof raw !== "object" || raw === null) return null;
  const obj = raw as Record<string, unknown>;
  const result: Partial<Record<NotificationStage, NotifiedStageEntry>> = {};
  for (const stage of NOTIFICATION_STAGES) {
    const stageEntry = obj[stage];
    if (typeof stageEntry !== "object" || stageEntry === null) continue;
    const { notifiedAt, recoveredAt } = stageEntry as {
      notifiedAt?: unknown;
      recoveredAt?: unknown;
    };
    if (typeof notifiedAt === "number" && Number.isFinite(notifiedAt)) {
      // recoveredAt must round-trip through parse, not just stringify --
      // dropping it here erases the unknown-reset recovery marker on
      // reload or cross-tab hydrate, re-suppressing a stage that had
      // already recovered and was waiting to fire again.
      result[stage] =
        typeof recoveredAt === "number" && Number.isFinite(recoveredAt)
          ? { notifiedAt, recoveredAt }
          : { notifiedAt };
    }
  }
  return result;
}

/** Parses one notified-map entry in its current shape:
 * `{ resetsAt, stages: { threshold?, exhausted? } }`. */
function parseNotifiedWindowState(rawEntry: unknown): NotifiedWindowState | null {
  if (typeof rawEntry !== "object" || rawEntry === null) return null;
  const entry = rawEntry as { resetsAt?: unknown; lastObservedAtMs?: unknown; stages?: unknown };
  if (!isFiniteOrNull(entry.resetsAt)) return null;

  const stages = parseNotifiedStages(entry.stages);
  if (!stages) return null;
  // Absent on entries written before this field existed; treated as
  // unknown, same as any other missing/invalid value here -- see
  // NotifiedWindowState.lastObservedAtMs's doc comment.
  const lastObservedAtMs = isFiniteOrNull(entry.lastObservedAtMs) ? entry.lastObservedAtMs : null;
  return { resetsAt: entry.resetsAt, lastObservedAtMs, stages };
}

function parseStoredNotifiedMap(raw: string | null): NotifiedMap {
  if (!raw) return {};
  try {
    const parsed: unknown = JSON.parse(raw);
    if (typeof parsed !== "object" || parsed === null) return {};
    const value = parsed as Partial<StoredNotifiedMap>;
    const entries = value.entries;
    if (!entries || typeof entries !== "object") return {};
    const result: NotifiedMap = {};
    for (const [key, rawEntry] of Object.entries(entries as Record<string, unknown>)) {
      const state = parseNotifiedWindowState(rawEntry);
      if (state) result[key] = state;
    }
    return result;
  } catch {
    return {};
  }
}

/** Rate-limit alert preferences and the once-per-cycle notified-state
 * map, backed by localStorage.
 *
 * The permission flow is deliberately narrow: `enable()` is the ONLY
 * method that calls `Notification.requestPermission()`, and it must
 * only ever be invoked as the direct result of the user turning the
 * master toggle on. Construction/hydration only ever *reads*
 * `Notification.permission` (a synchronous, side-effect-free getter) —
 * it never prompts.
 */
export class RateLimitAlertSettingsStore {
  enabled: boolean = $state(false);
  defaultThresholdPercent: number = $state(DEFAULT_THRESHOLD_PERCENT);
  /** Keyed by vendor id, e.g. "codex". */
  vendorThresholdOverrides: Partial<Record<string, number>> = $state({});
  /** Keyed by the full window key. Not surfaced by the settings panel
   * today (per-vendor overrides keep the UI small), but supported end
   * to end so a caller can still set one. */
  windowThresholdOverrides: Partial<Record<string, number>> = $state({});
  /** Source keys (see utils/rateLimitAlerts.ts `sourceKey`) the user
   * has muted. Storing the muted set (rather than an enabled set)
   * means a newly-observed source is on by default. */
  mutedSourceKeys: string[] = $state([]);
  /** Master switch for the second ("exhausted") notification stage.
   * Defaults to true and is shown regardless of the threshold slider's
   * value — see NotificationThresholdSettings.notifyOnExhausted. */
  notifyOnExhausted: boolean = $state(true);
  notifiedMap: NotifiedMap = $state({});
  /** True when the most recent write to SETTINGS_KEY/NOTIFIED_KEY threw
   * (e.g. storage full). While true, `hydrate()` skips reading the
   * corresponding key back from storage rather than overwriting
   * already-correct in-memory state with the stale value still on disk
   * -- see `hydrate()`'s doc comment. Cleared on the next successful
   * write to that key. */
  private settingsPersistFailed = false;
  private notifiedPersistFailed = false;
  /** Last known permission state for whichever notifier is active (browser
   * Notification API, or the desktop shell's native notifications --
   * see utils/notifier.ts). This field has one owner: `applyPermissionRead`
   * below, gated by `permissionReadGeneration` so a slower, older read
   * (e.g. the constructor's refreshPermission(), still in flight) can
   * never clobber a newer one (e.g. an enable() call started after it) --
   * see that method's doc comment. Never mutated outside an async read,
   * and never by anything that prompts. */
  permission: NotifierPermission = $state("default");
  /** Which notifier is backing `permission`/`enable()` -- "desktop" inside
   * the AgentsView desktop shell, "browser" otherwise. Read by the
   * settings panel to choose non-"browser" copy when running in the
   * shell. */
  notifierKind: "browser" | "desktop" = $state(getNotifier().kind);
  /** True when the most recent `enable()` call's `requestPermission()`
   * itself rejected (a transient IPC/plugin-load failure, not a real
   * denial) -- distinct from `permission`, which in that case is reset
   * to "default" and can't by itself tell the settings panel this was
   * an error rather than an ordinary not-yet-decided state. Cleared at
   * the start of every `enable()` call, so a retry that succeeds (or
   * fails a "normal" way, e.g. a real denial) stops showing it. */
  requestFailed: boolean = $state(false);
  /** Bumped by every call into `applyPermissionRead`; only the read that
   * is still the latest when it resolves may write `permission`. */
  private permissionReadGeneration = 0;
  /** Invoked so a caller (the poll runner) can run an immediate check
   * instead of waiting for the next timer tick or SSE event. Fired by
   * `refreshPermission()` only when its read settles on "granted" where
   * the field previously read something else (covers permission granted
   * through the OS outside the app, which only a background read would
   * otherwise observe) -- but by every successful `enable()` regardless
   * of whether permission just changed, since re-enabling while already
   * granted (no transition) can just as easily be uncovering an existing
   * threshold crossing. */
  onPermissionGranted: (() => void) | null = null;

  constructor(private readonly storage: Storage | null = getLocalStorage()) {
    this.hydrate();
    this.refreshPermission();
    // Re-hydrate on the `storage` event (fires in every OTHER same-origin
    // tab when this tab writes SETTINGS_KEY/NOTIFIED_KEY) so multiple open
    // tabs converge on whichever tab wrote last: a disable/mute takes
    // effect everywhere, and one tab's notified-state write suppresses a
    // duplicate alert in another tab's next check().
    if (typeof window !== "undefined" && typeof window.addEventListener === "function") {
      window.addEventListener("storage", this.handleStorageEvent);
    }
  }

  private readonly handleStorageEvent = (event: StorageEvent): void => {
    if (!this.storage) return;
    // A null key means the storage area was cleared wholesale; any other
    // key that isn't one of ours is unrelated and ignored.
    if (event.key !== null && event.key !== SETTINGS_KEY && event.key !== NOTIFIED_KEY) return;
    this.hydrate();
    // hydrate() picks up `enabled` from the writing tab but never touches
    // `permission`; refresh it too (read-only, never prompts) so a tab
    // that loaded before permission was granted elsewhere doesn't stay
    // stuck skipping every check().
    this.refreshPermission();
  };

  /** Re-reads persisted state from storage into memory. Roborev-ci
   * finding: unconditionally doing so defeats the in-memory fallback
   * `persistSettings()`/`persistNotified()` already have for a failed
   * write (storage full) -- a notification that fires and successfully
   * updates `notifiedMap` in memory, but fails to persist, would
   * otherwise be silently reverted by the very next hydrate() (the poll
   * runner's own cross-tab-lock call before each evaluation reads stale,
   * pre-notification state from disk and re-fires the same alert; a
   * failed settings write could likewise undo a mute or disable the same
   * way). Skips re-reading a key while its last write is known to have
   * failed, so already-correct in-memory state survives until a write
   * actually succeeds again. */
  hydrate(): void {
    if (!this.storage) return;
    if (!this.settingsPersistFailed) {
      const stored = parseStoredSettings(this.storage.getItem(SETTINGS_KEY));
      this.enabled = stored.enabled;
      this.defaultThresholdPercent = stored.defaultThresholdPercent;
      this.vendorThresholdOverrides = stored.vendorThresholdOverrides;
      this.windowThresholdOverrides = stored.windowThresholdOverrides;
      this.mutedSourceKeys = stored.mutedSourceKeys;
      this.notifyOnExhausted = stored.notifyOnExhausted;
    }
    if (!this.notifiedPersistFailed) {
      this.notifiedMap = parseStoredNotifiedMap(this.storage.getItem(NOTIFIED_KEY));
    }
  }

  /** Reads (never requests) the active notifier's current, authoritative
   * permission state -- never the synchronous `getPermission()` placeholder,
   * which `applyPermissionRead` would just overwrite moments later anyway.
   * Safe to call in the background (construction, a cross-tab storage
   * event, a poll that finds permission not yet granted): it only ever
   * reads. Fires `onPermissionGranted` itself on a transition into
   * "granted" -- unlike `enable()` below, nothing here needs to sequence
   * that after some other synchronous state change. */
  refreshPermission(): void {
    const notifier = getNotifier();
    this.notifierKind = notifier.kind;
    void this.applyPermissionRead(() => notifier.readPermission()).then(({ becameGranted }) => {
      if (becameGranted) this.onPermissionGranted?.();
    });
  }

  /** Runs one permission read/request `op` under a generation counter, so
   * only the read that's still the latest when it resolves writes
   * `permission` -- the single place that field is ever assigned. Without
   * this, two overlapping reads -- e.g. the constructor's own
   * `refreshPermission()` and a near-simultaneous `enable()` call -- can
   * resolve out of order: an older read started first but finishing later
   * would otherwise clobber a newer, more accurate result (concretely:
   * enable() grants and stores "granted", then the constructor's earlier,
   * slower read resolves with the "default"/"denied" it observed before
   * the grant and overwrites it, leaving the runner stuck skipping
   * check() until a later refresh). A superseded read's `becameGranted`
   * is always false, since it never gets to compare against or write the
   * current `permission`.
   *
   * Returns the resolved permission (regardless of whether it was
   * applied -- `enable()` needs it either way to decide whether to
   * prompt) and whether this read caused the transition into "granted".
   * Callers decide when to act on `becameGranted` rather than this method
   * firing `onPermissionGranted` itself, since the right moment can
   * depend on what the caller does next -- see `enable()`, which must
   * finish setting `enabled` first. */
  private async applyPermissionRead(
    op: () => Promise<NotifierPermission>,
  ): Promise<{ permission: NotifierPermission; becameGranted: boolean }> {
    const generation = ++this.permissionReadGeneration;
    const permission = await op();
    if (generation !== this.permissionReadGeneration) return { permission, becameGranted: false };
    const becameGranted = permission === "granted" && this.permission !== "granted";
    this.permission = permission;
    return { permission, becameGranted };
  }

  /** Turns the master toggle on. Requests permission from this user
   * gesture (never automatically); returns true when notifications end
   * up enabled, false (with the toggle reverted) on denial or an
   * unsupported notifier.
   *
   * The two notifiers gate the request differently: browsers only need
   * one prompt while permission is undecided ("default") -- requesting
   * again after a real "denied" is pointless, since a browser's
   * requestPermission() just re-resolves "denied" without UI. The
   * desktop notifier's state is platform-dependent --
   * tauri-plugin-notification's Windows shim initializes to "denied"
   * rather than "default" before the user has ever been asked (roborev
   * finding, kata 50c6) -- so gating on "default" there would leave
   * Windows users unable to ever request permission; request whenever
   * it isn't already granted instead. */
  async enable(): Promise<boolean> {
    const notifier = getNotifier();
    this.notifierKind = notifier.kind;
    this.requestFailed = false;
    if (!notifier.isSupported()) {
      await this.applyPermissionRead(async () => "unsupported");
      this.setEnabled(false);
      return false;
    }

    let read = await this.applyPermissionRead(() => notifier.readPermission());
    const shouldRequest =
      notifier.kind === "desktop" ? read.permission !== "granted" : read.permission === "default";
    if (shouldRequest) {
      try {
        read = await this.applyPermissionRead(() => notifier.requestPermission());
      } catch (err) {
        // Unlike readPermission() (documented to never reject),
        // requestPermission() has no such contract -- the desktop path's
        // dynamic plugin import, or its IPC call, can genuinely fail
        // here. Treat it the same as a request that simply didn't grant:
        // keep alerts off and fall back to "default" (the same
        // not-yet-resolved placeholder readPermission() itself uses on
        // its own failure path). `requestFailed` lets the settings panel
        // show a distinct "something went wrong" message instead of the
        // ordinary not-yet-decided copy for "default".
        console.error("[rateLimitAlertSettings] requestPermission() failed", err);
        await this.applyPermissionRead(async () => "default");
        this.requestFailed = true;
        this.setEnabled(false);
        return false;
      }
    }

    if (read.permission !== "granted") {
      this.setEnabled(false);
      return false;
    }

    this.setEnabled(true);
    // Fired on every successful enable(), not just a permission
    // transition -- roborev-ci finding: re-enabling the toggle while
    // permission was already granted (e.g. the user disabled and
    // re-enabled) never changes `permission`, so gating this on
    // becameGranted left an existing threshold crossing waiting for the
    // next poll/SSE event instead of checking immediately. Fired only
    // after `enabled` is already true, so the check() it triggers (see
    // the poll runner's onPermissionGranted wiring) sees notifications
    // as actually on rather than no-op'ing.
    this.onPermissionGranted?.();
    return true;
  }

  /** Turns the master toggle off. Never touches browser permission. */
  disable(): void {
    this.setEnabled(false);
  }

  setDefaultThresholdPercent(value: number): void {
    this.defaultThresholdPercent = clampSliderThresholdPercent(value);
    this.persistSettings();
  }

  setVendorThresholdOverride(vendor: string, value: number | null): void {
    const next = { ...this.vendorThresholdOverrides };
    if (value === null) {
      delete next[vendor];
    } else {
      next[vendor] = clampSliderThresholdPercent(value);
    }
    this.vendorThresholdOverrides = next;
    this.persistSettings();
  }

  setWindowThresholdOverride(windowKey: string, value: number | null): void {
    const next = { ...this.windowThresholdOverrides };
    if (value === null) {
      delete next[windowKey];
    } else {
      next[windowKey] = clampSliderThresholdPercent(value);
    }
    this.windowThresholdOverrides = next;
    this.persistSettings();
  }

  setNotifyOnExhausted(value: boolean): void {
    this.notifyOnExhausted = value;
    this.persistSettings();
  }

  isSourceMuted(sourceKey: string): boolean {
    return this.mutedSourceKeys.includes(sourceKey);
  }

  setSourceMuted(sourceKey: string, muted: boolean): void {
    const current = new Set(this.mutedSourceKeys);
    if (muted) {
      current.add(sourceKey);
    } else {
      current.delete(sourceKey);
    }
    this.mutedSourceKeys = [...current];
    this.persistSettings();
  }

  setNotifiedMap(map: NotifiedMap): void {
    this.notifiedMap = map;
    this.persistNotified();
  }

  /** A plain snapshot for the pure evaluator in utils/rateLimitAlerts.ts. */
  snapshot(): NotificationThresholdSettings {
    return {
      enabled: this.enabled,
      defaultThresholdPercent: this.defaultThresholdPercent,
      vendorThresholdOverrides: this.vendorThresholdOverrides,
      windowThresholdOverrides: this.windowThresholdOverrides,
      mutedSourceKeys: this.mutedSourceKeys,
      notifyOnExhausted: this.notifyOnExhausted,
    };
  }

  private setEnabled(enabled: boolean): void {
    this.enabled = enabled;
    this.persistSettings();
  }

  private persistSettings(): void {
    if (!this.storage) return;
    const stored: StoredSettings = {
      version: SETTINGS_VERSION,
      enabled: this.enabled,
      defaultThresholdPercent: this.defaultThresholdPercent,
      vendorThresholdOverrides: this.vendorThresholdOverrides,
      windowThresholdOverrides: this.windowThresholdOverrides,
      mutedSourceKeys: this.mutedSourceKeys,
      notifyOnExhausted: this.notifyOnExhausted,
    };
    try {
      this.storage.setItem(SETTINGS_KEY, JSON.stringify(stored));
      this.settingsPersistFailed = false;
    } catch {
      // Storage can be unavailable or full; in-memory state still works
      // for the current tab. hydrate() skips re-reading SETTINGS_KEY
      // until a write here succeeds again, so this failure doesn't get
      // silently reverted by the write's own next hydrate() call.
      this.settingsPersistFailed = true;
    }
  }

  private persistNotified(): void {
    if (!this.storage) return;
    const stored: StoredNotifiedMap = { version: NOTIFIED_VERSION, entries: this.notifiedMap };
    try {
      this.storage.setItem(NOTIFIED_KEY, JSON.stringify(stored));
      this.notifiedPersistFailed = false;
    } catch {
      // Same as above: best-effort persistence, tracked so hydrate()
      // doesn't revert an already-correct in-memory notifiedMap.
      this.notifiedPersistFailed = true;
    }
  }
}

export const rateLimitAlertSettings = new RateLimitAlertSettingsStore();
