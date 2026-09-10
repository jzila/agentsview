package energy_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/agentsview/internal/energy"
	"go.kenn.io/agentsview/internal/export"
)

// TestModelIdentity: Estimate is called with the catalog's matched pattern,
// not the resolved alias -- shared by every storage backend's energy.go.
func TestModelIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, pricedModel, want string
		lookup                  export.PricingLookup
	}{
		{"pattern differs from resolved model, pattern wins", "claude-sonnet-4.6", "claude-sonnet-4-6", export.PricingLookup{Pattern: "claude-sonnet-4-6", OK: true}},
		{"no catalog match, falls back to priced model", "some-unpriced-model", "some-unpriced-model", export.PricingLookup{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, energy.ModelIdentity(tc.pricedModel, tc.lookup))
		})
	}
}

// TestBillableOutputTokens: output_tokens==0 with reasoning_tokens>0 must
// estimate energy on the reasoning tokens -- shared by every storage
// backend's energy.go.
func TestBillableOutputTokens(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		outputTokens, reasoningTokens int64
		want                          int64
	}{
		{"reasoning-only row falls back to reasoning tokens", 0, 750, 750},
		{"normal row ignores reasoning, uses output", 1200, 750, 1200},
		{"no output and no reasoning stays zero", 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, energy.BillableOutputTokens(tc.outputTokens, tc.reasoningTokens))
		})
	}
}
