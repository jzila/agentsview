import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";

import { initI18n } from "../i18n/index.js";
import { getNotifier } from "../utils/notifier.js";
import type { CurrentRateLimitApiRow } from "../utils/rateLimitAlerts.js";
import { rateLimitAlertRunner } from "./rateLimitAlertRunner.svelte.js";

const fetchCurrentRateLimitsMock = vi.hoisted(() => vi.fn());
vi.mock("../api/rateLimits.js", () => ({ fetchCurrentRateLimits: fetchCurrentRateLimitsMock }));

// notifier.ts's DesktopNotifier dynamically imports the official plugin
// package (for permission checks) and invokes the notification command
// directly (bypassing the plugin's own fire-and-forget sendNotification()
// -- see DesktopNotifier's doc comment), so exercising the desktop
// notifier path here needs both mocked the same way notifier.test.ts does.
const invokeMock = vi.hoisted(() => vi.fn<(cmd: string, args?: unknown) => Promise<unknown>>());
vi.mock("@tauri-apps/plugin-notification", () => ({
  isPermissionGranted: vi.fn(async () => true),
  requestPermission: vi.fn(async () => "granted"),
}));
vi.mock("@tauri-apps/api/core", () => ({ invoke: invokeMock }));

const navigateMock = vi.hoisted(() => vi.fn());
vi.mock("./router.svelte.js", () => ({ router: { navigate: navigateMock } }));
vi.mock("./events.svelte.js", () => ({ events: { subscribeDebounced: vi.fn(() => () => {}) } }));

// rateLimitAlertRunner.svelte.ts imports the singleton store; replacing it
// wholesale lets each test control enabled/permission/notifiedMap directly
// without touching localStorage or the real Notification API.
const rateLimitAlertSettingsMock = vi.hoisted(() => ({
  enabled: true,
  permission: "granted" as string,
  notifiedMap: {} as Record<string, unknown>,
  snapshot: vi.fn(),
  setNotifiedMap: vi.fn(),
  refreshPermission: vi.fn(),
  hydrate: vi.fn(),
  onPermissionGranted: null as (() => void) | null,
}));
vi.mock("./rateLimitAlertSettings.svelte.js", () => ({
  rateLimitAlertSettings: rateLimitAlertSettingsMock,
}));

function currentRow(overrides: Partial<CurrentRateLimitApiRow> = {}): CurrentRateLimitApiRow {
  return {
    vendor: "codex",
    machine: "machine-a",
    limitId: "limit-1",
    planType: "pro",
    windowKind: "primary",
    usedPercent: 96,
    windowMinutes: 300,
    resetsAt: Math.floor(Date.now() / 1000) + 60,
    creditsHas: false,
    creditsUnlimited: false,
    observedAt: "2026-01-01T00:00:00Z",
    ...overrides,
  };
}

/** Sets/clears `window.__TAURI_INTERNALS__`, the Tauri v2 IPC bridge
 * notifier.ts's isDesktopShell() detects. */
function setDesktopShell(present: boolean) {
  if (present) (window as { __TAURI_INTERNALS__?: unknown }).__TAURI_INTERNALS__ = {};
  else delete (window as { __TAURI_INTERNALS__?: unknown }).__TAURI_INTERNALS__;
}

class FakeNotification {
  static shouldThrow = false;
  static instances: FakeNotification[] = [];
  onclick: (() => void) | null = null;
  close = vi.fn();
  constructor(
    public title: string,
    public options?: NotificationOptions,
  ) {
    if (FakeNotification.shouldThrow) throw new Error("no user gesture");
    FakeNotification.instances.push(this);
  }
}

beforeEach(() => {
  initI18n();
  (globalThis as { Notification?: unknown }).Notification = FakeNotification;
  FakeNotification.instances = [];
  FakeNotification.shouldThrow = false;
  rateLimitAlertSettingsMock.enabled = true;
  rateLimitAlertSettingsMock.permission = "granted";
  rateLimitAlertSettingsMock.notifiedMap = {};
  rateLimitAlertSettingsMock.snapshot.mockReturnValue({
    enabled: true,
    defaultThresholdPercent: 10,
    vendorThresholdOverrides: {},
    windowThresholdOverrides: {},
    mutedSourceKeys: [],
    notifyOnExhausted: true,
  });
  rateLimitAlertSettingsMock.setNotifiedMap.mockClear();
  rateLimitAlertSettingsMock.refreshPermission.mockClear();
  rateLimitAlertSettingsMock.hydrate.mockReset();
  fetchCurrentRateLimitsMock.mockReset();
  navigateMock.mockClear();
  invokeMock.mockReset().mockResolvedValue(undefined);
  setDesktopShell(false);
});

afterEach(() => {
  setDesktopShell(false);
  vi.restoreAllMocks();
});

describe("rateLimitAlertRunner.check()", () => {
  it("fires a browser Notification when a window crosses its threshold, and clicking it navigates to usage", async () => {
    fetchCurrentRateLimitsMock.mockResolvedValue([currentRow()]);

    await rateLimitAlertRunner.check();

    expect(FakeNotification.instances).toHaveLength(1);
    expect(rateLimitAlertSettingsMock.setNotifiedMap).toHaveBeenCalledOnce();
    FakeNotification.instances[0]?.onclick?.();
    expect(navigateMock).toHaveBeenCalledWith("usage");
  });

  it("does not fire, fetch, or touch state when disabled", async () => {
    rateLimitAlertSettingsMock.enabled = false;
    fetchCurrentRateLimitsMock.mockResolvedValue([currentRow()]);

    await rateLimitAlertRunner.check();

    expect(FakeNotification.instances).toHaveLength(0);
    expect(fetchCurrentRateLimitsMock).not.toHaveBeenCalled();
    expect(rateLimitAlertSettingsMock.refreshPermission).not.toHaveBeenCalled();
  });

  it.each(["denied", "default", "unsupported"] as const)(
    "when permission is %s, retries permission discovery (never a prompt) instead of fetching or firing",
    async (permission) => {
      rateLimitAlertSettingsMock.permission = permission;
      fetchCurrentRateLimitsMock.mockResolvedValue([currentRow()]);

      await rateLimitAlertRunner.check();

      expect(rateLimitAlertSettingsMock.refreshPermission).toHaveBeenCalledOnce();
      expect(fetchCurrentRateLimitsMock).not.toHaveBeenCalled();
      expect(FakeNotification.instances).toHaveLength(0);
    },
  );

  it("re-hydrates from the settings store after the fetch resolves and before evaluating, picking up another tab's write", async () => {
    // Roborev finding: two tabs reacting to the same SSE event could each
    // evaluate against their own in-memory notifiedMap before either
    // observed the other's `storage` event, so both could fire the same
    // alert. check() calls the store's (real) hydrate() right after its
    // network fetch resolves and before evaluating.
    const row = currentRow();
    const calls: string[] = [];
    fetchCurrentRateLimitsMock.mockImplementation(async () => {
      calls.push("fetch");
      // Stands in for a real hydrate() picking up another tab's write
      // that landed during this fetch -- an already-notified entry this
      // tab must honor instead of firing again.
      rateLimitAlertSettingsMock.hydrate.mockImplementation(() => {
        calls.push("hydrate");
        rateLimitAlertSettingsMock.notifiedMap = {
          "codex:machine-a:limit-1:primary": {
            resetsAt: row.resetsAt,
            stages: { threshold: { notifiedAt: Date.now() } },
          },
        };
      });
      return [row];
    });

    await rateLimitAlertRunner.check();

    expect(calls).toEqual(["fetch", "hydrate"]);
    expect(FakeNotification.instances).toHaveLength(0);
  });

  it("fires through the desktop notification command instead of the browser API when inside the shell", async () => {
    setDesktopShell(true);
    fetchCurrentRateLimitsMock.mockResolvedValue([currentRow()]);

    await rateLimitAlertRunner.check();

    expect(getNotifier().kind).toBe("desktop");
    expect(invokeMock).toHaveBeenCalledWith("plugin:notification|notify", expect.anything());
    expect(FakeNotification.instances).toHaveLength(0);
  });
});

describe("rateLimitAlertRunner.check() - delivery failure retry eligibility", () => {
  it("does not consume a stage when the desktop notification command rejects, so it's still eligible to fire on the next check()", async () => {
    // Roborev-ci finding: DesktopNotifier.notify() used to report success
    // before the command had actually resolved, so check() marked the
    // stage notified before an async failure was even known. notify()
    // now awaits the command and check() awaits notify(), inside the same
    // cross-tab lock that persists notified-state.
    setDesktopShell(true);
    invokeMock.mockRejectedValueOnce(new Error("ipc failure"));
    vi.spyOn(console, "error").mockImplementation(() => {});
    const row = currentRow();
    fetchCurrentRateLimitsMock.mockResolvedValue([row]);

    await rateLimitAlertRunner.check();

    expect(invokeMock).toHaveBeenCalledOnce();
    const persisted = rateLimitAlertSettingsMock.setNotifiedMap.mock.calls[0]?.[0];
    expect(persisted?.["codex:machine-a:limit-1:primary"]?.stages?.threshold).toBeUndefined();

    rateLimitAlertSettingsMock.notifiedMap = persisted;
    fetchCurrentRateLimitsMock.mockResolvedValue([row]);

    await rateLimitAlertRunner.check();

    expect(invokeMock).toHaveBeenCalledTimes(2);
  });

  it("does not consume a stage whose Notification construction throws, so it's still eligible to fire on the next check()", async () => {
    // Roborev-ci finding: evaluateRateLimitWindows marks a stage as fired
    // in the persisted map before delivery is even attempted; a runner
    // that persists that result unconditionally would let a failed
    // construction (e.g. off a user-gesture requirement) permanently
    // suppress the alert for the rest of the cycle.
    const row = currentRow();
    FakeNotification.shouldThrow = true;
    fetchCurrentRateLimitsMock.mockResolvedValue([row]);

    await rateLimitAlertRunner.check();

    expect(FakeNotification.instances).toHaveLength(0);
    const persisted = rateLimitAlertSettingsMock.setNotifiedMap.mock.calls[0]?.[0];
    expect(persisted?.["codex:machine-a:limit-1:primary"]?.stages?.threshold).toBeUndefined();

    // The (mocked) store reflects what check() just persisted, matching
    // the real store's behavior for a second check() in the same cycle.
    rateLimitAlertSettingsMock.notifiedMap = persisted;
    FakeNotification.shouldThrow = false;
    fetchCurrentRateLimitsMock.mockResolvedValue([row]);

    await rateLimitAlertRunner.check();

    expect(FakeNotification.instances).toHaveLength(1);
  });

  it("rolls back the silently-marked threshold stage too when a failed exhausted delivery was the only thing that set it, so a later still-crossed poll still notifies", async () => {
    // Roborev-ci finding: a window first observed already exhausted also
    // marks threshold fired (silently, no separate notification -- see
    // evaluateRateLimitWindows' doc comment) so a later "merely no longer
    // exhausted" poll doesn't misread as a fresh crossing. If the
    // exhausted delivery itself fails, undoing only the exhausted mark
    // left threshold "fired" despite the user never having seen either
    // notification, permanently suppressing a later still-above-threshold
    // poll.
    const resetsAt = Math.floor(Date.now() / 1000) + 3600;
    FakeNotification.shouldThrow = true;
    fetchCurrentRateLimitsMock.mockResolvedValue([currentRow({ usedPercent: 100, resetsAt })]);

    await rateLimitAlertRunner.check();

    expect(FakeNotification.instances).toHaveLength(0);
    const persisted = rateLimitAlertSettingsMock.setNotifiedMap.mock.calls[0]?.[0];
    expect(persisted?.["codex:machine-a:limit-1:primary"]?.stages).toEqual({});

    rateLimitAlertSettingsMock.notifiedMap = persisted;
    FakeNotification.shouldThrow = false;
    fetchCurrentRateLimitsMock.mockResolvedValue([currentRow({ usedPercent: 95, resetsAt })]);

    await rateLimitAlertRunner.check();

    expect(FakeNotification.instances).toHaveLength(1);
  });

  it("skips (and leaves eligible for retry) a still-queued notification when alerts are disabled mid-batch, without touching the one already in flight", async () => {
    // Roborev-ci finding: the delivery loop only consulted `enabled` once,
    // before the loop started, so a disable landing between two awaited
    // sends used to let every later send in the same batch fire anyway.
    setDesktopShell(true);
    const first = currentRow({ limitId: "limit-1" });
    const second = currentRow({ limitId: "limit-2" });
    invokeMock.mockImplementationOnce(async () => {
      rateLimitAlertSettingsMock.enabled = false; // flips while the first send is pending
      return undefined;
    });
    fetchCurrentRateLimitsMock.mockResolvedValue([first, second]);

    await rateLimitAlertRunner.check();

    expect(invokeMock).toHaveBeenCalledOnce(); // second send was skipped, not attempted
    const persisted = rateLimitAlertSettingsMock.setNotifiedMap.mock.calls[0]?.[0];
    expect(persisted?.["codex:machine-a:limit-1:primary"]?.stages?.threshold).toBeDefined();
    expect(persisted?.["codex:machine-a:limit-2:primary"]?.stages?.threshold).toBeUndefined();
  });
});

describe("rateLimitAlertRunner.start()", () => {
  it("wires onPermissionGranted to an immediate check(), and clears it on stop()", async () => {
    fetchCurrentRateLimitsMock.mockResolvedValue([]);
    const stop = rateLimitAlertRunner.start(); // start()'s own initial check() consumes this call
    expect(rateLimitAlertSettingsMock.onPermissionGranted).toBeTypeOf("function");
    await new Promise((resolve) => setTimeout(resolve, 0)); // let that initial check() finish

    fetchCurrentRateLimitsMock.mockClear().mockResolvedValue([currentRow()]);
    rateLimitAlertSettingsMock.onPermissionGranted?.();
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(fetchCurrentRateLimitsMock).toHaveBeenCalled();

    stop();
    expect(rateLimitAlertSettingsMock.onPermissionGranted).toBeNull();
  });
});
