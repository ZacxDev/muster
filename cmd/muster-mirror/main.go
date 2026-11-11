// Command muster-mirror shows this machine's tmux sessions in muster's Agents
// view as MIRRORED sessions — one card per tmux WINDOW. It is READ-ONLY unless started with -interactive. It is the optional host half of the mirror plugin: install it
// only on the machines whose sessions muster should show, and only against a
// muster running with MUSTER_MIRROR=1.
//
// It dials OUT to muster and opens no listener — unless -mcp-listen is set,
// which serves a LOOPBACK-only MCP endpoint for this host's own Claude Code
// sessions (internal/mirrorhost's MCPProxy; docs/attention.md). Its one credential is this
// host's mirror token, minted on the muster side with
//
//	muster-server mirror token create <host>
//
// Usage:
//
//	muster-mirror -url https://muster.example.com -token-file ~/.config/muster-mirror/token
//
// `muster-mirror ask` is a different, short-lived command: Claude Code's
// PreToolUse hook for AskUserQuestion (ask.go).
//
// Every flag also reads an environment variable (named in -help), so a systemd
// unit can configure it without putting the token in argv.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	// The zone database, built in: a usage-limit message names its reset in an
	// IANA zone ("resets 10:30pm (Europe/London)", cctranscript.LimitReset),
	// and a host with no /usr/share/zoneinfo (a minimal container) must still
	// place it — else the queue holds until a real answer instead of resuming
	// at the reset.
	_ "time/tzdata"

	"go.opentelemetry.io/otel/attribute"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
	"github.com/ZacxDev/muster/internal/opencodehost"
	"github.com/ZacxDev/muster/internal/previewhost"
	"github.com/ZacxDev/muster/internal/telemetry"
)

// e2eBackend replaces the tmux backend in a browser-test build (e2emirror.go,
// `-tags e2emirror`). nil in every shipped binary.
var e2eBackend func() mirrorhost.Backend

// e2eNoPromptIndex, in a browser-test build, makes the host behave as one that
// predates the prompt index (mirrorhost.Config.NoPromptIndex). nil in every
// shipped binary.
var e2eNoPromptIndex func() bool

// e2eNoCatalog, in a browser-test build, makes the host behave as one that
// predates the composer menu's catalog (mirrorhost.Config.NoCatalog). nil in
// every shipped binary.
var e2eNoCatalog func() bool

// e2eNoSubagents, in a browser-test build, makes the host behave as one that
// predates the subagent cards (mirrorhost.Config.NoSubagents). nil in every
// other build.
var e2eNoSubagents func() bool

// e2eNoWork, in a browser-test build, makes the host behave as one that
// predates the live work counts and identity colours
// (mirrorhost.Config.NoWork). nil in every other build.
var e2eNoWork func() bool

// e2ePreviews, in a browser-test build, is the per-window preview counts the
// host reports (mirrorhost.Config.Previews) when the spec plays them
// (e2emirror.go's MUSTER_MIRROR_E2E_PREVIEWS_FILE); nil when it does not — and
// in every shipped binary, whose counts come from -preview alone.
var e2ePreviews func() func() map[string]mirror.WindowPreviews

// e2eRefresh, in a browser-test build, is how often the host re-reads its
// windows (mirrorhost.Config.Refresh; 0 = mirrorhost's default) — so a spec
// waiting for a status flip to reach muster waits a fraction of a second, not
// the default refresh. nil in every shipped binary, which therefore has no way
// to change its refresh at all (TestTheShippedBuildCannotChangeItsRefresh).
var e2eRefresh func() time.Duration

// e2eWrap, in a browser-test build, wraps the backend AFTER NoShrink: a
// capability added there (e2emirror.go's copy-mode scroll) is found through the
// wrapper chain without hiding what NoShrink asserts on the backend it wraps.
var e2eWrap func(mirrorhost.Backend) mirrorhost.Backend

// e2eSpawn, in a browser-test build, replaces the agent spawn with the fake's
// (mirrorhost.Config.FakeSpawn), offering e2eSpawnRuntimes. nil in every
// shipped binary (TestTheShippedBuildSpawnsForReal).
var (
	e2eSpawn         func(ctx context.Context, dispatch int64, req mirror.AgentSpawnReq) (mirror.AgentSpawned, error)
	e2eSpawnRuntimes []string
)

// buildRevision is stamped by the build (-X main.buildRevision=…).
var buildRevision = "dev"

const (
	envURL         = "MUSTER_MIRROR_URL"
	envToken       = "MUSTER_MIRROR_TOKEN"
	envTokenFile   = "MUSTER_MIRROR_TOKEN_FILE"
	envHost        = "MUSTER_MIRROR_HOST"
	envSocket      = "MUSTER_MIRROR_TMUX_SOCKET"
	envSocketPath  = "MUSTER_MIRROR_TMUX_SOCKET_PATH"
	envInteractive = "MUSTER_MIRROR_INTERACTIVE"
	// The mail resume (mirrorhost's resume.go): off by default.
	envResumeSpawn   = "MUSTER_MIRROR_RESUME_SPAWN"
	envResumeSession = "MUSTER_MIRROR_RESUME_SESSION"
	envResumeClaude  = "MUSTER_MIRROR_RESUME_CLAUDE"
	envAllowHTTP     = "MUSTER_MIRROR_ALLOW_HTTP"
	// The chief's host spawn (mirrorhost's agentspawn.go): off by default, and
	// with an EMPTY repository allow-list by default.
	envAgentSpawn      = "MUSTER_MIRROR_AGENT_SPAWN"
	envAgentSpawnRoots = "MUSTER_MIRROR_AGENT_SPAWN_ROOTS"
	envAgentSpawnDir   = "MUSTER_MIRROR_AGENT_SPAWN_DIR"
	envAgentSpawnRepos = "MUSTER_MIRROR_AGENT_SPAWN_REPOS"
	envAgentSpawnRate  = "MUSTER_MIRROR_AGENT_SPAWN_RATE"
	envAgentSpawnLog   = "MUSTER_MIRROR_AGENT_SPAWN_LOG"
	// envAgentSpawnSkipPermissions: -agent-spawn-skip-permissions, ON unless
	// set to a false value (what strconv.ParseBool reads as false: 0, f, F,
	// FALSE, false, False); any other non-bool value stops the daemon.
	envAgentSpawnSkipPermissions = "MUSTER_MIRROR_AGENT_SPAWN_SKIP_PERMISSIONS"

	// envAgentSpawnTenantServer: the one non-default tmux server a tenant's
	// mirror may spawn on — it must name the server the mirror mirrors.
	envAgentSpawnTenantServer = "MUSTER_MIRROR_AGENT_SPAWN_TENANT_SERVER"

	envLabelsFile = "MUSTER_MIRROR_LABELS_FILE"
	envPaneMapDir = "MUSTER_MIRROR_PANE_MAP_DIR"
	envMCPListen  = "MUSTER_MIRROR_MCP_LISTEN"
	envMCPSecret  = "MUSTER_MIRROR_MCP_SECRET_FILE"
	// envWriteHookConfig: record this daemon's URL and token file for
	// `muster-mirror ask` (hookconfig.go). Set by the managed service only.
	envWriteHookConfig = "MUSTER_MIRROR_WRITE_HOOK_CONFIG"
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// refreshOverride is a browser-test build's refresh, or 0 (mirrorhost's
// default) in every shipped binary.
func refreshOverride() time.Duration {
	if e2eRefresh == nil {
		return 0
	}
	return e2eRefresh()
}

// defaultHost is this machine's short hostname, lowercased: what a token is
// minted for unless -host says otherwise.
func defaultHost() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	h, _, _ = strings.Cut(strings.ToLower(h), ".")
	return h
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	// `muster-mirror ask`: the AskUserQuestion hook (ask.go), a short-lived
	// command of its own — not the host daemon.
	if len(args) > 0 && args[0] == "ask" {
		return runAsk(args[1:], getenv, stdin, stdout, stderr)
	}
	// `muster-mirror opencode-pair`: mint this host's opencode token
	// (opencode.go). Run once per host.
	if len(args) > 0 && args[0] == "opencode-pair" {
		return runOpenCodePair(args[1:], getenv, stdout, stderr)
	}
	// `muster-mirror opencode`: start the opencode TUI bound to a session
	// muster can send to (opencode_launch.go).
	if len(args) > 0 && args[0] == "opencode" {
		return runOpenCodeLaunch(args[1:], realLaunchDeps(getenv), stderr)
	}
	// `muster-mirror agent-spawn-log [-n N]`: the last entries of the host's
	// spawn audit log, without knowing its path.
	if len(args) > 0 && args[0] == "agent-spawn-log" {
		return runAgentSpawnLog(args[1:], getenv, stdout, stderr)
	}
	fs := flag.NewFlagSet("muster-mirror", flag.ContinueOnError)
	fs.SetOutput(stderr)
	envBool := func(k string) bool { v := getenv(k); return v == "1" || strings.EqualFold(v, "true") }
	or := func(v, d string) string {
		if v != "" {
			return v
		}
		return d
	}
	url := fs.String("url", getenv(envURL), "muster's base URL, https://… ("+envURL+")")
	tokenFile := fs.String("token-file", getenv(envTokenFile), "file holding this host's mirror token ("+envTokenFile+"; or the token itself in "+envToken+")")
	host := fs.String("host", or(getenv(envHost), defaultHost()), "this host's name; the token must have been minted for it ("+envHost+")")
	socket := fs.String("tmux-socket", getenv(envSocket), "tmux -L socket name; empty = the default server ("+envSocket+")")
	socketPath := fs.String("tmux-socket-path", getenv(envSocketPath), "tmux -S socket path ("+envSocketPath+")")
	interactive := fs.Bool("interactive", envBool(envInteractive), "let muster open INTERACTIVE terminals on this host — effectively a SHELL here: an interactive tmux client can switch to any session or open a new window (default: read-only; muster then offers no Interactive switch) ("+envInteractive+")")
	allowHTTP := fs.Bool("allow-http", envBool(envAllowHTTP), "TESTS AND LOCAL DEVELOPMENT ONLY: permit a plain http:// muster URL, which sends the token in cleartext ("+envAllowHTTP+")")
	labelsFile := fs.String("labels-file", getenv(envLabelsFile), "JSON object {\"<session>\": \"<codename>\"}: muster shows such a session as <codename>:<active window index> instead of its name; unreadable or invalid = no labels, logged, never fatal ("+envLabelsFile+")")
	paneMapDir := fs.String("pane-map-dir", getenv(envPaneMapDir), "directory of per-pane Claude Code records (claude-p<N>.json for pane %N, written by a hook on every event) that the rendered view resolves a pane's transcript from FIRST, each validated against the process in the pane; empty = not consulted ("+envPaneMapDir+")")
	writeHook := fs.Bool("write-hook-config", envBool(envWriteHookConfig), "record this daemon's URL and token file (never the token) for `muster-mirror ask`, in "+defaultHookConfig+" ("+envHookConfig+"); for the host's managed service only — an ad-hoc daemon must not repoint the hook ("+envWriteHookConfig+")")
	mcpListen := fs.String("mcp-listen", getenv(envMCPListen), "serve muster's attention and task tools to this host's Claude Code sessions as an MCP endpoint at http://<addr>/mcp — a LOOPBACK address only, e.g. 127.0.0.1:47823; each call needs the secret in -mcp-secret-file (Authorization: Bearer ${MUSTER_MIRROR_MCP_SECRET:-}) and names its pane and tmux server with X-Muster-Pane: ${TMUX_PANE:-} and X-Muster-Tmux: ${TMUX:-}; empty = off ("+envMCPListen+")")
	mcpSecretFile := fs.String("mcp-secret-file", or(getenv(envMCPSecret), mirrorhost.DefaultMCPSecretFile()), "the local MCP endpoint's secret, created (0600) when missing ("+envMCPSecret+")")
	catalog := fs.Bool("catalog", false, "print the composer menu's catalog this host would report for a pane whose working directory is the current one — commands, skills (source and name) and snippet triggers — and exit")
	list := fs.Bool("list", false, "print the sessions this host would register, and exit (a labelled session's line ends in a label=<label> column)")
	listClaude := fs.Bool("claude", false, "with -list: print, for each WINDOW whose active pane runs Claude Code, what its rendered view would stream (one line each: session, window id, index, claude, pane, state, session id, transcript, source = pane-map|sessions|guessed, then label=<label>); per session on a backend that does not list windows")
	listWindows := fs.Bool("windows", false, "with -list: print every WINDOW this host would register instead (one line each: session, window id, index, active|-, command, then claude <pane> <state> <session id> <transcript> <source> for a Claude Code window, or opencode <pane> <state> <session id or -> <source> for an opencode one, then label=<label>)")
	resumeSpawn := fs.Bool("resume-spawn", envBool(envResumeSpawn), "with -interactive: let muster RESUME an ended Claude Code session on this host — a new detached tmux window running `<-resume-claude> --resume <session id>` in the working directory its transcript records, when agent mail or ClankMail arrives for it (muster's MUSTER_MAIL_RESUME_ALLOW decides whose mail may). OFF by default: it starts a process here. Refused for a session running here already, a transcript this host does not hold, or a directory that is gone; at most one resume per session per 10 minutes and 6 an hour ("+envResumeSpawn+")")
	resumeSession := fs.String("resume-session", or(getenv(envResumeSession), mirrorhost.DefaultResumeSession), "the tmux session resumed windows start in, created when absent ("+envResumeSession+")")
	resumeClaude := fs.String("resume-claude", or(getenv(envResumeClaude), mirrorhost.DefaultResumeClaude), "the Claude Code binary a resume runs; a bare name is looked up on the tmux SERVER's PATH, so an absolute path is safer ("+envResumeClaude+")")
	listOpenCode := fs.Bool("opencode", false, "with -list: diagnose this host's opencode windows — the opencode token (by fingerprint) and whether the server accepts it, and per window its TUI, pane title, the session the title matched and why it has no rendered view")
	openCodeTokenFile := fs.String("opencode-token-file", openCodeTokenFileDefault(getenv), "the opencode pair token, {url, token}, mode 0600, written by `muster-mirror opencode-pair` and renewed by this daemon before it expires; missing = no opencode access ("+envOpenCodeTokenFile+")")
	openCodeReadOnly := fs.Bool("opencode-read-only", envBool(envOpenCodeReadOnly), "read the opencode token file, and re-read it when it changes, but NEVER renew it: another muster-mirror on this host (tenant default's) renews it. Set on every muster-mirror@<tenant> instance, so exactly one daemon renews the token ("+envOpenCodeReadOnly+")")
	openCodeSend := fs.Bool("opencode-send", envBool(envOpenCodeSend), "with -interactive: let muster SEND messages to opencode sessions on this host through the opencode server's API — only to a pane started with `muster-mirror opencode`, only while its session is idle, behind a fresh sign-in. OFF by default: whatever the agent there may run, a message from muster can ask it to ("+envOpenCodeSend+")")
	agentSpawn := fs.Bool("agent-spawn", envBool(envAgentSpawn), "with -interactive, on the DEFAULT tmux server (or, with -agent-spawn-tenant-server, on the tenant tmux server this mirror mirrors): let your chief START a Claude Code agent (or an opencode one, with -opencode-send and a token too) on this host — a fresh git worktree of a repository on -agent-spawn-repos, cut from origin, and a window in tmux session `agents`, without permission prompts unless -agent-spawn-skip-permissions=false. OFF by default: it starts a process here. The host enforces its own allow-list, rate limit and audit log whatever muster sends ("+envAgentSpawn+")")
	// ON BY DEFAULT, so its env var is read as a bool where unset is true; a
	// value that is not a bool stops the daemon below rather than guess.
	skipPermsDefault, skipPermsErr := true, error(nil)
	if v := getenv(envAgentSpawnSkipPermissions); v != "" {
		skipPermsDefault, skipPermsErr = strconv.ParseBool(v)
	}
	agentSpawnSkipPermissions := fs.Bool("agent-spawn-skip-permissions", skipPermsDefault, "with -agent-spawn: start each spawned agent WITHOUT permission prompts — Claude Code with `--permission-mode bypassPermissions`, opencode with `--auto` — so it runs its tools without asking anyone (muster answers no permission prompt itself and assumes its sessions run without them). ON by default; =false starts them as before, in each CLI's normal permission mode. As root, claude starts in normal permission mode whatever this says: Claude Code refuses bypassPermissions as root ("+envAgentSpawnSkipPermissions+")")
	agentSpawnRoots := fs.String("agent-spawn-roots", or(getenv(envAgentSpawnRoots), "~/src"), "colon-separated directories whose DIRECT children are the clones a spawned agent's worktree may be cut from ("+envAgentSpawnRoots+")")
	agentSpawnDir := fs.String("agent-spawn-dir", or(getenv(envAgentSpawnDir), defaultAgentSpawnDir(getenv)), "where spawned agents' worktrees go, <dir>/<owner>__<repo>/<dispatch> ("+envAgentSpawnDir+")")
	agentSpawnRepos := fs.String("agent-spawn-repos", getenv(envAgentSpawnRepos), "comma-separated <owner>/<repo> this host will spawn agents in. EMPTY BY DEFAULT: with none listed, -agent-spawn starts nothing ("+envAgentSpawnRepos+")")
	agentSpawnRate := fs.String("agent-spawn-rate", or(getenv(envAgentSpawnRate), mirrorhost.DefaultAgentSpawnRate), "this host's own spawn rate limit, counted from the audit log ("+envAgentSpawnRate+")")
	agentSpawnLog := fs.String("agent-spawn-log", or(getenv(envAgentSpawnLog), defaultAgentSpawnLog(getenv)), "the append-only spawn audit log (JSON lines, 0600): every request before any work starts, and its result; a spawn whose first line cannot be written is refused ("+envAgentSpawnLog+")")
	// Previews (preview.go): off by default; in this release -preview only
	// validates and logs.
	pf := previewFlags{}
	fs.BoolVar(&pf.On, "preview", envBool(envPreview), "PREVIEWS: serve an allowlisted loopback web server started in a mirrored window to its owner through the preview gateway; on the default tmux server's daemon only; a bad configuration turns previews off with a logged reason, never fatal ("+envPreview+")")
	fs.StringVar(&pf.URLs, "preview-urls", getenv(envPreviewURLs), "with -preview: the preview gateway's host-tunnel endpoints, ws:// or wss://, comma-separated IN DIAL ORDER (the overlay-mesh endpoint first, then the public one) ("+envPreviewURLs+")")
	fs.StringVar(&pf.TokenFile, "preview-token-file", getenv(envPreviewTokenFile), "with -preview: file holding this host's preview gateway token (mpt_…), not readable by group or others ("+envPreviewTokenFile+")")
	fs.StringVar(&pf.File, "preview-file", or(getenv(envPreviewFile), previewhost.DefaultFile()), "with -preview: previews.json, the allowlist {\"ports\": [\"5173\", \"8000-8099\"], \"offer\": true, \"maxOpen\": 8, \"maxConns\": 64}; ports 1024-65535 only; missing or invalid = previews off ("+envPreviewFile+")")
	listPreviews := fs.Bool("previews", false, "with -list: print the listener reports this host would offer from -preview-file — one line each: window id, port, process name, session — and dial nothing")
	agentSpawnTenantServer := fs.String("agent-spawn-tenant-server", getenv(envAgentSpawnTenantServer), "with -agent-spawn, on a TENANT's mirror (-tmux-socket/-tmux-socket-path naming a non-default tmux server): opt in to spawning on that server. The value must name the SAME server as this mirror's own -tmux-socket (a socket name) or -tmux-socket-path (an absolute path); anything else refuses every spawn. The tenant's tmux server must already be running: this host never starts it. Empty (the default) = spawn on the default tmux server only ("+envAgentSpawnTenantServer+")")
	version := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	// A leftover argument is a subcommand this binary does not have (one a
	// newer or older release had), or a typo: refused, never run as the
	// daemon it would otherwise start.
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "muster-mirror: unknown command or argument %q (subcommands: ask, opencode, opencode-pair, agent-spawn-log; see -h)\n", fs.Arg(0))
		return 2
	}
	if *version {
		fmt.Fprintf(stdout, "muster-mirror %s (tunnel protocol %d)\n", buildRevision, mirror.ProtocolVersion)
		return 0
	}

	ctx, stop := runContext()
	defer stop()
	logger := log.New(stderr, "", log.LstdFlags)
	// The opencode token (opencode.go): ONE Credentials for the source that
	// reads opencode windows and the renewal that keeps it valid.
	// A browser-test build reads no token file: its opencode is a fake server
	// of its own (e2eopencode.go).
	ocCreds, ocWhy := (*opencodehost.Credentials)(nil), "this is a browser-test build"
	if e2eBackend == nil {
		ocCreds, ocWhy = openCodeCredentials(*openCodeTokenFile)
		if ocCreds == nil && *openCodeReadOnly {
			// A reader keeps watching for the file (opencode.go).
			ocCreds = openCodePending(*openCodeTokenFile)
		}
	}
	ocSrc := mirrorhost.NewOpenCodeSource(ctx, ocCreds, ocWhy, logger)
	// The launcher records this daemon binds panes by: those naming ITS tmux
	// server (opencode_launch.go). A socket that cannot be derived binds none.
	if sock, err := tmuxSocketPath(*socket, *socketPath, getenv, os.Getuid()); err == nil && e2eBackend == nil {
		ocSrc.SetLaunches(opencodehost.LaunchDirDefault(getenv("HOME")), sock)
	}
	backend := withOpenCode(newTmuxBackend(*socket, *socketPath, *paneMapDir), ocSrc)
	if e2eBackend != nil {
		backend = e2eBackend()
	}
	// No attach shrinks a session's window; the browser scrolls instead
	// (internal/termsize). Wrapped AFTER the e2e swap, so the browser suite
	// drives the same wrapper the shipped binary runs.
	backend = mirrorhost.NoShrink(backend)
	if e2eWrap != nil {
		backend = e2eWrap(backend)
	}
	codenames := loadCodenames(*labelsFile, logger)

	if *catalog {
		cwd, _ := os.Getwd()
		c := (&mirrorhost.CatalogScanner{}).Scan(cwd)
		for _, e := range c.Commands {
			fmt.Fprintf(stdout, "%s\t%s\t/%s\t%s\n", e.Source, e.Kind, e.Name, e.Description)
		}
		if c.Truncated {
			fmt.Fprintln(stdout, "# truncated to the wire's bounds")
		}
		return 0
	}
	if *list && *listOpenCode {
		lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return printOpenCode(lctx, backend, ocCreds, ocWhy, stdout, stderr)
	}
	if *list && *listPreviews {
		lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return printPreviews(lctx, backend, pf, alwaysRefusedPorts(*mcpListen, ocCreds), previewProcRoot, stdout, stderr)
	}
	if *list && *listWindows {
		lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return printWindows(lctx, backend, codenames, false, stdout, stderr)
	}
	// -list -claude is PER WINDOW: a session's windows each run their own
	// Claude Code, and the rendered view resolves each on its own — a row per
	// session showed only the active window's.
	if _, ok := mirrorhost.WindowListerOf(backend); ok && *list && *listClaude {
		lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return printWindows(lctx, backend, codenames, true, stdout, stderr)
	}
	if *list {
		lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		sessions, err := backend.List(lctx)
		if err != nil {
			fmt.Fprintf(stderr, "muster-mirror: %v\n", err)
			return 1
		}
		var claude map[string]mirrorhost.ClaudePane
		if cb, ok := mirrorhost.ClaudeOf(backend); ok && *listClaude {
			claude = cb.ClaudePanes(lctx, sessions)
		}
		labels, lerr := mirrorhost.SessionLabels(lctx, backend, codenames, sessions)
		if lerr != nil {
			logger.Printf("muster-mirror: read active windows for labels (labelling by codename alone): %v", lerr)
		}
		for _, s := range sessions {
			// The label is the LAST column, so the columns before it keep
			// their positions whether or not a session has one.
			suffix := ""
			if l, ok := labels[s]; ok {
				suffix = "\tlabel=" + l
			}
			if p, ok := claude[s]; ok {
				state := "idle"
				if p.Busy {
					state = "busy"
				}
				fmt.Fprintf(stdout, "%s\tclaude\t%s\t%s\t%s\t%s\t%s%s\n", s, p.Pane, state, p.Gen, p.Transcript, p.Source, suffix)
				continue
			}
			fmt.Fprintln(stdout, s+suffix)
		}
		return 0
	}

	token := strings.TrimSpace(getenv(envToken))
	if *tokenFile != "" {
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			fmt.Fprintf(stderr, "muster-mirror: read the token file: %v\n", err)
			return 2
		}
		token = strings.TrimSpace(string(b))
	}
	if *url == "" || token == "" {
		fmt.Fprintf(stderr, "muster-mirror: -url (%s) and a token (-token-file / %s / %s) are required\n", envURL, envTokenFile, envToken)
		return 2
	}

	if skipPermsErr != nil {
		fmt.Fprintf(stderr, "muster-mirror: %s=%q is not a bool (1, t, T, TRUE, true, True, 0, f, F, FALSE, false or False)\n", envAgentSpawnSkipPermissions, getenv(envAgentSpawnSkipPermissions))
		return 2
	}
	spawnRepos, err := mirrorhost.ParseAgentSpawnRepos(*agentSpawnRepos)
	if err != nil {
		fmt.Fprintln(stderr, "muster-mirror:", err)
		return 2
	}
	if _, err := mirrorhost.ParseAgentSpawnTenantServer(*agentSpawnTenantServer); err != nil {
		fmt.Fprintln(stderr, "muster-mirror:", err)
		return 2
	}
	if _, err := mirrorhost.ParseAgentSpawnRate(*agentSpawnRate); err != nil {
		fmt.Fprintln(stderr, "muster-mirror:", err)
		return 2
	}
	var spawnRoots []string
	for _, r := range filepath.SplitList(*agentSpawnRoots) {
		if r = strings.TrimSpace(r); r != "" {
			spawnRoots = append(spawnRoots, expandHome(r))
		}
	}
	// The preview side's per-window counts (previews.go's previewSummary),
	// read on every refresh: nil until -preview's Host exists — and for good
	// when previews are off.
	var previewHost atomic.Pointer[previewhost.Host]
	previews := previewSummary(&previewHost)
	if e2ePreviews != nil {
		if f := e2ePreviews(); f != nil {
			previews = f
		}
	}
	// Built here, before mirrorhost.New, so the startup line below words the
	// permission mode from the very config the host spawns by.
	spawnCfg := mirrorhost.AgentSpawnConfig{Enabled: *agentSpawn, Roots: spawnRoots, Dir: expandHome(*agentSpawnDir),
		Repos: spawnRepos, Rate: *agentSpawnRate, Log: expandHome(*agentSpawnLog), Server: *url, RuntimeDir: getenv("XDG_RUNTIME_DIR"),
		TenantServer: *agentSpawnTenantServer, KeepPermissionPrompts: !*agentSpawnSkipPermissions, Geteuid: geteuid}
	c, err := mirrorhost.New(mirrorhost.Config{
		URL: *url, Token: token, Host: *host, Backend: backend, Codenames: codenames,
		Interactive: *interactive, OpenCodeSend: *openCodeSend, AllowHTTP: *allowHTTP, Logger: logger,
		ResumeSpawn: *resumeSpawn, ResumeSession: *resumeSession, ResumeClaude: *resumeClaude,
		AgentSpawn:    spawnCfg,
		NoPromptIndex: e2eNoPromptIndex != nil && e2eNoPromptIndex(),
		NoCatalog:     e2eNoCatalog != nil && e2eNoCatalog(),
		NoSubagents:   e2eNoSubagents != nil && e2eNoSubagents(),
		NoWork:        e2eNoWork != nil && e2eNoWork(),
		Previews:      previews,
		Refresh:       refreshOverride(),
		FakeSpawn:     e2eSpawn, FakeSpawnRuntimes: e2eSpawnRuntimes,
	})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	var mcpSettled chan struct{}
	var proxy *mirrorhost.MCPProxy
	var mcpUp atomic.Bool // the MCP endpoint is listening (startMCP succeeded)
	if *mcpListen != "" {
		// 🔴 OPT-IN, SO NEVER FATAL: a port in use, a secret file with the wrong
		// mode or anything else that stops the MCP endpoint is logged and the
		// host mirrors without it — mirroring is the job this process exists for.
		// 🔴 AND NEVER IN THE WAY: it runs on its own goroutine, so however
		// long setting it up takes, mirroring starts now.
		proxy = &mirrorhost.MCPProxy{URL: *url, Token: token, AllowHTTP: *allowHTTP, Logger: logger, Refresh: c.RefreshNow, Version: buildRevision}
		mcpSettled = make(chan struct{})
		go func() {
			defer close(mcpSettled)
			if err := startMCP(ctx, *mcpListen, *mcpSecretFile, backend, proxy, logger); err != nil {
				logger.Printf("muster-mirror: the MCP endpoint is OFF, mirroring continues without it: %v", err)
				return
			}
			mcpUp.Store(true)
		}()
	}
	var previewDone chan struct{}
	switch {
	case pf.On:
		// 🔴 OPT-IN, SO NEVER FATAL, AND NEVER IN THE WAY: like the MCP
		// endpoint above, previews run on their own goroutine with a recover;
		// a bad configuration, an unreachable gateway or a panic is logged and
		// the host mirrors without them.
		mcpMount := previewMCPMount(proxy, mcpSettled, &mcpUp, logger)
		previewDone = startPreview(ctx, pf, isDefaultDaemon(*socket, *socketPath, getenv), alwaysRefusedPorts(*mcpListen, ocCreds), *host, backend, logger, func(h *previewhost.Host) {
			previewHost.Store(h)
			if mcpMount != nil {
				mcpMount(h)
			}
		})
	case pf.URLs != "" || pf.TokenFile != "":
		logger.Printf("muster-mirror: -preview-urls / -preview-token-file have no effect without -preview: previews are off")
	}
	mode := "read-only"
	if *interactive {
		mode = "INTERACTIVE (-interactive)"
	}
	logger.Printf("muster-mirror %s: host %s → %s, %s", buildRevision, *host, *url, mode)
	switch {
	case *resumeSpawn && !*interactive:
		logger.Printf("muster-mirror: -resume-spawn has no effect without -interactive: this host resumes no session")
	case *resumeSpawn:
		logger.Printf("muster-mirror: RESUMING ENDED SESSIONS IS ON (-resume-spawn): muster may start `%s --resume <id>` in tmux session %q for a session whose transcript is on this host", *resumeClaude, *resumeSession)
	}
	switch {
	case *agentSpawn && !*interactive:
		logger.Printf("muster-mirror: -agent-spawn has no effect without -interactive: this host starts no agent")
	case *agentSpawn:
		if why := c.AgentSpawnUnavailable(); why != "" {
			logger.Printf("muster-mirror: -agent-spawn is set but this host starts no agent: %s", why)
		} else if len(spawnRepos) == 0 {
			logger.Printf("muster-mirror: -agent-spawn is ON but -agent-spawn-repos names no repository: this host starts no agent until you list one")
		} else {
			server := "the default tmux server"
			if *agentSpawnTenantServer != "" {
				server = fmt.Sprintf("the tenant tmux server %q (-agent-spawn-tenant-server)", *agentSpawnTenantServer)
			}
			perms := spawnPermissionsLog(spawnCfg)
			logger.Printf("muster-mirror: STARTING AGENTS IS ON (-agent-spawn): your chief may start claude (or opencode, with -opencode-send and a token) in tmux session %q on %s, in a new worktree of %s, rate %s, audit log %s, %s",
				"agents", server, strings.Join(spawnRepos, ", "), *agentSpawnRate, expandHome(*agentSpawnLog), perms)
		}
	}
	if *agentSpawnTenantServer != "" && !*agentSpawn {
		logger.Printf("muster-mirror: -agent-spawn-tenant-server has no effect without -agent-spawn: this host starts no agent")
	}
	switch {
	case *openCodeSend && !*interactive:
		logger.Printf("muster-mirror: -opencode-send has no effect without -interactive: muster sends nothing to opencode here")
	case *openCodeSend:
		logger.Printf("muster-mirror: SENDING TO OPENCODE IS ON (-opencode-send): muster may post a message to an opencode session here started with `muster-mirror opencode`, through the opencode server's API")
	}
	// Record where muster is for `muster-mirror ask` (hookconfig.go), whose
	// Claude Code hook does not inherit this service's environment — only when
	// told to (the managed service), and never in a browser-test build.
	//
	// Each record names the tmux server this daemon mirrors, DERIVED from its
	// flags (tmuxsocket.go) — the server may not exist yet — so that, with one
	// daemon per muster server, the hook picks the record of the caller's
	// tmux server (hookconfig.go). recordHookConfig refuses to let a daemon
	// that is not tenant default's own overwrite the legacy record when it
	// names another tmux server, and to write a per-tenant record
	// without a token file; each refusal is logged and never fatal.
	if *writeHook && e2eBackend == nil {
		hc := hookConfig{URL: *url, AllowHTTP: *allowHTTP}
		if *tokenFile != "" {
			if abs, err := filepath.Abs(expandHome(*tokenFile)); err == nil {
				hc.TokenFile = abs
			}
		}
		p := hookConfigPath(getenv)
		sock, err := tmuxSocketPath(*socket, *socketPath, getenv, os.Getuid())
		if err == nil {
			hc.TmuxSocket = sock
			err = recordHookConfig(p, hc, isDefaultDaemon(*socket, *socketPath, getenv))
		}
		if err != nil {
			logger.Printf("muster-mirror: could not record %s for `muster-mirror ask` (the hook then fails open for this daemon's sessions): %v", p, err)
		} else {
			logger.Printf("muster-mirror: recorded %s for `muster-mirror ask`: tmux server %s", p, sock)
		}
	}
	// A muster-mirror that was killed between creating a viewer session and
	// attaching to it left that session behind; nothing else would remove it.
	if vc, ok := mirrorhost.ViewCleanerOf(backend); ok {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		ended, err := vc.CleanViewSessions(cctx)
		cancel()
		if len(ended) > 0 {
			logger.Printf("muster-mirror: removed %d stale viewer session(s): %q", len(ended), ended)
		}
		if err != nil {
			logger.Printf("muster-mirror: clean up stale viewer sessions: %v", err)
		}
	}
	// The opencode token is renewed whether or not a window uses it, so a
	// paired host stays paired (opencode.go). Never in a browser-test build.
	if e2eBackend == nil {
		startOpenCode(ctx, ocCreds, ocWhy, *openCodeReadOnly, logger)
	}
	// Tracing (internal/telemetry): on only when an OTLP endpoint is set, as
	// service muster-mirror with this host's name, so a view's open can be
	// followed from the browser into this host's transcript read.
	flushTraces, terr := telemetry.Setup(ctx, getenv, telemetry.Options{Service: "muster-mirror", Version: buildRevision,
		Attributes: []attribute.KeyValue{attribute.String("host", *host)}, Logger: logger})
	if terr != nil {
		logger.Printf("muster-mirror: tracing stays off: %v", terr)
	}
	defer func() { _ = flushTraces(context.Background()) }()
	err = c.Run(ctx)
	// Give the MCP setup a moment to settle (and say why it is off) before
	// exiting; it never holds the exit longer than that.
	if mcpSettled != nil {
		select {
		case <-mcpSettled:
		case <-time.After(5 * time.Second):
		}
	}
	previewSettle(previewDone, 5*time.Second)
	if err != nil && !errors.Is(err, context.Canceled) {
		logger.Printf("muster-mirror: %v", err)
		return 1
	}
	return 0
}

// geteuid is the mirror's effective uid, handed to the spawn config
// (AgentSpawnConfig.Geteuid); a test replaces it to say "root".
var geteuid = os.Geteuid

// spawnPermissionsLog is the startup line's words for how spawned agents
// start, read from the config the host spawns by — so the line cannot say
// what the host does not do. As root, claude keeps its prompts whatever the
// setting says (AgentSpawnConfig.ClaudeSkipsPermissions), and the line says why.
func spawnPermissionsLog(sc mirrorhost.AgentSpawnConfig) string {
	switch {
	case sc.KeepPermissionPrompts:
		return "in normal permission mode (-agent-spawn-skip-permissions=false)"
	case !sc.ClaudeSkipsPermissions():
		return "claude in normal permission mode because this mirror runs as root (effective uid 0) and Claude Code refuses " +
			"bypassPermissions as root, opencode WITHOUT permission prompts (-agent-spawn-skip-permissions, --auto)"
	}
	return "WITHOUT permission prompts (-agent-spawn-skip-permissions): they run their tools without asking"
}

// runContext is the daemon's life: until SIGINT or SIGTERM. A test replaces it
// to end a daemon it started.
var runContext = func() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}

// newTmuxBackend is tmuxBackend; a test replaces it to see what run passes.
var newTmuxBackend = tmuxBackend

// tmuxBackend is the host's tmux, with its rendered-view lookup reading the pane
// map in paneMapDir ("" = none).
func tmuxBackend(socket, socketPath, paneMapDir string) mirrorhost.Backend {
	return mirrorhost.Tmux{Socket: socket, SocketPath: socketPath,
		Claude:     mirrorhost.ClaudeLocator{PaneMapDir: paneMapDir},
		LiveCopies: &mirrorhost.LiveCopies{}}
}

// printWindows is `-list -windows`: every window this host would register — or,
// with claudeOnly (`-list -claude`), each window whose active pane runs Claude
// Code, without the active/command columns.
func printWindows(ctx context.Context, backend mirrorhost.Backend, codenames map[string]string, claudeOnly bool, stdout, stderr io.Writer) int {
	wl, ok := mirrorhost.WindowListerOf(backend)
	if !ok {
		fmt.Fprintln(stderr, "muster-mirror: this backend lists sessions only, not windows")
		return 1
	}
	raw, err := wl.Windows(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "muster-mirror: %v\n", err)
		return 1
	}
	labels := map[string]string{}
	for _, w := range mirrorhost.WireWindows(raw, codenames, true, time.Now()) {
		labels[w.Session+"\x00"+w.ID] = w.Label
	}
	for _, w := range raw {
		active := "-"
		if w.Active {
			active = "active"
		}
		if claudeOnly && w.Claude == nil {
			continue
		}
		line := fmt.Sprintf("%s\t%s\t%d\t%s\t%s", w.Session, w.ID, w.Index, active, w.Command)
		if claudeOnly {
			line = fmt.Sprintf("%s\t%s\t%d", w.Session, w.ID, w.Index)
		}
		if p := w.Claude; p != nil {
			state := "idle"
			if p.Busy {
				state = "busy"
			}
			line += fmt.Sprintf("\tclaude\t%s\t%s\t%s\t%s\t%s", p.Pane, state, p.Gen, p.Transcript, p.Source)
		} else if p := w.Agent; p != nil && !claudeOnly {
			// A window of another runtime (opencode): its pane, its state as
			// the host reports it ("unknown" when muster does not believe one),
			// its session id ("-" when it has no rendered view; -list -opencode
			// says why) and how it was found.
			line += fmt.Sprintf("\t%s\t%s\t%s\t%s\t%s", p.Runtime, p.Pane, stateOrUnknown(p.Status), orDash(p.Gen), p.Source)
		}
		if l := labels[w.Session+"\x00"+w.ID]; l != "" {
			line += "\tlabel=" + l
		}
		fmt.Fprintln(stdout, line)
	}
	return 0
}

// loadCodenames reads the labels file at path ("" = none configured). It is
// NEVER fatal: a missing, unreadable or invalid file is logged once, here at
// start, and the host runs with no labels — muster then shows raw session
// names, exactly as before labels existed. Entries it had to drop are named.
func loadCodenames(path string, logger *log.Logger) map[string]string {
	if path == "" {
		return nil
	}
	codenames, skipped, err := mirrorhost.LoadCodenames(path)
	if err != nil {
		logger.Printf("muster-mirror: no session labels: %v", err)
		return nil
	}
	if len(skipped) > 0 {
		sort.Strings(skipped)
		logger.Printf("muster-mirror: the labels file %s has %d invalid entr(y/ies), ignored: %q", path, len(skipped), skipped)
	}
	logger.Printf("muster-mirror: %d session label(s) from %s", len(codenames), path)
	return codenames
}

// startMCP serves the local MCP endpoint (internal/mirrorhost's MCPProxy) on
// listen until ctx ends, completing proxy with the secret and the backend's
// window and tmux-server lookups. An error means the endpoint is not served.
func startMCP(ctx context.Context, listen, secretFile string, backend mirrorhost.Backend, proxy *mirrorhost.MCPProxy, logger *log.Logger) error {
	secret, err := mirrorhost.EnsureMCPSecret(secretFile)
	if err != nil {
		return err
	}
	l, err := mirrorhost.ListenMCP(listen)
	if err != nil {
		return err
	}
	proxy.Secret = secret
	proxy.Windows, _ = mirrorhost.WindowListerOf(backend)
	proxy.Socket, _ = mirrorhost.ServerSocketOf(backend)
	if src := mirrorhost.OpenCodeSourceOf(backend); src != nil {
		proxy.OpenCodeAncestors = src.Ancestors
	}
	logger.Printf("muster-mirror: MCP endpoint for this host's Claude Code sessions at http://%s/mcp (secret in %s)", l.Addr(), secretFile)
	go func() {
		if err := mirrorhost.ServeMCP(ctx, l, proxy); err != nil {
			logger.Printf("muster-mirror: MCP endpoint: %v", err)
		}
	}()
	return nil
}

// defaultAgentSpawnDir is $XDG_DATA_HOME/muster-mirror/agents (~/.local/share
// when XDG_DATA_HOME is unset).
func defaultAgentSpawnDir(getenv func(string) string) string {
	if d := getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "muster-mirror", "agents")
	}
	return filepath.Join("~", ".local", "share", "muster-mirror", "agents")
}

// defaultAgentSpawnLog is $XDG_STATE_HOME/muster-mirror/agent-spawn.jsonl
// (~/.local/state when XDG_STATE_HOME is unset).
func defaultAgentSpawnLog(getenv func(string) string) string {
	if d := getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "muster-mirror", "agent-spawn.jsonl")
	}
	return filepath.Join("~", ".local", "state", "muster-mirror", "agent-spawn.jsonl")
}

// runAgentSpawnLog is `muster-mirror agent-spawn-log [-n N] [-log PATH]`: the
// last N entries of the spawn audit log.
func runAgentSpawnLog(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("muster-mirror agent-spawn-log", flag.ContinueOnError)
	fs.SetOutput(stderr)
	n := fs.Int("n", 20, "how many of the last lines to print (two per request)")
	def := getenv(envAgentSpawnLog)
	if def == "" {
		def = defaultAgentSpawnLog(getenv)
	}
	path := fs.String("log", def, "the audit log ("+envAgentSpawnLog+")")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if err := mirrorhost.AgentSpawnLogTail(stdout, expandHome(*path), *n); err != nil {
		fmt.Fprintf(stderr, "muster-mirror agent-spawn-log: %v\n", err)
		return 1
	}
	return 0
}
