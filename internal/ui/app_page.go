package ui

import (
	"strconv"
	"strings"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/web"
)

// THE APP TAB'S PAGE (docs/design/meetings.md, "The tab", "The page", "The
// frame, exactly", "When the token is revoked", "The Android app", "Mobile
// layout"; internal/api plugin_app.go decides who sees it).
//
//   - The sidebar draws one entry per live app plugin of the signed-in owner
//     (Features.Apps), after every built-in section link, oldest first; its
//     own page marks it aria-current (NavAppEntry).
//   - The page is the frame: the sidebar an overlay at every width, a slim
//     header (☰, the tab label, New tab), and below it one frame filling the
//     rest of the dynamic viewport, inside the safe-area insets. No message
//     bar, no chief panel.
//   - 🔴 THE LABEL IS THE OWNER'S TEXT, AND IT CONTAINS ' AND & (1a's label
//     rule). It is written ONLY through g.Text and g.Attr, which escape it for
//     HTML text and for a quoted attribute, and it never enters a script: the
//     page's script is a static file (appPageScriptSrc) that reads no label at
//     all. TestTheAppPageEscapesAHostileLabelInEveryContext renders a label no
//     validator would admit and checks each context.
//   - The script mints a hand-off at load (POST /plugin-handoffs {"plugin",
//     "target": {"app": {…}}}) and puts a NEW frame, its src set before it is
//     inserted, in the place of the one drawn here — the preview panel's
//     no-history trick, so a reload re-mints and Back never walks the frame to
//     a spent code. A refused mint is said in place of the frame.
//   - Where the Android app's bridge exists (window.musterApp) no frame is
//     loaded: the frame is removed and a card says the app opens in the
//     browser; its Open mints at tap time and navigates top-level, which the
//     app hands to a Custom Tab (its WebView grants no camera to another
//     origin and has no screen capture).
//   - While the page is visible it polls the alive route every 30 s (and at
//     once when it becomes visible); a 404 (the plugin is no longer live) or a
//     401 (the owner signed out or disabled) REMOVES the frame — ending the
//     plugin's page and with it its camera, microphone and screen capture —
//     and says AppRemoved. Core's enforcement; it does not need the plugin to
//     behave.

// AppTab is one app plugin's sidebar entry: its id, the owner's tab label and
// the icon's emoji.
type AppTab struct {
	ID    int64
	Label string
	Icon  string
}

// navAppPrefix is the nav entry an app page marks: NavAppEntry.
const navAppPrefix = "app:"

// NavAppEntry is the nav entry (sidebar's activeTab / currentTab) of the app
// plugin id's page.
func NavAppEntry(id int64) string { return navAppPrefix + strconv.FormatInt(id, 10) }

// AppPagePath is the app plugin id's page; AppAlivePath the route its page
// polls.
func AppPagePath(id int64) string  { return "/apps/" + strconv.FormatInt(id, 10) }
func AppAlivePath(id int64) string { return "/ui/apps/" + strconv.FormatInt(id, 10) + "/alive" }

// AppRemoved is what the page says once the alive poll answers 404 or 401.
const AppRemoved = "This app was removed."

// AppPollSeconds is the alive poll's period while the page is visible.
const AppPollSeconds = 30

// AppFrameAllow is the frame's allow attribute for origin: exactly the six
// features the design delegates, each to exactly that origin (never a
// wildcard), in the page Permissions-Policy's order.
func AppFrameAllow(origin string) string {
	features := []string{"camera", "microphone", "display-capture", "fullscreen", "autoplay", "clipboard-write"}
	parts := make([]string, 0, len(features))
	for _, f := range features {
		parts = append(parts, f+" "+origin)
	}
	return strings.Join(parts, "; ")
}

// appTabLinks is the sidebar's app entries (Features.Apps), nil for none.
func appTabLinks(feat Features, activeTab, currentTab string) g.Node {
	if len(feat.Apps) == 0 {
		return nil
	}
	links := make([]g.Node, 0, len(feat.Apps))
	for _, a := range feat.Apps {
		entry := NavAppEntry(a.ID)
		links = append(links, A(ID("sidebar-app-"+strconv.FormatInt(a.ID, 10)), Href(AppPagePath(a.ID)), hx("hx-boost", "false"),
			g.Attr("data-app-tab", strconv.FormatInt(a.ID, 10)),
			g.If(currentTab == entry, g.Attr("aria-current", "page")),
			Class("press flex min-h-[44px] w-full items-center gap-3 rounded-lg px-3 text-left text-sm font-medium transition "+navEntryTone(activeTab == entry)),
			Span(Class("text-base"), g.Attr("aria-hidden", "true"), g.Text(a.Icon)), Span(Class("min-w-0 truncate"), g.Text(a.Label))))
	}
	return g.Group(links)
}

// AppPageView is an app tab's page.
type AppPageView struct {
	Nav NavChrome
	// ID is the plugin's id; Label the owner's tab label (plain text).
	ID    int64
	Label string
	// Origin is the entry URL's exact origin: the frame's allow names it.
	Origin string
	// Ref is a validated ?ref= hint (plugins.ValidAppRef), "" for none: the
	// mint passes it through.
	Ref string
	// MintPath is the hand-off route the script posts to.
	MintPath string
}

// AppPage is the app tab's page document.
func AppPage(v AppPageView) g.Node {
	id := strconv.FormatInt(v.ID, 10)
	btn := "press inline-flex min-h-[44px] shrink-0 items-center rounded-lg px-3 text-sm font-semibold"
	// A control the script may hide: its display only while not [hidden]
	// (a display utility would otherwise win over the hidden attribute).
	hiddenBtn := "press [&:not([hidden])]:inline-flex min-h-[44px] shrink-0 items-center rounded-lg px-3 text-sm font-semibold"
	return Doctype(
		HTML(Class("dark"), Lang("en"),
			Head(
				Meta(Charset("utf-8")),
				Meta(Name("viewport"), Content("width=device-width, initial-scale=1, viewport-fit=cover")),
				Meta(Name("color-scheme"), Content("dark light")),
				ThemeHead(),
				Meta(Name("robots"), Content("noindex")),
				docHead(v.Nav.Feat.Brand),
				TitleEl(g.Text(v.Label+" · muster")),
				UserMarker(v.Nav.Feat),
				Link(Rel("stylesheet"), Href(web.Asset("/static/app.css"))),
				overlayNavHead(),
				faroHead(),
			),
			Body(
				// One dynamic-viewport-high column: the header, then the frame
				// in the rest. Nothing scrolls but the plugin's own page.
				Class("flex h-dvh flex-col overflow-hidden bg-bg text-fg antialiased"),
				// An overlay at every width, so the frame has the whole width.
				overlayNav(v.Nav, NavAppEntry(v.ID)),
				Header(
					g.Attr("data-app-header", ""),
					Class("shrink-0 border-b border-line bg-bg pt-[env(safe-area-inset-top)] pl-[env(safe-area-inset-left)] pr-[env(safe-area-inset-right)]"),
					Div(Class("flex flex-nowrap items-center gap-2 px-2 py-1"),
						overlayNavButton(nil, true),
						H1(Class("min-w-0 flex-1 truncate text-base font-semibold text-fg"), g.Text(v.Label)),
						// New tab; in the Android app the script hides it (the
						// card's Open is the way out there).
						Button(Type("button"), g.Attr("data-app-newtab", ""), Class(hiddenBtn+" text-accent hover:bg-s2"), g.Text("New tab")),
					),
				),
				Main(
					ID("app-main"),
					g.Attr("data-app-page", ""),
					g.Attr("data-plugin", id),
					g.Attr("data-mint", v.MintPath),
					g.Attr("data-alive", AppAlivePath(v.ID)),
					g.Attr("data-poll", strconv.Itoa(AppPollSeconds)),
					g.Attr("data-removed", AppRemoved),
					g.If(v.Ref != "", g.Attr("data-ref", v.Ref)),
					Class("relative flex min-h-0 flex-1 flex-col pb-[env(safe-area-inset-bottom)] pl-[env(safe-area-inset-left)] pr-[env(safe-area-inset-right)]"),
					P(ID("app-status"), g.Attr("role", "status"), g.Attr("aria-live", "polite"), g.Attr("hidden", ""),
						Class("shrink-0 border-b border-line bg-danger/15 px-3 py-2 text-sm text-fg break-words")),
					IFrame(
						g.Attr("data-app-frame", ""),
						g.Attr("sandbox", PluginChipSandbox),
						g.Attr("allow", AppFrameAllow(v.Origin)),
						g.Attr("referrerpolicy", "no-referrer"),
						g.Attr("title", v.Label),
						Class("min-h-0 w-full flex-1 border-0 bg-bg"),
					),
					// The Android app's card (window.musterApp): shown by the
					// script in place of the frame.
					Div(g.Attr("data-app-card", ""), g.Attr("hidden", ""),
						Class("m-4 flex-col items-start gap-3 rounded-xl bg-s1 p-4 ring-1 ring-line [&:not([hidden])]:flex"),
						P(Class("text-sm text-fg"), g.Text(v.Label+" opens in your browser")),
						Button(Type("button"), g.Attr("data-app-open", ""), Class(btn+" bg-accent text-white"), g.Text("Open")),
					),
				),
				appScript(v.Nav.Feat),
				pwaChrome(),
				staticScript(appPageScriptSrc),
			),
		),
	)
}

// appPageScriptSrc mints at load, swaps in the frame (or, in the Android
// app, shows the card), opens New tab, and runs the alive poll. It reads no
// label: every value it needs is a data-* attribute of #app-main.
var appPageScriptSrc = staticJS("app-page", `
(function () {
  if (window.__musterAppPage) return;
  window.__musterAppPage = true;
  var SIGN_IN = 'Your sign-in has expired: reload the page and sign in again.';
  var BLOCKED = 'The browser blocked the new tab. Allow pop-ups for this site.';
  var gone = false, busy = false, timer = null;
  function root() { return document.getElementById('app-main'); }
  function frame() { var r = root(); return r && r.querySelector('[data-app-frame]'); }
  function card() { var r = root(); return r && r.querySelector('[data-app-card]'); }
  function say(msg) {
    var e = document.getElementById('app-status');
    if (!e) return;
    e.textContent = msg || '';
    e.hidden = !msg;
  }
  function mint() {
    var r = root();
    var app = {};
    if (r.getAttribute('data-ref')) app.ref = r.getAttribute('data-ref');
    return fetch(r.getAttribute('data-mint'), {
      method: 'POST', credentials: 'same-origin', cache: 'no-store', redirect: 'manual',
      headers: { 'Content-Type': 'application/json', 'Accept': 'application/json' },
      body: JSON.stringify({ plugin: Number(r.getAttribute('data-plugin')), target: { app: app } })
    }).then(function (res) {
      var json = (res.headers.get('Content-Type') || '').indexOf('application/json') === 0;
      if (res.type === 'opaqueredirect' || res.status === 401 || !json) throw new Error(SIGN_IN);
      return res.json().catch(function () { return {}; }).then(function (b) {
        if (!res.ok || !b || typeof b.url !== 'string') throw new Error((b && b.error) || ('the hand-off was refused (' + res.status + ')'));
        return b.url;
      });
    });
  }
  // 🔴 A NEW FRAME, src SET BEFORE IT IS INSERTED, in the old one's place,
  // with every one of its attributes (the sandbox and allow first of all):
  // a new frame's first load adds no history entry.
  function load(url) {
    var old = frame();
    if (!old || gone) return;
    var f = document.createElement('iframe');
    for (var i = 0; i < old.attributes.length; i++) {
      var a = old.attributes[i];
      if (a.name !== 'src') f.setAttribute(a.name, a.value);
    }
    f.src = url;
    old.replaceWith(f);
  }
  function removeFrame() { var f = frame(); if (f) f.remove(); }
  function removed() {
    if (gone) return;
    gone = true;
    removeFrame();
    var c = card(); if (c) c.remove();
    var n = document.querySelector('[data-app-newtab]'); if (n) n.hidden = true;
    if (timer) clearInterval(timer);
    say(root().getAttribute('data-removed'));
  }
  function alive() {
    if (gone || document.visibilityState !== 'visible') return;
    fetch(root().getAttribute('data-alive'), { credentials: 'same-origin', cache: 'no-store', redirect: 'manual' })
      // 404: the plugin is no longer live. 401: the session door refused
      // the owner (signed out, or the account disabled). Either way the
      // frame, and any capture in it, goes.
      .then(function (res) { if (res.status === 404 || res.status === 401) removed(); })
      .catch(function () {});
  }
  function newTab() {
    if (busy || gone) return;
    say('');
    var tab = window.open('about:blank', '_blank');
    if (!tab) { say(BLOCKED); return; }
    try { tab.opener = null; } catch (e) {}
    busy = true;
    mint().then(function (url) {
      if (tab.closed) { say(BLOCKED); return; }
      tab.location.replace(url);
    }).catch(function (e) {
      try { tab.close(); } catch (x) {}
      say(e && e.message ? e.message : 'the hand-off failed');
    }).then(function () { busy = false; });
  }
  function openTop() {
    if (busy || gone) return;
    busy = true;
    say('');
    mint().then(function (url) { window.location.assign(url); })
      .catch(function (e) { say(e && e.message ? e.message : 'the hand-off failed'); })
      .then(function () { busy = false; });
  }
  function start() {
    if (!root()) return;
    if (window.musterApp) {
      removeFrame();
      var n = document.querySelector('[data-app-newtab]'); if (n) n.hidden = true;
      var c = card(); if (c) c.hidden = false;
    } else {
      mint().then(load).catch(function (e) { removeFrame(); say(e && e.message ? e.message : 'the hand-off failed'); });
    }
    var secs = Number(root().getAttribute('data-poll')) || 30;
    timer = setInterval(alive, secs * 1000);
    document.addEventListener('visibilitychange', alive);
  }
  document.addEventListener('click', function (ev) {
    var t = ev.target;
    if (!t || !t.closest) return;
    if (t.closest('[data-app-newtab]')) { ev.preventDefault(); newTab(); return; }
    if (t.closest('[data-app-open]')) { ev.preventDefault(); openTop(); }
  });
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', start);
  else start();
})();
`)
