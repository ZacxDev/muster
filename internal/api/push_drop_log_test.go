package api

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/users"
)

// teamNoOneUsers is stubUsers whose owner teamID is a TEAM with nobody to
// push: every operator and manager has the team's notifications off.
type teamNoOneUsers struct {
	stubUsers
	teamID int64
}

func (u teamNoOneUsers) TeamPushTargets(_ context.Context, owner int64) (users.TeamPushTargets, error) {
	if owner == u.teamID {
		return users.TeamPushTargets{Team: true, Name: "platform"}, nil
	}
	return users.TeamPushTargets{}, nil
}

// TestEveryPushDropIsLogged: each way a notification reaches nobody leaves ONE
// log line naming its tag, event, owner and reason — a muted event, every
// drop past the owner's cap (not only the first), an owner with no device, a
// team with no recipient, an owner-less notification, and a server with no
// push at all (once: there every notification is dropped). The positive
// control: a delivered push logs no drop.
//
// 🔴 REGRESSION TEST for the page_create push that could not be diagnosed:
// at the base, four of these drops (muted, the cap past its first refusal,
// no device, a team with no recipient) left no line at all, and the
// not-configured one none either.
func TestEveryPushDropIsLogged(t *testing.T) {
	store := &memPush{}
	claim(t, store, 7, ep("seven"))
	pusher := &payloadPusher{}
	var logs syncBuffer
	s := New(nil, AuthConfig{}, log.New(&logs, "", 0))
	s.UseExtensions(Extensions{Users: teamNoOneUsers{teamID: 50},
		Push: push.NewService(store, pusher, push.VAPIDConfig{PublicKey: "k"}, log.New(io.Discard, "", 0))})
	now := time.Unix(2_000_000, 0)
	s.now = func() time.Time { return now }
	as7 := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 7, Role: authz.RoleMember, Via: authz.ViaSession})

	count := func(sub string) int { return strings.Count(logs.String(), sub) }

	// The positive control: delivered, and no drop line.
	s.pushTask(7, push.EventComment, RouterNotification{Type: "task", Title: "ok", Tag: "t-ok"})
	s.WaitForPushes()
	if len(pusher.to(ep("seven"))) != 1 || count("push: dropped") != 0 {
		t.Fatalf("control: delivered %d, log:\n%s", len(pusher.to(ep("seven"))), logs.String())
	}

	// Muted.
	if err := store.SetMuted(as7, []string{push.EventReview}); err != nil {
		t.Fatal(err)
	}
	s.pushTask(7, push.EventReview, RouterNotification{Type: "task", Title: "m", Tag: "t-muted"})
	s.WaitForPushes()
	if n := count("push: dropped tag=t-muted event=review owner=7: muted"); n != 1 {
		t.Errorf("a muted push logged %d drop line(s); log:\n%s", n, logs.String())
	}
	_ = store.SetMuted(as7, nil)

	// Past the owner's cap: EVERY drop, not only the first. One slot is spent
	// (t-ok), so pushOwnerBurst-1 more go and the next two are dropped.
	for i := 0; i < pushOwnerBurst+1; i++ {
		s.pushTask(7, push.EventComment, RouterNotification{Type: "task", Title: "b", Tag: "t-cap"})
	}
	s.WaitForPushes()
	if n := count("push: dropped tag=t-cap event=comment owner=7: the owner's cap"); n != 2 {
		t.Errorf("past the cap: %d drop line(s), want 2 (one per dropped push); log:\n%s", n, logs.String())
	}

	// No device.
	s.pushTask(9, push.EventComment, RouterNotification{Type: "task", Title: "n", Tag: "t-nodev"})
	s.WaitForPushes()
	if n := count("push: dropped tag=t-nodev event=comment owner=9: no device is subscribed"); n != 1 {
		t.Errorf("an owner with no device: %d drop line(s); log:\n%s", n, logs.String())
	}

	// A team with nobody to push.
	s.pushTask(50, push.EventComment, RouterNotification{Type: "task", Title: "team", Tag: "t-team"})
	s.WaitForPushes()
	if n := count("push: dropped tag=t-team event=comment owner=50: a team owner with no operator or manager"); n != 1 {
		t.Errorf("a team with no recipient: %d drop line(s); log:\n%s", n, logs.String())
	}

	// No owner.
	s.goNotify(0, push.EventComment, RouterNotification{Type: "task", Title: "x", Tag: "t-noowner"}, nil)
	s.WaitForPushes()
	if n := count("push: dropped tag=t-noowner event=comment owner=0: it names no owner"); n != 1 {
		t.Errorf("an owner-less push: %d drop line(s); log:\n%s", n, logs.String())
	}

	// No push at all: said once, for the first, and never again.
	var offLogs syncBuffer
	off := New(nil, AuthConfig{}, log.New(&offLogs, "", 0))
	off.UseExtensions(Extensions{})
	off.pushTask(7, push.EventComment, RouterNotification{Type: "task", Title: "a", Tag: "t-off-1"})
	off.pushTask(7, push.EventComment, RouterNotification{Type: "task", Title: "b", Tag: "t-off-2"})
	off.WaitForPushes()
	if got := offLogs.String(); strings.Count(got, "push: dropped") != 1 || !strings.Contains(got, "tag=t-off-1 event=comment: push is not configured") {
		t.Errorf("a server with no push: want one line naming the first drop, got:\n%s", got)
	}
}
