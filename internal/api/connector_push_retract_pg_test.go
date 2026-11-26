package api

import (
	"net/http"
	"strconv"
	"testing"
	"time"

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

// TestEveryOtherWayAConnectorRequestEndsWithdrawsItsNotification: a pending
// request revoked on the Connectors tab; one still waiting when a standing
// grant came to cover it, approved by its agent asking again; and the pending
// requests of an agent that is deleted (its grants go with its row) — each
// withdraws the request's push. A request still waiting from another agent
// keeps its push through the agent's delete.
//
// Red at the branch's first commit: none of the three sent a withdrawal.
func TestEveryOtherWayAConnectorRequestEndsWithdrawsItsNotification(t *testing.T) {
	t.Run("revoked while pending", func(t *testing.T) {
		f := newDecideFixture(t)
		f.srv.clearRetry.after = (&capturedAfter{}).after
		id := f.request(t, f.opTok, "svc", "work")
		if w := f.call(t, http.MethodDelete, "/connector-grants/"+strconv.FormatInt(id, 10), nil, f.operator()); w.Code != http.StatusOK {
			t.Fatalf("revoke: %d %s", w.Code, w.Body)
		}
		if got := f.connectorWithdrawals(id); got != 1 {
			t.Fatalf("the revoked request: %d withdrawals, want 1", got)
		}
	})
	t.Run("approved by a standing grant on a re-ask", func(t *testing.T) {
		f := newDecideFixture(t)
		f.srv.clearRetry.after = (&capturedAfter{}).after
		id := f.request(t, f.opTok, "svc", "work")
		if _, _, err := f.store.CreateStandingGrant(f.ctx, "svc", connectors.SourceStored, "", id); err != nil {
			t.Fatal(err)
		}
		if status, _ := f.ask(t, f.opTok, "svc"); status != "approved" {
			t.Fatalf("CONTROL: the re-ask's grant: %q, want approved by the standing grant", status)
		}
		if got := f.connectorWithdrawals(id); got != 1 {
			t.Fatalf("the request approved on its re-ask: %d withdrawals, want 1", got)
		}
	})
	t.Run("its agent deleted", func(t *testing.T) {
		f := newDecideFixture(t)
		f.srv.clearRetry.after = (&capturedAfter{}).after
		tok := "push-retract-doomed-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		doomed := f.agentWith(t, 1, "doomed-"+strconv.FormatInt(time.Now().UnixNano(), 36), "", "", tok)
		gone := f.request(t, tok, "svc", "work")
		kept := f.request(t, f.opTok, "svc", "work")
		if _, err := f.srv.destroyAgent(f.ctx, doomed.ID, f.srv.destroyHostRow, destroyHooks{}); err != nil {
			t.Fatal(err)
		}
		f.srv.destroyInFlight.Wait()
		if got := f.connectorWithdrawals(gone); got != 1 {
			t.Errorf("the deleted agent's request: %d withdrawals, want 1", got)
		}
		if got := f.connectorWithdrawals(kept); got != 0 {
			t.Errorf("another agent's waiting request was withdrawn (%d)", got)
		}
	})
}

// TestAConnectorWithdrawalIsSentAgainAfterOneFanOut: a denied request's
// withdrawal is re-checked one fan-out later and, the request still decided,
// sent again (push_order.go).
//
// Red at the branch's first commit: nothing was scheduled; one withdrawal.
func TestAConnectorWithdrawalIsSentAgainAfterOneFanOut(t *testing.T) {
	f := newDecideFixture(t)
	later := &capturedAfter{}
	f.srv.clearRetry.after = later.after
	id := f.request(t, f.opTok, "svc", "work")
	if w := f.call(t, http.MethodPost, "/ui/connector-requests/"+strconv.FormatInt(id, 10)+"/deny", nil, f.operator()); w.Code != http.StatusOK {
		t.Fatalf("deny: %d %s", w.Code, w.Body)
	}
	if got := f.connectorWithdrawals(id); got != 1 {
		t.Fatalf("CONTROL: %d withdrawals after the deny, want 1", got)
	}
	if later.runAll() == 0 {
		t.Fatal("no re-check was scheduled after the withdrawal")
	}
	if got := f.connectorWithdrawals(id); got != 2 {
		t.Fatalf("after the re-check: %d withdrawals, want 2", got)
	}
}
