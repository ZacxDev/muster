package cctranscript

import (
	"reflect"
	"strconv"
	"testing"
)

// The calibration and the injected-context rule (toolctx.go), with synthetic
// fixtures; every expected value is worked out by hand beside it.

// asstOut is asst with an output_tokens of its own (asst's is always 97).
func asstOut(block string, in, create, read, output int64) string {
	return `{"type":"assistant","message":{"model":"claude-fixture","content":[` + block +
		`],"usage":{"input_tokens":` + i64(in) + `,"cache_creation_input_tokens":` + i64(create) +
		`,"cache_read_input_tokens":` + i64(read) + `,"output_tokens":` + i64(output) + `}}}`
}

func i64(n int64) string { return strconv.FormatInt(n, 10) }

// 🔴 THE CALIBRATION, PINNED. docs/mirroring.md states it; changing it is a
// decision made with cmd/muster-toolctx-calibrate's numbers, not a drift.
func TestTheEstimateIsTheCalibratedSplit(t *testing.T) {
	if ResultBytesPerToken != 2.3 {
		t.Fatalf("ResultBytesPerToken = %v, want the calibrated 2.3", ResultBytesPerToken)
	}
	cases := []struct {
		output, in, out, want int64
	}{
		// The turn's output in full + 1197 / 2.3 = 520.4 → 521: 455 + 521.
		{455, 37, 1197, 976},
		// No output known: (37 + 1197) / 2.3 = 536.5 → 537.
		{0, 37, 1197, 537},
		// A result with no text: the output alone.
		{613, 41, 0, 613},
	}
	for _, c := range cases {
		if got := EstimateTokens(c.output, c.in, c.out); got != c.want {
			t.Errorf("EstimateTokens(%d, %d, %d) = %d, want %d", c.output, c.in, c.out, got, c.want)
		}
	}
}

func TestToolContextAnInterruptedTurnsOutputIsSharedByInputSize(t *testing.T) {
	got := ledgerRun(t,
		asstOut(toolUse("toolu_a1", `{"command":"ls -la"}`), 211, 1307, 39011, 613), // 40529; input 20 B
		asstOut(toolUse("toolu_a2", `{"command":"cat a"}`), 211, 1307, 39011, 613),  // input 19 B
		result("toolu_a1", 300),
		result("toolu_a2", 1000),
		prompt("wait"),
		asstOut(textBlock("OK."), 7, 2203, 40529, 29),
	)
	// Output 613 by input: a1 = 613 × 20 / 39 = 314.4 → 314, a2 = 613 − 314 = 299.
	// a1: 314 + 300 / 2.3 (130.4 → 131) = 445; a2: 299 + 1000 / 2.3 (434.8 → 435) = 734.
	want := []ToolCost{
		{ToolID: "toolu_a1", Tokens: 445, Kind: CostEstimate, Why: WhyInterrupted, Output: 314},
		{ToolID: "toolu_a2", Tokens: 734, Kind: CostEstimate, Why: WhyInterrupted, Output: 299},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("costs:\n got %+v\nwant %+v", got, want)
	}
}

// 🔴 A SMALL CLI REMINDER IS NOT INJECTED CONTEXT. total_tokens_reminder is in
// almost every real gap; counting it spoilt nearly every measurement.
func TestToolContextASmallCLIReminderLeavesTheGapClean(t *testing.T) {
	for _, typ := range []string{"total_tokens_reminder", "silent_turn_reminder", "hook_success", "credential_org"} {
		t.Run(typ, func(t *testing.T) {
			got := ledgerRun(t,
				asstOut(toolUse("toolu_r", `{"command":"date"}`), 503, 1117, 44021, 83), // 45641
				result("toolu_r", 81),
				attachment(typ),
				asstOut(textBlock("Noted."), 19, 2741, 45641, 31), // 48401 → +2760
			)
			want := []ToolCost{{ToolID: "toolu_r", Tokens: 2760, Kind: CostMeasured}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("costs:\n got %+v\nwant %+v", got, want)
			}
		})
	}
}

// An INVARIANT GUARD (it held before this change too): a marker with no type —
// what a host that predates the type sends — still spoils the gap.
func TestToolContextAnUntypedInjectedMarkerIsNotClean(t *testing.T) {
	var c ToolContext
	c.Observe(Event{Kind: KindToolCall, ToolID: "toolu_u", Bytes: 18, Usage: &Usage{Tokens: 45641, Output: 83}})
	c.Observe(Event{Kind: KindToolResult, ToolID: "toolu_u", Bytes: 81})
	c.Observe(Event{Kind: KindInjected})
	got := c.Observe(Event{Kind: KindText, Usage: &Usage{Tokens: 48401, Output: 31}})
	// 83 + 81 / 2.3 (35.2 → 36) = 119.
	want := []ToolCost{{ToolID: "toolu_u", Tokens: 119, Kind: CostEstimate, Why: WhyInterrupted, Output: 83}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("costs:\n got %+v\nwant %+v", got, want)
	}
}

// A Clone is independent: what the original goes on to observe — here, one of
// two pending calls answered — is not in the copy.
func TestToolContextACloneIsIndependent(t *testing.T) {
	var c ToolContext
	c.Observe(Event{Kind: KindToolCall, ToolID: "toolu_x1", Bytes: 20, Usage: &Usage{Tokens: 60013, Output: 233}})
	c.Observe(Event{Kind: KindToolCall, ToolID: "toolu_x2", Bytes: 20, Usage: &Usage{Tokens: 60013, Output: 233}})
	cp := c.Clone()
	c.Observe(Event{Kind: KindToolResult, ToolID: "toolu_x1", Bytes: 509})
	// The copy hears only x2's result: x2 is the one call answered, so the
	// whole growth, 60013 → 61127 = +1114, is x2's, measured.
	cp.Observe(Event{Kind: KindToolResult, ToolID: "toolu_x2", Bytes: 311})
	got := cp.Observe(Event{Kind: KindText, Usage: &Usage{Tokens: 61127, Output: 17}})
	want := []ToolCost{{ToolID: "toolu_x2", Tokens: 1114, Kind: CostMeasured}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("costs:\n got %+v\nwant %+v", got, want)
	}
}
