package api

import (
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestARouteReplyToItsOwnSessionFilesNoCardOnASpawnHost (operator decision):
// on a host whose token is marked as a spawning host's, route_reply from the
// session the chief routed the message to runs at once — the server-side
// binding (Route.SentToMirrorSession) is its check — while the same reply
// from ANOTHER session of that host files a card, and so does every other
// owner-authority tool from it.
func TestARouteReplyToItsOwnSessionFilesNoCardOnASpawnHost(t *testing.T) {
	f := newRouteFixture(t)
	tok := f.withRouteReply(t)
	marked := time.Now()
	f.srv.ext.MirrorTokens = fakeMirrorTokens{tok: mirror.Token{ID: 9, OwnerID: f.owner, Host: "tower", SpawnedAt: &marked}}
	f.allow(t, "example-inbox:someone@example.test")
	_, m := f.pluginMessage(t, "example-inbox", "route-reply-spawnhost", "someone@example.test", true)
	id := f.route(t, m)

	text, _ := f.routeReplyCall(t, tok, otherRouteSID, map[string]any{"route_id": id, "body": "Not mine."})
	if !strings.Contains(text, "confirmation") {
		t.Fatalf("route_reply from a session the route was NOT sent to, on a marked host: %s, want the step-up", text)
	}
	text, isErr := f.routeReplyCall(t, tok, routeSID, map[string]any{"route_id": id, "body": "Noon works for us."})
	if isErr || strings.Contains(text, "confirmation") || !strings.Contains(text, `"state":"relayed"`) {
		t.Fatalf("route_reply from the routed session on a marked host: %s, want it relayed at once", text)
	}
	text, _ = f.routeReplyCall(t, tok, routeSID, map[string]any{"route_id": "rt_" + strings.Repeat("0", 32), "body": "x"})
	if !strings.Contains(text, "confirmation") {
		t.Fatalf("route_reply naming a route that is not this session's: %s, want the step-up", text)
	}
}

// TestADispatchApproveCardShowsTheHeldStart: on a host whose token is
// marked, dispatch_approve files a card that names what it starts — the
// target, the model, the instructions — not only the hold's id; and a hold
// that cannot be read files no card at all.
func TestADispatchApproveCardShowsTheHeldStart(t *testing.T) {
	h := newHoldFixture(t)
	marked := time.Now()
	h.tokens[h.ws] = mirror.Token{ID: 11, OwnerID: h.owner, Host: "ws", SpawnedAt: &marked}
	held := h.request(t, gatewayRequest())
	text, isErr, rpc := h.tool(t, h.ws, focusSID, dispatchApproveTool, map[string]any{"hold_id": held.HoldID})
	if rpc != nil || isErr || !strings.Contains(text, "awaiting_owner_confirmation") {
		t.Fatalf("dispatch_approve on a marked host: %v %s, want a card", rpc, text)
	}
	h.nothingStarted(t)
	asks, err := h.srv.ext.Attention.Open(h.asOwner)
	if err != nil {
		t.Fatal(err)
	}
	var q string
	for _, a := range asks {
		if a.Asker.Label == hostDecisionLabel && strings.Contains(a.Question, dispatchApproveTool) {
			q = a.Question
		}
	}
	for _, want := range []string{"The held start: Start a new agent on pod/gateway", "openrouter/vendor-a/coder-7",
		"Profile the nightly export and open a draft PR.", "muster chooses the Claude account"} {
		if !strings.Contains(q, want) {
			t.Errorf("the card does not say %q:\n%s", want, q)
		}
	}
	text, isErr, _ = h.tool(t, h.ws, focusSID, dispatchApproveTool, map[string]any{"hold_id": "held:" + strings.Repeat("0", 32)})
	if !isErr || !strings.Contains(text, "could not be read") {
		t.Fatalf("an unreadable hold: %s, want no card", text)
	}
}
