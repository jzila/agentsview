import type { UsageMode } from "../../stores/usage.svelte.js";

export function usageModeFromParams(params: Record<string, string>): UsageMode {
  if (params.view === "tokens") return "token";
  if (params.view === "energy") return "energy";
  return "cost";
}

export function withUsageMode(
  params: Record<string, string>,
  mode: UsageMode,
): Record<string, string> {
  const next = { ...params };
  if (mode === "token") {
    next.view = "tokens";
  } else if (mode === "energy") {
    next.view = "energy";
  } else {
    delete next.view;
  }
  return next;
}
