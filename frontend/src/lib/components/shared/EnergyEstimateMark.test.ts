// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import EnergyEstimateMark from "./EnergyEstimateMark.svelte";
import { energyEstimate } from "../../stores/energyEstimate.svelte.js";
import { ConfigService } from "../../api/generated/index";

vi.mock("../../api/generated/index", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../api/generated/index")>()),
  ConfigService: { getApiV1ConfigEnergy: vi.fn(), postApiV1ConfigEnergy: vi.fn() },
}));

const configService = ConfigService as unknown as { getApiV1ConfigEnergy: ReturnType<typeof vi.fn> };

let component: ReturnType<typeof mount> | undefined;

beforeEach(() => {
  vi.clearAllMocks();
  Object.assign(energyEstimate, { scenario: "mid", models: [], loaded: false, loading: false, error: null });
  configService.getApiV1ConfigEnergy.mockResolvedValue({ scenario: "low", models: [] });
});

afterEach(() => {
  if (component) unmount(component);
  component = undefined;
  document.body.innerHTML = "";
});

describe("EnergyEstimateMark", () => {
  it("shows the resolved scenario, an equivalence, and a methodology link on focus", async () => {
    component = mount(EnergyEstimateMark, { target: document.body, props: { microWh: 130_000_000 } });
    await tick(); await Promise.resolve(); await Promise.resolve(); await tick();

    const trigger = document.querySelector<HTMLElement>(".kit-tooltip-trigger");
    trigger?.focus();
    trigger?.dispatchEvent(new FocusEvent("focusin", { bubbles: true }));
    await tick();

    const content = document.querySelector(".energy-estimate-content");
    expect(content?.textContent).toContain("Low");
    expect(content?.textContent).toContain("60"); // 130 Wh at 130 W = 60 minutes.
    const href = document.querySelector<HTMLAnchorElement>(".energy-estimate-link")?.getAttribute("href");
    expect(href).toContain("energy-model.md");
  });
});
