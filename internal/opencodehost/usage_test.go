package opencodehost

import (
	"encoding/json"
	"testing"

	"github.com/ZacxDev/muster/internal/opencodehost/opencodetest"
)

// THE CONTEXT FILL (usage.go): every expected value below is a literal worked
// out by hand from the fixture's numbers, never computed by the code under
// test. Each fixture's token counts are pairwise distinct and distinct from
// every constant the code names, so a dropped, doubled or swapped term moves
// the figure.

func msgs(t *testing.T, ss ...string) []Message {
	t.Helper()
	out := make([]Message, 0, len(ss))
	for _, s := range ss {
		out = append(out, msg(t, s))
	}
	return out
}

// TestTheContextFillCountsInputAndBothCacheTermsOnly: input 3517 + cache
// read 41203 + cache write 2089 = 46809. Output (611) and reasoning (97) are
// not counted: what a step wrote is in the next step's input.
func TestTheContextFillCountsInputAndBothCacheTermsOnly(t *testing.T) {
	got, ok := ContextOf(msgs(t,
		`{"id":"msg_1","type":"user","text":"go","time":{"created":1}}`,
		`{"id":"msg_2","type":"assistant","model":{"id":"fixture-model","providerID":"fixture-provider"},"finish":"stop",
		  "time":{"created":2,"completed":3},"tokens":{"input":3517,"output":611,"reasoning":97,"cache":{"read":41203,"write":2089}}}`))
	if !ok || got.Tokens != 46809 || got.ModelID != "fixture-model" || got.ProviderID != "fixture-provider" {
		t.Fatalf("ContextOf = %+v, %v; want 46809 tokens of fixture-provider/fixture-model", got, ok)
	}
}

// TestTheNewestStepThatReportedUsageCounts: of two completed steps, the
// newer's (2345 + 7890 + 123 = 10358, not the older's 1234 + 5678 + 0 =
// 6912) — and a newer step still streaming (no tokens) or reporting zero is
// skipped, so the readout holds the last completed step's figure, on ITS
// model.
func TestTheNewestStepThatReportedUsageCounts(t *testing.T) {
	older := `{"id":"msg_2","type":"assistant","model":{"id":"older-model","providerID":"p1"},"finish":"tool-calls","time":{"created":2,"completed":3},
		"tokens":{"input":1234,"output":45,"reasoning":0,"cache":{"read":5678,"write":0}}}`
	newer := `{"id":"msg_3","type":"assistant","model":{"id":"newer-model","providerID":"p2"},"finish":"stop","time":{"created":4,"completed":5},
		"tokens":{"input":2345,"output":67,"reasoning":8,"cache":{"read":7890,"write":123}}}`
	streaming := `{"id":"msg_4","type":"assistant","model":{"id":"streaming-model","providerID":"p3"},"time":{"created":6},"content":[{"type":"text","text":"Wor"}]}`
	zero := `{"id":"msg_5","type":"assistant","model":{"id":"zero-model","providerID":"p4"},"time":{"created":7},
		"tokens":{"input":0,"output":0,"reasoning":0,"cache":{"read":0,"write":0}}}`
	idle := `{"id":"msg_6","type":"idle","outcome":"succeeded","time":{"created":8}}`
	for name, in := range map[string][]string{
		"two steps":                  {older, newer},
		"then one streaming":         {older, newer, streaming},
		"then one reporting zero":    {older, newer, zero},
		"then its idle":              {older, newer, idle},
		"then streaming, zero, idle": {older, newer, streaming, zero, idle},
	} {
		got, ok := ContextOf(msgs(t, in...))
		if !ok || got.Tokens != 10358 || got.ModelID != "newer-model" || got.ProviderID != "p2" {
			t.Fatalf("%s: ContextOf = %+v, %v; want 10358 tokens of p2/newer-model", name, got, ok)
		}
	}
	if got, ok := ContextOf(msgs(t, older)); !ok || got.Tokens != 6912 {
		t.Fatalf("control: the older step alone = %+v, %v; want 6912", got, ok)
	}
}

// TestASessionWithNoAssistantMessageHasNoContext: no readout at all (false),
// for no messages, prompts only, and steps that never reported usage.
func TestASessionWithNoAssistantMessageHasNoContext(t *testing.T) {
	for name, in := range map[string][]string{
		"none":         nil,
		"prompts only": {`{"id":"msg_1","type":"user","text":"hello","time":{"created":1}}`},
		"a streaming step": {`{"id":"msg_1","type":"user","text":"hello","time":{"created":1}}`,
			`{"id":"msg_2","type":"assistant","time":{"created":2},"content":[]}`},
	} {
		if got, ok := ContextOf(msgs(t, in...)); ok || got != (Context{}) {
			t.Fatalf("%s: ContextOf = %+v, %v; want none", name, got, ok)
		}
	}
}

// TestTheContextFillOfTheCapturedSession: the real server's messages
// (testdata/prompt-v2.0.32.json): the newest step, "second", reported input
// 91 and cache read 5248, so 5339 on big-pickle — the older step's 3533 +
// 1792 = 5325 is not it.
func TestTheContextFillOfTheCapturedSession(t *testing.T) {
	var page struct {
		Data []Message `json:"data"`
	}
	if err := json.Unmarshal(opencodetest.Captured("messages"), &page); err != nil {
		t.Fatal(err)
	}
	// The capture is newest first (order=desc); ContextOf reads oldest first.
	for i, j := 0, len(page.Data)-1; i < j; i, j = i+1, j-1 {
		page.Data[i], page.Data[j] = page.Data[j], page.Data[i]
	}
	got, ok := ContextOf(page.Data)
	if !ok || got.Tokens != 5339 || got.ModelID != "big-pickle" || got.ProviderID != "opencode" {
		t.Fatalf("ContextOf(captured) = %+v, %v; want 5339 tokens of opencode/big-pickle", got, ok)
	}
}

// TestTheContextLimitIsTheNamedModelsWindow: limit.context of the model the
// step ran on, matched by provider AND id; 0 (unknown) for a model the list
// does not name, one listed with no limit, or no model at all.
func TestTheContextLimitIsTheNamedModelsWindow(t *testing.T) {
	var models []Model
	if err := json.Unmarshal([]byte(`[
		{"id":"fixture-model","modelID":"fixture-model","providerID":"fixture-provider","limit":{"context":187500,"output":9001}},
		{"id":"fixture-model","modelID":"fixture-model","providerID":"other-provider","limit":{"context":96000,"output":9002}},
		{"id":"vendor/long-name","modelID":"vendor/long-name","providerID":"fixture-provider","limit":{"context":333333}},
		{"id":"nolimit-model","providerID":"fixture-provider"}]`), &models); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		provider, id string
		want         int64
	}{
		{"fixture-provider", "fixture-model", 187500},
		{"other-provider", "fixture-model", 96000},
		{"fixture-provider", "vendor/long-name", 333333},
		{"fixture-provider", "nolimit-model", 0},
		{"fixture-provider", "unlisted-model", 0},
		{"unlisted-provider", "fixture-model", 0},
		{"fixture-provider", "", 0},
	} {
		if got := ContextLimit(models, c.provider, c.id); got != c.want {
			t.Errorf("ContextLimit(%s/%s) = %d, want %d", c.provider, c.id, got, c.want)
		}
	}
}
