package features

import (
	"strings"
	"testing"
)

func testCatalog() []Feature {
	return []Feature{
		{ID: Dictation, Name: "Dictation", Description: "A microphone.",
			Enable: []Var{{Name: "EXAMPLE_STT_URL", Placeholder: "url"}, {Name: "EXAMPLE_STT_TOKEN", Placeholder: "token"}}},
		{ID: Plugins, Name: "Plugins", Description: "Bots.", Enable: []Var{{Name: "EXAMPLE_PLUGINS", Value: "1"}}},
		{ID: AgentAskTool, Name: "Session tools", Description: "Tools in sessions.", HostSide: true, Note: "run it with -mcp-listen"},
	}
}

// TestEvaluateIsATwoWayLedger: an entry with no state, a state with no entry,
// a duplicate, a bad state and an off feature with no reason each refuse —
// and the complete, consistent input (the positive control) is accepted.
func TestEvaluateIsATwoWayLedger(t *testing.T) {
	ok := map[string]Status{Dictation: {State: Off, Reason: "x unset"}, Plugins: {State: On}, AgentAskTool: {State: Misconfigured, Reason: "y"}}
	set, err := Evaluate(testCatalog(), ok)
	if err != nil || set.Len() != 3 {
		t.Fatalf("positive control: %v (len %d)", err, set.Len())
	}
	without := func(id string) map[string]Status {
		m := map[string]Status{}
		for k, v := range ok {
			if k != id {
				m[k] = v
			}
		}
		return m
	}
	with := func(id string, st Status) map[string]Status {
		m := without("")
		m[id] = st
		return m
	}
	for name, c := range map[string]struct {
		cat  []Feature
		st   map[string]Status
		want string
	}{
		"an entry with no state":       {testCatalog(), without(Plugins), `"plugins" has no evaluated state`},
		"a state with no entry":        {testCatalog(), with("not-in-the-catalog", Status{State: On}), "not-in-the-catalog, which the catalog does not list"},
		"a duplicate entry":            {append(testCatalog(), testCatalog()[0]), ok, `"dictation" is in the catalog twice`},
		"an unknown state":             {testCatalog(), with(Plugins, Status{State: "maybe"}), `state "maybe"`},
		"off with no reason":           {testCatalog(), with(Dictation, Status{State: Off}), `"dictation" is off with no reason`},
		"optional with no way to turn": {append(testCatalog(), Feature{ID: "x", Name: "X", Description: "d"}), with("x", Status{State: On}), "says nothing about how to turn it on"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Evaluate(c.cat, c.st); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
		})
	}
}

// TestTheHintAndTheDisabledMessageArePinned pins the exact sentences every
// consumer prints, for each shape: variables by name, a fixed value, a
// host-side feature, and a misconfigured one whose reason leads.
func TestTheHintAndTheDisabledMessageArePinned(t *testing.T) {
	set, err := Evaluate(testCatalog(), map[string]Status{
		Dictation: {State: Off, Reason: "EXAMPLE_STT_TOKEN is unset"}, Plugins: {State: Misconfigured, Reason: "no gateway"},
		AgentAskTool: {State: Off, Reason: "mirror off"}})
	if err != nil {
		t.Fatal(err)
	}
	get := func(id string) Feature { f, _ := set.Get(id); return f }
	for _, c := range []struct{ got, want string }{
		{get(Dictation).EnableHint(), "set EXAMPLE_STT_URL and EXAMPLE_STT_TOKEN in .env and restart (docs/features.md#dictation)"},
		{get(Dictation).DisabledMessage(), "disabled: dictation — set EXAMPLE_STT_URL and EXAMPLE_STT_TOKEN in .env and restart (docs/features.md#dictation)"},
		{get(Plugins).DisabledMessage(), "misconfigured: plugins — no gateway; set EXAMPLE_PLUGINS=1 in .env and restart (docs/features.md#plugins)"},
		{get(AgentAskTool).EnableHint(), "run it with -mcp-listen (docs/features.md#agent-ask-tool)"},
		{strings.Join(get(Dictation).EnableLines(), "|"), "EXAMPLE_STT_URL=<url>|EXAMPLE_STT_TOKEN=<token>"},
		{strings.Join(get(Plugins).EnableLines(), "|"), "EXAMPLE_PLUGINS=1"},
		{LogLine(get(Dictation)), "feature dictation: OFF — EXAMPLE_STT_TOKEN is unset. Enable: set EXAMPLE_STT_URL and EXAMPLE_STT_TOKEN in .env and restart (docs/features.md#dictation)"},
		{get(Dictation).DocsLink(), "https://github.com/ZacxDev/muster/blob/main/docs/features.md#dictation"},
	} {
		if c.got != c.want {
			t.Errorf("got  %q\nwant %q", c.got, c.want)
		}
	}
	hosted := Feature{ID: "h", HostSide: true, Enable: []Var{{Name: "EXAMPLE_HOST_VAR", Value: "1"}}}
	if got, want := hosted.EnableHint(), "on each host, set EXAMPLE_HOST_VAR=1 in muster-mirror's environment and restart it (docs/features.md#h)"; got != want {
		t.Errorf("host-side hint %q, want %q", got, want)
	}
	if n := set.Counts(); n[On] != 0 || n[Off] != 2 || n[Misconfigured] != 1 {
		t.Errorf("counts %v", n)
	}
	if off := set.NotOn(); len(off) != 3 {
		t.Errorf("NotOn = %d, want 3", len(off))
	}
}
