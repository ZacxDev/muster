package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
)

// A FAKE OPENCODE SERVER (v2's API as measured on 2.3.2: opencode.go's header)
// for the lane's logic; TestTheRealOpencodeBinaryRunsALane drives the real one.

type fakeOC struct {
	mu       sync.Mutex
	sessions map[string]map[string]string // id -> model {providerID, id}
	msgs     map[string][]map[string]any  // id -> messages, oldest first
	running  map[string]bool
	prompts  []string
	// hold, when set, keeps every turn running until it is closed.
	hold chan struct{}
	// never: a turn never ends on its own (only an interrupt ends it).
	never bool
	// outcome is a finished turn's idle outcome ("succeeded" when empty);
	// errText its error.
	outcome, errText string
	activeErr        bool
	mcp              string // muster's MCP status ("connected" when empty)
	interrupts       int
	interrupted      chan struct{}
}

func newFakeOC() *fakeOC {
	return &fakeOC{sessions: map[string]map[string]string{}, msgs: map[string][]map[string]any{}, running: map[string]bool{},
		interrupted: make(chan struct{}, 8)}
}

var fakeOCPath = regexp.MustCompile(`^/api/session/([^/]+)(/[a-z]+)?$`)

func (f *fakeOC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, pw, ok := r.BasicAuth(); !ok || pw == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	writeJSON := func(v any) { w.Header().Set("Content-Type", "application/json"); _ = json.NewEncoder(w).Encode(v) }
	f.mu.Lock()
	defer f.mu.Unlock()
	switch p := r.URL.Path; {
	case p == "/api/info":
		writeJSON(map[string]any{"version": "fake"})
	case p == "/api/mcp":
		st := f.mcp
		if st == "" {
			st = "connected"
		}
		writeJSON(map[string]any{"data": []any{map[string]any{"name": "muster", "status": map[string]any{"status": st, "error": "fake failure"}}}})
	case p == "/api/session/active":
		if f.activeErr {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		d := map[string]any{}
		for id, on := range f.running {
			if on {
				d[id] = map[string]any{"type": "running"}
			}
		}
		writeJSON(map[string]any{"data": d})
	case p == "/api/session" && r.Method == http.MethodPost:
		var b struct {
			ID    string            `json:"id"`
			Model map[string]string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.sessions[b.ID] = b.Model
		writeJSON(map[string]any{"data": map[string]any{"id": b.ID}})
	default:
		m := fakeOCPath.FindStringSubmatch(p)
		if m == nil {
			http.NotFound(w, r)
			return
		}
		sid, sub := m[1], m[2]
		if _, ok := f.sessions[sid]; !ok {
			http.NotFound(w, r)
			return
		}
		switch sub {
		case "":
			writeJSON(map[string]any{"data": map[string]any{"id": sid}})
		case "/prompt":
			var b struct{ ID, Text string }
			_ = json.NewDecoder(r.Body).Decode(&b)
			f.prompts = append(f.prompts, b.Text)
			f.msgs[sid] = append(f.msgs[sid], map[string]any{"id": b.ID, "type": "user", "text": b.Text,
				"time": map[string]any{"created": float64(time.Now().UnixMilli())}})
			f.running[sid] = true
			go f.finish(sid, b.Text)
			writeJSON(map[string]any{"data": map[string]any{"id": b.ID, "sessionID": sid, "payload": map[string]any{"text": b.Text}, "delivery": "queue"}})
		case "/message":
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			if limit > 200 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			all := f.msgs[sid]
			if len(all) > limit {
				all = all[len(all)-limit:]
			}
			out := make([]any, 0, len(all))
			for i := len(all) - 1; i >= 0; i-- {
				out = append(out, all[i])
			}
			writeJSON(map[string]any{"data": out})
		case "/interrupt":
			f.interrupts++
			was := f.running[sid]
			if was {
				f.endLocked(sid, "interrupted", "", "")
			}
			select {
			case f.interrupted <- struct{}{}:
			default:
			}
			writeJSON(map[string]any{"interrupted": was})
		default:
			http.NotFound(w, r)
		}
	}
}

// finish ends sid's turn as configured.
func (f *fakeOC) finish(sid, text string) {
	f.mu.Lock()
	hold, never := f.hold, f.never
	f.mu.Unlock()
	if never {
		return
	}
	if hold != nil {
		<-hold
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running[sid] {
		f.endLocked(sid, f.outcome, f.errText, "fake reply: "+text)
	}
}

func (f *fakeOC) endLocked(sid, outcome, errText, reply string) {
	if outcome == "" {
		outcome = "succeeded"
	}
	now := float64(time.Now().UnixMilli())
	if reply != "" {
		a := map[string]any{"id": "msg_a" + strconv.Itoa(len(f.msgs[sid])), "type": "assistant", "time": map[string]any{"created": now},
			"content": []any{
				map[string]any{"type": "tool", "id": "call_1", "name": "muster_fleet_list", "state": map[string]any{"status": "completed", "input": map[string]any{}}},
				map[string]any{"type": "text", "text": reply},
			},
			"tokens": map[string]any{"input": 120, "output": 7, "reasoning": 3, "cache": map[string]any{"read": 30, "write": 10}}}
		f.msgs[sid] = append(f.msgs[sid], a)
	}
	idle := map[string]any{"id": "msg_i" + strconv.Itoa(len(f.msgs[sid])), "type": "idle", "outcome": outcome, "time": map[string]any{"created": now}}
	if errText != "" {
		idle["error"] = map[string]any{"name": "APIError", "data": map[string]any{"message": errText}}
	}
	f.msgs[sid] = append(f.msgs[sid], idle)
	f.running[sid] = false
}

// fakeOCProc is a launched fake opencode: an HTTP server on the lane's port.
type fakeOCProc struct {
	pid  int
	srv  *http.Server
	done chan struct{}
	once sync.Once
}

func (p *fakeOCProc) PID() int    { return p.pid }
func (p *fakeOCProc) Wait() error { <-p.done; return nil }
func (p *fakeOCProc) Stop()       { p.once.Do(func() { _ = p.srv.Close(); close(p.done) }) }
func (p *fakeOCProc) stopped() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// fakeLauncher launches f on each lane's port, with pids from 9001 up; every
// launch is recorded. Its port reserves the port by holding a listener on it,
// which launch serves on: no close-then-rebind window another process could
// take the port in.
type fakeLauncher struct {
	f     *fakeOC
	mu    sync.Mutex
	procs []*fakeOCProc
	specs []ocSpec
	held  map[int]net.Listener
}

func (fl *fakeLauncher) port() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	fl.mu.Lock()
	if fl.held == nil {
		fl.held = map[int]net.Listener{}
	}
	fl.held[port] = ln
	fl.mu.Unlock()
	return port, nil
}

func (fl *fakeLauncher) launch(spec ocSpec) (ocProcess, error) {
	fl.mu.Lock()
	ln, ok := fl.held[spec.Port]
	delete(fl.held, spec.Port)
	if !ok {
		fl.mu.Unlock()
		return nil, fmt.Errorf("fake launcher: port %d was not reserved by its port func", spec.Port)
	}
	p := &fakeOCProc{pid: 9001 + len(fl.procs), srv: &http.Server{Handler: fl.f}, done: make(chan struct{})}
	fl.procs = append(fl.procs, p)
	fl.specs = append(fl.specs, spec)
	fl.mu.Unlock()
	go func() { _ = p.srv.Serve(ln) }()
	return p, nil
}

const (
	ocTestKey   = "agent:chief:webchat:oc-1"
	ocTestModel = "openrouter/deepseek/deepseek-v4.1-flash"
)

// ocTestServer is a ccd server (the scripted primary CLI) whose lanes run
// opencode lanes on fl, at most max at once. Its config dir holds the
// .claude.json seed writes (muster's MCP server).
func ocTestServer(t *testing.T, max int) (*server, *scriptedCLI, *fakeLauncher) {
	t.Helper()
	srv, cli := newScripted(t, cliScript{})
	fl := &fakeLauncher{f: newFakeOC()}
	l := testLanes(t, max)
	l.configDir = srv.cfg.ConfigDir
	l.workspace = t.TempDir()
	st := map[string]any{}
	seedMCP(st, "http://muster.invalid:8106", "agent-token")
	if err := writeJSONAtomic(filepath.Join(l.configDir, ".claude.json"), st); err != nil {
		t.Fatal(err)
	}
	l.ocCfg = &ocConfig{bin: "opencode", connectURL: "http://muster.invalid:8106", token: "agent-token",
		dataDir: filepath.Join(t.TempDir(), "opencode"), runDir: t.TempDir(), instructions: filepath.Join(t.TempDir(), "none.md"),
		launch: fl.launch, port: fl.port, readyTimeout: 5 * time.Second, turnTimeout: 5 * time.Second, poll: 20 * time.Millisecond, mcpWait: 2 * time.Second}
	srv.runLanes(l)
	return srv, cli, fl
}

// ocTurnReq is an opencode turn's request.
func ocTurnReq(s *server, key, model, text string) *http.Request {
	body, _ := json.Marshal(map[string]any{"model": "x", "stream": true, "input": []any{
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}}})
	req := httptest.NewRequest(http.MethodPost, opencodeResponsesPath, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+s.cfg.Bearer)
	req.Header.Set(headerOCSession, key)
	req.Header.Set(headerOCModel, model)
	return req
}

// TestTheOpencodeHeadersAreMusters pins ccd's spelling of the opencode wire to
// muster's (internal/agents turnroute.go).
func TestTheOpencodeHeadersAreMusters(t *testing.T) {
	for _, c := range [][2]string{{headerOCSession, agents.HeaderOCSession}, {headerOCModel, agents.HeaderOCModel},
		{opencodeResponsesPath, agents.OpencodeResponsesPath}} {
		if c[0] != c[1] {
			t.Errorf("ccd spells %q, muster %q", c[0], c[1])
		}
	}
}

// TestAnOpencodeTurnRoundTripsThroughMustersClient: muster's own client, on
// an opencode session's route, reaches ccd's opencode path; the turn's
// session is created on the session's model (provider muster), the reply's
// text comes back, the tool call is a tool row, and the primary Claude CLI is
// never typed into.
func TestAnOpencodeTurnRoundTripsThroughMustersClient(t *testing.T) {
	srv, cli, fl := ocTestServer(t, 3)
	gw := mustersGateway(t, gatewayServer(t, srv).URL)
	h := http.Header{}
	h.Set(agents.HeaderOCSession, ocTestKey)
	h.Set(agents.HeaderOCModel, ocTestModel)
	ctx := agents.WithTurnRoute(context.Background(), agents.TurnRoute{Runtime: agents.SessionRuntimeOpencode, Header: h})
	reply, err := gw.Chat(ctx, contractAgent, ocTestKey, "hello opencode", nil)
	if err != nil {
		t.Fatal(err)
	}
	if reply != "fake reply: hello opencode" {
		t.Fatalf("reply %q", reply)
	}
	if len(cli.pasted) != 0 {
		t.Fatalf("the primary Claude CLI was typed into: %q", cli.pasted)
	}
	fl.f.mu.Lock()
	model := fl.f.sessions[ocSessionID(ocTestKey)]
	fl.f.mu.Unlock()
	if model["providerID"] != opencodeProvider || model["id"] != "deepseek/deepseek-v4.1-flash" {
		t.Fatalf("the opencode session was created on %v", model)
	}
	// The raw answer carries the tool call as a native item and the tokens.
	rec := httptest.NewRecorder()
	srv.handleOpencodeResponses(rec, ocTurnReq(srv, ocTestKey, ocTestModel, "second"))
	body := rec.Body.String()
	for _, want := range []string{`"muster_fleet_list"`, `"input_tokens":120`, `"output_tokens":10`, `"cache_read_input_tokens":30`, `"cache_creation_input_tokens":10`, `"context_tokens":160`, "fake reply: second"} {
		if !strings.Contains(body, want) {
			t.Errorf("the answer lacks %s: %s", want, body)
		}
	}
	if n := len(fl.procs); n != 1 {
		t.Errorf("%d opencode servers started for one session, want 1", n)
	}
}

// onWriteRecorder records an answer and calls after with each write once it
// is recorded: what a client has read by then.
type onWriteRecorder struct {
	*httptest.ResponseRecorder
	after func(b []byte)
}

func (w *onWriteRecorder) Write(b []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(b)
	if w.after != nil {
		w.after(b)
	}
	return n, err
}

// TestAFollowUpTurnSentOnTheAnswerIsNotRefusedBusy: muster's client stops
// reading at the answer's terminal event (agents.parseResponsesSSE), so the
// session's next turn can arrive while the first turn's handler is still
// writing. That turn runs: the lane's turn slot is free once the answer goes
// out. The follow-up is sent from inside the first answer's response.completed
// write, so the window the CI flake hit by chance is hit every run.
func TestAFollowUpTurnSentOnTheAnswerIsNotRefusedBusy(t *testing.T) {
	srv, _, _ := ocTestServer(t, 3)
	var second *httptest.ResponseRecorder
	first := &onWriteRecorder{ResponseRecorder: httptest.NewRecorder()}
	first.after = func(b []byte) {
		if second != nil || !bytes.Contains(b, []byte("event: response.completed")) {
			return
		}
		second = httptest.NewRecorder()
		srv.handleOpencodeResponses(second, ocTurnReq(srv, ocTestKey, ocTestModel, "follow-up"))
	}
	srv.handleOpencodeResponses(first, ocTurnReq(srv, ocTestKey, ocTestModel, "first"))
	if body := first.Body.String(); !strings.Contains(body, "fake reply: first") {
		t.Fatalf("the first turn's answer: %s", body)
	}
	if second == nil {
		t.Fatal("no response.completed was written: the follow-up was never sent")
	}
	if body := second.Body.String(); second.Code != http.StatusOK || !strings.Contains(body, "fake reply: follow-up") {
		t.Fatalf("a follow-up sent on the first answer's terminal event: HTTP %d %s", second.Code, body)
	}
}

// TestAnOldCcdAnswers404ToTheOpencodePath: the opencode path is a path of
// its own, so a ccd that does not serve it answers 404 and runs nothing. An
// INVARIANT GUARD of the mux (the request is the one muster sends).
func TestAnOldCcdAnswers404ToTheOpencodePath(t *testing.T) {
	srv, cli := newScripted(t, cliScript{transcript: fixture(t, "turn_success_tools_2.1.289.jsonl"), stopEvent: "Stop"})
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/responses", srv.handleResponses) // a ccd from before opencode lanes
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, ocTurnReq(srv, ocTestKey, ocTestModel, "hi"))
	if rec.Code != http.StatusNotFound || len(cli.pasted) != 0 {
		t.Fatalf("an old mux answered %d and pasted %q", rec.Code, cli.pasted)
	}
	// Through muster's own client: the 404 is ErrOpencodeUnsupported, and no
	// fallback (toolless /v1/responses, chat-completions) reaches the CLI.
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	gw := mustersGateway(t, ts.URL)
	h := http.Header{}
	h.Set(agents.HeaderOCSession, ocTestKey)
	h.Set(agents.HeaderOCModel, ocTestModel)
	ctx := agents.WithTurnRoute(context.Background(), agents.TurnRoute{Runtime: agents.SessionRuntimeOpencode, Header: h})
	if _, err := gw.Chat(ctx, contractAgent, ocTestKey, "hi", nil); !errors.Is(err, agents.ErrOpencodeUnsupported) {
		t.Fatalf("muster's client on an old ccd: %v, want ErrOpencodeUnsupported", err)
	}
	if len(cli.pasted) != 0 {
		t.Fatalf("the primary Claude CLI was typed into: %q", cli.pasted)
	}
}

// cliVerdict is muster's owner_asked verdict on s's /v1/clis answer, read
// through muster's own client, for an owner whose turn runs in lane.
func cliVerdict(t *testing.T, s *server, lane string) (agents.LaneVerdict, agents.CLIActivity) {
	t.Helper()
	gw := mustersGateway(t, gatewayServer(t, s).URL)
	act, err := gw.CLIActivity(context.Background(), contractAgent)
	if err != nil {
		t.Fatal(err)
	}
	return agents.OnlyTheOwnersLaneWorks(act, lane), act
}

// TestABusyOpencodeLaneRefusesAnotherLanesOwnerAsked — 🔴 THE FAIL-OPEN SEAM
// (plan §2 "Who is working"). The owner types in the primary chat (the
// primary CLI, busy with the owner's turn) while an opencode side chat is
// mid-turn: owner_asked must refuse, because the tool call may be the
// opencode lane's. ccd's real /v1/clis answer, read by muster's client and
// judged by muster's one rule, says OtherBusy — by ccd's turn slot, and by
// opencode's own account while ccd holds no turn — and OK once it is idle
// (the control). Mutation-checked: dropping the opencode entries from
// handleCLIs turns the busy cases into "" (owner_asked would pass).
func TestABusyOpencodeLaneRefusesAnotherLanesOwnerAsked(t *testing.T) {
	srv, _, fl := ocTestServer(t, 3)
	old := procRoot
	procRoot = t.TempDir() // a fixture /proc holding the primary CLI only
	t.Cleanup(func() { procRoot = old })
	writeSession(t, srv.cfg.ConfigDir, 4242, fixtureSessionID, "busy", "")
	writeProc(t, procRoot, 4242, "S", "800")

	if v, _ := cliVerdict(t, srv, ""); v != agents.LaneVerdictOK {
		t.Fatalf("control: with no opencode lane the owner's turn passes, got %q", v)
	}
	hold := make(chan struct{})
	fl.f.mu.Lock()
	fl.f.hold = hold
	fl.f.mu.Unlock()
	done := make(chan int)
	go func() {
		rec := httptest.NewRecorder()
		srv.handleOpencodeResponses(rec, ocTurnReq(srv, ocTestKey, ocTestModel, "work"))
		done <- rec.Code
	}()
	waitFor(t, "the opencode turn to run", func() bool {
		fl.f.mu.Lock()
		defer fl.f.mu.Unlock()
		return fl.f.running[ocSessionID(ocTestKey)]
	})
	v, act := cliVerdict(t, srv, "")
	if v != agents.LaneVerdictOtherBusy {
		t.Fatalf("a busy opencode lane while the owner types in the primary: verdict %q, want %q (answer %+v)", v, agents.LaneVerdictOtherBusy, act)
	}
	close(hold)
	if code := <-done; code != http.StatusOK {
		t.Fatalf("the held turn answered %d", code)
	}
	if v, act := cliVerdict(t, srv, ""); v != agents.LaneVerdictOK {
		t.Fatalf("control: the opencode lane idle again, verdict %q (answer %+v)", v, act)
	}
	// Busy by opencode's own account, with no turn of ccd's in it (a turn
	// ccd gave up on that opencode is still running).
	fl.f.mu.Lock()
	fl.f.running[ocSessionID(ocTestKey)] = true
	fl.f.mu.Unlock()
	if v, _ := cliVerdict(t, srv, ""); v != agents.LaneVerdictOtherBusy {
		t.Fatalf("an opencode session running by its own account: verdict %q, want %q", v, agents.LaneVerdictOtherBusy)
	}
	// And the owner in the opencode lane itself: its own entry is the owner's.
	if v, _ := cliVerdict(t, srv, ocTestKey); v != agents.LaneVerdictOtherBusy {
		t.Fatalf("the owner in the opencode lane while the primary is busy: verdict %q, want other_busy", v)
	}
}

// TestAnOpencodeLaneThatCannotBeAskedIsUnaccounted: a lane whose server
// cannot say whether it runs, and an opencode process that is not a lane's
// server, are both Unaccounted — never left out.
func TestAnOpencodeLaneThatCannotBeAskedIsUnaccounted(t *testing.T) {
	srv, _, fl := ocTestServer(t, 3)
	old := procRoot
	procRoot = t.TempDir()
	t.Cleanup(func() { procRoot = old })
	writeSession(t, srv.cfg.ConfigDir, 4242, fixtureSessionID, "idle", "")
	writeProc(t, procRoot, 4242, "S", "800")
	rec := httptest.NewRecorder()
	srv.handleOpencodeResponses(rec, ocTurnReq(srv, ocTestKey, ocTestModel, "hi"))
	if rec.Code != http.StatusOK {
		t.Fatalf("turn: %d %s", rec.Code, rec.Body)
	}
	// The lane's own server process is in /proc too: it is the lane, not a stray.
	writeProcAs(t, procRoot, fl.procs[0].pid, "opencode", "/usr/local/bin/opencode", "S", "900")
	if v, act := cliVerdict(t, srv, ""); v != agents.LaneVerdictOK {
		t.Fatalf("control: an idle lane and its own process: %q (%+v)", v, act)
	}
	fl.f.mu.Lock()
	fl.f.activeErr = true
	fl.f.mu.Unlock()
	if v, _ := cliVerdict(t, srv, ""); v != agents.LaneVerdictUnaccounted {
		t.Fatalf("a lane whose server cannot be asked: verdict %q, want unaccounted", v)
	}
	fl.f.mu.Lock()
	fl.f.activeErr = false
	fl.f.mu.Unlock()
	writeProcAs(t, procRoot, 5555, "opencode", "opencode", "S", "901") // a stray: `opencode serve &` from a tool call
	if v, _ := cliVerdict(t, srv, ""); v != agents.LaneVerdictUnaccounted {
		t.Fatalf("a stray opencode process: verdict %q, want unaccounted", v)
	}
}

// TestAnOpencodeTurnPastItsDeadlineIsInterrupted: opencode retries a 503 for
// ever, so a turn with no end within the budget is interrupted and answered
// 504 turn_timeout — never left running.
func TestAnOpencodeTurnPastItsDeadlineIsInterrupted(t *testing.T) {
	srv, _, fl := ocTestServer(t, 3)
	srv.lanes.ocCfg.turnTimeout = 300 * time.Millisecond
	fl.f.mu.Lock()
	fl.f.never = true
	fl.f.mu.Unlock()
	rec := httptest.NewRecorder()
	srv.handleOpencodeResponses(rec, ocTurnReq(srv, ocTestKey, ocTestModel, "hang"))
	if rec.Code != http.StatusGatewayTimeout || !strings.Contains(rec.Body.String(), failTurnTimeout) {
		t.Fatalf("a turn past its deadline: %d %s", rec.Code, rec.Body)
	}
	fl.f.mu.Lock()
	n := fl.f.interrupts
	fl.f.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d interrupts, want 1", n)
	}
}

// TestAFailedOpencodeTurnCarriesTheProxysRefusal: a turn opencode ends as
// failed answers 502 turn_failed with opencode's error text — the proxy's
// budget refusal reaches muster this way — and never a type that marks the
// chief's Claude account (rate_limited, auth_failed, billing).
func TestAFailedOpencodeTurnCarriesTheProxysRefusal(t *testing.T) {
	srv, _, fl := ocTestServer(t, 3)
	fl.f.mu.Lock()
	fl.f.outcome, fl.f.errText = "failed", "muster: the daily budget is spent ($20.00 of $20.00)"
	fl.f.mu.Unlock()
	rec := httptest.NewRecorder()
	srv.handleOpencodeResponses(rec, ocTurnReq(srv, ocTestKey, ocTestModel, "spend"))
	body := rec.Body.String()
	if rec.Code != http.StatusBadGateway || !strings.Contains(body, `"type":"turn_failed"`) || !strings.Contains(body, "daily budget is spent") {
		t.Fatalf("a failed turn: %d %s", rec.Code, body)
	}
}

// TestOpencodeLanesShareTheCap: an opencode lane counts against
// CCD_MAX_SESSIONS. At a cap of 1, a second session stops the first, idle,
// one; while the first runs a turn, the second is refused 503 capacity — and
// so is a Claude lane, which is never started beside it.
func TestOpencodeLanesShareTheCap(t *testing.T) {
	srv, _, fl := ocTestServer(t, 1)
	turn := func(key string) int {
		rec := httptest.NewRecorder()
		srv.handleOpencodeResponses(rec, ocTurnReq(srv, key, ocTestModel, "hi"))
		return rec.Code
	}
	if c := turn("agent:chief:webchat:a"); c != http.StatusOK {
		t.Fatalf("first session: %d", c)
	}
	if c := turn("agent:chief:webchat:b"); c != http.StatusOK {
		t.Fatalf("second session at the cap, the first idle: %d", c)
	}
	if !fl.procs[0].stopped() || fl.procs[1].stopped() {
		t.Fatalf("at the cap the idle first server was not the one stopped (first stopped=%v, second=%v)", fl.procs[0].stopped(), fl.procs[1].stopped())
	}
	hold := make(chan struct{})
	fl.f.mu.Lock()
	fl.f.hold = hold
	fl.f.mu.Unlock()
	done := make(chan int)
	go func() { done <- turn("agent:chief:webchat:b") }()
	waitFor(t, "b's turn to run", func() bool {
		return srv.lanes.get(laneID("agent:chief:webchat:b")) == nil && len(srv.lanes.ocHealth()) == 1 && srv.lanes.ocHealth()[0].Busy
	})
	if c := turn("agent:chief:webchat:c"); c != http.StatusServiceUnavailable {
		t.Fatalf("a third session while the one lane works: %d, want 503", c)
	}
	if _, f := srv.lanes.acquire(context.Background(), "agent:chief:webchat:claude", ""); f == nil || f.Type != failCapacity {
		t.Fatalf("a Claude lane beside a busy opencode lane at a cap of 1: %+v, want capacity", f)
	}
	close(hold)
	<-done
}

// TestAnOpencodeSessionsModelAndRuntimeAreFixed: a running lane refuses
// another model, and the session key of a Claude lane is refused on the
// opencode path (and the reverse), before anything runs.
func TestAnOpencodeSessionsModelAndRuntimeAreFixed(t *testing.T) {
	srv, _, _ := ocTestServer(t, 3)
	rec := httptest.NewRecorder()
	srv.handleOpencodeResponses(rec, ocTurnReq(srv, ocTestKey, ocTestModel, "hi"))
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}
	rec = httptest.NewRecorder()
	srv.handleOpencodeResponses(rec, ocTurnReq(srv, ocTestKey, "openrouter/qwen/qwen3-coder", "hi"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("another model on a running lane: %d", rec.Code)
	}
	if _, f := srv.lanes.acquire(context.Background(), ocTestKey, ""); f == nil || f.Status != http.StatusBadRequest {
		t.Fatalf("an opencode session's key as a Claude lane: %+v", f)
	}
	for _, bad := range []string{"", "sonnet", "openrouter/", "openrouter/x\"}", "openrouter/a b"} {
		rec = httptest.NewRecorder()
		srv.handleOpencodeResponses(rec, ocTurnReq(srv, "agent:chief:webchat:other", bad, "hi"))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("model %q: %d, want 400", bad, rec.Code)
		}
	}
}

// TestACcdWithoutOpencodeSaysWhy: no binary (or no connector) answers 503
// not_ready naming the reason, and starts nothing.
func TestACcdWithoutOpencodeSaysWhy(t *testing.T) {
	srv, _, fl := ocTestServer(t, 3)
	srv.lanes.ocCfg, srv.lanes.ocOff = nil, "no opencode binary on PATH (opencode)"
	rec := httptest.NewRecorder()
	srv.handleOpencodeResponses(rec, ocTurnReq(srv, ocTestKey, ocTestModel, "hi"))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "no opencode binary") || len(fl.procs) != 0 {
		t.Fatalf("%d %s (%d started)", rec.Code, rec.Body, len(fl.procs))
	}
}

// TestAnOpencodeLaneWithoutMustersToolsIsRefused: a server reporting muster's
// MCP server failed refuses the turn rather than answering it without tools.
func TestAnOpencodeLaneWithoutMustersToolsIsRefused(t *testing.T) {
	srv, _, fl := ocTestServer(t, 3)
	fl.f.mu.Lock()
	fl.f.mcp = "failed"
	fl.f.mu.Unlock()
	rec := httptest.NewRecorder()
	srv.handleOpencodeResponses(rec, ocTurnReq(srv, ocTestKey, ocTestModel, "hi"))
	if rec.Code == http.StatusOK || !strings.Contains(rec.Body.String(), failMCPConfig) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	fl.f.mu.Lock()
	n := len(fl.f.prompts)
	fl.f.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d prompts posted to a server without muster's tools", n)
	}
}

// TestTheOpencodeConfigIsTheSpikes: the generated config and environment
// carry every setting Spike 3A-0 measured as needed, the one provider points
// at muster's proxy with the token by reference, and no credential variable
// of the pod reaches opencode.
func TestTheOpencodeConfigIsTheSpikes(t *testing.T) {
	cfg := t.TempDir()
	st := map[string]any{}
	seedMCP(st, "http://muster.invalid:8106", "agent-token-value")
	st["mcpServers"].(map[string]any)["hq"] = hqMCPEntry("http://muster.invalid:8106", "hq.example", "agent-token-value")
	if err := writeJSONAtomic(filepath.Join(cfg, ".claude.json"), st); err != nil {
		t.Fatal(err)
	}
	b, err := opencodeConfig(cfg, "http://muster.invalid:8106/", ocTestKey, ocTestModel, "/run/i.md")
	if err != nil {
		t.Fatal(err)
	}
	var c map[string]any
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	get := func(path ...string) any {
		var v any = c
		for _, p := range path {
			m, _ := v.(map[string]any)
			v = m[p]
		}
		return v
	}
	checks := map[string][2]any{
		"update":              {get("update"), "disable"},
		"share":               {get("share"), "disabled"},
		"title agent":         {get("agents", "title", "disabled"), true},
		"baseURL":             {get("providers", "muster", "settings", "baseURL"), "http://muster.invalid:8106/llm/v1"},
		"package":             {get("providers", "muster", "package"), "@opencode/ai/providers/anthropic-compatible"},
		"token header":        {get("providers", "muster", "headers", "X-Muster-Token"), "{env:MUSTER_HOOK_TOKEN}"},
		"session header":      {get("providers", "muster", "headers", headerChiefSession), ocTestKey},
		"muster MCP type":     {get("mcp", "servers", "muster", "type"), "remote"},
		"muster MCP codemode": {get("mcp", "servers", "muster", "codemode"), false},
		"muster MCP session":  {get("mcp", "servers", "muster", "headers", headerOCSession), ocTestKey},
		"muster MCP url":      {get("mcp", "servers", "muster", "url"), "http://muster.invalid:8106/mcp"},
		"hq MCP type":         {get("mcp", "servers", "hq", "type"), "local"},
	}
	for name, c := range checks {
		if c[0] != c[1] {
			t.Errorf("%s: %v, want %v", name, c[0], c[1])
		}
	}
	if _, ok := get("providers", "muster", "models").(map[string]any)["deepseek/deepseek-v4.1-flash"]; !ok {
		t.Errorf("the model is not declared under its catalogue id: %v", get("providers", "muster", "models"))
	}
	perms, _ := json.Marshal(get("permissions"))
	if string(perms) != `[{"action":"*","effect":"allow","resource":"*"},{"action":"question","effect":"deny","resource":"*"}]` {
		t.Errorf("permissions %s", perms)
	}
	env := opencodeEnv([]string{"PATH=/bin", "HOME=/home/node", "MUSTER_HOOK_TOKEN=agent-token-value", "MUSTER_CONNECT_URL=http://m",
		"CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat-secret", "ANTHROPIC_API_KEY=sk-ant-secret", "OPENROUTER_API_KEY=sk-or-secret",
		"OPENAI_API_KEY=sk-secret", "HOOKS_TOKEN=x", "GIT_CONFIG_GLOBAL=/g"}, "/data/opencode/abc", "pw", b)
	joined := strings.Join(env, "\n")
	for _, want := range []string{"OPENCODE_DISABLE_PROJECT_CONFIG=1", "OPENCODE_DISABLE_AUTOUPDATE=1", "OPENCODE_DISABLE_MODELS_FETCH=1",
		"OPENCODE_SERVER_PASSWORD=pw", "TMPDIR=/data/opencode/abc/tmp", "XDG_DATA_HOME=/data/opencode/abc/data",
		"MUSTER_HOOK_TOKEN=agent-token-value", "GIT_CONFIG_GLOBAL=/g", "PATH=/bin"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the environment lacks %s", want)
		}
	}
	for _, bad := range []string{"sk-ant", "sk-or", "OPENAI_API_KEY", "HOOKS_TOKEN"} {
		if strings.Contains(joined, bad) {
			t.Errorf("the environment passes %s", bad)
		}
	}
	if strings.Contains(string(b), "agent-token-value") && !strings.Contains(string(b), "hq") {
		t.Errorf("the token is written into the provider config")
	}
}

// TestTheOpencodeInstructionsUseOpencodesSpelling: the chief's real
// instructions, rewritten for opencode, name no tool in the Claude Code
// spelling, and every tool they named is named in opencode's.
func TestTheOpencodeInstructionsUseOpencodesSpelling(t *testing.T) {
	src := agentspec.ChiefInstructionsClaudeCode(true, true, "", nil)
	before := mcpToolSpelling.FindAllStringSubmatch(src, -1)
	if len(before) < 5 {
		t.Fatalf("instrument check: the Claude Code instructions name %d tools", len(before))
	}
	out := opencodeInstructions(src, ocTestModel)
	if strings.Contains(out, "mcp__") {
		i := strings.Index(out, "mcp__")
		t.Fatalf("the opencode instructions still say %q", out[i:min(len(out), i+40)])
	}
	for _, m := range before {
		if !strings.Contains(out, m[1]+"_"+m[2]) {
			t.Errorf("%s is not named %s_%s", m[0], m[1], m[2])
		}
	}
	if !strings.Contains(out, "deepseek/deepseek-v4.1-flash") || !strings.Contains(out, "recon session") {
		t.Error("the preamble does not name the model and the recon convention")
	}
}

// opencodeBuiltinTools are opencode 2.3.2's built-in tool names as its model
// call offered them (measured: the probe's request, with question denied).
var opencodeBuiltinTools = []string{"edit", "execute", "experimental_image_generate", "glob", "grep", "read", "shell", "skill",
	"subagent", "webfetch", "websearch", "write"}

// TestNoMCPToolClashesWithAnOpencodeBuiltin: muster's and hq's tools reach
// opencode as <server>_<tool>, and no recorded built-in starts with either
// prefix — the gateway runtime refused a whole turn on a clash.
func TestNoMCPToolClashesWithAnOpencodeBuiltin(t *testing.T) {
	sort.Strings(opencodeBuiltinTools)
	for _, b := range opencodeBuiltinTools {
		for _, srv := range []string{mcpServerName, "hq"} {
			if strings.HasPrefix(b, srv+"_") {
				t.Errorf("built-in %q clashes with the %s_ prefix", b, srv)
			}
		}
	}
}
