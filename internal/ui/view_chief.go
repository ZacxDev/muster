package ui

import (
	"net/url"
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// ---------------------------------------------------------------------------
// THE VIEW'S CHIEF MODE (docs/design/chief-in-agent-view.md, phase 1): a
// toggle button on a mirrored session's Rendered view composer — the row's
// FIRST control, left of the paperclip — that sends the box's text to the
// owner's CHIEF, about this session, instead of to the pane; and the chief
// strip above the input, where the chief's replies about this session are
// shown.
//
// THE TOGGLE (#mv-to, viewChiefToggle) is one <button type="button"> whose
// aria-pressed is the mode (true = the chief). It shows the CURRENT target's
// glyph (viewIconAgent / viewIconChief) and its aria-label names the current
// target and the action. In chief mode the composer wears the chief's colour —
// the review status pair (st-review-fg / st-review-bg), distinct from the
// amber accent the agent's Send wears: the composer's top edge, the box's ring
// and fill, the toggle pressed, and the chief's Send. Agent mode is drawn as
// the composer always was.
//
// 🔴 A REAL FORM FIRST, THE SCRIPT SECOND (the composer's rule, mirror_compose.go).
// Which Send is shown, and which route a submit reaches, follows the mode by a
// stylesheet rule (viewChiefOn: #mv-compose[data-to=chief], which the script
// sets, OR #mv-compose:has(#mv-to-chief:checked), the no-script checkbox), so
// a browser submit with no script still reaches the chief's door: its Send
// (#mv-send-chief) is a submit button whose formaction is the chief's route.
// Without script the button cannot toggle anything, so it is hidden and a
// <noscript> checkbox (#mv-to-chief, in a label drawn like the button) takes
// its place — the radios' old behaviour, one input. The script
// (viewChiefScript) adds the rest — the toggle, the form's own action for
// Enter, the placeholder, the box enabled on a view whose agent composer is
// disabled, the strip's live refresh, and the mode remembered per window.
//
// 🔴 THE STRIP RE-MOUNTS THE CHIEF'S CHAT, NEVER A SECOND ONE: its content is
// ui.ChatLogBody, the renderer of the chief's own #chat-log, served narrowed to
// this session's exchange (internal/api handleViewChiefLog), refreshed on the
// same chat.reply event.
//
// ⚠ WHAT PHASE 1 LEAVES OUT, STATED: one draft for the box in both modes (the
// design keeps one per mode); with the agent composer DISABLED there is no mic
// or paperclip in chief mode either (a disabled composer draws neither), and
// without script the box stays disabled there, so the chief mode needs the
// script on such a view.
// ---------------------------------------------------------------------------

// MirrorChiefPath is where the view's chief mode posts the owner's line.
const MirrorChiefPath = "/mirror/view/chief"

// MirrorChiefLogPath is the view's chief strip: the chief's replies about the
// session.
const MirrorChiefLogPath = "/mirror/view/chief/log"

// ChiefLogPath is the entry's chief strip.
func (r MirrorRef) ChiefLogPath() string { return MirrorChiefLogPath + "?" + r.query() }

// The chief mode's words.
const (
	ViewChiefPlaceholder = "Ask the chief about this session…"
	ViewChiefAria        = "Ask your chief about this session"
	viewChiefSendLabel   = "Send to the chief"
	viewChiefStripLabel  = "Chief · about this session"
	// ViewChiefStripEmpty is the strip before anything was asked about the
	// session.
	ViewChiefStripEmpty = "Nothing asked about this session yet. Your chief answers here."
)

// The mode's two values (#mv-compose's data-to, and the remembered mode).
const (
	ViewToAgent = "agent"
	ViewToChief = "chief"
)

// ViewToAgentLabel and ViewToChiefLabel are the toggle's accessible name (and
// title) in each mode: the current target, then what a press does.
func ViewToAgentLabel(agent string) string { return "Sending to " + agent + " — switch to the chief" }

// ViewToChiefLabel is the toggle's name in chief mode (see ViewToAgentLabel).
func ViewToChiefLabel(agent string) string { return "Sending to the chief — switch to " + agent }

// The toggle's two glyphs, in the composer's icon style (24-unit stroked
// paths in currentColor, as host_icons.go's): the agent a bot's head, the
// chief a crown. Each is drawn in the mode it names (viewChiefStyle).
const (
	viewIconAgent = `<svg data-to-icon="agent" class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><rect x="4" y="8" width="16" height="12" rx="3"/><path d="M12 8V4"/><path d="M9 13h.01M15 13h.01"/><path d="M9.5 16.5h5"/></svg>`
	viewIconChief = `<svg data-to-icon="chief" class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><path d="M3 8l4.5 4L12 5l4.5 7L21 8l-2 11H5z"/><path d="M5 19h14"/></svg>`
)

// viewChiefToggleClass is the toggle's look in agent mode: the mic's clothes
// (dictateButtonClass), a 44px square — and, like the mic, no hover fill: a
// tap leaves a touch screen "hovering", and the toggle would stay lit in agent
// mode (measured in e2e/tests/chief-in-view.spec.ts). Chief mode is
// viewChiefStyle's.
const viewChiefToggleClass = "press inline-flex min-h-[44px] min-w-[44px] shrink-0 items-center justify-center rounded-xl bg-s2 text-fg2 ring-1 ring-inset ring-line transition"

// viewChiefOn is a selector list for "the composer is in chief mode", each
// selector followed by suffix: the script's mode (data-to) or, without
// script, the checked <noscript> checkbox.
func viewChiefOn(suffix string) string {
	return `#mv-compose[data-to="chief"]` + suffix + `, #mv-compose:has(#mv-to-chief:checked)` + suffix
}

// ComposerChief is a composer's chief mode (ComposerView.Chief).
type ComposerChief struct {
	// Agent names the agent segment ("Claude", "opencode").
	Agent string
	// LogSrc is the chief strip's URL (MirrorRef.ChiefLogPath).
	LogSrc string
	// ModeKey is the localStorage key the mode is remembered under, per
	// window (MirrorDraftKey's scheme, view "chief").
	ModeKey string
}

// chiefAction is the chief route with the composer's address in its query
// (ComposerView.action's rule: a sign-in redirect comes back as a GET).
func (v ComposerView) chiefAction() string {
	q := url.Values{}
	for _, f := range []ComposerField{{"host", v.Host}, {"session", v.Session}, {"window", v.Window}} {
		if f.Value != "" {
			q.Set(f.Name, f.Value)
		}
	}
	if len(q) == 0 {
		return MirrorChiefPath
	}
	return MirrorChiefPath + "?" + q.Encode()
}

// viewChiefStyle is the mode's stylesheet: what chief mode shows, hides and
// colours, with or without script (viewChiefOn). In chief mode the agent's own
// controls — its Send (and the Queue caret), the send queue, the suggested
// replies, the `/` menu and the disabled composer's note — are not drawn; the
// chief's Send and the strip are. The chief's colour is the review status
// pair, from the theme's tokens in either theme.
var viewChiefStyle = `
#mv-send-chief, #mv-chief, :is(#mv-to, #mv-to-ns) [data-to-icon="chief"] { display: none; }
` + viewChiefOn(` #mv-send-chief`) + ` { display: inline-flex; }
` + viewChiefOn(` #mv-chief`) + ` { display: block; }
` + viewChiefOn(` :is(#mv-send, #mv-send-group, #mv-queue, #mv-queue-off, #mv-suggest, #mv-suggest-paused, #mv-complete-box, #mv-readonly)`) + ` { display: none !important; }
` + viewChiefOn(` :is(#mv-to, #mv-to-ns) [data-to-icon="agent"]`) + ` { display: none; }
` + viewChiefOn(` :is(#mv-to, #mv-to-ns) [data-to-icon="chief"]`) + ` { display: block; }
` + viewChiefOn(``) + ` { border-top-color: rgb(var(--mu-st-review-fg)); box-shadow: inset 0 2px 0 rgb(var(--mu-st-review-fg)); }
` + viewChiefOn(` #mv-input`) + ` { background-color: rgb(var(--mu-st-review-bg)); box-shadow: inset 0 0 0 2px rgb(var(--mu-st-review-fg)); }
` + viewChiefOn(` #mv-input:focus`) + ` { box-shadow: inset 0 0 0 2px rgb(var(--mu-st-review-fg)), 0 0 0 3px rgb(var(--mu-st-review-fg) / 0.35); }
` + viewChiefOn(` :is(#mv-to, #mv-to-ns)`) + ` { background-color: rgb(var(--mu-st-review-bg)); color: rgb(var(--mu-st-review-fg)); box-shadow: inset 0 0 0 2px rgb(var(--mu-st-review-fg)); }
` + viewChiefOn(` :is(#mv-to, #mv-to-ns) svg path`) + ` { fill: currentColor; fill-opacity: 0.3; }
#mv-send-chief { background-color: rgb(var(--mu-st-review-fg)); color: rgb(var(--mu-bg)); }
#mv-to-chief { position: absolute; opacity: 0; width: 1px; height: 1px; }
#mv-to-ns:has(#mv-to-chief:focus-visible) { outline: 2px solid currentColor; outline-offset: 2px; }
#mv-chief-log { max-height: 45dvh; }
`

// viewChiefToggle is the mode's toggle, the composer row's first control: a
// button, on the agent (the default; the script restores the window's last
// mode). Without script it is hidden and a <noscript> checkbox in its place
// does its job (see the file's header).
func viewChiefToggle(c *ComposerChief) g.Node {
	if c == nil {
		return nil
	}
	return g.Group([]g.Node{
		Button(
			ID("mv-to"),
			Type("button"),
			g.Attr("aria-pressed", "false"),
			g.Attr("aria-label", ViewToAgentLabel(c.Agent)),
			g.Attr("title", ViewToAgentLabel(c.Agent)),
			g.Attr("data-label-agent", ViewToAgentLabel(c.Agent)),
			g.Attr("data-label-chief", ViewToChiefLabel(c.Agent)),
			g.Attr("data-mode-key", c.ModeKey),
			Class(viewChiefToggleClass),
			g.Raw(viewIconAgent),
			g.Raw(viewIconChief),
		),
		// The no-script equivalent: the same face, a checkbox the stylesheet
		// follows. No name: the mode is never a form field.
		NoScript(
			StyleEl(g.Raw(`#mv-to { display: none !important; }`)),
			Label(
				ID("mv-to-ns"),
				g.Attr("title", "Send to the chief"),
				Class(viewChiefToggleClass+" relative cursor-pointer"),
				Input(ID("mv-to-chief"), Type("checkbox"), g.Attr("aria-label", "Send to the chief")),
				g.Raw(viewIconAgent),
				g.Raw(viewIconChief),
			),
		),
	})
}

// viewChiefStrip is the chief strip: the chief's chat about this session, a
// scrolling box above the switch, collapsible (a native <details>), filled
// and refreshed by viewChiefScript.
func viewChiefStrip(c *ComposerChief) g.Node {
	if c == nil {
		return nil
	}
	return g.El("details",
		ID("mv-chief"),
		g.Attr("open", ""),
		Class("border-b border-line"),
		g.El("summary",
			Class("press flex min-h-[44px] cursor-pointer items-center gap-2 px-3 text-xs font-semibold text-fg2"),
			g.Text(viewChiefStripLabel),
		),
		Div(
			ID("mv-chief-log"),
			g.Attr("data-src", c.LogSrc),
			g.Attr("aria-label", "The chief's replies about this session"),
			g.Attr("aria-live", "polite"),
			Class("flex flex-col gap-3 overflow-y-auto overscroll-contain px-3 pb-2"),
		),
	)
}

// viewChiefSend is the chief's Send: a submit whose formaction is the chief's
// route, shown only in chief mode (viewChiefStyle).
func viewChiefSend(v ComposerView) g.Node {
	return Button(
		ID("mv-send-chief"),
		Type("submit"),
		g.Attr("formaction", v.chiefAction()),
		g.Attr("aria-label", viewChiefSendLabel),
		g.Attr("title", viewChiefSendLabel),
		Class(composerSendIconClass),
		sendIcon(),
	)
}

// viewChiefScript is the mode's behaviour on top of the form's own (see the
// file's header). The strip's stream is opened only while the mode is chief.
func viewChiefScript() g.Node {
	return Script(g.Raw(`
(function () {
  var compose = document.getElementById('mv-compose');
  var form = document.getElementById('mv-form');
  var input = document.getElementById('mv-input');
  var sw = document.getElementById('mv-to');
  var chiefSend = document.getElementById('mv-send-chief');
  var log = document.getElementById('mv-chief-log');
  if (!compose || !form || !input || !sw || !chiefSend || !log) return;
  // A press on the toggle, like one on Send, keeps the box's focus (and a
  // phone's keyboard up).
  (` + sendKeepsFocusJS + `)(chiefSend);
  (` + sendKeepsFocusJS + `)(sw);
  var chief = false;
  var agentAction = form.getAttribute('action');
  var chiefAction = chiefSend.getAttribute('formaction');
  var agentSend = document.getElementById('mv-send');
  var off = input.disabled;
  var agentPlaceholder = input.getAttribute('placeholder') || '';
  var agentAria = input.getAttribute('aria-label') || '';
  var key = sw.getAttribute('data-mode-key');
  form.setAttribute('data-chief-path', ` + strconv.Quote(MirrorChiefPath) + `);
  function load() {
    try { return key && localStorage.getItem(key) === ` + strconv.Quote(ViewToChief) + `; } catch (e) { return false; }
  }
  function save(chief) {
    try { if (key) { if (chief) localStorage.setItem(key, ` + strconv.Quote(ViewToChief) + `); else localStorage.removeItem(key); } } catch (e) {}
  }
  var version = '', es = null, timer = null;
  function refresh() {
    if (!chief || typeof fetch !== 'function') return;
    var h = { 'X-Requested-With': 'XMLHttpRequest' };
    if (version) h[` + strconv.Quote(ChatLogVersionHeader) + `] = version;
    fetch(log.getAttribute('data-src'), { credentials: 'same-origin', headers: h }).then(function (r) {
      if (r.status === 204 || !r.ok) return null;
      version = r.headers.get(` + strconv.Quote(ChatLogVersionHeader) + `) || '';
      return r.text();
    }).then(function (html) {
      if (html === null || html === undefined) return;
      log.innerHTML = html;
      log.scrollTop = log.scrollHeight;
      if (window.musterPinMeasure) window.musterPinMeasure();
    }).catch(function () {});
  }
  function listen(on) {
    if (on && !es && typeof EventSource === 'function') {
      try {
        es = new EventSource('/events');
        es.addEventListener('chat.reply', refresh);
        es.addEventListener('open', refresh);
      } catch (e) { es = null; }
    }
    if (!on && es) { try { es.close(); } catch (e) {} es = null; }
    if (on && !timer) timer = setInterval(refresh, 20000);
    if (!on && timer) { clearInterval(timer); timer = null; }
  }
  function apply() {
    var label = sw.getAttribute(chief ? 'data-label-chief' : 'data-label-agent') || '';
    sw.setAttribute('aria-pressed', chief ? 'true' : 'false');
    sw.setAttribute('aria-label', label);
    sw.setAttribute('title', label);
    compose.setAttribute('data-to', chief ? ` + strconv.Quote(ViewToChief) + ` : ` + strconv.Quote(ViewToAgent) + `);
    form.setAttribute('action', chief ? chiefAction : agentAction);
    input.setAttribute('placeholder', chief ? ` + strconv.Quote(ViewChiefPlaceholder) + ` : agentPlaceholder);
    input.setAttribute('aria-label', chief ? ` + strconv.Quote(ViewChiefAria) + ` : agentAria);
    // 🔴 A DISABLED AGENT COMPOSER STAYS DISABLED FOR THE AGENT: only the
    // chief's mode enables the box (the server refuses a pane send anyway).
    if (off) {
      input.disabled = !chief;
      if (agentSend) agentSend.disabled = true;
    }
    listen(chief);
    if (chief) refresh();
    if (window.musterPinMeasure) window.musterPinMeasure();
  }
  sw.addEventListener('click', function () { chief = !chief; save(chief); apply(); });
  // The composer says a line went to the chief (composer_pending.go): its
  // "working…" is drawn by the server as soon as the turn is counted.
  document.addEventListener('mv-chief-sent', refresh);
  document.addEventListener('visibilitychange', function () { if (!document.hidden) refresh(); });
  chief = !!load();
  apply();
})();
`))
}
