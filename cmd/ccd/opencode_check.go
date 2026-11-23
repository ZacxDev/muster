package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// `ccd opencode-check`: AN OPENCODE LANE, END TO END, AGAINST A FAKE MUSTER.
//
// The image's smoke (cc-smoke) runs it with the REAL opencode binary the image
// pins and no credential: ccd starts an opencode lane whose provider and MCP
// door are a fake muster served in this process (ocFakeMuster: an Anthropic
// /llm/v1/messages and a streamable-HTTP /mcp), drives turns through
// POST /v1/opencode/responses, and checks what each side saw:
//
//   - a plain turn round-trips its text and tokens;
//   - the model call reached the proxy door with the agent's muster token
//     (X-Muster-Token) and the session's key (X-Muster-Chief-Session), and
//     the generated instructions in its system prompt;
//   - the model was offered muster's tools in opencode's spelling
//     (muster_<tool>), and a turn that calls one returns it as a tool item,
//     with the MCP call carrying the session's X-Muster-OC-Session;
//   - while a turn runs, GET /v1/clis lists the lane busy; after, idle;
//   - a proxy refusal (a 400 naming the budget) ends the turn at once with
//     that text.
//
// No real model and no real muster are reached; nothing is written outside a
// temporary directory, which is removed.

// ocFakeMuster is the proxy door and MCP door an opencode lane is pointed at.
type ocFakeMuster struct {
	token string
	mu    sync.Mutex
	llm   []ocFakeCall // every /llm/v1/messages request
	mcp   []ocFakeCall // every /mcp tools/call
	// slow holds a "SLOW" turn's model call until it is closed.
	slow chan struct{}
}

type ocFakeCall struct {
	Header http.Header
	Body   map[string]any
}

func newOCFakeMuster(token string) *ocFakeMuster {
	return &ocFakeMuster{token: token, slow: make(chan struct{})}
}

// ocFakeMCPTools are the tools the fake MCP door lists.
var ocFakeMCPTools = []string{"fleet_list", "task_get"}

func (f *ocFakeMuster) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	var body map[string]any
	_ = json.Unmarshal(b, &body)
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/mcp"):
		f.serveMCP(w, r, body)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/llm/v1/messages"):
		f.serveMessages(w, r, body)
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/mcp"):
		w.WriteHeader(http.StatusMethodNotAllowed)
	default:
		http.NotFound(w, r)
	}
}

func (f *ocFakeMuster) serveMCP(w http.ResponseWriter, r *http.Request, body map[string]any) {
	if r.Header.Get("X-Muster-Token") != f.token {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"bad token"}`)
		return
	}
	id, ok := body["id"]
	if !ok || id == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var res any = map[string]any{}
	switch body["method"] {
	case "initialize":
		pv, _ := body["params"].(map[string]any)["protocolVersion"].(string)
		if pv == "" {
			pv = "2025-06-18"
		}
		res = map[string]any{"protocolVersion": pv, "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": "fake-muster", "version": "0"}}
	case "tools/list":
		var tools []any
		for _, n := range ocFakeMCPTools {
			tools = append(tools, map[string]any{"name": n, "description": "fake " + n,
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}})
		}
		res = map[string]any{"tools": tools}
	case "tools/call":
		f.mu.Lock()
		f.mcp = append(f.mcp, ocFakeCall{Header: r.Header.Clone(), Body: body})
		f.mu.Unlock()
		res = map[string]any{"content": []any{map[string]any{"type": "text", "text": "fake fleet: 0 sessions"}}}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": res})
}

// ocLastUserText is the last user message's text, "TOOL_RESULT" when it carries
// a tool result.
func ocLastUserText(body map[string]any) string {
	msgs, _ := body["messages"].([]any)
	for i := len(msgs) - 1; i >= 0; i-- {
		m, _ := msgs[i].(map[string]any)
		if m["role"] != "user" {
			continue
		}
		if s, ok := m["content"].(string); ok {
			return s
		}
		var out []string
		parts, _ := m["content"].([]any)
		for _, p := range parts {
			pm, _ := p.(map[string]any)
			switch pm["type"] {
			case "tool_result":
				return "TOOL_RESULT"
			case "text":
				s, _ := pm["text"].(string)
				out = append(out, s)
			}
		}
		return strings.Join(out, " ")
	}
	return ""
}

func (f *ocFakeMuster) serveMessages(w http.ResponseWriter, r *http.Request, body map[string]any) {
	f.mu.Lock()
	f.llm = append(f.llm, ocFakeCall{Header: r.Header.Clone(), Body: body})
	f.mu.Unlock()
	text := ocLastUserText(body)
	if strings.Contains(text, "FAIL400") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"muster: the chief sessions' daily budget is spent ($20.00 of $20.00); it resets at 00:00 UTC. Nothing was sent"}}`)
		return
	}
	if strings.Contains(text, "SLOW") {
		select {
		case <-f.slow:
		case <-r.Context().Done():
			return
		case <-time.After(60 * time.Second):
		}
	}
	var tool string
	if strings.Contains(text, "CALLMCP") {
		tools, _ := body["tools"].([]any)
		for _, t := range tools {
			tm, _ := t.(map[string]any)
			if n, _ := tm["name"].(string); strings.HasSuffix(n, "fleet_list") {
				tool = n
			}
		}
	}
	reply := "pong from fake muster"
	if text == "TOOL_RESULT" {
		reply = "done after the tool"
	}
	var sb strings.Builder
	ev := func(name string, d any) {
		b, _ := json.Marshal(d)
		fmt.Fprintf(&sb, "event: %s\ndata: %s\n\n", name, b)
	}
	usage := map[string]any{"input_tokens": 120, "output_tokens": 1, "cache_read_input_tokens": 30, "cache_creation_input_tokens": 10}
	ev("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_fake", "type": "message",
		"role": "assistant", "model": body["model"], "content": []any{}, "stop_reason": nil, "usage": usage}})
	stop := "end_turn"
	if tool != "" {
		stop = "tool_use"
		ev("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "tool_use", "id": fmt.Sprintf("toolu_%d", time.Now().UnixNano()), "name": tool, "input": map[string]any{}}})
		ev("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": "{}"}})
	} else {
		ev("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		ev("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": reply}})
	}
	ev("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	ev("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": 7}})
	ev("message_stop", map[string]any{"type": "message_stop"})
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, sb.String())
}

func (f *ocFakeMuster) llmCalls() []ocFakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ocFakeCall(nil), f.llm...)
}

func (f *ocFakeMuster) mcpCalls() []ocFakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ocFakeCall(nil), f.mcp...)
}

// ocCheckEnv is a fake pod for one check: a config dir holding the
// .claude.json `ccd seed` writes (muster's MCP server, at the fake), a
// workspace, the instructions file, and the server whose lanes run the
// opencode servers.
type ocCheckEnv struct {
	dir   string
	fake  *ocFakeMuster
	front *httptest.Server
	srv   *server
	key   string
	model string
}

// ocCheckInstructions is the instructions text the check's lane is started
// with; the model's system prompt must carry its marker.
const ocCheckInstructionsMarker = "OPENCODE-CHECK-INSTRUCTIONS-7731"

// newOCCheckEnv builds the fake pod in a new temporary directory. launch is
// the opencode launcher (execLauncher for the real binary).
func newOCCheckEnv(bin string, launch ocLauncher) (*ocCheckEnv, error) {
	dir, err := os.MkdirTemp("", "ccd-opencode-check-")
	if err != nil {
		return nil, err
	}
	e := &ocCheckEnv{dir: dir, key: "agent:chief:oc-check", model: "openrouter/deepseek/deepseek-v4.1-flash"}
	e.fake = newOCFakeMuster("check-token-" + randomSecret()[:8])
	e.front = httptest.NewServer(e.fake)
	configDir, workspace := filepath.Join(dir, "claude"), filepath.Join(dir, "workspace")
	for _, d := range []string{configDir, workspace} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			e.close()
			return nil, err
		}
	}
	st := map[string]any{}
	seedMCP(st, e.front.URL, e.fake.token)
	if err := writeJSONAtomic(filepath.Join(configDir, ".claude.json"), st); err != nil {
		e.close()
		return nil, err
	}
	instr := filepath.Join(dir, "CLAUDE.md")
	if err := os.WriteFile(instr, []byte("# chief\n"+ocCheckInstructionsMarker+": call mcp__muster__fleet_list when asked.\n"), 0o600); err != nil {
		e.close()
		return nil, err
	}
	l := &lanes{configDir: configDir, workspace: workspace, max: 3, now: time.Now, set: map[string]*lane{},
		ocCfg: &ocConfig{bin: bin, connectURL: e.front.URL, token: e.fake.token, dataDir: filepath.Join(dir, "opencode"),
			runDir: filepath.Join(dir, "run"), instructions: instr, launch: launch,
			readyTimeout: 60 * time.Second, turnTimeout: 90 * time.Second, poll: 200 * time.Millisecond, mcpWait: 30 * time.Second}}
	e.srv = &server{cfg: serverConfig{Bearer: "check-bearer", ConfigDir: configDir}, lanes: l}
	// The lane's server reads the token from its environment, as in the pod.
	os.Setenv(envAgentToken, e.fake.token)
	return e, nil
}

func (e *ocCheckEnv) close() {
	if e.srv != nil && e.srv.lanes != nil {
		e.srv.lanes.stopAllOC()
		e.srv.lanes.runs.Wait()
	}
	if e.front != nil {
		e.front.Close()
	}
	_ = os.RemoveAll(e.dir)
}

// turn posts prompt as one opencode turn and answers the status and body.
func (e *ocCheckEnv) turn(ctx context.Context, prompt string) (int, string) {
	body, _ := json.Marshal(map[string]any{"model": "x", "stream": true, "input": []any{
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": prompt}}}}})
	req := httptest.NewRequest(http.MethodPost, opencodeResponsesPath, bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer check-bearer")
	req.Header.Set(headerOCSession, e.key)
	req.Header.Set(headerOCModel, e.model)
	rec := httptest.NewRecorder()
	e.srv.handleOpencodeResponses(rec, req)
	return rec.Code, rec.Body.String()
}

// clis is GET /v1/clis's opencode entries for the check's lane.
func (e *ocCheckEnv) clis(ctx context.Context) []cliEntry {
	var out []cliEntry
	for _, c := range e.srv.opencodeCLIs(ctx, procRoot) {
		if c.Lane == e.key {
			out = append(out, c)
		}
	}
	return out
}

// runOpencodeCheck is the check: nil when every step held.
func runOpencodeCheck(bin string, launch ocLauncher, logf func(string, ...any)) error {
	e, err := newOCCheckEnv(bin, launch)
	if err != nil {
		return err
	}
	defer e.close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	// 1. A plain turn.
	code, out := e.turn(ctx, "Reply with exactly one word: pong")
	if code != http.StatusOK || !strings.Contains(out, "pong from fake muster") {
		return fmt.Errorf("a plain turn: HTTP %d %s", code, trimOut(out))
	}
	if !strings.Contains(out, `"input_tokens"`) {
		return fmt.Errorf("a plain turn's answer carries no tokens: %s", trimOut(out))
	}
	logf("ok: a plain turn round-trips its text and tokens")
	calls := e.fake.llmCalls()
	if len(calls) == 0 {
		return errors.New("no model call reached the fake proxy door")
	}
	h := calls[0].Header
	if h.Get("X-Muster-Token") != e.fake.token || h.Get(headerChiefSession) != e.key {
		return fmt.Errorf("the model call's headers: X-Muster-Token set=%v, %s=%q (want the agent token and %q)",
			h.Get("X-Muster-Token") == e.fake.token, headerChiefSession, h.Get(headerChiefSession), e.key)
	}
	sys, _ := json.Marshal(calls[0].Body["system"])
	if !strings.Contains(string(sys), ocCheckInstructionsMarker) {
		return errors.New("the generated instructions are not in the model's system prompt")
	}
	var names []string
	tools, _ := calls[0].Body["tools"].([]any)
	for _, t := range tools {
		tm, _ := t.(map[string]any)
		n, _ := tm["name"].(string)
		names = append(names, n)
	}
	for _, want := range ocFakeMCPTools {
		if !containsStr(names, mcpServerName+"_"+want) {
			return fmt.Errorf("the model was not offered %s_%s (offered %v)", mcpServerName, want, names)
		}
	}
	logf("ok: the model call carried the token and session key, the instructions, and muster's tools as muster_<tool>")

	// 2. A turn that calls a muster tool.
	code, out = e.turn(ctx, "CALLMCP: list the fleet")
	if code != http.StatusOK || !strings.Contains(out, "fleet_list") {
		return fmt.Errorf("a tool turn: HTTP %d %s", code, trimOut(out))
	}
	mc := e.fake.mcpCalls()
	if len(mc) == 0 || mc[0].Header.Get(headerOCSession) != e.key {
		return fmt.Errorf("the MCP call did not name the session (%d calls)", len(mc))
	}
	logf("ok: a tool turn ran muster_fleet_list over MCP, naming its session")

	// 3. Busy while a turn runs, idle after.
	done := make(chan struct{})
	var slowCode int
	go func() { slowCode, _ = e.turn(ctx, "SLOW: wait"); close(done) }()
	busy := false
	for i := 0; i < 100 && !busy; i++ {
		for _, c := range e.clis(ctx) {
			busy = busy || (c.Known && c.Status == cliStatusBusy)
		}
		time.Sleep(100 * time.Millisecond)
	}
	close(e.fake.slow)
	<-done
	if !busy || slowCode != http.StatusOK {
		return fmt.Errorf("a running turn: listed busy=%v, its answer HTTP %d", busy, slowCode)
	}
	var idle bool
	for i := 0; i < 50 && !idle; i++ {
		cs := e.clis(ctx)
		idle = len(cs) == 1 && cs[0].Status == "idle" && !cs[0].Unaccounted
		if !idle {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if !idle {
		return fmt.Errorf("after the turn the lane is not listed idle: %+v", e.clis(ctx))
	}
	logf("ok: /v1/clis lists the lane busy while it works and idle after")

	// 4. A budget refusal from the proxy ends the turn with its text.
	start := time.Now()
	code, out = e.turn(ctx, "FAIL400 please")
	if code == http.StatusOK || !strings.Contains(out, "daily budget is spent") {
		return fmt.Errorf("a refused model call: HTTP %d %s", code, trimOut(out))
	}
	if time.Since(start) > 60*time.Second {
		return fmt.Errorf("a refused model call took %s to end the turn", time.Since(start))
	}
	logf("ok: a proxy refusal ends the turn with its text (HTTP %d, %s)", code, time.Since(start).Round(time.Millisecond))
	return nil
}

func containsStr(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func trimOut(s string) string {
	if len(s) > 600 {
		return s[:600] + "…"
	}
	return s
}

// runOpencodeCheckCmd is `ccd opencode-check [-bin opencode]`.
func runOpencodeCheckCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("opencode-check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	bin := fs.String("bin", envOr(envOpencodeBin, "opencode"), "the opencode binary")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// The check needs loopback only.
	if _, err := net.LookupHost("127.0.0.1"); err != nil {
		fmt.Fprintln(stderr, "ccd opencode-check:", err)
		return 1
	}
	if err := runOpencodeCheck(*bin, execLauncher, func(f string, a ...any) { fmt.Fprintf(stdout, f+"\n", a...) }); err != nil {
		fmt.Fprintln(stderr, "ccd opencode-check: FAIL:", err)
		return 1
	}
	fmt.Fprintln(stdout, "ccd opencode-check: PASS")
	return 0
}
