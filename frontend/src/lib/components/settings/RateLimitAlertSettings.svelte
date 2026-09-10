<script lang="ts">
  import { Checkbox, Toggle } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import { rateLimitAlertRunner } from "../../stores/rateLimitAlertRunner.svelte.js";
  import { rateLimitAlertSettings } from "../../stores/rateLimitAlertSettings.svelte.js";
  import {
    deriveSources,
    deriveVendors,
    SLIDER_MAX_THRESHOLD_PERCENT,
    SLIDER_MIN_THRESHOLD_PERCENT,
    vendorDisplayName,
  } from "../../utils/rateLimitAlerts.js";

  // The master toggle is the ONLY user gesture that may request browser
  // notification permission (see RateLimitAlertSettingsStore.enable()).
  // `toggling` gates the control while that (possibly async, permission
  // dialog-showing) request resolves, and `pendingEnabled` reflects the
  // in-flight state so the switch doesn't visibly snap back and forth
  // if the browser is slow to answer.
  let toggling = $state(false);
  let pendingEnabled = $state(rateLimitAlertSettings.enabled);

  $effect(() => {
    if (!toggling) pendingEnabled = rateLimitAlertSettings.enabled;
  });

  async function handleToggle(next: boolean) {
    if (toggling) return;
    pendingEnabled = next;
    toggling = true;
    try {
      if (next) {
        await rateLimitAlertSettings.enable();
      } else {
        rateLimitAlertSettings.disable();
      }
    } finally {
      // enable()/disable() already update rateLimitAlertSettings.permission
      // and .enabled; re-reading Notification.permission here isn't
      // needed and would only race a mocked/stubbed implementation.
      pendingEnabled = rateLimitAlertSettings.enabled;
      toggling = false;
    }
  }

  const permissionMessage = $derived.by(() => {
    switch (rateLimitAlertSettings.permission) {
      case "granted":
        return m.settings_rate_limit_alerts_permission_granted();
      case "denied":
        return m.settings_rate_limit_alerts_permission_denied();
      case "unsupported":
        return m.settings_rate_limit_alerts_permission_unsupported();
      default:
        return m.settings_rate_limit_alerts_permission_default();
    }
  });

  function sliderValue(event: Event): number {
    return Number((event.currentTarget as HTMLInputElement).value);
  }

  const observedVendors = $derived(deriveVendors(rateLimitAlertRunner.observedWindows));
  const observedSources = $derived(deriveSources(rateLimitAlertRunner.observedWindows));

  function setVendorOverrideEnabled(vendor: string, checked: boolean, currentValue: number) {
    rateLimitAlertSettings.setVendorThresholdOverride(vendor, checked ? currentValue : null);
  }
</script>

<div class="rate-limit-alert-settings">
  <div class="setting-row">
    <span class="setting-label">{m.settings_rate_limit_alerts_enable_label()}</span>
    <Toggle
      checked={pendingEnabled}
      disabled={toggling}
      ariaLabel={m.settings_rate_limit_alerts_enable_label()}
      onchange={handleToggle}
    >
      {pendingEnabled ? m.settings_rate_limit_alerts_enabled() : m.settings_rate_limit_alerts_disabled()}
    </Toggle>
  </div>

  <p
    class="permission-note"
    class:permission-note--warn={rateLimitAlertSettings.permission === "denied" ||
      rateLimitAlertSettings.permission === "unsupported"}
  >
    {permissionMessage}
  </p>

  {#if rateLimitAlertSettings.enabled}
    <div class="setting-row column">
      <label class="setting-label" for="rate-limit-default-threshold">
        {m.settings_rate_limit_alerts_default_threshold_label()}
      </label>
      <div class="slider-field">
        <input
          id="rate-limit-default-threshold"
          class="threshold-slider"
          type="range"
          min={SLIDER_MIN_THRESHOLD_PERCENT}
          max={SLIDER_MAX_THRESHOLD_PERCENT}
          step="1"
          value={rateLimitAlertSettings.defaultThresholdPercent}
          aria-label={m.settings_rate_limit_alerts_percent_aria()}
          oninput={(e) => rateLimitAlertSettings.setDefaultThresholdPercent(sliderValue(e))}
        />
        <span class="slider-value">
          {m.settings_rate_limit_alerts_slider_value({ percent: rateLimitAlertSettings.defaultThresholdPercent })}
        </span>
      </div>
      <span class="setting-hint">{m.settings_rate_limit_alerts_default_threshold_hint()}</span>
    </div>

    <div class="setting-row">
      <Checkbox
        checked={rateLimitAlertSettings.notifyOnExhausted}
        label={m.settings_rate_limit_alerts_exhausted_checkbox()}
        onchange={(checked) => rateLimitAlertSettings.setNotifyOnExhausted(checked)}
      />
    </div>

    {#if observedVendors.length > 0}
      <div class="setting-row column">
        <span class="setting-label">{m.settings_rate_limit_alerts_vendor_overrides_title()}</span>
        {#each observedVendors as vendor (vendor)}
          {@const overrideValue = rateLimitAlertSettings.vendorThresholdOverrides[vendor]}
          {@const hasOverride = overrideValue !== undefined}
          {@const currentValue = overrideValue ?? rateLimitAlertSettings.defaultThresholdPercent}
          <div class="override-row">
            <Checkbox
              checked={hasOverride}
              label={m.settings_rate_limit_alerts_vendor_override_checkbox({
                vendor: vendorDisplayName(vendor),
              })}
              onchange={(checked) => setVendorOverrideEnabled(vendor, checked, currentValue)}
            />
            {#if hasOverride}
              <div class="slider-field">
                <input
                  class="threshold-slider"
                  type="range"
                  min={SLIDER_MIN_THRESHOLD_PERCENT}
                  max={SLIDER_MAX_THRESHOLD_PERCENT}
                  step="1"
                  value={currentValue}
                  aria-label={`${vendorDisplayName(vendor)} ${m.settings_rate_limit_alerts_percent_aria()}`}
                  oninput={(e) => rateLimitAlertSettings.setVendorThresholdOverride(vendor, sliderValue(e))}
                />
                <span class="slider-value">
                  {m.settings_rate_limit_alerts_slider_value({ percent: currentValue })}
                </span>
              </div>
            {/if}
          </div>
        {/each}
      </div>
    {/if}

    <div class="setting-row column">
      <span class="setting-label">{m.settings_rate_limit_alerts_sources_title()}</span>
      {#if observedSources.length === 0}
        <span class="setting-hint">{m.settings_rate_limit_alerts_sources_empty()}</span>
      {:else}
        <span class="setting-hint">{m.settings_rate_limit_alerts_sources_hint()}</span>
        <div class="sources-list">
          {#each observedSources as source (source.key)}
            <Checkbox
              checked={!rateLimitAlertSettings.isSourceMuted(source.key)}
              label={source.label}
              ariaLabel={`${source.label} (${source.account})`}
              onchange={(checked) => rateLimitAlertSettings.setSourceMuted(source.key, !checked)}
            />
          {/each}
        </div>
      {/if}
    </div>
  {/if}
</div>

<style>
  .rate-limit-alert-settings {
    display: flex;
    flex-direction: column;
    gap: var(--space-5);
  }

  .setting-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
  }

  .setting-row.column {
    flex-direction: column;
    align-items: flex-start;
    gap: 6px;
  }

  .setting-label {
    font-size: 12px;
    font-weight: 500;
    color: var(--text-secondary);
  }

  .setting-hint {
    font-size: 11px;
    color: var(--text-muted);
  }

  .permission-note {
    margin: 0;
    font-size: 11px;
    color: var(--text-muted);
  }

  .permission-note--warn {
    color: var(--accent-amber, #f59e0b);
  }

  /* kit-ui has no slider/range control (checked node_modules and
   * DESIGN.md's component list) — this is a native <input type="range">
   * with minimal scoped styling on the app's CSS custom properties,
   * following the same pattern DESIGN.md documents for other kit-ui
   * gaps. Revisit if/when kit-ui gains one. */
  .slider-field {
    display: flex;
    align-items: center;
    gap: 10px;
    width: 100%;
    max-width: 320px;
  }

  .threshold-slider {
    flex: 1;
    min-width: 0;
    accent-color: var(--accent-blue);
  }

  .slider-value {
    font-size: 12px;
    color: var(--text-muted);
    white-space: nowrap;
    min-width: 6ch;
    text-align: right;
  }

  .override-row {
    display: flex;
    flex-direction: column;
    gap: 6px;
    width: 100%;
  }

  .sources-list {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
</style>
