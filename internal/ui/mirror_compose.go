package ui

import (
	"net/url"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE MESSAGE COMPOSER: a text box and Send, at the bottom of every view that
// can be sent a message.
//
//   - A mirrored session's Rendered view (a pane running Claude Code): posts to
//     MirrorSendPath, and the host pastes only into a pane that still runs
//     Claude Code.
//   - A mirrored session's Terminal view (any window of an -interactive host):
//     posts to MirrorTerminalSendPath, and the host pastes into the window's
//     active pane whatever runs there — the shell or TUI on screen above it.
//   - A dispatched agent's Terminal view: posts to AgentSendPath, which hands
//     the text to the agent as a chat turn (the agent's own chat page keeps its
//     streaming form, in the same clothes: composerInputClass / composerSendIconClass).
//
// 🔴 ONE FORM AND ONE SCRIPT (mirrorComposer, mirrorComposerScript). The views
// differ in a route, the hidden fields that address what is sent to, and a
// placeholder; a fix to one is a fix to all because there is no second copy.
//
// 🔴 IT IS A REAL FORM, AND THE SCRIPT IS AN ENHANCEMENT. `method=post` and
// `action` are on the element and the address is in hidden inputs, so a page
// whose script did not run — or threw — still sends: the browser submits the
// form, and the server answers that NAVIGATION with a redirect back to the same
// view carrying a notice (ComposerView.Notice), or with a plain "Confirm it's
// you" page when the sign-in is stale (StepUpFormPage). The script, when it
// runs, does the same send without leaving the page — and hands over to the
// browser's own submit whenever its way fails. Send is never a silent no-op.
//
// 🔴 IT IS NOT THE INTERACTIVE SWITCH. The switch turns the page's terminal
// into a live keyboard; the composer is one discrete, gated paste per Send and
// works with the terminal read-only. Both need the host's -interactive and a
// fresh sign-in — the server checks each send, whatever the page shows.
//
// PHONE ERGONOMICS, which the browser suite measures at 390×844: the input is
// 16px (text-base) — iOS zooms the page on focusing anything smaller and does
// not zoom back; both controls are 44px touch targets; the form carries the
// safe-area inset. On the Terminal views it is the last thing in an h-dvh
// column, so with interactive-widget=resizes-content it sits on top of the
// on-screen keyboard rather than under it. On the Rendered view it is Pinned
// (position:fixed to the visible viewport's bottom; composerPinScript).

// MirrorSendPath is where the rendered view's composer posts a message: into
// the session's Claude Code pane, and only that.
const MirrorSendPath = "/mirror/view/send"

// MirrorTerminalSendPath is where the Terminal view's composer posts a message:
// into the session's active pane, whatever runs in it.
const MirrorTerminalSendPath = "/mirror/terminal/send"

// MirrorCompletePath is the Rendered view composer's menu data: the window's
// `/` commands and skills and `:` snippets, as JSON (internal/api's
// handleMirrorComplete), fetched by composerMenuScript.
const MirrorCompletePath = "/mirror/view/complete"

// ComposerView is one composer.
type ComposerView struct {
	// SendPath is the route the form posts to.
	SendPath string
	// Host, Session and Window address a mirrored entry (hidden fields `host`,
	// `session`, `window`); Window is "" for the session's active window. All
	// empty for a composer that is not a mirrored entry's.
	Host    string
	Session string
	Window  string
	// Extra are further hidden fields, in order: what else the route needs to
	// know where the message goes and where to come back to.
	Extra []ComposerField
	// Placeholder and Aria name what the text reaches ("Message Claude…").
	Placeholder string
	Aria        string
	// Disabled, when set, draws the composer disabled and is the reason shown
	// under it — a read-only host, a muster-mirror too old to be sent to, an
	// agent that is not running. The server refuses the send regardless.
	Disabled string
	// DisabledPlaceholder is the input's placeholder while Disabled.
	DisabledPlaceholder string
	// Notice, when set, is drawn in the status line when the page loads: the
	// outcome of a send the BROWSER submitted (no script), which came back here
	// as a redirect. NoticeOK says it is good news (it is styled as such).
	Notice   string
	NoticeOK bool
	// Nonce names ONE message for the server (hidden field `nonce`): a send
	// repeated with the same nonce is delivered once. The server mints one per
	// page; the script replaces it after each message that was sent.
	//
	// 🔴 IT IS WHAT MAKES "TRY AGAIN ANOTHER WAY" SAFE. When a send gets no
	// answer the script hands the form to the browser, which posts it again —
	// and the first post may have been DELIVERED with only its answer lost. A
	// prompt typed into an agent twice is run twice. With the nonce the second
	// post is recognised and answered as sent, without being pasted. "" = no
	// such protection (the server then delivers every post).
	Nonce string
	// Dictate is where the microphone posts a recording (TranscribePath), or ""
	// for no microphone — the server has no speech-to-text service. A disabled
	// composer draws none either way: there is nothing its text could be sent
	// to. See dictate.go.
	Dictate string
	// Upload is where the paperclip posts a file (UploadPath), or "" for no
	// attachments — uploads are not configured. A disabled composer draws none
	// either way. See attach.go.
	Upload string
	// DictateOff and UploadOff say the server has no dictation / no
	// attachments (the feature is off, not merely this composer disabled): an
	// enabled composer then draws a MUTED mic / paperclip in the same place,
	// linking to that feature's row on the Features page (FeatureHref), so the
	// control does not vanish without a word. False draws nothing — the zero
	// value is the composer as it was.
	DictateOff, UploadOff bool
	// Pinned draws the composer position:fixed to the bottom of the VISIBLE
	// viewport instead of as the last in-flow child of the page's column, and
	// adds composerPinScript. The page must reserve the room it takes
	// (pinnedShellClass on its body). See composerPinScript for why.
	Pinned bool
	// Queue, when set, draws the send queue: "Queue for when idle" behind a
	// caret beside Send (composer_menu.go's sendSplit) and the window's queue
	// above the input (send_queue.go). Only a mirrored window's
	// Rendered view sets it; nil — no database, or any other composer — draws
	// neither. A disabled composer draws neither either.
	Queue *ComposerQueue
	// DraftKey is the localStorage key its text is kept under as it is typed
	// (composer_draft.go: MirrorDraftKey, AgentTerminalDraftKey); "" keeps no
	// draft. A disabled composer keeps none either way.
	DraftKey string
	// Switcher, when set, draws the session switcher below the input row
	// (switcher.go), spanning the screen as the composer's last row; nil draws
	// none. The Rendered view sets it, and a Terminal view (AgentTerminalPage)
	// on the composer it draws, where the composer is in flow, not Pinned.
	Switcher *Switcher
	// Suggest, when set, is the window's suggested-replies URL
	// (SuggestPath with its address) and draws the chip row above the
	// composer (suggest_row.go); "" draws none. Only a Claude Code window's
	// Rendered view on an interactive host sets it — never a Terminal view,
	// never Hermes. A disabled composer draws none either way.
	Suggest string
	// SuggestPaused draws one line saying the viewer's suggested replies are
	// paused (the auto-pause), with the way back on (Account). Drawn with the
	// page, so it moves nothing.
	SuggestPaused bool
}

// MirrorComposerView is ComposerView under the name it had while only mirrored
// sessions had one.
type MirrorComposerView = ComposerView

// ComposerField is one hidden field of a composer.
type ComposerField struct{ Name, Value string }

// fields is every hidden field the form carries, in order.
func (v ComposerView) fields() []ComposerField {
	var out []ComposerField
	for _, f := range []ComposerField{{"host", v.Host}, {"session", v.Session}, {"window", v.Window}} {
		if f.Value != "" {
			out = append(out, f)
		}
	}
	return append(out, v.Extra...)
}

// action is the form's action: the send route with the address in its QUERY as
// well as in the hidden fields.
//
// ⚠ THE QUERY IS FOR A REQUEST THAT COMES BACK AS A GET. Behind a forward-auth
// edge a POST with an expired edge session is answered with a redirect to the
// sign-in portal, which returns the browser to this URL with GET and no body.
// The GET route reads the address from here and sends the browser back to the
// view it came from, saying the message was not sent (ComposerNoticeAgain). The
// POST itself reads its fields from the BODY only.
func (v ComposerView) action() string {
	q := url.Values{}
	for _, f := range v.fields() {
		q.Set(f.Name, f.Value)
	}
	if len(q) == 0 {
		return v.SendPath
	}
	return v.SendPath + "?" + q.Encode()
}

// The composer's own messages. The script shows them in #mv-note.
const (
	// MirrorComposeEmpty answers a Send with nothing typed: said, not swallowed.
	MirrorComposeEmpty = "Type a message first."
	mirrorComposeDown  = "Not sent: you are offline."
)

// composerInputClass and composerSendIconClass are the composer's look: a 16px
// input (iOS zooms the page on focusing anything smaller) and a 44×44 Send.
// Send is an icon button, a paper plane (sendIcon) named "Send" by aria-label
// and title — every Send: the Rendered view's plain one, its split one beside
// the Queue caret (composerSendSplitClass) and the agent chat's. The agent
// chat's own form wears the same two, so every message box in the app is one
// design — and grows the same way (composerFitClass, composer_fit.go).
//
// composerSendClass is the WORDED accent button in the same clothes, px-2.5
// side padding: the ask card's Submit and Try again (mirror_ask.go) and the
// script's "Send again". It is not Send's class: an icon button's padless
// square would crush a word.
const (
	composerInputClass    = "composer-fit min-h-[44px] min-w-0 flex-1 resize-none rounded-xl border-0 bg-s2 px-3 py-2.5 text-base text-fg ring-1 ring-inset ring-line placeholder:text-muted focus:outline-none focus:ring-2 focus:ring-focus disabled:opacity-50"
	composerSendIconClass = "press inline-flex min-h-[44px] min-w-[44px] shrink-0 items-center justify-center rounded-xl bg-accent text-on-accent transition disabled:opacity-40"
	composerSendClass     = "press min-h-[44px] shrink-0 rounded-xl bg-accent px-2.5 text-sm font-semibold text-on-accent transition disabled:opacity-40"
)

// sendIcon is Send's face: a paper plane, inline, in currentColor (so it is
// text-on-accent on bg-accent in either theme), hidden from assistive tech —
// the button's aria-label names it.
func sendIcon() g.Node {
	return g.Raw(`<svg class="h-5 w-5" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true"><path d="M3.105 2.288a.75.75 0 0 0-.826.95l1.414 4.926A1.5 1.5 0 0 0 5.135 9.25h6.115a.75.75 0 0 1 0 1.5H5.135a1.5 1.5 0 0 0-1.442 1.086l-1.414 4.926a.75.75 0 0 0 .826.95 28.897 28.897 0 0 0 15.293-7.155.75.75 0 0 0 0-1.114A28.897 28.897 0 0 0 3.105 2.288Z"/></svg>`)
}

// sendKeepsFocusJS is a JS function expression that takes a Send button
// (#mv-send, #chat-send) and keeps a press on it from taking focus from the
// message box: the mousedown's default (moving focus) is prevented, as the
// mic's and the `/` menu's are. 🔴 THE FIRST TAP ON SEND MUST SEND WITH THE
// KEYBOARD UP. A press that blurs the box closes a phone's keyboard and, on
// the Rendered view, brings the session switcher's bar back
// (switcherKeyboardJS): the composer moves between the finger going down and
// coming up, the release lands off Send, and nothing is sent — measured in
// e2e/tests/composer-menu.spec.ts, where Send moved 44px under a held press.
// The click itself is untouched (it still submits, and a disabled Send still
// does nothing), and so is the box after a send: cleared, still focused, the
// keyboard still up for the next message. The Queue caret is NOT given this:
// its menu takes focus by design (sendMenuScript).
const sendKeepsFocusJS = `function (b) {
    if (b) b.addEventListener('mousedown', function (e) { if (e.button === 0) e.preventDefault(); });
  }`

// mirrorComposer is the composer's markup: a status line, then the input row.
func mirrorComposer(v ComposerView) g.Node {
	off := v.Disabled != ""
	dictate, upload := v.Dictate, v.Upload
	if off {
		dictate, upload = "", ""
	}
	dictateOff := !off && dictate == "" && v.DictateOff
	uploadOff := !off && upload == "" && v.UploadOff
	placeholder := v.Placeholder
	if off {
		placeholder = v.DisabledPlaceholder
	}
	noteClass := "px-3 pt-2 text-xs break-words "
	if v.NoticeOK {
		noteClass += "text-fg2"
	} else {
		noteClass += "text-st-error-fg"
	}
	hidden := make([]g.Node, 0, 4)
	for _, f := range v.fields() {
		hidden = append(hidden, Input(Type("hidden"), Name(f.Name), Value(f.Value)))
	}
	menu := composerMenuSrc(v)
	place := "flex-none"
	if menu != "" {
		place += " relative"
	}
	if v.Pinned {
		// 🔴 INVISIBLE UNTIL IT IS WHOLE (composePending): composerPinScript, its
		// last child, shows it.
		place = "fixed inset-x-0 bottom-0 z-20 [&[data-compose-pending]]:invisible"
	}
	suggest := v.Suggest
	if off {
		suggest = ""
	}
	if suggest != "" && !v.Pinned {
		// The row is placed against the composer's top (bottom-full).
		place += " relative"
	}
	formPad := "pb-[max(0.75rem,env(safe-area-inset-bottom))]"
	if v.Switcher != nil {
		formPad = "pb-2"
		// The switcher's room, reserved before the bar is parsed (switcherRoom).
		place += " " + switcherRoom(true)
		// 🔴 THE BAR IS ABSOLUTE (switcherBar), so the composer must be its
		// containing block. A Pinned one is (fixed); an in-flow one — a
		// Terminal view's — is not unless it says so, and the bar would then
		// be placed against the page instead of in the room reserved here.
		if !v.Pinned && menu == "" && suggest == "" {
			place += " relative"
		}
	}
	return Div(
		ID("mv-compose"),
		Class(place+" border-t border-line bg-bg"),
		// The bar's room, which steps aside with the bar (switcherRoomAttr).
		g.If(v.Switcher != nil, switcherRoomMark(v.Pinned)),
		// 🔴 A PINNED COMPOSER IS NOT SHOWN HALF-PARSED. It is fixed to the
		// bottom, so whatever of it a frame paints mid-parse is moved UP by
		// every row parsed after — and the parser yields anywhere under load,
		// not only at a script: measured at 390×844, throttled, the composer
		// moved 34–80px (layout shift 0.010–0.027, about 1 open in 20, on main
		// too) and once its Send button widened 20→104px mid-row. Invisible
		// (visibility, so its box is laid out) until its last child, the pin
		// script, runs; a <noscript> rule shows it without script.
		g.If(v.Pinned, g.Attr("data-compose-pending", "")),
		g.If(v.Pinned, NoScript(StyleEl(g.Raw(`#mv-compose { visibility: visible !important; }`)))),
		// The suggested replies (suggest_row.go): above the composer, out of
		// flow, drawn hidden — or, auto-paused, one line saying so.
		suggestRow(suggest),
		g.If(v.SuggestPaused && !off && suggest == "", suggestPausedNote()),
		// The send's outcome, inline: why it was not sent, or that there was
		// nothing to send. Empty and hidden otherwise — unless the page was
		// reached by the redirect that answers a send the browser submitted,
		// when the server draws the outcome here itself.
		P(ID("mv-note"), g.Attr("role", "status"), g.Attr("aria-live", "polite"),
			g.If(v.Notice == "", g.Attr("hidden", "")),
			g.If(v.Notice != "" && v.NoticeOK, g.Attr("data-ok", "")),
			Class(noteClass), g.Text(v.Notice)),
		// The microphone's own status line (dictate.go): kept apart from the
		// send's, so a recording's timer and a send's outcome never overwrite
		// each other.
		dictateNote(dictate, "mv-dictate-note", "px-3 pt-2"),
		sendQueueBox(v.Queue, off),
		// The `/` and `:` menu: over the composer, never over its input.
		composerMenuBox(menu),
		// The attachments' chips (attach.go): above the input, hidden while empty.
		attachBox(upload),
		Form(
			ID("mv-form"),
			// 🔴 method + action: without the script this is still a form the
			// browser can submit. The script reads the route from `action`.
			Method("post"),
			Action(v.action()),
			g.Attr("data-empty", MirrorComposeEmpty),
			g.Attr("data-down", mirrorComposeDown),
			g.Attr("novalidate", ""),
			// The page may be hx-boost'ed (an agent's): a boosted form would be
			// submitted by htmx as well as by the script.
			hx("hx-boost", "false"),
			// The safe-area inset is the LAST row's: the switcher's when it is
			// drawn below (switcherBar), this row's otherwise.
			Class("flex items-end gap-2 px-3 pt-2 "+formPad),
			g.Group(hidden),
			g.If(v.Nonce != "", Input(ID("mv-nonce"), Type("hidden"), Name("nonce"), Value(v.Nonce))),
			// The paperclip sits INSIDE the input, on its left (attach.go).
			attachFieldOrOff(upload, uploadOff, func(pad string) g.Node {
				return Textarea(
					ID("mv-input"),
					Name("text"),
					Rows("1"),
					g.Attr("aria-label", v.Aria),
					g.Attr("enterkeyhint", "send"),
					g.If(off, g.Attr("disabled", "")),
					g.If(off, g.Attr("title", v.Disabled)),
					g.If(!off, draftAttrs(v.DraftKey, "mv-note")),
					Placeholder(placeholder),
					Class(composerInputClass+pad),
				)
			}),
			dictateControl(dictate, "mv-input", "mv-dictate-note", "mv-dictate"),
			g.If(dictateOff, dictateOffControl("mv-dictate-off")),
			sendSplit(v.Queue, off),
		),
		g.If(off, P(ID("mv-readonly"), Class("px-3 pb-2 text-[11px] text-muted"), g.Text(v.Disabled))),
		// 🔴 EVERY SCRIPT OF THE COMPOSER IS AFTER ITS FORM. One inside it lets
		// the browser paint the composer half-parsed, and the height it then
		// grows by is a layout shift (measured: 0.006–0.014 at 390×844 with the
		// queue's script before the form). The draft's is FIRST among them: it
		// puts a restored text in the box before anything else measures it.
		composerFitScript(),
		g.If(v.DraftKey != "" && !off, composerDraftScript()),
		// The session switcher, below the input row (switcher.go): drawn with
		// the page, at its fixed height, so nothing moves when it is filled. It
		// comes AFTER the draft's script (its own script is the first after it),
		// so a restored draft has sized the box before anything below is parsed.
		switcherBar(v.Switcher, true),
		g.If(v.Queue != nil && !off, sendQueueScript()),
		g.If(v.Queue != nil && !off, sendMenuScript()),
		dictateScript(dictate),
		attachScript(upload),
		composerMenuScript(menu),
		g.If(suggest != "", suggestRowScript()),
		g.If(v.Pinned, composerPinScript()),
	)
}

// pinnedShellClass is the body of a page whose composer is Pinned: the same
// fixed-height app shell (a flex column, only the transcript scrolls), but its
// height is the VISIBLE viewport's as composerPinScript measures it (100dvh
// until it runs, or without script), and its bottom padding is the pinned
// composer's height (4.5rem — one input row — until measured), so the
// column's last child ends exactly where the composer starts.
const pinnedShellClass = "flex h-[var(--mv-vh,100dvh)] flex-col overflow-hidden pb-[var(--mv-compose-h,4.5rem)]"

// pinnedShellSwitcherClass is pinnedShellClass for a pinned composer with the
// session switcher below it: the same shell, its unmeasured room one input row
// AND the switcher's 44px row (7rem), so a paint before composerPinScript has
// measured still ends the column at about the composer's top.
const pinnedShellSwitcherClass = "flex h-[var(--mv-vh,100dvh)] flex-col overflow-hidden pb-[var(--mv-compose-h,7rem)]"

// composerPinScript keeps a Pinned composer at the bottom of what is on
// screen, and the page's column sized to end above it.
//
// 🔴 WHY PINNED, NOT IN FLOW (the Rendered view, a phone): an in-flow composer
// is wherever the bottom of an h-dvh body is, and on a real phone that can be
// below what is visible — the body cannot scroll (overflow-hidden), so a
// collapsing browser toolbar that sized 100dvh against one state while the
// screen shows the other leaves the composer under the chrome with no way to
// scroll it back (reported: it reappeared only after Terminal → tap →
// Rendered). position:fixed; bottom:0 is placed against the viewport itself,
// whatever the body's height. ⚠ The mechanism is a hypothesis; emulation has
// no collapsing toolbar, so no browser test reproduces it.
//
// What this script adds on top:
//   - --mv-compose-h: the composer's measured height (a notice, the mic's
//     line, a read-only note all change it) PLUS the suggested replies' row
//     while it is shown (suggest_row.go: it is out of flow above the
//     composer, so the box's own height does not count it), which the body
//     reserves;
//   - --mv-vh: the bottom of the VISUAL viewport in layout-viewport pixels,
//     the body's height — so the transcript's end is never behind the
//     composer or off screen;
//   - the composer's `bottom`: the layout viewport below the visual one. Zero
//     where the keyboard resizes the page (interactive-widget=resizes-content,
//     Chrome); the keyboard's height where it only overlays it (iOS Safari),
//     so the composer rides on top of the keyboard.
//
// 🔴 A PINCH IS NOT A RESIZE (the Terminal view's rule, terminal.go): a zoom
// changes visualViewport.scale and shrinks its height without the page being
// any shorter, so a change at a scale other than 1 is ignored.
func composerPinScript() g.Node {
	return Script(g.Raw(`
(function () {
  var box = document.getElementById('mv-compose');
  if (!box) return;
  // The last child of the composer: it is whole now (mirrorComposer).
  box.removeAttribute('data-compose-pending');
  var root = document.documentElement;
  // 🔴 IN THE SWITCH'S FRAME (view_swap.go) THE KEYBOARD IS THE TOP WINDOW'S.
  // The frame is laid over the whole viewport, so its height is the top
  // window's — but a keyboard that only overlays the page (iOS) shrinks the TOP
  // window's visual viewport, never the frame's. Measured against the top
  // window, the composer still rides on top of it.
  var vw = window;
  try { if (window.frameElement && window.frameElement.hasAttribute('` + ViewFrameAttr + `')) vw = window.parent; } catch (e) {}
  var pending = false;
  function measure() {
    pending = false;
    var vv = vw.visualViewport;
    var bottom = vw.innerHeight;
    if (vv) {
      if (Math.abs(vv.scale - 1) > 0.001) return;
      bottom = Math.min(vw.innerHeight, vv.height + vv.offsetTop);
    }
    var kb = Math.max(0, Math.round(vw.innerHeight - bottom));
    box.style.bottom = kb ? kb + 'px' : '';
    root.style.setProperty('--mv-vh', Math.round(bottom) + 'px');
    var sg = document.getElementById('mv-suggest');
    var extra = sg && !sg.hidden ? sg.getBoundingClientRect().height : 0;
    root.style.setProperty('--mv-compose-h', Math.ceil(box.getBoundingClientRect().height + extra) + 'px');
  }
  function schedule() {
    if (pending) return;
    pending = true;
    requestAnimationFrame(measure);
  }
  measure();
  // The suggested replies' row says when it took or gave back its room.
  window.musterPinMeasure = measure;
  if (typeof ResizeObserver !== 'undefined') { new ResizeObserver(schedule).observe(box); }
  window.addEventListener('resize', schedule);
  window.addEventListener('pageshow', schedule);
  if (vw.visualViewport) {
    vw.visualViewport.addEventListener('resize', schedule);
    vw.visualViewport.addEventListener('scroll', schedule);
    // A frame that goes away leaves nothing listening on the top window.
    if (vw !== window) window.addEventListener('pagehide', function () {
      vw.visualViewport.removeEventListener('resize', schedule);
      vw.visualViewport.removeEventListener('scroll', schedule);
    });
  }
})();
`))
}

// mirrorComposerScript sends the composer's text without leaving the page.
//
// 🔴 IT DOES NOT WAIT FOR THE ANSWER (composer_pending.go, which holds the
// script: mirrorComposerJS). The box empties and the message is drawn as
// pending at the press; the answer then settles it — sent (replaced by its
// transcript row), refused (failed, with the reason, Retry, and the text back
// in the box), or a step-up.
//
// A 403 carrying X-Muster-Step-Up means the sign-in is not fresh: "Confirm it's
// you" is asked (window.musterStepUp, stepup_confirm.go) and on success the
// SAME request — the pending message's own text and nonce — is sent again.
// The page never decides freshness; the server does. Nothing typed →
// MirrorComposeEmpty rather than a Send that silently does nothing.
//
// 🔴 WHEN THIS SCRIPT'S WAY FAILS, THE BROWSER'S TAKES OVER (native()). Three
// cases, each of which used to end in nothing visible or in a guess:
//
//   - the request got NO ANSWER while the browser believes it is online — a
//     sign-in redirect at an edge that a fetch cannot follow, a proxy's own
//     error. The browser's own form submission CAN follow a redirect, and shows
//     whatever the edge has to say;
//   - a stale sign-in on a page with no confirm dialog to ask through;
//   - anything this script throws.
//
// Offline is the one failure it keeps for itself: it says so and keeps the
// text, because the browser's answer to a submit with no network is an error
// page in place of the conversation.
// SendAgainHeader marks a send's answer that MAY have been delivered (the
// host never confirmed it): the composer offers "Send again", which mints a
// new nonce; the same nonce is never delivered again (internal/api).
const SendAgainHeader = "X-Muster-Send-Again"

func mirrorComposerScript() g.Node {
	return Script(g.Raw(mirrorComposerJS))
}
