package pages

import (
	"encoding/json"
	"maps"
	"testing"
)

// TestWaitingIsTheCurrentVersionsRule: Store.Waiting (the Pages list's
// "Waiting on you") is, at each page's CURRENT version, (a) an approval or a
// required component with no response, or (b) an answerable component and no
// response at all. Each page below differs from the others in one way:
//
//   - open: one optional question, no response                  → waiting (b)
//   - byOwner / byLink: it answered from the owner's view / a link → not
//   - older: answered at v1; v2 (current) asks the same id      → waiting
//   - rolledBack: older's twin, rolled back to its answered v1  → not
//   - htmlOnly: an html block whose script could send anything  → not
//   - approvalDone: approval answered, an optional question left → not
//   - allOptional: an optional question and choice, no answers  → waiting (b)
//   - oneOptional: allOptional's twin, the choice answered      → not
//   - requiredLeft: a REQUIRED question unanswered, the approval
//     beside it answered                                         → waiting (a)
//   - approvalLeft: the approval unanswered, an optional question
//     answered                                                   → waiting (a)
//   - formRequired: a form with a required field unanswered, an
//     optional question answered                                 → waiting (a)
//   - uploaded: an upload answered by a FILE (KindUpload)        → not
//   - gallery: a pick gallery (and one without), no picks       → waiting (b)
//   - picked: a REQUIRED pick, picked with the real {picks} shape → not
//   - unlisted: like open, but its id is not asked about        → absent
//
// Another owner reads none of them.
//
// At bab5ee17f the rule was "any answerable component unanswered" (and the
// method was Unanswered): approvalDone, oneOptional and uploaded's twin
// rules differed there, and Waiting did not exist — so this file does not
// compile there. The rule's regression value is the cases' pairwise
// contrast and the mutation checks recorded in the PR.
func TestWaitingIsTheCurrentVersionsRule(t *testing.T) {
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
	answer := func(p Page, v int, component, data string) {
		t.Helper()
		if _, err := s.AddOwnerResponse(two, p.ID, v, component, KindSubmit, []byte(data), ""); err != nil {
			t.Fatal(err)
		}
	}
	const q = `[{"type":"markdown","text":"# Plan"},{"type":"question","id":"q1","label":"Which date?"}]`
	const day = `{"answer":"Tuesday"}`

	open := create("Open question", q)

	byOwner := create("Answered by its owner", q)
	answer(byOwner, 1, "q1", day)

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
	answer(older, 1, "q1", day)
	newVersion(older)

	rolledBack := create("Rolled back to its answer", q)
	answer(rolledBack, 1, "q1", day)
	newVersion(rolledBack)
	if _, err := s.SetCurrent(two, rolledBack.ID, 1); err != nil {
		t.Fatal(err)
	}

	htmlOnly := create("Custom script", `[{"type":"html","html":"<button onclick=\"muster.submit({a:1})\">Go</button>"}]`)

	const approvalAndQuestion = `[{"type":"approval","id":"a1","label":"Ship it?"},{"type":"question","id":"q1","label":"Anything else?"}]`
	approvalDone := create("Approved, a note left", approvalAndQuestion)
	answer(approvalDone, 1, "a1", `{"decision":"approve"}`)

	const optional = `[{"type":"question","id":"q1","label":"Anything else?"},{"type":"choice","id":"c1","label":"Which venue?","options":["Hall","Garden"]}]`
	allOptional := create("All optional", optional)
	oneOptional := create("All optional, one answered", optional)
	answer(oneOptional, 1, "c1", `{"choice":"Hall"}`)

	requiredLeft := create("Required left", `[{"type":"approval","id":"a1","label":"Ship it?"},{"type":"question","id":"q1","label":"Which date?","required":true}]`)
	answer(requiredLeft, 1, "a1", `{"decision":"approve"}`)

	approvalLeft := create("Approval left", approvalAndQuestion)
	answer(approvalLeft, 1, "q1", day)

	formRequired := create("Form left", `[{"type":"form","id":"f1","fields":[{"name":"email","type":"email","required":true}]},{"type":"question","id":"q1","label":"Anything else?"}]`)
	answer(formRequired, 1, "q1", day)

	uploaded := create("Uploaded", `[{"type":"upload","id":"u1","label":"Your logo"}]`)
	if _, err := s.AddOwnerResponse(two, uploaded.ID, 1, "u1", KindUpload, []byte(`{}`), "upl000000000000000071"); err != nil {
		t.Fatal(err)
	}

	gallery := create("Gallery", `[{"type":"markdown","text":"# Logos"}]`)
	ga := addAsset(t, s, 2, agentA, gallery.ID, 71, 10, "image/png")
	gb, gn := blocksOf(t, `[
	 {"type":"gallery","id":"look","items":[{"asset":"`+ga.Ref()+`"}]},
	 {"type":"gallery","id":"pick","items":[{"asset":"`+ga.Ref()+`"}],"pick":{}}]`)
	if _, _, err := s.AddVersion(two, agentA, gallery.ID, gallery.Title, gb, gn); err != nil {
		t.Fatal(err)
	}

	picked := create("Picked", `[{"type":"markdown","text":"# Logos"}]`)
	pa := addAsset(t, s, 2, agentA, picked.ID, 72, 10, "image/png")
	pb, pn := blocksOf(t, `[{"type":"gallery","id":"pick","items":[{"asset":"`+pa.Ref()+`"}],"pick":{"required":true}}]`)
	if _, _, err := s.AddVersion(two, agentA, picked.ID, picked.Title, pb, pn); err != nil {
		t.Fatal(err)
	}
	// The picks as the shell sends them, through the submit check.
	picks, err := CheckSubmit(pb, "pick", json.RawMessage(`{"picks":[{"index":0,"asset":"`+pa.Ref()+`"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddOwnerResponse(two, picked.ID, 2, "pick", KindSubmit, picks, ""); err != nil {
		t.Fatal(err)
	}

	unlisted := create("Not asked about", q)

	all := []*Page{&open, &byOwner, &byLink, &older, &rolledBack, &htmlOnly, &approvalDone, &allOptional, &oneOptional,
		&requiredLeft, &approvalLeft, &formRequired, &uploaded, &gallery, &picked}
	var ids []int64
	for _, p := range all {
		ids = append(ids, p.ID)
	}
	got, err := s.Waiting(two, ids)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]bool{open.ID: true, older.ID: true, allOptional.ID: true, requiredLeft.ID: true, approvalLeft.ID: true, formRequired.ID: true, gallery.ID: true}
	if !maps.Equal(got, want) {
		t.Errorf("Waiting: %v, want %v", got, want)
	}
	for name, c := range map[string]struct {
		id   int64
		want bool
	}{
		"an optional question, no answer":                      {open.ID, true},
		"a question the owner answered":                        {byOwner.ID, false},
		"a question answered through a link":                   {byLink.ID, false},
		"a question answered only on an OLDER version":         {older.ID, true},
		"a page rolled back to its answered version":           {rolledBack.ID, false},
		"an html-only page":                                    {htmlOnly.ID, false},
		"the approval answered, an optional question left":     {approvalDone.ID, false},
		"an all-optional page, no answer":                      {allOptional.ID, true},
		"an all-optional page, one answer":                     {oneOptional.ID, false},
		"a required question left beside an answered approval": {requiredLeft.ID, true},
		"the approval left beside an answered question":        {approvalLeft.ID, true},
		"a form's required field left":                         {formRequired.ID, true},
		"an upload answered with a file":                       {uploaded.ID, false},
		"a pick gallery, no picks":                             {gallery.ID, true},
		"a required pick, picked":                              {picked.ID, false},
		"a page whose id was not asked about":                  {unlisted.ID, false},
	} {
		if got[c.id] != c.want {
			t.Errorf("%s (page %d): waiting %v, want %v", name, c.id, got[c.id], c.want)
		}
	}
	// CONTROL: the unlisted page IS waiting when asked about.
	if w, _ := s.Waiting(two, []int64{unlisted.ID}); !w[unlisted.ID] {
		t.Errorf("CONTROL: the unlisted page is not waiting when asked about")
	}
	if w, err := s.Waiting(two, nil); err != nil || len(w) != 0 {
		t.Errorf("no ids: %v (%v)", w, err)
	}
	// Another owner reads none of it, whatever ids it names.
	if other, err := s.Waiting(asUser(3), ids); err != nil || len(other) != 0 {
		t.Errorf("user 3 reads %v (%v) of user 2's pages", other, err)
	}
}

// TestATeamPageAnsweredByAnotherMemberIsNotWaiting: a team's page (0165) is
// waiting for every member until ANY member answers it; once another member
// has, it is not waiting for the one reading. Someone off the team reads
// nothing of it.
func TestATeamPageAnsweredByAnotherMemberIsNotWaiting(t *testing.T) {
	f := newTeamFixture(t)
	b, n := blocksOf(t, `[{"type":"question","id":"q1","label":"Anything else?"}]`)
	p, _, err := f.s.Create(asTeam(f.teamA), Creator{AgentID: 900 + f.teamA, Label: "team-worker"}, "Team plan", b, n)
	if err != nil {
		t.Fatal(err)
	}
	ids := []int64{p.ID}
	viewer := f.person(t, f.viewerA)
	if w, err := f.s.Waiting(viewer, ids); err != nil || !w[p.ID] {
		t.Fatalf("CONTROL: before any answer the team's page is not waiting for a member: %v (%v)", w, err)
	}
	if _, err := f.s.AddOwnerResponse(f.person(t, f.both), p.ID, 1, "q1", KindSubmit, []byte(`{"answer":"Monday"}`), ""); err != nil {
		t.Fatal(err)
	}
	if w, err := f.s.Waiting(viewer, ids); err != nil || w[p.ID] {
		t.Errorf("answered by another member, the team's page is still waiting for a member: %v (%v)", w, err)
	}
	if w, err := f.s.Waiting(f.person(t, f.outsider), ids); err != nil || len(w) != 0 {
		t.Errorf("someone off the team reads %v (%v)", w, err)
	}
}
