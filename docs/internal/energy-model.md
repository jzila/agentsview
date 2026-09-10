# Energy (Wh) Estimation Model

AgentsView's Usage surfaces can show an estimated energy dimension (watt-hours)
beside cost and tokens. There is no vendor-published number for "Wh per token,"
so the estimator uses each model's own list price as a proxy for how energy-
intensive it is to serve, anchored to the handful of published energy
measurements that do exist. This is an order-of-magnitude estimate: expect
roughly 2x uncertainty either way, and treat it as directional, not a utility
bill.

The estimator is code plus a committed calibration dataset, not a hand-picked
constant. It has three reproducible, tested steps, implemented in
`internal/energy`.

## Why pricing as a proxy

List price already tracks serving cost for each token type. For Anthropic:
cache read = 0.1x input, cache write = 1.25x input, output = 5x input. OpenAI
has similar ratios (cache read 0.1x-0.25x, output 4x-8x). Two independent
third-party Claude energy estimators (the Claude Code energy gist and
claude-carbon) already use exactly these price ratios as their energy weights,
which is the load-bearing assumption behind this whole approach: price ratio
approximates energy ratio well enough to be useful, even though price also
folds in margin and demand that energy does not.

## Step 1: calibration dataset

`internal/energy/calibration/points.json` has one row per published
measurement (or per third-party model that already derived Wh/MTok from price
ratios). Every row is normalized to Wh per million OUTPUT tokens at full-stack
scope by `Point.EOutWhPerMTok()`:

1. Input tokens are converted to output-token equivalents using the row's own
   price ratio (`tokens_in * price_in/price_out`), so a measurement made with a
   long prompt does not inflate the resulting Wh/MTok-output figure.
2. The measured Wh for the query is divided by the output-equivalent token
   count and scaled to one million tokens.
3. Active-accelerator-only measurements (GPU/TPU compute time only, no data
   center overhead) are multiplied by `FullStackFactor = 2.4`, the ratio
   Google measured between full-stack and active-accelerator-only energy for
   the median Gemini Apps text prompt (0.24 Wh vs 0.10 Wh). GPU-only unbatched
   hardware benchmarks (batch size 1) have no such established conversion
   factor, so they are left as measured and instead given a low `weight` --
   they systematically overstate production (batched) serving cost by an
   unknown amount.

Rows whose method bills wall-clock energy on a small fixed-batch node (the "How
Hungry is AI" benchmark) are also kept at low weight: useful for the *shape*
of energy across models, not the absolute level.

<!-- energy-refit:points-table:start -->
| id | model | vendor | scope | method | $/MTok out | Wh/MTok out (normalized) | weight | source |
|---|---|---|---|---|---|---|---|---|
| gpt4o-epoch-2025 | gpt-4o | openai | full_stack | measured | 10 | 600 | 0.6 | [2025-01-01](https://epoch.ai/gradient-updates/how-much-energy-does-chatgpt-use) |
| gemini-apps-median-2025 | gemini-2.5-flash | google | full_stack | measured | 2.5 | 480 | 0.3 | [2025-08-01](https://arxiv.org/abs/2508.15734) |
| joule-frontier-median-2026 | frontier-median | (pooled only) | full_stack | measured | 15 | 447 | 0.8 | [2026-04-01](https://www.cell.com/joule/fulltext/S2542-4351(26)00114-5) |
| claude-sonnet-gist-2026 | claude-sonnet-class | anthropic | full_stack | modeled | 15 | 1950 | 1 | [2026-03-01](https://gist.github.com/mdodkins/9b49624855cc41570c9d1012e0d5d157) |
| claude-sonnet-carbon | claude-sonnet-class | anthropic | full_stack | modeled | 15 | 333.3 | 0.6 | [2026-01-01](https://github.com/metztim/claude-carbon/blob/main/METHODOLOGY.md) |
| claude-sonnet-hha-short | claude-3-7-sonnet | anthropic | full_stack | measured | 15 | 2612 | 0.15 | [2025-05-01](https://arxiv.org/html/2505.09598v2) |
| claude-sonnet-hha-medium | claude-3-7-sonnet | anthropic | full_stack | measured | 15 | 2318 | 0.15 | [2025-05-01](https://arxiv.org/html/2505.09598v2) |
| claude-sonnet-hha-long | claude-3-7-sonnet | anthropic | full_stack | measured | 15 | 1577 | 0.15 | [2025-05-01](https://arxiv.org/html/2505.09598v2) |
| llama3-70b-tokenpowerbench | llama-3-70b | meta | gpu_only_unbatched | measured | 0.79 | 108.3 | 0.08 | [2025-12-01](https://arxiv.org/abs/2512.03024) |
| gpt-oss-120b-energyscore | gpt-oss-120b | openai | gpu_only_unbatched | measured | 0.45 | 4.25e+04 | 0.03 | [2025-09-01](https://huggingface.co/blog/sasha/ai-energy-score-v2) |

<!-- energy-refit:points-table:end -->

## Step 2: the price-to-energy fit

The calibration points are fit in log space, weighted by each row's `weight`:

```
log(E_out) = log(a) + b * log(price_out)
```

so `E_out(price_out) = a * price_out^b` in Wh per million output tokens.
`b` is expected well below 1: list prices include margin and demand and rise
faster than the underlying compute cost (Opus is 5x Sonnet's price and is not
5x the energy). `internal/energy/fit.go` implements the weighted least squares
fit directly (no external dependency) and reports `a`, `b`, the weighted
residual standard deviation of the log-residuals, and every row's own
residual.

A second fit is made per vendor family (Anthropic, OpenAI, ...) whenever that
vendor has at least three calibration rows; a model from a vendor with its own
family fit uses that fit, otherwise it falls back to the pooled fit across
every vendor.

The fit is committed to `internal/energy/calibration/fit.json` together with
the SHA-256 of the exact `points.json` bytes it was fitted from
(`points_hash`), so a stale fit (points.json edited without regenerating
fit.json) is detectable. Regenerate both with:

```sh
go run ./internal/energy/cmd/refit
```

(or `make energy-refit`). `internal/energy/fit_test.go` asserts that refitting
the committed points.json reproduces the committed fit.json to within 1e-9,
and that the fitted Sonnet-class estimate lands between the claude-carbon
lower bound (333 Wh/MTok) and the "How Hungry is AI" long-prompt upper bound,
so a bad edit to either file fails CI instead of silently drifting.

<!-- energy-refit:fit:start -->
Points hash: `sha256:3f7fc380e03793b1abf0b6dcd3b81c6d84eb6ca401f64ee908b0739781989b63`. Generated: 2026-09-10T19:36:28Z.

Pooled fit: a=470.494, b=0.216602, weighted residual sd=0.853249 (n=10).

Per-row residuals (log(actual) - log(fitted), pooled fit):

| id | actual Wh/MTok | fitted Wh/MTok | log residual |
|---|---|---|---|
| claude-sonnet-carbon | 333.3 | 845.9 | -0.9312 |
| claude-sonnet-gist-2026 | 1950 | 845.9 | +0.8352 |
| claude-sonnet-hha-long | 1577 | 845.9 | +0.6227 |
| claude-sonnet-hha-medium | 2318 | 845.9 | +1.008 |
| claude-sonnet-hha-short | 2612 | 845.9 | +1.128 |
| gemini-apps-median-2025 | 480 | 573.8 | -0.1785 |
| gpt-oss-120b-energyscore | 4.25e+04 | 395.8 | +4.676 |
| gpt4o-epoch-2025 | 600 | 774.7 | -0.2556 |
| joule-frontier-median-2026 | 447 | 845.9 | -0.6378 |
| llama3-70b-tokenpowerbench | 108.3 | 447.1 | -1.418 |

<!-- energy-refit:fit:end -->

### Scenarios

Low and high scenarios come from the fit's own residual spread, not a
hand-picked multiplier: `low = mid * exp(-sd)` and `high = mid * exp(+sd)`,
where `sd` is the fit's weighted residual standard deviation in log space and
`mid` is the raw fitted value. `mid` is the default; set `[energy].scenario`
in `config.toml` to `low` or `high` to change which one the app reports, or
change it from Settings > Preferences > Energy estimate, which reads and
writes the same setting through `GET`/`POST /api/v1/config/energy` and
applies it to the running server immediately (no restart required).

## Step 3: applying the fit to a usage row

For a row (or an aggregated bucket of rows sharing a priced model) with token
counts by type -- input, output, cache write, cache read -- the estimate is:

```
E_wh = E_out(M) * sum_t( tokens_t * price_t(M) / price_out(M) ) / 1e6
```

using the model's own current list prices from `internal/pricing` for the
per-type weights. `internal/energy.Estimator.Estimate` computes this directly
in micro-Wh (`E_out(M) * outputEquivalentTokens`, no intermediate rounding),
mirroring the codebase's microdollars convention for cost. When the pricing
catalog does not publish a cache-write or cache-read rate for a model, the
estimator falls back to 1.25x input and 0.1x input respectively -- the same
ratios most cache-priced catalog entries already use.

Models with no catalog output rate get `energy_status: "no_rate"` and no
estimate (cost is unavailable for the same models, for the same reason). A
per-model override in `config.toml` (`[energy.overrides]`) replaces the fitted
`E_out` for that model and reports `energy_status: "override"`.

### Reasoning tokens are already inside output

Both Anthropic (`thinking`) and OpenAI (`reasoning`) bill reasoning tokens as
output tokens: their `output_tokens` counters already include them, and
`usage_events.reasoning_tokens` is preserved only as an informational subset
(see `internal/usagefacts/fact.go`), never one of the four normalized token
counters. So reasoning energy is already inside the output term above --
`internal/energy.Tokens` has no fifth "reasoning" field, and adding one would
double-count. This is verified per provider in
[`docs/internal/session-format-sources.md`](./session-format-sources.md); if
any currently-supported provider is later found to report reasoning tokens
*outside* `output_tokens`, that provider's entry there must say so and this
estimator must add those tokens to the output term for that provider only.

### What is excluded

Web search requests and other non-token billables (for example Anthropic's
per-request web search fee) are excluded from the energy estimate entirely --
there is no published per-request energy figure for them, and they are a small
fraction of total tokens for sessions that use them.

### Cache format exception

Energy is computed at read time from token counts `usage_daily_rollups`
already stores, with one disclosed exception:
`energy_billable_output_tokens` (`usageCacheFormatVersion` 11 -> 12). A
reasoning-only fact (`output_tokens == 0`, `reasoning_tokens > 0`) needs the
same output-or-reasoning fallback the cost pipeline already applies
(`energy.BillableOutputTokens`), but that fallback must run per fact, before
facts merge into a rollup row -- recomputing it from the merged
`output_tokens`/`reasoning_tokens` sums afterward cannot recover which fact
contributed which, so a reasoning-only fact merged with an ordinary-output
fact would silently lose its own energy contribution. This is a correctness
requirement, not a convenience, so the column stays. The format bump forces a
one-time cold-cache rebuild (re-derive facts, re-aggregate rollups) on each
archive's next usage read after upgrading; large archives can see this take
several seconds to low minutes on that first read, with no user action
required and no effect on the SQLite archive itself.

## Worked examples

**Claude Sonnet-class model** ($3/$15 per MTok in/out, no published cache
rate override): for 10,000 input tokens, 2,000 output tokens, 500 cache-write
tokens, and 50,000 cache-read tokens, the effective rate ratios are input
0.2x, cache write 0.25x (1.25/15, using the fallback since Anthropic's actual
cache-write rate is $3.75/MTok = 0.25x), cache read 0.02x. Output-equivalent
tokens = 2,000 + 10,000(0.2) + 500(0.25) + 50,000(0.02) = 5,125. At the
Anthropic family fit's mid scenario for $15/MTok out, multiply by that
E_out figure (run `agentsview usage energy-model <model>` for the exact
per-model number, or use the fit summary above) to get micro-Wh.

**An OpenAI GPT-5-class model** ($1.25/$10 per MTok in/out, cache read
$0.125/MTok): ratios are input 0.125x, cache read 0.0125x, cache write falls
back to 1.25x input = 0.15625x (no published cache-write rate for this tier).
For the same token mix, output-equivalent tokens = 2,000 + 10,000(0.125) +
500(0.15625) + 50,000(0.0125) = 4,000. Since OpenAI does not yet have three
calibration rows, this uses the pooled fit rather than an OpenAI family fit
(see the fit summary above).

## Sources

- Google, "Measuring the environmental impact of delivering AI at Google
  scale" -- <https://arxiv.org/abs/2508.15734>
- Epoch AI, "How much energy does ChatGPT use?" --
  <https://epoch.ai/gradient-updates/how-much-energy-does-chatgpt-use>
- Joule, Apr 2026 -- <https://www.cell.com/joule/fulltext/S2542-4351(26)00114-5>
- "How Hungry is AI?" v2 -- <https://arxiv.org/html/2505.09598v2>
- "TokenPowerBench" -- <https://arxiv.org/abs/2512.03024>
- Claude Code energy gist --
  <https://gist.github.com/mdodkins/9b49624855cc41570c9d1012e0d5d157>
- claude-carbon methodology --
  <https://github.com/metztim/claude-carbon/blob/main/METHODOLOGY.md>
- Hugging Face, "AI Energy Score v2" --
  <https://huggingface.co/blog/sasha/ai-energy-score-v2>
- "French AI startup discloses full lifecycle consumption and emissions for
  Mistral Large 2" --
  <https://www.deeplearning.ai/the-batch/french-ai-startup-discloses-full-lifecycle-consumption-and-emissions-for-mistral-large-2>
  (background reading; not currently a calibration row)

## Out of scope

Carbon (gCO2e) and water are not estimated here. Either would multiply this
energy estimate by a grid carbon-intensity factor and a site water-usage
factor, both of which vary by region and time in ways this estimator does not
model; that belongs in a follow-up once energy itself ships.
