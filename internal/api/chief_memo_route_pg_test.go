package api

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/ui"
)

// TestAMemoAfterAFreshPrimaryReachesThePrimarysLane (PR #609 round 2 N3):
// after a fresh primary on a claude-code chief, the memo relay's send —
// DeliveryRoute's context into agentgateway.Gateway.Send, exactly what
// memorelay.Relay.deliver does — reaches a fake ccd naming the NEW primary's
// lane (X-Muster-CC-Session) and pinning it. Without the route the request
// carried no session header, and a real ccd pasted it into its original CLI:
// the archived conversation.
func TestAMemoAfterAFreshPrimaryReachesThePrimarysLane(t *testing.T) {
	f := newChatsFixture(t, agents.KindClaudeCode)
	ctx := asUser(1)
	old, err := f.srv.primarySession(ctx, f.chief)
	if err != nil {
		t.Fatal(err)
	}
	if w := f.post(t, ui.ChiefFreshPrimaryPath(f.chief.ID)); w.Code != http.StatusOK {
		t.Fatalf("fresh primary: %d %s", w.Code, w.Body)
	}
	eventuallyAPI(t, "the handover's two turns", func() bool { k, _, _ := f.gw.turns(); return len(k) == 2 })
	fresh, err := f.srv.primarySession(ctx, f.chief)
	if err != nil || fresh.ID == old.ID {
		t.Fatalf("the primary did not move: %v", err)
	}

	var mu sync.Mutex
	var lanes, pins []string
	ccd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		lanes, pins = append(lanes, r.Header.Get(agents.HeaderCCSession)), append(pins, r.Header.Get(agents.HeaderCCPinned))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":"+
			"[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"noted\"}]}]}}\n\n")
	}))
	t.Cleanup(ccd.Close)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(ccd.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	gw, err := agentgateway.New(agentgateway.Config{Driver: fixedEndpoint{provision.Endpoint{Host: host, Port: port}},
		Runtime: agentgateway.HooksSHA256(), Model: "runtime-sentinel", Client: ccd.Client()})
	if err != nil {
		t.Fatal(err)
	}
	target, err := gw.Resolve(ctx, f.chief)
	if err != nil {
		t.Fatal(err)
	}
	sctx, sess, err := f.srv.DeliveryRoute(ctx, f.chief)
	if err != nil || sess.ID != fresh.ID {
		t.Fatalf("DeliveryRoute picked %d (%v), want the new primary %d", sess.ID, err, fresh.ID)
	}
	if _, err := gw.Send(sctx, target, sess.SessionKey, "a memo", nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lanes) != 1 || lanes[0] != fresh.SessionKey || pins[0] != "1" {
		t.Errorf("the memo reached ccd on lane %q pinned %q; want the new primary's lane %q, pinned", lanes, pins, fresh.SessionKey)
	}
}
