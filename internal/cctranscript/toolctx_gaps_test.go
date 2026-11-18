package cctranscript

import (
	"reflect"
	"strconv"
	"strings"
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
	if ResultBytesPerToken != 2.2 {
		t.Fatalf("ResultBytesPerToken = %v, want the calibrated 2.2", ResultBytesPerToken)
	}
	cases := []struct {
		output, in, out int64
		known           bool
		want            int64
	}{
		// The turn's output in full + 1197 / 2.2 = 544.1 → 545: 455 + 545.
		{455, 37, 1197, true, 1000},
		// No output known: (37 + 1197) / 2.2 = 560.9 → 561.
		{0, 37, 1197, false, 561},
		// Output KNOWN, its share rounded to 0: the result alone, not the
		// sizes: 1197 / 2.2 → 545.
		{0, 37, 1197, true, 545},
		// A result with no text: the output alone.
		{613, 41, 0, true, 613},
	}
	for _, c := range cases {
		if got := EstimateTokens(c.output, c.in, c.out, c.known); got != c.want {
			t.Errorf("EstimateTokens(%d, %d, %d, %v) = %d, want %d", c.output, c.in, c.out, c.known, got, c.want)
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
	// a1: 314 + 300 / 2.2 (136.4 → 137) = 451; a2: 299 + 1000 / 2.2 (454.5 → 455) = 754.
	want := []ToolCost{
		{ToolID: "toolu_a1", Tokens: 451, Kind: CostEstimate, Why: WhyInterrupted, OutputKnown: true, Output: 314},
		{ToolID: "toolu_a2", Tokens: 754, Kind: CostEstimate, Why: WhyInterrupted, OutputKnown: true, Output: 299},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("costs:\n got %+v\nwant %+v", got, want)
	}
}

// 🔴 A SMALL CLI REMINDER IS NOT INJECTED CONTEXT. total_tokens_reminder is in
// almost every real gap; counting it spoilt nearly every measurement.
func TestToolContextASmallCLIReminderLeavesTheGapClean(t *testing.T) {
	for _, typ := range []string{"total_tokens_reminder", "hook_permission_decision", "prompt_snapshot", "credential_org"} {
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
	// 83 + 81 / 2.2 (36.8 → 37) = 120.
	want := []ToolCost{{ToolID: "toolu_u", Tokens: 120, Kind: CostEstimate, Why: WhyInterrupted, OutputKnown: true, Output: 83}}
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

// 🔴 THE NEGLIGIBLE SET, PINNED — cmd/muster-toolctx-calibrate decides it and
// says whether this matches; docs/mirroring.md has its table.
func TestTheNegligibleSetIsTheCalibratedOne(t *testing.T) {
	for typ, want := range map[string]bool{
		"total_tokens_reminder": true, "hook_permission_decision": true, "prompt_snapshot": true, "credential_org": true,
		"silent_turn_reminder": false, "hook_success": false, "deferred_tools_record": false, "read_truncation_notice": false,
		"diagnostics": false, "queued_command": false, "": false,
	} {
		if got := NegligibleInjected(typ); got != want {
			t.Errorf("NegligibleInjected(%q) = %v, want %v", typ, got, want)
		}
	}
}

// An output share that rounds to 0 is still a KNOWN output: the call is
// estimated as its result alone, and says the output was counted.
func TestToolContextAZeroOutputShareIsStillKnown(t *testing.T) {
	big := `{"command":"` + strings.Repeat("x", 4986) + `"}` // 5000 B
	got := ledgerRun(t,
		asstOut(toolUse("toolu_s1", `{"command":"ls -la"}`), 17, 909, 70003, 50), // 70929; input 20 B
		asstOut(toolUse("toolu_s2", big), 17, 909, 70003, 50),
		result("toolu_s1", 400),
		result("toolu_s2", 900),
		prompt("hold on"),
		asstOut(textBlock("OK."), 3, 1201, 70929, 11),
	)
	// Output 50 by input: s1 = 50 × 20 / 5020 = 0.2 → 0; s2 = 50.
	// s1: 0 + 400 / 2.2 (181.8 → 182) = 182; s2: 50 + 900 / 2.2 (409.1 → 410) = 460.
	want := []ToolCost{
		{ToolID: "toolu_s1", Tokens: 182, Kind: CostEstimate, Why: WhyInterrupted, OutputKnown: true, Output: 0},
		{ToolID: "toolu_s2", Tokens: 460, Kind: CostEstimate, Why: WhyInterrupted, OutputKnown: true, Output: 50},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("costs:\n got %+v\nwant %+v", got, want)
	}
}
