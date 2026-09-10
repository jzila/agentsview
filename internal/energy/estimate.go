// ABOUTME: Package energy estimates Wh energy for a usage row from token
// ABOUTME: counts and list prices, using price as a proxy for serving cost.
package energy

import (
	"embed"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

//go:embed calibration/fit.json
var fitFS embed.FS

// Scenario selects which point on the fitted low/mid/high band an estimate
// reports. The zero value behaves like ScenarioMid.
type Scenario string

const (
	ScenarioLow  Scenario = "low"
	ScenarioMid  Scenario = "mid"
	ScenarioHigh Scenario = "high"
)

// Status mirrors the cost pipeline's cost_status so callers can tell an
// unpriced model from a priced one, and a config.toml override from the fit.
type Status string

const (
	// StatusOK means the fit (pooled or vendor-family) priced this row.
	StatusOK Status = "ok"
	// StatusNoRate means the pricing catalog has no output rate for this
	// model, so cost is also unavailable and no energy estimate is made.
	StatusNoRate Status = "no_rate"
	// StatusOverride means a config.toml per-model override replaced the
	// fitted E_out for this model.
	StatusOverride Status = "override"
)

// CombineStatus folds one more constituent's energy_status string into an
// aggregate (a day, a project, a session, a whole totals row): "no_rate"
// wins over everything else, since it means part of the aggregate's energy
// is missing entirely; "override" wins over "ok", since it means a
// config.toml override applied somewhere in the aggregate; the empty
// string means "no constituent seen yet" and never wins. Callers that
// store status as plain strings (matching cost_status's JSON convention)
// can use this directly without importing Status.
func CombineStatus(existing, next string) string {
	switch {
	case existing == "":
		return next
	case existing == string(StatusNoRate) || next == string(StatusNoRate):
		return string(StatusNoRate)
	case existing == string(StatusOverride) || next == string(StatusOverride):
		return string(StatusOverride)
	default:
		return string(StatusOK)
	}
}

// Rates is the subset of a model's list price the estimator needs, in
// dollars per million tokens. It mirrors export.ModelRates' four token-type
// rates; callers convert from export.ModelRates (or any other pricing
// source) at the call site so this package stays free of that dependency.
type Rates struct {
	InputPerMTok      float64
	OutputPerMTok     float64
	CacheWritePerMTok float64
	CacheReadPerMTok  float64
}

// Tokens is one row's (or one aggregated bucket's) token counts by type --
// exactly the four counters usage_daily_rollups already stores per
// (day, priced_model). ReasoningTokens is deliberately absent: for every
// provider currently supported, reasoning/thinking tokens are already
// included in Output (see docs/internal/session-format-sources.md), so
// counting them again here would double-count energy.
type Tokens struct {
	Input      int64
	Output     int64
	CacheWrite int64
	CacheRead  int64
}

// cacheWriteFallbackRatio and cacheReadFallbackRatio price a model's cache
// tokens relative to its input rate when the catalog does not publish a
// cache rate for it, matching the ratios the cost pipeline's Anthropic-style
// pricing already uses when a cache rate is present (cache write 1.25x
// input, cache read 0.1x input).
const (
	cacheWriteFallbackRatio = 1.25
	cacheReadFallbackRatio  = 0.10
)

// FitSet is the committed, versioned output of the refit command: the
// pooled fit across every calibration point, a per-vendor family fit for
// vendors with enough rows, and the pooled fit's per-row residuals.
type FitSet struct {
	PointsHash  string               `json:"points_hash"`
	GeneratedAt string               `json:"generated_at"`
	Pooled      FitResult            `json:"pooled"`
	Families    map[string]FitResult `json:"families"`
	Residuals   []Residual           `json:"residuals"`
}

// EOutForModel returns the fitted Wh/MTok-out for a model: its vendor's
// family fit when one exists, otherwise the pooled fit.
func (fs FitSet) EOutForModel(vendor string, priceOutPerMTok float64, scenario Scenario) float64 {
	fit := fs.Pooled
	if vendor != "" {
		if family, ok := fs.Families[vendor]; ok {
			fit = family
		}
	}
	return fit.EOut(priceOutPerMTok, scenario)
}

// DecodeFitSet parses a fit.json document.
func DecodeFitSet(raw []byte) (FitSet, error) {
	var fs FitSet
	if err := json.Unmarshal(raw, &fs); err != nil {
		return FitSet{}, fmt.Errorf("energy: decoding fit.json: %w", err)
	}
	return fs, nil
}

// EmbeddedFitJSON returns the raw bytes of the committed fit.json.
func EmbeddedFitJSON() ([]byte, error) {
	raw, err := fitFS.ReadFile("calibration/fit.json")
	if err != nil {
		return nil, fmt.Errorf("energy: reading embedded fit.json: %w", err)
	}
	return raw, nil
}

// LoadEmbeddedFitSet decodes the committed calibration/fit.json.
func LoadEmbeddedFitSet() (FitSet, error) {
	raw, err := EmbeddedFitJSON()
	if err != nil {
		return FitSet{}, err
	}
	return DecodeFitSet(raw)
}

// VendorForModel guesses a model's pricing vendor from its priced-model
// pattern, for family-fit selection. An empty result means "use the pooled
// fit"; it is not an error.
func VendorForModel(model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.HasPrefix(m, "claude-"):
		return "anthropic"
	case strings.HasPrefix(m, "gpt-"), strings.HasPrefix(m, "o1"),
		strings.HasPrefix(m, "o3"), strings.HasPrefix(m, "o4"),
		strings.Contains(m, "codex"):
		return "openai"
	case strings.HasPrefix(m, "gemini-"):
		return "google"
	case strings.HasPrefix(m, "mistral"):
		return "mistral"
	case strings.HasPrefix(m, "llama"), strings.HasPrefix(m, "meta-llama"):
		return "meta"
	case strings.HasPrefix(m, "deepseek"):
		return "deepseek"
	case strings.HasPrefix(m, "grok"):
		return "xai"
	default:
		return ""
	}
}

// Estimator computes energy estimates from a fit, a scenario, and
// per-model overrides. It holds no I/O and no global state: the caller
// loads the fit (LoadEmbeddedFitSet) and any config.toml overrides once
// and injects them here.
type Estimator struct {
	Fit       FitSet
	Scenario  Scenario
	Overrides map[string]float64 // priced-model pattern -> Wh/MTok-out override
}

// NewEstimator builds an Estimator. An empty scenario behaves as
// ScenarioMid; a nil overrides map behaves as no overrides.
func NewEstimator(fit FitSet, scenario Scenario, overrides map[string]float64) *Estimator {
	if scenario == "" {
		scenario = ScenarioMid
	}
	return &Estimator{Fit: fit, Scenario: scenario, Overrides: overrides}
}

// Estimate returns the energy estimate, in micro-Wh (1e-6 Wh, matching the
// codebase's microdollars convention), for one model's token usage at list
// price, and a status mirroring the cost pipeline's cost_status.
//
//	E_wh = E_out(M) * sum_t( tokens_t * price_t(M) / price_out(M) ) / 1e6
//
// which rearranges to microWh = E_out(M) * outputEquivalentTokens exactly
// (Wh/MTok times tokens, divided by 1e6 to get Wh, times 1e6 to get
// micro-Wh), so no intermediate rounding to whole Wh ever happens.
//
// Estimate is monotonic in every token count (each term of
// outputEquivalentTokens is a nonnegative count times a nonnegative rate
// ratio) and is exactly zero when every token count is zero.
func (e *Estimator) Estimate(model string, rates Rates, tokens Tokens) (int64, Status) {
	if rates.OutputPerMTok <= 0 {
		return 0, StatusNoRate
	}

	cacheWriteRate := rates.CacheWritePerMTok
	if cacheWriteRate <= 0 {
		cacheWriteRate = rates.InputPerMTok * cacheWriteFallbackRatio
	}
	cacheReadRate := rates.CacheReadPerMTok
	if cacheReadRate <= 0 {
		cacheReadRate = rates.InputPerMTok * cacheReadFallbackRatio
	}

	outputEquiv := float64(tokens.Output) +
		float64(tokens.Input)*ratio(rates.InputPerMTok, rates.OutputPerMTok) +
		float64(tokens.CacheWrite)*ratio(cacheWriteRate, rates.OutputPerMTok) +
		float64(tokens.CacheRead)*ratio(cacheReadRate, rates.OutputPerMTok)

	eOut, status := e.eOutForModel(model, rates.OutputPerMTok)
	microWh := int64(math.Round(eOut * outputEquiv))
	return microWh, status
}

func (e *Estimator) eOutForModel(model string, priceOutPerMTok float64) (float64, Status) {
	if e != nil && e.Overrides != nil {
		if override, ok := e.Overrides[model]; ok {
			return override, StatusOverride
		}
	}
	scenario := ScenarioMid
	var fit FitSet
	if e != nil {
		scenario = e.Scenario
		fit = e.Fit
	}
	return fit.EOutForModel(VendorForModel(model), priceOutPerMTok, scenario), StatusOK
}

func ratio(a, b float64) float64 {
	if b <= 0 {
		return 0
	}
	return a / b
}
