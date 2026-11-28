package pages

import (
	"maps"
	"testing"
)

// TestUnansweredCountsTheCurrentVersionsOpenComponents: Store.Unanswered
// (the Pages list's "Waiting on you" filter) counts, per page, the answerable
// components of the CURRENT version that nobody has answered at that version.
// Each page below differs from the others in exactly one way, and every count
// is distinct where it is drawn at all:
//
//   - open: one question, no response                         → 1
//   - byOwner: the question answered from the owner's view     → absent
//   - byLink: the question answered through a share link       → absent
//   - older: answered at v1; v2 (current) asks the same id     → 1
//   - rolledBack: older's twin, rolled back to its answered v1 → absent
//   - htmlOnly: an html block whose script could send anything → absent
//   - mixed: form, choice, approval, upload, a question; only the question
//     and the choice answered                                 → 3
//   - picked: a gallery with a pick (no answer), and one without → 1
//
// Another owner reads none of them. NEW FEATURE: there is no Unanswered at
// origin/main, so this cannot be red there; its regression value is the
// mutation check (the current-version condition), recorded in the PR.
func TestUnansweredCountsTheCurrentVersionsOpenComponents(t *testing.T) {
	s, _ := pgStore(t)
	two := asUser(2)
	create := func(title, raw string) Page {
		t.Helper()
		b, n := blocksOf(t, raw)
		p, _, err := s.Create(two, agentA, title, b, n)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	ownerAnswer := func(p Page, v int, component string) {
		t.Helper()
		if _, err := s.AddOwnerResponse(two, p.ID, v, component, KindSubmit, []byte(`{"answer":"Tuesday"}`), ""); err != nil {
			t.Fatal(err)
		}
	}
	const q = `[{"type":"markdown","text":"# Plan"},{"type":"question","id":"q1","label":"Which date?"}]`

	open := create("Open question", q)

	byOwner := create("Answered by its owner", q)
	ownerAnswer(byOwner, 1, "q1")

	byLink := create("Answered through a link", q)
	l, err := s.CreateLink(two, byLink.ID, LinkLive, "you", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddResponse(two, l, 1, "q1", KindSubmit, []byte(`{"answer":"Friday"}`), ""); err != nil {
		t.Fatal(err)
	}

	newVersion := func(p Page) {
		t.Helper()
		b, n := blocksOf(t, `[{"type":"markdown","text":"# Plan, revised"},{"type":"question","id":"q1","label":"Which date now?"}]`)
		if _, _, err := s.AddVersion(two, agentA, p.ID, p.Title, b, n); err != nil {
			t.Fatal(err)
		}
	}
	older := create("Answered only before", q)
	ownerAnswer(older, 1, "q1")
	newVersion(older)

	rolledBack := create("Rolled back to its answer", q)
	ownerAnswer(rolledBack, 1, "q1")
	newVersion(rolledBack)
	if _, err := s.SetCurrent(two, rolledBack.ID, 1); err != nil {
		t.Fatal(err)
	}

	htmlOnly := create("Custom script", `[{"type":"html","html":"<button onclick=\"muster.submit({a:1})\">Go</button>"}]`)

	mixed := create("Every kind", `[
	 {"type":"question","id":"q1","label":"Anything else?"},
	 {"type":"choice","id":"c1","label":"Which venue?","options":["Hall","Garden"]},
	 {"type":"form","id":"f1","fields":[{"name":"email","type":"email"}]},
	 {"type":"approval","id":"a1","label":"Ship it?"},
	 {"type":"upload","id":"u1","label":"Your logo"}]`)
	ownerAnswer(mixed, 1, "q1")
	ownerAnswer(mixed, 1, "c1")

	picked := create("Gallery", `[{"type":"markdown","text":"# Logos"}]`)
	a := addAsset(t, s, 2, agentA, picked.ID, 71, 10, "image/png")
	b, n := blocksOf(t, `[
	 {"type":"gallery","id":"look","items":[{"asset":"`+a.Ref()+`"}]},
	 {"type":"gallery","id":"pick","items":[{"asset":"`+a.Ref()+`"}],"pick":{}}]`)
	if _, _, err := s.AddVersion(two, agentA, picked.ID, picked.Title, b, n); err != nil {
		t.Fatal(err)
	}

	got, err := s.Unanswered(two)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]int{open.ID: 1, older.ID: 1, mixed.ID: 3, picked.ID: 1}
	if !maps.Equal(got, want) {
		t.Errorf("Unanswered: %v, want %v (open %d, byOwner %d, byLink %d, older %d, rolledBack %d, htmlOnly %d, mixed %d, picked %d)",
			got, want, open.ID, byOwner.ID, byLink.ID, older.ID, rolledBack.ID, htmlOnly.ID, mixed.ID, picked.ID)
	}
	// Each case by name, so a failure says which rule broke.
	for name, c := range map[string]struct {
		id   int64
		want int
	}{
		"an unanswered question":                      {open.ID, 1},
		"a question the owner answered":               {byOwner.ID, 0},
		"a question answered through a link":          {byLink.ID, 0},
		"a question answered only on an OLDER version": {older.ID, 1},
		"a page rolled back to its answered version":  {rolledBack.ID, 0},
		"an html-only page":                           {htmlOnly.ID, 0},
		"three of five components unanswered":         {mixed.ID, 3},
		"a gallery with a pick (and one without)":     {picked.ID, 1},
	} {
		if got[c.id] != c.want {
			t.Errorf("%s: %d unanswered, want %d", name, got[c.id], c.want)
		}
	}
	// Another owner reads none of it.
	if other, err := s.Unanswered(asUser(3)); err != nil || len(other) != 0 {
		t.Errorf("user 3 reads %v (%v) of user 2's pages", other, err)
	}
}
