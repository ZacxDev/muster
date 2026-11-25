package api

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/push"
)

// A CONNECTOR REQUEST'S NOTIFICATION GOES WHEN THE REQUEST STOPS WAITING
// (connectors.go connectorRequestsWithdraw; docs/push.md "When a notification
// is withdrawn"): denied or approved on the Connectors tab, approved by the
// shared approval of another, or gone with its connector. Its tap would open
// a card already decided. Names only identifiers that exist before the change
// (the tag is spelled here), so it is red on the pre-change tree for a
// behavioural reason.

// connectorWithdrawals is how many withdrawals of request id's notification
// the operator (user 1) was sent.
func (f *decideFixture) connectorWithdrawals(id int64) int {
	f.srv.WaitForPushes()
	n := 0
	for _, p := range f.pusher.to(ep("attn-operator")) {
		if p.Type == push.TypeResolved && p.Tag == "connector-"+strconv.FormatInt(id, 10) {
			n++
		}
	}
	return n
}

// TestADecidedConnectorRequestWithdrawsItsNotification: a request's push is
// withdrawn when it is denied, and when it is approved — its own and, through
// the shared approval, the request already waiting from another agent — and
// once its connector is deleted. A request still waiting keeps its push.
//
// Red at origin/main: no withdrawal was ever sent for a connector request.
func TestADecidedConnectorRequestWithdrawsItsNotification(t *testing.T) {
	t.Run("denied", func(t *testing.T) {
		f := newDecideFixture(t)
		id := f.request(t, f.opTok, "svc", "work")
		if f.prompts(id) != 1 {
			t.Fatalf("CONTROL: request %d raised %d prompts, want 1", id, f.prompts(id))
		}
		if f.connectorWithdrawals(id) != 0 {
			t.Fatal("a waiting request's notification was withdrawn")
		}
		if w := f.call(t, http.MethodPost, "/ui/connector-requests/"+strconv.FormatInt(id, 10)+"/deny", nil, f.operator()); w.Code != http.StatusOK {
			t.Fatalf("deny: %d %s", w.Code, w.Body)
		}
		if got := f.connectorWithdrawals(id); got != 1 {
			t.Fatalf("the denied request's notification: %d withdrawals, want 1", got)
		}
	})
	t.Run("approved, and the waiting one it shared with", func(t *testing.T) {
		f := newDecideFixture(t)
		first := f.request(t, f.opTok, "svc", "work")
		other := f.request(t, f.op2Tok, "svc", "more work")
		f.approveOn(t, "tab", first)
		for _, id := range []int64{first, other} {
			if got := f.connectorWithdrawals(id); got != 1 {
				t.Errorf("request %d after the approval: %d withdrawals, want 1", id, got)
			}
		}
	})
	t.Run("its connector deleted", func(t *testing.T) {
		f := newDecideFixture(t)
		id := f.request(t, f.opTok, "svc", "work")
		if _, err := f.store.CreateConnector(f.ctx, connectors.NewConnector{Name: "svc-kept", Hosts: []string{"kept.example.com"},
			Scheme: connectors.SchemeBearer, Secret: connTestSecret}); err != nil {
			t.Fatal(err)
		}
		kept := f.request(t, f.op2Tok, "svc-kept", "work")
		if w := f.call(t, http.MethodDelete, "/connectors/svc", nil, f.operator()); w.Code != http.StatusOK {
			t.Fatalf("delete: %d %s", w.Code, w.Body)
		}
		if got := f.connectorWithdrawals(id); got != 1 {
			t.Errorf("the deleted connector's request: %d withdrawals, want 1", got)
		}
		if got := f.connectorWithdrawals(kept); got != 0 {
			t.Errorf("another connector's waiting request was withdrawn (%d)", got)
		}
	})
}
