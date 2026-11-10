package ui

import (
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE SESSION-VIEW CHIP (docs/plugins.md#the-session-view-chip;
// docs/design/previews.md seam (b)): one static chip per live viewer plugin of
// the owner, on a mirrored window's Rendered and Terminal views. Each has
// Open (a new tab) and Show here (a sandboxed frame over the page). Both mint
// a one-time hand-off at CLICK time (POST /plugin-handoffs, aimed at this
// window) and go to the entry URL it answers; drawing the chip mints nothing.
//
//   - 🔴 Open is a target=_blank link with rel="noopener noreferrer": the
//     plugin's page gets no window.opener handle on muster's tab, and no
//     Referer (muster's own Referrer-Policy is no-referrer as well).
//   - 🔴 Show here's frame is sandboxed WITHOUT allow-top-navigation, so the
//     plugin's content cannot navigate muster's tab; allow="" grants it no
//     camera, microphone or location; referrerpolicy="no-referrer".
//     allow-same-origin is safe because the frame's origin is the plugin's,
//     never muster's (a Host under MUSTER_PLUGIN_ORIGINS is refused 421).
//   - The row has a FIXED height and is server-drawn with the page, so it
//     moves nothing once the page has loaded (the cls.spec.ts gate).
//   - The page's CSP carries frame-src 'self' <the plugins' origins> only
//     while this is drawn (api's allowSameOriginFrame).

// PluginChipSandbox is Show here's frame's sandbox, exactly: no
// allow-top-navigation of any kind.
const PluginChipSandbox = "allow-scripts allow-same-origin allow-forms allow-popups allow-downloads allow-modals"

// PluginChip is one viewer plugin's chip.
type PluginChip struct {
	ID    int64
	Label string
}

// PluginChips is a session view's chip row: the plugins, the mint route and
// the window every hand-off is aimed at. nil draws nothing.
type PluginChips struct {
	MintPath     string
	Host, Window string
	Chips        []PluginChip
}

// pluginChipRow is the row under the header, and the one frame dialog its
// Show here buttons share.
func pluginChipRow(c *PluginChips) g.Node {
	if c == nil || len(c.Chips) == 0 {
		return nil
	}
	chips := make([]g.Node, 0, len(c.Chips))
	for _, p := range c.Chips {
		chips = append(chips, Div(
			g.Attr("data-plugin-chip", strconv.FormatInt(p.ID, 10)),
			g.Attr("data-plugin-label", p.Label),
			Class("flex shrink-0 items-center gap-1 rounded-full bg-s2 py-0.5 pl-3 pr-1 ring-1 ring-inset ring-line"),
			Span(Class("max-w-[10rem] truncate font-semibold text-fg"), g.Text(p.Label)),
			A(
				g.Attr("data-plugin-open", ""),
				Href("#"),
				Target("_blank"),
				Rel("noopener noreferrer"),
				g.Attr("aria-label", "Open "+p.Label+" in a new tab"),
				Class("press inline-flex min-h-[44px] items-center rounded-full px-2 font-semibold text-accent hover:bg-s1"),
				g.Text("Open"),
			),
			Button(
				Type("button"),
				g.Attr("data-plugin-show", ""),
				g.Attr("aria-label", "Show "+p.Label+" here"),
				Class("press inline-flex min-h-[44px] items-center rounded-full px-2 font-semibold text-accent hover:bg-s1"),
				g.Text("Show here"),
			),
		))
	}
	return g.Group{
		Div(
			ID("plugin-chips"),
			g.Attr("data-mint", c.MintPath),
			g.Attr("data-host", c.Host),
			g.Attr("data-window", c.Window),
			g.Attr("aria-label", "Plugins"),
			Class("flex h-12 shrink-0 items-center gap-2 overflow-x-auto overflow-y-hidden border-b border-line px-2 text-xs"),
			g.Group(chips),
			Span(ID("plugin-chip-error"), g.Attr("role", "status"), g.Attr("hidden", ""), Class("min-w-0 truncate text-danger")),
		),
		Dialog(
			ID("plugin-frame"),
			g.Attr("aria-labelledby", "plugin-frame-title"),
			Class("mx-auto mb-0 mt-auto h-[85dvh] w-full max-w-3xl flex-col overflow-hidden rounded-t-2xl bg-s1 p-0 text-fg shadow-2xl ring-1 ring-line backdrop:bg-black/60 sm:m-auto sm:rounded-2xl [&[open]]:flex"),
			Div(Class("flex h-12 shrink-0 items-center gap-2 border-b border-line pl-4 pr-1"),
				H2(ID("plugin-frame-title"), Class("min-w-0 flex-1 truncate text-sm font-semibold")),
				Button(Type("button"), g.Attr("data-plugin-close", ""),
					Class("press inline-flex min-h-[44px] items-center rounded-lg px-3 text-sm font-semibold text-fg2 hover:bg-s2"),
					g.Text("Close")),
			),
			IFrame(
				g.Attr("sandbox", PluginChipSandbox),
				g.Attr("allow", ""),
				g.Attr("referrerpolicy", "no-referrer"),
				g.Attr("title", "Plugin"),
				Class("min-h-0 w-full flex-1 border-0 bg-bg"),
			),
		),
		staticScript(pluginChipScriptSrc),
	}
}

// pluginChipScriptSrc mints at click time and sends the browser on: Open
// follows its own link once the minted URL is its href (still inside the
// click's user activation, so it is not a blocked popup); Show here loads it
// into the dialog's frame. A refused mint (a revoked token, the window gone,
// the rate cap) says why in the row and goes nowhere.
var pluginChipScriptSrc = staticJS("plugin-chip", `
(function () {
  if (window.__musterPluginChip) return;
  window.__musterPluginChip = true;
  function row() { return document.getElementById('plugin-chips'); }
  function say(msg) {
    var e = document.getElementById('plugin-chip-error');
    if (!e) return;
    e.textContent = msg || '';
    e.hidden = !msg;
  }
  function mint(chip) {
    var r = row();
    var body = { plugin: Number(chip.getAttribute('data-plugin-chip')),
      target: { mirror: { host: r.getAttribute('data-host'), window: r.getAttribute('data-window') } } };
    return fetch(r.getAttribute('data-mint'), {
      method: 'POST', credentials: 'same-origin', cache: 'no-store',
      headers: { 'Content-Type': 'application/json', 'Accept': 'application/json' },
      body: JSON.stringify(body)
    }).then(function (res) {
      return res.json().catch(function () { return {}; }).then(function (b) {
        if (!res.ok || !b || typeof b.url !== 'string') throw new Error((b && b.error) || ('the hand-off was refused (' + res.status + ')'));
        return b.url;
      });
    });
  }
  function dialog() { return document.getElementById('plugin-frame'); }
  function closeFrame() {
    var d = dialog();
    if (!d) return;
    if (d.open) d.close();
  }
  document.addEventListener('close', function (ev) {
    var d = dialog();
    if (ev.target !== d) return;
    // The plugin's page ends with the dialog: nothing of it runs on hidden.
    var f = d.querySelector('iframe');
    if (f) f.src = 'about:blank';
  }, true);
  document.addEventListener('click', function (ev) {
    var t = ev.target;
    if (!t || !t.closest) return;
    if (t.closest('[data-plugin-close]')) { closeFrame(); return; }
    var open = t.closest('[data-plugin-open]');
    var show = t.closest('[data-plugin-show]');
    var chip = (open || show) && (open || show).closest('[data-plugin-chip]');
    if (!chip) return;
    if (open && open.getAttribute('data-ready') === '1') {
      // The second, scripted click: the browser follows the minted href.
      open.removeAttribute('data-ready');
      setTimeout(function () { open.setAttribute('href', '#'); }, 0);
      return;
    }
    ev.preventDefault();
    say('');
    mint(chip).then(function (url) {
      if (open) {
        open.setAttribute('href', url);
        open.setAttribute('data-ready', '1');
        open.click();
        return;
      }
      var d = dialog();
      var f = d.querySelector('iframe');
      document.getElementById('plugin-frame-title').textContent = chip.getAttribute('data-plugin-label') || 'Plugin';
      f.src = url;
      if (!d.open) d.showModal();
    }).catch(function (e) { say(e && e.message ? e.message : 'the hand-off failed'); });
  });
})();
`)
