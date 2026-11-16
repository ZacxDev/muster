package focus

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// TestASessionsTaskLinkReplacesNoneAndClears: against Postgres (0137's
// columns). Each write REPLACES the declaration: keys (de-duplicated, order
// kept) with a note; other keys drop the old ones and the note; a none line
// replaces the keys; an empty write clears it — and every write stamps
// set_at. Nothing touches the focus or the name.
func TestASessionsTaskLinkReplacesNoneAndClears(t *testing.T) {
	s, _ := pgStore(t)
	sub := MirrorSubject("0b2c3d4e-5f60-4718-8a9b-00000000bb01")
	set := func(l TaskLink) TaskLink {
		t.Helper()
		got, err := s.SetTaskLink(as(2), sub, l)
		if err != nil {
			t.Fatalf("set %+v: %v", l, err)
		}
		if got.SetAt.IsZero() {
			t.Fatalf("set %+v: set_at not stamped", l)
		}
		return got
	}
	got := set(TaskLink{Keys: []string{"ex:b2", "ex:a1", "ex:b2"}, Note: "  the \nbilling fix "})
	if !reflect.DeepEqual(got.Keys, []string{"ex:b2", "ex:a1"}) || got.Note != "the billing fix" || got.None != "" {
		t.Fatalf("replace: %+v; want [ex:b2 ex:a1] with the note cleaned", got)
	}
	if f, ok, _ := s.Get(as(2), sub); !ok || f.Has() || f.Name != (Name{}) {
		t.Fatalf("a task link alone: row %v, focus %v, name %+v; want a title-less, nameless row", ok, f.Has(), f.Name)
	}
	got = set(TaskLink{Keys: []string{"ex:c3"}})
	if !reflect.DeepEqual(got.Keys, []string{"ex:c3"}) || got.Note != "" {
		t.Fatalf("a second replace: %+v; want only [ex:c3] and no note", got)
	}
	got = set(TaskLink{None: "a scratch experiment"})
	if got.Keys != nil || got.None != "a scratch experiment" {
		t.Fatalf("none: %+v; want no keys, the none line", got)
	}
	first := got.SetAt
	got = set(TaskLink{})
	if got.Declared() || got.Note != "" || got.SetAt.Before(first) {
		t.Fatalf("clear: %+v; want nothing declared, set_at kept fresh", got)
	}
	if f, _, _ := s.Get(as(2), sub); f.Tasks.Declared() || f.Tasks.SetAt.IsZero() {
		t.Fatalf("Get after the clear: %+v", f.Tasks)
	}
}

// TestATaskLinkIsRefusedOutOfShape: the store's own refusals — an agent
// subject, keys beside none, a note with no keys, more than MaxTaskKeys, and
// an over-long note.
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
		want error
	}{
		{"agent subject", AgentSubject(a.ID), TaskLink{Keys: []string{"ex:1"}}, ErrTaskLinkAgent},
		{"keys and none", sub, TaskLink{Keys: []string{"ex:1"}, None: "why"}, ErrTaskLinkNoneKeys},
		{"note alone", sub, TaskLink{Note: "why"}, ErrTaskLinkNote},
		{"eleven keys", sub, TaskLink{Keys: eleven}, ErrTaskLinkTooMany},
		{"long note", sub, TaskLink{Keys: []string{"ex:1"}, Note: long}, ErrTaskLinkText},
	} {
		if _, err := s.SetTaskLink(as(2), c.sub, c.l); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	// A positive control on the same subject: ten keys fit.
	if got, err := s.SetTaskLink(as(2), sub, TaskLink{Keys: eleven[:10]}); err != nil || len(got.Keys) != 10 {
		t.Fatalf("ten keys: %d, %v", len(got.Keys), err)
	}
}

// TestASessionCannotWriteAnotherOwnersSessionsTaskLink: row-level security
// makes the link the PRINCIPAL's. Owner 3 writing the same session key as
// owner 2 writes its OWN row: owner 2's declaration is unchanged, and neither
// reads the other's.
func TestASessionCannotWriteAnotherOwnersSessionsTaskLink(t *testing.T) {
	s, _ := pgStore(t)
	sub := MirrorSubject("0b2c3d4e-5f60-4718-8a9b-00000000bb03")
	if _, err := s.SetTaskLink(as(2), sub, TaskLink{Keys: []string{"ex:owner2"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetTaskLink(as(3), sub, TaskLink{Keys: []string{"ex:owner3"}}); err != nil {
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
