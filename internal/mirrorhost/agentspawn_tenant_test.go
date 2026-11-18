package mirrorhost

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// A TENANT'S MIRROR SPAWNS ONLY BEHIND AN EXPLICIT OPT-IN THAT NAMES ITS OWN
// SERVER (agentspawn.go spawnTmuxServerRefusal, design 3.8 as revised). The
// spawn against real tmux servers is agentspawn_tenant_tmux_integration_test.go.
//
// Fixture names are distinct from every constant the code compares with:
// "teamq" and "otherq" are no default socket name, and no namespace prefix.

const (
	tenantMismatch = "-agent-spawn-tenant-server names another tmux server than the one this mirror mirrors"
	tenantInvalid  = "-agent-spawn-tenant-server is not a tmux socket name or an absolute socket path"
	tenantAbsent   = "the tenant tmux server is not running (this host never starts it)"
)

// TestATenantSpawnsOnlyWhenTheOptInNamesItsOwnServer: every combination of the
// mirror's server and the opt-in, through agentSpawnUnavailable (what startup
// logs and every spawn re-checks). No opt-in keeps the old refusal, word for
// word; an opt-in naming any server but the mirror's own refuses with its own
// detail; the same server spelled -L or -S is the same server.
func TestATenantSpawnsOnlyWhenTheOptInNamesItsOwnServer(t *testing.T) {
	base := newSpawnUnit(t).c.cfg
	tmpdir := os.Getenv("TMUX_TMPDIR")
	sockDir := filepath.Join(realOrClean(tmpdir), "tmux-"+strconv.Itoa(os.Getuid()))
	for _, c := range []struct {
		name    string
		backend Tmux
		optIn   string
		want    string
	}{
		{"default server, no opt-in (unchanged)", Tmux{}, "", ""},
		{"tenant server, no opt-in: a mis-set tenant unit still refuses", Tmux{Socket: "teamq"}, "", "not the default tmux server"},
		{"tenant server, opt-in names it", Tmux{Socket: "teamq"}, "teamq", ""},
		{"tenant server by -S, opt-in names it by -L", Tmux{SocketPath: filepath.Join(sockDir, "teamq")}, "teamq", ""},
		{"tenant server by -L, opt-in names it by -S", Tmux{Socket: "teamq"}, filepath.Join(sockDir, "teamq"), ""},
		{"tenant server, opt-in names another tenant's", Tmux{Socket: "teamq"}, "otherq", tenantMismatch},
		{"tenant server, opt-in names the default server", Tmux{Socket: "teamq"}, "default", tenantMismatch},
		{"tenant server, opt-in names a same-named socket elsewhere", Tmux{Socket: "teamq"}, filepath.Join(tmpdir, "elsewhere", "teamq"), tenantMismatch},
		{"default server, opt-in names a tenant's", Tmux{}, "teamq", tenantMismatch},
		{"tenant server, an opt-in that is no name", Tmux{Socket: "teamq"}, "team q", tenantInvalid},
		{"tenant server, a relative opt-in path", Tmux{Socket: "teamq"}, "x/teamq", tenantInvalid},
		{"tenant server, opt-in ..", Tmux{Socket: "teamq"}, "..", tenantInvalid},
	} {
		cfg := base
		cfg.Backend = c.backend
		cfg.AgentSpawn.TenantServer = c.optIn
		if got := agentSpawnUnavailable(cfg); got != c.want {
			t.Errorf("%s: %q; want %q", c.name, got, c.want)
		}
	}
}

// TestTheTenantOptInFlagValueParses: what main refuses at startup (exit 2).
func TestTheTenantOptInFlagValueParses(t *testing.T) {
	for v, ok := range map[string]bool{"": true, "teamq": true, "team.q_1-x": true, "/run/user/7/tmux-q/teamq": true,
		"team q": false, "x/teamq": false, "/a/../teamq": false, ".": false, "..": false, "a;b": false, strings.Repeat("q", 65): false} {
		if _, err := ParseAgentSpawnTenantServer(v); (err == nil) != ok {
			t.Errorf("%q: err %v; want ok=%v", v, err, ok)
		}
	}
}

// TestATenantsSpawnsLiveInItsOwnNamespace: on a tenant's server, the
// worktrees go under <agent-spawn-dir>/tenant-<name> (a directory the default
// mirror's sweep never reads: it has no "__"); on the default server nothing
// changes.
func TestATenantsSpawnsLiveInItsOwnNamespace(t *testing.T) {
	u := newSpawnUnit(t)
	dir := realOrClean(u.dir)
	for _, c := range []struct {
		backend Tmux
		ns      string
	}{
		{Tmux{}, ""},
		{Tmux{Socket: "default"}, ""},
		{Tmux{Socket: "teamq"}, "tenant-teamq"},
	} {
		u.c.cfg.Backend = c.backend
		if got := u.c.spawnNamespace(); got != c.ns {
			t.Errorf("%+v: namespace %q; want %q", c.backend, got, c.ns)
		}
		want := dir
		if c.ns != "" {
			want = filepath.Join(dir, c.ns)
		}
		if got := u.c.spawnDir(); got != want {
			t.Errorf("%+v: spawn dir %q; want %q", c.backend, got, want)
		}
		if strings.Contains(c.ns, "__") {
			t.Errorf("namespace %q would be read by the default mirror's sweep", c.ns)
		}
	}
	// An -S server's namespace is a hash of its resolved path: two sockets of
	// the same base name in different directories never share one.
	u.c.cfg.Backend = Tmux{SocketPath: filepath.Join(u.tmp, "a", "teamq")}
	a := u.c.spawnNamespace()
	u.c.cfg.Backend = Tmux{SocketPath: filepath.Join(u.tmp, "b", "teamq")}
	if b := u.c.spawnNamespace(); a == b || !strings.HasPrefix(a, "tenant-") || !hostWordRE.MatchString(a) {
		t.Errorf("-S namespaces %q and %q", a, b)
	}
}

// noServerTmux is a tmux that records each argv (one line each) and answers every
// command as tmux does with no server running.
func noServerTmux(t *testing.T, dir string) (bin, calls string) {
	t.Helper()
	bin, calls = filepath.Join(dir, "faketmux"), filepath.Join(dir, "tmux.calls")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho \"$*\" >> "+calls+"\necho 'no server running on /x' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, calls
}

// TestATenantSpawnWithItsServerAbsentStartsNoServer: the opt-in is right, but
// the tenant's tmux server is not running. The spawn is refused unavailable
// with its own detail, and NO tmux command that can start a server ran — the
// recording tmux saw only commands that cannot (positive control: it did see
// the server check, so it was the tmux the spawn used).
func TestATenantSpawnWithItsServerAbsentStartsNoServer(t *testing.T) {
	u := newSpawnUnit(t)
	bin, calls := noServerTmux(t, u.tmp)
	u.c.cfg.Backend = Tmux{Bin: bin, Socket: "teamq"}
	u.c.cfg.AgentSpawn.TenantServer = "teamq"
	_, e := u.spawn(t, 61, widgets(testPrompt))
	if e == nil || e.code != mirror.SpawnUnavailable || e.detail != tenantAbsent {
		t.Fatalf("refusal %+v; want unavailable %q", e, tenantAbsent)
	}
	b, _ := os.ReadFile(calls)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatal("control: the recording tmux never ran, so it proves nothing")
	}
	for _, l := range lines {
		// Only the server check, which cannot start a server.
		if !strings.HasPrefix(l, "-L teamq display-message ") {
			t.Errorf("a tmux call other than the tenant server's check: %q", l)
		}
		for _, w := range strings.Fields(l) {
			if w == "new-session" || w == "start-server" || w == "start" || w == "new" {
				t.Errorf("a tmux call that starts a server: %q", l)
			}
		}
	}
	if n := u.gitCalls(); n != 0 {
		t.Fatalf("git ran %d times", n)
	}
}

// TestTheSpawnsNewSessionNeverLoadsAConfig: when the `agents` session is
// absent the spawn creates it with new-session — the one command that starts a
// server if none runs (one may have exited since the check). It carries
// `-f /dev/null` before the command, on the mirror's own server, so such a
// server loads no user configuration.
func TestTheSpawnsNewSessionNeverLoadsAConfig(t *testing.T) {
	dir := t.TempDir()
	bin, calls := noServerTmux(t, dir)
	_, _, err := Tmux{Bin: bin, Socket: "teamq"}.newAgentWindow(context.Background(), "cc-7", "/x", []string{"/bin/sh"})
	if err == nil {
		t.Fatal("the fake answered nothing; newAgentWindow cannot have succeeded")
	}
	b, _ := os.ReadFile(calls)
	var created []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.Contains(" "+l+" ", " new-session ") {
			created = append(created, l)
		}
	}
	if len(created) != 1 || !strings.HasPrefix(created[0], "-L teamq -f /dev/null new-session -s "+mirror.AgentSpawnSession+" ") {
		t.Fatalf("new-session calls %q; want one, beginning `-L teamq -f /dev/null new-session -s %s`", created, mirror.AgentSpawnSession)
	}
}
