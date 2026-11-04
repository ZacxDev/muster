package api

import (
	"html"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/features"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE FEATURE REGISTRY'S CONSUMERS (internal/features).
//
// cmd/muster-server evaluates the registry once, from what it wired, and hands
// it here with UseFeatures. Everything below reads that one Set:
//
//   - GET /api/features: the JSON list (session tier).
//   - GET /account/features: the Features page, one row per feature with its
//     state and the exact .env line(s) that turn it on; every hint beside a
//     hidden control links to its row.
//   - /api/doctor's `features` (doctor.go), which `muster doctor` prints.
//   - the MCP doors' answer to a tool whose feature is off (mcpDisabledTool),
//     and muster_help's list of the features that are off (mcp_help.go).
//   - the typed 404 a route answers when its feature is off
//     (writeFeatureDisabled), which the CLI reports as "feature X is disabled".
//
// A server that was never handed a Set (most tests) serves an empty list and
// answers every feature-off route exactly as it did before the registry.

// FeaturesPath is the JSON list; FeaturesPagePath is the page.
const (
	FeaturesPath     = "/api/features"
	FeaturesPagePath = ui.FeaturesPagePath
)

// FeatureDisabledError is the `error` of the typed answer a route gives when
// the feature behind it is off: the CLI branches on this value, never on the
// sentence beside it.
const FeatureDisabledError = "feature_disabled"

// UseFeatures installs the evaluated feature registry. Call before Handler.
func (s *Server) UseFeatures(set features.Set) { s.features = set }

// Features is the installed registry (the zero Set when none was installed).
func (s *Server) Features() features.Set { return s.features }

// FeatureView is one feature as GET /api/features (and /api/doctor) serve it:
// the registry entry plus what is derived from it, so a client needs no copy of
// the docs-anchor or hint rules.
type FeatureView struct {
	features.Feature
	Docs        string   `json:"docs"`
	EnableLines []string `json:"enable_lines"`
	EnableHint  string   `json:"enable_hint"`
}

// FeatureViews is set as the API serves it.
//
// 🔴 IT IS THE ONE SERIALISATION. The handler, the doctor and the leak test
// (cmd/muster-server, TestTheFeatureRegistryLeaksNoConfiguredValue) all call
// it, so what the test scans is byte for byte what a client is sent.
func FeatureViews(set features.Set) []FeatureView {
	all := set.All()
	out := make([]FeatureView, 0, len(all))
	for _, f := range all {
		out = append(out, FeatureView{Feature: f, Docs: f.Docs(), EnableLines: f.EnableLines(), EnableHint: f.EnableHint()})
	}
	return out
}

func (s *Server) registerFeatureRoutes(mux Mux) {
	mux.HandleFunc("GET "+FeaturesPath, s.requireSession(s.handleFeatures))
	mux.HandleFunc("GET "+FeaturesPagePath, s.requireSession(s.handleFeaturesPage))
}

// featuresReport is GET /api/features' body.
type featuresReport struct {
	Features []FeatureView  `json:"features"`
	Counts   map[string]int `json:"counts"`
	// SessionTools is what this server has seen of the per-host setup behind
	// agent-ask-tool, for the caller's own hosts (sessionToolHosts).
	SessionTools []SessionToolHost `json:"session_tools"`
	// SessionToolsNote says what SessionTools can and cannot show.
	SessionToolsNote string `json:"session_tools_note"`
}

func (s *Server) handleFeatures(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	counts := map[string]int{}
	for st, n := range s.features.Counts() {
		counts[string(st)] = n
	}
	s.writeJSON(w, http.StatusOK, featuresReport{Features: FeatureViews(s.features), Counts: counts,
		SessionTools: s.sessionToolHosts(r), SessionToolsNote: SessionToolsNote})
}

// feature is one feature of the installed registry.
func (s *Server) feature(id string) (features.Feature, bool) { return s.features.Get(id) }

// featureOn reports whether the registry says id is on. With no registry it
// answers true: no consumer may invent an "off" the server never decided.
func (s *Server) featureOn(id string) bool {
	f, ok := s.feature(id)
	return !ok || f.State == features.On
}

// writeFeatureDisabled answers a request for a route whose feature is off:
// 404, with a body that says which feature and how to turn it on — JSON for a
// client, the notice page for a person who typed the address.
//
// 🔴 404, NOT 409: every one of these routes answered 404 while its feature was
// off before the registry existed, and a caller that branches on the status
// keeps working. What changes is that the body is now typed, so the CLI can
// tell "this server has the route and the feature is off" from "this server
// predates the route" (a bare mux 404, text/plain).
func (s *Server) writeFeatureDisabled(w http.ResponseWriter, r *http.Request, id string) {
	body := map[string]any{"error": FeatureDisabledError, "feature": id, "docs": features.DocsPage + "#" + id}
	msg := "disabled: " + id + " (" + features.DocsPage + "#" + id + ")"
	name := id
	if f, ok := s.feature(id); ok {
		msg, name = f.DisabledMessage(), f.Name
		body["name"], body["state"], body["enable"], body["message"] = f.Name, f.State, f.EnableHint(), msg
		if f.Reason != "" {
			body["reason"] = f.Reason
		}
	}
	if isDocumentNavigation(r) {
		s.renderNotice(w, r, http.StatusNotFound, name+" is off on this server", msg)
		return
	}
	s.writeJSON(w, http.StatusNotFound, body)
}

// --- the MCP doors -------------------------------------------------------------

// toolFeatures is the feature behind each MCP tool that a feature gates, read
// off the tools' own definitions so no name is spelled twice. A tool missing
// from it (the attention and task tools, which need only the database) is not
// gated by any optional feature.
func toolFeatures() map[string]string {
	m := map[string]string{}
	add := func(id string, defs ...agents.ToolDef) {
		for _, d := range defs {
			m[d.Name] = id
		}
	}
	add(features.Connectors, connectorToolDefs()...)
	add(features.Connectors, connectorDecideToolDefs()...)
	add(features.BrowserFlows, flowToolDefs()...)
	add(features.Plugins, gatedTaskToolDef(), replyDraftToolDef(), pluginMessageSettleToolDef())
	add(features.Plugins, pluginDraftToolDefs()...)
	add(features.Focus, focusToolDefs()...)
	add(features.Focus, sessionsOverviewToolDef(), fleetReportToolDef())
	add(features.SessionRouting, routeToolDef(), routeReplyToolDef())
	add(features.SessionRouting, pluginMessageRouteToolDefs()...)
	add(features.TaskRouting, taskRoutingToolDefs(true)...)
	add(features.Officers, officerToolDefs()...)
	add(features.Chief, chiefAskToolDef())
	add(features.Dispatch, chiefRestartToolDef())
	return m
}

// mcpDisabledTool is the explanation for a call to a tool this door does not
// list because its feature is off: "disabled: <feature> — set … (docs/…)". ""
// when the tool is not gated by a feature, when its feature is on (it is then
// unlisted for another reason — the caller is not the chief, say — and stays
// an unknown name), or when no registry is installed.
func (s *Server) mcpDisabledTool(name string) string {
	id, gated := toolFeatures()[name]
	if !gated {
		return ""
	}
	f, ok := s.feature(id)
	if !ok || f.State == features.On {
		return ""
	}
	return f.DisabledMessage()
}

// mcpOffFeatures is every feature that gates a muster tool and is not on: what
// muster_help lists so an agent knows what this server leaves out and how to
// turn it on.
func (s *Server) mcpOffFeatures() []mcpOffFeature {
	gating := map[string][]string{}
	for tool, id := range toolFeatures() {
		gating[id] = append(gating[id], tool)
	}
	var out []mcpOffFeature
	for _, f := range s.features.NotOn() {
		tools, ok := gating[f.ID]
		if !ok {
			continue
		}
		sort.Strings(tools)
		out = append(out, mcpOffFeature{Feature: f.ID, State: string(f.State), Tools: tools, Enable: f.DisabledMessage()})
	}
	return out
}

// mcpOffFeature is one row of muster_help's `disabled_features`.
type mcpOffFeature struct {
	Feature string   `json:"feature"`
	State   string   `json:"state"`
	Tools   []string `json:"tools"`
	Enable  string   `json:"enable"`
}

// --- the per-host setup behind agent-ask-tool ------------------------------------

// SessionToolsNote is what the server can and cannot tell about the per-host
// setup that gives a mirrored Claude Code session muster's tools.
const SessionToolsNote = "Seen = this server received a call from that host since it started: on its MCP door " +
	"(POST /mirror/mcp, from muster-mirror -mcp-listen, which Claude Code calls when a session starts or uses a muster tool) " +
	"or from its AskUserQuestion hook (POST /mirror/ask, when a session asks a question). Not seen means the host is not " +
	"set up, OR no session there has started or used a tool since this server's restart — the server cannot tell those apart. " +
	"Whether Claude Code's MCP config and the PreToolUse hook are installed is never visible to the server."

// sessionToolSeen records, per owner and host, when this process last received
// a call on a mirrored host's MCP door and on its ask hook. In memory: a
// restart forgets it, which SessionToolsNote says.
type sessionToolSeen struct {
	mu   sync.Mutex
	mcp  map[ownerHost]time.Time
	hook map[ownerHost]time.Time
}

type ownerHost struct {
	owner int64
	host  string
}

func (t *sessionToolSeen) mark(kind *map[ownerHost]time.Time, owner int64, host string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if *kind == nil {
		*kind = map[ownerHost]time.Time{}
	}
	(*kind)[ownerHost{owner, host}] = now
}

func (t *sessionToolSeen) markMCP(owner int64, host string, now time.Time) {
	t.mark(&t.mcp, owner, host, now)
}
func (t *sessionToolSeen) markHook(owner int64, host string, now time.Time) {
	t.mark(&t.hook, owner, host, now)
}

// seen reports whether owner's host called each door since this process started.
func (t *sessionToolSeen) seen(owner int64, host string) (mcp, hook bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, mcp = t.mcp[ownerHost{owner, host}]
	_, hook = t.hook[ownerHost{owner, host}]
	return mcp, hook
}

// SessionToolHost is one of the caller's mirrored hosts as GET /api/features
// reports it.
type SessionToolHost struct {
	Host        string `json:"host"`
	Online      bool   `json:"online"`
	MCPSeen     bool   `json:"mcp_seen"`
	AskHookSeen bool   `json:"ask_hook_seen"`
}

// sessionToolHosts is every host the hub lists for the caller, with what this
// process has seen of its session tools. Empty with the mirror plugin off.
func (s *Server) sessionToolHosts(r *http.Request) []SessionToolHost {
	out := []SessionToolHost{}
	if s.ext.Mirror == nil {
		return out
	}
	owner := requestOwner(r.Context())
	for _, hv := range s.ext.Mirror.Hosts(r.Context()) {
		mcp, hook := s.sessionTools.seen(owner, hv.Host)
		out = append(out, SessionToolHost{Host: hv.Host, Online: hv.Online, MCPSeen: mcp, AskHookSeen: hook})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

// --- the page ------------------------------------------------------------------

func (s *Server) handleFeaturesPage(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder
	b.WriteString(`<div class="flex flex-col gap-4" data-features-page>`)
	b.WriteString(`<section class="flex flex-col gap-3 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line">
<h1 class="text-balance text-base font-semibold text-fg">Features</h1>
<p class="text-sm text-fg2">What this server runs. Core features come with the install; everything else is optional. To turn one on, add its line(s) to the server's <code>.env</code> and restart it.</p>`)
	if s.features.Len() == 0 {
		b.WriteString(`<p class="text-sm text-fg" data-features-empty>This server did not report its features.</p>`)
	} else {
		n := s.features.Counts()
		b.WriteString(`<p class="text-sm text-fg" data-features-counts>` + strconv.Itoa(n[features.On]) + ` on · ` + strconv.Itoa(n[features.Off]) +
			` off · ` + strconv.Itoa(n[features.Misconfigured]) + ` misconfigured</p>`)
	}
	b.WriteString(`</section>`)
	hosts := s.sessionToolHosts(r)
	for _, f := range s.features.All() {
		b.WriteString(featureRow(f, hosts))
	}
	b.WriteString(`<a href="/account" class="inline-flex min-h-[44px] items-center text-sm text-accent underline">Back to account</a></div>`)
	s.writeNavDocument(w, r, http.StatusOK, "Features", ui.NavFeatures, false, b.String())
}

// featureStateLabel is a state as the page's pill reads it.
func featureStateLabel(st features.State) string {
	switch st {
	case features.On:
		return "On"
	case features.Misconfigured:
		return "Misconfigured"
	}
	return "Off"
}

func featureStateTone(st features.State) string {
	switch st {
	case features.On:
		return "bg-st-running-bg text-st-running-fg"
	case features.Misconfigured:
		return "bg-st-warning-bg text-st-warning-fg"
	}
	return "bg-s2 text-fg2"
}

// featureRow is one feature on the page, anchored by its id so a hint can
// link straight to it (/account/features#dictation).
func featureRow(f features.Feature, hosts []SessionToolHost) string {
	e := html.EscapeString
	var b strings.Builder
	b.WriteString(`<section id="` + e(f.ID) + `" data-feature="` + e(f.ID) + `" data-state="` + e(string(f.State)) +
		`" class="flex min-w-0 flex-col gap-2 rounded-2xl border border-line bg-s1/70 px-5 py-4 ring-1 ring-line">`)
	b.WriteString(`<div class="flex items-center justify-between gap-3"><h2 class="text-sm font-semibold text-fg">` + e(f.Name))
	if f.Core {
		b.WriteString(` <span class="text-xs font-normal text-muted">core</span>`)
	}
	b.WriteString(`</h2><span data-feature-state class="shrink-0 rounded-full px-2 py-0.5 text-xs font-semibold ` +
		featureStateTone(f.State) + `">` + featureStateLabel(f.State) + `</span></div>`)
	b.WriteString(`<p class="text-sm text-fg2">` + e(f.Description) + `</p>`)
	if f.Reason != "" {
		b.WriteString(`<p data-feature-reason class="break-words text-sm text-fg">` + e(f.Reason) + `</p>`)
	}
	if f.State != features.On {
		if lines := f.EnableLines(); len(lines) > 0 {
			where := "Add to .env, then restart:"
			if f.HostSide {
				where = "On each host, in muster-mirror's environment:"
			}
			b.WriteString(`<p class="text-xs text-muted">` + e(where) + `</p><pre data-feature-enable class="min-w-0 max-w-full overflow-x-auto rounded-md bg-bg px-3 py-2 text-xs text-fg ring-1 ring-inset ring-line">` +
				e(strings.Join(lines, "\n")) + `</pre>`)
		}
		if f.Note != "" {
			b.WriteString(`<p data-feature-note class="break-words text-sm text-fg2">` + e(f.Note) + `.</p>`)
		}
	}
	if f.ID == features.AgentAskTool && f.State == features.On {
		b.WriteString(sessionToolsList(hosts))
	}
	b.WriteString(`<a href="` + e(f.DocsLink()) + `" class="break-all text-xs text-accent underline">` + e(f.Docs()) + `</a></section>`)
	return b.String()
}

// sessionToolsList is the agent-ask-tool row's per-host readout.
func sessionToolsList(hosts []SessionToolHost) string {
	e := html.EscapeString
	var b strings.Builder
	b.WriteString(`<div data-session-tools class="flex flex-col gap-1 text-sm">`)
	if len(hosts) == 0 {
		b.WriteString(`<p class="text-fg2">No mirrored host is connected.</p>`)
	}
	yes := func(v bool) string {
		if v {
			return "seen"
		}
		return "not seen"
	}
	for _, h := range hosts {
		b.WriteString(`<p data-session-tools-host="` + e(h.Host) + `" class="text-fg">` + e(h.Host) + `: MCP ` + yes(h.MCPSeen) +
			`, ask hook ` + yes(h.AskHookSeen) + `</p>`)
	}
	b.WriteString(`<p class="break-words text-xs text-muted">` + e(SessionToolsNote) + `</p></div>`)
	return b.String()
}
