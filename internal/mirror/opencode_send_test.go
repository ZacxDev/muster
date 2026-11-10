package mirror_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// THE GATED SEND, SERVER HALF: a message to an opencode window reaches its
// host only when the host advertised openCodeSend, the window is one it sends
// to (agentSend) and idle with a status time — and then carries the runtime,
// the status time and the message id derived from its key.

const ocSendSID = "ses_e5d559cd5ffecrpBJHzHzu6wl9"

// ocSendSIDHand is the session of the hand-opened windows @5 and @6.
const ocSendSIDHand = "ses_e5d559cd5ffeHandOpenedPane1"

// ocSendWindows: work:@1 a launcher-bound opencode window, idle since 111;
// @2 a title-matched (unbound) one from an older host (no AgentBind), idle;
// @3 a bound one, busy; @4 Claude Code; @5 and @6 below.
func ocSendWindows() []mirror.Window {
	return []mirror.Window{
		{Session: "work", ID: "@1", Index: 0, Active: true, Runtime: mirror.RuntimeOpenCode, AgentSession: ocSendSID, AgentSend: true,
			Status: mirror.StatusIdle, StatusSince: 111},
		{Session: "work", ID: "@2", Index: 1, Runtime: mirror.RuntimeOpenCode, AgentSession: ocSendSID, Status: mirror.StatusIdle, StatusSince: 112},
		{Session: "work", ID: "@3", Index: 2, Runtime: mirror.RuntimeOpenCode, AgentSession: ocSendSID, AgentSend: true,
			Status: mirror.StatusBusy, StatusSince: 113},
		{Session: "work", ID: "@4", Index: 3, Claude: true, Status: mirror.StatusIdle, StatusSince: 114},
		// @5 a hand-opened pane its host VERIFIED (opencode_bind.go), idle;
		// @6 one whose title is cut, which its host does not send to.
		{Session: "work", ID: "@5", Index: 4, Runtime: mirror.RuntimeOpenCode, AgentSession: ocSendSIDHand, AgentBind: mirror.AgentBindVerified,
			Status: mirror.StatusIdle, StatusSince: 115},
		{Session: "work", ID: "@6", Index: 5, Runtime: mirror.RuntimeOpenCode, AgentSession: ocSendSIDHand, AgentBind: mirror.AgentBindTitleCut,
			Status: mirror.StatusIdle, StatusSince: 116},
	}
}

func ocSendHost(t *testing.T, sends, interactive bool) (*rig, *rawHost) {
	t.Helper()
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "h")
	m := winHello("h", interactive, ocSendWindows()...)
	m.StatusReport, m.OpenCodeSend, m.PanePaste = true, sends, true
	raw, err := r.rawConnect(t, tok, m)
	if err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "h", "work")
	return r, raw
}

// TestAnOpenCodeSendCarriesItsRuntimeSinceAndMessageID: with the switch open,
// a send to the bound, idle window reaches the host as a paste naming the
// runtime, the status time it was decided on, and the id derived from its key
// — the SAME id for the same key, another for another key or owner.
func TestAnOpenCodeSendCarriesItsRuntimeSinceAndMessageID(t *testing.T) {
	r, raw := ocSendHost(t, true, true)
	if hv := r.svc.Hosts(asUser(1)); len(hv) != 1 || !hv[0].OpenCodeSend {
		t.Fatalf("the host's openCodeSend was not believed: %+v", hv)
	}
	go func() {
		eventually(t, "the paste", func() bool { return len(raw.of(mirror.TypePaste)) == 1 })
		raw.write(t, mirror.Msg{Type: mirror.TypePasted, ID: raw.of(mirror.TypePaste)[0].ID})
	}()
	ctx, cancel := context.WithTimeout(asUser(1), 5*time.Second)
	defer cancel()
	if err := r.svc.PasteWindow(ctx, "h", "work", "@1", "hello", mirror.PasteClaude, mirror.MessageKey("nonce-aaaaaaaaaaaaaaaa")); err != nil {
		t.Fatal(err)
	}
	m := raw.of(mirror.TypePaste)[0]
	want := mirror.OpenCodeMessageID(1, "nonce-aaaaaaaaaaaaaaaa")
	if m.Runtime != mirror.RuntimeOpenCode || m.Since != 111 || m.MessageID != want || m.AnyPane || m.Text != "hello" {
		t.Fatalf("the paste: runtime %q since %d id %q anyPane %v; want opencode, 111, %q", m.Runtime, m.Since, m.MessageID, m.AnyPane, want)
	}
	if !strings.HasPrefix(want, "msg_") || len(want) != 30 {
		t.Fatalf("the id %q is not shaped like opencode's", want)
	}
	if mirror.OpenCodeMessageID(1, "nonce-aaaaaaaaaaaaaaaa") != want || mirror.OpenCodeMessageID(1, "nonce-bbbbbbbbbbbbbbbb") == want ||
		mirror.OpenCodeMessageID(2, "nonce-aaaaaaaaaaaaaaaa") == want || mirror.OpenCodeMessageID(1, "") != "" {
		t.Fatal("the id is not a function of exactly (owner, key)")
	}
}

// TestAnOpenCodeSendIsRefusedBeforeTheHost: every refusal the hub makes is
// made before anything reaches the host, each with its own cause and kind.
func TestAnOpenCodeSendIsRefusedBeforeTheHost(t *testing.T) {
	for _, c := range []struct {
		name               string
		sends, interactive bool
		window             string
		ctx                func() context.Context
		want               error
		refusal            string
	}{
		{"the host did not advertise it", false, true, "@1", nil, mirror.ErrOpenCodeSendOff, mirror.PasteRefusalOpenCodeOff},
		{"a title-matched window", true, true, "@2", nil, mirror.ErrOpenCodeUnbound, mirror.PasteRefusalOpenCodeUnbound},
		{"a busy window", true, true, "@3", nil, mirror.ErrOpenCodeNotIdle, ""},
		{"a stale sign-in", true, true, "@1", func() context.Context { return asStaleUser(1) }, mirror.ErrStepUpRequired, ""},
		{"a read-only host", true, false, "@1", nil, mirror.ErrInteractiveRefused, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, raw := ocSendHost(t, c.sends, c.interactive)
			base := asUser(1)
			if c.ctx != nil {
				base = c.ctx()
			}
			ctx, cancel := context.WithTimeout(base, 5*time.Second)
			defer cancel()
			err := r.svc.PasteWindow(ctx, "h", "work", c.window, "hello", mirror.PasteClaude, mirror.MessageKey("nonce-aaaaaaaaaaaaaaaa"))
			if !errors.Is(err, c.want) || errors.Is(err, mirror.ErrPasteUnconfirmed) {
				t.Fatalf("err = %v, want %v (and not unconfirmed)", err, c.want)
			}
			if c.refusal != "" && mirror.PasteGuardRefusal(err) != c.refusal {
				t.Fatalf("refusal kind %q, want %q", mirror.PasteGuardRefusal(err), c.refusal)
			}
			time.Sleep(30 * time.Millisecond)
			if got := raw.of(mirror.TypePaste); len(got) != 0 {
				t.Fatalf("the host received %+v", got)
			}
		})
	}
}

// TestAQueuedOpenCodeSendToABusyWindowIsSuperseded: a queued paste
// (ExpectTurn) to a window that is not idle is superseded — decided on again
// later — not refused in words, and nothing reaches the host; so is one the
// HOST refuses as not idle.
func TestAQueuedOpenCodeSendToABusyWindowIsSuperseded(t *testing.T) {
	r, raw := ocSendHost(t, true, true)
	ctx, cancel := context.WithTimeout(asUser(1), 5*time.Second)
	defer cancel()
	st := r.svc.AgentState(ctx, "h", "work", "@3")
	err := r.svc.PasteWindow(ctx, "h", "work", "@3", "hello", mirror.PasteClaude, mirror.ExpectTurn(mirror.Turn{Since: st.Since, Pasted: st.Pasted}))
	if !errors.Is(err, mirror.ErrSuperseded) {
		t.Fatalf("a queued send to a busy window = %v, want superseded", err)
	}
	if got := raw.of(mirror.TypePaste); len(got) != 0 {
		t.Fatalf("the host received %+v", got)
	}
	go func() {
		eventually(t, "the paste", func() bool { return len(raw.of(mirror.TypePaste)) == 1 })
		raw.write(t, mirror.Msg{Type: mirror.TypePasted, ID: raw.of(mirror.TypePaste)[0].ID, Error: mirror.PasteRefusedOpenCodeBusyText,
			Refusal: mirror.PasteRefusalOpenCodeNotIdle})
	}()
	st = r.svc.AgentState(ctx, "h", "work", "@1")
	err = r.svc.PasteWindow(ctx, "h", "work", "@1", "hello", mirror.PasteClaude, mirror.ExpectTurn(mirror.Turn{Since: st.Since, Pasted: st.Pasted}))
	if !errors.Is(err, mirror.ErrSuperseded) {
		t.Fatalf("a queued send the host found busy = %v, want superseded", err)
	}
}

// TestAnOpenCodeSendTheHostCouldNotConfirmIsUnconfirmed: the host's
// "posted, no answer" is ErrPasteUnconfirmed (it may have landed) — and its
// other refusals are refusals, by kind.
func TestAnOpenCodeSendTheHostCouldNotConfirmIsUnconfirmed(t *testing.T) {
	for refusal, unconfirmed := range map[string]bool{
		mirror.PasteRefusalOpenCodeUnconfirmed: true,
		mirror.PasteRefusalOpenCodeGone:        false,
		mirror.PasteRefusalOpenCodeSlash:       false,
		mirror.PasteRefusalOpenCodeUnbound:     false,
	} {
		t.Run(refusal, func(t *testing.T) {
			r, raw := ocSendHost(t, true, true)
			go func() {
				eventually(t, "the paste", func() bool { return len(raw.of(mirror.TypePaste)) == 1 })
				raw.write(t, mirror.Msg{Type: mirror.TypePasted, ID: raw.of(mirror.TypePaste)[0].ID, Error: "why", Refusal: refusal})
			}()
			ctx, cancel := context.WithTimeout(asUser(1), 5*time.Second)
			defer cancel()
			err := r.svc.PasteWindow(ctx, "h", "work", "@1", "hello", mirror.PasteClaude)
			if errors.Is(err, mirror.ErrPasteUnconfirmed) != unconfirmed {
				t.Fatalf("%s: %v; unconfirmed want %v", refusal, err, unconfirmed)
			}
			if !unconfirmed && mirror.PasteGuardRefusal(err) != refusal {
				t.Fatalf("%s: kind %q", refusal, mirror.PasteGuardRefusal(err))
			}
		})
	}
}

// TestAnOpenCodeWindowKeepsAgentSendOnlyWhenItNamesASession: NormalizeWindows
// keeps AgentSend only on a gated-send runtime's window naming its session.
func TestAnOpenCodeWindowKeepsAgentSendOnlyWhenItNamesASession(t *testing.T) {
	in := []mirror.Window{
		{Session: "s", ID: "@1", Runtime: mirror.RuntimeOpenCode, AgentSession: ocSendSID, AgentSend: true},
		{Session: "s", ID: "@2", Runtime: mirror.RuntimeOpenCode, AgentSend: true},
		{Session: "s", ID: "@3", Claude: true, AgentSend: true},
	}
	out := mirror.NormalizeWindows(in, 0)
	got := map[string]bool{}
	for _, w := range out {
		got[w.ID] = w.AgentSend
	}
	if !got["@1"] || got["@2"] || got["@3"] {
		t.Fatalf("AgentSend kept %v; want only @1", got)
	}
}
