package api

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/fleet"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/sessionstate"
)

// THE "N PROPOSALS WAITING" PUSH FOLLOWS THE PROPOSALS (fleet_actions.go
// fleetPushSync; docs/chief.md "Fleet proposals").
//
// Before: the push was sent when a proposal arrived and never touched again.
// A proposal that expired — its session moved on, or its 12 hours passed — or
// was approved or rejected left the push on the phone, and its tap opened
// "That proposal expired". Now: once none waits the push is withdrawn; with
// fewer (or more) still waiting it is replaced in place, quietly, naming one
// that still waits; a pass with no change sends nothing; and none of it
// touches the 30-minute alert clock. Every name is invented.

// fleetPushes is every push the owner got, after the fan-out settled.
func (f *proposeFixture) fleetPushes(cp *morningPush) []push.Payload {
	f.srv.WaitForPushes()
	var out []push.Payload
	for _, p := range cp.to(f.owner) {
		if p.Tag == fleetPushTag {
			out = append(out, p)
		}
	}
	return out
}

// endStop ends the open stop of the session keyed key (the session moved on).
func (f *proposeFixture) endStop(t *testing.T, key string) {
	t.Helper()
	ep, ok, err := f.eps.OpenFor(f.asOwner, sessionstate.KindMirror, key)
	if err != nil || !ok {
		t.Fatalf("the stop of %s: %v %v", key, ok, err)
	}
	if _, err := f.eps.End(f.asOwner, ep.ID, "owner", time.Now()); err != nil {
		t.Fatal(err)
	}
}

// pushedAt is the owner's recorded alert clock (ClaimPush's).
func (f *proposeFixture) pushedAt(t *testing.T) time.Time {
	t.Helper()
	var at time.Time
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT proposals_pushed_at FROM fleet_owner_marks WHERE owner_id = $1`, f.owner).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

func newRetractFixture(t *testing.T) (*proposeFixture, *morningPush) {
	t.Helper()
	f := newProposeFixture(t)
	cp := &morningPush{}
	f.srv.ext.Push = cp
	return f, cp
}

// wantWithdrawn: the last fleet push is the withdrawal of its tag.
func wantWithdrawn(t *testing.T, got []push.Payload, n int) {
	t.Helper()
	if len(got) != n {
		t.Fatalf("%d fleet pushes, want %d: %+v", len(got), n, got)
	}
	if last := got[n-1]; last.Type != push.TypeResolved || last.Tag != "fleet-proposals" || last.Title != "" {
		t.Fatalf("the last fleet push is not its withdrawal: %+v", last)
	}
}

// TestTheOnlyProposalExpiringWithdrawsItsPush: the one waiting proposal
// expires on the pass — its session moved on, or (a second owner's) its 12
// hours passed — and the push that alerted for it is withdrawn: a resolved
// push under its tag, once.
//
// Red at origin/main: no push after the alert; the phone kept it.
func TestTheOnlyProposalExpiringWithdrawsItsPush(t *testing.T) {
	t.Run("moved on", func(t *testing.T) {
		f, cp := newRetractFixture(t)
		s, key, _ := f.stopped(t, f.owner)
		f.propose(t, s, fleet.ClassContinue)
		f.pass()
		if got := f.fleetPushes(cp); len(got) != 1 || got[0].Type == push.TypeResolved || got[0].Silent {
			t.Fatalf("CONTROL: the alert: %+v", got)
		}
		f.endStop(t, key)
		f.pass()
		wantWithdrawn(t, f.fleetPushes(cp), 2)
		f.pass()
		wantWithdrawn(t, f.fleetPushes(cp), 2)
	})
	t.Run("12 hours", func(t *testing.T) {
		f, cp := newRetractFixture(t)
		s, _, _ := f.stopped(t, f.owner)
		id := f.propose(t, s, fleet.ClassStopNote)
		f.pass()
		if got := f.fleetPushes(cp); len(got) != 1 {
			t.Fatalf("CONTROL: the alert: %+v", got)
		}
		f.at = f.at.Add(13 * time.Hour)
		f.pass()
		if a := f.action(t, id); a.State != fleet.StateExpired {
			t.Fatalf("the proposal 13 h on: %+v", a)
		}
		wantWithdrawn(t, f.fleetPushes(cp), 2)
	})
}

// TestDecidingTheLastProposalWithdrawsItsPush: approving (or rejecting) the
// last waiting proposal on its card withdraws the push at the tap — no pass
// needed — and deciding the first of two does not (one still waits).
//
// Red at origin/main: no push after the alert.
func TestDecidingTheLastProposalWithdrawsItsPush(t *testing.T) {
	for _, verb := range []string{"approve", "reject"} {
		t.Run(verb, func(t *testing.T) {
			f, cp := newRetractFixture(t)
			s1, _, _ := f.stopped(t, f.owner)
			s2, _, _ := f.stopped(t, f.owner)
			first := f.propose(t, s1, fleet.ClassContinue)
			f.at = f.at.Add(time.Minute)
			last := f.propose(t, s2, fleet.ClassStopNote)
			f.pass()
			if got := f.fleetPushes(cp); len(got) != 1 {
				t.Fatalf("CONTROL: the alert: %+v", got)
			}
			f.card(t, fresh(f.asOwner), first, verb, nil)
			got := f.fleetPushes(cp)
			for _, p := range got[1:] {
				if p.Type == push.TypeResolved {
					t.Fatalf("deciding the first of two withdrew the push: %+v", got)
				}
			}
			n := len(got)
			f.card(t, fresh(f.asOwner), last, verb, nil)
			wantWithdrawn(t, f.fleetPushes(cp), n+1)
		})
	}
}

// TestTheLinkedProposalExpiringReplacesThePushQuietly: two wait; the push
// names the newer. The newer expires (its session moved on): the push is
// replaced in place — its tag, "1 proposal waiting", its link naming the one
// still waiting — Update and Silent (no sound, no second alert), and the
// alert clock is untouched. Passes with nothing new send nothing more. A
// genuinely new proposal 31 minutes after the alert is still alerted for, on
// the clock's own cadence (not silent).
//
// Red at origin/main: nothing was sent when the linked proposal expired, so
// the phone kept "2 proposals waiting" and a link to one no longer waiting.
func TestTheLinkedProposalExpiringReplacesThePushQuietly(t *testing.T) {
	f, cp := newRetractFixture(t)
	s1, _, _ := f.stopped(t, f.owner)
	s2, key2, _ := f.stopped(t, f.owner)
	older := f.propose(t, s1, fleet.ClassContinue)
	f.at = f.at.Add(time.Minute)
	newer := f.propose(t, s2, fleet.ClassHandoff)
	f.pass()
	got := f.fleetPushes(cp)
	if len(got) != 1 || got[0].Title != "2 proposals waiting" || got[0].Data["url"] != "/tasks?chief=open&fleet=proposed&proposal="+strconv.FormatInt(newer, 10) {
		t.Fatalf("CONTROL: the alert: %+v", got)
	}
	clock := f.pushedAt(t)

	f.endStop(t, key2)
	f.pass()
	f.pass()
	f.pass()
	got = f.fleetPushes(cp)
	if len(got) != 2 {
		t.Fatalf("%d fleet pushes after the linked proposal expired and three passes, want exactly 2 (the alert, one replacement): %+v", len(got), got)
	}
	r := got[1]
	if r.Type == push.TypeResolved || r.Tag != "fleet-proposals" || r.Title != "1 proposal waiting" ||
		r.Data["url"] != "/tasks?chief=open&fleet=proposed&proposal="+strconv.FormatInt(older, 10) {
		t.Fatalf("the replacement: %+v; want 1 waiting, linking proposal %d", r, older)
	}
	if !r.Silent || !r.Update {
		t.Fatalf("the replacement alerts: Silent %v, Update %v; want both", r.Silent, r.Update)
	}
	if after := f.pushedAt(t); !after.Equal(clock) {
		t.Fatalf("the replacement moved the alert clock: %v → %v", clock, after)
	}

	// A new proposal, 31 minutes after the alert: alerted for, out loud.
	s3, _, _ := f.stopped(t, f.owner)
	f.propose(t, s3, fleet.ClassStopNote)
	dateBackPush(t, f, 31*time.Minute)
	f.pass()
	got = f.fleetPushes(cp)
	if len(got) != 3 || got[2].Silent || got[2].Update || got[2].Title != "2 proposals waiting" {
		t.Fatalf("a new proposal after the clock: %+v; want a third push, an alert for 2", got)
	}
}

// TestTurningFleetProposalsOffWithdrawsThePush: with a push showing, fleet
// proposals turned off: the next pass withdraws it (its link opens a group no
// longer drawn), once.
//
// Red at origin/main: only the Attention notice was withdrawn.
func TestTurningFleetProposalsOffWithdrawsThePush(t *testing.T) {
	f, cp := newRetractFixture(t)
	s, _, _ := f.stopped(t, f.owner)
	f.propose(t, s, fleet.ClassContinue)
	f.pass()
	if got := f.fleetPushes(cp); len(got) != 1 {
		t.Fatalf("CONTROL: the alert: %+v", got)
	}
	f.srv.ext.FleetPropose = false
	f.pass()
	f.pass()
	wantWithdrawn(t, f.fleetPushes(cp), 2)
}

// TestTheGoneLineSaysWhyAProposalExpired: a fleet link to a proposal that
// expired says why — its session moved on ("No longer needed: the session
// moved on at <when it was noticed>."), or its 12 hours passed ("Expired
// after 12 hours at <its 12 h>.") — read from the reason recorded on the
// row, not its outcome's words.
//
// Red at origin/main: both said "That proposal expired at …".
func TestTheGoneLineSaysWhyAProposalExpired(t *testing.T) {
	f, _ := newRetractFixture(t)
	s1, key1, _ := f.stopped(t, f.owner)
	s2, _, _ := f.stopped(t, f.owner)
	moved := f.propose(t, s1, fleet.ClassContinue)
	aged := f.propose(t, s2, fleet.ClassStopNote)
	f.endStop(t, key1)
	f.pass()
	a := f.action(t, moved)
	if a.State != fleet.StateExpired {
		t.Fatalf("the moved-on proposal: %+v", a)
	}
	m := goneLine.FindStringSubmatch(f.fleetCardFor(t, "?fleet=proposed&proposal="+strconv.FormatInt(moved, 10)))
	if m == nil || m[1] != "No longer needed: the session moved on at " || m[2] != strconv.FormatInt(a.FinishedAt.UnixMilli(), 10) || m[3] != "." {
		t.Errorf("the moved-on line: %q", m)
	}
	created := f.action(t, aged).CreatedAt
	f.at = f.at.Add(13 * time.Hour)
	f.pass()
	m = goneLine.FindStringSubmatch(f.fleetCardFor(t, "?fleet=proposed&proposal="+strconv.FormatInt(aged, 10)))
	if m == nil || m[1] != "Expired after 12 hours at " || m[2] != strconv.FormatInt(created.Add(12*time.Hour).UnixMilli(), 10) || m[3] != "." {
		t.Errorf("the 12-hour line: %q (want the time %d)", m, created.Add(12*time.Hour).UnixMilli())
	}
}
