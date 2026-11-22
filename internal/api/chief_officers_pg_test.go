package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/plugins"
)

// THE CHIEF'S CONTROL OF ITS OWNER'S OFFICERS (officers UX PR 4/4;
// chief_officers.go), through the real server, the real built-in, plugin and
// users stores: DB-declared officers (declRig) for the gate, the controls,
// the schedules and their audit; the outbound world (an env officer with a
// real instance, a bound plugin and drafts) for the chat, the confirm card
// and the no-approval rule.
//
// The refusal words, tool names and audit actions are pinned here as
// literals, never derived from the code under test.
const (
	tNone         = "there is no officer with that id that your owner can see"
	tManager      = "only a manager of its team changes, runs, messages or asks approval for a team's officer"
	tTeamChief    = "a team's own chief does not control officers"
	tNeverDecides = "Your chief never decides an officer's drafts or outreach: it can only put a confirm card in your chat, and your own click decides it."
	tTeamOff      = "team:ops"
	tOtherOwn     = "other:own"
	tChiefName    = "chief-fixture"
	tChiefBadge   = `data-timeline-entry="chief"`
)

// chiefOf is a chief agent row owned by uid, as dispatchAgentTool is handed one.
func chiefOf(uid int64) agents.Agent {
	return agents.Agent{ID: 880000 + uid, Name: tChiefName, OwnerID: uid}
}

// chiefCall runs chief officer tool name for uid's chief, under uid's own
// principal on door via (principalFor: the person's real memberships), as
// dispatchAgentTool does once it has re-checked the chief.
func chiefCall(s *Server, uid int64, via authz.Via, a agents.Agent, name, args string) string {
	ctx := context.Background()
	ctx = withChiefCall(authz.WithPrincipal(ctx, s.principalFor(ctx, uid, via)))
	return s.dispatchChiefOfficerTool(ctx, a, name, args)
}

// chiefAuditRows is every officer.chief.* row on object, oldest first:
// "<action>|<changed>|<team_id>|<user_id>".
func chiefAuditRows(t *testing.T, f *connFixture, object string) []string {
	t.Helper()
	rows, err := f.pool.Query(authz.System(context.Background()), `SELECT action, coalesce(detail->>'changed', ''), coalesce(team_id, 0), coalesce(user_id, 0)
		FROM audit_log WHERE object = $1 AND action LIKE 'officer.chief.%' ORDER BY id`, object)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a, c string
		var team, user int64
		if err := rows.Scan(&a, &c, &team, &user); err != nil {
			t.Fatal(err)
		}
		out = append(out, a+"|"+c+"|"+strconv.FormatInt(team, 10)+"|"+strconv.FormatInt(user, 10))
	}
	return out
}

func wipeChiefAudit(t *testing.T) {
	dbtest.OwnerExec(t, dbtest.DSN(t), `DELETE FROM audit_log WHERE action LIKE 'officer.chief.%'`)
}

// chiefRig is a declRig with: user 1's DB officer declID; the team declTeam
// (user 1 its MANAGER, declOther its VIEWER) and its DB officer tTeamOff;
// declOther's own DB officer tOtherOwn.
func newChiefRig(t *testing.T) *declRig {
	t.Helper()
	r := newDeclRig(t, declRigOpts{pods: true})
	wipeChiefAudit(t)
	t.Cleanup(func() {
		wipeChiefAudit(t)
		dbtest.OwnerExec(t, dbtest.DSN(t), `DELETE FROM team_members WHERE team_id = 987302`)
	})
	if rec := r.hook(t, http.MethodPost, "/api/officers/declared", declPodBody); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	sys := authz.System(context.Background())
	for _, q := range []string{
		`INSERT INTO team_members (team_id, user_id, role, added_by) VALUES (987302, 1, 'manager', 1), (987302, 987301, 'viewer', 1)`,
		`INSERT INTO officer_types (owner_id, key, name) VALUES (987302, 'team-ops-chief', 'Ops'), (987301, 'other-own-chief', 'Own')`,
		`INSERT INTO officer_defs (owner_id, officer, type_key, runtime, instance_slug) VALUES (987302, '` + tTeamOff + `', 'team-ops-chief', 'pod', 'team-ops-chief'),
			(987301, '` + tOtherOwn + `', 'other-own-chief', 'pod', 'other-own-chief')`,
	} {
		if _, err := r.f.pool.Exec(sys, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.s.reloadDeclaredOfficers(context.Background()); err != nil {
		t.Fatal(err)
	}
	return r
}

// paused is owner's DB officer id's switch, read from the store.
func (r *declRig) paused(t *testing.T, owner int64, id string) bool {
	t.Helper()
	row, err := r.store.GetDeclared(authz.System(context.Background()), owner, id)
	if err != nil {
		t.Fatal(err)
	}
	return !row.Enabled
}

func errOf(out string) string {
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal([]byte(out), &e)
	return e.Error
}

// TestTheChiefActsOnlyOnOfficersItsOwnerOwns: the gate. User 1's chief lists
// and reads its own officer and its team's (it manages the team), and acts on
// both; it never sees, reads or acts on declOther's own officer, by id alone
// or naming its owner. declOther's chief (a VIEWER of the team) reads the
// team's officer and is refused every act on it. A chief agent that is not
// the principal's, and a team principal, are refused. CONTROLS: each refused
// officer is acted on by a chief allowed to, so the refusals are the gate,
// not a broken fixture.
func TestTheChiefActsOnlyOnOfficersItsOwnerOwns(t *testing.T) {
	r := newChiefRig(t)
	mine, viewer := chiefOf(1), chiefOf(declOther)
	team := strconv.FormatInt(declTeam, 10)
	other := strconv.FormatInt(declOther, 10)

	// The list: its own and the team's, can_change by role.
	var list struct {
		Officers []struct {
			Officer   string `json:"officer"`
			OwnerID   int64  `json:"owner_id"`
			CanChange bool   `json:"can_change"`
		} `json:"officers"`
	}
	if err := json.Unmarshal([]byte(chiefCall(r.s, 1, authz.ViaAgent, mine, chiefOfficersListTool, `{}`)), &list); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, o := range list.Officers {
		got[o.Officer+"@"+strconv.FormatInt(o.OwnerID, 10)] = o.CanChange
	}
	if len(got) != 2 || !got[declID+"@1"] || !got[tTeamOff+"@"+team] {
		t.Fatalf("user 1's chief lists %v; want exactly its own %s and the team's %s, both changeable", got, declID, tTeamOff)
	}
	if err := json.Unmarshal([]byte(chiefCall(r.s, declOther, authz.ViaAgent, viewer, chiefOfficersListTool, `{}`)), &list); err != nil {
		t.Fatal(err)
	}
	got = map[string]bool{}
	for _, o := range list.Officers {
		got[o.Officer+"@"+strconv.FormatInt(o.OwnerID, 10)] = o.CanChange
	}
	if c, ok := got[tTeamOff+"@"+team]; len(got) != 2 || !ok || c || !got[tOtherOwn+"@"+other] {
		t.Fatalf("the viewer's chief lists %v; want its own (changeable) and the team's (read-only)", got)
	}

	// Another person's officer: refused with one answer, read or act, by id
	// or naming its owner, on every door the chief turns through.
	acts := []struct{ tool, args string }{
		{chiefOfficerReadTool, `{"officer":"` + tOtherOwn + `"%s}`},
		{chiefOfficerControlTool, `{"officer":"` + tOtherOwn + `","action":"pause"%s}`},
		{chiefOfficerScheduleTool, `{"officer":"` + tOtherOwn + `","action":"put","cron":"0 9 * * *"%s}`},
		{chiefOfficerMessageTool, `{"officer":"` + tOtherOwn + `","message":"hi","wait_seconds":0%s}`},
		{chiefOfficerCardTool, `{"officer":"` + tOtherOwn + `","draft_id":1%s}`},
	}
	for _, via := range []authz.Via{authz.ViaAgent, authz.ViaAgentMessage, authz.ViaKickoff, authz.ViaSession} {
		for _, act := range acts {
			for _, owner := range []string{"", `,"owner_id":` + other} {
				args := strings.Replace(act.args, "%s", owner, 1)
				if out := chiefCall(r.s, 1, via, mine, act.tool, args); !strings.HasPrefix(errOf(out), tNone) {
					t.Errorf("user 1's chief via %s: %s %s: %s; want the not-yours refusal", via, act.tool, args, out)
				}
			}
		}
	}
	if r.paused(t, declOther, tOtherOwn) || len(chiefAuditRows(t, r.f, "officer_def:"+tOtherOwn)) != 0 {
		t.Fatalf("a refused chief changed or audited %s", tOtherOwn)
	}
	// CONTROL: its owner's chief pauses it.
	if out := chiefCall(r.s, declOther, authz.ViaAgent, viewer, chiefOfficerControlTool, `{"officer":"`+tOtherOwn+`","action":"pause"}`); isToolErr(out) || !r.paused(t, declOther, tOtherOwn) {
		t.Fatalf("control: declOther's chief pausing its own officer: %s", out)
	}

	// The team's officer: the viewer's chief reads it, and is refused every act.
	if out := chiefCall(r.s, declOther, authz.ViaAgent, viewer, chiefOfficerReadTool, `{"officer":"`+tTeamOff+`","owner_id":`+team+`,"section":"status"}`); isToolErr(out) {
		t.Fatalf("a viewer's chief reading the team's officer: %s", out)
	}
	for _, act := range []struct{ tool, args string }{
		{chiefOfficerControlTool, `{"officer":"` + tTeamOff + `","owner_id":` + team + `,"action":"pause"}`},
		{chiefOfficerControlTool, `{"officer":"` + tTeamOff + `","owner_id":` + team + `,"action":"run_now"}`},
		{chiefOfficerScheduleTool, `{"officer":"` + tTeamOff + `","owner_id":` + team + `,"action":"put","cron":"0 9 * * *"}`},
		{chiefOfficerMessageTool, `{"officer":"` + tTeamOff + `","owner_id":` + team + `,"message":"hi","wait_seconds":0}`},
		{chiefOfficerCardTool, `{"officer":"` + tTeamOff + `","owner_id":` + team + `,"draft_id":1}`},
	} {
		if out := chiefCall(r.s, declOther, authz.ViaAgent, viewer, act.tool, act.args); !strings.HasPrefix(errOf(out), tManager) {
			t.Errorf("the viewer's chief: %s %s: %s; want the manager-only refusal", act.tool, act.args, out)
		}
	}
	if r.paused(t, declTeam, tTeamOff) || len(chiefAuditRows(t, r.f, "officer_def:"+tTeamOff)) != 0 {
		t.Fatalf("a viewer's chief changed or audited the team's officer")
	}
	// CONTROL: the manager's chief pauses it, in the TEAM's audit.
	if out := chiefCall(r.s, 1, authz.ViaAgent, mine, chiefOfficerControlTool, `{"officer":"`+tTeamOff+`","owner_id":`+team+`,"action":"pause"}`); isToolErr(out) || !r.paused(t, declTeam, tTeamOff) {
		t.Fatalf("control: the manager's chief pausing the team's officer: %s", out)
	}
	if rows := chiefAuditRows(t, r.f, "officer_def:"+tTeamOff); strings.Join(rows, ",") != "officer.chief.pause|true|"+team+"|1" {
		t.Fatalf("the team officer's audit = %v; want one pause in the team's audit, by user 1", rows)
	}

	// A chief agent that is not the principal's own, and a team's chief.
	if out := chiefCall(r.s, 1, authz.ViaAgent, chiefOf(declOther), chiefOfficerReadTool, `{"officer":"`+declID+`"}`); !strings.HasPrefix(errOf(out), tNone) {
		t.Errorf("another person's chief agent under user 1's principal: %s", out)
	}
	if out := chiefCall(r.s, declTeam, authz.ViaAgent, chiefOf(declTeam), chiefOfficerReadTool, `{"officer":"`+tTeamOff+`"}`); !strings.HasPrefix(errOf(out), tTeamChief) {
		t.Errorf("a team's chief: %s", out)
	}
	// CONTROL: user 1's own chief reads its own officer.
	if out := chiefCall(r.s, 1, authz.ViaAgent, mine, chiefOfficerReadTool, `{"officer":"`+declID+`"}`); isToolErr(out) {
		t.Errorf("control: user 1's chief reading %s: %s", declID, out)
	}
}

// TestChiefControlsAreIdempotentReversibleAndAudited: pause, pause again
// (nothing changes, said so), resume (the undo), run now, run now again (the
// waiting run, none added) — each one audit row in user 1's own audit, and
// each in the officer's What it did. An "approve" action is refused.
func TestChiefControlsAreIdempotentReversibleAndAudited(t *testing.T) {
	r := newChiefRig(t)
	a := chiefOf(1)
	call := func(action string) map[string]any {
		t.Helper()
		out := chiefCall(r.s, 1, authz.ViaAgent, a, chiefOfficerControlTool, `{"officer":"`+declID+`","action":"`+action+`"}`)
		var m map[string]any
		if isToolErr(out) || json.Unmarshal([]byte(out), &m) != nil {
			t.Fatalf("%s: %s", action, out)
		}
		return m
	}
	if m := call("pause"); m["changed"] != true || !r.paused(t, 1, declID) || m["undo"] != "chief_officer_control action resume" {
		t.Fatalf("pause: %v (paused %v)", m, r.paused(t, 1, declID))
	}
	if m := call("pause"); m["changed"] != false || !r.paused(t, 1, declID) {
		t.Fatalf("pause again: %v; want nothing changed", m)
	}
	if m := call("resume"); m["changed"] != true || r.paused(t, 1, declID) {
		t.Fatalf("resume: %v (paused %v)", m, r.paused(t, 1, declID))
	}
	first := call("run_now")
	second := call("run_now")
	if first["changed"] != true || second["changed"] != false || first["item_id"] != second["item_id"] {
		t.Fatalf("run now twice: %v then %v; want one item, the second call adding none", first, second)
	}
	var open int
	if err := r.f.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM officer_items WHERE owner_id = 1 AND officer = $1 AND kind = 'manual.run'`, declID).Scan(&open); err != nil || open != 1 {
		t.Fatalf("run-now items: %d %v; want 1", open, err)
	}
	if out := chiefCall(r.s, 1, authz.ViaAgent, a, chiefOfficerControlTool, `{"officer":"`+declID+`","action":"approve"}`); !isToolErr(out) {
		t.Fatalf("an approve action: %s; want a refusal", out)
	}
	want := []string{"officer.chief.pause|true|0|1", "officer.chief.pause|false|0|1", "officer.chief.resume|true|0|1", "officer.chief.run|true|0|1", "officer.chief.run|false|0|1"}
	if rows := chiefAuditRows(t, r.f, "officer_def:"+declID); strings.Join(rows, ",") != strings.Join(want, ",") {
		t.Fatalf("the audit = %v; want %v", rows, want)
	}
	page := r.officerPage(t, declID)
	for _, words := range []string{"Your chief paused it", "Your chief asked to pause it (it already was)", "Your chief resumed it",
		"Your chief asked it to run now", "Your chief asked it to run now (a run was already waiting)"} {
		if !strings.Contains(page, ">"+words+"</span>") {
			t.Errorf("What it did lacks %q", words)
		}
	}
	if n := strings.Count(page, tChiefBadge); n != 5 {
		t.Errorf("What it did draws %d chief entries; want 5", n)
	}
}

// officerPage is officer id's page (user 1's own), as user 1's browser reads it.
func (r *declRig) officerPage(t *testing.T, id string) string {
	t.Helper()
	rec := r.page(t, 1, http.MethodGet, "/officers/"+id, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("the page of %s: %d %.400s", id, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// TestChiefSchedulesPutChangeAndDeleteWithTheirUndo: put a new schedule (its
// undo is a delete), the same cron again (nothing changes), a new cron (the
// previous one returned, kept otherwise), delete (what it was returned),
// delete again (nothing). A cron muster cannot read is refused. Each act one
// audit row and a What it did entry.
func TestChiefSchedulesPutChangeAndDeleteWithTheirUndo(t *testing.T) {
	r := newChiefRig(t)
	a := chiefOf(1)
	call := func(args string) map[string]any {
		t.Helper()
		out := chiefCall(r.s, 1, authz.ViaAgent, a, chiefOfficerScheduleTool, `{"officer":"`+declID+`",`+args+`}`)
		var m map[string]any
		if isToolErr(out) || json.Unmarshal([]byte(out), &m) != nil {
			t.Fatalf("%s: %s", args, out)
		}
		return m
	}
	cronOf := func() string {
		t.Helper()
		var c string
		if err := r.f.pool.QueryRow(authz.System(context.Background()), `SELECT coalesce((SELECT cron FROM officer_schedules WHERE owner_id = 1 AND officer = $1 AND name = 'weekly'), '')`, declID).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	if m := call(`"action":"put","name":"weekly","cron":"0 9 * * 1"`); m["changed"] != true || m["undo"] != `chief_officer_schedule action delete name "weekly"` || cronOf() != "0 9 * * 1" {
		t.Fatalf("put: %v (cron %q)", m, cronOf())
	}
	if m := call(`"action":"put","name":"weekly","cron":"0 9 * * 1"`); m["changed"] != false {
		t.Fatalf("put the same cron: %v", m)
	}
	if m := call(`"action":"put","name":"weekly","cron":"30 10 * * 2"`); m["changed"] != true || m["previous_cron"] != "0 9 * * 1" || cronOf() != "30 10 * * 2" {
		t.Fatalf("change: %v (cron %q)", m, cronOf())
	}
	if out := chiefCall(r.s, 1, authz.ViaAgent, a, chiefOfficerScheduleTool, `{"officer":"`+declID+`","action":"put","name":"weekly","cron":"every monday"}`); !isToolErr(out) || cronOf() != "30 10 * * 2" {
		t.Fatalf("an unreadable cron: %s (cron %q)", out, cronOf())
	}
	m := call(`"action":"delete","name":"weekly"`)
	deleted, _ := m["deleted"].(map[string]any)
	if m["changed"] != true || deleted["cron"] != "30 10 * * 2" || cronOf() != "" || m["undo"] != `chief_officer_schedule action put name "weekly" cron "30 10 * * 2"` {
		t.Fatalf("delete: %v (cron %q)", m, cronOf())
	}
	if m := call(`"action":"delete","name":"weekly"`); m["changed"] != false {
		t.Fatalf("delete again: %v", m)
	}
	want := []string{"officer.chief.schedule.put|true|0|1", "officer.chief.schedule.put|false|0|1", "officer.chief.schedule.put|true|0|1",
		"officer.chief.schedule.delete|true|0|1", "officer.chief.schedule.delete|false|0|1"}
	if rows := chiefAuditRows(t, r.f, "officer_def:"+declID); strings.Join(rows, ",") != strings.Join(want, ",") {
		t.Fatalf("the audit = %v; want %v (an unreadable cron is no act)", rows, want)
	}
	page := r.officerPage(t, declID)
	for _, words := range []string{"Your chief added the schedule weekly: 0 9 * * 1 (UTC)", "Your chief changed its schedule weekly from 0 9 * * 1 to 30 10 * * 2 (UTC)",
		"Your chief deleted its schedule weekly (30 10 * * 2)", "Your chief asked to set its schedule weekly to 0 9 * * 1 (it already was)",
		"Your chief asked to delete its schedule weekly (it had none by that name)"} {
		if !strings.Contains(page, ">"+words+"</span>") {
			t.Errorf("What it did lacks %q", words)
		}
	}
}

// TestAChatPersonPrincipalAdmitsATeamManager: the officer door's rule for who
// may decide a team officer's work admits the team's MANAGER and refuses its
// viewer. 🔴 RED at the base (6af6d8357): it read the role from TeamScope(),
// which the officer door never carries, so it refused every manager — and
// chief_officer_confirm_card could never ask one. CONTROL: the owner itself.
func TestAChatPersonPrincipalAdmitsATeamManager(t *testing.T) {
	r := newChiefRig(t)
	o := officers.Officer{ID: tTeamOff, Owner: declTeam, Backend: officers.BackendBuiltin}
	if _, ok := r.s.chatPersonPrincipal(context.Background(), o, 1); !ok {
		t.Errorf("the team's manager (user 1) was refused")
	}
	if _, ok := r.s.chatPersonPrincipal(context.Background(), o, declOther); ok {
		t.Errorf("the team's viewer was admitted")
	}
	own := officers.Officer{ID: tOtherOwn, Owner: declOther, Backend: officers.BackendBuiltin}
	if _, ok := r.s.chatPersonPrincipal(context.Background(), own, declOther); !ok {
		t.Errorf("control: the owner of %s was refused", tOtherOwn)
	}
}

// TestTheChiefNeverApprovesAnOfficersWork: the chief's confirm card is only
// a card — recorded in user 1's own chat with the officer, the draft still
// pending, drawn on its page — and every decision door refuses a call a
// chief-only tool reached, under the owner's own SESSION principal (the
// principal a gateway chief's turn from its owner's browser runs under),
// deciding nothing. The mirrored sessions' decide tool is refused to the
// chief. CONTROL: the owner's own click on the card approves exactly it.
func TestTheChiefNeverApprovesAnOfficersWork(t *testing.T) {
	w := newOutboundWorld(t, true)
	wipeCards(t)
	wipeChiefAudit(t)
	t.Cleanup(func() { wipeCards(t); wipeChiefAudit(t) })
	a := chiefOf(1)
	id := w.readyDraft()
	d := w.draft(id)
	ids, _ := w.followsFor("mo")
	batch := plugins.BatchID(1, builtinOfficerID, w.draft(ids[0]).CreatedAt.UTC().Format("2006-01-02"))

	// Through the real dispatch, as user 1's real chief (its agent row
	// labelled chief): offered the six tools, which its owner's workers are
	// not.
	sess := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaSession})
	chief, err := w.srv.ext.Agents.Create(sess, agents.Agent{Name: "chief-officers-fixture", Namespace: "devpod-chief-officers-fixture",
		DisplayName: "chief", Status: agents.StatusRunning, HooksToken: "chief-officers-fixture-hooks"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = w.f.pool.Exec(authz.System(context.Background()), `DELETE FROM agents WHERE id = $1`, chief.ID) })
	if !w.srv.isChiefAgent(sess, chief) {
		t.Fatal("fixture: the agent labelled chief is not user 1's chief")
	}
	offered := func(ag agents.Agent) int {
		n := 0
		for _, d := range w.srv.AgentToolDefsFor(sess, ag) {
			if strings.HasPrefix(d.Name, "chief_officer") {
				n++
			}
		}
		return n
	}
	if offered(chief) != 6 || offered(w.agent) != 0 {
		t.Fatalf("chief tools offered: %d to the chief, %d to the officer's instance; want 6 and 0", offered(chief), offered(w.agent))
	}
	out := w.srv.dispatchAgentTool(sess, chief, chiefOfficerCardTool, `{"officer":"`+builtinOfficerID+`","draft_id":`+strconv.FormatInt(id, 10)+`}`)
	var got struct {
		CardID int64 `json:"card_id"`
	}
	if isToolErr(out) || json.Unmarshal([]byte(out), &got) != nil || got.CardID == 0 {
		t.Fatalf("the chief's confirm card: %s", out)
	}
	cs := w.cards()
	if len(cs) != 1 || cs[0].id != got.CardID || cs[0].owner != 1 || cs[0].draft != id || cs[0].decision != "" {
		t.Fatalf("the card: %+v; want one undecided card for draft %d in user 1's chat", cs, id)
	}
	if st := w.draft(id).Status; st != plugins.DraftPending {
		t.Fatalf("the chief's card decided the draft: %s", st)
	}
	if !strings.Contains(chatSection(w.officerPage()), `data-chat-card="`+strconv.FormatInt(got.CardID, 10)+`"`) {
		t.Fatalf("the chief's card is not in user 1's chat on the page")
	}
	// Another owner's draft is not this officer's.
	theirs := w.readyDraft()
	w.draftSQL(theirs, `owner_id = `+strconv.FormatInt(w.other, 10)+`, plugin_id = (SELECT id FROM plugins WHERE name = 'example-other')`)
	if out := chiefCall(w.srv, 1, authz.ViaSession, a, chiefOfficerCardTool, `{"officer":"`+builtinOfficerID+`","draft_id":`+strconv.FormatInt(theirs, 10)+`}`); !strings.Contains(errOf(out), "is not one of its drafts") {
		t.Errorf("a card for another owner's draft: %s", out)
	}

	// Every decision door, as a call a chief-only tool reached.
	for via, ctx := range map[authz.Via]context.Context{
		authz.ViaSession:      withChiefCall(authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaSession})),
		authz.ViaAgentMessage: withChiefCall(authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaAgentMessage})),
		authz.ViaKickoff:      withChiefCall(authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaKickoff})),
	} {
		if _, r := w.srv.decideDraftAs(ctx, id, plugins.DraftDecision{Approve: true, BodySHA256: plugins.DraftBodySHA256(d.Body), MediaSHA256: d.MediaSHA256(),
			Via: plugins.DraftDecisionSource{Via: plugins.DraftViaBrowser}}); r == nil || r.code != http.StatusForbidden || r.msg != tNeverDecides {
			t.Errorf("decideDraftAs as the chief via %s: %+v; want the chief refusal", via, r)
		}
		if _, r := w.srv.decideBatchAs(ctx, batch, plugins.BatchDecision{BatchSHA256: plugins.BatchSHA256(ids), Approve: ids,
			Via: plugins.DraftDecisionSource{Via: plugins.DraftViaBrowser}}); r == nil || r.code != http.StatusForbidden || r.msg != tNeverDecides {
			t.Errorf("decideBatchAs as the chief via %s: %+v; want the chief refusal", via, r)
		}
		if _, r := w.srv.decideChatCard(ctx, 1, builtinOfficerID, got.CardID, cs[0].sha, true); r == nil || r.code != http.StatusForbidden || r.msg != tNeverDecides {
			t.Errorf("decideChatCard as the chief via %s: %+v; want the chief refusal", via, r)
		}
	}
	if st := w.draft(id).Status; st != plugins.DraftPending {
		t.Fatalf("a chief call decided the draft: %s", st)
	}
	for _, x := range ids {
		if st := w.draft(x).Status; st != plugins.DraftPending {
			t.Fatalf("a chief call decided the batch: %d is %s", x, st)
		}
	}
	if c := w.cards(); c[0].decision != "" {
		t.Fatalf("a chief call decided the card: %+v", c[0])
	}
	// The mirrored sessions' decide tool, through the chief's own dispatch
	// (invariant guard: refused before any decision by its door's rule).
	if out := w.srv.dispatchAgentTool(sess, chief, pluginDraftDecideTool,
		`{"id":`+strconv.FormatInt(id, 10)+`,"decision":"approve","body_sha256":"`+plugins.DraftBodySHA256(d.Body)+`"}`); !isToolErr(out) {
		t.Errorf("plugin_draft_decide from the chief: %s", out)
	}
	if rows := chiefAuditRows(t, w.f, "officer_def:"+builtinOfficerID); strings.Join(rows, ",") != "officer.chief.confirm_card||0|1" {
		t.Errorf("the audit = %v; want the one card", rows)
	}
	if page := w.officerPage(); !strings.Contains(page, ">Your chief asked you to approve its draft "+strconv.FormatInt(id, 10)+" (a card in your chat with it)</span>") {
		t.Errorf("What it did lacks the chief's card")
	}
	// CONTROL: the owner's own click approves exactly the card.
	if rec := w.cardPost(got.CardID, "approve", url.Values{"card_sha256": {cs[0].sha}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("control: the owner's click: %d %.400s", rec.Code, rec.Body.String())
	}
	if st := w.draft(id).Status; st != plugins.DraftApproved {
		t.Fatalf("control: after the owner's click: %s", st)
	}
}

// TestAChiefMessageIsItsOwnersChatTurnCountedNeverHeld: the officer is past
// its daily budget (held); the chief's message is still sent — as a turn of
// user 1's OWN chat lane with it, marked as the chief's — the chief gets the
// reply, the page draws it as the chief's (never as "You"), the turn's usage
// is booked into officer_llm_usage, and the message is audited and in What
// it did. A worker (the officer's own instance) is refused the tool.
func TestAChiefMessageIsItsOwnersChatTurnCountedNeverHeld(t *testing.T) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	src := fixedUsage{buckets: []cctranscript.UsageBucket{{Session: "lane-1", Hour: today.Add(time.Hour), APICalls: 7,
		InputTokens: 300, OutputTokens: 40, Model: "claude-sonnet-5-5"}}}
	gw := &laneChat{reply: func() (string, error) { return "One draft waits on your owner.", nil }}
	w := newOutboundWorldWith(t, true, outboundFixture{officerExtra: `,"daily_budget_usd":2`, usage: src, gateway: gw})
	wipeCards(t)
	wipeChiefAudit(t)
	t.Cleanup(func() { wipeCards(t); wipeChiefAudit(t) })
	llmCleanup(t, w.f, builtinOfficerID)
	if _, err := w.f.pool.Exec(authz.System(w.ctx), `INSERT INTO officer_llm_usage (owner_id, officer, pool, instance, day, requests, cost_usd)
		VALUES (1, $1, 'judgement', 9, $2, 1, 5)`, builtinOfficerID, today); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.officerPage(), "data-run-held") {
		t.Fatal("fixture: the officer is not drawn held")
	}
	const msg = "What is waiting on my owner?"
	out := chiefCall(w.srv, 1, authz.ViaAgentMessage, chiefOf(1), chiefOfficerMessageTool, `{"officer":"`+builtinOfficerID+`","message":"`+msg+`"}`)
	var got struct {
		Status, Reply string
	}
	if isToolErr(out) || json.Unmarshal([]byte(out), &got) != nil || got.Status != "answered" || got.Reply != "One draft waits on your owner." {
		t.Fatalf("the chief's message: %s", out)
	}
	w.srv.WaitForOfficerChats()
	sent := gw.sent()
	if len(sent) != 1 || sent[0].sessionKey != tOperatorLane || !strings.HasSuffix(sent[0].text, msg) ||
		!strings.Contains(sent[0].text, "[muster] Your owner's chief agent writes this for your owner.") {
		t.Fatalf("the turn: %+v; want one turn of user 1's chat (%s), marked as the chief's", sent, tOperatorLane)
	}
	chat := chatSection(w.officerPage())
	for _, want := range []string{`data-chat-message="chief"`, ">Your chief</span>", ">" + msg + "</p>", ">One draft waits on your owner.</p>"} {
		if !strings.Contains(chat, want) {
			t.Errorf("the chat lacks %q\n%.1500s", want, chat)
		}
	}
	if strings.Contains(chat, `data-chat-message="user"`) || strings.Contains(chat, "[muster] Your owner's chief") {
		t.Errorf("the chat draws the chief's message as the person's, or its mark:\n%.1500s", chat)
	}
	var reqs int64
	if err := w.f.pool.QueryRow(authz.System(w.ctx), `SELECT coalesce(sum(requests), 0) FROM officer_llm_usage WHERE officer = $1 AND instance = 1 AND day = $2`,
		builtinOfficerID, today).Scan(&reqs); err != nil {
		t.Fatal(err)
	}
	if reqs != 7 {
		t.Fatalf("instance 1's requests booked today: %d; want the chief's chat's 7", reqs)
	}
	// The chief reads its owner's chat back.
	var read struct {
		Chat []operatorChatMessage `json:"chat"`
	}
	if err := json.Unmarshal([]byte(chiefCall(w.srv, 1, authz.ViaAgent, chiefOf(1), chiefOfficerReadTool, `{"officer":"`+builtinOfficerID+`","section":"chat"}`)), &read); err != nil ||
		len(read.Chat) != 2 || !read.Chat[0].FromChief || read.Chat[0].Text != msg || read.Chat[1].Role != "assistant" {
		t.Fatalf("the chat as the chief reads it: %+v %v", read.Chat, err)
	}
	if rows := chiefAuditRows(t, w.f, "officer_def:"+builtinOfficerID); strings.Join(rows, ",") != "officer.chief.message||0|1" {
		t.Errorf("the audit = %v; want the one message", rows)
	}
	if page := w.officerPage(); !strings.Contains(page, ">Your chief sent it a message</span>") {
		t.Errorf("What it did lacks the chief's message")
	}
	// A worker is refused the chief's tools (dispatchAgentTool's chief-only
	// re-check).
	if out := w.srv.dispatchAgentTool(w.ctx, w.agent, chiefOfficerMessageTool, `{"officer":"`+builtinOfficerID+`","message":"x"}`); !strings.Contains(errOf(out), "available to your owner's chief agent only") {
		t.Errorf("the officer's own instance calling %s: %s", chiefOfficerMessageTool, out)
	}
	if len(gw.sent()) != 1 {
		t.Errorf("a refused call sent a turn")
	}
}

// TestAnEnvOfficersSchedulesAreReadOnlyToTheChief: MUSTER_OFFICERS declares
// the outbound world's officer, so its schedules are the environment's — the
// page's rule — while its pause and resume work (they write its switch).
func TestAnEnvOfficersSchedulesAreReadOnlyToTheChief(t *testing.T) {
	w := newOutboundWorld(t, true)
	wipeChiefAudit(t)
	t.Cleanup(func() { wipeChiefAudit(t) })
	a := chiefOf(1)
	if out := chiefCall(w.srv, 1, authz.ViaAgent, a, chiefOfficerScheduleTool, `{"officer":"`+builtinOfficerID+`","action":"put","cron":"0 9 * * *"}`); !strings.Contains(errOf(out), "MUSTER_OFFICERS declares this officer") {
		t.Fatalf("a schedule put on an env officer: %s", out)
	}
	if out := chiefCall(w.srv, 1, authz.ViaAgent, a, chiefOfficerScheduleTool, `{"officer":"`+builtinOfficerID+`","action":"list"}`); isToolErr(out) {
		t.Fatalf("control: listing its schedules: %s", out)
	}
	if out := chiefCall(w.srv, 1, authz.ViaAgent, a, chiefOfficerControlTool, `{"officer":"`+builtinOfficerID+`","action":"pause"}`); isToolErr(out) {
		t.Fatalf("control: pausing the env officer: %s", out)
	}
	if out := chiefCall(w.srv, 1, authz.ViaAgent, a, chiefOfficerControlTool, `{"officer":"`+builtinOfficerID+`","action":"resume"}`); isToolErr(out) {
		t.Fatalf("control: resuming the env officer: %s", out)
	}
}
