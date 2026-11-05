// Package features is muster's feature registry: every optional (and core)
// feature, whether this server runs it, why not, and the exact .env line(s)
// that turn it on — written for the documented install, the Compose stack
// (docs/features.md).
//
// 🔴 THIS PACKAGE HOLDS THE SHAPE AND THE IDS, NOT THE PREDICATES. Whether a
// feature is on is decided by cmd/muster-server from the SAME wired extensions
// and the SAME parsed config the server itself runs on (its features.go), and
// handed to internal/api once, at boot, as a Set.
//
// ⚠ WHAT READS THE SET, EXACTLY (internal/api features.go has the list beside
// the code): the boot log's summary line, /api/doctor's `features` (and so
// `muster doctor`), the Features page, the Account page's suggested-replies
// note, the Credentials page's variable name, the MCP doors' "disabled"
// answers and muster_help's disabled_features, and the typed feature_disabled
// 404. The GATES — whether a microphone or paperclip is drawn, whether the
// Connectors tab, the push button and the dispatch "+" are drawn, and which
// MCP tools are listed — read their extension AND the Set (Server.featureOff):
// the Set can only take a feature away, so a feature the registry calls OFF is
// never offered. A MISCONFIGURED one keeps what is wired (its store and tools
// work as far as they go) and is reported where its gap is true: `muster
// doctor` FAILs on it, and the Features page and the banner say so. Route handlers still gate on
// their own extension (each answers its own unwired case); the ones the CLI
// and the composer reach answer the typed 404.
//
// The variable names are spelled in cmd/muster-server's config.go and nowhere
// else; this package never names a variable.
package features

import (
	"fmt"
	"sort"
	"strings"
)

// The feature ids. They are also the anchors of docs/features.md
// (DocsRef), so renaming one breaks every link to it: don't.
const (
	Mirror               = "mirror"
	PasswordSignIn       = "password-sign-in"
	Push                 = "push"
	Dictation            = "dictation"
	Attachments          = "attachments"
	SuggestedReplies     = "suggested-replies"
	Focus                = "focus"
	Accounts             = "accounts"
	Connectors           = "connectors"
	GitHubApp            = "github-app"
	Dispatch             = "dispatch"
	AgentGateway         = "agent-gateway"
	ClaudeCodeAgents     = "claude-code-agents"
	Chief                = "chief"
	ChiefSessionRuntimes = "chief-session-runtimes"
	ChiefNudges          = "chief-nudges"
	SessionState         = "session-state"
	Plugins              = "plugins"
	PluginTasks          = "plugin-tasks"
	SessionRouting       = "session-routing"
	TaskRouting          = "task-routing"
	PRMailRouting        = "pr-mail-routing"
	FleetReports         = "fleet-reports"
	MailResume           = "mail-resume"
	MemoRelay            = "memo-relay"
	ApprovalRouter       = "approval-router"
	BrowserFlows         = "browser-flows"
	AgentAskTool         = "agent-ask-tool"
	ServiceToken         = "service-token"
	Android              = "android"
	Telemetry            = "telemetry"
	Officers             = "officers"
)

// DocsPage is the document whose headings are the feature ids.
const DocsPage = "docs/features.md"

// DocsURL is DocsPage on GitHub, for a link a browser can follow.
const DocsURL = "https://github.com/ZacxDev/muster/blob/main/" + DocsPage

// State is a feature's state on this server.
type State string

const (
	// On: the feature runs.
	On State = "on"
	// Off: the feature is not configured. Nothing is wrong; it is optional.
	Off State = "off"
	// Misconfigured: the feature was asked for (or is on by default) but a
	// dependency it needs is missing, so it does not do what it says — e.g.
	// suggested replies with no OpenRouter key.
	Misconfigured State = "misconfigured"
)

// Var is one .env line a feature needs. Value is a fixed value the variable
// must have ("1", "gotrue"); Placeholder describes a value the operator
// supplies ("a 32-byte base64 key"). At most one is set.
//
// Profile names a Compose profile instead: the line is COMPOSE_PROFILES=<it>
// (Name is then "COMPOSE_PROFILES"). Compose marks a variable the Compose
// stack reads and muster-server does not (S3_STORE_SECRET_KEY): it is a real
// .env line, and the registry's ledger against docs/configuration.md skips it.
type Var struct {
	Name        string `json:"name"`
	Value       string `json:"value,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
	Profile     string `json:"profile,omitempty"`
	Compose     bool   `json:"compose,omitempty"`
}

// ComposeProfilesVar is the variable a profile line sets.
const ComposeProfilesVar = "COMPOSE_PROFILES"

// ProfileVar is the .env line that turns a Compose profile on.
func ProfileVar(profile string) Var {
	return Var{Name: ComposeProfilesVar, Value: profile, Profile: profile, Compose: true}
}

// Line is the variable as one .env line: NAME=value, or NAME=<placeholder>.
func (v Var) Line() string {
	switch {
	case v.Value != "":
		return v.Name + "=" + v.Value
	case v.Placeholder != "":
		return v.Name + "=<" + v.Placeholder + ">"
	}
	return v.Name + "="
}

// hintName is the variable as a sentence names it: with its fixed value when
// it has one ("MUSTER_PLUGINS=1"), otherwise by name alone.
func (v Var) hintName() string {
	if v.Value != "" {
		return v.Name + "=" + v.Value
	}
	return v.Name
}

// Feature is one registry entry: what the catalog says about it (ID through
// HostSide) and, once evaluated, its State and Reason.
//
// 🔴 IT CARRIES VARIABLE NAMES, NEVER VALUES. Reason is written by the
// evaluator and must not quote a configured value either; the evaluator's test
// feeds every variable a distinctive value and fails if one comes out.
type Feature struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// Core: part of the documented install (the Compose stack, its Postgres,
	// muster-mirror and tmux). Everything else is optional.
	Core bool `json:"core"`
	// Enable is what turns the feature on, as .env lines.
	Enable []Var `json:"enable"`
	// Settings are the other variables that tune the feature (by name). They
	// are not needed to turn it on.
	Settings []string `json:"settings,omitempty"`
	// Note is what turning it on takes beyond the variables (a runner to host,
	// a host to configure). "" for none.
	Note string `json:"note,omitempty"`
	// HostSide: the feature is set up on each machine running muster-mirror,
	// not in the hub's .env.
	HostSide bool `json:"host_side,omitempty"`
	// Kubernetes: the feature needs in-cluster dispatch, which the Compose
	// stack cannot provide (it does not forward these variables). Its hint
	// then says so instead of "set X in .env", which would not work there.
	// Enable still lists the variables, for a Kubernetes deployment.
	Kubernetes bool `json:"kubernetes,omitempty"`

	State State `json:"state"`
	// Reason is why the feature is off or misconfigured ("" when on), or, for
	// a feature that is on, what it still depends on ("" when nothing).
	Reason string `json:"reason,omitempty"`
}

// Docs is the feature's docs reference, e.g. docs/features.md#dictation.
func (f Feature) Docs() string { return DocsPage + "#" + f.ID }

// DocsLink is Docs as a URL a browser can follow.
func (f Feature) DocsLink() string { return DocsURL + "#" + f.ID }

// EnableLines is Enable as .env lines.
func (f Feature) EnableLines() []string {
	out := make([]string, 0, len(f.Enable))
	for _, v := range f.Enable {
		out = append(out, v.Line())
	}
	return out
}

// EnableHint is the one sentence that says how to turn the feature on:
//
//	set MUSTER_STT_URL and MUSTER_STT_TOKEN in .env and restart (docs/features.md#dictation)
//
// A Kubernetes feature's hint never says "set X in .env":
//
//	needs Kubernetes (in-cluster dispatch) — see docs/features.md#chief
func (f Feature) EnableHint() string {
	if f.Kubernetes {
		return KubernetesHint + " — see " + f.Docs()
	}
	names := make([]string, 0, len(f.Enable))
	for _, v := range f.Enable {
		names = append(names, v.hintName())
	}
	var b strings.Builder
	switch {
	case len(names) > 0 && f.HostSide:
		b.WriteString("on each host, set " + joinAnd(names) + " in muster-mirror's environment and restart it")
	case len(names) > 0:
		b.WriteString("set " + joinAnd(names) + " in .env and restart")
	}
	if f.Note != "" {
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		b.WriteString(f.Note)
	}
	b.WriteString(" (" + f.Docs() + ")")
	return b.String()
}

// DisabledMessage is what a surface answers when a caller reaches for this
// feature while it is not on:
//
//	disabled: dictation — set MUSTER_STT_URL and MUSTER_STT_TOKEN in .env and restart (docs/features.md#dictation)
//
// A misconfigured feature's reason comes first, since it names what is
// actually missing.
func (f Feature) DisabledMessage() string {
	word := "disabled"
	if f.State == Misconfigured {
		word = "misconfigured"
	}
	msg := word + ": " + f.ID + " — "
	if f.State == Misconfigured && f.Reason != "" {
		msg += f.Reason + "; "
	}
	return msg + f.EnableHint()
}

// KubernetesHint is what a feature the Compose stack cannot run needs.
const KubernetesHint = "needs Kubernetes (in-cluster dispatch)"

func joinAnd(xs []string) string {
	switch len(xs) {
	case 0:
		return ""
	case 1:
		return xs[0]
	}
	return strings.Join(xs[:len(xs)-1], ", ") + " and " + xs[len(xs)-1]
}

// Status is the evaluated half of an entry.
type Status struct {
	State  State
	Reason string
}

// Set is the evaluated registry, in catalog order.
//
// The zero Set holds nothing: a server that was never handed one reports no
// feature (and its consumers fall back to what they did before the registry).
type Set struct {
	list []Feature
	byID map[string]int
}

// Evaluate joins a catalog with its statuses.
//
// 🔴 IT IS A TWO-WAY LEDGER, NOT A LOOKUP. An entry with no status, a status
// for no entry, a duplicate id, an unknown state, a feature with no way to turn
// it on, or an "on" feature with no state at all is an error — so adding a
// feature to the catalog without deciding its state (or the reverse) fails the
// boot of every test that builds the registry rather than quietly reading
// "off".
func Evaluate(catalog []Feature, statuses map[string]Status) (Set, error) {
	s := Set{byID: make(map[string]int, len(catalog))}
	for _, f := range catalog {
		if f.ID == "" || f.Name == "" || f.Description == "" {
			return Set{}, fmt.Errorf("features: an entry is missing its id, name or description: %+v", f)
		}
		if _, dup := s.byID[f.ID]; dup {
			return Set{}, fmt.Errorf("features: %q is in the catalog twice", f.ID)
		}
		if len(f.Enable) == 0 && f.Note == "" && !f.Core {
			return Set{}, fmt.Errorf("features: %q is optional and says nothing about how to turn it on", f.ID)
		}
		st, ok := statuses[f.ID]
		if !ok {
			return Set{}, fmt.Errorf("features: %q has no evaluated state", f.ID)
		}
		switch st.State {
		case On, Off, Misconfigured:
		default:
			return Set{}, fmt.Errorf("features: %q has state %q, not on, off or misconfigured", f.ID, st.State)
		}
		if st.State != On && st.Reason == "" {
			return Set{}, fmt.Errorf("features: %q is %s with no reason", f.ID, st.State)
		}
		f.State, f.Reason = st.State, st.Reason
		s.byID[f.ID] = len(s.list)
		s.list = append(s.list, f)
	}
	var extra []string
	for id := range statuses {
		if _, ok := s.byID[id]; !ok {
			extra = append(extra, id)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return Set{}, fmt.Errorf("features: state given for %s, which the catalog does not list", strings.Join(extra, ", "))
	}
	return s, nil
}

// All is every feature, in catalog order. The slice is a copy.
func (s Set) All() []Feature { return append([]Feature(nil), s.list...) }

// Len is how many features the set holds.
func (s Set) Len() int { return len(s.list) }

// Get is one feature by id.
func (s Set) Get(id string) (Feature, bool) {
	i, ok := s.byID[id]
	if !ok {
		return Feature{}, false
	}
	return s.list[i], true
}

// NotOn is every feature that is off or misconfigured, in catalog order.
func (s Set) NotOn() []Feature {
	var out []Feature
	for _, f := range s.list {
		if f.State != On {
			out = append(out, f)
		}
	}
	return out
}

// Counts is how many features are in each state.
func (s Set) Counts() map[State]int {
	out := map[State]int{On: 0, Off: 0, Misconfigured: 0}
	for _, f := range s.list {
		out[f.State]++
	}
	return out
}

// Summary is the boot log's one registry line:
//
//	features: 9 on; off: dictation, attachments; misconfigured: none — see /account/features
//
// The per-feature detail is on the Features page and in `muster doctor`; the
// banner's own posture lines say the rest.
func (s Set) Summary(pagePath string) string {
	var off, bad []string
	on := 0
	for _, f := range s.list {
		switch f.State {
		case On:
			on++
		case Off:
			off = append(off, f.ID)
		case Misconfigured:
			bad = append(bad, f.ID)
		}
	}
	list := func(xs []string) string {
		if len(xs) == 0 {
			return "none"
		}
		return strings.Join(xs, ", ")
	}
	return fmt.Sprintf("features: %d on; off: %s; misconfigured: %s — see %s", on, list(off), list(bad), pagePath)
}
