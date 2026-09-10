import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";

const isPermissionGrantedMock = vi.hoisted(() => vi.fn<() => Promise<boolean>>());
const requestPermissionMock = vi.hoisted(() => vi.fn<() => Promise<string>>());
// notify() bypasses the plugin's own (fire-and-forget) sendNotification()
// and invokes the underlying Tauri command directly -- see DesktopNotifier's
// doc comment -- so its tests mock @tauri-apps/api/core's invoke instead.
const invokeMock = vi.hoisted(() => vi.fn<(cmd: string, args?: unknown) => Promise<unknown>>());

vi.mock("@tauri-apps/plugin-notification", () => ({
  isPermissionGranted: isPermissionGrantedMock,
  requestPermission: requestPermissionMock,
}));
vi.mock("@tauri-apps/api/core", () => ({ invoke: invokeMock }));

import { getNotifier, hashTagToId, isDesktopShell } from "./notifier.js";

class FakeNotification {
  static permission: NotificationPermission = "default";
  static requestPermission = vi.fn<() => Promise<NotificationPermission>>();
  static instances: FakeNotification[] = [];
  onclick: (() => void) | null = null;
  close = vi.fn();
  constructor(
    public title: string,
    public options?: NotificationOptions,
  ) {
    FakeNotification.instances.push(this);
  }
}

let originalNotification: unknown;

/** Sets/clears `window.__TAURI_INTERNALS__`, the low-level Tauri v2 IPC
 * bridge this module's isDesktopShell() detects (never the legacy
 * `window.__TAURI__` -- see notifier.ts's top doc comment). */
function setTauriShell(present: boolean): void {
  if (present) (window as { __TAURI_INTERNALS__?: unknown }).__TAURI_INTERNALS__ = {};
  else delete (window as { __TAURI_INTERNALS__?: unknown }).__TAURI_INTERNALS__;
}

beforeEach(() => {
  originalNotification = (globalThis as { Notification?: unknown }).Notification;
  FakeNotification.permission = "default";
  FakeNotification.requestPermission = vi.fn(async () => "granted");
  FakeNotification.instances = [];
  (globalThis as { Notification?: unknown }).Notification = FakeNotification;
  setTauriShell(false);
  isPermissionGrantedMock.mockReset();
  requestPermissionMock.mockReset();
  invokeMock.mockReset().mockResolvedValue(undefined);
});

afterEach(() => {
  (globalThis as { Notification?: unknown }).Notification = originalNotification;
  setTauriShell(false);
  vi.restoreAllMocks();
});

describe("shell detection and notifier selection", () => {
  it.each([
    [false, "browser"],
    [true, "desktop"],
  ] as const)("__TAURI_INTERNALS__ present=%s -> isDesktopShell and getNotifier().kind = %s", (present, kind) => {
    setTauriShell(present);
    expect(isDesktopShell()).toBe(present);
    expect(getNotifier().kind).toBe(kind);
  });
});

describe("browser notifier", () => {
  it("reports unsupported (both sync and async) when Notification is undefined, without prompting", () => {
    (globalThis as { Notification?: unknown }).Notification = undefined;
    const notifier = getNotifier();
    expect(notifier.isSupported()).toBe(false);
    expect(notifier.getPermission()).toBe("unsupported");
    expect(FakeNotification.requestPermission).not.toHaveBeenCalled();
  });

  it("getPermission/readPermission both reflect Notification.permission synchronously", async () => {
    FakeNotification.permission = "denied";
    const notifier = getNotifier();
    expect(notifier.getPermission()).toBe("denied");
    await expect(notifier.readPermission()).resolves.toBe("denied");
  });

  it("requestPermission delegates to Notification.requestPermission", async () => {
    await expect(getNotifier().requestPermission()).resolves.toBe("granted");
    expect(FakeNotification.requestPermission).toHaveBeenCalledOnce();
  });

  it("notify constructs a tagged Notification, wires onClick to fire-then-close, and never throws even if construction does", () => {
    const onClick = vi.fn();
    getNotifier().notify({ title: "t", body: "b", tag: "tag-1", onClick });
    const [instance] = FakeNotification.instances;
    if (!instance) throw new Error("expected a constructed Notification instance");
    expect(instance.options).toMatchObject({ body: "b", tag: "tag-1" });
    instance.onclick?.();
    expect(onClick).toHaveBeenCalledOnce();
    expect(instance.close).toHaveBeenCalledOnce();

    getNotifier().notify({ title: "t", body: "b", tag: "tag-2" }); // no onClick: onclick stays unset
    expect(FakeNotification.instances[1]?.onclick).toBeNull();

    (globalThis as { Notification?: unknown }).Notification = class {
      constructor() {
        throw new Error("no user gesture");
      }
    };
    expect(() => getNotifier().notify({ title: "t", body: "b", tag: "x" })).not.toThrow();
  });
});

describe("desktop notifier", () => {
  beforeEach(() => setTauriShell(true));

  it("getPermission is a 'default' placeholder; readPermission maps the is_permission_granted command's null/true/false to default/granted/denied", async () => {
    // readPermission() bypasses the plugin's own isPermissionGranted() JS
    // wrapper and invokes the command directly -- see this describe
    // block's next test, and DesktopNotifier's doc comment, for why.
    expect(getNotifier().getPermission()).toBe("default");
    invokeMock.mockResolvedValueOnce(null);
    await expect(getNotifier().readPermission()).resolves.toBe("default");
    invokeMock.mockResolvedValueOnce(true);
    await expect(getNotifier().readPermission()).resolves.toBe("granted");
    invokeMock.mockResolvedValueOnce(false);
    await expect(getNotifier().readPermission()).resolves.toBe("denied");
  });

  it("readPermission bypasses the plugin's cached isPermissionGranted() wrapper, so a permission granted through the OS is observed on the next call", async () => {
    // Roborev-ci finding: the plugin's isPermissionGranted() JS wrapper
    // caches window.Notification.permission and only re-queries the OS
    // while that cache still reads "default" -- once set once (e.g. at
    // init), it returns the SAME stale value forever, so polling it could
    // never observe a permission later granted outside the app. Calling
    // the invoke command directly has no such cache: each call reflects
    // the current state, deliberately simulated here as flipping between
    // calls with no caching of its own on this side either.
    invokeMock.mockResolvedValueOnce(false);
    await expect(getNotifier().readPermission()).resolves.toBe("denied");
    invokeMock.mockResolvedValueOnce(true);
    await expect(getNotifier().readPermission()).resolves.toBe("granted");
  });

  it("readPermission never rejects (resolves 'default' and logs) even when the command rejects, and recovers on the next call", async () => {
    const consoleError = vi.spyOn(console, "error").mockImplementation(() => {});
    invokeMock.mockRejectedValueOnce(new Error("ipc timeout")).mockResolvedValueOnce(true);

    await expect(getNotifier().readPermission()).resolves.toBe("default");
    expect(consoleError).toHaveBeenCalled();
    await expect(getNotifier().readPermission()).resolves.toBe("granted");
  });

  it("requestPermission delegates to the plugin, and retries loading it after a rejected dynamic import instead of caching the failure forever", async () => {
    requestPermissionMock.mockResolvedValueOnce("denied");
    await expect(getNotifier().requestPermission()).resolves.toBe("denied");

    // Regression: a rejected import() used to be cached the same way a
    // successful one is, so one transient load failure permanently wedged
    // every later call into replaying that rejection. vi.resetModules()
    // gives this test its own notifier.ts with an empty plugin-load cache.
    vi.resetModules();
    vi.doMock("@tauri-apps/plugin-notification", () => {
      throw new Error("chunk load failed");
    });
    const fresh = await import("./notifier.js");
    await expect(fresh.getNotifier().requestPermission()).rejects.toThrow();

    vi.doMock("@tauri-apps/plugin-notification", () => ({
      isPermissionGranted: isPermissionGrantedMock,
      requestPermission: requestPermissionMock,
    }));
    requestPermissionMock.mockResolvedValueOnce("granted");
    await expect(fresh.getNotifier().requestPermission()).resolves.toBe("granted");
  });

  it("notify invokes the notification command directly with a stable numeric id derived from tag, resolves true, never touching onClick (no click callback exists on desktop)", async () => {
    // Bypasses the plugin's own sendNotification()/window.Notification,
    // which never expose a promise for a real delivery outcome -- see
    // DesktopNotifier's doc comment.
    const onClick = vi.fn();
    await expect(
      getNotifier().notify({ title: "t", body: "b", tag: "same-tag", onClick }),
    ).resolves.toBe(true);
    await expect(
      getNotifier().notify({ title: "t2", body: "b2", tag: "same-tag", onClick }),
    ).resolves.toBe(true);

    const [firstCall, secondCall] = invokeMock.mock.calls;
    if (!firstCall || !secondCall) throw new Error("expected two invoke calls");
    expect(firstCall[0]).toBe("plugin:notification|notify");
    const firstOptions = (firstCall[1] as { options: { id: number } }).options;
    const secondOptions = (secondCall[1] as { options: { id: number } }).options;
    expect(firstOptions.id).toBe(secondOptions.id);
    expect(typeof firstOptions.id).toBe("number");
    expect(onClick).not.toHaveBeenCalled();
  });

  it("notify resolves false (never rejects) when unsupported or when the command rejects, logging the latter", async () => {
    // Roborev-ci finding: notify() used to report success before the
    // command had actually resolved, so a caller marking the stage
    // notified on that basis could permanently lose an alert whose
    // delivery later failed. notify() now awaits the command and
    // resolves to whether it truly succeeded.
    const notifier = getNotifier(); // resolved while shell=true, from the outer beforeEach
    setTauriShell(false);
    await expect(notifier.notify({ title: "t", body: "b", tag: "x" })).resolves.toBe(false);
    expect(invokeMock).not.toHaveBeenCalled();
    setTauriShell(true);

    const consoleError = vi.spyOn(console, "error").mockImplementation(() => {});
    invokeMock.mockRejectedValueOnce(new Error("ipc failure"));
    await expect(getNotifier().notify({ title: "t", body: "b", tag: "x" })).resolves.toBe(false);
    expect(consoleError).toHaveBeenCalled();

    invokeMock.mockResolvedValueOnce(undefined);
    await expect(getNotifier().notify({ title: "t", body: "b", tag: "x" })).resolves.toBe(true);
  });
});

describe("hashTagToId", () => {
  it("is deterministic, differs across tags, and stays within a signed 32-bit range", () => {
    expect(hashTagToId("a")).toBe(hashTagToId("a"));
    expect(hashTagToId("a")).not.toBe(hashTagToId("b"));
    const id = hashTagToId("codex:machine-a:limit-1:pro:primary:threshold");
    expect(Number.isInteger(id)).toBe(true);
    expect(id).toBeGreaterThanOrEqual(-(2 ** 31));
    expect(id).toBeLessThan(2 ** 31);
  });
});
