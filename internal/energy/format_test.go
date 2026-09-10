package energy_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/agentsview/internal/energy"
)

// TestFormatMicroWh_UnitBoundaries checks the mWh/Wh/kWh/MWh threshold
// crossings and that values render as plain decimals (a %g verb would
// print values like 400 as "4e+02", which is what regressed here).
func TestFormatMicroWh_UnitBoundaries(t *testing.T) {
	cases := []struct {
		name    string
		microWh int64
		want    string
	}{
		{"zero", 0, "0 Wh"},
		{"small mWh", 500, "0.5 mWh"},
		{"just under 1 Wh, rounds up within its own unit", 999_000, "1000 mWh"},
		{"exactly 1 Wh", 1_000_000, "1 Wh"},
		{"three-figure Wh, no scientific notation", 400_000_000, "400 Wh"},
		{"just under 1 kWh, rounds up within its own unit", 999_000_000, "1000 Wh"},
		{"exactly 1 kWh", 1_000_000_000, "1 kWh"},
		{"just under 1 MWh, rounds up within its own unit", 999_000_000_000, "1000 kWh"},
		{"exactly 1 MWh", 1_000_000_000_000, "1 MWh"},
		{"negative value keeps its sign", -400_000_000, "-400 Wh"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, energy.FormatMicroWh(tc.microWh))
		})
	}
}
