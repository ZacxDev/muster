package ccusage

import (
	"net/http"
	"testing"
	"time"
)

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

// TestParseRateLimitHeadersOnAMeasuredAnswer is the header set a pool token's
// one-token probe was answered with: a 7d window only (no 5h headers), its
// utilization a 0-1 fraction (stored as a percent), unix-seconds resets.
func TestParseRateLimitHeadersOnAMeasuredAnswer(t *testing.T) {
	h := hdr("anthropic-ratelimit-unified-status", "allowed",
		"anthropic-ratelimit-unified-representative-claim", "seven_day",
		"anthropic-ratelimit-unified-reset", "1794866400",
		"anthropic-ratelimit-unified-7d-status", "allowed",
		"anthropic-ratelimit-unified-7d-utilization", "0.53",
		"anthropic-ratelimit-unified-7d-reset", "1794866400",
		"anthropic-ratelimit-unified-fallback-percentage", "0.5",
		"anthropic-ratelimit-unified-overage-status", "rejected",
		"anthropic-ratelimit-unified-overage-disabled-reason", "org_level_disabled")
	r, ok := ParseRateLimitHeaders(h)
	if !ok {
		t.Fatal("no reading")
	}
	reset := time.Unix(1794866400, 0).UTC()
	w := r.Windows[SevenDay]
	if w.Utilization == nil || *w.Utilization != 53 || w.ResetsAt == nil || !w.ResetsAt.Equal(reset) || w.Status != "allowed" {
		t.Fatalf("seven_day = %+v", w)
	}
	if _, ok := r.Windows[FiveHour]; ok {
		t.Fatal("five_hour appeared with no 5h headers")
	}
	if o := r.Windows["overage"]; o.Status != "rejected" || o.Utilization != nil {
		t.Fatalf("overage = %+v, want its status alone", o)
	}
	if r.Status != "allowed" || r.RepresentativeClaim != "seven_day" || r.ResetsAt == nil || !r.ResetsAt.Equal(reset) || len(r.Windows) != 2 {
		t.Fatalf("reading = %+v", r)
	}
}

func TestParseRateLimitHeadersWithAFiveHourWindow(t *testing.T) {
	r, ok := ParseRateLimitHeaders(hdr("anthropic-ratelimit-unified-status", "allowed_warning",
		"anthropic-ratelimit-unified-representative-claim", "five_hour",
		"anthropic-ratelimit-unified-5h-utilization", "0.912", "anthropic-ratelimit-unified-5h-reset", "1794800000",
		"anthropic-ratelimit-unified-5h-status", "allowed_warning",
		"anthropic-ratelimit-unified-7d-utilization", "0.1", "anthropic-ratelimit-unified-7d-reset", "1794866400",
		"anthropic-ratelimit-unified-7d_oi-utilization", "0.02"))
	if !ok {
		t.Fatal("no reading")
	}
	if w := r.Windows[FiveHour]; w.Utilization == nil || *w.Utilization != 91.2 || w.Status != "allowed_warning" || !w.ResetsAt.Equal(time.Unix(1794800000, 0)) {
		t.Fatalf("five_hour = %+v", w)
	}
	if *r.Windows[SevenDay].Utilization != 10 || *r.Windows["seven_day_overage_included"].Utilization != 2 || r.RepresentativeClaim != "five_hour" {
		t.Fatalf("reading = %+v", r)
	}
}

// TestParseRateLimitHeadersDropsGarbage: a garbage value drops that value
// only; an answer with none of the headers is no reading.
func TestParseRateLimitHeadersDropsGarbage(t *testing.T) {
	if _, ok := ParseRateLimitHeaders(hdr("content-type", "application/json", "anthropic-ratelimit-requests-limit", "50")); ok {
		t.Fatal("an answer without unified headers read as a reading")
	}
	r, ok := ParseRateLimitHeaders(hdr("anthropic-ratelimit-unified-5h-utilization", "lots",
		"anthropic-ratelimit-unified-5h-reset", "tomorrow",
		"anthropic-ratelimit-unified-7d-utilization", "NaN",
		"anthropic-ratelimit-unified-7d-reset", "12",
		"anthropic-ratelimit-unified-7d_oi-utilization", "-0.1",
		"anthropic-ratelimit-unified-overage-utilization", "Inf",
		"anthropic-ratelimit-unified-status", "allowed; drop table",
		"anthropic-ratelimit-unified-representative-claim", "seven_day",
		"anthropic-ratelimit-unified-reset", "1794866400"))
	if !ok || len(r.Windows) != 0 || r.Status != "" || r.RepresentativeClaim != "seven_day" || r.ResetsAt == nil {
		t.Fatalf("reading = %+v ok=%v; want no windows, no status, the claim and the reset", r, ok)
	}
}
