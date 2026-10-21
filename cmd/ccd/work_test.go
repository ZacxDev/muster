package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/cctasks"
	"github.com/ZacxDev/muster/internal/mirror"
)

// The hook fixtures *.shape.json are NOT recorded payloads: they are built
// field for field from the payload builders in the 2.1.296 bundle the image
// pins (SubagentStart: agent_id, agent_type; SubagentStop and Stop:
// background_tasks of {id, type, status, description, agent_type|command},
// type "subagent" for a local agent task, whose id is its agent id), around the
// common fields of the recorded hook_SessionStart_2.1.296.json. A recorded
// capture (CCD_HOOK_LOG) should replace them.

const fixtureSession = "11111111-2222-4333-8444-555555555555"

// hookFixture is a fixture payload with a field replaced: name -> raw JSON value.
func hookFixture(t *testing.T, name string, set map[string]string) []byte {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(fixture(t, name), &m); err != nil {
		t.Fatal(err)
	}
	for k, v := range set {
		m[k] = json.RawMessage(v)
	}
	b, _ := json.Marshal(m)
	return b
}

// deliver sends one payload through ccd's own hook handler (its decoding
// included), as `ccd hook <Event>` does.
func deliver(t *testing.T, s *server, body []byte) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/hook/x", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	s.handleHook(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("hook: %d %s", w.Code, w.Body)
	}
}

func q(s string) string { return strconv.Quote(s) }

// TestTheSubagentSetFollowsTheCLIsHooks: the subagents ccd counts for a CLI,
// from its real-shaped hooks — with counts that differ at every step, so a
// step that did nothing cannot pass.
//
//   - SubagentStart adds, SubagentStop removes — including an agent its own
//     SubagentStop snapshot still lists as running;
//   - a Stop's background_tasks REPLACES the set (a missed stop and two missed
//     starts are corrected), counting running AND pending agent tasks, never a
//     shell, a monitor or a backgrounded main session;
//   - a Stop with no background_tasks (an older CLI) changes nothing;
//   - a hook from ANOTHER session (a `claude` run inside this one) is not this
//     CLI's; SessionEnd empties the set.
func TestTheSubagentSetFollowsTheCLIsHooks(t *testing.T) {
	s := newServer(serverConfig{}, nil, newAuthTracker())
	pokes := 0
	s.workPoke = func() { pokes++ }
	deliver(t, s, fixture(t, "hook_SessionStart_2.1.296.json"))
	start := func(id string) {
		deliver(t, s, hookFixture(t, "hook_SubagentStart_2.1.296.shape.json", map[string]string{"agent_id": q(id)}))
	}
	want := func(step string, n int) {
		t.Helper()
		if got := s.runningSubagents(); got != n {
			t.Fatalf("%s: %d subagents running, want %d (%v)", step, got, n, s.subagents)
		}
	}
	start("a0123456789abcdef")
	start("a1111111111111111")
	start("a9999999999999999")
	want("three SubagentStarts", 3)
	if pokes != 3 {
		t.Errorf("three changes poked the reporter %d times", pokes)
	}
	deliver(t, s, hookFixture(t, "hook_SubagentStart_2.1.296.shape.json", map[string]string{
		"agent_id": q("a7777777777777777"), "session_id": q("99999999-2222-4333-8444-555555555555")}))
	want("a SubagentStart from another session", 3)
	// a0123… stops; its snapshot lists itself and a1111… running — so a9999…,
	// whose stop ccd never heard, is dropped too.
	deliver(t, s, fixture(t, "hook_SubagentStop_2.1.296.shape.json"))
	want("SubagentStop with its snapshot", 1)
	if _, ok := s.subagents["a1111111111111111"]; !ok {
		t.Fatalf("the agent still running is not the one counted: %v", s.subagents)
	}
	// The turn ends with three agent tasks running or pending; a1111… is not
	// among them (a stop ccd never heard), and three it never heard start are.
	deliver(t, s, fixture(t, "hook_Stop_bgtasks_2.1.296.shape.json"))
	want("Stop with background_tasks", 3)
	for _, id := range []string{"a2222222222222222", "a3333333333333333", "a4444444444444444"} {
		if _, ok := s.subagents[id]; !ok {
			t.Errorf("%s, running per the Stop, is not counted: %v", id, s.subagents)
		}
	}
	var stop map[string]json.RawMessage
	_ = json.Unmarshal(fixture(t, "hook_Stop_bgtasks_2.1.296.shape.json"), &stop)
	delete(stop, "background_tasks")
	older, _ := json.Marshal(stop)
	deliver(t, s, older)
	want("a Stop with no background_tasks", 3)
	deliver(t, s, hookFixture(t, "hook_SessionStart_2.1.296.json", map[string]string{"hook_event_name": q("SessionEnd")}))
	want("SessionEnd", 0)
}

// fakeProcFS is a /proc with the processes a test names: pid -> ppid, start
// time, argv and fd links.
type fakeProcess struct {
	ppid  int
	start string
	argv  []string
	fds   map[int]string
}

func fakeProcFS(t *testing.T, procs map[int]fakeProcess) string {
	t.Helper()
	root := t.TempDir()
	for pid, p := range procs {
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(filepath.Join(dir, "fd"), 0o755); err != nil {
			t.Fatal(err)
		}
		// Fields 3.. of a stat line; field 22 is the start time.
		fields := []string{"S", strconv.Itoa(p.ppid)}
		for len(fields) < 19 {
			fields = append(fields, "0")
		}
		fields = append(fields, p.start, "0")
		stat := strconv.Itoa(pid) + " (" + filepath.Base(p.argv[0]) + ") " + strings.Join(fields, " ") + "\n"
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(p.argv, "\x00")+"\x00"), 0o644); err != nil {
			t.Fatal(err)
		}
		for fd, target := range p.fds {
			if err := os.Symlink(target, filepath.Join(dir, "fd", strconv.Itoa(fd))); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

// sessionFile writes <config>/sessions/<pid>.json.
func sessionFile(t *testing.T, config string, pid int, session, status, procStart string, updated time.Time) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"pid": pid, "sessionId": session, "cwd": "/data/workspace", "startedAt": 1,
		"procStart": procStart, "kind": "interactive", "status": status, "statusUpdatedAt": updated.UnixMilli()})
	dir := filepath.Join(config, "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(pid)+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

const shellTasks = "/tmp/claude-1000/-data-workspace/" + fixtureSession + "/tasks/"

// TestPodWorkIsTheMirrorsRuleOverEveryCLI: what ccd reports is cctasks.Decide —
// the mirror's rule — over each CLI's RAW status from its own sessions file,
// its process-tree shells and, while busy, its hook-derived subagents; summed
// over the primary and its lanes. Counts are pairwise distinct.
func TestPodWorkIsTheMirrorsRuleOverEveryCLI(t *testing.T) {
	const laneSession = "22222222-2222-4333-8444-555555555555"
	cli := func(ppid int) fakeProcess { return fakeProcess{ppid: ppid, start: "777", argv: []string{"claude"}} }
	task := func(ppid int, id string) fakeProcess {
		return fakeProcess{ppid: ppid, start: "800", argv: []string{"/bin/bash", "-c", "sleep 600"},
			fds: map[int]string{1: shellTasks + id + ".output", 2: shellTasks + id + ".output"}}
	}
	root := fakeProcFS(t, map[int]fakeProcess{
		100: cli(1), 101: task(100, "b0000000a"), 102: task(100, "b0000000b"), 103: task(101, "b0000000c"),
		104: {ppid: 100, start: "801", argv: []string{"node", "mcp-server.js"}, fds: map[int]string{1: "socket:[1]", 2: "socket:[2]"}},
		200: {ppid: 1, start: "778", argv: []string{"claude"}}, 201: task(200, "b0000000d"),
	})
	config := t.TempDir()
	clock := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	primary := newServer(serverConfig{}, nil, newAuthTracker())
	primary.clock = func() time.Time { return clock }
	deliver(t, primary, fixture(t, "hook_SessionStart_2.1.296.json"))
	lane := newServer(serverConfig{}, nil, newAuthTracker())
	lane.clock = func() time.Time { return clock }
	deliver(t, lane, hookFixture(t, "hook_SessionStart_2.1.296.json", map[string]string{"session_id": q(laneSession)}))
	for _, id := range []string{"a0000000000000001", "a0000000000000002"} {
		deliver(t, lane, hookFixture(t, "hook_SubagentStart_2.1.296.shape.json", map[string]string{"agent_id": q(id), "session_id": q(laneSession)}))
	}

	work := func(servers ...*server) (cctasks.Work, bool) { return podWork(config, root, servers) }

	// No sessions file at all: nothing may be claimed.
	if w, ok := work(primary, lane); ok || w != (cctasks.Work{}) {
		t.Fatalf("with no sessions file: %+v ok=%v, want nothing", w, ok)
	}
	// "shell": no subagent, and the three task ids — a grandchild's included,
	// the MCP server's socket child not — exactly.
	sessionFile(t, config, 100, fixtureSession, cctasks.StatusShell, "777", clock.Add(-time.Minute))
	if w, ok := work(primary); !ok || w != (cctasks.Work{Shells: 3}) {
		t.Fatalf("primary at shell: %+v ok=%v, want exactly 3 shells", w, ok)
	}
	// The lane, busy with its 2 subagents and a shell that may be a foreground
	// call: "2 subagents · 1+ shell"; the pod's sum is "2 subagents · 4+ shells".
	sessionFile(t, config, 200, laneSession, cctasks.StatusBusy, "778", clock.Add(-time.Minute))
	if w, _ := work(lane); w != (cctasks.Work{Subagents: 2, Shells: 1, ShellsAtLeast: true}) {
		t.Fatalf("busy lane: %+v, want 2 subagents and 1+ shell", w)
	}
	if w, _ := work(primary, lane); w != (cctasks.Work{Subagents: 2, Shells: 4, ShellsAtLeast: true}) {
		t.Fatalf("the pod: %+v, want 2 subagents and 4+ shells", w)
	}
	// A sessions file whose process is not the one it names (a reused pid after
	// a restart) is not live: the lane says nothing.
	sessionFile(t, config, 200, laneSession, cctasks.StatusBusy, "999", clock.Add(-time.Minute))
	if w, ok := work(lane); ok || w != (cctasks.Work{}) {
		t.Fatalf("a stale sessions file counted: %+v ok=%v", w, ok)
	}
	// The lane goes idle: the CLI says no agent runs, so the two ccd learned
	// before that status are forgotten; one learned after it is kept (for the
	// next busy), and the card shows nothing now.
	sessionFile(t, config, 200, laneSession, cctasks.StatusIdle, "778", clock.Add(time.Second))
	clock = clock.Add(2 * time.Second)
	deliver(t, lane, hookFixture(t, "hook_SubagentStart_2.1.296.shape.json", map[string]string{"agent_id": q("a0000000000000003"), "session_id": q(laneSession)}))
	if w, ok := work(lane); !ok || w != (cctasks.Work{}) {
		t.Fatalf("idle lane: %+v ok=%v, want nothing running", w, ok)
	}
	if got := lane.runningSubagents(); got != 1 {
		t.Fatalf("after idle, %d subagents kept, want only the one learned after the status (%v)", got, lane.subagents)
	}
	// A server whose session has no live file (a nested CLI's file is not
	// any server's session) contributes nothing.
	sessionFile(t, config, 200, "33333333-2222-4333-8444-555555555555", cctasks.StatusBusy, "778", clock)
	if w, ok := work(lane); ok {
		t.Fatalf("another session's file was read as the lane's: %+v", w)
	}
}

// TestTheReporterPostsWorkOnChangeAndHeartbeatsWhileRunning: the body is
// muster's own mirror.WindowWork (decoded with muster's type), sent with the
// agent's own token on a change, re-sent every heartbeat only while something
// runs, and a zero is sent once — the clear.
func TestTheReporterPostsWorkOnChangeAndHeartbeatsWhileRunning(t *testing.T) {
	var mu sync.Mutex
	var got []mirror.WindowWork
	var tokens []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent/work" || r.Method != http.MethodPost {
			t.Errorf("reported to %s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		var ww mirror.WindowWork
		if err := json.Unmarshal(b, &ww); err != nil {
			t.Errorf("body %s: %v", b, err)
		}
		mu.Lock()
		got, tokens = append(got, ww), append(tokens, r.Header.Get("X-Muster-Token"))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()
	var cur cctasks.Work
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	var smu sync.Mutex
	r := newWorkReporter(ts.URL, "agent-token", func() cctasks.Work { smu.Lock(); defer smu.Unlock(); return cur }, t.Logf)
	r.interval = time.Hour // samples only when poked
	r.now = func() time.Time { smu.Lock(); defer smu.Unlock(); return now }
	set := func(w cctasks.Work, adv time.Duration) {
		smu.Lock()
		cur, now = w, now.Add(adv)
		smu.Unlock()
	}
	posts := func() int { mu.Lock(); defer mu.Unlock(); return len(got) }
	waitPosts := func(n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for posts() < n && time.Now().Before(deadline) {
			r.pokeNow()
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond)
		if p := posts(); p != n {
			t.Fatalf("%d posts, want %d: %+v", p, n, got)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.run(ctx)
	// Nothing running from the start: nothing to clear, no post.
	time.Sleep(20 * time.Millisecond)
	if posts() != 0 {
		t.Fatalf("a reporter with nothing running posted %+v", got)
	}
	set(cctasks.Work{Subagents: 3, Shells: 2}, 0)
	waitPosts(1)
	r.pokeNow() // unchanged, inside the heartbeat: no post
	waitPosts(1)
	set(cctasks.Work{Subagents: 3, Shells: 2}, workHeartbeat)
	waitPosts(2) // the heartbeat
	set(cctasks.Work{Shells: 1, ShellsAtLeast: true}, time.Second)
	waitPosts(3)
	set(cctasks.Work{}, time.Second)
	waitPosts(4) // the clear
	set(cctasks.Work{}, 3*workHeartbeat)
	waitPosts(4) // nothing running is not re-sent
	want := []mirror.WindowWork{{Subagents: 3, Shells: 2}, {Subagents: 3, Shells: 2}, {Shells: 1, ShellsAtLeast: true}, {}}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("post %d: %+v, want %+v", i, got[i], want[i])
		}
		if tokens[i] != "agent-token" {
			t.Errorf("post %d sent token %q", i, tokens[i])
		}
	}
}
