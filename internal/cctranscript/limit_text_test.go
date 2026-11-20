package cctranscript

import "testing"

// 🔴 A TRANSIENT RATE LIMIT IS NOT A USAGE LIMIT. IsLimitText is how ccd marks
// a 429 `usage_limit` (cmd/ccd classifyAPIError), and a marked 429 cools the
// account down and fails its agents over. A bare "limit reached" read "Rate
// limit reached" as one; every usage-limit wording the repository knows must
// still be read, and no transient wording may be.
func TestIsLimitTextReadsEveryUsageLimitAndNoTransientRateLimit(t *testing.T) {
	// Every usage-limit wording the repository's tests and fixtures carry: the
	// pinned samples (the current CLI's and an earlier CLI's), plus the shapes
	// cmd/ccd, internal/agentgateway, internal/ccpool, internal/credentials and
	// internal/ccusage use.
	usage := []string{
		"You've hit your limit",
		"You've hit your limit · resets 5pm",
		"You’ve hit your weekly limit · resets Nov 2, 1am (UTC)",
		"You've hit your weekly limit · resets Nov 23, 1am (UTC)",
		"You've hit your session limit · resets 9:45pm (UTC)",
		"Claude usage limit reached. Your limit will reset at 3pm (America/Los_Angeles).",
		"5-hour limit reached ∙ resets 3pm",
		"weekly limit reached",
		"You've hit your Opus limit · resets Oct 15, 10pm (UTC)",
		"You've hit your weekly Opus limit",
	}
	for _, s := range limitSamples {
		usage = append(usage, s.text)
	}
	for _, text := range usage {
		if !IsLimitText(text) {
			t.Errorf("IsLimitText(%q) = false; it is a usage limit", text)
		}
	}
	transient := []string{
		"Rate limit reached",
		"API Error: Rate limit reached for requests per minute",
		"Concurrent request limit reached, retry shortly",
		"API Error: Request rate limited",
		"API Error: 429 too many requests",
	}
	for _, text := range transient {
		if IsLimitText(text) {
			t.Errorf("IsLimitText(%q) = true; it is a transient rate limit, not a usage limit", text)
		}
	}
}
