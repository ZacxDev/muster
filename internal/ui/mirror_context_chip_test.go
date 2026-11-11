package ui

import (
	"testing"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/mirror"
)

// THE HEADER'S CONTEXT CHIP (ContextView, built by contextView): a SHORT label
// that fits a phone's header — the model without its provider prefix and the
// count of the window, "<name> · <count>/<window>" — the exact figures in
// Detail (the chip's popover and tooltip), and a Level that tints it as the
// window fills. Every value below is invented fixture data; the token counts
// and windows are pairwise distinct and none is 70 or 90 (the thresholds) or a
// round number a hardcoded literal could produce.

func agentCtxView(t *testing.T, c mirror.AgentContext) *ContextView {
	t.Helper()
	var tr MirrorTranscript
	tr.SetAgentContext(&c)
	v := tr.TakeContext()
	if v == nil {
		t.Fatalf("no readout for %+v", c)
	}
	return v
}

// TestTheContextChipLabelIsShort: the provider prefix is stripped from the
// model (everything up to its LAST "/"), the counts lose their spaces and the
// word "context", and with no window known only the count is left.
func TestTheContextChipLabelIsShort(t *testing.T) {
	for _, c := range []struct {
		name string
		in   mirror.AgentContext
		want string
	}{
		{"a provider prefix is stripped", mirror.AgentContext{Tokens: 29_214, Limit: 1_048_576, Model: "fixtureprov/fixture-model-v4.1-flash"}, "fixture-model-v4.1-flash · 29k/1M"},
		{"every prefix segment is stripped", mirror.AgentContext{Tokens: 12_345, Limit: 262_144, Model: "router/vendor/deep-name"}, "deep-name · 12k/262k"},
		{"a bare model id stays as it is", mirror.AgentContext{Tokens: 46_809, Limit: 187_500, Model: "fixture-model"}, "fixture-model · 47k/188k"},
		{"no window known: the count alone", mirror.AgentContext{Tokens: 5_339, Model: "fixtureprov/other-model"}, "other-model · 5k"},
		{"no model, no window", mirror.AgentContext{Tokens: 913}, "913"},
		{"a trailing slash keeps the whole id", mirror.AgentContext{Tokens: 3_141, Limit: 96_017, Model: "odd/"}, "odd/ · 3k/96k"},
	} {
		if got := agentCtxView(t, c.in).Label; got != c.want {
			t.Errorf("%s: label %q, want %q", c.name, got, c.want)
		}
	}
}

// TestTheContextChipSendsItsParts: the page draws the chip from the parts the
// server sends (the name truncates, the count never does), so they must say
// what the label says.
func TestTheContextChipSendsItsParts(t *testing.T) {
	v := agentCtxView(t, mirror.AgentContext{Tokens: 29_214, Limit: 1_048_576, Model: "fixtureprov/fixture-model-v4.1-flash"})
	if v.Name != "fixture-model-v4.1-flash" || v.Count != "29k/1M" || v.Model != "fixtureprov/fixture-model-v4.1-flash" {
		t.Errorf("parts: name %q count %q model %q", v.Name, v.Count, v.Model)
	}
	v = agentCtxView(t, mirror.AgentContext{Tokens: 5_339})
	if v.Name != "" || v.Count != "5k" {
		t.Errorf("no model: name %q count %q", v.Name, v.Count)
	}
}

// TestTheContextChipDetailIsExact: the full model id and the exact counts,
// with the share of the window when it is known.
func TestTheContextChipDetailIsExact(t *testing.T) {
	for _, c := range []struct {
		name string
		in   mirror.AgentContext
		want string
	}{
		{"model and window", mirror.AgentContext{Tokens: 29_214, Limit: 1_048_576, Model: "fixtureprov/fixture-model-v4.1-flash"}, "fixtureprov/fixture-model-v4.1-flash — 29,214 of 1,048,576 tokens (3%)"},
		{"no window", mirror.AgentContext{Tokens: 46_809, Model: "fixtureprov/other-model"}, "fixtureprov/other-model — 46,809 tokens"},
		{"no model", mirror.AgentContext{Tokens: 5_339, Limit: 262_144}, "5,339 of 262,144 tokens (2%)"},
		{"a small count", mirror.AgentContext{Tokens: 913, Limit: 1_234}, "913 of 1,234 tokens (74%)"},
		{"past the window", mirror.AgentContext{Tokens: 2_000_003, Limit: 1_048_576, Model: "m"}, "m — 2,000,003 of 1,048,576 tokens (191%)"},
	} {
		if got := agentCtxView(t, c.in).Detail; got != c.want {
			t.Errorf("%s: detail %q, want %q", c.name, got, c.want)
		}
	}
}

// TestTheContextChipTintsByFill: "warn" from 70% of a KNOWN window, "danger"
// from 90%, "" below 70% and whenever the window is unknown. Each boundary
// is pinned one token below it and exactly at it (213,400 × 70% = 149,380;
// 187,300 × 90% = 168,570).
func TestTheContextChipTintsByFill(t *testing.T) {
	for _, c := range []struct {
		name          string
		tokens, limit int64
		want          string
	}{
		{"well below", 41_203, 213_400, ""},
		{"one below 70%", 149_379, 213_400, ""},
		{"at 70%", 149_380, 213_400, "warn"},
		{"between", 160_011, 187_300, "warn"},
		{"one below 90%", 168_569, 187_300, "warn"},
		{"at 90%", 168_570, 187_300, "danger"},
		{"past the window", 2_000_003, 1_048_576, "danger"},
		{"no window: never tinted", 999_983, 0, ""},
	} {
		v := agentCtxView(t, mirror.AgentContext{Tokens: c.tokens, Limit: c.limit, Model: "fixture-model"})
		if v.Level != c.want {
			t.Errorf("%s (%d of %d): level %q, want %q", c.name, c.tokens, c.limit, v.Level, c.want)
		}
	}
}

// TestClaudeCodesChipIsTheCountAlone: Claude Code reports no model or window
// here, so its chip is the count, its detail the exact count, never tinted.
func TestClaudeCodesChipIsTheCountAlone(t *testing.T) {
	var tr MirrorTranscript
	tr.Ops([]cctranscript.Event{{Kind: cctranscript.KindText, Text: "x", Usage: &cctranscript.Usage{Tokens: 124_437}}})
	v := tr.TakeContext()
	if v == nil || v.Label != "124k" || v.Count != "124k" || v.Name != "" || v.Detail != "124,437 tokens" || v.Level != "" || v.Title != "" {
		t.Fatalf("Claude Code's readout: %+v", v)
	}
}
