package mirrorhost

import (
	"context"
	"io"
	"log"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/opencodehost"
	"github.com/ZacxDev/muster/internal/opencodehost/opencodetest"
)

// What muster can offer of an opencode question: one single-select question's
// own option labels (the Yes/No form); a form of two questions names the
// first and how many more, and offers nothing (the terminal answers it).
func TestAnOpenCodeQuestionOffersItsOptionsOnlyWhenMusterCanAnswerIt(t *testing.T) {
	srv, _, pane := askSource(t, "OC | Fixture session")
	srv.EmitFrame(opencodetest.FixtureFrame("form.created", 0)) // two questions
	waitUntil(t, "the questions", func() bool { return pane().Status == mirror.StatusWaiting })
	if p := pane().Pending; p == nil || p.Summary != "Which database should the fixture use? (and 1 more question)" || p.Options != "" || p.PromptID != "" {
		t.Fatalf("two questions: %+v", p)
	}
	srv.EmitFrame(opencodetest.FixtureFrame("form.replied", 0))
	srv.EmitFrame(opencodetest.FixtureFrame("form.created", 1)) // "Proceed?"
	waitUntil(t, "the one question", func() bool { p := pane().Pending; return p != nil && p.Summary == "Proceed?" })
	if p := pane().Pending; p.Options != "Yes\nNo" || p.PromptID != opencodetest.FixtureProceedForm || !p.Offers("Yes") || p.Offers("Maybe") {
		t.Fatalf("one question: %+v", p)
	}
}

// offerableCase is a question form ask with labels.
func offerableCase(typ string, labels ...string) openCodeAsk {
	f := opencodehost.Field{Key: "q0", Type: typ, Description: "Pick one"}
	for _, l := range labels {
		f.Options = append(f.Options, struct {
			Value string `json:"value"`
			Label string `json:"label"`
		}{Value: "v-" + l, Label: l})
	}
	return openCodeAsk{Ask: opencodehost.Ask{Kind: opencodehost.AskQuestion, ID: "frm_case", Session: "ses_case", Fields: []opencodehost.Field{f}}}
}

// The offer is all of a question's labels or none: a multi-select, more than
// mirror.MaxPendingOptions options, a label the wire's cleaning would change
// (a newline, one over MaxPendingOption runes), a duplicate, or one shaped
// like a secret leaves it to the terminal — never a cut or partial list.
func TestAQuestionIsOfferedWholeOrNotAtAll(t *testing.T) {
	ok := offerableCase("string", "Yes", "No")
	if got, can := ok.offerable(); !can || strings.Join(got, ",") != "Yes,No" {
		t.Fatalf("the control: %v %v", got, can)
	}
	long := strings.Repeat("x", mirror.MaxPendingOption+1)
	for name, a := range map[string]openCodeAsk{
		"multiselect":        offerableCase("multiselect", "Yes", "No"),
		"too many options":   offerableCase("string", "a", "b", "c", "d", "e"),
		"a label with a \\n": offerableCase("string", "Yes", "N\no"),
		"a label over bound": offerableCase("string", "Yes", long),
		"a duplicate label":  offerableCase("string", "Yes", "Yes"),
		"a secret label":     offerableCase("string", "Yes", "ghp_"+strings.Repeat("A", 36)),
		"no options":         offerableCase("string"),
	} {
		if got, can := a.offerable(); can {
			t.Errorf("%s: offered %q", name, got)
		}
		if p := a.pendingOf(); p.Options != "" || p.PromptID != "" {
			t.Errorf("%s: its Pending offers %+v", name, p)
		}
	}
}

// An ask pending BEFORE the stream opened (raised while muster-mirror was
// down) is read from the session's pending lists when the window is matched.
func TestAnAskPendingBeforeTheStreamOpenedIsRead(t *testing.T) {
	srv, src := ocServer(t, map[string]string{opencodetest.FixturePermissionSession: "Ask session"})
	srv.EmitFrame(opencodetest.FixtureFrame("permission.asked", 0)) // no stream yet: only the list holds it
	_ = src.state()
	waitUntil(t, "the event stream", func() bool { s, _ := src.status("x"); return s != "" })
	procs, children, root := ocPaneTable(t, "opencode")
	rt := openCodeRuntime{proc: root, src: src, title: titled("OC | Ask session")}
	waitUntil(t, "the seeded request", func() bool {
		return rt.LocatePanes(ocPane, procs, children)["%1"].Status == mirror.StatusWaiting
	})
	n := 0
	for _, r := range srv.Requests() {
		if strings.HasSuffix(r.Path, "/permission") || strings.HasSuffix(r.Path, "/form") {
			n++
		}
	}
	// Listed again and again, the session is read once per connect.
	for i := 0; i < 5; i++ {
		rt.LocatePanes(ocPane, procs, children)
	}
	time.Sleep(50 * time.Millisecond)
	m := 0
	for _, r := range srv.Requests() {
		if strings.HasSuffix(r.Path, "/permission") || strings.HasSuffix(r.Path, "/form") {
			m++
		}
	}
	if n != 2 || m != 2 {
		t.Fatalf("the pending lists were read %d then %d times, want 2 (one connect: permission and form)", n, m)
	}
}

// While the stream is down the host claims no ask (it cannot see one settle):
// the window's status is unknown, not waiting.
func TestNoAskIsClaimedWhileTheStreamIsDown(t *testing.T) {
	srv, src, pane := askSource(t, "OC | Ask session")
	srv.EmitFrame(opencodetest.FixtureFrame("permission.asked", 0))
	waitUntil(t, "waiting", func() bool { return pane().Status == mirror.StatusWaiting })
	src.mu.Lock()
	src.connected = false
	src.mu.Unlock()
	if ap := pane(); ap.Status == mirror.StatusWaiting || ap.Pending != nil {
		t.Fatalf("with the stream down: %+v", ap)
	}
}

// liveOCBackend lists one opencode window resolved NOW from src, as the tmux
// backend does on every listing.
type liveOCBackend struct {
	*windowsBackend
	src   *OpenCodeSource
	title string
}

func (b liveOCBackend) OpenCodeSource() *OpenCodeSource { return b.src }
func (b liveOCBackend) Windows(context.Context) ([]WindowInfo, error) {
	return []WindowInfo{ocWindowInfo(b.src, b.title)}, nil
}

// runOCHost runs a host over d listing one opencode window titled title.
func runOCHost(t *testing.T, src *OpenCodeSource, title string, interactive bool) *drivenMuster {
	t.Helper()
	d := newDrivenMuster(t)
	b := liveOCBackend{windowsBackend: &windowsBackend{}, src: src, title: title}
	c, err := New(Config{URL: d.srv.URL, Token: mirror.TokenPrefix + "t", Host: "laptop", Backend: b, Interactive: interactive,
		AllowHTTP: true, Refresh: time.Hour, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = c.RunOnce(ctx) }()
	waitUntil(t, "the hello", func() bool { return len(d.of(mirror.TypeHello)) == 1 })
	return d
}

// helloAnswers is what the host's hello advertised as OpenCodeAnswer.
func helloAnswers(d *drivenMuster) bool { return d.of(mirror.TypeHello)[0].OpenCodeAnswer }

func ocAnswer(id uint32, prompt, choice string) mirror.Msg {
	return mirror.Msg{Type: mirror.TypeAnswer, ID: id, Session: "o", Window: "@7", Runtime: mirror.RuntimeOpenCode,
		OpenCodeReply: &mirror.OpenCodeReply{PromptID: prompt, Choice: choice}}
}

// lastPending is the Pending the host last sent for window @7 (nil: none).
func lastPending(d *drivenMuster) *mirror.Pending {
	ms := append(d.of(mirror.TypeHello), d.of(mirror.TypeSessions)...)
	var p *mirror.Pending
	for _, m := range ms {
		for _, w := range m.Windows {
			if w.ID == "@7" {
				p = w.Pending
			}
		}
	}
	return p
}

// A raised ask reaches muster at ONCE (no refresh: the host's is an hour
// here), and so does its settling in the terminal.
func TestARaisedAskIsSentAtOnceAndSoIsItsSettling(t *testing.T) {
	srv, src, _ := askSource(t, "OC | Ask session")
	d := runOCHost(t, src, "OC | Ask session", false)
	srv.EmitFrame(opencodetest.FixtureFrame("permission.asked", 0))
	waitUntil(t, "the request on the wire", func() bool { p := lastPending(d); return p != nil && p.Kind == mirror.PendingPermission })
	srv.EmitFrame(opencodetest.FixtureFrame("permission.replied", 0))
	waitUntil(t, "the settling on the wire", func() bool { return lastPending(d) == nil })
}

// 🔴 The host's own half of an answer, whatever muster sends: a read-only
// host (not -interactive: a non-default tenant's, say) relays nothing to
// opencode and advertises no answer — it still LISTS the ask (muster
// notifies); an -interactive one relays only the ask the window waits on NOW, with an answer it offers —
// sent as the reply the real server takes — and "always", another ask's id
// or an option the question does not have are refused without a request.
func TestTheHostRelaysOnlyAnAnswerTheWindowOffersNow(t *testing.T) {
	for _, c := range []struct{ name string }{{"read-only host"}} {
		t.Run(c.name, func(t *testing.T) {
			srv, src, _ := askSource(t, "OC | Ask session")
			srv.EmitFrame(opencodetest.FixtureFrame("permission.asked", 0))
			d := runOCHost(t, src, "OC | Ask session", false)
			waitUntil(t, "the request on the wire", func() bool { p := lastPending(d); return p != nil && p.Kind == mirror.PendingPermission })
			d.send(t, ocAnswer(1, opencodetest.FixtureBashRequest, "once"))
			if m := answeredOf(t, d, 1); m.Refusal != mirror.AskRefusalUnsupported || m.Error == "" {
				t.Fatalf("%+v", m)
			}
			if n := len(srv.Replies()); n != 0 {
				t.Fatalf("%d replies reached opencode", n)
			}
			// Checked AFTER the refusal, so the refusal is what a broken host
			// guard fails first.
			if helloAnswers(d) {
				t.Fatal("hello.OpenCodeAnswer advertised an answer this host refuses")
			}
		})
	}
	t.Run("a permission", func(t *testing.T) {
		srv, src, _ := askSource(t, "OC | Ask session")
		srv.EmitFrame(opencodetest.FixtureFrame("permission.asked", 0))
		d := runOCHost(t, src, "OC | Ask session", true)
		if !helloAnswers(d) {
			t.Fatal("hello.OpenCodeAnswer is off on an -interactive host with a token")
		}
		waitUntil(t, "the request on the wire", func() bool { return lastPending(d) != nil })
		// Another session's question, pending on the same server: this window
		// does not wait on it, so its id is no answer here.
		srv.EmitFrame(opencodetest.FixtureFrame("form.created", 1))
		waitUntil(t, "the other session's question at the host", func() bool {
			src.mu.Lock()
			defer src.mu.Unlock()
			_, ok := src.asks[opencodetest.FixtureProceedForm]
			return ok
		})
		for i, bad := range []mirror.Msg{
			ocAnswer(1, opencodetest.FixtureBashRequest, "always"),
			ocAnswer(2, opencodetest.FixtureEditRequest, "once"),
			ocAnswer(3, "per_../../api/pair", "once"),
			ocAnswer(4, opencodetest.FixtureProceedForm, "Yes"),
		} {
			d.send(t, bad)
			if m := answeredOf(t, d, uint32(i+1)); m.Refusal != mirror.AskRefusalOpenCodeGone || m.Error == "" {
				t.Fatalf("bad answer %d: %+v", i+1, m)
			}
		}
		if n := len(srv.Replies()); n != 0 {
			t.Fatalf("a refused answer reached opencode: %+v", srv.Replies())
		}
		d.send(t, ocAnswer(5, opencodetest.FixtureBashRequest, "reject"))
		if m := answeredOf(t, d, 5); m.Error != "" {
			t.Fatalf("the answer: %+v", m)
		}
		if r := srv.Replies(); len(r) != 1 || r[0].Body != `{"decision":"reject"}` ||
			r[0].Path != "/api/session/"+opencodetest.FixturePermissionSession+"/permission/"+opencodetest.FixtureBashRequest+"/reply" {
			t.Fatalf("the reply: %+v", r)
		}
		waitUntil(t, "the request to leave the wire", func() bool { return lastPending(d) == nil })
		// Answered: a second answer is the ask gone.
		d.send(t, ocAnswer(6, opencodetest.FixtureBashRequest, "once"))
		if m := answeredOf(t, d, 6); m.Refusal != mirror.AskRefusalOpenCodeGone {
			t.Fatalf("a second answer: %+v", m)
		}
	})
	t.Run("a question", func(t *testing.T) {
		srv, src, _ := askSource(t, "OC | Fixture session")
		srv.EmitFrame(opencodetest.FixtureFrame("form.created", 1))
		d := runOCHost(t, src, "OC | Fixture session", true)
		waitUntil(t, "the question on the wire", func() bool { p := lastPending(d); return p != nil && p.Options == "Yes\nNo" })
		d.send(t, ocAnswer(1, opencodetest.FixtureProceedForm, "Maybe"))
		if m := answeredOf(t, d, 1); m.Refusal != mirror.AskRefusalOpenCodeGone {
			t.Fatalf("an option it does not have: %+v", m)
		}
		d.send(t, ocAnswer(2, opencodetest.FixtureProceedForm, "No"))
		if m := answeredOf(t, d, 2); m.Error != "" {
			t.Fatalf("the answer: %+v", m)
		}
		if r := srv.Replies(); len(r) != 1 || r[0].Body != `{"answer":{"q0":"No"}}` {
			t.Fatalf("the reply: %+v", r)
		}
	})
	t.Run("answered in the terminal first", func(t *testing.T) {
		srv, src, _ := askSource(t, "OC | Ask session")
		srv.EmitFrame(opencodetest.FixtureFrame("permission.asked", 0))
		d := runOCHost(t, src, "OC | Ask session", true)
		waitUntil(t, "the request on the wire", func() bool { return lastPending(d) != nil })
		// The server settles it, but the event is lost: the host's own set
		// still holds it, and opencode's 404 is what says it is gone.
		srv.RefuseReplies(404)
		d.send(t, ocAnswer(1, opencodetest.FixtureBashRequest, "once"))
		if m := answeredOf(t, d, 1); m.Refusal != mirror.AskRefusalOpenCodeGone {
			t.Fatalf("%+v", m)
		}
	})
}

// The refusals the host names are the ones the hub maps (one name, both
// sides), and the ids it carries agree with opencodehost's.
func TestTheOpenCodeIDRulesAgree(t *testing.T) {
	for _, id := range []string{"per_1a2aa6345001HPvxtQP9FD941m", "frm_1a2aa3ec2001NmTTM10yDwGseq", "per", "frm_", "per_a/b", "ses_x",
		"per" + strings.Repeat("a", 125), "per" + strings.Repeat("a", 126), "frm_" + strings.Repeat("a", 124), "frm_" + strings.Repeat("a", 125), "frmx"} {
		host := opencodehost.ValidRequestID(id) || opencodehost.ValidFormID(id)
		if wire := mirror.ValidOpenCodeAskID(id); wire != host {
			t.Errorf("%q: the wire says %v, opencodehost %v", id, wire, host)
		}
	}
	if !slices.Equal(mirror.OpenCodePermissionChoices, []string{string(opencodehost.PermissionOnce), string(opencodehost.PermissionReject)}) {
		t.Errorf("the choices muster offers %v are not the two opencodehost sends", mirror.OpenCodePermissionChoices)
	}
}
