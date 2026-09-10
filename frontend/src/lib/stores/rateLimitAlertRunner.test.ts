import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";

import { initI18n } from "../i18n/index.js";
import type { CurrentRateLimitApiRow } from "../utils/rateLimitAlerts.js";
import { rateLimitAlertRunner } from "./rateLimitAlertRunner.svelte.js";

const fetchCurrentRateLimitsMock = vi.hoisted(() => vi.fn());
vi.mock("../api/rateLimits.js", () => ({ fetchCurrentRateLimits: fetchCurrentRateLimitsMock }));
vi.mock("./events.svelte.js", () => ({ events: { subscribeDebounced: vi.fn(() => () => {}) } }));

// rateLimitAlertRunner.svelte.ts imports the singleton store; replacing it
// wholesale lets this test control enabled/permission/notifiedMap directly.
const rateLimitAlertSettingsMock = vi.hoisted(() => ({
  enabled: true,
  permission: "granted" as string,
  notifiedMap: {} as Record<string, unknown>,
  snapshot: vi.fn(),
  setNotifiedMap: vi.fn(),
  hydrate: vi.fn(),
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
  rateLimitAlertSettingsMock.hydrate.mockReset();
  fetchCurrentRateLimitsMock.mockReset();
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe("rateLimitAlertRunner.check() - delivery failure retry eligibility", () => {
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
});
