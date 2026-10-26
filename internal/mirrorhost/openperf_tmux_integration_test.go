//go:build tmuxit

package mirrorhost

import (
	"bytes"
	"compress/flate"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/telemetry"
)

// OPENING A RENDERED VIEW ON A REAL tmux SERVER, MEASURED (the host leg of a
// session open): a private server of many windows, each running a process
// that IS Claude Code by the locator's rule (argv[0] "claude"), with a
// synthetic sessions file and transcript each, and one window whose
// transcript is several MiB. A real muster-mirror connects to a fake muster
// that asks it to watch that window, again and again, and the host's own
// spans say what each open cost. tmux is run through a wrapper that logs every
// invocation, so a test can count what an open EXECUTED.

// perfRig is the measured host.
type perfRig struct {
	t         testing.TB
	sock      string
	claudeDir string
	tmuxLog   string
	backend   Tmux
	// windows is every (session, window id) the rig made, sorted; target is
	// the one perfTranscripts gives the big transcript.
	windows []windowKey
	target  windowKey
	panes   map[windowKey]paneProc
	// byName and names are each window's rig name (<session><n>, in creation
	// order) and back.
	byName map[string]windowKey
	names  map[windowKey]string
}

// paneProc is one window's active pane and the "claude" in it.
type paneProc struct {
	pane      string
	root, cli int
}

const (
	perfSessions   = 4
	perfPerSession = 10
	perfBig        = 6 << 20
)

// perfCLI is a pane's command: its shell runs the "CLI" as its child, the
// way a shell runs claude — root (bash) -> claude. (bash as the CLI: NixOS's
// coreutils is one binary that dispatches on argv[0], so `exec -a claude
// sleep` is not a process at all.)
const perfCLI = "bash -c '(exec -a claude bash -c \"sleep infinity; :\") & wait'"

// newPerfRig is the measured rig: perfSessions sessions of perfPerSession
// windows, the middle one with a perfBig transcript, the rest small.
func newPerfRig(t testing.TB) *perfRig {
	t.Helper()
	counts := map[string]int{}
	for s := 0; s < perfSessions; s++ {
		counts[fmt.Sprintf("perf%d", s)] = perfPerSession
	}
	r := newPerfRigOf(t, counts)
	r.target = r.windows[len(r.windows)/2]
	for i, k := range r.windows {
		size := 4 << 10
		if k == r.target {
			size = perfBig
		}
		perfClaudeHome(t, r.claudeDir, r.panes[k].cli, fmt.Sprintf("00000000-0000-4000-8000-%012d", i), "/src/project", size)
	}
	return r
}

// newPerfRigOf starts a private server with counts[session] windows per
// session, each running perfCLI, and no sessions files yet.
func newPerfRigOf(t testing.TB, counts map[string]int) *perfRig {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Fatalf("tmux is not installed; this suite fails rather than skips: %v", err)
	}
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	r := &perfRig{t: t, sock: "muster-mirror-perf-" + hex.EncodeToString(rnd[:]), claudeDir: t.TempDir(), panes: map[windowKey]paneProc{},
		byName: map[string]windowKey{}, names: map[windowKey]string{}}
	t.Setenv("TMUX", "/tmp/not-the-operators-server,1,0")
	t.Cleanup(func() { _ = r.cmd("kill-server").Run() })
	dir := t.TempDir()
	r.tmuxLog = filepath.Join(dir, "tmux.log")
	bin := filepath.Join(dir, "tmux")
	script := "#!/usr/bin/env bash\nprintf '%s\\n' \"$*\" >> " + strconv.Quote(r.tmuxLog) + "\nexec tmux \"$@\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	sessions := make([]string, 0, len(counts))
	for s := range counts {
		sessions = append(sessions, s)
	}
	sort.Strings(sessions)
	for _, s := range sessions {
		for w := 0; w < counts[s]; w++ {
			var id string
			if w == 0 {
				id = r.run("new-session", "-d", "-P", "-F", "#{window_id}", "-s", s, "-x", "120", "-y", "40", perfCLI)
			} else {
				id = r.run("new-window", "-d", "-P", "-F", "#{window_id}", "-t", "="+s+":", perfCLI)
			}
			k := windowKey{s, strings.TrimSpace(id)}
			r.byName[fmt.Sprintf("%s%d", s, w)], r.names[k] = k, fmt.Sprintf("%s%d", s, w)
		}
	}
	out := r.run("list-panes", "-a", "-F", "#{session_name}\t#{window_id}\t#{pane_id}\t#{pane_pid}")
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, "\t")
		root, _ := strconv.Atoi(f[3])
		k := windowKey{f[0], f[1]}
		r.windows = append(r.windows, k)
		r.panes[k] = paneProc{pane: f[2], root: root, cli: childOf(t, root)}
	}
	sort.Slice(r.windows, func(i, j int) bool {
		if r.windows[i].session != r.windows[j].session {
			return r.windows[i].session < r.windows[j].session
		}
		return r.windows[i].window < r.windows[j].window
	})
	r.backend = Tmux{Bin: bin, Socket: r.sock, Claude: ClaudeLocator{Home: r.claudeDir}}
	return r
}

func (r *perfRig) cmd(args ...string) *exec.Cmd {
	c := exec.Command("tmux", append([]string{"-f", "/dev/null", "-L", r.sock}, args...)...)
	c.Env = tmuxEnv()
	return c
}

func (r *perfRig) run(args ...string) string {
	r.t.Helper()
	out, err := r.cmd(args...).CombinedOutput()
	if err != nil {
		r.t.Fatalf("tmux %v: %v: %s", args, err, out)
	}
	return string(out)
}

// childOf is root's one child, waited for (the pane's shell forks it).
func childOf(t testing.TB, root int) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", root, root))
		if f := strings.Fields(string(b)); len(f) > 0 {
			pid, _ := strconv.Atoi(f[0])
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pane process %d never started its CLI", root)
	return 0
}

// execs is every tmux invocation logged since mark (a line count).
func (r *perfRig) execs(mark int) []string {
	b, _ := os.ReadFile(r.tmuxLog)
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	if mark > len(lines) {
		return nil
	}
	return lines[mark:]
}

// perfMuster is a fake muster: it registers one host and asks it to watch.
type perfMuster struct {
	srv  *httptest.Server
	conn chan *websocket.Conn
	ws   *websocket.Conn
	// hello is the host's hello, as it arrived.
	hello mirror.Msg
	mu    sync.Mutex
}

func newPerfMuster(t testing.TB) *perfMuster {
	t.Helper()
	m := &perfMuster{conn: make(chan *websocket.Conn, 1)}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ws.SetReadLimit(mirror.MaxFrame)
		m.conn <- ws
		<-r.Context().Done()
	}))
	t.Cleanup(m.srv.Close)
	return m
}

// connect runs a muster-mirror of backend against m and waits for its hello.
func (m *perfMuster) connect(t testing.TB, backend Backend) {
	t.Helper()
	c, err := New(Config{URL: m.srv.URL, Token: mirror.TokenPrefix + "perf", Host: "perfhost", Backend: backend, AllowHTTP: true,
		ViewPoll: time.Hour, Refresh: time.Hour, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = c.RunOnce(ctx) }()
	select {
	case m.ws = <-m.conn:
	case <-time.After(10 * time.Second):
		t.Fatal("the host never connected")
	}
	rctx, rcancel := context.WithTimeout(ctx, 30*time.Second)
	defer rcancel()
	_, data, err := m.ws.Read(rctx)
	if err != nil || json.Unmarshal(data, &m.hello) != nil || m.hello.Type != mirror.TypeHello {
		t.Fatalf("no hello: %v", err)
	}
	b, _ := json.Marshal(mirror.Msg{Type: mirror.TypeWelcome, Version: m.hello.Version, Host: "perfhost"})
	if err := m.ws.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

// openResult is one watch's first answer.
type openResult struct {
	took  time.Duration
	first mirror.Msg
	bytes int
}

// watchOrRefusal is watch with a refusal returned rather than failed on.
func (m *perfMuster) watchOrRefusal(t testing.TB, id uint32, k windowKey) (mirror.Msg, string) {
	t.Helper()
	res, refused := m.open(t, id, k, "")
	return res.first, refused
}

// watch asks for one window's view under traceparent tp and returns its first
// batch (the open, from the frame written to that batch read), then closes it.
func (m *perfMuster) watch(t testing.TB, id uint32, k windowKey, tp string) openResult {
	t.Helper()
	res, refused := m.open(t, id, k, tp)
	if refused != "" {
		t.Fatalf("watch %d was refused: %s", id, refused)
	}
	return res
}

func (m *perfMuster) open(t testing.TB, id uint32, k windowKey, tp string) (openResult, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, _ := json.Marshal(mirror.Msg{Type: mirror.TypeWatch, ID: id, Session: k.session, Window: k.window, Traceparent: tp})
	start := time.Now()
	if err := m.ws.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
	for {
		typ, data, err := m.ws.Read(ctx)
		if err != nil {
			t.Fatalf("watch %d: %v", id, err)
		}
		var msg mirror.Msg
		if typ != websocket.MessageText || json.Unmarshal(data, &msg) != nil || msg.ID != id {
			continue
		}
		if msg.Type == mirror.TypeClosed {
			return openResult{}, "refused: " + msg.Error
		}
		if msg.Type != mirror.TypeEvents {
			continue
		}
		res := openResult{took: time.Since(start), first: msg, bytes: len(data)}
		cb, _ := json.Marshal(mirror.Msg{Type: mirror.TypeClose, ID: id})
		_ = m.ws.Write(ctx, websocket.MessageText, cb)
		// Drain what the open still sends (the rest of the backfill, the
		// prompt index) so the next open's read starts clean.
		m.drain(id)
		return res, ""
	}
}

// drain reads until stream id has been closed by the host, or a quiet 300 ms.
func (m *perfMuster) drain(id uint32) {
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		_, data, err := m.ws.Read(ctx)
		cancel()
		if err != nil {
			return
		}
		var msg mirror.Msg
		if json.Unmarshal(data, &msg) == nil && msg.ID == id && msg.Type == mirror.TypeClosed {
			return
		}
	}
}

// spanDurations is each host span's duration by name, in ms, for the spans
// of trace tp.
func spanDurations(exp *tracetest.InMemoryExporter, traceID string) map[string]float64 {
	out := map[string]float64{}
	for _, s := range exp.GetSpans() {
		if s.SpanContext.TraceID().String() != traceID {
			continue
		}
		out[s.Name] += float64(s.EndTime.Sub(s.StartTime).Microseconds()) / 1000
	}
	return out
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[len(s)/2]
}

// TestTmuxOpenPerfReport is the measurement, logged (go test -v): what each
// open of the big window cost on the host, phase by phase, how many tmux
// commands it ran and how many of them listed the whole server, and the
// first batch's size.
func TestTmuxOpenPerfReport(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	restore := telemetry.Use(tp)
	t.Cleanup(func() { restore(); _ = tp.Shutdown(context.Background()) })

	r := newPerfRig(t)
	be := r.backend
	be.Hermes = NewHermesSource(t.TempDir(), t.TempDir())
	be.OpenCode = NewOpenCodeSource(context.Background(), nil, "not paired (perf rig)", log.New(io.Discard, "", 0))
	m := newPerfMuster(t)
	m.connect(t, be)

	const opens = 15
	phases := map[string][]float64{}
	var execs, listAll []float64
	var sizes, took []float64
	var lastFirst mirror.Msg
	for i := 0; i < opens; i++ {
		traceID := fmt.Sprintf("%032x", 0xabc000+i)
		mark := len(r.execs(0))
		res := m.watch(t, uint32(100+i), r.target, "00-"+traceID+"-00f067aa0ba902b7-01")
		var d map[string]float64
		deadline := time.Now().Add(5 * time.Second)
		for {
			d = spanDurations(exp, traceID)
			if _, ok := d["host.watch"]; ok || time.Now().After(deadline) {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		for _, p := range []string{"host.watch", "host.resolve", "host.open", "host.backfill", "host.redact", "host.frame"} {
			phases[p] = append(phases[p], d[p])
		}
		ex := r.execs(mark)
		n := 0
		for _, e := range ex {
			if strings.Contains(e, "list-panes -a") {
				n++
			}
		}
		execs, listAll = append(execs, float64(len(ex))), append(listAll, float64(n))
		sizes, took = append(sizes, float64(res.bytes)), append(took, float64(res.took.Microseconds())/1000)
		lastFirst = res.first
		if !res.first.Reset {
			t.Fatalf("open %d: the first batch is not a reset backfill", i)
		}
	}
	if phases["host.resolve"][0] == 0 {
		t.Fatal("POSITIVE CONTROL: no host.resolve span was exported")
	}
	for _, p := range []string{"host.watch", "host.resolve", "host.open", "host.backfill", "host.redact", "host.frame"} {
		t.Logf("%-14s median %7.2f ms  (min %.2f max %.2f)", p, median(phases[p]), minOf(phases[p]), maxOf(phases[p]))
	}
	t.Logf("open (frame written -> first batch read) median %.2f ms", median(took))
	t.Logf("tmux commands per open: median %.0f; server-wide listings (list-panes -a) per open: median %.0f", median(execs), median(listAll))
	t.Logf("first batch: %.0f bytes encoded, %d deflated (flate.BestSpeed, what the tunnel's permessage-deflate uses)", median(sizes), deflated(t, lastFirst))
}

// deflated is m's JSON deflated as the tunnel deflates it.
func deflated(t testing.TB, m mirror.Msg) int {
	b, _ := json.Marshal(m)
	var buf bytes.Buffer
	w, _ := flate.NewWriter(&buf, flate.BestSpeed)
	_, _ = w.Write(b)
	_ = w.Close()
	return buf.Len()
}

func minOf(v []float64) float64 {
	m := v[0]
	for _, x := range v {
		m = min(m, x)
	}
	return m
}

func maxOf(v []float64) float64 {
	m := v[0]
	for _, x := range v {
		m = max(m, x)
	}
	return m
}

// BenchmarkTmuxWindows is ONE whole-server listing (Tmux.Windows) on the rig:
// what every open paid, once per runtime that could claim the window.
func BenchmarkTmuxWindows(b *testing.B) {
	r := newPerfRig(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.backend.Windows(ctx); err != nil {
			b.Fatal(err)
		}
	}
}
