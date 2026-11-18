package mirrorhost

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/opencodehost"
)

// AN opencode DISPATCH'S FINAL REPLY, HOST HALF (opencode_outcome.go): which
// message is the final reply, which windows are read, once per change, and the
// outcomes message after the window list.

func ocMsg(t *testing.T, raw string) opencodehost.Message {
	t.Helper()
	var m opencodehost.Message
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestTheFinalReplyIsTheNewestCompletedAssistantMessage: the mark is the
// newest assistant message's id and the text the newest assistant text after
// the person's last prompt; nothing is judged while the newest assistant
// message streams, while a prompt is unanswered, or with no assistant message.
func TestTheFinalReplyIsTheNewestCompletedAssistantMessage(t *testing.T) {
	user := `{"id":"msg_u1","type":"user","text":"Fix the README","time":{"created":1}}`
	textA := `{"id":"msg_a2","type":"assistant","time":{"created":2,"completed":3},"content":[{"type":"text","text":"Opened the PR.\nMUSTER-DONE: fixed it"}]}`
	toolOnly := `{"id":"msg_a3","type":"assistant","time":{"created":4,"completed":5},"content":[{"type":"tool","id":"c1","name":"shell","state":{"status":"completed","input":{}}}]}`
	streaming := `{"id":"msg_a4","type":"assistant","time":{"created":6},"content":[{"type":"text","text":"half"}]}`
	idle := `{"id":"msg_i5","type":"idle","outcome":"succeeded","time":{"created":7}}`
	user2 := `{"id":"msg_u6","type":"user","text":"And the docs?","time":{"created":8}}`
	cases := []struct {
		why        string
		msgs       []string
		mark, text string
		ok         bool
	}{
		{"a finished reply", []string{user, textA}, "msg_a2", "Opened the PR.\nMUSTER-DONE: fixed it", true},
		{"an idle marker after it", []string{user, textA, idle}, "msg_a2", "Opened the PR.\nMUSTER-DONE: fixed it", true},
		{"a tool-only step after the text: the mark is the newest, the text the newest text", []string{user, textA, toolOnly}, "msg_a3", "Opened the PR.\nMUSTER-DONE: fixed it", true},
		{"a tool-only reply alone: no reply text", []string{user, toolOnly}, "msg_a3", "", true},
		{"still streaming", []string{user, textA, streaming}, "", "", false},
		{"a newer prompt not answered yet", []string{user, textA, user2}, "", "", false},
		{"only the prompt", []string{user}, "", "", false},
		{"nothing", nil, "", "", false},
	}
	for _, c := range cases {
		var msgs []opencodehost.Message
		for _, raw := range c.msgs {
			msgs = append(msgs, ocMsg(t, raw))
		}
		mark, text, ok := finalReply(msgs)
		if mark != c.mark || text != c.text || ok != c.ok {
			t.Errorf("%s: (%q, %q, %v), want (%q, %q, %v)", c.why, mark, text, ok, c.mark, c.text, c.ok)
		}
	}
}

// ocDispatchWindow is an opencode window the chief started (dispatch 61),
// launcher-bound to sid, with status.
func ocDispatchWindow(id, sid, status string, dispatch int64) WindowInfo {
	return WindowInfo{Session: "agents", ID: id, Index: 1, Pane: "%" + strings.TrimPrefix(id, "@"), PanePID: 61, Command: "opencode",
		Dispatch: dispatch, Agent: &AgentPane{Runtime: mirror.RuntimeOpenCode, Pane: "%" + strings.TrimPrefix(id, "@"), Gen: sid,
			Status: status, Send: true}}
}

// TestOnlyAnIdleDispatchWindowsSessionIsReadAndOnlyOncePerChange: of an idle
// dispatch window, a busy one, an idle opencode window nobody dispatched and a
// title-matched (not launcher-bound) dispatch window, only the first is read;
// a second listing with nothing changed reads nothing; a new message is read.
func TestOnlyAnIdleDispatchWindowsSessionIsReadAndOnlyOncePerChange(t *testing.T) {
	const sidA, sidB, sidC, sidD = "ses_outcomeAAAA", "ses_outcomeBBBB", "ses_outcomeCCCC", "ses_outcomeDDDD"
	srv, src := ocServer(t, map[string]string{sidA: "a", sidB: "b", sidC: "c", sidD: "d"})
	// The event stream is what moves a session's change count (a listing's
	// resolve opens it in production).
	src.state()
	waitUntil(t, "the event stream", func() bool { st, _ := src.status(sidA); return st != "" })
	base := src.changed(sidA)
	for _, sid := range []string{sidA, sidB, sidC, sidD} {
		srv.PutMessage(sid, map[string]any{"id": "msg_" + sid[4:], "type": "assistant", "time": map[string]any{"created": 1, "completed": 2},
			"content": []map[string]any{{"type": "text", "text": "Which branch?\nreply of " + sid}}})
	}
	// Its event has arrived (and moved the count) before the first read.
	waitUntil(t, "the message's event", func() bool { return src.changed(sidA) > base })
	titled := ocDispatchWindow("@64", sidD, mirror.StatusIdle, 64)
	titled.Agent.Send = false
	windows := []WindowInfo{
		ocDispatchWindow("@61", sidA, mirror.StatusIdle, 61),
		ocDispatchWindow("@62", sidB, mirror.StatusBusy, 62),
		ocDispatchWindow("@63", sidC, mirror.StatusIdle, 0),
		titled,
	}
	reads := func() map[string]int {
		n := map[string]int{}
		for _, r := range srv.Requests() {
			if strings.HasSuffix(r.Path, "/message") {
				n[strings.TrimSuffix(strings.TrimPrefix(r.Path, "/api/session/"), "/message")]++
			}
		}
		return n
	}
	got := src.dispatchOutcomes(windows)
	if len(got) != 1 || got["@61"].Mark != "msg_outcomeAAAA" || fmt.Sprint(got["@61"].Lines) != fmt.Sprint([]string{"Which branch?", "reply of " + sidA}) {
		t.Fatalf("outcomes %+v", got)
	}
	if n := reads(); n[sidA] != 1 || len(n) != 1 {
		t.Fatalf("reads %v, want one of %s only", n, sidA)
	}
	if again := src.dispatchOutcomes(windows); again["@61"].Mark != "msg_outcomeAAAA" {
		t.Fatalf("the remembered read: %+v", again)
	}
	if n := reads(); n[sidA] != 1 {
		t.Fatalf("an unchanged session was read again: %v", n)
	}
	// A new reply (the event moves the session's change count): read again.
	srv.PutMessage(sidA, map[string]any{"id": "msg_outcomeAAA2", "type": "assistant", "time": map[string]any{"created": 3, "completed": 4},
		"content": []map[string]any{{"type": "text", "text": "MUSTER-DONE: done now"}}})
	waitUntil(t, "the new reply read", func() bool {
		return src.dispatchOutcomes(windows)["@61"].Mark == "msg_outcomeAAA2"
	})
}

// TestADispatchHostSendsItsOutcomesAfterItsWindows: with an opencode source, a
// per-window host sends one outcomes message after the hello naming the idle
// dispatch window's final reply, nothing more while nothing changes, and an
// empty one once the window goes busy (it clears muster's).
func TestADispatchHostSendsItsOutcomesAfterItsWindows(t *testing.T) {
	const sid = "ses_outcomeSEND"
	srv, src := ocServer(t, map[string]string{sid: "send"})
	srv.PutMessage(sid, map[string]any{"id": "msg_outcomeSEND", "type": "assistant", "time": map[string]any{"created": 1, "completed": 2},
		"content": []map[string]any{{"type": "text", "text": "All set.\nMUSTER-DONE: opened the draft PR"}}})
	d := newDrivenMuster(t)
	wb := &windowsBackend{windows: []WindowInfo{ocDispatchWindow("@71", sid, mirror.StatusIdle, 71)}}
	d.run(t, NoShrink(ocBackend{wb, src}), nil, true)
	waitUntil(t, "the first outcomes message", func() bool { return len(d.of(mirror.TypeOutcomes)) == 1 })
	got := d.of(mirror.TypeOutcomes)[0].Outcomes
	if len(got) != 1 || got["@71"].Mark != "msg_outcomeSEND" || fmt.Sprint(got["@71"].Lines) != fmt.Sprint([]string{"All set.", "MUSTER-DONE: opened the draft PR"}) {
		t.Fatalf("outcomes = %+v", got)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(d.of(mirror.TypeOutcomes)); n != 1 {
		t.Fatalf("%d outcomes messages with nothing changed, want 1", n)
	}
	wb.set(ocDispatchWindow("@71", sid, mirror.StatusBusy, 71))
	waitUntil(t, "the second outcomes message", func() bool { return len(d.of(mirror.TypeOutcomes)) == 2 })
	if got := d.of(mirror.TypeOutcomes)[1].Outcomes; len(got) != 0 {
		t.Fatalf("outcomes after the window went busy = %v, want empty", got)
	}
}
