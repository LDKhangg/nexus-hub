package llm

import (
	"github.com/LDKhangg/nexus-hub/internal/domain"
)

// modelPrice is a model's list price in USD per million tokens.
type modelPrice struct {
	Input, Output, CacheRead, CacheWrite float64
}

// modelPrices is the hub's only price table (Phase 12 spend ledger). The
// values are ESTIMATES from Anthropic's public first-party list prices
// (cached 2026-11): cache writes at 1.25x input (5-minute TTL), cache reads
// at the listed rate (Haiku: 10% of input). Long-context surcharges,
// batch discounts and fast mode are ignored. Update here when prices change;
// nothing else in the hub holds a price.
var modelPrices = map[string]modelPrice{
	"claude-opus-5-5":   {Input: 4.00, Output: 20.00, CacheRead: 0.20, CacheWrite: 5.00},
	"claude-sonnet-5-5": {Input: 2.00, Output: 10.00, CacheRead: 0.10, CacheWrite: 2.50},
	"claude-haiku-5-5":  {Input: 0.10, Output: 0.50, CacheRead: 0.01, CacheWrite: 0.125},
}

// KnownPricedModel reports whether model has a price in the table. Metrics
// use it to keep the model label a closed set.
func KnownPricedModel(model string) bool {
	_, ok := modelPrices[model]
	return ok
}

// EstimateCostUSD is the estimated cost of usage on model. An unknown
// model costs 0 (ok is false); its tokens are still counted by the caller.
func EstimateCostUSD(model string, u domain.Usage) (cost float64, ok bool) {
	p, ok := modelPrices[model]
	if !ok {
		return 0, false
	}
	const perMillion = 1_000_000
	cost = (float64(max(u.Input, 0))*p.Input +
		float64(max(u.Output, 0))*p.Output +
		float64(max(u.CacheRead, 0))*p.CacheRead +
		float64(max(u.CacheWrite, 0))*p.CacheWrite) / perMillion
	return cost, true
}
