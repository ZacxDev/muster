package api

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/notes"
)

// fleet_list's linked_tasks through the real claimant path, the real store and
// Postgres's row-level security: a reference whose claimants name a mirrored
// session S puts that task on S's row; a session with no link carries [];
// another owner's link naming the same session id, and another owner's
// session, never reach the owner's list; one read serves every row.
//
// Every session id, title and key below is invented and pairwise distinct.

const (
	// ltSIDQuiet is the owner's second mirrored session: linked to nothing.
	ltSIDQuiet = "5e55a1d0-0000-4000-8000-00000000b0b1"
	// ltSIDCap is the owner's third mirrored session: more links than the cap.
	ltSIDCap = "5e55a1d0-0000-4000-8000-00000000b0b2"
	// ltSIDUnlisted is a session of the owner's that no host lists: a task
	// linked to it must not show on any listed session's row.
	ltSIDUnlisted = "5e55a1d0-0000-4000-8000-00000000b0b3"
	// ltSIDStranger is the second owner's own mirrored session.
	ltSIDStranger = "5e55a1d0-0000-4000-8000-00000000b0b4"
	// ltCap is the documented per-session cap, pinned as a literal.
	ltCap = 10
)

// ownerMirror answers Hosts by the caller: the owner sees routeSID, ltSIDQuiet
// and ltSIDCap on one host; the stranger sees ltSIDStranger on another.
// Everything else is the embedded routeMirror's.
type ownerMirror struct {
	*routeMirror
	owner, stranger int64
}

func ltHost(host string, sids ...string) mirror.HostView {
	var raw []mirror.Window
	for i, sid := range sids {
		raw = append(raw, mirror.Window{Session: "work", ID: "@" + string(rune('4'+i)), Index: i + 1, Claude: true,
			ClaudeSession: sid, Status: mirror.StatusIdle, StatusSince: 1})
	}
	ws := mirror.NormalizeWindows(raw, 0)
	h := mirror.HostView{Host: host, Windows: ws, Online: true, Rendered: true, Interactive: true, PanePaste: true, StatusReport: true}
	h.Sessions, h.Claude, h.Labels, h.Activity = mirror.SessionsOf(ws)
	return h
}

func (m *ownerMirror) Hosts(ctx context.Context) []mirror.HostView {
	p, _ := authz.From(ctx)
	switch p.UserID {
	case m.owner:
		return []mirror.HostView{ltHost("tower", routeSID, ltSIDQuiet, ltSIDCap)}
	case m.stranger:
		return []mirror.HostView{ltHost("bench", ltSIDStranger)}
	}
	return nil
}

// countingLinks counts the reverse-lookup reads fleet_list makes.
type countingLinks struct {
	notes.Store
	mu              sync.Mutex
	batched, single int
}

func (c *countingLinks) TasksForSession(ctx context.Context, sid string) ([]notes.TaskLink, error) {
	c.mu.Lock()
	c.single++
	c.mu.Unlock()
	return c.Store.TasksForSession(ctx, sid)
}

func (c *countingLinks) TasksForSessions(ctx context.Context, sids []string, per int) (map[string][]notes.TaskLink, error) {
	c.mu.Lock()
	c.batched++
	c.mu.Unlock()
	return c.Store.TasksForSessions(ctx, sids, per)
}

type ltTask struct {
	ID     int64  `json:"id"`
	Key    string `json:"key"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

type ltRow struct {
	ID          string    `json:"id"`
	LinkedTasks *[]ltTask `json:"linked_tasks"`
}

// ltFleet is fleet_list as ctx's principal: its rows by id.
func ltFleet(t *testing.T, s *Server, ctx context.Context) map[string]ltRow {
	t.Helper()
	text := s.toolFleetList(ctx, `{}`)
	var got struct {
		Sessions []ltRow `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(text), &got); err != nil || got.Sessions == nil {
		t.Fatalf("fleet_list: %v:\n%s", err, text)
	}
	out := map[string]ltRow{}
	for _, r := range got.Sessions {
		out[r.ID] = r
	}
	return out
}

func ltIDs(ts []ltTask) []int64 {
	out := make([]int64, 0, len(ts))
	for _, x := range ts {
		out = append(out, x.ID)
	}
	return out
}

// ltCreate files an ordinary task as ctx's principal and links it to sid.
func ltCreate(t *testing.T, s *Server, ctx context.Context, title, status, sid string) int64 {
	t.Helper()
	n, err := s.ext.Notes.Create(ctx, notes.Note{Title: title, Body: "linked-tasks fixture", Status: status})
	if err != nil {
		t.Fatalf("create %q: %v", title, err)
	}
	if _, _, err := s.ext.Notes.LinkSession(ctx, n.ID, notes.SessionLink{SessionID: sid, Role: notes.RoleWorked}, false); err != nil {
		t.Fatalf("link %q to %s: %v", title, sid, err)
	}
	return n.ID
}

// TestFleetListCarriesEachSessionsLinkedTasks: see the file comment. Red at
// the base (no linked_tasks field at all).
func TestFleetListCarriesEachSessionsLinkedTasks(t *testing.T) {
	f := newTriageFixture(t, false)
	stranger := f.newMember(t)
	f.srv.ext.Mirror = &ownerMirror{routeMirror: f.mirror, owner: f.owner, stranger: stranger}
	count := &countingLinks{Store: f.srv.ext.Notes}
	f.srv.ext.Notes = count
	asStranger := f.as(stranger)

	// S's task: a reference whose claimants name S — linked by core's own
	// claimant path (linkUpstreamClaimants), not by the test.
	key := refKey()
	refID := f.upsertRef(t, key, claimSnap("in_progress", "doing", map[string]any{
		"title":     "Recalibrate the lathe sensor",
		"claimants": []map[string]any{{"agent": "ag_lathe01", "agent_session": routeSID}}}))

	// The owner's task linked to a session no host lists.
	elsewhere := ltCreate(t, f.srv, f.asOwner, "Sweep the loading dock", "open", ltSIDUnlisted)
	// More links than the cap on the third session: the newest are kept.
	var capped []int64
	for i := 0; i < ltCap+2; i++ {
		capped = append(capped, ltCreate(t, f.srv, f.asOwner, "Stack crate "+string(rune('A'+i)), "open", ltSIDCap))
	}
	// The stranger's task linked to the OWNER's session id, and one linked to
	// the stranger's own session.
	strangerOnS := ltCreate(t, f.srv, asStranger, "Repaint the stranger's fence", "ready_for_review", routeSID)
	strangerOwn := ltCreate(t, f.srv, asStranger, "Tune the stranger's piano", "in_progress", ltSIDStranger)

	// Positive controls: the stranger's links exist and are readable by the
	// stranger, so their absence below is RLS, not an empty fixture.
	for sid, want := range map[string]int64{routeSID: strangerOnS, ltSIDStranger: strangerOwn} {
		if got, err := f.srv.ext.Notes.TasksForSession(asStranger, sid); err != nil || !slices.Equal(ltLinkIDs(got), []int64{want}) {
			t.Fatalf("fixture: the stranger's own view of %s's links = %+v, %v; want [%d]", sid, got, err, want)
		}
	}

	count.mu.Lock()
	count.batched, count.single = 0, 0
	count.mu.Unlock()
	rows := ltFleet(t, f.srv, f.asOwner)

	s := rows[focus.MirrorSubject(routeSID).String()]
	var sTasks []ltTask
	if s.LinkedTasks != nil {
		sTasks = *s.LinkedTasks
	}
	var mine []ltTask
	for _, x := range sTasks {
		switch {
		case x.ID == strangerOnS:
			t.Errorf("RLS: another owner's task linked to S's session id is on S's row: %+v", x)
		case x.ID == elsewhere || slices.Contains(capped, x.ID):
			t.Errorf("LEAK: a task linked to another session is on S's row: %+v", x)
		default:
			mine = append(mine, x)
		}
	}
	if want := []ltTask{{ID: refID, Key: key, Title: "Recalibrate the lathe sensor", Status: "in_progress"}}; !slices.Equal(mine, want) {
		t.Errorf("CLAIMANT: S's linked_tasks = %+v; want %+v", mine, want)
	}

	q := rows[focus.MirrorSubject(ltSIDQuiet).String()]
	if q.ID == "" || q.LinkedTasks == nil || len(*q.LinkedTasks) != 0 {
		t.Errorf("EMPTY: the unlinked session's row = %+v; want linked_tasks: []", q)
	}

	c := rows[focus.MirrorSubject(ltSIDCap).String()]
	wantCap := slices.Clone(capped)
	slices.Reverse(wantCap)
	wantCap = wantCap[:ltCap]
	if c.LinkedTasks == nil || !slices.Equal(ltIDs(*c.LinkedTasks), wantCap) {
		var got []int64
		if c.LinkedTasks != nil {
			got = ltIDs(*c.LinkedTasks)
		}
		t.Errorf("CAP: the over-linked session's ids = %v; want the %d newest, newest first: %v", got, ltCap, wantCap)
	}

	if _, ok := rows[focus.MirrorSubject(ltSIDStranger).String()]; ok {
		t.Errorf("OWNER: the stranger's session is on the owner's fleet_list")
	}
	for id, r := range rows {
		if r.LinkedTasks == nil {
			continue
		}
		for _, x := range *r.LinkedTasks {
			if x.ID == strangerOwn || (x.ID == strangerOnS && id != focus.MirrorSubject(routeSID).String()) {
				t.Errorf("RLS: the stranger's task %d is on the owner's row %s", x.ID, id)
			}
		}
	}

	count.mu.Lock()
	batched, single := count.batched, count.single
	count.mu.Unlock()
	if batched != 1 || single != 0 {
		t.Errorf("N+1: fleet_list read the links %d batched + %d per-session times; want 1 + 0", batched, single)
	}

	// The stranger's own list does carry its own session's task: the
	// stranger's links are visible to the stranger, so their absence above is
	// the owner scope, not an empty fixture.
	if sr := ltFleet(t, f.srv, asStranger)[focus.MirrorSubject(ltSIDStranger).String()]; sr.LinkedTasks == nil ||
		!slices.Equal(ltIDs(*sr.LinkedTasks), []int64{strangerOwn}) {
		t.Errorf("CONTROL: the stranger's own fleet_list row = %+v; want its task %d", sr, strangerOwn)
	}
}

func ltLinkIDs(ls []notes.TaskLink) []int64 {
	out := make([]int64, 0, len(ls))
	for _, l := range ls {
		out = append(out, l.NoteID)
	}
	return out
}
