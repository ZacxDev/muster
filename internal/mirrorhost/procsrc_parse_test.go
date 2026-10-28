package mirrorhost

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// The macOS process source's parsers, on Linux. The fixtures under
// testdata/darwin are SYNTHETIC: written in the documented output shape of
// macOS `ps -o …=` and `lsof -F`, and of the kern.procargs2 buffer, not
// captured from a Mac (none was available). A real Mac run is the PR's
// manual test script, and procsrc_darwin_test.go on a Mac.

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "darwin", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParsePSReadsEveryWellFormedRowAndSkipsTheRest(t *testing.T) {
	rows := parsePS(readFixture(t, "ps.txt"))
	byPID := map[int]psRow{}
	for _, r := range rows {
		byPID[r.pid] = r
	}
	if len(rows) != 9 {
		t.Fatalf("parsed %d rows, want the 9 well-formed ones: %+v", len(rows), rows)
	}
	for _, bad := range []int{710, 720} {
		if _, ok := byPID[bad]; ok {
			t.Errorf("pid %d (malformed row) was parsed: %+v", bad, byPID[bad])
		}
	}
	want := map[int]psRow{
		611: {pid: 611, ppid: 610, uid: 501, state: "S", start: "Wed Oct 28 15:40:02 2026", comm: "2.1.289"},
		// 🔴 A one-digit day is space-padded by ps, and Claude Code's own
		// procStart keeps that double space (it only trims the ends): the start
		// is the substring AS PRINTED, never the words re-joined.
		621: {pid: 621, ppid: 620, uid: 501, state: "S", start: "Thu Oct  8 07:05:30 2026", comm: "node"},
		640: {pid: 640, ppid: 630, uid: 501, state: "Z", start: "Thu Oct  8 07:06:01 2026", comm: "claude"},
		// The name is the rest of the line, spaces and all.
		700: {pid: 700, ppid: 1, uid: 0, state: "S", start: "Mon Oct 26 09:00:03 2026", comm: "Google Chrome He"},
	}
	for pid, w := range want {
		if got := byPID[pid]; got != w {
			t.Errorf("pid %d:\n got %+v\nwant %+v", pid, got, w)
		}
	}
}

func TestParseLsofCwdsTakesEachPidsAbsolutePath(t *testing.T) {
	got := parseLsofCwds(readFixture(t, "lsof-cwd.txt"))
	want := map[int]string{611: "/opt/dev/src/app", 621: "/opt/dev/src/my.repo"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseLsofCwds = %v, want %v (640's relative name and 650's missing one dropped)", got, want)
	}
}

// procargs2 builds a kern.procargs2 buffer: argc, the exec path, NUL padding
// to 8 bytes, argv, env, the empty string that ends env, then the kernel's own
// "apple" strings, which are not env.
func procargs2(exe string, argv, env []string) []byte {
	b := binary.LittleEndian.AppendUint32(nil, uint32(len(argv)))
	b = append(b, exe...)
	b = append(b, 0)
	for (len(b)-4)%8 != 0 { // to a word boundary counted from exec_path's start
		b = append(b, 0)
	}
	for _, s := range argv {
		b = append(b, s...)
		b = append(b, 0)
	}
	if env == nil { // no environment section: the buffer ends after argv
		return b
	}
	for _, s := range env {
		b = append(b, s...)
		b = append(b, 0)
	}
	b = append(b, 0)
	b = append(b, "executable_path="+exe...)
	b = append(b, 0, 'p', 't', 'r', '_', 'm', 'u', 'n', 'g', 'e', '=', 0)
	return b
}

func TestParseProcArgs2(t *testing.T) {
	const exe = "/opt/dev/.local/share/claude/versions/2.1.289"
	buf := procargs2(exe, []string{"claude", "--continue", ""},
		[]string{"HOME=/opt/dev", "CLAUDE_CONFIG_DIR=/opt/dev/.claude-work", "CLAUDE_CONFIG_DIR=/opt/dev/second"})
	pa, ok := parseProcArgs2(buf)
	if !ok {
		t.Fatal("a well-formed buffer did not parse")
	}
	if pa.exe != exe {
		t.Errorf("exe = %q, want %q", pa.exe, exe)
	}
	// An empty argument is an argument: argc says how many, not the NULs.
	if want := []string{"claude", "--continue", ""}; !reflect.DeepEqual(pa.argv, want) {
		t.Errorf("argv = %q, want %q", pa.argv, want)
	}
	if want := []string{"HOME=/opt/dev", "CLAUDE_CONFIG_DIR=/opt/dev/.claude-work", "CLAUDE_CONFIG_DIR=/opt/dev/second"}; !reflect.DeepEqual(pa.env, want) {
		t.Errorf("env = %q, want %q (the apple strings after the empty one are not env)", pa.env, want)
	}
	if v, ok := envLookup(pa.env, "CLAUDE_CONFIG_DIR"); !ok || v != "/opt/dev/.claude-work" {
		t.Errorf("envLookup = %q, %v; want the FIRST assignment, as getenv reads it", v, ok)
	}
	if _, ok := envLookup(pa.env, "CLAUDE_CONFIG"); ok {
		t.Error("envLookup matched a key that is only a prefix of one set")
	}

	// Truncated inside argv: the argv it held, no env.
	cut := buf[:strings.Index(string(buf), "--continue")+4]
	if p, ok := parseProcArgs2(cut); !ok || !reflect.DeepEqual(p.argv, []string{"claude"}) || p.env != nil {
		t.Errorf("truncated buffer = %+v, %v; want argv [claude], no env", p, ok)
	}
	// 🔴 AN EMPTY argv[0]: its terminator is a NUL right after the exec path's
	// padding. Skipping "every NUL" there (as `ps` does) eats it and shifts
	// every argument and the environment by one. Exec paths of both padding
	// shapes: "/bin/ab" + NUL is exactly one word (no padding), "/bin/x" + NUL
	// needs one byte of it.
	for _, exe := range []string{"/bin/ab", "/bin/x"} {
		p, ok := parseProcArgs2(procargs2(exe, []string{"", "60"}, []string{"A=1"}))
		if !ok || !reflect.DeepEqual(p.argv, []string{"", "60"}) || !reflect.DeepEqual(p.env, []string{"A=1"}) {
			t.Errorf("exec path %q, empty argv[0]: %+v, %v; want argv [\"\" 60], env [A=1]", exe, p, ok)
		}
	}

	// 🟡 NO ENVIRONMENT SECTION (XNU omits it for some processes): reported as
	// absent, and environOf turns that into errEnvAbsent — not an empty env,
	// which configDirOf would read as "sets no CLAUDE_CONFIG_DIR".
	p, ok := parseProcArgs2(procargs2(exe, []string{"claude"}, nil))
	if !ok || !p.envAbsent || p.env != nil || !reflect.DeepEqual(p.argv, []string{"claude"}) {
		t.Errorf("no env section: %+v, %v; want argv [claude], envAbsent", p, ok)
	}
	if env, err := environOf(p, ok); !errors.Is(err, errEnvAbsent) {
		t.Errorf("environOf(no env section) = %q, %v; want errEnvAbsent", env, err)
	}
	// An EMPTY environment is not an absent one: the terminator and the
	// kernel's apple strings are there.
	p, ok = parseProcArgs2(procargs2(exe, []string{"claude"}, []string{}))
	if env, err := environOf(p, ok); err != nil || len(env) != 0 {
		t.Errorf("environOf(empty env) = %q, %v; want empty, no error", env, err)
	}

	for name, b := range map[string][]byte{
		"empty":        nil,
		"argc only":    {2, 0, 0, 0},
		"no exec path": {1, 0, 0, 0, 0, 'x', 0},
		"negative":     append([]byte{0xff, 0xff, 0xff, 0xff}, "/bin/x\x00"...),
		"huge argc":    append([]byte{0, 0, 0, 0x10}, "/bin/x\x00"...),
	} {
		if _, ok := parseProcArgs2(b); ok {
			t.Errorf("%s: parsed, want refused", name)
		}
	}
}

// envLookup (test helper) is key's value in env (KEY=VALUE strings), the first assignment
// winning, as getenv reads it.
func envLookup(env []string, key string) (string, bool) {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v, true
		}
	}
	return "", false
}

// fakeProcs is a procSource over a table already built.
type fakeProcs struct {
	procs    map[int]procInfo
	children map[int][]int
	err      error
	cwd_     map[int]string
	env      map[int][]string
	envErr   map[int]error
}

func (f fakeProcs) table() (map[int]procInfo, map[int][]int, error) {
	return f.procs, f.children, f.err
}

func (f fakeProcs) cwd(pid int) (string, error) {
	if d, ok := f.cwd_[pid]; ok {
		return d, nil
	}
	return "", os.ErrNotExist
}

func (f fakeProcs) cwds(pids []int) map[int]string {
	out := map[int]string{}
	for _, p := range pids {
		if d, ok := f.cwd_[p]; ok {
			out[p] = d
		}
	}
	return out
}

func (f fakeProcs) environ(pid int) ([]string, error) {
	if err := f.envErr[pid]; err != nil {
		return nil, err
	}
	if env, ok := f.env[pid]; ok {
		return env, nil
	}
	return nil, os.ErrNotExist
}

// darwinFixture is the macOS source's view of testdata/darwin, built through
// the same parsers and psTable the darwin backend runs, with these
// kern.procargs2 buffers (pid -> buffer; a pid missing reads as refused).
func darwinFixture(t *testing.T, args map[int][]byte) fakeProcs {
	t.Helper()
	f := fakeProcs{env: map[int][]string{}, envErr: map[int]error{}}
	f.procs, f.children = psTable(parsePS(readFixture(t, "ps.txt")), func(r psRow) (procArgs, bool) {
		b, ok := args[r.pid]
		if !ok {
			return procArgs{}, false
		}
		pa, ok := parseProcArgs2(b)
		// What darwinProcs.environ answers for this pid, through the same call.
		if env, err := environOf(pa, ok); err != nil {
			f.envErr[r.pid] = err
		} else {
			f.env[r.pid] = env
		}
		return pa, ok
	})
	f.cwd_ = parseLsofCwds(readFixture(t, "lsof-cwd.txt"))
	return f
}

func writeTranscript(t *testing.T, cfg, cwd, id string) string {
	t.Helper()
	p := filepath.Join(cfg, "projects", ProjectSlug(cwd), id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"type":"user"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeSessionsFile(t *testing.T, cfg string, pid int, body string) {
	t.Helper()
	p := filepath.Join(cfg, "sessions", strconv.Itoa(pid)+".json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// THE SEAM: the Claude Code locator, handed a macOS-shaped table, finds Claude
// Code in each pane and its transcript — through every way the darwin source
// differs from /proc: the CLI found by the path it was exec'd with, its own
// CLAUDE_CONFIG_DIR read from procargs2, its cwd from lsof, and its start time
// as ps prints it, matched against the sessions file's procStart.
func TestTheLocatorFindsClaudeCodeThroughADarwinShapedSource(t *testing.T) {
	home := t.TempDir() // the service's own config dir: holds nothing
	work := t.TempDir() // the CLI's CLAUDE_CONFIG_DIR
	src := darwinFixture(t, map[int][]byte{
		610: procargs2("/bin/zsh", []string{"-zsh"}, []string{"HOME=/opt/dev"}),
		// Launched by the version binary's full path: neither its name (as
		// macOS reports it) nor argv[0] says "claude"; the exec path does.
		611: procargs2("/opt/dev/.local/share/claude/versions/2.1.289",
			[]string{"/opt/dev/.local/share/claude/versions/2.1.289"},
			[]string{"HOME=/opt/dev", "CLAUDE_CONFIG_DIR=" + work}),
		620: procargs2("/bin/zsh", []string{"zsh"}, nil),
		// npm's CLI: node running the package's entry script; no sessions file,
		// so its transcript is the newest in its cwd (from lsof).
		621: procargs2("/opt/homebrew/bin/node",
			[]string{"node", "/opt/homebrew/lib/node_modules/@anthropic-ai/claude-code/cli.js"},
			[]string{"HOME=/opt/dev"}),
	})
	const sid = "0d9a3c1e-0000-4000-8000-000000000611"
	want611 := writeTranscript(t, work, "/opt/dev/src/app", sid)
	writeSessionsFile(t, work, 611, `{"pid":611,"sessionId":"`+sid+`","cwd":"/opt/dev/src/app","status":"busy","statusUpdatedAt":1800000000000,"procStart":"Wed Oct 28 15:40:02 2026"}`)
	want621 := writeTranscript(t, home, "/opt/dev/src/my.repo", "0d9a3c1e-0000-4000-8000-000000000621")

	l := ClaudeLocator{Home: home, src: src}
	got := l.LocatePanes([]pane{{Session: "a", ID: "%1", PID: 610}, {Session: "b", ID: "%2", PID: 620}})
	if c := got["%1"]; c.PID != 611 || c.Transcript != want611 || c.Gen != sid || c.Source != SourceSessions || c.ConfigDir != work || !c.Busy {
		t.Errorf("pane %%1 = %+v\nwant pid 611, transcript %s from the CLI's own config dir, busy", c, want611)
	}
	if c := got["%2"]; c.PID != 621 || c.Transcript != want621 || c.Source != SourceGuessed {
		t.Errorf("pane %%2 = %+v\nwant pid 621, the guessed %s", c, want621)
	}

	// A process whose argv the kernel refused (no procargs2 for 640) keeps
	// the name ps gave it, and is still found by that name.
	if p := src.procs[640]; !p.argvUnread || p.comm != "claude" {
		t.Errorf("pid 640 = %+v, want argvUnread with its ps name kept", p)
	}
	if got := findClaude(630, src.procs, src.children); got != 640 {
		t.Errorf("findClaude(630) = %d, want 640 by its name alone", got)
	}

	// The start-time token: a live sessions file whose procStart is ps's lstart
	// AS PRINTED is alive; the same time with the day's padding collapsed is
	// another string, so it is not.
	writeSessionsFile(t, home, 621, `{"pid":621,"sessionId":"s621","cwd":"/opt/dev/src/my.repo","procStart":"Thu Oct  8 07:05:30 2026"}`)
	writeSessionsFile(t, home, 620, `{"pid":620,"sessionId":"s620","cwd":"/opt/dev","procStart":"Thu Oct 8 07:05:09 2026"}`)
	live := l.liveSessions(home, src.procs)
	if _, ok := live.byID["s621"]; !ok {
		t.Error("a sessions file whose procStart is the lstart as printed was not taken as live")
	}
	if _, ok := live.byID["s620"]; ok {
		t.Error("a sessions file whose procStart differs from the lstart (padding collapsed) was taken as live")
	}
}

// A darwin source whose table read fails gives a Claude-looking window the
// one reason left for a host that cannot read processes.
func TestADarwinSourceThatCannotReadItsTableSaysSo(t *testing.T) {
	l := ClaudeLocator{Home: t.TempDir(), src: fakeProcs{err: errors.New("ps: exit 1")}}
	procs, children, err := l.procs().table()
	if got := l.whyNoView(pane{ID: "%1", PID: 1}, "claude", procs, children, err); got != NoViewNoProcs {
		t.Fatalf("whyNoView = %q, want NoViewNoProcs", got)
	}
	if strings.Contains(NoViewNoProcs, "macOS") || strings.Contains(NoViewNoProcs, "/proc") {
		t.Errorf("NoViewNoProcs still names macOS or /proc, which now has a process source: %q", NoViewNoProcs)
	}
}

// The native installer's binary is recognised by its exec path alone — the
// whole shape claude/versions/<x.y.z> — and nothing shorter of it.
func TestIsClaudeByTheInstallersExecPath(t *testing.T) {
	for exe, want := range map[string]bool{
		"/opt/dev/.local/share/claude/versions/2.1.289": true,
		"/opt/dev/.local/bin/claude":                    true,
		"/opt/dev/.local/share/claude/versions/2.1":     false, // not x.y.z
		"/opt/dev/.local/share/claude/versions/latest":  false,
		"/opt/dev/.local/share/other/versions/2.1.289":  false, // not claude's
		"/opt/dev/.local/share/claude/builds/2.1.289":   false, // not versions/
		"/opt/dev/node/versions/2.1.289":                false,
		"/usr/local/bin/2.1.289":                        false,
	} {
		// Name and argv say nothing: only the exec path can decide.
		if got := isClaude(procInfo{comm: "2.1.289", argv: []string{"x"}, exe: exe}); got != want {
			t.Errorf("isClaude(exe %q) = %v, want %v", exe, got, want)
		}
	}
	if isClaude(procInfo{comm: "2.1.289"}) {
		t.Error("a version-numbered name with no exec path was taken as Claude Code")
	}
}

// 🟡 A Claude Code process whose environment the kernel withheld, with nothing
// else resolving it: the line names THAT step, not a generic "no transcript".
func TestADarwinCLIWithNoEnvironmentSaysSo(t *testing.T) {
	src := darwinFixture(t, map[int][]byte{
		620: procargs2("/bin/zsh", []string{"zsh"}, []string{"HOME=/opt/dev"}),
		621: procargs2("/opt/homebrew/bin/node",
			[]string{"node", "/opt/homebrew/lib/node_modules/@anthropic-ai/claude-code/cli.js"}, nil),
	})
	if _, err := src.environ(621); !errors.Is(err, errEnvAbsent) {
		t.Fatalf("control: environ(621) = %v, want errEnvAbsent", err)
	}
	l := ClaudeLocator{Home: t.TempDir(), src: src}
	if _, err := l.configDirOfErr(621); !errors.Is(err, errEnvAbsent) {
		t.Errorf("configDirOfErr(621) error = %v, want errEnvAbsent", err)
	}
	if got := l.whyNoView(pane{ID: "%2", PID: 620}, "node", src.procs, src.children, nil); got != NoViewNoEnv {
		t.Errorf("whyNoView = %q, want NoViewNoEnv", got)
	}
}

// countingCwds counts the reads of the cwds it wraps.
type countingCwds struct {
	fakeProcs
	calls *int
	asked *[][]int
}

func (c countingCwds) cwds(pids []int) map[int]string {
	*c.calls++
	*c.asked = append(*c.asked, append([]int(nil), pids...))
	return c.fakeProcs.cwds(pids)
}

// 🟢 ONE cwd read per listing: the locator's cwd guesses for every pane and
// then every window's own cwd come from one call to the source (one lsof on
// macOS), for the pids cwdCandidates names; a pid outside them is one more.
func TestAListingReadsCwdsOnce(t *testing.T) {
	home := t.TempDir()
	base := darwinFixture(t, map[int][]byte{
		610: procargs2("/bin/zsh", []string{"-zsh"}, []string{"HOME=/opt/dev"}),
		611: procargs2("/opt/dev/.local/share/claude/versions/2.1.289", []string{"claude"}, []string{"HOME=/opt/dev"}),
		620: procargs2("/bin/zsh", []string{"zsh"}, []string{"HOME=/opt/dev"}),
		621: procargs2("/opt/homebrew/bin/node",
			[]string{"node", "/opt/homebrew/lib/node_modules/@anthropic-ai/claude-code/cli.js"}, []string{"HOME=/opt/dev"}),
	})
	// Neither CLI has a sessions file: both resolve by their cwd (a guess).
	writeTranscript(t, home, "/opt/dev/src/app", "0d9a3c1e-0000-4000-8000-000000000611")
	writeTranscript(t, home, "/opt/dev/src/my.repo", "0d9a3c1e-0000-4000-8000-000000000621")
	var calls int
	var asked [][]int
	windows := []WindowInfo{{Session: "a", Pane: "%1", PanePID: 610}, {Session: "b", Pane: "%2", PanePID: 620}}
	cands := cwdCandidates(windows, base.procs, base.children)
	if want := []int{610, 611, 620, 621}; !reflect.DeepEqual(cands, want) {
		t.Fatalf("cwdCandidates = %v, want %v", cands, want)
	}
	memo := newCwdMemo(countingCwds{base, &calls, &asked}, cands)
	l := ClaudeLocator{Home: home, src: memo}
	got := l.LocatePanes([]pane{{ID: "%1", PID: 610}, {ID: "%2", PID: 620}})
	if len(got) != 2 {
		t.Fatalf("control: located %+v, want both panes by their cwd", got)
	}
	_ = memo.cwds([]int{611, 621, 610})
	if calls != 1 || !reflect.DeepEqual(asked[0], []int{610, 611, 620, 621}) {
		t.Errorf("source cwds called %d times with %v, want once with every candidate", calls, asked)
	}
	_ = memo.cwds([]int{700})
	if calls != 2 || !reflect.DeepEqual(asked[1], []int{700}) {
		t.Errorf("a pid outside the candidates: calls %d, asked %v; want one more read of just it", calls, asked)
	}
}

// Which source a locator reads: its test source, else the proc root it names,
// else this host's — on Linux exactly the /proc it always read.
func TestTheLocatorsProcessSource(t *testing.T) {
	f := fakeProcs{}
	if got := (ClaudeLocator{Proc: "/x", src: f}).procs(); !reflect.DeepEqual(got, f) {
		t.Errorf("with src set: %#v, want the src", got)
	}
	if got := (ClaudeLocator{Proc: "/fixture"}).procs(); got != (procFS{root: "/fixture"}) {
		t.Errorf("with Proc set: %#v, want procFS{/fixture}", got)
	}
	if runtime.GOOS == "linux" {
		if got := (ClaudeLocator{}).procs(); got != (procFS{root: "/proc"}) {
			t.Errorf("on Linux, by default: %#v, want procFS{/proc} (the Linux path unchanged)", got)
		}
	}
}
