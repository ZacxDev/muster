package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
)

// tap is the operator tapping key's notification: GET /attention/open/<key>,
// and where it redirects.
func (f *answerFixture) tap(t *testing.T, key string) string {
	t.Helper()
	w := f.call(t, http.MethodGet, attentionOpenURL(key), nil, f.operator())
	if w.Code != http.StatusSeeOther {
		t.Fatalf("tap %s: %d %s", key, w.Code, w.Body)
	}
	return w.Header().Get("Location")
}

// TestAnAskOverEachDoorPushesItsSession: on a real Postgres, through the real
// MCP doors (row-level security and all), each push carries its tap route; a
// tap on an ask opens the ask's own page, whose context link is the asker's
// session — a mirrored Claude session's window (secondSID runs in ws's @3,
// session "side"), a dispatched agent's page (the chief's) — and a tap on a
// waiting window opens that window.
func TestAnAskOverEachDoorPushesItsSession(t *testing.T) {
	f := newAnswerFixture(t)
	urls := func() map[string]string {
		f.srv.WaitForPushes()
		out := map[string]string{}
		for _, p := range f.pusher.to(ep("attn-operator")) {
			if p.Type == attentionNotificationType {
				out[p.Tag] = p.Data["url"]
			}
		}
		return out
	}
	mirroredID := f.mirrorAsk(t, secondSID, "Keep the old schema?")
	chiefID := f.ask(t, f.chiefTok, "Route task 5 to whom?")
	mirrored, chiefAsk := attentionAskKey(mirroredID), attentionAskKey(chiefID)
	got := urls()
	for _, k := range []string{mirrored, chiefAsk} {
		if got[attentionTag(k)] != "/attention/open/"+k {
			t.Fatalf("%s's push carries %q (pushes %v)", k, got[attentionTag(k)], got)
		}
	}
	for _, c := range []struct {
		key     string
		id      int64
		context string
	}{
		{mirrored, mirroredID, "/mirror/view?host=ws&amp;session=side&amp;window=%403"},
		{chiefAsk, chiefID, "/agents/" + f.chief.Name},
	} {
		loc := f.tap(t, c.key)
		if loc != askPagePath(c.id) {
			t.Errorf("a tap on %s opens %q, want its ask page %q", c.key, loc, askPagePath(c.id))
			continue
		}
		w := f.call(t, http.MethodGet, loc, nil, f.operator())
		// "View in session" for a mirrored session and an agent alike (the
		// operator's words; it read "Open the session/agent for context").
		if w.Code != http.StatusOK || askPageLink(w.Body.String()) != c.context {
			t.Errorf("%s's ask page (%d) does not link its context %q as %q:\n%s", c.key, w.Code, c.context, askViewInSession, w.Body.String())
		}
	}
	// ws's @1 starts waiting at a prompt after the host's first observation
	// (which seeds silently).
	f.setWindows(mirror.StatusBusy)
	f.srv.BroadcastAgentsChangedFor(1)
	f.setWindows(mirror.StatusWaiting)
	f.srv.BroadcastAgentsChangedFor(1)
	waiting := sessionKey("ws", "main", "@1")
	if got := urls()[attentionTag(waiting)]; got != "/attention/open/"+waiting {
		t.Fatalf("the waiting window's push carries %q", got)
	}
	if loc, want := f.tap(t, waiting), "/mirror/view?host=ws&session=main&window=%401"; loc != want {
		t.Errorf("a tap on the waiting window opens %q, want %q", loc, want)
	}
}

// TestATapResolvesWhenTappedNotWhenSent: what changed between the push and
// the tap decides where the tap lands — the waiting window stopped waiting,
// the asking agent was destroyed (its ask goes with it), the ask was answered:
// each opens its item on the queue, never a dead page; an ask still open
// whose asking session's window closed still opens its own page. Another
// owner's ask reads exactly like a missing one (the queue, naming nothing of
// theirs), and a key that is not one is a 404.
func TestATapResolvesWhenTappedNotWhenSent(t *testing.T) {
	f := newAnswerFixture(t)
	mirroredID := f.mirrorAsk(t, secondSID, "Keep the old schema?")
	chiefID := f.ask(t, f.chiefTok, "Route task 5 to whom?")
	mirrored, chiefAsk := attentionAskKey(mirroredID), attentionAskKey(chiefID)
	answered := f.ask(t, f.opTok, "Answered elsewhere?")
	// While everything still stands, each ask opens its own page (the control).
	for k, id := range map[string]int64{mirrored: mirroredID, chiefAsk: chiefID, attentionAskKey(answered): answered} {
		if loc := f.tap(t, k); loc != askPagePath(id) {
			t.Fatalf("control: %s opens %q", k, loc)
		}
	}
	// ws's @1 starts waiting; the window list it is read from no longer has
	// @3, so the window running secondSID has CLOSED since its ask was sent.
	f.setWindows(mirror.StatusBusy)
	f.srv.BroadcastAgentsChangedFor(1)
	f.setWindows(mirror.StatusWaiting)
	f.srv.BroadcastAgentsChangedFor(1)
	waiting := sessionKey("ws", "main", "@1")
	f.srv.WaitForPushes()
	if loc := f.tap(t, waiting); !strings.HasPrefix(loc, "/mirror/view?") {
		t.Fatalf("control: %s opens %q", waiting, loc)
	}

	// @1 stops waiting, the chief is destroyed, and the third ask is answered
	// on another device.
	f.setWindows(mirror.StatusBusy)
	if _, err := f.pool.Exec(authz.System(context.Background()), `DELETE FROM agents WHERE id = $1`, f.chief.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.answerAsk(asUser(1), answered, "done"); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{waiting, chiefAsk, attentionAskKey(answered)} {
		if loc := f.tap(t, k); loc != "/attention#"+k {
			t.Errorf("a tap on %s after it went stale opens %q, want its queue item", k, loc)
		}
	}
	// Still open, its window gone: its page, with no session to link.
	if loc := f.tap(t, mirrored); loc != askPagePath(mirroredID) {
		t.Errorf("a tap on the open ask whose window closed opens %q", loc)
	} else if w := f.call(t, http.MethodGet, loc, nil, f.operator()); w.Code != http.StatusOK || strings.Contains(w.Body.String(), "data-ask-context") {
		t.Errorf("its page (%d) links a session that is gone:\n%s", w.Code, w.Body.String())
	}

	// Another owner's ask: the queue, exactly as for a missing id.
	theirs := attentionAskKey(f.ask(t, f.workerTok, "A member's question?"))
	missing := "ask-987654321"
	if loc := f.tap(t, theirs); loc != "/attention#"+theirs || strings.Contains(loc, f.worker.Name) {
		t.Errorf("another owner's ask opens %q", loc)
	}
	if loc := f.tap(t, missing); loc != "/attention#"+missing {
		t.Errorf("a missing ask opens %q", loc)
	}
	for _, bad := range []string{"draft-1", "ask-0", "ask-1x", "session-zz", "ask-1%0d%0aSet-Cookie:x=1"} {
		if w := f.call(t, http.MethodGet, attentionOpenPath+bad, nil, f.operator()); w.Code != http.StatusNotFound {
			t.Errorf("GET %s%s: %d, want 404", attentionOpenPath, bad, w.Code)
		}
	}
}
