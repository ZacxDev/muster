package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/ZacxDev/muster/internal/opencodehost"
	"github.com/ZacxDev/muster/internal/opencodehost/opencodetest"
)

// SEVERAL MUSTER SERVERS ON ONE HOST (docs/design/multi-tenant.md, phase 2):
// one muster-mirror per server, one tmux server each. Every daemon records
// the tmux server it mirrors, and `muster-mirror ask` sends a question to the
// server whose daemon recorded the caller's tmux server — and to no other.
//
// These run the REAL daemon (run) and the REAL hook (runAsk) against counting
// fake muster servers. The $TMUX values are spelled literally, the way tmux
// spells them (realpath(TMUX_TMPDIR)/tmux-<uid>/<name>, measured on tmux 3.7c),
// never through the code under test; tenants_tmux_integration_test.go checks
// the same against real tmux servers.

// lockedBuf is a buffer a daemon's logger writes while the test reads it.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// tenantServer is a fake muster: it counts the questions it is sent, and the
// token each came with, and refuses the tunnel.
type tenantServer struct {
	*httptest.Server
	asks   atomic.Int32
	mu     sync.Mutex
	tokens []string
}

func newTenantServer(t *testing.T) *tenantServer {
	t.Helper()
	ts := &tenantServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mirror/ask", func(w http.ResponseWriter, r *http.Request) {
		ts.asks.Add(1)
		ts.mu.Lock()
		ts.tokens = append(ts.tokens, r.Header.Get("Authorization"))
		ts.mu.Unlock()
		var req struct {
			ID string `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": req.ID, "state": "pending"})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusUnauthorized) })
	ts.Server = httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func (ts *tenantServer) lastToken() string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if len(ts.tokens) == 0 {
		return ""
	}
	return ts.tokens[len(ts.tokens)-1]
}

// tenantHost is a scratch host: its HOME, and a private TMUX_TMPDIR (real
// is that directory resolved, as tmux reports it).
type tenantHost struct {
	t       *testing.T
	homeDir string
	tmpdir  string
	real    string
}

func newTenantHost(t *testing.T) *tenantHost {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	tmp := t.TempDir()
	real, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatal(err)
	}
	return &tenantHost{t: t, homeDir: home, tmpdir: tmp, real: real}
}

// socket is the socket path of tmux server name under this host's
// TMUX_TMPDIR, spelled literally.
func (h *tenantHost) socket(name string) string {
	return filepath.Join(h.real, fmt.Sprintf("tmux-%d", os.Getuid()), name)
}

// placeSocket stands a file in for server name's socket (the old-record
// fallback resolves it, so it must exist; a real one is in the tmuxit test).
func (h *tenantHost) placeSocket(name string) {
	h.t.Helper()
	p := h.socket(name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *tenantHost) legacy() string { return filepath.Join(h.homeDir, ".muster", "mirror-hook.json") }
func (h *tenantHost) tenantRec(n string) string {
	return filepath.Join(h.homeDir, ".muster", "mirror-hook.d", n+".json")
}

// token writes a mirror token file for tenant n and returns its path.
func (h *tenantHost) token(n string) string {
	h.t.Helper()
	p := filepath.Join(h.homeDir, ".config", "muster-mirror", n, "token")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(mirrorToken(n)+"\n"), 0o600); err != nil {
		h.t.Fatal(err)
	}
	return p
}

// mirrorToken is a well-formed, distinct, synthetic mirror token for n.
func mirrorToken(n string) string {
	s := "mmt_" + strings.Repeat(strings.ToUpper(n[:1]), 43-len(n)) + n
	return s[:47]
}

// defaultEnv is today's muster-mirror.service: tenant default, the default
// tmux server, the legacy record.
func (h *tenantHost) defaultEnv(srv *tenantServer) map[string]string {
	return map[string]string{envURL: srv.URL, envAllowHTTP: "1", envTokenFile: h.token("default"), envHost: "laptop",
		envWriteHookConfig: "1", "TMUX_TMPDIR": h.tmpdir}
}

// unitEnv is what muster-mirror@<n>.service's Environment= lines set (INSTALL.md).
func (h *tenantHost) unitEnv(n string) map[string]string {
	return map[string]string{envHookConfig: h.tenantRec(n), envWriteHookConfig: "1", envOpenCodeReadOnly: "1", "TMUX_TMPDIR": h.tmpdir}
}

// tenantEnv is muster-mirror@<n>: the unit's settings plus a FRESH env file
// naming its own server, token file and tmux server (-L n).
func (h *tenantHost) tenantEnv(n string, srv *tenantServer) map[string]string {
	return overlay(h.unitEnv(n), map[string]string{envURL: srv.URL, envAllowHTTP: "1", envTokenFile: h.token(n), envHost: "laptop", envSocket: n})
}

// overlay is base with the env file's values over it: EnvironmentFile=
// overrides Environment= (systemd.exec; measured on systemd 261).
func overlay(base, file map[string]string) map[string]string {
	m := map[string]string{}
	for k, v := range base {
		m[k] = v
	}
	for k, v := range file {
		m[k] = v
	}
	return m
}

// runDaemons starts each daemon (a list backend: no tmux needed to record),
// waits until each has recorded or refused to, stops them all, and returns
// each one's log.
func runDaemons(t *testing.T, envs ...map[string]string) []string {
	t.Helper()
	until := make([]string, len(envs))
	for i := range until {
		until[i] = "record"
	}
	return runDaemonsUntil(t, 0, until, envs...)
}

// runDaemonsUntil is runDaemons waiting until daemon i's log holds until[i],
// then a further grace, before stopping them.
func runDaemonsUntil(t *testing.T, grace time.Duration, until []string, envs ...map[string]string) []string {
	t.Helper()
	prevB, prevC := newTmuxBackend, runContext
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	newTmuxBackend = func(string, string, string, *mirrorhost.HermesSource) mirrorhost.Backend { return listBackend{} }
	runContext = func() (context.Context, context.CancelFunc) { return ctx, cancel }
	defer func() { newTmuxBackend, runContext = prevB, prevC }()
	logs := make([]*lockedBuf, len(envs))
	var wg sync.WaitGroup
	for i, e := range envs {
		logs[i] = &lockedBuf{}
		wg.Add(1)
		go func(e map[string]string, l *lockedBuf) {
			defer wg.Done()
			run(nil, func(k string) string { return e[k] }, &bytes.Buffer{}, l)
		}(e, logs[i])
	}
	deadline := time.Now().Add(10 * time.Second)
	for i := range envs {
		for !strings.Contains(logs[i].String(), until[i]) {
			if time.Now().After(deadline) {
				cancel()
				wg.Wait()
				t.Fatalf("daemon %d never logged %q: %s", i, until[i], logs[i].String())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	time.Sleep(grace)
	cancel()
	wg.Wait()
	out := make([]string, len(logs))
	for i, l := range logs {
		out[i] = l.String()
	}
	return out
}

// ask is `muster-mirror ask` run in a pane of tmux server name (its $TMUX),
// with extra hook environment. It returns the hook's stderr.
func (h *tenantHost) ask(t *testing.T, tmuxSocket string, extra map[string]string) string {
	t.Helper()
	e := overlay(map[string]string{"TMUX": tmuxSocket + ",4242,0", "TMUX_TMPDIR": h.tmpdir}, extra)
	var out, errb bytes.Buffer
	if code := runAsk(nil, env(e), bytes.NewReader(askPayload(t)), &out, &errb); code != 0 || out.Len() != 0 {
		t.Fatalf("the hook exited %d and printed %q", code, out.String())
	}
	return errb.String()
}

func counts(t *testing.T, a, b *tenantServer, wantA, wantB int32, what string) {
	t.Helper()
	if a.asks.Load() != wantA || b.asks.Load() != wantB {
		t.Fatalf("%s: server A has %d question(s), server B %d; want %d and %d", what, a.asks.Load(), b.asks.Load(), wantA, wantB)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestTwoTenantsEachQuestionLandsOnItsOwnServer: two daemons, two muster
// servers, two tmux servers. A question from each tmux server reaches its own
// server and not the other (both counted); one from a third tmux server, with
// no record, reaches neither.
func TestTwoTenantsEachQuestionLandsOnItsOwnServer(t *testing.T) {
	h := newTenantHost(t)
	a, b := newTenantServer(t), newTenantServer(t)
	logs := runDaemons(t, h.defaultEnv(a), h.tenantEnv("work", b))
	ra, err := readHookConfig(h.legacy())
	if err != nil || ra.TmuxSocket != h.socket("default") || ra.URL != a.URL {
		t.Fatalf("the legacy record %+v (%v), want tmux server %s; log %s", ra, err, h.socket("default"), logs[0])
	}
	rb, err := readHookConfig(h.tenantRec("work"))
	if err != nil || rb.TmuxSocket != h.socket("work") || rb.URL != b.URL || rb.TokenFile == "" {
		t.Fatalf("work's record %+v (%v), want tmux server %s; log %s", rb, err, h.socket("work"), logs[1])
	}

	h.ask(t, h.socket("default"), nil)
	counts(t, a, b, 1, 0, "a question from the default tmux server")
	if a.lastToken() != "Bearer "+mirrorToken("default") {
		t.Fatalf("server A got token %q", a.lastToken())
	}
	h.ask(t, h.socket("work"), nil)
	counts(t, a, b, 1, 1, "a question from tmux server work")
	if b.lastToken() != "Bearer "+mirrorToken("work") {
		t.Fatalf("server B got token %q", b.lastToken())
	}
	// A third tmux server: no daemon recorded it, so it fails open.
	errs := h.ask(t, h.socket("third"), nil)
	counts(t, a, b, 1, 1, "a question from tmux server third")
	if !strings.Contains(errs, "no muster-mirror recorded tmux server "+h.socket("third")) {
		t.Fatalf("the hook did not say why: %q", errs)
	}
	// Outside tmux altogether: fails open too.
	var out, errb bytes.Buffer
	runAsk(nil, env(map[string]string{"TMUX_TMPDIR": h.tmpdir}), bytes.NewReader(askPayload(t)), &out, &errb)
	counts(t, a, b, 1, 1, "a question from outside tmux")
}

// TestTheLegacyRecordIsUntouchedByAMisconfiguredTenant: once tenant default's
// daemon has recorded its socket, a template instance whose env file was
// COPIED from today's leaves the legacy record byte for byte as it was, and so
// does one whose env file points MUSTER_MIRROR_HOOK_CONFIG at the legacy path
// itself (refused: the legacy record names another tmux server). Questions
// from the default server still reach tenant default.
func TestTheLegacyRecordIsUntouchedByAMisconfiguredTenant(t *testing.T) {
	h := newTenantHost(t)
	a, b := newTenantServer(t), newTenantServer(t)
	runDaemons(t, h.defaultEnv(a))
	before := mustRead(t, h.legacy())

	// A copied env file: today's values (its URL, its token file, its
	// MUSTER_MIRROR_WRITE_HOOK_CONFIG=1) over the unit's.
	copied := overlay(h.unitEnv("work"), map[string]string{envURL: a.URL, envAllowHTTP: "1", envTokenFile: h.token("default"), envHost: "laptop", envWriteHookConfig: "1"})
	runDaemons(t, copied)
	if !bytes.Equal(mustRead(t, h.legacy()), before) {
		t.Fatalf("a copied env file changed the legacy record:\n%s\nwas\n%s", mustRead(t, h.legacy()), before)
	}

	// An env file that also repoints the record at the legacy path.
	repointed := overlay(h.tenantEnv("work", b), map[string]string{envHookConfig: h.legacy()})
	logs := runDaemons(t, repointed)
	if !bytes.Equal(mustRead(t, h.legacy()), before) {
		t.Fatalf("an env file pointing at the legacy path overwrote it:\n%s\nwas\n%s", mustRead(t, h.legacy()), before)
	}
	if !strings.Contains(logs[0], "not overwriting "+h.legacy()) || !strings.Contains(logs[0], h.socket("default")) {
		t.Fatalf("the refusal was not logged: %s", logs[0])
	}
	// The copied instance wrote work.json naming the DEFAULT server (it
	// mirrors it), so the default server now has two records and fails open:
	// remove it, and default's questions reach default.
	if err := os.Remove(h.tenantRec("work")); err != nil {
		t.Fatal(err)
	}
	h.ask(t, h.socket("default"), nil)
	counts(t, a, b, 1, 0, "a question from the default server")
}

// TestASymlinkedTMUX_TMPDIR: with TMUX_TMPDIR a symlink to a real directory,
// tmux reports (and puts in $TMUX) the RESOLVED directory. A question from the
// default server is matched by a new legacy record (recorded socket) and by
// an old one with no socket (the EvalSymlinks fallback) — the positive control
// for macOS, where /tmp is a symlink to /private/tmp.
func TestASymlinkedTMUX_TMPDIR(t *testing.T) {
	h := newTenantHost(t)
	link := filepath.Join(t.TempDir(), "tmux-link")
	if err := os.Symlink(h.real, link); err != nil {
		t.Fatal(err)
	}
	h.tmpdir = link // the daemon and the hook see the symlink; tmux reports h.real
	a, b := newTenantServer(t), newTenantServer(t)

	runDaemons(t, h.defaultEnv(a))
	if rec, _ := readHookConfig(h.legacy()); rec.TmuxSocket != h.socket("default") {
		t.Fatalf("recorded %q, want the resolved %q", rec.TmuxSocket, h.socket("default"))
	}
	h.ask(t, h.socket("default"), nil)
	counts(t, a, b, 1, 0, "new legacy record, a question from the default server")

	// An old daemon's record: no socket.
	if err := writeHookConfig(h.legacy(), hookConfig{URL: a.URL, AllowHTTP: true, TokenFile: h.token("default")}); err != nil {
		t.Fatal(err)
	}
	h.placeSocket("default")
	h.placeSocket("work")
	h.ask(t, h.socket("default"), nil)
	counts(t, a, b, 2, 0, "old legacy record, $TMUX resolved")
	// The same socket spelled through the symlink still matches.
	h.ask(t, filepath.Join(link, fmt.Sprintf("tmux-%d", os.Getuid()), "default"), nil)
	counts(t, a, b, 3, 0, "old legacy record, $TMUX through the symlink")
	// The negative control: another server never matches an old record.
	h.ask(t, h.socket("work"), nil)
	counts(t, a, b, 3, 0, "old legacy record, a question from tmux server work")
}

// TestASingleDaemonOnATmuxSocketKeepsWorking: a host with one daemon on
// -tmux-socket and an old legacy record (no socket): the upgraded daemon
// rewrites it with its socket, and its own sessions' questions reach it.
func TestASingleDaemonOnATmuxSocketKeepsWorking(t *testing.T) {
	h := newTenantHost(t)
	a, b := newTenantServer(t), newTenantServer(t)
	if err := writeHookConfig(h.legacy(), hookConfig{URL: b.URL, AllowHTTP: true}); err != nil {
		t.Fatal(err)
	}
	runDaemons(t, overlay(h.defaultEnv(a), map[string]string{envSocket: "solo"}))
	if rec, _ := readHookConfig(h.legacy()); rec.TmuxSocket != h.socket("solo") || rec.URL != a.URL {
		t.Fatalf("the old legacy record was not rewritten: %+v", rec)
	}
	h.ask(t, h.socket("solo"), nil)
	counts(t, a, b, 1, 0, "a question from the daemon's own tmux server")
	h.placeSocket("default")
	h.ask(t, h.socket("default"), nil)
	counts(t, a, b, 1, 0, "a question from the default server, which this daemon does not mirror")
}

// TestTwoDaemonsOnOneTmuxServerFailOpen: two daemons mirroring the same tmux
// server record the same socket; the hook cannot tell which server a question
// is for, and sends it to neither.
func TestTwoDaemonsOnOneTmuxServerFailOpen(t *testing.T) {
	h := newTenantHost(t)
	a, b := newTenantServer(t), newTenantServer(t)
	both := h.tenantEnv("work", b)
	delete(both, envSocket) // work's daemon on the DEFAULT tmux server too
	runDaemons(t, h.defaultEnv(a), both)
	errs := h.ask(t, h.socket("default"), nil)
	counts(t, a, b, 0, 0, "a question from a tmux server two daemons mirror")
	if !strings.Contains(errs, "2 records name tmux server") {
		t.Fatalf("the hook did not say why: %q", errs)
	}
}

// TestAPerTenantDaemonWithOnlyATokenValueWritesNoRecord: its token came from
// MUSTER_MIRROR_TOKEN, so a record could name no token file and the hook
// would fall back to the default one (another tenant's): it writes none.
func TestAPerTenantDaemonWithOnlyATokenValueWritesNoRecord(t *testing.T) {
	h := newTenantHost(t)
	b := newTenantServer(t)
	e := h.tenantEnv("work", b)
	delete(e, envTokenFile)
	e[envToken] = mirrorToken("work")
	logs := runDaemons(t, e)
	if _, err := os.Stat(h.tenantRec("work")); !os.IsNotExist(err) {
		t.Fatalf("a record was written (stat: %v)", err)
	}
	if !strings.Contains(logs[0], "not recording "+h.tenantRec("work")) || !strings.Contains(logs[0], envTokenFile) {
		t.Fatalf("the refusal was not logged: %s", logs[0])
	}
}

// TestAPerTenantRecordWithoutATokenFileNeverUsesTheDefaultOne: a per-tenant
// record naming no token file (no daemon writes one; a hand-written one) fails
// open — the default token file, tenant default's credential, is never sent
// to its URL. The control: the same record naming its token file is used.
func TestAPerTenantRecordWithoutATokenFileNeverUsesTheDefaultOne(t *testing.T) {
	h := newTenantHost(t)
	a, b := newTenantServer(t), newTenantServer(t)
	def := filepath.Join(h.homeDir, ".muster", "mirror-token")
	if err := os.MkdirAll(filepath.Dir(def), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(def, []byte("mmt_default_tenants_credential\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeHookConfig(h.tenantRec("work"), hookConfig{URL: b.URL, AllowHTTP: true, TmuxSocket: h.socket("work")}); err != nil {
		t.Fatal(err)
	}
	errs := h.ask(t, h.socket("work"), nil)
	counts(t, a, b, 0, 0, "a per-tenant record without a token file")
	if !strings.Contains(errs, "never uses "+DefaultTokenFile) {
		t.Fatalf("the hook did not say why: %q", errs)
	}
	if err := writeHookConfig(h.tenantRec("work"), hookConfig{URL: b.URL, AllowHTTP: true, TmuxSocket: h.socket("work"), TokenFile: h.token("work")}); err != nil {
		t.Fatal(err)
	}
	h.ask(t, h.socket("work"), nil)
	counts(t, a, b, 0, 1, "the control: the record names its token file")
	if b.lastToken() != "Bearer "+mirrorToken("work") {
		t.Fatalf("sent %q", b.lastToken())
	}
}

// TestAPerTenantRecordWithoutASocketNeverMatches: only the LEGACY record gets
// the old-record fallback to the default tmux server. A per-tenant record with
// no socket (no daemon writes one) matches nothing, even from the default
// server. The control: the legacy record without a socket does match there.
func TestAPerTenantRecordWithoutASocketNeverMatches(t *testing.T) {
	h := newTenantHost(t)
	a, b := newTenantServer(t), newTenantServer(t)
	h.placeSocket("default")
	if err := writeHookConfig(h.tenantRec("work"), hookConfig{URL: b.URL, AllowHTTP: true, TokenFile: h.token("work")}); err != nil {
		t.Fatal(err)
	}
	h.ask(t, h.socket("default"), nil)
	counts(t, a, b, 0, 0, "a socket-less per-tenant record, a question from the default server")
	if err := writeHookConfig(h.legacy(), hookConfig{URL: a.URL, AllowHTTP: true, TokenFile: h.token("default")}); err != nil {
		t.Fatal(err)
	}
	h.ask(t, h.socket("default"), nil)
	counts(t, a, b, 1, 0, "the control: a socket-less legacy record")
}

// TestASymlinkedSpellingOfTheLegacyRecordIsStillRefused: an env file that
// names the legacy record through a symlinked directory still reaches the
// refusal (not the per-tenant path, which would overwrite it). The control:
// the per-tenant path through the same symlink is written.
func TestASymlinkedSpellingOfTheLegacyRecordIsStillRefused(t *testing.T) {
	h := newTenantHost(t)
	a, b := newTenantServer(t), newTenantServer(t)
	runDaemons(t, h.defaultEnv(a))
	before := mustRead(t, h.legacy())
	alias := filepath.Join(t.TempDir(), "muster-alias")
	if err := os.Symlink(filepath.Join(h.homeDir, ".muster"), alias); err != nil {
		t.Fatal(err)
	}
	logs := runDaemons(t, overlay(h.tenantEnv("work", b), map[string]string{envHookConfig: filepath.Join(alias, "mirror-hook.json")}))
	if !bytes.Equal(mustRead(t, h.legacy()), before) || !strings.Contains(logs[0], "not overwriting") {
		t.Fatalf("the legacy record, named through a symlink, was overwritten; log %s", logs[0])
	}
	runDaemons(t, overlay(h.tenantEnv("work", b), map[string]string{envHookConfig: filepath.Join(alias, "mirror-hook.d", "work.json")}))
	if rec, err := readHookConfig(h.tenantRec("work")); err != nil || rec.URL != b.URL {
		t.Fatalf("the control: the per-tenant record through the symlink: %+v %v", rec, err)
	}
}

// inDefaultTmux is the hook environment of a pane in the DEFAULT tmux server
// under a private TMUX_TMPDIR (a file stands in for its socket).
func inDefaultTmux(t *testing.T) map[string]string {
	t.Helper()
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(real, fmt.Sprintf("tmux-%d", os.Getuid()), "default")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return map[string]string{"TMUX_TMPDIR": dir, "TMUX": p + ",4242,0"}
}

// TestTheDerivedSocketPath pins the derivation's rule, literal expected values:
// -S as given (even relative), -L and the default under realpath(TMUX_TMPDIR),
// /tmp when TMUX_TMPDIR is unset or does not exist (tmux's own fallback,
// measured on tmux 3.7c).
func TestTheDerivedSocketPath(t *testing.T) {
	dir := t.TempDir()
	real, _ := filepath.EvalSymlinks(dir)
	tmp, _ := filepath.EvalSymlinks("/tmp")
	uid := fmt.Sprintf("tmux-%d", 4321)
	for name, c := range map[string]struct {
		name, path, tmpdir, want string
	}{
		"-S absolute":         {"", "/run/x/s.sock", dir, "/run/x/s.sock"},
		"-S relative":         {"ignored", "rel.sock", dir, "rel.sock"},
		"-L":                  {"work", "", dir, filepath.Join(real, uid, "work")},
		"default":             {"", "", dir, filepath.Join(real, uid, "default")},
		"no TMUX_TMPDIR":      {"", "", "", filepath.Join(tmp, uid, "default")},
		"missing TMUX_TMPDIR": {"work", "", filepath.Join(dir, "nope"), filepath.Join(tmp, uid, "work")},
	} {
		got, err := tmuxSocketPath(c.name, c.path, env(map[string]string{"TMUX_TMPDIR": c.tmpdir}), 4321)
		if err != nil || got != c.want {
			t.Errorf("%s: %q %v, want %q", name, got, err, c.want)
		}
	}
	if _, err := tmuxSocketPath("a/b", "", env(nil), 1); err == nil {
		t.Error("a socket name with a / was accepted")
	}
}

// TestOnlyTenantDefaultRenewsTheOpenCodeToken: the opencode token file is
// shared by every daemon on the host; a template instance (the unit sets
// MUSTER_MIRROR_OPENCODE_READ_ONLY) reads it and never renews it, so exactly
// one daemon — today's unit, tenant default — mints.
func TestOnlyTenantDefaultRenewsTheOpenCodeToken(t *testing.T) {
	h := newTenantHost(t)
	a, b := newTenantServer(t), newTenantServer(t)
	oc := opencodetest.New("fixture-opencode-password-0123456789")
	defer oc.Close()
	tokFile := filepath.Join(h.homeDir, ".muster", "opencode-token")
	// Due for renewal: two days left (renewal starts RenewBefore ahead).
	if err := opencodehost.WriteTokenFile(tokFile, opencodehost.TokenFile{URL: oc.URL, Token: oc.IssueAt(time.Now().Add(48 * time.Hour))}); err != nil {
		t.Fatal(err)
	}
	pairs := func() int {
		n := 0
		for _, r := range oc.Requests() {
			if r.Path == "/api/pair" {
				n++
			}
		}
		return n
	}
	reader := overlay(h.tenantEnv("work", b), map[string]string{envOpenCodeTokenFile: tokFile})
	renewer := overlay(h.defaultEnv(a), map[string]string{envOpenCodeTokenFile: tokFile})

	// The template instance alone, given time to renew if it were going to.
	logs := runDaemonsUntil(t, 500*time.Millisecond, []string{"READ-ONLY"}, reader)
	if n := pairs(); n != 0 {
		t.Fatalf("a read-only daemon made %d /api/pair request(s), want 0; log %s", n, logs[0])
	}
	// Both together, as on a host: exactly one mint, by tenant default.
	logs = runDaemonsUntil(t, 200*time.Millisecond, []string{"READ-ONLY", "renewed the opencode token"}, reader, renewer)
	if n := pairs(); n != 1 {
		t.Fatalf("%d /api/pair request(s) with a reader and a renewer, want exactly 1", n)
	}
	if strings.Contains(logs[0], "renewed the opencode token") {
		t.Fatalf("the read-only daemon renewed: %s", logs[0])
	}
}

// TestOneServerDownTheOtherKeepsMirroring: tenant default's server is gone;
// tenant work's daemon keeps redialling its own server on its own backoff,
// and work's questions still reach work.
func TestOneServerDownTheOtherKeepsMirroring(t *testing.T) {
	h := newTenantHost(t)
	a := newTenantServer(t)
	a.Close() // tenant default's server is down
	var dials atomic.Int32
	b := &tenantServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mirror/ask", func(w http.ResponseWriter, r *http.Request) {
		b.asks.Add(1)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"state":"pending"}`))
	})
	mux.HandleFunc("/mirror/connect", func(w http.ResponseWriter, r *http.Request) {
		dials.Add(1)
		http.Error(w, "busy", http.StatusServiceUnavailable) // a transient refusal: retried on the short backoff
	})
	b.Server = httptest.NewServer(mux)
	defer b.Close()

	prevB, prevC := newTmuxBackend, runContext
	ctx, cancel := context.WithCancel(context.Background())
	newTmuxBackend = func(string, string, string, *mirrorhost.HermesSource) mirrorhost.Backend { return listBackend{} }
	runContext = func() (context.Context, context.CancelFunc) { return ctx, cancel }
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait(); newTmuxBackend, runContext = prevB, prevC }()
	for _, e := range []map[string]string{h.defaultEnv(a), h.tenantEnv("work", b)} {
		wg.Add(1)
		go func(e map[string]string) {
			defer wg.Done()
			run(nil, func(k string) string { return e[k] }, &bytes.Buffer{}, &lockedBuf{})
		}(e)
	}
	deadline := time.Now().Add(10 * time.Second)
	for dials.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := dials.Load(); n < 3 {
		t.Fatalf("work's daemon dialled its server %d time(s) in 10s while default's server was down, want at least 3", n)
	}
	h.ask(t, h.socket("work"), nil)
	if b.asks.Load() != 1 {
		t.Fatalf("work's question: server B has %d", b.asks.Load())
	}
}
