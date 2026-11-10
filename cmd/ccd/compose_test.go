package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// fakeComposeChild stands in for the entrypoint `ccd compose` runs: it serves
// CCD_LISTEN and writes its environment to $CCD_TEST_ENV_DUMP.
//
//	GET /healthz   200, unless the file $CCD_TEST_HEALTH_FILE holds "fail" (503)
//	ANY /echo      a JSON echo of the method, path, Authorization and body
//	GET /stream    an SSE event, then a second one once $CCD_TEST_RELEASE exists
//
// FAKE_EXIT (from the spec's env) makes it exit at once with that status.
// FAKE_TERM_DELAY_MS makes it take that long to exit after SIGTERM.
func fakeComposeChild() int {
	if v := os.Getenv("FAKE_EXIT"); v != "" {
		n, _ := strconv.Atoi(v)
		return n
	}
	if p := os.Getenv("CCD_TEST_ENV_DUMP"); p != "" {
		_ = os.WriteFile(p+".tmp", []byte(strings.Join(os.Environ(), "\n")), 0o600)
		_ = os.Rename(p+".tmp", p)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if b, _ := os.ReadFile(os.Getenv("CCD_TEST_HEALTH_FILE")); strings.TrimSpace(string(b)) == "fail" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"ok":true,"pid":`+strconv.Itoa(os.Getpid())+`}`)
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(map[string]string{"method": r.Method, "path": r.URL.Path,
			"auth": r.Header.Get("Authorization"), "body": string(b)})
	})
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: one\ndata: first\n\n")
		w.(http.Flusher).Flush()
		for {
			if _, err := os.Stat(os.Getenv("CCD_TEST_RELEASE")); err == nil {
				break
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
		fmt.Fprint(w, "event: two\ndata: second\n\n")
	})
	// SO_REUSEPORT: the harness holds this port bound (reservedAddr), so the
	// child must be allowed to share it. Sharing would also let two children
	// listen at once, which an exclusive bind used to refuse; the lock keeps
	// that refusal: a second child while the first lives exits 1.
	if p := os.Getenv("CCD_TEST_CHILD_LOCK"); p != "" {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake child:", err)
			return 1
		}
		if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			fmt.Fprintln(os.Stderr, "fake child: another child is still running:", err)
			return 1
		}
	}
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
		}); err != nil {
			return err
		}
		return serr
	}}
	ln, err := lc.Listen(context.Background(), "tcp", os.Getenv("CCD_LISTEN"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake child:", err)
		return 1
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	go func() {
		<-sig
		// FAKE_TERM_DELAY_MS (spec env): a child slow to stop.
		ms, _ := strconv.Atoi(os.Getenv("FAKE_TERM_DELAY_MS"))
		time.Sleep(time.Duration(ms) * time.Millisecond)
		os.Exit(0)
	}()
	_ = http.Serve(ln, mux)
	return 1
}

// composeHarness is a composeSup with a fake child, fast clocks and a captured log.
type composeHarness struct {
	t       *testing.T
	sup     *composeSup
	srv     *httptest.Server
	dir     string // CCD_SPEC_DIR
	envDump string
	health  string
	release string

	mu       sync.Mutex
	logs     strings.Builder
	cleanups [][]string
	// cleanupErr, when set, is what the stubbed cleanup returns (a stop that
	// left something alive); it is called with the call's 1-based number.
	cleanupErr func(n int) error
}

// reservedAddr is a loopback port held for the whole test: a socket bound to
// it with SO_REUSEPORT and never listening. 🔴 Not a port found free and
// released: between that release and the child's bind (and again across every
// restart, while no child holds it) any other process — CI runs the packages
// at once — could take it, and every start then dies "address already in use"
// until the wait gives up. A bound socket keeps the port out of everyone
// else's bind(0) and connect(), while the fake child, binding it with
// SO_REUSEPORT as the same user, may still listen there. Nothing connects to
// the reservation: only a listening socket accepts.
func reservedAddr(t *testing.T) string {
	t.Helper()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(fd) })
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); err != nil {
		t.Fatal(err)
	}
	if err := unix.Bind(fd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	sa, err := unix.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(sa.(*unix.SockaddrInet4).Port))
}

func newComposeHarness(t *testing.T) *composeHarness {
	t.Helper()
	tmp := t.TempDir()
	h := &composeHarness{t: t, dir: filepath.Join(tmp, "spec"), envDump: filepath.Join(tmp, "env"),
		health: filepath.Join(tmp, "health"), release: filepath.Join(tmp, "release")}
	if err := os.MkdirAll(filepath.Join(h.dir, "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(tmp, "data")
	if err := os.Mkdir(data, 0o700); err != nil {
		t.Fatal(err)
	}
	s := newComposeSup(h.dir, nil)
	s.dataDir = data
	// The epoch through the real `ccd compose-epoch` helper (this binary as
	// ccd), as the supervisor runs it — as this test's own user here.
	s.epoch = func(d, e string) error {
		return runAsCLI(context.Background(), nil, []string{"CCD_TEST_AS=ccd"}, "", os.Args[0], "compose-epoch", d, e)
	}
	s.argv = []string{os.Args[0]}
	s.childAddr = reservedAddr(t)
	s.baseEnv = append(os.Environ(), "CCD_TEST_AS=composechild", "CCD_TEST_ENV_DUMP="+h.envDump,
		"CCD_TEST_HEALTH_FILE="+h.health, "CCD_TEST_RELEASE="+h.release, envSpecDir+"="+h.dir,
		"CCD_TEST_CHILD_LOCK="+filepath.Join(tmp, "child.lock"))
	s.poll, s.stopGrace, s.healthTimeout = 30*time.Millisecond, 3*time.Second, time.Second
	s.backoffMin, s.backoffMax = 50*time.Millisecond, 100*time.Millisecond
	s.logf = func(f string, a ...any) {
		h.mu.Lock()
		defer h.mu.Unlock()
		fmt.Fprintf(&h.logs, f+"\n", a...)
	}
	s.cleanup = func(_ context.Context, env []string) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.cleanups = append(h.cleanups, env)
		if h.cleanupErr != nil {
			return h.cleanupErr(len(h.cleanups))
		}
		return nil
	}
	// 🔴 Never reap in the test process: a zombie child of ANOTHER test's
	// exec.Cmd, collected here, would turn that test's Wait into an error.
	// reapOrphans and killDescendants are tested in a process of their own.
	s.reap = nil
	h.sup = s
	h.srv = httptest.NewServer(s.handler())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
		h.srv.Close()
	})
	return h
}

func (h *composeHarness) log() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.logs.String()
}

func (h *composeHarness) cleanupCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.cleanups)
}

// writeSpec writes spec.json as the driver does: a temporary file, renamed.
func (h *composeHarness) writeSpec(v any) {
	h.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		h.t.Fatal(err)
	}
	h.writeRaw(string(b))
}

func (h *composeHarness) writeRaw(s string) {
	h.t.Helper()
	p := filepath.Join(h.dir, composeSpecFile)
	if err := os.WriteFile(p+".tmp", []byte(s), 0o640); err != nil {
		h.t.Fatal(err)
	}
	if err := os.Rename(p+".tmp", p); err != nil {
		h.t.Fatal(err)
	}
}

func spec(gen int64, replicas int, epoch string) map[string]any {
	return map[string]any{"version": 1, "name": "chief", "generation": gen, "replicas": replicas,
		"epoch": epoch, "env": []any{}, "secrets": []any{}}
}

func (h *composeHarness) get(path string) (int, string) {
	h.t.Helper()
	resp, err := http.Get(h.srv.URL + path)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (h *composeHarness) status() composeStatus {
	h.t.Helper()
	code, body := h.get("/compose/status")
	if code != http.StatusOK {
		h.t.Fatalf("/compose/status answered %d, want always 200: %s", code, body)
	}
	var st composeStatus
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		h.t.Fatalf("/compose/status is not a status: %v: %s", err, body)
	}
	return st
}

// waitFor polls /compose/status until ok, or fails naming what it waited for.
func (h *composeHarness) waitFor(what string, ok func(composeStatus) bool) composeStatus {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var st composeStatus
	for time.Now().Before(deadline) {
		if st = h.status(); ok(st) {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("waited 10s for %s; last status %+v; log:\n%s", what, st, h.log())
	return st
}

func running(st composeStatus) bool { return st.State == composeRunning && st.Ready }

// childPIDFromHealthz is the pid the fake child reports, through the proxy.
func (h *composeHarness) childPIDFromHealthz() int {
	h.t.Helper()
	code, body := h.get("/healthz")
	if code != http.StatusOK {
		h.t.Fatalf("/healthz through the proxy answered %d: %s", code, body)
	}
	var v struct{ PID int }
	if err := json.Unmarshal([]byte(body), &v); err != nil || v.PID == 0 {
		h.t.Fatalf("/healthz through the proxy is not the fake child's: %s", body)
	}
	return v.PID
}

func processGone(pid int) bool {
	return syscall.Kill(pid, 0) == syscall.ESRCH
}

func TestComposeAwaitsItsSpecAndAnswersNotReady(t *testing.T) {
	h := newComposeHarness(t)
	st := h.status()
	if st.State != composeAwaitingSpec || st.Reason != reasonAwaiting || st.Boot != "" || st.Ready {
		t.Fatalf("before any spec: %+v, want awaiting_spec with reason %q", st, reasonAwaiting)
	}
	code, body := h.get("/healthz")
	var hz struct{ Compose composeStatus }
	if code != http.StatusServiceUnavailable || json.Unmarshal([]byte(body), &hz) != nil || hz.Compose.State != composeAwaitingSpec {
		t.Fatalf("/healthz with no child: %d %s, want 503 carrying the compose status", code, body)
	}
	resp, err := http.Post(h.srv.URL+"/v1/responses", "application/json", strings.NewReader(`{"input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var f struct {
		Error struct{ Type, Code, Message string }
	}
	if resp.StatusCode != http.StatusServiceUnavailable || json.Unmarshal(b, &f) != nil ||
		f.Error.Type != failNotReady || f.Error.Code != failNotReady || f.Error.Message != reasonAwaiting {
		t.Fatalf("a turn with no child: %d %s, want 503 {error:{type:not_ready,code:not_ready,message:%q}}",
			resp.StatusCode, b, reasonAwaiting)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(h.envDump); err == nil {
		t.Fatal("a child was started with no spec")
	}
}

func TestComposeStartsStopsAndRestartsTheChildPerSpec(t *testing.T) {
	h := newComposeHarness(t)
	h.writeSpec(spec(1, 1, "aa"))
	st1 := h.waitFor("the first child running", running)
	if st1.Boot == "" || st1.Generation != 1 || st1.Replicas != 1 || st1.Epoch != "aa" {
		t.Fatalf("running: %+v", st1)
	}
	pid1 := h.childPIDFromHealthz()

	// Proxied: method, path, header and body reach the child.
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/echo", strings.NewReader("the body"))
	req.Header.Set("Authorization", "Bearer x")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var echo map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&echo)
	resp.Body.Close()
	if echo["method"] != "POST" || echo["path"] != "/echo" || echo["auth"] != "Bearer x" || echo["body"] != "the body" {
		t.Fatalf("the proxied request arrived as %v", echo)
	}

	// replicas 0: the child is gone, the supervisor is not.
	h.writeSpec(spec(1, 0, "aa"))
	off := h.waitFor("off", func(st composeStatus) bool { return st.State == composeOff })
	if off.Reason != reasonOff || off.Boot != "" || off.Ready {
		t.Fatalf("off: %+v", off)
	}
	if !processGone(pid1) {
		t.Fatalf("replicas 0: the child (pid %d) is still running", pid1)
	}
	if n := h.cleanupCount(); n != 1 {
		t.Fatalf("replicas 0: the stop ran cleanup %d times, want 1 (the tmux server and what it left)", n)
	}
	if code, body := h.get("/healthz"); code != http.StatusServiceUnavailable || !strings.Contains(body, `"state":"off"`) {
		t.Fatalf("/healthz while off: %d %s", code, body)
	}

	// Back on, a new generation: a new child and a new boot id.
	h.writeSpec(spec(2, 1, "aa"))
	st2 := h.waitFor("the second child running", func(st composeStatus) bool { return running(st) && st.Generation == 2 })
	pid2 := h.childPIDFromHealthz()
	if st2.Boot == st1.Boot || pid2 == pid1 {
		t.Fatalf("generation 2 runs boot %s pid %d; generation 1 was boot %s pid %d", st2.Boot, pid2, st1.Boot, pid1)
	}

	// A generation bump while running: stop, clean up, start again.
	h.writeSpec(spec(3, 1, "aa"))
	st3 := h.waitFor("generation 3 running", func(st composeStatus) bool { return running(st) && st.Generation == 3 })
	pid3 := h.childPIDFromHealthz()
	if st3.Boot == st2.Boot || pid3 == pid2 || !processGone(pid2) {
		t.Fatalf("generation 3: boot %s pid %d; generation 2 boot %s pid %d (gone: %v)", st3.Boot, pid3, st2.Boot, pid2, processGone(pid2))
	}
	if n := h.cleanupCount(); n != 2 {
		t.Fatalf("after two stops cleanup ran %d times, want 2", n)
	}
}

// While the old child is still stopping, the status must not already name the
// new generation beside the old child's boot: a reader would take the old
// child for the new one.
func TestComposeStatusNamesANewGenerationOnlyWithItsOwnChild(t *testing.T) {
	h := newComposeHarness(t)
	slow := func(gen int64) map[string]any {
		sp := spec(gen, 1, "aa")
		sp["env"] = []any{map[string]string{"name": "FAKE_TERM_DELAY_MS", "value": "600"}}
		return sp
	}
	h.writeSpec(slow(1))
	old := h.waitFor("generation 1 running", running)
	h.writeSpec(slow(2))
	deadline := time.Now().Add(10 * time.Second)
	sawStop := false
	for {
		st := h.status()
		if st.Generation == 2 && st.Boot == old.Boot {
			t.Fatalf("the status names generation 2 beside generation 1's boot %s: %+v", old.Boot, st)
		}
		if strings.HasPrefix(st.Reason, "stopping the chief") {
			sawStop = true
		}
		if st.Generation == 2 && st.Boot != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("generation 2 never started: %+v", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !sawStop {
		t.Fatal("control: no sample fell inside the 600ms stop, so the check above saw nothing")
	}
}

func TestComposeRefusesASpecVersionItDoesNotKnow(t *testing.T) {
	h := newComposeHarness(t)
	sp := spec(1, 1, "aa")
	sp["version"] = 2
	h.writeSpec(sp)
	st := h.waitFor("error", func(st composeStatus) bool { return st.State == composeError })
	if st.Reason != "spec.json is version 2; this image reads version 1 only" {
		t.Fatalf("reason %q", st.Reason)
	}
	time.Sleep(150 * time.Millisecond)
	if _, err := os.Stat(h.envDump); err == nil {
		t.Fatal("a child was started for a version-2 spec")
	}
	// A running child is stopped when its spec turns into one it refuses.
	h.writeSpec(spec(1, 1, "aa"))
	h.waitFor("running", running)
	pid := h.childPIDFromHealthz()
	h.writeSpec(sp)
	h.waitFor("error", func(st composeStatus) bool { return st.State == composeError })
	if !processGone(pid) {
		t.Fatalf("the child (pid %d) still runs under a refused spec", pid)
	}
}

func TestComposeRefusesAVariableTheSupervisorSets(t *testing.T) {
	h := newComposeHarness(t)
	sp := spec(1, 1, "aa")
	sp["env"] = []any{map[string]string{"name": "CCD_LISTEN", "value": "0.0.0.0:1"}}
	h.writeSpec(sp)
	st := h.waitFor("error", func(st composeStatus) bool { return st.State == composeError })
	if st.Reason != "spec.json: env names CCD_LISTEN, which this supervisor sets itself" {
		t.Fatalf("reason %q", st.Reason)
	}
}

// The child's prompts are denied because the supervisor says so, not because a
// spec remembered to: a spec may not set the flag at all, to any value.
func TestComposeRefusesASpecThatSetsTheUnansweredPromptFlag(t *testing.T) {
	for _, list := range []string{"env", "secrets"} {
		t.Run(list, func(t *testing.T) {
			h := newComposeHarness(t)
			sp := spec(1, 1, "aa")
			sp[list] = []any{map[string]string{"name": "CCD_APPROVE_UNANSWERED", "value": ""}}
			h.writeSpec(sp)
			st := h.waitFor("error", func(st composeStatus) bool { return st.State == composeError })
			if want := "spec.json: " + list + " names CCD_APPROVE_UNANSWERED, which this supervisor sets itself"; st.Reason != want {
				t.Fatalf("reason %q, want %q", st.Reason, want)
			}
			if _, err := os.Stat(h.envDump); err == nil {
				t.Fatal("a child was started for a spec that sets CCD_APPROVE_UNANSWERED")
			}
		})
	}
}

func TestComposeReadsSpecFieldsItDoesNotKnow(t *testing.T) {
	h := newComposeHarness(t)
	sp := spec(1, 1, "aa")
	sp["fingerprint"] = "the driver's own"
	h.writeSpec(sp)
	if err := os.WriteFile(filepath.Join(h.dir, "driver.json"), []byte(`{"anything":1}`), 0o640); err != nil {
		t.Fatal(err)
	}
	h.waitFor("running despite an unknown field and driver.json", running)
}

const (
	composeSecretValue = "sekrit-turn-bearer-7f3a9c"
	composeEnvValue    = "env-value-41d2e8"
	composeNumberValue = 987654321
)

func TestComposeExportsSecretsToTheChildAndNeverLogsThem(t *testing.T) {
	h := newComposeHarness(t)
	sp := spec(1, 1, "aa")
	sp["env"] = []any{map[string]string{"name": "SOME_SETTING", "value": composeEnvValue}}
	sp["secrets"] = []any{map[string]string{"name": "MUSTER_GATEWAY_BEARER", "value": composeSecretValue}}
	h.writeSpec(sp)
	h.waitFor("running", running)
	b, err := os.ReadFile(h.envDump)
	if err != nil {
		t.Fatal(err)
	}
	env := "\n" + string(b) + "\n"
	// The positive control: the values DID pass through this process.
	for _, want := range []string{"\nMUSTER_GATEWAY_BEARER=" + composeSecretValue + "\n", "\nSOME_SETTING=" + composeEnvValue + "\n",
		"\nCCD_LISTEN=" + h.sup.childAddr + "\n", "\nCCD_APPROVE_UNANSWERED=deny\n"} {
		if !strings.Contains(env, want) {
			t.Fatalf("the child's environment lacks %q", strings.TrimSpace(want))
		}
	}
	for _, gone := range []string{"\n" + envSpecDir + "="} {
		if strings.Contains(env, gone) {
			t.Fatalf("the child's environment still holds %s", strings.TrimSpace(gone))
		}
	}
	// Through every path that writes a log line or a reason: a stop, a new
	// generation, and a spec refused with a value in the wrong place.
	sp["generation"] = 2
	h.writeSpec(sp)
	h.waitFor("generation 2", func(st composeStatus) bool { return running(st) && st.Generation == 2 })
	h.writeRaw(fmt.Sprintf(`{"version":1,"name":"chief","generation":3,"replicas":1,"epoch":"aa","secrets":[{"name":"X","value":%d}]}`, composeNumberValue))
	bad := h.waitFor("a refused spec", func(st composeStatus) bool { return st.State == composeError })
	if bad.Reason != `spec.json: field "secrets.value" is not a string` {
		t.Fatalf("reason %q", bad.Reason)
	}
	_, statusBody := h.get("/compose/status")
	all := h.log() + statusBody + bad.Reason
	if !strings.Contains(h.log(), "started the chief") {
		t.Fatalf("control: the captured log holds no start line, so it captured nothing:\n%s", h.log())
	}
	for _, v := range []string{composeSecretValue, composeEnvValue, strconv.Itoa(composeNumberValue)} {
		if strings.Contains(all, v) {
			t.Fatalf("a value from spec.json (%s) reached the log or the status:\n%s", v, all)
		}
	}
}

func TestComposeEpochEmptiesDataOnlyWhenARecordedEpochDiffers(t *testing.T) {
	logf := func(string, ...any) {}
	dir := t.TempDir()
	keep := filepath.Join(dir, "claude", "transcript.jsonl")
	if err := os.MkdirAll(filepath.Dir(keep), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keep, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, composeEpochFile)
	// No record: a fresh volume, or one used before epochs. Stamped, not emptied.
	if err := prepareEpoch(dir, "aa", logf); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("a volume with no recorded epoch was emptied: %v", err)
	}
	if b, _ := os.ReadFile(marker); string(b) != "aa\n" {
		t.Fatalf("the epoch recorded is %q, want aa", b)
	}
	// The same epoch: nothing happens.
	if err := prepareEpoch(dir, "aa", logf); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("the same epoch emptied the volume: %v", err)
	}
	// A different epoch: everything under it goes, the directory itself stays.
	if err := prepareEpoch(dir, "bb", logf); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 || ents[0].Name() != composeEpochFile {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Fatalf("after a new epoch the volume holds %v, want only %s", names, composeEpochFile)
	}
	if b, _ := os.ReadFile(marker); string(b) != "bb\n" {
		t.Fatalf("the epoch recorded is %q, want bb", b)
	}
}

// TestComposeEpochAFailedWipeIsRetriedNotStamped: a wipe that fails part-way
// (here a read-only directory under workspace/, the shape of Go's module
// cache) keeps the OLD epoch recorded, so the next attempt wipes again rather
// than finding no record and stamping the new epoch over the old chief's
// files. Once the entry can be removed, the wipe completes.
func TestComposeEpochAFailedWipeIsRetriedNotStamped(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes a 0555 directory's entries, so the wipe cannot be made to fail this way")
	}
	logf := func(string, ...any) {}
	dir := t.TempDir()
	marker := filepath.Join(dir, composeEpochFile)
	if err := os.WriteFile(marker, []byte("aa\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ro := filepath.Join(dir, "workspace", "mod", "ro")
	stale := filepath.Join(ro, "f")
	if err := os.MkdirAll(ro, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("old chief"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })
	for i := 1; i <= 2; i++ {
		if err := prepareEpoch(dir, "bb", logf); err == nil {
			t.Fatalf("attempt %d: a wipe that could not remove %s returned no error", i, stale)
		}
		if b, err := os.ReadFile(marker); err != nil || string(b) != "aa\n" {
			t.Fatalf("attempt %d: after a failed wipe the record is %q (%v), want the old epoch aa kept", i, b, err)
		}
		if _, err := os.Stat(stale); err != nil {
			t.Fatalf("attempt %d: control: the entry the wipe cannot remove is gone (%v), so the wipe never failed", i, err)
		}
	}
	if err := os.Chmod(ro, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := prepareEpoch(dir, "bb", logf); err != nil {
		t.Fatalf("the retried wipe: %v", err)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 || ents[0].Name() != composeEpochFile {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Fatalf("after the retried wipe the volume holds %v, want only %s", names, composeEpochFile)
	}
	if b, _ := os.ReadFile(marker); string(b) != "bb\n" {
		t.Fatalf("the epoch recorded is %q, want bb", b)
	}
}

func TestComposeEpochIsAppliedBeforeTheChildStarts(t *testing.T) {
	h := newComposeHarness(t)
	data := h.sup.dataDir
	if err := os.WriteFile(filepath.Join(data, composeEpochFile), []byte("aa\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "old-chief"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.writeSpec(spec(1, 1, "cc"))
	h.waitFor("running", running)
	if _, err := os.Stat(filepath.Join(data, "old-chief")); err == nil {
		t.Fatal("a new epoch did not empty /data before the start")
	}
	if b, _ := os.ReadFile(filepath.Join(data, composeEpochFile)); string(b) != "cc\n" {
		t.Fatalf("recorded epoch %q, want cc", b)
	}
}

func TestComposeStopsTheChildWhenItsSpecIsRemoved(t *testing.T) {
	h := newComposeHarness(t)
	h.writeSpec(spec(1, 1, "aa"))
	h.waitFor("running", running)
	pid := h.childPIDFromHealthz()
	if err := os.Remove(filepath.Join(h.dir, composeSpecFile)); err != nil {
		t.Fatal(err)
	}
	st := h.waitFor("awaiting_spec", func(st composeStatus) bool { return st.State == composeAwaitingSpec })
	if st.Reason != reasonRemoved {
		t.Fatalf("reason %q, want %q", st.Reason, reasonRemoved)
	}
	if !processGone(pid) || h.cleanupCount() != 1 {
		t.Fatalf("spec removed: child gone %v, cleanups %d", processGone(pid), h.cleanupCount())
	}
}

func TestComposeStartsAChildThatExitedAgain(t *testing.T) {
	h := newComposeHarness(t)
	sp := spec(1, 1, "aa")
	sp["env"] = []any{map[string]string{"name": "FAKE_EXIT", "value": "3"}}
	h.writeSpec(sp)
	st := h.waitFor("exited", func(st composeStatus) bool { return st.State == composeExited })
	if !strings.HasPrefix(st.Reason, "the chief's process exited with status 3; starting it again in ") {
		t.Fatalf("reason %q", st.Reason)
	}
	// It is started again after the backoff, without a new spec.
	deadline := time.Now().Add(10 * time.Second)
	for strings.Count(h.log(), "started the chief") < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the child that exited was never started again:\n%s", h.log())
		}
		time.Sleep(20 * time.Millisecond)
	}
	// And a new generation without FAKE_EXIT comes up at once.
	h.writeSpec(spec(2, 1, "aa"))
	h.waitFor("running after the exits", running)
}

func TestComposeRestartsAChildWhoseHealthzStopsAnswering(t *testing.T) {
	h := newComposeHarness(t)
	h.sup.livenessGrace = 300 * time.Millisecond
	h.writeSpec(spec(1, 1, "aa"))
	st1 := h.waitFor("running", running)
	if err := os.WriteFile(h.health, []byte("fail"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.waitFor("not ready", func(st composeStatus) bool { return st.State == composeRunning && !st.Ready })
	st := h.waitFor("a restart", func(st composeStatus) bool { return st.Boot != "" && st.Boot != st1.Boot })
	if !strings.Contains(h.log(), "stopped answering /healthz 200 for 300ms; restarting it") {
		t.Fatalf("restarted (%+v) without the liveness line:\n%s", st, h.log())
	}
}

func TestComposeRestartsAChildThatNeverComesUp(t *testing.T) {
	h := newComposeHarness(t)
	h.sup.startupBudget = 300 * time.Millisecond
	if err := os.WriteFile(h.health, []byte("fail"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.writeSpec(spec(1, 1, "aa"))
	st1 := h.waitFor("starting", func(st composeStatus) bool { return st.State == composeStarting && st.Boot != "" })
	h.waitFor("a restart", func(st composeStatus) bool { return st.Boot != "" && st.Boot != st1.Boot })
	if !strings.Contains(h.log(), "did not answer /healthz 200 within 300ms of starting; restarting it") {
		t.Fatalf("no startup-budget line:\n%s", h.log())
	}
}

// ⚠ AN INVARIANT GUARD OF THE PROXY, NOT REGRESSION COVERAGE: Go's
// ReverseProxy flushes a text/event-stream response after every write whatever
// its FlushInterval, so mutating FlushInterval to 0 does not redden this (an
// equivalent mutant). It pins that the second event is not needed for the
// first to arrive — that nothing between the client and the child buffers.
func TestComposeProxiesAStreamIncrementally(t *testing.T) {
	h := newComposeHarness(t)
	h.writeSpec(spec(1, 1, "aa"))
	h.waitFor("running", running)
	resp, err := http.Get(h.srv.URL + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	got := make(chan string, 1)
	go func() {
		var b strings.Builder
		for !strings.Contains(b.String(), "data: first\n\n") {
			line, err := r.ReadString('\n')
			b.WriteString(line)
			if err != nil {
				break
			}
		}
		got <- b.String()
	}()
	select {
	case s := <-got:
		if !strings.Contains(s, "data: first") || strings.Contains(s, "second") {
			t.Fatalf("first read: %q", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first event did not arrive while the child held the second back: the proxy buffers")
	}
	if err := os.WriteFile(h.release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rest, _ := io.ReadAll(r)
	if !strings.Contains(string(rest), "data: second") {
		t.Fatalf("the rest of the stream: %q", rest)
	}
}

// subreaperHelper (CCD_TEST_AS=subreaper) runs in a process of its own,
// because a subreaper and a reaper of zombies would disturb every other test's
// children. It orphans a `sleep` (its shell parent exits at once) and prints
// what descendants() saw before and after killDescendants, as JSON.
// CCD_TEST_SUBREAPER=0 skips the prctl: the control.
func subreaperHelper() int {
	if os.Getenv("CCD_TEST_SUBREAPER") != "0" {
		if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
			fmt.Println(err)
			return 1
		}
	}
	out, err := exec.Command("sh", "-c", "sleep 300 >/dev/null 2>&1 & echo $!").Output()
	if err != nil {
		fmt.Println(err)
		return 1
	}
	orphan, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	before := descendants(os.Getpid())
	_ = killDescendants(os.Getpid(), -1, 200*time.Millisecond, 2*time.Second, func(string, ...any) {})
	_, statErr := os.Stat(filepath.Join("/proc", strconv.Itoa(orphan)))
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"orphan": orphan, "before": before,
		"after": descendants(os.Getpid()), "orphan_gone": os.IsNotExist(statErr)})
	if os.Getenv("CCD_TEST_SUBREAPER") == "0" {
		_ = syscall.Kill(orphan, syscall.SIGKILL)
	}
	return 0
}

func TestComposeSweepFindsKillsAndReapsADaemonizedDescendant(t *testing.T) {
	run := func(subreaper string) (res struct {
		Orphan     int
		Before     []int
		After      []int
		OrphanGone bool `json:"orphan_gone"`
	}) {
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), "CCD_TEST_AS=subreaper", "CCD_TEST_SUBREAPER="+subreaper)
		out, err := cmd.Output()
		if err != nil || json.Unmarshal(out, &res) != nil {
			t.Fatalf("helper: %v: %s", err, out)
		}
		return res
	}
	r := run("1")
	if len(r.Before) != 1 || r.Before[0] != r.Orphan {
		t.Fatalf("as a subreaper, descendants() saw %v; want exactly the orphaned sleep %d", r.Before, r.Orphan)
	}
	if len(r.After) != 0 || !r.OrphanGone {
		t.Fatalf("after killDescendants: descendants %v, orphan gone (killed AND reaped: no /proc entry) %v", r.After, r.OrphanGone)
	}
	// The control: without the subreaper the orphan is re-parented away, and the
	// sweep cannot see it — which is why runCompose sets it.
	c := run("0")
	if len(c.Before) != 0 || c.OrphanGone {
		t.Fatalf("control (no subreaper): descendants %v, orphan gone %v; want it invisible and alive", c.Before, c.OrphanGone)
	}
}

func TestComposeRunsOnlyAsRootAndStartsTheChildAsTheCLIUser(t *testing.T) {
	if _, err := composeIdentity(composeCLIUID); err == nil ||
		!strings.Contains(err.Error(), "running as uid 1000; it must run as uid 0") {
		t.Fatalf("as uid 1000 (the CLI's own uid): %v, want a refusal", err)
	}
	cred, err := composeIdentity(0)
	if err != nil {
		t.Fatal(err)
	}
	if cred.Uid != 1000 || cred.Gid != 1000 || len(cred.Groups) != 1 || cred.Groups[0] != 1000 || cred.NoSetGroups {
		t.Fatalf("the child's credential is %+v, want uid 1000 gid 1000 groups exactly [1000]", cred)
	}
	s := newComposeSup(t.TempDir(), cred)
	attr := s.childCmd(nil).SysProcAttr
	if attr == nil || attr.Credential != cred || !attr.Setpgid {
		t.Fatalf("the child command's SysProcAttr is %+v, want the credential and its own process group", attr)
	}
}

// TestComposeIsNeverOffWhileAProcessItStartedLives: a stop whose cleanup finds
// something still alive (here a real process, standing in for a CLI that
// outlived kill-server and SIGKILL) must not be reported off — "off" is
// muster's word that the chief has stopped calling the owner's model. The
// status says error naming it, every step retries the cleanup, and a new spec
// starts nothing beside it; once it is gone the supervisor carries on.
func TestComposeIsNeverOffWhileAProcessItStartedLives(t *testing.T) {
	h := newComposeHarness(t)
	h.writeSpec(spec(1, 1, "aa"))
	h.waitFor("running", running)

	survivor := exec.Command("sleep", "300")
	if err := survivor.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = survivor.Wait(); close(exited) }()
	t.Cleanup(func() { _ = survivor.Process.Kill(); <-exited })
	alive := func() bool {
		select {
		case <-exited:
			return false
		default:
			return true
		}
	}
	h.mu.Lock()
	h.cleanupErr = func(int) error {
		if alive() {
			return fmt.Errorf("%d (sleep) still running 5s after SIGKILL", survivor.Process.Pid)
		}
		return nil
	}
	h.mu.Unlock()

	h.writeSpec(spec(1, 0, "aa"))
	st := h.waitFor("the failed stop reported", func(st composeStatus) bool { return st.State == composeError })
	want := fmt.Sprintf("the chief did not stop: %d (sleep) still running 5s after SIGKILL", survivor.Process.Pid)
	if st.Reason != want || st.Boot != "" || st.Ready {
		t.Fatalf("a stop that left a process alive: %+v, want error %q", st, want)
	}
	// Every step retries, and none says off; a new spec starts nothing.
	before := h.cleanupCount()
	h.writeSpec(spec(2, 1, "aa"))
	for end := time.Now().Add(500 * time.Millisecond); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if st := h.status(); st.State != composeError || st.Boot != "" {
			t.Fatalf("while a process it started lives the status moved to %+v", st)
		}
	}
	if h.sup.childPID() != 0 {
		t.Fatal("a child was started beside what the stopped one left")
	}
	if n := h.cleanupCount(); n < before+3 {
		t.Fatalf("the cleanup ran %d more time(s) in 500ms of 30ms steps; want it retried every step", n-before)
	}

	// Gone: the supervisor goes on to the spec it now reads.
	_ = survivor.Process.Kill()
	<-exited
	h.waitFor("generation 2 running once nothing is left", func(st composeStatus) bool { return running(st) && st.Generation == 2 })
	h.writeSpec(spec(2, 0, "aa"))
	h.waitFor("off, with nothing alive", func(st composeStatus) bool { return st.State == composeOff })
}

// TestComposeSweepWaitsForWhatItKilledAndNamesWhatSurvives: the sweep returns
// nil only once nothing is left. A process that is still there after its
// SIGKILL is waited for, and one that never goes is an error naming it.
func TestComposeSweepWaitsForWhatItKilledAndNamesWhatSurvives(t *testing.T) {
	run := func(left func(kills int) []int) (err error, kills int) {
		var clock time.Time
		op := sweepOps{
			left:  func() []int { return left(kills) },
			kill:  func(int) error { kills++; return nil },
			reap:  func() {},
			sleep: func(d time.Duration) { clock = clock.Add(d) },
			now:   func() time.Time { return clock },
		}
		err = sweep(op, time.Second, 2*time.Second, func(string, ...any) {})
		return err, kills
	}
	if err, kills := run(func(int) []int { return nil }); err != nil || kills != 0 {
		t.Fatalf("nothing left: %v, %d kill(s)", err, kills)
	}
	// Ends only well after its first SIGKILL: still waited for, and nil.
	polls := 0
	if err, kills := run(func(kills int) []int {
		if kills > 0 {
			if polls++; polls > 5 {
				return nil
			}
		}
		return []int{424242}
	}); err != nil || kills == 0 {
		t.Fatalf("a process that ends after its kill: %v, %d kill(s)", err, kills)
	}
	// Never ends: an error naming it, not a stop.
	err, kills := run(func(int) []int { return []int{424242} })
	if err == nil || !strings.Contains(err.Error(), "424242") || !strings.Contains(err.Error(), "after SIGKILL") || kills == 0 {
		t.Fatalf("a process that survives its kill: %v, %d kill(s); want an error naming it", err, kills)
	}
}

// TestComposeLeftoversIncludeTheAgentsUID: what a stop must see gone includes
// every process of the agent's uid, descended from the supervisor or not (here
// this test's own uid, and a process that is no descendant of root), and never
// the supervisor itself. Control: without a uid only descendants count.
func TestComposeLeftoversIncludeTheAgentsUID(t *testing.T) {
	p := exec.Command("sleep", "300")
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Process.Kill(); _ = p.Wait() }()
	const noSuchRoot = -7 // nothing descends from it
	got := leftovers(noSuchRoot, os.Getuid())
	if !slices.Contains(got, p.Process.Pid) || slices.Contains(got, os.Getpid()) {
		t.Fatalf("leftovers for uid %d: %v; want the sleep %d and not this process %d", os.Getuid(), got, p.Process.Pid, os.Getpid())
	}
	if got := leftovers(noSuchRoot, -1); len(got) != 0 {
		t.Fatalf("control, no uid: %v, want nothing (nothing descends from root)", got)
	}
}

// TestComposeLeftoversIncludeAnUnreapedZombie: a killed CLI is a zombie until it
// is reaped, and a stop that reported clean in that instant left the
// `[claude] <defunct>` #474's smoke saw after off. So a zombie is a leftover,
// below root or of the agent's uid, until it is reaped.
func TestComposeLeftoversIncludeAnUnreapedZombie(t *testing.T) {
	z := exec.Command("true")
	if err := z.Start(); err != nil {
		t.Fatal(err)
	}
	pid := z.Process.Pid
	waited := false
	defer func() {
		if !waited {
			_ = z.Wait()
		}
	}()
	// It has exited and is not reaped: a zombie.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, st, err := procStat(pid); err == nil && st == "Z" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d never became a zombie", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := leftovers(os.Getpid(), -1); !slices.Contains(got, pid) {
		t.Fatalf("below this process: leftovers %v lack the unreaped zombie %d", got, pid)
	}
	if got := leftovers(-7, os.Getuid()); !slices.Contains(got, pid) {
		t.Fatalf("by uid: leftovers %v lack the unreaped zombie %d", got, pid)
	}
	_ = z.Wait()
	waited = true
	if got := leftovers(os.Getpid(), -1); slices.Contains(got, pid) {
		t.Fatalf("after it was reaped: leftovers %v still name %d", got, pid)
	}
}
