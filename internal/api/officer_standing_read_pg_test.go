package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/githubapp"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
)

// standingReadServer is officerAppServer with the built-in officer store wired
// (builtinOfficerServer's), declaring raw: the GitHub App configured in WRITE
// mode, so nothing read-only below can come from the deployment's mode.
func standingReadServer(t *testing.T, f *connFixture, up *fakeGitHubUpstream, raw string, logs *bytes.Buffer) (*Server, http.Handler) {
	t.Helper()
	dsn := dbtest.DSN(t)
	wipe := func() {
		dbtest.OwnerExec(t, dsn, `TRUNCATE officer_schedules, officer_items, officer_defs, officer_types RESTART IDENTITY`)
	}
	wipe()
	t.Cleanup(func() {
		wipe()
		_, _ = f.pool.Exec(dbtest.AsOperator(context.Background()), `DELETE FROM agents WHERE name LIKE 'officer-marketing-example%'`)
	})
	list, err := officers.Parse(raw, func(string) string { return "" }, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	list = officers.StampReleaseTag(list, "9.9.9")
	_, _, app := appServerAccess(t, f, up, githubapp.AccessWrite)
	store := builtin.NewStore(f.pool)
	s := New(nil, AuthConfig{HookToken: connTestHookToken, ServiceToken: connTestServiceToken, UIPassword: connTestPassword}, log.New(logs, "", 0))
	s.UseExtensions(Extensions{Agents: agents.NewPG(f.pool), Connectors: connectors.WithVirtual(f.store, app, authz.IsAdmin),
		GitHubApp: app, ConnectorPolicy: fakeTestPolicy(), Provisioner: stubProvisioner{}, Kinds: stubKinds{pick: "acct-one"},
		Attention: attention.NewPG(f.pool)})
	s.EnableOfficerStore(store)
	if _, err := s.EnableOfficers(OfficersConfig{Officers: list, InstallSession: discardSession, Builtin: store}); err != nil {
		t.Fatal(err)
	}
	return s, s.Handler()
}

// TestABuiltinOfficersStandingReadIsPublicReadOnlyAndDeclaredOnly is the
// standing public read (read_grant "standing") at its seam, end to end from
// MUSTER_OFFICERS: officers.Parse → the registry → callerOf
// (Caller.PublicReadRepos) → the real connect door and proxy → a fake GitHub
// on loopback → a real `git` clone. What it pins:
//
//   - a declared PUBLIC repository is read with NO grant: a git clone at the
//     release tag and a REST read both succeed, GitHub is sent no
//     Authorization, nothing is minted, and no grant is filed (the owner is
//     never prompted);
//   - a declared repository GitHub does not show as public is REFUSED;
//   - a write to a declared public repository (an API POST, a git push) is
//     REFUSED before GitHub sees it;
//   - a repository the officer does not declare is NOT a standing read: it
//     takes the ordinary path, which files a grant for the owner to decide.
func TestABuiltinOfficersStandingReadIsPublicReadOnlyAndDeclaredOnly(t *testing.T) {
	f := newConnFixture(t)
	up := newFakeGitHubUpstream(t)
	for _, r := range []string{"example-org/docs", "example-org/secret", "example-org/other"} {
		up.addRepo(t, r)
	}
	runGit(t, gitTestEnv(t), "--git-dir", filepath.Join(up.root, "example-org/docs.git"), "tag", "v9.9.9", "main")
	up.public["example-org/docs"], up.public["example-org/other"] = true, true

	const id = "marketing:example"
	raw := `[{"id":"` + id + `","backend":"builtin","read_repos":["Example-Org/Docs","example-org/secret"],
	         "read_grant":"standing","read_at":"release"}]`
	logs := &bytes.Buffer{}
	srv, h := standingReadServer(t, f, up, raw, logs)
	if want := "officers: " + id + ": standing public read of example-org/docs, example-org/secret at muster's release tag v9.9.9"; !strings.Contains(logs.String(), want) {
		t.Fatalf("the boot log does not state the standing read (%q):\n%s", want, logs)
	}
	const tok = "officer-standing-agent-token-012345"
	a := startOfficerInstance(t, f, srv, id, 1, tok)

	c := srv.callerOf(a)
	if want := []string{"example-org/docs", "example-org/secret"}; !reflect.DeepEqual(c.PublicReadRepos, want) || c.ReadRepos != nil || c.Repo != "" {
		t.Fatalf("callerOf = %+v, want PublicReadRepos %q, no ReadRepos, no Repo", c, want)
	}
	noGrant := func(when string) {
		t.Helper()
		if g, err := f.store.LatestGrant(f.ctx, a.ID, "github"); !errors.Is(err, connectors.ErrNotFound) {
			t.Fatalf("%s: a github grant exists (%+v, %v); a standing read must never file one", when, g, err)
		}
	}
	upSnap := func() (mints int, git, api []string) {
		up.mu.Lock()
		defer up.mu.Unlock()
		return len(up.mints), append([]string(nil), up.gitSeen...), append([]string(nil), up.apiSeen...)
	}

	muster := httptest.NewServer(h)
	t.Cleanup(muster.Close)
	proxy := muster.URL + "/connect/github/"
	env := gitTestEnv(t)
	git := func(args ...string) ([]byte, error) {
		cmd := exec.Command("git", append([]string{"-c", "http.extraHeader=X-Muster-Token: " + tok}, args...)...)
		cmd.Env = env
		return cmd.CombinedOutput()
	}

	// 1. The kickoff's own recipe: a shallow clone of the release tag.
	dir := filepath.Join(t.TempDir(), "docs")
	if out, err := git("clone", "-q", "--depth", "1", "--branch", "v9.9.9", proxy+"github.fake.test:"+up.port()+"/example-org/docs.git", dir); err != nil {
		t.Fatalf("the standing read of a public repository did not clone: %v\n%s", err, out)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "README.md")); err != nil || string(b) != "synthetic example-org/docs\n" {
		t.Fatalf("the clone holds %q (%v)", b, err)
	}
	mints, gitSeen, _ := upSnap()
	if mints != 0 || len(gitSeen) == 0 {
		t.Fatalf("mints=%d git requests=%d: a standing read mints nothing and must have reached GitHub", mints, len(gitSeen))
	}
	for _, s := range gitSeen {
		if !strings.HasSuffix(s, " ") {
			t.Fatalf("GitHub was sent a credential for a standing read: %q", s)
		}
	}
	noGrant("after the clone")

	// The REST API, at the tag.
	agentHdr := map[string]string{"X-Muster-Token": tok}
	api := "/connect/github/api.fake.test:" + up.port() + "/repos/example-org/"
	w := serveCall(t, h, "GET", api+"docs/contents/README.md?ref=v9.9.9", nil, agentHdr)
	if w.Code != http.StatusOK {
		t.Fatalf("a REST read of the public repository: %d %s", w.Code, w.Body)
	}
	if _, _, apiSeen := upSnap(); len(apiSeen) != 1 || apiSeen[0] != "" {
		t.Fatalf("GitHub's API was sent Authorization %q for a standing read, want none", apiSeen)
	}

	// 2. A declared repository GitHub does not show as public.
	w = serveCall(t, h, "GET", api+"secret/contents/README.md", nil, agentHdr)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "PUBLIC repositories only") {
		t.Fatalf("a declared private repository: %d %s, want 403 naming the public-only rule", w.Code, w.Body)
	}
	if out, err := git("clone", "-q", proxy+"github.fake.test:"+up.port()+"/example-org/secret.git", filepath.Join(t.TempDir(), "s")); err == nil {
		t.Fatalf("a declared private repository cloned through the standing read:\n%s", out)
	}
	if _, _, apiSeen := upSnap(); len(apiSeen) != 1 {
		t.Fatalf("a refused private read reached GitHub's API: %q", apiSeen)
	}

	// 3. Writes to the public one.
	w = serveCall(t, h, "POST", api+"docs/issues", strings.NewReader(`{"title":"x"}`), agentHdr)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "serves reads only") {
		t.Fatalf("an API write to the standing repository: %d %s, want 403", w.Code, w.Body)
	}
	runGit(t, env, "-C", dir, "-c", "user.email=a@example.com", "-c", "user.name=a", "commit", "-q", "--allow-empty", "-m", "push me")
	if out, err := git("-C", dir, "push", "-q", "origin", "HEAD:refs/heads/pushed"); err == nil {
		t.Fatalf("a push through the standing read succeeded:\n%s", out)
	}
	_, gitSeen, apiSeen := upSnap()
	for _, s := range gitSeen {
		if strings.Contains(s, "receive-pack") {
			t.Fatalf("a push reached GitHub through the standing read: %q", s)
		}
	}
	if len(apiSeen) != 1 {
		t.Fatalf("a refused API write reached GitHub: %q", apiSeen)
	}
	noGrant("after the refusals")

	// 4. A repository the officer does not declare is the ordinary path: a
	// grant is filed for the owner, and nothing is read.
	w = serveCall(t, h, "GET", api+"other/contents/README.md", nil, agentHdr)
	var ref connectors.RefusalBody
	_ = json.Unmarshal(w.Body.Bytes(), &ref)
	if w.Code != http.StatusForbidden || ref.Grant != connectors.StatusPending {
		t.Fatalf("an undeclared repository: %d %s, want the ordinary path's 403 with a pending grant", w.Code, w.Body)
	}
	if g, err := f.store.LatestGrant(f.ctx, a.ID, "github"); err != nil || g.Status != connectors.StatusPending {
		t.Fatalf("an undeclared repository filed no pending grant: %+v %v", g, err)
	}
	if _, _, apiSeen := upSnap(); len(apiSeen) != 1 {
		t.Fatalf("an undeclared repository was read without a grant: %q", apiSeen)
	}
}

// TestTheStandingReadIsNeverAnHQOfficers: an HQ officer that declares
// read_grant "standing" is refused at boot, and an HQ officer's read_repos
// (approved per instance) never reach Caller.PublicReadRepos — its callerOf is
// exactly what it was before the standing read existed.
func TestTheStandingReadIsNeverAnHQOfficers(t *testing.T) {
	env := func(k string) string {
		if strings.HasPrefix(k, officers.TokenEnvPrefix) {
			return "hq-token"
		}
		return ""
	}
	_, err := officers.Parse(`[{"id":"triage:example-web","hq_url":"https://hq.example.invalid/api/v1","repo":"example-org/widget",
	   "read_repos":["example-org/docs"],"read_grant":"standing"}]`, env, "muster-agent-")
	if err == nil || !strings.Contains(err.Error(), `read_grant "standing" is for a builtin officer only`) {
		t.Fatalf("an HQ officer declaring a standing read: %v, want a boot refusal", err)
	}

	f := newConnFixture(t)
	up := newFakeGitHubUpstream(t)
	list, err := officers.Parse(`[{"id":"triage:example-web","hq_url":"https://hq.example.invalid/api/v1","repo":"example-org/widget",
	   "read_repos":["example-org/docs"]}]`, env, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	// Even an HQ officer whose value says "standing" (built outside Parse):
	// the one rule (officers.Officer.StandingReadRepos) gives it nothing.
	list[0].ReadGrant = officers.ReadGrantStanding
	srv, _ := officerAppServer(t, f, up, githubapp.AccessWrite)
	if _, err := srv.EnableOfficers(OfficersConfig{Officers: list, InstallSession: discardSession}); err != nil {
		t.Fatal(err)
	}
	a := startOfficerInstance(t, f, srv, "triage:example-web", 1, "officer-hq-standing-token-0123456")
	if c := srv.callerOf(a); c.PublicReadRepos != nil || !reflect.DeepEqual(c.ReadRepos, []string{"example-org/docs"}) {
		t.Fatalf("an HQ officer's caller = %+v, want no PublicReadRepos and its read_repos under the grant", c)
	}
}
