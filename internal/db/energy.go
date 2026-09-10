// ABOUTME: Adapts export.ModelRates/PricingResolver into the internal/energy
// ABOUTME: estimator, and computes energy_micro_wh at usage-read time.
package db

import (
	"sync"

	"go.kenn.io/agentsview/internal/energy"
	"go.kenn.io/agentsview/internal/export"
)

var embeddedEnergyFit = sync.OnceValues(func() (energy.FitSet, error) {
	return energy.LoadEmbeddedFitSet()
})

// energyEstimator returns the estimator configured by SetEnergyConfig (or
// the fit's mid scenario with no overrides when unset). The embedded fit is
// parsed once per process. Reads energyScenario/energyOverrides under
// energyMu so a concurrent SetEnergyConfig (the GET/POST
// /api/v1/config/energy handler can call it while usage reads are in
// flight) never hands back a torn scenario/overrides pair, and builds one
// Estimator value per call so a single row's estimate is never split
// across two scenarios.
func (db *DB) energyEstimator() (*energy.Estimator, error) {
	fit, err := embeddedEnergyFit()
	if err != nil {
		return nil, err
	}
	db.energyMu.RLock()
	scenario, overrides := db.energyScenario, db.energyOverrides
	db.energyMu.RUnlock()
	return energy.NewEstimator(fit, scenario, overrides), nil
}

// combineEnergyStatus folds one more constituent's energy status into an
// aggregate (a day, a project, a whole totals row). See energy.CombineStatus.
func combineEnergyStatus(existing, next string) string {
	return energy.CombineStatus(existing, next)
}

// estimateEnergyWith computes the energy_micro_wh/energy_status pair for
// one model's token usage at its currently resolved list price, using an
// already-built estimator (see energyEstimator) rather than deriving one
// per call. groupEnergyEstimate and sessionRowEnergy run inside per-group/
// per-row loops over a single usage-read request; building the estimator
// once before that loop (as every other read path already does -- see
// GetDailyUsage/GetTopSessionsByCost's legacy per-row variants and
// postgres.pgDailyUsageAmounts/pgSessionRowEnergy) means a SetEnergyConfig
// call concurrent with the request can only affect the *next* request, not
// split one aggregate across two scenarios. model must be the pricing
// catalog's matched pattern (see energy.ModelIdentity), the same identity
// config.toml's [energy.overrides] and the cost pipeline's matched_pattern
// field use, so vendor detection and per-model overrides line up with cost
// and with what a user reads in the catalog.
func estimateEnergyWith(
	estimator *energy.Estimator, model string, rates export.ModelRates, tokens energy.Tokens,
) (int64, string) {
	microWh, status := estimator.Estimate(model, energy.RatesFromModelRates(rates), tokens)
	return microWh, string(status)
}

// ratesAtBandThreshold returns the tiered rate a caller already pinned by
// AboveInputTokens threshold (group.BandThreshold), reusing
// export.ModelRates.RatesForTokens' own band-matching instead of
// duplicating it: threshold+1 as a synthetic input-token count always
// lands in exactly the band whose AboveInputTokens equals threshold, never
// a higher one, since threshold is by construction the largest
// AboveInputTokens a real request here matched. Returns rates unchanged
// when threshold is nil (no band applied) or no longer matches any
// currently published band (the catalog's bands changed since this group
// was built), the same safe fallback RatesForTokens itself returns for an
// unmatched total.
func ratesAtBandThreshold(rates export.ModelRates, threshold *int) export.ModelRates {
	if threshold == nil {
		return rates
	}
	return rates.RatesForTokens(*threshold+1, 0, 0)
}

// groupEnergyEstimate resolves one usageFactsGroup's current list-price
// rates (the same resolver call recordUsageFactsPricing makes; the
// resolver's own lookup cache makes the repeat call cheap) and estimates
// its energy from the token counts the daily rollup already stores. It
// always uses the model's *current* rates, not the historical rate pinned
// at rollup-build time for cost -- ratios between token types rarely
// change even when absolute price does, and the estimator has no
// historical-rate story of its own. The current rate is still resolved to
// the correct price *band*, though: group.BandThreshold pins the band
// every underlying fact in this group resolved to when they were merged
// (aggregateUsageRollupExceptions keys grouping on the band, so facts on
// different bands are never combined into one group), and reselecting a
// band from the group's summed token totals instead -- rather than the
// band its individual, pre-merge facts actually matched -- would be wrong
// the same way it would be for cost, which never rebands an aggregate from
// its summed totals either.
func groupEnergyEstimate(
	estimator *energy.Estimator, resolver *export.PricingResolver, group usageFactsGroup,
) (int64, string) {
	timestamp := usagePricingTimestamp(group.PricingTimestamp)
	pricedModel, lookup := resolver.ResolveAt(group.Model, group.PricedModel, timestamp)
	rates := ratesAtBandThreshold(lookup.Rates, group.BandThreshold)
	tokens := energy.Tokens{
		Input: group.InputTokens,
		// group.EnergyBillableOutputTokens, not
		// energy.BillableOutputTokens(group.OutputTokens,
		// group.ReasoningTokens): the fallback must apply per fact before
		// the group's raw counters are summed together, or a reasoning-only
		// fact merged with an ordinary-output fact loses its contribution
		// entirely (see EnergyBillableOutputTokens's doc comment).
		Output:     group.EnergyBillableOutputTokens,
		CacheWrite: group.CacheCreationTokens,
		CacheRead:  group.CacheReadTokens,
	}
	return estimateEnergyWith(estimator, energy.ModelIdentity(pricedModel, lookup), rates, tokens)
}

// sessionRowEnergy estimates one session-usage row's energy from the same
// token extraction sessionRowCostWithWebSearchRequests and
// sessionUsageBreakdownEntryWithWebSearchRequests use, at the row's
// currently resolved list-price rate (see groupEnergyEstimate for why
// current, not historical, rates are used) and, for a request-scoped row,
// the same tiered band cost resolves via CostForTokensScoped(requestScoped,
// ...) in sessionRowCostWithWebSearchRequests -- otherwise a row whose
// tokens cross a pricing band's threshold would price its cost and its
// energy off two different rates.
func sessionRowEnergy(
	estimator *energy.Estimator, r usageScanRow, resolver *export.PricingResolver,
) (int64, string) {
	var inTok, outTok, crTok, rdTok int
	reasoningTok := r.reasoningTokens
	if r.usageSource == "message" {
		inTok, outTok, crTok, rdTok, reasoningTok = clampedUsageTokenCountersWithReasoning(r.tokenJSON)
	} else {
		inTok, outTok, crTok, rdTok = usageEventRowTokens(
			r.usageSource, r.inputTokens, r.outputTokens,
			r.cacheCreationInputTokens, r.cacheReadInputTokens)
	}
	pricedModel, lookup := resolver.ResolveAt(
		r.model, usageLookupModel(r.model, r.pricingTS),
		usagePricingTimestamp(r.pricingTS),
	)
	rates := lookup.Rates
	if usageRowIsRequestScoped(r.usageSource, r.messageOrdinal) {
		rates = rates.RatesForTokens(inTok, crTok, rdTok)
	}
	tokens := energy.Tokens{
		Input:      int64(inTok),
		Output:     energy.BillableOutputTokens(int64(outTok), int64(reasoningTok)),
		CacheWrite: int64(crTok), CacheRead: int64(rdTok),
	}
	return estimateEnergyWith(estimator, energy.ModelIdentity(pricedModel, lookup), rates, tokens)
}
