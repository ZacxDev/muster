package ui

// Features says which OPTIONAL subsystems the process rendering a document
// actually built. It is what lets a document show a surface only when there is
// something behind it.
//
// 🔴 IT IS A RENDER PARAMETER, NOT PACKAGE STATE, AND THE DIFFERENCE IS
// CORRECTNESS RATHER THAN TASTE. faroConfig is process-wide because telemetry is
// a property of the process; this is a property of the SERVER's extensions, and
// the only honest place to read it is the handler holding that server. A package
// global would also have to be written from a request path — every handler races
// every other one, and a test that renders one shape would decide what a
// concurrent test renders.
//
// 🔴 IT DOES NOT DECIDE WHICH ROUTES EXIST, AND IT MUST NEVER GROW INTO THAT.
// Route registration in internal/api is unconditional on purpose (see that
// package's routes.go): the recorded route set is a function of the code and
// never of a fixture. This type gates what a DOCUMENT draws — a link and a panel
// — while every route stays registered and answers its own unwired case. The two
// are independent claims and both are needed: without the handler's answer a
// stale page still reaches a dead route, and without this a live page mounts a
// panel whose only possible content is a refusal.
type Features struct {
	// Connectors is true when the server holds a connector store (an encryption
	// key is configured). False removes the Connectors tab and its panel: the
	// store is built only with a key.
	//
	// 🔴 FALSE IS THE SHIPPING DEFAULT, NOT AN EDGE CASE, which is why the zero
	// value is the unbuilt state. A caller that forgets to populate this draws
	// LESS than it could, which is the safe direction: the opposite default
	// draws a tab over nothing.
	Connectors bool
	// Admin is true when the viewer is an admin and the admin pages are
	// served (accounts PR 5). It draws the sidebar's Admin link and nothing
	// else: the pages refuse a member on their own (requireAdmin).
	Admin bool
	// Setup is true when the viewer's role is admin — the password operator
	// included, where no admin pages are served: they may see SETUP details
	// (how to turn a feature on, the muster-mirror setting a computer lacks).
	// Anyone else is told to ask their admin (InfoSheet.Withheld). Not Admin,
	// which is the Admin LINK and needs the admin pages too.
	Setup bool
	// Push is true when the server holds VAPID keys and a subscription store
	// (muster-native Web Push). False draws no Enable button, no "on" pill and
	// no subscribe script: the routes behind them would only answer 503, and an
	// Enable button over a 503 asks for the OS permission and then fails.
	Push bool
	// Attention is true when the server holds the attention queue's store
	// (internal/attention). False draws no Attention link and no badge: the
	// routes behind them would only answer 404.
	Attention bool
	// Pages is true when shareable pages are served (internal/pages, the
	// feature not called off). It draws the sidebar's Pages link; false draws
	// none, since /pages would only answer that the feature is off.
	Pages bool
	// Officers is true when the server keeps officers (the built-in officer
	// store: a database). It draws the sidebar's Officers link; false draws
	// none, since /officers would only answer that there are none here.
	Officers bool
	// SnapUser and SnapBuild are the signed-in user and the running build: the
	// key, and the check, of the mirrored view's snapshot store
	// (mirror_snap.go). A zero SnapUser draws no snapshot script at all.
	SnapUser  int64
	SnapBuild string
	// FleetDigest is true when the fleet digest is on (MUSTER_FLEET_DIGEST=1,
	// internal/api fleet_digest.go). It draws the digest card's slot in the
	// chief panel; false draws none, since its route would only answer 404.
	FleetDigest bool
	// FleetPropose is true when fleet proposals are on
	// (MUSTER_FLEET_ORCHESTRATION=propose, internal/api fleet_actions.go): the
	// digest card's skeleton draws the Proposed group.
	FleetPropose bool
	// Brand is the instance's logo and name (brand.go): the header's and the
	// sidebar's brand, and the shell's favicon. The zero value draws the
	// wordmark and muster's own icons.
	Brand Brand
	// Apps are the signed-in owner's live app plugins, oldest first
	// (internal/api plugin_app.go, liveAppPlugins): one sidebar entry each,
	// after the built-in section links (app_page.go). nil draws none.
	Apps []AppTab
}

// visibleTabs is the subset of musterTabs a document with these features may
// draw.
//
// 🔴 EVERY CONSUMER OF THE TAB REGISTRY THAT DRAWS SOMETHING GOES THROUGH HERE —
// the sidebar links, the shell's panels, and the values navRegistryJS
// hands the browser router. They must agree or the failure is silent in both
// directions: a link with no panel shows nothing when clicked, and a panel with
// no link is unreachable. ui.TabKeys() deliberately does NOT go through here; it
// feeds route registration, which is unconditional.
// FeaturesPagePath is the Features page (internal/api features.go): every
// feature this server runs or does not, with the line(s) that turn each on.
const FeaturesPagePath = "/account/features"

// FeatureSheetID is the element id of a feature's ⓘ sheet on the Features
// page (its row's id is the feature id itself, a docs anchor).
func FeatureSheetID(id string) string { return id + "-setup" }

// FeatureHref is a feature's ⓘ sheet on the Features page — where every "how
// to turn it on" hint beside a control that is hidden because its feature is
// off links to. The page opens the sheet its address names (FeaturesSheetJS):
// for an admin it holds the setting and a Copy button; anyone else's says to
// ask their admin.
func FeatureHref(id string) string { return FeaturesPagePath + "#" + FeatureSheetID(id) }

// FeaturesSheetJS opens the ⓘ sheet the Features page's address names
// (FeatureHref), on load and when the address changes, with the sheet's row
// scrolled into view. Only an element that IS a sheet is opened; a malformed
// address opens nothing; the hashchange listener is added once per document.
const FeaturesSheetJS = `(function(){
  function open(){
    var id;
    try { id = decodeURIComponent(location.hash.slice(1)); } catch (e) { return; }
    if (!id) return;
    var el = document.getElementById(id);
    if (!el || !el.hasAttribute('data-info-sheet') || !el.showPopover) return;
    var row = el.closest('[data-feature]');
    if (row) row.scrollIntoView({block: 'start'});
    if (!el.matches(':popover-open')) el.showPopover();
  }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', open); else open();
  // One listener per document, however many times a boosted visit runs this.
  if (!window.cgFeatureSheetHash) {
    window.cgFeatureSheetHash = true;
    window.addEventListener('hashchange', open);
  }
})();`

func visibleTabs(f Features) []tab {
	out := make([]tab, 0, len(musterTabs))
	for _, t := range musterTabs {
		if t.Key == "connectors" && !f.Connectors {
			continue
		}
		out = append(out, t)
	}
	return out
}

// defaultVisibleTab is the tab `/` serves in a document with these features:
// defaultTab when this document draws it, otherwise the first tab it does draw.
//
// 🔴 THE DEFAULT IS A PROPERTY OF THE DOCUMENT, NOT A CONSTANT, AND THE BROWSER
// IS TOLD IT RATHER THAN KEEPING ITS OWN. navRegistryJS emits this value as
// DEFAULT_TAB. The script used to spell its own default — 'requests', the tab
// the product this was extracted from opened on, which muster has never had —
// so on `/` it resolved the path to a tab with no panel and hid every panel and
// every FAB the server had just drawn correctly. A blank main area under a 200,
// for anyone whose browser had not already saved a tab; it was first seen by a
// freshly invited member. A default that is not one of the drawn tabs is the
// whole defect, so it is derived from the drawn set here, once.
//
// It returns "" only for a document that draws no tab at all, which no
// Features value produces today (tasks and agents are unconditional).
func defaultVisibleTab(f Features) string {
	visible := visibleTabs(f)
	for _, vt := range visible {
		if vt.Key == defaultTab {
			return defaultTab
		}
	}
	if len(visible) > 0 {
		return visible[0].Key
	}
	return ""
}

// normalizeVisibleTab clamps an arbitrary tab string to one this document
// actually draws, defaulting to defaultVisibleTab.
//
// 🔴 IT IS WHAT MAKES A DEEP LINK TO A HIDDEN TAB LAND SOMEWHERE. Every tab is a
// real route whether or not its subsystem was built, so /connectors is served on
// a deployment with no connector store. Clamping only against the full registry
// would render the shell with `activeTab = "connectors"`, i.e. with no panel un-hidden at
// all: a blank page under a correct-looking URL. Clamping here sends that URL to
// the default tab, which is what an unrouted path already does.
func normalizeVisibleTab(t string, f Features) string {
	for _, vt := range visibleTabs(f) {
		if vt.Key == t {
			return t
		}
	}
	return defaultVisibleTab(f)
}
