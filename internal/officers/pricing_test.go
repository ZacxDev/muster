package officers_test

import (
	"math"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// TestTheClaudePriceList pins literal prices (USD per million tokens, the
// source in pricing.go) on a fixture whose four token counts differ, so a
// rate applied to the wrong direction moves the total. A decorated id
// ("[1m]", a snapshot date) is its model's row; an unknown model and the
// empty one are priced at the table's highest rate in every column, and say
// so.
func TestTheClaudePriceList(t *testing.T) {
	const in, out, read, write = 1_000_000, 100_000, 10_000_000, 1_000
	for _, c := range []struct {
		model string
		usd   float64
		known bool
	}{
		// 1×2 + 0.1×10 + 10×0.20 + 0.001×4
		{"claude-sonnet-5-5", 5.004, true},
		{"claude-sonnet-5-5[1m]", 5.004, true},
		{"claude-sonnet-5-5-20261001", 5.004, true},
		// 1×4 + 0.1×20 + 10×0.20 + 0.001×8
		{"claude-opus-5-5", 8.008, true},
		// 1×1 + 0.1×5 + 10×0.10 + 0.001×2
		{"claude-haiku-4-5", 2.502, true},
		// pessimistic: 1×10 + 0.1×50 + 10×1.00 + 0.001×20
		{"claude-model-a", 25.02, false},
		{"", 25.02, false},
	} {
		usd, known := officers.EstimateClaudeUSD(c.model, in, out, read, write)
		if math.Abs(usd-c.usd) > 1e-9 || known != c.known {
			t.Errorf("EstimateClaudeUSD(%q) = %v, %v; want %v, %v", c.model, usd, known, c.usd, c.known)
		}
	}
	// claude-haiku-5-5 is what CCD_MODEL=haiku resolves to (a live marketing
	// officer transcript's model field, 2026-11-18). Its price is tiered by
	// prompt length; the row is the >100k tier (0.50 in, 2.50 out, 0.05 read,
	// 1.00 1h write). These are that day's 18 requests' live token counts:
	// 36×0.5 + 6070×2.5 + 11163142×0.05 + 654858×1.00, /1e6. Unpriced, they
	// were booked at the pessimistic 24.564162.
	for _, model := range []string{"claude-haiku-5-5", "claude-haiku-5-5[1m]", "claude-haiku-5-5-20261101"} {
		usd, known := officers.EstimateClaudeUSD(model, 36, 6070, 11_163_142, 654_858)
		if math.Abs(usd-1.2282081) > 1e-9 || !known {
			t.Errorf("EstimateClaudeUSD(%q, live day) = %v, %v; want 1.2282081, true", model, usd, known)
		}
	}
	if p, known := officers.ClaudePriceOf("claude-haiku-5-5"); !known ||
		p != (officers.ClaudePrice{Input: 0.50, Output: 2.50, CacheRead: 0.05, CacheWrite: 1.00}) {
		t.Errorf("ClaudePriceOf(claude-haiku-5-5) = %+v, %v; want the >100k tier {0.5 2.5 0.05 1}, true", p, known)
	}

	if usd, _ := officers.EstimateClaudeUSD("claude-sonnet-5-5", -1, -1, -1, -1); usd != 0 {
		t.Errorf("negative counts priced at %v, want 0", usd)
	}
}
