import { fetchCurrentRateLimits } from "../api/rateLimits.js";
import { m } from "../i18n/index.js";
import { withCrossTabLock } from "../utils/crossTabLock.js";
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

/** Attempts delivery of one notification, returning whether it actually
 * got constructed -- callers must not treat a stage as delivered (and
 * therefore consumed for this cycle) when this returns false, since some
 * browsers throw off a user-gesture requirement or in restricted
 * contexts; see `check()`'s retry-eligibility handling below. */
function fireNotification({ window: win, stage }: NotificationToSend): boolean {
  if (typeof Notification === "undefined") return false;

  const label = notificationLabel(win);
  const title =
    stage === "exhausted"
      ? m.rate_limit_exhausted_title({ label })
      : m.rate_limit_alert_title({ label, usedPercent: Math.round(win.usedPercent) });
  const resetIn = formatTimeRemaining(win.resetsAt, Date.now());
  const body = resetIn
    ? m.rate_limit_alert_body_with_reset({ resetIn })
    : m.rate_limit_alert_body_reset_unknown();

  let notification: Notification;
  try {
    // Tag includes the stage so the two stages' notifications don't
    // replace each other (the browser coalesces same-tag notifications).
    notification = new Notification(title, { body, tag: `${win.key}:${stage}` });
  } catch {
    // Some browsers throw when constructing Notification off a user
    // gesture requirement or in restricted contexts; skip silently.
    return false;
  }

  notification.onclick = () => {
    try {
      window.focus();
    } catch {
      // ignore focus failures (e.g. popup-blocked contexts)
    }
    router.navigate("usage");
    notification.close();
  };
  return true;
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
  }

  async check(): Promise<void> {
    if (this.checking) return;
    if (!rateLimitAlertSettings.enabled) return;
    if (rateLimitAlertSettings.permission !== "granted") return;

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
      await withCrossTabLock(() => {
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
        // would let a Notification construction failure permanently
        // consume the alert (every later poll, and every other tab, then
        // sees the stage as already notified and skips it for the rest
        // of the cycle). Undo the "notified" mark for any stage whose
        // delivery fails, so it's still eligible to fire on a later poll.
        const notifiedMap = { ...result.nextNotifiedMap };
        for (const toSend of result.toNotify) {
          if (fireNotification(toSend)) continue;
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
