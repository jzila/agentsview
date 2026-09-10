<script lang="ts">
  import { Tooltip } from "@kenn-io/kit-ui";
  import { onMount } from "svelte";
  import { m } from "../../i18n/index.js";
  import { energyEquivalenceOfTV, formatEnergyEquivalenceValue } from "../../energy.js";
  import { energyEstimate } from "../../stores/energyEstimate.svelte.js";

  interface Props {
    /** The micro-Wh value this mark annotates, used only to compute the
     * tooltip's one relatable equivalence -- never re-derives the figure
     * shown next to it. */
    microWh: number;
    align?: "start" | "end";
  }

  let { microWh, align = "end" }: Props = $props();

  onMount(() => {
    void energyEstimate.ensureLoaded();
  });

  const scenarioLabel = $derived.by(() => {
    switch (energyEstimate.scenario) {
      case "low":
        return m.usage_energy_scenario_low();
      case "high":
        return m.usage_energy_scenario_high();
      default:
        return m.usage_energy_scenario_mid();
    }
  });

  const equivalence = $derived(energyEquivalenceOfTV(microWh));
  const equivalenceLabel = $derived(formatEnergyEquivalenceValue(equivalence.value));
</script>

<Tooltip {align} focusable class="energy-estimate-tooltip">
  {#snippet content()}
    <div class="energy-estimate-content">
      <p class="energy-estimate-scenario">
        {m.usage_energy_estimate_tooltip_scenario({ scenario: scenarioLabel })}
      </p>
      <p class="energy-estimate-anchor">{m.usage_energy_estimate_tooltip_anchor()}</p>
      {#if equivalence.value > 0}
        <p class="energy-estimate-equivalence">
          {#if equivalence.unit === "minutes"}
            {m.usage_energy_estimate_tooltip_equivalence_minutes({
              value: equivalence.value,
              valueLabel: equivalenceLabel,
            })}
          {:else if equivalence.unit === "hours"}
            {m.usage_energy_estimate_tooltip_equivalence_hours({
              value: equivalence.value,
              valueLabel: equivalenceLabel,
            })}
          {:else}
            {m.usage_energy_estimate_tooltip_equivalence_days({
              value: equivalence.value,
              valueLabel: equivalenceLabel,
            })}
          {/if}
        </p>
      {/if}
      <a
        class="energy-estimate-link"
        href="https://github.com/kenn-io/agentsview/blob/main/docs/internal/energy-model.md"
        target="_blank"
        rel="noreferrer noopener"
      >
        {m.usage_energy_estimate_tooltip_methodology()}
      </a>
    </div>
  {/snippet}
  <span class="energy-estimate-mark" aria-label={m.usage_energy_estimate_mark_aria()}
    >~</span
  >
</Tooltip>

<style>
  .energy-estimate-mark {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: 13px;
    height: 13px;
    margin-left: 2px;
    border-radius: 50%;
    background: var(--bg-inset);
    color: var(--text-muted);
    font-size: 9px;
    font-weight: 700;
    line-height: 1;
    cursor: help;
  }

  .energy-estimate-content {
    display: flex;
    flex-direction: column;
    gap: 4px;
    max-width: 220px;
    font-size: 11px;
    line-height: 1.4;
  }

  .energy-estimate-scenario {
    font-weight: 600;
    color: var(--text-primary);
  }

  .energy-estimate-anchor,
  .energy-estimate-equivalence {
    color: var(--text-secondary);
  }

  .energy-estimate-link {
    color: var(--accent-blue);
    text-decoration: underline;
  }
</style>
