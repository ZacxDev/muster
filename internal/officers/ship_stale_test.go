package officers_test

import (
	"strings"
	"testing"
	"time"
)

// TestStaleDraftsIsOffByDefaultAndDraftOnly: no block, or enabled false, is
// no age-out (nil); enabled with no days is 3 days; days 1..90 only; any stage
// but draft refuses the block; an unknown key is refused.
func TestStaleDraftsIsOffByDefaultAndDraftOnly(t *testing.T) {
	parse := func(block any) (*time.Duration, error) {
		t.Helper()
		m := draftShip()
		if block != nil {
			m["stale_drafts"] = block
		}
		list, err := parseDecl(t, draftDecl(m), shipEnv(t))
		if err != nil {
			return nil, err
		}
		if sd := list[0].Ship.StaleDrafts; sd != nil {
			d := sd.After()
			return &d, nil
		}
		return nil, nil
	}
	for name, block := range map[string]any{
		"no block":        nil,
		"enabled false":   map[string]any{"enabled": false, "days": 5},
		"an empty block":  map[string]any{},
		"days but not on": map[string]any{"days": 2},
	} {
		if d, err := parse(block); err != nil || d != nil {
			t.Errorf("%s: %v %v, want no age-out", name, d, err)
		}
	}
	for days, want := range map[int]time.Duration{0: 72 * time.Hour, 1: 24 * time.Hour, 7: 168 * time.Hour, 90: 2160 * time.Hour} {
		block := map[string]any{"enabled": true}
		if days != 0 {
			block["days"] = days
		}
		if d, err := parse(block); err != nil || d == nil || *d != want {
			t.Errorf("enabled, days %d: %v %v, want %v", days, d, err, want)
		}
	}
	for _, days := range []int{-1, 91} {
		if _, err := parse(map[string]any{"enabled": true, "days": days}); err == nil || !strings.Contains(err.Error(), "stale_drafts.days") {
			t.Errorf("days %d: %v, want a refusal naming stale_drafts.days", days, err)
		}
	}
	if _, err := parse(map[string]any{"enabled": true, "close_after": 3}); err == nil {
		t.Error("an unknown key was accepted")
	}
	m := fullShip()
	m["stale_drafts"] = map[string]any{"enabled": true}
	if _, err := parseShip(t, m, shipEnv(t)); err == nil || !strings.Contains(err.Error(), "stale_drafts is enforced by the draft stage only") {
		t.Errorf("a release-stage block: %v, want refused", err)
	}
}
