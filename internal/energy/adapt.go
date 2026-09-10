// ABOUTME: Storage-agnostic adapters shared by every backend's energy.go
// ABOUTME: (internal/db, internal/postgres, internal/duckdb) and by the CLI
// ABOUTME: and Settings-panel diagnostics that resolve a model's effective rate.
package energy

import (
	"time"

	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/pricing"
)

// RatesFromModelRates adapts the cost pipeline's resolved export.ModelRates
// (money.Money microdollar fields) to the plain-dollar Rates this package's
// Estimator expects. Shared by every storage backend's energy.go so the
// microdollar-to-dollar conversion has one implementation instead of three
// copies that could drift.
func RatesFromModelRates(r export.ModelRates) Rates {
	dollars := func(m money.Money) float64 { return float64(m.Microdollars) / 1_000_000 }
	return Rates{
		InputPerMTok:      dollars(r.InputPerMTok),
		OutputPerMTok:     dollars(r.OutputPerMTok),
		CacheWritePerMTok: dollars(r.CacheWritePerMTok),
		CacheReadPerMTok:  dollars(r.CacheReadPerMTok),
	}
}

// ModelIdentity returns the identity Estimate uses for vendor-family
// detection and config.toml [energy.overrides] lookup: the pricing
// catalog's matched pattern (export.PricingLookup.Pattern), which is what a
// user reads back as matched_pattern and what they key an override on.
// pricedModel (ResolveAt's canonical/reported model name) can differ from
// the matched pattern after normalization or alias resolution, so falling
// back to it silently when Pattern is set would make a correctly-keyed
// override miss. Pattern is only empty when nothing matched, in which case
// OutputPerMTok is also zero and Estimate already short-circuits to
// StatusNoRate before either identity would be used. Shared by every
// storage backend's energy.go.
func ModelIdentity(pricedModel string, lookup export.PricingLookup) string {
	if lookup.Pattern != "" {
		return lookup.Pattern
	}
	return pricedModel
}

// BillableOutputTokens mirrors export.ModelRates.CostForTokensScoped's
// reasoning fallback exactly: reasoning_tokens is a breakdown of
// output_tokens for every currently supported provider (see
// docs/internal/session-format-sources.md), not additional volume, so this
// never double-counts. It exists because a reasoning-only row can still
// have output_tokens == 0 despite genuinely contributing (the same reason
// cost's fallback exists); without it such a row's energy silently reads as
// zero while its cost, computed by the same fallback, does not. Shared by
// every storage backend's energy.go.
func BillableOutputTokens(outputTokens, reasoningTokens int64) int64 {
	if outputTokens == 0 {
		return reasoningTokens
	}
	return outputTokens
}

// EffectiveRates is one model's resolved vendor, which fit it uses, its
// energy status, and its effective Wh/MTok for each token type at the
// estimator's configured scenario and overrides. It backs both
// `usage energy-model <model>` and the Settings > Preferences energy
// panel's effective-rate table, so the CLI diagnostic and the UI cannot
// silently disagree about what a model's number means.
type EffectiveRates struct {
	Model           string
	Vendor          string
	FitUsed         string
	Status          Status
	PriceOutPerMTok float64
	// MicroWhPerMTok maps "input"/"output"/"cache_write"/"cache_read" to
	// the energy of one million tokens of that type, in micro-Wh, at the
	// estimator's configured scenario and any per-model override.
	MicroWhPerMTok map[string]int64
}

// energyRateTokenTypes lists the four token types EffectiveRates reports,
// each probed with 1,000,000 tokens of just that type so the returned
// MicroWhPerMTok is exactly the effective per-type rate.
var energyRateTokenTypes = []struct {
	name   string
	tokens Tokens
}{
	{"input", Tokens{Input: 1_000_000}},
	{"output", Tokens{Output: 1_000_000}},
	{"cache_write", Tokens{CacheWrite: 1_000_000}},
	{"cache_read", Tokens{CacheRead: 1_000_000}},
}

// ResolveEffectiveRates resolves model through resolver -- the same
// export.PricingResolver live usage reads build from the archive's
// refreshed pricing catalog -- and computes its effective per-token-type
// energy rate with estimator. ok is false when the resolver has no output
// rate for the model (no catalog price, so no cost and no energy either),
// matching Estimate's own StatusNoRate short-circuit.
func ResolveEffectiveRates(
	model string, resolver *export.PricingResolver, estimator *Estimator,
) (EffectiveRates, bool) {
	canonical := pricing.CanonicalModelForTimestamp(model, "")
	pricedModel, lookup := resolver.ResolveAt(model, canonical, time.Time{})
	if !lookup.OK {
		return EffectiveRates{Model: model, Status: StatusNoRate}, false
	}

	rates := RatesFromModelRates(lookup.Rates)
	if rates.OutputPerMTok <= 0 {
		return EffectiveRates{Model: model, Status: StatusNoRate}, false
	}

	identity := ModelIdentity(pricedModel, lookup)
	vendor := VendorForModel(identity)
	fitUsed := "pooled"
	if estimator != nil {
		if _, ok := estimator.Fit.Families[vendor]; ok && vendor != "" {
			fitUsed = vendor
		}
	}

	microWh := make(map[string]int64, len(energyRateTokenTypes))
	var status Status
	for _, tt := range energyRateTokenTypes {
		v, s := estimator.Estimate(identity, rates, tt.tokens)
		microWh[tt.name] = v
		if tt.name == "output" {
			status = s
		}
	}
	return EffectiveRates{
		Model: pricedModel, Vendor: vendor, FitUsed: fitUsed, Status: status,
		PriceOutPerMTok: rates.OutputPerMTok, MicroWhPerMTok: microWh,
	}, true
}
