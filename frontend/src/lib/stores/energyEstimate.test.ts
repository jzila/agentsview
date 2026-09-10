import { beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { energyEstimate } from "./energyEstimate.svelte.js";
import { ConfigService } from "../api/generated/index";

vi.mock("../api/generated/index", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../api/generated/index")>()),
  ConfigService: { getApiV1ConfigEnergy: vi.fn(), postApiV1ConfigEnergy: vi.fn() },
}));

const configService = ConfigService as unknown as {
  getApiV1ConfigEnergy: ReturnType<typeof vi.fn>;
  postApiV1ConfigEnergy: ReturnType<typeof vi.fn>;
};

function body(scenario: "low" | "mid" | "high") {
  return {
    scenario,
    models: [{ model: "claude-sonnet-class", list_price_out_microdollars_per_mtok: 15_000_000, rates: [{ token_type: "input", wh_per_mtok_micro_wh: 100 }] }],
  };
}

beforeEach(() => {
  vi.clearAllMocks();
  Object.assign(energyEstimate, { scenario: "mid", models: [], loaded: false, loading: false, saving: false, error: null, saveError: null });
});

describe("energyEstimate store", () => {
  it("load populates state and ensureLoaded caches without refetching", async () => {
    configService.getApiV1ConfigEnergy.mockResolvedValue(body("mid"));
    await energyEstimate.load();
    expect(energyEstimate.scenario).toBe("mid");
    expect(energyEstimate.loaded).toBe(true);
    await energyEstimate.ensureLoaded();
    expect(configService.getApiV1ConfigEnergy).toHaveBeenCalledOnce();
  });

  it("a load error leaves loaded false so a retry can succeed", async () => {
    configService.getApiV1ConfigEnergy.mockRejectedValue(new Error("network down"));
    await energyEstimate.load();
    expect(energyEstimate.error).toBe("network down");
    expect(energyEstimate.loaded).toBe(false);
    configService.getApiV1ConfigEnergy.mockResolvedValue(body("mid"));
    await energyEstimate.ensureLoaded();
    expect(energyEstimate.loaded).toBe(true);
  });

  it("setScenario persists on success and keeps the prior scenario on failure", async () => {
    configService.postApiV1ConfigEnergy.mockResolvedValue(body("high"));
    expect(await energyEstimate.setScenario("high")).toBe(true);
    expect(energyEstimate.scenario).toBe("high");
    configService.postApiV1ConfigEnergy.mockRejectedValue(new Error("read-only"));
    expect(await energyEstimate.setScenario("low")).toBe(false);
    expect(energyEstimate.saveError).toBe("read-only");
    expect(energyEstimate.scenario).toBe("high");
  });

  // A stale load response must never roll back a newer save already applied.
  it("ignores a stale load response that resolves after a newer save already applied", async () => {
    let resolveLoad!: (value: ReturnType<typeof body>) => void;
    configService.getApiV1ConfigEnergy.mockReturnValue(new Promise((r) => (resolveLoad = r)));
    const loadPromise = energyEstimate.load();
    configService.postApiV1ConfigEnergy.mockResolvedValue(body("high"));
    await energyEstimate.setScenario("high");
    resolveLoad(body("mid"));
    await loadPromise;
    expect(energyEstimate.scenario).toBe("high");
  });
});
