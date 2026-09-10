import { describe, expect, it } from "vite-plus/test";
import { setLocale } from "./i18n/index.js";
import { energyEquivalenceOfTV, formatEnergy } from "./energy.js";

describe("formatEnergy", () => {
  it.each([
    [0, "0 Wh"],
    [500, "0.5 mWh"],
    [999_000, "1,000 mWh"],
    [1_000_000, "1 Wh"],
    [400_000_000, "400 Wh"],
    [1_000_000_000, "1 kWh"],
    [1_000_000_000_000, "1 MWh"],
  ] as const)("formats %i micro-Wh as %s at the unit boundary", (microWh, want) => {
    setLocale("en");
    expect(formatEnergy(microWh)).toBe(want);
  });
});

// Each row is a hand-computable Wh/130W reference point, rounded to 2 sig figs.
describe("energyEquivalenceOfTV", () => {
  it.each([
    [5_000_000, "minutes", 2.3],
    [130_000_000, "minutes", 60],
    [260_000_000, "hours", 2],
    [6_240_000_000, "days", 2],
    [13_000_000_000, "days", 4.2],
  ] as const)("scales %i micro-Wh to %s (%s)", (microWh, unit, value) => {
    expect(energyEquivalenceOfTV(microWh)).toEqual({ unit, value });
  });
});
