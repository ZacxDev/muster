package cctranscript

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// The tool-call context ledger (toolctx.go), fed SYNTHETIC transcripts through
// the real decoder (DecodeLine → Events → Observe). Every token figure in these
// fixtures is distinct from every other and from EstimateBytesPerToken, so an
// expectation can only be met by the arithmetic it names; each expected value
// is worked out by hand in the comment beside it.

// asst is an assistant record carrying one block and, when in > 0, a usage
// block of input + cache creation + cache read.
func asst(block string, in, create, read int64) string {
	usage := ""
	if in > 0 {
		usage = fmt.Sprintf(`,"usage":{"input_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,"output_tokens":97}`, in, create, read)
	}
	return `{"type":"assistant","message":{"model":"claude-fixture","content":[` + block + `]` + usage + `}}`
}

func toolUse(id, input string) string {
	return `{"type":"tool_use","id":"` + id + `","name":"Bash","input":` + input + `}`
}

func textBlock(s string) string { return `{"type":"text","text":"` + s + `"}` }

func result(id string, size int) string {
	return `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"` + id +
		`","content":"` + strings.Repeat("x", size) + `","is_error":false}]}}`
}

func prompt(s string) string { return `{"type":"user","message":{"content":"` + s + `"}}` }

// ledgerRun feeds lines to a fresh ledger and returns every cost it settled, in
// order, and the ledger.
func ledgerRun(t *testing.T, lines ...string) ([]ToolCost, *ToolContext) {
	t.Helper()
	var c ToolContext
	var got []ToolCost
	for i, l := range lines {
		r, ok := DecodeLine([]byte(l))
		if !ok {
			t.Fatalf("fixture line %d does not decode: %s", i, l)
		}
		for _, ev := range Events(r) {
			got = append(got, c.Observe(ev)...)
		}
	}
	return got, &c
}

func TestToolContextSingleCallIsMeasured(t *testing.T) {
	got, c := ledgerRun(t,
		prompt("list it"),
		// The FIRST turn of the transcript issues the call: context 1203 + 4511 + 20017 = 25731.
		asst(textBlock("Looking."), 1203, 4511, 20017),
		asst(toolUse("toolu_a", `{"command":"ls -la"}`), 1203, 4511, 20017),
		result("toolu_a", 2600),
		// The next turn: 7 + 3292 + 25731 = 29030. Growth 29030 − 25731 = 3299.
		asst(textBlock("Done."), 7, 3292, 25731),
	)
	want := []ToolCost{{ToolID: "toolu_a", Tokens: 3299, Kind: CostMeasured}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("costs:\n got %+v\nwant %+v", got, want)
	}
	if c.Measured() != 3299 {
		t.Fatalf("measured total %d, want 3299", c.Measured())
	}
}

func TestToolContextParallelCallsSplitTheMeasuredGrowth(t *testing.T) {
	got, c := ledgerRun(t,
		prompt("two at once"),
		// One message, two tool_use records, the same usage: 311 + 1789 + 40126 = 42226.
		asst(toolUse("toolu_p", `{"command":"ls -la"}`), 311, 1789, 40126),
		asst(toolUse("toolu_q", `{"command":"pwd -P"}`), 311, 1789, 40126),
		result("toolu_p", 380),
		result("toolu_q", 1180),
		// Next: 59 + 6113 + 42054 = 48226; growth 48226 − 42226 = 6000.
		asst(textBlock("Both done."), 59, 6113, 42054),
	)
	// Weights by bytes: p = 20 + 380 = 400, q = 20 + 1180 = 1200, total 1600.
	// p = 6000 × 400 / 1600 = 1500; q takes the remainder, 6000 − 1500 = 4500.
	want := []ToolCost{
		{ToolID: "toolu_p", Tokens: 1500, Kind: CostSplit, GroupTokens: 6000, GroupSize: 2},
		{ToolID: "toolu_q", Tokens: 4500, Kind: CostSplit, GroupTokens: 6000, GroupSize: 2},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("costs:\n got %+v\nwant %+v", got, want)
	}
	if c.Measured() != 6000 {
		t.Fatalf("measured total %d, want 6000", c.Measured())
	}
}

func TestToolContextSplitRemainderGoesToTheLastCall(t *testing.T) {
	got, _ := ledgerRun(t,
		asst(toolUse("toolu_r", `{"command":"ls -la"}`), 13, 2207, 50331), // 52551
		asst(toolUse("toolu_s", `{"command":"pwd -P"}`), 13, 2207, 50331),
		asst(toolUse("toolu_t", `{"command":"id -un"}`), 13, 2207, 50331),
		result("toolu_r", 101), result("toolu_s", 233), result("toolu_t", 619),
		asst(textBlock("Three."), 17, 1013, 52551), // 53581 → growth 1030
	)
	// Weights: r = 20+101 = 121, s = 20+233 = 253, t = 20+619 = 639; total 1013.
	// r = 1030×121/1013 = 123 (123.03), s = 1030×253/1013 = 257 (257.25),
	// t = 1030 − 123 − 257 = 650.
	want := []ToolCost{
		{ToolID: "toolu_r", Tokens: 123, Kind: CostSplit, GroupTokens: 1030, GroupSize: 3},
		{ToolID: "toolu_s", Tokens: 257, Kind: CostSplit, GroupTokens: 1030, GroupSize: 3},
		{ToolID: "toolu_t", Tokens: 650, Kind: CostSplit, GroupTokens: 1030, GroupSize: 3},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("costs:\n got %+v\nwant %+v", got, want)
	}
}

func TestToolContextWithoutUsageEstimatesFromSize(t *testing.T) {
	got, c := ledgerRun(t,
		prompt("no usage here"),
		asst(toolUse("toolu_e", `{"command":"cat a"}`), 0, 0, 0), // input 19 bytes
		result("toolu_e", 1238),
	)
	// (19 + 1238) / 4 = 314.25 → rounded up: 315.
	want := []ToolCost{{ToolID: "toolu_e", Tokens: 315, Kind: CostEstimate}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("costs:\n got %+v\nwant %+v", got, want)
	}
	if c.Measured() != 0 {
		t.Fatalf("an estimate counted as measured: %d", c.Measured())
	}
}

func TestToolContextAPromptBeforeTheNextTurnIsNotMeasured(t *testing.T) {
	got, c := ledgerRun(t,
		asst(toolUse("toolu_i", `{"command":"sleep"}`), 401, 977, 30210), // 31588
		result("toolu_i", 702),
		// The human typed before the model answered: the growth includes the prompt.
		prompt("stop, do something else"),
		asst(textBlock("OK."), 5, 1777, 31588), // 33370
	)
	// (19 + 702) / 4 = 180.25 → 181, an estimate.
	want := []ToolCost{{ToolID: "toolu_i", Tokens: 181, Kind: CostEstimate}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("costs:\n got %+v\nwant %+v", got, want)
	}
	if c.Measured() != 0 {
		t.Fatalf("measured total %d, want 0", c.Measured())
	}
}

func TestToolContextAShrinkingContextIsNotMeasured(t *testing.T) {
	got, _ := ledgerRun(t,
		asst(toolUse("toolu_k", `{"command":"echo"}`), 211, 3001, 90233), // 93445
		result("toolu_k", 66),
		asst(textBlock("After a compaction."), 3, 4403, 12007), // 16413: smaller
	)
	// (18 + 66) / 4 = 21, an estimate.
	want := []ToolCost{{ToolID: "toolu_k", Tokens: 21, Kind: CostEstimate}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("costs:\n got %+v\nwant %+v", got, want)
	}
}

func TestToolContextNothingUntilTheNextTurn(t *testing.T) {
	got, _ := ledgerRun(t,
		asst(toolUse("toolu_w", `{"command":"make"}`), 37, 821, 15013),
		result("toolu_w", 4409),
	)
	if len(got) != 0 {
		t.Fatalf("a call whose next turn has not happened got a cost: %+v", got)
	}
}

func TestToolContextAResultInsideTheIssuingMessage(t *testing.T) {
	// A CLI that runs a call while its message streams can write the result
	// between two of the message's records; both calls still belong to one
	// turn and are measured together.
	got, _ := ledgerRun(t,
		asst(toolUse("toolu_m", `{"command":"ls -la"}`), 23, 1409, 70111), // 71543
		result("toolu_m", 380),
		asst(toolUse("toolu_n", `{"command":"pwd -P"}`), 23, 1409, 70111),
		result("toolu_n", 1180),
		asst(textBlock("Both."), 41, 2961, 71543), // 74545 → growth 3002
	)
	// Weights 400 / 1200: m = 3002×400/1600 = 750 (750.5), n = 3002 − 750 = 2252.
	want := []ToolCost{
		{ToolID: "toolu_m", Tokens: 750, Kind: CostSplit, GroupTokens: 3002, GroupSize: 2},
		{ToolID: "toolu_n", Tokens: 2252, Kind: CostSplit, GroupTokens: 3002, GroupSize: 2},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("costs:\n got %+v\nwant %+v", got, want)
	}
}

func TestToolContextConsecutiveTurnsEachMeasureTheirOwn(t *testing.T) {
	got, c := ledgerRun(t,
		asst(toolUse("toolu_1", `{"command":"ls -la"}`), 19, 2003, 10111), // 12133
		result("toolu_1", 500),
		asst(toolUse("toolu_2", `{"command":"pwd -P"}`), 29, 917, 12133), // 13079 → toolu_1: 946
		result("toolu_2", 900),
		asst(textBlock("End."), 31, 1621, 13079), // 14731 → toolu_2: 1652
	)
	want := []ToolCost{
		{ToolID: "toolu_1", Tokens: 946, Kind: CostMeasured},
		{ToolID: "toolu_2", Tokens: 1652, Kind: CostMeasured},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("costs:\n got %+v\nwant %+v", got, want)
	}
	if c.Measured() != 946+1652 {
		t.Fatalf("measured total %d, want %d", c.Measured(), 946+1652)
	}
}

func TestToolContextAnUnansweredCallGetsNoCost(t *testing.T) {
	// Two calls, one result: the growth is the answered call's alone, and the
	// call with no result gets nothing (not a share, not an estimate).
	got, _ := ledgerRun(t,
		asst(toolUse("toolu_y", `{"command":"ls -la"}`), 43, 1301, 60127), // 61471
		asst(toolUse("toolu_x", `{"command":"pwd -P"}`), 43, 1301, 60127),
		result("toolu_y", 777),
		asst(textBlock("One came back."), 47, 2389, 61471), // 63907 → growth 2436
	)
	want := []ToolCost{{ToolID: "toolu_y", Tokens: 2436, Kind: CostMeasured}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("costs:\n got %+v\nwant %+v", got, want)
	}
}

func TestToolContextNoSizeAndNoUsageIsNoCost(t *testing.T) {
	// opencode's shape: a call with a target and no input text, a result with
	// no output, no usage anywhere. Nothing to measure or estimate by: no
	// cost, rather than "~+0".
	var c ToolContext
	got := c.Observe(Event{Kind: KindToolCall, ToolID: "oc_1", Tool: "read", Target: "a.go"})
	got = append(got, c.Observe(Event{Kind: KindToolResult, ToolID: "oc_1", OK: true})...)
	if len(got) != 0 {
		t.Fatalf("a call of unknown size got a cost: %+v", got)
	}
}
