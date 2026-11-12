package ui

import (
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/textplain"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE SESSION VIEWS' PREVIEW BUTTON AND PANEL (docs/plugins.md#the-session-view-chip;
// docs/previews.md): the owner's live viewer plugin (the preview gateway),
// opened from a mirrored window's Rendered and Terminal views — and ONLY while
// that window has something to preview (operator decision: a control above
// every chat whether or not anything was running was noise).
//
//   - THE BUTTON: "Preview" and a count (the window's offers plus its open
//     previews, mirror.WindowPreviews, which the window's host reports),
//     drawn only while that count is above zero. From `sm` (640px) up it is
//     in the header row, beside the Rendered | Terminal switch. 🔴 BELOW `sm`
//     IT IS IN THE HEADER'S SECOND ROW (the status line under the title) —
//     operator decision: at a phone's width the header row holds no room for
//     another 44px button, and taking it would leave the title a few pixels.
//     There it is a 20px pill, the height the row already has, so neither the
//     title row nor the header grows when it appears; its hit area is 44px
//     tall (a ::before over the header's padding, above the title's own hit
//     area). TWO SLOTS, one per place, each `display:none` at the other's
//     width and each re-reading its button (PreviewButtonPath, Place) every
//     previewPollEvery only while the tab is shown AND the page is at its
//     width — so one request per tick, and the page holds no event stream.
//   - THE PANEL opens on the button: the plugin's page (the gateway's session
//     page) in a sandboxed frame, a side panel beside the conversation from
//     `sm` up and the whole screen below it — where everything behind it is
//     made inert while it is open, so Tab stays in it. Close (or Escape, when
//     no other dialog or popover is open to take it) unloads the page. "New
//     tab" in its header is the hand-off in a new tab instead — except in the
//     Android app, where that same control reads "Full screen": the same frame fills the
//     screen, never a new tab (the app would hand that to the system browser;
//     see enterFull in the script).
//   - Both mint a one-time hand-off at CLICK time (POST /plugin-handoffs,
//     aimed at this window) and go to the entry URL it answers; drawing the
//     button mints nothing. One mint at a time.
//   - 🔴 New tab opens its tab SYNCHRONOUSLY, inside the tap (window.open of
//     about:blank), and only then mints; the tab is sent to the minted URL
//     once it answers. A popup blocker (Safari's among them, which drops a tab
//     opened after an await) therefore refuses at the tap, before any code is
//     minted, and the panel says so. A refused mint closes the blank tab and
//     says why. NOOPENER IS KEPT BY HAND: window.open with "noopener" returns
//     no handle to navigate, so the tab is opened with one, its opener is set
//     to null while it is still about:blank (same-origin with muster, so
//     muster may), and only then is it navigated — so the plugin's page sees
//     window.opener === null, as rel="noopener" would give. What is not the
//     same: the tab stays in muster's browsing context group (no process
//     isolation from it), which hands the plugin no reference to muster's
//     window. Referrer: the blank document inherits muster's Referrer-Policy
//     no-referrer, so the plugin gets no Referer. The link still carries
//     target=_blank rel="noopener noreferrer" for what its markup says.
//   - 🔴 The panel's frame is sandboxed WITHOUT allow-top-navigation, so the
//     plugin's content cannot navigate muster's tab; allow="" grants it no
//     camera, microphone or location; referrerpolicy="no-referrer".
//     allow-same-origin is safe because the frame's origin is the plugin's,
//     never muster's (a Host under MUSTER_PLUGIN_ORIGINS is refused 421).
//   - 🔴 THE PAGE'S CSP CARRIES frame-src 'self' <the plugins' origins>
//     WHENEVER THE SLOT IS DRAWN — the owner has a live viewer plugin — and
//     not only while the count is above zero: the button can appear after the
//     page loaded, and its panel must then be allowed to frame the plugin
//     (api's allowSameOriginFrame). With no live viewer plugin nothing here is
//     drawn, nothing polls, and the CSP is the switch's own.
//   - The panel is position:fixed, and the button takes room only inside a
//     row that already has its height (the header row's 44px controls, the
//     second row's 20px), so neither moves the conversation: the views with a
//     live preview are measured by cls.spec.ts, and plugin-chip.spec.ts
//     measures the title and the header unchanged when the button appears
//     at a phone's width.

// PluginChipSandbox is the panel's frame's sandbox, exactly: no
// allow-top-navigation of any kind.
const PluginChipSandbox = "allow-scripts allow-same-origin allow-forms allow-popups allow-downloads allow-modals"

// PreviewButtonPath is the header button's refresh: GET with the window's
// host, session and window, answering PreviewButton for its counts NOW.
const PreviewButtonPath = "/ui/mirror/preview-button"

// previewPollEvery is how often the slot re-reads the button while the tab is
// shown. The host reports its counts every refresh (5 s) after its own 5 s
// listener scan, so this is the same order of delay.
const previewPollEvery = "10s"

// PreviewPlace is where a Preview button is drawn: the header row (from `sm`
// up) or the header's second row (below `sm`).
type PreviewPlace string

const (
	PreviewInHeader PreviewPlace = "header"
	PreviewInRow    PreviewPlace = "row"
)

// PreviewPlaceOf is the place a query's `place` names; anything else is the
// header.
func PreviewPlaceOf(s string) PreviewPlace {
	if s == string(PreviewInRow) {
		return PreviewInRow
	}
	return PreviewInHeader
}

// PreviewButtonSrc is PreviewButtonPath for one window's button at place.
func PreviewButtonSrc(host, session, window string, place PreviewPlace) string {
	q := url.Values{}
	q.Set("host", host)
	q.Set("session", session)
	q.Set("window", window)
	q.Set("place", string(place))
	return PreviewButtonPath + "?" + q.Encode()
}

// PluginChip is one live viewer plugin the panel can open.
type PluginChip struct {
	ID    int64
	Label string
}

// PluginChips is a session view's preview control: the owner's live viewer
// plugins (newest first), the mint route, the window every hand-off is aimed
// at, and what that window has to preview now. nil draws nothing.
type PluginChips struct {
	MintPath              string
	Host, Session, Window string
	Chips                 []PluginChip
	// Previews is the window's counts when the page was drawn
	// (mirror.SessionView.Previews): the button's first state.
	Previews mirror.WindowPreviews
}

// on: there is a plugin to open, so the slot, the panel and the frame-src
// are drawn.
func (c *PluginChips) on() bool { return c != nil && len(c.Chips) > 0 }

// previewWords is what a window has to preview, as words: "1 preview open,
// 2 servers to preview".
func previewWords(p mirror.WindowPreviews) string {
	var parts []string
	plural := func(n int, one, many string) string {
		if n == 1 {
			return "1 " + one
		}
		return strconv.Itoa(n) + " " + many
	}
	if p.Open > 0 {
		parts = append(parts, plural(p.Open, "preview open", "previews open"))
	}
	if p.Offered > 0 {
		parts = append(parts, plural(p.Offered, "server to preview", "servers to preview"))
	}
	return strings.Join(parts, ", ")
}

// previewCount is a count as drawn: "99+" past it.
func previewCount(n int) string {
	if n > mirror.MaxPreviewCount {
		return strconv.Itoa(mirror.MaxPreviewCount) + "+"
	}
	return strconv.Itoa(n)
}

const previewIcon = `<svg class="h-5 w-5 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="4" width="18" height="16" rx="2"/><path d="M3 9h18M7 6.5h.01M10 6.5h.01"/></svg>`

// PreviewButton is the Preview button for a window's counts p at place: nil
// while it has nothing to preview. It carries no htmx wiring (the slot does),
// so a swapped-in button never re-arms a trigger. The two places' buttons have
// their own ids (preview-toggle, preview-toggle-row) and share
// data-preview-toggle, which the panel's script reads.
func PreviewButton(p mirror.WindowPreviews, place PreviewPlace) g.Node {
	if p.IsZero() {
		return nil
	}
	words := previewWords(p)
	id, cls, icon := "preview-toggle",
		"press relative inline-flex h-11 shrink-0 items-center justify-center gap-1.5 rounded-lg bg-s2 px-3 text-xs font-semibold text-fg ring-1 ring-inset ring-line transition hover:bg-s3 aria-expanded:ring-2 aria-expanded:ring-accent",
		previewIcon
	badge := "inline-flex h-[1.125rem] min-w-[1.125rem] items-center justify-center rounded-full bg-accent px-1 text-[11px] font-semibold leading-none tabular-nums text-on-accent"
	if place == PreviewInRow {
		badge = "inline-flex h-3.5 min-w-[0.875rem] items-center justify-center rounded-full bg-accent px-1 text-[10px] font-semibold leading-none tabular-nums text-on-accent"
		// 20px tall with -2px margins above and below: it lays out as 16px,
		// the status line's own height, so the row — and the title row above
		// it, which the header centres with it — does not move when it
		// appears. The ::before is its 44px
		// thumb target (centred on it, so it reaches over the header's
		// padding, not the title row's text), z-20 above the title's z-10 hit
		// area (mirrorTitleHitClass), which spans this row too.
		id = "preview-toggle-row"
		cls = "press relative z-20 -my-0.5 inline-flex h-5 shrink-0 items-center gap-1 rounded-full bg-s2 px-2 text-[11px] font-semibold leading-none text-fg ring-1 ring-inset ring-line aria-expanded:ring-2 aria-expanded:ring-accent " +
			"before:absolute before:-inset-x-1 before:top-1/2 before:h-11 before:-translate-y-1/2 before:content-['']"
		icon = strings.Replace(previewIcon, `class="h-5 w-5 shrink-0"`, `class="h-3.5 w-3.5 shrink-0"`, 1)
	}
	return Button(
		Type("button"),
		ID(id),
		g.Attr("data-preview-toggle", string(place)),
		g.Attr("data-preview-count", strconv.Itoa(p.Total())),
		g.Attr("aria-controls", "preview-panel"),
		g.Attr("aria-expanded", "false"),
		g.Attr("aria-label", "Preview: "+words),
		g.Attr("title", "Preview: "+words),
		Class(cls),
		g.Raw(icon),
		g.If(place == PreviewInHeader, Span(g.Text("Preview"))),
		Span(g.Attr("aria-hidden", "true"), Class(badge), g.Text(previewCount(p.Total()))),
	)
}

// RenderPreviewButton writes PreviewButton(p, place) (nothing at zero).
func RenderPreviewButton(w io.Writer, p mirror.WindowPreviews, place PreviewPlace) error {
	n := PreviewButton(p, place)
	if n == nil {
		return nil
	}
	return n.Render(w)
}

// previewWide is the slots' width test: the page is at `sm` (640px) or
// wider — Tailwind's sm, a min-width on the viewport, which innerWidth is.
const previewWide = "window.innerWidth>=640"

// previewButtonSlot is the slot for place: the button as the page was drawn,
// re-read every previewPollEvery while the tab is shown and the page is at
// the place's width. Hidden at the other width, and while empty (empty:hidden:
// an empty slot takes no room and adds no gap to its row).
func previewButtonSlot(c *PluginChips, place PreviewPlace) g.Node {
	if !c.on() {
		return nil
	}
	id, cls, at := "preview-slot", "inline-flex shrink-0 empty:hidden max-sm:hidden", previewWide
	if place == PreviewInRow {
		id, cls, at = "preview-slot-row", "ml-auto inline-flex shrink-0 empty:hidden sm:hidden", "!("+previewWide+")"
	}
	return Span(
		ID(id),
		Class(cls),
		hx("hx-get", PreviewButtonSrc(c.Host, c.Session, c.Window, place)),
		hx("hx-trigger", "every "+previewPollEvery+" [!document.hidden&&"+at+"]"),
		hx("hx-swap", "innerHTML"),
		hx("hx-target", "this"),
		PreviewButton(c.Previews, place),
	)
}

// PreviewFullAttr marks the panel while it is full screen (the Android app
// only: its header's one hand-off control reads Full screen there).
const PreviewFullAttr = "data-preview-full"

const exitFullIcon = `<svg class="h-4 w-4 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M9 4v5H4M15 4v5h5M9 20v-5H4M15 20v-5h5"/></svg>`

// previewPanel is the panel the button opens (closed), and its script.
func previewPanel(c *PluginChips) g.Node {
	if !c.on() {
		return nil
	}
	first := c.Chips[0]
	// More than one live viewer plugin: the panel's header picks which one
	// it shows (newest first, the first chosen).
	var picker g.Node
	if len(c.Chips) > 1 {
		opts := make([]g.Node, 0, len(c.Chips))
		for _, p := range c.Chips {
			opts = append(opts, Option(Value(strconv.FormatInt(p.ID, 10)), g.Text(textplain.Title(p.Label))))
		}
		picker = Select(g.Attr("data-preview-plugin", ""), g.Attr("aria-label", "Plugin"),
			Class("min-h-[44px] max-w-[9rem] shrink-0 truncate rounded-lg bg-s2 px-2 text-xs text-fg ring-1 ring-inset ring-line"),
			g.Group(opts))
	}
	btn := "press inline-flex min-h-[44px] shrink-0 items-center rounded-lg px-3 text-sm font-semibold hover:bg-s2"
	return g.Group{
		Aside(
			ID("preview-panel"),
			g.Attr("hidden", ""),
			g.Attr("aria-labelledby", "preview-panel-title"),
			g.Attr("data-mint", c.MintPath),
			g.Attr("data-host", c.Host),
			g.Attr("data-window", c.Window),
			g.Attr("data-plugin", strconv.FormatInt(first.ID, 10)),
			// The whole screen below `sm`; a side panel from `sm` up, over
			// the conversation's right part and under nothing (z-40: above
			// the page, below the sidebar's overlay and the dialogs). Full
			// screen (PreviewFullAttr, the Android app only) it is the whole
			// screen at every width, its header hidden.
			Class("group fixed inset-0 z-40 flex-col bg-s1 text-fg shadow-2xl ring-1 ring-line [&:not([hidden])]:flex sm:left-auto sm:w-[min(40rem,60vw)] sm:border-l sm:border-line "+
				"data-[preview-full]:ring-0 data-[preview-full]:sm:left-0 data-[preview-full]:sm:w-full data-[preview-full]:sm:border-l-0"),
			Div(Class("flex h-12 shrink-0 items-center gap-1 border-b border-line pl-4 pr-1 group-data-[preview-full]:hidden"),
				H2(ID("preview-panel-title"), Class("min-w-0 flex-1 truncate text-sm font-semibold"), g.Text("Preview")),
				picker,
				// ONE hand-off control: New tab in a browser; in the Android
				// app the script relabels it Full screen when the panel opens,
				// and its tap keeps the preview in this frame.
				A(
					g.Attr("data-preview-newtab", ""),
					Href("#"),
					Target("_blank"),
					Rel("noopener noreferrer"),
					g.Attr("aria-label", "Open the preview in a new tab"),
					Class(btn+" text-accent"),
					g.Text("New tab"),
				),
				Button(Type("button"), g.Attr("data-preview-close", ""), g.Attr("aria-label", "Close the preview"),
					Class(btn+" text-fg2"), g.Text("Close")),
			),
			// Full screen's way out, over the preview's top right corner:
			// drawn only while full screen.
			Button(Type("button"), g.Attr("data-preview-exit-full", ""), g.Attr("aria-label", "Exit full screen"),
				Class("press absolute right-2 top-[max(0.5rem,env(safe-area-inset-top))] z-10 hidden min-h-[44px] items-center gap-1.5 rounded-full bg-s1/90 px-3 text-sm font-semibold text-fg shadow-lg ring-1 ring-line group-data-[preview-full]:inline-flex"),
				g.Raw(exitFullIcon), Span(g.Text("Exit"))),
			P(ID("preview-error"), g.Attr("role", "status"), g.Attr("hidden", ""),
				Class("shrink-0 border-b border-line bg-danger/15 px-3 py-2 text-xs text-fg break-words")),
			IFrame(
				g.Attr("sandbox", PluginChipSandbox),
				g.Attr("allow", ""),
				g.Attr("referrerpolicy", "no-referrer"),
				g.Attr("title", "Preview"),
				Class("min-h-0 w-full flex-1 border-0 bg-bg"),
			),
		),
		staticScript(previewPanelScriptSrc),
	}
}

// previewPanelScriptSrc opens and closes the panel, mints at click time and
// sends the frame or the new tab on (see the file comment for the tab). A
// refused mint (a revoked token, the window gone, the rate cap), an expired
// sign-in and a blocked tab each say why in the panel and go nowhere.
var previewPanelScriptSrc = staticJS("preview-panel", `
(function () {
  if (window.__musterPreviewPanel) return;
  window.__musterPreviewPanel = true;
  var SIGN_IN = 'Your sign-in has expired: reload the page and sign in again.';
  var BLOCKED = 'The browser blocked the new tab. Allow pop-ups for this site, or use the preview here.';
  var FULL = '`+PreviewFullAttr+`';
  var busy = false;
  function panel() { return document.getElementById('preview-panel'); }
  // The button on screen: the header's, or the second row's below sm.
  function toggles() { return document.querySelectorAll('[data-preview-toggle]'); }
  function toggle() {
    var ts = toggles();
    for (var i = 0; i < ts.length; i++) if (ts[i].getClientRects().length) return ts[i];
    return null;
  }
  function frame() { return panel().querySelector('iframe'); }
  function say(msg) {
    var e = document.getElementById('preview-error');
    if (!e) return;
    e.textContent = msg || '';
    e.hidden = !msg;
  }
  function expanded() {
    var ts = toggles(), p = panel(), on = p && !p.hidden ? 'true' : 'false';
    for (var i = 0; i < ts.length; i++) ts[i].setAttribute('aria-expanded', on);
  }
  function plugin() {
    var p = panel(), s = p.querySelector('[data-preview-plugin]');
    return Number(s ? s.value : p.getAttribute('data-plugin'));
  }
  function mint() {
    var p = panel();
    var body = { plugin: plugin(), target: { mirror: { host: p.getAttribute('data-host'), window: p.getAttribute('data-window') } } };
    return fetch(p.getAttribute('data-mint'), {
      method: 'POST', credentials: 'same-origin', cache: 'no-store', redirect: 'manual',
      headers: { 'Content-Type': 'application/json', 'Accept': 'application/json' },
      body: JSON.stringify(body)
    }).then(function (res) {
      // An expired session is sent to the sign-in page: a redirect, or an
      // HTML page that is not this route's JSON.
      var json = (res.headers.get('Content-Type') || '').indexOf('application/json') === 0;
      if (res.type === 'opaqueredirect' || res.status === 401 || !json) throw new Error(SIGN_IN);
      return res.json().catch(function () { return {}; }).then(function (b) {
        if (!res.ok || !b || typeof b.url !== 'string') throw new Error((b && b.error) || ('the hand-off was refused (' + res.status + ')'));
        return b.url;
      });
    });
  }
  function show() {
    if (busy) return;
    busy = true;
    say('');
    mint().then(function (url) {
      if (!panel().hidden) frame().src = url;
    }).catch(function (e) {
      say(e && e.message ? e.message : 'the hand-off failed');
    }).then(function () { busy = false; });
  }
  // FULL SCREEN (below sm) the panel is all there is: everything behind it is
  // made inert, so Tab cannot reach a control it covers. Only what this set
  // inert is restored. Beside the page (sm and up) nothing is: the
  // conversation stays usable next to it.
  var inerted = [];
  function shield() {
    var p = panel(), full = p && !p.hidden && (window.innerWidth < 640 || p.hasAttribute(FULL));
    if (full && !inerted.length) {
      var kids = document.body.children;
      for (var i = 0; i < kids.length; i++) {
        var k = kids[i];
        if (k === p || k.contains(p) || k.tagName === 'SCRIPT' || k.inert) continue;
        k.inert = true;
        inerted.push(k);
      }
    } else if (!full && inerted.length) {
      inerted.forEach(function (k) { k.inert = false; });
      inerted = [];
    }
  }
  window.addEventListener('resize', shield);

  // 🔴 INSIDE THE ANDROID APP NEW TAB IS FULL SCREEN (operator decision): the
  // app's WebView sends every origin but muster's to the system browser, so a
  // new tab of the plugin's page left the app. There the same control reads
  // Full screen, and its tap makes the SAME frame, never reloaded (its
  // partitioned cookie and state carry over), fill the screen at every width,
  // the header hidden, with an Exit button over its corner. Escape, Exit, Close and the app's
  // Back each leave it (Close also closes the panel).
  //
  // THE APP is the bridge object the app's WebView injects (AppBridgeName),
  // read in the MAIN frame: the panel can be inside the view-swap frame, and
  // the app refuses a bridge message from a subframe. In a browser there is
  // no such object, and New tab is exactly what it was.
  //
  // BACK: the shell offers its Back press to the page while the page posts a
  // back-claim (switcher_dock.go, docs/design/switcher-dock.md "The shell
  // change"), asking the main frame's window.musterBack. While full screen
  // this panel claims it, and the main frame's musterBack is wrapped (once)
  // to leave full screen first, else to answer as before (the dock's close).
  function shellWin() {
    var w = window;
    try { while (w !== w.top && w.parent && w.parent.document) w = w.parent; } catch (e) {}
    return w;
  }
  function bridge() {
    try { var b = shellWin().`+AppBridgeName+`; return b && typeof b.postMessage === 'function' ? b : null; } catch (e) { return null; }
  }
  function inApp() { return !!bridge(); }
  function claim(on) {
    var b = bridge();
    try { if (b) b.postMessage(JSON.stringify({ type: 'back-claim', open: on })); } catch (e) {}
  }
  // Wrapped when full screen is entered (the dock has defined its own by
  // then), and again if anything has replaced the wrapper since.
  function hookBack() {
    var S = shellWin();
    if (S.musterBack && S.musterBack === S.__musterPreviewBack) return;
    var prev = S.musterBack;
    S.musterBack = S.__musterPreviewBack = function () {
      var o = S.__musterPreviewFull;
      if (o) {
        S.__musterPreviewFull = null;
        var live = false;
        try { live = !o.win.closed && o.exit(); } catch (e) {}
        if (live) return true;
      }
      return typeof prev === 'function' ? prev() : false;
    };
  }
  function isFull() { var p = panel(); return !!(p && p.hasAttribute(FULL)); }
  function enterFull() {
    var p = panel();
    if (!p || p.hidden || isFull()) return;
    p.setAttribute(FULL, '');
    shield();
    hookBack();
    var S = shellWin();
    S.__musterPreviewFull = { win: window, exit: exitFull };
    claim(true);
    var x = p.querySelector('[data-preview-exit-full]');
    if (x) x.focus({ preventScroll: true });
  }
  // exitFull leaves full screen; true when it was. Focus goes back to Full
  // screen unless the panel is closing (close() moves it to the button).
  function exitFull(closing) {
    var p = panel();
    if (!p || !p.hasAttribute(FULL)) return false;
    p.removeAttribute(FULL);
    try { var S = shellWin(); if (S.__musterPreviewFull && S.__musterPreviewFull.win === window) S.__musterPreviewFull = null; } catch (e) {}
    claim(false);
    shield();
    if (closing !== true) {
      var f = p.querySelector('[data-preview-newtab]');
      if (f) f.focus({ preventScroll: true });
    }
    return true;
  }
  // htmx's history snapshot is the body's HTML: never take it full screen.
  document.addEventListener('htmx:beforeHistorySave', function () { exitFull(true); });
  window.addEventListener('pagehide', function () { if (isFull()) exitFull(true); });

  // label is the hand-off control's words for where the panel runs: New tab
  // in a browser (as drawn), Full screen in the app.
  function label(p) {
    var a = p.querySelector('[data-preview-newtab]');
    if (!a || !inApp()) return;
    a.textContent = 'Full screen';
    a.setAttribute('aria-label', 'Show the preview full screen');
  }
  function open() {
    var p = panel();
    if (!p.hidden) return;
    label(p);
    p.hidden = false;
    shield();
    expanded();
    var c = p.querySelector('[data-preview-close]');
    if (c) c.focus();
    show();
  }
  function close() {
    var p = panel();
    if (!p || p.hidden) return;
    exitFull(true);
    p.hidden = true;
    shield();
    // The plugin's page ends with the panel: nothing of it runs on hidden.
    frame().src = 'about:blank';
    say('');
    expanded();
    var t = toggle();
    if (t) t.focus();
  }
  function newTab() {
    if (busy) return;
    say('');
    // Inside the tap, before any await (see the file comment).
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
  document.addEventListener('click', function (ev) {
    var t = ev.target;
    if (!t || !t.closest || !panel()) return;
    if (t.closest('[data-preview-toggle]')) { ev.preventDefault(); if (panel().hidden) open(); else close(); return; }
    if (t.closest('[data-preview-close]')) { ev.preventDefault(); close(); return; }
    // The one hand-off control: in the app a new tab would leave it, so
    // there it is full screen, never window.open.
    if (t.closest('[data-preview-newtab]')) { ev.preventDefault(); if (inApp()) enterFull(); else newTab(); return; }
    if (t.closest('[data-preview-exit-full]')) { ev.preventDefault(); exitFull(); }
  });
  document.addEventListener('change', function (ev) {
    var t = ev.target;
    if (t && t.matches && t.matches('[data-preview-plugin]') && !panel().hidden) show();
  });
  // Escape closes the panel only when it is the panel's: not one another
  // handler already took, and not while another dialog or popover is open
  // (Escape is that one's to close).
  function otherOpen() {
    if (document.querySelector('dialog[open]')) return true;
    try { return !!document.querySelector(':popover-open'); } catch (e) { return false; }
  }
  document.addEventListener('keydown', function (ev) {
    if (ev.key !== 'Escape' || ev.defaultPrevented || !panel() || panel().hidden || otherOpen()) return;
    ev.preventDefault();
    // Full screen, the first Escape leaves it; the next closes the panel.
    if (!exitFull()) close();
  });
  // The slot's refresh swaps the button in fresh: keep it saying whether the
  // panel is open.
  document.addEventListener('htmx:afterSwap', function (ev) {
    if (ev.target && (ev.target.id === 'preview-slot' || ev.target.id === 'preview-slot-row')) expanded();
  });
})();
`)
