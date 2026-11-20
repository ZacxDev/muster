package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirrorhost"
)

const extraToken = "mmt_EEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEE"

func writeExtraTarget(t *testing.T, url string, mode os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	tok := filepath.Join(dir, "la.token")
	if err := os.WriteFile(tok, []byte(extraToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tok, mode); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, "targets.json")
	if err := os.WriteFile(f, []byte(`[{"name":"la","url":"`+url+`","token_file":"`+tok+`"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// TestABadTargetsSettingIsLoggedNeverFatal: the targets file is still
// validated at boot, but — like every MCP failure (the endpoint is opt-in) — a
// bad one is LOGGED with its reason and IGNORED, never fatal: a token file
// readable by others, a bad primary name, a bad refresh. Each run ends at its
// context with rc 0 (a daemon told to stop), names the entry, and never prints
// the token. The control: a valid file logs no refusal.
func TestABadTargetsSettingIsLoggedNeverFatal(t *testing.T) {
	base := map[string]string{envURL: "https://m.example", envToken: "mmt_" + strings.Repeat("A", 43), envHost: "laptop"}
	with := func(k, v string) map[string]string {
		m := map[string]string{}
		for kk, vv := range base {
			m[kk] = vv
		}
		m[k] = v
		return m
	}
	// The daemon's context is ended before anything runs.
	prevB, prevC := newTmuxBackend, runContext
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	t.Cleanup(func() { newTmuxBackend, runContext = prevB, prevC })
	newTmuxBackend = func(string, string, string) mirrorhost.Backend { return listBackend{} }
	runContext = func() (context.Context, context.CancelFunc) { return ctx, cancel }
	for what, c := range map[string]struct {
		env  map[string]string
		want []string
	}{
		"a loose token file": {with(envMCPTargets, writeExtraTarget(t, "https://la.example", 0o644)), []string{"MCP targets file is IGNORED", `MCP target #1 ("la")`, "0600 or tighter"}},
		"a bad primary name": {with(envMCPPrimaryName, "Home"), []string{"-mcp-primary-name", "IGNORED"}},
		"a bad refresh":      {with(envMCPToolsRefresh, "soon"), []string{"-mcp-tools-refresh", "IGNORED"}},
	} {
		rc, _, errs := runWith(t, c.env)
		if rc != 0 {
			t.Errorf("%s: rc %d (%s); want 0 — never fatal", what, rc, errs)
		}
		for _, w := range c.want {
			if !strings.Contains(errs, w) {
				t.Errorf("%s: the log lacks %q: %s", what, w, errs)
			}
		}
		if strings.Contains(errs, extraToken) {
			t.Errorf("%s: the token was printed", what)
		}
	}
	if rc, _, errs := runWith(t, with(envMCPTargets, writeExtraTarget(t, "https://la.example", 0o600))); rc != 0 || strings.Contains(errs, "IGNORED") {
		t.Fatalf("CONTROL: a valid targets file: rc %d %s", rc, errs)
	}
}

// TestABadTargetsFileLeavesTheMirrorRunning: with a targets file whose token
// is group-readable, the live daemon still dials the primary's
// /mirror/connect and serves MCP (its tool watch reads the primary's list),
// logs the refusal, and the would-be extra is sent NOTHING. The positive
// control for that zero is TestAnExtraTargetGetsNoMirroring, where the same
// extra with a 0600 token is sent requests.
func TestABadTargetsFileLeavesTheMirrorRunning(t *testing.T) {
	var extraN atomic.Int32
	extra := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		extraN.Add(1)
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	defer extra.Close()
	dialled, listed := make(chan struct{}, 1), make(chan struct{}, 1)
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mirror/connect" {
			select {
			case dialled <- struct{}{}:
			default:
			}
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		select {
		case listed <- struct{}{}:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":"x","result":{"tools":[]}}`)
	}))
	defer primary.Close()
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listen := free.Addr().String()
	free.Close()
	prevB, prevC := newTmuxBackend, runContext
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { newTmuxBackend, runContext = prevB, prevC; cancel() })
	newTmuxBackend = func(string, string, string) mirrorhost.Backend { return listBackend{} }
	runContext = func() (context.Context, context.CancelFunc) { return ctx, cancel }
	done := make(chan struct {
		rc   int
		errs string
	}, 1)
	secretFile := filepath.Join(t.TempDir(), "mcp-secret")
	go func() {
		rc, _, errs := runWith(t, map[string]string{envURL: primary.URL, envToken: "mmt_" + strings.Repeat("A", 43), envHost: "laptop",
			envAllowHTTP: "1", envMCPListen: listen, envMCPSecret: secretFile,
			envMCPTargets: writeExtraTarget(t, extra.URL, 0o640)})
		done <- struct {
			rc   int
			errs string
		}{rc, errs}
	}()
	for name, ch := range map[string]chan struct{}{"dial the primary": dialled, "read the primary's tool list": listed} {
		select {
		case <-ch:
		case r := <-done:
			t.Fatalf("run ended (rc %d) before it could %s: %s", r.rc, name, r.errs)
		case <-time.After(10 * time.Second):
			t.Fatalf("the daemon did not %s within 10s", name)
		}
	}
	// A call naming the ignored target is refused saying why — the wiring of
	// the refusal into the proxy (MCPProxy.ExtrasIgnored), through the real
	// endpoint. Refused before any pane is resolved, so this backend's lack
	// of windows does not matter.
	secret, err := os.ReadFile(secretFile)
	if err != nil {
		t.Fatal(err)
	}
	// With the tool watch on (the default) a call names the MCP session its
	// initialize was issued, as Claude Code's does.
	init, _ := http.NewRequest(http.MethodPost, "http://"+listen+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`))
	init.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(secret)))
	iresp, err := http.DefaultClient.Do(init)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, iresp.Body)
	iresp.Body.Close()
	session := iresp.Header.Get(mirrorhost.MCPSessionIDHeader)
	if session == "" {
		t.Fatalf("initialize issued no session: %d", iresp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodPost, "http://"+listen+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"agents_list","arguments":{"instance":"la"}}}`))
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(secret)))
	req.Header.Set(mirrorhost.MCPSessionIDHeader, session)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	reply, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(reply), "targets file was ignored") || !strings.Contains(string(reply), "0600 or tighter") {
		t.Fatalf("a call naming the ignored target: %s", reply)
	}
	cancel()
	select {
	case r := <-done:
		if r.rc != 0 || !strings.Contains(r.errs, "MCP targets file is IGNORED") || !strings.Contains(r.errs, "MCP endpoint for this host's Claude Code sessions") {
			t.Fatalf("rc %d, log: %s", r.rc, r.errs)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not stop")
	}
	if n := extraN.Load(); n != 0 {
		t.Fatalf("the refused extra was sent %d request(s)", n)
	}
}

// TestAnExtraTargetGetsNoMirroring: the daemon with an extra target dials the
// PRIMARY's /mirror/connect, and the extra is sent only remote-marked
// /mirror/mcp requests carrying its own token (the tool watch's tools/list) —
// never /mirror/connect, never the primary's token. The positive control is
// that the extra is sent something at all, so the absence is not a target
// wired to nothing; the startup log names it without its token.
func TestAnExtraTargetGetsNoMirroring(t *testing.T) {
	var mu sync.Mutex
	var extraSaw []*http.Request
	extraHit := make(chan struct{}, 1)
	extra := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		extraSaw = append(extraSaw, r.Clone(context.Background()))
		mu.Unlock()
		select {
		case extraHit <- struct{}{}:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":"x","result":{"tools":[],"_meta":{"muster/caller":"remote"}}}`)
	}))
	defer extra.Close()
	dialled := make(chan struct{}, 1)
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mirror/connect" {
			select {
			case dialled <- struct{}{}:
			default:
			}
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":"x","result":{"tools":[]}}`)
	}))
	defer primary.Close()
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listen := free.Addr().String()
	free.Close()

	prevB, prevC := newTmuxBackend, runContext
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { newTmuxBackend, runContext = prevB, prevC; cancel() })
	newTmuxBackend = func(string, string, string) mirrorhost.Backend { return listBackend{} }
	runContext = func() (context.Context, context.CancelFunc) { return ctx, cancel }
	done := make(chan string, 1)
	go func() {
		_, _, errs := runWith(t, map[string]string{envURL: primary.URL, envToken: "mmt_" + strings.Repeat("A", 43), envHost: "laptop",
			envAllowHTTP: "1", envMCPListen: listen, envMCPSecret: filepath.Join(t.TempDir(), "mcp-secret"),
			envMCPTargets: writeExtraTarget(t, extra.URL, 0o600)})
		done <- errs
	}()
	for _, ch := range []chan struct{}{dialled, extraHit} {
		select {
		case <-ch:
		case <-time.After(10 * time.Second):
			t.Fatal("the daemon did not dial the primary and read the extra's tool list within 10s")
		}
	}
	cancel()
	var errs string
	select {
	case errs = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("run did not stop")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(extraSaw) == 0 {
		t.Fatal("POSITIVE CONTROL: the extra was sent nothing")
	}
	for _, r := range extraSaw {
		if r.URL.Path != "/mirror/mcp" || r.Method != http.MethodPost || r.Header.Get(mirrorhost.MCPClientHeader) != mirrorhost.MCPClientRemote ||
			r.Header.Get("Authorization") != "Bearer "+extraToken {
			t.Errorf("the extra was sent %s %s (client %q, its own token: %v)", r.Method, r.URL.Path, r.Header.Get(mirrorhost.MCPClientHeader),
				r.Header.Get("Authorization") == "Bearer "+extraToken)
		}
	}
	if !strings.Contains(errs, "also reach la=") || strings.Contains(errs, extraToken) {
		t.Fatalf("startup log: %s", errs)
	}
}
