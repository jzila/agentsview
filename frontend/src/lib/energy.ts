import { getLocale, m } from "./i18n/index.js";

// TV_EQUIVALENCE_WATTS anchors the "N minutes/hours/days of TV" comparison
// an energy tile's estimate marker shows alongside the formatted figure,
// the same illustrative comparison Jegham et al. use for LLM energy
// figures. It is a fixed constant, not a live lookup.
const TV_EQUIVALENCE_WATTS = 130;

/**
 * Formats a micro-Wh (1e-6 Wh) energy estimate at two significant figures,
 * scaling the unit so the number stays readable: milliwatt-hours below
 * 1 Wh, watt-hours up to 999, kilowatt-hours above that, and megawatt-hours
 * above 999 kWh. Mirrors internal/energy.FormatMicroWh on the Go side so
 * the CLI and the UI never disagree about which unit a number is in.
 */
export function formatEnergy(microWh: number): string {
  const wh = microWh / 1_000_000;
  const abs = Math.abs(wh);
  const sig = (value: number) =>
    new Intl.NumberFormat(getLocale(), { maximumSignificantDigits: 2 }).format(value);
  if (abs === 0) return "0 Wh";
  if (abs < 1) return `${sig(wh * 1_000)} mWh`;
  if (abs < 1_000) return `${sig(wh)} Wh`;
  if (abs < 1_000_000) return `${sig(wh / 1_000)} kWh`;
  return `${sig(wh / 1_000_000)} MWh`;
}

/** A relatable "minutes/hours/days of TV" equivalence, scaled to whichever
 * unit keeps the number readable (see energyEquivalenceOfTV). `value` is
 * rounded to two significant figures, matching formatEnergy. */
export interface EnergyEquivalence {
  unit: "minutes" | "hours" | "days";
  value: number;
}

function roundToSignificantFigures(value: number, figures: number): number {
  if (value === 0 || !Number.isFinite(value)) return value;
  const magnitude = Math.floor(Math.log10(Math.abs(value)));
  const factor = Math.pow(10, figures - 1 - magnitude);
  return Math.round(value * factor) / factor;
}

/**
 * Converts a micro-Wh estimate into a relatable "N minutes/hours/days of a
 * 130 W television" equivalence. Below 120 minutes reports minutes; below
 * 48 hours reports hours; beyond that reports days. Each value is rounded
 * to two significant figures so a large estimate reads as "34 days" rather
 * than "34324 minutes".
 */
export function energyEquivalenceOfTV(microWh: number): EnergyEquivalence {
  const wh = microWh / 1_000_000;
  const minutes = (wh / TV_EQUIVALENCE_WATTS) * 60;
  if (minutes < 120) {
    return { unit: "minutes", value: roundToSignificantFigures(minutes, 2) };
  }
  const hours = minutes / 60;
  if (hours < 48) {
    return { unit: "hours", value: roundToSignificantFigures(hours, 2) };
  }
  const days = hours / 24;
  return { unit: "days", value: roundToSignificantFigures(days, 2) };
}

/** Formats an EnergyEquivalence's value at up to two significant digits,
 * for display alongside the plural-aware unit text (see
 * energyEquivalenceOfTV and EnergyEstimateMark.svelte). */
export function formatEnergyEquivalenceValue(value: number): string {
  return new Intl.NumberFormat(getLocale(), { maximumSignificantDigits: 2 }).format(value);
}

/**
 * Folds one more constituent's energy_status string into an aggregate
 * (a derived date-range summary combining several days' totals): "no_rate"
 * wins, since it means part of the aggregate's energy is missing; then
 * "override"; the empty string is "no constituent seen yet" and never wins.
 * Mirrors internal/energy.CombineStatus on the Go side, which the backend
 * uses to combine the same statuses server-side.
 */
export function combineEnergyStatus(existing: string, next: string): string {
  if (existing === "") return next;
  if (existing === "no_rate" || next === "no_rate") return "no_rate";
  if (existing === "override" || next === "override") return "override";
  return "ok";
}

/**
 * Formats a micro-Wh value for display, mirroring cmd/agentsview's
 * fmtEnergyStatus exactly so the CLI and every energy-mode UI surface
 * (summary cards, top sessions, attribution, the cost/energy chart, and
 * the session breadcrumb) agree on what a partial or fully unpriced
 * aggregate looks like, instead of each rendering it (or not) its own way:
 *
 *  - no_rate and zero: every constituent is unpriced, so this is not a
 *    real zero -- renders the localized "n/a", not "0 Wh".
 *  - no_rate and nonzero: some constituents are unpriced -- renders the
 *    priced sum with a partial marker ("~") so it cannot look complete.
 *  - override: a config.toml per-model override applied -- renders the
 *    value with an override marker ("*").
 *  - otherwise: the plain formatted value.
 */
export function formatEnergyStatus(microWh: number, status: string): string {
  if (status === "no_rate" && microWh === 0) return m.usage_energy_not_available();
  if (status === "no_rate") return `${formatEnergy(microWh)}~`;
  if (status === "override") return `${formatEnergy(microWh)}*`;
  return formatEnergy(microWh);
}

/** The explanation to show alongside formatEnergyStatus's output, or
 *  undefined for a plain (ok/override) value -- pairs as a title, tooltip,
 *  or sub-label across the same surfaces formatEnergyStatus covers. */
export function energyStatusTitle(microWh: number, status: string): string | undefined {
  if (status === "no_rate" && microWh === 0) return m.usage_energy_not_available_title();
  if (status === "no_rate") return m.usage_energy_partial_title();
  return undefined;
}
