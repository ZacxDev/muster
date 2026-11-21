package plugins

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/users"
)

// draftSearchIDs is SearchDraftCards' answer as ctx's principal, as ids.
func draftSearchIDs(t *testing.T, s *PGStore, ctx context.Context, id int64, text string) []int64 {
	t.Helper()
	cards, err := s.SearchDraftCards(ctx, id, text, 0)
	if err != nil {
		t.Fatalf("search drafts (%d, %q): %v", id, text, err)
	}
	out := make([]int64, 0, len(cards))
	for _, c := range cards {
		out = append(out, c.Draft.ID)
	}
	return out
}

// TestSearchDraftCardsByIdAndLiteralText: a draft is found by its id and by a
// piece of its text, case folded, newest first; % and _ are themselves; a
// word no draft holds finds none (the control).
func TestSearchDraftCardsByIdAndLiteralText(t *testing.T) {
	w := newDraftWorld(t)
	a := w.file(t, w.m, "The Heron launch is Tuesday.")
	m2 := w.delivered(t, "m-2", false)
	b := w.file(t, m2, "Raise it by 50% for the heron crew")
	m3 := w.delivered(t, "m-3", false)
	c := w.file(t, m3, "rename a_c please")
	me := session(2)
	for _, x := range []struct {
		name string
		id   int64
		text string
		want []int64
	}{
		{"by id", b.ID, "", []int64{b.ID}},
		{"by text, another case, newest first", 0, "HERON", []int64{b.ID, a.ID}},
		{"% is itself", 0, "%", []int64{b.ID}},
		{"_ is itself", 0, "a_c", []int64{c.ID}},
		{"control: a word no draft holds", 0, "pelican", []int64{}},
		{"control: an id no draft has", c.ID + 1000, "", []int64{}},
	} {
		if got := draftSearchIDs(t, w.s, me, x.id, x.text); !slices.Equal(got, x.want) {
			t.Errorf("%s: %v, want %v", x.name, got, x.want)
		}
	}
}

// TestSearchDraftCardsFindsADraftOfAnyAge: a draft created well over 30 days
// ago is found by its text (operator decision: search has no age limit).
// RED while search kept to a 30-day text window: only the recent draft was
// listed.
func TestSearchDraftCardsFindsADraftOfAnyAge(t *testing.T) {
	w := newDraftWorld(t)
	old := w.file(t, w.m, "The Heron launch is Tuesday.")
	m2 := w.delivered(t, "m-2", false)
	recent := w.file(t, m2, "Move the heron launch")
	dbtest.OwnerExec(t, w.dsn, `SET session_replication_role = replica;
		UPDATE plugin_reply_drafts SET created_at = now() - interval '400 days' WHERE id = `+strconv.FormatInt(old.ID, 10))
	if got := draftSearchIDs(t, w.s, session(2), 0, "heron"); !slices.Equal(got, []int64{recent.ID, old.ID}) {
		t.Errorf("by text: %v, want the recent draft %d and the 400-day-old draft %d", got, recent.ID, old.ID)
	}
}

// TestSearchDraftCardsTextIsOwnIdIsWhatTheCallerCanOpen: a team's operator
// reads the team's drafts (the control proves the card is READABLE to them
// through row-level security). Their TEXT search lists only their own — never
// the team's, never another owner's: RED with the text branch's owner
// condition removed. An ID finds any draft they can open (operator decision):
// the team's by its id — RED at 2244c0e1, where an id was the owner's own
// only — but another owner's still none, as an id nobody has.
func TestSearchDraftCardsTextIsOwnIdIsWhatTheCallerCanOpen(t *testing.T) {
	s, pool, dsn := pgStore(t)
	ctx := context.Background()
	dbtest.OwnerExec(t, dsn, `SELECT setval(pg_get_serial_sequence('public.users', 'id'), (SELECT max(id) FROM public.users))`)
	us := users.NewPG(pool)
	team, err := us.CreateTeam(ctx, users.Actor{UserID: 1, Via: authz.ViaSession}, "Drafters "+strconv.FormatInt(time.Now().UnixNano()%1e9, 36), 3)
	if err != nil {
		t.Fatal(err)
	}
	acct, err := us.Account(authz.System(ctx), 3)
	if err != nil {
		t.Fatal(err)
	}
	me := authz.WithPrincipal(ctx, authz.Principal{UserID: 3, Role: acct.Role, Via: authz.ViaSession, Teams: acct.Teams})
	asTeam := authz.WithPrincipal(ctx, authz.Principal{UserID: team.ID, Role: authz.RoleMember, Via: authz.ViaAgent, Team: true})
	// One draft each for the member, the team and another owner (user 2),
	// every one holding the same word. A team signs in at no plugin door, so
	// the team's is filed as user 2's and then handed to the team, row by row
	// (plugin, message, agent, draft), with the owner triggers off.
	draftFor := func(owner int64, name string) Draft {
		t.Helper()
		sess, plug := session(owner), plugin(owner)
		if owner == 3 {
			sess = me
			plug = authz.WithPrincipal(ctx, authz.Principal{UserID: 3, Role: acct.Role, Via: authz.ViaPlugin, Teams: acct.Teams})
		}
		_, p, err := s.Create(sess, name, "Example inbox", "example-inbox")
		if err != nil {
			t.Fatalf("plugin for %d: %v", owner, err)
		}
		chief := mustAgentAs(t, dsn, sess, "plg-"+name)
		m, _, err := s.Submit(plug, p.ID, msg("ext-"+name))
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := s.BeginDelivery(plug, m.ID, chief.ID); err != nil || !ok {
			t.Fatalf("begin delivery: %v %v", ok, err)
		}
		if ok, err := s.RecordDelivery(plug, m.ID, chief.ID, StatusDelivered); err != nil || !ok {
			t.Fatalf("deliver: %v %v", ok, err)
		}
		d, _, err := s.FileDraft(sess, chief.ID, owner, chief.Name, m.ID, "the shared gannet word from "+name)
		if err != nil {
			t.Fatalf("file for %d: %v", owner, err)
		}
		return d
	}
	handTo := func(d Draft, to int64) {
		t.Helper()
		id, owner := strconv.FormatInt(d.ID, 10), strconv.FormatInt(to, 10)
		dbtest.OwnerExec(t, dsn, `SET session_replication_role = replica;
			UPDATE plugins SET owner_id = `+owner+` WHERE id = (SELECT m.plugin_id FROM plugin_messages m JOIN plugin_reply_drafts d ON d.message_id = m.id WHERE d.id = `+id+`);
			UPDATE agents SET owner_id = `+owner+` WHERE id = (SELECT agent_id FROM plugin_reply_drafts WHERE id = `+id+`);
			UPDATE plugin_messages SET owner_id = `+owner+` WHERE id = (SELECT message_id FROM plugin_reply_drafts WHERE id = `+id+`);
			UPDATE plugin_reply_drafts SET owner_id = `+owner+` WHERE id = `+id)
	}
	mine := draftFor(3, "mine")
	teams := draftFor(2, "teams")
	handTo(teams, team.ID)
	others := draftFor(2, "others")

	// Control: the team's draft IS readable to the member.
	if c, err := s.DraftCard(me, teams.ID); err != nil || c.Draft.ID != teams.ID {
		t.Fatalf("control: the member reads the team's draft %d: %v", teams.ID, err)
	}
	if got := draftSearchIDs(t, s, asTeam, 0, "gannet"); !slices.Equal(got, []int64{teams.ID}) {
		t.Fatalf("control: the team's own search: %v, want %d", got, teams.ID)
	}
	if got := draftSearchIDs(t, s, me, 0, "gannet"); !slices.Equal(got, []int64{mine.ID}) {
		t.Errorf("the member's search by text: %v, want only their own %d (not the team's %d or another owner's %d)", got, mine.ID, teams.ID, others.ID)
	}
	if got := draftSearchIDs(t, s, me, teams.ID, ""); !slices.Equal(got, []int64{teams.ID}) {
		t.Errorf("the member's search for the team's draft %d by id: %v, want it (they can open it)", teams.ID, got)
	}
	for _, id := range []int64{others.ID, others.ID + 1_000_000} {
		if got := draftSearchIDs(t, s, me, id, ""); len(got) != 0 {
			t.Errorf("the member's search for draft %d (another owner's, or nobody's): %v, want none", id, got)
		}
	}
}

// TestSearchDraftCardsCutKeepsTheNewest: with more matches than the limit, the
// search keeps the NEWEST, whatever their status — an older pending draft
// never pushes out a newer decided one. The reply and outbound drafts are two
// queries merged and then cut, so the fixture spans both: a pending reply
// draft (oldest) and two rejected outbound drafts (newer). RED while the merge
// put pending first before the cut: [pending, newest] instead of the two
// newest.
func TestSearchDraftCardsCutKeepsTheNewest(t *testing.T) {
	w := newDraftWorld(t)
	pending := w.file(t, w.m, "the plover note, pending")
	var outbound []int64
	for _, ext := range []string{"m-2", "m-3"} {
		d := w.file(t, w.delivered(t, ext, false), "the plover note, "+ext)
		if got, err := w.s.DecideDraftFrom(session(2), d.ID, DraftDecision{}); err != nil || got.Status != DraftRejected {
			t.Fatalf("reject %d: %+v %v", d.ID, got, err)
		}
		// An outbound draft: no message, its plugin, an officer, a venue and
		// a post target (plugin_reply_drafts_outbound_shape).
		dbtest.OwnerExec(t, w.dsn, `SET session_replication_role = replica;
			UPDATE plugin_reply_drafts SET message_id = NULL, plugin_id = `+strconv.FormatInt(w.p.ID, 10)+`,
			       officer = 'marketing', venue = 'example', target_kind = 'post' WHERE id = `+strconv.FormatInt(d.ID, 10))
		outbound = append(outbound, d.ID)
	}
	newest := []int64{outbound[1], outbound[0]}
	// Control, whatever the order: uncut, all three match — the two outbound
	// drafts and the pending reply draft, the oldest.
	all, err := w.s.SearchDraftCards(session(2), 0, "plover", 0)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	var outboundSeen, pendingSeen int
	for _, c := range all {
		ids = append(ids, c.Draft.ID)
		if c.Draft.MessageID == 0 {
			outboundSeen++
		}
		if c.Draft.ID == pending.ID && c.Draft.Status == DraftPending {
			pendingSeen++
		}
	}
	sorted := slices.Sorted(slices.Values(ids))
	if !slices.Equal(sorted, []int64{pending.ID, outbound[0], outbound[1]}) || outboundSeen != 2 || pendingSeen != 1 {
		t.Fatalf("control: %v (%d outbound, %d pending), want the pending %d and the outbound %v", ids, outboundSeen, pendingSeen, pending.ID, outbound)
	}
	cut, err := w.s.SearchDraftCards(session(2), 0, "plover", 2)
	if err != nil {
		t.Fatal(err)
	}
	ids = ids[:0]
	for _, c := range cut {
		ids = append(ids, c.Draft.ID)
	}
	if !slices.Equal(ids, newest) {
		t.Errorf("cut to 2: %v, want the two newest %v (not the older pending %d)", ids, newest, pending.ID)
	}
}

// mustAgentAs is mustAgent for any principal (a team's included).
func mustAgentAs(t *testing.T, dsn string, ctx context.Context, name string) agents.Agent {
	t.Helper()
	pool, err := db.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	a, err := agents.NewPG(pool).Create(ctx, agents.Agent{Name: name, Namespace: "devpod-" + name, DisplayName: "chief",
		Status: agents.StatusRunning, HooksToken: "hooks-" + name})
	if err != nil {
		t.Fatalf("agent %s: %v", name, err)
	}
	return a
}
