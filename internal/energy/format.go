package energy

import (
	"math"
	"strconv"
	"strings"
)

// FormatMicroWh renders a micro-Wh (1e-6 Wh) estimate as a human string at
// two significant figures, scaling the unit so the number stays readable:
// milliwatt-hours below 1 Wh, watt-hours up to 999, kilowatt-hours above
// that, and megawatt-hours above 999 kWh. The frontend's formatEnergy
// (frontend/src/lib/utils/energy.ts) follows the same thresholds so the CLI
// and the UI never disagree about which unit a number is in.
func FormatMicroWh(microWh int64) string {
	wh := float64(microWh) / 1_000_000
	abs := wh
	if abs < 0 {
		abs = -abs
	}
	switch {
	case abs == 0:
		return "0 Wh"
	case abs < 1:
		return formatSignificant(wh*1000, 2) + " mWh"
	case abs < 1_000:
		return formatSignificant(wh, 2) + " Wh"
	case abs < 1_000_000:
		return formatSignificant(wh/1_000, 2) + " kWh"
	default:
		return formatSignificant(wh/1_000_000, 2) + " MWh"
	}
}

// formatSignificant formats v to sig significant digits in plain decimal
// notation (never Go's %g scientific form, which is unreadable in a table
// of everyday Wh figures), trimming trailing fractional zeros. Rounding can
// carry into a higher magnitude (999 at 2 sig figs is 1000, not 990), so
// decimal places are computed from the *rounded* value's own magnitude,
// not the input's.
func formatSignificant(v float64, sig int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	magnitude := math.Floor(math.Log10(v))
	scale := math.Pow(10, float64(sig-1)-magnitude)
	rounded := math.Round(v*scale) / scale

	roundedMagnitude := int(math.Floor(math.Log10(rounded)))
	decimals := max(sig-1-roundedMagnitude, 0)
	s := strconv.FormatFloat(rounded, 'f', decimals, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimRight(s, ".")
	}
	if neg {
		s = "-" + s
	}
	return s
}
