package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
)

// officerAsks is owner 1's open Attention asks and notices a builtin officer
// raised.
func officerAsks(t *testing.T, s *Server) []attention.Ask {
	t.Helper()
	open, err := s.ext.Attention.Open(s.attentionPrincipal(context.Background(), 1))
	if err != nil {
		t.Fatal(err)
	}
	var out []attention.Ask
	for _, a := range open {
		if strings.HasPrefix(a.Asker.Label, "officer ") {
			out = append(out, a)
		}
	}
	return out
}

// defineBuiltin puts the marketing type and officer (mode) in the store.
func defineBuiltin(t *testing.T, store *builtin.Store, mode string) {
	t.Helper()
	ctx := dbtest.AsOperator(context.Background())
	if err := store.PutType(ctx, 1, builtin.Type{Key: "marketing", Name: "Marketing", Charter: "Draft posts; never post."}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutDef(ctx, 1, builtin.Def{Officer: builtinOfficerID, Enabled: true, Mode: mode}); err != nil {
		t.Fatal(err)
	}
}

// TestABuiltinOfficersAskReachesTheOwnerAndTheAnswerComesBack is slice 6's
// ask path through the instance's own tool: officer_ask_person files ONE
// Attention ask for the officer's owner (muster's own: no session waits on
// it) and sets the item waiting; the owner answers on the Attention view; the
// sweep re-opens the item with the answer on it, which the next claim reads.
func TestABuiltinOfficersAskReachesTheOwnerAndTheAnswerComesBack(t *testing.T) {
	f := newConnFixture(t)
	srv, store := builtinOfficerServer(t, f, `[{"id":"`+builtinOfficerID+`","backend":"builtin"}]`, &bytes.Buffer{})
	defineBuiltin(t, store, "live")
	ctx := dbtest.AsOperator(context.Background())
	id, _, err := store.Enqueue(ctx, 1, builtin.NewItem{Officer: builtinOfficerID, Kind: "release.post", SubjectKey: "v2"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	a := startPoolInstance(t, f, srv, builtinOfficerID, "", 1, "fixture-builtin-ask-token")
	listed := false
	for _, d := range srv.MCPToolDefsFor(ctx, a) {
		listed = listed || d.Name == officers.ToolAskPerson
	}
	if !listed {
		t.Fatal("a builtin officer is not offered officer_ask_person though its backend serves asks")
	}
	if out := srv.dispatchAgentTool(ctx, a, officers.ToolClaim, `{}`); isToolErr(out) {
		t.Fatalf("claim: %s", out)
	}
	item := strconv.FormatInt(id, 10)
	out := srv.dispatchAgentTool(ctx, a, officers.ToolAskPerson, `{"item_id":"`+item+`","to":"owner","question":"Post the v2 thread as drafted?","options":["yes","no"]}`)
	if isToolErr(out) {
		t.Fatalf("ask: %s", out)
	}
	asks := officerAsks(t, srv)
	if len(asks) != 1 || !asks[0].Asker.System || asks[0].Kind == attention.KindNotice ||
		!strings.Contains(asks[0].Question, "Post the v2 thread as drafted?") || strings.Join(asks[0].Options, ",") != "yes,no" {
		t.Fatalf("Attention after the ask: %+v, want one muster-raised question with the officer's text and options", asks)
	}
	srv.SweepOfficerAttention(context.Background())
	var status string
	_ = f.pool.QueryRow(ctx, `SELECT status FROM officer_items WHERE id = $1`, id).Scan(&status)
	if status != officers.StatusWaiting {
		t.Fatalf("with the ask unanswered, a sweep left the item %s, want waiting", status)
	}
	if _, err := srv.ext.Attention.Answer(srv.attentionPrincipal(context.Background(), 1), asks[0].ID, "yes"); err != nil {
		t.Fatal(err)
	}
	srv.SweepOfficerAttention(context.Background())
	claim := srv.dispatchAgentTool(ctx, a, officers.ToolClaim, `{}`)
	if isToolErr(claim) || !strings.Contains(claim, `"answer":"yes"`) || !strings.Contains(claim, `"id":`+item) {
		t.Fatalf("claim after the owner answered: %s, want item %s with the answer on its asks", claim, item)
	}
}

// TestABuiltinEscalationNotifiesEachRecipientOnceAndNobodyInShadow: an
// escalating complete becomes one answerable Attention ask per recipient
// however many sweeps run (two here, as two replicas would), and none at all
// for a shadow officer; the owner's answer sends the item back to the
// officer, whose next claim reads it.
func TestABuiltinEscalationNotifiesEachRecipientOnceAndNobodyInShadow(t *testing.T) {
	for _, mode := range []string{"live", "shadow"} {
		f := newConnFixture(t)
		srv, store := builtinOfficerServer(t, f, `[{"id":"`+builtinOfficerID+`","backend":"builtin"}]`, &bytes.Buffer{})
		defineBuiltin(t, store, mode)
		ctx := dbtest.AsOperator(context.Background())
		id, _, err := store.Enqueue(ctx, 1, builtin.NewItem{Officer: builtinOfficerID, Kind: "release.post", SubjectKey: "v3-" + mode}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		a := startPoolInstance(t, f, srv, builtinOfficerID, "", 1, "fixture-builtin-esc-token-"+mode)
		if out := srv.dispatchAgentTool(ctx, a, officers.ToolClaim, `{}`); isToolErr(out) {
			t.Fatalf("%s: claim: %s", mode, out)
		}
		item := strconv.FormatInt(id, 10)
		if out := srv.dispatchAgentTool(ctx, a, officers.ToolComplete, `{"item_id":"`+item+`","outcome":"escalated","summary":"the draft names an unreleased feature"}`); isToolErr(out) {
			t.Fatalf("%s: complete: %s", mode, out)
		}
		srv.SweepOfficerAttention(context.Background())
		srv.SweepOfficerAttention(context.Background())
		got := officerAsks(t, srv)
		if mode == "shadow" {
			if len(got) != 0 {
				t.Fatalf("shadow: %d Attention entries for an escalation, want none", len(got))
			}
			continue
		}
		if len(got) != 1 || got[0].Kind == attention.KindNotice || !strings.Contains(got[0].Question, "the draft names an unreleased feature") ||
			!strings.Contains(got[0].Question, "escalated item "+item) || !strings.HasSuffix(got[0].Question, officerEscalationTail) {
			t.Fatalf("live: Attention after two sweeps: %+v, want exactly one answerable ask carrying the summary", got)
		}
		// The owner's answer flows back: the item re-opens for the officer
		// with the answer on it, and the next claim reads it.
		if _, err := srv.ext.Attention.Answer(srv.attentionPrincipal(context.Background(), 1), got[0].ID, "drop the feature name and post"); err != nil {
			t.Fatal(err)
		}
		srv.SweepOfficerAttention(context.Background())
		claim := srv.dispatchAgentTool(ctx, a, officers.ToolClaim, `{}`)
		if isToolErr(claim) || !strings.Contains(claim, `"id":`+item) || !strings.Contains(claim, "drop the feature name and post") {
			t.Fatalf("claim after the owner answered the escalation: %s, want item %s with the answer", claim, item)
		}
	}
}

// adminCall serves one admin API request with the shared hook token (the
// operator) through the full handler: the auth wrapper and the routes.
func adminCall(t *testing.T, h http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+connTestHookToken)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// TestTheAdminAPIDefinesAnOfficerAndFeedsItsQueue is slice 7's closing
// condition: the admin API (what `muster officer` calls) puts a type and an
// officer and enqueues an item, which that builtin officer's instance then
// claims; the item is listed and resolved through the API; a type an officer
// names is refused deletion, a bad id is a 400, and with the store off every
// route is a 404.
func TestTheAdminAPIDefinesAnOfficerAndFeedsItsQueue(t *testing.T) {
	f := newConnFixture(t)
	srv, _ := builtinOfficerServer(t, f, `[{"id":"`+builtinOfficerID+`","backend":"builtin"}]`, &bytes.Buffer{})
	h := srv.Handler()
	if code, out := adminCall(t, h, "PUT", "/api/officers/types/marketing", `{"name":"Marketing","charter":"Draft; never post."}`); code != 200 || out["charter_version"] != float64(1) {
		t.Fatalf("put type: %d %v", code, out)
	}
	if code, out := adminCall(t, h, "PUT", "/api/officers/types/marketing", `{"name":"Marketing","charter":"Draft; never post. v2"}`); code != 200 || out["charter_version"] != float64(2) {
		t.Fatalf("re-put with a changed charter: %d %v, want version 2", code, out)
	}
	if code, out := adminCall(t, h, "PUT", "/api/officers/defs/"+builtinOfficerID, `{}`); code != 200 || out["enabled"] != true || out["declared"] != true {
		t.Fatalf("put def: %d %v, want enabled and declared", code, out)
	}
	if code, out := adminCall(t, h, "PUT", "/api/officers/defs/"+builtinOfficerID, `{"escalate_to":[987654]}`); code != http.StatusBadRequest || out["missing"] == nil {
		t.Fatalf("escalate_to naming no user: %d %v, want 400 naming it", code, out)
	}
	if code, _ := adminCall(t, h, "PUT", "/api/officers/defs/nosuchtype:x", `{}`); code != http.StatusNotFound {
		t.Fatalf("an officer of an undefined type: %d, want 404", code)
	}
	if code, _ := adminCall(t, h, "PUT", "/api/officers/types/Bad_Key", `{"charter":"x"}`); code != http.StatusBadRequest {
		t.Fatalf("a malformed type key: %d, want 400", code)
	}
	code, out := adminCall(t, h, "POST", "/api/officers/defs/"+builtinOfficerID+"/items", `{"kind":"release.post","subject_key":"v4","payload":{"tag":"v4"}}`)
	if code != http.StatusCreated || out["created"] != true {
		t.Fatalf("enqueue: %d %v", code, out)
	}
	id := int64(out["id"].(float64))
	if code, out := adminCall(t, h, "POST", "/api/officers/defs/"+builtinOfficerID+"/items", `{"kind":"release.post","subject_key":"v4"}`); code != 200 || out["created"] != false || int64(out["id"].(float64)) != id {
		t.Fatalf("a second enqueue of the live subject: %d %v, want the same item, not created", code, out)
	}
	ctx := dbtest.AsOperator(context.Background())
	a := startPoolInstance(t, f, srv, builtinOfficerID, "", 1, "fixture-builtin-admin-token")
	claim := srv.dispatchAgentTool(ctx, a, officers.ToolClaim, `{}`)
	if isToolErr(claim) || !strings.Contains(claim, `"id":`+strconv.FormatInt(id, 10)) || !strings.Contains(claim, `"tag":"v4"`) {
		t.Fatalf("the instance's claim: %s, want the item the admin API enqueued", claim)
	}
	if code, out := adminCall(t, h, "POST", "/api/officers/defs/"+builtinOfficerID+"/items/"+strconv.FormatInt(id, 10)+"/resolve", `{"outcome":"done"}`); code != http.StatusConflict {
		t.Fatalf("resolve while the instance holds it: %d %v, want 409", code, out)
	}
	if code, out := adminCall(t, h, "GET", "/api/officers/defs/"+builtinOfficerID+"/items?status=claimed", ""); code != 200 || len(out["items"].([]any)) != 1 {
		t.Fatalf("list claimed: %d %v", code, out)
	}
	if code, _ := adminCall(t, h, "DELETE", "/api/officers/types/marketing", ""); code != http.StatusConflict {
		t.Fatalf("delete a type an officer names: %d, want 409", code)
	}
	if code, out := adminCall(t, h, "GET", "/api/officers/types/marketing", ""); code != 200 || out["charter"] != "Draft; never post. v2" {
		t.Fatalf("get type: %d %v", code, out)
	}

	off := New(nil, AuthConfig{HookToken: connTestHookToken, ServiceToken: connTestServiceToken, UIPassword: connTestPassword}, srv.logger)
	if code, _ := adminCall(t, off.Handler(), "GET", "/api/officers/types", ""); code != http.StatusNotFound {
		t.Fatalf("store off: %d, want 404", code)
	}
}

// TestTheAdminAPIIsOwnerScoped: what one user puts, another neither lists
// nor reads nor deletes — the store is called with the caller's owner AND
// under its principal.
func TestTheAdminAPIIsOwnerScoped(t *testing.T) {
	f := newConnFixture(t)
	srv, _ := builtinOfficerServer(t, f, `[]`, &bytes.Buffer{})
	dbtest.OwnerExec(t, dbtest.DSN(t), `INSERT INTO users (id, role) OVERRIDING SYSTEM VALUE VALUES (2, 'member') ON CONFLICT DO NOTHING`)
	if code, _ := adminCall(t, srv.Handler(), "PUT", "/api/officers/types/marketing", `{"charter":"operator's"}`); code != 200 {
		t.Fatalf("operator put: %d", code)
	}
	as2 := func(method, path string, h http.HandlerFunc) (int, string) {
		req := httptest.NewRequest(method, path, nil)
		req.SetPathValue("key", "marketing")
		req = req.WithContext(srv.attentionPrincipal(context.Background(), 2))
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec.Code, rec.Body.String()
	}
	if code, body := as2("GET", "/api/officers/types", srv.handleOfficerTypes); code != 200 || strings.Contains(body, "marketing") {
		t.Fatalf("user 2 lists %d %s, want none of the operator's types", code, body)
	}
	if code, _ := as2("GET", "/api/officers/types/marketing", srv.handleOfficerTypeGet); code != http.StatusNotFound {
		t.Fatalf("user 2 reads the operator's type: %d, want 404", code)
	}
	if code, _ := as2("DELETE", "/api/officers/types/marketing", srv.handleOfficerTypeDelete); code != http.StatusNotFound {
		t.Fatalf("user 2 deletes the operator's type: %d, want 404", code)
	}
	if code, out := adminCall(t, srv.Handler(), "GET", "/api/officers/types/marketing", ""); code != 200 || out["charter"] != "operator's" {
		t.Fatalf("the operator's type after user 2's attempts: %d %v", code, out)
	}
}

// TestAnEscalationNobodyCanReceiveDoesNotHoldTheQueue: an escalation entry
// for a user id that does not exist (written past the admin API's check, as
// a deleted user would leave it) is given up by the sweep — and the owner's
// entry on a later item is still raised in the same sweep.
func TestAnEscalationNobodyCanReceiveDoesNotHoldTheQueue(t *testing.T) {
	f := newConnFixture(t)
	srv, store := builtinOfficerServer(t, f, `[{"id":"`+builtinOfficerID+`","backend":"builtin"}]`, &bytes.Buffer{})
	defineBuiltin(t, store, "live")
	ctx := dbtest.AsOperator(context.Background())
	bad, _, err := store.Enqueue(ctx, 1, builtin.NewItem{Officer: builtinOfficerID, Kind: "k", SubjectKey: "bad"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	good, _, err := store.Enqueue(ctx, 1, builtin.NewItem{Officer: builtinOfficerID, Kind: "k", SubjectKey: "good"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE officer_items SET status = 'escalated', outcome = 'escalated', summary = 'x',
		notices = CASE WHEN id = $1 THEN '[{"recipient": 987654, "suppressed": false, "attention_id": null, "settled": false}]'::jsonb
			ELSE '[{"recipient": 1, "suppressed": false, "attention_id": null, "settled": false}]'::jsonb END
		WHERE id IN ($1, $2)`, bad, good); err != nil {
		t.Fatal(err)
	}
	srv.SweepOfficerAttention(context.Background())
	var badN string
	_ = f.pool.QueryRow(ctx, `SELECT notices::text FROM officer_items WHERE id = $1`, bad).Scan(&badN)
	if !strings.Contains(badN, `"suppressed": true`) {
		t.Fatalf("the unreceivable escalation after a sweep: %s, want it given up", badN)
	}
	if got := officerAsks(t, srv); len(got) != 1 || !strings.Contains(got[0].Question, "escalated item "+strconv.FormatInt(good, 10)) {
		t.Fatalf("the owner's escalation behind it: %+v, want it raised in the same sweep", got)
	}
}
