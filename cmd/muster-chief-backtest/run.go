package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/llmproxy"
)

func cmdInstructions(args []string) error {
	fs := flag.NewFlagSet("instructions", flag.ExitOnError)
	plugins := fs.Bool("plugins", true, "MUSTER_PLUGINS")
	routing := fs.Bool("routing", true, "MUSTER_SESSION_ROUTING")
	_ = fs.Parse(args)
	_, err := io.WriteString(os.Stdout, agentspec.ChiefInstructionsClaudeCode(*plugins, *routing))
	return err
}

// ProxyLine is one proxied request's record, as the run writes it.
type ProxyLine struct {
	Case      string  `json:"case"`
	Model     string  `json:"model"`
	Requested string  `json:"requested_model"`
	Status    int     `json:"status"`
	Result    string  `json:"result"`
	Input     int64   `json:"input"`
	Output    int64   `json:"output"`
	CacheRead int64   `json:"cache_read"`
	CacheW    int64   `json:"cache_write"`
	CostUSD   float64 `json:"cost_usd"`
	Estimated bool    `json:"cost_estimated"`
	Ms        int64   `json:"ms"`
	Error     string  `json:"upstream_error,omitempty"`
	Path      string  `json:"path,omitempty"` // a path the proxy does not serve (count_tokens, …)
}

// startProxy serves muster's LLM proxy on 127.0.0.1 at /llm/v1/messages, as
// muster's connect listener does, every request pinned to model on key and
// unbudgeted (the chief's Caller). Every other /llm/ path is refused 404 and
// recorded. Each record goes to lines.
func startProxy(model, key, upstream string, lines func(ProxyLine)) (string, func(), error) {
	px, err := llmproxy.New(llmproxy.Config{Upstream: upstream, Logger: log.New(io.Discard, "", 0),
		Audit: func(_ context.Context, r llmproxy.Record) {
			lines(ProxyLine{Case: r.AgentName, Model: r.Model, Requested: r.RequestedModel, Status: r.Status, Result: r.Result,
				Input: r.Input, Output: r.Output, CacheRead: r.CacheRead, CacheW: r.CacheWrite, CostUSD: r.CostUSD,
				Estimated: r.CostEstimated, Ms: r.Duration.Milliseconds(), Error: r.UpstreamError})
		}})
	if err != nil {
		return "", nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /llm/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		px.ServeMessages(w, r, llmproxy.Caller{Officer: "chief", InstanceLabel: "chief", AgentName: r.Header.Get("X-Backtest-Case"),
			Model: model, Key: key, Unbudgeted: true, Title: "muster chief backtest",
			KeyRemedy: "the backtest's BT_OPENROUTER_KEY"})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		lines(ProxyLine{Case: r.Header.Get("X-Backtest-Case"), Status: http.StatusNotFound, Result: "refused_path", Path: r.Method + " " + r.URL.Path})
		llmproxy.Refuse(w, http.StatusNotFound, "not_found_error", "only POST /llm/v1/messages is served here")
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return "http://" + ln.Addr().String() + "/llm", func() { _ = srv.Close() }, nil
}

// ToolUse is one tool the replayed chief asked the CLI to run.
type ToolUse struct {
	Name   string          `json:"name"`
	Input  json.RawMessage `json:"input"`
	Error  bool            `json:"error"`
	Result string          `json:"result,omitempty"`
}

// RunResult is one case's replay.
type RunResult struct {
	Case      string       `json:"case"`
	WallMs    int64        `json:"wall_ms"`
	ToolUses  []ToolUse    `json:"tool_uses"`
	Calls     []LoggedCall `json:"calls"` // what reached the stub MCP server
	Final     string       `json:"final"`
	NumTurns  int          `json:"num_turns"`
	IsError   bool         `json:"is_error"`
	ExitError string       `json:"exit_error,omitempty"`
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	casesPath := fs.String("cases", "", "cases.jsonl (extract.sh)")
	toolsPath := fs.String("tools", "", "the chief's MCP tools/list answer")
	initPath := fs.String("init", "", "the chief's MCP initialize answer")
	outDir := fs.String("out", "", "output directory (outside the repository)")
	model := fs.String("model", "deepseek/deepseek-v4.1-flash", "the OpenRouter model every request is pinned to")
	claudeBin := fs.String("claude", "claude", "the Claude Code CLI")
	parallel := fs.Int("parallel", 4, "cases at once")
	only := fs.String("only", "", "comma-separated turn ids to run (default: all)")
	timeout := fs.Duration("timeout", 8*time.Minute, "per case")
	maxTurns := fs.Int("max-turns", 25, "the CLI's --max-turns")
	_ = fs.Parse(args)
	key := strings.TrimSpace(os.Getenv("BT_OPENROUTER_KEY"))
	if key == "" {
		return errors.New("BT_OPENROUTER_KEY is empty")
	}
	cases, err := readCases(*casesPath)
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	for _, p := range []*string{toolsPath, initPath, outDir} {
		if *p, err = filepath.Abs(*p); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(*outDir, 0o700); err != nil {
		return err
	}
	pf, err := os.OpenFile(filepath.Join(*outDir, "proxy.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer pf.Close()
	var pmu sync.Mutex
	base, stop, err := startProxy(*model, key, "", func(l ProxyLine) {
		b, _ := json.Marshal(l)
		pmu.Lock()
		pf.Write(append(b, '\n'))
		pmu.Unlock()
	})
	if err != nil {
		return err
	}
	defer stop()
	instr := agentspec.ChiefInstructionsClaudeCode(true, true)
	want := map[string]bool{}
	for _, s := range strings.Split(*only, ",") {
		if s = strings.TrimSpace(s); s != "" {
			want[s] = true
		}
	}
	sem := make(chan struct{}, *parallel)
	var wg sync.WaitGroup
	for _, c := range cases {
		id := strconv.FormatInt(c.TurnID, 10)
		if len(want) > 0 && !want[id] {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(c Case, id string) {
			defer wg.Done()
			defer func() { <-sem }()
			res := runCase(c, id, *outDir, self, *claudeBin, base, instr, *toolsPath, *initPath, *timeout, *maxTurns)
			b, _ := json.MarshalIndent(res, "", "  ")
			_ = os.WriteFile(filepath.Join(*outDir, "cases", id, "result.json"), b, 0o600)
			fmt.Fprintf(os.Stderr, "case %s: %d tool uses, %d ms%s\n", id, len(res.ToolUses), res.WallMs, map[bool]string{true: " (" + res.ExitError + ")"}[res.ExitError != ""])
		}(c, id)
	}
	wg.Wait()
	return nil
}

// removedTools are the CLI built-ins a replay is not offered at all (see
// runCase).
var removedTools = []string{"SendMessage", "ListAgents", "CronCreate", "CronDelete", "CronList", "ScheduleWakeup",
	"Workflow", "EnterWorktree", "ExitWorktree", "TaskStop", "DesignSync", "ReportFindings", "Skill"}

func runCase(c Case, id, outDir, self, claudeBin, base, instr, toolsPath, initPath string, timeout time.Duration, maxTurns int) RunResult {
	res := RunResult{Case: id}
	dir := filepath.Join(outDir, "cases", id)
	work := filepath.Join(dir, "work")
	cfgDir := filepath.Join(dir, "claude-config")
	for _, d := range []string{work, cfgDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			res.ExitError = err.Error()
			return res
		}
	}
	_ = os.Remove(filepath.Join(dir, "calls.jsonl"))
	cb, _ := json.Marshal(c)
	_ = os.WriteFile(filepath.Join(dir, "case.json"), cb, 0o600)
	// The chief's prose where the CLI reads a CLAUDE.md (the pod places it as
	// the managed policy file; both are loaded as the session's memory).
	_ = os.WriteFile(filepath.Join(work, "CLAUDE.md"), []byte(instr), 0o600)
	mcpCfg := map[string]any{"mcpServers": map[string]any{"muster": map[string]any{
		"type": "stdio", "command": self,
		"args": []string{"mcp", "-tools", toolsPath, "-init", initPath, "-case", filepath.Join(dir, "case.json"), "-log", filepath.Join(dir, "calls.jsonl")}}}}
	mb, _ := json.Marshal(mcpCfg)
	_ = os.WriteFile(filepath.Join(dir, "mcp.json"), mb, 0o600)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// 🔴 dontAsk: only the allowed tools run, every other one is denied with
	// no prompt — never the CLI's default here, which can be "auto" (a
	// classifier that approves tool calls). The tools that would reach THIS
	// machine's other sessions or schedule work (SendMessage, ListAgents,
	// Cron*, ScheduleWakeup, Workflow, worktrees) are removed outright: the
	// chief's pod has no peers to message, and a replay must not reach them.
	cmd := exec.CommandContext(ctx, claudeBin, "-p", c.Input, "--output-format", "stream-json", "--verbose",
		"--mcp-config", filepath.Join(dir, "mcp.json"), "--strict-mcp-config",
		"--permission-mode", "dontAsk",
		"--allowedTools", "mcp__muster", "--allowedTools", "ToolSearch",
		"--disallowedTools", strings.Join(removedTools, ","),
		"--max-turns", strconv.Itoa(maxTurns))
	cmd.Dir = work
	cmd.Env = caseEnv(cfgDir, base, id)
	stream, err := os.Create(filepath.Join(dir, "stream.jsonl"))
	if err != nil {
		res.ExitError = err.Error()
		return res
	}
	defer stream.Close()
	stderr, _ := os.Create(filepath.Join(dir, "stderr.txt"))
	defer stderr.Close()
	cmd.Stdout, cmd.Stderr = stream, stderr
	start := time.Now()
	err = cmd.Run()
	res.WallMs = time.Since(start).Milliseconds()
	if err != nil {
		res.ExitError = err.Error()
	}
	parseStream(filepath.Join(dir, "stream.jsonl"), &res)
	res.Calls = readCalls(filepath.Join(dir, "calls.jsonl"))
	return res
}

// caseEnv is the CLI's environment: this process's, minus every credential
// and endpoint the CLI could select, plus the proxy. CLAUDE_CONFIG_DIR is
// fresh, so no user setting, hook, plugin or MCP server of this machine's
// applies.
func caseEnv(cfgDir, base, id string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k := kv[:strings.IndexByte(kv, '=')]
		if strings.HasPrefix(k, "ANTHROPIC_") || strings.HasPrefix(k, "CLAUDE") || k == "BT_OPENROUTER_KEY" ||
			strings.Contains(k, "OPENROUTER") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "CLAUDE_CONFIG_DIR="+cfgDir, "ANTHROPIC_BASE_URL="+base, "ANTHROPIC_AUTH_TOKEN=backtest-not-a-credential",
		"ANTHROPIC_CUSTOM_HEADERS=X-Backtest-Case: "+id, "DISABLE_AUTOUPDATER=1", "DISABLE_TELEMETRY=1",
		"DISABLE_ERROR_REPORTING=1")
}

func parseStream(path string, res *RunResult) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	byID := map[string]int{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type      string          `json:"type"`
					ID        string          `json:"id"`
					Name      string          `json:"name"`
					Input     json.RawMessage `json:"input"`
					ToolUseID string          `json:"tool_use_id"`
					IsError   bool            `json:"is_error"`
					Content   json.RawMessage `json:"content"`
				} `json:"content"`
			} `json:"message"`
			Result   string `json:"result"`
			NumTurns int    `json:"num_turns"`
			IsError  bool   `json:"is_error"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "assistant":
			for _, b := range ev.Message.Content {
				if b.Type == "tool_use" {
					byID[b.ID] = len(res.ToolUses)
					res.ToolUses = append(res.ToolUses, ToolUse{Name: b.Name, Input: b.Input})
				}
			}
		case "user":
			for _, b := range ev.Message.Content {
				if i, ok := byID[b.ToolUseID]; ok && b.Type == "tool_result" {
					res.ToolUses[i].Error = b.IsError
					s := string(b.Content)
					var str string
					if json.Unmarshal(b.Content, &str) == nil {
						s = str
					}
					res.ToolUses[i].Result = s[:min(len(s), 400)]
				}
			}
		case "result":
			res.Final, res.NumTurns, res.IsError = ev.Result, ev.NumTurns, ev.IsError
		}
	}
}

func readCalls(path string) []LoggedCall {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []LoggedCall
	for _, l := range strings.Split(string(b), "\n") {
		var lc LoggedCall
		if json.Unmarshal([]byte(l), &lc) == nil && lc.Tool != "" {
			out = append(out, lc)
		}
	}
	return out
}
