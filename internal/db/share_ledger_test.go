package db_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/users"
)

// Phase 2a of docs/design/team-sharing.md (migration 0126): the team access
// core. Every owned table is in exactly ONE share class.

// shareClass is a share-class table's minimum team role to read and to write
// (1 viewer, 2 operator, 3 manager).
type shareClass struct{ read, write int }

// shareClasses is every owned table a team's members may reach, with its
// roles. Every owned table NOT here is personal: owner_isolation, never
// shared. TestEveryOwnedTableHasAShareClass fails when a class changes here
// without the migration, the migration without here, or an owned table
// appears in neither ledger.
var shareClasses = map[string]shareClass{
	// shared: read viewer, write operator
	"agent_focus": {1, 2}, "agent_focus_history": {1, 2},
	"ask_attempts": {1, 2}, "asks": {1, 2}, "attention_asks": {1, 2},
	"chat_messages": {1, 2}, "chat_sessions": {1, 2}, "chief_nudges": {1, 2},
	"chief_proposals": {1, 2}, "note_attachments": {1, 2},
	"note_comments": {1, 2}, "notes": {1, 2}, "officer_items": {1, 2},
	"officer_pr_loops": {1, 2},
	"routing_rules":    {1, 2}, "send_queue": {1, 2}, "session_episodes": {1, 2},
	"session_prs": {1, 2}, "session_routes": {1, 2}, "task_ref_writes": {1, 2},
	"task_sessions": {1, 2}, "uploads": {1, 2},
	// managed: write manager
	"mirror_tokens": {1, 3}, "officer_defs": {1, 3}, "officer_schedules": {1, 3},
	"officer_types": {1, 3}, "plugin_handoffs": {2, 3}, "plugin_message_handling": {2, 3},
	"plugin_messages": {2, 3}, "plugin_reply_drafts": {2, 3}, "plugin_threads": {2, 3},
	"plugins": {2, 3},
	// usage: the budgets a team's spending is checked against. Read at
	// viewer; written by NO member (4: no role reaches it) — only by the
	// owner's own recording path or the system scope, so an operator cannot
	// delete the day's row and reset a cap.
	"chief_turn_usage": {1, noMember}, "officer_judgement_sessions": {1, noMember}, "officer_llm_usage": {1, noMember},
}

// noMember is a write role no team role reaches (manager is 3).
const noMember = 4

// personalOwned is EVERY owned table that is personal — owner_isolation
// only, no team access — listed explicitly, so a NEW owner_id table is in
// neither ledger and fails TestEveryOwnedTableHasAShareClass until someone
// decides its class (it is never personal by default).
var personalOwned = []string{
	"agents", "cc_account_failovers", "cc_account_marks", "cc_account_usage", "chief_preferences",
	"connector_audit", "connector_grants", "connector_standing_grants", "connectors", "device_challenges",
	"device_pairing_codes", "devices", "flow_jobs", "flow_requests", "flow_runner_tokens",
	"host_dispatches", "jobs", "mail_resumes", "notify_preferences", "page_links",
	"page_responses", "page_versions", "pages", "pins", "plugin_pair_requests",
	"push_preferences", "push_subscriptions", "recents", "session_tags", "suggest_preferences",
	"suggest_usage", "user_credentials",
}

// neverShared are the owned tables that must stay personal whatever a later
// phase decides: credentials, connectors, devices and push (the operator's
// decision), named so moving one into shareClasses fails by name.
var neverShared = []string{
	"user_credentials", "connectors", "connector_grants", "connector_standing_grants", "connector_audit",
	"devices", "device_pairing_codes", "device_challenges", "push_subscriptions", "push_preferences",
}

// notSharedUntil is an owned table that stays personal FOR NOW, with the
// phase that may share it and what it must do first. agents rows carry an
// agent's live secrets (hooks_token, gateway_secret: internal/agents
// pgstore.go's agentCols), so phase 4 moves those into an owner-isolated table
// before agents is shared (coordinated with the phase-4 builder).
var notSharedUntil = map[string]string{
	"agents": "phase 4: its secrets (hooks_token, gateway_secret) move to an owner-isolated table first",
}

func sharedClassTables() []string {
	out := make([]string, 0, len(shareClasses))
	for t := range shareClasses {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func personalTables() []string {
	out := append([]string(nil), personalOwned...)
	sort.Strings(out)
	return out
}

// unclassified is each of tables (the database's owner_id tables) in NEITHER
// personalOwned nor shareClasses, and each table listed in both.
func unclassified(tables []string) (missing, both []string) {
	personal := map[string]bool{}
	for _, t := range personalOwned {
		personal[t] = true
		if _, shared := shareClasses[t]; shared {
			both = append(both, t)
		}
	}
	for _, t := range tables {
		if _, shared := shareClasses[t]; !shared && !personal[t] {
			missing = append(missing, t)
		}
	}
	sort.Strings(missing)
	sort.Strings(both)
	return missing, both
}

// teamPolicyCmds is each team policy's command.
var teamPolicyCmds = map[string]string{"team_read": "SELECT", "team_insert": "INSERT", "team_update": "UPDATE", "team_delete": "DELETE"}

const (
	userArm  = "owner_id = (NULLIF(current_setting('muster.uid'::text, true), ''::text))::bigint"
	scopeArm = "current_setting('muster.scope'::text, true) = 'all'::text"
)

func teamArm(role int) string {
	return fmt.Sprintf("owner_id = ANY (COALESCE(( SELECT muster_team_ids(%d) AS muster_team_ids), '{}'::bigint[]))", role)
}

// checkTeamPolicy pins one of a share-class table's four policies whole: its
// command, and in each expression it has, the user arm, the scope-all arm and
// the team arm at the class's role for that command.
func checkTeamPolicy(t *testing.T, tbl, name, cmd, permissive, qual, check string, c shareClass) {
	t.Helper()
	want, ok := teamPolicyCmds[name]
	if !ok || cmd != want || permissive != "PERMISSIVE" {
		t.Errorf("%s: policy %q cmd=%s %s; want one of the four team policies, each for its own command", tbl, name, cmd, permissive)
		return
	}
	role := c.write
	if name == "team_read" {
		role = c.read
	}
	exprs := map[string]string{}
	switch name {
	case "team_read", "team_delete":
		exprs["USING"] = qual
		if check != "" {
			t.Errorf("%s %s has a WITH CHECK %q; want none", tbl, name, check)
		}
	case "team_insert":
		exprs["WITH CHECK"] = check
		if qual != "" {
			t.Errorf("%s %s has a USING %q; want none", tbl, name, qual)
		}
	case "team_update":
		exprs["USING"], exprs["WITH CHECK"] = qual, check
	}
	for which, expr := range exprs {
		if !strings.Contains(expr, userArm) || !strings.Contains(expr, scopeArm) || !strings.Contains(expr, teamArm(role)) {
			t.Errorf("%s %s %s = %s; want the user arm, the scope-all arm and %s", tbl, name, which, expr, teamArm(role))
		}
	}
}

// TestEveryOwnedTableHasAShareClass is the share ledger: every table with an
// owner_id is personal (owner_isolation only) or in shareClasses (exactly the
// four team policies at its roles, and owner_fixed), the never-shared tables
// are personal, and owner_id still defaults to the person on all of them.
func TestEveryOwnedTableHasAShareClass(t *testing.T) {
	f := newRLSFixture(t, "ms_0126_ledger")
	ownedTables := ownerIDTables(t, f)
	if missing, both := unclassified(ownedTables); len(missing) > 0 || len(both) > 0 {
		t.Errorf("owned tables with no share class (decide: personalOwned, or a class in shareClasses): %v; listed in both: %v", missing, both)
	}
	owned := map[string]bool{}
	for _, tbl := range ownedTables {
		owned[tbl] = true
	}
	for _, tbl := range personalOwned {
		if !owned[tbl] {
			t.Errorf("personalOwned lists %s, which is not an owned table", tbl)
		}
	}
	for tbl := range shareClasses {
		if !owned[tbl] {
			t.Errorf("shareClasses lists %s, which is not an owned table", tbl)
		}
	}
	for _, tbl := range neverShared {
		if _, shared := shareClasses[tbl]; shared {
			t.Errorf("%s is shared; credentials, connectors, devices and push are never shared", tbl)
		}
	}
	for tbl, until := range notSharedUntil {
		if _, shared := shareClasses[tbl]; shared {
			t.Errorf("%s is shared, but it is not shared until %s", tbl, until)
		}
		if !owned[tbl] {
			t.Errorf("notSharedUntil lists %s, which is not an owned table", tbl)
		}
	}
	type pol struct{ name, cmd, permissive, qual, check string }
	got := map[string][]pol{}
	rows, err := f.pool.Query(f.mig, `SELECT tablename::text, policyname::text, cmd, permissive, coalesce(qual,''), coalesce(with_check,'')
		FROM pg_policies WHERE schemaname='public' AND tablename = ANY($1)`, rlsTables())
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var tbl string
		var p pol
		if err := rows.Scan(&tbl, &p.name, &p.cmd, &p.permissive, &p.qual, &p.check); err != nil {
			t.Fatal(err)
		}
		got[tbl] = append(got[tbl], p)
	}
	rows.Close()
	var triggers []string
	trows, err := f.pool.Query(f.mig, `SELECT c.relname::text FROM pg_trigger g JOIN pg_class c ON c.oid = g.tgrelid
		WHERE g.tgname = 'owner_fixed' AND NOT g.tgisinternal ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	if triggers, err = pgx.CollectRows(trows, pgx.RowTo[string]); err != nil {
		t.Fatal(err)
	}
	if want := sharedClassTables(); !slices.Equal(triggers, want) {
		t.Errorf("tables with owner_fixed = %v\nwant the share-class tables %v", triggers, want)
	}
	for _, tbl := range rlsTables() {
		ps := got[tbl]
		var names []string
		for _, p := range ps {
			names = append(names, p.name)
		}
		sort.Strings(names)
		if c, shared := shareClasses[tbl]; shared {
			if strings.Join(names, ",") != "team_delete,team_insert,team_read,team_update" {
				t.Errorf("%s (shared, read %d write %d) has policies %v; want exactly the four team policies", tbl, c.read, c.write, names)
				continue
			}
			for _, p := range ps {
				checkTeamPolicy(t, tbl, p.name, p.cmd, p.permissive, p.qual, p.check, c)
			}
		} else if strings.Join(names, ",") != "owner_isolation" {
			t.Errorf("%s (personal) has policies %v; want owner_isolation only", tbl, names)
		}
	}
	if len(rlsTables()) < 60 || len(shareClasses) < 30 {
		t.Fatalf("%d owned tables, %d share-class ones; the ledger is reading nothing", len(rlsTables()), len(shareClasses))
	}
}

// member is a person's principal through door via, with the memberships
// users.Account reads — as every request builds it.
func member(t *testing.T, f *rlsFixture, uid int64, via authz.Via) context.Context {
	t.Helper()
	a, err := users.NewPG(f.pool).Account(authz.System(f.base), uid)
	if err != nil {
		t.Fatal(err)
	}
	return authz.WithPrincipal(f.base, authz.Principal{UserID: uid, Role: a.Role, Via: via, Teams: a.Teams})
}

// TestTheShareLedgerHoldsForEveryRole is the behaviour behind the ledger, on
// EVERY owned table: a team writes a row into each (planted: phase 2a creates
// no team rows), and a person in each role — none, viewer, operator,
// manager — reads, updates and deletes it as its class says (each write in a
// transaction rolled back, so every role meets every row). CONTROLS: the team
// itself and System see every row, so a zero is about the role; and the SAME
// manager through every door where no person is present — their personal
// API token, their agents and personal chief, their mirror host, every
// background loop — sees nothing, while their web session (the role loop)
// and their paired phone see the team's rows.
func TestTheShareLedgerHoldsForEveryRole(t *testing.T) {
	f := newRLSFixture(t, "ms_0126_roles")
	store := users.NewPG(f.pool)
	by := users.Actor{UserID: 1, Via: authz.ViaSession}
	people := map[string]int64{}
	for _, r := range []string{"none", "viewer", "operator", "manager"} {
		people[r] = scanID(f.mig, t, f.pool, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, "p-"+r)
	}
	team, err := store.CreateTeam(f.base, by, "Platform", people["manager"])
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []authz.TeamRole{authz.TeamViewer, authz.TeamOperator} {
		if err := store.SetTeamMember(f.base, by, team.ID, people[string(r)], r); err != nil {
			t.Fatal(err)
		}
	}
	asTeam := authz.WithPrincipal(f.base, authz.Principal{UserID: team.ID, Role: authz.RoleMember, Via: authz.ViaSession})
	if err := writeEveryOwnedTable(asTeam, f.pool, "team"); err != nil {
		t.Fatalf("the team's writes: %v", err)
	}
	level := map[string]int{"none": 0, "viewer": 1, "operator": 2, "manager": 3}
	// Every door where no PERSON is present (authz TeamScope): the manager's
	// personal API token, their agents — their PERSONAL chief included —
	// their mirror host's MCP door, and every background loop acting as them.
	// Their paired phone (ViaDevice) is a person's door, like the session.
	device := member(t, f, people["manager"], authz.ViaDevice)
	var machineDoors []context.Context
	for _, via := range []authz.Via{authz.ViaToken, authz.ViaAgent, authz.ViaMirrorHost, authz.ViaConnect, authz.ViaRelay, authz.ViaKickoff,
		authz.ViaAgentMessage, authz.ViaReap, authz.ViaAttention, authz.ViaAsk, authz.ViaJobQueue, authz.ViaFocus, authz.ViaRoute,
		authz.ViaOfficer, authz.ViaHostDispatch, authz.ViaQueue, authz.ViaRunner, authz.ViaPlugin, authz.ViaPageLink,
		authz.ViaHook, authz.ViaService} {
		machineDoors = append(machineDoors, member(t, f, people["manager"], via))
	}
	for _, tbl := range rlsTables() {
		owned := count(t, f.mig, f.pool, tbl, "WHERE owner_id = $1", team.ID)
		if owned == 0 {
			t.Errorf("fixture: the team owns no row in %s", tbl)
			continue
		}
		if got := count(t, asTeam, f.pool, tbl, "WHERE owner_id = $1", team.ID); got != owned {
			t.Errorf("control: the team itself sees %d of its %d rows in %s", got, owned, tbl)
		}
		for _, ctx := range machineDoors {
			if got := count(t, ctx, f.pool, tbl, "WHERE owner_id = $1", team.ID); got != 0 {
				p, _ := authz.From(ctx)
				t.Errorf("the team's manager through the %s door sees %d of the team's rows in %s; want 0 (only a person's own doors carry teams)", p.Via, got, tbl)
			}
		}
		c, shared := shareClasses[tbl]
		wantDevice := 0
		if shared && level["manager"] >= c.read {
			wantDevice = owned
		}
		if got := count(t, device, f.pool, tbl, "WHERE owner_id = $1", team.ID); got != wantDevice {
			t.Errorf("the team's manager through their paired phone (device) sees %d of the team's rows in %s; want %d", got, tbl, wantDevice)
		}
		for role, lvl := range level {
			ctx := member(t, f, people[role], authz.ViaSession)
			wantRead, wantWrite := 0, 0
			if shared && lvl >= c.read {
				wantRead = owned
			}
			if shared && lvl >= c.write {
				wantWrite = owned
			}
			if got := count(t, ctx, f.pool, tbl, "WHERE owner_id = $1", team.ID); got != wantRead {
				t.Errorf("READ %s as %s: %d of the team's %d rows; want %d", tbl, role, got, owned, wantRead)
			}
			// UPDATE's row filter, without firing a column trigger: SELECT …
			// FOR UPDATE is admitted by the UPDATE policies' USING.
			if got := countLocked(t, ctx, f, tbl, team.ID); got != wantWrite {
				t.Errorf("UPDATE-lock %s as %s: %d of the team's %d rows; want %d", tbl, role, got, owned, wantWrite)
			}
			n, err := rolledBack(ctx, f, "DELETE FROM "+tbl+" WHERE owner_id = $1", team.ID)
			switch {
			case err != nil && wantWrite > 0 && pgState(err) == "23503":
				// Admitted by the DELETE policy, then stopped by a child's
				// foreign key: the row WAS reachable.
			case err != nil:
				t.Errorf("DELETE %s as %s: %v", tbl, role, err)
			case n != int64(wantWrite):
				t.Errorf("DELETE %s as %s: %d rows; want %d", tbl, role, n, wantWrite)
			}
		}
	}
}

// countLocked is how many of owner's rows in tbl ctx may lock FOR UPDATE —
// the rows the UPDATE policies admit — in a transaction it rolls back.
func countLocked(t *testing.T, ctx context.Context, f *rlsFixture, tbl string, owner int64) int {
	t.Helper()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	rows, err := tx.Query(ctx, `SELECT 1 FROM `+tbl+` WHERE owner_id = $1 FOR UPDATE`, owner)
	if err != nil {
		t.Fatalf("lock %s: %v", tbl, err)
	}
	n := 0
	for rows.Next() {
		n++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("lock %s: %v", tbl, err)
	}
	return n
}

func pgState(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return ""
}

// rolledBack runs one statement in a transaction it rolls back, and reports
// how many rows it affected.
func rolledBack(ctx context.Context, f *rlsFixture, sql string, args ...any) (int64, error) {
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	tag, err := tx.Exec(ctx, sql, args...)
	return tag.RowsAffected(), err
}

// TestATeamRowCannotChangeHands: on a share-class table nobody but the system
// scope moves a row between owners — not an operator re-owning the team's row
// to themselves, nor a person moving their own row into the team. Inserting
// AS the team is admitted only at the write role (notes: operator; officer
// types: manager). CONTROL: System may move a row.
func TestATeamRowCannotChangeHands(t *testing.T) {
	f := newRLSFixture(t, "ms_0126_hands")
	store := users.NewPG(f.pool)
	by := users.Actor{UserID: 1, Via: authz.ViaSession}
	op := scanID(f.mig, t, f.pool, `INSERT INTO users (role, display_name) VALUES ('member', 'op') RETURNING id`)
	viewer := scanID(f.mig, t, f.pool, `INSERT INTO users (role, display_name) VALUES ('member', 'viewer') RETURNING id`)
	team, err := store.CreateTeam(f.base, by, "Platform", 1)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.SetTeamMember(f.base, by, team.ID, op, authz.TeamOperator)
	_ = store.SetTeamMember(f.base, by, team.ID, viewer, authz.TeamViewer)
	opCtx, viewerCtx := member(t, f, op, authz.ViaSession), member(t, f, viewer, authz.ViaSession)
	teamNote := scanID(authz.System(f.base), t, f.pool, `INSERT INTO notes (owner_id, title) VALUES ($1, 'team') RETURNING id`, team.ID)
	ownNote := scanID(opCtx, t, f.pool, `INSERT INTO notes (title) VALUES ('mine') RETURNING id`)

	_, err = f.pool.Exec(opCtx, `UPDATE notes SET owner_id = $1 WHERE id = $2`, op, teamNote)
	assertSQLState(t, "an operator re-owning the team's task to themselves", err, "42501")
	_, err = f.pool.Exec(opCtx, `UPDATE notes SET owner_id = $1 WHERE id = $2`, team.ID, ownNote)
	assertSQLState(t, "a member moving their own task into the team", err, "42501")
	if _, err := f.pool.Exec(opCtx, `UPDATE notes SET title = 'renamed' WHERE id = $1`, teamNote); err != nil {
		t.Errorf("control: an operator editing the team's task: %v", err)
	}
	if _, err := f.pool.Exec(authz.System(f.base), `UPDATE notes SET owner_id = $1 WHERE id = $2`, op, ownNote); err != nil {
		t.Errorf("control: System moving a row: %v", err)
	}

	// Inserting as the team: notes at operator, officer_types at manager.
	if _, err := f.pool.Exec(opCtx, `INSERT INTO notes (owner_id, title) VALUES ($1, 'filed')`, team.ID); err != nil {
		t.Errorf("an operator filing a team task: %v", err)
	}
	_, err = f.pool.Exec(viewerCtx, `INSERT INTO notes (owner_id, title) VALUES ($1, 'filed')`, team.ID)
	assertSQLState(t, "a viewer filing a team task", err, "42501")
	_, err = f.pool.Exec(opCtx, `INSERT INTO officer_types (owner_id, key, name, charter) VALUES ($1, 'mk', 'Marketing', 'c')`, team.ID)
	assertSQLState(t, "an operator defining a team officer type (managed)", err, "42501")
	if _, err := f.pool.Exec(member(t, f, 1, authz.ViaSession), `INSERT INTO officer_types (owner_id, key, name, charter) VALUES ($1, 'mk', 'Marketing', 'c')`, team.ID); err != nil {
		t.Errorf("control: the team's manager defining a team officer type: %v", err)
	}
}

// TestTheActingPersonIsRecordedOnEachSharedAction: on a team's row, each
// shared action records WHO did it beside the owner it was done to — a queued
// send (queued_by), a comment (author_id), an answered ask
// (answered_by_user), a decided draft (decided_by_user) — as the acting
// person, and NULL when the system scope does it. CONTROL: the owner of each
// row is the team, so the column is not just the owner again.
func TestTheActingPersonIsRecordedOnEachSharedAction(t *testing.T) {
	f := newRLSFixture(t, "ms_0126_attribution")
	store := users.NewPG(f.pool)
	by := users.Actor{UserID: 1, Via: authz.ViaSession}
	op := scanID(f.mig, t, f.pool, `INSERT INTO users (role, display_name) VALUES ('member', 'op') RETURNING id`)
	team, err := store.CreateTeam(f.base, by, "Platform", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetTeamMember(f.base, by, team.ID, op, authz.TeamOperator); err != nil {
		t.Fatal(err)
	}
	opCtx := member(t, f, op, authz.ViaSession)
	sys := authz.System(f.base)
	asTeam := authz.WithPrincipal(f.base, authz.Principal{UserID: team.ID, Role: authz.RoleMember, Via: authz.ViaSession})
	if err := writeEveryOwnedTable(asTeam, f.pool, "attr"); err != nil {
		t.Fatal(err)
	}
	note := scanID(sys, t, f.pool, `SELECT id FROM notes WHERE owner_id = $1 LIMIT 1`, team.ID)

	// A comment and a queued send, written by the operator onto the team's rows.
	comment := scanID(opCtx, t, f.pool, `INSERT INTO note_comments (note_id, owner_id, author, body) VALUES ($1, $2, 'op', 'hi') RETURNING id`, note, team.ID)
	queued := scanID(opCtx, t, f.pool, `INSERT INTO send_queue (owner_id, mirror_host, mirror_session, mirror_window, body, stepup_at)
		VALUES ($1, 'build-01', 'main', '', 'run the tests', now()) RETURNING id`, team.ID)
	// An ask answered, and a draft decided, by the operator.
	ask := scanID(sys, t, f.pool, `SELECT id FROM attention_asks WHERE owner_id = $1 AND status = 'open' LIMIT 1`, team.ID)
	if _, err := f.pool.Exec(opCtx, `UPDATE attention_asks SET status = 'answered', answer = 'yes', answered_by = 'operator', resolved_at = now() WHERE id = $1`, ask); err != nil {
		t.Fatal(err)
	}
	var draft int64
	if err := f.pool.QueryRow(sys, `SELECT id FROM plugin_reply_drafts WHERE owner_id = $1 AND status = 'pending' LIMIT 1`, team.ID).Scan(&draft); err != nil {
		t.Fatalf("fixture: the team has no pending draft: %v", err)
	}
	// A draft is decided by a MANAGER (managed class); user 1 manages the team.
	if _, err := f.pool.Exec(member(t, f, 1, authz.ViaSession), `UPDATE plugin_reply_drafts SET status = 'rejected', decided_at = now() WHERE id = $1`, draft); err != nil {
		t.Fatal(err)
	}
	for what, c := range map[string]struct {
		sql  string
		id   int64
		want int64
	}{
		"send_queue.queued_by":                {`SELECT queued_by FROM send_queue WHERE id = $1`, queued, op},
		"note_comments.author_id":             {`SELECT author_id FROM note_comments WHERE id = $1`, comment, op},
		"attention_asks.answered_by_user":     {`SELECT answered_by_user FROM attention_asks WHERE id = $1`, ask, op},
		"plugin_reply_drafts.decided_by_user": {`SELECT decided_by_user FROM plugin_reply_drafts WHERE id = $1`, draft, 1},
	} {
		var got *int64
		if err := f.pool.QueryRow(sys, c.sql, c.id).Scan(&got); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if got == nil || *got != c.want {
			t.Errorf("%s = %v; want the acting person %d (the row's owner is the team, %d)", what, got, c.want, team.ID)
		}
	}
	// The system scope records no person.
	ask2 := scanID(sys, t, f.pool, `INSERT INTO attention_asks (owner_id, agent_id, question, expires_at)
		SELECT $1, id, 'again?', now() + interval '1 hour' FROM agents WHERE owner_id = $1 LIMIT 1 RETURNING id`, team.ID)
	if _, err := f.pool.Exec(sys, `UPDATE attention_asks SET status = 'answered', answer = 'no', answered_by = 'chief', resolved_at = now() WHERE id = $1`, ask2); err != nil {
		t.Fatal(err)
	}
	var sysAnswer *int64
	if err := f.pool.QueryRow(sys, `SELECT answered_by_user FROM attention_asks WHERE id = $1`, ask2).Scan(&sysAnswer); err != nil || sysAnswer != nil {
		t.Errorf("an answer under the system scope recorded person %v (%v); want NULL", sysAnswer, err)
	}
}

// TestATeamsAuditIsItsManagersToRead: an audit row owned by a team (every
// membership change, users teams.go) is read by the team's managers through
// audit_log's self_read — not by its other members, nor by an outsider. The
// CONTROL is that the acting admin, the row's own actor, reads it too.
func TestATeamsAuditIsItsManagersToRead(t *testing.T) {
	f := newRLSFixture(t, "ms_0126_audit")
	store := users.NewPG(f.pool)
	manager := scanID(f.mig, t, f.pool, `INSERT INTO users (role, display_name) VALUES ('member', 'mgr') RETURNING id`)
	team, err := store.CreateTeam(f.base, users.Actor{UserID: 1, Via: authz.ViaSession}, "Platform", manager)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetTeamMember(f.base, users.Actor{UserID: 1, Via: authz.ViaSession}, team.ID, f.two, authz.TeamOperator); err != nil {
		t.Fatal(err)
	}
	for who, c := range map[string]struct {
		ctx  context.Context
		want bool
	}{
		"the team's manager":             {member(t, f, manager, authz.ViaSession), true},
		"the team's operator":            {member(t, f, f.two, authz.ViaSession), false},
		"the acting admin (control)":     {member(t, f, 1, authz.ViaSession), true},
		"the manager via a machine door": {member(t, f, manager, authz.ViaAgent), false},
	} {
		entries, err := store.TeamAudit(c.ctx, team.ID, 10)
		if err != nil {
			t.Fatalf("%s: %v", who, err)
		}
		if (len(entries) >= 3) != c.want {
			t.Errorf("%s read %d of the team's audit rows; want them: %v", who, len(entries), c.want)
		}
	}
}

// TestTheTeamIDsFunctionParsesTheSetting: muster_team_ids reads muster.teams
// in phase 1's canonical form, '<team>:<role>,…', by role NAME — an unknown
// role, a numeric role, a malformed id or an empty setting grants nothing.
// Read through the very connection setting the pool writes (set_config in
// one transaction).
func TestTheTeamIDsFunctionParsesTheSetting(t *testing.T) {
	f := newRLSFixture(t, "ms_0126_parse")
	for _, c := range []struct {
		setting          string
		ids1, ids2, ids3 string // muster_team_ids(1|2|3), as text
	}{
		{"12:operator,15:viewer,18:manager", "{12,15,18}", "{12,18}", "{18}"},
		{"", "{}", "{}", "{}"},
		{"12:owner", "{}", "{}", "{}"},
		{"12:2", "{}", "{}", "{}"},
		{"x:manager,12:viewer", "{12}", "{}", "{}"},
		{"112:manager", "{112}", "{112}", "{112}"},
	} {
		tx, err := f.pool.Begin(f.mig)
		if err != nil {
			t.Fatal(err)
		}
		var i1, i2, i3, i0 string
		_, err = tx.Exec(f.mig, `SELECT set_config('muster.teams', $1, true)`, c.setting)
		if err == nil {
			err = tx.QueryRow(f.mig, `SELECT muster_team_ids(1)::text, muster_team_ids(2)::text, muster_team_ids(3)::text, muster_team_ids(0)::text`).Scan(&i1, &i2, &i3, &i0)
		}
		_ = tx.Rollback(f.mig)
		if err != nil {
			t.Fatalf("%q: %v", c.setting, err)
		}
		if i1 != c.ids1 || i2 != c.ids2 || i3 != c.ids3 || i0 != "{}" {
			t.Errorf("setting %q: ids(1,2,3,0) = %s %s %s %s; want %s %s %s {}", c.setting, i1, i2, i3, i0, c.ids1, c.ids2, c.ids3)
		}
	}
}

// ownerIDTables is every public table with an owner_id column, read from the
// database itself (not from a test ledger).
func ownerIDTables(t *testing.T, f *rlsFixture) []string {
	t.Helper()
	rows, err := f.pool.Query(f.mig, `SELECT table_name::text FROM information_schema.columns
		WHERE table_schema = 'public' AND column_name = 'owner_id' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestANewOwnedTableIsUnclassifiedUntilDecided is the share ledger's
// positive control: a NEW table with an owner_id column, created in the
// fixture's database, is reported unclassified — it is not personal by
// default — while every existing owned table is classified.
func TestANewOwnedTableIsUnclassifiedUntilDecided(t *testing.T) {
	f := newRLSFixture(t, "ms_0126_new_table")
	if missing, _ := unclassified(ownerIDTables(t, f)); len(missing) != 0 {
		t.Fatalf("control: owned tables already unclassified: %v", missing)
	}
	mustExec(f.mig, t, f.pool, `CREATE TABLE public.probe_new_owned (id bigint, owner_id bigint)`)
	missing, _ := unclassified(ownerIDTables(t, f))
	if len(missing) != 1 || missing[0] != "probe_new_owned" {
		t.Errorf("a new owned table: unclassified = %v; want [probe_new_owned]", missing)
	}
}

// TestTheActingPersonCannotBeForged: the attribution columns say who acted,
// never who the statement claimed acted. The application role's INSERT that
// names someone else is overwritten with the acting person (queued_by,
// author_id) — or with nobody, for an ask or a draft not born answered or
// decided — and an UPDATE that rewrites one without the state change that
// stamps it is refused (42501). CONTROLS: the honest writes record the actor
// (TestTheActingPersonIsRecordedOnEachSharedAction), and the login role (a
// restore) may still write the column.
func TestTheActingPersonCannotBeForged(t *testing.T) {
	f := newRLSFixture(t, "ms_0126_forge")
	store := users.NewPG(f.pool)
	by := users.Actor{UserID: 1, Via: authz.ViaSession}
	op := scanID(f.mig, t, f.pool, `INSERT INTO users (role, display_name) VALUES ('member', 'op') RETURNING id`)
	team, err := store.CreateTeam(f.base, by, "Platform", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetTeamMember(f.base, by, team.ID, op, authz.TeamOperator); err != nil {
		t.Fatal(err)
	}
	opCtx, mgr := member(t, f, op, authz.ViaSession), member(t, f, 1, authz.ViaSession)
	sys := authz.System(f.base)
	asTeam := authz.WithPrincipal(f.base, authz.Principal{UserID: team.ID, Role: authz.RoleMember, Via: authz.ViaSession})
	if err := writeEveryOwnedTable(asTeam, f.pool, "forge"); err != nil {
		t.Fatal(err)
	}
	note := scanID(sys, t, f.pool, `SELECT id FROM notes WHERE owner_id = $1 LIMIT 1`, team.ID)
	read := func(sql string, id int64) *int64 {
		var v *int64
		if err := f.pool.QueryRow(sys, sql, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	is := func(v *int64, want int64) bool { return v != nil && *v == want }

	// INSERTs that name someone else.
	queued := scanID(opCtx, t, f.pool, `INSERT INTO send_queue (owner_id, mirror_host, mirror_session, mirror_window, body, stepup_at, queued_by)
		VALUES ($1, 'build-01', 'main', '', 'x', now(), 1) RETURNING id`, team.ID)
	if v := read(`SELECT queued_by FROM send_queue WHERE id = $1`, queued); !is(v, op) {
		t.Errorf("a forged queued_by on insert stored %v; want the acting person %d", v, op)
	}
	comment := scanID(opCtx, t, f.pool, `INSERT INTO note_comments (note_id, owner_id, author, body, author_id) VALUES ($1, $2, 'op', 'hi', 1) RETURNING id`, note, team.ID)
	if v := read(`SELECT author_id FROM note_comments WHERE id = $1`, comment); !is(v, op) {
		t.Errorf("a forged author_id on insert stored %v; want the acting person %d", v, op)
	}
	// A mirrored session's ask (an agent's is refused to a member until phase
	// 4 shares agents: the ask's owner check reads the agent).
	ask := scanID(opCtx, t, f.pool, `INSERT INTO attention_asks (owner_id, mirror_host, mirror_session, question, expires_at, answered_by_user)
		VALUES ($1, 'build-01', 'sess-forge', 'which?', now() + interval '1 hour', 1) RETURNING id`, team.ID)
	if v := read(`SELECT answered_by_user FROM attention_asks WHERE id = $1`, ask); v != nil {
		t.Errorf("an open ask inserted with a forged answered_by_user stored %v; want none", *v)
	}

	// UPDATEs that rewrite the column without the state change.
	for what, c := range map[string]struct {
		ctx context.Context
		sql string
		id  int64
	}{
		"queued_by":        {opCtx, `UPDATE send_queue SET queued_by = 1 WHERE id = $1`, queued},
		"author_id":        {opCtx, `UPDATE note_comments SET author_id = 1 WHERE id = $1`, comment},
		"answered_by_user": {opCtx, `UPDATE attention_asks SET answered_by_user = 1 WHERE id = $1`, ask},
	} {
		_, err := f.pool.Exec(c.ctx, c.sql, c.id)
		assertSQLState(t, "rewriting "+what, err, "42501")
	}
	draft := scanID(sys, t, f.pool, `SELECT id FROM plugin_reply_drafts WHERE owner_id = $1 LIMIT 1`, team.ID)
	_, err = f.pool.Exec(mgr, `UPDATE plugin_reply_drafts SET decided_by_user = $2 WHERE id = $1`, draft, op)
	assertSQLState(t, "rewriting decided_by_user", err, "42501")

	// CONTROL: the login role (a migration, a restore) is not held to it.
	if _, err := f.pool.Exec(f.mig, `UPDATE send_queue SET queued_by = 1 WHERE id = $1`, queued); err != nil {
		t.Errorf("control: the login role rewriting queued_by: %v", err)
	}
	if v := read(`SELECT queued_by FROM send_queue WHERE id = $1`, queued); !is(v, 1) {
		t.Errorf("control: the login role's write stored %v; want 1", v)
	}
}

// TestNoMemberWritesATeamsUsage: the usage tables are a team's budgets; a
// member reads them, and even the team's MANAGER can neither change nor
// delete a row — that would reset a cap. CONTROL: the system scope (the
// recording path's) changes one.
func TestNoMemberWritesATeamsUsage(t *testing.T) {
	f := newRLSFixture(t, "ms_0126_usage")
	store := users.NewPG(f.pool)
	team, err := store.CreateTeam(f.base, users.Actor{UserID: 1, Via: authz.ViaSession}, "Platform", 1)
	if err != nil {
		t.Fatal(err)
	}
	asTeam := authz.WithPrincipal(f.base, authz.Principal{UserID: team.ID, Role: authz.RoleMember, Via: authz.ViaSession})
	if err := writeEveryOwnedTable(asTeam, f.pool, "usage"); err != nil {
		t.Fatal(err)
	}
	mgr := member(t, f, 1, authz.ViaSession)
	for _, tbl := range []string{"officer_llm_usage", "chief_turn_usage", "officer_judgement_sessions"} {
		if c := shareClasses[tbl]; c.write != noMember {
			t.Errorf("%s's write role is %d; want noMember", tbl, c.write)
		}
		if got := count(t, mgr, f.pool, tbl, "WHERE owner_id = $1", team.ID); got == 0 {
			t.Errorf("control: the team's manager reads none of the team's %s", tbl)
		}
		for _, verb := range []string{"DELETE FROM " + tbl + " WHERE owner_id = $1", "UPDATE " + tbl + " SET owner_id = owner_id WHERE owner_id = $1"} {
			if n, err := rolledBack(mgr, f, verb, team.ID); err != nil || n != 0 {
				t.Errorf("the team's manager: %s → %d rows, %v; want 0", verb, n, err)
			}
		}
		if n, err := rolledBack(authz.System(f.base), f, "DELETE FROM "+tbl+" WHERE owner_id = $1", team.ID); err != nil || n == 0 {
			t.Errorf("control: the system scope deleting the team's %s: %d rows, %v", tbl, n, err)
		}
	}
}
