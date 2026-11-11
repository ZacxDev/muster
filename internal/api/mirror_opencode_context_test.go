package api

import (
	"testing"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// AN OPENCODE VIEW'S CONTEXT READOUT, through the real socket handler
// (mirror_view.go): a host's mirror.Msg.Context reaches the page as the
// batch's context — the chip's parts (its short label, the model's name,
// the count of the window, the exact detail, the tint) and the title saying
// how the count is derived — and a batch from an OLDER host, which
// sends none, carries no readout. No database: the fake mirror suffices.

func readoutOf(t *testing.T, batches ...mirror.Msg) map[string]any {
	t.Helper()
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{batches: batches}}
	s, ts := viewServer(t, fm)
	c, _, err := dialTerm(t, s, ts, ui.MirrorViewWSPath("ws", "main"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	var last map[string]any
	for range batches {
		b := readBatch(t, c)
		if ctx, ok := b["context"].(map[string]any); ok {
			last = ctx
		}
	}
	return last
}

// TestAHostsContextReachesThePageAsTheReadout: the backfill, then the
// host's context marker (the order mirrorhost's opencode tail sends them).
func TestAHostsContextReachesThePageAsTheReadout(t *testing.T) {
	got := readoutOf(t,
		mirror.Msg{Type: mirror.TypeEvents, Reset: true, Gen: "ses_fixtureA", Offset: 2, Events: []cctranscript.Event{{Kind: cctranscript.KindText, Text: "hi"}}},
		mirror.Msg{Type: mirror.TypeEvents, Gen: "ses_fixtureA", Offset: 2, Context: &mirror.AgentContext{Tokens: 146809, Limit: 187500, Model: "fixtureprov/fixture-model"}},
	)
	// Every part the page places (ui.ContextView), as literals: the name
	// without its provider, the short count, the exact detail, the tint
	// (78% of the window: warn).
	want := map[string]any{
		"tokens": float64(146809),
		"limit":  float64(187500),
		"model":  "fixtureprov/fixture-model",
		"label":  "fixture-model · 147k/188k",
		"name":   "fixture-model",
		"count":  "147k/188k",
		"detail": "fixtureprov/fixture-model — 146,809 of 187,500 tokens (78%)",
		"level":  "warn",
		"title":  ui.AgentContextTitle,
	}
	if got == nil || len(got) != len(want) {
		t.Fatalf("the readout: %+v, want %+v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("the readout's %s = %#v, want %#v (readout %+v)", k, got[k], v, got)
		}
	}
}

// TestAnOlderHostsBatchCarriesNoReadout: the same batches with no Context
// (an older host's) draw no readout at all.
func TestAnOlderHostsBatchCarriesNoReadout(t *testing.T) {
	if got := readoutOf(t,
		mirror.Msg{Type: mirror.TypeEvents, Reset: true, Gen: "ses_fixtureA", Offset: 2, Events: []cctranscript.Event{{Kind: cctranscript.KindText, Text: "hi"}}},
		mirror.Msg{Type: mirror.TypeEvents, Gen: "ses_fixtureA", Offset: 3, Events: []cctranscript.Event{{Kind: cctranscript.KindText, Text: "more"}}},
	); got != nil {
		t.Fatalf("an older host's batches drew a readout: %+v", got)
	}
}
