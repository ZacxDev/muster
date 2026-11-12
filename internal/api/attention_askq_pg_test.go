package api

import (
	"html"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// TestAHeldQuestionIsOneItemLinkedToItsSessionsView: a session whose
// AskUserQuestion is held (internal/askq, the muster-mirror ask hook) is ONE
// attention item — the question, with its text, linked to the rendered view
// where its card is — never a "session waiting" item beside it, whether the
// window also reports waiting (the hub's overlay) or not. Once answered, a
// window still waiting is a plain session item again; an idle window with
// nothing held is none.
func TestAHeldQuestionIsOneItemLinkedToItsSessionsView(t *testing.T) {
	f := newAttnFixture(t)
	ctx := asUser(1)
	view := "/mirror/view?host=ws&session=main&window=%401"
	key := sessionKey("ws", "main", "@1")
	items := func() []attentionItem {
		all, err := f.srv.attentionItems(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var out []attentionItem
		for _, it := range all {
			if it.Kind == attentionKindSession || it.Kind == attentionKindQuestion {
				out = append(out, it)
			}
		}
		return out
	}
	qs := []askq.Question{{Question: "Which database?", Header: "DB", Options: []askq.Option{{Label: "postgres"}, {Label: "sqlite"}}},
		{Question: "Migrate now?", Options: []askq.Option{{Label: "yes"}, {Label: "no"}}}}
	e, err := f.srv.Questions().Open(askq.Key{Owner: 1, Source: askSourceMirror, Scope: "ws", Session: focusSID, ToolUseID: "toolu_attn1"},
		qs, askq.NewID())
	if err != nil {
		t.Fatal(err)
	}
	// The window reports waiting (Claude Code does, with its picker up) AND a question is open: one item.
	got := items()
	if len(got) != 1 || got[0].Key != key || got[0].Kind != attentionKindQuestion || got[0].Text != "1. Which database?\n2. Migrate now?" || got[0].Link != view {
		t.Fatalf("held + waiting: %+v", got)
	}
	// The list draws it ONCE, and its card is one link to the session's view
	// (where the question card is answered).
	body := f.page(t, "/attention/items")
	if strings.Count(body, `id="`+key+`"`) != 1 {
		t.Fatalf("the view: %s", body)
	}
	li := body[strings.Index(body, `<li id="`+key+`"`):]
	li = li[:strings.Index(li, "</li>")]
	if !strings.Contains(li, `<a href="`+html.EscapeString(view)+`" class="`+ui.AsksRowClass+`" data-attention-open>`) || strings.Count(li, "<a ") != 1 {
		t.Fatalf("the held question's card is not one link to its session's view: %s", li)
	}
	// Held while the host still says idle: the question is still listed, once.
	f.setWindows(mirror.StatusIdle)
	if got := items(); len(got) != 1 || got[0].Kind != attentionKindQuestion {
		t.Fatalf("held + idle: %+v", got)
	}
	// Answered: no question; the idle window is no item at all.
	if _, ok := f.srv.Questions().Resolve(e.Key.Owner, e.Key.Source, e.Key.Scope, e.Key.Session, e.Key.ToolUseID,
		map[string]string{"Which database?": "postgres", "Migrate now?": "yes"}); !ok {
		t.Fatal("not resolved")
	}
	if got := items(); len(got) != 0 {
		t.Fatalf("answered + idle: %+v", got)
	}
	// …and a window still waiting for something else is a session item again.
	f.setWindows(mirror.StatusWaiting)
	if got := items(); len(got) != 1 || got[0].Kind != attentionKindSession {
		t.Fatalf("answered + waiting: %+v", got)
	}
	// Another owner's held question is not the operator's item.
	if _, err := f.srv.Questions().Open(askq.Key{Owner: f.member, Source: askSourceMirror, Scope: "ws", Session: focusSID, ToolUseID: "toolu_attn2"},
		qs, askq.NewID()); err != nil {
		t.Fatal(err)
	}
	if got := items(); len(got) != 1 || got[0].Kind != attentionKindSession {
		t.Fatalf("another owner's question: %+v", got)
	}
}

// attnItemLI is the list's ONE <li> whose data-kind is kind (fails when the
// list has none, or more than one).
func attnItemLI(t *testing.T, body, kind string) string {
	t.Helper()
	marker := `data-kind="` + kind + `"`
	if n := strings.Count(body, marker); n != 1 {
		t.Fatalf("%d %s items, want 1: %s", n, kind, body)
	}
	i := strings.LastIndex(body[:strings.Index(body, marker)], "<li ")
	li := body[i:]
	return li[:strings.Index(li, "</li>")]
}

// TestAHeldQuestionsCardSaysAnswerInSession: a held question's card carries
// the muted cue "Answer in session" exactly once, INSIDE its one row link (the
// card stays one link); an agent's plain Question card beside it and a
// session-waiting row do not carry it.
func TestAHeldQuestionsCardSaysAnswerInSession(t *testing.T) {
	const cue = "Answer in session"
	f := newAttnFixture(t)
	f.ask(t, f.opTok, "Ship the release tonight?", "ship", "wait")
	e, err := f.srv.Questions().Open(askq.Key{Owner: 1, Source: askSourceMirror, Scope: "ws", Session: focusSID, ToolUseID: "toolu_cue1"},
		[]askq.Question{{Question: "Which region?", Options: []askq.Option{{Label: "eu-west"}, {Label: "us-east"}}}}, askq.NewID())
	if err != nil {
		t.Fatal(err)
	}
	body := f.page(t, "/attention/items")

	held := attnItemLI(t, body, attentionKindQuestion)
	link := held[strings.Index(held, "<a "):]
	link = link[:strings.Index(link, "</a>")+len("</a>")]
	if strings.Count(held, "<a ") != 1 || strings.Count(link, cue) != 1 || strings.Count(held, cue) != 1 {
		t.Fatalf("the held question's card does not say %q once inside its one link: %s", cue, held)
	}
	if !strings.Contains(link, `<span class="`+ui.AsksMutedTextClass+`" data-attention-held-cue>`+cue+`</span>`) {
		t.Fatalf("the cue is not one muted line: %s", link)
	}

	if plain := attnItemLI(t, body, attentionKindAsk); strings.Contains(plain, cue) {
		t.Fatalf("a plain Question card says %q: %s", cue, plain)
	}

	// Answered, the window still waiting: a Session waiting row, no cue.
	if _, ok := f.srv.Questions().Resolve(e.Key.Owner, e.Key.Source, e.Key.Scope, e.Key.Session, e.Key.ToolUseID,
		map[string]string{"Which region?": "eu-west"}); !ok {
		t.Fatal("not resolved")
	}
	body = f.page(t, "/attention/items")
	if waiting := attnItemLI(t, body, attentionKindSession); strings.Contains(waiting, cue) {
		t.Fatalf("a Session waiting row says %q: %s", cue, waiting)
	}
	if strings.Contains(body, cue) {
		t.Fatalf("%q with no held question: %s", cue, body)
	}
}
