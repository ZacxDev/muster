package api

import (
	"context"
	"io"
	"log"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/notifypref"
	"github.com/ZacxDev/muster/internal/push"
)

// "Start notification titles with the tmux name" on the two pushes about a
// tmux window that name it by no human name (operator decision on #619,
// round 0 Q2): a host dispatch's outcome and a successor's start. With the
// switch on each starts with its window's codename@host, and does not say
// the host a second time. End to end on a real Postgres, through the real
// paths (the outcomes snapshot; the successor push's timer) and goNotify.

// setCodenameFirst wires the real preference store into f's server and sets
// the owner's codename switch (the row removed after the test).
func setCodenameFirst(t *testing.T, f *hostFixture, on bool) {
	t.Helper()
	p := notifypref.NewPG(f.pool)
	f.srv.ext.NotifyPrefs = p
	sys := authz.System(context.Background())
	_, _ = f.pool.Exec(sys, `DELETE FROM notify_preferences WHERE owner_id = $1`, f.owner)
	t.Cleanup(func() { _, _ = f.pool.Exec(sys, `DELETE FROM notify_preferences WHERE owner_id = $1`, f.owner) })
	if on {
		if err := p.SetCodenameFirst(asUser(f.owner), f.owner, true); err != nil {
			t.Fatal(err)
		}
	}
}

// TestAHostDispatchsOutcomePushStartsWithItsWindow: an opencode dispatch's
// window, listed with the host's label kiwi:3, replies without the done line
// (waiting on its owner). Off, the push title is today's; on, it starts with
// "kiwi:3@host-echo" and drops its own " on host-echo".
func TestAHostDispatchsOutcomePushStartsWithItsWindow(t *testing.T) {
	for _, c := range []struct {
		on   bool
		want string
	}{
		{false, "opencode · qwen/qwen3-coder (OpenRouter) on host-echo is waiting for you: Which README should I fix?"},
		{true, "kiwi:3@host-echo · opencode · qwen/qwen3-coder (OpenRouter) is waiting for you: Which README should I fix?"},
	} {
		t.Run(map[bool]string{true: "on", false: "off"}[c.on], func(t *testing.T) {
			f := newHostOutcomeFixture(t)
			setCodenameFirst(t, f.hostFixture, c.on)
			id, w := f.startOC(t, "oc-codename-"+strconv.FormatBool(c.on), ocOutcomeSID)
			w.Index, w.Label = 3, "kiwi:3"
			f.listOn("host-echo", w)
			ws, oc := f.idleWith(w, "msg_0utc0meCodename1", "I found two READMEs.", "Which README should I fix?")
			f.snap(f.owner, "host-echo", ws, oc)
			if ps := f.pushes(id); len(ps) != 1 || ps[0].Title != c.want {
				t.Fatalf("pushes %+v, want one titled %q", ps, c.want)
			}
		})
	}
}

// TestTheSuccessorPushStartsWithItsWindow: a successor no view opened is
// pushed once; with the switch on its title starts with the successor's
// window and drops "on host-echo"; off, it is today's title.
func TestTheSuccessorPushStartsWithItsWindow(t *testing.T) {
	old := successionPushDelay.Load()
	successionPushDelay.Store(int64(50 * time.Millisecond))
	t.Cleanup(func() { successionPushDelay.Store(old) })
	for _, on := range []bool{false, true} {
		t.Run(map[bool]string{true: "on", false: "off"}[on], func(t *testing.T) {
			f := newSuccFixture(t)
			pusher, store, device := &payloadPusher{}, &memPush{}, ep("succ-codename-owner")
			f.srv.ext.Push = push.NewService(store, pusher, push.VAPIDConfig{PublicKey: "k"}, log.New(io.Discard, "", 0))
			claim(t, store, f.owner, device)
			setCodenameFirst(t, f.hostFixture, on)
			f.viewLine(t, sxOpenCodeKey, "continue it")
			_, id := f.propose(t, sxCall(sxOpenCodeKey, "spinoff", sxRefFenc, intp(0)))
			if w := f.tapAs(t, f.ownerSession(), id, "@3", "Start it"); w.Code != http.StatusNoContent {
				t.Fatalf("the tap: %d %s", w.Code, w.Body)
			}
			x, _ := f.plugins.ProposalByID(f.asOwner, id)
			win := mirror.Window{Session: "agents", ID: "@" + strconv.FormatInt(x.HostDispatchID, 10), Index: 4, Label: "plum:4",
				Runtime: mirror.RuntimeOpenCode, AgentSession: "ses_0succcodename", Dispatch: x.HostDispatchID}
			ws := append(sxWindows(), win)
			f.m.mu.Lock()
			for i := range f.m.hosts {
				if f.m.hosts[i].Host == "host-echo" {
					f.m.hosts[i].Windows = mirror.NormalizeWindows(ws, 0)
				}
			}
			f.m.mu.Unlock()
			f.srv.reconcileListing(context.Background(), f.owner, "host-echo", ws, time.Now())
			deadline := time.Now().Add(3 * time.Second)
			for {
				if x, _ := f.plugins.ProposalByID(f.asOwner, id); !x.NotifiedAt.IsZero() {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("no push was recorded for the successor")
				}
				time.Sleep(20 * time.Millisecond)
			}
			f.srv.WaitForPushes()
			var titles []string
			for _, p := range pusher.to(device) {
				if p.Tag == proposalEventID(x) {
					titles = append(titles, p.Title)
				}
			}
			want := map[bool]string{false: "Your new session started on host-echo", true: "plum:4@host-echo · Your new session started"}[on]
			if len(titles) != 1 || titles[0] != want {
				t.Fatalf("successor pushes %q, want one %q", titles, want)
			}
		})
	}
}
