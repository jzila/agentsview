import { describe, expect, it } from "vite-plus/test";

import {
  adaptCurrentRateLimitRow,
  clampSliderThresholdPercent,
  clampThresholdPercent,
  DEFAULT_THRESHOLD_PERCENT,
  deriveSources,
  deriveVendors,
  evaluateRateLimitWindows,
  formatTimeRemaining,
  formatWindowDuration,
  remainingThresholdCrossed,
  resolveThresholdPercent,
  sourceKey,
  vendorDisplayName,
  type CurrentRateLimitApiRow,
  type NotificationThresholdSettings,
  type NotificationToSend,
  type NotifiedMap,
  type RateLimitWindowInfo,
} from "./rateLimitAlerts.js";

function settings(
  overrides: Partial<NotificationThresholdSettings> = {},
): NotificationThresholdSettings {
  return {
    enabled: true,
    defaultThresholdPercent: 20,
    vendorThresholdOverrides: {},
    windowThresholdOverrides: {},
    mutedSourceKeys: [],
    notifyOnExhausted: true,
    ...overrides,
  };
}

// An offset from "now" so tests that omit resetsAt/now don't trip the
// expired-window guard; expiry/arm-rearm tests set both explicitly.
const FAR_FUTURE_RESETS_AT = Math.floor(Date.now() / 1000) + 3600;

function window(overrides: Partial<RateLimitWindowInfo> = {}): RateLimitWindowInfo {
  return {
    key: "codex:machine-a:limit-1:primary",
    vendor: "codex",
    account: "machine-a",
    planType: "pro",
    accountLabel: "machine-a (pro)",
    windowKind: "primary",
    limitId: "limit-1",
    limitName: "",
    usedPercent: 90,
    resetsAt: FAR_FUTURE_RESETS_AT,
    windowMinutes: 300,
    exhausted: false,
    observedAtMs: null,
    ...overrides,
  };
}

/** Minimal valid API row, matching the generated ServiceRateLimitWindow
 * contract's required fields, for adaptCurrentRateLimitRow tests. */
function apiRow(overrides: Partial<CurrentRateLimitApiRow> = {}): CurrentRateLimitApiRow {
  return {
    vendor: "codex",
    machine: "laptop",
    limitId: "limit-1",
    windowKind: "primary",
    usedPercent: 10,
    windowMinutes: 300,
    creditsHas: false,
    creditsUnlimited: false,
    observedAt: "2026-01-01T00:00:00Z",
    ...overrides,
  };
}

function notifyDescriptions(toNotify: readonly NotificationToSend[]): string[] {
  return toNotify.map((n) => `${n.window.key}:${n.stage}`);
}

const DEFAULT_KEY = "codex:machine-a:limit-1:primary";

/** Evaluates a sequence of polls for one window (the DEFAULT_KEY window
 * with each step's overrides layered on), threading the notified map
 * through, and returns each step's notification descriptions. Lets a
 * multi-poll regression read as one table instead of repeated
 * evaluate/expect/re-assign boilerplate. */
function runSequence(
  steps: readonly (Partial<RateLimitWindowInfo> & { now: number })[],
  s: NotificationThresholdSettings = settings(),
): string[][] {
  let map: NotifiedMap = {};
  const out: string[][] = [];
  for (const { now, ...overrides } of steps) {
    const result = evaluateRateLimitWindows([window(overrides)], s, map, now);
    out.push(notifyDescriptions(result.toNotify));
    map = result.nextNotifiedMap;
  }
  return out;
}

describe("threshold helpers", () => {
  it.each([
    [-5, 0],
    [150, 100],
    [19.6, 20],
    [Number.NaN, DEFAULT_THRESHOLD_PERCENT],
    [Number.POSITIVE_INFINITY, DEFAULT_THRESHOLD_PERCENT],
  ])("clampThresholdPercent(%s) -> %s (0-100)", (input, expected) => {
    expect(clampThresholdPercent(input)).toBe(expected);
  });

  it.each([
    [-5, 0],
    [100, 25],
    [26, 25],
    [12, 12],
    [Number.NaN, DEFAULT_THRESHOLD_PERCENT],
  ])("clampSliderThresholdPercent(%s) -> %s (0-25)", (input, expected) => {
    expect(clampSliderThresholdPercent(input)).toBe(expected);
  });

  it("resolves window override > vendor override > global default, in that order", () => {
    const key = "codex:a:1:primary";
    const s = settings({
      defaultThresholdPercent: 20,
      vendorThresholdOverrides: { codex: 10, claude: 5 },
      windowThresholdOverrides: { [key]: 3 },
    });
    const resolve = (vendor: string, k: string) => resolveThresholdPercent(s, window({ vendor, key: k }));
    expect(resolve("codex", key)).toBe(3);
    expect(resolve("codex", "codex:a:2:primary")).toBe(10);
    expect(resolve("claude", "claude:a:1:primary")).toBe(5);
    expect(resolve("gemini", "gemini:a:1:primary")).toBe(20);
  });

  it.each([
    [80, 20, true, "fires exactly at the boundary (>=)"],
    [79, 20, false, "does not fire just below the boundary"],
    [99, 0, false, "threshold 0 does not fire while anything remains"],
    [100, 0, true, "threshold 0 fires once nothing remains"],
    [0, 100, true, "threshold 100 fires as soon as anything is used"],
  ])("remainingThresholdCrossed(%s, %s) -> %s (%s)", (usedPercent, threshold, expected) => {
    expect(remainingThresholdCrossed(usedPercent, threshold)).toBe(expected);
  });
});

describe("evaluateRateLimitWindows - single poll", () => {
  it("does nothing when the master toggle is off, skips null/undefined windows, and never fires (or touches state) for a muted source", () => {
    expect(evaluateRateLimitWindows([window()], settings({ enabled: false }), {})).toEqual({
      toNotify: [],
      nextNotifiedMap: {},
    });
    expect(
      evaluateRateLimitWindows([window(), null, undefined], settings(), {}).toNotify,
    ).toHaveLength(1);
    expect(
      evaluateRateLimitWindows(
        [window({ usedPercent: 100, exhausted: true })],
        settings({ mutedSourceKeys: ["codex:machine-a"] }),
        {},
      ),
    ).toEqual({ toNotify: [], nextNotifiedMap: {} });
  });

  it("notifies once when a window first crosses its threshold, recording the notified-state entry", () => {
    const result = evaluateRateLimitWindows([window({ usedPercent: 85 })], settings(), {}, 1_000_000);
    expect(notifyDescriptions(result.toNotify)).toEqual([`${DEFAULT_KEY}:threshold`]);
    expect(result.nextNotifiedMap[DEFAULT_KEY]).toEqual({
      resetsAt: FAR_FUTURE_RESETS_AT,
      lastObservedAtMs: null,
      stages: { threshold: { notifiedAt: 1_000_000 } },
    });
  });

  it.each([
    [1_000, 99, 5_000_000, [], "a known, already-passed resetsAt is skipped (expired)"],
    [0, 99, 5_000_000, [], "resetsAt exactly 0 is a real expired timestamp, not 'unknown'"],
    [null, 99, 100, [`${DEFAULT_KEY}:threshold`], "a null (unknown) resetsAt is never expired"],
  ] as const)(
    "resetsAt=%s, usedPercent=%s, now=%s -> %s (%s)",
    (resetsAt, usedPercent, now, expected, _label) => {
      const result = evaluateRateLimitWindows([window({ resetsAt, usedPercent })], settings(), {}, now);
      expect(notifyDescriptions(result.toNotify)).toEqual([...expected]);
    },
  );

  it.each([
    [20, true, "exhausted", "first observed already exhausted: only exhausted fires, not both"],
    [0, true, "exhausted", "threshold 0 + notifyOnExhausted on: exhausted alone covers the crossing"],
    [0, false, "threshold", "threshold 0 + notifyOnExhausted off: threshold covers it instead"],
  ] as const)(
    "threshold=%s, notifyOnExhausted=%s -> exactly one notification, stage=%s (%s)",
    (defaultThresholdPercent, notifyOnExhausted, expectedStage, _label) => {
      const result = evaluateRateLimitWindows(
        [window({ usedPercent: 100, exhausted: true })],
        settings({ defaultThresholdPercent, notifyOnExhausted }),
        {},
        100,
      );
      expect(result.toNotify).toHaveLength(1);
      expect(result.toNotify[0]?.stage).toBe(expectedStage);
    },
  );

  it("only fires exhausted for the window actually at 100%, not a sibling window at low usage", () => {
    const primary = window({ usedPercent: 12, exhausted: false });
    const secondary = window({
      key: "codex:machine-a:limit-1:secondary",
      windowKind: "secondary",
      usedPercent: 100,
      exhausted: true,
    });
    const result = evaluateRateLimitWindows([primary, secondary], settings(), {}, 100);
    expect(notifyDescriptions(result.toNotify)).toEqual([
      "codex:machine-a:limit-1:secondary:exhausted",
    ]);
  });
});

// Every row plays one window through a sequence of polls (threading the
// notified map through, via runSequence) and asserts each poll's
// notifications. This is the two-stage/reset/recovery invariant surface:
// same-cycle suppression, rearm only on a strictly later known resetsAt
// (never regressed by a stale out-of-order observation), null<->known
// resetsAt transitions, unknown-reset recovery-based rearm for both
// stages, and how the two stages block/unblock each other.
// Every row plays one window through a sequence of polls (via
// runSequence) and asserts each poll's notifications: the
// two-stage/reset/recovery invariant surface.
describe("evaluateRateLimitWindows - sequences", () => {
  it.each([
    [
      "suppresses a same-cycle refire, re-arms on a later known resetsAt, then suppresses a fired stage through a usage dip within that new cycle",
      [
        { usedPercent: 95, resetsAt: 1_000, now: 100 },
        { usedPercent: 95, resetsAt: 1_000, now: 200 }, // same cycle: suppressed
        { usedPercent: 95, resetsAt: 2_000, now: 300 }, // later resetsAt: fresh cycle, fires
        { usedPercent: 10, resetsAt: 2_000, now: 400 }, // same (new) cycle, dips: stays suppressed, not a rearm
      ],
      [[`${DEFAULT_KEY}:threshold`], [], [`${DEFAULT_KEY}:threshold`], []],
    ],
    [
      "never regresses the remembered cycle on a stale, out-of-order resetsAt: 5000 -> 4000 -> 5000 notifies once, not 1 -> 0 -> 1",
      [
        { usedPercent: 90, resetsAt: 5_000, now: 100 },
        { usedPercent: 90, resetsAt: 4_000, now: 200 },
        { usedPercent: 90, resetsAt: 5_000, now: 300 },
      ],
      [[`${DEFAULT_KEY}:threshold`], [], []],
    ],
    [
      "preserves stage state across a known<->null resetsAt transition in either direction",
      [
        { usedPercent: 95, resetsAt: 1_000, now: 100 },
        { usedPercent: 95, resetsAt: null, now: 200 }, // transiently unknown: no refire
        { usedPercent: 95, resetsAt: 100, now: 300 }, // a newly-known (even old-looking) value is not proof of a reset
      ],
      [[`${DEFAULT_KEY}:threshold`], [], []],
    ],
    [
      "unknown resetsAt: threshold stays suppressed while crossed, refires only after a genuine dip below the boundary and re-cross",
      [
        { usedPercent: 85, resetsAt: null, now: 100 },
        { usedPercent: 90, resetsAt: null, now: 200 }, // still crossed: suppressed
        { usedPercent: 50, resetsAt: null, now: 300 }, // dips below boundary: recovery recorded, no fire
        { usedPercent: 88, resetsAt: null, now: 400 }, // re-crosses: fires again
      ],
      [[`${DEFAULT_KEY}:threshold`], [], [], [`${DEFAULT_KEY}:threshold`]],
    ],
    [
      "roborev-ci regression: a recovery marker recorded while resetsAt was null is still consumed once resetsAt later becomes known",
      [
        { usedPercent: 85, resetsAt: null, now: 100 },
        { usedPercent: 50, resetsAt: null, now: 200 }, // dips below boundary: recovery recorded
        { usedPercent: 88, resetsAt: 5_000, now: 300 }, // resetsAt now known, re-crosses: must fire again
      ],
      [[`${DEFAULT_KEY}:threshold`], [], [`${DEFAULT_KEY}:threshold`]],
    ],
    [
      "unknown resetsAt: exhausted refires after dropping below 100% and returning to fully used",
      [
        { usedPercent: 100, resetsAt: null, exhausted: true, now: 100 },
        { usedPercent: 40, resetsAt: null, exhausted: false, now: 200 }, // recovery recorded
        { usedPercent: 100, resetsAt: null, exhausted: true, now: 300 },
      ],
      [[`${DEFAULT_KEY}:exhausted`], [], [`${DEFAULT_KEY}:exhausted`]],
    ],
    [
      "fires threshold once exhausted recovers AND the boundary is genuinely re-crossed",
      [
        { usedPercent: 100, resetsAt: null, exhausted: true, now: 100 },
        { usedPercent: 70, resetsAt: null, exhausted: false, now: 200 }, // dips below the 80 boundary
        { usedPercent: 85, resetsAt: null, exhausted: false, now: 300 }, // re-crosses: fires
      ],
      [[`${DEFAULT_KEY}:exhausted`], [], [`${DEFAULT_KEY}:threshold`]],
    ],
    [
      "does not fire threshold on a bounce that recovers from exhausted but never dips below the threshold boundary",
      [
        { usedPercent: 100, resetsAt: null, exhausted: true, now: 100 },
        { usedPercent: 90, resetsAt: null, exhausted: false, now: 200 },
      ],
      [[`${DEFAULT_KEY}:exhausted`], []],
    ],
    [
      "fires threshold then exhausted once each across a rising sequence, and never refires while still exhausted",
      [
        { usedPercent: 50, resetsAt: 10_000, now: 100 },
        { usedPercent: 85, resetsAt: 10_000, now: 200 }, // crosses threshold (boundary 80)
        { usedPercent: 95, resetsAt: 10_000, now: 300 }, // still crossed, not exhausted: no refire
        { usedPercent: 100, resetsAt: 10_000, exhausted: true, now: 400 }, // exhausted fires, threshold does not
        { usedPercent: 100, resetsAt: 10_000, exhausted: true, now: 500 }, // stays exhausted: nothing
      ],
      [[], [`${DEFAULT_KEY}:threshold`], [], [`${DEFAULT_KEY}:exhausted`], []],
    ],
    [
      "roborev-ci regression: a later resetsAt that doesn't cross is still remembered (not deleted), so a subsequent stale/older resetsAt is skipped instead of re-firing the original alert",
      [
        { usedPercent: 90, resetsAt: 5_000, now: 100 }, // fires
        { usedPercent: 10, resetsAt: 6_000, now: 200 }, // fresh cycle, not crossed: must still remember 6000
        { usedPercent: 90, resetsAt: 5_000, now: 300 }, // stale (older than remembered 6000): skipped entirely
      ],
      [[`${DEFAULT_KEY}:threshold`], [], []],
    ],
    [
      "local-roborev regression: once a remembered known resetsAt has elapsed, unknown-reset observations may still record recovery instead of staying suppressed forever",
      [
        { usedPercent: 90, resetsAt: 5, now: 100 }, // fires; resetsAt 5s = 5000ms, still ahead of `now`
        { usedPercent: 90, resetsAt: null, now: 6_000 }, // remembered boundary (5000ms) has elapsed; still crossed: no refire
        { usedPercent: 50, resetsAt: null, now: 7_000 }, // dips below boundary: recovery now recorded (boundary is stale)
        { usedPercent: 88, resetsAt: null, now: 8_000 }, // re-crosses: fires again
      ],
      [[`${DEFAULT_KEY}:threshold`], [], [], [`${DEFAULT_KEY}:threshold`]],
    ],
    [
      "local-roborev regression: an out-of-order observation with an older observedAtMs is skipped as stale, even with an unchanged (unknown) resetsAt, so a delayed low-usage response can't fake a recovery",
      [
        { usedPercent: 90, resetsAt: null, observedAtMs: 2_000, now: 100 }, // fires
        { usedPercent: 10, resetsAt: null, observedAtMs: 1_000, now: 200 }, // delayed response describing OLDER data: skipped entirely
        { usedPercent: 90, resetsAt: null, observedAtMs: 3_000, now: 300 }, // still crossed: no refire (the stale dip never recorded a recovery)
      ],
      [[`${DEFAULT_KEY}:threshold`], [], []],
    ],
  ] as const)("%s", (_name, steps, expected) => {
    expect(
      runSequence(steps as unknown as (Partial<RateLimitWindowInfo> & { now: number })[]),
    ).toEqual([...expected]);
  });
});

describe("evaluateRateLimitWindows - muting across a sequence, and formatting helpers", () => {
  it("unmuting re-arms a not-yet-notified cycle but does not re-fire an already-notified one", () => {
    const enabled = settings();
    const muted = settings({ mutedSourceKeys: ["codex:machine-a"] });
    const evalAt = (s: NotificationThresholdSettings, map: NotifiedMap, now: number, resetsAt: number) =>
      evaluateRateLimitWindows([window({ usedPercent: 95, resetsAt })], s, map, now);

    const first = evalAt(enabled, {}, 100, 1_000);
    expect(first.toNotify).toHaveLength(1);

    const whileMuted = evalAt(muted, first.nextNotifiedMap, 200, 1_000);
    expect(whileMuted).toEqual({ toNotify: [], nextNotifiedMap: first.nextNotifiedMap });

    expect(evalAt(enabled, whileMuted.nextNotifiedMap, 300, 1_000).toNotify).toEqual([]); // same cycle
    expect(notifyDescriptions(evalAt(enabled, whileMuted.nextNotifiedMap, 400, 2_000).toNotify)).toEqual([
      `${DEFAULT_KEY}:threshold`,
    ]); // new cycle
  });

  it.each([
    [300, "5h"],
    [10_080, "7d"],
    [90, "1h 30m"],
    [1_500, "1d 1h"],
    [45, "45m"],
    [0, "0m"],
    [-5, "0m"],
    [Number.NaN, "0m"],
  ])("formatWindowDuration(%s) -> %s", (minutes, expected) => {
    expect(formatWindowDuration(minutes)).toBe(expected);
  });

  it.each([
    [null, 0, null, "unknown resetsAt"],
    [3 * 3600, 0, "3h", "compact units, 3 hours from now"],
    [10, 1_000_000, "0m", "already passed"],
  ] as const)("formatTimeRemaining(%s, %s) -> %s (%s)", (resetsAt, nowMs, expected, _label) => {
    expect(formatTimeRemaining(resetsAt, nowMs)).toBe(expected);
  });

  it.each([
    ["codex", "Codex"],
    ["claude", "Claude"],
    ["", ""],
  ])("vendorDisplayName(%s) -> %s", (input, expected) => {
    expect(vendorDisplayName(input)).toBe(expected);
  });
});

describe("sourceKey / deriveSources / deriveVendors / adaptCurrentRateLimitRow", () => {
  it("derives source keys/labels and vendors, sorted, distinguishing two machines on the same plan", () => {
    expect(sourceKey({ vendor: "codex", account: "machine-a" })).toBe("codex:machine-a");
    const windows = [
      window({ vendor: "codex", account: "machine-b", accountLabel: "plus" }),
      window({ key: "codex:machine-a:2:secondary", account: "machine-a", windowKind: "secondary" }),
      window({ key: "claude:acct-1:1:primary", vendor: "claude", account: "acct-1", accountLabel: "max" }),
    ];
    expect(deriveSources(windows).map((s) => `${s.key}=${s.label}`)).toEqual([
      "claude:acct-1=Claude · max",
      "codex:machine-a=Codex · machine-a (pro)",
      "codex:machine-b=Codex · plus",
    ]);
    expect(deriveVendors(windows)).toEqual(["claude", "codex"]);
  });

  it("adapts a full row, excluding planType from the key so a plan-label flip alone doesn't re-arm a window", () => {
    const adapted = adaptCurrentRateLimitRow(
      apiRow({ limitId: "codex-primary", limitName: "Codex", planType: "Pro", resetsAt: 5_000 }),
    );
    expect(adapted).toEqual({
      key: "codex:laptop:codex-primary:primary",
      vendor: "codex",
      account: "laptop",
      planType: "Pro",
      accountLabel: "laptop (Pro)",
      windowKind: "primary",
      limitId: "codex-primary",
      limitName: "Codex",
      usedPercent: 10,
      resetsAt: 5_000,
      windowMinutes: 300,
      exhausted: false,
      observedAtMs: Date.parse("2026-01-01T00:00:00Z"),
    });

    const free = adaptCurrentRateLimitRow(apiRow({ planType: "free", resetsAt: 1_000 }));
    const pro = adaptCurrentRateLimitRow(apiRow({ planType: "pro", resetsAt: 1_000 }));
    expect(free?.key).toBe(pro?.key);
  });

  it("normalizes vendor case, rejects missing required fields, falls back limitId/account/accountLabel sensibly, and clamps out-of-range usedPercent", () => {
    expect(adaptCurrentRateLimitRow(apiRow({ vendor: "Claude" }))?.vendor).toBe("claude");
    expect(adaptCurrentRateLimitRow(apiRow({ vendor: "" }))).toBeNull();
    expect(adaptCurrentRateLimitRow(apiRow({ windowKind: "" }))).toBeNull();
    expect(adaptCurrentRateLimitRow(null)).toBeNull();
    expect(adaptCurrentRateLimitRow(apiRow({ usedPercent: null as unknown as number }))).toBeNull();

    expect(adaptCurrentRateLimitRow(apiRow({ limitId: "", windowKind: "seven_day" }))?.limitId).toBe(
      "seven_day",
    );
    expect(adaptCurrentRateLimitRow(apiRow({ accountId: "acct-1", machine: "laptop" }))?.account).toBe(
      "acct-1",
    );
    expect(adaptCurrentRateLimitRow(apiRow({ accountId: undefined, machine: undefined }))?.account).toBe(
      "default",
    );

    // accountLabel leads display, but the raw accountId stays the stable
    // key/mute identity.
    const watt = adaptCurrentRateLimitRow(
      apiRow({ vendor: "claude", accountId: "c3691235-...-b6d1feec4a05", accountLabel: "Watt", planType: "pro" }),
    );
    expect(watt?.account).toBe("c3691235-...-b6d1feec4a05");
    expect(watt?.accountLabel).toBe("Watt (pro)");

    expect(adaptCurrentRateLimitRow(apiRow({ usedPercent: 142 }))?.usedPercent).toBe(100);
    expect(adaptCurrentRateLimitRow(apiRow({ usedPercent: -5 }))?.usedPercent).toBe(0);
    expect(adaptCurrentRateLimitRow(apiRow({ usedPercent: 142 }))?.exhausted).toBe(true);
  });

  const spendLimit = (reached: boolean) => JSON.stringify({ extra_usage: { spend_limit_reached: reached } });
  // rateLimitReachedType is a snapshot-level "why a limit was reached"
  // string the Go parser copies onto every row of one snapshot -- it
  // names no window, so it must never drive exhaustion (only usedPercent
  // and, for Claude, the details flag may).
  it.each([
    [12, "rate_limit_reached", undefined, false, "rateLimitReachedType alone, below 100%, never exhausts"],
    [100, "rate_limit_reached", undefined, true, "usedPercent >= 100 is exhausted regardless"],
    [90, undefined, spendLimit(true), true, "Claude's spend_limit_reached flag marks exhaustion ahead of 100%"],
    [90, undefined, spendLimit(false), false, "a false flag does not mark exhaustion"],
    [90, undefined, "not json", false, "an unparseable details string is ignored, not thrown"],
  ] as const)(
    "usedPercent=%s, rateLimitReachedType=%s, details=%s -> exhausted=%s (%s)",
    (usedPercent, rateLimitReachedType, details, expected, _label) => {
      expect(
        adaptCurrentRateLimitRow(apiRow({ usedPercent, rateLimitReachedType, details }))?.exhausted,
      ).toBe(expected);
    },
  );
});
