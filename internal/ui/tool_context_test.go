package ui

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/cctranscript"
)

// The mirrored view's context badge (tool_context.go), driven through Ops the
// way the view's socket drives it. Synthetic events; every figure distinct.

// A measured call: the row is drawn with an empty hidden slot, the result
// keeps it empty, and the NEXT turn's usage fills it in place with a patch.
func TestOpsPatchesAMeasuredBadgeIntoTheRow(t *testing.T) {
	var tr MirrorTranscript
	id := toolRowID("toolu_a")
	call := tr.Ops([]cctranscript.Event{
		{Kind: cctranscript.KindToolCall, ToolID: "toolu_a", Tool: "Bash", Target: "ls", Input: `{"command":"ls"}`, Bytes: 16,
			Usage: &cctranscript.Usage{Tokens: 25731}},
	})
	if !strings.Contains(opsHTML(call), `id="`+id+`-x" data-tool-ctx="" hidden`) {
		t.Fatalf("the call's row has no empty badge slot:\n%s", opsHTML(call))
	}
	res := tr.Ops([]cctranscript.Event{{Kind: cctranscript.KindToolResult, ToolID: "toolu_a", OK: true, Output: "out", Bytes: 2600}})
	if strings.Contains(opsHTML(res), "ctx<") {
		t.Fatalf("a badge before the next turn was measured:\n%s", opsHTML(res))
	}
	next := tr.Ops([]cctranscript.Event{{Kind: cctranscript.KindText, Text: "Done.", Usage: &cctranscript.Usage{Tokens: 29030}}})
	var patch *MirrorOp
	for i := range next {
		if next[i].Op == "patch" {
			patch = &next[i]
		}
	}
	if patch == nil || patch.ID != id+"-x" {
		t.Fatalf("no patch of the badge slot:\n%s", opsHTML(next))
	}
	for _, want := range []string{`data-tool-ctx="measured"`, `data-tool-ctx-tokens="3299"`, `hidden=""`} {
		if !strings.Contains(patch.HTML, want) {
			t.Errorf("badge lacks %q:\n%s", want, patch.HTML)
		}
	}
	// A later redraw of the row keeps the badge.
	again := tr.Ops([]cctranscript.Event{{Kind: cctranscript.KindToolResult, ToolID: "toolu_a", OK: true, Output: "out", Bytes: 2600}})
	if !strings.Contains(opsHTML(again), `data-tool-ctx-tokens="3299"`) {
		t.Errorf("a redrawn row lost its badge:\n%s", opsHTML(again))
	}
}

// No usage at all (a runtime that records none): the result's own redraw
// carries an estimate, marked "~".
func TestOpsDrawsAnEstimateWithTheResult(t *testing.T) {
	var tr MirrorTranscript
	tr.Ops([]cctranscript.Event{{Kind: cctranscript.KindToolCall, ToolID: "call_e", Tool: "terminal", Input: `{"command":"cat a"}`, Bytes: 19}})
	ops := tr.Ops([]cctranscript.Event{{Kind: cctranscript.KindToolResult, ToolID: "call_e", OK: true, Output: "x", Bytes: 1238}})
	html := opsHTML(ops)
	// No output known: (19 + 1238) / 2.2 = 571.4, rounded up: 572.
	for _, want := range []string{`data-tool-ctx="estimate"`, `data-tool-ctx-tokens="572"`} {
		if !strings.Contains(html, want) {
			t.Errorf("estimate lacks %q:\n%s", want, html)
		}
	}
	for _, o := range ops {
		if o.Op == "patch" {
			t.Errorf("the row the result redraws was also patched:\n%s", html)
		}
	}
}

// Parallel calls: each row gets its share, marked "~", and the title names the
// measured total it was split from.
func TestOpsSplitBadgesNameTheirMeasuredTotal(t *testing.T) {
	var tr MirrorTranscript
	tr.Ops([]cctranscript.Event{
		{Kind: cctranscript.KindToolCall, ToolID: "toolu_p", Tool: "Bash", Input: `{"command":"ls -la"}`, Bytes: 20, Usage: &cctranscript.Usage{Tokens: 42226}},
		{Kind: cctranscript.KindToolCall, ToolID: "toolu_q", Tool: "Bash", Input: `{"command":"pwd -P"}`, Bytes: 20, Usage: &cctranscript.Usage{Tokens: 42226}},
		{Kind: cctranscript.KindToolResult, ToolID: "toolu_p", OK: true, Bytes: 380},
		{Kind: cctranscript.KindToolResult, ToolID: "toolu_q", OK: true, Bytes: 1180},
	})
	ops := tr.Ops([]cctranscript.Event{{Kind: cctranscript.KindText, Text: "Both.", Usage: &cctranscript.Usage{Tokens: 48226}}})
	html := opsHTML(ops)
	for _, want := range []string{
		"patch|" + toolRowID("toolu_p") + "-x|", `data-tool-ctx-tokens="1500"`,
		"patch|" + toolRowID("toolu_q") + "-x|", `data-tool-ctx-tokens="4500"`, `data-tool-ctx="split"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("split badges lack %q:\n%s", want, html)
		}
	}
}

func TestGroupDigits(t *testing.T) {
	if got := groupDigits(1234567); got != "1,234,567" {
		t.Errorf("groupDigits = %q", got)
	}
}

// A call whose next turn came after something else entered the context is an
// estimate: its turn's output share plus its result at the calibrated rate.
func TestAnInterruptedCallIsAnEstimate(t *testing.T) {
	var tr MirrorTranscript
	tr.Ops([]cctranscript.Event{
		{Kind: cctranscript.KindToolCall, ToolID: "toolu_w", Tool: "Bash", Input: `{"command":"date"}`, Bytes: 18, Usage: &cctranscript.Usage{Tokens: 45641, Output: 211}},
		{Kind: cctranscript.KindToolResult, ToolID: "toolu_w", OK: true, Bytes: 81},
		{Kind: cctranscript.KindInjected},
	})
	html := opsHTML(tr.Ops([]cctranscript.Event{{Kind: cctranscript.KindText, Text: "Noted.", Usage: &cctranscript.Usage{Tokens: 48401}}}))
	// The turn's output 211 + 81 / 2.2 = 36.8 → 37: 248.
	for _, want := range []string{`data-tool-ctx-tokens="248"`, `data-tool-ctx="estimate"`} {
		if !strings.Contains(html, want) {
			t.Errorf("the slot lacks %q:\n%s", want, html)
		}
	}
}

// An output share that rounds to 0 is a KNOWN output: the estimate is the
// result's size alone.
func TestAZeroOutputShareCountsTheResultAlone(t *testing.T) {
	var tr MirrorTranscript
	tr.Ops([]cctranscript.Event{
		{Kind: cctranscript.KindToolCall, ToolID: "toolu_z1", Tool: "Bash", Bytes: 20, Usage: &cctranscript.Usage{Tokens: 70929, Output: 50}},
		{Kind: cctranscript.KindToolCall, ToolID: "toolu_z2", Tool: "Bash", Bytes: 5000, Usage: &cctranscript.Usage{Tokens: 70929, Output: 50}},
		{Kind: cctranscript.KindToolResult, ToolID: "toolu_z1", OK: true, Bytes: 400},
		{Kind: cctranscript.KindToolResult, ToolID: "toolu_z2", OK: true, Bytes: 900},
		{Kind: cctranscript.KindPrompt, Text: "hold on"},
	})
	p := patchFor(tr.Ops([]cctranscript.Event{{Kind: cctranscript.KindText, Text: "OK.", Usage: &cctranscript.Usage{Tokens: 72133, Output: 11}}}), "toolu_z1")
	// 0 + 400 / 2.2 (181.8 → 182).
	if p == nil || !strings.Contains(p.HTML, `data-tool-ctx-tokens="182"`) || !strings.Contains(p.HTML, `data-tool-ctx="estimate"`) {
		t.Fatalf("a zero share is not counted as the result alone: %+v", p)
	}
}
