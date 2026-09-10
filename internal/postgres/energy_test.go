package postgres

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/energy"
)

// testPGEnergyEstimator returns the default estimator for tests calling
// internal energy helpers directly. Mirrors internal/db's testEnergyEstimator.
func testPGEnergyEstimator(tb testing.TB) *energy.Estimator {
	tb.Helper()
	estimator, err := (&Store{}).energyEstimator()
	require.NoError(tb, err)
	return estimator
}

// literalEnergyFitEstimator: E_out(price) = price exactly (A=1, B=1), a
// hand-computable fit so a tiered-rate regression's expected micro-Wh is
// literal rather than a positivity check. Mirrors internal/db's helper of
// the same name against the identical resolver rates, so the two backends
// assert the same expected totals for the same fixture.
func literalEnergyFitEstimator() *energy.Estimator {
	return energy.NewEstimator(
		energy.FitSet{Pooled: energy.FitResult{A: 1, B: 1}}, energy.ScenarioMid, nil)
}

// TestPGSessionRowEnergyUsesRequestScopedBand: "banded-model" is
// $1/$2/$0.50/$0.10 (in/out/cache-write/cache-read per MTok) base,
// $2/$3/$1/$0.20 above 200,000 input tokens. At 300,000 input tokens with
// the literal fit: base outputEquiv=150,000, E_out($2)=2 -> 300,000
// microWh; banded outputEquiv=200,000, E_out($3)=3 -> 600,000 microWh.
func TestPGSessionRowEnergyUsesRequestScopedBand(t *testing.T) {
	resolver := pgPricingBandTestResolver()
	estimator := literalEnergyFitEstimator()

	baseMicroWh, baseStatus := pgSessionRowEnergy(pgUsageScanRow{
		usageSource: "session", model: "banded-model", inputTokens: 300_000,
	}, estimator, resolver)
	bandedMicroWh, bandedStatus := pgSessionRowEnergy(pgUsageScanRow{
		usageSource:    "usage-event",
		messageOrdinal: sql.NullInt64{Int64: 1, Valid: true},
		model:          "banded-model", inputTokens: 300_000,
	}, estimator, resolver)
	assert.Equal(t, "ok", baseStatus)
	assert.Equal(t, "ok", bandedStatus)
	assert.Equal(t, int64(300_000), baseMicroWh)
	assert.Equal(t, int64(600_000), bandedMicroWh)
}
