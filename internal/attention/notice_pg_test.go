package attention

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
)

// NOTICES (migration 0071, KindNotice): a pointer muster raises itself, with no
// options and no answer, that does not count toward MaxOpenPerOwner.

var noticeAsker = Asker{System: true, Label: "ws · main:1"}

func mustNotice(t *testing.T, s *PGStore, uid int64, text string) Ask {
	t.Helper()
	a := mustAsk(t, s, uid, New{Asker: noticeAsker, Question: text, Notice: true})
	if a.Kind != KindNotice || len(a.Options) != 0 || a.Status != Open {
		t.Fatalf("notice %+v", a)
	}
	return a
}

// TestNoticesNeitherCountTowardNorAreRefusedAtTheAskCap: with MaxOpenPerOwner
// notices open, a question is still accepted (notices are not counted); with
// MaxOpenPerOwner questions open, a notice is still accepted (it is not
// refused at the cap) while one more question is refused — the cap still holds
// for questions (the control).
func TestNoticesNeitherCountTowardNorAreRefusedAtTheAskCap(t *testing.T) {
	s, _, _ := pgStore(t)
	for range MaxOpenPerOwner {
		mustNotice(t, s, 2, "Waiting on you: a notice")
	}
	q := mustAsk(t, s, 2, New{Asker: session, Question: "a real question, past 50 notices"})
	if q.Kind != KindAsk {
		t.Fatalf("question %+v", q)
	}
	for range MaxOpenPerOwner - 1 {
		mustAsk(t, s, 2, New{Asker: session, Question: "filling the cap"})
	}
	if _, err := s.Create(as(2), New{Asker: session, Question: "one too many"}); !errors.Is(err, ErrTooMany) {
		t.Fatalf("question past the cap: %v, want ErrTooMany (the control)", err)
	}
	mustNotice(t, s, 2, "Waiting on you: a notice at a full cap")
}

// TestANoticeHasNoAnswer: an answer to an open notice — from the view or
// through a mirrored session — is refused as ErrNotice and the notice is left
// exactly as it was; it can still be dismissed.
func TestANoticeHasNoAnswer(t *testing.T) {
	s, _, _ := pgStore(t)
	n := mustNotice(t, s, 2, "Waiting on you: merge or wait?")
	if got, err := s.Answer(as(2), n.ID, "merge"); !errors.Is(err, ErrNotice) || got.Status != Open || got.Answer != "" {
		t.Fatalf("view answer: %v %+v, want ErrNotice and the notice unchanged", err, got)
	}
	if got, err := s.AnswerFromSession(as(2), n.ID, "merge", "otherhost", session.MirrorSession); !errors.Is(err, ErrNotice) || got.Status != Open {
		t.Fatalf("session answer: %v %+v, want ErrNotice", err, got)
	}
	if got, ok, err := s.SetSuggestions(as(2), n.ID, []string{"merge it"}); err != nil || ok || len(got.Suggestions) != 0 {
		t.Fatalf("suggestions on a notice: ok %v %v %+v", ok, err, got)
	}
	if got, err := s.Cancel(as(2), n.ID); err != nil || got.Status != Cancelled {
		t.Fatalf("dismiss: %v %+v", err, got)
	}
}

// TestTheSchemaRefusesAnAnswerOnANotice: 0071's CHECK, whatever writes it — an
// UPDATE that answers a notice row outright is refused by the database, so no
// future code path can put an answer (and so a relay or a delivery) on one.
// The CONTROL: the same UPDATE on a question moves it.
func TestTheSchemaRefusesAnAnswerOnANotice(t *testing.T) {
	s, pool, _ := pgStore(t)
	n := mustNotice(t, s, 2, "Waiting on you: the schema guard")
	sys := authz.System(context.Background())
	_, err := pool.Exec(sys, `UPDATE attention_asks SET status = 'answered', answer = 'yes', answered_by = 'operator',
		resolved_at = now() WHERE id = $1`, n.ID)
	if err == nil || !strings.Contains(err.Error(), "attention_asks_kind_check") {
		t.Fatalf("answering a notice row: %v, want the kind check to refuse it", err)
	}
	if _, err := pool.Exec(sys, `UPDATE attention_asks SET options = '["a","b"]'::jsonb WHERE id = $1`, n.ID); err == nil {
		t.Fatal("options on a notice row were accepted")
	}
	q := mustAsk(t, s, 2, New{Asker: session, Question: "the control"})
	if tag, err := pool.Exec(sys, `UPDATE attention_asks SET status = 'answered', answer = 'yes', answered_by = 'operator',
		resolved_at = now() WHERE id = $1`, q.ID); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("the control (a question) was not answered: %v", err)
	}
}

// TestANoticeIsMustersOwnAndCarriesNoOptions: Clean refuses a notice with
// options, from a session asker, or tied to a task.
func TestANoticeIsMustersOwnAndCarriesNoOptions(t *testing.T) {
	for _, c := range []struct {
		name string
		n    New
	}{
		{"options", New{Asker: noticeAsker, Question: "q", Notice: true, Options: []string{"a", "b"}}},
		{"a session's", New{Asker: session, Question: "q", Notice: true}},
		{"a task's", New{Asker: noticeAsker, Question: "q", Notice: true, TaskID: 4}},
		{"to the chief", New{Asker: noticeAsker, Question: "q", Notice: true, ToChief: true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Clean(c.n); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Clean: %v, want ErrInvalid", err)
			}
		})
	}
}
