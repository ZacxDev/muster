package previewhost

import (
	"maps"
	"testing"

	"github.com/ZacxDev/muster/internal/preview/wire"
)

// TestSummaryCountsEachWindowsOffersAndOpenPreviews: per window, its open
// previews, and its reported listeners WITHOUT an open preview on their port
// (the gateway's session page lists exactly those as offers: an open port is
// shown as its preview, not again). Windows with nothing are absent. While no
// gateway tunnel serves an offer stream, and while previews are off, there is
// nothing to offer from muster: nil. Each nil case is paired with the same
// state plus the one thing it lacks.
func TestSummaryCountsEachWindowsOffersAndOpenPreviews(t *testing.T) {
	h := NewHost(HostConfig{Config: Config{MaxOpen: 8}})
	h.mu.Lock()
	h.reports = []wire.Listener{
		{Port: 5173, Window: "@3"},
		{Port: 3000, Window: "@3"},
		{Port: 8000, Window: "@5"},
		{Port: 6006, Window: "@8"},
	}
	h.opens = map[string]Open{
		"p1": {Preview: "p1", Window: "@3", Port: 5173},
		"p2": {Preview: "p2", Window: "@9", Port: 4000},
		// The same port open in ANOTHER window does not hide @8's offer.
		"p3": {Preview: "p3", Window: "@5", Port: 6006},
	}
	h.mu.Unlock()

	if got := h.Summary(); got != nil {
		t.Fatalf("Summary with no gateway tunnel = %v, want nil", got)
	}
	h.mu.Lock()
	h.stream = &offerStream{out: make(chan wire.Offer, 1), done: make(chan struct{})}
	h.mu.Unlock()
	want := map[string]WindowSummary{
		"@3": {Offered: 1, Open: 1}, // 5173 is open: only 3000 is offered
		"@5": {Offered: 1, Open: 1},
		"@8": {Offered: 1},
		"@9": {Open: 1},
	}
	if got := h.Summary(); !maps.Equal(got, want) {
		t.Fatalf("Summary = %v, want %v", got, want)
	}

	h.mu.Lock()
	h.disabled = "previews.json: bad"
	h.mu.Unlock()
	if got := h.Summary(); got != nil {
		t.Fatalf("Summary while previews are off = %v, want nil", got)
	}

	h.mu.Lock()
	h.disabled, h.reports, h.opens = "", nil, map[string]Open{}
	h.mu.Unlock()
	if got := h.Summary(); got != nil {
		t.Fatalf("Summary with nothing reported or open = %v, want nil", got)
	}
}
