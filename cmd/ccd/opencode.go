package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/opencodehost"
	"github.com/ZacxDev/muster/internal/podagent"
)

// OPENCODE LANES: ONE OPENCODE SERVER PER OPENCODE MUSTER SESSION.
//
// A chief side chat started on the opencode runtime (muster's
// chat_sessions.runtime 'opencode', migration 0183) runs on an OpenRouter
// model in an opencode server of its own, beside the Claude Code CLIs:
//
//   - its turns come on POST /v1/opencode/responses (opencodeResponsesPath),
//     never /v1/responses, with X-Muster-OC-Session (the session key) and
//     X-Muster-OC-Model ("openrouter/<id>"). 🔴 A DIFFERENT PATH so a ccd that
//     predates this file answers 404 and runs nothing — headers it did not
//     know would have put the turn in its primary Claude CLI;
//   - each lane is a child process `opencode serve --hostname 127.0.0.1
//     --port <free port>`, with its data under /data/opencode/<laneID> (so a
//     restarted server resumes the same opencode session, ocSessionID) and a
//     password of its own (OPENCODE_SERVER_PASSWORD);
//   - its configuration is generated on every start (opencodeConfig) and
//     handed over in OPENCODE_CONFIG_CONTENT, never written to disk: ONE
//     provider, `muster`, the anthropic-compatible one pointed at muster's
//     LLM proxy ($MUSTER_CONNECT_URL/llm/v1) with the agent's own muster
//     token (X-Muster-Token) and the session's key (X-Muster-Chief-Session)
//     in its headers. 🔴 THE PROVIDER KEY NEVER ENTERS THE POD: muster's
//     proxy attaches the chief sessions' OpenRouter key and pins the model;
//   - its MCP servers are the ones `ccd seed` registered for the CLIs
//     (.claude.json mcpServers): muster's (whose tools then read
//     muster_<tool>: codemode off) carrying X-Muster-OC-Session too, so
//     muster can stamp the session's model on what its calls write, and hq;
//   - it counts against the SAME cap as the Claude lanes (CCD_MAX_SESSIONS)
//     and is stopped by the same least-recently-used-idle rule; it is never
//     pinned (the primary chat is never an opencode session).
//
// 🔴 WHO IS WORKING (clis.go): every opencode lane is listed on GET /v1/clis
// with its busy state, and any opencode process that is not a lane's server
// is listed as Unaccounted — so muster's owner_asked, which passes only
// while every OTHER CLI in the pod is idle, cannot fail open on a busy
// opencode lane it would otherwise not see.
//
// 🔴 A TURN HAS A DEADLINE THAT INTERRUPTS. opencode retries a 503 from its
// provider for ever (measured, v2.0.32), so a turn past its budget is
// interrupted (POST /api/session/{id}/interrupt) and answered 504, never left
// running.
//
// The turn is driven over opencode's HTTP API (internal/opencodehost's client
// for its reads, its session create and its prompt): create the session the
// first time, post the prompt, poll the session's messages until the turn's
// idle record, read the reply's text, tool calls and tokens from them.

// The request headers an opencode turn carries: a WIRE CONTRACT with muster's
// internal/agents (HeaderOCSession, HeaderOCModel, OpencodeResponsesPath),
// pinned by TestTheOpencodeHeadersAreMusters.
const (
	headerOCSession       = "X-Muster-OC-Session"
	headerOCModel         = "X-Muster-OC-Model"
	opencodeResponsesPath = "/v1/opencode/responses"
	// headerChiefSession is the LLM proxy's session header (muster's
	// internal/api ChiefSessionHeader): which session a model call is for.
	headerChiefSession = "X-Muster-Chief-Session"
)

// envOpencodeBin names the opencode binary ccd starts (default "opencode").
const envOpencodeBin = "CCD_OPENCODE_BIN"

// opencodeModelPattern is what an opencode lane's model may be: muster's own
// set (agents.OpenRouterModelPatternSource), checked again here because the
// model is written into the config ccd generates.
var opencodeModelPattern = regexp.MustCompile(agents.OpenRouterModelPatternSource)

// opencodeProvider is the one provider an opencode lane's config defines.
const opencodeProvider = "muster"

// opencodeContextWindow and opencodeOutputTokens are the limits the lane's
// one model is declared with. ⚠ A TABLE OF CCD'S, NOT THE MODEL'S: muster's
// proxy cuts max_tokens to what the budget pays for whatever opencode asks,
// and a context window declared smaller than the model's only makes opencode
// compact sooner.
const (
	opencodeContextWindow = 128000
	opencodeOutputTokens  = 16000
)

// opencodeTurnPoll is how often a turn's messages are read for its end.
const opencodeTurnPoll = 400 * time.Millisecond

// opencodeMessageWindow is how many of a session's newest messages a turn
// reads: the most the server answers (measured on v2.3.2: 200 passes, 250 is
// a 400). A turn of many tool steps writes one assistant message per step;
// one that writes more than this is read from its newest window
// (ocTurnOutcome's fallback).
const opencodeMessageWindow = 200

// opencodeMCPWait bounds the wait for muster's MCP server to connect before a
// new server's first prompt (measured: a cold first turn had no tools).
const opencodeMCPWait = 30 * time.Second

// ocSessionID is the opencode session id of muster session key: deterministic,
// so a server started again (after an eviction, a crash, a pod restart) goes
// on with the same session from its data directory.
func ocSessionID(key string) string {
	sum := sha256.Sum256([]byte("ccd-oc-session:" + key))
	return "ses_ccd" + hex.EncodeToString(sum[:12])
}

// ocCatalogID is the model id the lane's provider declares: the stored model
// without its route prefix. muster's proxy pins the session's model whatever
// the request names; this is what opencode records and shows.
func ocCatalogID(model string) string { return strings.TrimPrefix(model, "openrouter/") }

// ocProcess is a running opencode server.
type ocProcess interface {
	// PID is the server's process id.
	PID() int
	// Wait blocks until the server has exited.
	Wait() error
	// Stop ends the server (and anything in its process group).
	Stop()
}

// ocSpec is what an opencode server is started with.
type ocSpec struct {
	Bin, Dir string
	Port     int
	Env      []string
	// Log receives the server's output; nil discards it.
	Log io.Writer
}

// ocLauncher starts an opencode server; tests replace it.
type ocLauncher func(spec ocSpec) (ocProcess, error)

// execProcess is an ocProcess run by os/exec, in a process group of its own.
type execProcess struct {
	cmd  *exec.Cmd
	once sync.Once
	err  error
	done chan struct{}
}

func (p *execProcess) PID() int { return p.cmd.Process.Pid }

func (p *execProcess) Wait() error { <-p.done; return p.err }

// Stop sends the group SIGTERM, then SIGKILL after 3s.
func (p *execProcess) Stop() {
	pgid := p.cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(3 * time.Second):
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		<-p.done
	}
}

// execLauncher is the production launcher.
func execLauncher(spec ocSpec) (ocProcess, error) {
	cmd := exec.Command(spec.Bin, "serve", "--hostname", "127.0.0.1", "--port", strconv.Itoa(spec.Port))
	cmd.Dir, cmd.Env = spec.Dir, spec.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if spec.Log != nil {
		cmd.Stdout, cmd.Stderr = spec.Log, spec.Log
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &execProcess{cmd: cmd, done: make(chan struct{})}
	go func() { p.err = cmd.Wait(); close(p.done) }()
	return p, nil
}

// ocConfig is what an opencode lane is started from; nil on lanes when ccd
// runs no opencode lanes (no binary, or no muster connector to reach the
// proxy and the MCP door through).
type ocConfig struct {
	bin, connectURL, token string
	// dataDir is the lanes' persistent directory (/data/opencode).
	dataDir string
	// runDir is where a lane's instructions file is written: off the volume,
	// regenerated on every start (the managed-path lesson).
	runDir string
	// instructions is the chief's instructions file (the CLI's managed
	// CLAUDE.md), rewritten for opencode's tool spelling.
	instructions string
	launch       ocLauncher
	// readyTimeout bounds a new server's start; turnTimeout one turn.
	readyTimeout, turnTimeout time.Duration
	poll                      time.Duration
	mcpWait                   time.Duration
}

// newOCConfig is the opencode lanes' configuration from the environment, or
// nil (with the reason) when ccd runs none.
func newOCConfig(getenv func(string) string, configDir string, turnTimeout time.Duration) (*ocConfig, string) {
	bin := getenv(envOpencodeBin)
	if bin == "" {
		bin = "opencode"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, "no opencode binary on PATH (" + bin + ")"
	}
	base, tok := strings.TrimRight(getenv(envConnectURL), "/"), getenv(envAgentToken)
	if base == "" || tok == "" {
		return nil, "the pod is not offered muster's connector store (" + envConnectURL + "), which an opencode session reaches its model and tools through"
	}
	return &ocConfig{bin: path, connectURL: base, token: tok,
		dataDir: filepath.Join(filepath.Dir(configDir), "opencode"), runDir: filepath.Join(os.TempDir(), "ccd-opencode"),
		instructions: managedInstructionsPath, launch: execLauncher,
		readyTimeout: laneStartTimeout, turnTimeout: turnTimeout, poll: opencodeTurnPoll, mcpWait: opencodeMCPWait}, ""
}

// managedInstructionsPath is the chief's instructions as the Claude Code CLI
// reads them (muster's agentspec.ClaudeCodeInstructionsPath).
const managedInstructionsPath = "/etc/claude-code/CLAUDE.md"

// ocLane is one opencode session's server.
type ocLane struct {
	id, key, model string
	port           int
	password       string
	proc           ocProcess
	client         *opencodehost.Client
	base           string
	done           chan struct{} // closed when the server has exited
	lastUsed       time.Time     // guarded by lanes.mu
	// reserved counts the turns holding the lane (lanes.mu), as lane.reserved.
	reserved int
	// turn is the lane's one turn slot (capacity 1).
	turn chan struct{}
	// mcpReady: muster's MCP server was seen connected since the server
	// started (guarded by mu).
	mu       sync.Mutex
	mcpReady bool
}

// busy reports whether ccd is driving a turn in the lane now.
func (ln *ocLane) busy() bool { return len(ln.turn) > 0 }

// stop ends the lane's server.
func (ln *ocLane) stop() {
	if ln.proc != nil {
		ln.proc.Stop()
	}
}

// freePort is a loopback port free now (the server binds it next).
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func randomSecret() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// acquireOC returns key's opencode lane, starting it — and stopping the least
// recently used idle lane of either kind when at the cap — if it is not
// running. model must match a running lane's. The lane comes back RESERVED
// (releaseOC once the turn is over).
func (l *lanes) acquireOC(key, model string) (*ocLane, *failure) {
	id := laneID(key)
	l.mu.Lock()
	if ln, ok := l.oc[id]; ok {
		if ln.model != model {
			l.mu.Unlock()
			return nil, &failure{Status: http.StatusBadRequest, Type: failBadRequest,
				Message: fmt.Sprintf("session %q runs on model %q, not %q: a session's model is fixed", key, ln.model, model)}
		}
		ln.reserved++
		ln.lastUsed = l.now()
		l.mu.Unlock()
		return ln, nil
	}
	if _, ok := l.set[id]; ok {
		l.mu.Unlock()
		return nil, &failure{Status: http.StatusBadRequest, Type: failBadRequest,
			Message: fmt.Sprintf("session %q runs in a Claude Code CLI, not opencode: a session's runtime is fixed", key)}
	}
	var evictCL *lane
	var evictOC *ocLane
	if l.unpinnedLocked() >= l.max {
		evictCL, _, evictOC = l.pickAnyEvictLocked()
		if evictCL == nil && evictOC == nil {
			l.mu.Unlock()
			return nil, &failure{Status: http.StatusServiceUnavailable, Type: failCapacity,
				Message: fmt.Sprintf("%v (%d; CCD_MAX_SESSIONS) — retry when one finishes", errLanesFull, l.max)}
		}
		if evictCL != nil {
			delete(l.set, evictCL.id)
		} else {
			delete(l.oc, evictOC.id)
		}
	}
	ln := &ocLane{id: id, key: key, model: model, turn: make(chan struct{}, 1), done: make(chan struct{}),
		reserved: 1, lastUsed: l.now()}
	if l.oc == nil {
		l.oc = map[string]*ocLane{}
	}
	l.oc[id] = ln
	l.mu.Unlock()
	if evictCL != nil {
		log.Printf("ccd: stopping idle session %s (%s) to start opencode session %s: at most %d run at once", evictCL.id, evictCL.key, id, l.max)
		evictCL.stop()
	}
	if evictOC != nil {
		log.Printf("ccd: stopping idle opencode session %s (%s) to start %s: at most %d run at once", evictOC.id, evictOC.key, id, l.max)
		evictOC.stop()
	}
	if f := l.startOC(ln); f != nil {
		l.mu.Lock()
		if l.oc[id] == ln {
			delete(l.oc, id)
		}
		l.mu.Unlock()
		return nil, f
	}
	return ln, nil
}

// pickOCEvictLocked is the least recently used opencode lane no turn holds
// and none is running in. Caller holds l.mu.
func (l *lanes) pickOCEvictLocked() *ocLane {
	var evict *ocLane
	for _, ln := range l.oc {
		if ln.reserved == 0 && !ln.busy() && (evict == nil || ln.lastUsed.Before(evict.lastUsed)) {
			evict = ln
		}
	}
	return evict
}

// pickAnyEvictLocked is the lane to stop for a new one when at the cap:
// pickEvictLocked's Claude lane (stuck: one at a permission prompt) or
// pickOCEvictLocked's opencode lane, whichever was used least recently — a
// stuck Claude lane only when no idle opencode lane is left. At most one of
// the two is set. Caller holds l.mu.
func (l *lanes) pickAnyEvictLocked() (cl *lane, stuck bool, oc *ocLane) {
	cl, stuck = l.pickEvictLocked(false)
	oc = l.pickOCEvictLocked()
	switch {
	case oc == nil:
		return cl, stuck, nil
	case cl == nil, stuck, oc.lastUsed.Before(cl.lastUsed):
		return nil, false, oc
	}
	return cl, stuck, nil
}

// releaseOC ends one reservation acquireOC took.
func (l *lanes) releaseOC(ln *ocLane) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ln.reserved > 0 {
		ln.reserved--
	}
	l.trimLocked()
}

// startOC starts ln's server and waits until it answers. Its run goroutine
// drops the lane when the server exits.
func (l *lanes) startOC(ln *ocLane) *failure {
	c := l.ocCfg
	dir := filepath.Join(c.dataDir, ln.id)
	for _, d := range []string{"data", "state", "config", "cache", "tmp"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			return &failure{Status: http.StatusInternalServerError, Type: failNotReady, Message: "opencode lane: " + err.Error()}
		}
	}
	instr, err := writeOCInstructions(c.instructions, filepath.Join(c.runDir, ln.id), ln.model)
	if err != nil {
		return &failure{Status: http.StatusInternalServerError, Type: failNotReady, Message: "opencode lane: its instructions: " + err.Error()}
	}
	cfg, err := opencodeConfig(l.configDir, c.connectURL, ln.key, ln.model, instr)
	if err != nil {
		return &failure{Status: http.StatusInternalServerError, Type: failMCPConfig, Message: "opencode lane: its config: " + err.Error()}
	}
	port, err := freePort()
	if err != nil {
		return &failure{Status: http.StatusServiceUnavailable, Type: failNotReady, Message: "opencode lane: no free port: " + err.Error()}
	}
	ln.port, ln.password = port, randomSecret()
	ln.base = "http://127.0.0.1:" + strconv.Itoa(port)
	cl, err := opencodehost.NewClient(ln.base, func() string { return ln.password })
	if err != nil {
		return &failure{Status: http.StatusInternalServerError, Type: failNotReady, Message: "opencode lane: " + err.Error()}
	}
	ln.client = cl
	logf, _ := os.OpenFile(filepath.Join(dir, "serve.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	env := opencodeEnv(os.Environ(), dir, ln.password, cfg)
	var w io.Writer
	if logf != nil {
		w = logf
	}
	proc, err := c.launch(ocSpec{Bin: c.bin, Dir: l.workspace, Port: port, Env: env, Log: w})
	if err != nil {
		if logf != nil {
			logf.Close()
		}
		return &failure{Status: http.StatusServiceUnavailable, Type: failNotReady, Message: "opencode lane: start: " + err.Error()}
	}
	ln.proc = proc
	l.runs.Add(1)
	go func() {
		defer l.runs.Done()
		err := proc.Wait()
		if logf != nil {
			logf.Close()
		}
		close(ln.done)
		l.mu.Lock()
		owned := l.oc[ln.id] == ln
		if owned {
			delete(l.oc, ln.id)
		}
		l.mu.Unlock()
		if owned {
			log.Printf("ccd: opencode session %s (%s) ended (err=%v); its next turn starts it again", ln.id, ln.key, err)
		}
	}()
	return ln.waitReady(c.readyTimeout)
}

// waitReady waits until the lane's server answers /api/info.
func (ln *ocLane) waitReady(d time.Duration) *failure {
	deadline := time.Now().Add(d)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := ln.client.Info(ctx)
		cancel()
		if err == nil {
			return nil
		}
		select {
		case <-ln.done:
			return &failure{Status: http.StatusServiceUnavailable, Type: failNotReady,
				Message: "the session's opencode server did not stay up (see /data/opencode/" + ln.id + "/serve.log)"}
		default:
		}
		if time.Now().After(deadline) {
			ln.stop()
			return &failure{Status: http.StatusServiceUnavailable, Type: failNotReady,
				Message: fmt.Sprintf("the session's opencode server did not answer within %s: %v", d, err)}
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// opencodeEnvAllowed reports whether variable name of ccd's environment is
// handed to an opencode server. 🔴 AN ALLOW-LIST: opencode configures a
// provider from any credential variable it knows (ANTHROPIC_API_KEY,
// OPENROUTER_API_KEY, …), and the pod's environment may carry the Claude
// subscription token or a provider key. Only the shell basics and muster's
// own variables (the agent's muster token and URLs, which the chief's
// instructions use) pass; the session's one provider is the generated config's.
func opencodeEnvAllowed(name string) bool {
	switch name {
	case "PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "TZ", "TERM", "CCD_WORKSPACE":
		return true
	}
	return strings.HasPrefix(name, "LC_") || strings.HasPrefix(name, "MUSTER_") || strings.HasPrefix(name, "GIT_")
}

// opencodeEnv is the server's environment: the allowed part of environ, its
// data directories under dir, and opencode's switches (Spike 3A-0: every one
// measured as needed).
func opencodeEnv(environ []string, dir, password string, config []byte) []string {
	var out []string
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if opencodeEnvAllowed(name) {
			out = append(out, kv)
		}
	}
	return append(out,
		"XDG_DATA_HOME="+filepath.Join(dir, "data"),
		"XDG_STATE_HOME="+filepath.Join(dir, "state"),
		"XDG_CONFIG_HOME="+filepath.Join(dir, "config"),
		"XDG_CACHE_HOME="+filepath.Join(dir, "cache"),
		"TMPDIR="+filepath.Join(dir, "tmp"),
		// No self-update, no model list from the network, and 🔴 NO PROJECT
		// CONFIG: a repository's opencode.json was measured to forge the
		// session header and remove tools.
		"OPENCODE_DISABLE_AUTOUPDATE=1",
		"OPENCODE_DISABLE_MODELS_FETCH=1",
		"OPENCODE_DISABLE_PROJECT_CONFIG=1",
		"OPENCODE_SERVER_PASSWORD="+password,
		"OPENCODE_CONFIG_CONTENT="+string(config),
	)
}

// opencodeConfig is a lane's opencode config (opencode v2's schema): the one
// muster provider, the pod's MCP servers, every permission allowed but the
// question tool (a headless turn would hang on it), no title agent (an extra
// billed call), no sharing and no update. The token is referenced from the
// environment ({env:MUSTER_HOOK_TOKEN}), never written into it.
func opencodeConfig(configDir, connectURL, key, model, instructions string) ([]byte, error) {
	if !opencodeModelPattern.MatchString(model) {
		return nil, fmt.Errorf("%q is not an OpenRouter model ccd will configure", model)
	}
	servers, err := opencodeMCPServers(configDir, key)
	if err != nil {
		return nil, err
	}
	tokenRef := "{env:" + envAgentToken + "}"
	cfg := map[string]any{
		"$schema":      "https://opencode.ai/config.json",
		"update":       "disable",
		"autoupdate":   false,
		"share":        "disabled",
		"instructions": []string{instructions},
		"providers": map[string]any{
			opencodeProvider: map[string]any{
				"name":    "muster",
				"package": "@opencode/ai/providers/anthropic-compatible",
				"settings": map[string]any{
					"baseURL": strings.TrimRight(connectURL, "/") + "/llm/v1",
					// Not a key: muster's door reads X-Muster-Token, and strips
					// the x-api-key and Authorization opencode sends.
					"apiKey": "muster-proxy",
				},
				"headers": map[string]any{
					"X-Muster-Token":   tokenRef,
					headerChiefSession: key,
				},
				"models": map[string]any{
					ocCatalogID(model): map[string]any{
						"name":  ocCatalogID(model),
						"limit": map[string]any{"context": opencodeContextWindow, "output": opencodeOutputTokens},
					},
				},
			},
		},
		"mcp": map[string]any{"servers": servers},
		"permissions": []map[string]any{
			{"action": "*", "resource": "*", "effect": "allow"},
			{"action": "question", "resource": "*", "effect": "deny"},
		},
		"agents": map[string]any{"title": map[string]any{"disabled": true}},
	}
	return json.Marshal(cfg)
}

// opencodeMCPServers is .claude.json's user-scope mcpServers (the ones `ccd
// seed` registered), in opencode's spelling: an http server is "remote", a
// stdio one "local", each with codemode off (so muster's tools read
// muster_<tool>). Muster's entry also carries headerOCSession: key. An
// absent muster entry is an error: the session would have no muster tools.
func opencodeMCPServers(configDir, key string) (map[string]any, error) {
	st, err := readJSONObject(filepath.Join(configDir, ".claude.json"), nil)
	if err != nil {
		return nil, err
	}
	in, _ := st["mcpServers"].(map[string]any)
	if _, ok := in[mcpServerName].(map[string]any); !ok {
		return nil, fmt.Errorf("no %q server in .claude.json's mcpServers", mcpServerName)
	}
	out := map[string]any{}
	for name, v := range in {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		switch m["type"] {
		case "http", "sse":
			headers := map[string]any{}
			if h, ok := m["headers"].(map[string]any); ok {
				for k, v := range h {
					headers[k] = v
				}
			}
			if name == mcpServerName {
				headers[headerOCSession] = key
			}
			out[name] = map[string]any{"type": "remote", "url": m["url"], "oauth": false, "headers": headers, "codemode": false}
		case "stdio", nil:
			cmd, _ := m["command"].(string)
			if cmd == "" {
				continue
			}
			argv := []any{cmd}
			if args, ok := m["args"].([]any); ok {
				argv = append(argv, args...)
			}
			e := map[string]any{"type": "local", "command": argv, "codemode": false}
			if env, ok := m["env"].(map[string]any); ok {
				e["environment"] = env
			}
			out[name] = e
		}
	}
	return out, nil
}

// mcpToolSpelling matches a tool name in the Claude Code CLI's MCP spelling;
// mcpServerSpelling its server prefix alone, which the instructions also
// write bare ("mcp__muster__<tool>", "no mcp__muster__ tool").
var (
	mcpToolSpelling   = regexp.MustCompile(`mcp__([A-Za-z0-9-]+)__([A-Za-z0-9_]+)`)
	mcpServerSpelling = regexp.MustCompile(`mcp__([A-Za-z0-9-]+)__`)
)

// opencodeInstructions is the chief's instructions for an opencode session:
// a preamble naming the runtime and the model, then the Claude Code text
// with every tool named in opencode's spelling (mcp__muster__task_get →
// muster_task_get).
func opencodeInstructions(claudeText, model string) string {
	pre := "# This session\n\n" +
		"You are one of your owner's chief side sessions, running in opencode on the OpenRouter model " +
		ocCatalogID(model) + " (not Claude Code). Your tools are named `<server>_<tool>`: muster's are " +
		"`muster_<tool>` (for example `muster_task_get`). The instructions below were written for " +
		"Claude Code; the tool names in them are already in this spelling. Claude Code features they mention " +
		"(subagents, its todo list, slash commands) may not exist here.\n\n" +
		"This is a recon session: read and investigate freely; for a write (a task change, a route, a " +
		"dispatch), propose it and hand over to your owner rather than doing it unasked.\n\n---\n\n"
	return pre + mcpServerSpelling.ReplaceAllString(claudeText, "${1}_")
}

// writeOCInstructions writes the lane's instructions file under dir and
// answers its path. A missing source file (no managed instructions in this
// pod) still writes the preamble.
func writeOCInstructions(src, dir, model string) (string, error) {
	b, err := os.ReadFile(src)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "instructions.md")
	if err := os.WriteFile(path, []byte(opencodeInstructions(string(b), model)), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// handleOpencodeResponses serves POST /v1/opencode/responses: one turn of an
// opencode session.
func (s *server) handleOpencodeResponses(w http.ResponseWriter, r *http.Request) {
	if !bearerOK(r, s.cfg.Bearer) {
		writeFailure(w, &failure{Status: http.StatusUnauthorized, Type: failUnauthorized, Message: "missing or wrong bearer"})
		return
	}
	var req responsesRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeFailure(w, &failure{Status: http.StatusBadRequest, Type: failBadRequest, Message: "body: " + err.Error()})
		return
	}
	prompt, err := promptFrom(req.Input)
	if err != nil {
		writeFailure(w, &failure{Status: http.StatusBadRequest, Type: failBadRequest, Message: err.Error()})
		return
	}
	if strings.TrimSpace(prompt) == "" {
		writeFailure(w, &failure{Status: http.StatusBadRequest, Type: failBadRequest, Message: "empty prompt"})
		return
	}
	key, model := r.Header.Get(headerOCSession), r.Header.Get(headerOCModel)
	if key == "" || len(key) > 200 {
		writeFailure(w, &failure{Status: http.StatusBadRequest, Type: failBadRequest, Message: "an opencode turn names its session (" + headerOCSession + ")"})
		return
	}
	if !opencodeModelPattern.MatchString(model) {
		writeFailure(w, &failure{Status: http.StatusBadRequest, Type: failInvalidInput,
			Message: fmt.Sprintf("%q is not an OpenRouter model ccd will configure (%s)", model, headerOCModel)})
		return
	}
	if s.lanes == nil || s.lanes.ocCfg == nil {
		why := "this ccd runs no sessions of its own (CCD_MAX_SESSIONS is 0, or it does not supervise)"
		if s.lanes != nil {
			why = s.lanes.ocOff
		}
		writeFailure(w, paneNotReady("this ccd runs no opencode sessions: "+why))
		return
	}
	if s.checkout != nil && !s.checkout.Ready() {
		writeFailure(w, paneNotReady(s.checkout.NotReady()))
		return
	}
	ln, f := s.lanes.acquireOC(key, model)
	if f != nil {
		writeFailure(w, f)
		return
	}
	defer s.lanes.releaseOC(ln)
	select {
	case ln.turn <- struct{}{}:
	default:
		writeFailure(w, busyFailure())
		return
	}
	defer func() { <-ln.turn }()
	ctx := r.Context()
	var accepted func()
	if r.Header.Get(headerCCPromptAccepted) == "1" {
		accepted = func() {
			w.Header().Set(headerCCPromptAccepted, "1")
			w.WriteHeader(http.StatusEarlyHints)
			w.Header().Del(headerCCPromptAccepted)
		}
	}
	res, f := s.lanes.runOCTurn(ctx, ln, prompt, accepted)
	if f != nil {
		log.Printf("ccd: opencode turn failed: %s", f.Error())
		writeFailure(w, f)
		return
	}
	stream := req.Stream == nil || *req.Stream
	podagent.WriteResponseUsage(w, res.id, res.items, res.usage, stream)
}

// ocTurnResult is one opencode turn's answer.
type ocTurnResult struct {
	id    string
	items []podagent.ReplyItem
	usage *podagent.Usage
}

// runOCTurn runs prompt in ln's session: create it the first time, wait for
// muster's MCP server, post the prompt, wait for the turn's end (interrupting
// it at the deadline), read the reply.
func (l *lanes) runOCTurn(ctx context.Context, ln *ocLane, prompt string, accepted func()) (ocTurnResult, *failure) {
	c := l.ocCfg
	sid := ocSessionID(ln.key)
	if _, err := ln.client.GetSession(ctx, sid); errors.Is(err, opencodehost.ErrSessionNotFound) {
		if _, err := ln.client.CreateSession(ctx, sid, l.workspace, &opencodehost.ModelRef{ProviderID: opencodeProvider, ID: ocCatalogID(ln.model)}); err != nil {
			return ocTurnResult{}, &failure{Status: http.StatusBadGateway, Type: failTurn, Message: "opencode could not create the session: " + err.Error()}
		}
	} else if err != nil {
		return ocTurnResult{}, &failure{Status: http.StatusServiceUnavailable, Type: failNotReady, Message: "opencode did not answer: " + err.Error()}
	}
	if f := ln.waitMCP(ctx, l.workspace, c.mcpWait); f != nil {
		return ocTurnResult{}, f
	}
	postAt := time.Now()
	msgID := opencodehost.NewMessageID(postAt)
	if _, err := ln.client.Prompt(ctx, sid, msgID, prompt); err != nil {
		return ocTurnResult{}, &failure{Status: http.StatusBadGateway, Type: failTurn, Message: "opencode refused the prompt: " + err.Error()}
	}
	if accepted != nil {
		accepted()
	}
	tctx, cancel := context.WithTimeout(ctx, c.turnTimeout)
	defer cancel()
	tick := time.NewTicker(c.poll)
	defer tick.Stop()
	var readErr error
	for {
		msgs, err := ln.client.Messages(tctx, sid, opencodeMessageWindow)
		readErr = err
		if err == nil {
			if res, f, done := ocTurnOutcome(msgs, msgID, float64(postAt.UnixMilli())); done {
				return res, f
			}
		}
		select {
		case <-ln.done:
			return ocTurnResult{}, &failure{Status: http.StatusBadGateway, Type: failTurn, Message: "the session's opencode server exited during the turn"}
		case <-tctx.Done():
			// 🔴 INTERRUPT, never leave it running: opencode retries a 503 for ever.
			ictx, icancel := context.WithTimeout(context.Background(), 5*time.Second)
			ierr := ln.interrupt(ictx, sid)
			icancel()
			why := "the caller went away"
			if ctx.Err() == nil {
				why = fmt.Sprintf("no end within the turn budget (%s)", c.turnTimeout)
			}
			if readErr != nil && !errors.Is(readErr, context.DeadlineExceeded) && !errors.Is(readErr, context.Canceled) {
				why += " (its messages could not be read: " + readErr.Error() + ")"
			}
			if ierr != nil {
				why += "; interrupting it failed: " + ierr.Error()
			} else {
				why += "; it was interrupted"
			}
			return ocTurnResult{}, &failure{Status: http.StatusGatewayTimeout, Type: failTurnTimeout, Message: "the opencode turn: " + why}
		case <-tick.C:
		}
	}
}

// waitMCP waits until opencode reports muster's MCP server connected in dir:
// the first prompt of a new server otherwise runs with no tools (measured).
// It is waited for once per server start; a server that reports it failed
// refuses the turn rather than answering it without muster's tools.
func (ln *ocLane) waitMCP(ctx context.Context, dir string, d time.Duration) *failure {
	ln.mu.Lock()
	ready := ln.mcpReady
	ln.mu.Unlock()
	if ready {
		return nil
	}
	deadline := time.Now().Add(d)
	last := "not listed"
	for {
		st, err := ln.client.MCPStatus(ctx, dir)
		if err == nil {
			s, ok := st[mcpServerName]
			switch {
			case ok && s.Status == "connected":
				ln.mu.Lock()
				ln.mcpReady = true
				ln.mu.Unlock()
				return nil
			case ok && s.Status == "failed":
				return &failure{Status: http.StatusBadGateway, Type: failMCPConfig,
					Message: "opencode could not connect muster's MCP server: " + s.Error}
			case ok:
				last = s.Status
			}
		} else {
			last = err.Error()
		}
		if time.Now().After(deadline) {
			return &failure{Status: http.StatusServiceUnavailable, Type: failNotReady,
				Message: fmt.Sprintf("muster's MCP server was not connected in opencode within %s (%s)", d, last)}
		}
		select {
		case <-ctx.Done():
			return &failure{Status: http.StatusGatewayTimeout, Type: failTurnTimeout, Message: "caller went away while opencode connected its tools"}
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// interrupt asks the server to stop session sid's running turn
// (POST /api/session/{id}/interrupt). 🔴 ccd's own request, not the shared
// client's: that client is read-mostly by construction (its write ledger),
// and an interrupt is a lane's alone.
func (ln *ocLane) interrupt(ctx context.Context, sid string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ln.base+"/api/session/"+url.PathEscape(sid)+"/interrupt", nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(opencodehost.User, ln.password)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// ocTurnOutcome reads the turn started by user message msgID out of msgs (a
// session's messages, oldest first): done once an "idle" record follows it.
// The reply is every assistant message between the two — its text and tool
// calls in order, its tokens summed — and a failed or interrupted turn is a
// failure carrying opencode's error (muster's proxy's refusal, a budget one
// included, reaches muster this way).
//
// ⚠ A TURN LONGER THAN THE WINDOW: when msgID is no longer among msgs (a
// full window of newer messages), the turn is taken to be every message
// created at or after promptAt (milliseconds, the prompt's post) — so its
// end is still seen, and its reply is the window's part of it.
func ocTurnOutcome(msgs []opencodehost.Message, msgID string, promptAt float64) (ocTurnResult, *failure, bool) {
	start := -1
	for i, m := range msgs {
		if m.ID == msgID {
			start = i
			break
		}
	}
	if start < 0 {
		if len(msgs) < opencodeMessageWindow || len(msgs) == 0 || msgs[0].Time.Created < promptAt {
			return ocTurnResult{}, nil, false
		}
		start = -1 // the whole window is the turn's
	}
	res := ocTurnResult{id: msgID}
	usage := &podagent.Usage{}
	var lastErr string
	for _, m := range msgs[start+1:] {
		switch m.Type {
		case "assistant":
			for _, c := range m.Content {
				switch c.Type {
				case "text":
					if strings.TrimSpace(c.Text) != "" {
						res.items = append(res.items, podagent.ReplyItem{Text: c.Text})
					}
				case "tool":
					status := "in_progress"
					switch c.State.Status {
					case "completed":
						status = "completed"
					case "error":
						status = "failed"
					}
					t := podagent.NewNativeToolItem(c.ID, c.Name, string(c.State.Input), status, "")
					res.items = append(res.items, podagent.ReplyItem{Tool: &t})
				}
			}
			if m.Tokens != nil {
				usage.InputTokens += m.Tokens.Input
				usage.OutputTokens += m.Tokens.Output + m.Tokens.Reasoning
				usage.CacheReadInputTokens += m.Tokens.Cache.Read
				usage.CacheCreationInputTokens += m.Tokens.Cache.Write
				ctxTok := m.Tokens.Input + m.Tokens.Cache.Read + m.Tokens.Cache.Write
				if ctxTok > 0 {
					usage.ContextTokens = &ctxTok
				}
			}
			if e := ocErrorText(m.Error); e != "" {
				lastErr = e
			}
		case "idle":
			if e := ocErrorText(m.Error); e != "" {
				lastErr = e
			}
			res.usage = usage
			switch m.Outcome {
			case "succeeded":
				if len(res.items) == 0 {
					return res, &failure{Status: http.StatusBadGateway, Type: failEmptyReply, Message: "the opencode turn ended with no answer"}, true
				}
				return res, nil, true
			case "interrupted":
				return res, &failure{Status: http.StatusBadGateway, Type: failTurn, Message: "the opencode turn was interrupted" + withErr(lastErr)}, true
			default:
				return res, &failure{Status: http.StatusBadGateway, Type: failTurn, Message: "the opencode turn failed" + withErr(lastErr)}, true
			}
		}
	}
	return ocTurnResult{}, nil, false
}

func withErr(e string) string {
	if e == "" {
		return ""
	}
	return ": " + e
}

// ocErrorText is an opencode message error's text: its data.message, else its
// message, else its name; "" for none.
func ocErrorText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var e struct {
		Name    string `json:"name"`
		Message string `json:"message"`
		Data    struct {
			Message string `json:"message"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &e) != nil {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		return string(raw)
	}
	switch {
	case e.Data.Message != "":
		return e.Data.Message
	case e.Message != "":
		return e.Message
	}
	return e.Name
}

// ocLaneHealth is one opencode lane on /healthz.
type ocLaneHealth struct {
	ID    string `json:"id"`
	Model string `json:"model"`
	PID   int    `json:"pid"`
	Busy  bool   `json:"busy"`
}

func (l *lanes) ocHealth() []ocLaneHealth {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]ocLaneHealth, 0, len(l.oc))
	for _, ln := range l.oc {
		h := ocLaneHealth{ID: ln.id, Model: ln.model, Busy: ln.busy()}
		if ln.proc != nil {
			h.PID = ln.proc.PID()
		}
		out = append(out, h)
	}
	return out
}

// stopAllOC stops every opencode lane (ccd's shutdown).
func (l *lanes) stopAllOC() {
	l.mu.Lock()
	list := make([]*ocLane, 0, len(l.oc))
	for id, ln := range l.oc {
		list = append(list, ln)
		delete(l.oc, id)
	}
	l.mu.Unlock()
	for _, ln := range list {
		ln.stop()
	}
}
