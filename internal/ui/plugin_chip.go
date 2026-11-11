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
//   - THE BUTTON is in the header, beside the Rendered | Terminal switch:
//     "Preview" and a count (the window's offers plus its open previews,
//     mirror.WindowPreviews, which the window's host reports). It is drawn
//     only while that count is above zero; below `sm` it is a 44×44 icon with
//     the count on it. Its slot re-reads it (PreviewButtonPath) every
//     previewPollEvery while the tab is shown, so it appears and goes as the
//     host's counts move — the page holds no event stream for it.
//   - THE PANEL opens on the button: the plugin's page (the gateway's session
//     page) in a sandboxed frame, a side panel beside the conversation from
//     `sm` up and the whole screen below it. Close (or Escape) unloads the
//     page. "New tab" in its header is the hand-off in a new tab instead.
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
//   - The panel is position:fixed and the button is in a header row that
//     already holds the switch, so neither moves the conversation: the views
//     with a live preview are measured by cls.spec.ts.

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

// PreviewButtonSrc is PreviewButtonPath for one window.
func PreviewButtonSrc(host, session, window string) string {
	q := url.Values{}
	q.Set("host", host)
	q.Set("session", session)
	q.Set("window", window)
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

// PreviewButton is the header button for a window's counts p: nil while it
// has nothing to preview. It carries no htmx wiring (the slot does), so a
// swapped-in button never re-arms a trigger.
func PreviewButton(p mirror.WindowPreviews) g.Node {
	if p.IsZero() {
		return nil
	}
	words := previewWords(p)
	return Button(
		Type("button"),
		ID("preview-toggle"),
		g.Attr("data-preview-toggle", ""),
		g.Attr("data-preview-count", strconv.Itoa(p.Total())),
		g.Attr("aria-controls", "preview-panel"),
		g.Attr("aria-expanded", "false"),
		g.Attr("aria-label", "Preview: "+words),
		g.Attr("title", "Preview: "+words),
		// Below `sm` a 44×44 icon with the count on its corner (the ☰
		// button's badge); from `sm` up the word and the count beside it.
		Class("press relative inline-flex h-11 w-11 shrink-0 items-center justify-center gap-1.5 rounded-lg bg-s2 text-xs font-semibold text-fg ring-1 ring-inset ring-line transition hover:bg-s3 aria-expanded:ring-2 aria-expanded:ring-accent sm:w-auto sm:px-3"),
		g.Raw(previewIcon),
		Span(Class("max-sm:hidden"), g.Text("Preview")),
		Span(
			g.Attr("aria-hidden", "true"),
			Class("inline-flex h-[1.125rem] min-w-[1.125rem] items-center justify-center rounded-full bg-accent px-1 text-[11px] font-semibold leading-none tabular-nums text-on-accent max-sm:pointer-events-none max-sm:absolute max-sm:right-0 max-sm:top-0 max-sm:ring-2 max-sm:ring-bg"),
			g.Text(previewCount(p.Total())),
		),
	)
}

// RenderPreviewButton writes PreviewButton(p) (nothing at zero).
func RenderPreviewButton(w io.Writer, p mirror.WindowPreviews) error {
	n := PreviewButton(p)
	if n == nil {
		return nil
	}
	return n.Render(w)
}

// previewButtonSlot is the header's slot: the button as the page was drawn,
// refreshed every previewPollEvery while the tab is shown. display:contents,
// so an empty slot takes no room and adds no gap to the header row.
func previewButtonSlot(c *PluginChips) g.Node {
	if !c.on() {
		return nil
	}
	return Span(
		ID("preview-slot"),
		Class("contents"),
		hx("hx-get", PreviewButtonSrc(c.Host, c.Session, c.Window)),
		hx("hx-trigger", "every "+previewPollEvery+" [!document.hidden]"),
		hx("hx-swap", "innerHTML"),
		hx("hx-target", "this"),
		PreviewButton(c.Previews),
	)
}

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
			// the page, below the sidebar's overlay and the dialogs).
			Class("fixed inset-0 z-40 flex-col bg-s1 text-fg shadow-2xl ring-1 ring-line [&:not([hidden])]:flex sm:left-auto sm:w-[min(40rem,60vw)] sm:border-l sm:border-line"),
			Div(Class("flex h-12 shrink-0 items-center gap-1 border-b border-line pl-4 pr-1"),
				H2(ID("preview-panel-title"), Class("min-w-0 flex-1 truncate text-sm font-semibold"), g.Text("Preview")),
				picker,
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
  var busy = false;
  function panel() { return document.getElementById('preview-panel'); }
  function toggle() { return document.getElementById('preview-toggle'); }
  function frame() { return panel().querySelector('iframe'); }
  function say(msg) {
    var e = document.getElementById('preview-error');
    if (!e) return;
    e.textContent = msg || '';
    e.hidden = !msg;
  }
  function expanded() {
    var t = toggle(), p = panel();
    if (t) t.setAttribute('aria-expanded', p && !p.hidden ? 'true' : 'false');
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
  function open() {
    var p = panel();
    if (!p.hidden) return;
    p.hidden = false;
    expanded();
    var c = p.querySelector('[data-preview-close]');
    if (c) c.focus();
    show();
  }
  function close() {
    var p = panel();
    if (!p || p.hidden) return;
    p.hidden = true;
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
    if (t.closest('[data-preview-newtab]')) { ev.preventDefault(); newTab(); }
  });
  document.addEventListener('change', function (ev) {
    var t = ev.target;
    if (t && t.matches && t.matches('[data-preview-plugin]') && !panel().hidden) show();
  });
  document.addEventListener('keydown', function (ev) {
    if (ev.key === 'Escape' && panel() && !panel().hidden) close();
  });
  // The slot's refresh swaps the button in fresh: keep it saying whether the
  // panel is open.
  document.addEventListener('htmx:afterSwap', function (ev) {
    if (ev.target && ev.target.id === 'preview-slot') expanded();
  });
})();
`)
