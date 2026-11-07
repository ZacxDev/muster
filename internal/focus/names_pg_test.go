package focus

import (
	"errors"
	"testing"
	"time"
)

// TestTheAutoNameFreezesOnceInTheStore: against Postgres (the upsert's own
// SQL, migration 0090). A write not marked nameable freezes nothing; the first
// nameable write freezes its title; a later nameable write with another title
// does not move it; and the history is untouched by any of it.
func TestTheAutoNameFreezesOnceInTheStore(t *testing.T) {
	s, pool := pgStore(t)
	a := mustAgent(t, pool, 2, "focus-name")
	sub := AgentSubject(a.ID)
	write := func(title string, nameable bool) Focus {
		t.Helper()
		f, err := s.Upsert(as(2), sub, Write{Title: title, Source: SourceSummarizer, Nameable: nameable})
		if err != nil {
			t.Fatalf("upsert %q: %v", title, err)
		}
		return f
	}
	if f := write("Greeting the user", false); f.Name.Auto != "" || !f.Name.AutoAt.IsZero() {
		t.Fatalf("a non-nameable write froze %q", f.Name.Auto)
	}
	f := write("Billing migration", true)
	if f.Name.Auto != "Billing migration" || f.Name.AutoAt.IsZero() {
		t.Fatalf("first nameable write: name %q at %v", f.Name.Auto, f.Name.AutoAt)
	}
	frozenAt := f.Name.AutoAt
	f = write("Fixing the Stripe webhook retry", true)
	if f.Name.Auto != "Billing migration" || !f.Name.AutoAt.Equal(frozenAt) || f.Title != "Fixing the Stripe webhook retry" {
		t.Fatalf("a later nameable write: name %q at %v (title %q); want the frozen name kept", f.Name.Auto, f.Name.AutoAt, f.Title)
	}
	// The store checks the title half of the bar itself: a caller that marks
	// an opener nameable freezes nothing.
	b := mustAgent(t, pool, 2, "focus-name-b")
	if g, err := s.Upsert(as(2), AgentSubject(b.ID), Write{Title: "Greeting the user", Source: SourceSummarizer, Nameable: true}); err != nil || g.Name.Auto != "" {
		t.Fatalf("an opener marked nameable: name %q (%v); want none", g.Name.Auto, err)
	}
	got, _, err := s.Get(as(2), sub)
	if err != nil || got.Name.Auto != "Billing migration" {
		t.Fatalf("Get: name %q (%v)", got.Name.Auto, err)
	}
	page, err := s.History(as(2), sub, "", 10)
	if err != nil || len(page.Entries) != 3 {
		t.Fatalf("history: %d rows (%v), want the 3 focuses", len(page.Entries), err)
	}
}

// TestAManualNameIsAMirrorsOwnAndNeverMovedByAFocus: a mirrored session's
// manual name is stored without a focus (a title-less row the readers skip),
// survives every later focus write — nameable ones included — and is cleared
// by "". An agent subject refuses one; another owner never sees it.
func TestAManualNameIsAMirrorsOwnAndNeverMovedByAFocus(t *testing.T) {
	s, _ := pgStore(t)
	sub := MirrorSubject("0b2c3d4e-5f60-4718-8a9b-00000000aa01")
	n, err := s.SetManualName(as(2), sub, "  Payments\nwork ")
	if err != nil || n.Manual != "Payments work" || n.ManualAt.IsZero() {
		t.Fatalf("set: %+v %v", n, err)
	}
	if f, ok, _ := s.Get(as(2), sub); !ok || f.Has() {
		t.Fatalf("a manual name alone: row %v has focus %v; want a title-less row", ok, f.Has())
	}
	f, err := s.Upsert(as(2), sub, Write{Title: "Billing migration", Source: SourceSummarizer, Nameable: true})
	if err != nil || f.Name.Manual != "Payments work" || f.Name.Auto != "Billing migration" {
		t.Fatalf("a focus after the manual name: %+v %v; want both halves", f.Name, err)
	}
	if _, ok, _ := s.Get(as(3), sub); ok {
		t.Fatal("another owner reads the row")
	}
	if n, err := s.SetManualName(as(2), sub, ""); err != nil || n.Manual != "" || !n.ManualAt.IsZero() || n.Auto != "Billing migration" {
		t.Fatalf("clear: %+v %v; want the manual half gone, the auto kept", n, err)
	}
	if _, err := s.SetManualName(as(2), AgentSubject(1), "Mine"); !errors.Is(err, ErrAgentManualName) {
		t.Fatalf("an agent subject: %v, want ErrAgentManualName", err)
	}
	if _, err := s.SetManualName(as(2), sub, "   "); !errors.Is(err, ErrBadName) {
		t.Fatalf("a blank (not empty) name: %v, want ErrBadName", err)
	}
}

// TestPruneKeepsANamedMirrorLonger: a mirrored row unseen past the retention
// is pruned — unless it carries a name, manual or auto, which is kept for
// NameRetention more.
func TestPruneKeepsANamedMirrorLonger(t *testing.T) {
	s, pool := pgStore(t)
	plain, named := MirrorSubject("0b2c3d4e-5f60-4718-8a9b-00000000aa02"), MirrorSubject("0b2c3d4e-5f60-4718-8a9b-00000000aa03")
	auto := MirrorSubject("0b2c3d4e-5f60-4718-8a9b-00000000aa04")
	for _, sub := range []Subject{plain, named} {
		if _, err := s.Upsert(as(2), sub, Write{Title: "Some work here", Source: SourceSummarizer}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Upsert(as(2), auto, Write{Title: "Billing migration", Source: SourceSummarizer, Nameable: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetManualName(as(2), named, "Kept"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(sys(as(2)), `UPDATE agent_focus SET seen_at = now() - interval '10 days'`); err != nil {
		t.Fatal(err)
	}
	n, err := s.PruneAll(as(2), time.Now().Add(-DefaultRetention))
	if err != nil || n != 1 {
		t.Fatalf("prune: %d (%v), want 1 (the unnamed one)", n, err)
	}
	if _, ok, _ := s.Get(as(2), named); !ok {
		t.Fatal("the manually named row was pruned with the retention")
	}
	if _, ok, _ := s.Get(as(2), auto); !ok {
		t.Fatal("the auto-named row was pruned with the retention")
	}
	if _, ok, _ := s.Get(as(2), plain); ok {
		t.Fatal("the unnamed row survived the retention")
	}
	if _, err := pool.Exec(sys(as(2)), `UPDATE agent_focus SET seen_at = now() - interval '200 days'`); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PruneAll(as(2), time.Now().Add(-DefaultRetention)); err != nil || n != 2 {
		t.Fatalf("prune past the name retention: %d (%v), want 2", n, err)
	}
}
