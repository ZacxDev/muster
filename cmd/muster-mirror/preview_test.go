package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirrorhost"
	"github.com/ZacxDev/muster/internal/opencodehost"
	"github.com/ZacxDev/muster/internal/previewhost"
)

const goodPreviewToken = "mpt_" + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// previewFiles writes a previews.json and a token file (0600) into a temp dir.
func previewFiles(t *testing.T, previews string) (file, token string) {
	t.Helper()
	dir := t.TempDir()
	file, token = filepath.Join(dir, "previews.json"), filepath.Join(dir, "preview-token")
	if previews != "" {
		if err := os.WriteFile(file, []byte(previews), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(token, []byte(goodPreviewToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return file, token
}

// TestCheckPreviewRefusesEachBadInputWithItsReason: every input -preview
// validates, wrong one way at a time, is refused with the reason naming it;
// the all-good control passes. The token never appears in a reason.
func TestCheckPreviewRefusesEachBadInputWithItsReason(t *testing.T) {
	file, token := previewFiles(t, `{"ports": ["5173"]}`)
	good := previewFlags{On: true, URLs: "ws://192.0.2.7:9000/v1/host,wss://tunnel.example.net/v1/host", TokenFile: token, File: file}
	setup, err := checkPreview(good, true)
	if err != nil {
		t.Fatalf("control: %v", err)
	}
	if setup.Token != goodPreviewToken || len(setup.URLs) != 2 || setup.URLs[0] != "ws://192.0.2.7:9000/v1/host" || !setup.Config.Ports.Contains(5173) {
		t.Fatalf("control setup: %+v", setup)
	}

	dir := t.TempDir()
	write := func(name, body string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(token, link); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		mut     func(*previewFlags)
		tenant  bool
		wantWhy string
	}{
		{"per-tenant daemon", func(*previewFlags) {}, true, "default tmux server's daemon only"},
		{"missing previews.json", func(p *previewFlags) { p.File = filepath.Join(dir, "nope.json") }, false, "does not exist"},
		{"privileged port", func(p *previewFlags) { p.File = write("low.json", `{"ports": ["443"]}`, 0o600) }, false, "port 443 is outside 1024-65535"},
		{"malformed range", func(p *previewFlags) { p.File = write("rng.json", `{"ports": ["9000-8000"]}`, 0o600) }, false, "the range ends (8000) before it starts (9000)"},
		{"no urls", func(p *previewFlags) { p.URLs = " , " }, false, "names no gateway endpoint"},
		{"http url", func(p *previewFlags) { p.URLs = "https://tunnel.example.net/v1/host" }, false, "is not ws:// or wss://"},
		{"url with a query", func(p *previewFlags) { p.URLs = "wss://tunnel.example.net/v1/host?t=1" }, false, "nothing else"},
		{"url with user info", func(p *previewFlags) { p.URLs = "wss://u:p@tunnel.example.net/v1/host" }, false, "nothing else"},
		{"too many urls", func(p *previewFlags) { p.URLs = "ws://a/,ws://b/,ws://c/,ws://d/,ws://e/" }, false, "at most 4"},
		{"no token file", func(p *previewFlags) { p.TokenFile = "" }, false, "set -preview-token-file"},
		{"token readable by group and others", func(p *previewFlags) { p.TokenFile = write("open", goodPreviewToken, 0o644) }, false, "readable by group or others (mode 0644)"},
		{"token readable by group", func(p *previewFlags) { p.TokenFile = write("grp", goodPreviewToken, 0o640) }, false, "readable by group or others (mode 0640)"},
		{"token readable by others only", func(p *previewFlags) { p.TokenFile = write("oth", goodPreviewToken, 0o604) }, false, "readable by group or others (mode 0604)"},
		{"token symlink", func(p *previewFlags) { p.TokenFile = link }, false, "not a regular file"},
		{"not a preview token", func(p *previewFlags) { p.TokenFile = write("mmt", "mmt_"+strings.Repeat("B", 43), 0o600) }, false, "does not hold a preview host token"},
		{"bare prefix", func(p *previewFlags) { p.TokenFile = write("bare", "mpt_", 0o600) }, false, "does not hold a preview host token"},
	} {
		t.Run(c.name, func(t *testing.T) {
			pf := good
			c.mut(&pf)
			_, err := checkPreview(pf, !c.tenant)
			if err == nil || !strings.Contains(err.Error(), c.wantWhy) {
				t.Fatalf("got %v, want a reason saying %q", err, c.wantWhy)
			}
			if strings.Contains(err.Error(), goodPreviewToken[4:]) {
				t.Fatalf("the reason prints the token: %v", err)
			}
		})
	}
}

// TestATokenOnlyItsOwnerCanReadIsAccepted: the controls for the mode cases
// above — 0400 and 0700 (owner only, whatever the owner's bits) pass.
func TestATokenOnlyItsOwnerCanReadIsAccepted(t *testing.T) {
	file, _ := previewFiles(t, `{"ports": ["5173"]}`)
	for _, mode := range []os.FileMode{0o400, 0o600, 0o700} {
		p := filepath.Join(t.TempDir(), "tok")
		if err := os.WriteFile(p, []byte(goodPreviewToken), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := checkPreview(previewFlags{On: true, URLs: "wss://tunnel.example.net/v1/host", TokenFile: p, File: file}, true); err != nil {
			t.Errorf("mode %04o refused: %v", mode, err)
		}
	}
}

// TestTheRefusedMCPPortIsTheOneTheEndpointBinds is the relationship between
// -mcp-listen and previews: for every address, EITHER ListenMCP refuses it
// (no endpoint, nothing to protect) OR the port it actually bound is refused by
// alwaysRefusedPorts(that address). Port 0 and a service name used to bind a
// port the text did not show, leaving the refused set empty.
func TestTheRefusedMCPPortIsTheOneTheEndpointBinds(t *testing.T) {
	cfg, err := previewhost.Parse([]byte(`{"ports": ["1024-65535"]}`))
	if err != nil {
		t.Fatal(err)
	}
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	freePort := free.Addr().(*net.TCPAddr).Port
	free.Close()
	for _, c := range []struct {
		addr       string
		mustRefuse bool
	}{
		{"127.0.0.1:0", true},
		{"127.0.0.1:http", true},
		{"127.0.0.1:" + fmt.Sprint(freePort), false},
		{"127.0.0.1:+" + fmt.Sprint(freePort), true},
	} {
		l, err := mirrorhost.ListenMCP(c.addr)
		if err != nil {
			if !c.mustRefuse {
				t.Errorf("%s: refused: %v", c.addr, err)
			} else if !strings.Contains(err.Error(), "the port must be a number 1-65535") {
				t.Errorf("%s: refused for another reason: %v", c.addr, err)
			}
			continue
		}
		bound := l.Addr().(*net.TCPAddr).Port
		l.Close()
		if c.mustRefuse {
			t.Errorf("%s: ListenMCP bound port %d", c.addr, bound)
		}
		aerr := cfg.Allows(bound, alwaysRefusedPorts(c.addr, nil))
		var r *previewhost.Refusal
		if !errors.As(aerr, &r) || r.Code != previewhost.CodePortNotAllowed || !strings.Contains(r.Why, "MCP endpoint") {
			t.Errorf("%s: the endpoint bound port %d and previews allow it (%v)", c.addr, bound, aerr)
		}
	}
}

// TestAlwaysRefusedPortsAreTheMCPAndOpencodePorts: -mcp-listen's port and the
// opencode token's server port, re-read at each call; neither when unset.
func TestAlwaysRefusedPortsAreTheMCPAndOpencodePorts(t *testing.T) {
	oc, err := opencodehost.EnvCredentials("http://127.0.0.1:4096", "9999999999.sig")
	if err != nil {
		t.Fatal(err)
	}
	got := alwaysRefusedPorts("127.0.0.1:47823", oc)()
	if len(got) != 2 || !strings.Contains(got[47823], "MCP endpoint") || !strings.Contains(got[4096], "opencode") {
		t.Fatalf("refused = %v", got)
	}
	if got := alwaysRefusedPorts("", nil)(); len(got) != 0 {
		t.Fatalf("nothing configured, refused = %v", got)
	}
}

// startPreviewLog runs startPreview with an already-ended context — so it
// validates, logs and returns without dialling — and returns its log.
func startPreviewLog(t *testing.T, pf previewFlags, defaultDaemon bool, refused map[int]string) string {
	t.Helper()
	var buf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := startPreview(ctx, pf, defaultDaemon, func() map[int]string { return refused }, "laptop", nil, log.New(&buf, "", 0), nil)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("startPreview never finished")
	}
	return buf.String()
}

// TestStartPreviewLogsWhatItOffersOrWhyItIsOff: a valid configuration is
// logged with its numbers and endpoints and an always-refused port it lists
// is named; a bad one is logged as OFF with its reason; a panic inside is
// recovered and logged as OFF, never crashing the host.
func TestStartPreviewLogsWhatItOffersOrWhyItIsOff(t *testing.T) {
	file, token := previewFiles(t, `{"ports": ["5173", "40000-50000"], "maxOpen": 4}`)
	pf := previewFlags{On: true, URLs: "ws://192.0.2.7:9000/v1/host", TokenFile: token, File: file}
	out := startPreviewLog(t, pf, true, map[int]string{47823: "muster-mirror's MCP endpoint (-mcp-listen)"})
	for _, want := range []string{
		"previews are ON (ports 5173,40000-50000, maxOpen 4, maxConns 64",
		"dialling ws://192.0.2.7:9000/v1/host",
		"port 47823 is on the allowlist but is muster-mirror's MCP endpoint (-mcp-listen), and is always refused",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q\nlacks %q", out, want)
		}
	}
	if strings.Contains(out, goodPreviewToken) {
		t.Fatalf("the log prints the token: %s", out)
	}
	if out := startPreviewLog(t, previewFlags{On: true, File: filepath.Join(t.TempDir(), "x.json")}, true, nil); !strings.Contains(out, "previews are OFF, mirroring continues:") || !strings.Contains(out, "does not exist") {
		t.Errorf("bad config log: %q", out)
	}
	var buf bytes.Buffer
	done := startPreview(context.Background(), pf, true, func() map[int]string { panic("boom") }, "laptop", nil, log.New(&buf, "", 0), nil)
	<-done
	if !strings.Contains(buf.String(), "previews are OFF, mirroring continues: preview setup panicked: boom") {
		t.Errorf("panic log: %q", buf.String())
	}
}

// countingGateway is a TCP listener that counts connections and closes each:
// a gateway endpoint that is up at the TCP level and answers nothing.
func countingGateway(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	gw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { gw.Close() })
	var accepts atomic.Int32
	go func() {
		for {
			c, err := gw.Accept()
			if err != nil {
				return
			}
			accepts.Add(1)
			c.Close()
		}
	}()
	return "ws://" + gw.Addr().String() + "/v1/host", &accepts
}

// TestPreviewNeverStopsMirroring is -preview through run: with the gateway
// unreachable (its endpoint dialled — the counter shows the host really
// tried — and every dial failing), with a bad previews.json, and with the
// preview goroutine panicking, the host dials muster AFTER the preview code
// has done its worst, and exits 0.
func TestPreviewNeverStopsMirroring(t *testing.T) {
	goodFile, token := previewFiles(t, `{"ports": ["5173"]}`)
	t.Run("gateway unreachable", func(t *testing.T) {
		gwURL, accepts := countingGateway(t)
		rc, errs := runUntil(t, map[string]string{envPreview: "1", envPreviewURLs: gwURL, envPreviewTokenFile: token, envPreviewFile: goodFile},
			func() bool { return accepts.Load() >= 1 })
		if rc != 0 || !strings.Contains(errs, "previews are ON") || !strings.Contains(errs, "no gateway endpoint answered") {
			t.Fatalf("rc %d, log:\n%s", rc, errs)
		}
	})
	t.Run("bad previews.json", func(t *testing.T) {
		gwURL, accepts := countingGateway(t)
		rc, errs := runUntil(t, map[string]string{envPreview: "1", envPreviewURLs: gwURL, envPreviewTokenFile: token, envPreviewFile: filepath.Join(t.TempDir(), "missing.json")}, nil)
		if rc != 0 || !strings.Contains(errs, "previews are OFF, mirroring continues:") {
			t.Fatalf("rc %d, log:\n%s", rc, errs)
		}
		if n := accepts.Load(); n != 0 {
			t.Fatalf("previews are off yet the gateway was dialled %d time(s)", n)
		}
	})
	t.Run("the preview goroutine panics", func(t *testing.T) {
		panicked := make(chan struct{})
		previewStartHook = func() { close(panicked); panic("boom") }
		t.Cleanup(func() { previewStartHook = nil })
		gwURL, _ := countingGateway(t)
		rc, errs := runUntil(t, map[string]string{envPreview: "1", envPreviewURLs: gwURL, envPreviewTokenFile: token, envPreviewFile: goodFile},
			func() bool {
				select {
				case <-panicked:
					return true
				default:
					return false
				}
			})
		if rc != 0 || !strings.Contains(errs, "preview setup panicked: boom") {
			t.Fatalf("rc %d, log:\n%s", rc, errs)
		}
	})
}

// TestPreviewFlagsWithoutPreviewSayTheyDoNothing: the endpoints or token set
// with -preview off is logged as having no effect.
func TestPreviewFlagsWithoutPreviewSayTheyDoNothing(t *testing.T) {
	rc, errs := runUntilDialled(t, map[string]string{envPreviewURLs: "wss://tunnel.example.net/v1/host"})
	if rc != 0 || !strings.Contains(errs, "have no effect without -preview: previews are off") {
		t.Fatalf("rc %d, log:\n%s", rc, errs)
	}
	if strings.Contains(errs, "previews are configured") {
		t.Fatalf("previews configured with -preview off:\n%s", errs)
	}
}

// runUntilDialled runs the daemon against a fake muster with extra env, waits
// for it to dial /mirror/connect, stops it, and returns its status and log.
func runUntilDialled(t *testing.T, extra map[string]string) (int, string) {
	t.Helper()
	return runUntil(t, extra, nil)
}

// runUntil is runUntilDialled that first waits for first() (nil: nothing),
// then for a dial of /mirror/connect made AFTER first() held: proof that
// mirroring goes on after whatever first() waited for.
// backend, when given, replaces the fake tmux (default listBackend).
func runUntil(t *testing.T, extra map[string]string, first func() bool, backend ...mirrorhost.Backend) (int, string) {
	t.Helper()
	var armed atomic.Bool
	armed.Store(first == nil)
	dialled := make(chan struct{}, 1)
	muster := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mirror/connect" && armed.Load() {
			select {
			case dialled <- struct{}{}:
			default:
			}
		}
		// 503, not 401: an unauthorized host waits a minute to redial, and
		// this needs the redial that proves mirroring went on.
		http.Error(w, "no", http.StatusServiceUnavailable)
	}))
	defer muster.Close()
	prevB, prevC := newTmuxBackend, runContext
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { newTmuxBackend, runContext = prevB, prevC; cancel() })
	var b mirrorhost.Backend = listBackend{}
	if len(backend) > 0 {
		b = backend[0]
	}
	newTmuxBackend = func(string, string, string) mirrorhost.Backend { return b }
	runContext = func() (context.Context, context.CancelFunc) { return ctx, cancel }
	env := map[string]string{envURL: muster.URL, envToken: "mmt_" + strings.Repeat("A", 43), envHost: "laptop", envAllowHTTP: "1"}
	for k, v := range extra {
		env[k] = v
	}
	type result struct {
		rc   int
		errs string
	}
	done := make(chan result, 1)
	go func() {
		rc, _, errs := runWith(t, env)
		done <- result{rc, errs}
	}()
	if first != nil {
		deadline := time.Now().Add(15 * time.Second)
		for !first() {
			select {
			case r := <-done:
				t.Fatalf("run ended early: rc %d %s", r.rc, r.errs)
			case <-time.After(10 * time.Millisecond):
			}
			if time.Now().After(deadline) {
				cancel()
				t.Fatal("the awaited preview event never happened")
			}
		}
		armed.Store(true)
	}
	select {
	case <-dialled:
	case r := <-done:
		t.Fatalf("run ended before dialling muster: rc %d %s", r.rc, r.errs)
	case <-time.After(20 * time.Second):
		cancel()
		t.Fatal("the host never dialled muster")
	}
	cancel()
	select {
	case r := <-done:
		return r.rc, r.errs
	case <-time.After(10 * time.Second):
		t.Fatal("run did not stop")
	}
	return 0, ""
}

// windowsBackend is listBackend that also lists windows: two, with pane roots
// 100 (window @3) and 300 (window @5).
type windowsBackend struct{ listBackend }

func (windowsBackend) Windows(context.Context) ([]mirrorhost.WindowInfo, error) {
	return []mirrorhost.WindowInfo{
		{Session: "work", ID: "@3", Index: 1, Pane: "%1", PanePID: 100},
		{Session: "docs", ID: "@5", Index: 0, Pane: "%2", PanePID: 300},
	}, nil
}

// A synthetic /proc/net/tcp (format of a Linux 6.x host, values invented):
// 5173 and 8000 and 47823 listen on 127.0.0.1 as uid <uid>, 8001 as uid 4242.
func previewTCP(uid int) string {
	row := func(n int, port int, u int, inode int) string {
		return fmt.Sprintf("   %d: 0100007F:%04X 00000000:0000 0A 00000000:00000000 00:00000000 00000000 %5d        0 %d 1 0000000000000000 100 0 0 10 0\n", n, port, u, inode)
	}
	return "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		row(0, 5173, uid, 61001) + row(1, 8000, uid, 61002) + row(2, 47823, uid, 61003) + row(3, 8001, 4242, 61004)
}

// TestListPreviewsReadsTheProcSeamEndToEnd is `-list -previews` on a fixture
// proc tree: the listener walk goes through mirrorhost.ProcTable (stat files,
// the locator's own reader) from the socket's fd up to a window's pane root.
// node (5173, in @3) and hugo (8000, in @5) are printed; the -mcp-listen port
// (47823, listed) and another user's 8001 (listed) are not.
func TestListPreviewsReadsTheProcSeamEndToEnd(t *testing.T) {
	root := t.TempDir()
	mk := func(pid, ppid int, comm string, inodes ...int) {
		dir := filepath.Join(root, fmt.Sprint(pid), "fd")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		stat := fmt.Sprintf("%d (%s) S %d 1 1 0 -1 4194560 0 0 0 0 0 0 0 0 20 0 1 0 100 0 0", pid, comm, ppid)
		if err := os.WriteFile(filepath.Join(root, fmt.Sprint(pid), "stat"), []byte(stat), 0o644); err != nil {
			t.Fatal(err)
		}
		for i, ino := range inodes {
			if err := os.Symlink(fmt.Sprintf("socket:[%d]", ino), filepath.Join(dir, fmt.Sprint(3+i))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "net", "tcp"), []byte(previewTCP(os.Getuid())), 0o644); err != nil {
		t.Fatal(err)
	}
	mk(100, 1, "zsh")
	mk(101, 100, "node", 61001)
	mk(102, 100, "muster-mirror", 61003)
	mk(103, 100, "intruder", 61004)
	mk(300, 1, "bash")
	mk(301, 300, "sh")
	mk(302, 301, "hugo", 61002)
	file, _ := previewFiles(t, `{"ports": ["5173", "8000-8001", "47823"]}`)

	prevB, prevRoot := e2eBackend, previewProcRoot
	t.Cleanup(func() { e2eBackend, previewProcRoot = prevB, prevRoot })
	e2eBackend = func() mirrorhost.Backend { return windowsBackend{} }
	previewProcRoot = root
	rc, out, errs := runWith(t, map[string]string{envPreviewFile: file, envMCPListen: "127.0.0.1:47823"}, "-list", "-previews")
	if previewhost.Supported() != nil {
		// macOS: reports are off, and the command says so (exit 1).
		if rc != 1 || out != "" || !strings.Contains(errs, "UNVERIFIED on") {
			t.Fatalf("off-Linux: rc %d, out %q, errs %q", rc, out, errs)
		}
		return
	}
	if want := "@3\t5173\tnode\twork\n@5\t8000\thugo\tdocs\n"; rc != 0 || out != want {
		t.Fatalf("rc %d, stdout %q (stderr %q); want %q", rc, out, errs, want)
	}
	// Control: without -mcp-listen, 47823 is just another listed port.
	rc, out, _ = runWith(t, map[string]string{envPreviewFile: file}, "-list", "-previews")
	if rc != 0 || !strings.Contains(out, "@3\t47823\tmuster-mirror\twork\n") {
		t.Fatalf("control without -mcp-listen: rc %d, stdout %q", rc, out)
	}
	// A bad previews.json: off, with the reason, exit 1.
	rc, _, errs = runWith(t, map[string]string{envPreviewFile: filepath.Join(root, "none.json")}, "-list", "-previews")
	if rc != 1 || !strings.Contains(errs, "previews are off:") {
		t.Fatalf("bad file: rc %d, %q", rc, errs)
	}
}

// TestThePreviewToolsAreMountedOnlyWhenPreviewsAreOn: startPreview hands
// the Host to its mount once the whole setup validated, and never when
// previews are off (a missing previews.json, a per-tenant daemon); the
// mount serves the three preview tools at /preview-mcp on the MCP endpoint,
// which is 404 before it.
func TestThePreviewToolsAreMountedOnlyWhenPreviewsAreOn(t *testing.T) {
	file, token := previewFiles(t, `{"ports": ["5173"]}`)
	good := previewFlags{On: true, URLs: "ws://192.0.2.7:9000/v1/host", TokenFile: token, File: file}
	run := func(pf previewFlags, defaultDaemon bool) int {
		var mounted atomic.Int32
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		<-startPreview(ctx, pf, defaultDaemon, func() map[int]string { return nil }, "laptop", nil, log.New(&bytes.Buffer{}, "", 0),
			func(*previewhost.Host) { mounted.Add(1) })
		return int(mounted.Load())
	}
	if n := run(good, true); n != 1 {
		t.Fatalf("previews on: mounted %d time(s); want 1", n)
	}
	missing := good
	missing.File = filepath.Join(t.TempDir(), "none.json")
	if n := run(missing, true); n != 0 {
		t.Fatalf("no previews.json: mounted %d time(s)", n)
	}
	if n := run(good, false); n != 0 {
		t.Fatalf("a per-tenant daemon: mounted %d time(s)", n)
	}

	if previewMCPMount(nil, log.New(&bytes.Buffer{}, "", 0)) != nil {
		t.Fatal("a mount with no MCP endpoint")
	}
	proxy := &mirrorhost.MCPProxy{Secret: strings.Repeat("s", 40)}
	list := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, mirrorhost.PreviewMCPPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		r.Header.Set("Authorization", "Bearer "+proxy.Secret)
		w := httptest.NewRecorder()
		proxy.ServeHTTP(w, r)
		return w
	}
	if w := list(); w.Code != http.StatusNotFound {
		t.Fatalf("before the mount: %d", w.Code)
	}
	var lg bytes.Buffer
	previewMCPMount(proxy, log.New(&lg, "", 0))(previewhost.NewHost(previewhost.HostConfig{Logger: log.New(&bytes.Buffer{}, "", 0)}))
	w := list()
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"preview_open"`) || !strings.Contains(w.Body.String(), `"preview_close"`) {
		t.Fatalf("after the mount: %d %s", w.Code, w.Body)
	}
	if !strings.Contains(lg.String(), "preview tools are served at /preview-mcp") {
		t.Fatalf("mount log %q", lg.String())
	}
}
