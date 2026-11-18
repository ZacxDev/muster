package agents

import (
	"context"
	"net/http"
	"testing"
)

// TestTheTransportCarriesThePinnedHeader: a turn routed with
// X-Muster-CC-Pinned (the chief's primary lane, internal/api withRoute) puts it
// on the request to ccd — the header ccd pins the lane by (cmd/ccd
// lanes_pinned_test.go). A route without it adds nothing.
func TestTheTransportCarriesThePinnedHeader(t *testing.T) {
	for _, pinned := range []bool{true, false} {
		h := http.Header{}
		h.Set(HeaderCCSession, "agent:chief:webchat:1")
		if pinned {
			h.Set(HeaderCCPinned, "1")
		}
		ctx := WithTurnRoute(context.Background(), TurnRoute{Runtime: SessionRuntimeClaudeCode, Header: h})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://ccd.invalid/v1/responses", nil)
		applyTurnRoute(req)
		if got := req.Header.Get(HeaderCCPinned) == "1"; got != pinned {
			t.Errorf("route pinned=%v: the request carries pinned=%v", pinned, got)
		}
		if req.Header.Get(HeaderCCSession) == "" {
			t.Error("control: the session header was not carried")
		}
	}
}
