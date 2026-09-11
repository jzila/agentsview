// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import type { ServiceRateLimitWindow } from "../../api/generated/index";
import { sessions } from "../../stores/sessions.svelte.js";
import { usage } from "../../stores/usage.svelte.js";

const rateLimitsServiceMocks = vi.hoisted(() => ({
  getApiV1RateLimitsCurrent: vi.fn(),
  getApiV1RateLimitsHistory: vi.fn(),
}));

vi.mock("../../api/runtime.js", async (importOriginal) => {
  const orig = await importOriginal<typeof import("../../api/runtime.js")>();
  return {
    ...orig,
    callGenerated: vi.fn((request: (options?: { signal?: AbortSignal }) => Promise<unknown>) =>
      request(),
    ),
  };
});

vi.mock("../../api/generated/index", async (importOriginal) => {
  const orig = await importOriginal<typeof import("../../api/generated/index")>();
  return {
    ...orig,
    RateLimitsService: rateLimitsServiceMocks,
  };
});

const { rateLimits } = await import("../../stores/ratelimits.svelte.js");
const { default: RateLimitsSection } = await import("./RateLimitsSection.svelte");

function snapshot(overrides: Partial<ServiceRateLimitWindow> = {}): ServiceRateLimitWindow {
  return {
    vendor: "codex",
    machine: "laptop",
    limitId: "codex",
    planType: "pro",
    windowKind: "primary",
    usedPercent: 95,
    windowMinutes: 10080,
    resetsAt: Math.floor(Date.now() / 1000) + 3600,
    creditsHas: true,
    creditsUnlimited: false,
    creditsBalance: "2714.1675630000",
    observedAt: "2026-09-09T10:00:00Z",
    ...overrides,
  };
}

let component: ReturnType<typeof mount> | undefined;

afterEach(() => {
  if (component) {
    void unmount(component);
    component = undefined;
  }
  vi.clearAllMocks();
  rateLimits.current = [];
  rateLimits.history = {};
  rateLimits.loaded = false;
  rateLimits.refreshToken = 0;
  usage.excludedAgents = "";
  usage.selectedTimeRange = null;
  sessions.filters.agent = "";
  document.body.innerHTML = "";
});

async function mountSection(from = "2026-09-01", to = "2026-09-09") {
  component = mount(RateLimitsSection, { target: document.body, props: { from, to } });
  await tick();
  await tick();
  await tick();
}

describe("RateLimitsSection", () => {
  it("renders nothing when there are zero snapshots", async () => {
    rateLimitsServiceMocks.getApiV1RateLimitsCurrent.mockResolvedValue([]);
    rateLimitsServiceMocks.getApiV1RateLimitsHistory.mockResolvedValue([]);
    await mountSection();
    expect(document.body.textContent?.trim()).toBe("");
  });

  it("renders a card per snapshot, grouped by machine, with a history chart once history has more than one point", async () => {
    rateLimitsServiceMocks.getApiV1RateLimitsCurrent.mockResolvedValue([
      snapshot({ machine: "laptop" }),
      snapshot({ machine: "desktop" }),
    ]);
    rateLimitsServiceMocks.getApiV1RateLimitsHistory.mockResolvedValue([
      snapshot({ observedAt: "2026-09-08T10:00:00Z", usedPercent: 80 }),
      snapshot({ observedAt: "2026-09-09T10:00:00Z", usedPercent: 95 }),
    ]);
    await mountSection();

    // Header shows the window label on its own, and a separate
    // scope-badge only when the vendor reports a limitName; a
    // 10080-minute window with no limitName omits the badge entirely.
    expect(document.querySelector(".window-label")?.textContent).toBe("Weekly limit");
    expect(document.querySelector(".scope-badge")).toBeNull();
    expect(document.querySelector(".used-percent")?.textContent).toContain("95");
    expect(document.body.textContent).toContain("pro");
    // formatCreditsBalance truncates and comma-formats the display text;
    // the raw value survives in the title attribute for the tooltip.
    expect(document.body.textContent).toContain("2,714");
    expect(document.querySelector(".meta-value[title]")?.getAttribute("title")).toBe(
      "2714.1675630000",
    );
    expect(document.querySelector(".progress-track")?.getAttribute("aria-valuenow")).toBe("95");
    expect(document.querySelector(".history svg")).not.toBeNull();

    // Codex snapshots carry no account identity, so the account group is
    // keyed by machine: two machines render as two separate groups.
    expect(document.querySelectorAll(".account-group")).toHaveLength(2);
    const accountTitles = [...document.querySelectorAll(".account-title")].map((el) => el.textContent);
    expect(accountTitles).toEqual(expect.arrayContaining(["laptop", "desktop"]));
  });

  // No windowMinutes: "primary"/"secondary" are Codex slot names, not
  // fixed durations (either slot can carry either window length), so
  // guessing "Session limit" from the slot name would sometimes be
  // wrong (roborev finding); a neutral "— limit" is used instead.
  it("falls back to a neutral label when windowMinutes is unknown", async () => {
    rateLimitsServiceMocks.getApiV1RateLimitsCurrent.mockResolvedValue([
      snapshot({ windowMinutes: undefined, windowKind: "primary" }),
    ]);
    rateLimitsServiceMocks.getApiV1RateLimitsHistory.mockResolvedValue([]);
    await mountSection();
    expect(document.querySelector(".window-label")?.textContent).toContain("— limit");
  });

  it("shows reset time unknown when resetsAt is absent", async () => {
    rateLimitsServiceMocks.getApiV1RateLimitsCurrent.mockResolvedValue([snapshot({ resetsAt: undefined })]);
    rateLimitsServiceMocks.getApiV1RateLimitsHistory.mockResolvedValue([]);
    await mountSection();
    expect(document.querySelector(".resets")?.textContent?.trim()).toBe("Reset time unknown");
  });

  it("hides the section when the Usage page excludes Codex, and when the shared agent filter excludes it", async () => {
    rateLimitsServiceMocks.getApiV1RateLimitsCurrent.mockImplementation(
      async (params: { agent?: string }) =>
        params.agent && params.agent !== "codex" ? [] : [snapshot()],
    );
    rateLimitsServiceMocks.getApiV1RateLimitsHistory.mockResolvedValue([]);

    usage.excludedAgents = "codex";
    await mountSection();
    expect(document.querySelector(".rate-limits-grid")).toBeNull();
    usage.excludedAgents = "";

    await mountSection();
    expect(document.querySelector(".rate-limits-grid")).not.toBeNull();

    sessions.filters.agent = "claude";
    await tick();
    await tick();
    await tick();
    expect(document.querySelector(".rate-limits-grid")).toBeNull();

    sessions.filters.agent = "";
    await tick();
    await tick();
    await tick();
    expect(document.querySelector(".rate-limits-grid")).not.toBeNull();
  });

  it("converts the effective date range (page range, or a narrower brushed range) to UTC bounds with an exclusive next-day upper bound", async () => {
    rateLimitsServiceMocks.getApiV1RateLimitsCurrent.mockResolvedValue([snapshot()]);
    rateLimitsServiceMocks.getApiV1RateLimitsHistory.mockResolvedValue([]);
    await mountSection();

    let [params] = rateLimitsServiceMocks.getApiV1RateLimitsHistory.mock.calls.at(-1) as [
      { since: string; until: string },
    ];
    expect(new Date(params.since).getTime()).toBe(new Date(2026, 8, 1).getTime());
    expect(new Date(params.until).getTime()).toBe(new Date(2026, 8, 10).getTime());

    usage.selectedTimeRange = { from: "2026-09-05", to: "2026-09-05" };
    await tick();
    await tick();
    await tick();
    [params] = rateLimitsServiceMocks.getApiV1RateLimitsHistory.mock.calls.at(-1) as [
      { since: string; until: string },
    ];
    expect(new Date(params.since).getTime()).toBe(new Date(2026, 8, 5).getTime());
    expect(new Date(params.until).getTime()).toBe(new Date(2026, 8, 6).getTime());
  });

  it("refreshes a card's history after a new fetchCurrent (Usage page refresh)", async () => {
    rateLimitsServiceMocks.getApiV1RateLimitsCurrent.mockResolvedValue([snapshot()]);
    rateLimitsServiceMocks.getApiV1RateLimitsHistory.mockResolvedValue([
      snapshot({ observedAt: "2026-09-08T10:00:00Z" }),
      snapshot({ observedAt: "2026-09-09T10:00:00Z" }),
    ]);
    await mountSection();

    const historyCallsAfterMount = rateLimitsServiceMocks.getApiV1RateLimitsHistory.mock.calls.length;
    expect(historyCallsAfterMount).toBeGreaterThan(0);

    // Manual clicks and kit-ui's periodic auto-refresh both call
    // rateLimits.fetchCurrent() directly (see UsagePage.svelte); a fresh
    // fetchCurrent() must also re-fetch each visible card's history.
    rateLimitsServiceMocks.getApiV1RateLimitsCurrent.mockResolvedValue([snapshot({ usedPercent: 77 })]);
    await rateLimits.fetchCurrent();
    await tick();
    await tick();

    expect(rateLimitsServiceMocks.getApiV1RateLimitsHistory.mock.calls.length).toBeGreaterThan(
      historyCallsAfterMount,
    );
    expect(document.querySelector(".used-percent")?.textContent).toContain("77");
  });

  it("groups cards by vendor then account, and keeps Claude visible when only Codex is excluded", async () => {
    const claudeSnapshot: ServiceRateLimitWindow = {
      vendor: "claude",
      accountId: "acct-1",
      accountLabel: "Acme Org",
      windowKind: "five_hour",
      usedPercent: 42,
      resetsAt: Math.floor(Date.now() / 1000) + 3600,
      observedAt: "2026-09-09T10:00:00Z",
    };
    rateLimitsServiceMocks.getApiV1RateLimitsCurrent.mockResolvedValue([
      snapshot(),
      claudeSnapshot,
    ]);
    rateLimitsServiceMocks.getApiV1RateLimitsHistory.mockResolvedValue([]);
    usage.excludedAgents = "codex";

    component = mount(RateLimitsSection, {
      target: document.body,
      props: { from: "2026-09-01", to: "2026-09-09" },
    });
    await tick();
    await tick();
    await tick();

    const vendorTitles = [...document.querySelectorAll(".vendor-title")].map(
      (el) => el.textContent,
    );
    expect(vendorTitles).toEqual(["Claude"]);
    expect(document.querySelector(".account-title")?.textContent).toBe("Acme Org");
    expect(document.body.textContent).toContain("42");
  });

  // Covers the roborev Medium finding on kata 9rs0 (filed as kata qs2f):
  // Codex snapshots carry no account identity, so every Codex machine
  // group previously rendered under the same "codex " keyed-loop key
  // (accountGroupKey used only vendor + accountId, and accountId is
  // always "" for Codex). Two distinct Codex machines must render as two
  // distinct, independently keyed account groups.
  it("renders a separate account group per Codex machine", async () => {
    rateLimitsServiceMocks.getApiV1RateLimitsCurrent.mockResolvedValue([
      snapshot({ machine: "laptop", usedPercent: 30 }),
      snapshot({ machine: "desktop", usedPercent: 70 }),
    ]);
    rateLimitsServiceMocks.getApiV1RateLimitsHistory.mockResolvedValue([]);

    component = mount(RateLimitsSection, {
      target: document.body,
      props: { from: "2026-09-01", to: "2026-09-09" },
    });
    await tick();
    await tick();
    await tick();

    const accountTitles = [...document.querySelectorAll(".account-title")].map(
      (el) => el.textContent,
    );
    expect(accountTitles.sort()).toEqual(["desktop", "laptop"]);

    const grids = document.querySelectorAll(".rate-limits-grid");
    expect(grids).toHaveLength(2);
    expect(document.body.textContent).toContain("30");
    expect(document.body.textContent).toContain("70");
  });

  // Covers kata eabb's UI requirement (scope badge) and kata k5bm's
  // "final word" card format: a scoped limits-array entry (e.g.
  // "weekly_scoped:Fable") shows its window label ("Weekly limit") with
  // its scope as a qualifier badge, "Fable" -- and, per kata k5bm (f),
  // severity is never rendered even though the snapshot still carries
  // one (the field stays in the API for future use).
  it("shows the scope badge for a scoped Claude window, and never renders severity", async () => {
    const scopedSnapshot: ServiceRateLimitWindow = {
      vendor: "claude",
      accountId: "acct-1",
      accountLabel: "Acme Org",
      windowKind: "weekly_scoped:Fable",
      usedPercent: 5,
      scopeLabel: "Fable",
      severity: "normal",
      windowMinutes: 10080,
      observedAt: "2026-09-10T00:00:00Z",
    };
    rateLimitsServiceMocks.getApiV1RateLimitsCurrent.mockResolvedValue([scopedSnapshot]);
    rateLimitsServiceMocks.getApiV1RateLimitsHistory.mockResolvedValue([]);

    component = mount(RateLimitsSection, {
      target: document.body,
      props: { from: "2026-09-01", to: "2026-09-09" },
    });
    await tick();
    await tick();
    await tick();

    expect(document.querySelector(".window-label")?.textContent).toBe("Weekly limit");
    expect(document.querySelector(".scope-badge")?.textContent).toBe("Fable");
    expect(document.body.textContent).not.toContain("normal");
  });

  // Covers kata k5bm's "final word" card format example directly: a
  // Codex model-specific limit bucket (limit_id "codex_bengalfox",
  // limit_name "GPT-5.3-Codex-Spark") renders through the same
  // "<window label> [<scope badge>] <duration>" title Claude cards use
  // -- windowMinutes selects the label (300 -> "Session limit"), and
  // limitName becomes the badge, never the raw limit_id or the raw
  // "primary"/"secondary" slot name.
  it("titles a Codex model-specific bucket like a scoped Claude window", async () => {
    const sparkSnapshot: ServiceRateLimitWindow = {
      vendor: "codex",
      machine: "laptop",
      limitId: "codex_bengalfox",
      limitName: "GPT-5.3-Codex-Spark",
      planType: "pro",
      windowKind: "primary",
      usedPercent: 12,
      windowMinutes: 300,
      observedAt: "2026-09-10T00:00:00Z",
    };
    rateLimitsServiceMocks.getApiV1RateLimitsCurrent.mockResolvedValue([sparkSnapshot]);
    rateLimitsServiceMocks.getApiV1RateLimitsHistory.mockResolvedValue([]);

    component = mount(RateLimitsSection, {
      target: document.body,
      props: { from: "2026-09-01", to: "2026-09-09" },
    });
    await tick();
    await tick();
    await tick();

    expect(document.querySelector(".window-label")?.textContent).toBe("Session limit");
    expect(document.querySelector(".scope-badge")?.textContent).toBe("GPT-5.3-Codex-Spark");
    expect(document.body.textContent).not.toContain("codex_bengalfox");
    expect(document.body.textContent).not.toContain("PRIMARY");
    expect(document.body.textContent).not.toContain("primary");
  });

  // Covers a roborev finding: a Codex model-specific bucket that has not
  // been given a display name yet (limit_name absent) used to render
  // with no badge at all, making it indistinguishable from every other
  // window sharing the same "Session limit"/"Weekly limit" heading. The
  // badge now falls back to the raw limit_id in that case.
  it("falls back to the raw limit_id as the badge for an unnamed Codex model-specific bucket", async () => {
    const unnamedSnapshot: ServiceRateLimitWindow = {
      vendor: "codex",
      machine: "laptop",
      limitId: "codex_bengalfox",
      planType: "pro",
      windowKind: "primary",
      usedPercent: 12,
      windowMinutes: 300,
      observedAt: "2026-09-10T00:00:00Z",
    };
    rateLimitsServiceMocks.getApiV1RateLimitsCurrent.mockResolvedValue([unnamedSnapshot]);
    rateLimitsServiceMocks.getApiV1RateLimitsHistory.mockResolvedValue([]);

    component = mount(RateLimitsSection, {
      target: document.body,
      props: { from: "2026-09-01", to: "2026-09-09" },
    });
    await tick();
    await tick();
    await tick();

    expect(document.querySelector(".window-label")?.textContent).toBe("Session limit");
    expect(document.querySelector(".scope-badge")?.textContent).toBe("codex_bengalfox");
  });

  // Covers the roborev finding on kata ztf4: a Claude session/weekly
  // window with no windowMinutes (a statusline or fixed-bucket
  // observation predating the ingestion fix that now populates it, or a
  // not-yet-migrated row) must still title correctly from its
  // window_kind rather than falling back to a duration-less generic
  // "— limit" label -- and the missing duration must simply omit the
  // trailing duration badge, not corrupt the title.
  it.each([
    ["session", "Session limit"],
    ["weekly", "Weekly limit"],
  ] as const)(
    "titles a Claude %s window correctly even without windowMinutes",
    async (windowKind, wantLabel) => {
      const snapshotNoMinutes: ServiceRateLimitWindow = {
        vendor: "claude",
        accountId: "acct-1",
        accountLabel: "Acme Org",
        windowKind,
        usedPercent: 22,
        observedAt: "2026-09-10T00:00:00Z",
      };
      rateLimitsServiceMocks.getApiV1RateLimitsCurrent.mockResolvedValue([snapshotNoMinutes]);
      rateLimitsServiceMocks.getApiV1RateLimitsHistory.mockResolvedValue([]);

      component = mount(RateLimitsSection, {
        target: document.body,
        props: { from: "2026-09-01", to: "2026-09-09" },
      });
      await tick();
      await tick();
      await tick();

      expect(document.querySelector(".window-label")?.textContent).toBe(wantLabel);
      expect(document.body.textContent).not.toContain("— limit");
      expect(document.querySelector(".window-length")).toBeNull();
    },
  );

  // Covers kata eabb's UI requirement: the extra_usage_monthly window
  // shows utilization against its monetary limit ("96% of $1,100"),
  // sourced from the details JSON's monthly_limit_minor/exponent.
  it("shows the extra-usage card's utilization against its monetary limit", async () => {
    const extraUsageSnapshot: ServiceRateLimitWindow = {
      vendor: "claude",
      accountId: "acct-1",
      accountLabel: "Acme Org",
      windowKind: "extra_usage_monthly",
      usedPercent: 96.14,
      details: { source: "extra_usage", monthly_limit_minor: 110000, currency: "USD", exponent: 2 },
      observedAt: "2026-09-10T00:00:00Z",
    };
    rateLimitsServiceMocks.getApiV1RateLimitsCurrent.mockResolvedValue([extraUsageSnapshot]);
    rateLimitsServiceMocks.getApiV1RateLimitsHistory.mockResolvedValue([]);

    component = mount(RateLimitsSection, {
      target: document.body,
      props: { from: "2026-09-01", to: "2026-09-09" },
    });
    await tick();
    await tick();
    await tick();

    const usedPercentEl = document.querySelector(".used-percent");
    expect(usedPercentEl?.textContent).toContain("96");
    expect(usedPercentEl?.textContent).toContain("1,100");
  });
});
