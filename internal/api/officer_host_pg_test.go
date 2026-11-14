package api

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
	"github.com/ZacxDev/muster/internal/plugins"
)

// HOST OFFICERS OVER POSTGRES (officer_host.go; docs/design/officers-on-hosts.md):
// the Compose shape — no provisioner, no kinds, a mirror hub — with the real
// agents, builtin officer, plugin and connector stores. The instance is started
// through the host fleet (its agent row created, never provisioned) and every
// tool is called through the mirror door's binding: its recorded session, in
// its recorded window.

// TestAHostOfficersDraftGoesThroughApprovalAndThePlugin: a host instance claims
// an item and files officer_post_draft for it; the draft is filed against the
// instance's AGENT ROW (the drafts table requires one); it waits for the
// owner's approval of the exact text; the bound plugin claims it, posts and
// reports; the item completes with the post's reference. The instance cannot
// approve its own draft through the mirror door.
func TestAHostOfficersDraftGoesThroughApprovalAndThePlugin(t *testing.T) {
	w := newHostOutboundWorld(t, true)
	if w.agent.Provisioned() || w.agent.RunsOnHost != hostOffHost || w.agent.Namespace != "" || w.agent.Status != "stopped" {
		t.Fatalf("the host instance's row = %+v", w.agent)
	}
	item, _, err := w.store.Enqueue(w.ctx, 1, builtin.NewItem{Officer: builtinOfficerID, Kind: "plugin.reply", SubjectKey: "host:1"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if out := w.call(officers.ToolClaim, `{}`); isToolErr(out) {
		t.Fatalf("claim: %s", out)
	}
	args := `{"plugin":"` + outboundPluginName + `","venue":"example.forum","target":{"kind":"post"},"body":"A post from a host.","item_id":"` +
		strconv.FormatInt(item, 10) + `"}`
	id, out := w.postDraft(args)
	if id == 0 {
		t.Fatalf("file: %s", out)
	}
	var agentID int64
	if err := w.f.pool.QueryRow(w.ctx, `SELECT agent_id FROM plugin_reply_drafts WHERE id = $1`, id).Scan(&agentID); err != nil || agentID != w.agent.ID {
		t.Fatalf("the draft's agent = %d (%v), want the host instance's row %d", agentID, err, w.agent.ID)
	}
	// The instance cannot decide its own draft through muster.
	if out, ok := w.srv.hostOfficerMirrorTool(w.mirrorCtx(), hostOffHost, w.instanceCaller(), "plugin_draft_decide",
		`{"id":`+strconv.FormatInt(id, 10)+`,"decision":"approve"}`); !ok || !strings.Contains(out, "not one of your owner's sessions") {
		t.Fatalf("the instance approving its own draft: %v %s", ok, out)
	}
	if d := w.draft(id); d.Status == plugins.DraftApproved {
		t.Fatal("the draft is approved without the owner")
	}
	if rec := w.check(w.tok, id, true, ""); rec.Code != 200 {
		t.Fatalf("check: %d %s", rec.Code, rec.Body.String())
	}
	if rec := w.approve(id, bindingForm("A post from a host.")); rec.Code != 303 {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	if rec := w.claim(); rec.Code != 200 {
		t.Fatalf("plugin claim: %d %s", rec.Code, rec.Body.String())
	}
	if rec := w.do("POST", "/plugin/v1/drafts/"+strconv.FormatInt(id, 10)+"/result", `{"ok":true,"external_ref":"post-host-91"}`, bearerTok(w.tok)); rec.Code != 200 {
		t.Fatalf("result: %d %s", rec.Code, rec.Body.String())
	}
	var status, summary string
	if err := w.f.pool.QueryRow(w.ctx, `SELECT status, coalesce(summary, '') FROM officer_items WHERE id = $1`, item).Scan(&status, &summary); err != nil {
		t.Fatal(err)
	}
	if status != officers.StatusDone || !strings.Contains(summary, "post-host-91") {
		t.Fatalf("the item after the post: %s %q", status, summary)
	}
}

// TestAHostInstanceRequestsAConnectorAsItsOwnRow: connector_request from a
// host instance files a PENDING grant for the instance's agent row — the
// owner decides it per request, as for a pod — github and an undeclared
// connector are refused, and the same call from another window of the same
// session is refused before anything is filed. The instance cannot approve its
// own request through the mirror door.
func TestAHostInstanceRequestsAConnectorAsItsOwnRow(t *testing.T) {
	w := newHostOutboundWorld(t, false)
	out := w.call("connector_request", `{"name":"pg-example","reason":"read the product's feedback table"}`)
	if isToolErr(out) {
		t.Fatalf("connector_request: %s", out)
	}
	g, err := w.f.store.LatestGrant(asUser(1), w.agent.ID, "pg-example")
	if err != nil || g.Status != connectors.StatusPending {
		t.Fatalf("the grant for the host row: %+v (%v), want pending", g, err)
	}
	for _, name := range []string{"github", "other-db"} {
		if out := w.call("connector_request", `{"name":"`+name+`","reason":"x"}`); !isToolErr(out) || !strings.Contains(out, "host officer") {
			t.Errorf("connector_request %s: %s, want refused", name, out)
		}
	}
	if out, ok := w.srv.hostOfficerMirrorTool(w.mirrorCtx(), hostOffHost, w.instanceCaller(), "connector_request_decide",
		`{"id":`+strconv.FormatInt(g.ID, 10)+`,"decision":"approve"}`); !ok || !strings.Contains(out, "not one of your owner's sessions") {
		t.Fatalf("the instance approving its own grant: %v %s", ok, out)
	}
	moved := toolCaller{asker: attention.Asker{MirrorHost: hostOffHost, MirrorSession: w.spawn.AgentSession}, mirrorWindow: "@4040"}
	if out, ok := w.srv.hostOfficerMirrorTool(w.mirrorCtx(), hostOffHost, moved, "connector_status", `{"name":"pg-example"}`); !ok || !strings.Contains(out, "is an officer tool") {
		t.Fatalf("connector_status from another window: %v %s, want the binding refusal", ok, out)
	}
	if g2, _ := w.f.store.GetGrant(asUser(1), g.ID); g2.Status != connectors.StatusPending {
		t.Fatalf("the grant is %s, want still pending", g2.Status)
	}
}
