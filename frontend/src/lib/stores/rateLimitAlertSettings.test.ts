import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";

import { RateLimitAlertSettingsStore } from "./rateLimitAlertSettings.svelte.js";

// notifier.ts's DesktopNotifier dynamically imports the official plugin
// package for requestPermission, and invokes the underlying Tauri
// commands directly for readPermission/notify (bypassing the plugin's
// own cached/fire-and-forget JS wrappers -- see its doc comment), so the
// desktop notifier permission flow tests below need both mocked (see
// notifier.test.ts for the same pattern).
const invokeMock = vi.hoisted(() => vi.fn<(cmd: string, args?: unknown) => Promise<unknown>>());
const requestPermissionMock = vi.hoisted(() => vi.fn<() => Promise<string>>());
vi.mock("@tauri-apps/plugin-notification", () => ({
  isPermissionGranted: vi.fn(),
  requestPermission: requestPermissionMock,
}));
vi.mock("@tauri-apps/api/core", () => ({ invoke: invokeMock }));

/** Sets/clears `window.__TAURI_INTERNALS__`, the Tauri v2 IPC bridge
 * notifier.ts's isDesktopShell() detects (see its doc comment). */
function setDesktopShell(present: boolean): void {
  if (present) {
    (window as { __TAURI_INTERNALS__?: unknown }).__TAURI_INTERNALS__ = {};
  } else {
    delete (window as { __TAURI_INTERNALS__?: unknown }).__TAURI_INTERNALS__;
  }
}

class FakeNotification {
  static permission: NotificationPermission = "default";
  static requestPermission = vi.fn<() => Promise<NotificationPermission>>();
}

let originalNotification: unknown;

beforeEach(() => {
  originalNotification = (globalThis as { Notification?: unknown }).Notification;
  FakeNotification.permission = "default";
  FakeNotification.requestPermission = vi.fn(async () => "granted");
  (globalThis as { Notification?: unknown }).Notification = FakeNotification;
  localStorage.clear();
});

afterEach(() => {
  (globalThis as { Notification?: unknown }).Notification = originalNotification;
  localStorage.clear();
  vi.restoreAllMocks();
});

describe("RateLimitAlertSettingsStore permission flow", () => {
  it.each([
    ["default", "granted", true, true, "granted", true],
    ["granted", "granted", true, true, "granted", false],
    ["default", "denied", false, false, "denied", true],
    ["denied", "denied", false, false, "denied", false],
  ] as const)(
    "starting permission=%s, prompt resolves %s -> enabled=%s, result=%s, permission=%s, prompted=%s",
    async (starting, promptResult, expectEnabled, expectResult, expectPermission, expectPrompted) => {
      FakeNotification.permission = starting;
      FakeNotification.requestPermission = vi.fn(async () => promptResult);
      const store = new RateLimitAlertSettingsStore();

      const result = await store.enable();

      expect(result).toBe(expectResult);
      expect(store.enabled).toBe(expectEnabled);
      expect(store.permission).toBe(expectPermission);
      expect(FakeNotification.requestPermission.mock.calls.length > 0).toBe(expectPrompted);
    },
  );

  it("reports unsupported and never prompts (on construction, hydrate(), disable(), or enable()) when Notification does not exist", async () => {
    delete (globalThis as { Notification?: unknown }).Notification;
    const store = new RateLimitAlertSettingsStore();
    await new Promise((resolve) => setTimeout(resolve, 0)); // settle construction's own async read
    expect(store.permission).toBe("unsupported");
    store.hydrate();
    store.disable();

    const result = await store.enable();
    expect(result).toBe(false);
    expect(store.enabled).toBe(false);
    expect(store.permission).toBe("unsupported");
  });
});

describe("RateLimitAlertSettingsStore desktop notifier permission flow", () => {
  beforeEach(() => {
    setDesktopShell(true);
    invokeMock.mockReset();
    requestPermissionMock.mockReset();
  });

  afterEach(() => {
    setDesktopShell(false);
    vi.restoreAllMocks();
  });

  // Windows' tauri-plugin-notification shim initializes to "denied" (not
  // "default") before the user has ever been asked, so enable() must
  // still request whenever permission isn't already granted -- gating on
  // "default" (the browser notifier's rule) would leave Windows users
  // unable to ever request.
  it.each([
    [false, "granted", true, true, true, "granted"],
    [false, "denied", true, false, false, "denied"],
    [true, "granted", false, true, true, "granted"],
  ] as const)(
    "initialGranted=%s, request resolves %s -> requested=%s, result=%s, enabled=%s, permission=%s",
    async (initialGranted, requestResult, expectRequested, expectResult, expectEnabled, expectPermission) => {
      invokeMock.mockResolvedValue(initialGranted);
      requestPermissionMock.mockResolvedValue(requestResult);
      const store = new RateLimitAlertSettingsStore();

      const result = await store.enable();

      expect(requestPermissionMock.mock.calls.length > 0).toBe(expectRequested);
      expect(result).toBe(expectResult);
      expect(store.enabled).toBe(expectEnabled);
      expect(store.permission).toBe(expectPermission);
    },
  );

  it("keeps alerts off, sets requestFailed, and never throws when requestPermission() itself rejects (unlike readPermission, which never does)", async () => {
    const consoleError = vi.spyOn(console, "error").mockImplementation(() => {});
    invokeMock.mockResolvedValue(false);
    requestPermissionMock.mockRejectedValue(new Error("ipc timeout"));
    const store = new RateLimitAlertSettingsStore();

    await expect(store.enable()).resolves.toBe(false);

    expect(store.enabled).toBe(false);
    expect(store.permission).toBe("default");
    expect(store.requestFailed).toBe(true);
    expect(consoleError).toHaveBeenCalled();
  });

  it("never calls requestPermission on construction/hydration, even on the desktop path (background reads never prompt)", async () => {
    invokeMock.mockResolvedValue(false);
    new RateLimitAlertSettingsStore();
    // A macrotask boundary flushes every pending microtask regardless of
    // how many .then()/await hops deep the constructor's own async
    // refreshPermission() is.
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(requestPermissionMock).not.toHaveBeenCalled();
  });

  it("does not clobber a newer permission with an older read that resolves later (generation-gated ownership)", async () => {
    // Roborev-ci finding: the constructor's own refreshPermission() read
    // can still be in flight when enable() runs and grants; if that older
    // read is allowed to write `permission` once it finally resolves, it
    // overwrites the fresh "granted" with the stale value it observed
    // before the grant, and the runner goes back to skipping check().
    let resolveConstructorRead!: (granted: boolean) => void;
    invokeMock.mockReturnValueOnce(
      new Promise((resolve) => {
        resolveConstructorRead = resolve;
      }),
    );
    const store = new RateLimitAlertSettingsStore(); // kicks off the slow read above

    invokeMock.mockResolvedValue(true);
    requestPermissionMock.mockResolvedValue("granted");
    await store.enable(); // a newer read+request, resolves before the constructor's
    expect(store.permission).toBe("granted");

    resolveConstructorRead(false); // the stale, slower read finally settles
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(store.permission).toBe("granted"); // unchanged by the stale read
  });

  it("fires onPermissionGranted exactly once on the transition to granted, after enabled is already true", async () => {
    invokeMock.mockResolvedValue(false);
    requestPermissionMock.mockResolvedValue("granted");
    const store = new RateLimitAlertSettingsStore();
    await new Promise((resolve) => setTimeout(resolve, 0)); // settle construction's own read

    const onGranted = vi.fn(() => expect(store.enabled).toBe(true));
    store.onPermissionGranted = onGranted;
    await store.enable();

    expect(onGranted).toHaveBeenCalledOnce();
  });

  it("also fires onPermissionGranted on a successful enable() when permission was already granted (no transition) -- re-enabling can uncover an existing crossing", async () => {
    invokeMock.mockResolvedValue(true);
    const store = new RateLimitAlertSettingsStore();
    await new Promise((resolve) => setTimeout(resolve, 0));

    const onGranted = vi.fn();
    store.onPermissionGranted = onGranted;
    await expect(store.enable()).resolves.toBe(true);

    expect(onGranted).toHaveBeenCalledOnce();
  });

  it("recovers from a rejected permission read on a later refreshPermission() call, never throwing in between", async () => {
    vi.spyOn(console, "error").mockImplementation(() => {});
    invokeMock.mockRejectedValueOnce(new Error("ipc timeout")).mockResolvedValueOnce(true);
    const store = new RateLimitAlertSettingsStore();
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(store.permission).toBe("default");

    store.refreshPermission(); // the non-prompting recheck rateLimitAlertRunner.check() calls each poll
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(store.permission).toBe("granted");
  });
});

describe("RateLimitAlertSettingsStore persistence", () => {
  it("persists and reloads thresholds, mutes, exhausted flag, and the notified map", () => {
    const store = new RateLimitAlertSettingsStore();
    store.setDefaultThresholdPercent(15);
    store.setVendorThresholdOverride("claude", 5);
    store.setWindowThresholdOverride("codex:machine-a:1:secondary", 3);
    store.setSourceMuted("codex:machine-b", true);
    store.setNotifyOnExhausted(false);
    store.setNotifiedMap({
      "codex:machine-a:1:primary": {
        resetsAt: 1000,
        lastObservedAtMs: 5_000,
        stages: { threshold: { notifiedAt: 500 }, exhausted: { notifiedAt: 100, recoveredAt: 200 } },
      },
    });

    const reloaded = new RateLimitAlertSettingsStore();
    expect(reloaded.defaultThresholdPercent).toBe(15);
    expect(reloaded.vendorThresholdOverrides).toEqual({ claude: 5 });
    expect(reloaded.windowThresholdOverrides).toEqual({ "codex:machine-a:1:secondary": 3 });
    expect(reloaded.mutedSourceKeys).toEqual(["codex:machine-b"]);
    expect(reloaded.notifyOnExhausted).toBe(false);
    expect(reloaded.notifiedMap).toEqual({
      "codex:machine-a:1:primary": {
        resetsAt: 1000,
        lastObservedAtMs: 5_000,
        stages: { threshold: { notifiedAt: 500 }, exhausted: { notifiedAt: 100, recoveredAt: 200 } },
      },
    });
  });

  it("clears an override, unmutes a source, and clamps a runtime threshold write, each back to its unset/default state", () => {
    const store = new RateLimitAlertSettingsStore();
    store.setVendorThresholdOverride("codex", 10);
    store.setVendorThresholdOverride("codex", null);
    expect(store.vendorThresholdOverrides.codex).toBeUndefined();

    store.setSourceMuted("codex:machine-a", true);
    store.setSourceMuted("codex:machine-a", false);
    expect(store.mutedSourceKeys).toEqual([]);

    store.setDefaultThresholdPercent(100);
    expect(store.defaultThresholdPercent).toBe(25);
    store.setDefaultThresholdPercent(-5);
    expect(store.defaultThresholdPercent).toBe(0);
  });

  it("reads stored settings defensively: clamps an out-of-range threshold, defaults a missing notifyOnExhausted to true, and falls back to defaults for corrupt JSON", () => {
    expect(new RateLimitAlertSettingsStore().notifyOnExhausted).toBe(true);

    localStorage.setItem(
      "agentsview-rate-limit-alerts",
      JSON.stringify({
        enabled: false,
        defaultThresholdPercent: 80,
        vendorThresholdOverrides: { codex: 999 },
        windowThresholdOverrides: {},
        mutedSourceKeys: [],
        // notifyOnExhausted intentionally omitted.
      }),
    );
    const clamped = new RateLimitAlertSettingsStore();
    expect(clamped.defaultThresholdPercent).toBe(25);
    expect(clamped.vendorThresholdOverrides.codex).toBe(25);
    expect(clamped.notifyOnExhausted).toBe(true);

    localStorage.setItem("agentsview-rate-limit-alerts", "not json");
    localStorage.setItem("agentsview-rate-limit-alerts-notified", "not json");
    const corrupt = new RateLimitAlertSettingsStore();
    expect(corrupt.enabled).toBe(false);
    expect(corrupt.mutedSourceKeys).toEqual([]);
    expect(corrupt.notifiedMap).toEqual({});
  });

  it("preserves an already-correct in-memory notifiedMap across hydrate() when persisting it fails, until a write succeeds again", () => {
    // Roborev-ci finding: unconditional hydrate() re-reads the stale,
    // pre-notification value still on disk after a failed write (storage
    // full), silently reverting a notification the store already fired
    // and recorded in memory -- letting the same alert fire again on the
    // runner's next poll.
    const backing = new Map<string, string>();
    let failNotified = false;
    const flakyStorage: Storage = {
      getItem: (key) => backing.get(key) ?? null,
      setItem: (key, value) => {
        if (failNotified && key === "agentsview-rate-limit-alerts-notified") {
          throw new Error("quota exceeded");
        }
        backing.set(key, value);
      },
      removeItem: (key) => void backing.delete(key),
      clear: () => backing.clear(),
      key: () => null,
      length: 0,
    };
    const store = new RateLimitAlertSettingsStore(flakyStorage);
    const entry = { "codex:machine-a:1:primary": { resetsAt: 1_000, stages: {} } };

    failNotified = true;
    store.setNotifiedMap(entry);
    store.hydrate(); // must not revert to the stale (empty) on-disk value
    expect(store.notifiedMap).toEqual(entry);

    failNotified = false;
    store.setNotifiedMap({}); // a successful write clears the failed flag
    store.hydrate();
    expect(store.notifiedMap).toEqual({});
  });
});

describe("RateLimitAlertSettingsStore cross-tab sync", () => {
  // Simulates the `storage` event a real browser fires in every OTHER
  // same-origin tab. `storageArea` is omitted (jsdom's fallback Storage
  // isn't a real Storage instance); the store's handler doesn't check it.
  function dispatchStorageEvent(key: string | null): void {
    window.dispatchEvent(new StorageEvent("storage", { key }));
  }

  it("picks up another tab's disable, mute, and notified-state writes via storage events", () => {
    const a = new RateLimitAlertSettingsStore();
    const b = new RateLimitAlertSettingsStore();

    b.setSourceMuted("codex:machine-a", true);
    dispatchStorageEvent("agentsview-rate-limit-alerts");
    expect(a.isSourceMuted("codex:machine-a")).toBe(true);

    b.setNotifiedMap({ "codex:machine-a:limit-1:primary": { resetsAt: 1_000, stages: {} } });
    dispatchStorageEvent("agentsview-rate-limit-alerts-notified");
    expect(a.notifiedMap).toEqual({
      "codex:machine-a:limit-1:primary": { resetsAt: 1_000, lastObservedAtMs: null, stages: {} },
    });
  });

  it("ignores a storage event for an unrelated key", () => {
    const a = new RateLimitAlertSettingsStore();
    const b = new RateLimitAlertSettingsStore();
    b.disable();
    dispatchStorageEvent("some-other-app-key");
    expect(a.enabled).toBe(false); // still the constructor default, unaffected by b
  });

  it("refreshes permission on a storage event too, so a tab constructed before permission was granted elsewhere doesn't stay stuck skipping check() forever", async () => {
    FakeNotification.permission = "default";
    const a = new RateLimitAlertSettingsStore();
    expect(a.permission).toBe("default");

    const b = new RateLimitAlertSettingsStore();
    await b.enable();
    FakeNotification.permission = "granted";

    dispatchStorageEvent("agentsview-rate-limit-alerts");
    await new Promise((resolve) => setTimeout(resolve, 0)); // settle the storage handler's own async read
    expect(a.enabled).toBe(true);
    expect(a.permission).toBe("granted");
  });
});
