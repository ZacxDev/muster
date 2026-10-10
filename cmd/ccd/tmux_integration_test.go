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
			out[rec.PromptID] = s
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
	// A leading `!` or `/` would run locally in the real CLI (and here); ccd
	// prefixes one space, so these reach the model as text.
	for _, p := range []string{"!echo this must not run in a shell", "/help is a path, not a command"} {
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
// environment the image's entrypoint sets, creating the tmux session itself
// (CCD_SESSION_COMMAND) after its listeners are bound, with the probe off. Its
// SessionStart must arrive and a turn must round-trip through muster's client.
func TestTmuxTheServeBinaryStartsTheSessionAndAnswers(t *testing.T) {
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
		"CCD_TMUX_SOCKET="+sock, "CCD_TMUX_CONF=/dev/null", "CCD_PROBE=off",
		"CCD_SESSION_COMMAND="+filepath.Join(bin, "claude"))
	var logs strings.Builder
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	var h health
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		resp, err := http.Get("http://" + gwAddr + "/healthz")
		if err != nil {
			continue
		}
		_ = json.NewDecoder(resp.Body).Decode(&h)
		resp.Body.Close()
		if h.SessionStarted {
			break
		}
	}
	if !h.SessionStarted || h.Terminal != "ok" || h.Auth != authUnknown {
		t.Fatalf("healthz %+v; ccd log:\n%s", h, logs.String())
	}
	reply, err := mustersGateway(t, "http://"+gwAddr).Chat(context.Background(), contractAgent, "s", "whole binary", nil)
	sum := sha256.Sum256([]byte("whole binary"))
	if err != nil || reply != fmt.Sprintf("received 12 bytes\n\nsha256 %s", hex.EncodeToString(sum[:])) {
		t.Fatalf("reply %q err %v; ccd log:\n%s", reply, err, logs.String())
	}
}
