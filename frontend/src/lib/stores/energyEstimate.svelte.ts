import { ConfigService, type EnergyConfigBody } from "../api/generated/index";

export type EnergyScenario = "low" | "mid" | "high";

const DEFAULT_SCENARIO: EnergyScenario = "mid";

function isEnergyScenario(value: string): value is EnergyScenario {
  return value === "low" || value === "mid" || value === "high";
}

/**
 * Server-backed [energy] config: the scenario the estimator reports
 * (low/mid/high, see docs/internal/energy-model.md) plus a read-only
 * effective Wh/MTok-per-token-type table. That table lists the models a
 * caller passes to load() (the Settings panel passes the Usage page's
 * current range, see EnergyEstimateSettings.svelte), or, when none are
 * passed or none resolve, two representative model classes -- see
 * energyConfigBodyFromConfig on the Go side. Backs both the Settings >
 * Preferences "Energy estimate" panel and the scenario name every energy
 * tile's estimate marker shows.
 */
class EnergyEstimateStore {
  scenario: EnergyScenario = $state(DEFAULT_SCENARIO);
  models: EnergyConfigBody["models"] = $state([]);
  loaded: boolean = $state(false);
  loading: boolean = $state(false);
  saving: boolean = $state(false);
  error: string | null = $state(null);
  saveError: string | null = $state(null);

  // version guards against out-of-order responses. load() and setScenario()
  // can overlap -- an EnergyEstimateMark's ensureLoaded() request in flight
  // when Settings calls setScenario, or two overlapping setScenario calls
  // from quick clicks -- and network resolution order does not have to
  // match call order. Only the response from whichever call started most
  // recently is allowed to update state; an older call's response is
  // discarded even if it resolves later.
  private version = 0;

  // lastModels is the model list the most recent load() was asked for, so
  // setScenario() (which has no caller-supplied model list of its own) can
  // resend the same range instead of silently falling back to the
  // representative classes the moment a user changes the scenario.
  private lastModels: string[] = [];

  private applyBody(body: EnergyConfigBody) {
    this.scenario = isEnergyScenario(body.scenario) ? body.scenario : DEFAULT_SCENARIO;
    this.models = body.models;
  }

  /** Fetches the config once; safe to call repeatedly (e.g. from every
   *  estimate marker mounting) since it no-ops once loaded or in flight. */
  async ensureLoaded(): Promise<void> {
    if (this.loaded || this.loading) return;
    await this.load();
  }

  /** models are the model names currently shown in the caller's Usage
   *  range (see UsageSummaryResponse.modelTotals); omit for the
   *  representative-class fallback. */
  async load(models: string[] = []): Promise<void> {
    this.lastModels = models;
    const v = ++this.version;
    this.loading = true;
    this.error = null;
    try {
      const data = await ConfigService.getApiV1ConfigEnergy(
        models.length ? { model: models } : undefined,
      );
      if (v === this.version) {
        this.applyBody(data);
        // Only a successful response marks configuration as loaded. A
        // failed fetch leaves loaded false so ensureLoaded() retries on
        // the next mount instead of permanently giving up after one
        // transient error (matching how SessionBreadcrumb's cost fetch
        // leaves its own fetch key unset on failure for the same reason).
        this.loaded = true;
      }
    } catch (e) {
      if (v === this.version) {
        this.error = e instanceof Error ? e.message : "Failed to load energy config";
      }
    } finally {
      // loading tracks only this call's own in-flight status, so it always
      // resolves when this call's own request finishes; the version check
      // above guards only whether the response's *data* (and loaded/error)
      // is still the most current answer, so a stale response can't
      // overwrite what a newer load or save already applied.
      this.loading = false;
    }
  }

  async setScenario(scenario: EnergyScenario): Promise<boolean> {
    const v = ++this.version;
    this.saving = true;
    this.saveError = null;
    try {
      const data = await ConfigService.postApiV1ConfigEnergy({
        scenario,
        models: this.lastModels.length ? this.lastModels : undefined,
      });
      if (v === this.version) this.applyBody(data);
      return true;
    } catch (e) {
      if (v === this.version) {
        this.saveError = e instanceof Error ? e.message : "Failed to save energy config";
      }
      return false;
    } finally {
      this.saving = false;
    }
  }
}

export const energyEstimate = new EnergyEstimateStore();
