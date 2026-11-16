package officers

import (
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// THE CLAUDE PRICE LIST: the one table muster estimates a Claude Code
// officer instance's USD from (builtin.Backend.PutUsage → officer_llm_usage).
//
// 🔴 AN ESTIMATE, NEVER A BILL. A Claude Code instance on muster's pooled
// accounts runs on a subscription (CLAUDE_CODE_OAUTH_TOKEN), so nothing is
// billed per token and the CLI's transcript carries tokens and a model but no
// dollars. What this table yields is what those tokens would cost at
// Anthropic's first-party API list price: a comparable, per-officer figure,
// not an invoice. Every row it books counts in estimated_requests (0075's
// column for "USD booked from an estimate, not a provider-reported cost").
// The LLM proxy's own rows keep the provider's reported cost; this table is
// never read for them.
//
// SOURCE: Anthropic's API price list (platform.claude.com pricing; the
// claude-api skill's "Current Models" table and its shared/prompt-caching.md
// "Economics"), USD per million tokens, as of claudePricesAsOf. Update the
// rows, and that date, together.
//
// CACHE WRITES are priced at the 1-HOUR TTL rate, 2× input. The usage bucket
// (cctranscript.UsageBucket) carries one cache_creation total with no TTL
// split, and Claude Code writes 1-hour entries: measured on a live officer
// instance's transcript, every cache-creation token was ephemeral_1h
// (7,620,328 of 7,620,328; 0 ephemeral_5m). A 5-minute write costs 1.25×, so
// a session that wrote 5-minute entries is OVER-estimated here, never under.
// ---------------------------------------------------------------------------

// ClaudePrice is one model's price, USD per million tokens.
type ClaudePrice struct {
	Input, Output, CacheRead, CacheWrite float64
}

// claudePricesAsOf is the price list's date (SOURCE above).
const claudePricesAsOf = "2026-09-25"

// ClaudePricesAsOf is the date of the price list EstimateClaudeUSD reads.
func ClaudePricesAsOf() string { return claudePricesAsOf }

// price builds a row: cache write is the 1-hour rate, 2× input.
func price(in, out, read float64) ClaudePrice {
	return ClaudePrice{Input: in, Output: out, CacheRead: read, CacheWrite: 2 * in}
}

// claudePrices is THE table, keyed by model id with no date suffix.
var claudePrices = map[string]ClaudePrice{
	"claude-fable-5-1":  price(10, 50, 0.25),
	"claude-mythos-5-1": price(10, 50, 0.25),
	"claude-fable-5":    price(10, 50, 1.00),
	"claude-mythos-5":   price(10, 50, 1.00),
	"claude-opus-5-5":   price(4, 20, 0.20),
	"claude-opus-5":     price(5, 25, 0.50),
	"claude-opus-4-8":   price(5, 25, 0.50),
	"claude-opus-4-7":   price(5, 25, 0.50),
	"claude-opus-4-6":   price(5, 25, 0.50),
	"claude-sonnet-5-5": price(2, 10, 0.20),
	"claude-sonnet-5":   price(2, 10, 0.20),
	"claude-sonnet-4-6": price(3, 15, 0.30),
	"claude-haiku-4-5":  price(1, 5, 0.10),
}

// claudePessimistic is what a model the table does not know is priced at:
// the table's highest rate in every column, so an unknown model is
// over-estimated rather than booked as free (the LLM proxy's rule for a
// response that reported no cost, docs/officers.md "The LLM proxy").
var claudePessimistic = func() ClaudePrice {
	var p ClaudePrice
	for _, r := range claudePrices {
		p.Input, p.Output = max(p.Input, r.Input), max(p.Output, r.Output)
		p.CacheRead, p.CacheWrite = max(p.CacheRead, r.CacheRead), max(p.CacheWrite, r.CacheWrite)
	}
	return p
}()

// modelDecorations are what a model id may carry beyond the table's key: a
// context-window tag ("[1m]") and a snapshot date ("-20260401").
var modelDecorations = regexp.MustCompile(`(\[[^\]]*\]|-\d{8})$`)

// ClaudePriceOf is model's row and whether the table knows it; an unknown
// model (or "") answers the pessimistic row and false.
func ClaudePriceOf(model string) (ClaudePrice, bool) {
	m := strings.ToLower(strings.TrimSpace(model))
	for {
		cut := modelDecorations.ReplaceAllString(m, "")
		if cut == m {
			break
		}
		m = cut
	}
	if p, ok := claudePrices[m]; ok {
		return p, true
	}
	return claudePessimistic, false
}

// EstimateClaudeUSD is what input (uncached), output, cache-read and
// cache-write tokens of model cost at list price, and whether model was
// priced from its own row (false: the pessimistic row).
func EstimateClaudeUSD(model string, input, output, cacheRead, cacheWrite int64) (float64, bool) {
	p, known := ClaudePriceOf(model)
	usd := (float64(max(input, 0))*p.Input + float64(max(output, 0))*p.Output +
		float64(max(cacheRead, 0))*p.CacheRead + float64(max(cacheWrite, 0))*p.CacheWrite) / 1e6
	return usd, known
}
