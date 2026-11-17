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

// The charters these tests put: distinct, and neither a substring of the other.
const (
	charterV1 = "Draft posts and replies only."
	charterV2 = "Draft posts, replies and outreach to people who engaged."
)

// charterRefused decodes out as the charter gate's refusal (charterRefusal),
// failing t unless it is exactly that: a tool error whose backend body has
// code charter_changed and names both versions.
func charterRefused(t *testing.T, what, out string, current, briefed int) {
	t.Helper()
	var got struct {
		Refused struct {
			Status int `json:"status"`
			HQ     struct {
				Code    string `json:"code"`
				Error   string `json:"error"`
				Current int    `json:"charter_version"`
				Briefed int    `json:"briefed_charter_version"`
			} `json:"hq"`
		} `json:"refused"`
	}
	if !isToolErr(out) || json.Unmarshal([]byte(out), &got) != nil {
		t.Fatalf("%s: %s, want the charter_changed refusal", what, out)
	}
	r := got.Refused
	if r.Status != 409 || r.HQ.Code != builtin.CharterChanged || r.HQ.Current != current || r.HQ.Briefed != briefed ||
		!strings.Contains(r.HQ.Error, officers.ToolBrief) {
		t.Fatalf("%s: %s, want 409 %s naming v%d (briefed v%d) and %s", what, out, builtin.CharterChanged, current, briefed, officers.ToolBrief)
	}
}

// claimedID is the item id of a successful claim's answer (0 if none).
func claimedID(out string) int64 {
	var got struct {
		Item *struct {
			ID int64 `json:"id"`
		} `json:"item"`
	}
	if isToolErr(out) || json.Unmarshal([]byte(out), &got) != nil || got.Item == nil {
		return 0
	}
	return got.Item.ID
}

// itemState is item id's status, holder and attempts in the store.
func itemState(t *testing.T, f *connFixture, id int64) (status, holder string, attempts int) {
	t.Helper()
	if err := f.pool.QueryRow(dbtest.AsOperator(context.Background()), `SELECT status, coalesce(claimed_by, ''), attempts FROM officer_items WHERE id = $1`, id).
		Scan(&status, &holder, &attempts); err != nil {
		t.Fatal(err)
	}
	return status, holder, attempts
}

// charterFixture declares builtinOfficerID on charter v1 with two items
// queued (first enqueued first, so claim-next takes it first) and starts
// its instance 1.
func charterFixture(t *testing.T) (f *connFixture, srv *Server, store *builtin.Store, a agents.Agent, first, second int64) {
	t.Helper()
	f = newConnFixture(t)
	ctx := dbtest.AsOperator(context.Background())
	srv, store = builtinOfficerServer(t, f, `[{"id":"`+builtinOfficerID+`","backend":"builtin"}]`, &bytes.Buffer{})
	if err := store.PutType(ctx, 1, builtin.Type{Key: "marketing", Name: "Marketing", Charter: charterV1}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutDef(ctx, 1, builtin.Def{Officer: builtinOfficerID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	for i, subject := range []string{"v1.2.0", "v1.3.0"} {
		id, created, err := store.Enqueue(ctx, 1, builtin.NewItem{Officer: builtinOfficerID, Kind: "release.post", SubjectKey: subject}, time.Now())
		if err != nil || !created {
			t.Fatalf("enqueue: %v, %v", created, err)
		}
		if i == 0 {
			first = id
		} else {
			second = id
		}
	}
	a = startPoolInstance(t, f, srv, builtinOfficerID, "", 1, "fixture-charter-hooks-token")
	return f, srv, store, a, first, second
}

// briefInstance calls officer_brief as a, as the kickoff's step 1 does: a
// builtin instance's claims are refused until it has (officers.CharterGated).
func briefInstance(t *testing.T, srv *Server, a agents.Agent) {
	t.Helper()
	if out := srv.dispatchAgentTool(dbtest.AsOperator(context.Background()), a, officers.ToolBrief, `{}`); isToolErr(out) {
		t.Fatalf("brief: %s", out)
	}
}

// briefedWith calls officer_brief as a, failing t unless it answers charter
// at charter_version version.
func briefedWith(t *testing.T, srv *Server, a agents.Agent, charter string, version int) {
	t.Helper()
	out := srv.dispatchAgentTool(dbtest.AsOperator(context.Background()), a, officers.ToolBrief, `{}`)
	var got struct {
		Type struct {
			Charter string `json:"charter"`
			Version int    `json:"charter_version"`
		} `json:"type"`
	}
	if isToolErr(out) || json.Unmarshal([]byte(out), &got) != nil || got.Type.Charter != charter || got.Type.Version != version {
		t.Fatalf("brief: %s, want charter %q at v%d", out, charter, version)
	}
}

// TestABuiltinInstanceCannotWorkOnAStaleCharter is the live bug: an instance
// briefed at v1 kept claiming (and completing) for days after its type's
// charter was re-put, because nothing told it. Now, once the type is at v2:
// its claim-next and claim by id are refused charter_changed with nothing
// claimed, its complete of the item it took under v1 is refused (the item
// stays claimed, its heartbeat still renews it), and one officer_brief —
// which hands it v2 — clears all three.
func TestABuiltinInstanceCannotWorkOnAStaleCharter(t *testing.T) {
	f, srv, store, a, first, second := charterFixture(t)
	ctx := dbtest.AsOperator(context.Background())

	briefedWith(t, srv, a, charterV1, 1)
	// briefed with the current version: the claim proceeds
	if got := claimedID(srv.dispatchAgentTool(ctx, a, officers.ToolClaim, `{}`)); got != first {
		t.Fatalf("claim-next at the briefed version took item %d, want %d", got, first)
	}

	if err := store.PutType(ctx, 1, builtin.Type{Key: "marketing", Name: "Marketing", Charter: charterV2}); err != nil {
		t.Fatal(err)
	}

	charterRefused(t, "claim-next after the charter was re-put", srv.dispatchAgentTool(ctx, a, officers.ToolClaim, `{}`), 2, 1)
	if st, holder, n := itemState(t, f, second); st != officers.StatusOpen || holder != "" || n != 0 {
		t.Fatalf("after the refused claim-next item %d is %s held by %q (%d attempts), want open, untouched", second, st, holder, n)
	}
	charterRefused(t, "claim by id after the charter was re-put",
		srv.dispatchAgentTool(ctx, a, officers.ToolClaim, `{"item_id":"`+strconv.FormatInt(second, 10)+`"}`), 2, 1)

	held := strconv.FormatInt(first, 10)
	charterRefused(t, "complete of the item claimed under v1",
		srv.dispatchAgentTool(ctx, a, officers.ToolComplete, `{"item_id":"`+held+`","outcome":"done","summary":"posted"}`), 2, 1)
	if st, holder, _ := itemState(t, f, first); st != officers.StatusClaimed || holder != a.Name {
		t.Fatalf("after the refused complete item %d is %s held by %q, want still claimed by %s", first, st, holder, a.Name)
	}
	if out := srv.dispatchAgentTool(ctx, a, officers.ToolHeartbeat, `{"item_id":"`+held+`"}`); isToolErr(out) {
		t.Fatalf("heartbeat while stale: %s, want the lease renewed (heartbeat is not gated)", out)
	}

	// the re-brief hands it v2, and the same calls proceed
	briefedWith(t, srv, a, charterV2, 2)
	if out := srv.dispatchAgentTool(ctx, a, officers.ToolComplete, `{"item_id":"`+held+`","outcome":"done","summary":"posted"}`); isToolErr(out) {
		t.Fatalf("complete after the re-brief: %s", out)
	}
	if got := claimedID(srv.dispatchAgentTool(ctx, a, officers.ToolClaim, `{}`)); got != second {
		t.Fatalf("claim-next after the re-brief took item %d, want %d", got, second)
	}
}

// TestABuiltinInstanceNeverBriefedIsRefusedItsClaim: an instance with no
// record — never briefed, or briefed by an image before migration 0140 (the
// live instance) — is briefed version 0, so its first claim is refused until
// it calls officer_brief.
func TestABuiltinInstanceNeverBriefedIsRefusedItsClaim(t *testing.T) {
	_, srv, _, a, first, _ := charterFixture(t)
	ctx := dbtest.AsOperator(context.Background())
	charterRefused(t, "claim-next never briefed", srv.dispatchAgentTool(ctx, a, officers.ToolClaim, `{}`), 1, 0)
	briefedWith(t, srv, a, charterV1, 1)
	if got := claimedID(srv.dispatchAgentTool(ctx, a, officers.ToolClaim, `{}`)); got != first {
		t.Fatalf("claim-next after the brief took item %d, want %d", got, first)
	}
}

// TestABuiltinInstanceBriefOutlivesScaleToZeroAndRestart: the record is in
// muster's database, keyed by the instance's name, so (a) an instance scaled
// to zero and woken, on a muster that restarted in between, claims at the
// version it was briefed with without a second brief; and (b) a charter put
// while it is scaled to zero is refused on the very next wake's claim.
func TestABuiltinInstanceBriefOutlivesScaleToZeroAndRestart(t *testing.T) {
	f, srv, store, a, first, second := charterFixture(t)
	ctx := dbtest.AsOperator(context.Background())
	briefedWith(t, srv, a, charterV1, 1)

	o, err := srv.officerRT.reg.Get(builtinOfficerID)
	if err != nil {
		t.Fatal(err)
	}
	fleet := officerFleet{s: srv}
	if err := fleet.Stop(ctx, o, 1); err != nil {
		t.Fatalf("scale to zero: %v", err)
	}
	// a restarted muster: a new server on the same database (tables kept)
	srv2 := restartedBuiltinServer(t, f, store)
	if err := (officerFleet{s: srv2}).Start(ctx, o, 1, officers.KickoffText(o, 1)); err != nil {
		t.Fatalf("wake: %v", err)
	}
	if got := claimedID(srv2.dispatchAgentTool(ctx, a, officers.ToolClaim, `{}`)); got != first {
		t.Fatalf("claim-next after scale-to-zero and a restart, same version: took item %d, want %d", got, first)
	}
	if out := srv2.dispatchAgentTool(ctx, a, officers.ToolRelease, `{"item_id":"`+strconv.FormatInt(first, 10)+`"}`); isToolErr(out) {
		t.Fatalf("release: %s", out)
	}

	if err := (officerFleet{s: srv2}).Stop(ctx, o, 1); err != nil {
		t.Fatalf("scale to zero: %v", err)
	}
	if err := store.PutType(ctx, 1, builtin.Type{Key: "marketing", Name: "Marketing", Charter: charterV2}); err != nil {
		t.Fatal(err)
	}
	if err := (officerFleet{s: srv2}).Start(ctx, o, 1, officers.RepromptText(o, 1)); err != nil {
		t.Fatalf("wake: %v", err)
	}
	charterRefused(t, "the first claim after a charter put while scaled to zero", srv2.dispatchAgentTool(ctx, a, officers.ToolClaim, `{}`), 2, 1)
	if st, _, _ := itemState(t, f, second); st != officers.StatusOpen {
		t.Fatalf("item %d is %s after the refused claim, want open", second, st)
	}
}

// restartedBuiltinServer is builtinOfficerServer's server again on the same
// database WITHOUT emptying the officer tables: muster restarted.
func restartedBuiltinServer(t *testing.T, f *connFixture, store *builtin.Store) *Server {
	t.Helper()
	list, err := officers.Parse(`[{"id":"`+builtinOfficerID+`","backend":"builtin"}]`, func(string) string { return "" }, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	s := New(nil, AuthConfig{HookToken: connTestHookToken, ServiceToken: connTestServiceToken, UIPassword: connTestPassword}, log.New(&bytes.Buffer{}, "", 0))
	s.UseExtensions(Extensions{Agents: agents.NewPG(f.pool), Connectors: f.store, Provisioner: stubProvisioner{}, Kinds: stubKinds{pick: "acct-one"},
		Attention: attention.NewPG(f.pool)})
	s.EnableOfficerStore(store)
	if _, err := s.EnableOfficers(OfficersConfig{Officers: list, InstallSession: discardSession, Builtin: store}); err != nil {
		t.Fatal(err)
	}
	return s
}
