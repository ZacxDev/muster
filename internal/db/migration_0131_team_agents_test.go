package db_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/db"
)

// teamAgentsVersion is 0131_team_agents.sql's number.
// 🔴 RENUMBER IT WITH THE FILE (and db.teamAgentsMigrationVersion) at merge.
const teamAgentsVersion = 131

// TestMigration0131TeamAgents: just below 0131 agents is personal (owner_isolation
// only) and a team agent's row holds the credentials an older image wrote (the
// control); 0131 moves them to agent_secrets, blanks the row, shares agents
// with the four team policies and owner_fixed, and from then on every write of
// a team agent's credential into the row is forced to ” while a person's is
// kept. The header's ROLLBACK runs as written and restores owner_isolation;
// the body applies again after it; --rls-off then --rls-on leaves agents
// shared (db.RLSOn re-applies 0131 after 0007).
func TestMigration0131TeamAgents(t *testing.T) {
	ctx := ctx0006(t)
	// The migration just below 0131 that this tree embeds: 0130 is phase 3's
	// (#668), which may not be merged yet.
	ms, err := db.EmbeddedMigrationsForTest()
	if err != nil {
		t.Fatal(err)
	}
	prev := 0
	for _, m := range ms {
		if m.Version < teamAgentsVersion && m.Version > prev {
			prev = m.Version
		}
	}
	pool := freshAt(ctx, t, "ms_0131_team_agents", prev)
	policies := func() string {
		rows, err := pool.Query(ctx, `SELECT policyname::text FROM pg_policies WHERE schemaname='public' AND tablename='agents'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				t.Fatal(err)
			}
			out = append(out, n)
		}
		sort.Strings(out)
		return strings.Join(out, ",")
	}
	if got := policies(); got != "owner_isolation" {
		t.Fatalf("control: below 0131 agents has policies %s; want owner_isolation", got)
	}
	team := scanID(ctx, t, pool, `INSERT INTO users (kind, role, display_name) VALUES ('team', 'member', 'mig team') RETURNING id`)
	person := scanID(ctx, t, pool, `INSERT INTO users (role, display_name) VALUES ('member', 'mig person') RETURNING id`)
	teamAgent := scanID(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace, hooks_token, gateway_secret)
		VALUES ($1, 'mig-team', 'ns-mig-team', 'tok-team-old', 'gw-team-old') RETURNING id`, team)
	personAgent := scanID(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace, hooks_token)
		VALUES ($1, 'mig-person', 'ns-mig-person', 'tok-person') RETURNING id`, person)
	row := func(id int64) (string, string) {
		var h, g string
		if err := pool.QueryRow(ctx, `SELECT hooks_token, gateway_secret FROM agents WHERE id=$1`, id).Scan(&h, &g); err != nil {
			t.Fatal(err)
		}
		return h, g
	}
	if h, g := row(teamAgent); h != "tok-team-old" || g != "gw-team-old" {
		t.Fatalf("control: below 0131 the team agent's row holds %q/%q", h, g)
	}

	applyThroughFrom(ctx, t, pool, teamAgentsVersion)

	if got := policies(); got != "team_delete,team_insert,team_read,team_update" {
		t.Errorf("at 0131 agents has policies %s; want the four team policies", got)
	}
	if h, g := row(teamAgent); h != "" || g != "" {
		t.Errorf("after 0131 the team agent's row still holds hooks_token %v / gateway_secret %v", h != "", g != "")
	}
	var st, sg string
	if err := pool.QueryRow(ctx, `SELECT hooks_token, gateway_secret FROM agent_secrets WHERE agent_id=$1`, teamAgent).Scan(&st, &sg); err != nil ||
		st != "tok-team-old" || sg != "gw-team-old" {
		t.Errorf("after 0131 agent_secrets for the team agent: %q/%q (%v); want the credentials moved there", st, sg, err)
	}
	if h, _ := row(personAgent); h != "tok-person" {
		t.Errorf("0131 changed a person's agent's dual-written token: %q", h)
	}
	// From now on: the row's credential columns are forced to '' for a team
	// owner, on insert and on update; a person's are kept.
	mustExec(ctx, t, pool, `UPDATE agents SET hooks_token='tok-team-new', gateway_secret='gw-team-new' WHERE id=$1`, teamAgent)
	if h, g := row(teamAgent); h != "" || g != "" {
		t.Errorf("an UPDATE wrote a team agent's credential into its row: %v/%v", h != "", g != "")
	}
	teamAgent2 := scanID(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace, hooks_token)
		VALUES ($1, 'mig-team-2', 'ns-mig-team-2', 'tok-team-2') RETURNING id`, team)
	if h, _ := row(teamAgent2); h != "" {
		t.Errorf("an INSERT wrote a team agent's credential into its row")
	}
	mustExec(ctx, t, pool, `UPDATE agents SET hooks_token='tok-person-2' WHERE id=$1`, personAgent)
	if h, _ := row(personAgent); h != "tok-person-2" {
		t.Errorf("control: a person's agent's token update was not kept: %q", h)
	}

	for _, q := range asksRollbackSQL(t, "0131_team_agents.sql") {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("rollback %q: %v", q, err)
		}
	}
	if got := policies(); got != "owner_isolation" {
		t.Errorf("after the rollback agents has policies %s; want owner_isolation", got)
	}
	var leftovers int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_proc WHERE proname IN ('muster_owner_is_team', 'muster_agents_team_no_secret'))
		+ (SELECT count(*) FROM pg_trigger WHERE tgrelid = 'public.agents'::regclass AND tgname IN ('owner_fixed', 'agents_team_no_secret'))`).Scan(&leftovers); err != nil {
		t.Fatal(err)
	}
	if leftovers != 0 {
		t.Errorf("after the rollback %d functions or triggers of 0131 remain", leftovers)
	}
	applyThroughFrom(ctx, t, pool, teamAgentsVersion)
	if got := policies(); got != "team_delete,team_insert,team_read,team_update" {
		t.Errorf("re-applied after the rollback: agents has policies %s", got)
	}
	if err := db.RLSOff(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := db.RLSOn(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if got := policies(); got != "team_delete,team_insert,team_read,team_update" {
		t.Errorf("after --rls-off then --rls-on agents has policies %s; want the four team policies (RLSOn must re-apply 0131 after 0007)", got)
	}
}
