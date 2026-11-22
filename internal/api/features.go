package api

import (
	"html"
	"net/http"
	"sort"
	"strconv"
	"strings"

	g "maragu.dev/gomponents"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/features"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE FEATURE REGISTRY'S CONSUMERS (internal/features).
//
// cmd/muster-server evaluates the registry once, from what it wired, and hands
// it here with UseFeatures. What reads that Set, exactly:
//
//   - GET /account/features: the Features page, one row per feature with its
//     plain name, what it does and its state, and an ⓘ sheet holding the
//     line(s) that turn it on (an admin's only); every hint beside a hidden
//     control opens its sheet.
//   - /api/doctor's `features` (doctor.go), which `muster doctor` prints and
//     FAILs on when a feature is misconfigured.
//   - the Account page's suggested-replies note and the Credentials page's
//     variable name.
//   - the MCP doors: a tool whose feature is off is not listed (the listings
//     call featureOff) and a call to it explains (mcpDisabledTool); each door's
//     muster_help lists the features its own tools need that are off.
//   - the typed 404 a route answers when its feature is off
//     (writeFeatureDisabled), which the CLI reports as "feature X is disabled";
//     a person who opens such a route gets the plain page (renderFeatureOff):
//     the feature's plain name, and the setup behind an admin's ⓘ only.
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
// client, the plain notice page (renderFeatureOff) for a person who typed the
// address.
//
// 🔴 404, NOT 409: every one of these routes answered 404 while its feature was
// off before the registry existed, and a caller that branches on the status
// keeps working. What changes is that the body is now typed, so the CLI can
// tell "this server has the route and the feature is off" from "this server
// predates the route" (a bare mux 404, text/plain).
//
// 🔴 THE JSON IS THE MACHINES', THE PAGE IS A PERSON'S. The JSON body keeps its
// setup text (`enable`, `message`, `reason`): the CLI prints it
// (cmd/muster featureDisabled, which sends Accept: application/json) and the
// MCP doors say the same through mcpDisabledTool. The page says none of it in
// its top layer — see renderFeatureOff.
//
// WHO GETS WHICH, for a request that looks like a page load:
//   - a browser session (Via session): the page;
//   - NO caller yet — a gate that runs before any credential is examined
//     (requirePluginsOn, requirePluginOriginsOn): requireSession decides, so
//     a signed-in browser gets the page and a signed-out one is sent to sign
//     in (the page differs by reader, an admin's ⓘ, so it needs one);
//   - a caller a token admitted (an API or hook token): the JSON, unchanged —
//     never a redirect to a sign-in form a token cannot fill
//     (TestATokenCallerGetsTheJSONNeverASignInRedirect).
func (s *Server) writeFeatureDisabled(w http.ResponseWriter, r *http.Request, id string) {
	if isDocumentNavigation(r) {
		p, ok := authz.From(r.Context())
		switch {
		case !ok:
			s.requireSession(func(w http.ResponseWriter, r *http.Request) { s.renderFeatureOff(w, r, id) })(w, r)
			return
		case p.Via == authz.ViaSession:
			s.renderFeatureOff(w, r, id)
			return
		}
	}
	body := map[string]any{"error": FeatureDisabledError, "feature": id, "docs": features.DocsPage + "#" + id}
	if f, ok := s.feature(id); ok {
		msg := f.DisabledMessage()
		body["name"], body["state"], body["enable"], body["message"] = f.Name, f.State, f.EnableHint(), msg
		if f.Reason != "" {
			body["reason"] = f.Reason
		}
	}
	s.writeJSON(w, http.StatusNotFound, body)
}

// The turned-off notice's plain words (its tests pin them as literals).
const (
	// FeatureOffNoticeText is the notice's line under the feature's name.
	FeatureOffNoticeText = "Turned off on this server."
	// FeatureNotSetUpNoticeText is that line for a misconfigured feature.
	FeatureNotSetUpNoticeText = "Not fully set up on this server."
	// FeatureOffUnknownName names a feature the registry does not list. A
	// real server's catalog lists every gated feature; this is the fallback
	// for a server handed no registry (tests), which draws no next step.
	FeatureOffUnknownName = "This feature"
)

// renderFeatureOff is the page a person gets for a route whose feature is
// off: the feature's plain name (the catalog's PlainName, as the Features page
// shows it), that it is turned off, and the next step.
//
// 🔴 THE SETUP IS AN ADMIN'S, AND ABSENT — NOT HIDDEN — FOR ANYONE ELSE (the
// Features page's rule, featureSheet). An admin's next step is the ⓘ, which is
// the Features page row's own sheet: the exact .env line(s) as its code, with
// Copy, and the docs link. A member is told to ask their admin (the Features
// page's own words, featureAskAdmin), and the HTML carries no variable,
// `.env`, docs path or ⓘ at all.
//
// The reader is always signed in here (writeFeatureDisabled), so the page is
// always inside the app, with the nav.
func (s *Server) renderFeatureOff(w http.ResponseWriter, r *http.Request, id string) {
	e := html.EscapeString
	admin := authz.IsAdmin(r.Context())
	f, known := s.feature(id)
	name, line, next, sheet := FeatureOffUnknownName, FeatureOffNoticeText, "", ""
	if known {
		name = f.ShownName()
		if f.State == features.Misconfigured {
			line = FeatureNotSetUpNoticeText
		}
		switch {
		case !admin:
			next = featureAskAdmin(f.State)
		case f.State == features.Misconfigured:
			next, sheet = FeatureNeedsSetupText, ui.InfoSheetHTML(featureSheet(f, name, true))
		default:
			next, sheet = FeatureOpenSheetText, ui.InfoSheetHTML(featureSheet(f, name, true))
		}
	}
	inner := `<section data-feature-off="` + e(id) + `" class="flex flex-col gap-4 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line">
<div class="flex items-center gap-2"><h1 class="min-w-0 flex-1 text-balance text-base font-semibold text-fg">` + e(name) + `</h1>` + sheet + `</div>
<p data-feature-off-state class="text-sm text-fg2">` + line + `</p>`
	if next != "" {
		inner += `
<p data-feature-off-next class="text-sm text-fg">` + next + `</p>`
	}
	inner += `
</section>`
	s.writeNavDocument(w, r, http.StatusNotFound, name, "", false, inner)
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
		add(features.SessionRouting, routeToolDef(), routeManyToolDef())
		add(features.TaskRouting, taskRoutingToolDefs(true)...)
		add(features.Officers, officerToolDefs()...)
		add(features.Officers, officerSpawnShareToolDef())
		add(features.Officers, chiefOfficerToolDefs()...)
		add(features.Pages, pageToolDefs()...)
		add(features.Push, notifyOwnerToolDef())
	case mirrorDoor:
		add(features.Connectors, connectorDecideToolDefs()...)
		add(features.Plugins, pluginDraftToolDefs()...)
		add(features.SessionRouting, routeReplyToolDef())
		add(features.SessionRouting, pluginMessageRouteToolDefs()...)
		add(features.Dispatch, chiefRestartToolDef())
		add(features.TaskRouting, dispatchHoldToolDefs()...)
		add(features.Pages, pageToolDefs()...)
		add(features.Pages, pageAssetToolDefs()...)
		add(features.Push, notifyOwnerToolDef())
		add(features.Officers, hostOfficerToolDefs()...)
		add(features.Connectors, connectorToolDefs()...)
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

// THE FEATURES PAGE'S TOP LAYER IS PLAIN (the operator rule: muster is for
// people who are not engineers, so its own chrome shows no variable, Compose
// profile, flag or command in its top layer). Each row is the feature's plain
// name, one plain sentence of what it does and its state; everything that sets
// it up — the .env lines, the reason it is off, the settings, the docs path —
// is in the row's ⓘ sheet (ui.InfoSheet), with a Copy button.
//
// 🔴 THE SETUP IS AN ADMIN'S, AND ABSENT — NOT HIDDEN — FOR ANYONE ELSE. The
// viewer may see setup details exactly when the shell's Setup is true
// (authz.IsAdmin, shellFeatures); a member's sheet is Withheld, so its HTML
// carries no variable, line or reason at all, and its rows say to ask their
// admin. Every "how to turn it on" link elsewhere opens a row's sheet
// (ui.FeatureHref, ui.FeaturesSheetJS).

// The page's own plain words (its tests pin them as literals).
const (
	// FeatureAskAdminText is a member's line under a feature that is not on,
	// and what its sheet says.
	FeatureAskAdminText = "Ask your admin to turn this on."
	// FeatureAskAdminFinishText is that line for a feature that is on but
	// not fully set up: there is nothing to turn on, only setup to finish.
	FeatureAskAdminFinishText = "Ask your admin to finish setting it up."
	// FeatureOpenSheetText is an admin's line under a feature that is off.
	FeatureOpenSheetText = "To turn it on, open its ⓘ."
	// FeatureNeedsSetupText is an admin's line under a misconfigured feature.
	FeatureNeedsSetupText = "Something it needs is missing. Its ⓘ says what."
	// FeatureLearnMoreText is each row's docs link.
	FeatureLearnMoreText = "Learn more"
)

func (s *Server) handleFeaturesPage(w http.ResponseWriter, r *http.Request) {
	admin := s.shellFeatures(r.Context()).Setup
	var b strings.Builder
	b.WriteString(`<div class="flex flex-col gap-4" data-features-page>`)
	b.WriteString(`<section class="flex flex-col gap-3 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line">
<h1 class="text-balance text-base font-semibold text-fg">Features</h1>`)
	if admin {
		b.WriteString(`<p class="text-sm text-fg2" data-features-intro>What this server can do. Some features come built in; the rest are optional. To turn one on, open its ⓘ: it has the exact setting to copy.</p>`)
	} else {
		b.WriteString(`<p class="text-sm text-fg2" data-features-intro>What this server can do. Some features come built in; the rest are optional, and your admin turns them on.</p>`)
	}
	if s.features.Len() == 0 {
		b.WriteString(`<p class="text-sm text-fg" data-features-empty>This server did not report its features.</p>`)
	} else {
		n := s.features.Counts()
		b.WriteString(`<p class="text-sm text-fg" data-features-counts>` + strconv.Itoa(n[features.On]) + ` on · ` + strconv.Itoa(n[features.Off]) +
			` off · ` + strconv.Itoa(n[features.Misconfigured]) + ` ` + needSetup(n[features.Misconfigured]) + `</p>`)
	}
	b.WriteString(`</section>`)
	for _, f := range s.features.All() {
		b.WriteString(featureRow(f, admin))
	}
	b.WriteString(`<a href="/account" class="inline-flex min-h-[44px] items-center text-sm text-accent underline">Back to account</a></div>`)
	b.WriteString(`<script>` + ui.FeaturesSheetJS + `</script>`)
	s.writeNavDocument(w, r, http.StatusOK, "Features", ui.NavFeatures, false, b.String())
}

// featureAskAdmin is a member's next step for a feature in state st that is
// not on: ONE spelling, which the Features page row and the turned-off page
// both draw.
func featureAskAdmin(st features.State) string {
	if st == features.Misconfigured {
		return FeatureAskAdminFinishText
	}
	return FeatureAskAdminText
}

// needSetup is the counts line's verb for n misconfigured features.
func needSetup(n int) string {
	if n == 1 {
		return "needs setup"
	}
	return "need setup"
}

// featureStateLabel is a state as the page's pill reads it.
func featureStateLabel(st features.State) string {
	switch st {
	case features.On:
		return "On"
	case features.Misconfigured:
		return "Needs setup"
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

// featureRow is one feature on the page, anchored by its id (a docs anchor),
// with its ⓘ sheet (ui.FeatureSheetID) the place every hint links to.
// admin: the viewer may see setup details (ui.Features.Setup).
func featureRow(f features.Feature, admin bool) string {
	e := html.EscapeString
	name := f.ShownName()
	var b strings.Builder
	b.WriteString(`<section id="` + e(f.ID) + `" data-feature="` + e(f.ID) + `" data-state="` + e(string(f.State)) +
		`" class="flex min-w-0 flex-col gap-2 rounded-2xl border border-line bg-s1/70 px-5 py-4 ring-1 ring-line">`)
	b.WriteString(`<div class="flex items-center gap-2"><h2 data-feature-name class="min-w-0 flex-1 text-sm font-semibold text-fg">` + e(name))
	if f.Core {
		b.WriteString(` <span class="text-xs font-normal text-muted">built in</span>`)
	}
	b.WriteString(`</h2><span data-feature-state class="shrink-0 rounded-full px-2 py-0.5 text-xs font-semibold ` +
		featureStateTone(f.State) + `">` + featureStateLabel(f.State) + `</span>`)
	b.WriteString(ui.InfoSheetHTML(featureSheet(f, name, admin)))
	b.WriteString(`</div>`)
	if f.OneLine != "" {
		b.WriteString(`<p data-feature-oneline class="text-sm text-fg2">` + e(f.OneLine) + `</p>`)
	}
	switch {
	case f.State == features.On:
	case f.State == features.Off && f.Replaced != "":
		b.WriteString(`<p data-feature-replaced class="text-sm text-fg">` + e(f.Replaced) + `</p>`)
	case !admin:
		b.WriteString(`<p data-feature-ask class="text-sm text-fg">` + featureAskAdmin(f.State) + `</p>`)
	case f.State == features.Misconfigured:
		b.WriteString(`<p data-feature-next class="text-sm text-fg">` + FeatureNeedsSetupText + `</p>`)
	default:
		b.WriteString(`<p data-feature-next class="text-sm text-fg">` + FeatureOpenSheetText + `</p>`)
	}
	b.WriteString(`<a href="` + e(f.DocsLink()) + `" data-feature-docs class="inline-flex min-h-[44px] items-center self-start text-xs text-accent underline">` +
		FeatureLearnMoreText + `<span class="sr-only"> about ` + e(name) + `</span></a></section>`)
	return b.String()
}

// featureSheet is a row's ⓘ: for an admin, what turns the feature on (its
// .env lines, copied whole), why it is in its state, what else it needs and
// what tunes it; for anyone else, Withheld — the plain state and "ask your
// admin", with none of the setup in the HTML.
//
// A Kubernetes feature's sheet says the Compose stack cannot run it; its lines
// are still shown, for a Kubernetes deployment.
func featureSheet(f features.Feature, name string, admin bool) ui.InfoSheet {
	on := f.State == features.On
	replaced := f.State == features.Off && f.Replaced != ""
	sh := ui.InfoSheet{ID: ui.FeatureSheetID(f.ID), Label: "How to turn on " + name, Heading: "Turn on " + name}
	if on || replaced {
		sh.Label, sh.Heading = "Setup of "+name, name
	}
	if !admin {
		// Nothing a member can turn on: the sheet is about the feature.
		sh.Withheld, sh.Label, sh.Heading = true, "About "+name, name
		switch {
		case replaced:
			sh.Lead, sh.WithheldText = name+" is not used on this server.", f.Replaced
		case on:
			sh.Lead, sh.WithheldText = name+" is on.", "Ask your admin if it needs changing."
		case f.State == features.Misconfigured:
			sh.Lead, sh.WithheldText = name+" is not fully set up on this server.", FeatureAskAdminFinishText
		default:
			sh.Lead, sh.WithheldText = name+" is off on this server.", FeatureAskAdminText
		}
		return sh
	}
	e := html.EscapeString
	lines := f.EnableLines()
	switch {
	case len(lines) == 0:
	case on && f.Kubernetes:
		sh.Lead = "It is on. In a Kubernetes deployment, these lines turn it on:"
	case on && f.HostSide:
		sh.Lead = "It is on. On each computer, these lines in muster-mirror's environment turn it on:"
	case on:
		sh.Lead = "It is on. These lines in the server's .env file turn it on:"
	case f.Kubernetes:
		sh.Lead = "In a Kubernetes deployment, set:"
	case f.HostSide:
		sh.Lead = "On each computer, in muster-mirror's environment:"
	default:
		sh.Lead = "Add these lines to the server's .env file, then run docker compose up -d --wait:"
	}
	sh.Code, sh.Multiline = strings.Join(lines, "\n"), true
	var x strings.Builder
	x.WriteString(`<p data-feature-description class="text-sm text-fg2">` + e(f.Description) + `</p>`)
	if f.Reason != "" {
		x.WriteString(`<p data-feature-reason class="break-words text-sm text-fg">` + e(ui.SentenceStart(f.Reason)) + `</p>`)
	}
	if !on && f.Kubernetes {
		x.WriteString(`<p data-feature-kubernetes class="break-words text-sm text-fg2">` + e(features.KubernetesHint) +
			`: not available in the Compose stack.</p>`)
	}
	if !on && f.Note != "" {
		x.WriteString(`<p data-feature-note class="break-words text-sm text-fg2">` + e(ui.SentenceStart(f.Note)) + `.</p>`)
	}
	if len(f.Settings) > 0 {
		x.WriteString(`<p data-feature-settings class="break-words text-xs text-muted">Other settings: ` + e(strings.Join(f.Settings, ", ")) + `</p>`)
	}
	x.WriteString(`<a href="` + e(f.DocsLink()) + `" class="break-all text-xs text-accent underline">` + e(f.Docs()) + `</a>`)
	sh.Extra = g.Raw(x.String())
	return sh
}
