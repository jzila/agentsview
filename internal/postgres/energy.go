// ABOUTME: Adapts export.ModelRates/PricingResolver into the internal/energy
// ABOUTME: estimator for the PostgreSQL usage-read path, mirroring
// ABOUTME: internal/db/energy.go and internal/duckdb/energy.go.
package postgres

import (
	"sync"

	"github.com/tidwall/gjson"

	"go.kenn.io/agentsview/internal/energy"
	"go.kenn.io/agentsview/internal/export"
)

var embeddedPGEnergyFit = sync.OnceValues(func() (energy.FitSet, error) {
	return energy.LoadEmbeddedFitSet()
})

// SetEnergyConfig installs the config.toml-derived scenario and per-model
// E_out overrides the energy estimator uses for every subsequent usage
// read from this store. An empty scenario behaves as energy.ScenarioMid; a
// nil overrides map behaves as no overrides. Mirrors db.DB.SetEnergyConfig.
func (s *Store) SetEnergyConfig(scenario energy.Scenario, overrides map[string]float64) {
	s.energyMu.Lock()
	defer s.energyMu.Unlock()
	s.energyScenario = scenario
	s.energyOverrides = overrides
}

// energyEstimator returns the estimator configured by SetEnergyConfig (or
// the fit's mid scenario with no overrides when unset). The embedded fit is
// parsed once per process.
func (s *Store) energyEstimator() (*energy.Estimator, error) {
	fit, err := embeddedPGEnergyFit()
	if err != nil {
		return nil, err
	}
	s.energyMu.RLock()
	scenario, overrides := s.energyScenario, s.energyOverrides
	s.energyMu.RUnlock()
	return energy.NewEstimator(fit, scenario, overrides), nil
}

// pgSessionRowEnergy estimates one session-usage row's energy from its own
// token counts, at the row's currently resolved (non-billing-scaled) list
// price rate -- rebanded from this row's own tokens when it is
// request-scoped exactly the way pgSessionRowCostWithWebSearchRequests
// bands cost via CostForTokensScoped(requestScoped, ...), so a row whose
// tokens cross a pricing band's threshold cannot price its cost and its
// energy off two different rates. Otherwise mirrors db.sessionRowEnergy's
// convention, so the SQLite and PostgreSQL session-usage paths agree for
// the same archive. Rate conversion, model-identity resolution, and the
// reasoning fallback are shared with internal/db and internal/duckdb via
// energy.RatesFromModelRates/ModelIdentity/BillableOutputTokens rather than
// duplicated here. There is no error return: every failure mode this
// function could hit (an unresolved model, a missing rate) is already
// represented by StatusNoRate, not an error.
func pgSessionRowEnergy(
	r pgUsageScanRow, estimator *energy.Estimator, resolver *export.PricingResolver,
) (int64, string) {
	var inTok, outTok, crTok, rdTok int
	reasoningTok := r.reasoningTokens
	if r.usageSource == "message" {
		usage := gjson.Parse(r.tokenJSON)
		inTok = pgTokenJSONCount(usage, "input_tokens")
		outTok = pgTokenJSONCount(usage, "output_tokens")
		crTok = pgTokenJSONCount(usage, "cache_creation_input_tokens")
		rdTok = pgTokenJSONCount(usage, "cache_read_input_tokens")
		reasoningTok = pgTokenJSONCount(usage, "reasoning_tokens")
	} else {
		inTok, outTok, crTok, rdTok = pgUsageEventRowTokens(
			r.usageSource,
			r.inputTokens, r.outputTokens,
			r.cacheCreationInputTokens, r.cacheReadInputTokens)
	}
	pricedModel, lookup := resolver.ResolveAt(
		r.model, pgUsageLookupModel(r.model, r.pricingTS),
		pgUsagePricingTimestamp(r.pricingTS),
	)
	rates := lookup.Rates
	if pgUsageRowIsRequestScoped(r.usageSource, r.messageOrdinal) {
		rates = rates.RatesForTokens(inTok, crTok, rdTok)
	}
	tokens := energy.Tokens{
		Input:      int64(inTok),
		Output:     energy.BillableOutputTokens(int64(outTok), int64(reasoningTok)),
		CacheWrite: int64(crTok), CacheRead: int64(rdTok),
	}
	microWh, status := estimator.Estimate(
		energy.ModelIdentity(pricedModel, lookup), energy.RatesFromModelRates(rates), tokens)
	return microWh, string(status)
}
