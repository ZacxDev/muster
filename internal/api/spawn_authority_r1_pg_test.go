package api

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
)

// Round 1 of the spawned-agent authority change: rotation while a spawned
// agent still runs, route_reply bound to its route's host, the hold card's
// instructions, and an open dispatch older than the newest 200 rows.
// ⚠ These tests name only identifiers that exist before round 1, so the file
// compiles against the earlier head, where each is red for a behavioural
// reason.

// TestRotatingWhileASpawnedAgentRunsKeepsTheHostGated: the host's token is
// rotated (a new one minted, the marked one revoked) while a host dispatch on
// it is still running. A call with the NEW token, naming the owner's own
// session, is still carded — the agent still running there can read the new
// token. Once the dispatch has ended, the same call runs at once.
func TestRotatingWhileASpawnedAgentRunsKeepsTheHostGated(t *testing.T) {
	f := newSpawnFixture(t, false, true)
	f.recordDispatch(t, spawnSID, "@9", "running", 0)
	old := f.ws
	f.ws, f.tokID = f.mintWS(t)
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE mirror_tokens SET revoked_at = now() WHERE token_hash = $1`, mirror.HashToken(old)); err != nil {
		t.Fatal(err)
	}
	id := f.newConnectorRequest(t, "svc-rot-open")
	text, isErr := f.decide(t, f.ws, focusSID, id, "approve")
	f.notTaken(t, "rotated during an open dispatch", id, text, isErr)
	if !strings.Contains(text, spawnPending) {
		t.Fatalf("rotated during an open dispatch: %s, want a confirmation card", text)
	}

	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE host_dispatches SET status = 'ended', ended_at = now()
		WHERE owner_id = 1 AND host = 'ws' AND status <> 'ended'`); err != nil {
		t.Fatal(err)
	}
	f.dropSpawnWindow()
	id = f.newConnectorRequest(t, "svc-rot-ended")
	text, isErr = f.decide(t, f.ws, focusSID, id, "approve")
	if g := f.grantRow(t, id); isErr || strings.Contains(text, spawnPending) || g.status != "approved" {
		t.Fatalf("rotated, its dispatches ended: %s (grant %+v), want it approved at once", text, g)
	}
}

// TestAnOpenDispatchOlderThanTheNewest200IsStillFound: the window clause of
// the owner-authority check sees an OPEN dispatch however many rows the host
// has: one open row (its session not recorded yet), then 205 newer ended
// ones; a caller in the open row's window is refused.
func TestAnOpenDispatchOlderThanTheNewest200IsStillFound(t *testing.T) {
	f := newSpawnFixture(t, false, false)
	f.setSpawnTag(0)
	f.recordDispatch(t, "", "@9", "running", 0)
	for i := 0; i < 205; i++ {
		f.recordDispatch(t, "", "", "ended", time.Minute)
	}
	ownerCtx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaMirrorHost})
	in9 := toolCaller{asker: attention.Asker{MirrorHost: "ws", MirrorSession: spawnSID, Label: "s"}, mirrorWindow: "@9"}
	if got := f.srv.toolConnectorRequestDecide(ownerCtx, in9, `{"id": 1, "decision": "deny"}`); !strings.Contains(got, spawnRefused) {
		t.Fatalf("a caller in an open dispatch's window, behind 205 newer rows: %s, want the spawned refusal", got)
	}
}

// twoHostRouteMirror is the route fixture's hub with a second host, "far",
// whose window also runs routeSID.
type twoHostRouteMirror struct{ *routeMirror }

func (m twoHostRouteMirror) Hosts(ctx context.Context) []mirror.HostView {
	hs := m.routeMirror.Hosts(ctx)
	ws := mirror.NormalizeWindows([]mirror.Window{{Session: "work", ID: "@2", Index: 1, Active: true, Claude: true,
		ClaudeSession: routeSID, Status: mirror.StatusIdle, StatusSince: 1}}, 0)
	h := mirror.HostView{Host: "far", Windows: ws, Online: true, Rendered: true, Interactive: true, PanePaste: true, StatusReport: true}
	h.Sessions, h.Claude, h.Labels, h.Activity = mirror.SessionsOf(ws)
	return append(hs, h)
}

// TestARouteReplyIsBoundToItsRoutesHost: with both hosts' tokens marked as
// spawning hosts', a route sent to routeSID on "tower":
//   - answered as routeSID from ANOTHER host ("far", whose listing shows the
//     same session) is carded, and route_reply itself refuses it there;
//   - answered from tower while an agent muster started there is still
//     running (an open host dispatch: it could be naming the routed pane) is
//     carded;
//   - answered from tower once that dispatch has ended is relayed at once.
func TestARouteReplyIsBoundToItsRoutesHost(t *testing.T) {
	f := newRouteFixture(t)
	tok := f.withRouteReply(t)
	marked := time.Now()
	farTok := mirrorTok('f')
	f.srv.ext.MirrorTokens = fakeMirrorTokens{
		tok:    mirror.Token{ID: 9, OwnerID: f.owner, Host: "tower", SpawnedAt: &marked},
		farTok: mirror.Token{ID: 10, OwnerID: f.owner, Host: "far", SpawnedAt: &marked},
	}
	f.srv.ext.ChiefDispatch = f.plugins
	f.allow(t, "example-inbox:someone@example.test")
	_, m := f.pluginMessage(t, "example-inbox", "route-reply-host", "someone@example.test", true)
	id := f.route(t, m)
	f.srv.ext.Mirror = twoHostRouteMirror{f.mirror}

	text, _ := f.routeReplyCall(t, farTok, routeSID, map[string]any{"route_id": id, "body": "From far."})
	if !strings.Contains(text, "confirmation") {
		t.Fatalf("route_reply as the routed session from ANOTHER host: %s, want the step-up", text)
	}
	asOwner := authz.WithPrincipal(context.Background(), authz.Principal{UserID: f.owner, Role: authz.RoleMember, Via: authz.ViaMirrorHost})
	far := toolCaller{asker: attention.Asker{MirrorHost: "far", MirrorSession: routeSID, Label: "s"}, mirrorWindow: "@2"}
	if out := f.srv.toolRouteReply(asOwner, far, `{"route_id": "`+id+`", "body": "From far."}`); !strings.Contains(out, "was not routed to this session") {
		t.Fatalf("route_reply's own binding, from another host: %s, want it refused", out)
	}

	var dispatch int64
	if err := f.pool.QueryRow(authz.System(context.Background()), `INSERT INTO host_dispatches (owner_id, host, runtime, repo, reservation_id, status)
		VALUES ($1, 'tower', 'claude', 'acme/widgets', $2, 'running') RETURNING id`, f.owner, time.Now().UnixNano()).Scan(&dispatch); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM host_dispatches WHERE id = $1`, dispatch)
	})
	text, _ = f.routeReplyCall(t, tok, routeSID, map[string]any{"route_id": id, "body": "Noon works for us."})
	if !strings.Contains(text, "confirmation") {
		t.Fatalf("route_reply from the routed session while a spawned agent runs on its host: %s, want the step-up", text)
	}

	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE host_dispatches SET status = 'ended', ended_at = now() WHERE id = $1`, dispatch); err != nil {
		t.Fatal(err)
	}
	text, isErr := f.routeReplyCall(t, tok, routeSID, map[string]any{"route_id": id, "body": "Noon works for us."})
	if isErr || strings.Contains(text, "confirmation") || !strings.Contains(text, `"state":"relayed"`) {
		t.Fatalf("route_reply from the routed session and host, no agent running there: %s, want it relayed at once", text)
	}
}

// approveCard files a dispatch_approve card for a hold of prompt on a marked
// host and returns the card's text.
func approveCard(t *testing.T, prompt string) string {
	t.Helper()
	h := newHoldFixture(t)
	marked := time.Now()
	h.tokens[h.ws] = mirror.Token{ID: 11, OwnerID: h.owner, Host: "ws", SpawnedAt: &marked}
	req := gatewayRequest()
	req["prompt"] = prompt
	held := h.request(t, req)
	text, isErr, rpc := h.tool(t, h.ws, focusSID, dispatchApproveTool, map[string]any{"hold_id": held.HoldID})
	if rpc != nil || isErr || !strings.Contains(text, "awaiting_owner_confirmation") {
		t.Fatalf("dispatch_approve on a marked host: %v %s, want a card", rpc, text)
	}
	asks, err := h.srv.ext.Attention.Open(h.asOwner)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range asks {
		if a.Asker.Label == hostDecisionLabel && a.StepUp != nil && a.StepUp.Tool == dispatchApproveTool {
			return a.Question
		}
	}
	t.Fatal("no confirmation card was filed")
	return ""
}

// TestTheHoldCardShowsShortInstructionsInFullAndLabelsAnExcerpt: a held
// prompt that fits the card is shown in full, with its length and SHA-256; a
// longer one (an 8 KiB hold cannot fit a 4,000-character card) is labelled
// as an excerpt of its first characters, says how many are NOT shown, carries
// the SHA-256 of the WHOLE text, and does not show its tail.
func TestTheHoldCardShowsShortInstructionsInFullAndLabelsAnExcerpt(t *testing.T) {
	short := "Profile the nightly export and open a draft PR."
	q := approveCard(t, short)
	for _, want := range []string{"The instructions in full (" + strconv.Itoa(len([]rune(short))) + " characters, SHA-256 " + promptDigest(short) + "):\n" + short} {
		if !strings.Contains(q, want) {
			t.Errorf("a short hold's card does not say %q:\n%s", want, q)
		}
	}
	if strings.Contains(q, "EXCERPT") {
		t.Errorf("a short hold's card calls its instructions an excerpt:\n%s", q)
	}

	long := "Profile the nightly export. " + strings.Repeat("Then check the next table. ", 250) + "TAIL-ONLY-INSTRUCTION"
	q = approveCard(t, long)
	n := len([]rune(long))
	for _, want := range []string{
		"AN EXCERPT of the instructions: the first 1500 of " + strconv.Itoa(n) + " characters, SHA-256 " + promptDigest(long),
		"The other " + strconv.Itoa(n-1500) + " characters are NOT shown on this card",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("a long hold's card does not say %q:\n%s", want, q)
		}
	}
	if strings.Contains(q, "TAIL-ONLY-INSTRUCTION") || strings.Contains(q, "full instructions") {
		t.Errorf("a long hold's card shows its tail, or calls an excerpt full:\n%s", q)
	}
}
