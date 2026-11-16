package mirror_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// A QUEUED CLAUDE CODE MESSAGE GOES ONLY TO THE CLAUDE SESSION IT WAS QUEUED
// FOR (mirror's session_pin.go). The row names the session (ShownSession); the
// hub refuses it, before anything reaches the host, when the window's list
// names another Claude session now — Claude Code restarted in the same pane,
// or /clear — and otherwise carries the session on the paste
// (Msg.AgentSession) for the host's own check at the moment it types.
//
// 🔴 REGRESSION TEST. At origin/main 9d0fb60d the Claude paste ignored the
// option: a message queued for one Claude session was typed into the next one
// started in the same pane.

const (
	pinQueuedFor = "0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b" // claudeGuardHello's session
	pinRestarted = "9f8e7d6c-5b4a-4392-8170-6f5e4d3c2b1a"
)

// TestAQueuedClaudeMessageIsNeverPastedIntoARestartedSession: the window @2
// holds pinQueuedFor; a row queued for pinRestarted's predecessor — i.e. for
// any session but the one listed — is refused with the moved words, never
// superseded (it would wait and land later), and the host receives nothing.
// It is THIS guard's refusal: its own kind and cause, not the prompt guard's.
func TestAQueuedClaudeMessageIsNeverPastedIntoARestartedSession(t *testing.T) {
	for _, queued := range []bool{true, false} {
		r := newRig(t)
		ctx, cancel := context.WithTimeout(asUser(1), 5*time.Second)
		tok, _ := r.store.mint(t, 1, "on")
		h := connectScripted(t, r, tok, claudeGuardHello("on", "idle"), okReply)
		r.online(t, 1, "on", "work")
		opts := []mirror.PasteOption{mirror.ShownSession(pinRestarted)}
		if queued {
			st := r.svc.AgentState(ctx, "on", "work", "@2")
			opts = append(opts, mirror.ExpectTurn(mirror.Turn{Since: st.Since, Pasted: st.Pasted}))
		}
		err := r.svc.PasteWindow(ctx, "on", "work", "@2", "queued for the old session", mirror.PasteClaude, opts...)
		var pr *mirror.PasteRefusedError
		if !errors.Is(err, mirror.ErrClaudeMoved) || errors.Is(err, mirror.ErrSuperseded) || !errors.As(err, &pr) ||
			pr.Refusal != "claude-moved" || pr.Why != mirror.ClaudeMovedText {
			t.Fatalf("queued=%v: err = %v (%T); want the claude-moved refusal, never superseded", queued, err, err)
		}
		time.Sleep(30 * time.Millisecond)
		if got := h.got(); len(got) != 0 {
			t.Fatalf("queued=%v: the host received %+v; want nothing", queued, got)
		}
		cancel()
	}
}

// TestAClaudeMessageForTheSessionHeldGoesAndNamesIt: the controls on the same
// window — a row queued for the session it still holds is pasted and the paste
// names that session (the host's check); a row naming none (queued before
// muster recorded it, or a person's Send) is pasted naming none, as before;
// the Terminal composer (AnyPane) is never pinned and names none.
func TestAClaudeMessageForTheSessionHeldGoesAndNamesIt(t *testing.T) {
	for _, c := range []struct {
		name, shown, want string
		target            mirror.PasteTarget
	}{
		{"queued for the session held", pinQueuedFor, pinQueuedFor, mirror.PasteClaude},
		{"a legacy row naming none", "", "", mirror.PasteClaude},
		{"the Terminal composer, even naming another", pinRestarted, "", mirror.PasteActivePane},
	} {
		r := newRig(t)
		ctx, cancel := context.WithTimeout(asUser(1), 5*time.Second)
		tok, _ := r.store.mint(t, 1, "on")
		h := connectScripted(t, r, tok, claudeGuardHello("on", "idle"), okReply)
		r.online(t, 1, "on", "work")
		if err := r.svc.PasteWindow(ctx, "on", "work", "@2", "hello", c.target, mirror.ShownSession(c.shown)); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got := h.got()
		if len(got) != 1 || got[0].AgentSession != c.want || got[0].Runtime != "" {
			t.Fatalf("%s: the host received %+v; want one paste naming %q", c.name, got, c.want)
		}
		cancel()
	}
}

// TestTheHostsClaudeMovedRefusalFailsAQueuedMessage: the race the host's half
// closes — muster's list still names the session, the pane holds another by
// the time the host types. The host's claude-moved refusal is returned as it
// is (a queued paste is NOT superseded: waiting does not clear it), and the
// window is not held as for a prompt.
func TestTheHostsClaudeMovedRefusalFailsAQueuedMessage(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithTimeout(asUser(1), 5*time.Second)
	defer cancel()
	tok, _ := r.store.mint(t, 1, "on")
	connectScripted(t, r, tok, claudeGuardHello("on", "idle"), func(mirror.Msg) mirror.Msg {
		return mirror.Msg{Error: mirror.ClaudeMovedText, Refusal: mirror.PasteRefusalClaudeMoved}
	})
	r.online(t, 1, "on", "work")
	st := r.svc.AgentState(ctx, "on", "work", "@2")
	err := r.svc.PasteWindow(ctx, "on", "work", "@2", "queued", mirror.PasteClaude, mirror.ShownSession(pinQueuedFor),
		mirror.ExpectTurn(mirror.Turn{Since: st.Since, Pasted: st.Pasted}))
	var pr *mirror.PasteRefusedError
	if errors.Is(err, mirror.ErrSuperseded) || !errors.As(err, &pr) || pr.Refusal != mirror.PasteRefusalClaudeMoved || mirror.ClaudeWaitingRefusal(err) {
		t.Fatalf("err = %v (%T); want the host's claude-moved refusal, not superseded", err, err)
	}
}
