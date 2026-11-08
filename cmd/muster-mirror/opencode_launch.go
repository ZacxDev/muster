package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ZacxDev/muster/internal/opencodehost"
	"github.com/ZacxDev/muster/internal/procstat"
)

// THE LAUNCHER: `muster-mirror opencode [flags] [<directory>]`
// (docs/design/opencode-first-class.md). It starts the opencode TUI on a
// session it NAMES, and records which, so this host's daemon can bind the pane
// to that session deterministically — the only way muster sends to opencode.
//
//  1. It refuses a TUI whose version differs from the running opencode
//     service: upstream's TUI would restart the service to match
//     (packages/cli/src/commands/handlers/default.ts, mismatch "replace"),
//     which signs other clients out (operator decision: refuse, naming both).
//  2. It chooses the session: -s <id> when given (created when it does not
//     exist, as opencode's own -s does), else a fresh id it creates on the
//     shared server FIRST, so the session takes a send before anyone types.
//  3. It writes the launcher record for $TMUX's server and $TMUX_PANE, naming
//     its own pid (which exec keeps) and start time, then execs
//     `opencode -s <id> …`.
//
// What it cannot bind — outside tmux, --standalone, --server, -c, no token, a
// server it cannot reach — it starts UNBOUND (the plain title-matched,
// view-only window), saying why on stderr; it never fails the start for that.
// An argument it does not know is refused, never passed on blind.

// launchDeps is what the launcher touches outside itself (tests replace it).
type launchDeps struct {
	getenv   func(string) string
	lookPath func(string) (string, error)
	// version runs `<bin> --version`.
	version func(bin string) (string, error)
	// exec replaces the process (syscall.Exec).
	exec  func(bin string, argv, env []string) error
	pid   func() int
	start func(pid int) (string, bool)
	now   func() time.Time
	cwd   func() (string, error)
}

func realLaunchDeps(getenv func(string) string) launchDeps {
	return launchDeps{getenv: getenv, lookPath: exec.LookPath,
		version: func(bin string) (string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, bin, "--version").Output()
			return strings.TrimSpace(string(out)), err
		},
		exec: syscall.Exec, pid: os.Getpid,
		start: func(pid int) (string, bool) {
			b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
			if err != nil {
				return "", false
			}
			st, ok := procstat.Parse(b)
			return st.Start, ok
		},
		now: time.Now, cwd: os.Getwd}
}

// launchArgs is the launcher's parsed arguments.
type launchArgs struct {
	session, prompt, logLevel, server, dir string
	auto, cont, standalone, printLogs      bool
}

// parseLaunchArgs reads the opencode TUI's flags at 6ec5682
// (packages/cli/src/commands/commands.ts) that the launcher passes on.
func parseLaunchArgs(args []string) (launchArgs, error) {
	var a launchArgs
	value := func(i *int, name string, inline string, has bool) (string, error) {
		if has {
			return inline, nil
		}
		if *i+1 >= len(args) {
			return "", fmt.Errorf("%s needs a value", name)
		}
		*i++
		return args[*i], nil
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, inline, has := strings.Cut(arg, "=")
		var err error
		switch {
		case name == "-s" || name == "--session":
			a.session, err = value(&i, name, inline, has)
		case name == "--prompt":
			a.prompt, err = value(&i, name, inline, has)
		case name == "--log-level":
			a.logLevel, err = value(&i, name, inline, has)
		case name == "--server":
			a.server, err = value(&i, name, inline, has)
		case arg == "--auto":
			a.auto = true
		case arg == "-c" || arg == "--continue":
			a.cont = true
		case arg == "--standalone":
			a.standalone = true
		case arg == "--print-logs":
			a.printLogs = true
		case strings.HasPrefix(arg, "-"):
			return a, fmt.Errorf("unknown flag %q (the launcher passes on only -s/--session, --prompt, --auto, -c/--continue, --standalone, --server, --log-level, --print-logs)", arg)
		case openCodeSubcommandNames[arg]:
			return a, fmt.Errorf("%q is an opencode subcommand, not the TUI: run `opencode %s` itself", arg, arg)
		case a.dir != "":
			return a, fmt.Errorf("a second directory %q", arg)
		default:
			a.dir = arg
		}
		if err != nil {
			return a, err
		}
	}
	return a, nil
}

// openCodeSubcommandNames are opencode's subcommands at 6ec5682.
var openCodeSubcommandNames = map[string]bool{
	"upgrade": true, "update": true, "uninstall": true, "acp": true, "api": true, "debug": true, "auth": true, "mcp": true,
	"plugin": true, "models": true, "stats": true, "mini": true, "run": true, "session": true, "service": true,
	"reload": true, "pair": true, "serve": true,
}

// argv is the TUI's argv for a: the session it is bound to (session "" for
// an unbound start keeps a's own) and every other flag as given.
func (a launchArgs) argv(session string) []string {
	out := []string{"opencode"}
	if session == "" {
		session = a.session
	}
	if session != "" {
		out = append(out, "--session", session)
	}
	if a.cont {
		out = append(out, "--continue")
	}
	if a.standalone {
		out = append(out, "--standalone")
	}
	if a.server != "" {
		out = append(out, "--server", a.server)
	}
	if a.auto {
		out = append(out, "--auto")
	}
	if a.prompt != "" {
		out = append(out, "--prompt", a.prompt)
	}
	if a.logLevel != "" {
		out = append(out, "--log-level", a.logLevel)
	}
	if a.printLogs {
		out = append(out, "--print-logs")
	}
	if a.dir != "" {
		out = append(out, a.dir)
	}
	return out
}

// normalizeOpenCodeVersion is opencodehost.NormalizeVersion: the ONE rule the
// launcher and the host spawn compare versions by.
func normalizeOpenCodeVersion(v string) string { return opencodehost.NormalizeVersion(v) }

// errVersionMismatch is the launcher's refusal of a mismatched TUI.
var errVersionMismatch = errors.New("opencode version mismatch")

// versionMismatchText names both versions and the fix (operator decision).
func versionMismatchText(tui, service string) string {
	return fmt.Sprintf("muster-mirror opencode: refused: the opencode TUI is %s but the running opencode service is %s. "+
		"Starting the TUI would restart the service to match, which can sign every other client out. "+
		"Run `opencode service restart` so the service runs the same version, then start it again.", tui, service)
}

// runOpenCodeLaunch is `muster-mirror opencode`.
func runOpenCodeLaunch(args []string, d launchDeps, stderr io.Writer) int {
	a, err := parseLaunchArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "muster-mirror opencode: %v\n", err)
		return 2
	}
	bin, err := d.lookPath("opencode")
	if err != nil {
		fmt.Fprintf(stderr, "muster-mirror opencode: no opencode on PATH: %v\n", err)
		return 1
	}
	unbound := func(why string) int {
		fmt.Fprintf(stderr, "muster-mirror opencode: starting opencode UNBOUND (muster views it by its title and cannot send to it): %s\n", why)
		return execOr(d, bin, a.argv(""), stderr)
	}
	// 🔴 THE VERSION CHECK RUNS ON EVERY PATH, bound or not, whenever the
	// host's opencode service is running (the token reaches it): a TUI of
	// another version would restart that service, whatever this pane is for.
	// Only a KNOWN mismatch refuses; a version that cannot be read is said and
	// the start goes on — a probe must not block the TUI.
	creds, credsWhy := openCodeCredentials(openCodeTokenFileDefault(d.getenv))
	var cl *opencodehost.Client
	service, serviceErr := "", errors.New(credsWhy)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if creds != nil {
		if cl, serviceErr = creds.Client(); serviceErr == nil {
			service, serviceErr = cl.Info(ctx)
		}
	}
	if serviceErr == nil {
		tuiOut, err := d.version(bin)
		switch {
		case err != nil:
			fmt.Fprintf(stderr, "muster-mirror opencode: warning: `opencode --version` failed (%v), so its version could not be checked against the running service's (%s); starting it anyway\n",
				err, normalizeOpenCodeVersion(service))
		case normalizeOpenCodeVersion(tuiOut) != normalizeOpenCodeVersion(service):
			fmt.Fprintln(stderr, versionMismatchText(normalizeOpenCodeVersion(tuiOut), normalizeOpenCodeVersion(service)))
			return 1
		}
	}
	tmuxEnv, pane := d.getenv("TMUX"), d.getenv("TMUX_PANE")
	socket, _, _ := strings.Cut(tmuxEnv, ",")
	switch {
	case tmuxEnv == "" || pane == "":
		return unbound("it is not running inside tmux")
	case !filepath.IsAbs(socket):
		return unbound("tmux's socket path " + strconv.Quote(socket) + " is not absolute")
	case a.standalone || a.server != "":
		return unbound("--standalone and --server run another server than this host's")
	case a.cont:
		return unbound("-c/--continue resumes a session the launcher cannot name in advance; pass -s <session id>")
	case creds == nil:
		return unbound(credsWhy)
	case serviceErr != nil:
		return unbound("the opencode service cannot be reached: " + serviceErr.Error())
	}
	dir := a.dir
	if dir == "" {
		if dir, err = d.cwd(); err != nil {
			return unbound("the working directory cannot be read: " + err.Error())
		}
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return unbound(err.Error())
	}
	session := a.session
	switch {
	case session == "":
		session = opencodehost.NewSessionID(d.now())
		if _, err := cl.CreateSession(ctx, session, dir, nil); err != nil {
			return unbound("the session could not be created: " + err.Error())
		}
	case !opencodehost.ValidSessionID(session):
		fmt.Fprintf(stderr, "muster-mirror opencode: %q is not an opencode session id\n", session)
		return 2
	default:
		s, err := cl.GetSession(ctx, session)
		switch {
		case errors.Is(err, opencodehost.ErrSessionNotFound):
			// opencode's own -s creates a session that does not exist.
			if _, err := cl.CreateSession(ctx, session, dir, nil); err != nil {
				return unbound("the session could not be created: " + err.Error())
			}
		case err != nil:
			return unbound("the session cannot be read: " + err.Error())
		case s.ParentID != "":
			return unbound("session " + session + " is a subagent's session, not a root session")
		}
	}
	pid := d.pid()
	start, ok := d.start(pid)
	if !ok {
		return unbound("this process's start time cannot be read")
	}
	rec := opencodehost.LaunchRecord{V: opencodehost.LaunchVersion, Socket: socket, Pane: pane, PID: pid, PIDStart: start,
		Session: session, Server: creds.URL(), Directory: dir, Created: d.now().Unix()}
	if err := opencodehost.WriteLaunch(opencodehost.LaunchDirDefault(d.getenv("HOME")), rec); err != nil {
		return unbound("the launcher record could not be written: " + err.Error())
	}
	fmt.Fprintf(stderr, "muster-mirror opencode: pane %s bound to opencode session %s\n", pane, session)
	return execOr(d, bin, a.argv(session), stderr)
}

// execOr execs bin with argv; it returns only when that fails.
func execOr(d launchDeps, bin string, argv []string, stderr io.Writer) int {
	if err := d.exec(bin, argv, os.Environ()); err != nil {
		fmt.Fprintf(stderr, "muster-mirror opencode: could not start opencode: %v\n", err)
		return 1
	}
	return 0
}
