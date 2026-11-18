package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/push"
)

// TestEveryPageVersionPushesItsOwnerUnderTheSendersCap: page_update pushes
// "Page updated: <title> (v<N>)", opening THAT version's preview, under the
// page_updated toggle, one tag per page (a newer version replaces the older on
// the device) — at most notifySenderBurst per notifySenderWindow from one
// sender, each sender its own bucket, and every drop past it logged. The
// page_create push ("Page ready") is unchanged and is not spent by updates.
//
// 🔴 REGRESSION TEST: at the base page_update pushed nothing at all.
func TestEveryPageVersionPushesItsOwnerUnderTheSendersCap(t *testing.T) {
	f := newPageFixture(t)
	store, pusher, logs := &memPush{}, &payloadPusher{}, &syncBuffer{}
	f.srv.ext.Push = push.NewService(store, pusher, push.VAPIDConfig{PublicKey: "k"}, log.New(io.Discard, "", 0))
	f.srv.logger = log.New(logs, "", 0)
	now := time.Unix(5_000_000, 0)
	f.srv.now = func() time.Time { return now }
	dev := ep("page-owner")
	claim(t, store, f.owner, dev)
	tagged := func(tag string) []push.Payload {
		f.srv.WaitForPushes()
		var out []push.Payload
		for _, p := range pusher.to(dev) {
			if p.Tag == tag {
				out = append(out, p)
			}
		}
		return out
	}
	update := func(c toolCaller, ctx context.Context, id int64, text string) map[string]any {
		return f.toolAs(t, ctx, c, pageUpdateTool, map[string]any{"page_id": id, "blocks": json.RawMessage(`[{"type":"markdown","text":"` + text + `"}]`)})
	}

	id := f.create(t, f.agent, "Launch plan", `[{"type":"markdown","text":"v1"}]`)
	sid := strconv.FormatInt(id, 10)
	agentCtx := f.as(f.owner, authz.ViaAgent)
	for v := 2; v <= notifySenderBurst+2; v++ {
		if out := update(callerOfAgent(f.agent), agentCtx, id, "v"+strconv.Itoa(v)); pnum(out["version"]) != int64(v) {
			t.Fatalf("page_update v%d: %v", v, out)
		}
	}
	got := tagged("page-" + sid + "-updated")
	if len(got) != notifySenderBurst {
		t.Fatalf("%d update pushes from one sender; want the cap, %d", len(got), notifySenderBurst)
	}
	if got[0].Title != "Page updated: Launch plan (v2)" || got[0].Data["url"] != "/pages/"+sid+"/preview?v=2" ||
		!strings.HasPrefix(got[0].Body, "From ") {
		t.Fatalf("the first update push: %+v", got[0])
	}
	if last := got[len(got)-1]; last.Title != "Page updated: Launch plan (v"+strconv.Itoa(notifySenderBurst+1)+")" {
		t.Fatalf("the last update push: %+v", last)
	}
	if n := strings.Count(logs.String(), "push: dropped tag=page-"+sid+"-updated event="+push.EventPageUpdated); n != 1 {
		t.Fatalf("%d drop line(s) for the one update past the cap; log:\n%s", n, logs.String())
	}
	if ready := tagged("page-" + sid + "-ready"); len(ready) != 1 {
		t.Fatalf("page_create's push: %d", len(ready))
	}

	// Another sender's own bucket: a mirrored session's page still pushes.
	mc := mirrorCaller("host-echo", "sess-1")
	mctx := f.as(f.owner, authz.ViaMirrorHost)
	mp := f.toolAs(t, mctx, mc, pageCreateTool, map[string]any{"title": "Session page", "blocks": json.RawMessage(`[{"type":"markdown","text":"a"}]`)})
	mid := strconv.FormatInt(pnum(mp["page_id"]), 10)
	update(mc, mctx, pnum(mp["page_id"]), "b")
	if len(tagged("page-"+mid+"-updated")) != 1 {
		t.Fatal("another sender's update was capped by the first sender's")
	}

	// The window rolls: the first sender pushes again.
	now = now.Add(notifySenderWindow)
	update(callerOfAgent(f.agent), agentCtx, id, "rolled")
	if len(tagged("page-"+sid+"-updated")) != notifySenderBurst+1 {
		t.Fatal("the sender's window did not roll")
	}

	// Its own toggle: page_updated off silences updates, not new pages.
	if err := store.SetMuted(f.as(f.owner, authz.ViaSession), []string{push.EventPageUpdated}); err != nil {
		t.Fatal(err)
	}
	update(callerOfAgent(f.agent), agentCtx, id, "muted")
	if len(tagged("page-"+sid+"-updated")) != notifySenderBurst+1 {
		t.Fatal("a muted page_updated still pushed")
	}
	nid := f.create(t, f.agent, "Second page", `[{"type":"markdown","text":"x"}]`)
	if len(tagged("page-"+strconv.FormatInt(nid, 10)+"-ready")) != 1 {
		t.Fatal("muting page_updated also hid a new page")
	}
}
