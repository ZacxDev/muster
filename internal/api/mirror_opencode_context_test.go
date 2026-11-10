package api

import (
	"testing"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// AN OPENCODE VIEW'S CONTEXT READOUT, through the real socket handler
// (mirror_view.go): a host's mirror.Msg.Context reaches the page as the
// batch's context — model, count and window in one label, and the tooltip
// saying how the count is derived — and a batch from an OLDER host, which
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
		mirror.Msg{Type: mirror.TypeEvents, Gen: "ses_fixtureA", Offset: 2, Context: &mirror.AgentContext{Tokens: 46809, Limit: 187500, Model: "fixture-model"}},
	)
	if got == nil || got["label"] != "fixture-model · 47k / 188k context" || got["tokens"] != float64(46809) ||
		got["limit"] != float64(187500) || got["model"] != "fixture-model" || got["title"] != ui.AgentContextTitle {
		t.Fatalf("the readout: %+v", got)
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
