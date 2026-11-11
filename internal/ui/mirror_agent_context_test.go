package ui

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/mirror"
)

// THE READOUT A RUNTIME REPORTS BESIDE ITS EVENTS (opencode: mirror.Msg.Context,
// SetAgentContext): its model, the count and the model's window, pinned as
// literal labels. Values are pairwise distinct and none is a round constant.

// TestAnAgentContextReadoutNamesItsModelAndWindow: "<name> · <count>/
// <window>" (the model without its provider prefix); with no window known,
// the count alone ("<name> · <count>", as Claude Code's reads); with no
// model, no model. The title says how the number is derived.
func TestAnAgentContextReadoutNamesItsModelAndWindow(t *testing.T) {
	for _, c := range []struct {
		name string
		in   mirror.AgentContext
		want string
	}{
		{"model and window", mirror.AgentContext{Tokens: 46809, Limit: 187500, Model: "fixture-model"}, "fixture-model · 47k/188k"},
		{"an unknown window", mirror.AgentContext{Tokens: 46809, Model: "fixture-model"}, "fixture-model · 47k"},
		{"no model", mirror.AgentContext{Tokens: 5339, Limit: 262144}, "5k/262k"},
		{"a small count", mirror.AgentContext{Tokens: 913, Limit: 1_048_576, Model: "vendor/long-name"}, "long-name · 913/1M"},
	} {
		var tr MirrorTranscript
		in := c.in
		tr.SetAgentContext(&in)
		got := tr.TakeContext()
		if got == nil || got.Label != c.want || got.Tokens != c.in.Tokens || got.Limit != c.in.Limit || got.Model != c.in.Model {
			t.Errorf("%s: %+v, want label %q", c.name, got, c.want)
			continue
		}
		if got.Title != AgentContextTitle || !strings.Contains(got.Title, "input + cache read + cache write") {
			t.Errorf("%s: tooltip %q", c.name, got.Title)
		}
	}
}

// TestAnAgentContextIsReportedOnceAndForgottenOnReset: reported once; nil (a
// batch with none) leaves it; a new one is reported; a Reset forgets it and
// a later Claude Code usage reads as Claude Code's again.
func TestAnAgentContextIsReportedOnceAndForgottenOnReset(t *testing.T) {
	var tr MirrorTranscript
	tr.SetAgentContext(&mirror.AgentContext{Tokens: 10358, Limit: 96000, Model: "m1"})
	if tr.TakeContext() == nil || tr.TakeContext() != nil {
		t.Fatal("not reported exactly once")
	}
	tr.SetAgentContext(nil)
	tr.Ops([]cctranscript.Event{{Kind: cctranscript.KindText, Text: "no usage"}})
	if c := tr.TakeContext(); c != nil {
		t.Fatalf("a batch with no context reported %+v", c)
	}
	tr.SetAgentContext(&mirror.AgentContext{Tokens: 0, Limit: 96000, Model: "m1"})
	if c := tr.TakeContext(); c != nil {
		t.Fatalf("a context of no tokens was reported: %+v", c)
	}
	tr.SetAgentContext(&mirror.AgentContext{Tokens: 12345, Limit: 96000, Model: "m1"})
	if c := tr.TakeContext(); c == nil || c.Label != "m1 · 12k/96k" {
		t.Fatalf("a new context: %+v", c)
	}
	tr.Reset()
	if c := tr.TakeContext(); c != nil {
		t.Fatalf("a Reset kept %+v", c)
	}
	tr.Ops([]cctranscript.Event{{Kind: cctranscript.KindText, Text: "t", Usage: &cctranscript.Usage{Tokens: 2468}}})
	if c := tr.TakeContext(); c == nil || c.Label != "2k" || c.Title != "" {
		t.Fatalf("Claude Code's readout after a Reset: %+v", c)
	}
}

// TestAnAgentContextsModelIsCleaned: control characters dropped, cut to
// mirror.MaxContextModel bytes — it is the host's text, drawn as text.
func TestAnAgentContextsModelIsCleaned(t *testing.T) {
	var tr MirrorTranscript
	tr.SetAgentContext(&mirror.AgentContext{Tokens: 4321, Model: "a\x1b[31mb\n" + strings.Repeat("x", 200)})
	c := tr.TakeContext()
	if c == nil || strings.ContainsAny(c.Model, "\x1b\n") || len(c.Model) != mirror.MaxContextModel || !strings.HasPrefix(c.Model, "a[31mb") {
		t.Fatalf("the model %q (%d bytes)", c.Model, len(c.Model))
	}
}
