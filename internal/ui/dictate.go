package ui

import (
	"strconv"
	"strings"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/internal/features"
)

// DICTATION: a microphone on every message box.
//
// Tap the mic and speak. While it records, the box's place shows a LIVE
// WAVEFORM of the microphone (dictateWave: a canvas fed by a Web Audio
// AnalyserNode on the recorder's own MediaStream), the elapsed time, and a ×
// that DISCARDS the recording; the mic itself becomes a ✓ and the box's Send
// gives its place to a ↑ (dictateSend). The recording is posted to
// TranscribePath (internal/api/transcribe.go), which hands it to the
// speech-to-text service.
//
// 🔴 WHAT HAPPENS TO THE TEXT (operator decision, 2026-11-12; before it a
// transcript was never sent):
//
//   - ✓ — the mic, and likewise the DictateMaxSeconds cap and the view switch
//     hiding the view: the text is INSERTED INTO THE BOX AT THE CARET and NOT
//     sent. The operator reads it and presses Send.
//   - ↑ — Send's place while recording: the text is put AFTER the box's
//     draft (draft + transcript, operator decision; the draft is previewed
//     beside the waveform while recording) and then sent by CLICKING THE
//     BOX'S OWN SEND — the one on screen when the transcript comes back,
//     never one remembered from the start (a chief-mode switch can flip
//     between). Never a request of this script's own: the send takes exactly
//     the path a tap on Send takes, with its confirm-it's-you step-up
//     (docs/sending-and-step-up.md), its pending bubble, its nonce and its
//     refusals. It is sent ONLY when a non-empty transcript was inserted: a
//     refused, failed or silent recording sends nothing and leaves the box as
//     it was; a transcript whose Send is disabled or gone stays in the box
//     and says so (DictateNotSent). The ↑ is disabled while its Send is,
//     read live. It is sent AT MOST ONCE per tap: for DictateSendHoldMs after
//     the send a TAP on the box's Send is swallowed — Send is back where the
//     ↑ was by then, so the second tap of an impatient double tap would land
//     on it — while this script's own click for a NEXT voice send goes
//     through.
//   - × — the recording is thrown away: nothing is posted, the box is as it
//     was.
//
// Every way a recording ends — ✓, ↑, ×, a recorder error, the cap, the page
// or view going — stops the waveform's animation frame, disconnects and
// closes its AudioContext and stops the microphone's tracks (release). Under
// prefers-reduced-motion the waveform is a calm level meter instead.
//
// 🔴 ONE CONTROL AND ONE SCRIPT, on every message box: the shared composer
// (mirrorComposer — a mirrored session's Rendered and Terminal views and an
// agent's Terminal view), the agent chat form (agentChatPane), and the Asks
// queue's answer boxes (through DictateControlHTML, below). Each draws
// dictateControl beside its own textarea; dictateScript is document-wide and
// finds the box through the button's data-dictate-input, so there is no second
// copy to drift.
//
// 🔴 PROGRESSIVE ENHANCEMENT. The button is drawn with the `hidden` CLASS and
// only the script shows it, so a page whose script did not run has no mic at
// all (and its form still sends). A server with no speech-to-text service
// draws neither the button nor the script (an empty path).
//
// 🔴 getUserMedia NEEDS A SECURE CONTEXT. Over https (the public host) or
// localhost it works; over a plain-http LAN address the browser does not offer
// the microphone at all, and the mic says so when tapped instead of failing
// silently.

// TranscribePath is where a recording is posted.
const TranscribePath = "/transcribe"

// DictateSendHoldMs is how long, after the ↑ clicked the box's Send, a further
// click on that Send is swallowed (see the file's header: no double send).
const DictateSendHoldMs = 1500

// DictateMaxSeconds is how long one recording may run; the page stops it there
// and transcribes what it has.
const DictateMaxSeconds = 120

// DictateRevealEvent is the event a page dispatches (bubbling) on content it
// put on the page by itself — innerHTML, not an htmx swap, so no htmx:load —
// for dictateScript to show the mics in it. The Asks list's and a mirrored
// view's asks block's live refresh (AttentionLiveScript) is that case: without
// it every mic a refresh drew stayed hidden.
const DictateRevealEvent = "muster:dictate-reveal"

// DictateIdleEvent is dispatched (bubbling) on a mic when it returns to idle —
// recorded and transcribed, refused, or nothing heard. A live refresh the mic
// held back (askBusyFunc) runs then (AttentionLiveScript): without it, a
// recording that inserted nothing left the refresh pending until the next
// focusout, which a mic tap never causes.
const DictateIdleEvent = "muster:dictate-idle"

// THE ANSWER BOXES' MICROPHONE (operator ask: "integrate the stt input option
// for replying to asks"). The Asks queue's answer boxes — the ask page's
// pinned composer, every ask card on /attention, and the same cards in a
// session page's asks block — are built as raw HTML strings in internal/api,
// so they cannot hold a g.Node. These three render THE SAME control, note and
// script to strings: one control, never a second copy of its markup or of the
// recorder. Each is "" when path is "" (no speech-to-text service), exactly
// as the gomponents are nothing.
//
// The ids are the caller's: every box on a page needs its own input, note and
// button ids (the Asks list draws one box per ask), and the script finds the
// box and note through the button's data-dictate-input / data-dictate-note.

// DictateControlHTML is dictateControl rendered to a string.
func DictateControlHTML(path, inputID, noteID, buttonID string) string {
	return nodeString(dictateControl(path, inputID, noteID, buttonID))
}

// DictateWaveHTML is dictateWave rendered to a string: an answer box's
// waveform, drawn right after its textarea.
func DictateWaveHTML(path, buttonID string) string {
	return nodeString(dictateWave(path, buttonID))
}

// DictateSendHTML is dictateSend rendered to a string: an answer box's ↑,
// drawn right before its Answer button submitID (the button it clicks, and
// hides while it stands in for it).
func DictateSendHTML(path, buttonID, submitID string, shape DictateSendShape) string {
	return nodeString(dictateSend(path, buttonID, shape, []string{submitID}, []string{submitID}))
}

// DictateNoteHTML is dictateNote rendered to a string.
func DictateNoteHTML(path, noteID, class string) string {
	return nodeString(dictateNote(path, noteID, class))
}

// DictateOffControlHTML is dictateOffControl rendered to a string: the muted
// mic of an answer box built as HTML in internal/api.
func DictateOffControlHTML(id string) string { return nodeString(dictateOffControl(id)) }

// DictateScriptHTML is dictateScript rendered to a string: once per page,
// AFTER every mic the page draws at load (its first run reveals the mics then
// on the page; a later one is shown by htmx:load or DictateRevealEvent).
func DictateScriptHTML(path string) string {
	return nodeString(dictateScript(path))
}

// nodeString renders n ("" for nil). A gomponents render into a strings.Builder
// cannot fail.
func nodeString(n g.Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	_ = n.Render(&b)
	return b.String()
}

// dictateIdleLabel and friends are the button's accessible names per state.
const (
	dictateIdleLabel = "Dictate: record a message"
	// DictateStopLabel is the mic's name while it records: it is the ✓.
	DictateStopLabel = "Stop and insert transcript"
	dictateBusyLabel = "Transcribing…"
	// DictateSendLabel is the ↑'s name; DictateSendingLabel its name once
	// tapped, while the recording is transcribed and then sent.
	DictateSendLabel    = "Transcribe and send"
	DictateSendingLabel = "Transcribing, then sending…"
	// DictateDiscardLabel is the ×'s name.
	DictateDiscardLabel = "Discard recording"
	// DictateDraftPrefix is said (visually hidden) before the draft preview.
	DictateDraftPrefix = "Your draft, sent before the transcript: "
)

// dictateMessages are the inline notes. Spelled once, here, and handed to the
// script, so a test can read the same sentence the page shows.
const (
	DictateInsecure    = "The microphone needs a secure (https) address. Open muster at its https address to dictate."
	DictateUnsupported = "This browser cannot record audio, so dictation is not available here."
	DictateDenied      = "Microphone access was blocked. Allow it for this site in the browser's settings, then tap the mic again."
	// DictateDeniedApp replaces DictateDenied inside the Android app's WebView
	// (window[AppBridgeName] exists). There the block is Android's RECORD_AUDIO
	// permission for the app, not a site setting, and the WebView has no
	// browser settings to point at; the app's own "Open settings" dialog
	// names the same path. App versions 0.1.0 and 0.1.1 inject the bridge
	// but declare no RECORD_AUDIO, so they have no Microphone row; the page
	// cannot see the app's version, so the sentence says to update the app.
	DictateDeniedApp   = "Microphone access is off for the muster app. Allow it in Android's Settings for muster, under Permissions, then Microphone, then tap the mic again. If Microphone isn't listed, update the muster app."
	DictateNoMic       = "No microphone was found."
	DictateMicBusy     = "The microphone is in use by another app."
	DictateNothing     = "Nothing was heard in that recording. Try again, closer to the mic."
	DictateUnavailable = "Transcription is unavailable right now. Try again in a moment."
	DictateSignedOut   = "You are signed out. Reload the page to sign in again."
	DictateEmpty       = "Nothing was recorded."
	DictateFailed      = "The recording stopped with an error, so nothing was transcribed. Try again."
	// DictateNotSent: the ↑'s transcript is in the box, but the box's Send
	// could not be pressed when it came back (disabled, or gone).
	DictateNotSent = "Inserted, not sent: Send is unavailable right now."
)

// dictateButtonClass is the mic's look: a 40px square target
// (the composer's 40px), the composer's surface at rest, a raised surface
// with a ring while recording (it is the ✓ then).
const dictateButtonClass = "group hidden press min-h-[40px] min-w-[40px] shrink-0 items-center justify-center rounded-xl bg-s2 text-fg2 ring-1 ring-inset ring-line transition disabled:opacity-60 data-[state=recording]:bg-s3 data-[state=recording]:text-fg data-[state=recording]:ring-2 data-[state=recording]:ring-accent"

// dictateSpinner is the busy glyph the mic and the ↑ share.
const dictateSpinner = `<svg class="hidden h-5 w-5 animate-spin group-data-[state=busy]:block" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><path d="M21 12a9 9 0 1 1-6.2-8.6"/></svg>`

// dictateWaveID and dictateSendID are the ids of a mic's waveform and ↑,
// derived from the mic's own, so every box on a page (the Asks list draws one
// per ask) has its own.
func dictateWaveID(buttonID string) string { return buttonID + "-wave" }
func dictateSendID(buttonID string) string { return buttonID + "-send" }

// dictateControl is the mic button for the textarea inputID, reporting into
// the note noteID. Nothing when path is "" (no speech-to-text service). The
// box's waveform (dictateWave) and ↑ (dictateSend) are found through
// data-dictate-wave and data-dictate-arrow; a box that draws neither still
// dictates, inserting only.
func dictateControl(path, inputID, noteID, buttonID string) g.Node {
	if path == "" {
		return nil
	}
	return Button(
		ID(buttonID),
		Type("button"),
		g.Attr("data-dictate", path),
		g.Attr("data-dictate-input", inputID),
		g.Attr("data-dictate-note", noteID),
		g.Attr("data-dictate-wave", dictateWaveID(buttonID)),
		g.Attr("data-dictate-arrow", dictateSendID(buttonID)),
		g.Attr("data-dictate-max", strconv.Itoa(DictateMaxSeconds)),
		g.Attr("data-state", "idle"),
		g.Attr("aria-label", dictateIdleLabel),
		g.Attr("title", "Dictate"),
		Class(dictateButtonClass),
		// Three glyphs, one shown per state (group-data-[state=…]): the mic,
		// the ✓ while recording, the spinner while transcribing.
		g.Raw(`<svg class="h-5 w-5 group-data-[state=recording]:hidden group-data-[state=busy]:hidden" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="9" y="2" width="6" height="12" rx="3"/><path d="M5 10v1a7 7 0 0 0 14 0v-1"/><path d="M12 18v4"/></svg>`),
		g.Raw(`<svg class="hidden h-5 w-5 group-data-[state=recording]:block" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" data-dictate-check><path d="M5 12.5l4.5 4.5L19 7.5"/></svg>`),
		g.Raw(dictateSpinner),
	)
}

// dictateWave is the recording's face, drawn right AFTER the box's textarea
// and hidden (the `hidden` CLASS) until a recording starts; the script then
// hides the textarea and shows this in its place: a × that discards, the live
// waveform (a canvas, drawn in its own text colour), and the elapsed time.
// The note (dictateNote) still announces the recording to a screen reader;
// the clock here is for the eye (aria-hidden). Nothing when path is "".
func dictateWave(path, buttonID string) g.Node {
	if path == "" {
		return nil
	}
	return Div(
		ID(dictateWaveID(buttonID)),
		g.Attr("data-dictate-wave-for", buttonID),
		Class("hidden min-h-[44px] min-w-0 flex-1 items-center gap-2 rounded-xl bg-s2 py-0.5 pl-0.5 pr-3 ring-1 ring-inset ring-line"),
		Button(
			ID(buttonID+"-discard"),
			Type("button"),
			g.Attr("data-dictate-discard", buttonID),
			g.Attr("aria-label", DictateDiscardLabel),
			g.Attr("title", DictateDiscardLabel),
			Class("press inline-flex min-h-[40px] min-w-[40px] shrink-0 items-center justify-center rounded-lg text-fg2 transition hover:bg-s3 hover:text-fg"),
			g.Raw(`<svg class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18"/></svg>`),
		),
		// The box's draft, while it is out of sight: one line, cut with an
		// ellipsis (the whole text in its title), so the operator knows the ↑
		// sends it ahead of the transcript. Shown only when there is one.
		Span(g.Attr("data-dictate-draft", ""), Class("hidden min-w-0 max-w-[45%] shrink truncate text-sm text-fg2"),
			Span(Class("sr-only"), g.Text(DictateDraftPrefix)),
			Span(g.Attr("data-dictate-draft-text", "")),
		),
		Canvas(g.Attr("data-dictate-canvas", ""), g.Attr("aria-hidden", "true"), Class("block h-8 min-w-0 flex-1 text-accent")),
		Span(g.Attr("data-dictate-clock", ""), g.Attr("aria-hidden", "true"), Class("shrink-0 text-sm tabular-nums text-fg2"), g.Text("0:00")),
	)
}

// DictateSendShape is the ↑'s shape: it takes the place of the box's Send, so
// it is drawn as that Send is.
type DictateSendShape int

const (
	// DictateSendIcon: a composer's or the agent chat's 40px icon Send.
	DictateSendIcon DictateSendShape = iota
	// DictateSendPill: the ask page's round Answer.
	DictateSendPill
	// DictateSendWide: an Asks card's full-width Answer.
	DictateSendWide
)

func (s DictateSendShape) class() string {
	const base = "group hidden press shrink-0 items-center justify-center bg-accent text-on-accent transition disabled:opacity-60 "
	switch s {
	case DictateSendPill:
		return base + "min-h-[44px] min-w-[44px] rounded-full px-5"
	case DictateSendWide:
		return base + "min-h-[44px] flex-1 rounded-md px-3"
	default:
		return base + "min-h-[40px] min-w-[40px] rounded-xl"
	}
}

// dictateSend is the ↑: drawn right BEFORE the box's Send and hidden until a
// recording starts, when the script hides the Send elements `hide` and shows
// this in their place. Its tap stops the recording, transcribes it and, on a
// non-empty transcript, CLICKS the first of `submit` that was on screen when
// the recording started — the box's own Send (the file's header). Nothing
// when path is "".
func dictateSend(path, buttonID string, shape DictateSendShape, submit, hide []string) g.Node {
	if path == "" {
		return nil
	}
	return Button(
		ID(dictateSendID(buttonID)),
		Type("button"),
		g.Attr("data-dictate-send", buttonID),
		g.Attr("data-dictate-submit", strings.Join(submit, " ")),
		g.Attr("data-dictate-hide", strings.Join(hide, " ")),
		g.Attr("data-state", "idle"),
		g.Attr("aria-label", DictateSendLabel),
		g.Attr("title", DictateSendLabel),
		Class(shape.class()),
		g.Raw(`<svg class="h-5 w-5 group-data-[state=busy]:hidden" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" data-dictate-up><path d="M12 19V5M5.5 11.5L12 5l6.5 6.5"/></svg>`),
		g.Raw(dictateSpinner),
	)
}

// DictateOffLabel is the muted mic's accessible name.
const DictateOffLabel = "Dictation is off on this server — how to turn it on"

// dictateOffControl is the mic's place when the server has no dictation: a
// muted mic that links to the feature's row on the Features page. Shown
// without script (it is a plain link), unlike the real mic. id is the box's
// own (one per answer box on the Asks list).
//
// 🔴 EVERY MESSAGE BOX THAT DRAWS A MIC DRAWS THIS WHEN DICTATION IS OFF: the
// mirrored composer, the agent chat, the chief panel, the ask page and every
// Asks answer box (#441 round 1, R7).
func dictateOffControl(id string) g.Node {
	return A(ID(id), Href(FeatureHref(features.Dictation)), g.Attr("data-feature-off", features.Dictation),
		g.Attr("aria-label", DictateOffLabel), g.Attr("title", DictateOffLabel),
		Class("press inline-flex min-h-[40px] min-w-[40px] shrink-0 items-center justify-center rounded-xl bg-s2 text-muted opacity-50 ring-1 ring-inset ring-line transition hover:opacity-100"),
		g.Raw(`<svg class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="9" y="2" width="6" height="12" rx="3"/><path d="M5 10v1a7 7 0 0 0 14 0v-1"/><path d="M12 18v4"/><path d="M3 3l18 18"/></svg>`))
}

// dictateNote is the mic's status line: the recording timer, "Transcribing…",
// and why a recording was not transcribed. Hidden until it has something to
// say. Nothing when path is "".
func dictateNote(path, noteID, class string) g.Node {
	if path == "" {
		return nil
	}
	return P(ID(noteID), g.Attr("role", "status"), g.Attr("aria-live", "polite"), g.Attr("hidden", ""),
		Class("text-xs break-words text-fg2 "+class))
}

// dictateScript wires every mic on the page. Nothing when path is "".
//
// It is safe to run more than once (a boosted navigation re-runs a swapped
// body's scripts, and the chief panel re-renders its chat): the
// document-lifetime listeners bind once (window.__musterDictate), and each run
// only reveals the buttons now on the page.
//
// 🔴 ONE RECORDING AT A TIME, AND IT IS RELEASED WHEN ITS PAGE GOES. The
// microphone is held from the first tap to the second; a navigation away (a
// boosted swap of the body, or the page being hidden) stops the recorder and
// the tracks and throws the audio away, so the browser's "recording"
// indicator never outlives the box it was for. The Rendered|Terminal switch
// taking a view off screen also stops the recorder and releases the tracks at
// once, but TRANSCRIBES what was recorded into that view's box, which is
// still there (only hidden). The microphone is per DOCUMENT (each view of the
// switch is its own), so it is that switch rule, not `active`, that keeps two
// views from recording at once.
func dictateScript(path string) g.Node {
	if path == "" {
		return nil
	}
	msgs := map[string]string{
		"insecure": DictateInsecure, "unsupported": DictateUnsupported, "denied": DictateDenied, "deniedapp": DictateDeniedApp,
		"nomic": DictateNoMic, "micbusy": DictateMicBusy, "nothing": DictateNothing,
		"unavailable": DictateUnavailable, "signedout": DictateSignedOut, "empty": DictateEmpty,
		"idle": dictateIdleLabel, "stop": DictateStopLabel, "busy": dictateBusyLabel,
		"failed": DictateFailed, "send": DictateSendLabel, "sending": DictateSendingLabel, "notsent": DictateNotSent,
	}
	js := "{"
	first := true
	for _, k := range []string{"insecure", "unsupported", "denied", "deniedapp", "nomic", "micbusy", "nothing", "unavailable", "signedout", "empty", "idle", "stop", "busy", "failed", "send", "sending", "notsent"} {
		if !first {
			js += ","
		}
		first = false
		js += strconv.Quote(k) + ":" + strconv.Quote(msgs[k])
	}
	js += "}"
	return Script(g.Raw(`
(function () {
  var M = ` + js + `;
  function reveal(root) {
    var bs = (root && root.querySelectorAll ? root : document).querySelectorAll('[data-dictate]');
    for (var i = 0; i < bs.length; i++) { bs[i].classList.remove('hidden'); bs[i].classList.add('inline-flex'); }
  }
  reveal(document);
  if (window.__musterDictate) return;
  window.__musterDictate = true;

  // Opus in WebM first (Chrome, Firefox, Android), then MP4 (Safari, iOS).
  var TYPES = ['audio/webm;codecs=opus', 'audio/webm', 'audio/mp4', 'audio/ogg;codecs=opus'];
  function pickType() {
    if (typeof MediaRecorder === 'undefined' || typeof MediaRecorder.isTypeSupported !== 'function') return null;
    for (var i = 0; i < TYPES.length; i++) if (MediaRecorder.isTypeSupported(TYPES[i])) return TYPES[i];
    return '';
  }
  function byId(id) { return id ? document.getElementById(id) : null; }
  function parts(btn) {
    var wave = byId(btn.getAttribute('data-dictate-wave'));
    return { btn: btn, input: byId(btn.getAttribute('data-dictate-input')),
      note: byId(btn.getAttribute('data-dictate-note')),
      wave: wave, arrow: byId(btn.getAttribute('data-dictate-arrow')),
      canvas: wave ? wave.querySelector('canvas') : null,
      clock: wave ? wave.querySelector('[data-dictate-clock]') : null,
      draft: wave ? wave.querySelector('[data-dictate-draft]') : null,
      draftText: wave ? wave.querySelector('[data-dictate-draft-text]') : null };
  }
  function say(p, msg) { if (!p.note) return; p.note.textContent = msg || ''; p.note.hidden = !msg; }
  function state(p, s) {
    p.btn.setAttribute('data-state', s);
    p.btn.setAttribute('aria-label', s === 'recording' ? M.stop : s === 'busy' ? M.busy : M.idle);
    p.btn.setAttribute('aria-pressed', s === 'recording' ? 'true' : 'false');
    p.btn.disabled = s === 'busy';
    if (s === 'idle') { try { p.btn.dispatchEvent(new Event(` + strconv.Quote(DictateIdleEvent) + `, { bubbles: true })); } catch (e) {} }
  }
  function clock(s) { s = Math.max(0, Math.floor(s)); return Math.floor(s / 60) + ':' + (s % 60 < 10 ? '0' : '') + (s % 60); }
  // shown swaps an element's 'hidden' CLASS for its display class.
  function shown(el, on, cls) { if (!el) return; el.classList.toggle('hidden', !on); el.classList.toggle(cls, on); }
  // hideEl / unhide: an inline display:none (important, so it beats a
  // stylesheet's display rule), and back to what the element had.
  function hideEl(el) {
    var was = [el, el.style.getPropertyValue('display'), el.style.getPropertyPriority('display')];
    el.style.setProperty('display', 'none', 'important');
    return was;
  }
  function unhide(was) {
    if (was[1]) was[0].style.setProperty('display', was[1], was[2]); else was[0].style.removeProperty('display');
  }

  // insert puts text into the box at its caret (replacing a selection), with a
  // space on either side where the neighbouring text needs one, and leaves the
  // caret after it, focused.
  function insert(input, text) {
    var v = input.value, a = input.selectionStart, b = input.selectionEnd;
    if (typeof a !== 'number' || typeof b !== 'number') { a = b = v.length; }
    var before = v.slice(0, a), after = v.slice(b);
    if (before && !/\s$/.test(before)) text = ' ' + text;
    if (after && !/^\s/.test(after)) text = text + ' ';
    input.value = before + text + after;
    // The Rendered view's box may be collapsed: a transcript reveals it
    // (composer_reveal.go).
    ` + composerRevealCallJS("input") + `
    var at = before.length + text.length;
    try { input.focus({ preventScroll: true }); } catch (e) { input.focus(); }
    try { input.setSelectionRange(at, at); } catch (e) {}
    input.dispatchEvent(new Event('input', { bubbles: true }));
  }

  // active: the one recording. { p, rec, stream, chunks, t0, tick, abandoned,
  // opening, ac, src, an, raf, send, target, waved, arrowed }
  var active = null;

  // THE BOX'S PLACE: the waveform instead of the textarea while recording,
  // with the box's draft (if any) as a one-line preview: the ↑ sends it,
  // followed by the transcript. The caret is kept to put back.
  function waveOn(s) {
    var p = s.p;
    if (s.waved || !p.wave || !p.input) return;
    s.waved = hideEl(p.input);
    var d = p.input.value.trim();
    if (p.draft && p.draftText) {
      p.draftText.textContent = d.replace(/\s+/g, ' ');
      if (d) p.draft.setAttribute('title', d); else p.draft.removeAttribute('title');
      shown(p.draft, !!d, 'block');
    }
    shown(p.wave, true, 'flex');
    if (p.clock) p.clock.textContent = '0:00';
    // The note is still the screen reader's announcement; the clock beside
    // the waveform is the eye's.
    if (p.note) p.note.classList.add('sr-only');
  }
  function waveOff(s) {
    var p = s.p;
    if (s.waved) {
      unhide(s.waved);
      s.waved = null;
      shown(p.wave, false, 'flex');
      if (p.draft) shown(p.draft, false, 'block');
      if (p.note) p.note.classList.remove('sr-only');
    }
    refocus(s);
  }
  // FOCUS: a recording moves it to the ✓ (the box is out of sight, and on a
  // touch screen a tap would otherwise leave it on the body); every end of
  // the recording puts it back in the box at its caret when the box had it
  // at the start — else it stays on the mic, and it goes to the mic when the
  // box is gone. (A transcript's insert focuses the box itself.)
  // 🔴 NOT INTO A BOX THE OPERATOR NEVER FOCUSED: on the Asks list, focus in
  // an answer box counts as "typing" (askBusyFunc) and holds the list's
  // live refresh back; an empty box focused by this script held a refresh
  // forever. Not when the page or the view is going (noFocus) either: a
  // hidden view must not pull focus from the one on screen.
  function refocus(s) {
    var p = s.p;
    if (s.noFocus || s.refocused || !s.rec) return;
    s.refocused = true;
    var box = s.hadBox && p.input && p.input.isConnected && p.input.getClientRects().length ? p.input : null;
    var el = box || (p.btn.isConnected ? p.btn : null);
    if (!el) return;
    try { el.focus({ preventScroll: true }); } catch (e) { el.focus(); }
    if (box && s.caret) { try { box.setSelectionRange(s.caret[0], s.caret[1]); } catch (e) {} }
  }

  // SEND'S PLACE: the ↑ instead of the box's Send while recording.
  //
  // 🔴 ITS TARGET IS RESOLVED WHEN IT IS USED, NEVER KEPT: the Send the page
  // would show NOW, of data-dictate-submit's (a composer in chief mode shows
  // the chief's Send, else the agent's, and the switch can flip while
  // recording). sendNow measures them as the page would draw them — the
  // ↑'s own inline hiding lifted for the measurement, in the same task, so
  // nothing is painted. No Send on screen at the start, no ↑: the mic still
  // records and inserts.
  function sendNow(p, hidden) {
    var a = p.arrow, t = null, i;
    if (!a) return null;
    if (hidden) for (i = 0; i < hidden.length; i++) unhide(hidden[i]);
    var ids = (a.getAttribute('data-dictate-submit') || '').split(/\s+/);
    for (i = 0; i < ids.length && !t; i++) { var el = byId(ids[i]); if (el && el.getClientRects().length) t = el; }
    if (hidden) for (i = 0; i < hidden.length; i++) hidden[i][0].style.setProperty('display', 'none', 'important');
    return t;
  }
  // syncArrow: the ↑ is disabled exactly while the Send it would press is
  // (a send of the box's in flight, an agent's turn running), read live on
  // the recording's tick — never frozen at the start.
  function syncArrow(s) {
    if (!s.arrowed || s.send) return;
    var t = sendNow(s.p, s.arrowed);
    s.p.arrow.disabled = !t || !!t.disabled;
  }
  function arrowOn(s) {
    var p = s.p, a = p.arrow;
    if (s.arrowed || !a || !sendNow(p, null)) return;
    s.arrowed = [];
    var hs = (a.getAttribute('data-dictate-hide') || '').split(/\s+/);
    for (var j = 0; j < hs.length; j++) { var h = byId(hs[j]); if (h) s.arrowed.push(hideEl(h)); }
    a.setAttribute('data-state', 'idle');
    a.setAttribute('aria-label', M.send);
    syncArrow(s);
    shown(a, true, 'inline-flex');
  }
  function arrowOff(s) {
    var a = s.p.arrow;
    if (!s.arrowed) return;
    for (var i = 0; i < s.arrowed.length; i++) unhide(s.arrowed[i]);
    s.arrowed = null;
    shown(a, false, 'inline-flex');
    a.disabled = false;
    a.setAttribute('data-state', 'idle');
    a.setAttribute('aria-label', M.send);
  }

  // THE WAVEFORM: an AnalyserNode on the recorder's own stream (never routed
  // to the speakers), read once per animation frame (at most 20 times a
  // second) into a strip of bars that scrolls left, newest at the right.
  // Under prefers-reduced-motion it is one calm level meter, read 5 times a
  // second. A browser with no Web Audio records all the same, with no wave.
  function meterOn(s) {
    var p = s.p, c = p.canvas;
    var AC = window.AudioContext || window.webkitAudioContext;
    if (!c || !AC) return;
    try {
      s.ac = new AC();
      s.src = s.ac.createMediaStreamSource(s.stream);
      s.an = s.ac.createAnalyser();
      s.an.fftSize = 1024;
      s.src.connect(s.an);
      if (s.ac.state === 'suspended' && s.ac.resume) s.ac.resume().catch(function () {});
    } catch (e) { meterOff(s); return; }
    var calm = !!(window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches);
    p.wave.setAttribute('data-motion', calm ? 'reduced' : 'full');
    var an = s.an, buf = new Uint8Array(an.fftSize), levels = [], last = 0;
    s.raf = requestAnimationFrame(function dictateFrame(now) {
      if (s.an !== an) return; // released: no next frame
      s.raf = requestAnimationFrame(dictateFrame);
      if (now - last < (calm ? 200 : 50)) return;
      last = now;
      an.getByteTimeDomainData(buf);
      var sum = 0;
      for (var i = 0; i < buf.length; i++) { var v = (buf[i] - 128) / 128; sum += v * v; }
      draw(c, Math.min(1, Math.sqrt(sum / buf.length) * 4), levels, calm);
    });
  }
  function draw(c, lv, levels, calm) {
    var dpr = window.devicePixelRatio || 1, w = Math.round(c.clientWidth * dpr), h = Math.round(c.clientHeight * dpr);
    if (!w || !h) return;
    if (c.width !== w || c.height !== h) { c.width = w; c.height = h; }
    var g = c.getContext('2d');
    if (!g) return;
    g.clearRect(0, 0, w, h);
    g.fillStyle = getComputedStyle(c).color;
    if (calm) {
      var mh = Math.max(2, Math.round(h / 4)), y = Math.round((h - mh) / 2);
      g.globalAlpha = 0.25; g.fillRect(0, y, w, mh);
      g.globalAlpha = 1; g.fillRect(0, y, Math.max(mh, Math.round(w * lv)), mh);
      return;
    }
    var bw = Math.max(2, Math.round(3 * dpr)), step = bw + Math.max(1, Math.round(2 * dpr)), n = Math.max(1, Math.floor(w / step));
    levels.push(lv);
    while (levels.length > n) levels.shift();
    for (var i = 0; i < levels.length; i++) {
      var bh = Math.max(Math.round(2 * dpr), Math.round(levels[i] * h));
      g.fillRect(w - (levels.length - i) * step, Math.round((h - bh) / 2), bw, bh);
    }
  }
  function meterOff(s) {
    if (s.raf) { try { cancelAnimationFrame(s.raf); } catch (e) {} s.raf = 0; }
    s.an = null;
    if (s.src) { try { s.src.disconnect(); } catch (e) {} s.src = null; }
    if (s.ac) { try { s.ac.close().catch(function () {}); } catch (e) {} s.ac = null; }
  }

  // release lets go of everything a recording holds: its timer, its waveform
  // (animation frame, audio nodes, AudioContext) and the microphone's tracks.
  // EVERY end of a recording comes through here.
  function release(s) {
    if (s.tick) { clearInterval(s.tick); s.tick = null; }
    meterOff(s);
    if (s.stream) { var ts = s.stream.getTracks(); for (var i = 0; i < ts.length; i++) { try { ts[i].stop(); } catch (e) {} } }
  }
  // abandon throws the recording away (× , a page or box going, a recorder
  // error): nothing is posted, the box is as it was, why is said in the note.
  function abandon(why) {
    if (!active) return;
    var s = active; active = null; s.abandoned = true;
    try { if (s.rec && s.rec.state !== 'inactive') s.rec.stop(); } catch (e) {}
    release(s);
    waveOff(s);
    arrowOff(s);
    if (s.p.btn.isConnected) { state(s.p, 'idle'); say(s.p, why || ''); }
  }

  // held: the box Send the ↑ just clicked, and until when a further TAP on
  // it is swallowed (DictateSendHoldMs). selfClick marks this script's own
  // click, which the hold lets through: a second voice send inside the hold
  // still sends.
  var held = null, selfClick = false;
  // 🔴 THE ↑'S SEND IS THE BOX'S OWN, ONCE, AND ONLY ON A TRANSCRIPT. ok is
  // true only when upload inserted a non-empty transcript into a box still on
  // the page; a refusal, a failure or silence leaves it false and nothing is
  // sent. The send is a click on the box's Send — the one on screen NOW
  // (sendNow) — the same path, step-up included, as the operator's own tap.
  // A transcript whose Send cannot be pressed stays in the box, and says so.
  function voiceSend(s, ok) {
    arrowOff(s);
    if (!ok) return;
    var t = sendNow(s.p, null);
    if (!t || t.disabled) { say(s.p, M.notsent); return; }
    selfClick = true;
    try { t.click(); } finally { selfClick = false; }
    held = { el: t, until: Date.now() + ` + strconv.Itoa(DictateSendHoldMs) + ` };
  }

  function upload(s, blob) {
    var p = s.p, ok = false;
    state(p, 'busy');
    say(p, s.send ? M.sending : M.busy);
    var ctl = typeof AbortController === 'function' ? new AbortController() : null;
    var timer = ctl ? setTimeout(function () { ctl.abort(); }, 75000) : null;
    fetch(p.btn.getAttribute('data-dictate'), {
      method: 'POST', body: blob, credentials: 'same-origin', signal: ctl ? ctl.signal : undefined,
      headers: { 'Content-Type': blob.type || 'application/octet-stream', 'X-Requested-With': 'XMLHttpRequest' }
    }).then(function (r) {
      var plain = (r.headers.get('Content-Type') || '').toLowerCase().indexOf('text/plain') === 0;
      return r.text().then(function (t) {
        if (r.ok && plain) {
          t = t.trim();
          if (!t) { say(p, M.nothing); return; }
          if (!p.input || !p.input.isConnected) { say(p, ''); return; }
          say(p, '');
          // The ↑ sends the draft FOLLOWED BY the transcript (operator
          // decision): its text goes at the end, not at the caret.
          if (s.send) { var n = p.input.value.length; try { p.input.setSelectionRange(n, n); } catch (e) {} }
          insert(p.input, t);
          ok = true;
          return;
        }
        if (r.status === 401) { say(p, M.signedout); return; }
        // Only muster's own short sentences are shown; anything else (an edge's
        // page, a proxy's error) is not words for this box.
        if (plain && t.trim() && t.length < 400 && !/<[a-z!\/]/i.test(t)) { say(p, t.trim()); return; }
        say(p, M.unavailable + ' (HTTP ' + r.status + ')');
      });
    }, function () {
      say(p, M.unavailable);
    }).then(function () {
      if (timer) clearTimeout(timer);
      state(p, 'idle');
      if (s.send) voiceSend(s, ok);
    });
  }

  function start(p) {
    if (!window.isSecureContext) { say(p, M.insecure); return; }
    var type = pickType();
    if (!navigator.mediaDevices || typeof navigator.mediaDevices.getUserMedia !== 'function' || type === null) { say(p, M.unsupported); return; }
    var s = { p: p, chunks: [], abandoned: false, opening: true, send: false };
    active = s;
    // 'opening' while the browser asks for the microphone (its permission
    // prompt can stay up for as long as the person takes): a state on the
    // button, so a live refresh holds back for it as for a recording
    // (askBusyFunc) and does not swap the box out from under the request.
    state(p, 'opening');
    say(p, '');
    navigator.mediaDevices.getUserMedia({ audio: true }).then(function (stream) {
      s.stream = stream;
      s.opening = false;
      if (s.abandoned || active !== s) { release(s); return; }
      var opts = { audioBitsPerSecond: 64000 };
      if (type) opts.mimeType = type;
      var rec;
      try { rec = new MediaRecorder(stream, opts); } catch (e) {
        try { rec = new MediaRecorder(stream); } catch (e2) { active = null; release(s); state(p, 'idle'); say(p, M.unsupported); return; }
      }
      s.rec = rec;
      rec.addEventListener('dataavailable', function (e) { if (e.data && e.data.size > 0) s.chunks.push(e.data); });
      rec.addEventListener('error', function () { if (active === s) abandon(M.failed); });
      rec.addEventListener('stop', function () {
        release(s);
        if (active === s) active = null;
        if (s.abandoned) return;
        waveOff(s);
        // The ↑ keeps Send's place, disabled, until its send is decided.
        if (!s.send) arrowOff(s);
        var blob = new Blob(s.chunks, { type: rec.mimeType || type || 'application/octet-stream' });
        if (!blob.size) { arrowOff(s); state(p, 'idle'); say(p, M.empty); return; }
        upload(s, blob);
      });
      var max = Number(p.btn.getAttribute('data-dictate-max')) || 120;
      s.t0 = Date.now();
      var show = function () {
        var el = (Date.now() - s.t0) / 1000;
        if (p.clock) p.clock.textContent = clock(el);
        syncArrow(s);
        say(p, '● Recording ' + clock(el) + ' / ' + clock(max) + (s.arrowed ? ' — ✓ inserts the text, ↑ sends it, × discards it' : ' — ✓ inserts the text, × discards it'));
        if (el >= max && rec.state === 'recording') rec.stop();
      };
      s.caret = p.input ? [p.input.selectionStart, p.input.selectionEnd] : [0, 0];
      s.hadBox = !!p.input && document.activeElement === p.input;
      rec.start(1000);
      state(p, 'recording');
      // A collapsed Rendered-view box is revealed: the waveform takes ITS
      // place (composer_reveal.go).
      if (p.input) { ` + composerRevealCallJS("p.input") + ` }
      waveOn(s);
      arrowOn(s);
      meterOn(s);
      // Focus to the ✓ (refocus, above, puts it back at the end).
      try { p.btn.focus({ preventScroll: true }); } catch (e) { p.btn.focus(); }
      show();
      s.tick = setInterval(show, 250);
    }, function (err) {
      if (active === s) active = null;
      var n = err && err.name;
      // Inside the Android app the refusal is the app's Android permission,
      // so the note points at Android's settings, not the browser's.
      say(p, n === 'NotAllowedError' || n === 'SecurityError' ? (window.` + AppBridgeName + ` ? M.deniedapp : M.denied)
        : n === 'NotFoundError' || n === 'OverconstrainedError' ? M.nomic
        : n === 'NotReadableError' || n === 'AbortError' ? M.micbusy : M.unsupported);
      state(p, 'idle');
    });
  }

  // The PRESS itself never moves focus (its mousedown default is
  // prevented), so the box's caret is still where the operator left it when
  // the recording starts; where focus goes then is the script's choice: to
  // the ✓ while recording, and back at every end (refocus: into the box if
  // it had focus, else on the mic).
  document.addEventListener('mousedown', function (e) {
    if (e.target && e.target.closest && e.target.closest('[data-dictate], [data-dictate-send], [data-dictate-discard]')) e.preventDefault();
  });
  // The repeat tap on the box's Send just after the ↑ sent (held, above).
  document.addEventListener('click', function (e) {
    if (!held || selfClick) return;
    if (Date.now() > held.until || !held.el.isConnected) { held = null; return; }
    if (e.target && e.target.closest && e.target.closest('button') === held.el) { e.preventDefault(); e.stopImmediatePropagation(); }
  }, true);
  document.addEventListener('click', function (e) {
    var t = e.target && e.target.closest ? e.target : null;
    if (!t) return;
    // × : the recording thrown away.
    var x = t.closest('[data-dictate-discard]');
    if (x) {
      e.preventDefault();
      if (active && active.p.btn.id === x.getAttribute('data-dictate-discard')) abandon();
      return;
    }
    // ↑ : stop, transcribe, and send. A second tap before the recorder's
    // stop cannot send twice: stop() is a no-op once the recorder is
    // inactive, and the upload (and so the send) runs once per 'stop' event.
    // The ↑ is disabled as the visible cue; the repeat tap AFTER the send is
    // the hold's (voiceSend).
    var up = t.closest('[data-dictate-send]');
    if (up) {
      e.preventDefault();
      var s = active;
      if (!s || s.p.arrow !== up || !s.rec) return;
      // Read the Send live once more: it may have been disabled since the
      // last tick.
      syncArrow(s);
      if (up.disabled) return;
      s.send = true;
      up.disabled = true;
      up.setAttribute('data-state', 'busy');
      up.setAttribute('aria-label', M.sending);
      try { s.rec.stop(); } catch (err) { abandon(); }
      return;
    }
    var btn = t.closest('[data-dictate]');
    if (!btn || btn.disabled) return;
    e.preventDefault();
    var p = parts(btn);
    if (active) {
      if (active.p.btn !== btn) { abandon(); start(p); return; }
      if (active.opening) return;
      // ✓ : stop and insert.
      try { active.rec.stop(); } catch (err) { abandon(); }
      return;
    }
    start(p);
  });
  // A boosted navigation swaps the body away: the recording goes with it.
  document.addEventListener('htmx:beforeSwap', function (e) {
    if (active && e.detail && e.detail.target && e.detail.target.contains(active.p.btn)) abandon();
  });
  document.addEventListener('htmx:load', function (e) { reveal(e.target); });
  // Content a page's own script put in place (DictateRevealEvent).
  // A recording or request whose button that content replaced is let go, as
  // an htmx swap's is: nothing may record for a box no longer on the page.
  document.addEventListener(` + strconv.Quote(DictateRevealEvent) + `, function (e) {
    if (active && !active.p.btn.isConnected) abandon();
    reveal(e.target);
  });
  window.addEventListener('pagehide', function () { if (active) active.noFocus = true; abandon(); });
  // The Rendered|Terminal switch (view_swap.go) does not unload the view it
  // takes off screen: it tells that view's window 'muster:view-hidden' — the
  // top page when the frame is revealed, the frame when it is hidden again.
  // Unlike pagehide, the view's box is still there (only off screen), so its
  // recording is FINISHED, not thrown away: the recorder is stopped as a ✓
  // would stop it, the tracks are released at once, and the audio goes
  // through the ordinary upload into THAT view's own box — what was said is
  // not lost, and is there when the view is shown again. A microphone request
  // still opening has no audio yet and is only released (abandon). Without
  // this the hidden view kept the microphone, and the view on screen could
  // start a second recording beside it.
  window.addEventListener('muster:view-hidden', function () {
    var s = active;
    if (!s) return;
    s.noFocus = true;
    if (s.opening || !s.rec || s.rec.state === 'inactive') { abandon(); return; }
    try { s.rec.stop(); } catch (e) { abandon(); return; }
    release(s);
  });
})();
`))
}
