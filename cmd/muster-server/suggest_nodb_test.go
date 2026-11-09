package main

import (
	"context"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/features"
	"github.com/ZacxDev/muster/internal/suggest"
)

// TestNoDatabaseSuggestRepliesAreOffUnlessOn (operator, 2026-11-09): with no
// database nobody can turn suggested replies on for themselves, so the
// Features page says they are OFF for everyone, naming MUSTER_SUGGEST=on;
// with it (and a key) they are ON. With a database, MUSTER_SUGGEST unset is
// not a reason to be off: each user's own switch governs.
func TestNoDatabaseSuggestRepliesAreOffUnlessOn(t *testing.T) {
	keyed := config{AgentOpenRouterKey: "or-key-for-a-test-0123456789"}
	unset := featureSet(t, keyed, api.Extensions{Suggest: &suggest.Engine{}}, false)
	if f, _ := unset.Get(features.SuggestedReplies); f.State != features.Off || !strings.HasSuffix(f.Reason, envSuggest+"=on") {
		t.Errorf("no database, unset: %s %q, want off naming %s=on", f.State, f.Reason, envSuggest)
	}
	on := featureSet(t, keyed, api.Extensions{Suggest: &suggest.Engine{On: true}}, false)
	if f, _ := on.Get(features.SuggestedReplies); f.State != features.On {
		t.Errorf("no database, %s=on: %s %q, want on", envSuggest, f.State, f.Reason)
	}
	withDB := featureSet(t, keyed, api.Extensions{Suggest: &suggest.Engine{Prefs: noSavedChoices{}}}, true)
	if f, _ := withDB.Get(features.SuggestedReplies); f.State != features.On {
		t.Errorf("a database, unset: %s %q, want on (the per-user switch governs)", f.State, f.Reason)
	}
}

// TestMuster_SUGGESTParses: unset is neither on nor off; on/1/true set On;
// off/0/false set Off; anything else refuses to boot.
func TestMusterSuggestParses(t *testing.T) {
	for _, c := range []struct {
		v       string
		on, off bool
	}{{"", false, false}, {"on", true, false}, {"TRUE", true, false}, {"1", true, false}, {"off", false, true}, {"0", false, true}} {
		cfg, err := loadConfig(func(k string) string {
			if k == envSuggest {
				return c.v
			}
			return ""
		})
		if err != nil {
			t.Fatalf("%q: %v", c.v, err)
		}
		if cfg.SuggestOn != c.on || cfg.SuggestOff != c.off {
			t.Errorf("%s=%q: on=%v off=%v, want on=%v off=%v", envSuggest, c.v, cfg.SuggestOn, cfg.SuggestOff, c.on, c.off)
		}
	}
	if _, err := loadConfig(func(k string) string {
		if k == envSuggest {
			return "maybe"
		}
		return ""
	}); err == nil {
		t.Error("an unknown value booted")
	}
}

// noSavedChoices is a toggle store with no saved choices (a fresh database).
type noSavedChoices struct{}

func (noSavedChoices) Choice(context.Context, int64) (bool, bool, error) { return false, false, nil }
func (noSavedChoices) SetDisabled(context.Context, int64, bool) error    { return nil }
