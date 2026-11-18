package opencodehost

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/opencodehost/opencodetest"
)

// frameEvent is fixture frame typ #n as the client parses it.
func frameEvent(t *testing.T, typ string, n int) Event {
	t.Helper()
	var e Event
	if err := json.Unmarshal([]byte(opencodetest.FixtureFrame(typ, n)), &e); err != nil || e.Type != typ {
		t.Fatalf("fixture frame %s #%d: %v", typ, n, err)
	}
	return e
}

// TestTheRealFramesParseIntoAsks: every ask frame a real `opencode serve`
// sent parses into what it raised or settled — and a form's session, which
// is NESTED under data.form, is one Event.SessionID cannot see (why the
// source reads it through AskEvent).
func TestTheRealFramesParseIntoAsks(t *testing.T) {
	q := frameEvent(t, "form.created", 0)
	if q.SessionID() != "" {
		t.Fatalf("instrument check: SessionID read a form's nested session %q", q.SessionID())
	}
	raised, settled, session, ok := q.AskEvent()
	if !ok || raised == nil || settled != "" || session != opencodetest.FixtureFormSession || raised.Kind != AskQuestion ||
		raised.ID != opencodetest.FixtureQuestionsForm || len(raised.Fields) != 2 {
		t.Fatalf("the questions form: %+v %q %q %v", raised, settled, session, ok)
	}
	f := raised.Fields[0]
	if f.Key != "q0" || f.Type != "string" || f.Title != "Database" || f.Description != "Which database should the fixture use?" ||
		len(f.Options) != 2 || f.Options[0].Label != "Postgres (Recommended)" || f.Options[1].Value != "SQLite" || !f.Custom {
		t.Fatalf("its first question: %+v", f)
	}
	if raised.Fields[1].Type != "multiselect" {
		t.Fatalf("its second question: %+v", raised.Fields[1])
	}
	raised, _, session, ok = frameEvent(t, "permission.asked", 0).AskEvent()
	if !ok || raised == nil || raised.Kind != AskPermission || raised.ID != opencodetest.FixtureBashRequest ||
		session != opencodetest.FixturePermissionSession || raised.Action != "bash" || strings.Join(raised.Resources, ",") != "rm -rf build" {
		t.Fatalf("the permission request: %+v", raised)
	}
	for _, c := range []struct {
		typ  string
		n    int
		want string
	}{
		{"form.replied", 0, opencodetest.FixtureQuestionsForm},
		{"form.cancelled", 0, opencodetest.FixtureProceedForm},
		{"permission.replied", 0, opencodetest.FixtureBashRequest},
	} {
		if raised, settled, _, ok := frameEvent(t, c.typ, c.n).AskEvent(); !ok || raised != nil || settled != c.want {
			t.Errorf("%s settles %q (%v %v), want %q", c.typ, settled, raised, ok, c.want)
		}
	}
	if child, parent := frameEvent(t, "session.created", 2).ParentOf(); child != opencodetest.FixtureChildSession || parent != opencodetest.FixturePermissionSession {
		t.Errorf("the child session: %q under %q", child, parent)
	}
	if _, parent := frameEvent(t, "session.created", 0).ParentOf(); parent != "" {
		t.Errorf("a root session has parent %q", parent)
	}
	if _, _, _, ok := frameEvent(t, "session.created", 0).AskEvent(); ok {
		t.Error("a session event was read as an ask")
	}
}

// TestRepliesSendWhatTheRealServerTook: a permission's decision and a form's
// answer go as the bodies the real server answered 204 to; a second reply is
// ErrAskSettled (404 / 409 there); "always" is refused BEFORE a byte is sent;
// and the pending lists read what the stream raised.
func TestRepliesSendWhatTheRealServerTook(t *testing.T) {
	srv := opencodetest.New(fixturePassword)
	defer srv.Close()
	srv.EmitFrame(opencodetest.FixtureFrame("permission.asked", 0))
	srv.EmitFrame(opencodetest.FixtureFrame("form.created", 1))
	tok := srv.Issue()
	cl, err := NewClient(srv.URL, func() string { return tok })
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	asks, err := cl.PendingAsks(ctx, opencodetest.FixturePermissionSession)
	if err != nil || len(asks) != 1 || asks[0].ID != opencodetest.FixtureBashRequest {
		t.Fatalf("pending asks: %+v %v", asks, err)
	}
	if err := cl.ReplyPermission(ctx, opencodetest.FixturePermissionSession, opencodetest.FixtureBashRequest, PermissionAlways); !errors.Is(err, ErrAnswerRefused) {
		t.Fatalf("always: %v", err)
	}
	if n := len(srv.Replies()); n != 0 {
		t.Fatalf("always reached the server (%d replies)", n)
	}
	if err := cl.ReplyPermission(ctx, opencodetest.FixturePermissionSession, opencodetest.FixtureBashRequest, PermissionReject); err != nil {
		t.Fatal(err)
	}
	if err := cl.ReplyPermission(ctx, opencodetest.FixturePermissionSession, opencodetest.FixtureBashRequest, PermissionOnce); !errors.Is(err, ErrAskSettled) {
		t.Fatalf("a second reply: %v", err)
	}
	if err := cl.ReplyForm(ctx, opencodetest.FixtureFormSession, opencodetest.FixtureProceedForm, map[string]any{"q0": "Yes"}); err != nil {
		t.Fatal(err)
	}
	if err := cl.ReplyForm(ctx, opencodetest.FixtureFormSession, opencodetest.FixtureProceedForm, map[string]any{"q0": "No"}); !errors.Is(err, ErrAskSettled) {
		t.Fatalf("a second answer: %v", err)
	}
	r := srv.Replies()
	want := []opencodetest.Reply{
		{Path: "/api/session/" + opencodetest.FixturePermissionSession + "/permission/" + opencodetest.FixtureBashRequest + "/reply", Body: `{"decision":"reject"}`},
		{Path: "/api/session/" + opencodetest.FixturePermissionSession + "/permission/" + opencodetest.FixtureBashRequest + "/reply", Body: `{"decision":"once"}`},
		{Path: "/api/session/" + opencodetest.FixtureFormSession + "/form/" + opencodetest.FixtureProceedForm + "/reply", Body: `{"answer":{"q0":"Yes"}}`},
		{Path: "/api/session/" + opencodetest.FixtureFormSession + "/form/" + opencodetest.FixtureProceedForm + "/reply", Body: `{"answer":{"q0":"No"}}`},
	}
	if len(r) != len(want) {
		t.Fatalf("replies %+v", r)
	}
	for i := range want {
		if r[i] != want[i] {
			t.Errorf("reply %d: %+v, want %+v", i, r[i], want[i])
		}
	}
	// A 400 is the answer refused; a 401 the token.
	srv.EmitFrame(opencodetest.FixtureFrame("form.created", 0))
	srv.RefuseReplies(http.StatusBadRequest)
	if err := cl.ReplyForm(ctx, opencodetest.FixtureFormSession, opencodetest.FixtureQuestionsForm, map[string]any{"q0": "x"}); !errors.Is(err, ErrAnswerRefused) {
		t.Fatalf("a refused answer: %v", err)
	}
	srv.RefuseReplies(http.StatusUnauthorized)
	if err := cl.ReplyForm(ctx, opencodetest.FixtureFormSession, opencodetest.FixtureQuestionsForm, map[string]any{"q0": "x"}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("a refused token: %v", err)
	}
	// Ids that would leave the path are refused before a byte is sent.
	n := len(srv.Replies())
	for _, id := range []string{"per_../../api/pair", "frm_x/../y", "ses_a", ""} {
		if err := cl.ReplyPermission(ctx, opencodetest.FixturePermissionSession, id, PermissionOnce); err == nil {
			t.Errorf("request id %q was sent", id)
		}
		if err := cl.ReplyForm(ctx, opencodetest.FixtureFormSession, id, map[string]any{"q0": "x"}); err == nil {
			t.Errorf("form id %q was sent", id)
		}
	}
	if len(srv.Replies()) != n {
		t.Fatal("a reply with a bad id reached the server")
	}
}
