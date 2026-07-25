package agent

// Model identifiers. Exact strings, no date suffixes.
const (
	ModelSonnet5 = "claude-sonnet-5"
	ModelOpus5   = "claude-opus-5"
	ModelHaiku45 = "claude-haiku-4-5"
)

// DefaultModel is Sonnet 5: near-Opus quality on this kind of writing at a
// fraction of the cost. Workspaces that warrant more override it per workspace.
const DefaultModel = ModelSonnet5

// EscalationModel is used on a final retry, so a run starts cheap and only pays
// for the better model when the cheap one has already failed validation.
const EscalationModel = ModelOpus5

// Pricing is per-million-token rates for one model.
//
// The API returns token counts, not dollars, so cost is computed here. Rates are
// a local table and will drift: treat reported cost as an estimate for budgeting
// and reporting, not as a billing record.
type Pricing struct {
	InputPerMTok  float64
	OutputPerMTok float64
}

// modelPricing covers the models kiln uses. Cache reads bill at ~0.1x input and
// 5-minute cache writes at ~1.25x input.
var modelPricing = map[string]Pricing{
	ModelOpus5:   {InputPerMTok: 5.00, OutputPerMTok: 25.00},
	ModelSonnet5: {InputPerMTok: 3.00, OutputPerMTok: 15.00},
	ModelHaiku45: {InputPerMTok: 1.00, OutputPerMTok: 5.00},
}

const (
	cacheReadMultiplier  = 0.10
	cacheWriteMultiplier = 1.25
)

// EstimateCostUSD converts token usage into an approximate dollar cost.
//
// Cached tokens are billed separately from fresh input, which matters here: kiln
// re-sends the same repo map and steering documents for every unit in a run, so
// with caching on, most input after the first unit bills at a tenth of the rate.
func EstimateCostUSD(model string, u Usage) float64 {
	p, ok := modelPricing[model]
	if !ok {
		// An unknown model reports zero rather than guessing. Budget enforcement
		// treats zero as "unknown", not as "free".
		return 0
	}

	const perMillion = 1_000_000.0

	cost := float64(u.InputTokens) / perMillion * p.InputPerMTok
	cost += float64(u.OutputTokens) / perMillion * p.OutputPerMTok
	cost += float64(u.CacheReadInputTokens) / perMillion * p.InputPerMTok * cacheReadMultiplier
	cost += float64(u.CacheCreationInputTokens) / perMillion * p.InputPerMTok * cacheWriteMultiplier

	return cost
}

// KnownModel reports whether cost can be estimated for a model.
func KnownModel(model string) bool {
	_, ok := modelPricing[model]
	return ok
}
