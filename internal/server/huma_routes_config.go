package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/shlex"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/energy"
	"go.kenn.io/agentsview/internal/export"
)

func (s *Server) registerConfigRoutes() {
	group := newRouteGroup(s.api, "/api/v1/config", "Config")

	s.get(group, "/github", "Get GitHub config", s.humaGetGithubConfig)
	s.post(group, "/github", "Set GitHub config", s.humaSetGithubConfig)
	s.get(group, "/terminal", "Get terminal config", s.humaGetTerminalConfig)
	s.post(group, "/terminal", "Set terminal config", s.humaSetTerminalConfig)
	s.get(group, "/energy", "Get energy estimate config", s.humaGetEnergyConfig)
	s.post(group, "/energy", "Set energy estimate scenario", s.humaSetEnergyConfig)
}

type terminalMode string

const (
	terminalModeAuto      terminalMode = "auto"
	terminalModeCustom    terminalMode = "custom"
	terminalModeClipboard terminalMode = "clipboard"
)

type githubConfigResponse struct {
	Configured bool `json:"configured"`
}

type setGithubConfigInput struct {
	Body struct {
		Token string `json:"token" required:"true" minLength:"1" doc:"GitHub token"`
	}
}

type setGithubConfigResponse struct {
	Success  bool   `json:"success"`
	Username string `json:"username"`
}

type terminalConfigInput struct {
	Body terminalConfigBody
}

type terminalConfigBody struct {
	Mode       terminalMode `json:"mode" enum:"auto,custom,clipboard" doc:"Terminal launch mode"`
	CustomBin  string       `json:"custom_bin,omitempty" doc:"Terminal binary path when mode is custom"`
	CustomArgs string       `json:"custom_args,omitempty" doc:"Argument template containing {cmd} when mode is custom"`
}

func terminalConfigBodyFromConfig(tc config.TerminalConfig) terminalConfigBody {
	mode := terminalMode(tc.Mode)
	if mode == "" {
		mode = terminalModeAuto
	}
	return terminalConfigBody{
		Mode:       mode,
		CustomBin:  tc.CustomBin,
		CustomArgs: tc.CustomArgs,
	}
}

func (b terminalConfigBody) config() config.TerminalConfig {
	return config.TerminalConfig{
		Mode:       string(b.Mode),
		CustomBin:  b.CustomBin,
		CustomArgs: b.CustomArgs,
	}
}

func (s *Server) humaGetGithubConfig(
	ctx context.Context,
	_ *emptyInput,
) (*jsonOutput[githubConfigResponse], error) {
	return &jsonOutput[githubConfigResponse]{
		Body: githubConfigResponse{Configured: s.githubToken(ctx) != ""},
	}, nil
}

func (s *Server) humaSetGithubConfig(
	ctx context.Context,
	in *setGithubConfigInput,
) (*jsonOutput[setGithubConfigResponse], error) {
	token := strings.TrimSpace(in.Body.Token)
	if token == "" {
		return nil, apiError(http.StatusBadRequest, "token required")
	}
	username, err := validateGithubToken(ctx, token)
	if err != nil {
		return nil, apiError(http.StatusUnauthorized, err.Error())
	}
	s.mu.Lock()
	err = s.cfg.SaveGithubToken(token)
	s.mu.Unlock()
	if err != nil {
		return nil, apiError(http.StatusInternalServerError, "failed to save token")
	}
	return &jsonOutput[setGithubConfigResponse]{
		Body: setGithubConfigResponse{Success: true, Username: username},
	}, nil
}

func (s *Server) humaGetTerminalConfig(
	_ context.Context,
	_ *emptyInput,
) (*jsonOutput[terminalConfigBody], error) {
	s.mu.RLock()
	tc := s.cfg.Terminal
	s.mu.RUnlock()
	return &jsonOutput[terminalConfigBody]{
		Body: terminalConfigBodyFromConfig(tc),
	}, nil
}

func (s *Server) humaSetTerminalConfig(
	_ context.Context,
	in *terminalConfigInput,
) (*jsonOutput[terminalConfigBody], error) {
	body := in.Body
	tc := body.config()
	switch terminalMode(tc.Mode) {
	case terminalModeAuto, terminalModeCustom, terminalModeClipboard:
	default:
		return nil, apiError(http.StatusBadRequest,
			`mode must be "auto", "custom", or "clipboard"`)
	}
	if tc.Mode == string(terminalModeCustom) && tc.CustomBin == "" {
		return nil, apiError(http.StatusBadRequest,
			`custom_bin is required when mode is "custom"`)
	}
	if tc.Mode == string(terminalModeCustom) {
		if tc.CustomArgs != "" && !strings.Contains(tc.CustomArgs, "{cmd}") {
			return nil, apiError(http.StatusBadRequest,
				`custom_args must contain the {cmd} placeholder so the resume command is passed to the terminal`)
		}
		if tc.CustomArgs != "" {
			if _, splitErr := shlex.Split(tc.CustomArgs); splitErr != nil {
				return nil, apiError(http.StatusBadRequest,
					fmt.Sprintf("custom_args has invalid shell syntax: %v", splitErr))
			}
		}
	}
	s.mu.Lock()
	err := s.cfg.SaveTerminalConfig(tc)
	tc = s.cfg.Terminal
	s.mu.Unlock()
	if err != nil {
		return nil, internalError("save terminal config", err)
	}
	return &jsonOutput[terminalConfigBody]{
		Body: terminalConfigBodyFromConfig(tc),
	}, nil
}

// energyConfigStore is implemented by every usage-read backend this server
// can wrap (see internal/db.DB.SetEnergyConfig,
// internal/postgres.Store.SetEnergyConfig, and
// internal/duckdb.Store.SetEnergyConfig), so the type assertion below
// always matches in practice; it exists to keep this handler decoupled
// from which concrete db.Store implementation s.db holds.
type energyConfigStore interface {
	SetEnergyConfig(scenario energy.Scenario, overrides map[string]float64)
}

// energyPricingResolverStore is implemented by a usage-read backend that
// can report its currently effective (refreshed) pricing catalog -- see
// db.DB.EffectivePricingRows -- the same source live usage reads resolve
// against. PostgreSQL and DuckDB stores do not implement it yet, so
// energyPricingResolver returns nil for them and the Settings panel's
// effective-rate table falls back to the two representative model
// classes, the same fallback an empty Usage range already uses.
type energyPricingResolverStore interface {
	EffectivePricingRows(ctx context.Context) ([]export.EffectivePricingRow, error)
}

// energyPricingResolver builds a resolver from s.db's live pricing catalog
// when the store supports it, or returns nil so callers fall back to the
// representative-model classes. A nil resolver is not an error: it is the
// documented fallback for a backend without live pricing (or a fetch that
// failed or returned nothing), not a broken request.
func (s *Server) energyPricingResolver(ctx context.Context) *export.PricingResolver {
	store, ok := s.db.(energyPricingResolverStore)
	if !ok {
		return nil
	}
	rows, err := store.EffectivePricingRows(ctx)
	if err != nil || len(rows) == 0 {
		return nil
	}
	return export.NewPricingResolver(rows)
}

// energyRatePerTokenType is one row of energyModelRates.rates: the effective
// energy of one million tokens of a single token type, in micro-Wh, at the
// currently configured scenario.
type energyRatePerTokenType struct {
	TokenType        string `json:"token_type" enum:"input,output,cache_write,cache_read"`
	WhPerMTokMicroWh int64  `json:"wh_per_mtok_micro_wh" doc:"Energy of one million tokens of this type, in micro-Wh (1e-6 Wh)"`
}

// energyModelRates is a representative model's effective per-token-type
// rates, so a Settings viewer can see what the currently selected scenario
// means in practice without reading docs/internal/energy-model.md's full
// per-catalog-model table.
type energyModelRates struct {
	Model                    string                   `json:"model" doc:"Representative model class this row illustrates"`
	ListPriceOutMicrodollars int64                    `json:"list_price_out_microdollars_per_mtok" doc:"The model's list price per million output tokens, in microdollars"`
	Rates                    []energyRatePerTokenType `json:"rates"`
}

type energyConfigBody struct {
	Scenario string `json:"scenario" enum:"low,mid,high" doc:"Which point on the fitted low/mid/high energy band the app reports"`
	// Models lists effective Wh/MTok-per-token-type rates at the current
	// scenario: one row per model named in the request's Models list
	// (deduplicated, capped at energyMaxModelsFromRange, skipping any with
	// no catalog rate) when the caller passed models and this backend can
	// resolve live pricing, or the two representative model classes
	// (Anthropic Sonnet-class, OpenAI GPT-5-class) otherwise.
	Models []energyModelRates `json:"models"`
}

type getEnergyConfigInput struct {
	// Models are the model names currently shown in the caller's Usage
	// range (its summary response's modelTotals), reused here instead of
	// re-deriving a model list server-side so the Settings panel's table
	// matches whatever range and filters the Usage page has selected.
	Models []string `query:"model,explode" doc:"Model names from the current Usage range's summary; omit for the representative-class fallback"`
}

type setEnergyConfigInput struct {
	Body struct {
		Scenario string   `json:"scenario" enum:"low,mid,high" required:"true" doc:"low, mid, or high"`
		Models   []string `json:"models,omitempty" doc:"Same as GET's model query param, so the response after a scenario change still reflects the caller's current Usage range"`
	}
}

// energyRepresentativeModels anchors the Settings panel's effective-rate
// table to the same two worked examples docs/internal/energy-model.md
// walks through: an Anthropic Sonnet-class model and an OpenAI GPT-5-class
// model, at their published list prices (dollars per million tokens).
var energyRepresentativeModels = []struct {
	vendorTag string // only used for family-fit/vendor detection, not billed
	label     string
	rates     energy.Rates
}{
	{
		vendorTag: "claude-sonnet-4-5",
		label:     "claude-sonnet-class",
		rates: energy.Rates{
			InputPerMTok: 3, OutputPerMTok: 15,
			CacheWritePerMTok: 3.75, CacheReadPerMTok: 0.30,
		},
	},
	{
		vendorTag: "gpt-5",
		label:     "gpt-5-class",
		rates:     energy.Rates{InputPerMTok: 1.25, OutputPerMTok: 10, CacheReadPerMTok: 0.125},
	},
}

// energyMaxModelsFromRange caps how many of the caller's Usage-range models
// energyConfigBodyFromConfig resolves per request: each one costs a
// resolver lookup plus four Estimate calls, so this keeps a Settings-panel
// refresh cheap without meaningfully limiting what a user can see -- a
// range's top few models cover the overwhelming majority of its usage in
// practice.
const energyMaxModelsFromRange = 12

// energyConfigBodyFromConfig builds the Settings > Preferences energy
// panel's response: the current scenario, and an effective-rate table
// listing rangeModels (the Usage page's current models, resolved through
// resolver -- the same export.PricingResolver live usage reads use, via
// energy.ResolveEffectiveRates, the shared helper `usage energy-model`
// also uses) when both are available, or the two representative model
// classes otherwise -- an empty range, a backend with no live pricing
// (resolver == nil), or every named model having no catalog rate.
func energyConfigBodyFromConfig(
	ec config.EnergyConfig, resolver *export.PricingResolver, rangeModels []string,
) (energyConfigBody, error) {
	scenario := ec.Scenario
	if scenario == "" {
		scenario = "mid"
	}
	fit, err := energy.LoadEmbeddedFitSet()
	if err != nil {
		return energyConfigBody{}, fmt.Errorf("loading energy fit: %w", err)
	}
	estimator := energy.NewEstimator(fit, energy.Scenario(scenario), ec.Overrides)

	if resolver != nil && len(rangeModels) > 0 {
		if models := energyModelRatesFromRange(rangeModels, resolver, estimator); len(models) > 0 {
			return energyConfigBody{Scenario: scenario, Models: models}, nil
		}
	}
	return energyConfigBody{Scenario: scenario, Models: energyRepresentativeModelRates(estimator)}, nil
}

// energyModelRatesFromRange resolves each of rangeModels (deduplicated,
// capped at energyMaxModelsFromRange, in order) to its effective
// per-token-type energy rate. A model with no catalog rate is skipped,
// matching the rest of the app's no_rate handling: this table only shows
// models it can price.
func energyModelRatesFromRange(
	rangeModels []string, resolver *export.PricingResolver, estimator *energy.Estimator,
) []energyModelRates {
	seen := make(map[string]struct{}, len(rangeModels))
	out := make([]energyModelRates, 0, min(len(rangeModels), energyMaxModelsFromRange))
	for _, model := range rangeModels {
		if model == "" || len(out) >= energyMaxModelsFromRange {
			continue
		}
		if _, dup := seen[model]; dup {
			continue
		}
		seen[model] = struct{}{}
		er, ok := energy.ResolveEffectiveRates(model, resolver, estimator)
		if !ok {
			continue
		}
		out = append(out, energyModelRates{
			Model:                    er.Model,
			ListPriceOutMicrodollars: int64(er.PriceOutPerMTok * 1_000_000),
			Rates:                    energyRatesForTokenTypes(er.MicroWhPerMTok),
		})
	}
	return out
}

// energyRepresentativeModelRates is the fallback effective-rate table:
// docs/internal/energy-model.md's two worked examples (an Anthropic
// Sonnet-class model and an OpenAI GPT-5-class model), at their published
// list prices, for a backend or range that cannot supply real models.
func energyRepresentativeModelRates(estimator *energy.Estimator) []energyModelRates {
	models := make([]energyModelRates, 0, len(energyRepresentativeModels))
	for _, rep := range energyRepresentativeModels {
		rates := make(map[string]int64, 4)
		for _, tt := range energyRateTokenTypeProbes {
			microWh, _ := estimator.Estimate(rep.vendorTag, rep.rates, tt.tokens)
			rates[tt.name] = microWh
		}
		models = append(models, energyModelRates{
			Model:                    rep.label,
			ListPriceOutMicrodollars: int64(rep.rates.OutputPerMTok * 1_000_000),
			Rates:                    energyRatesForTokenTypes(rates),
		})
	}
	return models
}

// energyRateTokenTypeProbes lists the four token types an effective-rate
// row reports, each probed with 1,000,000 tokens of just that type.
var energyRateTokenTypeProbes = []struct {
	name   string
	tokens energy.Tokens
}{
	{"input", energy.Tokens{Input: 1_000_000}},
	{"output", energy.Tokens{Output: 1_000_000}},
	{"cache_write", energy.Tokens{CacheWrite: 1_000_000}},
	{"cache_read", energy.Tokens{CacheRead: 1_000_000}},
}

// energyRatesForTokenTypes renders a model's per-token-type micro-Wh map
// (from either energy.ResolveEffectiveRates or the representative-model
// probe above) as the ordered rows the API response documents.
func energyRatesForTokenTypes(microWhPerMTok map[string]int64) []energyRatePerTokenType {
	rates := make([]energyRatePerTokenType, 0, len(energyRateTokenTypeProbes))
	for _, tt := range energyRateTokenTypeProbes {
		rates = append(rates, energyRatePerTokenType{
			TokenType: tt.name, WhPerMTokMicroWh: microWhPerMTok[tt.name],
		})
	}
	return rates
}

func (s *Server) humaGetEnergyConfig(
	ctx context.Context,
	in *getEnergyConfigInput,
) (*jsonOutput[energyConfigBody], error) {
	s.mu.RLock()
	ec := s.cfg.Energy
	s.mu.RUnlock()
	body, err := energyConfigBodyFromConfig(ec, s.energyPricingResolver(ctx), in.Models)
	if err != nil {
		return nil, internalError("load energy config", err)
	}
	return &jsonOutput[energyConfigBody]{Body: body}, nil
}

func (s *Server) humaSetEnergyConfig(
	ctx context.Context,
	in *setEnergyConfigInput,
) (*jsonOutput[energyConfigBody], error) {
	switch in.Body.Scenario {
	case "low", "mid", "high":
	default:
		return nil, apiError(http.StatusBadRequest, `scenario must be "low", "mid", or "high"`)
	}
	// Energy scenario/overrides live in the application config (config.toml),
	// not the archive store, so this never gates on s.db.ReadOnly(): a
	// DuckDB or PostgreSQL `serve` store is always read-only (it never
	// accepts session writes) but its config.toml, and the energyConfigStore
	// apply below, are not -- matching humaSetTerminalConfig and
	// humaSetGithubConfig, which carry no such gate either.
	//
	// Persisting to disk/memory and installing into the live store happen
	// under the same s.mu critical section so two overlapping requests
	// can't interleave into save-A, save-B, install-B, install-A. Without
	// that, the later save always wins on disk and in GET's response, but
	// whichever install runs last wins in the live store, and those two
	// can end up disagreeing.
	s.mu.Lock()
	err := s.cfg.SaveEnergyConfig(in.Body.Scenario)
	ec := s.cfg.Energy
	if err == nil {
		// Apply immediately to the running store so the change takes effect
		// on the next usage read without a restart.
		if store, ok := s.db.(energyConfigStore); ok {
			store.SetEnergyConfig(energy.Scenario(ec.Scenario), ec.Overrides)
		}
	}
	s.mu.Unlock()
	if err != nil {
		return nil, internalError("save energy config", err)
	}
	body, err := energyConfigBodyFromConfig(ec, s.energyPricingResolver(ctx), in.Body.Models)
	if err != nil {
		return nil, internalError("load energy config", err)
	}
	return &jsonOutput[energyConfigBody]{Body: body}, nil
}
