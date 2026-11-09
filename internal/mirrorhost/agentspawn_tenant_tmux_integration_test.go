//go:build tmuxit

package mirrorhost

import (
	"context"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// A TENANT'S SPAWN against REAL tmux: two servers under the rig's private
// TMUX_TMPDIR — its default one (newSpawnRig's) and a tenant's, `-L teamq`.
// The operator's own servers are never touched.

const tenantName = "teamq"

// startTenantServer starts the tenant's server with NO configuration, and ends
// it with the test.
func startTenantServer(t *testing.T, name string) {
	t.Helper()
	if out, err := exec.Command("tmux", "-L", name, "-f", "/dev/null", "new-session", "-d", "-s", "base").CombinedOutput(); err != nil {
		t.Fatalf("start the tenant's tmux server: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", name, "kill-server").Run() })
}

// envStub is a stub claude that records its $TMUX in recs/<dispatch>.tmux,
// then stays.
func (r *spawnRig) envStub(t *testing.T) {
	t.Helper()
	stub := filepath.Join(r.tmp, "bin", "claude-env")
	body := "#!/bin/sh\nd=$(basename \"$PWD\")\nprintf '%s' \"$TMUX\" > " + r.recs + "/$d.tmp && mv " + r.recs + "/$d.tmp " + r.recs + "/$d.tmux\nexec sleep 300\n"
	if err := os.WriteFile(stub, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	r.c.cfg.AgentSpawn.ClaudeBin = stub
}

// onTenant points the rig's mirror at the tenant's server, opted in.
func (r *spawnRig) onTenant(name string) {
	r.tm = Tmux{Socket: name}
	r.c.cfg.Backend = r.tm
	r.c.cfg.AgentSpawn.TenantServer = name
}

// TestTmuxATenantSpawnLandsInItsServerAndReachesItsOwnMuster: opted in, the
// spawn's window is in the TENANT's server (session `agents`), and the default
// server has no `agents` session at all. Its worktree, branch and prompt file
// carry the tenant's namespace. The spawned process's $TMUX names the tenant's
// server — the one value that routes it to muster: the tenant mirror's MCP
// endpoint FORWARDS a caller with that $TMUX, while the default mirror's
// answers it as an outsider (no tools), and the AskUserQuestion hook picks the
// record whose socket equals it (cmd/muster-mirror's
// TestTwoTenantsEachQuestionLandsOnItsOwnServer).
func TestTmuxATenantSpawnLandsInItsServerAndReachesItsOwnMuster(t *testing.T) {
	r := newSpawnRig(t)
	startTenantServer(t, tenantName)
	r.envStub(t)
	// Controls first: the same request on the tenant's server WITHOUT the
	// opt-in, and with an opt-in naming another server, is refused — before
	// any git call — with each its own detail.
	r.tm = Tmux{Socket: tenantName}
	r.c.cfg.Backend = r.tm
	if _, e := r.spawn(t, 731, widgets(testPrompt)); e == nil || e.code != mirror.SpawnUnavailable || e.detail != "not the default tmux server" {
		t.Fatalf("no opt-in: %+v", e)
	}
	r.c.cfg.AgentSpawn.TenantServer = "otherq"
	if _, e := r.spawn(t, 732, widgets(testPrompt)); e == nil || e.code != mirror.SpawnUnavailable || e.detail != tenantMismatch {
		t.Fatalf("an opt-in naming another server: %+v", e)
	}
	if n := r.gitCalls(); n != 0 {
		t.Fatalf("git ran %d times for refused spawns", n)
	}
	r.onTenant(tenantName)
	w, e := r.spawn(t, 733, widgets(testPrompt))
	if e != nil {
		t.Fatalf("opted in: refused %+v", e)
	}
	// FIRST, $TMUX as the spawned process sees it: the value that routes it
	// to a muster (checked before the window's place, so a window opened on
	// the wrong server fails HERE, on what the agent would reach).
	p := filepath.Join(r.recs, "733.tmux")
	waitFor(t, "the stub to record $TMUX", func() bool { _, err := os.Stat(p); return err == nil })
	b, _ := os.ReadFile(p)
	tmuxEnv := string(b)
	sock, _, _ := strings.Cut(tmuxEnv, ",")
	if sock != resolvedTmuxSocket(r.tm) || sock == resolvedTmuxSocket(Tmux{}) {
		t.Fatalf("the spawned process's $TMUX is %q; want the tenant's socket %s", tmuxEnv, resolvedTmuxSocket(r.tm))
	}
	// The MCP endpoints' routing, on that value: the tenant's forwards it, the
	// default's does not.
	call := func(socket Tmux) error {
		req := httptest.NewRequest("POST", "/mcp", nil)
		req.Header.Set(MCPTmuxHeader, tmuxEnv)
		return (&MCPProxy{Socket: socket, Logger: log.New(io.Discard, "", 0)}).checkServer(context.Background(), req)
	}
	if err := call(Tmux{Socket: tenantName}); err != nil {
		t.Fatalf("the tenant mirror's MCP endpoint refuses its own spawned agent: %v", err)
	}
	if err := call(Tmux{}); err == nil {
		t.Fatal("the DEFAULT mirror's MCP endpoint would forward the tenant's spawned agent")
	}
	if got := r.tmux(t, "display-message", "-p", "-t", w.Window, "#{session_name} #{socket_path}"); got != mirror.AgentSpawnSession+" "+resolvedTmuxSocket(r.tm) {
		t.Fatalf("the window is in %q; want agents on the tenant's server %s", got, resolvedTmuxSocket(r.tm))
	}
	if _, err := (Tmux{}).run(context.Background(), "has-session", "-t", "="+mirror.AgentSpawnSession); err == nil {
		t.Fatal("the DEFAULT server has an agents session")
	}
	if tag, err := r.tm.dispatchTagOf(context.Background(), w.Window); err != nil || tag != 733 {
		t.Fatalf("the window's own tag: %d %v", tag, err)
	}
	wantDir := filepath.Join(realOrClean(r.c.cfg.AgentSpawn.Dir), "tenant-"+tenantName, "acme__widgets", "733")
	if w.Worktree != wantDir || w.Branch != "muster/tenant-"+tenantName+"-733-9-fix-the-typo" {
		t.Fatalf("worktree %q branch %q; want %q on muster/tenant-%s-733-…", w.Worktree, w.Branch, wantDir, tenantName)
	}
	// The listing of the TENANT's server reads the window's dispatch.
	ws, err := r.tm.Windows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, x := range ws {
		seen = seen || (x.ID == w.Window && x.Dispatch == 733)
	}
	if !seen {
		t.Fatal("the tenant's listing does not hold the spawned window")
	}
}

// TestTmuxATenantSpawnNeverStartsItsServer: (1) opted in, the tenant's server
// NOT running: refused unavailable with its own detail, and afterwards there
// is still no such server (nothing started it). (2) The new-session the spawn
// would send if the server vanished after its check starts a server that
// loaded NO configuration: the user's ~/.tmux.conf sets a marker option, and
// the server newAgentWindow started does not have it. The CONTROL: a server
// started the ordinary way under the same HOME does have it, so the marker
// would show.
func TestTmuxATenantSpawnNeverStartsItsServer(t *testing.T) {
	r := newSpawnRig(t)
	home := os.Getenv("HOME")
	if err := os.WriteFile(filepath.Join(home, ".tmux.conf"), []byte("set -g @musterfixture loaded-from-home\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.onTenant(tenantName)
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", tenantName, "kill-server").Run() })
	if _, e := r.spawn(t, 741, widgets(testPrompt)); e == nil || e.code != mirror.SpawnUnavailable || e.detail != tenantAbsent {
		t.Fatalf("refusal %+v; want unavailable %q", e, tenantAbsent)
	}
	if err := exec.Command("tmux", "-L", tenantName, "has-session").Run(); err == nil {
		t.Fatal("a tenant server is running after a refused spawn")
	}
	// (2) newAgentWindow straight away, as if the server exited after the check.
	if _, _, err := r.tm.newAgentWindow(context.Background(), "cc-742", r.tmp, []string{"/bin/sh", "-c", "exec sleep 300"}); err != nil {
		t.Fatalf("newAgentWindow: %v", err)
	}
	if out, _ := exec.Command("tmux", "-L", tenantName, "show-options", "-gqv", "@musterfixture").Output(); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("the server the spawn's new-session started loaded the user's config (@musterfixture=%q)", out)
	}
	// The control.
	ctl := "ctlq"
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", ctl, "kill-server").Run() })
	if out, err := exec.Command("tmux", "-L", ctl, "new-session", "-d", "-s", "c").CombinedOutput(); err != nil {
		t.Fatalf("control server: %v: %s", err, out)
	}
	if out, _ := exec.Command("tmux", "-L", ctl, "show-options", "-gqv", "@musterfixture").Output(); strings.TrimSpace(string(out)) != "loaded-from-home" {
		t.Fatalf("control: an ordinary server did not load ~/.tmux.conf (%q), so the check above proves nothing", out)
	}
}

// TestTmuxAMisSetTenantUnitNeitherSpawnsNorSweepsTheDefaultsWorktrees: the
// default mirror spawned an agent (a clean worktree, nothing unpushed: what
// the sweep removes once its window is gone). A tenant's mirror on the SAME
// agents directory, with -agent-spawn set by mistake and no opt-in, refuses a
// spawn — and its sweep, which lists only the TENANT's windows, removes
// nothing. A tenant's mirror that IS opted in sweeps only its own namespace,
// so it removes nothing either. The CONTROL: the default mirror's own sweep,
// once the window is gone, does remove it — so the sweep can.
func TestTmuxAMisSetTenantUnitNeitherSpawnsNorSweepsTheDefaultsWorktrees(t *testing.T) {
	r := newSpawnRig(t)
	startTenantServer(t, tenantName)
	w, e := r.spawn(t, 751, widgets(testPrompt))
	if e != nil {
		t.Fatalf("the default mirror's spawn: %+v", e)
	}
	tenant := func(optIn string) *Client {
		cfg := r.c.cfg
		cfg.Backend = Tmux{Socket: tenantName}
		cfg.AgentSpawn.TenantServer = optIn
		return &Client{log: log.New(io.Discard, "", 0), cfg: cfg}
	}
	misSet := tenant("")
	if why := misSet.AgentSpawnUnavailable(); why != "not the default tmux server" {
		t.Fatalf("a mis-set tenant unit: %q", why)
	}
	for _, c := range []*Client{misSet, tenant(tenantName)} {
		c.sweepOnce(context.Background())
		if _, err := os.Stat(w.Worktree); err != nil {
			t.Fatalf("a tenant mirror (opt-in %q) swept the default mirror's live worktree: %v", c.cfg.AgentSpawn.TenantServer, err)
		}
	}
	// The control.
	r.tmux(t, "kill-window", "-t", w.Window)
	r.c.sweepOnce(context.Background())
	if _, err := os.Stat(w.Worktree); !os.IsNotExist(err) {
		t.Fatalf("control: the default mirror's sweep kept its dead worktree (%v)", err)
	}
}

// TestTmuxATenantOpenCodeSpawnBindsInItsServer: opencode follows the same
// rules. Opted in, the launcher runs in the tenant's server, its record names
// that server's socket (its $TMUX), and the tenant mirror — whose launches are
// those of ITS server — binds the pane and posts the prompt once. Without the
// opt-in the same request is refused before the opencode service is asked.
func TestTmuxATenantOpenCodeSpawnBindsInItsServer(t *testing.T) {
	r := newOCRig(t)
	startTenantServer(t, tenantName)
	tm := Tmux{Socket: tenantName, OpenCode: r.src}
	r.tm, r.c.cfg.Backend = tm, tm
	creates := r.srv.Creates()
	if _, e := r.spawn(t, 761, ocWidgets(testPrompt)); e == nil || e.code != mirror.SpawnUnavailable || e.detail != "not the default tmux server" {
		t.Fatalf("no opt-in: %+v", e)
	}
	if r.srv.Creates() != creates {
		t.Fatal("a session was created for a refused spawn")
	}
	r.c.cfg.AgentSpawn.TenantServer = tenantName
	r.socket = resolvedTmuxSocket(tm)
	r.src.SetLaunches(filepath.Join(r.tmp, "launch"), r.socket)
	w, e := r.spawn(t, 762, ocWidgets(testPrompt))
	if e != nil {
		t.Fatalf("opted in: refused %+v", e)
	}
	if got := r.tmux(t, "display-message", "-p", "-t", w.Window, "#{session_name} #{socket_path}"); got != mirror.AgentSpawnSession+" "+r.socket {
		t.Fatalf("the window is in %q; want agents on the tenant's server %s", got, r.socket)
	}
	if _, err := (Tmux{}).run(context.Background(), "has-session", "-t", "="+mirror.AgentSpawnSession); err == nil {
		t.Fatal("the DEFAULT server has an agents session")
	}
	if ps := r.promptsTo(w.AgentSession); len(ps) != 1 || ps[0].Text != testPrompt {
		t.Fatalf("the fake admitted %+v; want the prompt once", ps)
	}
	if !strings.Contains(w.Worktree, string(filepath.Separator)+"tenant-"+tenantName+string(filepath.Separator)) {
		t.Fatalf("worktree %q is not in the tenant's namespace", w.Worktree)
	}
}
