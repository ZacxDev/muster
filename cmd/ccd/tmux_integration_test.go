//go:build tmuxit

package main

// The real-tmux suite: ccd driving a REAL tmux server whose pane runs the fake
// CLI (fakeclaude_test.go), with the image's REAL settings template wired to the
// REAL `ccd hook` subcommand, and the turn sent through muster's own client.
//
//	go test -tags tmuxit -race -count=1 -run TestTmux ./cmd/ccd/
//
// 🔴 IT NEVER SKIPS. A missing tmux is a FAILURE, because a suite that skips on
// the runner that lacks its tool reports green having tested nothing. The build
// tag keeps it out of `go test ./...` (and out of the nix sandbox, which has no
// tmux); the CI job that runs it installs tmux and asserts it is there.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type tmuxRig struct {
	srv     *server
	gwURL   string
	cfg, ws string
	tmux    tmuxTerminal
}

func startTmuxRig(t *testing.T, trustWorkspace bool) *tmuxRig {
	t.Helper()
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal("tmux is REQUIRED by the tmuxit suite and is not on PATH; install it (this suite never skips)")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	cfg := filepath.Join(root, "claude")
	ws := filepath.Join(root, "workspace")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, as := range map[string]string{"ccd": "ccd", "claude": "claude"} {
		shim := fmt.Sprintf("#!/bin/sh\nCCD_TEST_AS=%s exec %q \"$@\"\n", as, exe)
		if err := os.WriteFile(filepath.Join(bin, name), []byte(shim), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	seedWS := ws
	if !trustWorkspace {
		seedWS = filepath.Join(root, "some-other-workspace")
	}
	seedInto(t, cfg, seedWS)
	if err := os.MkdirAll(ws, 0o700); err != nil {
		t.Fatal(err)
	}

	sock := fmt.Sprintf("ccd-it-%d-%d", os.Getpid(), time.Now().UnixNano())
	term := tmuxTerminal{bin: tmuxBin, socket: sock, target: "cc", enterDelay: 150 * time.Millisecond}
	bearer, _ := resolveBearer(knownHooksToken, "")
	srv := newServer(serverConfig{Bearer: bearer, ConfigDir: cfg, SubmitTimeout: 10 * time.Second,
		TurnTimeout: 30 * time.Second}, term, newAuthTracker())

	hl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: srv.hookHandler()}
	go hs.Serve(hl)
	t.Cleanup(func() { hs.Close() })
	gw := gatewayServer(t, srv)

	cmd := exec.Command(tmuxBin, "-L", sock, "-f", "/dev/null", "new-session", "-d", "-s", "cc",
		"-x", "200", "-y", "50", "-c", ws, filepath.Join(bin, "claude"))
	// The environment goes on the tmux CLIENT that starts this private server, so
	// it becomes the server's global environment. (`new-session -e` was not
	// applied to PATH for the pane's command on tmux 3.7c, which left the
	// `ccd` shim unresolvable from the hooks.) SHELL=/bin/sh because tmux runs
	// the pane command through $SHELL, and a shell that reads rc files can
	// rewrite PATH.
	cmd.Env = append(os.Environ(),
		"PATH="+bin+":"+os.Getenv("PATH"),
		"CLAUDE_CONFIG_DIR="+cfg,
		"CCD_HOOK_URL=http://"+hl.Addr().String(),
		"SHELL=/bin/sh")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tmux new-session: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command(tmuxBin, "-L", sock, "kill-server").Run() })
	return &tmuxRig{srv: srv, gwURL: gw.URL, cfg: cfg, ws: ws, tmux: term}
}

func (r *tmuxRig) started() bool {
	r.srv.mu.Lock()
	defer r.srv.mu.Unlock()
	return r.srv.sessionStarted
}

func (r *tmuxRig) waitStarted(t *testing.T) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if r.started() {
			return
		}
	}
	pane, _ := exec.Command(r.tmux.bin, "-L", r.tmux.socket, "capture-pane", "-p", "-t", "cc").CombinedOutput()
	hooklog, _ := os.ReadFile(filepath.Join(r.cfg, "fake-hooks.log"))
	t.Fatalf("no SessionStart hook reached ccd within 15s; the pane shows:\n%s\nhook log:\n%s", pane, hooklog)
}

// transcriptHas reports whether some user record's content is exactly s.
func (r *tmuxRig) transcriptHas(t *testing.T, s string) bool {
	t.Helper()
	for _, v := range r.userRecords(t) {
		if v == s {
			return true
		}
	}
	return false
}

// userRecords returns promptId -> prompt content from the transcript the fake wrote.
func (r *tmuxRig) userRecords(t *testing.T) map[string]string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(r.cfg, "projects", "*", "*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("transcripts: %v", files)
	}
	f, err := os.Open(files[0])
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string]string{}
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var rec struct {
			Type     string `json:"type"`
			PromptID string `json:"promptId"`
			Message  struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.Type != "user" {
			continue
		}
		var s string
		if json.Unmarshal(rec.Message.Content, &s) == nil {
			key := rec.PromptID
			if key == "" { // a local command or shell record: no prompt id
				n++
				key = fmt.Sprintf("local-%d", n)
			}
			out[key] = s
		}
	}
	return out
}

func TestTmuxPastedBytesArriveExactlyAndTheReplyComesBack(t *testing.T) {
	rig := startTmuxRig(t, true)
	rig.waitStarted(t)
	gw := mustersGateway(t, rig.gwURL)

	prompts := []string{
		"single line",
		"para one\n\npara two with \"quotes\" and $HOME and ${PATH} and `backticks` and $(echo no)",
		"trailing newline kept inside\nthe paste\n\n\nthree blank lines above; 'single' \\backslash\\ ✓ é 日本",
	}
	for _, p := range prompts {
		reply, err := gw.Chat(context.Background(), contractAgent, "s", p, nil)
		if err != nil {
			t.Fatalf("prompt %q: %v", p, err)
		}
		sum := sha256.Sum256([]byte(p))
		want := fmt.Sprintf("received %d bytes\n\nsha256 %s", len(p), hex.EncodeToString(sum[:]))
		if reply != want {
			t.Fatalf("prompt %q:\nreply %q\nwant  %q", p, reply, want)
		}
	}
	// A leading `!` would run in a shell in the real CLI (and here); ccd prefixes
	// one space, so it reaches the model as text.
	for _, p := range []string{"!echo this must not run in a shell", "!ls"} {
		reply, err := gw.Chat(context.Background(), contractAgent, "s", p, nil)
		if err != nil {
			t.Fatalf("prompt %q: %v", p, err)
		}
		sum := sha256.Sum256([]byte(" " + p))
		if want := fmt.Sprintf("received %d bytes\n\nsha256 %s", len(p)+1, hex.EncodeToString(sum[:])); reply != want {
			t.Fatalf("prompt %q: reply %q, want %q", p, reply, want)
		}
		prompts = append(prompts, " "+p)
	}
	// A leading `/` is let through byte-exact. Text that names no command is an
	// ordinary turn (as on the real CLI)...
	{
		p := "/etc/hosts is broken"
		reply, err := gw.Chat(context.Background(), contractAgent, "s", p, nil)
		sum := sha256.Sum256([]byte(p))
		if want := fmt.Sprintf("received %d bytes\n\nsha256 %s", len(p), hex.EncodeToString(sum[:])); err != nil || reply != want {
			t.Fatalf("prompt %q: reply %q err %v, want %q", p, reply, err, want)
		}
		prompts = append(prompts, p)
	}
	// ...and a command runs in the CLI, reaching it byte-exact, with a typed
	// local_command error to the caller (there is no model turn to reply).
	if _, err := gw.Chat(context.Background(), contractAgent, "s", "/compact", nil); err == nil ||
		!strings.Contains(err.Error(), `"type":"local_command"`) {
		t.Fatalf("/compact: err %v, want local_command", err)
	}
	if !rig.transcriptHas(t, "<local-command>/compact</local-command>") {
		t.Fatal("the CLI did not receive /compact byte-exact as a command")
	}
	// The other end of the same claim: the transcript holds each prompt verbatim.
	got := rig.userRecords(t)
	seen := map[string]bool{}
	for _, v := range got {
		seen[v] = true
	}
	for _, p := range prompts {
		if !seen[p] {
			t.Fatalf("transcript does not hold %q verbatim; it holds %q", p, got)
		}
	}
}

func TestTmuxAFailedTurnIsAnErrorThroughMustersClient(t *testing.T) {
	rig := startTmuxRig(t, true)
	rig.waitStarted(t)
	reply, err := mustersGateway(t, rig.gwURL).Chat(context.Background(), contractAgent, "s", "please FAIL-AUTH now", nil)
	if err == nil || reply != "" || !strings.Contains(err.Error(), `"type":"auth_failed"`) {
		t.Fatalf("reply %q err %v", reply, err)
	}
	// And the session is usable again afterwards.
	if _, err := mustersGateway(t, rig.gwURL).Chat(context.Background(), contractAgent, "s", "after", nil); err != nil {
		t.Fatalf("turn after a failure: %v", err)
	}
}

// Without the trust seed for the EXACT working directory, the (fake) CLI sits on
// its trust screen and fires no hooks: ccd must report not_ready rather than
// paste a prompt into the dialog.
func TestTmuxAnUntrustedWorkspaceIsNotReady(t *testing.T) {
	rig := startTmuxRig(t, false)
	time.Sleep(2 * time.Second)
	if rig.started() {
		t.Fatal("SessionStart fired in an untrusted workspace")
	}
	_, err := mustersGateway(t, rig.gwURL).Chat(context.Background(), contractAgent, "s", "hi", nil)
	if err == nil || !strings.Contains(err.Error(), `"type":"not_ready"`) {
		t.Fatalf("err = %v", err)
	}
	if err := rig.tmux.Alive(context.Background()); err != nil {
		t.Fatalf("control: the tmux session itself should be alive: %v", err)
	}
}

// The WHOLE BINARY: `ccd serve` as its own process, configured only through the
// environment the image's entrypoint sets, creating and SUPERVISING the tmux
// session itself (CCD_SUPERVISE=1) after its listeners are bound.
//
//   - health is 200 with no credential proof (there is no probe) and reports a
//     FRESH supervised start;
//   - a turn round-trips through muster's client;
//   - 🔴 `/exit` typed in the pane restarts the CLI IN THE POD with --continue:
//     the same session id, ONE transcript file holding the prompts from before
//     and after, and a turn works again.
func TestTmuxTheServeBinarySupervisesTheSessionAndResumesAfterExit(t *testing.T) {
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal("tmux is REQUIRED by the tmuxit suite and is not on PATH; install it (this suite never skips)")
	}
	exe, _ := os.Executable()
	root := t.TempDir()
	bin, cfg, ws := filepath.Join(root, "bin"), filepath.Join(root, "claude"), filepath.Join(root, "workspace")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, as := range map[string]string{"ccd": "ccd", "claude": "claude"} {
		shim := fmt.Sprintf("#!/bin/sh\nCCD_TEST_AS=%s exec %q \"$@\"\n", as, exe)
		if err := os.WriteFile(filepath.Join(bin, name), []byte(shim), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	seedInto(t, cfg, ws)
	free := func() string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		return l.Addr().String()
	}
	gwAddr, hookAddr := free(), free()
	sock := fmt.Sprintf("ccd-it-bin-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { _ = exec.Command(tmuxBin, "-L", sock, "kill-server").Run() })

	cmd := exec.Command(filepath.Join(bin, "ccd"), "serve")
	cmd.Dir = ws
	cmd.Env = append(os.Environ(),
		"PATH="+bin+":"+os.Getenv("PATH"), "SHELL=/bin/sh",
		"HOOKS_TOKEN="+knownHooksToken, "CLAUDE_CONFIG_DIR="+cfg, "CCD_WORKSPACE="+ws,
		"CCD_LISTEN="+gwAddr, "CCD_HOOK_LISTEN="+hookAddr, "CCD_HOOK_URL=http://"+hookAddr,
		"CCD_TMUX_SOCKET="+sock, "CCD_TMUX_CONF=/dev/null",
		"CCD_SUPERVISE=1", "CCD_CLAUDE_BIN="+filepath.Join(bin, "claude"))
	var logs syncBuffer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	waitHealth := func(label string, ok func(health) bool) health {
		t.Helper()
		var h health
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			resp, err := http.Get("http://" + gwAddr + "/healthz")
			if err != nil {
				continue
			}
			h = health{}
			_ = json.NewDecoder(resp.Body).Decode(&h)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && ok(h) {
				return h
			}
		}
		t.Fatalf("%s: healthz never got there; last %+v sup=%+v; ccd log:\n%s", label, h, h.Supervisor, logs.String())
		return h
	}
	h := waitHealth("first start", func(h health) bool { return h.Session == sessionStartedState })
	if h.Terminal != "ok" || h.Auth != authUnknown || h.Supervisor == nil ||
		h.Supervisor.CLI != cliRunning || h.Supervisor.Mode != modeFresh || h.Supervisor.Starts != 1 {
		t.Fatalf("after the first start: %+v sup=%+v", h, h.Supervisor)
	}
	gw := mustersGateway(t, "http://"+gwAddr)
	chat := func(p string) {
		t.Helper()
		reply, err := gw.Chat(context.Background(), contractAgent, "s", p, nil)
		sum := sha256.Sum256([]byte(p))
		if want := fmt.Sprintf("received %d bytes\n\nsha256 %s", len(p), hex.EncodeToString(sum[:])); err != nil || reply != want {
			t.Fatalf("prompt %q: reply %q err %v; ccd log:\n%s", p, reply, err, logs.String())
		}
	}
	chat("before the exit")

	// The operator types /exit in the attached terminal.
	term := tmuxTerminal{bin: tmuxBin, socket: sock, target: "cc"}
	if err := term.run(context.Background(), nil, "send-keys", "-t", "cc", "-l", "/exit"); err != nil {
		t.Fatal(err)
	}
	if err := term.Enter(context.Background()); err != nil {
		t.Fatal(err)
	}
	h = waitHealth("restart after /exit", func(h health) bool {
		return h.Supervisor != nil && h.Supervisor.Starts == 2 && h.Session == sessionStartedState
	})
	if h.Supervisor.Mode != modeContinue || h.Supervisor.LastExit == nil || *h.Supervisor.LastExit != 0 {
		t.Fatalf("after /exit: sup=%+v", h.Supervisor)
	}
	chat("after the exit")

	files, _ := filepath.Glob(filepath.Join(cfg, "projects", "*", "*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("want ONE transcript (the conversation resumed, not forked), have %v", files)
	}
	b, _ := os.ReadFile(files[0])
	for _, p := range []string{`"content":"before the exit"`, `"content":"after the exit"`} {
		if !strings.Contains(string(b), p) {
			t.Fatalf("the one transcript lacks %s", p)
		}
	}
}

// 🔴 A CLI THAT DIES AT ONCE, IN A REAL tmux PANE: the supervisor restarts it with
// backoff, then reports crash_loop and /healthz turns 503. Also proves the
// remain-on-exit ordering: a process that exits immediately still leaves a dead
// pane to read its status from.
func TestTmuxACrashingCLIEndsInCrashLoopAndFailsHealth(t *testing.T) {
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal("tmux is REQUIRED by the tmuxit suite and is not on PATH; install it (this suite never skips)")
	}
	exe, _ := os.Executable()
	root := t.TempDir()
	shim := filepath.Join(root, "claude")
	if err := os.WriteFile(shim, []byte(fmt.Sprintf("#!/bin/sh\nCCD_TEST_AS=claude CCD_FAKE_CRASH=1 exec %q \"$@\"\n", exe)), 0o755); err != nil {
		t.Fatal(err)
	}
	sock := fmt.Sprintf("ccd-it-crash-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { _ = exec.Command(tmuxBin, "-L", sock, "kill-server").Run() })
	term := tmuxTerminal{bin: tmuxBin, socket: sock, conf: "/dev/null", target: "cc"}
	srv := newServer(serverConfig{Bearer: "x", ConfigDir: root}, term, newAuthTracker())
	sup := newSupervisor(term, shim, root, root)
	sup.poll, sup.backoffMin = 20*time.Millisecond, 10*time.Millisecond
	srv.sup = sup
	gw := gatewayServer(t, srv)

	done := make(chan error, 1)
	go func() { done <- sup.run(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("no crash loop within 20s: %+v", sup.state())
	}
	st := sup.state()
	if st.CLI != cliCrashLoop || st.Starts != 5 || st.LastExit == nil || *st.LastExit != 3 {
		t.Fatalf("state %+v", st)
	}
	resp, err := http.Get(gw.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("healthz %d during a crash loop, want 503", resp.StatusCode)
	}
	// Control: the tmux session itself is alive (the dead pane is kept), so the
	// 503 is the crash loop's, not a dead terminal's.
	if err := term.Alive(context.Background()); err != nil {
		t.Fatalf("control: %v", err)
	}
}

// syncBuffer is a strings.Builder safe to write from a child's two streams.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
