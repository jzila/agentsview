// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import EnergyEstimateSettings from "./EnergyEstimateSettings.svelte";
import { energyEstimate } from "../../stores/energyEstimate.svelte.js";
import { ConfigService } from "../../api/generated/index";

vi.mock("../../api/generated/index", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../api/generated/index")>()),
  ConfigService: { getApiV1ConfigEnergy: vi.fn(), postApiV1ConfigEnergy: vi.fn() },
}));

const configService = ConfigService as unknown as { getApiV1ConfigEnergy: ReturnType<typeof vi.fn>; postApiV1ConfigEnergy: ReturnType<typeof vi.fn> };

function body(scenario: "low" | "mid" | "high") {
  return {
    scenario,
    models: [{ model: "claude-sonnet-class", list_price_out_microdollars_per_mtok: 15_000_000, rates: [{ token_type: "output", wh_per_mtok_micro_wh: 1_052_000_000 }] }],
  };
}

let component: ReturnType<typeof mount> | undefined;

beforeEach(() => {
  vi.clearAllMocks();
  Object.assign(energyEstimate, { scenario: "mid", models: [], loaded: false, loading: false, error: null, saveError: null });
  configService.getApiV1ConfigEnergy.mockResolvedValue(body("mid"));
});

afterEach(() => {
  if (component) unmount(component);
  component = undefined;
  document.body.innerHTML = "";
});

describe("EnergyEstimateSettings", () => {
  it("loads and renders the effective Wh/MTok table for the current scenario", async () => {
    component = mount(EnergyEstimateSettings, { target: document.body });
    await tick(); await Promise.resolve(); await Promise.resolve(); await tick();
    const row = document.querySelector(".energy-rates-table tbody tr");
    expect(row?.textContent).toContain("claude-sonnet-class");
    expect(row?.textContent).toContain("1.1 kWh");
  });

  it("saves a new scenario when selected", async () => {
    configService.postApiV1ConfigEnergy.mockResolvedValue(body("high"));
    component = mount(EnergyEstimateSettings, { target: document.body });
    await tick(); await Promise.resolve(); await tick();
    document.querySelector<HTMLButtonElement>('button[aria-label="Scenario"]')?.click();
    await tick();
    Array.from(document.querySelectorAll<HTMLLIElement>('li[role="option"]'))
      .find((item) => item.textContent?.includes("High"))
      ?.dispatchEvent(new MouseEvent("mousedown", { bubbles: true }));
    await tick(); await Promise.resolve(); await tick();
    expect(configService.postApiV1ConfigEnergy).toHaveBeenCalledWith({ scenario: "high" });
  });
});
