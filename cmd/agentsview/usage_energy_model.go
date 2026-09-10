package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"math"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/energy"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/pricing"
)

// EnergyModelConfig holds `usage energy-model`'s flags.
type EnergyModelConfig struct {
	JSON    bool
	Model   string
	Offline bool
}

func newUsageEnergyModelCommand() *cobra.Command {
	var cfg EnergyModelConfig
	cmd := &cobra.Command{
		Use:   "energy-model [model]",
		Short: "Explain the estimated-energy model, or one model's numbers",
		Long: "Prints the fitted price-to-energy model behind the Usage " +
			"page's energy estimate: a and b, the residual standard " +
			"deviation the low/high scenario band comes from, and the " +
			"effective Wh per million tokens for every catalog model. " +
			"Pass a model name for a single worked example: its resolved " +
			"vendor, which fit it uses, and its per-token-type rates. " +
			"See docs/internal/energy-model.md for the full methodology.",
		SilenceUsage: true,
		Args:         cobra.MaximumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			cfg.JSON = outputFormat(cmd) == "json"
			if len(args) == 1 {
				cfg.Model = args[0]
			}
			runUsageEnergyModel(cmd, cfg)
		},
	}
	registerFormatFlags(cmd.Flags())
	cmd.Flags().BoolVar(&cfg.Offline, "offline", false,
		"Use fallback pricing only, matching `usage daily --offline`, "+
			"instead of the archive's refreshed catalog")
	return cmd
}

// energyModelFitReport is one fit's a/b/residual_sd, in a plain shape for
// JSON or table printing.
type energyModelFitReport struct {
	Vendor     string  `json:"vendor,omitempty"`
	A          float64 `json:"a"`
	B          float64 `json:"b"`
	ResidualSD float64 `json:"residual_sd"`
	N          int     `json:"n"`
}

// energyModelCatalogRow is one catalog model's resolved scenario band, for
// the no-argument summary table.
type energyModelCatalogRow struct {
	Model      string  `json:"model"`
	Vendor     string  `json:"vendor,omitempty"`
	FitUsed    string  `json:"fit_used"`
	PriceOut   float64 `json:"price_out_per_mtok"`
	LowWhMTok  float64 `json:"low_wh_per_mtok"`
	MidWhMTok  float64 `json:"mid_wh_per_mtok"`
	HighWhMTok float64 `json:"high_wh_per_mtok"`
}

// energyModelDetail is a single model's full worked example.
type energyModelDetail struct {
	Model      string  `json:"model"`
	Vendor     string  `json:"vendor,omitempty"`
	FitUsed    string  `json:"fit_used"`
	Status     string  `json:"status"`
	PriceOut   float64 `json:"price_out_per_mtok"`
	LowWhMTok  float64 `json:"low_wh_per_mtok"`
	MidWhMTok  float64 `json:"mid_wh_per_mtok"`
	HighWhMTok float64 `json:"high_wh_per_mtok"`
	// EffectiveWhPerMTok is the Wh/MTok for each token type at the
	// estimator's *currently configured* scenario and per-model override
	// (config.toml's [energy] section), computed the same way
	// energyConfigBodyFromConfig does for the Settings panel's table: one
	// Estimate call per token type at 1,000,000 tokens of that type alone.
	// It intentionally does not always equal LowWhMTok/MidWhMTok/HighWhMTok
	// above, which are the raw fitted band regardless of scenario or
	// override, so a caller can see both what the fit says and what the
	// app will actually report.
	EffectiveWhPerMTok map[string]float64 `json:"effective_wh_per_mtok"`
}

type energyModelReport struct {
	PointsHash string `json:"points_hash"`
	// PricingSource is "live" when Catalog/Detail resolved against the
	// archive's refreshed pricing catalog (see db.DB.EffectivePricingRows),
	// the same source real usage reads use, or "fallback" when --offline
	// was passed or no archive was reachable, in which case rates come from
	// the embedded LiteLLM catalog plus config.toml [custom_model_pricing].
	PricingSource string                  `json:"pricing_source"`
	Pooled        energyModelFitReport    `json:"pooled"`
	Families      []energyModelFitReport  `json:"families,omitempty"`
	Scenario      string                  `json:"scenario"`
	Catalog       []energyModelCatalogRow `json:"catalog,omitempty"`
	Detail        *energyModelDetail      `json:"detail,omitempty"`
}

func runUsageEnergyModel(cmd *cobra.Command, cfg EnergyModelConfig) {
	appCfg := mustLoadConfig(cmd)
	fitSet, err := energy.LoadEmbeddedFitSet()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	scenario := energy.Scenario(appCfg.Energy.Scenario)
	if scenario == "" {
		scenario = energy.ScenarioMid
	}
	estimator := energy.NewEstimator(fitSet, scenario, appCfg.Energy.Overrides)

	rows, resolver, source := energyModelPricingRows(cfg, appCfg)

	report := energyModelReport{
		PointsHash:    fitSet.PointsHash,
		PricingSource: source,
		Pooled: energyModelFitReport{
			A: fitSet.Pooled.A, B: fitSet.Pooled.B,
			ResidualSD: fitSet.Pooled.ResidualSD, N: fitSet.Pooled.N,
		},
		Scenario: string(scenario),
	}
	for _, vendor := range sortedFitVendors(fitSet) {
		f := fitSet.Families[vendor]
		report.Families = append(report.Families, energyModelFitReport{
			Vendor: vendor, A: f.A, B: f.B, ResidualSD: f.ResidualSD, N: f.N,
		})
	}

	if cfg.Model == "" {
		report.Catalog = energyModelCatalogRows(fitSet, effectivePricingRowsToModelPricing(rows))
	} else {
		report.Detail = energyModelDetailFor(cfg.Model, fitSet, estimator, resolver)
	}

	if cfg.JSON {
		enc := jsontext.NewEncoder(os.Stdout, jsontext.WithIndent("  "))
		if err := json.MarshalEncode(enc, report); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}
	printEnergyModelReport(report)
}

// energyModelPricingRows resolves the pricing this diagnostic reports
// against: the archive's currently effective (refreshed) catalog when an
// archive is reachable and cfg.Offline was not passed -- the same source
// db.DB's live usage-read path builds its resolver from -- so a catalog
// refresh cannot make this command explain a different rate than the Usage
// page. cfg.Offline is the explicit, user-requested equivalent of
// `usage daily --offline`'s fallback; the same embedded-catalog fallback
// also applies silently when no archive is reachable (no daemon and no
// local database yet), since a diagnostic command should still explain the
// model rather than fail outright.
func energyModelPricingRows(
	cfg EnergyModelConfig, appCfg config.Config,
) ([]export.EffectivePricingRow, *export.PricingResolver, string) {
	if !cfg.Offline {
		if database, err := openReadOnlyDB(appCfg); err == nil {
			defer database.Close()
			if rows, err := database.EffectivePricingRows(context.Background()); err == nil && len(rows) > 0 {
				return rows, export.NewPricingResolver(rows), "live"
			}
		}
	}
	byPattern := energyModelPricingByPattern(appCfg.CustomModelPricing)
	resolver, rows := energyModelResolver(byPattern, appCfg.CustomModelPricing)
	return rows, resolver, "fallback"
}

func sortedFitVendors(fitSet energy.FitSet) []string {
	vendors := make([]string, 0, len(fitSet.Families))
	for vendor := range fitSet.Families {
		vendors = append(vendors, vendor)
	}
	sort.Strings(vendors)
	return vendors
}

// energyModelPricingByPattern merges config.toml's [custom_model_pricing]
// on top of the embedded LiteLLM fallback catalog, custom pricing winning
// on a shared pattern -- the same precedence
// internal/db.loadPricingMapFrom applies for live usage reads -- so this
// diagnostic reports the same rate, and the same no_rate/override status,
// that the app's actual energy estimates use for a custom-priced or
// custom-overridden model when no archive is reachable (or --offline was
// passed). Without this, a model priced only through
// [custom_model_pricing] (no fallback catalog entry at all) always showed
// as no_rate here despite the API estimating its energy just fine.
func energyModelPricingByPattern(customPricing map[string]config.CustomModelRate) map[string]pricing.ModelPricing {
	byPattern := make(map[string]pricing.ModelPricing, len(customPricing))
	for _, m := range pricing.FallbackPricing() {
		byPattern[m.ModelPattern] = m
	}
	for model, cp := range customPricing {
		byPattern[model] = pricing.ModelPricing{
			ModelPattern:           model,
			InputPerMTok:           money.Money{Microdollars: cp.InputMicrodollarsPerMTok},
			OutputPerMTok:          money.Money{Microdollars: cp.OutputMicrodollarsPerMTok},
			CacheCreationPerMTok:   money.Money{Microdollars: cp.CacheCreationMicrodollarsPerMTok},
			CacheCreation1hPerMTok: money.Money{Microdollars: cp.CacheCreation1hMicrodollarsPerMTok},
			CacheReadPerMTok:       money.Money{Microdollars: cp.CacheReadMicrodollarsPerMTok},
		}
	}
	return byPattern
}

// energyModelResolver builds a real export.PricingResolver from the same
// fallback-plus-custom catalog energyModelPricingByPattern assembles, plus
// the embedded GenAI Prices document (the same one a database-less
// resolver falls back to), for use when no archive is reachable or
// --offline was passed. customPricing must be the same map
// energyModelPricingByPattern merged byPattern from, so custom entries can
// be marked with export.PricingRowSourceCustom: that marking is what gives
// an exact custom-priced model name precedence over canonicalization and
// GenAI in export.PricingResolver.ResolveAt, matching live usage reads
// exactly. It also returns the rows it built, so energyModelPricingRows can
// hand them to the catalog table without reconstructing them.
func energyModelResolver(
	byPattern map[string]pricing.ModelPricing,
	customPricing map[string]config.CustomModelRate,
) (*export.PricingResolver, []export.EffectivePricingRow) {
	rows := make([]export.EffectivePricingRow, 0, len(byPattern)+1)
	for _, m := range byPattern {
		source := export.PricingRowSource("")
		if _, ok := customPricing[m.ModelPattern]; ok {
			source = export.PricingRowSourceCustom
		}
		rows = append(rows, export.EffectivePricingRow{
			ModelPattern: m.ModelPattern,
			Rates: export.ModelRates{
				InputPerMTok:        m.InputPerMTok,
				OutputPerMTok:       m.OutputPerMTok,
				CacheWritePerMTok:   m.CacheCreationPerMTok,
				CacheWrite1hPerMTok: m.CacheCreation1hPerMTok,
				CacheReadPerMTok:    m.CacheReadPerMTok,
				Source:              source,
			},
		})
	}
	genAI := pricing.EmbeddedGenAIDocument()
	rows = append(rows, export.EffectivePricingRow{
		GenAI: genAI.Prices, GenAIVersion: genAI.Version,
		GenAISource: export.PricingRowSourceEmbedded,
	})
	return export.NewPricingResolver(rows), rows
}

// effectivePricingRowsToModelPricing adapts live or fallback pricing rows
// to the plain pricing.ModelPricing map energyModelCatalogRows renders,
// so the catalog table has one rendering path regardless of which source
// the rates came from. Only ModelPattern and OutputPerMTok are read by
// that renderer, so the conversion only carries those two fields; a
// GenAI-only row (empty ModelPattern) converts to a zero-priced entry and
// is skipped there the same way any zero-rate catalog entry always is.
func effectivePricingRowsToModelPricing(rows []export.EffectivePricingRow) map[string]pricing.ModelPricing {
	out := make(map[string]pricing.ModelPricing, len(rows))
	for _, row := range rows {
		if row.ModelPattern == "" {
			continue
		}
		out[row.ModelPattern] = pricing.ModelPricing{
			ModelPattern: row.ModelPattern, OutputPerMTok: row.Rates.OutputPerMTok,
		}
	}
	return out
}

func energyModelCatalogRows(
	fitSet energy.FitSet, byPattern map[string]pricing.ModelPricing,
) []energyModelCatalogRow {
	models := make([]pricing.ModelPricing, 0, len(byPattern))
	for _, m := range byPattern {
		models = append(models, m)
	}
	sort.Slice(models, func(i, j int) bool {
		return models[i].ModelPattern < models[j].ModelPattern
	})
	rows := make([]energyModelCatalogRow, 0, len(models))
	for _, m := range models {
		priceOut := float64(m.OutputPerMTok.Microdollars) / 1_000_000
		if priceOut <= 0 {
			continue
		}
		vendor := energy.VendorForModel(m.ModelPattern)
		fitUsed := "pooled"
		if _, ok := fitSet.Families[vendor]; ok && vendor != "" {
			fitUsed = vendor
		}
		rows = append(rows, energyModelCatalogRow{
			Model: m.ModelPattern, Vendor: vendor, FitUsed: fitUsed,
			PriceOut:   priceOut,
			LowWhMTok:  fitSet.EOutForModel(vendor, priceOut, energy.ScenarioLow),
			MidWhMTok:  fitSet.EOutForModel(vendor, priceOut, energy.ScenarioMid),
			HighWhMTok: fitSet.EOutForModel(vendor, priceOut, energy.ScenarioHigh),
		})
	}
	return rows
}

// energyModelDetailFor resolves model through resolver and estimator via
// energy.ResolveEffectiveRates -- the same shared helper the Settings >
// Preferences energy panel uses (internal/server/huma_routes_config.go) --
// so the CLI diagnostic and the UI cannot silently disagree about what a
// model's number means.
func energyModelDetailFor(
	model string, fitSet energy.FitSet, estimator *energy.Estimator,
	resolver *export.PricingResolver,
) *energyModelDetail {
	er, ok := energy.ResolveEffectiveRates(model, resolver, estimator)
	if !ok {
		return &energyModelDetail{Model: model, Status: "no_rate"}
	}
	effective := make(map[string]float64, len(er.MicroWhPerMTok))
	for tokenType, microWh := range er.MicroWhPerMTok {
		effective[tokenType] = float64(microWh) / 1_000_000
	}
	return &energyModelDetail{
		Model: er.Model, Vendor: er.Vendor, FitUsed: er.FitUsed,
		Status:             string(er.Status),
		PriceOut:           er.PriceOutPerMTok,
		LowWhMTok:          fitSet.EOutForModel(er.Vendor, er.PriceOutPerMTok, energy.ScenarioLow),
		MidWhMTok:          fitSet.EOutForModel(er.Vendor, er.PriceOutPerMTok, energy.ScenarioMid),
		HighWhMTok:         fitSet.EOutForModel(er.Vendor, er.PriceOutPerMTok, energy.ScenarioHigh),
		EffectiveWhPerMTok: effective,
	}
}

// mtokDollars and mtokWh convert this command's plain-float $/MTok and
// Wh/MTok fields to the money.Money and micro-Wh types fmtCost and
// energy.FormatMicroWh expect, so the table below renders through the same
// formatters `usage daily`'s table uses instead of its own %.4g rendering.
func mtokDollars(perMTok float64) money.Money {
	return money.Money{Microdollars: int64(math.Round(perMTok * 1_000_000))}
}

func mtokWh(perMTok float64) int64 {
	return int64(math.Round(perMTok * 1_000_000))
}

func printEnergyModelReport(report energyModelReport) {
	fmt.Printf("points hash: %s\n", report.PointsHash)
	fmt.Printf("scenario: %s\n", report.Scenario)
	fmt.Printf("pricing source: %s\n", report.PricingSource)
	fmt.Printf("pooled fit: a=%.6g b=%.6g residual_sd=%.6g (n=%d)\n",
		report.Pooled.A, report.Pooled.B, report.Pooled.ResidualSD, report.Pooled.N)
	for _, f := range report.Families {
		fmt.Printf("%s family fit: a=%.6g b=%.6g residual_sd=%.6g (n=%d)\n",
			f.Vendor, f.A, f.B, f.ResidualSD, f.N)
	}

	if report.Detail != nil {
		d := report.Detail
		fmt.Println()
		if d.Status == "no_rate" {
			fmt.Printf("%s: no catalog rate, no energy estimate\n", d.Model)
			return
		}
		fmt.Printf("%s (vendor=%s, fit=%s, status=%s)\n", d.Model, d.Vendor, d.FitUsed, d.Status)
		fmt.Printf("  $/MTok out: %s\n", fmtCost(mtokDollars(d.PriceOut)))
		fmt.Printf("  scenario band (Wh/MTok out): low=%s mid=%s high=%s\n",
			energy.FormatMicroWh(mtokWh(d.LowWhMTok)),
			energy.FormatMicroWh(mtokWh(d.MidWhMTok)),
			energy.FormatMicroWh(mtokWh(d.HighWhMTok)))
		fmt.Println("  effective Wh/MTok by token type:")
		for _, tokenType := range []string{"input", "output", "cache_write", "cache_read"} {
			fmt.Printf("    %-12s %s\n", tokenType, energy.FormatMicroWh(mtokWh(d.EffectiveWhPerMTok[tokenType])))
		}
		return
	}

	fmt.Println()
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "MODEL\tVENDOR\tFIT\t$/MTOK OUT\tLOW\tMID\tHIGH")
	for _, row := range report.Catalog {
		vendor := row.Vendor
		if vendor == "" {
			vendor = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			row.Model, vendor, row.FitUsed, fmtCost(mtokDollars(row.PriceOut)),
			energy.FormatMicroWh(mtokWh(row.LowWhMTok)),
			energy.FormatMicroWh(mtokWh(row.MidWhMTok)),
			energy.FormatMicroWh(mtokWh(row.HighWhMTok)))
	}
	w.Flush()
}
