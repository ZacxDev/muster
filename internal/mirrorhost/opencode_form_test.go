package mirrorhost

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/opencodehost/opencodetest"
)

// AN OPENCODE QUESTION, WHOLE — HOST HALF (opencode_form.go), against the
// fake server driven by the REAL captured frames (opencodetest): the form the
// question tool raised, sent whole; its answer relayed as the option VALUES
// for exactly the session and form the ask names; every other answer refused
// with nothing posted.

// askHost runs an -interactive host whose window @7 (pane titled "Fixture
// session") shows the fixture's form session, with the stream open.
func askHost(t *testing.T) (*drivenMuster, *opencodetest.Server, *titledOCBackend) {
	t.Helper()
	srv, src := ocServer(t, map[string]string{
		opencodetest.FixtureFormSession:       "Fixture session",
		opencodetest.FixturePermissionSession: "Ask session",
	})
	streamUp(t, srv, src)
	d := newDrivenMuster(t)
	b := &titledOCBackend{windowsBackend: &windowsBackend{}, src: src, title: "OC | Fixture session"}
	c, err := New(Config{URL: d.srv.URL, Token: mirror.TokenPrefix + "t", Host: "laptop", Backend: b, Interactive: true,
		AllowHTTP: true, Refresh: time.Hour, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	b.client = c
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = c.RunOnce(ctx) }()
	waitUntil(t, "the hello", func() bool { return len(d.of(mirror.TypeHello)) == 1 })
	return d, srv, b
}

// raise emits frame and waits until the window waits on ask id.
func raise(t *testing.T, srv *opencodetest.Server, b *titledOCBackend, frame, id string) {
	t.Helper()
	srv.EmitFrame(frame)
	waitUntil(t, "the ask "+id, func() bool {
		ws, _ := b.Windows(context.Background())
		p := ws[0].Agent.Pending
		return ws[0].Agent.Status == mirror.StatusWaiting && p != nil && p.PromptID == id
	})
}

func formedOf(t *testing.T, d *drivenMuster, id uint32) mirror.Msg {
	t.Helper()
	var out mirror.Msg
	waitUntil(t, "the formed answer", func() bool {
		for _, m := range d.of(mirror.TypeOpenCodeFormed) {
			if m.ID == id {
				out = m
				return true
			}
		}
		return false
	})
	return out
}

func formReq(id uint32, prompt string) mirror.Msg {
	return mirror.Msg{Type: mirror.TypeOpenCodeForm, ID: id, Session: "o", Window: "@7", Runtime: mirror.RuntimeOpenCode,
		OpenCodeReply: &mirror.OpenCodeReply{PromptID: prompt}}
}

func picksAnswer(id uint32, prompt string, picks ...mirror.OpenCodePick) mirror.Msg {
	return mirror.Msg{Type: mirror.TypeAnswer, ID: id, Session: "o", Window: "@7", Runtime: mirror.RuntimeOpenCode,
		OpenCodeReply: &mirror.OpenCodeReply{PromptID: prompt, Picks: picks}}
}

// TestTheHostSendsTheWholeQuestionForm: the captured two-question form —
// a single-select with two options, then a multi-select — each taking an
// answer of one's own, every word as the question tool wrote it.
func TestTheHostSendsTheWholeQuestionForm(t *testing.T) {
	d, srv, b := askHost(t)
	if !d.of(mirror.TypeHello)[0].OpenCodeForms {
		t.Fatal("an -interactive host with a token did not advertise OpenCodeForms")
	}
	raise(t, srv, b, opencodetest.FixtureFrame("form.created", 0), opencodetest.FixtureQuestionsForm)
	d.send(t, formReq(1, opencodetest.FixtureQuestionsForm))
	m := formedOf(t, d, 1)
	want := mirror.OpenCodeForm{PromptID: opencodetest.FixtureQuestionsForm, Title: "Questions", Questions: []mirror.OpenCodeQuestion{
		{Key: "q0", Header: "Database", Question: "Which database should the fixture use?", Custom: true,
			Options: []mirror.OpenCodeOption{{Label: "Postgres (Recommended)"}, {Label: "SQLite"}}},
		{Key: "q1", Header: "Extras", Question: "Which extras?", Multiple: true, Custom: true,
			Options: []mirror.OpenCodeOption{{Label: "cache"}, {Label: "metrics"}}},
	}}
	if m.Error != "" || m.Form == nil || !reflect.DeepEqual(*m.Form, want) {
		t.Fatalf("formed %+v (%+v)", m, m.Form)
	}
}

// TestAWholeFormsAnswerReachesExactlyItsSessionAndForm: picks become the
// options' VALUES — a single-select's one value, a multi-select's list, an
// answer of one's own after them — posted to the form the ask names in the
// session the ask names, once.
func TestAWholeFormsAnswerReachesExactlyItsSessionAndForm(t *testing.T) {
	for _, c := range []struct {
		name  string
		picks []mirror.OpenCodePick
		body  string
	}{
		{"options", []mirror.OpenCodePick{{Key: "q0", Picks: []int{1}}, {Key: "q1", Picks: []int{0, 1}}},
			`{"answer":{"q0":"SQLite","q1":["cache","metrics"]}}`},
		{"answers of one's own", []mirror.OpenCodePick{{Key: "q0", Other: "MySQL 8"}, {Key: "q1", Picks: []int{1}, Other: "tracing"}},
			`{"answer":{"q0":"MySQL 8","q1":["metrics","tracing"]}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, srv, b := askHost(t)
			raise(t, srv, b, opencodetest.FixtureFrame("form.created", 0), opencodetest.FixtureQuestionsForm)
			d.send(t, picksAnswer(1, opencodetest.FixtureQuestionsForm, c.picks...))
			if m := answeredOf(t, d, 1); m.Error != "" {
				t.Fatalf("refused: %+v", m)
			}
			r := srv.Replies()
			wantPath := "/api/session/" + opencodetest.FixtureFormSession + "/form/" + opencodetest.FixtureQuestionsForm + "/reply"
			if len(r) != 1 || r[0].Path != wantPath || !sameFormJSON(r[0].Body, c.body) {
				t.Fatalf("replies %+v; want one to %s with %s", r, wantPath, c.body)
			}
		})
	}
}

// TestAnAnswerTheFormCannotTakeIsRefusedAndNothingIsPosted: an ask the window
// does not wait on (another session's form, an id that is not pending), an
// option index past the question's, answers out of order or short, a Choice
// beside the picks — each refused, and the server receives nothing.
func TestAnAnswerTheFormCannotTakeIsRefusedAndNothingIsPosted(t *testing.T) {
	ok := []mirror.OpenCodePick{{Key: "q0", Picks: []int{0}}, {Key: "q1", Picks: []int{0}}}
	// Another session's form, raised in the permission session (the window
	// does not show it).
	other := strings.ReplaceAll(strings.ReplaceAll(opencodetest.FixtureFrame("form.created", 0),
		opencodetest.FixtureFormSession, opencodetest.FixturePermissionSession), opencodetest.FixtureQuestionsForm, "frm_1a2aa3ec2001OtherSessionFrm")
	for _, c := range []struct {
		name   string
		prep   func(*opencodetest.Server)
		answer mirror.Msg
	}{
		{"another session's form", func(srv *opencodetest.Server) { srv.EmitFrame(other) }, picksAnswer(1, "frm_1a2aa3ec2001OtherSessionFrm", ok...)},
		{"an id that is not pending", nil, picksAnswer(1, "frm_1a2aa3ec2001NoSuchFormXXX", ok...)},
		{"an index past the options", nil, picksAnswer(1, opencodetest.FixtureQuestionsForm, mirror.OpenCodePick{Key: "q0", Picks: []int{2}}, ok[1])},
		{"answers out of order", nil, picksAnswer(1, opencodetest.FixtureQuestionsForm, ok[1], ok[0])},
		{"a question unanswered", nil, picksAnswer(1, opencodetest.FixtureQuestionsForm, ok[0])},
		{"two answers to a single-select", nil, picksAnswer(1, opencodetest.FixtureQuestionsForm, mirror.OpenCodePick{Key: "q0", Picks: []int{0, 1}}, ok[1])},
		{"a choice beside the picks", nil, func() mirror.Msg {
			m := picksAnswer(1, opencodetest.FixtureQuestionsForm, ok...)
			m.OpenCodeReply.Choice = "SQLite"
			return m
		}()},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, srv, b := askHost(t)
			raise(t, srv, b, opencodetest.FixtureFrame("form.created", 0), opencodetest.FixtureQuestionsForm)
			if c.prep != nil {
				c.prep(srv)
			}
			d.send(t, c.answer)
			if m := answeredOf(t, d, 1); m.Error == "" {
				t.Fatalf("answered %+v; want a refusal", m)
			}
			if r := srv.Replies(); len(r) != 0 {
				t.Fatalf("the server received %+v", r)
			}
		})
	}
}

// TestAFormForAnAskTheWindowDoesNotWaitOnIsRefused: the form request too.
func TestAFormForAnAskTheWindowDoesNotWaitOnIsRefused(t *testing.T) {
	d, srv, b := askHost(t)
	raise(t, srv, b, opencodetest.FixtureFrame("form.created", 0), opencodetest.FixtureQuestionsForm)
	d.send(t, formReq(1, opencodetest.FixtureProceedForm))
	if m := formedOf(t, d, 1); m.Error == "" || m.Form != nil {
		t.Fatalf("formed %+v; want a refusal", m)
	}
}

// sameFormJSON reports whether two JSON texts say the same.
func sameFormJSON(a, b string) bool {
	var x, y any
	return json.Unmarshal([]byte(a), &x) == nil && json.Unmarshal([]byte(b), &y) == nil && reflect.DeepEqual(x, y)
}

// TestASubagentsFormIsAnsweredInTheChildSession: a form a subagent's child
// session raised waits on its ROOT's window; its answer goes to the CHILD
// session the ask names — never the window's own session.
func TestASubagentsFormIsAnsweredInTheChildSession(t *testing.T) {
	d, srv, b := askHost(t)
	b.set("OC | Ask session")
	srv.EmitFrame(opencodetest.FixtureFrame("session.created", 2)) // the child, under the Ask session
	child := strings.ReplaceAll(strings.ReplaceAll(opencodetest.FixtureFrame("form.created", 1),
		opencodetest.FixtureFormSession, opencodetest.FixtureChildSession), opencodetest.FixtureProceedForm, "frm_1a2aaa3d1001ChildSessionFrm")
	raise(t, srv, b, child, "frm_1a2aaa3d1001ChildSessionFrm")
	d.send(t, picksAnswer(1, "frm_1a2aaa3d1001ChildSessionFrm", mirror.OpenCodePick{Key: "q0", Picks: []int{0}}))
	m := answeredOf(t, d, 1)
	// Where it went first: a reply to the wrong session is what this pins.
	r := srv.Replies()
	want := "/api/session/" + opencodetest.FixtureChildSession + "/form/frm_1a2aaa3d1001ChildSessionFrm/reply"
	if len(r) != 1 || r[0].Path != want || !sameFormJSON(r[0].Body, `{"answer":{"q0":"Yes"}}`) {
		t.Fatalf("replies %+v; want one to %s", r, want)
	}
	if m.Error != "" {
		t.Fatalf("refused: %+v", m)
	}
}
