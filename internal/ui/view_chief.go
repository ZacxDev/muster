package ui

import (
	"net/url"
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// ---------------------------------------------------------------------------
// THE VIEW'S CHIEF MODE (docs/design/chief-in-agent-view.md, phase 1): a
// switch on a mirrored session's Rendered view composer — the input row's
// FIRST control, left of the paperclip — that sends the box's text to the
// owner's CHIEF, about this session, instead of to the pane; and the chief
// strip above the input row, where the chief's replies about this session
// are shown.
//
// THE SWITCH (viewChiefToggle) is ONE control, always present: a checkbox
// (#mv-to-chief, role="switch"; checked = the chief) inside a label (#mv-to)
// drawn as a 44px icon button. It shows the CURRENT target's glyph
// (viewIconAgent / viewIconChief). Its accessible name is FIXED
// (ViewChiefSwitchName, also its title) and its checked state is the mode;
// its description (#mv-to-desc) is the current target and the action, the
// visually hidden text for the current mode picked by the stylesheet, so it
// is right with no script at all. In
// chief mode the composer wears the chief's colour — the review status pair
// (st-review-fg / st-review-bg), distinct from the amber accent the agent's
// Send wears: the composer's top edge, the box's ring and fill, the switch,
// and the chief's Send. Agent mode is drawn as the composer always was.
//
// 🔴 A REAL FORM FIRST, THE SCRIPT SECOND (the composer's rule, mirror_compose.go).
// EVERYTHING THAT DECIDES WHERE A LINE GOES FOLLOWS THE CHECKBOX BY THE
// STYLESHEET (viewChiefOn: #mv-compose:has(#mv-to-chief:checked)), so the
// mode works with script, without script, AND when this file's script threw:
// the chief's Send (#mv-send-chief, a submit whose formaction is the chief's
// route) is the Send shown in chief mode, and the composer's Enter submits
// through the Send shown (composer_pending.go).
//
// 🔴 ENTER ON THE FOCUSED SWITCH NEVER SENDS. Enter on a checkbox submits
// the checkbox's FORM OWNER implicitly: Chromium clicks that form's first
// ENABLED submit button, shown or not (measured: the agent's #mv-send on an
// enabled composer, so Enter on the switch posted the box to the pane; the
// chief's hidden #mv-send-chief on a disabled one; a disabled "blocker"
// placed first is skipped). So the checkbox is not owned by #mv-form at all:
// its form attribute names #mv-to-form (viewChiefForm), an empty form with no
// submit button beside it, and Enter on the switch submits nothing — with or
// without script. The composer's script (which runs when this file's throws)
// makes Enter on the switch toggle it, as Space does; without script Enter on
// the switch does nothing (Space toggles it). The script (viewChiefScript)
// only adds to that — the mode remembered per window, the form's own action,
// the placeholder and the box's name, the box disabled again in agent mode
// on a view whose agent composer is disabled, a press that keeps the box's
// focus, and the strip's live refresh.
//
// 🔴 THE STRIP RE-MOUNTS THE CHIEF'S CHAT, NEVER A SECOND ONE: its content is
// ui.ChatLogBody, the renderer of the chief's own #chat-log, served narrowed to
// this session's exchange (internal/api handleViewChiefLog), refreshed on the
// same chat.reply event.
//
// 🔴 A DISABLED AGENT COMPOSER IS DRAWN WITH ITS BOX ENABLED when the chief
// mode is offered (mirror_compose.go, data-agent-off): a disabled box submits
// nothing, so the chief mode could not send without the script. The agent's
// own Send stays disabled, and the box is dimmed in agent mode; the script
// disables it again in agent mode.
//
// ⚠ WHAT PHASE 1 LEAVES OUT, STATED: one draft for the box in both modes (the
// design keeps one per mode); with the agent composer DISABLED there is no mic
// or paperclip in chief mode either (a disabled composer draws neither).
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

// ViewChiefSwitchName is the switch's fixed accessible name (and title); its
// checked state says whether it is on.
const ViewChiefSwitchName = "Send to the chief"

// The mode's two values (the remembered mode).
const (
	ViewToAgent = "agent"
	ViewToChief = "chief"
)

// ViewToAgentLabel and ViewToChiefLabel are the switch's accessible
// description in each mode: the current target, then what a press does.
func ViewToAgentLabel(agent string) string { return "Sending to " + agent + " — switch to the chief" }

// ViewToChiefLabel is the switch's description in chief mode (see
// ViewToAgentLabel).
func ViewToChiefLabel(agent string) string { return "Sending to the chief — switch to " + agent }

// The switch's two glyphs, in the composer's icon style (24-unit stroked
// paths in currentColor, as host_icons.go's): the agent a bot's head, the
// chief a crown. Each is shown in the mode it names (viewChiefStyle).
const (
	viewIconAgent = `<svg data-to-mode="agent" class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><rect x="4" y="8" width="16" height="12" rx="3"/><path d="M12 8V4"/><path d="M9 13h.01M15 13h.01"/><path d="M9.5 16.5h5"/></svg>`
	viewIconChief = `<svg data-to-mode="chief" class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><path d="M3 8l4.5 4L12 5l4.5 7L21 8l-2 11H5z"/><path d="M5 19h14"/></svg>`
)

// viewChiefToggleClass is the switch's look in agent mode: the mic's clothes
// (dictateButtonClass), a 44px square — and, like the mic, no hover fill: a
// tap leaves a touch screen "hovering", and the switch would stay lit in agent
// mode (measured in e2e/tests/chief-in-view.spec.ts). Chief mode is
// viewChiefStyle's.
const viewChiefToggleClass = "press relative inline-flex min-h-[44px] min-w-[44px] shrink-0 cursor-pointer items-center justify-center rounded-xl bg-s2 text-fg2 ring-1 ring-inset ring-line transition"

// viewChiefOn is the ONE selector for "the composer is in chief mode" — the
// switch's checkbox checked — followed by suffix.
func viewChiefOn(suffix string) string {
	return `#mv-compose:has(#mv-to-chief:checked)` + suffix
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
#mv-send-chief, #mv-chief, #mv-to [data-to-mode="chief"] { display: none; }
` + viewChiefOn(` #mv-send-chief`) + ` { display: inline-flex; }
` + viewChiefOn(` #mv-chief`) + ` { display: block; }
` + viewChiefOn(` :is(#mv-send, #mv-send-group, #mv-queue, #mv-queue-off, #mv-suggest, #mv-suggest-paused, #mv-complete-box, #mv-readonly)`) + ` { display: none !important; }
` + viewChiefOn(` #mv-to [data-to-mode="agent"]`) + ` { display: none; }
` + viewChiefOn(` #mv-to svg[data-to-mode="chief"]`) + ` { display: block; }
` + viewChiefOn(` #mv-to span[data-to-mode="chief"]`) + ` { display: inline; }
` + viewChiefOn(``) + ` { border-top-color: rgb(var(--mu-st-review-fg)); box-shadow: inset 0 2px 0 rgb(var(--mu-st-review-fg)); }
` + viewChiefOn(` #mv-input`) + ` { background-color: rgb(var(--mu-st-review-bg)); box-shadow: inset 0 0 0 2px rgb(var(--mu-st-review-fg)); opacity: 1; }
` + viewChiefOn(` #mv-input:focus`) + ` { box-shadow: inset 0 0 0 2px rgb(var(--mu-st-review-fg)), 0 0 0 3px rgb(var(--mu-st-review-fg) / 0.35); }
` + viewChiefOn(` #mv-to`) + ` { background-color: rgb(var(--mu-st-review-bg)); color: rgb(var(--mu-st-review-fg)); box-shadow: inset 0 0 0 2px rgb(var(--mu-st-review-fg)); }
` + viewChiefOn(` #mv-to svg path`) + ` { fill: currentColor; fill-opacity: 0.3; }
#mv-input[data-agent-off] { opacity: 0.5; }
#mv-send-chief { background-color: rgb(var(--mu-st-review-fg)); color: rgb(var(--mu-bg)); }
#mv-to-chief { position: absolute; inset: 0; width: 100%; height: 100%; margin: 0; opacity: 0; cursor: pointer; }
#mv-to:has(#mv-to-chief:focus-visible) { outline: 2px solid rgb(var(--mu-focus)); outline-offset: 2px; }
#mv-chief-log { max-height: 45dvh; }
`

// viewChiefToggle is the mode's switch, the composer row's first control: a
// checkbox (role="switch") in a label drawn as an icon button, unchecked (the
// agent; the script restores the window's last mode), named
// ViewChiefSwitchName and described by the label's hidden text for the
// current mode. The checkbox belongs to viewChiefForm, not to the
// composer's form, so Enter on it submits nothing; and it has no name
// attribute: the mode is never a form field. See the file's header.
func viewChiefToggle(c *ComposerChief) g.Node {
	if c == nil {
		return nil
	}
	return Label(
			ID("mv-to"),
			g.Attr("title", ViewChiefSwitchName),
			Class(viewChiefToggleClass),
		ID("mv-to"),
		g.Attr("title", ViewChiefSwitchName),
		Class(viewChiefToggleClass),
		Input(ID("mv-to-chief"), Type("checkbox"), g.Attr("form", viewChiefFormID), g.Attr("role", "switch"), g.Attr("autocomplete", "off"),
			g.Attr("aria-label", ViewChiefSwitchName), g.Attr("aria-describedby", "mv-to-desc"),
			g.Attr("data-mode-key", c.ModeKey)),
		Span(ID("mv-to-desc"), Class("sr-only"),
			Span(g.Attr("data-to-mode", ViewToAgent), g.Text(ViewToAgentLabel(c.Agent))),
			Span(g.Attr("data-to-mode", ViewToChief), g.Text(ViewToChiefLabel(c.Agent))),
		),
		g.Raw(viewIconAgent),
		g.Raw(viewIconChief),
	)
}

// viewChiefFormID is the switch's own form (viewChiefForm).
const viewChiefFormID = "mv-to-form"

// viewChiefForm is the switch's form owner: empty, hidden, never submitted —
// it has no submit button, and a checkbox does not submit a form without one
// (see the file's header). Drawn outside the composer's form (forms do not
// nest).
func viewChiefForm(c *ComposerChief) g.Node {
	if c == nil {
		return nil
	}
	return Form(ID(viewChiefFormID), g.Attr("hidden", ""), g.Attr("aria-hidden", "true"))
}

// viewChiefStrip is the chief strip: the chief's chat about this session, a
// scrolling box above the composer's input row, collapsible (a native <details>), filled
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

// viewChiefScript is the mode's behaviour on top of the form's own and the
// stylesheet's (see the file's header): nothing it does decides where a line
// goes. The strip's stream is opened only while the mode is chief.
func viewChiefScript() g.Node {
	return Script(g.Raw(`
(function () {
  var compose = document.getElementById('mv-compose');
  var form = document.getElementById('mv-form');
  var input = document.getElementById('mv-input');
  var sw = document.getElementById('mv-to');
  var cb = document.getElementById('mv-to-chief');
  var chiefSend = document.getElementById('mv-send-chief');
  var log = document.getElementById('mv-chief-log');
  if (!compose || !form || !input || !sw || !cb || !chiefSend || !log) return;
  // A press on the switch, like one on Send, keeps the box's focus (and a
  // phone's keyboard up).
  (` + sendKeepsFocusJS + `)(chiefSend);
  (` + sendKeepsFocusJS + `)(cb);
  var agentAction = form.getAttribute('action');
  var chiefAction = chiefSend.getAttribute('formaction');
  var agentSend = document.getElementById('mv-send');
  var off = input.hasAttribute('data-agent-off');
  var agentPlaceholder = input.getAttribute('placeholder') || '';
  var agentAria = input.getAttribute('aria-label') || '';
  var key = cb.getAttribute('data-mode-key');
  function load() {
    try { return key && localStorage.getItem(key) === ` + strconv.Quote(ViewToChief) + `; } catch (e) { return false; }
  }
  function save(chief) {
    try { if (key) { if (chief) localStorage.setItem(key, ` + strconv.Quote(ViewToChief) + `); else localStorage.removeItem(key); } } catch (e) {}
  }
  var version = '', es = null, timer = null;
  function refresh() {
    if (!cb.checked || typeof fetch !== 'function') return;
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
    var chief = cb.checked;
    form.setAttribute('action', chief ? chiefAction : agentAction);
    input.setAttribute('placeholder', chief ? ` + strconv.Quote(ViewChiefPlaceholder) + ` : agentPlaceholder);
    input.setAttribute('aria-label', chief ? ` + strconv.Quote(ViewChiefAria) + ` : agentAria);
    // 🔴 A DISABLED AGENT COMPOSER STAYS DISABLED FOR THE AGENT: its box is
    // drawn enabled for the chief mode's sake (see the file's header), and
    // only the chief's mode leaves it enabled (the server refuses a pane send
    // anyway).
    if (off) {
      input.disabled = !chief;
      if (agentSend) agentSend.disabled = true;
    }
    listen(chief);
    if (chief) refresh();
    if (window.musterPinMeasure) window.musterPinMeasure();
  }
  cb.addEventListener('change', function () { save(cb.checked); apply(); });
  // The composer says a line went to the chief (composer_pending.go): its
  // "working…" is drawn by the server as soon as the turn is counted.
  document.addEventListener('mv-chief-sent', refresh);
  document.addEventListener('visibilitychange', function () { if (!document.hidden) refresh(); });
  cb.checked = !!load();
  apply();
})();
`))
}
