package attention

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// WHAT AN ASK CARRIES BESIDE ITS QUESTION (ask_media.go, migration 0143): the
// shape rules Clean holds, and the round trip through Postgres.

const (
	idA = "AAAAAAAAAAAAAAAAAAAAAA"
	idB = "BBBBBBBBBBBBBBBBBBBBBB"
)

func TestTheContextIsOneSanitisedLineAndRefusedPastItsBound(t *testing.T) {
	w, err := CleanWhy(&AskWhy{Text: "  line one\nline\ttwo ‮\u0007 end  "})
	if err != nil {
		t.Fatal(err)
	}
	// A line break and a tab are spaces; a bidi override and a control are
	// U+FFFD (fence.Sanitize); runs of spaces fold; the edges are trimmed.
	if want := "line one line two �� end"; w.Text != want {
		t.Fatalf("Text = %q, want %q", w.Text, want)
	}
	if _, err := CleanWhy(&AskWhy{Text: strings.Repeat("x", MaxWhyRunes)}); err != nil {
		t.Fatalf("a context of exactly %d characters must be kept: %v", MaxWhyRunes, err)
	}
	_, err = CleanWhy(&AskWhy{Text: strings.Repeat("é", MaxWhyRunes+1)})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "the context is longer than 500 characters") {
		t.Fatalf("a context one character past the bound: %v, want ErrInvalid naming the bound", err)
	}
}

func TestTheAgentsContextReplacesTheFallbackAndTheFallbackIsCutNotRefused(t *testing.T) {
	w, err := CleanWhy(&AskWhy{Text: "mine", Focus: "focus", Last: "last"})
	if err != nil || !reflect.DeepEqual(w, &AskWhy{Text: "mine"}) {
		t.Fatalf("with a context: %+v %v, want the context alone", w, err)
	}
	w, err = CleanWhy(&AskWhy{Focus: strings.Repeat("f", MaxWhyFocusRunes+50), Last: "a\nb" + strings.Repeat("l", MaxWhyLastRunes+50)})
	if err != nil {
		t.Fatal(err)
	}
	if w.Text != "" || len([]rune(w.Focus)) != MaxWhyFocusRunes+1 || !strings.HasSuffix(w.Focus, "…") ||
		!strings.HasPrefix(w.Last, "a b") || len([]rune(w.Last)) != MaxWhyLastRunes+1 {
		t.Fatalf("the fallback is not cut to its bounds: %+v", w)
	}
	if w, err := CleanWhy(&AskWhy{Text: "  \n ", Focus: " ", Last: "\t"}); err != nil || w != nil {
		t.Fatalf("a blank why: %+v %v, want nil", w, err)
	}
}

func TestAttachmentsAreBoundedInCountAndShape(t *testing.T) {
	ok := Attachment{ID: idA, Name: "shot\n.png", Type: "image/png", Size: 10}
	got, err := CleanAttachments([]Attachment{ok})
	if err != nil || len(got) != 1 || got[0].Name != "shot ⏎ .png" {
		t.Fatalf("one attachment: %+v %v", got, err)
	}
	five := []Attachment{}
	for _, id := range []string{idA, idB, "CCCCCCCCCCCCCCCCCCCCCC", "DDDDDDDDDDDDDDDDDDDDDD", "EEEEEEEEEEEEEEEEEEEEEE"} {
		five = append(five, Attachment{ID: id, Name: "f", Type: "text/plain; charset=utf-8", Size: 1})
	}
	if _, err := CleanAttachments(five[:MaxAttachments]); err != nil {
		t.Fatalf("%d attachments must be kept: %v", MaxAttachments, err)
	}
	for name, in := range map[string][]Attachment{
		"five":         five,
		"bad id":       {{ID: "../etc/passwd", Name: "f", Type: "x", Size: 1}},
		"twice":        {ok, ok},
		"empty":        {{ID: idA, Name: "f", Type: "x", Size: 0}},
		"negative":     {{ID: idA, Name: "f", Type: "x", Size: -1}},
		"short id":     {{ID: "AAAA", Name: "f", Type: "x", Size: 1}},
		"slash in id":  {{ID: "AAAAAAAAAAAAAAAAAAAA/A", Name: "f", Type: "x", Size: 1}},
		"dot dot path": {{ID: "AAAAAAAAAAAAAAAAAAAA..", Name: "f", Type: "x", Size: 1}},
	} {
		if _, err := CleanAttachments(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
}

func TestAnAsksAttachmentsAndWhyRoundTripAndAnAskWithoutThemReadsNil(t *testing.T) {
	s, _, _ := pgStore(t)
	files := []Attachment{{ID: idA, Name: "shot.png", Type: "image/png", Size: 312}, {ID: idB, Name: "log.txt", Type: "text/plain; charset=utf-8", Size: 9}}
	a := mustAsk(t, s, 2, New{Asker: session, Question: "Ship it?", Attachments: files, Why: &AskWhy{Focus: "Release 1.2", Last: "Tests are green."}})
	got, err := s.Get(as(2), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Attachments, files) || !reflect.DeepEqual(got.Why, &AskWhy{Focus: "Release 1.2", Last: "Tests are green."}) {
		t.Fatalf("read back %+v / %+v", got.Attachments, got.Why)
	}
	plain := mustAsk(t, s, 2, New{Asker: session, Question: "Plain?"})
	if got, _ := s.Get(as(2), plain.ID); got.Attachments != nil || got.Why != nil {
		t.Fatalf("an ask with neither read back %+v / %+v", got.Attachments, got.Why)
	}
	// Another owner reads the ask, and so its files, as missing.
	if _, err := s.Get(as(3), a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another owner's read: %v, want ErrNotFound", err)
	}
}
