package ui

import (
	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE COLLAPSIBLE MESSAGE BOX (operator ask: "by default, don't show the text
// input … show a text icon beside the mic … more space by default").
//
// The Rendered view's composer (ComposerView.Collapsible) is drawn with its box
// COLLAPSED on a touch screen: the input row is "Type a message" (#mv-text),
// the mic, the session's ⋯ and Send. Tapping "Type a message" reveals the box
// and focuses it. Once revealed it stays open until the page is left — a send
// does not collapse it again.
//
// 🔴 DECIDED BY THE POINTER, IN CSS (web/css/input.css): `(pointer: coarse)`.
// The server only marks the form collapsible and its script collapses it
// (composerCollapsibleAttr); a fine pointer — a
// desktop — shows the box as before and never draws the button, so there is one
// behaviour per kind of device and no script deciding it before paint.
//
// 🔴 THE BOX IS HIDDEN, NEVER REMOVED. It stays in the DOM (display: none), so
// the microphone's transcript (dictate.go checks the box isConnected), the
// draft and the send queue all still reach it.
//
// 🔴 TEXT IS NEVER WRITTEN INTO A HIDDEN BOX. Every writer of #mv-input's
// value calls window.musterComposerReveal(input) — the ONE reveal — after it
// writes (and before the draft measures the box's height):
//
//   - the draft restored on load (composer_draft.go);
//   - a held or refused message put back in the box, a hand-off's text and the
//     box after a page comes back from one (composer_pending.go);
//   - the mic's transcript (dictate.go);
//   - the `/` and `:` menu's insert (composer_menu.go);
//   - a suggested reply's insert (suggest_row.go);
//   - a queued message's edit that could not be saved (send_queue.go);
//   - a file attached by paste or drop: its chip is above the box
//     (attach.go).
//
// And this script reveals a box that has text when it runs, and again after a
// form reset or a back/forward-cache restore — the browser's own restore of a
// field's value, which no writer here makes.

// composerCollapsibleAttr is what the SERVER puts on the form: this box may be
// collapsed. composerCollapsedAttr is what the reveal's script sets from it —
// collapsed — and removes to reveal the box.
//
// 🔴 THE SCRIPT COLLAPSES THE BOX, NEVER THE MARKUP. A page whose script did
// not run (or threw) must still have a box to type in: the button that would
// reveal it is a script's. So a collapsed state the server drew would be a
// box nobody could open (measured: mirror-stepup.spec's no-script phone test
// could not type). The script runs before the pinned composer is first shown
// (composePending), so collapsing it then moves nothing on screen.
const (
	composerCollapsibleAttr = "data-mv-collapsible"
	composerCollapsedAttr   = "data-mv-collapsed"
)

// ComposerTextLabel is the "Type a message" button's accessible name.
const ComposerTextLabel = "Type a message"

// composerFieldMark is what the collapsed state hides: the box's field (the
// box, or its wrapper with the paperclip) carries data-mv-field. A composer
// that cannot collapse carries nothing.
func composerFieldMark(collapse bool) g.Node {
	if !collapse {
		return nil
	}
	return g.Attr("data-mv-field", "")
}

// composerSpacer is the collapsed row's room, so the buttons stay at its right.
func composerSpacer() g.Node {
	return Span(g.Attr("data-mv-spacer", ""), g.Attr("aria-hidden", "true"), Class("min-w-0 flex-1"))
}

// composerTextButton is "Type a message": a keyboard, a 44px target in the
// mic's clothes, expanding the box it controls.
func composerTextButton() g.Node {
	return Button(
		ID("mv-text"),
		Type("button"),
		g.Attr("aria-label", ComposerTextLabel),
		g.Attr("title", ComposerTextLabel),
		g.Attr("aria-expanded", "false"),
		g.Attr("aria-controls", "mv-input"),
		Class("press min-h-[44px] min-w-[44px] shrink-0 items-center justify-center rounded-xl bg-s2 text-fg2 ring-1 ring-inset ring-line transition hover:bg-s3"),
		g.Raw(`<svg class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="2" y="6" width="20" height="12" rx="2"/><path d="M6 10h.01M10 10h.01M14 10h.01M18 10h.01M8 14h8"/></svg>`),
	)
}

// composerRevealJS is the reveal: window.musterComposerReveal(el, focus)
// shows the box el (#mv-input; another element is ignored) and, with focus,
// focuses it.
const composerRevealJS = `
(function () {
  var form = document.getElementById('mv-form');
  var input = document.getElementById('mv-input');
  var btn = document.getElementById('mv-text');
  if (!form || !input) return;
  // Collapsed from the start unless the box already holds text (a field the
  // browser restored); the draft, restored next, reveals it itself.
  if (!input.value) {
    form.setAttribute('` + composerCollapsedAttr + `', '');
    if (btn) btn.setAttribute('aria-expanded', 'false');
  }
  function reveal(focus) {
    if (form.hasAttribute('` + composerCollapsedAttr + `')) {
      form.removeAttribute('` + composerCollapsedAttr + `');
      if (btn) btn.setAttribute('aria-expanded', 'true');
    }
    if (focus) { try { input.focus({ preventScroll: true }); } catch (e) { input.focus(); } }
  }
  window.musterComposerReveal = function (el, focus) { if (!el || el === input) reveal(!!focus); };
  if (btn) btn.addEventListener('click', function () { reveal(true); });
  function check() { if (input.value) reveal(false); }
  check();
  form.addEventListener('reset', function () { setTimeout(check, 0); });
  window.addEventListener('pageshow', check);
})();
`

// composerRevealScript is composerRevealJS in a <script>: the FIRST of the
// composer's scripts (mirrorComposer), so the draft's can call it.
func composerRevealScript() g.Node { return Script(g.Raw(composerRevealJS)) }

// composerRevealCallJS is what a writer of the box runs after writing it: the
// reveal, when the page has one (a composer that cannot collapse has none).
// input is the JS expression naming the box.
func composerRevealCallJS(input string) string {
	return `if (window.musterComposerReveal) window.musterComposerReveal(` + input + `);`
}
