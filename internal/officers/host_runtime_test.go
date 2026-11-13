package officers_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// THE HOST RUNTIME'S CONFIGURATION (config.go parseRuntime;
// docs/design/officers-on-hosts.md, "What works on the host runtime").
// ---------------------------------------------------------------------------

// hostDecl is a valid host officer declaration with extra fields spliced in.
func hostDecl(extra string) string {
	return `[{"id":"marketing:example","backend":"builtin","runtime":"host","host":"host-alpha","repo":"example-org/example-site"` + extra + `}]`
}

// noEnv is read_grant_test.go's (one helper for the package).

// TestAHostOfficerParses: the fields land, the runtime is host, and String
// says so; a declaration with no runtime is a pod officer.
func TestAHostOfficerParses(t *testing.T) {
	list, err := officers.Parse(hostDecl(`,"max_instances":2`), noEnv, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	o := list[0]
	if !o.OnHost() || o.RuntimeKind() != officers.RuntimeHost || o.Host != "host-alpha" || o.Repo != "example-org/example-site" || o.MaxInstances != 2 {
		t.Fatalf("parsed %+v", o)
	}
	if !strings.Contains(o.String(), "runtime host on host-alpha") {
		t.Errorf("String() = %q, want it to name the host", o.String())
	}
	for _, rt := range []string{``, `,"runtime":"pod"`} {
		list, err := officers.Parse(`[{"id":"marketing:example","backend":"builtin"`+rt+`}]`, noEnv, "muster-agent-")
		if err != nil {
			t.Fatal(err)
		}
		if list[0].OnHost() || list[0].RuntimeKind() != officers.RuntimePod || list[0].Host != "" {
			t.Errorf("runtime %q parsed as %+v, want a pod officer", rt, list[0])
		}
	}
}

// TestTheHostRuntimeRefusesWhatItCannotHonour: each refusal names its field,
// at boot. One case per field, each otherwise valid, so the refusal seen is
// that field's and no other's.
func TestTheHostRuntimeRefusesWhatItCannotHonour(t *testing.T) {
	tok := func(k string) string {
		if k == "MUSTER_OFFICER_TOKEN_MARKETING_EXAMPLE" {
			return "tok-host-x"
		}
		return ""
	}
	for _, c := range []struct {
		name, raw, want string
		env             func(string) string
	}{
		{"hq backend", `[{"id":"marketing:example","hq_url":"https://hq.example.test/api/v1","runtime":"host","host":"host-alpha","repo":"example-org/example-site"}]`,
			`refuses backend "hq"`, tok},
		{"no host", `[{"id":"marketing:example","backend":"builtin","runtime":"host","repo":"example-org/example-site"}]`, "no host (", noEnv},
		{"bad host", `[{"id":"marketing:example","backend":"builtin","runtime":"host","host":"Host_Alpha","repo":"example-org/example-site"}]`,
			`host "Host_Alpha" (not a mirror host name`, noEnv},
		{"no repo", `[{"id":"marketing:example","backend":"builtin","runtime":"host","host":"host-alpha"}]`, "no repo (", noEnv},
		{"tiers", hostDecl(`,"tiers":{"judgement":{"model":"opus","max_instances":1},"routine":{"model":"haiku","max_instances":1}}`), "tiers (", noEnv},
		{"model", hostDecl(`,"model":"opus"`), "model (", noEnv},
		{"image", hostDecl(`,"image":"registry.example.test/runner@sha256:` + strings.Repeat("a", 64) + `"`), "image (", noEnv},
		{"cc_account", hostDecl(`,"cc_account":"acct-x"`), "cc_account/cc_failover (", noEnv},
		{"cc_failover", hostDecl(`,"cc_failover":true`), "cc_account/cc_failover (", noEnv},
		{"read_repos", hostDecl(`,"read_repos":["example-org/other"]`), "read_repos (", noEnv},
		{"llm", hostDecl(`,"llm":{"daily_usd":1}`), "llm (", noEnv},
		{"kube", hostDecl(`,"kube":{"clusters":["c1"]}`), "kube (", noEnv},
		{"unknown runtime", `[{"id":"marketing:example","backend":"builtin","runtime":"docker"}]`, `runtime "docker" is not "pod" or "host"`, noEnv},
		{"host on a pod officer", `[{"id":"marketing:example","backend":"builtin","host":"host-alpha"}]`, `host "host-alpha" is only for "runtime": "host"`, noEnv},
	} {
		_, err := officers.Parse(c.raw, c.env, "muster-agent-")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
		if err != nil && c.name != "unknown runtime" && c.name != "host on a pod officer" && !strings.Contains(err.Error(), "officer marketing:example") {
			t.Errorf("%s: %v does not name the officer", c.name, err)
		}
	}
}

// TestAHostOfficerIsOfferedTheQueueToolsAndAskOnly: the ONE tool rule. The
// same builtin officer on pods is offered officer_post_draft; on a host it is
// not, nor any ship, kube, obs or task tool.
func TestAHostOfficerIsOfferedTheQueueToolsAndAskOnly(t *testing.T) {
	pod := officers.Officer{ID: "marketing:example", Backend: officers.BackendBuiltin}
	host := pod
	host.Runtime, host.Host, host.Repo = officers.RuntimeHost, "host-alpha", "example-org/example-site"
	want := map[string]bool{officers.ToolBrief: true, officers.ToolItems: true, officers.ToolClaim: true, officers.ToolHeartbeat: true,
		officers.ToolComplete: true, officers.ToolRelease: true, officers.ToolAskPerson: true, officers.ToolPostDraft: true}
	all := []string{officers.ToolBrief, officers.ToolItems, officers.ToolClaim, officers.ToolHeartbeat, officers.ToolComplete, officers.ToolRelease,
		officers.ToolAskPerson, officers.ToolPostDraft, officers.ToolObsQueries, officers.ToolObsQuery, officers.ToolObsRaw, officers.ToolKubeRead,
		officers.ToolMergePR, officers.ToolTaskList, officers.ToolGapTask, officers.ToolReleaseEvaluation}
	for _, tool := range all {
		if got := officers.ToolOffered(host, tool); got != want[tool] {
			t.Errorf("host officer: ToolOffered(%s) = %v, want %v", tool, got, want[tool])
		}
	}
	// Positive control: the rule is the runtime's, not the officer's shape —
	// the same builtin officer on pods is offered officer_obs_queries' gate's
	// answer for its own shape (HQ-only tools aside), so compare a tool the
	// host rule alone withholds: officer_obs_queries is ToolOffered for a pod.
	if !officers.ToolOffered(pod, officers.ToolObsQueries) {
		t.Fatal("control: the same builtin officer on pods is not offered officer_obs_queries")
	}
}

// TestAHostOfficerMayDeclareConnectorsButNotGitHub: connectors parse on a host
// officer (its instance has an agent row the grants hang on), and the
// connector rule gives it exactly the declared ones — never github, which is
// used through the HTTP proxy with a token a host session never holds. The
// control: the same officer on pods may request github.
func TestAHostOfficerMayDeclareConnectorsButNotGitHub(t *testing.T) {
	list, err := officers.Parse(hostDecl(`,"connectors":["pg-example"]`), noEnv, "muster-agent-")
	if err != nil {
		t.Fatalf("connectors on a host officer: %v", err)
	}
	o := list[0]
	if !officers.MayRequestConnector(o, "pg-example") || officers.MayRequestConnector(o, "other-db") {
		t.Fatalf("host officer connectors: pg-example %v, other-db %v", officers.MayRequestConnector(o, "pg-example"), officers.MayRequestConnector(o, "other-db"))
	}
	if officers.MayRequestConnector(o, officers.GitHubConnector) {
		t.Fatal("a host officer may request github")
	}
	pod := o
	pod.Runtime, pod.Host = "", ""
	if !officers.MayRequestConnector(pod, officers.GitHubConnector) {
		t.Fatal("control: the same officer on pods may not request github")
	}
	if k := officers.KickoffText(o, 1); !strings.Contains(k, "connector_request") || !strings.Contains(k, "pg-example") {
		t.Errorf("the host kickoff does not teach its connectors: %.200q", k)
	}
}

// TestAHostOfficersKickoffSaysWhereItRuns: the host kickoff names its host and
// its worktree, reads its repository locally (no connector recipe), and
// offers no tool the rule withholds; a pod officer's text is unchanged.
func TestAHostOfficersKickoffSaysWhereItRuns(t *testing.T) {
	list, err := officers.Parse(hostDecl(``), noEnv, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	k := officers.KickoffText(list[0], 1)
	for _, want := range []string{"You run in a tmux window on host-alpha", "fresh git worktree of example-org/example-site", "do not commit, push"} {
		if !strings.Contains(k, want) {
			t.Errorf("host kickoff lacks %q", want)
		}
	}
	for _, not := range []string{"no repository checkout", "connector_request", "ccd connect-git", officers.ToolPostDraft} {
		if strings.Contains(k, not) {
			t.Errorf("host kickoff says %q", not)
		}
	}
	pod := list[0]
	pod.Runtime, pod.Host = "", ""
	if p := officers.KickoffText(pod, 1); !strings.Contains(p, "You have no muster task and no repository checkout; that is expected.") {
		t.Errorf("pod kickoff lost its sentence: %.200q", p)
	}
}

// hostSpawnContract is the store's contract, asserted on the memory store and
// on officer_host_spawns (migration 0125): a window or refusal is recorded
// once, only on the row's own host; the latest row per instance wins; a
// session names its newest row; ids are known only on their own host.
func hostSpawnContract(t *testing.T, ctx context.Context, st officers.HostSpawnStore) {
	t.Helper()
	a, err := st.InsertHostSpawn(ctx, "marketing:example", "officer-marketing-example-1", "host-alpha")
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.RecordHostSpawnWindow(ctx, a.ID, "host-beta", "@3", "s-1"); ok {
		t.Fatal("recorded a window from another host")
	}
	if ok, err := st.RecordHostSpawnWindow(ctx, a.ID, "host-alpha", "@3", "s-1"); !ok || err != nil {
		t.Fatalf("did not record the window: %v", err)
	}
	if ok, _ := st.RecordHostSpawnWindow(ctx, a.ID, "host-alpha", "@4", "s-2"); ok {
		t.Fatal("a second reply moved the recorded window")
	}
	if ok, _ := st.RecordHostSpawnRefusal(ctx, a.ID, "host-alpha", "failed: x"); ok {
		t.Fatal("a refusal overwrote a recorded window")
	}
	b, _ := st.InsertHostSpawn(ctx, "marketing:example", "officer-marketing-example-1", "host-alpha")
	if ok, err := st.RecordHostSpawnRefusal(ctx, b.ID, "host-alpha", "no_repo: not listed"); !ok || err != nil {
		t.Fatalf("did not record the refusal: %v", err)
	}
	latest, err := st.LatestHostSpawns(ctx, "marketing:example")
	if err != nil {
		t.Fatal(err)
	}
	if l := latest["officer-marketing-example-1"]; l.ID != b.ID || l.Refusal != "no_repo: not listed" || len(latest) != 1 {
		t.Fatalf("latest = %+v, want row %d with its refusal", latest, b.ID)
	}
	if got, err := st.HostSpawnForSession(ctx, "host-alpha", "s-1"); err != nil || got.ID != a.ID || got.Window != "@3" {
		t.Fatalf("session s-1 = %+v, %v", got, err)
	}
	if _, err := st.HostSpawnForSession(ctx, "host-beta", "s-1"); err == nil {
		t.Fatal("a session named a row on another host")
	}
	if _, err := st.HostSpawnForSession(ctx, "host-alpha", ""); err == nil {
		t.Fatal("an empty session named a row")
	}
	ids, err := st.HostSpawnIDs(ctx, "host-alpha", []int64{a.ID, b.ID, b.ID + 1000})
	if err != nil || !ids[a.ID] || !ids[b.ID] || len(ids) != 2 {
		t.Fatalf("ids on host-alpha = %v, %v", ids, err)
	}
	if ids, _ := st.HostSpawnIDs(ctx, "host-beta", []int64{a.ID}); len(ids) != 0 {
		t.Fatalf("ids on host-beta = %v, want none", ids)
	}
}

func TestTheMemHostSpawnStoreKeepsTheContract(t *testing.T) {
	hostSpawnContract(t, context.Background(), officers.NewMemHostSpawnStore())
}

// TestTheDatabaseHostSpawnStoreKeepsTheContract: the same contract on
// officer_host_spawns — and its ids come from host_dispatches' own sequence,
// so a spawn's id (its dispatch on the wire) can never equal a chief
// dispatch's.
func TestTheDatabaseHostSpawnStoreKeepsTheContract(t *testing.T) {
	dsn := dbtest.DSN(t)
	dbtest.OwnerExec(t, dsn, `TRUNCATE officer_host_spawns`)
	cctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := db.Connect(cctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ctx := dbtest.AsOperator(context.Background())
	hostSpawnContract(t, ctx, officers.NewPGHostSpawnStore(pool))
	var seq int64
	if err := pool.QueryRow(ctx, `SELECT nextval('public.host_dispatches_id_seq')`).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	h, err := officers.NewPGHostSpawnStore(pool).InsertHostSpawn(ctx, "marketing:example", "officer-marketing-example-2", "host-alpha")
	if err != nil {
		t.Fatal(err)
	}
	if h.ID != seq+1 {
		t.Fatalf("spawn id %d after host_dispatches' sequence gave %d: not drawn from it", h.ID, seq)
	}
}
