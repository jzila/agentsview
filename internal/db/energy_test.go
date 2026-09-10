package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/energy"
	"go.kenn.io/agentsview/internal/usagefacts"
)

// TestReasoningFallbackAppliesPerFactBeforeMerging: the fallback must run per fact before merging, for both aggregation paths.
func TestReasoningFallbackAppliesPerFactBeforeMerging(t *testing.T) {
	ordinary := usageRollupFact{Fact: usagefacts.Fact{OutputTokens: 100}}
	reasoningOnly := usageRollupFact{Fact: usagefacts.Fact{ReasoningTokens: 1000}}

	t.Run("group aggregation", func(t *testing.T) {
		group := &usageFactsGroup{}
		require.NoError(t, addUsageFactToGroup(group, ordinary, usagePriceResult{}))
		require.NoError(t, addUsageFactToGroup(group, reasoningOnly, usagePriceResult{}))
		assert.Equal(t, int64(1100), group.EnergyBillableOutputTokens)
	})

	t.Run("daily contribution aggregation", func(t *testing.T) {
		row := &usageDailyContribution{}
		require.NoError(t, addUsageFactToDailyContribution(row, ordinary, usagePriceResult{}))
		require.NoError(t, addUsageFactToDailyContribution(row, reasoningOnly, usagePriceResult{}))
		assert.Equal(t, int64(1100), row.EnergyBillableOutputTokens)
	})
}

// literalEnergyFitEstimator: E_out(price) = price exactly (A=1, B=1), so a
// tiered-rate regression's expected micro-Wh is a literal, hand-computed
// number instead of a positivity check. internal/postgres's
// TestPGSessionRowEnergyUsesRequestScopedBand shares this resolver's rates
// and literal fit, asserting the same totals for the same fixture.
func literalEnergyFitEstimator() *energy.Estimator {
	return energy.NewEstimator(
		energy.FitSet{Pooled: energy.FitResult{A: 1, B: 1}}, energy.ScenarioMid, nil)
}

// TestGroupEnergyEstimateUsesPinnedBand: "banded-model" is $1/$2/$0.50/$0.10
// (in/out/cache-write/cache-read per MTok) base, $2/$3/$1/$0.20 above
// 200,000 input tokens. At 300,000 input tokens with the literal fit:
// base outputEquiv=300,000*(1/2)=150,000, E_out($2)=2 -> 300,000 microWh;
// banded outputEquiv=300,000*(2/3)=200,000, E_out($3)=3 -> 600,000 microWh.
func TestGroupEnergyEstimateUsesPinnedBand(t *testing.T) {
	resolver := pricingBandTestResolver()
	estimator := literalEnergyFitEstimator()
	threshold := 200_000

	baseMicroWh, baseStatus := groupEnergyEstimate(estimator, resolver, usageFactsGroup{
		Model: "banded-model", PricedModel: "banded-model", InputTokens: 300_000,
	})
	bandedMicroWh, bandedStatus := groupEnergyEstimate(estimator, resolver, usageFactsGroup{
		Model: "banded-model", PricedModel: "banded-model", InputTokens: 300_000,
		BandThreshold: &threshold,
	})
	assert.Equal(t, "ok", baseStatus)
	assert.Equal(t, "ok", bandedStatus)
	assert.Equal(t, int64(300_000), baseMicroWh)
	assert.Equal(t, int64(600_000), bandedMicroWh)
}
