// @vitest-environment jsdom
import { cleanup, fireEvent, render, waitFor } from "@testing-library/svelte";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";

import { initI18n } from "../../i18n/index.js";
import { rateLimitAlertRunner } from "../../stores/rateLimitAlertRunner.svelte.js";
import { rateLimitAlertSettings } from "../../stores/rateLimitAlertSettings.svelte.js";
import type { RateLimitWindowInfo } from "../../utils/rateLimitAlerts.js";
import RateLimitAlertSettings from "./RateLimitAlertSettings.svelte";

class FakeNotification {
  static permission: NotificationPermission = "default";
  static requestPermission = vi.fn<() => Promise<NotificationPermission>>();
}

let originalNotification: unknown;

function resetStore(permission: NotificationPermission = "default") {
  FakeNotification.permission = permission;
  localStorage.clear();
  rateLimitAlertSettings.hydrate();
  rateLimitAlertSettings.refreshPermission();
}

function testWindow(overrides: Partial<RateLimitWindowInfo> = {}): RateLimitWindowInfo {
  return {
    key: "codex:machine-a:limit-1:primary",
    vendor: "codex",
    account: "machine-a",
    planType: "pro",
    accountLabel: "machine-a (pro)",
    windowKind: "primary",
    limitId: "limit-1",
    limitName: "",
    usedPercent: 40,
    resetsAt: 1_000,
    windowMinutes: 300,
    exhausted: false,
    observedAtMs: null,
    ...overrides,
  };
}

beforeEach(() => {
  initI18n();
  originalNotification = (globalThis as { Notification?: unknown }).Notification;
  FakeNotification.requestPermission = vi.fn(async () => "granted");
  (globalThis as { Notification?: unknown }).Notification = FakeNotification;
  resetStore();
});

afterEach(() => {
  cleanup();
  (globalThis as { Notification?: unknown }).Notification = originalNotification;
  localStorage.clear();
  rateLimitAlertRunner.observedWindows = [];
  vi.restoreAllMocks();
});

describe("RateLimitAlertSettings", () => {
  it("never requests permission on mount", () => {
    render(RateLimitAlertSettings);
    expect(FakeNotification.requestPermission).not.toHaveBeenCalled();
  });

  // Every permission-state combination is unit-tested against the store
  // directly (rateLimitAlertSettings.test.ts); this only checks the
  // toggle wires up to it and surfaces the denied-message copy.
  it.each([
    ["granted", undefined],
    [
      "denied",
      "Browser notifications are blocked for this site. Allow notifications in your browser's site settings, then turn this back on.",
    ],
  ] as const)("prompt resolves %s -> toggle reflects it, message=%s", async (promptResult, expectMessage) => {
    FakeNotification.requestPermission = vi.fn(async () => promptResult);
    const { getByRole, getByText } = render(RateLimitAlertSettings);
    const toggle = getByRole("switch") as HTMLInputElement;

    await fireEvent.click(toggle);
    await waitFor(() => expect(toggle.checked).toBe(promptResult === "granted"));

    expect(rateLimitAlertSettings.enabled).toBe(promptResult === "granted");
    if (expectMessage) expect(getByText(expectMessage)).toBeTruthy();
  });

  it("shows controls only once enabled, saves the slider value, and keeps the exhausted checkbox (checked by default) visible at threshold 0", async () => {
    resetStore("granted");
    const { getByRole, queryByLabelText } = render(RateLimitAlertSettings);
    expect(queryByLabelText("Alert threshold percent")).toBeNull();

    await fireEvent.click(getByRole("switch"));
    const slider = (await waitFor(
      () => queryByLabelText("Alert threshold percent") as HTMLInputElement,
    ))!;
    await fireEvent.input(slider, { target: { value: "0" } });
    expect(rateLimitAlertSettings.defaultThresholdPercent).toBe(0);

    const exhausted = getByRole("checkbox", {
      name: "Also notify when a window is exhausted",
    }) as HTMLInputElement;
    expect(exhausted.checked).toBe(true);
    await fireEvent.click(exhausted);
    expect(rateLimitAlertSettings.notifyOnExhausted).toBe(false);
  });

  it("renders per-vendor override and per-source mute rows for observed sources (using a friendly accountLabel, not the raw UUID), persists an override's value, and mutes/unmutes", async () => {
    rateLimitAlertRunner.observedWindows = [
      testWindow(),
      testWindow({
        key: "claude:acct-1:limit-2:five_hour",
        vendor: "claude",
        account: "c3691235-4e59-4757-8432-b6d1feec4a05",
        planType: "max",
        accountLabel: "Watt (max)",
        windowKind: "five_hour",
        usedPercent: 60,
      }),
    ];
    resetStore("granted");
    const { getByRole, getByLabelText, getByText, queryByText } = render(RateLimitAlertSettings);
    await fireEvent.click(getByRole("switch"));

    expect(getByText("Claude · Watt (max)")).toBeTruthy();
    expect(queryByText(/c3691235/)).toBeNull();
    const sourceCheckbox = getByRole("checkbox", {
      name: "Codex · machine-a (pro) (machine-a)",
    }) as HTMLInputElement;

    await fireEvent.click(getByRole("checkbox", { name: "Use a different threshold for Claude" }));
    const overrideSlider = getByLabelText("Claude Alert threshold percent") as HTMLInputElement;
    await fireEvent.input(overrideSlider, { target: { value: "7" } });
    expect(rateLimitAlertSettings.vendorThresholdOverrides.claude).toBe(7);

    await fireEvent.click(sourceCheckbox);
    expect(rateLimitAlertSettings.isSourceMuted("codex:machine-a")).toBe(true);
    await fireEvent.click(sourceCheckbox);
    expect(rateLimitAlertSettings.isSourceMuted("codex:machine-a")).toBe(false);
  });

  it("shows an empty-state message when no sources have been observed yet", async () => {
    resetStore("granted");
    const { getByRole, getByText } = render(RateLimitAlertSettings);
    await fireEvent.click(getByRole("switch"));
    expect(getByText("Sources appear here once usage data is synced.")).toBeTruthy();
  });
});
