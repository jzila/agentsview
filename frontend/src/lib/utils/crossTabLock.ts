/**
 * Cross-tab mutual exclusion for a critical section that must run in
 * at most one browser tab/window at a time — specifically,
 * rateLimitAlertRunner's read-evaluate-persist-notify sequence, where
 * two tabs racing on the same crossing could otherwise both decide to
 * notify and each persist an overlapping notified-state write.
 *
 * Uses the Web Locks API (https://developer.mozilla.org/en-US/docs/Web/API/Web_Locks_API)
 * exclusively: every participating context queues on it for real, with
 * no polling and no risk of two holders at once. It's available in
 * every browser and webview this app targets, including WKWebView for
 * the Tauri desktop build, so there's no fallback lock implementation
 * here — a hand-rolled localStorage-based substitute can't provide the
 * same guarantee (there is no atomic compare-and-swap on localStorage,
 * so two tabs can each observe "unclaimed" and both proceed) and was
 * removed after roborev-ci correctly flagged that exact gap. When
 * `navigator.locks` is genuinely unavailable, `fn` still runs (this
 * check must not silently stop delivering notifications), just without
 * cross-tab coordination — the same single-tab-only behavior this
 * feature had before cross-tab sync existed at all.
 */

const LOCK_NAME = "agentsview-rate-limit-alert-check";

interface NavigatorWithLocks {
  locks?: {
    request<T>(name: string, callback: () => T | Promise<T>): Promise<T>;
  };
}

let warnedNoWebLocks = false;

/** Runs `fn` with at most one tab/window in this browser profile
 * inside it at a time, so a check-and-persist sequence that reads
 * shared localStorage state, decides whether to notify, and writes an
 * updated notified-state map can't interleave with another tab's copy
 * of the same sequence and produce a duplicate alert.
 *
 * Without `navigator.locks` (unsupported context), runs `fn` directly
 * with no cross-tab coordination, logging once per page load so an
 * unexpectedly old/unusual environment is at least visible in the
 * console rather than silently degrading. */
export async function withCrossTabLock<T>(fn: () => T | Promise<T>): Promise<T> {
  const nav = (typeof navigator === "undefined" ? undefined : navigator) as
    | NavigatorWithLocks
    | undefined;
  if (nav?.locks && typeof nav.locks.request === "function") {
    return nav.locks.request(LOCK_NAME, fn);
  }

  if (!warnedNoWebLocks) {
    warnedNoWebLocks = true;
    console.warn(
      "[crossTabLock] navigator.locks unavailable; running without cross-tab coordination",
    );
  }
  return fn();
}
