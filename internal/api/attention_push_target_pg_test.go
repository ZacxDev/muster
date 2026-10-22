package api

import (
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestAnAskOverEachDoorPushesItsSession: on a real Postgres, through the real
// MCP doors (row-level security and all), an ask's push opens the asker's
// session — a mirrored Claude session's window (secondSID runs in ws's @3,
// session "side") and a dispatched agent's page (the chief's) — and a waiting
// window's push opens that window.
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
	mirrored := f.mirrorAsk(t, secondSID, "Keep the old schema?")
	if got, want := urls()[attentionTag(attentionAskKey(mirrored))], "/mirror/view?host=ws&session=side&window=%403"; got != want {
		t.Fatalf("a mirrored session's ask opens %q, want %q", got, want)
	}
	chiefAsk := f.ask(t, f.chiefTok, "Route task 5 to whom?")
	if got, want := urls()[attentionTag(attentionAskKey(chiefAsk))], "/agents/"+f.chief.Name; got != want {
		t.Fatalf("the chief's ask opens %q, want %q", got, want)
	}
	// ws's @1 starts waiting at a prompt after the host's first observation
	// (which seeds silently).
	f.setWindows(mirror.StatusBusy)
	f.srv.BroadcastAgentsChangedFor(1)
	f.setWindows(mirror.StatusWaiting)
	f.srv.BroadcastAgentsChangedFor(1)
	if got, want := urls()[attentionTag(sessionKey("ws", "main", "@1"))], "/mirror/view?host=ws&session=main&window=%401"; got != want {
		t.Fatalf("a waiting window opens %q, want %q (pushes %v)", got, want, f.pushes())
	}
}
