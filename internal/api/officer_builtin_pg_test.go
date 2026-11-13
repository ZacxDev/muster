package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
)

// builtinOfficerID is the builtin officer these tests declare.
const builtinOfficerID = "marketing:example"

// builtinOfficerServer is a server on f's database declaring raw (which must
// hold builtin officers only: no token is in the environment), with the
// built-in store wired as muster-server wires it. The store's officer tables
// are emptied first and after.
func builtinOfficerServer(t *testing.T, f *connFixture, raw string, logs *bytes.Buffer) (*Server, *builtin.Store) {
	t.Helper()
	dsn := dbtest.DSN(t)
	wipe := func() {
		dbtest.OwnerExec(t, dsn, `TRUNCATE officer_schedules, officer_items, officer_defs, officer_types RESTART IDENTITY`)
		dbtest.OwnerExec(t, dsn, `DELETE FROM attention_asks WHERE asker_label LIKE 'officer %'`)
	}
	wipe()
	t.Cleanup(func() {
		wipe()
		_, _ = f.pool.Exec(dbtest.AsOperator(context.Background()), `DELETE FROM agents WHERE name LIKE 'officer-marketing-example%'`)
	})
	list, err := officers.Parse(raw, func(string) string { return "" }, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	store := builtin.NewStore(f.pool)
	s := New(nil, AuthConfig{HookToken: connTestHookToken, ServiceToken: connTestServiceToken, UIPassword: connTestPassword}, log.New(logs, "", 0))
	s.UseExtensions(Extensions{Agents: agents.NewPG(f.pool), Connectors: f.store, Provisioner: stubProvisioner{}, Kinds: stubKinds{pick: "acct-one"},
		Attention: attention.NewPG(f.pool)})
	s.EnableOfficerStore(store)
	if len(list) == 0 {
		return s, store // the empty roster: the store on, no officer declared
	}
	if _, err := s.EnableOfficers(OfficersConfig{Officers: list, InstallSession: discardSession, Builtin: store}); err != nil {
		t.Fatal(err)
	}
	return s, store
}

// TestABuiltinOfficerWorksAnItemEndToEnd is slice 5's closing condition in
// process: an officer declared "backend": "builtin" and defined in the
// built-in store, an item enqueued there, an instance started the way the
// wake loop starts one, and that instance's own officer tools — brief, claim
// (claim-next), heartbeat, complete — served by the built-in backend acting
// as the officer's owner, the item ending done in muster's database. Its
// listing offers exactly what the backend serves.
func TestABuiltinOfficerWorksAnItemEndToEnd(t *testing.T) {
	f := newConnFixture(t)
	logs := &bytes.Buffer{}
	ctx := dbtest.AsOperator(context.Background())
	srv, store := builtinOfficerServer(t, f, `[{"id":"`+builtinOfficerID+`","backend":"builtin"}]`, logs)
	if !strings.Contains(logs.String(), "officers: "+builtinOfficerID+": the built-in store does not answer for it") {
		t.Fatalf("an officer the store does not define is not said so at boot:\n%s", logs)
	}
	const charter = "Draft one post per release; never post anything yourself."
	if err := store.PutType(ctx, 1, builtin.Type{Key: "marketing", Name: "Marketing", Charter: charter}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutDef(ctx, 1, builtin.Def{Officer: builtinOfficerID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	id, created, err := store.Enqueue(ctx, 1, builtin.NewItem{Officer: builtinOfficerID, Kind: "release.post", SubjectKey: "v1.2.0"}, time.Now())
	if err != nil || !created {
		t.Fatalf("enqueue: %v, %v", created, err)
	}
	a := startPoolInstance(t, f, srv, builtinOfficerID, "", 1, "fixture-builtin-hooks-token")

	if b := srv.officerRT.hq.Backend(srv.officerRT.reg.All()[0]); b == nil {
		t.Fatal("no backend")
	} else if _, ok := b.(*builtin.Backend); !ok {
		t.Fatalf("a builtin officer's backend is %T, want *builtin.Backend", b)
	}
	var listed []string
	for _, d := range srv.MCPToolDefsFor(ctx, a) {
		if isOfficerTool(d.Name) {
			listed = append(listed, d.Name)
		}
	}
	core := []string{officers.ToolBrief, officers.ToolItems, officers.ToolClaim, officers.ToolHeartbeat, officers.ToolComplete, officers.ToolRelease, officers.ToolAskPerson}
	if got, want := strings.Join(listed, ","), strings.Join(core, ","); got != want {
		t.Fatalf("a builtin officer is offered %s, want exactly what its backend serves: %s", got, want)
	}

	brief := srv.dispatchAgentTool(ctx, a, officers.ToolBrief, `{}`)
	if isToolErr(brief) || !strings.Contains(brief, charter) {
		t.Fatalf("brief: %s, want the charter from the built-in store", brief)
	}
	claim := srv.dispatchAgentTool(ctx, a, officers.ToolClaim, `{}`)
	var got struct {
		Item struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
		} `json:"item"`
	}
	if isToolErr(claim) || json.Unmarshal([]byte(claim), &got) != nil || got.Item.ID != id || got.Item.Status != officers.StatusClaimed {
		t.Fatalf("claim-next: %s, want item %d claimed", claim, id)
	}
	item := strconv.FormatInt(id, 10)
	if out := srv.dispatchAgentTool(ctx, a, officers.ToolHeartbeat, `{"item_id":"`+item+`"}`); isToolErr(out) {
		t.Fatalf("heartbeat: %s", out)
	}
	if out := srv.dispatchAgentTool(ctx, a, officers.ToolComplete, `{"item_id":"`+item+`","outcome":"done","summary":"drafted"}`); isToolErr(out) {
		t.Fatalf("complete: %s", out)
	}
	var status, outcome, holder string
	if err := f.pool.QueryRow(ctx, `SELECT status, outcome, coalesce(claimed_by, '') FROM officer_items WHERE id = $1`, id).Scan(&status, &outcome, &holder); err != nil {
		t.Fatal(err)
	}
	if status != officers.StatusDone || outcome != "done" || holder != "" {
		t.Fatalf("the item is %s/%s held by %q after complete, want done/done and no holder", status, outcome, holder)
	}
	if out := srv.dispatchAgentTool(ctx, a, officers.ToolClaim, `{}`); !strings.Contains(out, nothingToClaimNote) {
		t.Fatalf("claim-next on an empty queue: %s, want nothing to claim", out)
	}
}

// TestABuiltinOfficerWithNoDatabaseIsRefusedAtBoot: its queue lives in
// muster's database, so EnableOfficers refuses it without the store — it is
// never run against nothing.
func TestABuiltinOfficerWithNoDatabaseIsRefusedAtBoot(t *testing.T) {
	list, err := officers.Parse(`[{"id":"`+builtinOfficerID+`","backend":"builtin"}]`, func(string) string { return "" }, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	store := &officerAgents{}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(&bytes.Buffer{}, "", 0))
	s.UseExtensions(Extensions{Agents: store, Provisioner: &officerProv{store: store}, Gateway: &repromptChat{}, SessionLiveness: stubLiveness{}, Kinds: stubKinds{pick: "acct-one"}})
	_, err = s.EnableOfficers(OfficersConfig{Officers: list, InstallSession: discardSession})
	if err == nil || !strings.Contains(err.Error(), builtinOfficerID) || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("a builtin officer with no store: %v, want a refusal naming it and DATABASE_URL", err)
	}
	if s.officerRT != nil {
		t.Fatal("the officer runtime was left on after the refusal")
	}
}
