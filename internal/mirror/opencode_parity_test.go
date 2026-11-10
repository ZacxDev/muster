package mirror_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
	"github.com/ZacxDev/muster/internal/opencodehost"
	"github.com/ZacxDev/muster/internal/opencodehost/opencodetest"
)

// OPENCODE PARITY, SERVER HALF: the hand-opened pane's binding on the wire
// (agentbind.go), the context readout's Msg.Context, and an opencode
// question answered whole (opencode_form.go) — each compatible both ways
// with an older host and an older muster.

// TestAgentBindRidesOnlyAGatedWindowNamingItsSession: NormalizeWindows keeps
// AgentBind on an opencode window that names its session and is not
// launcher-bound, with a well-formed code — a code this muster does not know
// included (a newer host's) — and drops it everywhere else.
func TestAgentBindRidesOnlyAGatedWindowNamingItsSession(t *testing.T) {
	oc := func(id, session, bind string, send bool) mirror.Window {
		return mirror.Window{Session: "s", ID: id, Runtime: mirror.RuntimeOpenCode, AgentSession: session, AgentBind: bind, AgentSend: send}
	}
	in := []mirror.Window{
		oc("@1", ocSendSID, mirror.AgentBindVerified, false),
		oc("@2", ocSendSID, mirror.AgentBindTitleShared, false),
		oc("@3", ocSendSID, "a-newer-code", false),
		oc("@4", "", mirror.AgentBindVerified, false),       // no session
		oc("@5", ocSendSID, mirror.AgentBindVerified, true), // launcher-bound: no code
		oc("@6", ocSendSID, "Not A Code!", false),           // malformed
		{Session: "s", ID: "@7", Claude: true, ClaudeSession: "5a6b7c8d-9e0f-4a1b-8c2d-3e4f5a6b7c8d", AgentBind: mirror.AgentBindVerified},
		{Session: "s", ID: "@8", Runtime: "hermes", AgentSession: ocSendSID, AgentBind: mirror.AgentBindVerified},
	}
	want := map[string]string{"@1": mirror.AgentBindVerified, "@2": mirror.AgentBindTitleShared, "@3": "a-newer-code"}
	for _, w := range mirror.NormalizeWindows(in, 0) {
		if w.AgentBind != want[w.ID] {
			t.Errorf("%s: AgentBind %q, want %q", w.ID, w.AgentBind, want[w.ID])
		}
	}
}

// TestEachUnboundCodeIsRefusedInItsOwnWords: the page (SendBlocked) and the
// hub say the code's reason — pinned literally — and an older host's window
// (no code) or a code this muster does not know says the launcher's.
func TestEachUnboundCodeIsRefusedInItsOwnWords(t *testing.T) {
	for code, want := range map[string]string{
		mirror.AgentBindTitleShared: "Not sent: a subagent's opencode session has the same title as this pane (or one that starts the same way), so muster can't be sure which session it shows. Type it in the terminal, or start it with `muster-mirror opencode`.",
		mirror.AgentBindArgv:        "Not sent: this opencode was started on one session (its -s) but its title names another, so muster can't be sure which it shows. Type it in the terminal, or start it with `muster-mirror opencode`.",
		mirror.AgentBindStream:      "Not sent: muster can't confirm which session this opencode shows right now, because the opencode server's event stream is not answering. Try again in a moment.",
		"":                          mirror.PasteRefusedOpenCodeUnboundText,
		"a-newer-code":              mirror.PasteRefusedOpenCodeUnboundText,
	} {
		if got := mirror.AgentBindWords(code); got != want {
			t.Errorf("AgentBindWords(%q) = %q", code, got)
		}
	}
	r, raw := ocSendHost(t, true, true)
	ctx, cancel := context.WithTimeout(asUser(1), 5*time.Second)
	defer cancel()
	err := r.svc.PasteWindow(ctx, "h", "work", "@6", "hello", mirror.PasteClaude)
	var pr *mirror.PasteRefusedError
	if !errors.Is(err, mirror.ErrOpenCodeUnbound) || !errors.As(err, &pr) || pr.Why != mirror.PasteRefusedOpenCodeTitleSharedText ||
		mirror.PasteGuardRefusal(err) != mirror.PasteRefusalOpenCodeUnbound {
		t.Fatalf("the hub refused @6 with %v", err)
	}
	if got := raw.of(mirror.TypePaste); len(got) != 0 {
		t.Fatalf("the host received %+v", got)
	}
}

// TestAGatedPasteNamesTheSessionTheListShows: a send to a verified window —
// and to a launcher-bound one — carries Msg.AgentSession, the session the
// hub's list names for it, which is the only one its host may post to.
func TestAGatedPasteNamesTheSessionTheListShows(t *testing.T) {
	for window, session := range map[string]string{"@5": ocSendSIDHand, "@1": ocSendSID} {
		r, raw := ocSendHost(t, true, true)
		go func() {
			eventually(t, "the paste", func() bool { return len(raw.of(mirror.TypePaste)) == 1 })
			raw.write(t, mirror.Msg{Type: mirror.TypePasted, ID: raw.of(mirror.TypePaste)[0].ID})
		}()
		ctx, cancel := context.WithTimeout(asUser(1), 5*time.Second)
		if err := r.svc.PasteWindow(ctx, "h", "work", window, "hello", mirror.PasteClaude, mirror.MessageKey("nonce-aaaaaaaaaaaaaaaa"),
			mirror.ShownSession(session)); err != nil {
			t.Fatal(err)
		}
		cancel()
		if m := raw.of(mirror.TypePaste)[0]; m.AgentSession != session || m.Runtime != mirror.RuntimeOpenCode {
			t.Fatalf("%s: the paste names session %q (runtime %q), want %q", window, m.AgentSession, m.Runtime, session)
		}
	}
}

// TestTheNewWireIsReadByAnOlderPeerAsBefore: both directions, in JSON as the
// other side decodes it.
//   - An older MUSTER reads a new host's verified window with the fields it
//     knows: AgentSend is false, so it stays view-only there; Msg.Context and
//     a paste's agentSession are keys it does not have, ignored.
//   - An older HOST's hello has no agentBind: the new muster lists its
//     title-matched window unsendable, in the launcher's words.
func TestTheNewWireIsReadByAnOlderPeerAsBefore(t *testing.T) {
	// An older muster's view of a window and of an events message: the fields
	// it had before this change.
	type oldWindow struct {
		Runtime      string `json:"runtime"`
		AgentSession string `json:"agentSession"`
		AgentSend    bool   `json:"agentSend"`
	}
	type oldMsg struct {
		Type    string      `json:"type"`
		Offset  int64       `json:"offset"`
		Windows []oldWindow `json:"windows"`
	}
	host := mirror.Msg{Type: mirror.TypeHello, Windows: mirror.NormalizeWindows([]mirror.Window{{Session: "s", ID: "@1", Runtime: mirror.RuntimeOpenCode,
		AgentSession: ocSendSIDHand, AgentBind: mirror.AgentBindVerified}}, 0)}
	b, _ := json.Marshal(host)
	var old oldMsg
	if err := json.Unmarshal(b, &old); err != nil || len(old.Windows) != 1 || old.Windows[0].AgentSend || old.Windows[0].AgentSession != ocSendSIDHand {
		t.Fatalf("an older muster reads %s as %+v (%v)", b, old, err)
	}
	if !strings.Contains(string(b), `"agentBind":"verified"`) {
		t.Fatalf("instrument check: the new field is not on the wire: %s", b)
	}
	ev, _ := json.Marshal(mirror.Msg{Type: mirror.TypeEvents, Offset: 7, Context: &mirror.AgentContext{Tokens: 46809, Limit: 187500, Model: "m"},
		AgentSession: ocSendSID})
	var oldEv oldMsg
	if err := json.Unmarshal(ev, &oldEv); err != nil || oldEv.Type != mirror.TypeEvents || oldEv.Offset != 7 {
		t.Fatalf("an older peer reads %s as %+v (%v)", ev, oldEv, err)
	}

	// An older host: the same window with no agentBind.
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "h")
	m := winHello("h", true, mirror.Window{Session: "work", ID: "@1", Index: 0, Active: true, Runtime: mirror.RuntimeOpenCode,
		AgentSession: ocSendSIDHand, Status: mirror.StatusIdle, StatusSince: 9})
	m.StatusReport, m.OpenCodeSend, m.PanePaste = true, true, true
	raw, err := r.rawConnect(t, tok, m)
	if err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "h", "work")
	hosts := r.svc.Hosts(asUser(1))
	e := mirror.Ordered(hosts)[0]
	if why := mirror.SendBlocked(hosts[0], e); why != mirror.PasteRefusedOpenCodeUnboundText {
		t.Fatalf("an older host's title-matched window: %q", why)
	}
	ctx, cancel := context.WithTimeout(asUser(1), 5*time.Second)
	defer cancel()
	if err := r.svc.PasteWindow(ctx, "h", "work", "@1", "hello", mirror.PasteClaude); !errors.Is(err, mirror.ErrOpenCodeUnbound) {
		t.Fatalf("a send to an older host's title-matched window: %v", err)
	}
	if got := raw.of(mirror.TypePaste); len(got) != 0 {
		t.Fatalf("the older host received %+v", got)
	}
}

// ------------------------------------------------- an opencode question, whole

// fixtureForm is the captured two-question form as the wire carries it.
func fixtureForm() mirror.OpenCodeForm {
	return mirror.OpenCodeForm{PromptID: opencodetest.FixtureQuestionsForm, Title: "Questions", Questions: []mirror.OpenCodeQuestion{
		{Key: "q0", Header: "Database", Question: "Which database should the fixture use?", Custom: true,
			Options: []mirror.OpenCodeOption{{Label: "Postgres (Recommended)"}, {Label: "SQLite"}}},
		{Key: "q1", Header: "Extras", Question: "Which extras?", Multiple: true, Custom: true,
			Options: []mirror.OpenCodeOption{{Label: "cache"}, {Label: "metrics"}}},
	}}
}

// TestPicksAreCheckedAgainstTheForm: every rule of CheckOpenCodePicks, each
// refused case beside the accepted control.
func TestPicksAreCheckedAgainstTheForm(t *testing.T) {
	f := fixtureForm()
	ok := []mirror.OpenCodePick{{Key: "q0", Picks: []int{1}}, {Key: "q1", Picks: []int{0, 1}, Other: "tracing"}}
	if err := mirror.CheckOpenCodePicks(f, ok); err != nil {
		t.Fatalf("the control: %v", err)
	}
	// What the card's box takes is taken: its bound is the box's, and an API
	// answer may run over lines.
	if mirror.MaxFormOther != askq.MaxFreeText {
		t.Fatalf("MaxFormOther %d, the box's bound %d", mirror.MaxFormOther, askq.MaxFreeText)
	}
	for name, other := range map[string]string{"at the bound": strings.Repeat("x", askq.MaxFreeText), "over two lines": "first\nsecond"} {
		if err := mirror.CheckOpenCodePicks(f, []mirror.OpenCodePick{{Key: "q0", Other: other}, ok[1]}); err != nil {
			t.Errorf("an answer of one's own %s: %v", name, err)
		}
	}
	strict := fixtureForm()
	strict.Questions[0].Custom = false
	for name, c := range map[string]struct {
		f     mirror.OpenCodeForm
		picks []mirror.OpenCodePick
	}{
		"one answer short":            {f, ok[:1]},
		"out of order":                {f, []mirror.OpenCodePick{ok[1], ok[0]}},
		"an index past the options":   {f, []mirror.OpenCodePick{{Key: "q0", Picks: []int{2}}, ok[1]}},
		"a negative index":            {f, []mirror.OpenCodePick{{Key: "q0", Picks: []int{-1}}, ok[1]}},
		"an option twice":             {f, []mirror.OpenCodePick{ok[0], {Key: "q1", Picks: []int{0, 0}}}},
		"two answers to one question": {f, []mirror.OpenCodePick{{Key: "q0", Picks: []int{0}, Other: "x"}, ok[1]}},
		"no answer":                   {f, []mirror.OpenCodePick{{Key: "q0"}, ok[1]}},
		"blank text of one's own":     {f, []mirror.OpenCodePick{{Key: "q0", Other: "   "}, ok[1]}},
		"text over the bound":         {f, []mirror.OpenCodePick{{Key: "q0", Other: strings.Repeat("x", mirror.MaxFormOther+1)}, ok[1]}},
		"a control character":         {f, []mirror.OpenCodePick{{Key: "q0", Other: "a\x1bb"}, ok[1]}},
		"one's own where not taken":   {strict, []mirror.OpenCodePick{{Key: "q0", Other: "MySQL"}, ok[1]}},
	} {
		if err := mirror.CheckOpenCodePicks(c.f, c.picks); !errors.Is(err, mirror.ErrOpenCodeFormInvalid) {
			t.Errorf("%s: %v, want a refusal", name, err)
		}
	}
}

// TestAFormPastItsBoundsIsNotAnswered: NormalizeOpenCodeForm refuses a form
// whole — never cut, so an index the card shows is always the host's.
func TestAFormPastItsBoundsIsNotAnswered(t *testing.T) {
	if _, ok := mirror.NormalizeOpenCodeForm(fixtureForm()); !ok {
		t.Fatal("the control was refused")
	}
	many := fixtureForm()
	for i := 0; i < mirror.MaxFormOptions; i++ {
		many.Questions[0].Options = append(many.Questions[0].Options, mirror.OpenCodeOption{Label: "extra"})
	}
	dup := fixtureForm()
	dup.Questions[1].Key = "q0"
	noid := fixtureForm()
	noid.PromptID = "nope"
	bare := fixtureForm()
	bare.Questions[0].Options, bare.Questions[0].Custom = nil, false
	alike := fixtureForm()
	alike.Questions[1].Options[1].Label = "cache"
	for name, f := range map[string]mirror.OpenCodeForm{"too many options": many, "a repeated key": dup, "no ask id": noid, "nothing to answer": bare,
		"two options a person cannot tell apart": alike} {
		if _, ok := mirror.NormalizeOpenCodeForm(f); ok {
			t.Errorf("%s: accepted", name)
		}
	}
}

// formRig runs a REAL muster-mirror client (-interactive, or not) whose one
// window shows the fixture's form session, against the fake opencode server
// driven by the captured frames.
func formRig(t *testing.T, interactive bool) (*rig, *opencodetest.Server, func() (mirror.Window, bool)) {
	t.Helper()
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "on")
	srv := opencodetest.New("fixture-form-password")
	t.Cleanup(srv.Close)
	srv.PutSession(opencodetest.FixtureFormSession, "Form session", "")
	creds, err := opencodehost.EnvCredentials(srv.URL, srv.Issue())
	if err != nil {
		t.Fatal(err)
	}
	rctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	src := mirrorhost.NewOpenCodeSource(rctx, creds, "", log.New(io.Discard, "", 0))
	b := &formBackend{fakeBackend: &fakeBackend{}, src: src}
	c, err := mirrorhost.New(mirrorhost.Config{URL: r.srv.URL, Token: tok, Host: "on", Backend: b, Interactive: interactive,
		AllowHTTP: true, Refresh: time.Hour, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = c.RunOnce(rctx) }()
	window := func() (mirror.Window, bool) {
		for _, h := range r.svc.Hosts(asUser(1)) {
			for _, w := range h.Windows {
				if w.ID == "@1" {
					return w, true
				}
			}
		}
		return mirror.Window{}, false
	}
	eventually(t, "the window listed with its stream open", func() bool { _, ok := window(); return ok && srv.Subscribers() > 0 })
	return r, srv, window
}

type formBackend struct {
	*fakeBackend
	src *mirrorhost.OpenCodeSource
}

func (b *formBackend) Windows(context.Context) ([]mirrorhost.WindowInfo, error) {
	ap := mirrorhost.ResolveOpenCodePane(b.src, "%1", 11, "OC | Form session")
	return []mirrorhost.WindowInfo{{Session: "work", ID: "@1", Active: true, Pane: "%1", Command: "opencode", Agent: &ap}}, nil
}
func (b *formBackend) OpenCodeSource() *mirrorhost.OpenCodeSource { return b.src }

// TestAnOpenCodeQuestionIsAnsweredWholeEndToEnd: the question tool's
// two-question form — which the window list could never offer as buttons —
// is on the hub with its id, its whole form is read from the host, and its
// answer reaches the server for EXACTLY that form in that session, as the
// options' values; then the window stops waiting.
func TestAnOpenCodeQuestionIsAnsweredWholeEndToEnd(t *testing.T) {
	r, srv, window := formRig(t, true)
	srv.EmitFrame(opencodetest.FixtureFrame("form.created", 0))
	eventually(t, "the question on the hub", func() bool { w, _ := window(); return w.Status == mirror.StatusWaiting && w.Pending != nil })
	w, _ := window()
	if p := w.Pending; p.Kind != mirror.PendingQuestion || p.PromptID != opencodetest.FixtureQuestionsForm || p.Options != "" {
		t.Fatalf("on the hub: %+v", p)
	}
	if hv := r.svc.Hosts(asUser(1))[0]; !hv.OpenCodeForms || !hv.OpenCodeAnswer {
		t.Fatalf("the host's forms/answers: %+v", hv)
	}
	ctx, cancel := context.WithTimeout(asUser(1), 10*time.Second)
	defer cancel()
	f, err := r.svc.OpenCodeForm(ctx, "on", "work", "@1", opencodetest.FixtureQuestionsForm)
	if err != nil || len(f.Questions) != 2 || f.Questions[1].Options[1].Label != "metrics" || !f.Questions[1].Multiple {
		t.Fatalf("the form: %+v (%v)", f, err)
	}
	// 🔴 Another ask's id is refused before anything reaches the host.
	if _, err := r.svc.OpenCodeForm(ctx, "on", "work", "@1", opencodetest.FixtureProceedForm); !errors.Is(err, mirror.ErrOpenCodeAskGone) {
		t.Fatalf("another ask's form: %v", err)
	}
	other := f
	other.PromptID = opencodetest.FixtureProceedForm
	if err := r.svc.AnswerOpenCodeForm(ctx, "on", "work", "@1", other, []mirror.OpenCodePick{{Key: "q0", Picks: []int{0}}, {Key: "q1", Picks: []int{0}}}); !errors.Is(err, mirror.ErrOpenCodeAskGone) {
		t.Fatalf("an answer naming another ask: %v", err)
	}
	if rs := srv.Replies(); len(rs) != 0 {
		t.Fatalf("the server got %+v", rs)
	}
	if err := r.svc.AnswerOpenCodeForm(ctx, "on", "work", "@1", f, []mirror.OpenCodePick{{Key: "q0", Picks: []int{1}}, {Key: "q1", Picks: []int{1}, Other: "tracing"}}); err != nil {
		t.Fatal(err)
	}
	rs := srv.Replies()
	want := "/api/session/" + opencodetest.FixtureFormSession + "/form/" + opencodetest.FixtureQuestionsForm + "/reply"
	if len(rs) != 1 || rs[0].Path != want || rs[0].Body != `{"answer":{"q0":"SQLite","q1":["metrics","tracing"]}}` {
		t.Fatalf("the server got %+v", rs)
	}
	eventually(t, "the window to stop waiting", func() bool { w, _ := window(); return w.Status != mirror.StatusWaiting })
}

// TestAReadOnlyHostSendsNoForm: a host that is not -interactive advertises no
// forms, and muster asks it for none.
func TestAReadOnlyHostSendsNoForm(t *testing.T) {
	r, srv, window := formRig(t, false)
	srv.EmitFrame(opencodetest.FixtureFrame("form.created", 0))
	eventually(t, "the question on the hub", func() bool { w, _ := window(); return w.Status == mirror.StatusWaiting })
	if hv := r.svc.Hosts(asUser(1))[0]; hv.OpenCodeForms {
		t.Fatal("a read-only host advertised forms")
	}
	ctx, cancel := context.WithTimeout(asUser(1), 5*time.Second)
	defer cancel()
	if _, err := r.svc.OpenCodeForm(ctx, "on", "work", "@1", opencodetest.FixtureQuestionsForm); !errors.Is(err, mirror.ErrOpenCodeFormsUnsupported) {
		t.Fatalf("err = %v", err)
	}
}
