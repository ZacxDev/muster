package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/githubapp"
)

// TestInReadModeAPushThroughTheProxyIsRefusedByGitHubNotUpgraded is the read
// mode at its seam: MUSTER_GITHUB_APP_ACCESS=read, an agent on
// example-org/widget files its `github` request, the Connectors tab describes
// the grant as READ ONLY (never "read and write"), the owner approves, and the
// agent's clone through muster's real proxy succeeds with a token minted with
// the four READ permissions. Its `git push` through the same proxy reaches
// GitHub carrying THAT token and GitHub refuses it (403, modelled by the fake
// exactly as for any contents:read token) — and muster mints nothing else: no
// second, wider token appears behind the refusal.
func TestInReadModeAPushThroughTheProxyIsRefusedByGitHubNotUpgraded(t *testing.T) {
	f := newConnFixture(t)
	up := newFakeGitHubUpstream(t)
	up.addRepo(t, "example-org/widget")
	const tok = "readonly-agent-token-0123456789abcd"
	id := f.seedAgent(t, "readonly-fixture", tok)
	if _, err := f.pool.Exec(f.ctx, `UPDATE agents SET repo='example-org/widget' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	srv, h, app := appServerAccess(t, f, up, githubapp.AccessRead)
	if app.Access() != githubapp.AccessRead {
		t.Fatalf("control: the App is in %q mode", app.Access())
	}
	op := map[string]string{"Cookie": sessionCookieFor(srv), "HX-Request": "true", "Origin": "http://example.com"}

	w := serveCall(t, h, "POST", "/agent/connectors/github/request", strings.NewReader(`{"reason":"read example-org/widget"}`),
		map[string]string{"X-Muster-Token": tok, "Content-Type": "application/json"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("filing the request: %d %s", w.Code, w.Body)
	}
	var filed AgentConnectorView
	_ = json.Unmarshal(w.Body.Bytes(), &filed)

	// The Connectors tab describes the App's connector in the ACTUAL mode.
	page := serveCall(t, h, "GET", "/ui/connectors", nil, op).Body.String()
	if !strings.Contains(page, "READ ONLY: Contents, Pull requests, Checks and Commit statuses read") {
		t.Fatalf("the Connectors tab does not describe the read-only scope:\n%s", page)
	}
	if strings.Contains(page, "read and write") {
		t.Fatalf("the Connectors tab claims write access in read mode:\n%s", page)
	}

	w = serveCall(t, h, "POST", "/ui/connector-requests/"+strconv.FormatInt(filed.GrantID, 10)+"/approve", nil, op)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "approved for github") {
		t.Fatalf("approve: %d %s", w.Code, w.Body)
	}
	if v := agentView(t, h, tok); !v.Exists || v.Grant != connectors.StatusApproved {
		t.Fatalf("after approval the agent sees %+v", v)
	}

	muster := httptest.NewServer(h)
	t.Cleanup(muster.Close)
	proxy := muster.URL + "/connect/github/"
	env := gitTestEnv(t)
	remote := proxy + "github.fake.test:" + up.port() + "/example-org/widget.git"
	gitThrough := func(args ...string) ([]byte, error) {
		c := exec.Command("git", append([]string{"-c", "http." + proxy + ".extraHeader=X-Muster-Token: " + tok,
			"-c", "http." + proxy + ".followRedirects=false"}, args...)...)
		c.Env = env
		return c.CombinedOutput()
	}
	dir := filepath.Join(t.TempDir(), "checkout")
	if out, err := gitThrough("clone", "-q", remote, dir); err != nil {
		t.Fatalf("the read-mode clone failed (reading must still work): %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "CHANGE.md"), []byte("synthetic\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, env, "-C", dir, "add", "CHANGE.md")
	runGit(t, env, "-C", dir, "-c", "user.email=a@example.com", "-c", "user.name=a", "commit", "-q", "-m", "change")
	out, err := gitThrough("-C", dir, "push", remote, "HEAD:refs/heads/agent-branch")
	if err == nil {
		t.Fatalf("a push succeeded through the proxy in read mode:\n%s", out)
	}

	up.mu.Lock()
	defer up.mu.Unlock()
	const wantMint = `{"repositories":["widget"],"permissions":{"contents":"read","pull_requests":"read","checks":"read","statuses":"read"}}`
	if len(up.mints) != 1 || up.mints[0] != wantMint {
		t.Fatalf("mints = %q, want exactly one, the read set: %s", up.mints, wantMint)
	}
	// Positive control: the push really reached GitHub with the read token and
	// was refused THERE (not short-circuited inside muster, not by a 401).
	if up.pushRefused == 0 {
		t.Fatalf("no receive-pack request reached the fake GitHub with the read token; the push failed elsewhere:\n%s\ngit requests: %q",
			out, up.gitSeen)
	}
	if !strings.Contains(string(out), "403") && !strings.Contains(string(out), "Write access to repository not granted") {
		t.Errorf("the push failed, but not with GitHub's refusal:\n%s", out)
	}
}
