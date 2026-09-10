<script lang="ts">
  import { onMount } from "svelte";
  import { Typeahead, type TypeaheadOption } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import { formatEnergy } from "../../energy.js";
  import {
    energyEstimate,
    type EnergyScenario,
  } from "../../stores/energyEstimate.svelte.js";
  import { usage } from "../../stores/usage.svelte.js";

  // The Usage page's current model list (if it has one loaded), reused
  // here instead of re-deriving one server-side, so the effective-rate
  // table matches whatever range and filters the Usage page has selected.
  // The usage store is a page-independent singleton, so this still has a
  // value when Settings is opened without visiting Usage first in this
  // session -- energyConfigBodyFromConfig falls back to the two
  // representative model classes when it is empty.
  const rangeModels = $derived(
    (usage.summary?.modelTotals ?? []).map((m) => m.model),
  );

  onMount(() => {
    // load(), not ensureLoaded(): this panel is where a user checks or
    // changes the scenario, so it must always show the server's current
    // value, including a change made from another tab or another client
    // since this store last loaded -- ensureLoaded()'s "already loaded,
    // skip" shortcut is right for a passive estimate marker but wrong
    // here.
    void energyEstimate.load(rangeModels);
  });

  const scenarioOptions: TypeaheadOption[] = $derived([
    { name: "low", label: m.usage_energy_scenario_low() },
    { name: "mid", label: m.usage_energy_scenario_mid() },
    { name: "high", label: m.usage_energy_scenario_high() },
  ]);

  function isScenario(value: string): value is EnergyScenario {
    return value === "low" || value === "mid" || value === "high";
  }

  function handleScenarioSelect(value: string) {
    if (!isScenario(value)) return;
    void energyEstimate.setScenario(value);
  }

  function rateFor(
    model: (typeof energyEstimate.models)[number],
    tokenType: string,
  ): string {
    const rate = model.rates.find((r) => r.token_type === tokenType);
    return rate ? formatEnergy(rate.wh_per_mtok_micro_wh) : "--";
  }
</script>

<div class="energy-settings">
  <div class="setting-row">
    <span class="setting-label">{m.settings_energy_scenario_label()}</span>
    <Typeahead
      options={scenarioOptions}
      value={energyEstimate.scenario}
      fallbackLabel={m.usage_energy_scenario_mid()}
      placeholder={m.settings_energy_scenario_label()}
      title={m.settings_energy_scenario_label()}
      onselect={handleScenarioSelect}
      disabled={energyEstimate.saving}
    />
  </div>
  <p class="setting-description">{m.settings_energy_scenario_description()}</p>

  {#if energyEstimate.saveError}
    <p class="energy-settings-error">{energyEstimate.saveError}</p>
  {/if}

  <h4 class="energy-rates-title">{m.settings_energy_effective_rates_title()}</h4>
  {#if energyEstimate.loading && !energyEstimate.loaded}
    <p class="energy-settings-status">{m.settings_energy_loading()}</p>
  {:else if energyEstimate.error}
    <p class="energy-settings-error">{energyEstimate.error}</p>
  {:else}
    <div class="energy-rates-table-wrap">
      <table class="energy-rates-table">
        <thead>
          <tr>
            <th>{m.settings_energy_col_model()}</th>
            <th>{m.settings_energy_col_input()}</th>
            <th>{m.settings_energy_col_output()}</th>
            <th>{m.settings_energy_col_cache_write()}</th>
            <th>{m.settings_energy_col_cache_read()}</th>
          </tr>
        </thead>
        <tbody>
          {#each energyEstimate.models as model (model.model)}
            <tr>
              <td>{model.model}</td>
              <td>{rateFor(model, "input")}</td>
              <td>{rateFor(model, "output")}</td>
              <td>{rateFor(model, "cache_write")}</td>
              <td>{rateFor(model, "cache_read")}</td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
  {/if}
</div>

<style>
  .energy-settings {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }

  .setting-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
  }

  .setting-label {
    font-size: 12px;
    font-weight: 500;
    color: var(--text-secondary);
    white-space: nowrap;
  }

  .setting-description {
    margin: 0;
    font-size: 11px;
    color: var(--text-muted);
  }

  .energy-rates-title {
    margin: 8px 0 0;
    font-size: 12px;
    font-weight: 600;
    color: var(--text-primary);
  }

  .energy-settings-status,
  .energy-settings-error {
    margin: 0;
    font-size: 11px;
    color: var(--text-muted);
  }

  .energy-settings-error {
    color: var(--accent-red);
  }

  .energy-rates-table-wrap {
    overflow-x: auto;
  }

  .energy-rates-table {
    width: 100%;
    border-collapse: collapse;
    font-size: 11px;
  }

  .energy-rates-table th,
  .energy-rates-table td {
    padding: 4px 8px;
    text-align: right;
    border-bottom: 1px solid var(--border-muted);
    white-space: nowrap;
  }

  .energy-rates-table th:first-child,
  .energy-rates-table td:first-child {
    text-align: left;
    font-family: var(--font-mono);
    color: var(--text-secondary);
  }

  .energy-rates-table th {
    color: var(--text-muted);
    font-weight: 500;
  }

  .energy-rates-table td {
    color: var(--text-primary);
    font-variant-numeric: tabular-nums;
  }
</style>
