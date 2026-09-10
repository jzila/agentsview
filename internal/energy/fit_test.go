package energy_test

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/energy"
)

// TestWeightedLogLogFit_ComputesRegression checks the fit against a hand-computed unweighted three-point OLS regression.
func TestWeightedLogLogFit_ComputesRegression(t *testing.T) {
	x := []float64{1, math.E, math.E * math.E}
	y := []float64{1, math.E, math.E * math.E * math.E}
	fit, err := energy.WeightedLogLogFit(x, y, []float64{1, 1, 1})
	require.NoError(t, err)
	assert.InDelta(t, 1.5, fit.B, 1e-9)
	assert.InDelta(t, math.Exp(-1.0/6.0), fit.A, 1e-9)
	assert.Positive(t, fit.ResidualSD)
	assert.Equal(t, 3, fit.N)
}

func TestWeightedLogLogFit_RejectsInvalidInput(t *testing.T) {
	cases := map[string]struct{ x, y, w []float64 }{
		"too few points":                  {[]float64{1}, []float64{1}, []float64{1}},
		"mismatched lengths":              {[]float64{1, 2}, []float64{1}, []float64{1, 1}},
		"degenerate (no price variation)": {[]float64{5, 5, 5}, []float64{1, 2, 3}, []float64{1, 1, 1}},
		"non-positive weight":             {[]float64{1, 2}, []float64{1, 2}, []float64{1, 0}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := energy.WeightedLogLogFit(tc.x, tc.y, tc.w)
			assert.Error(t, err)
		})
	}
}

// TestRefitReproducesCommittedFit is the drift guard the kata requires.
func TestRefitReproducesCommittedFit(t *testing.T) {
	rawPoints, err := energy.EmbeddedPointsJSON()
	require.NoError(t, err)
	pointSet, err := energy.DecodePoints(rawPoints)
	require.NoError(t, err)
	committed, err := energy.LoadEmbeddedFitSet()
	require.NoError(t, err)
	recomputed, err := energy.FitPointSet(pointSet.Points, 3)
	require.NoError(t, err)
	assert.Equal(t, energy.PointsHash(rawPoints), committed.PointsHash,
		"run `go run ./internal/energy/cmd/refit` after editing points.json")
	assert.InDelta(t, recomputed.Pooled.A, committed.Pooled.A, 1e-9)
	assert.InDelta(t, recomputed.Pooled.B, committed.Pooled.B, 1e-9)
	assert.InDelta(t, recomputed.Pooled.ResidualSD, committed.Pooled.ResidualSD, 1e-9)
	require.Equal(t, len(recomputed.Families), len(committed.Families))
	for vendor, want := range recomputed.Families {
		got, ok := committed.Families[vendor]
		require.True(t, ok, "committed fit.json is missing family %q", vendor)
		assert.InDelta(t, want.A, got.A, 1e-9)
		assert.InDelta(t, want.B, got.B, 1e-9)
		assert.InDelta(t, want.ResidualSD, got.ResidualSD, 1e-9)
	}
}

// TestSonnetClassEstimateWithinPublishedBounds is the sanity-bound test the kata requires.
func TestSonnetClassEstimateWithinPublishedBounds(t *testing.T) {
	fitSet, err := energy.LoadEmbeddedFitSet()
	require.NoError(t, err)
	const (
		sonnetPriceOutPerMTok = 15.0
		lowerBoundWhPerMTok   = 333.33
		upperBoundWhPerMTok   = 1576.57
	)
	got := fitSet.EOutForModel("anthropic", sonnetPriceOutPerMTok, energy.ScenarioMid)
	assert.Greater(t, got, lowerBoundWhPerMTok)
	assert.Less(t, got, upperBoundWhPerMTok)
}
