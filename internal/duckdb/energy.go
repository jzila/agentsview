// ABOUTME: Adapts export.ModelRates/PricingResolver into the internal/energy
// ABOUTME: estimator for the DuckDB session-usage read path.
package duckdb

import (
	"sync"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/energy"
	"go.kenn.io/agentsview/internal/export"
)

var duckEmbeddedEnergyFit = sync.OnceValues(func() (energy.FitSet, error) {
	return energy.LoadEmbeddedFitSet()
})

// energyEstimator returns the estimator configured by SetEnergyConfig, or
// the fit's mid scenario with no overrides for a store that never had
// SetEnergyConfig called (the disposable push mirror behind `agentsview
// serve`, which has no server or config.toml of its own). The embedded fit
// is parsed once per process. Reads energyScenario/energyOverrides under
// energyMu so a concurrent SetEnergyConfig (only reachable from `duckdb
// serve`'s own GET/POST /api/v1/config/energy handler) never hands back a
// torn pair, and builds one Estimator value per call so a single request
// (which calls this once up front, not per row -- see GetDailyUsage,
// GetTopSessionsByCost, GetSessionUsage) is never split across two
// scenarios.
func (s *Store) energyEstimator() (*energy.Estimator, error) {
	fit, err := duckEmbeddedEnergyFit()
	if err != nil {
		return nil, err
	}
	s.energyMu.RLock()
	scenario, overrides := s.energyScenario, s.energyOverrides
	s.energyMu.RUnlock()
	return energy.NewEstimator(fit, scenario, overrides), nil
}

// duckSessionUsageRowEnergy estimates one session-usage breakdown row's
// energy from its own token counts, at the row's currently resolved
// list-price rate (matching db.sessionRowEnergy's convention on the SQLite
// path, so both backends agree for the same archive), rebanded from this
// row's own tokens when it is request-scoped exactly the way
// duckSessionUsageRowCost bands cost via
// CostForTokensScoped(requestScoped, ...), so a row whose tokens cross a
// pricing band's threshold cannot price its cost and its energy off two
// different rates. estimator is built once per request by the caller (see
// Store.energyEstimator), not per row.
func duckSessionUsageRowEnergy(
	estimator *energy.Estimator, r duckSessionUsageRow, resolver *export.PricingResolver,
) (int64, string) {
	requestScoped := db.UsageSourceIsRequestScoped(r.source) || r.messageOrdinal.Valid
	return duckEstimateEnergy(
		estimator, r.model, duckSessionUsageLookupModel(r), r.pricingTS,
		r.inputTok, r.outputTok, r.reasoningTok, r.cacheCr, r.cacheRd,
		requestScoped, resolver,
	)
}

// duckAggregateRowEnergy is duckSessionUsageRowEnergy's counterpart for a
// duckUsageAggregateRow: despite its name, each row here is still one
// deduplicated usage row (forEachDailyUsageAggregateRow and
// forEachSessionUsageAggregateRow group rows into buckets in Go after
// pricing, not in SQL), so it takes the same requestScoped condition the
// caller already computes for duckUsageAggregateResolvedCost and rebands
// on it exactly the way that cost call does, or a request-scoped row that
// crosses a pricing band would price its energy at the base rate while its
// cost bills at the band.
func duckAggregateRowEnergy(
	estimator *energy.Estimator, r duckUsageAggregateRow, requestScoped bool,
	resolver *export.PricingResolver,
) (int64, string) {
	return duckEstimateEnergy(
		estimator, r.model, r.priceModel, r.pricingTS,
		r.inputTok, r.outputTok, r.reasoningTok, r.cacheCr, r.cacheRd,
		requestScoped, resolver,
	)
}

func duckEstimateEnergy(
	estimator *energy.Estimator,
	reportedModel, canonicalModel, pricingTS string,
	inputTok, outputTok, reasoningTok, cacheCr, cacheRd int,
	requestScoped bool,
	resolver *export.PricingResolver,
) (int64, string) {
	pricedModel, lookup := resolver.ResolveAt(
		reportedModel, canonicalModel, duckUsagePricingTimestamp(pricingTS),
	)
	rates := lookup.Rates
	if requestScoped {
		rates = rates.RatesForTokens(inputTok, cacheCr, cacheRd)
	}
	tokens := energy.Tokens{
		Input:      int64(inputTok),
		Output:     energy.BillableOutputTokens(int64(outputTok), int64(reasoningTok)),
		CacheWrite: int64(cacheCr), CacheRead: int64(cacheRd),
	}
	microWh, status := estimator.Estimate(
		energy.ModelIdentity(pricedModel, lookup), energy.RatesFromModelRates(rates), tokens)
	return microWh, string(status)
}
