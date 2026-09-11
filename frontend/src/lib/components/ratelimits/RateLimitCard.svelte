<script lang="ts">
  import { onDestroy, onMount } from "svelte";
  import { formatDateTime, m } from "../../i18n/index.js";
  import {
    rateLimits,
    type RateLimitWindow,
  } from "../../stores/ratelimits.svelte.js";
  import {
    formatCreditsBalance,
    formatResetCountdown,
    formatWindowLength,
  } from "../../utils/rateLimitFormat.js";
  import { formatMoney, moneyFromMicrodollars, type Money } from "../../money.js";
  import RateLimitHistoryChart from "./RateLimitHistoryChart.svelte";

  interface Props {
    window: RateLimitWindow;
    since: string;
    until: string;
  }

  let { window: snapshot, since, until }: Props = $props();

  const accountId = $derived(snapshot.accountId ?? "");
  const limitId = $derived(snapshot.limitId ?? "");
  const isClaude = $derived(snapshot.vendor === "claude");

  /** Derives the vendor-neutral window-label part of a card's title
   * ("Session limit", "Weekly limit", ...) so both vendors render
   * through the same message keys instead of Codex showing its raw
   * "primary"/"secondary" slot names or Claude showing its raw
   * window_kind (kata k5bm, resolving kata j5md #1's window-kind
   * normalization finding too).
   *
   * A handful of Claude-only window kinds (the fixed-bucket fallback's
   * seven_day_opus/seven_day_sonnet/seven_day_oauth_apps/seven_day_
   * overage_included, and extra_usage_monthly, a $-denominated window
   * with no minutes at all) keep their own dedicated labels; every other
   * window -- both vendors' -- is labeled from windowMinutes: 300 is the
   * "Session limit" key Claude Code itself uses for its 5-hour window,
   * 10080 is the "Weekly limit" key, and any other length falls back to
   * a generic "{duration} limit" key. windowKind's ":<scope label>"
   * suffix (e.g. "weekly_scoped:Fable") never affects the label -- the
   * scope becomes the badge shown alongside it instead, mirroring how
   * Codex's limitName becomes the badge for a model-specific bucket.
   */
  function windowLabelFor(windowKind: string, windowMinutes: number | undefined): string {
    const baseKind = windowKind.split(":")[0];
    switch (baseKind) {
      case "seven_day_opus":
        return m.rate_limits_claude_window_seven_day_opus();
      case "seven_day_sonnet":
        return m.rate_limits_claude_window_seven_day_sonnet();
      case "seven_day_oauth_apps":
        return m.rate_limits_claude_window_seven_day_oauth_apps();
      case "seven_day_overage_included":
        return m.rate_limits_claude_window_seven_day_overage_included();
      case "extra_usage_monthly":
        return m.rate_limits_extra_usage_monthly_label();
      // Claude's own shared window_kind identity is authoritative even
      // when windowMinutes is missing or still zero (a statusline or
      // fixed-bucket observation predating the ingestion fix for this,
      // or a not-yet-migrated row): falling through to the
      // windowMinutes-based mapping below would otherwise show a
      // duration-less generic "— limit" title, and that title would
      // flip depending on which source most recently wrote the row
      // (roborev finding on kata ztf4). Codex's own kinds
      // ("primary"/"secondary") have no case here and fall through to
      // windowMinutes as before.
      case "session":
        return m.rate_limits_claude_window_five_hour();
      case "weekly":
        return m.rate_limits_claude_window_seven_day();
    }
    if (windowMinutes === 300) return m.rate_limits_claude_window_five_hour();
    if (windowMinutes === 10080) return m.rate_limits_claude_window_seven_day();
    // Codex's own two kinds ("primary"/"secondary") are slot names, not
    // fixed durations -- window_minutes is what actually determines
    // session vs. weekly, and either slot can carry either length (a
    // fixture elsewhere in this codebase pairs "primary" with a
    // 10080-minute window on purpose). An observation from before
    // window_minutes was always populated carries no duration at all,
    // so a missing value here falls through to the generic
    // "{duration} limit" title below rather than guessing a specific
    // one from the slot name (roborev finding): formatWindowLength
    // renders an unknown duration as "—", giving a neutral "— limit"
    // rather than a possibly-wrong "Session limit"/"Weekly limit".
    return m.rate_limits_window_generic_label({ duration: formatWindowLength(windowMinutes ?? 0) });
  }

  const windowLabel = $derived(windowLabelFor(snapshot.windowKind, snapshot.windowMinutes));

  /** The qualifier badge next to the window label -- Claude's model/
   * surface scope (e.g. "Fable" for a weekly_scoped:Fable window) or
   * Codex's limit_name (e.g. "GPT-5.3-Codex-Spark" for the
   * codex_bengalfox bucket). Empty for an account-wide Claude window or
   * Codex's general "codex" bucket, both of which render with no badge
   * at all. Falls back to Codex's raw limit_id (e.g. "codex_bengalfox")
   * for a model-specific bucket that has not been given a display name
   * yet, so distinct buckets stay visually distinguishable instead of
   * showing identical "Session limit"/"Weekly limit" headings with no
   * badge at all (roborev finding); the general "codex" id itself is
   * still suppressed rather than shown as a badge. This is vendor DATA,
   * passed through untranslated. */
  const scopeBadge = $derived(
    isClaude
      ? (snapshot.scopeLabel ?? "")
      : snapshot.limitName || (limitId && limitId !== "codex" ? limitId : ""),
  );

  const isExtraUsage = $derived(snapshot.windowKind === "extra_usage_monthly");

  /** Extracts the extra_usage_monthly window's monetary limit from its
   * `details` JSON (monthly_limit_minor + exponent, currency), for the
   * "96% of $1,100" summary. Returns null when details is absent or
   * missing the fields this window is expected to carry. */
  function extraUsageLimitMoney(details: unknown): Money | null {
    if (!details || typeof details !== "object") return null;
    const raw = details as Record<string, unknown>;
    const minor = raw.monthly_limit_minor;
    const exponent = raw.exponent;
    if (typeof minor !== "number" || typeof exponent !== "number") return null;
    const dollars = minor / 10 ** exponent;
    return moneyFromMicrodollars(Math.round(dollars * 1_000_000));
  }

  const extraUsageLimit = $derived(isExtraUsage ? extraUsageLimitMoney(snapshot.details) : null);

  let now = $state(Date.now());
  let timer: ReturnType<typeof setInterval> | undefined;

  onMount(() => {
    timer = setInterval(() => {
      now = Date.now();
    }, 60_000);
  });
  onDestroy(() => {
    if (timer !== undefined) clearInterval(timer);
  });

  const identity = $derived({
    vendor: snapshot.vendor,
    accountId,
    machine: snapshot.machine ?? "",
    limitId,
    windowKind: snapshot.windowKind,
  });

  $effect(() => {
    // Re-fetch this card's history when its full identity (vendor,
    // account, machine, limit id, window kind) or the selected date
    // range changes, and also whenever current snapshots refresh
    // (mount, the Usage page's manual refresh, or its periodic
    // auto-refresh) so the chart picks up newly synced observations
    // instead of only the card's own used-percent/credits fields
    // updating.
    const id = identity;
    void rateLimits.refreshToken;
    void since;
    void until;
    rateLimits.fetchHistory(id, since, until);
  });

  const usedPercent = $derived(Math.max(0, Math.min(100, snapshot.usedPercent)));

  const barColor = $derived(
    usedPercent >= 90
      ? "var(--accent-red)"
      : usedPercent >= 70
        ? "var(--accent-amber)"
        : "var(--accent-blue)",
  );

  // resetsAt is omitted from the API response (rather than sent as 0)
  // when the vendor did not report a reset time for this window, so a
  // missing value is genuinely unknown; any finite number, including 0,
  // is a real reset instant (a past one renders as "Resets now").
  const hasKnownReset = $derived(
    typeof snapshot.resetsAt === "number" && Number.isFinite(snapshot.resetsAt),
  );
  const resetCountdown = $derived(
    hasKnownReset ? formatResetCountdown(snapshot.resetsAt!, now) : null,
  );
  const resetAbsolute = $derived(
    hasKnownReset
      ? formatDateTime(snapshot.resetsAt! * 1000, {
          dateStyle: "medium",
          timeStyle: "short",
        })
      : "",
  );

  const history = $derived(rateLimits.historyFor(identity));
</script>

<div class="rate-limit-card">
  <div class="card-header">
    <span class="window-label">{windowLabel}</span>
    {#if scopeBadge}
      <span class="scope-badge">{scopeBadge}</span>
    {/if}
    {#if snapshot.windowMinutes}
      <span class="window-length">{formatWindowLength(snapshot.windowMinutes)}</span>
    {/if}
  </div>

  <div
    class="progress-track"
    role="progressbar"
    aria-label={m.rate_limits_used_percent_aria()}
    aria-valuemin={0}
    aria-valuemax={100}
    aria-valuenow={Math.round(usedPercent)}
  >
    <div
      class="progress-fill"
      style:width={`${usedPercent}%`}
      style:background={barColor}
    ></div>
  </div>
  <div class="progress-stats">
    {#if isExtraUsage && extraUsageLimit}
      <span class="used-percent" style:color={barColor}>
        {m.rate_limits_extra_usage_summary({
          percent: usedPercent.toLocaleString(undefined, { maximumFractionDigits: 1 }),
          limit: formatMoney(extraUsageLimit),
        })}
      </span>
    {:else}
      <span class="used-percent" style:color={barColor}>
        {usedPercent.toLocaleString(undefined, { maximumFractionDigits: 1 })}%
      </span>
    {/if}
    <span class="resets" title={resetAbsolute}>
      {#if !hasKnownReset}
        {m.rate_limits_resets_unknown()}
      {:else if resetCountdown !== null}
        {m.rate_limits_resets_in({ value: resetCountdown })}
      {:else}
        {m.rate_limits_resets_now()}
      {/if}
    </span>
  </div>

  <div class="meta-row">
    {#if snapshot.planType}
      <span class="meta-item">
        <span class="meta-label">{m.rate_limits_plan_label()}</span>
        <span class="meta-value">{snapshot.planType}</span>
      </span>
    {/if}
    {#if snapshot.creditsHas}
      <span class="meta-item">
        <span class="meta-label">{m.rate_limits_credits_label()}</span>
        <span class="meta-value" title={snapshot.creditsBalance}>
          {snapshot.creditsUnlimited
            ? m.rate_limits_credits_unlimited()
            : formatCreditsBalance(snapshot.creditsBalance ?? "")}
        </span>
      </span>
    {/if}
  </div>

  {#if history.length > 1}
    <div class="history">
      <RateLimitHistoryChart snapshots={history} color={barColor} />
    </div>
  {/if}
</div>

<style>
  .rate-limit-card {
    display: flex;
    flex-direction: column;
    gap: 8px;
    padding: 12px;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md, 8px);
    background: var(--bg-surface);
    min-width: 220px;
  }

  .card-header {
    display: flex;
    align-items: baseline;
    gap: 6px;
    font-size: 12px;
  }

  .window-label {
    font-weight: 600;
    color: var(--text-primary);
  }

  .scope-badge {
    color: var(--text-secondary);
    font-size: 11px;
    padding: 1px 6px;
    border-radius: var(--radius-sm, 4px);
    background: var(--bg-inset);
    border: 1px solid var(--border-muted);
  }

  .window-length {
    color: var(--text-secondary);
    margin-left: auto;
  }

  .progress-track {
    height: 6px;
    border-radius: 3px;
    background: var(--bg-inset);
    border: 1px solid var(--border-muted);
    overflow: hidden;
  }

  .progress-fill {
    height: 100%;
    transition: width 0.4s ease;
  }

  .progress-stats {
    display: flex;
    align-items: center;
    justify-content: space-between;
    font-size: 11px;
    color: var(--text-secondary);
  }

  .used-percent {
    font-weight: 600;
  }

  .meta-row {
    display: flex;
    gap: 16px;
    font-size: 11px;
  }

  .meta-item {
    display: flex;
    flex-direction: column;
    gap: 2px;
  }

  .meta-label {
    color: var(--text-muted);
  }

  .meta-value {
    color: var(--text-primary);
    font-family: var(--font-mono, monospace);
  }

  .history {
    margin-top: 4px;
  }
</style>
