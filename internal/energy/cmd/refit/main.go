// Command refit regenerates internal/energy/calibration/fit.json from
// internal/energy/calibration/points.json, and regenerates the points and
// fit-summary tables in docs/internal/energy-model.md between their
// markers, so neither can drift from the code that produced them.
//
// Run it with `go run ./internal/energy/cmd/refit` (or `make energy-refit`)
// after editing points.json, and commit both the regenerated fit.json and
// the regenerated doc tables.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/energy"
)

// minFamilyRows is the row threshold below which a vendor uses the pooled
// fit instead of its own family fit.
const minFamilyRows = 3

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "refit: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	repoRoot, err := findRepoRoot()
	if err != nil {
		return err
	}
	pointsPath := filepath.Join(repoRoot, "internal/energy/calibration/points.json")
	fitPath := filepath.Join(repoRoot, "internal/energy/calibration/fit.json")
	docPath := filepath.Join(repoRoot, "docs/internal/energy-model.md")

	rawPoints, err := os.ReadFile(pointsPath)
	if err != nil {
		return fmt.Errorf("reading points.json: %w", err)
	}
	pointSet, err := energy.DecodePoints(rawPoints)
	if err != nil {
		return err
	}

	fitSet, err := energy.FitPointSet(pointSet.Points, minFamilyRows)
	if err != nil {
		return fmt.Errorf("fitting points: %w", err)
	}
	fitSet.PointsHash = energy.PointsHash(rawPoints)
	fitSet.GeneratedAt = time.Now().UTC().Format(time.RFC3339)

	rawFit, err := json.MarshalIndent(fitSet, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding fit.json: %w", err)
	}
	rawFit = append(rawFit, '\n')
	if err := os.WriteFile(fitPath, rawFit, 0o644); err != nil {
		return fmt.Errorf("writing fit.json: %w", err)
	}
	fmt.Printf("wrote %s\n", fitPath)
	fmt.Printf("pooled fit: a=%.6g b=%.6g residual_sd=%.6g n=%d\n",
		fitSet.Pooled.A, fitSet.Pooled.B, fitSet.Pooled.ResidualSD, fitSet.Pooled.N)
	for _, vendor := range sortedKeys(fitSet.Families) {
		f := fitSet.Families[vendor]
		fmt.Printf("%s family fit: a=%.6g b=%.6g residual_sd=%.6g n=%d\n",
			vendor, f.A, f.B, f.ResidualSD, f.N)
	}

	if err := regenerateDoc(docPath, pointSet, fitSet); err != nil {
		return fmt.Errorf("regenerating %s: %w", docPath, err)
	}
	fmt.Printf("regenerated tables in %s\n", docPath)
	return nil
}

// findRepoRoot walks up from the working directory to the directory
// containing go.mod, so `go run ./internal/energy/cmd/refit` works from
// any subdirectory of the repo.
func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not find go.mod above %s", dir)
		}
		dir = parent
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

var (
	pointsTableMarkerStart = "<!-- energy-refit:points-table:start -->"
	pointsTableMarkerEnd   = "<!-- energy-refit:points-table:end -->"
	fitMarkerStart         = "<!-- energy-refit:fit:start -->"
	fitMarkerEnd           = "<!-- energy-refit:fit:end -->"
)

func regenerateDoc(path string, points energy.PointSet, fitSet energy.FitSet) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading doc: %w", err)
	}
	doc := string(raw)

	doc, err = replaceBetween(doc, pointsTableMarkerStart, pointsTableMarkerEnd, renderPointsTable(points))
	if err != nil {
		return err
	}
	doc, err = replaceBetween(doc, fitMarkerStart, fitMarkerEnd, renderFitSummary(fitSet))
	if err != nil {
		return err
	}

	return os.WriteFile(path, []byte(doc), 0o644)
}

func replaceBetween(doc, start, end, body string) (string, error) {
	re := regexp.MustCompile(regexp.QuoteMeta(start) + `(?s).*?` + regexp.QuoteMeta(end))
	if !re.MatchString(doc) {
		return "", fmt.Errorf("markers %q / %q not found", start, end)
	}
	replacement := start + "\n" + body + "\n" + end
	return re.ReplaceAllString(doc, replacement), nil
}

func renderPointsTable(points energy.PointSet) string {
	var b strings.Builder
	b.WriteString("| id | model | vendor | scope | method | $/MTok out | Wh/MTok out (normalized) | weight | source |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|\n")
	for _, p := range points.Points {
		eOut, err := p.EOutWhPerMTok()
		eOutStr := "n/a"
		if err == nil {
			eOutStr = fmt.Sprintf("%.4g", eOut)
		}
		vendor := p.Vendor
		if vendor == "" {
			vendor = "(pooled only)"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %.4g | %s | %.2g | [%s](%s) |\n",
			p.ID, p.Model, vendor, p.Scope, p.Method,
			p.PriceOutPerMTok, eOutStr, p.Weight, p.SourceDate, p.SourceURL)
	}
	return b.String()
}

func renderFitSummary(fitSet energy.FitSet) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Points hash: `%s`. Generated: %s.\n\n", fitSet.PointsHash, fitSet.GeneratedAt)
	fmt.Fprintf(&b, "Pooled fit: a=%.6g, b=%.6g, weighted residual sd=%.6g (n=%d).\n\n",
		fitSet.Pooled.A, fitSet.Pooled.B, fitSet.Pooled.ResidualSD, fitSet.Pooled.N)
	if len(fitSet.Families) > 0 {
		b.WriteString("Vendor family fits (used instead of the pooled fit when the model's vendor has one):\n\n")
		b.WriteString("| vendor | a | b | residual sd | n |\n|---|---|---|---|---|\n")
		for _, vendor := range sortedKeys(fitSet.Families) {
			f := fitSet.Families[vendor]
			fmt.Fprintf(&b, "| %s | %.6g | %.6g | %.6g | %d |\n", vendor, f.A, f.B, f.ResidualSD, f.N)
		}
		b.WriteString("\n")
	}
	b.WriteString("Per-row residuals (log(actual) - log(fitted), pooled fit):\n\n")
	b.WriteString("| id | actual Wh/MTok | fitted Wh/MTok | log residual |\n|---|---|---|---|\n")
	sortedResiduals := append([]energy.Residual(nil), fitSet.Residuals...)
	sort.Slice(sortedResiduals, func(i, j int) bool { return sortedResiduals[i].ID < sortedResiduals[j].ID })
	for _, r := range sortedResiduals {
		fmt.Fprintf(&b, "| %s | %.4g | %.4g | %+.4g |\n", r.ID, r.ActualWhMTok, r.FittedWhMTok, r.LogResidual)
	}
	return b.String()
}
