package modulegate

import (
	"regexp"
	"sort"
	"testing"
)

// heavyClient is what an agent pod's binary must not link: a websocket
// client, an object-store client, or a database client — a Postgres driver
// (pgx, lib/pq), a MySQL one, SQLite, or database/sql itself, which every
// database/sql driver links.
var heavyClient = regexp.MustCompile(`websocket|minio|sqlite|jackc/pgx|lib/pq|go-sql-driver|^database/sql(/|$)`)

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
// the ONE reading of a session's subagents, kept a leaf for the agent pod's
// ccd — imports no websocket, object-store or database client, transitively.
//
// The instrument is checked first, one class at a time, on packages of this
// module that DO pull each one, so a matcher that cannot see a class fails
// here rather than passing the leaf: the wire package (websocket), the store
// (pgx and database/sql) and the host package (MinIO and SQLite).
func TestTheSubagentLeafPullsNoNetworkOrStorageClient(t *testing.T) {
	root := moduleRoot(t)
	for _, c := range []struct{ pkg, class string }{
		{"./internal/mirror", "websocket"},
		{"./internal/db", "jackc/pgx"},
		{"./internal/db", "database/sql"},
		{"./internal/mirrorhost", "minio"},
		{"./internal/mirrorhost", "sqlite"},
	} {
		seen := false
		for _, p := range heavyIn(depsOf(t, root, c.pkg)) {
			if regexp.MustCompile(regexp.QuoteMeta(c.class)).MatchString(p) {
				seen = true
				break
			}
		}
		if !seen {
			t.Fatalf("instrument check FAILED: %s pulls a %s client, and the matcher does not see it", c.pkg, c.class)
		}
	}
	if got := heavyIn(depsOf(t, root, "./internal/ccsubagents")); len(got) != 0 {
		t.Fatalf("internal/ccsubagents pulls in %v; it is meant for the agent pod's ccd and must stay a leaf", got)
	}
}
