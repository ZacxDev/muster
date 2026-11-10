package api

import (
	"context"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/plugins"
)

// TestTheTriageHoldsTheChiefsClaim (round 1, 🟡3): while the triage holds an
// event, a retry finds ANOTHER of the owner's messages queued and would deliver
// it — a delivery that settles every event still "delivering" to the chief as
// cut off (RecoverDeliveries). The triage holds the chief's session claim, so
// that delivery waits: the other message stays queued, no chief turn runs,
// and the triage's event ends settled by muster, not unconfirmed.
func TestTheTriageHoldsTheChiefsClaim(t *testing.T) {
	f := newTriageFixture(t, false)
	other, _, err := f.plugins.Submit(f.asOwner, f.p.ID, plugins.MessageIn{ExternalID: "tri-other-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		Sender: "someone@example.test", Subject: "hello", Body: "an ordinary message, still queued"})
	if err != nil {
		t.Fatal(err)
	}
	asPlugin := authz.WithPrincipal(context.Background(), authz.Principal{UserID: f.owner, Role: authz.RoleMember, Via: authz.ViaPlugin})
	f.srv.triageClaimed = func() {
		f.srv.retryQueuedPluginMessages(httptest.NewRequest("POST", "/plugin/v1/drafts/claim", nil).WithContext(asPlugin), f.p)
		f.srv.WaitForPluginDeliveries()
	}
	_, m := f.assign(t, "Rotate the build key", "Rotate it.", false, 0)
	if n := f.turns.count(); n != 0 {
		t.Fatalf("a chief turn ran while the triage held the event: %d", n)
	}
	if m.Status != plugins.StatusDelivered || m.SettleReason == nil || f.handled(t, m) != "task_board" {
		t.Fatalf("the triage's event: %s, reason %v, handler %q; want delivered, settled, task_board", m.Status, m.SettleReason, f.handled(t, m))
	}
	if cur, err := f.plugins.Message(f.asOwner, other.ID); err != nil || cur.Status != plugins.StatusQueued {
		t.Fatalf("the other message: %s (%v); want still queued for the chief", cur.Status, err)
	}
}
