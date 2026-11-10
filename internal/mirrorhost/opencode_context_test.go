package mirrorhost

import (
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/opencodehost/opencodetest"
)

// THE CONTEXT READOUT ON AN OPENCODE VIEW'S STREAM (opencode_context.go): the
// view's events carry the session's context fill (mirror.Msg.Context) — the
// newest step's input + cache read + cache write, its model, and the model's
// window from GET /api/model — once after the backfill and again only when it
// moves. Literals worked out by hand; every token count pairwise distinct.

func ocStep(id string, completed int, model string, input, output, read, write int) map[string]any {
	m := ocAssistant(id, completed, ocText("step "+id))
	m["model"] = map[string]any{"id": model, "providerID": "fixture-provider"}
	m["tokens"] = map[string]any{"input": input, "output": output, "reasoning": 7, "cache": map[string]any{"read": read, "write": write}}
	return m
}

// contexts is every Context the watch id was sent, in order.
func contexts(d *drivenMuster, id uint32) []mirror.AgentContext {
	var out []mirror.AgentContext
	for _, m := range d.of(mirror.TypeEvents) {
		if m.ID == id && m.Context != nil {
			out = append(out, *m.Context)
		}
	}
	return out
}

// TestAnOpenCodeViewCarriesItsContextReadout: after the backfill, 3517 +
// 41203 + 2089 = 46809 of fixture-model's 187500; a later step (2345 + 7890 +
// 123 = 10358, on another-model the list does not name) moves it, its limit
// unknown (0); a poll that changes nothing sends nothing more.
func TestAnOpenCodeViewCarriesItsContextReadout(t *testing.T) {
	const sid = "ses_fixtureCtx"
	srv, src := ocServer(t, map[string]string{sid: "Context fixture"})
	srv.SetModels(opencodetest.Model{ProviderID: "fixture-provider", ID: "fixture-model", Context: 187500},
		opencodetest.Model{ProviderID: "other-provider", ID: "another-model", Context: 96000})
	srv.PutMessage(sid, ocUser("msg_01", 1700000000001, "go"))
	srv.PutMessage(sid, ocStep("msg_02", 5, "fixture-model", 3517, 611, 41203, 2089))
	streamUp(t, srv, src)
	wb := &windowsBackend{windows: []WindowInfo{ocWindowInfo(src, "OC | Context fixture")}}
	d := newDrivenMuster(t)
	d.run(t, NoShrink(ocBackend{wb, src}), nil, true)
	d.send(t, mirror.Msg{Type: mirror.TypeWatch, ID: 4, Session: "o", Window: "@7"})
	want1 := mirror.AgentContext{Tokens: 46809, Limit: 187500, Model: "fixture-model"}
	waitUntil(t, "the first readout", func() bool { c := contexts(d, 4); return len(c) == 1 && c[0] == want1 })

	srv.PutMessage(sid, ocUser("msg_03", 1700000000003, "more"))
	srv.PutMessage(sid, ocStep("msg_04", 9, "another-model", 2345, 67, 7890, 123))
	want2 := mirror.AgentContext{Tokens: 10358, Model: "another-model"} // fixture-provider lists no another-model
	waitUntil(t, "the moved readout", func() bool { c := contexts(d, 4); return len(c) == 2 && c[1] == want2 })
	// A message with no usage (a prompt) moves nothing.
	srv.PutMessage(sid, ocUser("msg_05", 1700000000005, "and again"))
	waitUntil(t, "the prompt drawn", func() bool {
		evs, _, _ := watchEvents(d, 4)
		return len(evs) > 0 && evs[len(evs)-1].Text == "and again"
	})
	if c := contexts(d, 4); len(c) != 2 {
		t.Fatalf("readouts %+v; a prompt with no usage sent another", c)
	}
}

// TestAnOpenCodeViewWithNoStepSendsNoReadout: a session with no assistant
// message: no Context at all (the page's readout stays hidden).
func TestAnOpenCodeViewWithNoStepSendsNoReadout(t *testing.T) {
	const sid = "ses_fixtureNone"
	srv, src := ocServer(t, map[string]string{sid: "No step fixture"})
	srv.PutMessage(sid, ocUser("msg_01", 1700000000001, "just a prompt"))
	streamUp(t, srv, src)
	wb := &windowsBackend{windows: []WindowInfo{ocWindowInfo(src, "OC | No step fixture")}}
	d := newDrivenMuster(t)
	d.run(t, NoShrink(ocBackend{wb, src}), nil, true)
	d.send(t, mirror.Msg{Type: mirror.TypeWatch, ID: 4, Session: "o", Window: "@7"})
	waitUntil(t, "the backfill", func() bool { evs, _, _ := watchEvents(d, 4); return len(evs) >= 2 })
	if c := contexts(d, 4); len(c) != 0 {
		t.Fatalf("a session with no step sent %+v", c)
	}
}
