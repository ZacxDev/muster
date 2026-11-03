package mirror_test

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
	"github.com/ZacxDev/muster/internal/opencodehost"
	"github.com/ZacxDev/muster/internal/opencodehost/opencodetest"
)

// ocStatusBackend is ocE2EBackend that REPORTS STATUS (mirrorhost's
// StatusReporter), as the real tmux backend does (Tmux.ReportsStatus). Without
// it the host's hello says statusReport false, and the hub answers every
// AgentState "unknown" (ErrNoStatusReport) whatever the window lists — so the
// seam below would measure the fake, not the chain.
type ocStatusBackend struct{ *ocE2EBackend }

func (ocStatusBackend) ReportsStatus() bool { return true }

// TestAnOpenCodeTurnsLifecycleReachesAgentState is a SEAM test: the REAL
// frames of an opencode turn (testdata/abort-v2.0.32.sse), through a real
// muster-mirror and the real hub, into Hub.AgentState — what the chief's
// stall classifier and its moved-on check read for a window. It pins the
// sequence they need:
//
//  1. idle, timed from the session's last update (seeded on connect);
//  2. busy on session.execution.started (the v2 server sends no
//     session.status);
//  3. waiting while a permission request is pending (not idle: not stalled);
//  4. idle on session.execution.interrupted, with a NEW Since (a new idle
//     streak) and an Observed time to measure it from.
func TestAnOpenCodeTurnsLifecycleReachesAgentState(t *testing.T) {
	const seededSince = 1700000000000 // opencodetest's time.updated
	r := newRig(t)
	ctx, cancel := context.WithTimeout(asUser(1), 20*time.Second)
	defer cancel()
	tok, _ := r.store.mint(t, 1, "on")
	srv := opencodetest.New("fixture-seam-password")
	defer srv.Close()
	srv.PutSession(opencodetest.AbortPermissionSession, "Ask session", "")
	creds, err := opencodehost.EnvCredentials(srv.URL, srv.Issue())
	if err != nil {
		t.Fatal(err)
	}
	rctx, stop := context.WithCancel(context.Background())
	defer stop()
	src := mirrorhost.NewOpenCodeSource(rctx, creds, "", log.New(io.Discard, "", 0))
	b := ocStatusBackend{&ocE2EBackend{fakeBackend: &fakeBackend{}, src: src}}
	c, err := mirrorhost.New(mirrorhost.Config{URL: r.srv.URL, Token: tok, Host: "on", Backend: b, Interactive: true,
		AllowHTTP: true, Refresh: 20 * time.Millisecond, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = c.RunOnce(rctx) }()
	state := func() mirror.AgentState { return r.svc.AgentState(ctx, "on", "work", "@1") }

	eventually(t, "1. idle, seeded from the session's last update", func() bool {
		st := state()
		return st.Unknown == nil && st.Runtime == mirror.RuntimeOpenCode && st.Status == mirror.StatusIdle && st.Since == seededSince &&
			!st.Observed.IsZero() && srv.Subscribers() > 0
	})
	srv.EmitFrame(opencodetest.AbortFrame("session.execution.started", 0))
	eventually(t, "2. busy on session.execution.started", func() bool {
		st := state()
		return st.Status == mirror.StatusBusy && st.Since > seededSince
	})
	busy := state()
	srv.EmitFrame(opencodetest.AbortFrame("permission.asked", 0))
	eventually(t, "3. waiting on the permission request", func() bool { return state().Status == mirror.StatusWaiting })
	srv.EmitFrame(opencodetest.AbortFrame("session.execution.interrupted", 0))
	eventually(t, "4. idle on session.execution.interrupted, a new streak", func() bool {
		st := state()
		return st.Status == mirror.StatusIdle && st.Since >= busy.Since && st.Since != seededSince && !st.Observed.IsZero()
	})
}
