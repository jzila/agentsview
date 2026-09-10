package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/energy"
)

// TestEnergyModelDetailForResolvesLikeRealUsage: --model resolves the same way real usage resolves a reported model name.
func TestEnergyModelDetailForResolvesLikeRealUsage(t *testing.T) {
	fitSet, err := energy.LoadEmbeddedFitSet()
	require.NoError(t, err)
	estimator := energy.NewEstimator(fitSet, energy.ScenarioMid, nil)

	t.Run("a custom-only model resolves through custom pricing", func(t *testing.T) {
		const customModel = "my-private-deployment"
		custom := map[string]config.CustomModelRate{
			customModel: {InputMicrodollarsPerMTok: 1_000_000, OutputMicrodollarsPerMTok: 5_000_000},
		}
		resolver, _ := energyModelResolver(energyModelPricingByPattern(custom), custom)

		detail := energyModelDetailFor(customModel, fitSet, estimator, resolver)
		require.NotNil(t, detail)
		assert.Equal(t, "ok", detail.Status)
		assert.Equal(t, 5.0, detail.PriceOut)
	})

	t.Run("a runtime alias resolves to its canonical catalog model", func(t *testing.T) {
		resolver, _ := energyModelResolver(energyModelPricingByPattern(nil), nil)
		detail := energyModelDetailFor("gpt-reserve", fitSet, estimator, resolver)
		require.NotNil(t, detail)
		assert.Equal(t, "ok", detail.Status)
		assert.Equal(t, "gpt-5.6-luna", detail.Model)
	})
}

// TestEnergyModelPricingRowsOfflineUsesFallback: --offline must skip the
// archive entirely and report the embedded fallback catalog, matching
// `usage daily --offline`'s semantics.
func TestEnergyModelPricingRowsOfflineUsesFallback(t *testing.T) {
	rows, resolver, source := energyModelPricingRows(
		EnergyModelConfig{Offline: true}, config.Config{})
	assert.Equal(t, "fallback", source)
	require.NotNil(t, resolver)
	require.NotEmpty(t, rows)
}
