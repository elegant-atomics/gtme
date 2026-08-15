package ai

import "strings"

// price is a model's list price in USD per million tokens.
type price struct {
	input  float64
	output float64
}

// prices is a small, deliberately-static table used only to attribute cost to a
// run. It is a floor, not an invoice: unknown models report unpriced and the
// receipt shows "?" rather than a wrong number. Cache reads are billed at ~0.1x
// input and cache writes at ~1.25x (5-minute TTL).
var prices = map[string]price{
	"claude-opus-5":     {5, 25},
	"claude-opus-4-8":   {5, 25},
	"claude-opus-4-7":   {5, 25},
	"claude-opus-4-6":   {5, 25},
	"claude-sonnet-5":   {3, 15},
	"claude-sonnet-4-6": {3, 15},
	"claude-sonnet-4-5": {3, 15},
	"claude-haiku-4-5":  {1, 5},
	"claude-fable-5":    {10, 50},
}

// Price estimates the cost of one call. The second return reports whether the
// model was priceable at all.
func Price(model string, inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens int) (float64, bool) {
	p, ok := lookupPrice(model)
	if !ok {
		return 0, false
	}
	const perToken = 1_000_000.0
	cost := float64(inputTokens)*p.input/perToken +
		float64(outputTokens)*p.output/perToken +
		float64(cacheReadTokens)*p.input*0.1/perToken +
		float64(cacheWriteTokens)*p.input*1.25/perToken
	return cost, true
}

// lookupPrice matches a model id exactly, then by prefix so a dated snapshot
// (claude-haiku-4-5-20251001) prices like its family.
func lookupPrice(model string) (price, bool) {
	if p, ok := prices[model]; ok {
		return p, true
	}
	for id, p := range prices {
		if strings.HasPrefix(model, id) {
			return p, true
		}
	}
	return price{}, false
}
