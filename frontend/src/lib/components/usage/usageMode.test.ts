import { describe, expect, it } from "vite-plus/test";
import { usageModeFromParams, withUsageMode } from "./usageMode.js";

describe("usageModeFromParams", () => {
  it.each([
    [{}, "cost"],
    [{ view: "" }, "cost"],
    [{ view: "cost" }, "cost"],
    [{ view: "unknown" }, "cost"],
    [{ view: "tokens" }, "token"],
    [{ view: "energy" }, "energy"],
  ] as const)("maps %o to %s", (params, expected) => {
    expect(usageModeFromParams(params)).toBe(expected);
  });
});

describe("withUsageMode", () => {
  it("adds token mode without dropping filters", () => {
    expect(withUsageMode({ project: "demo", window_days: "30", desktop: "" }, "token")).toEqual({
      project: "demo",
      window_days: "30",
      desktop: "",
      view: "tokens",
    });
  });

  it("adds energy mode without dropping filters", () => {
    expect(withUsageMode({ project: "demo" }, "energy")).toEqual({
      project: "demo",
      view: "energy",
    });
  });

  it("canonicalizes cost mode by removing view", () => {
    expect(withUsageMode({ project: "demo", view: "invalid" }, "cost")).toEqual({
      project: "demo",
    });
  });
});
