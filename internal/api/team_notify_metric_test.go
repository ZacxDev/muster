package api

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/users"
)

// scriptedTeamReader is a users store whose team read answers as told.
type scriptedTeamReader struct {
	users.Store
	team  bool
	fail  bool
	reads *int
}

func (r scriptedTeamReader) TeamPushTargets(context.Context, int64) (users.TeamPushTargets, error) {
	*r.reads++
	if r.fail {
		return users.TeamPushTargets{}, errors.New("synthetic read failure")
	}
	return users.TeamPushTargets{Team: r.team, Name: "t", Notify: []int64{91}, Withdraw: []int64{91}}, nil
}

// TestADroppedTeamPushIsCounted (#683 rounds 0 and 1): a failed recipient
// read sends the push to its owner alone — for a team, nobody — and
// muster_push_suppressed_total counts it BY WHAT MUSTER KNOWS of the owner:
//   - a known TEAM: team_read_failed (a dropped team push);
//   - an owner whose kind was never read: owner_kind_unread, never
//     team_read_failed (it may be a person whose push did go);
//   - a known PERSON: no read at all, nothing counted, their push goes.
func TestADroppedTeamPushIsCounted(t *testing.T) {
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	team := metrics.PushSuppressed.WithLabelValues("team_read_failed")
	unread := metrics.PushSuppressed.WithLabelValues("owner_kind_unread")
	reads := 0
	moved := func(f func()) (float64, float64) {
		t0, u0 := testutil.ToFloat64(team), testutil.ToFloat64(unread)
		f()
		return testutil.ToFloat64(team) - t0, testutil.ToFloat64(unread) - u0
	}
	ask := func(owner int64) ([]int64, string) {
		return s.teamPushRecipients(context.Background(), owner, "plugin", 0, false)
	}

	// An owner never read before, whose read fails.
	s.ext.Users = scriptedTeamReader{fail: true, reads: &reads}
	dt, du := moved(func() {
		if got, prefix := ask(4242); len(got) != 1 || got[0] != 4242 || prefix != "" {
			t.Errorf("a failed read answered %v %q; want the owner alone, no prefix", got, prefix)
		}
	})
	if dt != 0 || du != 1 {
		t.Errorf("an unread owner's failed read moved team_read_failed by %v and owner_kind_unread by %v; want 0 and 1", dt, du)
	}

	// A known team (one good read), then a failed read: a dropped team push.
	s.ext.Users = scriptedTeamReader{team: true, reads: &reads}
	if got, _ := ask(5151); len(got) != 1 || got[0] != 91 {
		t.Fatalf("control: the team's read answered %v; want its member 91", got)
	}
	s.ext.Users = scriptedTeamReader{fail: true, reads: &reads}
	if dt, du := moved(func() { ask(5151) }); dt != 1 || du != 0 {
		t.Errorf("a known team's failed read moved team_read_failed by %v and owner_kind_unread by %v; want 1 and 0", dt, du)
	}

	// A known person (one good read), then a failing store: no read, no
	// count, their own push.
	s.ext.Users = scriptedTeamReader{reads: &reads}
	ask(6262)
	s.ext.Users = scriptedTeamReader{fail: true, reads: &reads}
	before := reads
	dt, du = moved(func() {
		if got, _ := ask(6262); len(got) != 1 || got[0] != 6262 {
			t.Errorf("a known person's push went to %v; want them", got)
		}
	})
	if dt != 0 || du != 0 || reads != before {
		t.Errorf("a known person's push read %d time(s) and moved the counters by %v / %v; want no read and no count", reads-before, dt, du)
	}
}
