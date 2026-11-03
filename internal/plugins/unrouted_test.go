package plugins

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheBackfillIsTheSweepsPredicate pins the RELATIONSHIP between migration
// 0051's backfill and the sweep: the backfill settles exactly the messages the
// sweep would judge (awaitsDisposition), so a message the sweep would ask about
// cannot be left out of the backfill and fire at the deploy. The migration is
// found by its name, not its number, which moves as other branches land.
// TestTheThreadPluginsAreAgentMailAndClankMail pins the set by literal name —
// what the plugins are really called — and pins the sweep's SQL to the same
// set, so the thread route (IsThreadPlugin) and the unrouted sweep
// (fromThreadPlugin) cannot disagree.
func TestTheThreadPluginsAreAgentMailAndClankMail(t *testing.T) {
	got := ThreadPlugins()
	if strings.Join(got, ",") != "agent-mail,clankmail" {
		t.Fatalf("ThreadPlugins() = %q; want agent-mail and clankmail", got)
	}
	for _, n := range []string{"agent-mail", "clankmail"} {
		if !IsThreadPlugin(n) {
			t.Errorf("%q is not a thread plugin", n)
		}
	}
	for _, n := range []string{"", "foo", "discord-dm", "Agent-Mail", "clankmail ", "agent-mail,clankmail"} {
		if IsThreadPlugin(n) {
			t.Errorf("%q is a thread plugin", n)
		}
	}
	got[0] = "foo"
	if !IsThreadPlugin("agent-mail") || IsThreadPlugin("foo") {
		t.Fatal("a caller's copy changed the set")
	}
	if want := `EXISTS (SELECT 1 FROM plugins p WHERE p.id = m.plugin_id AND p.name IN ('agent-mail', 'clankmail'))`; fromThreadPlugin != want {
		t.Fatalf("fromThreadPlugin = %s; want %s", fromThreadPlugin, want)
	}
	for _, bad := range []string{"", "it's", "a b", "Upper"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("sqlStringList(%q) did not refuse", bad)
				}
			}()
			sqlStringList([]string{bad})
		}()
	}
}

func TestTheBackfillIsTheSweepsPredicate(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "db", "migrations", "*_plugin_message_unrouted.sql"))
	if err != nil || len(files) != 1 {
		t.Fatalf("the unrouted-message migration: %v %v; want exactly one file", files, err)
	}
	body, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	want := norm(`UPDATE public.plugin_messages m SET settle_reason = 'pre-existing' WHERE m.settle_reason IS NULL AND ` + awaitsDisposition + `;`)
	if !strings.Contains(norm(string(body)), want) {
		t.Fatalf("%s's backfill is not the sweep's predicate; want:\n%s", filepath.Base(files[0]), want)
	}
}
