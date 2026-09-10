import { fetchCurrentRateLimits } from "../api/rateLimits.js";
import { m } from "../i18n/index.js";
import { withCrossTabLock } from "../utils/crossTabLock.js";
import { getNotifier } from "../utils/notifier.js";
import { windowLabel } from "../utils/rateLimitFormat.js";
import {
  adaptCurrentRateLimitRow,
  evaluateRateLimitWindows,
  formatTimeRemaining,
  vendorDisplayName,
  type NotificationToSend,
  type RateLimitWindowInfo,
} from "../utils/rateLimitAlerts.js";
import { events } from "./events.svelte.js";
import { rateLimitAlertSettings } from "./rateLimitAlertSettings.svelte.js";
import { router } from "./router.svelte.js";

/** Fallback poll interval used alongside the SSE `data_changed` stream,
 * per the feature spec's ">= 60s if SSE is not suitable" fallback. Rate
 * limit snapshots don't yet have their own SSE scope, so this also
 * covers the case where a new snapshot lands without any of the
 * existing scopes ("messages" | "sessions" | "sync") firing. */
const FALLBACK_POLL_MS = 60_000;

/** Debounce for reacting to bursts of SSE events (e.g. a sync writing
 * many rows at once triggers many data_changed frames in a row). */
const EVENT_DEBOUNCE_MS = 1_000;

/** Builds a notification's window label the same way RateLimitCard
 * builds a card header -- via rateLimitFormat.ts's shared `windowLabel()`,
 * including its window-kind fallback for a window with no reported
 * duration -- so two windows on one account (e.g. two weekly buckets, or
 * a primary/secondary pair sharing a limit name with neither reporting a
 * duration) read differently: vendor and account identity, then the
 * named window ("Weekly limit [Spark]"). */
function notificationLabel(win: RateLimitWindowInfo): string {
  const namedWindow = m.rate_limits_card_header({
    window: windowLabel(win.windowMinutes ?? undefined, win.windowKind),
    name: win.limitName || win.limitId,
  });
  return `${vendorDisplayName(win.vendor)} · ${win.accountLabel} · ${namedWindow}`;
}

/** Attempts delivery of one notification, resolving to whether it actually
 * succeeded -- callers must await this (see `check()` below, inside the
 * same cross-tab lock that persists notified-state) and must not treat a
 * stage as delivered (and therefore consumed for this cycle) when it
 * resolves false, whether the failure is synchronous (unsupported, or the
 * browser notifier's `new Notification(...)` throwing) or async (the
 * desktop notifier's plugin import or IPC call failing). */
async function fireNotification({ window: win, stage }: NotificationToSend): Promise<boolean> {
  const notifier = getNotifier();
  if (!notifier.isSupported()) return false;

  const label = notificationLabel(win);
  const title =
    stage === "exhausted"
      ? m.rate_limit_exhausted_title({ label })
      : m.rate_limit_alert_title({ label, usedPercent: Math.round(win.usedPercent) });
  const resetIn = formatTimeRemaining(win.resetsAt, Date.now());
  const body = resetIn
    ? m.rate_limit_alert_body_with_reset({ resetIn })
    : m.rate_limit_alert_body_reset_unknown();

  return notifier.notify({
    title,
    body,
    // Tag includes the stage so the two stages' notifications don't
    // replace each other (both notifiers coalesce same-tag notifications;
    // see notifier.ts's hashTagToId for the desktop path).
    tag: `${win.key}:${stage}`,
    // Only the browser notifier can act on this -- see DesktopNotifier's
    // doc comment in utils/notifier.ts for why clicking a desktop
    // notification can't currently focus the window or navigate.
    onClick: () => {
      try {
        window.focus();
      } catch {
        // ignore focus failures (e.g. popup-blocked contexts)
      }
      router.navigate("usage");
    },
  });
}

/** Orchestrates the rate-limit threshold check: fetches the latest
 * "current" rate-limit snapshot, adapts it, evaluates it against the
 * user's thresholds and notified-state, fires any newly-crossed
 * notifications, and persists the updated notified-state map.
 *
 * Started from the app shell (see App.svelte's onMount) so it runs
 * regardless of which page the user is on, and reuses the app's
 * existing SSE data_changed stream (events.svelte.ts) with a >=60s
 * fallback poll for coverage while that stream doesn't yet carry a
 * rate-limit-specific scope. */
class RateLimitAlertRunner {
  private unsubscribeEvents: (() => void) | null = null;
  private pollTimer: ReturnType<typeof setInterval> | null = null;
  private checking = false;

  /** The most recently fetched, successfully-adapted windows. Read by
   * the settings panel to list observed usage sources (grouped by
   * vendor/account) for the per-source mute list and per-vendor
   * threshold overrides. Only populated once notifications are
   * enabled (see `check()`'s early return below), since that's the
   * only time this runner fetches. */
  observedWindows: RateLimitWindowInfo[] = $state([]);

  start(): () => void {
    if (!this.unsubscribeEvents) {
      this.unsubscribeEvents = events.subscribeDebounced(() => {
        void this.check();
      }, EVENT_DEBOUNCE_MS);
    }
    if (this.pollTimer === null) {
      this.pollTimer = setInterval(() => {
        void this.check();
      }, FALLBACK_POLL_MS);
    }
    // A background refreshPermission() (see check()'s early return below)
    // only updates the store's `permission` field; without this, a
    // permission granted outside the app (OS settings) between polls
    // would sit unused until the next timer tick or SSE event.
    rateLimitAlertSettings.onPermissionGranted = () => void this.check();
    void this.check();
    return () => this.stop();
  }

  stop(): void {
    this.unsubscribeEvents?.();
    this.unsubscribeEvents = null;
    if (this.pollTimer !== null) {
      clearInterval(this.pollTimer);
      this.pollTimer = null;
    }
    rateLimitAlertSettings.onPermissionGranted = null;
  }

  async check(): Promise<void> {
    if (this.checking) return;
    if (!rateLimitAlertSettings.enabled) return;
    if (rateLimitAlertSettings.permission !== "granted") {
      // Alerts are enabled but permission isn't (yet, or anymore) known
      // to be granted. This includes a permission check that failed
      // transiently on an earlier read (see notifier.ts's
      // DesktopNotifier.readPermission, which falls back to "default"
      // rather than reject) and was never retried, silently leaving a
      // previously-working setup inactive. Kick off a non-prompting
      // recheck so a later poll tick or SSE event can pick up a healed
      // (or freshly decided) permission state; this never calls
      // requestPermission(), only ever a read.
      rateLimitAlertSettings.refreshPermission();
      return;
    }

    this.checking = true;
    try {
      const rows = await fetchCurrentRateLimits();
      if (rows.length === 0) return;

      const windows = rows.map((row) => adaptCurrentRateLimitRow(row));
      this.observedWindows = windows.filter((w): w is RateLimitWindowInfo => w !== null);

      // Two tabs reacting to the same SSE event could each read their own
      // copy of the shared notified-state map and persist overlapping
      // writes, so withCrossTabLock runs the read-evaluate-persist-notify
      // sequence with at most one tab inside it at a time (Web Locks API).
      await withCrossTabLock(async () => {
        // Fresh read from localStorage right before evaluating, inside
        // the lock, so this tab acts on the latest state.
        rateLimitAlertSettings.hydrate();

        const result = evaluateRateLimitWindows(
          windows,
          rateLimitAlertSettings.snapshot(),
          rateLimitAlertSettings.notifiedMap,
        );

        // Roborev-ci finding: evaluateRateLimitWindows already marks every
        // toNotify entry as fired in nextNotifiedMap, regardless of
        // whether delivery actually succeeds -- persisting that as-is
        // would let a delivery failure permanently consume the alert
        // (every later poll, and every other tab, then sees the stage as
        // already notified and skips it for the rest of the cycle). Each
        // delivery is awaited (inside this same cross-tab lock, before
        // persisting) so a failure -- sync or async, browser or desktop --
        // is known before the map is written; the "notified" mark for any
        // stage whose delivery fails is undone, so it's still eligible to
        // fire on a later poll.
        // Roborev-ci finding: this loop awaits each delivery in turn, so a
        // settings change (the user disabling alerts) landing mid-loop --
        // after one await yields, before a later toSend is sent -- used to
        // keep firing and persisting the rest of the batch regardless,
        // since only the enabled state at the top of check() gated
        // anything. Rechecking the live `enabled` flag before each send
        // (skipping, not sending, once it flips) treats a mid-loop disable
        // the same as a delivery failure: skipped stages are rolled back
        // below and stay eligible for a later cycle instead of being
        // marked fired for a notification that was never sent.
        const notifiedMap = { ...result.nextNotifiedMap };
        for (const toSend of result.toNotify) {
          if (rateLimitAlertSettings.enabled && (await fireNotification(toSend))) continue;
          const entry = notifiedMap[toSend.window.key];
          if (!entry) continue;
          const stages = { ...entry.stages };
          delete stages[toSend.stage];
          // A stage silently marked fired as a side effect of this same
          // notification (never queued or delivered on its own -- see
          // NotificationToSend.silentlyAlsoMarked) must roll back too.
          for (const also of toSend.silentlyAlsoMarked ?? []) delete stages[also];
          notifiedMap[toSend.window.key] = { ...entry, stages };
        }
        rateLimitAlertSettings.setNotifiedMap(notifiedMap);
      });
    } finally {
      this.checking = false;
    }
  }
}

export const rateLimitAlertRunner = new RateLimitAlertRunner();
