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

// failingTeamReader is a users store whose team read always fails.
type failingTeamReader struct{ users.Store }

func (failingTeamReader) TeamPushTargets(context.Context, int64) (users.TeamPushTargets, error) {
	return users.TeamPushTargets{}, errors.New("synthetic read failure")
}

// TestADroppedTeamPushIsCounted (#683 round 0, choice 3): when the read of a
// notification owner's team recipients fails, the push goes to the owner
// alone — for a team, nobody — and muster_push_suppressed_total
// {reason="team_read_failed"} counts it. CONTROL: a read that works counts
// nothing.
func TestADroppedTeamPushIsCounted(t *testing.T) {
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	c := metrics.PushSuppressed.WithLabelValues("team_read_failed")
	before := testutil.ToFloat64(c)
	s.ext.Users = failingTeamReader{}
	if got, prefix := s.teamPushRecipients(context.Background(), 4242, "plugin", 0); len(got) != 1 || got[0] != 4242 || prefix != "" {
		t.Errorf("a failed read answered %v %q; want the owner alone, no prefix", got, prefix)
	}
	if d := testutil.ToFloat64(c) - before; d != 1 {
		t.Errorf("team_read_failed moved by %v; want 1", d)
	}
	s.ext.Users = okTeamReader{}
	before = testutil.ToFloat64(c)
	_, _ = s.teamPushRecipients(context.Background(), 4242, "plugin", 0)
	if d := testutil.ToFloat64(c) - before; d != 0 {
		t.Errorf("control: a read that works moved team_read_failed by %v", d)
	}
}

type okTeamReader struct{ users.Store }

func (okTeamReader) TeamPushTargets(context.Context, int64) (users.TeamPushTargets, error) {
	return users.TeamPushTargets{}, nil
}
