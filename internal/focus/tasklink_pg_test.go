package focus

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// TestASessionsTaskLinkReplaceAddClearAndNone: against Postgres (0136's
// columns and setTaskLinkSQL). replace sets the keys (de-duplicated, order
// kept) with a note; add appends only new keys and keeps the note; a none
// declaration replaces the keys; a further add drops it; an empty replace
// clears it — and every write stamps set_at/set_by. Nothing touches the
// focus or the name.
func TestASessionsTaskLinkReplaceAddClearAndNone(t *testing.T) {
	s, _ := pgStore(t)
	sub := MirrorSubject("0b2c3d4e-5f60-4718-8a9b-00000000bb01")
	by := sub.String()
	set := func(l TaskLink, mode TaskLinkMode) TaskLink {
		t.Helper()
		l.SetBy = by
		got, err := s.SetTaskLink(as(2), sub, l, mode)
		if err != nil {
			t.Fatalf("set %+v (%s): %v", l, mode, err)
		}
		if got.SetAt.IsZero() || got.SetBy != by {
			t.Fatalf("set %+v (%s): set_at %v set_by %q; want both stamped", l, mode, got.SetAt, got.SetBy)
		}
		return got
	}
	got := set(TaskLink{Keys: []string{"ex:b2", "ex:a1", "ex:b2"}, Note: "  the \nbilling fix "}, TaskLinkReplace)
	if !reflect.DeepEqual(got.Keys, []string{"ex:b2", "ex:a1"}) || got.Note != "the billing fix" || got.None != "" {
		t.Fatalf("replace: %+v; want [ex:b2 ex:a1] with the note cleaned", got)
	}
	if f, ok, _ := s.Get(as(2), sub); !ok || f.Has() || f.Name != (Name{}) {
		t.Fatalf("a task link alone: row %v, focus %v, name %+v; want a title-less, nameless row", ok, f.Has(), f.Name)
	}
	got = set(TaskLink{Keys: []string{"ex:a1", "ex:c3"}}, TaskLinkAdd)
	if !reflect.DeepEqual(got.Keys, []string{"ex:b2", "ex:a1", "ex:c3"}) || got.Note != "the billing fix" {
		t.Fatalf("add: %+v; want [ex:b2 ex:a1 ex:c3] and the note kept", got)
	}
	got = set(TaskLink{None: "a scratch experiment"}, TaskLinkReplace)
	if got.Keys != nil || got.None != "a scratch experiment" || got.Note != "" {
		t.Fatalf("none: %+v; want no keys, the none line, no note", got)
	}
	got = set(TaskLink{Keys: []string{"ex:d4"}}, TaskLinkAdd)
	if !reflect.DeepEqual(got.Keys, []string{"ex:d4"}) || got.None != "" {
		t.Fatalf("add after none: %+v; want [ex:d4] and the none line dropped", got)
	}
	first := got.SetAt
	got = set(TaskLink{}, TaskLinkReplace)
	if got.Declared() || got.Note != "" || got.SetAt.Before(first) {
		t.Fatalf("clear: %+v; want nothing declared, set_at kept fresh", got)
	}
	if f, _, _ := s.Get(as(2), sub); f.Tasks.Declared() || f.Tasks.SetBy != by {
		t.Fatalf("Get after the clear: %+v", f.Tasks)
	}
}

// TestATaskLinkIsRefusedOutOfShape: the store's own refusals — an agent
// subject, keys beside none, none on an add, more than MaxTaskKeys (also when
// an add would merge past it: the column's CHECK), an over-long note, and no
// set_by.
func TestATaskLinkIsRefusedOutOfShape(t *testing.T) {
	s, pool := pgStore(t)
	sub := MirrorSubject("0b2c3d4e-5f60-4718-8a9b-00000000bb02")
	a := mustAgent(t, pool, 2, "focus-task-link")
	eleven := []string{"ex:1", "ex:2", "ex:3", "ex:4", "ex:5", "ex:6", "ex:7", "ex:8", "ex:9", "ex:10", "ex:11"}
	long := strings.Repeat("x", MaxTaskTextRunes+1)
	for _, c := range []struct {
		name string
		sub  Subject
		l    TaskLink
		mode TaskLinkMode
		want error
	}{
		{"agent subject", AgentSubject(a.ID), TaskLink{Keys: []string{"ex:1"}, SetBy: "x"}, TaskLinkReplace, ErrTaskLinkAgent},
		{"keys and none", sub, TaskLink{Keys: []string{"ex:1"}, None: "why", SetBy: "x"}, TaskLinkReplace, ErrTaskLinkNoneKeys},
		{"none on add", sub, TaskLink{None: "why", SetBy: "x"}, TaskLinkAdd, ErrTaskLinkNoneKeys},
		{"eleven keys", sub, TaskLink{Keys: eleven, SetBy: "x"}, TaskLinkReplace, ErrTaskLinkTooMany},
		{"long note", sub, TaskLink{Keys: []string{"ex:1"}, Note: long, SetBy: "x"}, TaskLinkReplace, ErrTaskLinkText},
		{"no set_by", sub, TaskLink{Keys: []string{"ex:1"}}, TaskLinkReplace, ErrTaskLinkBy},
	} {
		if _, err := s.SetTaskLink(as(2), c.sub, c.l, c.mode); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	// The cap after a merge is the column's: ten stored, one more added.
	if _, err := s.SetTaskLink(as(2), sub, TaskLink{Keys: eleven[:10], SetBy: "x"}, TaskLinkReplace); err != nil {
		t.Fatalf("ten keys: %v", err)
	}
	if _, err := s.SetTaskLink(as(2), sub, TaskLink{Keys: eleven[10:], SetBy: "x"}, TaskLinkAdd); !errors.Is(err, ErrTaskLinkTooMany) {
		t.Fatalf("an add past the cap: %v, want ErrTaskLinkTooMany", err)
	}
	// A positive control on the same subject: an add of a key already stored
	// stays inside the cap.
	if got, err := s.SetTaskLink(as(2), sub, TaskLink{Keys: eleven[:1], SetBy: "x"}, TaskLinkAdd); err != nil || len(got.Keys) != 10 {
		t.Fatalf("an add of a stored key: %d keys, %v; want the ten", len(got.Keys), err)
	}
}

// TestASessionCannotWriteAnotherOwnersSessionsTaskLink: row-level security
// makes the link the PRINCIPAL's. Owner 3 writing the same session key as
// owner 2 writes its OWN row: owner 2's declaration is unchanged, and neither
// reads the other's.
func TestASessionCannotWriteAnotherOwnersSessionsTaskLink(t *testing.T) {
	s, _ := pgStore(t)
	sub := MirrorSubject("0b2c3d4e-5f60-4718-8a9b-00000000bb03")
	if _, err := s.SetTaskLink(as(2), sub, TaskLink{Keys: []string{"ex:owner2"}, SetBy: sub.String()}, TaskLinkReplace); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetTaskLink(as(3), sub, TaskLink{Keys: []string{"ex:owner3"}, SetBy: sub.String()}, TaskLinkReplace); err != nil {
		t.Fatal(err)
	}
	for owner, want := range map[int64]string{2: "ex:owner2", 3: "ex:owner3"} {
		f, ok, err := s.Get(as(owner), sub)
		if err != nil || !ok || !reflect.DeepEqual(f.Tasks.Keys, []string{want}) || f.Owner != owner {
			t.Errorf("owner %d reads %+v (owner %d, %v %v); want only its own [%s]", owner, f.Tasks, f.Owner, ok, err, want)
		}
	}
	list, err := s.List(as(3))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range list {
		if f.Owner != 3 {
			t.Errorf("owner 3's List carries owner %d's row %s", f.Owner, f.Subject)
		}
	}
}
