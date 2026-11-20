package officers_test

import (
	"strings"
	"testing"
	"time"
)

// TestCloseOldestDraftIsOnByDefaultAndDraftOnly: a draft ship with no block
// closes after 2 days; enabled true with no days is 2 days; idle_days moves
// it; enabled false turns it off; idle_days is 1..90; an unknown key is
// refused; another stage refuses the block and has none without one.
func TestCloseOldestDraftIsOnByDefaultAndDraftOnly(t *testing.T) {
	parse := func(block any) (*time.Duration, error) {
		t.Helper()
		m := draftShip()
		if block != nil {
			m["close_oldest_draft"] = block
		}
		list, err := parseDecl(t, draftDecl(m), shipEnv(t))
		if err != nil {
			return nil, err
		}
		if c := list[0].Ship.CloseOldestDraft; c != nil {
			d := c.After()
			return &d, nil
		}
		return nil, nil
	}
	for name, c := range map[string]struct {
		block any
		want  time.Duration
	}{
		"no block (the default)":  {nil, 48 * time.Hour},
		"an empty block":          {map[string]any{}, 48 * time.Hour},
		"enabled, no idle_days":   {map[string]any{"enabled": true}, 48 * time.Hour},
		"idle_days 5, no enabled": {map[string]any{"idle_days": 5}, 120 * time.Hour},
		"enabled, idle_days 1":    {map[string]any{"enabled": true, "idle_days": 1}, 24 * time.Hour},
		"enabled, idle_days 90":   {map[string]any{"enabled": true, "idle_days": 90}, 2160 * time.Hour},
	} {
		if d, err := parse(c.block); err != nil || d == nil || *d != c.want {
			t.Errorf("%s: %v %v, want on at %v", name, d, err, c.want)
		}
	}
	if d, err := parse(map[string]any{"enabled": false, "idle_days": 4}); err != nil || d != nil {
		t.Errorf("enabled false: %v %v, want off", d, err)
	}
	for _, days := range []int{-1, 91} {
		if _, err := parse(map[string]any{"idle_days": days}); err == nil || !strings.Contains(err.Error(), "close_oldest_draft.idle_days") {
			t.Errorf("idle_days %d: %v, want a refusal naming close_oldest_draft.idle_days", days, err)
		}
	}
	if _, err := parse(map[string]any{"days": 3}); err == nil {
		t.Error("an unknown key was accepted")
	}
	m := fullShip()
	m["close_oldest_draft"] = map[string]any{"enabled": true}
	if _, err := parseShip(t, m, shipEnv(t)); err == nil || !strings.Contains(err.Error(), "close_oldest_draft is enforced by the draft stage only") {
		t.Errorf("a release-stage block: %v, want refused", err)
	}
	list, err := parseShip(t, fullShip(), shipEnv(t))
	if err != nil || list[0].Ship.CloseOldestDraft != nil {
		t.Errorf("a release stage with no block: %v %+v, want none", err, list[0].Ship.CloseOldestDraft)
	}
}
