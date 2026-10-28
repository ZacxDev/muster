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
	for _, want := range []string{`data-tool-ctx="measured"`, `data-tool-ctx-tokens="3299"`, `>+3.3k ctx</span>`, "Measured: the context grew 3,299 tokens"} {
		if !strings.Contains(patch.HTML, want) {
			t.Errorf("badge lacks %q:\n%s", want, patch.HTML)
		}
	}
	if strings.Contains(patch.HTML, "~") {
		t.Errorf("a measured badge is marked approximate:\n%s", patch.HTML)
	}
	// A later redraw of the row keeps the badge.
	again := tr.Ops([]cctranscript.Event{{Kind: cctranscript.KindToolResult, ToolID: "toolu_a", OK: true, Output: "out", Bytes: 2600}})
	if !strings.Contains(opsHTML(again), `>+3.3k ctx</span>`) {
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
	// No output known: (19 + 1238) / 2.3 = 546.5, rounded up: 547.
	for _, want := range []string{`data-tool-ctx="estimate"`, `data-tool-ctx-tokens="547"`, `>~+547 ctx</span>`,
		"Estimated from the call and result size at about 2.3 bytes per token", "the turn&#39;s own output is not known, so it reads low",
		"No usage was recorded to measure it by"} {
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
		"patch|" + toolRowID("toolu_p") + "-x|", `>~+1.5k ctx</span>`,
		"patch|" + toolRowID("toolu_q") + "-x|", `>~+4.5k ctx</span>`,
		"a share of 6,000 measured for 2 parallel calls",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("split badges lack %q:\n%s", want, html)
		}
	}
}

func TestCtxCount(t *testing.T) {
	for n, want := range map[int64]string{7: "7", 999: "999", 1000: "1k", 3299: "3.3k", 9949: "9.9k", 9950: "10k", 12345: "12k", 1_260_000: "1.3M"} {
		if got := ctxCount(n); got != want {
			t.Errorf("ctxCount(%d) = %q, want %q", n, got, want)
		}
	}
	if got := groupDigits(1234567); got != "1,234,567" {
		t.Errorf("groupDigits = %q", got)
	}
}

// An estimate's tooltip says WHY it is one: the reason the ledger gave.
func TestAnEstimatesTitleSaysWhy(t *testing.T) {
	var tr MirrorTranscript
	tr.Ops([]cctranscript.Event{
		{Kind: cctranscript.KindToolCall, ToolID: "toolu_w", Tool: "Bash", Input: `{"command":"date"}`, Bytes: 18, Usage: &cctranscript.Usage{Tokens: 45641, Output: 211}},
		{Kind: cctranscript.KindToolResult, ToolID: "toolu_w", OK: true, Bytes: 81},
		{Kind: cctranscript.KindInjected},
	})
	html := opsHTML(tr.Ops([]cctranscript.Event{{Kind: cctranscript.KindText, Text: "Noted.", Usage: &cctranscript.Usage{Tokens: 48401}}}))
	// The turn's output 211 + 81 / 2.3 = 35.2 → 36: 247.
	for _, want := range []string{`>~+247 ctx</span>`, `data-tool-ctx="estimate"`, "context the CLI injected",
		"this call&#39;s share of its turn&#39;s output (211 tokens) plus its result at about 2.3 bytes per token"} {
		if !strings.Contains(html, want) {
			t.Errorf("the badge lacks %q:\n%s", want, html)
		}
	}
	if strings.Contains(html, "no usage was recorded") {
		t.Errorf("an interrupted estimate claims there was no usage:\n%s", html)
	}
}
