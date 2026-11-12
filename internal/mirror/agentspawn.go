package mirror

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// THE HOST SPAWN PROTOCOL (agent-spawn / agent-spawned): the wire and the
// server half. docs/design/chief-dispatch.md section 3; the host half is
// internal/mirrorhost/agentspawn.go.
//
// 🔴 IT COPIES spawn-resume (resume.go): a capability in the hello, a request
// and its answer on the connection's id space, and a host that decides what it
// starts. The request names a runtime, a repository, a base branch, the
// prompt and a display name; the HOST resolves the repository to its own
// clone, builds every argv word, and enforces its own allow-list, rate limit
// and audit log whatever the request says (5.5, a compromised server).
//
// NO VERSION BUMP, BACKWARD-COMPATIBLE BOTH WAYS: an older muster ignores the
// hello field and never sends a request; an older host never advertises it, so
// muster never sends it one.

const (
	// TypeAgentSpawn (muster → host): start an agent (Msg.Spawn) for host
	// dispatch Msg.Dispatch. ID is the request id (the stream id space).
	TypeAgentSpawn = "agent-spawn"
	// TypeAgentSpawned (host → muster): its outcome, always naming
	// Msg.Dispatch: Msg.Spawned on success, Msg.Refusal and Msg.Error on a
	// refusal.
	TypeAgentSpawned = "agent-spawned"
)

// The runtimes a host can spawn (Msg.AgentSpawn). opencode is advertised only
// beside Msg.OpenCodeSend (spawnRuntimesOf).
const (
	SpawnRuntimeClaude   = "claude"
	SpawnRuntimeOpenCode = "opencode"
)

// The refusal codes of an agent-spawned (Msg.Refusal), design 3.1. Each names
// the one thing the chief can do about it; Msg.Error is a fixed sentence.
const (
	SpawnUnavailable = "unavailable"
	SpawnNoRepo      = "no_repo"
	SpawnBadBase     = "bad_base"
	SpawnBadModel    = "bad_model"
	SpawnFailed      = "failed"
	// SpawnBadClone: the clone muster hinted (AgentSpawnReq.Clone) is not one
	// of the host's own clones of the repository under its spawn roots now.
	SpawnBadClone = "bad_clone"
	// SpawnNotListed is not a refusal: the window exists but has not been in a
	// listing on this connection yet. The row stays pending.
	SpawnNotListed = "not_listed"
)

// AgentSpawnSession is the tmux session every spawned agent's window opens in.
const AgentSpawnSession = "agents"

// DispatchOption is the tmux WINDOW option that names a spawned window's host
// dispatch (Window.Dispatch). A constant: nothing from outside the binary
// names another.
const DispatchOption = "@muster-dispatch"

// SpawnPrompt bounds.
const (
	// MaxSpawnPrompt is the prompt's limit in bytes (design 2.2: 8 KiB).
	MaxSpawnPrompt = 8 << 10
	// MaxSpawnName is the display name's limit before the host sanitises it.
	MaxSpawnName = 200
)

// SpawnPreface is how every prompt muster sends a spawned agent BEGINS.
//
// 🔴 IT IS LOAD-BEARING ON THE HOST, NOT DECORATION. Claude Code parses a
// positional word that equals a subcommand name (`update`, `mcp`, `doctor`) as
// that subcommand EVEN AFTER `--`: measured on 2.1.289, `claude --session-id
// <uuid> -- update` ran the updater (docs/design/chief-dispatch.md 7). So the
// host refuses any prompt that does not begin with this text, which is never a
// subcommand name and never begins with '-'.
const SpawnPreface = "muster: your owner's chief started you"

// THE MODEL OF AN opencode SPAWN (design 2.2, 3.6): an OpenRouter model id,
// <vendor>/<model>, which travels on the wire as openrouter/<vendor>/<model>
// and reaches opencode as Model.Ref {providerID: "openrouter", id:
// "<vendor>/<model>"} (upstream's Model.Ref.parse splits at the FIRST '/').
// ONE rule for the server's check, the configured model list and the host's
// check of what it is sent.

// SpawnModelProvider is the opencode provider id a spawn's model is run under:
// measured on opencode v2.0.32+6ec5682, OpenRouter's models are listed under
// providerID "openrouter" (GET /api/model).
const SpawnModelProvider = "openrouter"

// MaxSpawnModel bounds an OpenRouter model id, in bytes.
const MaxSpawnModel = 128

// spawnModelRE is an OpenRouter model id's shape: <vendor>/<model>, one '/'.
var spawnModelRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._:-]*$`)

// ValidSpawnModel reports whether id is an OpenRouter model id muster takes
// for an opencode spawn: the shape above, at most MaxSpawnModel bytes.
func ValidSpawnModel(id string) bool { return len(id) <= MaxSpawnModel && spawnModelRE.MatchString(id) }

// SpawnModelWire is id as an agent-spawn carries it: openrouter/<id>.
func SpawnModelWire(id string) string { return SpawnModelProvider + "/" + id }

// ParseSpawnModel reads an agent-spawn's model as opencode's Model.Ref does —
// the provider before the FIRST '/', the id after it — and reports whether it
// is an OpenRouter model id muster takes: the provider is exactly
// SpawnModelProvider and the id passes ValidSpawnModel. So "a/b/c" is provider
// "a", id "b/c", and refused.
func ParseSpawnModel(wire string) (provider, id string, ok bool) {
	provider, id, found := strings.Cut(wire, "/")
	return provider, id, found && provider == SpawnModelProvider && ValidSpawnModel(id)
}

// AgentSpawnReq is an agent-spawn's request (Msg.Spawn).
type AgentSpawnReq struct {
	Runtime string `json:"runtime"`
	// Repo is <owner>/<repo>; the host resolves it to one of its own clones.
	Repo string `json:"repo"`
	// Base is the branch the worktree is cut from; "" = the remote's default.
	Base string `json:"base,omitempty"`
	// Prompt is the first message, already cleaned and wrapped by muster
	// (beginning with SpawnPreface).
	Prompt string `json:"prompt"`
	// Name is the short display label, used only for the window name after
	// the host sanitises it.
	Name string `json:"name,omitempty"`
	// Model is opencode's openrouter/<vendor>/<model>; refused on claude.
	Model string `json:"model,omitempty"`
	// Clone is muster's HINT of which of the host's clones of Repo to cut the
	// worktree from: the main working tree's path EXACTLY as the host itself
	// reported it in an agent-clones-list (clones.go). "" = the host's own
	// rule (its only clone). A host that advertised AgentSpawnClones uses it
	// only when it is one of the clones it enumerates now, and refuses
	// (SpawnBadClone) anything else; an older host ignores the field and keeps
	// refusing a repository with several clones (no_repo), never guessing.
	Clone string `json:"clone,omitempty"`
}

// AgentSpawned is a successful agent-spawned's window (Msg.Spawned).
type AgentSpawned struct {
	Session      string `json:"session"`
	Window       string `json:"window"`
	Pane         string `json:"pane,omitempty"`
	Worktree     string `json:"worktree"`
	Branch       string `json:"branch"`
	AgentSession string `json:"agentSession"`
	// Warning is a step that did not finish but did not refuse the spawn
	// ("submodules incomplete").
	Warning string `json:"warning,omitempty"`
}

var (
	// ErrSpawnUnsupported: the host did not advertise the runtime.
	ErrSpawnUnsupported = errors.New("mirror: this host's muster-mirror does not start that runtime (it needs -interactive and -agent-spawn)")
	// ErrSpawnUnconfirmed: the host did not answer in time. The request was
	// written, so the window may exist: a late reply still lands
	// (SetSpawnReplyNotify), and nothing may treat this as a failure.
	ErrSpawnUnconfirmed = errors.New("mirror: the host did not answer the spawn in time; it may still have started the agent")
)

// SpawnRefusedError is a host's refusal: Code is one of the Spawn* codes,
// Detail its fixed sentence.
type SpawnRefusedError struct{ Code, Detail string }

func (e *SpawnRefusedError) Error() string { return "mirror: the host refused the spawn (" + e.Code + "): " + e.Detail }

// spawnTimeout is the server's wait for agent-spawned: the host's own deadline
// (120 s, from receipt) plus a 30 s margin for the frame to cross
// (docs/design/chief-dispatch.md 3.7, "Three deadlines").
const spawnTimeout = 150 * time.Second

// spawnWaitOverride, when set (tests), replaces spawnTimeout.
var spawnWaitOverride time.Duration

type spawnAnswer struct {
	w   AgentSpawned
	err error
}

// SpawnAgent asks owner's host to start an agent for host dispatch dispatch and
// waits for its answer. ErrSpawnUnconfirmed after the wait: the request WAS
// written. 🔴 ITS ONE CALLER is internal/api's toolAgentDispatch (a seam ledger
// pins that): no HTTP route or token reaches a host spawn.
func (h *Hub) SpawnAgent(ctx context.Context, owner int64, host string, dispatch int64, req AgentSpawnReq) (w AgentSpawned, err error) {
	ctx, done := hubOp(ctx, "agent-spawn")
	defer func() { done(err) }()
	if dispatch <= 0 {
		return AgentSpawned{}, errors.New("mirror: a spawn names its host dispatch")
	}
	h.mu.Lock()
	hc := h.hosts[hostKey{owner, host}]
	h.mu.Unlock()
	if hc == nil {
		return AgentSpawned{}, fmt.Errorf("%w (host %q)", ErrHostOffline, host)
	}
	if !hc.interactive || !slices.Contains(hc.agentSpawn, req.Runtime) {
		return AgentSpawned{}, fmt.Errorf("%w (host %q, runtime %q)", ErrSpawnUnsupported, host, req.Runtime)
	}
	hc.mu.Lock()
	hc.next++
	id := hc.next
	ch := make(chan spawnAnswer, 1)
	hc.spawns[id] = ch
	hc.mu.Unlock()
	defer func() {
		hc.mu.Lock()
		delete(hc.spawns, id)
		hc.mu.Unlock()
	}()
	r := req
	if err := writeJSON(ctx, hc.ws, Msg{Type: TypeAgentSpawn, ID: id, Dispatch: dispatch, Spawn: &r}); err != nil {
		return AgentSpawned{}, fmt.Errorf("mirror: send the spawn to the host: %w", err)
	}
	wait := spawnTimeout
	if spawnWaitOverride > 0 {
		wait = spawnWaitOverride
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case a := <-ch:
		return a.w, a.err
	case <-ctx.Done():
		return AgentSpawned{}, ErrSpawnUnconfirmed
	case <-t.C:
		return AgentSpawned{}, ErrSpawnUnconfirmed
	}
}

// SpawnReplyResult reads an agent-spawned frame: the window, or the refusal.
// A frame that names no valid window is a refusal of its own (failed).
func SpawnReplyResult(m Msg) (AgentSpawned, error) {
	if m.Refusal == SpawnNotListed {
		return AgentSpawned{}, &SpawnRefusedError{Code: SpawnNotListed, Detail: m.Error}
	}
	if m.Refusal != "" || m.Error != "" {
		code := m.Refusal
		if code == "" {
			code = SpawnFailed
		}
		return AgentSpawned{}, &SpawnRefusedError{Code: code, Detail: m.Error}
	}
	if m.Spawned == nil || ValidSession(m.Spawned.Session) != nil || ValidWindowID(m.Spawned.Window) != nil {
		return AgentSpawned{}, &SpawnRefusedError{Code: SpawnFailed, Detail: "the host named no valid window"}
	}
	return *m.Spawned, nil
}

// agentSpawned hands the host's answer to the spawn waiting on m.ID (if any)
// and to the late-reply hook, whether or not a waiter is still registered.
func (h *Hub) agentSpawned(hc *hostConn, m Msg) {
	w, err := SpawnReplyResult(m)
	hc.mu.Lock()
	ch := hc.spawns[m.ID]
	delete(hc.spawns, m.ID)
	hc.mu.Unlock()
	if ch != nil {
		ch <- spawnAnswer{w: w, err: err}
	}
	h.mu.Lock()
	fn := h.spawnReplyNotify
	h.mu.Unlock()
	if fn != nil && m.Dispatch > 0 {
		go fn(hc.key.owner, hc.key.host, m)
	}
}

// SetSpawnReplyNotify installs the late-reply hook: called (on its own
// goroutine) with the owner and host of the CONNECTION every agent-spawned
// frame arrived on, and the frame, whether or not a waiter still waits for it.
// The hook acts as that owner and checks the row is that host's: a host can
// settle only its own owner's dispatches addressed to it.
func (h *Hub) SetSpawnReplyNotify(fn func(owner int64, host string, m Msg)) {
	h.mu.Lock()
	h.spawnReplyNotify = fn
	h.mu.Unlock()
}

// SetListingNotify installs the hook called (on its own goroutine) with a
// host's owner, its name and its windows after every listing the hub applies
// (a hello's and each sessions message): the host-dispatch reconciler's input.
func (h *Hub) SetListingNotify(fn func(owner int64, host string, windows []Window)) {
	h.mu.Lock()
	h.listingNotify = fn
	h.mu.Unlock()
}

// listed calls the listing hook for hc's current windows.
func (h *Hub) listingApplied(hc *hostConn) {
	h.mu.Lock()
	fn := h.listingNotify
	h.mu.Unlock()
	if fn == nil {
		return
	}
	hc.mu.Lock()
	ws := slices.Clone(hc.windows)
	hc.mu.Unlock()
	go fn(hc.key.owner, hc.key.host, ws)
}

// spawnRuntimesOf is the runtimes a hello advertises that this muster knows,
// for an -interactive host only. opencode only beside hello.OpenCodeSend: the
// spawn posts the first prompt through the same API the gated send uses, so a
// host that does not send to opencode cannot start it (design 3.1).
func spawnRuntimesOf(hello Msg) []string {
	if !hello.Interactive {
		return nil
	}
	var out []string
	for _, r := range hello.AgentSpawn {
		if r == SpawnRuntimeOpenCode && !hello.OpenCodeSend {
			continue
		}
		if (r == SpawnRuntimeClaude || r == SpawnRuntimeOpenCode) && !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out
}

// ForDispatch stamps a paste with the host dispatch it is for (a chief's
// forward into a host-spawned window). The HOST then refuses it unless the
// window's own @muster-dispatch equals it (PasteRefusalNotMusterWindow): the
// real bound on forwarding (docs/design/chief-dispatch.md 3.10).
func ForDispatch(dispatch int64) PasteOption {
	return func(o *PasteOptions) { o.Dispatch = dispatch }
}

// PasteRefusalNotMusterWindow: a paste naming a dispatch reached a window whose
// own tag names another, or none. Nothing was typed.
const PasteRefusalNotMusterWindow = "not_muster_window"

// spawnSlugRE is a slug's allowed characters (window name and branch).
var spawnSlugRE = regexp.MustCompile(`[^a-z0-9]+`)

// SpawnSlug is name as a slug: lowercased, every run of other characters a
// single '-', trimmed of '-', at most max characters. ONE rule for the window
// name and the branch, on the host.
func SpawnSlug(name string, max int) string {
	s := spawnSlugRE.ReplaceAllString(stringsToLower(name), "-")
	s = trimDash(s)
	if len(s) > max {
		s = trimDash(s[:max])
	}
	return s
}

func stringsToLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

func trimDash(s string) string {
	for len(s) > 0 && s[0] == '-' {
		s = s[1:]
	}
	for len(s) > 0 && s[len(s)-1] == '-' {
		s = s[:len(s)-1]
	}
	return s
}
