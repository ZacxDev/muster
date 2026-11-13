package api

import (
	"html"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/features"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE FEATURE REGISTRY'S CONSUMERS (internal/features).
//
// cmd/muster-server evaluates the registry once, from what it wired, and hands
// it here with UseFeatures. What reads that Set, exactly:
//
//   - GET /account/features: the Features page, one row per feature with its
//     state and the line(s) that turn it on; every hint beside a hidden
//     control links to its row.
//   - /api/doctor's `features` (doctor.go), which `muster doctor` prints and
//     FAILs on when a feature is misconfigured.
//   - the Account page's suggested-replies note and the Credentials page's
//     variable name.
//   - the MCP doors: a tool whose feature is off is not listed (the listings
//     call featureOff) and a call to it explains (mcpDisabledTool); each door's
//     muster_help lists the features its own tools need that are off.
//   - the typed 404 a route answers when its feature is off
//     (writeFeatureDisabled), which the CLI reports as "feature X is disabled".
//   - the GATES that draw a control: dictatePath, uploadPath and shellFeatures
//     (the Connectors tab, the push button, the dispatch "+"), through
//     featureOff. The Set can only take a feature away there: a gate still
//     needs its own extension, and a feature the registry calls OFF is not
//     drawn. A misconfigured one keeps what is wired (featureOff says why).
//
// What does NOT read it: the route handlers, which gate on their own
// extension and answer their own unwired case (the ones the CLI and the
// composer reach answer the typed 404), and the boot banner's posture lines
// (which share their predicates with the evaluator where the two could
// disagree — see chiefNudgesProblem).
//
// A server that was never handed a Set (most tests) behaves as it did before
// the registry: every gate is its extension alone, and no hint is drawn.

// FeaturesPagePath is the page.
const FeaturesPagePath = ui.FeaturesPagePath

// FeatureDisabledError is the `error` of the typed answer a route gives when
// the feature behind it is off: the CLI branches on this value, never on the
// sentence beside it.
const FeatureDisabledError = "feature_disabled"

// UseFeatures installs the evaluated feature registry. Call before Handler.
func (s *Server) UseFeatures(set features.Set) { s.features = set }

// Features is the installed registry (the zero Set when none was installed).
func (s *Server) Features() features.Set { return s.features }

// FeatureView is one feature as /api/doctor serves it: the registry entry plus
// what is derived from it, so a client needs no copy of the docs-anchor or
// hint rules.
type FeatureView struct {
	features.Feature
	Docs        string   `json:"docs"`
	EnableLines []string `json:"enable_lines"`
	EnableHint  string   `json:"enable_hint"`
}

// FeatureViews is set as the API serves it.
//
// 🔴 IT IS THE ONE SERIALISATION. The doctor and the leak test
// (cmd/muster-server, TestTheFeatureRegistryLeaksNoConfiguredValue) both call
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
	mux.HandleFunc("GET "+FeaturesPagePath, s.requireSession(s.handleFeaturesPage))
}

// feature is one feature of the installed registry.
func (s *Server) feature(id string) (features.Feature, bool) { return s.features.Get(id) }

// featureOff reports whether the registry says id is OFF. With no registry
// installed, or for an id it does not list, it answers false: no gate may
// invent an "off" the server never decided.
//
// 🔴 OFF, NOT "NOT ON": A MISCONFIGURED FEATURE KEEPS WHAT IS WIRED. It was
// asked for and its own store is built — what is missing is a dependency
// elsewhere (plugins with no agent gateway: messages are never delivered to
// the chief). Its tools and controls still work as far as they go: the
// plugins e2e spec's claude-code chief drafts a reply over MCP on exactly that
// server, and hiding the drafting tools there broke it. The missing dependency
// is reported where it is true — `muster doctor` FAILs on it, the Features page
// and the banner say it — not by taking away the half that works.
func (s *Server) featureOff(id string) bool {
	f, ok := s.feature(id)
	return ok && f.State == features.Off
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

// mcpDoor names one of the two MCP doors.
type mcpDoor int

const (
	agentDoor  mcpDoor = iota // POST /mcp: a dispatched agent's tools
	mirrorDoor                // POST /mirror/mcp: a mirrored session's tools
)

// toolFeatures is the feature behind each tool THIS DOOR can offer that a
// feature gates, read off the tools' own definitions so no name is spelled
// twice. A tool missing from it (the attention and task tools, which need
// only the database) is not gated by any optional feature.
//
// 🔴 PER DOOR (#441 round 1, R10): muster_help on the mirror door lists only
// the features the mirror door's tools need. The listings that use these
// definitions (agentTools, mirrorTools) gate on the same features, through
// featureOff, so a feature the registry calls off never has its tools offered.
func toolFeatures(door mcpDoor) map[string]string {
	m := map[string]string{}
	add := func(id string, defs ...agents.ToolDef) {
		for _, d := range defs {
			m[d.Name] = id
		}
	}
	switch door {
	case agentDoor:
		add(features.Connectors, connectorToolDefs()...)
		add(features.BrowserFlows, flowToolDefs()...)
		add(features.Plugins, gatedTaskToolDef(), replyDraftToolDef(), pluginMessageSettleToolDef(), routingRuleProposeToolDef())
		add(features.SessionRouting, routeToolDef())
		add(features.TaskRouting, taskRoutingToolDefs(true)...)
		add(features.Officers, officerToolDefs()...)
		add(features.Pages, pageToolDefs()...)
	case mirrorDoor:
		add(features.Connectors, connectorDecideToolDefs()...)
		add(features.Plugins, pluginDraftToolDefs()...)
		add(features.SessionRouting, routeReplyToolDef())
		add(features.SessionRouting, pluginMessageRouteToolDefs()...)
		add(features.Dispatch, chiefRestartToolDef())
		add(features.TaskRouting, dispatchHoldToolDefs()...)
		add(features.Pages, pageToolDefs()...)
		add(features.Officers, hostOfficerToolDefs()...)
	}
	return m
}

// mcpDisabledTool is the explanation for a call on door to a tool it does not
// list because its feature is OFF: "disabled: <feature> — set … (docs/…)". ""
// when the tool is not gated by a feature on this door, when its feature is
// not off (a misconfigured feature keeps its wired tools, so an unlisted one
// is unlisted for another reason — the caller is not the chief, say — and
// stays an unknown name), or when no registry is installed.
//
// 🔴 THE SAME TEST AS THE LISTINGS (featureOff), so a name is explained as
// disabled exactly when the listing left it out for that reason.
func (s *Server) mcpDisabledTool(door mcpDoor, name string) string {
	id, gated := toolFeatures(door)[name]
	if !gated || !s.featureOff(id) {
		return ""
	}
	f, _ := s.feature(id)
	return f.DisabledMessage()
}

// mcpOffFeatures is what door's muster_help says about its features: the ones
// that are OFF (their tools are not offered here), and — separately — the ones
// that are MISCONFIGURED (their tools ARE offered, but something they need is
// missing, said in the reason).
//
// 🔴 OFF IS featureOff, THE LISTINGS' OWN TEST: a feature is never reported
// "not offered" while its tools are listed (#441 round 1).
func (s *Server) mcpOffFeatures(door mcpDoor) (off, degraded []mcpOffFeature) {
	gating := map[string][]string{}
	for tool, id := range toolFeatures(door) {
		gating[id] = append(gating[id], tool)
	}
	for _, f := range s.features.NotOn() {
		tools, ok := gating[f.ID]
		if !ok {
			continue
		}
		sort.Strings(tools)
		row := mcpOffFeature{Feature: f.ID, State: string(f.State), Tools: tools, Enable: f.DisabledMessage()}
		if s.featureOff(f.ID) {
			off = append(off, row)
		} else {
			row.Reason = f.Reason
			degraded = append(degraded, row)
		}
	}
	return off, degraded
}

// mcpOffFeature is one row of muster_help's `disabled_features`.
type mcpOffFeature struct {
	Feature string   `json:"feature"`
	State   string   `json:"state"`
	Tools   []string `json:"tools"`
	Enable  string   `json:"enable"`
	// Reason is a degraded (misconfigured) feature's missing dependency.
	Reason string `json:"reason,omitempty"`
}

// mcpFeatureHooks are a door's surface hooks for the registry.
func (s *Server) mcpFeatureHooks(door mcpDoor) (func(string) string, func() ([]mcpOffFeature, []mcpOffFeature)) {
	return func(name string) string { return s.mcpDisabledTool(door, name) },
		func() ([]mcpOffFeature, []mcpOffFeature) { return s.mcpOffFeatures(door) }
}

// --- the page ------------------------------------------------------------------

func (s *Server) handleFeaturesPage(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder
	b.WriteString(`<div class="flex flex-col gap-4" data-features-page>`)
	b.WriteString(`<section class="flex flex-col gap-3 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line">
<h1 class="text-balance text-base font-semibold text-fg">Features</h1>
<p class="text-sm text-fg2">What this server runs. Core features come with the install; everything else is optional. To turn one on, add its line(s) to the server's <code>.env</code> and run <code>docker compose up -d --wait</code>.</p>`)
	if s.features.Len() == 0 {
		b.WriteString(`<p class="text-sm text-fg" data-features-empty>This server did not report its features.</p>`)
	} else {
		n := s.features.Counts()
		b.WriteString(`<p class="text-sm text-fg" data-features-counts>` + strconv.Itoa(n[features.On]) + ` on · ` + strconv.Itoa(n[features.Off]) +
			` off · ` + strconv.Itoa(n[features.Misconfigured]) + ` misconfigured</p>`)
	}
	b.WriteString(`</section>`)
	for _, f := range s.features.All() {
		b.WriteString(featureRow(f))
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
//
// A Kubernetes feature shows no .env block: on the documented install there is
// nothing to set, and the hint says what it needs instead.
func featureRow(f features.Feature) string {
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
		switch lines := f.EnableLines(); {
		case f.Kubernetes:
			b.WriteString(`<p data-feature-kubernetes class="break-words text-sm text-fg2">` + e(features.KubernetesHint) +
				`: not available in the Compose stack.</p>`)
		case len(lines) > 0:
			where := "Add to .env, then run docker compose up -d --wait:"
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
	b.WriteString(`<a href="` + e(f.DocsLink()) + `" class="break-all text-xs text-accent underline">` + e(f.Docs()) + `</a></section>`)
	return b.String()
}
