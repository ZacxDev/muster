package focus

import (
	"context"
	"strings"
	"testing"
)

// TestNameableTitleIsTheBar: the title half of the name bar, case by case.
// Each refused title fails exactly one clause, so a bar missing that clause
// lets it through.
func TestNameableTitleIsTheBar(t *testing.T) {
	for _, c := range []struct {
		title string
		want  bool
	}{
		{"Fixing the Stripe webhook retry", true},
		{"Billing migration", true},
		{"Greeting the billing team", true}, // one word outside the vocabularies
		{"C# build fix", true},
		// Words that open a session in one phrase name work in another: not
		// openers ("check", "first", "work" are not in the vocabulary).
		{"Check the first work item", true},
		{"Help with first task", true},
		{"Debugging", false},                  // one word
		{"Greeting the user", false},          // an opener
		{"Continue previous session", false},  // an opener
		{"Awaiting user instructions", false}, // an opener
		{"Hi", false},
		{"", false},
		{"**Fix** the login page", false}, // markdown (FeedableTitle)
		{"- Fix the login page", false},   // a list marker (FeedableTitle)
		{strings.Repeat("word ", 13), false},
	} {
		if got := NameableTitle(c.title); got != c.want {
			t.Errorf("NameableTitle(%q) = %v, want %v", c.title, got, c.want)
		}
	}
}

// TestRealTurnNeedsAnAnsweredRealPrompt: a tail shows the session's work only
// once a prompt that is not an opener has been answered.
func TestRealTurnNeedsAnAnsweredRealPrompt(t *testing.T) {
	u := func(s string) Entry { return Entry{Role: "user", Text: s} }
	a := func(s string) Entry { return Entry{Role: "assistant", Text: s} }
	tool := Entry{Role: "tool", Text: "Read /src/app.go"}
	for _, c := range []struct {
		name string
		es   []Entry
		want bool
	}{
		{"answered real prompt", []Entry{u("Fix the webhook retry"), tool, a("On it.")}, true},
		{"greeting answered", []Entry{u("hi"), a("Hello! How can I help?")}, false},
		{"continue answered", []Entry{u("continue"), a("Continuing.")}, false},
		{"real prompt unanswered", []Entry{u("Fix the webhook retry"), tool}, false},
		{"reply before the prompt", []Entry{a("Hello."), u("Fix the webhook retry")}, false},
		{"greeting then real", []Entry{u("hey"), a("Hi."), u("Migrate billing to v2"), a("Reading the schema.")}, true},
		{"empty", nil, false},
	} {
		if got := RealTurn(c.es); got != c.want {
			t.Errorf("%s: RealTurn = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestNameableNeedsBothHalves: a good title over a greeting-only tail, and a
// bad title over a real turn, are both refused.
func TestNameableNeedsBothHalves(t *testing.T) {
	real := []Entry{{Role: "user", Text: "Fix the webhook retry"}, {Role: "assistant", Text: "Looking."}}
	hi := []Entry{{Role: "user", Text: "hi"}, {Role: "assistant", Text: "Hello."}}
	if !Nameable("Fixing the webhook retry", real) {
		t.Error("good title, real turn: want nameable")
	}
	if Nameable("Fixing the webhook retry", hi) {
		t.Error("good title, greeting-only tail: want not nameable")
	}
	if Nameable("Greeting the user", real) {
		t.Error("opener title, real turn: want not nameable")
	}
}

// TestTheRefreshFreezesTheFirstNameableTitleOnly: through the refresher — the
// production writer — a greeting focus freezes nothing, the first nameable
// one freezes, and a later different title leaves the name where it was.
func TestTheRefreshFreezesTheFirstNameableTitleOnly(t *testing.T) {
	ctx := context.Background()
	sub := AgentSubject(7)
	store, sum := newMem(), &fakeSummarizer{}
	src := &fakeSource{tails: map[Subject][]Entry{sub: {{Role: "user", Text: "hi"}, {Role: "assistant", Text: "Hello."}}}}
	r := &Refresher{Store: store, Sessions: src, Summarizer: sum}
	step := func(title string, tail []Entry) Focus {
		t.Helper()
		src.tails[sub] = tail
		sum.sum = Summary{Title: title, Description: "Working."}
		if _, err := r.Refresh(ctx, 1, sub); err != nil {
			t.Fatalf("refresh %q: %v", title, err)
		}
		return store.rows[sub]
	}
	if f := step("Greeting the user", src.tails[sub]); f.Name.Auto != "" {
		t.Fatalf("a greeting froze the name %q", f.Name.Auto)
	}
	real := []Entry{{Role: "user", Text: "hi"}, {Role: "assistant", Text: "Hello."}, {Role: "user", Text: "Migrate billing to v2"},
		{Role: "assistant", Text: "Reading the schema."}}
	if f := step("Billing migration", real); f.Name.Auto != "Billing migration" {
		t.Fatalf("first nameable title: name %q, want frozen to it", f.Name.Auto)
	}
	real = append(real, Entry{Role: "user", Text: "now the webhooks"}, Entry{Role: "assistant", Text: "Ok."})
	f := step("Fixing the Stripe webhook retry", real)
	if f.Name.Auto != "Billing migration" || f.Title != "Fixing the Stripe webhook retry" {
		t.Fatalf("later title: name %q title %q; want the name kept and the focus moved", f.Name.Auto, f.Title)
	}
}

// TestCleanNameBounds: a typed name is one line, 1 to MaxNameRunes runes.
func TestCleanNameBounds(t *testing.T) {
	if n, err := CleanName("  Billing\nmigration  "); err != nil || n != "Billing migration" {
		t.Errorf("CleanName folded: %q %v", n, err)
	}
	for _, bad := range []string{"", "   ", strings.Repeat("x", MaxNameRunes+1)} {
		if _, err := CleanName(bad); err != ErrBadName {
			t.Errorf("CleanName(%.10q…) err %v, want ErrBadName", bad, err)
		}
	}
	if _, err := CleanName(strings.Repeat("é", MaxNameRunes)); err != nil {
		t.Errorf("a name of exactly MaxNameRunes runes: %v", err)
	}
}
