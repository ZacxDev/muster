package focus

import (
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/dbtest"
)

// THE WINDOW READ (history_window.go) against a real Postgres. Synthetic
// fixtures only: invented session ids and titles, distinct from every other
// test's.

func windowOf(t *testing.T, s *PGStore, uid int64, since, until time.Time) map[Subject]WindowSubject {
	t.Helper()
	r, err := s.WindowHistory(as(uid), since, until)
	if err != nil {
		t.Fatal(err)
	}
	out := map[Subject]WindowSubject{}
	for _, w := range r.Subjects {
		out[w.Subject] = w
	}
	return out
}

func titlesOf(w WindowSubject) string {
	var out []string
	for _, t := range w.Titles {
		out = append(out, t.Title)
	}
	return strings.Join(out, " | ")
}

// 🔴 TestTheWindowReadsEndedSessionsAndOnlyTheCallersOwn: a session with no
// liveness at all (the read takes none) is reported with its in-window titles,
// oldest first; a title outside the window is not; another owner reads none of
// it.
func TestTheWindowReadsEndedSessionsAndOnlyTheCallersOwn(t *testing.T) {
	s, pool := pgStore(t)
	ended := MirrorSubject("0c1d2e3f-win-ended")
	agent := AgentSubject(mustAgent(t, pool, 2, "focus-win-a").ID)
	write(t, s, 2, ended, "Drafting the gadget changelog", "", "")
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE agent_focus_history SET created_at = now() - interval '5 days'
		WHERE subject_key = '0c1d2e3f-win-ended' AND title = 'Drafting the gadget changelog'`)
	write(t, s, 2, ended, "Pairing on the gadget importer", "", "")
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE agent_focus_history SET created_at = now() - interval '20 hours'
		WHERE subject_key = '0c1d2e3f-win-ended' AND title = 'Pairing on the gadget importer'`)
	write(t, s, 2, ended, "Shipping the gadget importer", "", "")
	// The current focus row's refresh time is ALSO activity: put it outside
	// the window, so the window's only rows are the two history rows.
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE agent_focus SET updated_at = now() - interval '10 days'
		WHERE subject_key = '0c1d2e3f-win-ended'`)
	write(t, s, 2, agent, "Tuning the gadget cache", "", "")

	now := time.Now()
	got := windowOf(t, s, 2, now.Add(-24*time.Hour), now.Add(time.Minute))
	w, ok := got[ended]
	if !ok {
		t.Fatalf("the ended session is not in the window: %+v", got)
	}
	if titlesOf(w) != "Pairing on the gadget importer | Shipping the gadget importer" || !w.FirstAt.Before(w.LastAt) {
		t.Fatalf("the ended session: %q first %s last %s; want its two in-window titles, oldest first", titlesOf(w), w.FirstAt, w.LastAt)
	}
	if a, ok := got[agent]; !ok || titlesOf(a) != "Tuning the gadget cache" {
		t.Fatalf("the agent: %+v %v", a, ok)
	}
	// Another owner reads none of it (row-level security). ⚠ Invariant guard:
	// the owner is never a parameter; the policy does it.
	other := windowOf(t, s, 3, now.Add(-24*time.Hour), now.Add(time.Minute))
	if _, leaked := other[ended]; leaked {
		t.Fatal("another owner's window read returned owner 2's session")
	}
	if _, leaked := other[agent]; leaked {
		t.Fatal("another owner's window read returned owner 2's agent")
	}
	// The window bounds both ends: a window that ends before the 20 h row
	// holds only the 5-day-old one.
	early := windowOf(t, s, 2, now.Add(-6*24*time.Hour+time.Hour), now.Add(-2*24*time.Hour))
	if e, ok := early[ended]; !ok || titlesOf(e) != "Drafting the gadget changelog" {
		t.Fatalf("an early window: %+v %v; want only the 5-day-old title", e, ok)
	}
}

// A session whose focus did not CHANGE in the window but was refreshed there
// (its current row's updated_at) is reported, with that focus.
func TestARefreshWithNoChangeIsActivity(t *testing.T) {
	s, _ := pgStore(t)
	sub := MirrorSubject("0c1d2e3f-win-steady")
	write(t, s, 2, sub, "Steadily porting the gizmo parser", "", "")
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE agent_focus_history SET created_at = now() - interval '3 days'
		WHERE subject_key = '0c1d2e3f-win-steady'`)
	now := time.Now()
	w, ok := windowOf(t, s, 2, now.Add(-time.Hour), now.Add(time.Minute))[sub]
	if !ok || titlesOf(w) != "Steadily porting the gizmo parser" {
		t.Fatalf("%+v %v; want the session, by its refresh", w, ok)
	}
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE agent_focus SET updated_at = now() - interval '3 days' WHERE subject_key = '0c1d2e3f-win-steady'`)
	if _, ok := windowOf(t, s, 2, now.Add(-time.Hour), now.Add(time.Minute))[sub]; ok {
		t.Fatal("control: with neither row in the window the session is still reported")
	}
}

// At most MaxWindowTitles titles per session, the newest, the rest counted.
func TestTheTitlesPerSessionAreCappedAndCounted(t *testing.T) {
	s, _ := pgStore(t)
	sub := MirrorSubject("0c1d2e3f-win-many")
	n := MaxWindowTitles + 3
	for i := 0; i < n; i++ {
		write(t, s, 2, sub, "Gizmo step "+strings.Repeat("i", i+1), "", "")
	}
	now := time.Now()
	w := windowOf(t, s, 2, now.Add(-time.Hour), now.Add(time.Minute))[sub]
	if len(w.Titles) != MaxWindowTitles || w.TitlesOmitted != 3 || w.Titles[len(w.Titles)-1].Title != "Gizmo step "+strings.Repeat("i", n) {
		t.Fatalf("%d titles, %d omitted, newest %q", len(w.Titles), w.TitlesOmitted, w.Titles[len(w.Titles)-1].Title)
	}
}

func TestAWindowThatIsEmptyOrTooLongIsRefused(t *testing.T) {
	s, _ := pgStore(t)
	now := time.Now()
	for name, w := range map[string][2]time.Time{
		"inverted": {now, now.Add(-time.Hour)},
		"empty":    {now, now},
		"too long": {now.Add(-MaxWindowSpan - time.Second), now},
		"no since": {{}, now},
	} {
		if _, err := s.WindowHistory(as(2), w[0], w[1]); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := s.WindowHistory(as(2), now.Add(-MaxWindowSpan), now); err != nil {
		t.Errorf("control: exactly the longest window is refused: %v", err)
	}
}
