package mirror_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// HERMES SUPPORT WAS REMOVED, AND AN OLDER muster-mirror MAY STILL SPEAK IT.
// Hosts run a pinned muster-mirror, so a newer muster must take an older
// host's Hermes-shaped wire without an error: its hello flags (hermesSend,
// hermesAnswer), a window it names as Runtime "hermes" with a session and a
// Hermes approval pending, and its paste guard's "hermes" refusal kind.

// oldHermesHello is what a muster-mirror from before the removal sends with
// -interactive -hermes-send -hermes-answer: @1 a Hermes window waiting on an
// approval muster could once answer, @2 a Claude Code window.
const oldHermesHello = `{"type":"hello","version":2,"host":"old","interactive":true,"panePaste":true,"statusReport":true,` +
	`"hermesSend":true,"hermesAnswer":true,"windows":[` +
	`{"session":"work","id":"@1","index":0,"active":true,"command":"python3.14","runtime":"hermes",` +
	`"agentSession":"local-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",` +
	`"status":"waiting","statusSince":1792000001111,"agentNote":"a Hermes note",` +
	`"pending":{"summary":"terminal: rm -r ./build","tool":"terminal","kind":"approval","promptId":"p1","choices":"once deny"}},` +
	`{"session":"work","id":"@2","index":1,"claude":true,"claudeSession":"0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b","status":"idle","statusSince":1792000002222}]}`

// oldHermesRefusalText is that host's paste guard's refusal, word for word.
const oldHermesRefusalText = "Not sent: Hermes runs in this pane. muster sends to Hermes only from its window's rendered view, " +
	"and only on a host whose muster-mirror runs with -hermes-send, because Enter there can answer a prompt Hermes is showing, " +
	"such as a command approval. Type it in the terminal."

// TestAnOlderHostsHermesWireIsIgnoredSafely: the older host's hello is
// accepted; its Hermes window is listed as a PLAIN TERMINAL window — no
// runtime, no session, no status, no pending ask, no note — so nothing
// offers a view, a send or an answer for it; its agent state is "no agent
// here" (ErrNotClaude), never an error of its own; the rendered view's send
// to it is refused before the host; a Terminal paste reaches the host (whose
// own guard still refuses Hermes), and that refusal's removed "hermes" kind
// comes back as a PLAIN refusal in the host's words. The Claude Code window
// beside it is untouched (the control).
func TestAnOlderHostsHermesWireIsIgnoredSafely(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithTimeout(asUser(1), 5*time.Second)
	defer cancel()
	tok, _ := r.store.mint(t, 1, "old")
	h := connectScripted(t, r, tok, oldHermesHello, func(m mirror.Msg) mirror.Msg {
		if m.Window == "@1" {
			return mirror.Msg{Error: oldHermesRefusalText, Refusal: "hermes"}
		}
		return mirror.Msg{}
	})
	r.online(t, 1, "old", "work")

	var w1, w2 *mirror.Window
	for _, hv := range r.svc.Hosts(ctx) {
		if hv.Host != "old" {
			continue
		}
		for i := range hv.Windows {
			switch hv.Windows[i].ID {
			case "@1":
				w1 = &hv.Windows[i]
			case "@2":
				w2 = &hv.Windows[i]
			}
		}
	}
	if w1 == nil || w2 == nil {
		t.Fatalf("the older host's windows were not listed: @1 %+v, @2 %+v", w1, w2)
	}
	if w1.Runtime != "" || w1.AgentRuntime() != "" || w1.AgentSession != "" || w1.Status != "" || w1.StatusSince != 0 ||
		w1.Pending != nil || w1.AgentNote != "" || w1.Claude || w1.SessionKey() != "" {
		t.Fatalf("the older host's Hermes window = %+v; want a plain terminal window", *w1)
	}
	if !w2.Claude || w2.Status != mirror.StatusIdle {
		t.Fatalf("CONTROL: the Claude Code window beside it = %+v", *w2)
	}
	if st := r.svc.AgentState(ctx, "old", "work", "@1"); !errors.Is(st.Unknown, mirror.ErrNotClaude) || st.Runtime != "" {
		t.Fatalf("the Hermes window's agent state = %+v; want ErrNotClaude, as any non-agent window", st)
	}
	if err := r.svc.PasteWindow(ctx, "old", "work", "@1", "into Hermes", mirror.PasteClaude); !errors.Is(err, mirror.ErrNotClaude) {
		t.Fatalf("a rendered-view send to the Hermes window = %v; want ErrNotClaude before the host", err)
	}
	err := r.svc.PasteWindow(ctx, "old", "work", "@1", "into Hermes", mirror.PasteActivePane)
	var pr *mirror.PasteRefusedError
	if !errors.Is(err, mirror.ErrPasteRefused) || !errors.As(err, &pr) || pr.Why != oldHermesRefusalText || pr.Refusal != "hermes" {
		t.Fatalf("a Terminal paste the older host refused as hermes = %v (%+v); want ErrPasteRefused in its words", err, pr)
	}
	if k := mirror.PasteGuardRefusal(err); k != "" {
		t.Fatalf("the removed hermes kind was read as a guard kind %q; want a plain refusal", k)
	}
	if err := r.svc.PasteWindow(ctx, "old", "work", "@2", "into Claude", mirror.PasteActivePane); err != nil {
		t.Fatalf("CONTROL: a paste into the Claude Code window: %v", err)
	}
	if got := h.got(); len(got) != 2 || got[0].Window != "@1" || got[0].Runtime != "" || got[1].Window != "@2" {
		t.Fatalf("the host received %+v; want the Terminal paste into @1 (with no runtime) and the control into @2", got)
	}
}
