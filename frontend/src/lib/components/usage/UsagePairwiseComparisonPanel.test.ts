import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { mount, tick, unmount } from "svelte";
import UsagePairwiseComparisonPanel from "./UsagePairwiseComparisonPanel.svelte";
import { usage } from "../../stores/usage.svelte.js";
import type {
  ServiceUsagePairwiseComparisonResponse,
  UsageSummaryResponse,
} from "../../api/generated/index";
import { testMoney } from "../../test/money.js";

function usageSummary(): UsageSummaryResponse {
  return {
    from: "2024-01-01",
    to: "2024-01-31",
    projects: {},
    totals: {
      inputTokens: 400,
      outputTokens: 200,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      totalCost: testMoney(99.99),
      cacheSavings: testMoney(0),
      energyMicroWh: 0, energyStatus: "",
    },
    daily: [],
    projectTotals: [
      {
        project_key: "pl1:sha256:alpha",
        project: "alpha",
        inputTokens: 200,
        outputTokens: 100,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        cost: testMoney(10),
        energyMicroWh: 0, energyStatus: "",
      },
      {
        project_key: "pl1:sha256:beta",
        project: "beta",
        inputTokens: 100,
        outputTokens: 50,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        cost: testMoney(8),
        energyMicroWh: 0, energyStatus: "",
      },
    ],
    modelTotals: [
      {
        model: "claude-sonnet-4-20250514",
        inputTokens: 200,
        outputTokens: 100,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        cost: testMoney(10),
        energyMicroWh: 0, energyStatus: "",
      },
      {
        model: "gpt-4o",
        inputTokens: 100,
        outputTokens: 50,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        cost: testMoney(8),
        energyMicroWh: 0, energyStatus: "",
      },
    ],
    agentTotals: [],
    sessionCounts: {
      total: 2,
      byProject: { alpha: 1, beta: 1 },
      byAgent: {},
    },
    cacheStats: {
      cacheReadTokens: 0,
      cacheCreationTokens: 0,
      uncachedInputTokens: 400,
      outputTokens: 200,
      hitRate: 0,
      savingsVsUncached: testMoney(0),
    },
  };
}

function pairwiseComparison(): ServiceUsagePairwiseComparisonResponse {
  return {
    left: {
      totalCost: testMoney(4),
      inputTokens: 200,
      outputTokens: 100,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      totalTokens: 300,
      sessionCount: 2,
      costPerSession: testMoney(2),
      tokensPerSession: 150,
      energyMicroWh: 0, energyStatus: "",
    },
    right: {
      totalCost: testMoney(5.5),
      inputTokens: 100,
      outputTokens: 50,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      totalTokens: 150,
      sessionCount: 1,
      costPerSession: testMoney(5.5),
      tokensPerSession: 150,
      energyMicroWh: 0, energyStatus: "",
    },
    deltas: {
      totalCostDelta: testMoney(1.5),
      totalCostDeltaRatio: 0.375,
      inputTokensDelta: -100,
      inputTokensDeltaRatio: -0.5,
      outputTokensDelta: -50,
      outputTokensDeltaRatio: -0.5,
      cacheCreationDelta: 0,
      cacheCreationDeltaRatio: null,
      cacheReadDelta: 0,
      cacheReadDeltaRatio: null,
      totalTokensDelta: -150,
      totalTokensDeltaRatio: -0.5,
      sessionCountDelta: -1,
      sessionCountDeltaRatio: -0.5,
      costPerSessionDelta: testMoney(3.5),
      costPerSessionRatio: 1.75,
      tokensPerSessionDelta: 0,
      tokensPerSessionRatio: 0,
      energyMicroWhDelta: 0, energyMicroWhDeltaRatio: null,
    },
  };
}

describe("UsagePairwiseComparisonPanel", () => {
  beforeEach(() => {
    usage.summary = usageSummary();
    usage.pairwiseSelection = {
      left: { dimension: "model", value: "claude-sonnet-4-20250514" },
      right: { dimension: "project", value: "pl1:sha256:beta" },
    };
    usage.pairwiseComparison = pairwiseComparison();
    usage.loading.pairwise = false;
    usage.errors.pairwise = null;
  });

  afterEach(() => {
    usage.summary = null;
    usage.pairwiseComparison = null;
    usage.pairwiseSelection = {
      left: { dimension: "model", value: "" },
      right: { dimension: "model", value: "" },
    };
    usage.loading.pairwise = false;
    usage.errors.pairwise = null;
    usage.mode = "cost";
    usage.setSelectedTokenTypes(["input", "cache_write", "cache_read", "output"]);
    document.body.innerHTML = "";
    vi.restoreAllMocks();
  });

  it("routes selector changes through the usage store", async () => {
    const spy = vi.spyOn(usage, "setPairwiseSide");
    const component = mount(UsagePairwiseComparisonPanel, {
      target: document.body,
    });
    await tick();

    const trigger = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Left comparison dimension"]',
    );
    expect(trigger).toBeTruthy();
    if (!trigger) return;
    trigger.click();
    await tick();

    const option = Array.from(document.querySelectorAll<HTMLLIElement>('li[role="option"]')).find(
      (item) => item.textContent?.includes("Project"),
    );
    expect(option).toBeTruthy();
    option?.dispatchEvent(new MouseEvent("mousedown", { bubbles: true }));
    await tick();

    expect(spy).toHaveBeenCalledWith("left", { dimension: "project" });
    unmount(component);
  });

  it("renders backend-provided delta fields instead of deriving them from summary totals", async () => {
    const component = mount(UsagePairwiseComparisonPanel, {
      target: document.body,
    });
    await tick();

    const text = document.body.textContent ?? "";
    expect(text).toContain("+$1.50");
    expect(text).toContain("+37.5%");
    expect(text).not.toContain("+$94.49");

    unmount(component);
  });

  it("renders null backend ratios as the empty-state marker", async () => {
    usage.pairwiseComparison = {
      ...pairwiseComparison(),
      deltas: {
        ...pairwiseComparison().deltas,
        costPerSessionDelta: null,
        costPerSessionRatio: null,
        tokensPerSessionDelta: null,
        tokensPerSessionRatio: null,
      },
    };
    const component = mount(UsagePairwiseComparisonPanel, {
      target: document.body,
    });
    await tick();

    const text = document.body.textContent ?? "";
    expect(text).toContain("None");
    expect(text).not.toContain("+$0.00");
    expect(text).not.toContain("+0.0%");

    unmount(component);
  });

  it("compares aggregate output tokens when Output is selected", async () => {
    usage.mode = "token";
    usage.setSelectedTokenTypes(["output"]);
    const component = mount(UsagePairwiseComparisonPanel, {
      target: document.body,
    });
    await tick();

    const firstRow = document.querySelector("tbody tr");
    const cells = Array.from(firstRow?.querySelectorAll("th, td") ?? []).map((cell) =>
      cell.textContent?.replace(/\s+/g, " ").trim(),
    );
    expect(cells).toEqual(["Total Tokens", "100", "50", "-50 -50.0%"]);

    unmount(component);
  });

  it.each([
    [
      "both sides priced",
      { energyMicroWh: 40_000_000 },
      { energyMicroWh: 25_000_000 },
      { energyMicroWhDelta: -15_000_000, energyMicroWhDeltaRatio: -0.375 },
      ["Total Energy", "40 Wh", "25 Wh", "-15 Wh -37.5%"],
      false,
    ],
    [
      // The misleading -100% is withheld, not shown, for an unpriced side.
      "right side unpriced",
      { energyMicroWh: 40_000_000, energyStatus: "ok" },
      { energyMicroWh: 0, energyStatus: "no_rate" },
      { energyMicroWhDelta: -40_000_000, energyMicroWhDeltaRatio: -1 },
      ["Total Energy", "40 Wh", "0 Wh*", "-40 Wh None"],
      true,
    ],
  ] as const)(
    "compares estimated energy in energy mode (%s)",
    async (_label, left, right, deltas, wantCells, wantRightTitle) => {
      usage.mode = "energy";
      const base = pairwiseComparison();
      usage.pairwiseComparison = {
        ...base,
        left: { ...base.left, ...left },
        right: { ...base.right, ...right },
        deltas: { ...base.deltas, ...deltas },
      };
      const component = mount(UsagePairwiseComparisonPanel, { target: document.body });
      await tick();

      expect(document.querySelector("h2")?.textContent).toContain("Comparative Energy Analysis");
      const cells = Array.from(document.querySelectorAll("tbody tr")[0]?.querySelectorAll("th, td") ?? [])
        .map((cell) => cell.textContent?.replace(/\s+/g, " ").trim());
      expect(cells).toEqual(wantCells);
      const rightCell = document.querySelectorAll("tbody tr td")[1];
      expect(!!rightCell?.getAttribute("title")).toBe(wantRightTitle);
      unmount(component);
    },
  );
});
