package mirror_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
	"github.com/ZacxDev/muster/internal/opencodehost"
	"github.com/ZacxDev/muster/internal/opencodehost/opencodetest"
)

// ANSWERING AN OPENCODE ASK: the hub sends it only to a host that advertised
// openCodeAnswer, -interactive, for the ask its window lists with an answer it
// offers, behind a fresh sign-in.

const ocAnsSID = "ses_e5d559cd5ffecrpBJHzHzu6wl9"

// openCodeHello is a hello listing work:@1, an opencode window waiting on the
// permission request per_1 (offering choices), work:@3 one waiting on the
// question frm_1 (Yes, No), and work:@2, a Claude Code window.
func openCodeHello(extra, choices string) string {
	w := []mirror.Window{
		{Session: "work", ID: "@1", Index: 0, Active: true, Runtime: mirror.RuntimeOpenCode, AgentSession: ocAnsSID,
			Status: mirror.StatusWaiting, StatusSince: 1792000001111,
			Pending: &mirror.Pending{Kind: mirror.PendingPermission, Summary: "bash: rm -rf build", Tool: "bash", PromptID: "per_1", Choices: choices}},
		{Session: "work", ID: "@2", Index: 1, Claude: true, Status: mirror.StatusIdle, StatusSince: 1},
		{Session: "work", ID: "@3", Index: 2, Runtime: mirror.RuntimeOpenCode, AgentSession: ocAnsSID,
			Status: mirror.StatusWaiting, StatusSince: 1792000001112,
			Pending: &mirror.Pending{Kind: mirror.PendingQuestion, Summary: "Proceed?", Tool: "question", PromptID: "frm_1", Options: "Yes\nNo"}},
	}
	b, _ := json.Marshal(w)
	return `{"type":"hello","version":2,"host":"on","interactive":true,"panePaste":true,"statusReport":true` + extra + `,"windows":` + string(b) + `}`
}

func TestAnOpenCodeAnswerReachesItsHost(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithTimeout(asUser(1), 5*time.Second)
	defer cancel()
	h := connectAnswerHost(t, r, openCodeHello(`,"openCodeAnswer":true`, "once reject"), func(mirror.Msg) mirror.Msg { return mirror.Msg{} })
	if hv := r.svc.Hosts(ctx); len(hv) != 1 || !hv[0].OpenCodeAnswer {
		t.Fatalf("the host's openCodeAnswer was not believed (or read as another runtime's): %+v", hv)
	}
	if err := r.svc.AnswerOpenCodeAsk(ctx, "on", "work", "@1", "per_1", "reject"); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.AnswerOpenCodeAsk(ctx, "on", "work", "@3", "frm_1", "Yes"); err != nil {
		t.Fatal(err)
	}
	got := h.got()
	if len(got) != 2 {
		t.Fatalf("the host received %d answers", len(got))
	}
	if m := got[0]; m.Runtime != mirror.RuntimeOpenCode || m.Window != "@1" || m.Ask != nil || m.OpenCodeReply == nil ||
		!reflect.DeepEqual(*m.OpenCodeReply, mirror.OpenCodeReply{PromptID: "per_1", Choice: "reject"}) {
		t.Fatalf("the permission's answer: %+v (%+v)", m, m.OpenCodeReply)
	}
	if m := got[1]; m.Window != "@3" || !reflect.DeepEqual(*m.OpenCodeReply, mirror.OpenCodeReply{PromptID: "frm_1", Choice: "Yes"}) {
		t.Fatalf("the question's answer: %+v", m.OpenCodeReply)
	}
}

// 🔴 Every refusal the hub can make is made BEFORE anything reaches the host —
// the read-only host's above all.
func TestAnOpenCodeAnswerIsRefusedBeforeTheHost(t *testing.T) {
	for _, c := range []struct {
		name, extra, choices, window, prompt, choice string
		ctx                                          func() context.Context
		want                                         error
	}{
		{"no openCodeAnswer", ``, "once reject", "@1", "per_1", "once", nil, mirror.ErrOpenCodeAnswerUnsupported},
		{"an older host's removed hermesAnswer flag", `,"hermesAnswer":true`, "once reject", "@1", "per_1", "once", nil, mirror.ErrOpenCodeAnswerUnsupported},
		{"read-only host", `,"openCodeAnswer":true,"interactive":false`, "once reject", "@1", "per_1", "once", nil, mirror.ErrOpenCodeAnswerUnsupported},
		{"a stale sign-in", `,"openCodeAnswer":true`, "once reject", "@1", "per_1", "once", func() context.Context { return asStaleUser(1) }, mirror.ErrStepUpRequired},
		{"another request", `,"openCodeAnswer":true`, "once reject", "@1", "per_2", "once", nil, mirror.ErrOpenCodeAskGone},
		{"always", `,"openCodeAnswer":true`, "once reject", "@1", "per_1", "always", nil, mirror.ErrOpenCodeAskGone},
		{"a choice not offered", `,"openCodeAnswer":true`, "reject", "@1", "per_1", "once", nil, mirror.ErrOpenCodeAskGone},
		{"an option the question lacks", `,"openCodeAnswer":true`, "once reject", "@3", "frm_1", "Maybe", nil, mirror.ErrOpenCodeAskGone},
		{"a permission choice for a question", `,"openCodeAnswer":true`, "once reject", "@3", "frm_1", "once", nil, mirror.ErrOpenCodeAskGone},
		{"a Claude window", `,"openCodeAnswer":true`, "once reject", "@2", "per_1", "once", nil, mirror.ErrOpenCodeAskGone},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			base := asUser(1)
			if c.ctx != nil {
				base = c.ctx()
			}
			ctx, cancel := context.WithTimeout(base, 5*time.Second)
			defer cancel()
			hello := openCodeHello(c.extra, c.choices)
			if strings.Contains(c.extra, `"interactive":false`) {
				hello = strings.Replace(hello, `"interactive":true,`, "", 1)
			}
			h := connectAnswerHost(t, r, hello, func(mirror.Msg) mirror.Msg { return mirror.Msg{} })
			err := r.svc.AnswerOpenCodeAsk(ctx, "on", "work", c.window, c.prompt, c.choice)
			if !errors.Is(err, c.want) || !errors.Is(err, mirror.ErrAskNotSent) || !mirror.AskOutcomeKnown(err) {
				t.Fatalf("err = %v, want %v (not sent)", err, c.want)
			}
			time.Sleep(30 * time.Millisecond)
			if got := h.got(); len(got) != 0 {
				t.Fatalf("the host received %+v", got)
			}
		})
	}
}

// The host's refusals come back as muster's own.
func TestAnOpenCodeAnswersHostRefusals(t *testing.T) {
	for refusal, want := range map[string]error{
		mirror.AskRefusalOpenCodeGone:        mirror.ErrOpenCodeAskGone,
		mirror.AskRefusalOpenCodeUnreachable: mirror.ErrOpenCodeUnreachable,
		mirror.AskRefusalOpenCodeUnknown:     mirror.ErrOpenCodeAnswerUnknown,
		mirror.AskRefusalUnsupported:         mirror.ErrAskUnsupported,
	} {
		r := newRig(t)
		ctx, cancel := context.WithTimeout(asUser(1), 5*time.Second)
		connectAnswerHost(t, r, openCodeHello(`,"openCodeAnswer":true`, "once reject"), func(mirror.Msg) mirror.Msg {
			return mirror.Msg{Error: "the host's words", Refusal: refusal}
		})
		err := r.svc.AnswerOpenCodeAsk(ctx, "on", "work", "@1", "per_1", "once")
		cancel()
		if !errors.Is(err, want) || !strings.Contains(err.Error(), "the host's words") {
			t.Errorf("%s: %v, want %v", refusal, err, want)
		}
	}
}

// The wire: an opencode window's Pending keeps its own asks — a permission's
// choices only muster's (never "always"), a question's options only whole —
// and loses another runtime's; a Claude window, or one of a runtime muster
// does not know, never carries an opencode ask.
func TestAnOpenCodeWindowsPendingOnTheWire(t *testing.T) {
	oc := func(id string, p mirror.Pending) mirror.Window {
		return mirror.Window{Session: "s", ID: id, Runtime: mirror.RuntimeOpenCode, AgentSession: ocAnsSID, Status: mirror.StatusWaiting, Pending: &p}
	}
	got := mirror.NormalizeWindows([]mirror.Window{
		oc("@1", mirror.Pending{Kind: "permission", Summary: "bash: ls", Tool: "bash", PromptID: "per_1", Choices: "always reject once", Last: "said"}),
		oc("@2", mirror.Pending{Kind: "question", Summary: "Proceed?", PromptID: "frm_1", Options: "Yes\nNo", Detail: "x", Last: "said"}),
		oc("@3", mirror.Pending{Kind: "question", Summary: "Pick", PromptID: "frm_1", Options: "a\nb\nc\nd\ne"}),
		oc("@4", mirror.Pending{Kind: "approval", Summary: "x", PromptID: "p1", Choices: "once deny"}),
		oc("@5", mirror.Pending{Kind: "permission", Summary: "x", PromptID: "frm_1", Choices: "once"}),
		oc("@6", mirror.Pending{Kind: "question", Summary: "Pick", PromptID: "frm_1", Options: "a\na"}),
		{Session: "s", ID: "@7", Runtime: "unknown-runtime", Status: mirror.StatusWaiting,
			Pending: &mirror.Pending{Kind: "permission", Summary: "x", PromptID: "per_1", Choices: "once"}},
		{Session: "s", ID: "@8", Claude: true, Status: mirror.StatusWaiting,
			Pending: &mirror.Pending{Kind: "question", Summary: "Q", PromptID: "frm_1", Options: "Yes\nNo"}},
	}, 0)
	want := []*mirror.Pending{
		{Kind: "permission", Summary: "bash: ls", Tool: "bash", PromptID: "per_1", Choices: "once reject"},
		{Kind: "question", Summary: "Proceed?", PromptID: "frm_1", Options: "Yes\nNo"},
		// Options the wire would cut or cannot tell apart are not offered,
		// but the question keeps its id: the view answers its whole form.
		{Kind: "question", Summary: "Pick", PromptID: "frm_1"},
		nil,
		{Kind: "permission", Summary: "x"},
		{Kind: "question", Summary: "Pick", PromptID: "frm_1"},
		nil,
		{Summary: "Q"},
	}
	for i, w := range want {
		g := got[i].Pending
		if (g == nil) != (w == nil) || (g != nil && *g != *w) {
			t.Errorf("%s: %+v, want %+v", got[i].ID, g, w)
		}
	}
}

// An opencode ask's Pending at every bound is never larger on the wire than a
// Claude window's at its bounds, so the frame bound measured with Claude
// windows (internal/mirrorhost's TestTheLargestWindowListFitsOneFrame) holds
// for any mix. Every rune is one JSON writes six-fold.
func TestAnOpenCodePendingIsNoLargerThanAClaudeOne(t *testing.T) {
	size := func(w mirror.Window) int {
		n := mirror.NormalizeWindows([]mirror.Window{w}, 0)
		if n[0].Pending == nil {
			t.Fatalf("instrument check: the Pending was dropped: %+v", w)
		}
		b, _ := json.Marshal(n[0].Pending)
		return len(b)
	}
	claude := size(mirror.Window{Session: "s", ID: "@1", Claude: true, Status: mirror.StatusWaiting, Pending: &mirror.Pending{
		Summary: strings.Repeat("<", mirror.MaxPendingSummary), Tool: strings.Repeat("&", mirror.MaxPendingTool), Last: strings.Repeat(">", mirror.MaxPendingLast)}})
	opt := strings.Repeat("<", mirror.MaxPendingOption)
	opts := []string{}
	for i := 0; i < mirror.MaxPendingOptions; i++ {
		opts = append(opts, opt[:len(opt)-1]+string(rune('a'+i)))
	}
	oc := func(p mirror.Pending) mirror.Window {
		return mirror.Window{Session: "s", ID: "@1", Runtime: mirror.RuntimeOpenCode, AgentSession: ocAnsSID, Status: mirror.StatusWaiting, Pending: &p}
	}
	long := strings.Repeat("<", 1000)
	q := size(oc(mirror.Pending{Kind: "question", Summary: long, Tool: long, Last: long, Detail: long,
		PromptID: "frm_" + strings.Repeat("a", 124), Options: strings.Join(opts, "\n")}))
	p := size(oc(mirror.Pending{Kind: "permission", Summary: long, Tool: long, Last: long, Detail: long,
		PromptID: "per" + strings.Repeat("a", 125), Choices: "once reject"}))
	if q > claude || p > claude {
		t.Fatalf("an opencode Pending at its bounds is larger than a Claude one: question %d, permission %d, Claude %d bytes", q, p, claude)
	}
	t.Logf("Claude %d, opencode question %d, permission %d bytes", claude, q, p)
}

// ---------------------------------------------------------------- end to end

// ocE2EBackend lists one opencode window resolved NOW from src.
type ocE2EBackend struct {
	*fakeBackend
	src *mirrorhost.OpenCodeSource
}

func (b *ocE2EBackend) Windows(context.Context) ([]mirrorhost.WindowInfo, error) {
	ap := mirrorhost.ResolveOpenCodePane(b.src, "%1", 11, "OC | Ask session")
	return []mirrorhost.WindowInfo{{Session: "work", ID: "@1", Active: true, Pane: "%1", Command: "opencode", Agent: &ap}}, nil
}
func (b *ocE2EBackend) OpenCodeSource() *mirrorhost.OpenCodeSource { return b.src }

// The whole path with a REAL muster-mirror client and the fake opencode
// server driven by the REAL frames: the request raised in opencode is on the
// hub's window list at once, muster answers it, the server gets the reply —
// and the window stops waiting. A read-only host (not -interactive) lists the ask
// all the same (muster notifies) and advertises no answer.
func TestAnOpenCodeAskAnsweredEndToEnd(t *testing.T) {
	for _, on := range []bool{true, false} {
		r := newRig(t)
		ctx, cancel := context.WithTimeout(asUser(1), 10*time.Second)
		tok, _ := r.store.mint(t, 1, "on")
		srv := opencodetest.New("fixture-e2e-password")
		srv.PutSession(ocAnsSID, "Ask session", "")
		creds, err := opencodehost.EnvCredentials(srv.URL, srv.Issue())
		if err != nil {
			t.Fatal(err)
		}
		rctx, stop := context.WithCancel(context.Background())
		src := mirrorhost.NewOpenCodeSource(rctx, creds, "", log.New(io.Discard, "", 0))
		b := &ocE2EBackend{fakeBackend: &fakeBackend{}, src: src}
		c, err := mirrorhost.New(mirrorhost.Config{URL: r.srv.URL, Token: tok, Host: "on", Backend: b, Interactive: on,
			AllowHTTP: true, Refresh: time.Hour, Logger: log.New(io.Discard, "", 0)})
		if err != nil {
			t.Fatal(err)
		}
		go func() { _ = c.RunOnce(rctx) }()
		window := func() (mirror.Window, bool) {
			for _, h := range r.svc.Hosts(ctx) {
				for _, w := range h.Windows {
					if w.ID == "@1" {
						return w, true
					}
				}
			}
			return mirror.Window{}, false
		}
		eventually(t, "the window listed with its stream open", func() bool { _, ok := window(); return ok && srv.Subscribers() > 0 })
		srv.EmitFrame(opencodetest.FixtureFrame("permission.asked", 0))
		eventually(t, "the request on the hub", func() bool { w, _ := window(); return w.Status == mirror.StatusWaiting && w.Pending != nil })
		w, _ := window()
		if p := w.Pending; p.Kind != mirror.PendingPermission || p.Summary != "bash: rm -rf build" || p.PromptID != opencodetest.FixtureBashRequest || p.Choices != "once reject" {
			t.Fatalf("on the hub: %+v", p)
		}
		hv := r.svc.Hosts(ctx)[0]
		if hv.OpenCodeAnswer != on {
			t.Fatalf("the host's openCodeAnswer = %v, want %v", hv.OpenCodeAnswer, on)
		}
		err = r.svc.AnswerOpenCodeAsk(ctx, "on", "work", "@1", opencodetest.FixtureBashRequest, "once")
		if on {
			if err != nil {
				t.Fatal(err)
			}
			if rs := srv.Replies(); len(rs) != 1 || rs[0].Body != `{"decision":"once"}` {
				t.Fatalf("the server got %+v", rs)
			}
			eventually(t, "the window to stop waiting", func() bool { w, _ := window(); return w.Status != mirror.StatusWaiting })
		} else {
			if !errors.Is(err, mirror.ErrOpenCodeAnswerUnsupported) {
				t.Fatalf("err = %v", err)
			}
			if rs := srv.Replies(); len(rs) != 0 {
				t.Fatalf("a read-only host replied: %+v", rs)
			}
			// Settled in the terminal: the window stops waiting all the same.
			srv.EmitFrame(opencodetest.FixtureFrame("permission.replied", 0))
			eventually(t, "the window to stop waiting", func() bool { w, _ := window(); return w.Status != mirror.StatusWaiting })
		}
		stop()
		cancel()
		srv.Close()
	}
}
