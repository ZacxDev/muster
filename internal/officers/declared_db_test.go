package officers

import (
	"strings"
	"testing"
	"time"
)

// TestADeclarationIsRefusedByTheOneDeclarationCheck: a DB declaration goes
// through declared.officer, so a host officer with no repo and a model is
// refused per field exactly as a MUSTER_OFFICERS entry is at boot, and a
// valid one becomes a builtin officer carrying its slug.
func TestADeclarationIsRefusedByTheOneDeclarationCheck(t *testing.T) {
	d := Declaration{ID: "scout:main", Owner: 7, Runtime: RuntimeHost, Host: "box", Model: "opus", InstanceSlug: "scout-main"}
	_, err := d.Officer(RuntimeHost, "box", "muster-agent-")
	fields := map[string]bool{}
	for _, f := range Fields(err) {
		fields[f.Field] = true
	}
	if !fields["repo"] || !fields["model"] || len(fields) != 2 {
		t.Fatalf("a host declaration with no repo and a model: %v (fields %v), want repo and model", err, fields)
	}
	// The same declaration through MUSTER_OFFICERS says the same thing.
	_, envErr := Parse(`[{"id":"scout:main","backend":"builtin","runtime":"host","host":"box","model":"opus"}]`, func(string) string { return "" }, "muster-agent-")
	if envErr == nil || !strings.Contains(envErr.Error(), err.Error()) {
		t.Fatalf("the env refusal %q does not carry the declaration's %q", envErr, err)
	}
	d.Model, d.Repo = "", "example-org/example-site"
	o, err := d.Officer(RuntimeHost, "box", "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	if !o.IsBuiltin() || !o.OnHost() || !o.FromDB() || o.Key() != "scout-main" || o.InstanceName(1) != "officer-scout-main-1" || o.Owner != 7 {
		t.Fatalf("the declared officer %+v", o)
	}
	d.Runtime, d.Host, d.IdleAfter = RuntimeAuto, "", 5*time.Minute
	if err := d.Validate("muster-agent-"); err != nil {
		t.Fatalf("an auto declaration valid on a host: %v", err)
	}
	d.Host = "box"
	if err := d.Validate("muster-agent-"); err == nil || Fields(err)[0].Field != "host" {
		t.Fatalf("an auto declaration naming a host: %v, want refused on host", err)
	}
}

// TestSlugCandidatesFitAndDistinctSlugsNeverShareANameSpace: the candidates
// are the id's slug then -2…-9, each cut to fit; and two distinct slugs, one
// the other's prefix, never bind each other's instance names.
func TestSlugCandidatesFitAndDistinctSlugsNeverShareANameSpace(t *testing.T) {
	c := SlugCandidates("scout:main", 40)
	if len(c) != 9 || c[0] != "scout-main" || c[1] != "scout-main-2" || c[8] != "scout-main-9" {
		t.Fatalf("candidates %v", c)
	}
	long := strings.Repeat("a", 30) + ":" + strings.Repeat("b", 30)
	max := SlugMaxFor("muster-agent-", 8)
	for _, s := range SlugCandidates(long, max) {
		if len(s) > max || !ValidSlug(s) {
			t.Fatalf("candidate %q over %d or invalid", s, max)
		}
	}
	a := Officer{ID: "a:b", InstanceSlug: "a-b", MaxInstances: 8}
	b := Officer{ID: "a:b", InstanceSlug: "a-b-2", MaxInstances: 8}
	list := []Officer{a, b}
	for _, tc := range []struct {
		name string
		want string
		n    int
	}{{"officer-a-b-2", "a-b", 2}, {"officer-a-b-2-1", "a-b-2", 1}, {"officer-a-b-1", "a-b", 1}} {
		o, n, ok := forAgentName(list, tc.name)
		if !ok || o.InstanceSlug != tc.want || n != tc.n {
			t.Errorf("%s binds to %q #%d (%v), want %q #%d", tc.name, o.InstanceSlug, n, ok, tc.want, tc.n)
		}
	}
}

// TestTheRegistrySwapsWholeRosters: Swap replaces the roster; Get is by key
// (an env officer's id, a DB officer's slug); ForOwner finds an owner's own
// officer of an id another owner also declares.
func TestTheRegistrySwapsWholeRosters(t *testing.T) {
	env := Officer{ID: "scout:main", Owner: 1}
	r := NewRegistry([]Officer{env})
	db := Officer{ID: "scout:main", Owner: 2, InstanceSlug: "scout-main-2", ManagedBy: ManagedByDB}
	r.Swap([]Officer{env, db})
	if o, ok := r.ForOwner(2, "scout:main"); !ok || o.InstanceSlug != "scout-main-2" {
		t.Fatalf("owner 2's scout:main: %+v %v", o, ok)
	}
	if o, ok := r.ForOwner(1, "scout:main"); !ok || o.FromDB() {
		t.Fatalf("owner 1's scout:main: %+v %v", o, ok)
	}
	if _, err := r.Get("scout:main"); err != nil {
		t.Fatal("the env officer by its id")
	}
	if _, err := r.Get("scout-main-2"); err != nil {
		t.Fatal("the DB officer by its slug")
	}
	r.Swap([]Officer{env})
	if _, ok := r.ForOwner(2, "scout:main"); ok || len(r.All()) != 1 {
		t.Fatalf("after the swap the DB officer is still there: %v", r.All())
	}
}
