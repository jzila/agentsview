// ABOUTME: Calibration dataset types and loading for the energy estimator:
// ABOUTME: one committed points.json row per published measurement, normalized.
package energy

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

//go:embed calibration/points.json
var calibrationFS embed.FS

// Scope describes what part of the serving stack a measurement covers.
type Scope string

const (
	// ScopeActiveAccelerator covers only the accelerator (GPU/TPU) actively
	// computing the request, excluding idle capacity, networking, storage,
	// and cooling. Rows in this scope are scaled by FullStackFactor.
	ScopeActiveAccelerator Scope = "active_accelerator"
	// ScopeFullStack covers the full data-center serving stack: accelerator,
	// host overhead, networking, cooling, and PUE. This is the estimator's
	// target scope; full_stack rows are used as measured.
	ScopeFullStack Scope = "full_stack"
	// ScopeGPUOnlyUnbatched covers a single GPU running one request at a
	// time (batch size 1), as in vendor-neutral hardware benchmarks. There
	// is no established factor to convert this to full-stack production
	// serving, so these rows are kept only for their weight, not rescaled.
	ScopeGPUOnlyUnbatched Scope = "gpu_only_unbatched"
)

// Method describes how a row's energy figure was obtained.
type Method string

const (
	MethodMeasured Method = "measured"
	MethodModeled  Method = "modeled"
)

// FullStackFactor is the ratio Google measured between full-stack and
// active-accelerator-only energy for the median Gemini Apps text prompt
// (0.24 Wh full stack vs 0.10 Wh TPU-only). Source:
// https://arxiv.org/abs/2508.15734
const FullStackFactor = 2.4

// Point is one calibration row: a published (or modeled-and-published)
// energy measurement for one model, normalized to Wh per million output
// tokens at full-stack scope by EOutWhPerMTok.
type Point struct {
	ID     string `json:"id"`
	Model  string `json:"model"`
	Vendor string `json:"vendor"`
	Date   string `json:"date"`

	// PriceInPerMTok and PriceOutPerMTok are the model's list price per
	// million tokens, in dollars, at the date of the measurement (from the
	// pricing catalog or the source paper).
	PriceInPerMTok  float64 `json:"price_in_per_mtok"`
	PriceOutPerMTok float64 `json:"price_out_per_mtok"`

	// TokensIn and TokensOut are the token counts the measured query (or
	// query class) used. A row that already reports Wh per million output
	// tokens directly sets TokensOut to 1_000_000 and TokensIn to 0, so
	// MeasuredWhPerQuery *is* the Wh/MTok-out figure with no per-query
	// conversion needed.
	TokensIn  int64 `json:"tokens_in"`
	TokensOut int64 `json:"tokens_out"`

	// MeasuredWhPerQuery is the reported energy, in Wh, for one query with
	// the token counts above.
	MeasuredWhPerQuery float64 `json:"measured_wh_per_query"`

	Scope  Scope  `json:"scope"`
	Method Method `json:"method"`

	SourceURL  string `json:"source_url"`
	SourceNote string `json:"source_note"`
	SourceDate string `json:"source_date"`

	Weight float64 `json:"weight"`
}

// EOutWhPerMTok normalizes the row to Wh per million OUTPUT tokens at
// full-stack scope:
//
//  1. input tokens are converted to output-token equivalents using the
//     row's own price ratio, so a long prompt does not inflate the
//     per-output-token figure: outputEquivTokens = TokensOut +
//     TokensIn*(PriceIn/PriceOut).
//  2. the raw Wh/MTok is MeasuredWhPerQuery scaled from outputEquivTokens
//     up to one million tokens.
//  3. active-accelerator-only rows are multiplied by FullStackFactor to
//     estimate full-stack energy; full_stack and gpu_only_unbatched rows
//     are left as measured (gpu_only_unbatched has no known conversion
//     factor, so it relies on Weight instead).
func (p Point) EOutWhPerMTok() (float64, error) {
	if p.PriceOutPerMTok <= 0 {
		return 0, fmt.Errorf("energy: point %q has non-positive price_out_per_mtok", p.ID)
	}
	outputEquivTokens := float64(p.TokensOut)
	if p.TokensIn > 0 {
		outputEquivTokens += float64(p.TokensIn) * (p.PriceInPerMTok / p.PriceOutPerMTok)
	}
	if outputEquivTokens <= 0 {
		return 0, fmt.Errorf("energy: point %q has non-positive output-equivalent tokens", p.ID)
	}
	raw := p.MeasuredWhPerQuery / outputEquivTokens * 1_000_000
	if p.Scope == ScopeActiveAccelerator {
		raw *= FullStackFactor
	}
	return raw, nil
}

// PointSet is the committed calibration dataset.
type PointSet struct {
	SchemaVersion int     `json:"schema_version"`
	Points        []Point `json:"points"`
}

// DecodePoints parses a points.json document.
func DecodePoints(raw []byte) (PointSet, error) {
	var set PointSet
	if err := json.Unmarshal(raw, &set); err != nil {
		return PointSet{}, fmt.Errorf("energy: decoding points.json: %w", err)
	}
	if len(set.Points) == 0 {
		return PointSet{}, fmt.Errorf("energy: points.json has no points")
	}
	return set, nil
}

// EmbeddedPointsJSON returns the raw bytes of the committed points.json, for
// hashing and for tools that must refit from the exact committed input.
func EmbeddedPointsJSON() ([]byte, error) {
	raw, err := calibrationFS.ReadFile("calibration/points.json")
	if err != nil {
		return nil, fmt.Errorf("energy: reading embedded points.json: %w", err)
	}
	return raw, nil
}

// PointsHash returns the hex-encoded SHA-256 of raw points.json bytes, the
// identity fit.json commits to so a refit is caught when points.json
// changes without regenerating the fit.
func PointsHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}
