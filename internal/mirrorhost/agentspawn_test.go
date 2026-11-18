package mirrorhost

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// The host spawn's checks that need no tmux (agentspawn.go). The spawn itself,
// against a real tmux server and real git, is agentspawn_tmux_integration_test.go.

// spawnUnit is a host configured to spawn, with a git that RECORDS every
// invocation (and runs the real one), so a test can count git calls.
type spawnUnit struct {
	c                          *Client
	tmp, dir, log, gitLog, git string
}

const testPrompt = mirror.SpawnPreface + " on task #9: fix the typo"

func newSpawnUnit(t *testing.T) *spawnUnit {
	t.Helper()
	tmp, err := os.MkdirTemp("/tmp", "msu")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	// HERMETIC: tmux's default server is a private, absent one under tmp, so
	// a spawn that gets as far as tmux finds no server there — never the
	// operator's ($TMUX would point a client at theirs).
	t.Setenv("TMUX_TMPDIR", tmp)
	t.Setenv("TMUX", "")
	u := &spawnUnit{tmp: tmp, dir: filepath.Join(tmp, "agents"), log: filepath.Join(tmp, "state", "spawn.jsonl"), gitLog: filepath.Join(tmp, "git.calls")}
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("git is not installed: %v", err)
	}
	u.git = filepath.Join(tmp, "git")
	if err := os.WriteFile(u.git, []byte("#!/bin/sh\necho \"$*\" >> "+u.gitLog+"\nexec "+real+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	u.c = &Client{log: log.New(io.Discard, "", 0), cfg: Config{Interactive: true, Backend: Tmux{},
		AgentSpawn: AgentSpawnConfig{Enabled: true, Roots: []string{filepath.Join(tmp, "src")}, Dir: u.dir, Repos: []string{"acme/widgets"},
			Log: u.log, Server: "https://muster.invalid", RuntimeDir: filepath.Join(tmp, "run"), Git: u.git, ClaudeBin: "/bin/true",
			ClaudeState: filepath.Join(tmp, "claude.json")}}}
	return u
}

// gitCalls is how many times the recording git ran.
func (u *spawnUnit) gitCalls() int {
	b, _ := os.ReadFile(u.gitLog)
	return strings.Count(string(b), "\n")
}

// auditLines is the audit log's lines, decoded, in order: counted, never grepped.
func (u *spawnUnit) auditLines(t *testing.T) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(u.log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("an audit line does not parse: %q", l)
		}
		out = append(out, m)
	}
	return out
}

func (u *spawnUnit) spawn(t *testing.T, dispatch int64, req mirror.AgentSpawnReq) (mirror.AgentSpawned, *errSpawn) {
	t.Helper()
	w, err := u.c.spawnAgent(context.Background(), nil, mirror.Msg{Type: mirror.TypeAgentSpawn, Dispatch: dispatch, Spawn: &req}, time.Now())
	if err == nil {
		return w, nil
	}
	e, ok := err.(*errSpawn)
	if !ok {
		t.Fatalf("a refusal that is not an *errSpawn: %v", err)
	}
	return w, e
}

func widgets(prompt string) mirror.AgentSpawnReq {
	return mirror.AgentSpawnReq{Runtime: mirror.SpawnRuntimeClaude, Repo: "acme/widgets", Prompt: prompt, Name: "#9 fix the typo"}
}

// TestASpawnRefusedBeforeGitRunsNoGitAndLogsBothLines: each refusal the host
// makes from the request alone — whatever the server sent — runs NO git
// command, and still writes the request's two audit lines (counted). Each
// case is its own code and detail, so none can pass for another.
func TestASpawnRefusedBeforeGitRunsNoGitAndLogsBothLines(t *testing.T) {
	for _, c := range []struct {
		name   string
		repos  []string
		req    mirror.AgentSpawnReq
		code   string
		detail string
	}{
		{"an empty allow-list", nil, widgets(testPrompt), mirror.SpawnNoRepo, "repo acme/widgets is not on this host's allow-list"},
		{"a repo not on the allow-list", []string{"acme/other"}, widgets(testPrompt), mirror.SpawnNoRepo, "repo acme/widgets is not on this host's allow-list"},
		{"a prompt that is a claude subcommand", []string{"acme/widgets"}, widgets("update"), mirror.SpawnFailed, "the prompt does not begin with muster's preface"},
		{"a prompt that is a flag", []string{"acme/widgets"}, widgets("--dangerously-skip-permissions"), mirror.SpawnFailed, "the prompt does not begin with muster's preface"},
		{"a base that is a git option", []string{"acme/widgets"}, func() mirror.AgentSpawnReq { r := widgets(testPrompt); r.Base = "--upload-pack=x"; return r }(),
			mirror.SpawnBadBase, `branch "--upload-pack=x" is not a branch name this host accepts`},
		{"a base with ..", []string{"acme/widgets"}, func() mirror.AgentSpawnReq { r := widgets(testPrompt); r.Base = "main/../x"; return r }(),
			mirror.SpawnBadBase, `branch "main/../x" is not a branch name this host accepts`},
		{"a model on claude", []string{"acme/widgets"}, func() mirror.AgentSpawnReq { r := widgets(testPrompt); r.Model = "openrouter/x/y"; return r }(),
			mirror.SpawnFailed, "muster sent a model for claude (a muster bug)"},
		{"opencode with no model", []string{"acme/widgets"}, func() mirror.AgentSpawnReq { r := widgets(testPrompt); r.Runtime = "opencode"; return r }(),
			mirror.SpawnBadModel, `"" is not an OpenRouter model (openrouter/<vendor>/<model>)`},
		{"opencode with a model of another provider", []string{"acme/widgets"}, func() mirror.AgentSpawnReq {
			r := widgets(testPrompt)
			r.Runtime, r.Model = "opencode", "a/b/c"
			return r
		}(), mirror.SpawnBadModel, `"a/b/c" is not an OpenRouter model (openrouter/<vendor>/<model>)`},
		{"opencode on a host without -opencode-send", []string{"acme/widgets"}, func() mirror.AgentSpawnReq {
			r := widgets(testPrompt)
			r.Runtime, r.Model = "opencode", "openrouter/qwen/qwen3-coder"
			return r
		}(), mirror.SpawnUnavailable, "this host does not send to opencode (-opencode-send is off)"},
		{"an unknown runtime", []string{"acme/widgets"}, func() mirror.AgentSpawnReq { r := widgets(testPrompt); r.Runtime = "sh"; return r }(),
			mirror.SpawnFailed, "muster sent an unknown runtime (a muster bug)"},
		{"a repo that is a path", []string{"acme/widgets"}, func() mirror.AgentSpawnReq { r := widgets(testPrompt); r.Repo = "../../etc"; return r }(),
			mirror.SpawnNoRepo, `"../../etc" is not <owner>/<repo>`},
	} {
		t.Run(c.name, func(t *testing.T) {
			u := newSpawnUnit(t)
			u.c.cfg.AgentSpawn.Repos = c.repos
			_, e := u.spawn(t, 41, c.req)
			if e == nil || e.code != c.code || e.detail != c.detail {
				t.Fatalf("refusal %+v; want %s %q", e, c.code, c.detail)
			}
			if n := u.gitCalls(); n != 0 {
				t.Fatalf("git ran %d times for a refused request", n)
			}
			lines := u.auditLines(t)
			if len(lines) != 2 || lines[0]["kind"] != "request" || lines[1]["kind"] != "result" || lines[1]["result"] != c.code ||
				lines[0]["prompt"] != c.req.Prompt || lines[1]["admitted"] != false {
				t.Fatalf("audit lines %v; want a request line with the prompt, then a result line %s, not admitted", lines, c.code)
			}
		})
	}
}

// TestAHostWithAgentSpawnOffRefusesAndTouchesNothing: -agent-spawn off (the
// default), or no -interactive: refused unavailable with its own detail,
// whatever the request — no git call, and no audit file is even created (a
// host that never opted in writes nothing). Its hello offers no runtime.
func TestAHostWithAgentSpawnOffRefusesAndTouchesNothing(t *testing.T) {
	for _, c := range []struct {
		name, why string
		mod       func(*Config)
	}{
		{"off", "agent spawn is off", func(c *Config) { c.AgentSpawn.Enabled = false }},
		{"read-only", "not interactive", func(c *Config) { c.Interactive = false }},
	} {
		u := newSpawnUnit(t)
		c.mod(&u.c.cfg)
		if _, e := u.spawn(t, 51, widgets(testPrompt)); e == nil || e.code != mirror.SpawnUnavailable || e.detail != c.why {
			t.Fatalf("%s: %+v", c.name, e)
		}
		if _, err := os.Stat(u.log); !os.IsNotExist(err) || u.gitCalls() != 0 {
			t.Fatalf("%s: the audit log exists (%v) or git ran (%d)", c.name, err, u.gitCalls())
		}
		h := &hostSide{c: u.c, version: 2, ctx: context.Background()}
		if rs := h.agentSpawnRuntimes(); len(rs) != 0 {
			t.Fatalf("%s: the hello offers %v", c.name, rs)
		}
	}
}

// TestAnUnwritableAuditLogRefusesTheSpawnBeforeAnythingRuns: the first audit
// line cannot be written (its directory is read-only) → unavailable, "audit log
// unwritable", and git never runs. The CONTROL: the same request with a
// writable log gets past the log (it is then refused later, by the allow-list,
// with a different code) — so the refusal is the log's.
func TestAnUnwritableAuditLogRefusesTheSpawnBeforeAnythingRuns(t *testing.T) {
	u := newSpawnUnit(t)
	ro := filepath.Join(u.tmp, "ro")
	if err := os.MkdirAll(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	u.c.cfg.AgentSpawn.Log = filepath.Join(ro, "spawn.jsonl")
	if _, e := u.spawn(t, 42, widgets(testPrompt)); e == nil || e.code != mirror.SpawnUnavailable || e.detail != "audit log unwritable" {
		t.Fatalf("with a read-only log: %+v", e)
	}
	if n := u.gitCalls(); n != 0 {
		t.Fatalf("git ran %d times with no audit line written", n)
	}
	u.c.cfg.AgentSpawn.Log = u.log
	u.c.cfg.AgentSpawn.Repos = nil
	if _, e := u.spawn(t, 43, widgets(testPrompt)); e == nil || e.code != mirror.SpawnNoRepo {
		t.Fatalf("control, writable log: %+v; want the allow-list's no_repo", e)
	}
}

// TestTheRateLimitIsCountedFromTheAuditLogAndSurvivesARestart: three admitted
// spawns in the last ten minutes (written to the log, as a previous process
// would have) refuse the fourth — on a FRESH client, so nothing in memory
// counts; ten in 24 hours refuse the eleventh; refused requests do not count.
// Positive control: two admitted spawns let the request past the limit (it is
// then refused for the next reason, with another code).
func TestTheRateLimitIsCountedFromTheAuditLogAndSurvivesARestart(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		name  string
		ago   []time.Duration
		extra int // refused (not admitted) result lines
		want  string
	}{
		{"3 in 10 minutes", []time.Duration{time.Minute, 2 * time.Minute, 9 * time.Minute}, 5, "rate limited: 3 spawns in the last 10 minutes"},
		{"10 in 24 hours", []time.Duration{20 * time.Minute, time.Hour, 2 * time.Hour, 3 * time.Hour, 4 * time.Hour, 5 * time.Hour, 6 * time.Hour, 7 * time.Hour, 8 * time.Hour, 23 * time.Hour}, 0, "rate limited: 10 spawns in the last 24 hours"},
		{"control: 2 in 10 minutes", []time.Duration{time.Minute, 2 * time.Minute}, 9, ""},
		{"control: 3, one 11 minutes old", []time.Duration{time.Minute, 2 * time.Minute, 11 * time.Minute}, 0, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			u := newSpawnUnit(t)
			for _, a := range c.ago {
				if err := appendAudit(u.log, spawnAuditAdmit{At: now.Add(-a), Kind: "admit", Dispatch: 1}); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < c.extra; i++ {
				if err := appendAudit(u.log, spawnAuditResult{At: now, Kind: "result", Dispatch: 2, Result: mirror.SpawnNoRepo}); err != nil {
					t.Fatal(err)
				}
			}
			// A fresh client over the same log: what a restarted mirror is.
			fresh := newSpawnUnit(t)
			fresh.c.cfg.AgentSpawn.Log = u.log
			fresh.c.cfg.AgentSpawn.Now = func() time.Time { return now }
			fresh.c.cfg.AgentSpawn.Roots = nil // past the limit, the next refusal is the clone's
			_, e := fresh.spawn(t, 44, widgets(testPrompt))
			if c.want != "" {
				if e == nil || e.code != mirror.SpawnUnavailable || e.detail != c.want {
					t.Fatalf("refusal %+v; want unavailable %q", e, c.want)
				}
				return
			}
			if e == nil || e.code == mirror.SpawnUnavailable && strings.HasPrefix(e.detail, "rate limited") {
				t.Fatalf("control: refused by the limit (%+v)", e)
			}
		})
	}
}

// TestTheHostRefusesASpawnUnlessItIsTheDefaultTmuxServer: design 3.8. A tenant's
// mirror (any non-default server) refuses whatever muster sends; the default
// server, however named, is accepted; a custom TMUX_TMPDIR is honoured.
func TestTheHostRefusesASpawnUnlessItIsTheDefaultTmuxServer(t *testing.T) {
	tmp, err := os.MkdirTemp("/tmp", "mst")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	t.Setenv("TMUX_TMPDIR", tmp)
	def := filepath.Join(tmp, "tmux-"+strconv.Itoa(os.Getuid()), "default")
	if err := os.MkdirAll(filepath.Dir(def), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tmp, "via-link")
	if err := os.Symlink(filepath.Dir(def), link); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(tmp, "elsewhere", "default")
	for _, c := range []struct {
		name string
		t    Tmux
		ok   bool
	}{
		{"no flag", Tmux{}, true},
		{"-L default", Tmux{Socket: "default"}, true},
		{"-S the default socket", Tmux{SocketPath: def}, true},
		{"-S a path that realpaths to it", Tmux{SocketPath: filepath.Join(link, "default")}, true},
		{"-L work", Tmux{Socket: "work"}, false},
		{"-S a socket named default elsewhere", Tmux{SocketPath: other}, false},
	} {
		if why := notDefaultTmuxServer(c.t); (why == "") != c.ok || (!c.ok && why != "not the default tmux server") {
			t.Errorf("%s: %q; want accepted=%v", c.name, why, c.ok)
		}
	}
	// The same -S path under ANOTHER TMUX_TMPDIR is not the default server.
	t.Setenv("TMUX_TMPDIR", filepath.Join(tmp, "elsewhere-root"))
	if why := notDefaultTmuxServer(Tmux{SocketPath: def}); why == "" {
		t.Error("TMUX_TMPDIR is not honoured: the old default socket is still accepted")
	}
}

// TestAgentSpawnPreconditions: each startup refusal, its own detail. A spawn
// directory with a ';' would put a word tmux splits on into the command.
func TestAgentSpawnPreconditions(t *testing.T) {
	base := newSpawnUnit(t).c.cfg
	for _, c := range []struct {
		name string
		mod  func(*Config)
		want string
	}{
		{"ready", func(*Config) {}, ""},
		{"not interactive", func(c *Config) { c.Interactive = false }, "not interactive"},
		{"off", func(c *Config) { c.AgentSpawn.Enabled = false }, "agent spawn is off"},
		{"no XDG_RUNTIME_DIR", func(c *Config) { c.AgentSpawn.RuntimeDir = "" }, "no XDG_RUNTIME_DIR for prompt files"},
		{"a ; in the agents directory", func(c *Config) { c.AgentSpawn.Dir += ";x" }, "a spawn path has characters tmux would parse"},
		{"a ; in the runtime directory", func(c *Config) { c.AgentSpawn.RuntimeDir += ";x" }, "a spawn path has characters tmux would parse"},
		{"agents directory under a root", func(c *Config) { c.AgentSpawn.Dir = filepath.Join(c.AgentSpawn.Roots[0], "agents") }, "the agents directory and a root must not contain each other"},
		{"a tenant's tmux server", func(c *Config) { c.Backend = Tmux{Socket: "tenant"} }, "not the default tmux server"},
	} {
		cfg := base
		cfg.AgentSpawn.Roots = append([]string(nil), base.AgentSpawn.Roots...)
		c.mod(&cfg)
		if got := agentSpawnUnavailable(cfg); got != c.want {
			t.Errorf("%s: %q; want %q", c.name, got, c.want)
		}
	}
}

// TestNoSpawnConstantWordEndsInASemicolon: tmux splits a command at an
// argument ending in ';'. Every constant word the spawn hands tmux is listed
// (spawnConstantWords), the claude wrapper among them, and none ends so. The
// control: a word that does end in ';' is caught by the same check.
func TestNoSpawnConstantWordEndsInASemicolon(t *testing.T) {
	ends := func(w string) bool { return strings.HasSuffix(w, ";") }
	if !ends(`x;`) {
		t.Fatal("control: the check cannot go red")
	}
	for _, w := range spawnConstantWords {
		if ends(w) {
			t.Errorf("constant word %q ends in ';'", w)
		}
	}
	if !strings.Contains(strings.Join(spawnConstantWords, "\x00"), claudeWrapper) {
		t.Error("the claude wrapper is not among the enumerated words")
	}
}

// TestTheWrapperReadsThePromptAndExits97WhenItCannot: the constant wrapper,
// run by a real sh: the prompt arrives as ONE final argument, byte for byte
// (trailing newlines included), the file is gone, and nothing in it runs. A
// file that cannot be read exits 97.
func TestTheWrapperReadsThePromptAndExits97WhenItCannot(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "pwned")
	for _, prompt := range []string{testPrompt + " a;b", testPrompt + " tail;", testPrompt + ` \;`, testPrompt + " $(touch " + marker + ") `touch " + marker + "`",
		testPrompt + ` 'single' "double"`, testPrompt + "\n-leading dash line\n\n", strings.Repeat("x", 8<<10)} {
		f := filepath.Join(dir, "prompt")
		if err := os.WriteFile(f, []byte(prompt), 0o600); err != nil {
			t.Fatal(err)
		}
		// The printer stands for claude: its $1 is "--" (the argv's last
		// constant word), its $2 the prompt the wrapper appended.
		out, err := exec.Command("/bin/sh", "-c", claudeWrapper, "sh", f, "/bin/sh", "-c", `printf '%s' "$2"`, "printer", "--").Output()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out, []byte(prompt)) {
			t.Errorf("the prompt arrived as %q, want %q", out, prompt)
		}
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Error("the prompt file is still there")
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("text in a prompt RAN")
	}
	err := exec.Command("/bin/sh", "-c", claudeWrapper, "sh", filepath.Join(dir, "missing"), "/bin/true").Run()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 97 {
		t.Fatalf("a missing prompt file: %v; want exit 97", err)
	}
}

// TestPromptFilesAreWrittenUnderTheRuntimeDirectoryOnly: 0700 directory,
// 0600 file, never overwritten, never outside the prompt directory.
func TestPromptFilesAreWrittenUnderTheRuntimeDirectoryOnly(t *testing.T) {
	rt := t.TempDir()
	p := filepath.Join(promptDir(rt), "7")
	if err := writePromptFile(rt, p, "hello"); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("the prompt file is %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(promptDir(rt)); fi.Mode().Perm() != 0o700 {
		t.Errorf("the prompt directory is %v", fi.Mode().Perm())
	}
	if err := writePromptFile(rt, p, "again"); err == nil {
		t.Error("an existing prompt file was overwritten")
	}
	if err := writePromptFile(rt, filepath.Join(rt, "elsewhere"), "x"); err == nil {
		t.Error("a prompt file was written outside the prompt directory")
	}
}

// TestTheListingReadsAWindowsDispatch: the listing's dispatch field (digits,
// before the session name) becomes WindowInfo.Dispatch and the wire's
// Window.Dispatch; an empty field is 0.
func TestTheListingReadsAWindowsDispatch(t *testing.T) {
	out := "1\t@3\t1\t1\t%3\t300\t1\t1\tclaude\t/dev/pts/3\t\t\t417\tagents\n" +
		"1\t@4\t2\t0\t%4\t400\t1\t1\tzsh\t/dev/pts/4\t\t\t\twork\n"
	ws := parseWindows(out)
	if len(ws) != 2 || ws[0].Session != "agents" || ws[0].Dispatch != 417 || ws[1].Dispatch != 0 {
		t.Fatalf("parseWindows = %+v", ws)
	}
	wire := WireWindows(ws, nil, false, time.Now())
	if wire[0].Dispatch != 417 || wire[1].Dispatch != 0 {
		t.Fatalf("the wire's dispatch: %d, %d", wire[0].Dispatch, wire[1].Dispatch)
	}
}

// TestRepoURLsAndRatesParse: the remote URLs a clone may have, and the rate flag.
func TestRepoURLsAndRatesParse(t *testing.T) {
	for raw, want := range map[string]string{
		"git@github.com:Acme/Widgets.git": "acme/widgets", "https://github.com/acme/widgets": "acme/widgets",
		"ssh://git@github.com/acme/widgets.git": "acme/widgets", "https://gitlab.com/acme/widgets": "", "https://github.com/acme/../x": "",
		"/srv/git/widgets.git": "",
	} {
		if got := githubRepoOf(raw); got != want {
			t.Errorf("githubRepoOf(%q) = %q, want %q", raw, got, want)
		}
	}
	r, err := ParseAgentSpawnRate("")
	if err != nil || len(r) != 2 || r[0] != (spawnRate{3, 10 * time.Minute}) || r[1] != (spawnRate{10, 24 * time.Hour}) {
		t.Fatalf("the default rate: %v %v", r, err)
	}
	for _, bad := range []string{"3", "x/10m", "3/forever", "0/10m"} {
		if _, err := ParseAgentSpawnRate(bad); err == nil {
			t.Errorf("rate %q parsed", bad)
		}
	}
	if repos, err := ParseAgentSpawnRepos(""); err != nil || len(repos) != 0 {
		t.Errorf("the empty allow-list: %v %v", repos, err)
	}
	if _, err := ParseAgentSpawnRepos("acme/widgets,../x"); err == nil {
		t.Error("a path parsed as a repository")
	}
}
