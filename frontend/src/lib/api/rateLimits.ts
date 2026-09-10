import { RateLimitsService } from "./generated/index.js";
import type { CurrentRateLimitApiRow } from "../utils/rateLimitAlerts.js";

/**
 * Fetches every currently observed rate-limit window (across all
 * machines/accounts) for the alert runner's threshold check, via the
 * generated `RateLimitsService.getApiV1RateLimitsCurrent()` client
 * (see codex-rate-limits' internal/server/huma_routes_ratelimits.go
 * and the generated ServiceRateLimitWindow type).
 *
 * Deliberately does NOT go through frontend/src/lib/stores/ratelimits.svelte.ts:
 * that store's `fetchCurrent()` scopes its query to the Usage page's
 * currently selected machine filter (`sessions.filters.machine`),
 * which is right for that page but would incorrectly narrow a
 * background alert check to whatever the user happens to have
 * filtered there. This calls the same generated client directly, with
 * no filter, so every source is considered regardless of what's
 * showing on screen.
 *
 * Resolves to an empty array — never throws — so a transient network
 * error, or an older server without this route, can't break the app.
 */
export async function fetchCurrentRateLimits(): Promise<CurrentRateLimitApiRow[]> {
  try {
    return await RateLimitsService.getApiV1RateLimitsCurrent();
  } catch {
    return [];
  }
}
