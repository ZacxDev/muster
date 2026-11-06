package api

import (
	"strings"
	"testing"
)

// TestEveryOwnerTurnRefusalHasARouteSentence: each sentence ownerTurnFor can
// return (the keys agent_dispatch's no-task wording covers, less the one only
// its reservation lock returns) has a route-worded sibling, so a request route
// never answers "the gate was not released" for a gate it never had.
func TestEveryOwnerTurnRefusalHasARouteSentence(t *testing.T) {
	n := 0
	for why := range ownerAskedNoTaskSibling {
		if why == chiefReleaseChanged {
			continue // agent_dispatch's lock only; ownerTurnFor never returns it
		}
		n++
		msg, ok := routeOwnerAskedSibling[why]
		if !ok {
			t.Errorf("no route sentence for ownerTurnFor's %q", why)
			continue
		}
		if !strings.HasPrefix(msg, "owner_asked: ") || strings.Contains(msg, "gate") || strings.Contains(msg, "task_route_attention") {
			t.Errorf("the route sentence for %q is not worded for a route: %q", why, msg)
		}
	}
	if n < 8 || len(routeOwnerAskedSibling) != n {
		t.Fatalf("checked %d sentences; the route map has %d (want the same, at least 8)", n, len(routeOwnerAskedSibling))
	}
}
