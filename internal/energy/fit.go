// ABOUTME: Weighted log-log least squares fit of Wh/MTok-out against
// ABOUTME: $/MTok-out, plus the fitted-value/scenario math built on it.
package energy

import (
	"errors"
	"fmt"
	"math"
)

// errDegenerateFit means every point shares the same price, so a power-law
// slope cannot be identified.
var errDegenerateFit = errors.New("energy: fit is degenerate, every point has the same price")

// FitResult is the fitted power law E_out(price) = A * price^B in Wh per
// million output tokens, plus the weighted residual standard deviation of
// log(E_out) around that line (used to derive the low/high scenario band)
// and the row count the fit used.
type FitResult struct {
	A          float64 `json:"a"`
	B          float64 `json:"b"`
	ResidualSD float64 `json:"residual_sd"`
	N          int     `json:"n"`
}

// Residual is one point's signed log-space residual against a fit, for
// reporting per-row fit quality.
type Residual struct {
	ID           string  `json:"id"`
	LogResidual  float64 `json:"log_residual"`
	FittedWhMTok float64 `json:"fitted_wh_per_mtok"`
	ActualWhMTok float64 `json:"actual_wh_per_mtok"`
}

// WeightedLogLogFit fits log(y) = log(a) + b*log(x) by weighted least
// squares. x and y must be strictly positive (they are prices and
// energies); w must be positive. It needs at least two points and at least
// two distinct x values (otherwise the line is undetermined).
func WeightedLogLogFit(x, y, w []float64) (FitResult, error) {
	if len(x) != len(y) || len(x) != len(w) {
		return FitResult{}, fmt.Errorf(
			"energy: fit inputs must have equal length, got x=%d y=%d w=%d",
			len(x), len(y), len(w))
	}
	if len(x) < 2 {
		return FitResult{}, fmt.Errorf(
			"energy: fit needs at least 2 points, got %d", len(x))
	}

	logX := make([]float64, len(x))
	logY := make([]float64, len(y))
	var sumW, sumWX, sumWY float64
	for i := range x {
		if x[i] <= 0 || y[i] <= 0 {
			return FitResult{}, fmt.Errorf(
				"energy: fit requires positive price and energy, got x=%v y=%v at index %d",
				x[i], y[i], i)
		}
		if w[i] <= 0 {
			return FitResult{}, fmt.Errorf(
				"energy: fit requires positive weight, got %v at index %d", w[i], i)
		}
		logX[i] = math.Log(x[i])
		logY[i] = math.Log(y[i])
		sumW += w[i]
		sumWX += w[i] * logX[i]
		sumWY += w[i] * logY[i]
	}
	meanX := sumWX / sumW
	meanY := sumWY / sumW

	var sxy, sxx float64
	for i := range x {
		dx := logX[i] - meanX
		dy := logY[i] - meanY
		sxy += w[i] * dx * dy
		sxx += w[i] * dx * dx
	}
	if sxx == 0 {
		return FitResult{}, errDegenerateFit
	}
	b := sxy / sxx
	logA := meanY - b*meanX

	var sumWResidSq float64
	for i := range x {
		resid := logY[i] - (logA + b*logX[i])
		sumWResidSq += w[i] * resid * resid
	}
	residualSD := math.Sqrt(sumWResidSq / sumW)

	return FitResult{A: math.Exp(logA), B: b, ResidualSD: residualSD, N: len(x)}, nil
}

// Residuals returns each point's log-space residual against the fit, in the
// same order as x/y/id.
func (f FitResult) Residuals(ids []string, x, y []float64) []Residual {
	out := make([]Residual, len(ids))
	for i := range ids {
		fitted := f.A * math.Pow(x[i], f.B)
		out[i] = Residual{
			ID:           ids[i],
			LogResidual:  math.Log(y[i]) - math.Log(fitted),
			FittedWhMTok: fitted,
			ActualWhMTok: y[i],
		}
	}
	return out
}

// EOut returns the fitted Wh per million output tokens for a model priced
// at priceOutPerMTok dollars per million output tokens, at the given
// scenario. Low and high are the fitted (mid) value times exp(-sd) and
// exp(+sd), where sd is the fit's weighted residual standard deviation.
func (f FitResult) EOut(priceOutPerMTok float64, scenario Scenario) float64 {
	mid := f.A * math.Pow(priceOutPerMTok, f.B)
	switch scenario {
	case ScenarioLow:
		return mid * math.Exp(-f.ResidualSD)
	case ScenarioHigh:
		return mid * math.Exp(f.ResidualSD)
	default:
		return mid
	}
}

// FitPointSet fits the pooled model across every point, plus a per-vendor
// family fit for every vendor with at least minFamilyRows rows. Points with
// an empty Vendor never form or join a family fit; they contribute only to
// the pooled fit.
func FitPointSet(points []Point, minFamilyRows int) (FitSet, error) {
	if len(points) == 0 {
		return FitSet{}, errors.New("energy: cannot fit an empty point set")
	}
	ids := make([]string, 0, len(points))
	x := make([]float64, 0, len(points))
	y := make([]float64, 0, len(points))
	w := make([]float64, 0, len(points))
	byVendor := make(map[string][]int)
	for i, p := range points {
		eOut, err := p.EOutWhPerMTok()
		if err != nil {
			return FitSet{}, err
		}
		ids = append(ids, p.ID)
		x = append(x, p.PriceOutPerMTok)
		y = append(y, eOut)
		w = append(w, p.Weight)
		if p.Vendor != "" {
			byVendor[p.Vendor] = append(byVendor[p.Vendor], i)
		}
	}

	pooled, err := WeightedLogLogFit(x, y, w)
	if err != nil {
		return FitSet{}, fmt.Errorf("energy: pooled fit: %w", err)
	}

	families := make(map[string]FitResult)
	for vendor, idx := range byVendor {
		if len(idx) < minFamilyRows {
			continue
		}
		vx := make([]float64, len(idx))
		vy := make([]float64, len(idx))
		vw := make([]float64, len(idx))
		for j, i := range idx {
			vx[j], vy[j], vw[j] = x[i], y[i], w[i]
		}
		fit, err := WeightedLogLogFit(vx, vy, vw)
		if err != nil {
			// A vendor whose rows all share one list price (no price
			// variation to identify a slope from) cannot support its own
			// family fit. Fall back to the pooled fit for that vendor
			// instead of failing the whole refit; a future row at a
			// different price makes the family fit possible again.
			if errors.Is(err, errDegenerateFit) {
				continue
			}
			return FitSet{}, fmt.Errorf("energy: %s family fit: %w", vendor, err)
		}
		families[vendor] = fit
	}

	return FitSet{
		Pooled:    pooled,
		Families:  families,
		Residuals: pooled.Residuals(ids, x, y),
	}, nil
}
