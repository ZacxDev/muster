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

// TestABadTargetsFileStopsTheDaemon: the targets file is validated at boot —
// a token file readable by others stops the daemon (rc 2) naming the entry,
// before anything dials, and never prints the token; so does a bad primary
// name or refresh. The control: the same file with the token at 0600 passes
// boot (the run then ends at its context, as a daemon told to stop).
func TestABadTargetsFileStopsTheDaemon(t *testing.T) {
	base := map[string]string{envURL: "https://m.example", envToken: "mmt_" + strings.Repeat("A", 43), envHost: "laptop"}
	with := func(k, v string) map[string]string {
		m := map[string]string{}
		for kk, vv := range base {
			m[kk] = vv
		}
		m[k] = v
		return m
	}
	// The daemon's context is ended before anything runs, so a check that
	// let a bad file through ends the run (rc 0, below the want of 2) instead
	// of running a daemon.
	prevB, prevC := newTmuxBackend, runContext
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	t.Cleanup(func() { newTmuxBackend, runContext = prevB, prevC })
	newTmuxBackend = func(string, string, string) mirrorhost.Backend { return listBackend{} }
	runContext = func() (context.Context, context.CancelFunc) { return ctx, cancel }
	for what, c := range map[string]struct {
		env  map[string]string
		want string
	}{
		"a loose token file": {with(envMCPTargets, writeExtraTarget(t, "https://la.example", 0o644)), `MCP target #1 ("la")`},
		"a bad primary name": {with(envMCPPrimaryName, "Home"), "-mcp-primary-name"},
		"a bad refresh":      {with(envMCPToolsRefresh, "soon"), "-mcp-tools-refresh"},
	} {
		rc, _, errs := runWith(t, c.env)
		if rc != 2 || !strings.Contains(errs, c.want) {
			t.Errorf("%s: rc %d, %s; want 2 naming %q", what, rc, errs, c.want)
		}
		if strings.Contains(errs, extraToken) {
			t.Errorf("%s: the token was printed", what)
		}
	}

	if rc, _, errs := runWith(t, with(envMCPTargets, writeExtraTarget(t, "https://la.example", 0o600))); rc != 0 {
		t.Fatalf("CONTROL: a valid targets file: rc %d %s", rc, errs)
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
