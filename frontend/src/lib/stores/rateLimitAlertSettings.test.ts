import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";

import { RateLimitAlertSettingsStore } from "./rateLimitAlertSettings.svelte.js";

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
    expect(store.permission).toBe("unsupported");
    store.hydrate();
    store.disable();

    const result = await store.enable();
    expect(result).toBe(false);
    expect(store.enabled).toBe(false);
    expect(store.permission).toBe("unsupported");
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
    expect(a.enabled).toBe(true);
    expect(a.permission).toBe("granted");
  });
});
