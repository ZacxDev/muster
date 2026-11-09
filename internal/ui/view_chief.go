package ui

import (
	"net/url"
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// ---------------------------------------------------------------------------
// THE VIEW'S CHIEF MODE (docs/design/chief-in-agent-view.md, phase 1): a
// two-segment switch on a mirrored session's Rendered view composer —
// "<runtime> | Chief" — that sends the box's text to the owner's CHIEF, about
// this session, instead of to the pane; and the chief strip above the input,
// where the chief's replies about this session are shown.
//
// 🔴 A REAL FORM FIRST, THE SCRIPT SECOND (the composer's rule, mirror_compose.go).
// The switch is two radio inputs; which Send is shown, and which route a
// submit reaches, follows the checked one by a stylesheet rule
// (#mv-compose:has(#mv-to-chief:checked)), so a browser submit with no script
// still reaches the chief's door: its Send (#mv-send-chief) is a submit button
// whose formaction is the chief's route. The script (viewChiefScript) adds the
// rest — the form's own action for Enter, the placeholder, the box enabled on a
// view whose agent composer is disabled, the strip's live refresh, and the
// mode remembered per window.
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

// The switch's two values (the radios' `to`).
const (
	ViewToAgent = "agent"
	ViewToChief = "chief"
)

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

// viewChiefStyle is the mode's stylesheet: what the checked radio shows and
// hides, with or without script. In chief mode the agent's own controls — its
// Send (and the Queue caret), the send queue, the suggested replies, the `/`
// menu and the disabled composer's note — are not drawn; the chief's Send and
// the strip are.
const viewChiefStyle = `
#mv-send-chief, #mv-chief { display: none; }
#mv-compose:has(#mv-to-chief:checked) #mv-send-chief { display: inline-flex; }
#mv-compose:has(#mv-to-chief:checked) #mv-chief { display: block; }
#mv-compose:has(#mv-to-chief:checked) :is(#mv-send, #mv-send-group, #mv-queue, #mv-queue-off, #mv-suggest, #mv-suggest-paused, #mv-complete-box, #mv-readonly) { display: none !important; }
#mv-to input { position: absolute; opacity: 0; width: 1px; height: 1px; }
#mv-to label:has(input:checked) { background: rgb(var(--mu-accent)); color: rgb(var(--mu-on-accent)); }
#mv-to label:has(input:focus-visible) { outline: 2px solid currentColor; outline-offset: -2px; }
#mv-chief-log { max-height: 45dvh; }
`

// viewChiefSwitch is the "<agent> | Chief" switch: two radios, the agent
// checked (the default; the script restores the window's last mode).
func viewChiefSwitch(c *ComposerChief) g.Node {
	if c == nil {
		return nil
	}
	seg := func(id, value, label string, checked bool) g.Node {
		return Label(
			Class(viewToggleSegClass+"cursor-pointer px-3 text-fg2"),
			Input(ID(id), Type("radio"), Name("to"), Value(value), g.If(checked, Checked())),
			g.Text(label),
		)
	}
	return Div(
		ID("mv-to-row"),
		Class("flex items-center gap-2 px-3 pt-2"),
		Div(
			ID("mv-to"),
			g.Attr("role", "radiogroup"),
			g.Attr("aria-label", "Send to"),
			g.Attr("data-mode-key", c.ModeKey),
			Class("relative inline-flex shrink-0 overflow-hidden rounded-lg bg-s2 ring-1 ring-inset ring-line"),
			seg("mv-to-agent", ViewToAgent, c.Agent, true),
			seg("mv-to-chief", ViewToChief, "Chief", false),
		),
	)
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
  var toChief = document.getElementById('mv-to-chief');
  var toAgent = document.getElementById('mv-to-agent');
  var chiefSend = document.getElementById('mv-send-chief');
  var log = document.getElementById('mv-chief-log');
  if (!compose || !form || !input || !sw || !toChief || !toAgent || !chiefSend || !log) return;
  (` + sendKeepsFocusJS + `)(chiefSend);
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
    if (!toChief.checked || typeof fetch !== 'function') return;
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
    var chief = toChief.checked;
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
  toChief.addEventListener('change', function () { save(true); apply(); });
  toAgent.addEventListener('change', function () { save(false); apply(); });
  // The composer says a line went to the chief (composer_pending.go): its
  // "working…" is drawn by the server as soon as the turn is counted.
  document.addEventListener('mv-chief-sent', refresh);
  document.addEventListener('visibilitychange', function () { if (!document.hidden) refresh(); });
  if (load()) toChief.checked = true;
  apply();
})();
`))
}
