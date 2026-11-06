package mirrorhost

import (
	"context"

	"github.com/ZacxDev/muster/internal/mirror"
)

// THE RUNTIME INTERFACE, HOST HALF: one implementation per agent CLI this
// host knows — Claude Code and opencode — and every per-runtime decision the
// host makes goes through it, never through a comparison with a runtime's
// name:
//
//   - DETECTION, SESSION RESOLUTION AND STATUS: LocatePanes (every runtime);
//   - the RENDERED VIEW SOURCE: viewSource (Claude Code's transcript tail,
//     opencode's server API), chosen by the window a watch names;
//   - the PROMPT CAPABILITY: answerer, for a runtime whose answers are its
//     own (opencode's asks, by Msg.Runtime);
//   - the PASTE GUARD: pasteGuard, a runtime whose panes the host's one
//     typing path (typeInto) refuses (opencode).
//
// The server's half is mirror.RuntimeSpec; what muster may do with a
// runtime's windows is its Caps there (mirror.LookupRuntime), and tests pin
// that a host view source exists exactly where the server's table says one
// does.

// Runtime is one agent CLI this host knows.
type Runtime interface {
	// Name is the runtime's wire name: mirror.RuntimeClaude or
	// mirror.RuntimeOpenCode.
	Name() string
	// LocatePanes maps each of panes that this runtime CLAIMS — by the
	// process running in it, never by #{pane_current_command} — to the agent
	// session in it, keyed by pane id, reading the proc table procs/children
	// (readProcTable), which the caller reads ONCE for every runtime. The
	// tmux backend asks each runtime in turn, Claude Code first, and the first
	// to claim a pane has it.
	LocatePanes(panes []pane, procs map[int]procInfo, children map[int][]int) map[string]AgentPane
}

// AgentPane is one tmux pane running an agent CLI, of any runtime: ClaudePane
// under the name the seam uses. Its Runtime field says which; "" is Claude
// Code (every ClaudePane written before the seam).
type AgentPane = ClaudePane

// viewSource is a Runtime whose windows this host serves a rendered view of.
type viewSource interface {
	// viewer is the runtime's view source on h's connection: claims, read
	// NOW, whether the window a watch names is one of its windows with a
	// view; serve streams it until ctx ends; gone is why a view ends that
	// no runtime claims. ok is false when this host or connection has none.
	viewer(h *hostSide) (claims func(context.Context, mirror.Msg) bool, serve func(context.Context, mirror.Msg) error, gone error, ok bool)
}

// answerer is a Runtime whose asks are answered its own way (Msg.Runtime
// names it), never by keys into Claude Code's picker.
type answerer interface {
	answer(h *hostSide, m mirror.Msg)
}

// pasteGuard is a Runtime whose panes the host paste guard refuses
// (hostSide.guardPane): shape is the broad match on one process — which rule
// matched, and true — and refusal what the person reads, of kind
// refusalKind (Msg.Refusal). owns reports whether a rule is one of its.
type pasteGuard interface {
	shape(p procInfo) (rule string, ok bool)
	owns(rule string) bool
	refusal() error
	refusalKind() string
	// label is how the host's log names the runtime.
	label() string
}

// knownRuntimes is every runtime this host knows, as decisions not tied to a
// listing (routing a message, the paste guard, capabilities) ask them; the
// guard asks them in THIS order. The runtimes that claim panes, with their
// locators, are Tmux.runtimes.
var knownRuntimes = []Runtime{claudeRuntime{}, openCodeRuntime{}}

// runtimeNamed is the known runtime called name.
func runtimeNamed(name string) (Runtime, bool) {
	for _, r := range knownRuntimes {
		if r.Name() == name {
			return r, true
		}
	}
	return nil, false
}

// claudeRuntime is Claude Code behind the seam: exactly the code that ran
// before it, moved, not changed.
type claudeRuntime struct{ loc ClaudeLocator }

func (claudeRuntime) Name() string { return mirror.RuntimeClaude }

// viewer is Claude Code's transcript tail, for a window whose pane runs
// Claude Code NOW (claudePaneOf, re-resolved by the tail itself).
func (claudeRuntime) viewer(h *hostSide) (func(context.Context, mirror.Msg) bool, func(context.Context, mirror.Msg) error, error, bool) {
	cb, ok := h.claudeBackend()
	if !ok {
		return nil, nil, nil, false
	}
	return func(context.Context, mirror.Msg) bool { return true },
		func(ctx context.Context, m mirror.Msg) error { return h.tail(ctx, h.claudePaneOf(cb, m), m) }, nil, true
}

func (r claudeRuntime) LocatePanes(panes []pane, procs map[int]procInfo, children map[int][]int) map[string]AgentPane {
	return r.loc.locatePanesIn(panes, procs, children)
}

// openCodeRuntime is opencode behind the interface (opencode.go): it DETECTS
// an opencode TUI in a pane (openCodeTUI, on the pane's terminal) and, with an
// OpenCodeSource, resolves the session its pane title names. Its view is the
// session's messages from the opencode server's API; what it waits on (a
// permission request, a question) is the window's Pending, and its answerer
// relays an answer through that API (opencode_asks.go). It has no send, and
// the paste guard refuses its panes (openCodeShape).
type openCodeRuntime struct {
	proc  string
	src   *OpenCodeSource
	title func(paneID string) (string, bool)
}

func (openCodeRuntime) Name() string { return mirror.RuntimeOpenCode }

// viewer is the opencode tail (opencode_stream.go), for an opencode window
// whose host names its session, on a backend with an OpenCodeSource and a v2
// connection.
func (openCodeRuntime) viewer(h *hostSide) (func(context.Context, mirror.Msg) bool, func(context.Context, mirror.Msg) error, error, bool) {
	src, ok := h.openCodeSource()
	if !ok {
		return nil, nil, nil, false
	}
	claims := func(ctx context.Context, m mirror.Msg) bool { _, is := h.openCodeWindow(ctx, m); return is }
	return claims, func(ctx context.Context, m mirror.Msg) error { return h.openCodeTail(ctx, src, m) }, errNoOpenCodeViewNow, true
}

// answer relays an answer to an opencode ask through the opencode server
// (opencode_asks.go).
func (openCodeRuntime) answer(h *hostSide, m mirror.Msg) { h.answerOpenCode(m) }

func (openCodeRuntime) shape(p procInfo) (string, bool) {
	if openCodeShape(p) {
		return ruleOpenCode, true
	}
	return "", false
}

func (openCodeRuntime) owns(rule string) bool { return rule == ruleOpenCode }
func (openCodeRuntime) refusal() error        { return ErrOpenCodePane }
func (openCodeRuntime) refusalKind() string   { return mirror.PasteRefusalOpenCode }
func (openCodeRuntime) label() string         { return "opencode" }

// SourceProcess: an AgentPane found by its process alone, with no session
// resolved yet.
const SourceProcess = "process"

// runtimes is t's runtimes that LOCATE panes, in the order they claim one:
// Claude Code first, so a pane that runs both is the Claude window it always
// was; then opencode.
func (t Tmux) runtimes() []Runtime {
	return []Runtime{t.claudeRuntime(), t.openCodeRuntime()}
}

func (t Tmux) openCodeRuntime() openCodeRuntime {
	return openCodeRuntime{proc: t.Claude.proc(), src: t.OpenCode, title: t.paneTitle}
}

func (t Tmux) claudeRuntime() claudeRuntime { return claudeRuntime{loc: t.Claude} }

// attachAgents sets each window's Agent: the active pane's agent session of a
// runtime other than Claude Code, for a window whose pane Claude Code did not
// claim (attachClaude ran first). Each runtime is asked in turn for the panes
// still unclaimed, over the one proc table read; the first claim wins.
func attachAgents(windows []WindowInfo, procs map[int]procInfo, children map[int][]int, others []Runtime) {
	for _, rt := range others {
		var free []pane
		for _, w := range windows {
			if w.Claude == nil && w.Agent == nil {
				free = append(free, pane{Session: w.Session, ID: w.Pane, PID: w.PanePID, TTY: w.TTY})
			}
		}
		if len(free) == 0 {
			return
		}
		claimed := rt.LocatePanes(free, procs, children)
		for i := range windows {
			if windows[i].Claude != nil || windows[i].Agent != nil {
				continue
			}
			if ap, ok := claimed[windows[i].Pane]; ok {
				windows[i].Agent = &ap
			}
		}
	}
}
