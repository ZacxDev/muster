package mirror

import (
	"regexp"
	"slices"
	"strings"
)

// THE RUNTIME INTERFACE, SERVER HALF: what muster knows of each agent CLI a
// mirrored window can run, in ONE table. Every decision that used to compare
// a window's runtime to a name — which runtimes the wire admits, how a
// session is keyed, whether a window has a rendered view, how a send reaches
// it, what its status means, how its pending ask is cleaned, how a list names
// it — asks the window's RuntimeSpec instead, so a runtime is an entry here
// (and its host implementation, internal/mirrorhost's Runtime), not a branch
// at every site.
//
// Claude Code and opencode are its implementations. Claude
// Code is still special where the WIRE is: it travels as Window.Claude and
// ClaudeSession (never Window.Runtime), and its questions as askq entries.

// Capabilities is what muster can do with a window of a runtime. The host
// reads them here (LookupRuntime); tests in internal/mirrorhost pin that its
// implementations exist where these say.
type Capabilities struct {
	// View: muster shows the window's conversation in a rendered view.
	View bool
	// Send: muster sends a message into the window from a rendered view or
	// the queue (Claude Code: a paste).
	Send bool
	// Prompts: what the window waits on (a permission, an approval, a
	// question) reaches muster, as the window's Pending.
	Prompts bool
}

// RuntimeSpec is one agent runtime as muster's server sees it.
type RuntimeSpec struct {
	// Name is the runtime's wire name (RuntimeClaude, RuntimeOpenCode).
	Name string
	// Kind is what a list names a window of it (SessionView.Kind).
	Kind string
	// Caps is what muster can do with its windows.
	Caps Capabilities
	// OnWire: a host names a window of it as Window.Runtime, and
	// NormalizeWindows keeps that. Claude Code travels as Window.Claude
	// instead; a stub nothing reports is not on the wire.
	OnWire bool
	// KeyPrefix qualifies the window's AgentSession into its stored session
	// key ("opencode:"); "" for a runtime whose session is its own key (Claude
	// Code) or that names none.
	KeyPrefix string
	// ViewNeedsSession: a window of it has a rendered view only once its
	// host names the conversation (Window.AgentSession).
	ViewNeedsSession bool
	// Answers reads, from a host's hello, whether it answers this runtime's
	// asks itself. nil: it answers none.
	Answers func(hello Msg) bool
	// PendingNamesAsk: a window of it waits on a NAMED ask (Pending.Kind,
	// PromptID, Choices), and a Pending that names none is none. false: the
	// ask fields are not its own and are stripped.
	PendingNamesAsk bool
	// AskKinds are the asks (Pending.Kind) a window of it may name, when
	// PendingNamesAsk: a Pending naming another runtime's kind is none.
	AskKinds []string
}

// RuntimeOpenCode: opencode v2 (docs/design/opencode.md). A host whose pane
// runs an opencode TUI names the session the pane's title matches
// (Window.AgentSession), and muster draws its rendered view from the opencode
// server's API. It sends no message to it (the host's paste guard refuses its
// panes). What it WAITS ON reaches muster (Caps.Prompts): a permission request
// or a question-tool form of its session (or of a subagent's child session)
// makes the window waiting, its Pending naming the ask (PendingPermission,
// PendingQuestion) — and a host that advertised it (Msg.OpenCodeAnswer:
// an -interactive host with an opencode token) relays an answer through opencode's API
// (Hub.AnswerOpenCodeAsk), never keys.
const RuntimeOpenCode = "opencode"

// runtimes is every runtime muster knows, Claude Code first.
var runtimes = []RuntimeSpec{
	{
		Name: RuntimeClaude, Kind: KindClaudeCode,
		Caps: Capabilities{View: true, Send: true, Prompts: true},
	},
	{
		Name: RuntimeOpenCode, Kind: KindOpenCode,
		Caps:   Capabilities{View: true, Prompts: true},
		OnWire: true, KeyPrefix: RuntimeOpenCode + ":", ViewNeedsSession: true,
		// It answers asks (OpenCodeAnswer) and sends nothing.
		Answers:         func(m Msg) bool { return m.OpenCodeAnswer },
		PendingNamesAsk: true,
		AskKinds:        []string{PendingPermission, PendingQuestion},
	},
}

// LookupRuntime is the runtime named name, and whether muster knows it.
func LookupRuntime(name string) (RuntimeSpec, bool) {
	for _, r := range runtimes {
		if r.Name == name {
			return r, true
		}
	}
	return RuntimeSpec{}, false
}

// Runtimes is every runtime muster knows, Claude Code first.
func Runtimes() []RuntimeSpec { return append([]RuntimeSpec(nil), runtimes...) }

// wireRuntime is the runtime a host's Window.Runtime names, when muster keeps
// that name on the wire (OnWire).
func wireRuntime(name string) (RuntimeSpec, bool) {
	r, ok := LookupRuntime(name)
	return r, ok && r.OnWire
}

// AnswersOn reports whether h's host advertised that it answers r's asks —
// read from the HostView's hello flags through r.Answers, the one place a
// flag is mapped to its runtime.
func (h HostView) AnswersOn(r RuntimeSpec) bool {
	if r.Answers == nil {
		return false
	}
	return r.Answers(Msg{OpenCodeAnswer: h.OpenCodeAnswer})
}

// HasView reports whether a window of r whose host names agentSession has a
// rendered view.
func (r RuntimeSpec) HasView(agentSession string) bool {
	return r.Caps.View && (!r.ViewNeedsSession || agentSession != "")
}

// AgentKey is the stored session key of r's conversation agentSession: the
// prefix and the id, "" when r keys none or the window names none.
func (r RuntimeSpec) AgentKey(agentSession string) string {
	if r.KeyPrefix == "" || agentSession == "" {
		return ""
	}
	return r.KeyPrefix + agentSession
}

// SessionIDOf is the bare conversation id a stored session key names: a
// Claude Code id as it is, a runtime-qualified one without its KeyPrefix.
func SessionIDOf(key string) string {
	for _, r := range runtimes {
		if r.KeyPrefix != "" && strings.HasPrefix(key, r.KeyPrefix) {
			return key[len(r.KeyPrefix):]
		}
	}
	return key
}

// cleanPending is p as a window of r keeps it (CleanPending first).
func (r RuntimeSpec) cleanPending(p *Pending) *Pending {
	p = CleanPending(p)
	switch {
	case p == nil:
	case !r.PendingNamesAsk:
		// The named ask's fields are another runtime's: this one's Pending is
		// what it always was.
		c := *p
		c.Kind, c.Detail, c.PromptID, c.Choices = "", "", "", ""
		p = CleanPending(&c)
	case p.Kind == "" || !slices.Contains(r.AskKinds, p.Kind):
		// A window of a runtime that names its asks, naming none of its own:
		// none.
		p = nil
	}
	return p
}

// sessionKeyPattern is SessionKeyPattern compiled.
var sessionKeyPattern = regexp.MustCompile(SessionKeyPattern)

// SessionKeyPattern is every session key muster stores (SessionKey): a
// Claude session id as it always was (1-64 bytes), or a runtime's KeyPrefix
// and an id of 1-128 bytes, for each runtime that keys its sessions so.
// 🔴 IT IS SPELLED ONCE, HERE, FROM THE RUNTIME TABLE: internal/focus
// validates with it, and the CHECKs of agent_focus.subject_key,
// agent_focus_history.subject_key, session_routes.target_key and
// attention_asks.mirror_session carry the same expression — the migration
// that last wrote them is read by a test in internal/db that fails
// when the two differ (0085 last wrote them). A runtime that adds a
// KeyPrefix changes this pattern, so it fails that test until a NEW migration
// writes the wider CHECKs.
var SessionKeyPattern = buildSessionKeyPattern()

func buildSessionKeyPattern() string {
	alts := []string{`[0-9A-Za-z][0-9A-Za-z_-]{0,63}`}
	for _, r := range runtimes {
		if r.KeyPrefix != "" {
			alts = append(alts, regexp.QuoteMeta(r.KeyPrefix)+`[0-9A-Za-z][0-9A-Za-z_-]{0,127}`)
		}
	}
	return `^(` + strings.Join(alts, "|") + `)$`
}
