package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirrorhost"
)

// runWith runs the daemon's run with env and args, answering its exit code,
// stdout and stderr. Its writers are locked: a goroutine the daemon started
// that still logs as run returns (a background loop not yet stopped) must not
// race the read of what it wrote.
func runWith(t *testing.T, env map[string]string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb lockedBuffer
	rc := run(args, func(k string) string { return env[k] }, &out, &errb)
	return rc, out.String(), errb.String()
}

// lockedBuffer is a bytes.Buffer safe for concurrent writes and reads.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestTheTokenNeverHasToBeAnArgument: there is no -token flag (argv is visible
// to every process on the host); the token comes from a file or the environment.
func TestTheTokenNeverHasToBeAnArgument(t *testing.T) {
	if rc, _, errs := runWith(t, nil, "-token", "mmt_x"); rc != 2 || !strings.Contains(errs, "flag provided but not defined: -token") {
		t.Fatalf("-token was accepted (rc %d): %s", rc, errs)
	}
}

// TestALeftoverArgumentIsRefusedNotRunAsTheDaemon: a word the parser leaves
// over — a subcommand this binary does not have, or a typo — fails with
// usage status 2 and says which word, before anything dials or lists. The
// control is the same word missing: the daemon's own usage error instead.
func TestALeftoverArgumentIsRefusedNotRunAsTheDaemon(t *testing.T) {
	for _, args := range [][]string{{"no-such-subcommand"}, {"-allow-http", "stray"}} {
		rc, out, errs := runWith(t, map[string]string{envURL: "https://m.example"}, args...)
		if rc != 2 || out != "" || !strings.Contains(errs, `unknown command or argument "`+args[len(args)-1]+`"`) {
			t.Fatalf("%q: rc %d, stdout %q, stderr %q; want 2 naming the word", args, rc, out, errs)
		}
	}
	if rc, _, errs := runWith(t, map[string]string{envURL: "https://m.example"}, "-allow-http"); rc != 2 || strings.Contains(errs, "unknown command") || !strings.Contains(errs, "are required") {
		t.Fatalf("CONTROL: no leftover word: rc %d, %s; want the missing-token usage error", rc, errs)
	}
}

// TestMissingURLOrTokenIsAUsageError, and a non-mirror token or a cleartext URL
// is refused before anything dials.
func TestMissingURLOrTokenIsAUsageError(t *testing.T) {
	if rc, _, errs := runWith(t, map[string]string{envURL: "https://m.example"}); rc != 2 || !strings.Contains(errs, "are required") {
		t.Fatalf("no token: rc %d %s", rc, errs)
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "token")
	if err := os.WriteFile(f, []byte("hook-token-not-a-mirror-one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rc, _, errs := runWith(t, map[string]string{envURL: "https://m.example", envHost: "laptop"}, "-token-file", f); rc != 2 || !strings.Contains(errs, "not a mirror token") {
		t.Fatalf("hook token in the file: rc %d %s", rc, errs)
	}
	if rc, _, errs := runWith(t, map[string]string{envURL: "http://192.0.2.1:8105", envToken: "mmt_x", envHost: "laptop"}); rc != 2 || !strings.Contains(errs, "cleartext") {
		t.Fatalf("http without -allow-http: rc %d %s", rc, errs)
	}
}

// TestABadTenantSpawnServerStopsTheDaemon: a -agent-spawn-tenant-server that is
// neither a socket name nor an absolute socket path is a usage error, from the
// flag or its env var, before anything dials. CONTROL: a valid one gets past it
// to the next check (the cleartext URL's).
func TestABadTenantSpawnServerStopsTheDaemon(t *testing.T) {
	env := map[string]string{envURL: "http://192.0.2.1:8105", envToken: "mmt_x", envHost: "laptop"}
	if rc, _, errs := runWith(t, env, "-agent-spawn-tenant-server", "x/teamq"); rc != 2 || !strings.Contains(errs, `-agent-spawn-tenant-server "x/teamq"`) {
		t.Fatalf("flag: rc %d %s", rc, errs)
	}
	env[envAgentSpawnTenantServer] = "team q"
	if rc, _, errs := runWith(t, env); rc != 2 || !strings.Contains(errs, `-agent-spawn-tenant-server "team q"`) {
		t.Fatalf("env: rc %d %s", rc, errs)
	}
	env[envAgentSpawnTenantServer] = "teamq"
	if rc, _, errs := runWith(t, env); rc != 2 || strings.Contains(errs, "agent-spawn-tenant-server") || !strings.Contains(errs, "cleartext") {
		t.Fatalf("CONTROL, a valid value: rc %d %s; want the cleartext refusal", rc, errs)
	}
}

// TestTheTenantSpawnOptInReachesTheHost: a tenant's daemon (-tmux-socket
// teamq) with -agent-spawn says at startup that it starts agents on that
// tenant's server only when -agent-spawn-tenant-server names it; without it,
// that it starts none, for the old reason. The daemon's context is ended
// before it dials, so only its startup runs.
func TestTheTenantSpawnOptInReachesTheHost(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMUX_TMPDIR", tmp) // a private, absent tenant server: nothing to touch
	t.Setenv("TMUX", "")
	prevC := runContext
	t.Cleanup(func() { runContext = prevC })
	for _, c := range []struct{ optIn, want string }{
		{"teamq", `STARTING AGENTS IS ON (-agent-spawn): your chief may start claude (or opencode, with -opencode-send and a token) in tmux session "agents" on the tenant tmux server "teamq"`},
		{"", "-agent-spawn is set but this host starts no agent: not the default tmux server"},
	} {
		runContext = func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, cancel
		}
		rc, _, errs := runWith(t, map[string]string{envURL: "https://m.example", envToken: "mmt_" + strings.Repeat("A", 43), envHost: "laptop",
			envSocket: "teamq", envInteractive: "1", envAgentSpawn: "1", envAgentSpawnRepos: "acme/widgets", envAgentSpawnTenantServer: c.optIn,
			"XDG_RUNTIME_DIR": filepath.Join(tmp, "run"), "XDG_DATA_HOME": filepath.Join(tmp, "data"), "XDG_STATE_HOME": filepath.Join(tmp, "state"),
			envAgentSpawnRoots: filepath.Join(tmp, "src"), "HOME": tmp})
		if rc != 0 || !strings.Contains(errs, c.want) {
			t.Fatalf("opt-in %q: rc %d, log:\n%s\nwant %q", c.optIn, rc, errs, c.want)
		}
	}
}

// listBackend is a host's tmux for -list: fixed sessions and active windows.
type listBackend struct{}

func (listBackend) List(context.Context) ([]string, error) {
	return []string{"0", "infra-work", "scratch13", "scratch2"}, nil
}
func (listBackend) Attach(context.Context, string, bool, uint16, uint16) (mirrorhost.Terminal, error) {
	return nil, errors.New("not in -list")
}
func (listBackend) ActiveWindows(context.Context) (map[string]string, error) {
	return map[string]string{"scratch13": "1", "scratch2": "8", "infra-work": "2", "0": "0"}, nil
}

func withListBackend(t *testing.T) {
	t.Helper()
	prev := e2eBackend
	e2eBackend = func() mirrorhost.Backend { return listBackend{} }
	t.Cleanup(func() { e2eBackend = prev })
}

// TestListShowsTheLabelAsALastColumn: -list prints what would be registered,
// and a labelled session's line ends in `label=<codename>:<window>`; the
// operator's own examples. Sessions with no codename print as before.
func TestListShowsTheLabelAsALastColumn(t *testing.T) {
	withListBackend(t)
	f := filepath.Join(t.TempDir(), "labels.json")
	if err := os.WriteFile(f, []byte(`{"scratch13":"mango","scratch2":"Gold"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rc, out, errs := runWith(t, map[string]string{envLabelsFile: f}, "-list")
	if rc != 0 {
		t.Fatalf("-list: rc %d: %s", rc, errs)
	}
	if want := "0\ninfra-work\nscratch13\tlabel=mango:1\nscratch2\tlabel=Gold:8\n"; out != want {
		t.Fatalf("-list printed %q, want %q", out, want)
	}
}

// TestABadLabelsFileIsNeverFatal: a missing or malformed labels file is logged
// and the host runs exactly as with none — -list still succeeds, unlabelled.
func TestABadLabelsFileIsNeverFatal(t *testing.T) {
	withListBackend(t)
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte(`["not","an","object"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{filepath.Join(t.TempDir(), "absent.json"), bad} {
		rc, out, errs := runWith(t, nil, "-list", "-labels-file", f)
		if rc != 0 || out != "0\ninfra-work\nscratch13\nscratch2\n" {
			t.Errorf("%s: rc %d, out %q, want 0 and the raw list", f, rc, out)
		}
		if strings.Count(errs, "no session labels") != 1 {
			t.Errorf("%s: want the failure logged exactly once, got: %s", f, errs)
		}
	}
}

// windowsListBackend is a per-window host: one session, three windows — two
// running Claude Code (resolved two different ways), one a shell.
type windowsListBackend struct{ listBackend }

func (windowsListBackend) Windows(context.Context) ([]mirrorhost.WindowInfo, error) {
	return []mirrorhost.WindowInfo{
		{Session: "scratch13", ID: "@46", Index: 1, Active: true, Pane: "%46", Command: "claude",
			Claude: &mirrorhost.ClaudePane{Pane: "%46", Gen: "sess-b", Transcript: "/p/sess-b.jsonl", Busy: true, Source: mirrorhost.SourcePaneMap}},
		{Session: "scratch13", ID: "@47", Index: 2, Pane: "%47", Command: "claude",
			Claude: &mirrorhost.ClaudePane{Pane: "%47", Gen: "sess-c", Transcript: "/p/sess-c.jsonl", Source: mirrorhost.SourceSessions}},
		{Session: "scratch13", ID: "@48", Index: 3, Pane: "%48", Command: "zsh"},
	}, nil
}

// TestListClaudeIsOneRowPerClaudeWINDOW: two Claude Code windows of ONE session
// are two rows, each with its own window id, pane, session id, transcript and
// how it was resolved; the shell window is not listed. (It printed one row per
// SESSION — the active window's — which hid the window being debugged.)
func TestListClaudeIsOneRowPerClaudeWINDOW(t *testing.T) {
	prev := e2eBackend
	e2eBackend = func() mirrorhost.Backend { return windowsListBackend{} }
	t.Cleanup(func() { e2eBackend = prev })
	f := filepath.Join(t.TempDir(), "labels.json")
	if err := os.WriteFile(f, []byte(`{"scratch13":"mango"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rc, out, errs := runWith(t, map[string]string{envLabelsFile: f}, "-list", "-claude")
	if rc != 0 {
		t.Fatalf("-list -claude: rc %d: %s", rc, errs)
	}
	want := "scratch13\t@46\t1\tclaude\t%46\tbusy\tsess-b\t/p/sess-b.jsonl\tpane-map\tlabel=mango:1\n" +
		"scratch13\t@47\t2\tclaude\t%47\tidle\tsess-c\t/p/sess-c.jsonl\tsessions\tlabel=mango:2\n"
	if out != want {
		t.Fatalf("-list -claude printed\n%q\nwant\n%q", out, want)
	}
}

// TestTheTmuxBackendLooksForLiveCopies: the host the daemon runs warns about a
// conversation that also runs elsewhere (mirrorhost's livecopy.go) — the
// scanner is on, not merely written.
func TestTheTmuxBackendLooksForLiveCopies(t *testing.T) {
	if b := tmuxBackend("", "", "").(mirrorhost.Tmux); b.LiveCopies == nil {
		t.Fatal("the tmux backend has no LiveCopies: no window would ever say its conversation runs elsewhere")
	}
}

// TestThePaneMapDirReachesTheTmuxBackend: the flag/env is not decoration — the
// backend the host runs reads the map from it.
func TestThePaneMapDirReachesTheTmuxBackend(t *testing.T) {
	b, ok := tmuxBackend("s", "", "/fx/ledger").(mirrorhost.Tmux)
	if !ok || b.Claude.PaneMapDir != "/fx/ledger" || b.Socket != "s" {
		t.Fatalf("tmuxBackend = %+v, want PaneMapDir /fx/ledger", b)
	}
	if b := tmuxBackend("", "", "").(mirrorhost.Tmux); b.Claude.PaneMapDir != "" {
		t.Fatalf("no pane map dir configured, yet the backend has %q", b.Claude.PaneMapDir)
	}
	// And run passes it: from the environment, and from the flag (which wins).
	prev := newTmuxBackend
	t.Cleanup(func() { newTmuxBackend = prev })
	var got []string
	newTmuxBackend = func(s, sp, dir string) mirrorhost.Backend {
		got = append(got, dir)
		return listBackend{}
	}
	if rc, _, errs := runWith(t, map[string]string{envPaneMapDir: "/fx/from-env"}, "-list"); rc != 0 {
		t.Fatalf("-list: rc %d %s", rc, errs)
	}
	if rc, _, errs := runWith(t, map[string]string{envPaneMapDir: "/fx/from-env"}, "-list", "-pane-map-dir", "/fx/from-flag"); rc != 0 {
		t.Fatalf("-list -pane-map-dir: rc %d %s", rc, errs)
	}
	if rc, _, errs := runWith(t, nil, "-list"); rc != 0 {
		t.Fatalf("-list: rc %d %s", rc, errs)
	}
	if strings.Join(got, "|") != "/fx/from-env|/fx/from-flag|" {
		t.Fatalf("run passed pane map dirs %q, want env, flag, none", got)
	}
}

func TestVersion(t *testing.T) {
	if rc, out, _ := runWith(t, nil, "-version"); rc != 0 || !strings.Contains(out, "tunnel protocol 2") {
		t.Fatalf("-version: rc %d %q", rc, out)
	}
}

// TestCatalogPrintsWhatTheHostWouldReport: -catalog prints the composer menu's
// catalog for a pane in the current directory — the user's skills (through
// CLAUDE_CONFIG_DIR) — without dialling anything.
func TestCatalogPrintsWhatTheHostWouldReport(t *testing.T) {
	cfg := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cfg, "skills", "printed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "skills", "printed", "SKILL.md"), []byte("---\nname: printed\ndescription: shown by -catalog\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	rc, out, errs := runWith(t, map[string]string{}, "-catalog")
	if rc != 0 {
		t.Fatalf("rc %d %s", rc, errs)
	}
	if !strings.Contains(out, "user\tskill\t/printed\tshown by -catalog\n") {
		t.Fatalf("printed:\n%s", out)
	}
}

// TestAnMCPSetupFailureDoesNotStopMirroring: with the opt-in MCP endpoint's
// port already taken, the host still dials muster — it mirrors without the
// endpoint and says so — and stops cleanly when told to.
func TestAnMCPSetupFailureDoesNotStopMirroring(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	dialled := make(chan struct{}, 1)
	muster := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mirror/connect" {
			select {
			case dialled <- struct{}{}:
			default:
			}
		}
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	defer muster.Close()
	prevB, prevC := newTmuxBackend, runContext
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { newTmuxBackend, runContext = prevB, prevC; cancel() })
	newTmuxBackend = func(string, string, string) mirrorhost.Backend { return listBackend{} }
	runContext = func() (context.Context, context.CancelFunc) { return ctx, cancel }
	type result struct {
		rc        int
		out, errs string
	}
	done := make(chan result, 1)
	go func() {
		rc, out, errs := runWith(t, map[string]string{envURL: muster.URL, envToken: "mmt_" + strings.Repeat("A", 43), envHost: "laptop",
			envAllowHTTP: "1", envMCPListen: taken.Addr().String(), envMCPSecret: filepath.Join(t.TempDir(), "mcp-secret")})
		done <- result{rc, out, errs}
	}()
	select {
	case <-dialled:
	case r := <-done:
		t.Fatalf("run ended before dialling muster: rc %d %s", r.rc, r.errs)
	case <-time.After(10 * time.Second):
		t.Fatal("the host never dialled muster")
	}
	cancel()
	select {
	case r := <-done:
		if r.rc != 0 || !strings.Contains(r.errs, "MCP endpoint is OFF, mirroring continues") {
			t.Fatalf("rc %d, log: %s", r.rc, r.errs)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not stop")
	}
}

// TestADanglingSymlinkSecretNeverDelaysMirroring: with the MCP secret file a
// dangling symlink, the host still dials muster at once (the endpoint is off,
// and says so) — setting MCP up never stands between the host and mirroring.
func TestADanglingSymlinkSecretNeverDelaysMirroring(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "mcp-secret")
	if err := os.Symlink(filepath.Join(dir, "nowhere"), secret); err != nil {
		t.Fatal(err)
	}
	dialled := make(chan struct{}, 1)
	muster := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mirror/connect" {
			select {
			case dialled <- struct{}{}:
			default:
			}
		}
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	defer muster.Close()
	prevB, prevC := newTmuxBackend, runContext
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { newTmuxBackend, runContext = prevB, prevC; cancel() })
	newTmuxBackend = func(string, string, string) mirrorhost.Backend { return listBackend{} }
	runContext = func() (context.Context, context.CancelFunc) { return ctx, cancel }
	done := make(chan string, 1)
	go func() {
		_, _, errs := runWith(t, map[string]string{envURL: muster.URL, envToken: "mmt_" + strings.Repeat("A", 43), envHost: "laptop",
			envAllowHTTP: "1", envMCPListen: "127.0.0.1:47999", envMCPSecret: secret})
		done <- errs
	}()
	select {
	case <-dialled:
	case <-time.After(5 * time.Second):
		t.Fatal("the host did not dial muster within 5s")
	}
	cancel()
	select {
	case errs := <-done:
		if !strings.Contains(errs, "symlink") {
			t.Fatalf("the reason MCP is off was not logged: %s", errs)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not stop")
	}
}

// TestSpawnedAgentsSkipPermissionPromptsUnlessTurnedOff: with -agent-spawn on,
// the daemon's startup line says its spawned agents start WITHOUT permission
// prompts by default; MUSTER_MIRROR_AGENT_SPAWN_SKIP_PERMISSIONS=0 (or false),
// or -agent-spawn-skip-permissions=false, says normal permission mode instead;
// =1 keeps it on; as root (geteuid 0) claude keeps its prompts and the line
// says why; and a value that is not a bool stops the daemon (rc 2) naming the
// variable and the values strconv.ParseBool accepts. The line is worded from
// the built AgentSpawnConfig, so it also pins the flag-to-config assignment.
// The daemon's context is ended before it dials.
func TestSpawnedAgentsSkipPermissionPromptsUnlessTurnedOff(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMUX_TMPDIR", tmp) // a private, absent tmux server: nothing to touch
	t.Setenv("TMUX", "")
	prevC := runContext
	t.Cleanup(func() { runContext = prevC })
	runContext = func() (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx, cancel
	}
	prevE := geteuid
	t.Cleanup(func() { geteuid = prevE })
	const (
		on   = ", WITHOUT permission prompts (-agent-spawn-skip-permissions): they run their tools without asking"
		off  = ", in normal permission mode (-agent-spawn-skip-permissions=false)"
		root = ", claude in normal permission mode because this mirror runs as root (effective uid 0) and Claude Code refuses " +
			"bypassPermissions as root, opencode WITHOUT permission prompts (-agent-spawn-skip-permissions, --auto)"
	)
	env := func(skip string) map[string]string {
		e := map[string]string{envURL: "https://m.example", envToken: "mmt_" + strings.Repeat("A", 43), envHost: "laptop",
			envSocket: "teamq", envInteractive: "1", envAgentSpawn: "1", envAgentSpawnRepos: "acme/widgets", envAgentSpawnTenantServer: "teamq",
			"XDG_RUNTIME_DIR": filepath.Join(tmp, "run"), "XDG_DATA_HOME": filepath.Join(tmp, "data"), "XDG_STATE_HOME": filepath.Join(tmp, "state"),
			envAgentSpawnRoots: filepath.Join(tmp, "src"), "HOME": tmp}
		if skip != "" {
			e[envAgentSpawnSkipPermissions] = skip
		}
		return e
	}
	for _, c := range []struct {
		name, env string
		args      []string
		euid      int
		want      string
	}{
		{"the default", "", nil, 1000, on},
		{"env 1", "1", nil, 1000, on},
		{"env 0", "0", nil, 1000, off},
		{"env false", "false", nil, 1000, off},
		{"env F", "F", nil, 1000, off},
		{"the flag =false", "", []string{"-agent-spawn-skip-permissions=false"}, 1000, off},
		{"the flag over env 0", "0", []string{"-agent-spawn-skip-permissions=true"}, 1000, on},
		// As root claude keeps its prompts (Claude Code refuses
		// bypassPermissions as root) and the line says why; off is off.
		{"root, the default", "", nil, 0, root},
		{"root, env 0", "0", nil, 0, off},
	} {
		geteuid = func() int { return c.euid }
		rc, _, errs := runWith(t, env(c.env), c.args...)
		if rc != 0 || !strings.Contains(errs, "STARTING AGENTS IS ON") || !strings.Contains(errs, c.want) {
			t.Fatalf("%s: rc %d, log:\n%s\nwant %q", c.name, rc, errs, c.want)
		}
		for _, other := range []string{on, off, root} {
			if other != c.want && strings.Contains(errs, other) {
				t.Fatalf("%s: the log also says %q:\n%s", c.name, other, errs)
			}
		}
	}
	geteuid = func() int { return 1000 }
	if rc, _, errs := runWith(t, env("off")); rc != 2 || !strings.Contains(errs,
		`MUSTER_MIRROR_AGENT_SPAWN_SKIP_PERMISSIONS="off" is not a bool (1, t, T, TRUE, true, True, 0, f, F, FALSE, false or False)`) {
		t.Fatalf("a value that is not a bool: rc %d: %s", rc, errs)
	}
}
