package energy_test

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/energy"
)

func testFitSet(t *testing.T) energy.FitSet {
	t.Helper()
	fs, err := energy.LoadEmbeddedFitSet()
	require.NoError(t, err)
	return fs
}

// TestEstimate_WeightingAndFallbacks checks the four-token weighting formula and its cache-rate fallback.
func TestEstimate_WeightingAndFallbacks(t *testing.T) {
	fitSet := testFitSet(t)
	e := energy.NewEstimator(fitSet, energy.ScenarioMid, nil)
	for _, tc := range []struct {
		name        string
		model       string
		vendor      string
		rates       energy.Rates
		tokens      energy.Tokens
		outputEquiv float64
	}{
		{
			name:        "published cache rates",
			model:       "claude-sonnet-4-6",
			vendor:      "anthropic",
			rates:       energy.Rates{InputPerMTok: 3, OutputPerMTok: 15, CacheWritePerMTok: 3.75, CacheReadPerMTok: 0.30},
			tokens:      energy.Tokens{Input: 10_000, Output: 2_000, CacheWrite: 500, CacheRead: 50_000},
			outputEquiv: 2_000 + 10_000*0.2 + 500*0.25 + 50_000*0.02,
		},
		{
			name:        "both cache rates fall back when unpublished",
			model:       "some-model",
			rates:       energy.Rates{InputPerMTok: 4, OutputPerMTok: 20},
			tokens:      energy.Tokens{CacheWrite: 1_000_000, CacheRead: 1_000_000},
			outputEquiv: 1_000_000*0.25 + 1_000_000*0.02,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eOut := fitSet.EOutForModel(tc.vendor, tc.rates.OutputPerMTok, energy.ScenarioMid)
			want := int64(math.Round(eOut * tc.outputEquiv))
			got, status := e.Estimate(tc.model, tc.rates, tc.tokens)
			assert.Equal(t, energy.StatusOK, status)
			assert.Equal(t, want, got)
		})
	}
}

func TestEstimate_NoRateWhenCatalogHasNothing(t *testing.T) {
	e := energy.NewEstimator(testFitSet(t), energy.ScenarioMid, nil)
	got, status := e.Estimate("unknown-model", energy.Rates{}, energy.Tokens{Output: 1000})
	assert.Equal(t, energy.StatusNoRate, status)
	assert.Zero(t, got)
}

// TestEstimate_Override checks a config override replaces the fit for a priced model but cannot rescue a model with no catalog rate.
func TestEstimate_Override(t *testing.T) {
	fitSet := testFitSet(t)
	rates := energy.Rates{InputPerMTok: 3, OutputPerMTok: 15, CacheWritePerMTok: 3.75, CacheReadPerMTok: 0.3}
	e := energy.NewEstimator(fitSet, energy.ScenarioMid, map[string]float64{"claude-sonnet-4-6": 1234.5})
	got, status := e.Estimate("claude-sonnet-4-6", rates, energy.Tokens{Output: 1_000_000})
	assert.Equal(t, energy.StatusOverride, status)
	assert.Equal(t, int64(1234.5*1_000_000), got)
	e2 := energy.NewEstimator(fitSet, energy.ScenarioMid, map[string]float64{"unknown-model": 999})
	got2, status2 := e2.Estimate("unknown-model", energy.Rates{}, energy.Tokens{Output: 1000})
	assert.Equal(t, energy.StatusNoRate, status2)
	assert.Zero(t, got2)
}

// TestEstimate_ScenarioOrdering checks low < mid < high energy across scenarios for the same tokens and rates.
func TestEstimate_ScenarioOrdering(t *testing.T) {
	fitSet := testFitSet(t)
	rates := energy.Rates{InputPerMTok: 3, OutputPerMTok: 15, CacheWritePerMTok: 3.75, CacheReadPerMTok: 0.3}
	tokens := energy.Tokens{Output: 1_000_000}
	lowV, _ := energy.NewEstimator(fitSet, energy.ScenarioLow, nil).Estimate("claude-sonnet-4-6", rates, tokens)
	midV, _ := energy.NewEstimator(fitSet, energy.ScenarioMid, nil).Estimate("claude-sonnet-4-6", rates, tokens)
	highV, _ := energy.NewEstimator(fitSet, energy.ScenarioHigh, nil).Estimate("claude-sonnet-4-6", rates, tokens)
	assert.Less(t, lowV, midV)
	assert.Less(t, midV, highV)
}

// TestEstimate_MonotonicAndZeroAtZero is the property test the kata requires: zero at zero tokens, non-decreasing in every token count.
func TestEstimate_MonotonicAndZeroAtZero(t *testing.T) {
	fitSet := testFitSet(t)
	e := energy.NewEstimator(fitSet, energy.ScenarioMid, nil)
	rates := energy.Rates{InputPerMTok: 3, OutputPerMTok: 15, CacheWritePerMTok: 3.75, CacheReadPerMTok: 0.3}
	zero, status := e.Estimate("claude-sonnet-4-6", rates, energy.Tokens{})
	require.Equal(t, energy.StatusOK, status)
	assert.Zero(t, zero)
	base := energy.Tokens{Input: 1000, Output: 2000, CacheWrite: 300, CacheRead: 4000}
	baseVal, _ := e.Estimate("claude-sonnet-4-6", rates, base)
	for _, bumped := range []energy.Tokens{
		{Input: base.Input + 500, Output: base.Output, CacheWrite: base.CacheWrite, CacheRead: base.CacheRead},
		{Input: base.Input, Output: base.Output + 500, CacheWrite: base.CacheWrite, CacheRead: base.CacheRead},
		{Input: base.Input, Output: base.Output, CacheWrite: base.CacheWrite + 500, CacheRead: base.CacheRead},
		{Input: base.Input, Output: base.Output, CacheWrite: base.CacheWrite, CacheRead: base.CacheRead + 500},
	} {
		bumpedVal, _ := e.Estimate("claude-sonnet-4-6", rates, bumped)
		assert.GreaterOrEqual(t, bumpedVal, baseVal)
	}
}
