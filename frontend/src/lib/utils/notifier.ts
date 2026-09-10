/** Notifier abstraction used by rateLimitAlertRunner to deliver alerts
 * through whichever notification surface is actually usable: the AgentsView
 * desktop shell's native notifications (via the official
 * `@tauri-apps/plugin-notification` package) when running inside it, or the
 * browser Notification API otherwise.
 *
 * Desktop detection checks for `window.__TAURI_INTERNALS__`, the low-level
 * IPC bridge Tauri v2 always injects into every webview it controls,
 * regardless of app config -- never user-agent sniffing. This is
 * deliberately not `window.__TAURI__`: that legacy global only appears when
 * `app.withGlobalTauri` is set, and nothing about its presence guarantees a
 * notification plugin shim was actually injected onto it. `__TAURI_INTERNALS__`
 * is the same bridge `@tauri-apps/api`'s `invoke()` relies on for every IPC
 * call an official plugin makes, so it's a more fundamental signal that this
 * is genuinely a Tauri webview.
 */

/** Mirrors the browser's `NotificationPermission`, plus "unsupported" for
 * environments with no usable notification surface at all (no Notification
 * API and no desktop shell notification plugin). */
export type NotifierPermission = "granted" | "denied" | "default" | "unsupported";

export interface NotifyOptions {
  title: string;
  body: string;
  /** Same-tag notifications replace each other instead of stacking. */
  tag: string;
  /** Invoked when the user clicks the notification. Only ever fires for the
   * browser notifier -- see DesktopNotifier's doc comment for why the
   * desktop path (tauri-plugin-notification 2.4.0) can't support this on
   * any desktop platform (macOS/Windows/Linux) today. */
  onClick?: () => void;
}

export interface Notifier {
  readonly kind: "browser" | "desktop";
  isSupported(): boolean;
  /** Best-effort synchronous guess. The desktop notifier's underlying
   * permission check is genuinely async, so this may return "default" as a
   * placeholder even when the real state is already known. Prefer
   * `readPermission()` for anything user-facing. */
  getPermission(): NotifierPermission;
  /** Authoritative permission read, without prompting. Always resolves
   * (never rejects) and never triggers a permission dialog. */
  readPermission(): Promise<NotifierPermission>;
  /** Must only be called as the direct result of a user gesture (the
   * enable toggle) -- both the browser and desktop permission prompts are
   * user-initiated. */
  requestPermission(): Promise<NotifierPermission>;
  /** Resolves once delivery has actually been attempted, to whether it
   * succeeded -- callers must await this (inside the same cross-tab lock
   * that persists notified-state) and treat a stage as still eligible to
   * retry on a later poll when it resolves false. Never rejects: any
   * failure (unsupported, the browser's `new Notification(...)` throwing,
   * the desktop plugin's dynamic import or IPC call failing) resolves
   * false instead. */
  notify(options: NotifyOptions): Promise<boolean>;
}

/** True when running inside the AgentsView desktop shell (a Tauri v2
 * webview). Detected via `window.__TAURI_INTERNALS__` -- see this file's
 * top doc comment for why, never user-agent sniffing. */
export function isDesktopShell(): boolean {
  return typeof window !== "undefined" && "__TAURI_INTERNALS__" in window;
}

/** The plugin module's exported shape this notifier needs. */
type NotificationPluginModule = typeof import("@tauri-apps/plugin-notification");

let notificationPluginPromise: Promise<NotificationPluginModule> | undefined;

/** Lazily loads `@tauri-apps/plugin-notification` via a dynamic import (Vite
 * code-splits it into its own chunk, so a plain browser tab -- which never
 * calls this, since `getNotifier()` only selects `DesktopNotifier` inside
 * the shell -- never fetches or evaluates it). Caches the successful import
 * so repeated notifier calls share one module instance; a rejected import
 * is NOT cached (cleared before re-throwing), so one transient load failure
 * doesn't permanently wedge every later permission check and notify() call
 * into replaying that same rejection for the rest of the session. */
function loadNotificationPlugin(): Promise<NotificationPluginModule> {
  if (!notificationPluginPromise) {
    notificationPluginPromise = import("@tauri-apps/plugin-notification").catch((err: unknown) => {
      notificationPluginPromise = undefined;
      throw err;
    });
  }
  return notificationPluginPromise;
}

type TauriInvoke = typeof import("@tauri-apps/api/core").invoke;

let tauriInvokePromise: Promise<TauriInvoke> | undefined;

/** Lazily loads `@tauri-apps/api/core`'s `invoke`, the same caching and
 * rejected-import handling as `loadNotificationPlugin` above. Used only
 * by `DesktopNotifier.notify()` -- see its doc comment for why the
 * plugin's own JS wrappers can't be awaited for a real delivery
 * outcome, so this bypasses them and calls the underlying Tauri command
 * directly. */
function loadTauriInvoke(): Promise<TauriInvoke> {
  if (!tauriInvokePromise) {
    tauriInvokePromise = import("@tauri-apps/api/core")
      .then((mod) => mod.invoke)
      .catch((err: unknown) => {
        tauriInvokePromise = undefined;
        throw err;
      });
  }
  return tauriInvokePromise;
}

function browserNotificationApiSupported(): boolean {
  return typeof Notification !== "undefined";
}

/** Deterministic 32-bit FNV-1a hash, used to turn a notification `tag`
 * string into the small integer id the `plugin:notification|notify`
 * command's `NotificationData.id` uses to replace an existing
 * notification instead of stacking a duplicate -- mirroring the browser
 * API's tag-based coalescing. Not cryptographic; collisions just mean two
 * distinct tags might coalesce, which is no worse than the pre-existing
 * per-stage tag collision surface. */
export function hashTagToId(tag: string): number {
  let hash = 0x811c9dc5;
  for (let i = 0; i < tag.length; i++) {
    hash ^= tag.charCodeAt(i);
    hash = Math.imul(hash, 0x01000193);
  }
  // Fit into a signed 32-bit range (NotificationData.id is a Rust i32).
  return hash | 0;
}

/** Browser Notification API notifier. Used outside the desktop shell. */
class BrowserNotifier implements Notifier {
  readonly kind = "browser" as const;

  isSupported(): boolean {
    return browserNotificationApiSupported();
  }

  getPermission(): NotifierPermission {
    return browserNotificationApiSupported() ? Notification.permission : "unsupported";
  }

  async readPermission(): Promise<NotifierPermission> {
    return this.getPermission();
  }

  async requestPermission(): Promise<NotifierPermission> {
    if (!browserNotificationApiSupported()) return "unsupported";
    return await Notification.requestPermission();
  }

  async notify({ title, body, tag, onClick }: NotifyOptions): Promise<boolean> {
    if (!browserNotificationApiSupported()) return false;
    let notification: Notification;
    try {
      notification = new Notification(title, { body, tag });
    } catch {
      // Some browsers throw when constructing Notification off a user
      // gesture requirement or in restricted contexts; skip silently.
      return false;
    }
    if (onClick) {
      notification.onclick = () => {
        onClick();
        notification.close();
      };
    }
    return true;
  }
}

/** Desktop shell notifier, backed by the official
 * `@tauri-apps/plugin-notification` package (dynamically imported -- see
 * `loadNotificationPlugin`'s doc comment) for permission checks/requests.
 *
 * `notify()` does NOT use that package's own `sendNotification()`, or the
 * `window.Notification` constructor it patches: both are fire-and-forget
 * by construction (the plugin's init script fires its `invoke()` call
 * from inside an un-awaited async IIFE), so neither exposes a promise a
 * caller could await for a real delivery outcome -- a `sendNotification()`
 * or `new Notification()` call that "returns" tells a caller nothing
 * about whether the underlying IPC to the Rust side actually succeeded.
 * `notify()` instead calls the plugin's underlying
 * `plugin:notification|notify` Tauri command directly, awaiting its own
 * promise (see `loadTauriInvoke`), so a real IPC failure is caught here
 * instead of surfacing only via an unawaited rejection after the fact.
 *
 * tauri-plugin-notification 2.4.0 does not deliver a click/action callback
 * on any desktop platform (macOS, Windows, Linux) -- its `onAction` /
 * `onNotificationReceived` listeners and the underlying
 * `plugin:notification|register_listener` command only exist in the
 * mobile (Android/iOS) implementation; desktop.rs has no equivalent. That
 * means clicking a desktop notification cannot currently focus the window
 * or navigate to the Usage route -- there is no event to react to. This is
 * a platform/plugin limitation, not a bug in this notifier: `notify()`
 * intentionally ignores `onClick` here rather than silently failing to
 * wire up a callback that would never fire.
 */
class DesktopNotifier implements Notifier {
  readonly kind = "desktop" as const;

  isSupported(): boolean {
    return isDesktopShell();
  }

  getPermission(): NotifierPermission {
    // isPermissionGranted() is async; callers needing a live read should
    // use readPermission() instead. A synchronous getter can only report
    // what's already known without an IPC round trip, so an unsupported
    // shell surface is the only synchronous case worth distinguishing.
    return this.isSupported() ? "default" : "unsupported";
  }

  async readPermission(): Promise<NotifierPermission> {
    if (!this.isSupported()) return "unsupported";
    try {
      const invoke = await loadTauriInvoke();
      // Bypasses the plugin's own isPermissionGranted() JS wrapper for
      // the same reason notify() bypasses sendNotification() -- see this
      // class's doc comment -- but the failure mode here is subtler than
      // fire-and-forget: that wrapper caches window.Notification.permission
      // and only re-queries the OS while the cache still reads "default"
      // (`if (window.Notification.permission !== 'default') return
      // Promise.resolve(...)` in the plugin's init script). Once the
      // cache has been set once -- at init, or after a request -- calling
      // it again returns the SAME stale value forever, no matter how many
      // times refreshPermission() polls it, so a permission later granted
      // through the OS outside the app would never be observed. The
      // underlying command has no such cache. The command's Rust return
      // type is `Option<bool>` (null/true/false for prompt/granted/denied,
      // per tauri-plugin-notification's commands.rs) -- null must map to
      // "default", not the "denied" a bare truthy check would give it.
      const granted = await invoke<boolean | null>("plugin:notification|is_permission_granted");
      if (granted === null) return "default";
      return granted ? "granted" : "denied";
    } catch (err) {
      // An IPC round trip (or, on the first call, a dynamic import) can
      // reject despite this method's documented never-reject contract.
      // "default" is the same not-yet-resolved placeholder getPermission()
      // uses, so callers don't need a separate error state; logged so a
      // recurring failure is still visible.
      console.error("[notifier] desktop readPermission() failed, treating as default", err);
      return "default";
    }
  }

  async requestPermission(): Promise<NotifierPermission> {
    if (!this.isSupported()) return "unsupported";
    const { requestPermission } = await loadNotificationPlugin();
    return await requestPermission();
  }

  async notify({ title, body, tag }: NotifyOptions): Promise<boolean> {
    if (!this.isSupported()) return false;
    try {
      const invoke = await loadTauriInvoke();
      // Matches the payload shape tauri-plugin-notification's own guest
      // JS uses internally (see this class's doc comment for why this
      // bypasses the plugin's own sendNotification()/window.Notification
      // wrappers): `options` is the command's NotificationData, `id`
      // mirroring the plugin's own tag-based coalescing.
      await invoke("plugin:notification|notify", { options: { id: hashTagToId(tag), title, body } });
      return true;
    } catch (err) {
      // The dynamic import, or the command itself, can fail (a transient
      // IPC/module-load hiccup); report failure so the caller
      // (rateLimitAlertRunner's check(), inside its cross-tab lock)
      // leaves the stage eligible to retry rather than marking it
      // notified before delivery is actually known to have succeeded.
      console.error("[notifier] desktop notify() failed", err);
      return false;
    }
  }
}

const browserNotifier = new BrowserNotifier();
const desktopNotifier = new DesktopNotifier();

/** Selects the notifier to use: the desktop shell's native notifications
 * when running inside it, the browser Notification API otherwise. */
export function getNotifier(): Notifier {
  return isDesktopShell() ? desktopNotifier : browserNotifier;
}
