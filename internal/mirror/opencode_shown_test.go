package mirror_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// ONLY THE SESSION THE MESSAGE WAS WRITTEN FOR (mirror.ShownSession, the
// hub's ShownSessionCheck): a gated send — typed now, or a queued row
// delivered later — names the session its view showed, and the hub refuses
// it, before anything reaches the host, when the window's list names another
// now. A hand-opened (verified) pane's send that names none is refused; a
// launcher-bound pane's is not (its record names the session). Claude Code
// is untouched by the option.
//
// 🔴 REGRESSION TEST. At 2afa5ab8 the hub took the session from its list at
// delivery: a message written for X was delivered to Y.

// TestASendWrittenForAnotherSessionNeverReachesTheHost: @5 (verified) lists
// ocSendSIDHand; a send written while the view showed another session — a
// person's, or a queued row's (ExpectTurn) — is refused as moved, never
// superseded, and the host receives nothing.
func TestASendWrittenForAnotherSessionNeverReachesTheHost(t *testing.T) {
	const other = "ses_e5d559cd5ffeOtherSessionXX"
	for _, c := range []struct {
		name, window, shown, why string
		queued                   bool
	}{
		{"a person's send, written for X, the window on Y", "@5", other, mirror.PasteRefusedOpenCodeMovedText, false},
		{"a queued row, written for X, the window on Y", "@5", other, mirror.PasteRefusedOpenCodeMovedText, true},
		{"a hand-opened pane's send naming no session", "@5", "", mirror.PasteRefusedOpenCodeUnnamedText, false},
		{"a hand-opened pane's queued row naming no session (an older row)", "@5", "", mirror.PasteRefusedOpenCodeUnnamedText, true},
		{"a launcher-bound pane's send written for another session", "@1", other, mirror.PasteRefusedOpenCodeMovedText, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, raw := ocSendHost(t, true, true)
			ctx, cancel := context.WithTimeout(asUser(1), 5*time.Second)
			defer cancel()
			opts := []mirror.PasteOption{mirror.ShownSession(c.shown)}
			if c.queued {
				st := r.svc.AgentState(ctx, "h", "work", c.window)
				opts = append(opts, mirror.ExpectTurn(mirror.Turn{Since: st.Since, Pasted: st.Pasted}))
			}
			err := r.svc.PasteWindow(ctx, "h", "work", c.window, "hello", mirror.PasteClaude, opts...)
			var pr *mirror.PasteRefusedError
			if !errors.Is(err, mirror.ErrOpenCodeMoved) || errors.Is(err, mirror.ErrSuperseded) || !errors.As(err, &pr) || pr.Why != c.why ||
				mirror.PasteGuardRefusal(err) != mirror.PasteRefusalOpenCodeMoved {
				t.Fatalf("err = %v; want the moved refusal %q, never superseded", err, c.why)
			}
			time.Sleep(30 * time.Millisecond)
			if got := raw.of(mirror.TypePaste); len(got) != 0 {
				t.Fatalf("the host received %+v", got)
			}
		})
	}
}

// TestASendWrittenForTheSessionShownGoes: the controls — written for the
// session the window still shows (verified or launcher-bound), or a
// launcher-bound pane's send naming none (an older page): sent, naming it.
func TestASendWrittenForTheSessionShownGoes(t *testing.T) {
	for _, c := range []struct{ window, shown, want string }{
		{"@5", ocSendSIDHand, ocSendSIDHand},
		{"@1", ocSendSID, ocSendSID},
		{"@1", "", ocSendSID},
	} {
		r, raw := ocSendHost(t, true, true)
		go func() {
			eventually(t, "the paste", func() bool { return len(raw.of(mirror.TypePaste)) == 1 })
			raw.write(t, mirror.Msg{Type: mirror.TypePasted, ID: raw.of(mirror.TypePaste)[0].ID})
		}()
		ctx, cancel := context.WithTimeout(asUser(1), 5*time.Second)
		if err := r.svc.PasteWindow(ctx, "h", "work", c.window, "hello", mirror.PasteClaude, mirror.ShownSession(c.shown)); err != nil {
			t.Fatalf("%s shown %q: %v", c.window, c.shown, err)
		}
		cancel()
		if m := raw.of(mirror.TypePaste)[0]; m.AgentSession != c.want {
			t.Fatalf("%s shown %q: the paste names %q, want %q", c.window, c.shown, m.AgentSession, c.want)
		}
	}
}
