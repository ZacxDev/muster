package ui

import (
	"strings"
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
// from 90%, "" below 70% and whenever the window is unknown — judged on the
// SAME rounded percentage the detail shows, so a chip never says "(70%)"
// untinted or "(89%)" red. Each boundary is pinned one token below it and
// exactly at it: a share rounds up to 70% from 69.5% (213,400 × 69.5% =
// 148,313) and to 90% from 89.5% (187,300: 167,634 is the first token count
// that rounds to 90).
func TestTheContextChipTintsByFill(t *testing.T) {
	for _, c := range []struct {
		name          string
		tokens, limit int64
		want          string
		pct           string
	}{
		{"well below", 41_203, 213_400, "", "(19%)"},
		{"one below 69.5%", 148_312, 213_400, "", "(69%)"},
		{"at 69.5%: shown as 70%", 148_313, 213_400, "warn", "(70%)"},
		{"69.6%", 175_323, 251_900, "warn", "(70%)"},
		{"between", 160_011, 187_300, "warn", "(85%)"},
		{"one below 89.5%", 167_633, 187_300, "warn", "(89%)"},
		{"at 89.5%: shown as 90%", 167_634, 187_300, "danger", "(90%)"},
		{"89.5%", 225_451, 251_900, "danger", "(90%)"},
		{"past the window", 2_000_003, 1_048_576, "danger", "(191%)"},
		{"no window: never tinted", 999_983, 0, "", ""},
	} {
		v := agentCtxView(t, mirror.AgentContext{Tokens: c.tokens, Limit: c.limit, Model: "fixture-model"})
		if v.Level != c.want {
			t.Errorf("%s (%d of %d): level %q, want %q", c.name, c.tokens, c.limit, v.Level, c.want)
		}
		if c.pct != "" && !strings.HasSuffix(v.Detail, c.pct) {
			t.Errorf("%s (%d of %d): detail %q, want it to end %q", c.name, c.tokens, c.limit, v.Detail, c.pct)
		}
	}
}

// TestClaudeCodesChipIsTheCountAlone: Claude Code reports no model or window
// here, so its chip is the count, its detail the exact count, never tinted —
// and its popover says what the number is (ClaudeContextTitle).
func TestClaudeCodesChipIsTheCountAlone(t *testing.T) {
	var tr MirrorTranscript
	tr.Ops([]cctranscript.Event{{Kind: cctranscript.KindText, Text: "x", Usage: &cctranscript.Usage{Tokens: 124_437}}})
	v := tr.TakeContext()
	if v == nil || v.Label != "124k" || v.Count != "124k" || v.Name != "" || v.Detail != "124,437 tokens" || v.Level != "" || v.Title != ClaudeContextTitle {
		t.Fatalf("Claude Code's readout: %+v", v)
	}
}

// TestClaudeContextTitleSaysWhatTheNumberIs pins the text: it is the only
// thing a tap on a bare "250k" shows beside the exact count.
func TestClaudeContextTitleSaysWhatTheNumberIs(t *testing.T) {
	const want = "Context used by the latest request: its input + cache read + cache write tokens " +
		"(output is not counted: it is in the next request's input)."
	if ClaudeContextTitle != want {
		t.Errorf("ClaudeContextTitle = %q, want %q", ClaudeContextTitle, want)
	}
}
