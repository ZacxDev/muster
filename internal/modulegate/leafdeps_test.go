package modulegate

import (
	"regexp"
	"sort"
	"testing"
)

// heavyClient is what an agent pod's binary must not link: a websocket
// client, an object-store client or a database driver.
var heavyClient = regexp.MustCompile(`websocket|minio|sqlite`)

func heavyIn(set map[string]bool) []string {
	var out []string
	for p := range set {
		if heavyClient.MatchString(p) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// TestTheSubagentLeafPullsNoNetworkOrStorageClient: internal/ccsubagents —
// the ONE reading of a session's subagents, kept a leaf for the agent
// pod's ccd — imports no websocket, object-store or database client, transitively.
// The wire package (internal/mirror) is the positive control: it does pull a
// websocket client, so the matcher is shown able to see one.
func TestTheSubagentLeafPullsNoNetworkOrStorageClient(t *testing.T) {
	root := moduleRoot(t)
	if got := heavyIn(depsOf(t, root, "./internal/mirror")); len(got) == 0 {
		t.Fatal("instrument check FAILED: internal/mirror's deps show no websocket client, so this matcher cannot see one")
	}
	if got := heavyIn(depsOf(t, root, "./internal/ccsubagents")); len(got) != 0 {
		t.Fatalf("internal/ccsubagents pulls in %v; it is meant for the agent pod's ccd and must stay a leaf", got)
	}
}
