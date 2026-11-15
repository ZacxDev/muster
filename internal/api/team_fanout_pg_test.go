package api

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/sse"
	"github.com/ZacxDev/muster/internal/users"
)

// The one team fan-out (team sharing phase 2b.1, docs/design/team-sharing.md
// part C), through the real server against a real Postgres: who a
// team-owned notification reaches, that each member is decided on their own
// (mutes, cap, codename, the team-prefixed title), the withdrawals, the
// notify switch, and a member's comment on a team task.
//
// Synthetic fixtures: team "fanout-<n>" with ana (manager), ben (operator),
// cal (viewer) and dee (not a member), plus an operator who turned the
// switch off, one who left and one who is disabled.

// fanPush is a PushService that records each payload by the owner sendPush
// named, and mutes events per owner.
type fanPush struct {
	mu    sync.Mutex
	got   map[int64][]push.Payload
	muted map[int64][]string
}

type fanStore struct {
	push.Store
	f *fanPush
}

func (s fanStore) Muted(_ context.Context, owner int64) ([]string, error) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	return s.f.muted[owner], nil
}

func (f *fanPush) PublicKey() string         { return "k" }
func (f *fanPush) Store() push.Store         { return fanStore{f: f} }
func (f *fanPush) Allowlist() push.Allowlist { return push.NewAllowlist(nil) }
func (f *fanPush) Probe(context.Context, push.Subscription, []byte) push.SendResult {
	return push.SendResult{}
}
func (f *fanPush) SendTo(_ context.Context, owner int64, p push.Payload) push.Report {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.got == nil {
		f.got = map[int64][]push.Payload{}
	}
	f.got[owner] = append(f.got[owner], p)
	return push.Report{}
}

func (f *fanPush) reset() {
	f.mu.Lock()
	f.got = nil
	f.mu.Unlock()
}

// sent is owner's payloads whose tag has prefix and whose type is (or, with
// "", is not) resolved.
func (f *fanPush) sent(owner int64, prefix string, resolved bool) []push.Payload {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []push.Payload
	for _, p := range f.got[owner] {
		if strings.HasPrefix(p.Tag, prefix) && (p.Type == push.TypeResolved) == resolved {
			out = append(out, p)
		}
	}
	return out
}

// to is which of people received at least one non-resolved push whose tag has
// prefix, sorted.
func (f *fanPush) to(prefix string, people ...int64) []int64 {
	var out []int64
	for _, id := range people {
		if len(f.sent(id, prefix, false)) > 0 {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// withdrawnFrom is which of people received a withdrawal (resolved) whose tag
// is exactly tag, sorted.
func (f *fanPush) withdrawnFrom(tag string, people ...int64) []int64 {
	var out []int64
	for _, id := range people {
		for _, p := range f.sent(id, tag, true) {
			if p.Tag == tag {
				out = append(out, id)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// fanTeam is the fixture team and its people.
type fanTeam struct {
	team                               int64
	name                               string
	ana, ben, cal, dee, off, gone, dis int64
}

func (f fanTeam) everyone() []int64 {
	return []int64{f.ana, f.ben, f.cal, f.dee, f.off, f.gone, f.dis}
}

// newFanTeam makes the fixture in w, with a fanPush wired as w's push.
func newFanTeam(t *testing.T, w *isoWorld) (fanTeam, *fanPush) {
	t.Helper()
	w.srv.ext.Users = users.NewPG(w.pool)
	rec := &fanPush{}
	w.srv.ext.Push = rec
	us := users.NewPG(w.pool)
	admin := users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}
	mk := func(name string) int64 {
		var id int64
		if err := w.pool.QueryRow(w.sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, name+"-"+w.suffix).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	f := fanTeam{ana: mk("fan-ana"), ben: mk("fan-ben"), cal: mk("fan-cal"), dee: mk("fan-dee"), off: mk("fan-off"), gone: mk("fan-gone"), dis: mk("fan-dis")}
	f.name = "fanout-" + w.suffix
	tm, err := us.CreateTeam(w.sys, admin, f.name, f.ana)
	if err != nil {
		t.Fatal(err)
	}
	f.team = tm.ID
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		for _, q := range []string{`DELETE FROM attention_asks WHERE owner_id = $1`, `DELETE FROM plugin_reply_drafts WHERE owner_id = $1`,
			`DELETE FROM officer_contacts WHERE owner_id = $1`, `DELETE FROM officer_items WHERE owner_id = $1`,
			`DELETE FROM officer_defs WHERE owner_id = $1`, `DELETE FROM officer_types WHERE owner_id = $1`,
			`DELETE FROM plugins WHERE owner_id = $1`, `DELETE FROM agents WHERE owner_id = $1`,
			`DELETE FROM note_comments WHERE owner_id = $1`, `DELETE FROM notes WHERE owner_id = $1`,
			`DELETE FROM team_members WHERE team_id = $1`, `DELETE FROM audit_log WHERE team_id = $1`} {
			_, _ = w.pool.Exec(bg, q, f.team)
		}
		for _, id := range f.everyone() {
			_, _ = w.pool.Exec(bg, `DELETE FROM note_comments WHERE owner_id = $1`, id)
			_, _ = w.pool.Exec(bg, `DELETE FROM notes WHERE owner_id = $1`, id)
			_, _ = w.pool.Exec(bg, `DELETE FROM team_members WHERE user_id = $1`, id)
			_, _ = w.pool.Exec(bg, `DELETE FROM audit_log WHERE user_id = $1`, id)
			_, _ = w.pool.Exec(bg, `DELETE FROM users WHERE id = $1`, id)
		}
	})
	for id, role := range map[int64]authz.TeamRole{f.ben: authz.TeamOperator, f.cal: authz.TeamViewer, f.off: authz.TeamOperator,
		f.gone: authz.TeamOperator, f.dis: authz.TeamOperator} {
		if err := us.SetTeamMember(w.sys, admin, f.team, id, role); err != nil {
			t.Fatal(err)
		}
	}
	if err := us.SetTeamNotify(w.sys, f.off, f.team, false); err != nil {
		t.Fatal(err)
	}
	if err := us.RemoveTeamMember(w.sys, admin, f.team, f.gone); err != nil {
		t.Fatal(err)
	}
	if _, err := w.pool.Exec(w.sys, `UPDATE users SET disabled_at = now() WHERE id = $1`, f.dis); err != nil {
		t.Fatal(err)
	}
	return f, rec
}

// fanDrafts seeds team's officer: one pending outbound DM draft and one
// pending follow (an outreach batch of one). It returns the DM's id, the
// follow's id and the batch's id.
func fanDrafts(t *testing.T, w *isoWorld, team int64) (dm, follow int64, batch string) {
	t.Helper()
	sys := w.sys
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	store := builtin.NewStore(w.pool)
	must(store.PutType(sys, team, builtin.Type{Key: "marketing", Name: "Team", Charter: "c"}))
	must(store.PutDef(sys, team, builtin.Def{Officer: builtinOfficerID, Enabled: true}))
	var agent, plugin, contact int64
	tid := strconv.FormatInt(team, 10)
	must(w.pool.QueryRow(sys, `INSERT INTO agents (owner_id, name, namespace) VALUES ($1, $2, $2) RETURNING id`, team, "fan-agent-"+tid).Scan(&agent))
	must(w.pool.QueryRow(sys, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope, capabilities)
		VALUES ($1, $2, 'r', md5($3) || md5($3), 'r', '{messages,draft_checks}') RETURNING id`, team, "fan-relay-"+tid, tid).Scan(&plugin))
	must(w.pool.QueryRow(sys, `INSERT INTO officer_contacts (owner_id, officer, venue, handle) VALUES ($1, $2, $3, 'tzed') RETURNING id`,
		team, builtinOfficerID, outVenue).Scan(&contact))
	ins := func(kind, body string) int64 {
		var id int64
		must(w.pool.QueryRow(sys, `INSERT INTO plugin_reply_drafts (owner_id, agent_id, agent_name, body, expires_at, check_state, checked_at, plugin_id, venue,
				target_kind, target_ref, officer, because_ref, contact_id, outreach_reason, requested_by_user)
			VALUES ($1, $2, 'a', $7, now() + interval '1 day', 'sendable', now(), $3, $4, $8, 'tzed', $5, 'r1', $6, 'r', $1) RETURNING id`,
			team, agent, plugin, outVenue, builtinOfficerID, contact, body, kind).Scan(&id))
		return id
	}
	dm, follow = ins("dm", "a synthetic hello"), ins("follow", "")
	d, err := plugins.NewPG(w.pool).Draft(sys, follow)
	must(err)
	return dm, follow, plugins.BatchID(team, builtinOfficerID, d.CreatedAt.UTC().Format("2006-01-02"))
}

// TestATeamPushReachesItsOperatorsAndManagers: a team-owned notification of
// each kind — a team task ready for review and a team's reply draft (neither
// an officer's: red at base, where only a team OFFICER's asks, escalations and
// drafts fanned out) and a team officer's ask — reaches the team's manager
// and operator, and NO viewer, no operator whose notify switch is off, no one
// who left and no disabled operator, and not the team id itself. The
// positive control is that ana's and ben's pushes are counted at all.
func TestATeamPushReachesItsOperatorsAndManagers(t *testing.T) {
	w := newIsoWorld(t)
	f, rec := newFanTeam(t, w)
	want := ids(f.ana, f.ben)
	people := append(f.everyone(), f.team)
	check := func(what, prefix string) {
		t.Helper()
		w.srv.WaitForPushes()
		if got := rec.to(prefix, people...); !sameIDs(got, want) {
			t.Errorf("%s pushed to %v; want its manager and operator %v (never viewer %d, switched-off %d, ex-member %d, disabled %d, outsider %d, the team %d)",
				what, got, want, f.cal, f.off, f.gone, f.dis, f.dee, f.team)
		}
		rec.reset()
	}

	w.srv.notifyTaskDone(context.Background(), notes.Note{ID: 990101, OwnerID: f.team, Status: notes.StatusReadyForReview, Title: "ship it"})
	check("a team task ready for review", "task-990101-done")

	w.srv.notifyDraftAwaitingNoting(context.Background(), f.team, plugins.Draft{ID: 990102, OwnerID: f.team}, plugins.Message{}, "")
	check("a team's reply draft", "plugin-draft-990102")

	if _, err := (officerAsker{w.srv}).Raise(context.Background(), officers.Officer{ID: "mk:a", Owner: f.team}, builtin.AskAbout{}, "Which launch date?", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	check("a team officer's ask", "attention-")

	// CONTROL: a personal push still reaches its owner alone, with no prefix.
	w.srv.notifyTaskDone(context.Background(), notes.Note{ID: 990103, OwnerID: f.ben, Status: notes.StatusReadyForReview, Title: "mine"})
	w.srv.WaitForPushes()
	if got := rec.to("task-990103-done", people...); !sameIDs(got, ids(f.ben)) {
		t.Errorf("a personal push went to %v; want only its owner %d", got, f.ben)
	}
}

// TestATeamNotificationReachesEachMemberOnce is the consolidation: a team
// officer's ask, its outbound draft and its outreach batch each reach every
// recipient EXACTLY once. It is red if any of phase 3's four member loops
// (team_notify.go officerAskFiled, reply_draft_tool.go's two, and
// outreach_batch.go's) is put back beside goNotify's fan-out: each member
// would get a second copy.
func TestATeamNotificationReachesEachMemberOnce(t *testing.T) {
	w := newIsoWorld(t)
	f, rec := newFanTeam(t, w)
	officer := builtinOfficerID
	if _, err := (officerAsker{w.srv}).Raise(context.Background(), officers.Officer{ID: officer, Owner: f.team}, builtin.AskAbout{}, "One copy?", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	w.srv.notifyDraftAwaitingNoting(context.Background(), f.team, plugins.Draft{ID: 990201, OwnerID: f.team, Officer: &officer}, plugins.Message{}, "")
	pid, follow := int64(1), plugins.TargetFollow
	w.srv.notifyDraftAwaitingNoting(context.Background(), f.team, plugins.Draft{ID: 990202, OwnerID: f.team, Officer: &officer, PluginID: &pid,
		TargetKind: &follow, CreatedAt: time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)}, plugins.Message{}, "")
	w.srv.WaitForPushes()
	for what, prefix := range map[string]string{"an officer's ask": "attention-", "an officer's draft": "plugin-draft-990201", "an outreach batch": "plugin-outreach-"} {
		for _, m := range []int64{f.ana, f.ben} {
			if n := len(rec.sent(m, prefix, false)); n != 1 {
				t.Errorf("%s reached member %d %d time(s); want exactly once", what, m, n)
			}
		}
	}
}

// TestATeamPushHonoursEachMembersMutesAndCap: the per-owner decisions run
// for EACH member, on that member's own settings — ben muted the event and
// gets nothing while ana is pushed; ana at her rate cap is throttled while
// ben (unmuted) is not; and each member's codename switch picks their own
// title. Red at base: the decisions ran once, for the team, which mutes
// nothing, has its own cap and no preference. INVARIANT beside it: sendPush
// handed a team id sends nothing (a team has no subscription).
func TestATeamPushHonoursEachMembersMutesAndCap(t *testing.T) {
	w := newIsoWorld(t)
	f, rec := newFanTeam(t, w)
	n := func(tag string) RouterNotification {
		return RouterNotification{Type: "plugin", ID: tag, Title: "plain title " + tag, CodenameTitle: "mango:1 led " + tag, Tag: tag}
	}

	rec.mu.Lock()
	rec.muted = map[int64][]string{f.ben: {push.EventPlugin}}
	rec.mu.Unlock()
	w.srv.goNotify(f.team, push.EventPlugin, n("fan-mute"), nil)
	w.srv.WaitForPushes()
	if got := rec.to("fan-mute", f.ana, f.ben); !sameIDs(got, ids(f.ana)) {
		t.Errorf("with ben muting %q the team push reached %v; want only ana %d", push.EventPlugin, got, f.ana)
	}

	rec.mu.Lock()
	rec.muted = nil
	rec.mu.Unlock()
	now := w.srv.now()
	for i := 0; i < pushOwnerBurst; i++ {
		w.srv.pushLimit.allow(f.ana, now)
	}
	w.srv.goNotify(f.team, push.EventPlugin, n("fan-cap"), nil)
	w.srv.WaitForPushes()
	if got := rec.to("fan-cap", f.ana, f.ben); !sameIDs(got, ids(f.ben)) {
		t.Errorf("with ana at her cap the team push reached %v; want only ben %d", got, f.ben)
	}
	w.srv.pushLimit.mu.Lock()
	w.srv.pushLimit.wins = nil
	w.srv.pushLimit.mu.Unlock()

	if err := w.srv.ext.NotifyPrefs.(interface {
		SetCodenameFirst(ctx context.Context, owner int64, on bool) error
	}).SetCodenameFirst(authz.WithPrincipal(context.Background(), authz.Principal{UserID: f.ana, Role: authz.RoleMember, Via: authz.ViaSession}), f.ana, true); err != nil {
		t.Fatal(err)
	}
	w.srv.goNotify(f.team, push.EventPlugin, n("fan-code"), nil)
	w.srv.WaitForPushes()
	prefix := f.name + " · " // the literal, never the constant under test
	for who, want := range map[int64]string{f.ana: prefix + "mango:1 led fan-code", f.ben: prefix + "plain title fan-code"} {
		got := rec.sent(who, "fan-code", false)
		if len(got) != 1 || got[0].Title != want {
			t.Errorf("member %d's title: %v; want exactly %q (their own codename switch)", who, got, want)
		}
	}

	// INVARIANT: sendPush itself is not taught about teams.
	real := &ownerPush{}
	w.srv.ext.Push = real
	w.srv.sendPush(context.Background(), f.team, push.Payload{Title: "direct", Tag: "fan-direct"})
	if got := real.to("fan-direct", f.ana, f.ben, f.cal); len(got) != 0 {
		t.Errorf("sendPush to the team id reached members %v; it sends to its owner only", got)
	}
}

// TestATeamPushTitleNamesTheTeam: a team push's title starts "<team> · ",
// with the team's name a fixture value no constant equals; the prefix is
// put on BEFORE the title rule, so the rule runs over the whole title once
// (the markdown in the title is plained, the kept tmux target is kept
// verbatim, and the prefix survives). CONTROL: a personal push has no prefix.
func TestATeamPushTitleNamesTheTeam(t *testing.T) {
	w := newIsoWorld(t)
	f, rec := newFanTeam(t, w)
	n := RouterNotification{Type: "plugin", ID: "t", Title: "**Ana** is asking in _scratch_:1", TitleKeep: "_scratch_:1", Tag: "fan-title"}
	w.srv.goNotify(f.team, push.EventPlugin, n, nil)
	w.srv.goNotify(f.ana, push.EventPlugin, n, nil)
	w.srv.WaitForPushes()
	want := f.name + " · " + "Ana is asking in _scratch_:1" // the literal, never the constant under test
	got := rec.sent(f.ben, "fan-title", false)
	if len(got) != 1 || got[0].Title != want {
		t.Errorf("the team push's title to ben = %v; want %q", got, want)
	}
	if !strings.HasPrefix(want, "fanout-") {
		t.Fatal("fixture: the team's name must be the fixture value")
	}
	var personal []string
	for _, p := range rec.sent(f.ana, "fan-title", false) {
		personal = append(personal, p.Title)
	}
	// ana got two: the team's (prefixed) and her own (not).
	sort.Strings(personal)
	if wantP := []string{"Ana is asking in _scratch_:1", want}; strings.Join(personal, "|") != strings.Join(wantP, "|") {
		t.Errorf("ana's titles = %q; want %q (her personal push has no prefix)", personal, wantP)
	}
}

// TestAnAnsweredTeamAskIsWithdrawnFromItsMembers: a team ask, once settled,
// is withdrawn from each of the team's operators and managers — red at base,
// where the withdrawal named the team, which has no devices — and from no
// one else.
func TestAnAnsweredTeamAskIsWithdrawnFromItsMembers(t *testing.T) {
	w := newIsoWorld(t)
	f, rec := newFanTeam(t, w)
	id, err := (officerAsker{w.srv}).Raise(context.Background(), officers.Officer{ID: "mk:a", Owner: f.team}, builtin.AskAbout{}, "Withdraw me?", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	w.srv.WaitForPushes()
	a, err := w.srv.ext.Attention.Answer(w.personAt(t, f.ben, authz.ViaSession), id, "yes")
	if err != nil {
		t.Fatal(err)
	}
	w.srv.askSettled(a)
	w.srv.WaitForPushes()
	tag := attentionTag(attentionAskKey(id))
	// The switched-off operator is withdrawn from too (G1: current operators
	// and managers); the viewer, ex-member, disabled and outsider are not.
	if got := rec.withdrawnFrom(tag, f.everyone()...); !sameIDs(got, ids(f.ana, f.ben, f.off)) {
		t.Errorf("the answered team ask was withdrawn from %v; want its operators and managers %v", got, ids(f.ana, f.ben, f.off))
	}
}

// TestADecidedTeamDraftIsWithdrawnFromItsMembers: a team's draft decided by
// its manager is withdrawn from every operator and manager, not only from
// the manager who decided it (red at base: draftSettled named the decider).
// INVARIANT beside it: a decided outreach batch was already withdrawn from
// the members at base (its own member loop), and still is.
func TestADecidedTeamDraftIsWithdrawnFromItsMembers(t *testing.T) {
	w := newIsoWorld(t)
	f, rec := newFanTeam(t, w)
	dm, follow, batch := fanDrafts(t, w, f.team)
	mgr := w.personAt(t, f.ana, authz.ViaSession)
	if _, refused := w.srv.decideDraftAs(mgr, dm, plugins.DraftDecision{Approve: false, Via: plugins.DraftDecisionSource{Via: plugins.DraftViaBrowser}}); refused != nil {
		t.Fatalf("the manager rejecting the team's draft: %+v", refused)
	}
	w.srv.WaitForPushes()
	if got := rec.withdrawnFrom(draftTag(dm), f.everyone()...); !sameIDs(got, ids(f.ana, f.ben, f.off)) {
		t.Errorf("the decided team draft was withdrawn from %v; want every operator and manager %v", got, ids(f.ana, f.ben, f.off))
	}
	// Nothing ticked: the batch is decided, every entry "not selected".
	if _, refused := w.srv.decideBatchAs(mgr, batch, plugins.BatchDecision{BatchSHA256: plugins.BatchSHA256([]int64{follow})}); refused != nil {
		t.Fatalf("the manager deciding the team's batch: %+v", refused)
	}
	w.srv.WaitForPushes()
	if got := rec.withdrawnFrom(batchTag(batch), f.ben); !sameIDs(got, ids(f.ben)) {
		t.Errorf("invariant: the decided team batch was not withdrawn from operator %d (%v)", f.ben, got)
	}
}

// TestLeavingATeamWithdrawsItsOpenItems (G1's sweep): a person removed from
// the team (by its manager, and by an admin), downgraded to viewer, or
// disabled is sent a withdrawal for each of the team's open asks, pending
// drafts and pending outreach batches — once the change commits. CONTROL:
// a member who stays (ana) gets no withdrawal, and nor does an outsider.
// Red at base: nothing withdrew a team's items from anyone.
func TestLeavingATeamWithdrawsItsOpenItems(t *testing.T) {
	w := newIsoWorld(t)
	f, rec := newFanTeam(t, w)
	ask, err := (officerAsker{w.srv}).Raise(context.Background(), officers.Officer{ID: "mk:a", Owner: f.team}, builtin.AskAbout{}, "Still open?", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	dm, _, batch := fanDrafts(t, w, f.team)
	w.srv.WaitForPushes()
	tags := []string{attentionTag(attentionAskKey(ask)), draftTag(dm), batchTag(batch)}
	us := users.NewPG(w.pool)
	admin := users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}
	team := strconv.FormatInt(f.team, 10)
	// Four more operators, one per way of leaving.
	var leavers []int64
	for _, n := range []string{"fan-rm", "fan-down", "fan-admin-down", "fan-disable"} {
		var id int64
		if err := w.pool.QueryRow(w.sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, n+"-"+w.suffix).Scan(&id); err != nil {
			t.Fatal(err)
		}
		uid := id
		t.Cleanup(func() {
			bg := authz.System(context.Background())
			_, _ = w.pool.Exec(bg, `DELETE FROM team_members WHERE user_id = $1`, uid)
			_, _ = w.pool.Exec(bg, `DELETE FROM audit_log WHERE user_id = $1`, uid)
			_, _ = w.pool.Exec(bg, `DELETE FROM users WHERE id = $1`, uid)
		})
		if err := us.SetTeamMember(w.sys, admin, f.team, id, authz.TeamOperator); err != nil {
			t.Fatal(err)
		}
		leavers = append(leavers, id)
	}
	rm, down, adminDown, dis := leavers[0], leavers[1], leavers[2], leavers[3]
	rec.reset()
	expect := func(what string, who int64) {
		t.Helper()
		w.srv.WaitForPushes()
		for _, tag := range tags {
			if got := rec.withdrawnFrom(tag, append(f.everyone(), leavers...)...); !sameIDs(got, ids(who)) {
				t.Errorf("%s: %s was withdrawn from %v; want only %d", what, tag, got, who)
			}
		}
		rec.reset()
	}
	if r := w.do(w.h, "POST", "/teams/"+team+"/members/"+strconv.FormatInt(rm, 10)+"/remove", "", cred{sessionUser: f.ana}); r.Code != http.StatusSeeOther {
		t.Fatalf("the manager removing an operator: %d %s", r.Code, r.Body.String())
	}
	expect("removed by the manager", rm)
	if r := w.do(w.h, "POST", "/teams/"+team+"/members", url.Values{"user": {strconv.FormatInt(down, 10)}, "role": {"viewer"}}.Encode(), cred{sessionUser: f.ana}); r.Code != http.StatusSeeOther {
		t.Fatalf("the manager downgrading an operator: %d %s", r.Code, r.Body.String())
	}
	expect("downgraded to viewer by the manager", down)
	if r := w.do(w.h, "POST", "/admin/teams/"+team+"/members", url.Values{"user": {strconv.FormatInt(adminDown, 10)}, "role": {"viewer"}}.Encode(), cred{sessionUser: w.op.uid}); r.Code != http.StatusSeeOther {
		t.Fatalf("an admin downgrading an operator: %d %s", r.Code, r.Body.String())
	}
	expect("downgraded to viewer by an admin", adminDown)
	if err := w.srv.DisableUserAs(context.Background(), admin, dis, true); err != nil {
		t.Fatal(err)
	}
	expect("disabled", dis)
	// CONTROL: an operator re-roled to MANAGER keeps their notifications.
	if r := w.do(w.h, "POST", "/teams/"+team+"/members", url.Values{"user": {strconv.FormatInt(f.ben, 10)}, "role": {"manager"}}.Encode(), cred{sessionUser: f.ana}); r.Code != http.StatusSeeOther {
		t.Fatalf("the manager promoting an operator: %d", r.Code)
	}
	w.srv.WaitForPushes()
	for _, tag := range tags {
		if got := rec.withdrawnFrom(tag, f.everyone()...); len(got) != 0 {
			t.Errorf("promoting ben withdrew %s from %v; want no one", tag, got)
		}
	}
}

// TestTheNotifySwitchIsTheMembersOwn (G6): a member flips only their own row
// (ana's is unchanged when ben turns his off); a person who is not a member,
// and a team id that names no team, get the same 404; Account -> Teams lists
// the member's teams with their role and the switch's state.
func TestTheNotifySwitchIsTheMembersOwn(t *testing.T) {
	w := newIsoWorld(t)
	f, _ := newFanTeam(t, w)
	notify := func(uid int64) (on, member bool) {
		err := w.pool.QueryRow(w.sys, `SELECT notify FROM team_members WHERE team_id = $1 AND user_id = $2`, f.team, uid).Scan(&on)
		return on, err == nil
	}
	path := accountTeamsPath + "/" + strconv.FormatInt(f.team, 10) + "/notify"
	if r := w.do(w.h, "POST", path, "on=0", cred{sessionUser: f.ben}); r.Code != http.StatusSeeOther {
		t.Fatalf("ben turning his switch off: %d %s", r.Code, r.Body.String())
	}
	if on, _ := notify(f.ben); on {
		t.Error("ben's switch is still on")
	}
	if on, _ := notify(f.ana); !on {
		t.Error("ben's write turned ana's switch off")
	}
	if r := w.do(w.h, "POST", path, "on=0", cred{sessionUser: f.dee}); r.Code != http.StatusNotFound {
		t.Errorf("a non-member's write: %d; want 404", r.Code)
	}
	if _, member := notify(f.dee); member {
		t.Error("a non-member's write made them a member")
	}
	if r := w.do(w.h, "POST", accountTeamsPath+"/999999999/notify", "on=0", cred{sessionUser: f.ben}); r.Code != http.StatusNotFound {
		t.Errorf("a write for a team that does not exist: %d; want 404", r.Code)
	}
	page := w.do(w.h, "GET", accountTeamsPath, "", cred{sessionUser: f.ben})
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `data-team-id="`+strconv.FormatInt(f.team, 10)+`" data-team-role="operator" data-team-notify="off"`) ||
		!strings.Contains(page.Body.String(), f.name) {
		t.Errorf("ben's Account -> Teams: %d, missing his team, role or switch:\n%s", page.Code, page.Body.String())
	}
	if r := w.do(w.h, "POST", path, "on=1", cred{sessionUser: f.ben}); r.Code != http.StatusSeeOther {
		t.Fatalf("ben turning it back on: %d", r.Code)
	}
	if on, _ := notify(f.ben); !on {
		t.Error("ben's switch did not turn back on")
	}
}

// TestACommentOnATeamTaskNotifiesItsOtherOperators (phase 4.3's limitation
// (c), carried into 2b.1): ben's comment on a team task, through the browser
// session, pushes the team's other operators and managers (ana) — not ben,
// who wrote it, nor the viewer, nor an outsider — under the team's prefix,
// with ben's name in the body. Red at base: the session route pushed nobody.
// CONTROL: ben's comment on his OWN task pushes no one, as before.
func TestACommentOnATeamTaskNotifiesItsOtherOperators(t *testing.T) {
	w := newIsoWorld(t)
	f, rec := newFanTeam(t, w)
	var task, own int64
	if err := w.pool.QueryRow(w.sys, `INSERT INTO notes (owner_id, title) VALUES ($1, 'team plan') RETURNING id`, f.team).Scan(&task); err != nil {
		t.Fatal(err)
	}
	if err := w.pool.QueryRow(w.sys, `INSERT INTO notes (owner_id, title) VALUES ($1, 'my plan') RETURNING id`, f.ben).Scan(&own); err != nil {
		t.Fatal(err)
	}
	tag := "task-" + strconv.FormatInt(task, 10) + "-comment"
	if r := w.do(w.h, "POST", "/tasks/"+strconv.FormatInt(task, 10)+"/comments", "body=looks+good", cred{sessionUser: f.ben}); r.Code != http.StatusOK {
		t.Fatalf("ben commenting on the team task: %d %s", r.Code, r.Body.String())
	}
	w.srv.WaitForPushes()
	if got := rec.to(tag, append(f.everyone(), f.team)...); !sameIDs(got, ids(f.ana)) {
		t.Errorf("ben's comment on the team task pushed %v; want only the other operators and managers %v", got, ids(f.ana))
	}
	if got := rec.sent(f.ana, tag, false); len(got) == 1 &&
		(!strings.HasPrefix(got[0].Title, f.name+" · ") || !strings.Contains(got[0].Body, "fan-ben-"+w.suffix+": looks good")) {
		t.Errorf("ana's push = %q / %q; want the team's prefix and ben's name before his words", got[0].Title, got[0].Body)
	}
	// The MACHINE route (POST /api/tasks/{id}/comments) carries no team: its
	// credentials (the hook token, a personal token) never reach a team's
	// task, so it neither comments there nor pushes anyone (docs/push.md).
	rec.reset()
	if r := w.do(w.h, "POST", "/api/tasks/"+strconv.FormatInt(task, 10)+"/comments", `{"body":"machine words"}`, cred{hook: true}); r.Code != http.StatusNotFound {
		t.Errorf("the hook token commenting on a team task: %d %s; want 404 (no machine door carries a team)", r.Code, r.Body.String())
	}
	w.srv.WaitForPushes()
	if got := rec.to(tag, append(f.everyone(), f.team, authz.OperatorUserID)...); len(got) != 0 {
		t.Errorf("a machine-route comment attempt on a team task pushed %v; want no one", got)
	}
	rec.reset()
	ownTag := "task-" + strconv.FormatInt(own, 10) + "-comment"
	if r := w.do(w.h, "POST", "/tasks/"+strconv.FormatInt(own, 10)+"/comments", "body=note+to+self", cred{sessionUser: f.ben}); r.Code != http.StatusOK {
		t.Fatalf("ben commenting on his own task: %d", r.Code)
	}
	w.srv.WaitForPushes()
	if got := rec.to(ownTag, f.everyone()...); len(got) != 0 {
		t.Errorf("ben's comment on his own task pushed %v; want no one", got)
	}
}

// refreshedFor is which of people's open pages received an attention.changed
// event naming key, within the wait. It drains each one's stream.
func refreshedFor(streams map[int64]<-chan sse.Event, key string, people ...int64) []int64 {
	var out []int64
	for _, id := range people {
		deadline := time.After(300 * time.Millisecond)
		got := false
	drain:
		for {
			select {
			case ev := <-streams[id]:
				if ev.Name == EventAttentionChanged && strings.Contains(ev.Data, `"`+key+`"`) {
					got = true
				}
			case <-deadline:
				break drain
			}
		}
		if got {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// TestATeamItemRefreshesItsMembersPages (#683 round 0, Q1, operator
// decision): when a TEAM owns an item, attentionChanged also refreshes the
// open Attention and ask pages of the team's current operators and managers
// — the operator whose notify switch is off included (a page refresh is not
// a push) — and of no viewer, ex-member, disabled operator or outsider. Both
// of #671's refresh points (an outreach batch arriving, and decided) are
// covered, and so are a team ask's and a team draft's. Red at bfcf934d,
// where the refresh named the team, which no page subscribes to.
func TestATeamItemRefreshesItsMembersPages(t *testing.T) {
	w := newIsoWorld(t)
	f, _ := newFanTeam(t, w)
	streams := map[int64]<-chan sse.Event{}
	for _, id := range f.everyone() {
		sub, ch := w.bus.Subscribe(id)
		streams[id] = ch
		t.Cleanup(func() { w.bus.Unsubscribe(sub) })
	}
	want := ids(f.ana, f.ben, f.off)
	check := func(what, key string) {
		t.Helper()
		w.srv.WaitForPushes()
		if got := refreshedFor(streams, key, f.everyone()...); !sameIDs(got, want) {
			t.Errorf("%s refreshed the pages of %v; want the team's operators and managers %v (never viewer %d, ex-member %d, disabled %d, outsider %d)",
				what, got, want, f.cal, f.gone, f.dis, f.dee)
		}
	}
	dm, follow, batch := fanDrafts(t, w, f.team)
	officer := builtinOfficerID
	pid, kind := int64(1), plugins.TargetFollow
	d, err := plugins.NewPG(w.pool).Draft(w.sys, follow)
	if err != nil {
		t.Fatal(err)
	}
	w.srv.notifyDraftAwaitingNoting(context.Background(), f.team, plugins.Draft{ID: follow, OwnerID: f.team, Officer: &officer, PluginID: &pid,
		TargetKind: &kind, CreatedAt: d.CreatedAt}, plugins.Message{}, "")
	check("an outreach batch arriving", batchKey(batch))
	w.srv.draftFiled(f.team, plugins.Draft{ID: dm, OwnerID: f.team, ExpiresAt: time.Now().Add(time.Hour)})
	check("a team draft arriving", draftKey(dm))
	ask, err := (officerAsker{w.srv}).Raise(context.Background(), officers.Officer{ID: "mk:a", Owner: f.team}, builtin.AskAbout{}, "Refresh?", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	check("a team ask arriving", attentionAskKey(ask))

	mgr := w.personAt(t, f.ana, authz.ViaSession)
	if _, refused := w.srv.decideBatchAs(mgr, batch, plugins.BatchDecision{BatchSHA256: plugins.BatchSHA256([]int64{follow})}); refused != nil {
		t.Fatalf("the manager deciding the team's batch: %+v", refused)
	}
	check("an outreach batch decided", batchKey(batch))
	if _, refused := w.srv.decideDraftAs(mgr, dm, plugins.DraftDecision{Approve: false, Via: plugins.DraftDecisionSource{Via: plugins.DraftViaBrowser}}); refused != nil {
		t.Fatalf("the manager rejecting the team's draft: %+v", refused)
	}
	check("a team draft decided", draftKey(dm))
	a, err := w.srv.ext.Attention.Answer(w.personAt(t, f.ben, authz.ViaSession), ask, "yes")
	if err != nil {
		t.Fatal(err)
	}
	w.srv.askSettled(a)
	check("a team ask answered", attentionAskKey(ask))

	// CONTROL: a person's item refreshes their own page alone.
	w.srv.attentionChanged(f.ben, "fan-own-key")
	if got := refreshedFor(streams, "fan-own-key", f.everyone()...); !sameIDs(got, ids(f.ben)) {
		t.Errorf("a personal item refreshed the pages of %v; want only its owner %d", got, f.ben)
	}
}
