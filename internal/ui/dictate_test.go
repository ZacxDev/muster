package ui

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The message box's microphone (dictate.go) on every kind of message box.

// dictateSurfaces renders each message box with the microphone path dict:
// a mirrored session's Rendered view, its Terminal view, an agent's Terminal
// view, an agent's chat page, and the chief panel's chat.
func dictateSurfaces(t *testing.T, dict string) map[string]string {
	t.Helper()
	out := map[string]string{}
	out["mirror rendered"] = renderString(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "main", Interactive: true,
		StepUp: stepUpFixture(), Nonce: "0123456789abcdef", Dictate: dict}))
	out["mirror terminal"] = renderString(t, AgentTerminalPage(AgentTerminalView{Name: "main", StepUp: stepUpFixture(),
		Composer: &ComposerView{SendPath: MirrorTerminalSendPath, Host: "host-a", Session: "main", Dictate: dict}}))
	out["agent terminal"] = renderString(t, AgentTerminalPage(AgentTerminalView{Name: "keen-wren",
		Composer: &ComposerView{SendPath: AgentSendPath("keen-wren"), Dictate: dict}}))
	v := sampleAgentDetailView()
	v.Dictate = dict
	var b bytes.Buffer
	if err := RenderAgentDetail(&b, v, featuresAllOn); err != nil {
		t.Fatal(err)
	}
	out["agent chat"] = b.String()
	b.Reset()
	cv := sampleAgentDetailView()
	cv.Dictate, cv.Surface = dict, ChatSurfaceChiefPanel
	if err := RenderChiefPanelBody(&b, ChiefPanelView{Configured: true, Chat: cv}); err != nil {
		t.Fatal(err)
	}
	out["chief panel"] = b.String()
	return out
}

var dictateButtonTag = regexp.MustCompile(`<button id="(mv|chat)-dictate" type="button"[^>]*>`)

// TestEveryMessageBoxHasTheMicrophone: the SAME control, addressing its own
// box, on every surface — and the script that drives it beside it.
func TestEveryMessageBoxHasTheMicrophone(t *testing.T) {
	for name, page := range dictateSurfaces(t, TranscribePath) {
		tags := dictateButtonTag.FindAllString(page, -1)
		if len(tags) != 1 {
			t.Errorf("%s: %d microphone buttons; want exactly 1", name, len(tags))
			continue
		}
		tag := tags[0]
		input := "mv-input"
		if strings.Contains(tag, `id="chat-dictate"`) {
			input = "chat-input"
		}
		for _, want := range []string{`data-dictate="/transcribe"`, `data-dictate-input="` + input + `"`, `data-dictate-max="120"`} {
			if !strings.Contains(tag, want) {
				t.Errorf("%s: the mic lacks %s: %s", name, want, tag)
			}
		}
		// The box it addresses and the note it reports into are on the page.
		note := regexp.MustCompile(`data-dictate-note="([^"]+)"`).FindStringSubmatch(tag)
		if note == nil || !strings.Contains(page, `id="`+note[1]+`"`) {
			t.Errorf("%s: the mic's note element is not on the page: %s", name, tag)
		}
		if !strings.Contains(page, `<textarea id="`+input+`"`) {
			t.Errorf("%s: the mic addresses #%s, which is not on the page", name, input)
		}
		// 🔴 HIDDEN UNTIL THE SCRIPT RUNS: with no script there is no mic (the
		// `hidden` CLASS — an attribute would lose to the display utility).
		if !regexp.MustCompile(`class="group hidden `).MatchString(tag) || strings.Contains(tag, "inline-flex") {
			t.Errorf("%s: the mic is visible before any script runs: %s", name, tag)
		}
		// A 40px target.
		if !strings.Contains(tag, "min-h-[40px]") || !strings.Contains(tag, "min-w-[40px]") {
			t.Errorf("%s: the mic is not a 40px target: %s", name, tag)
		}
		if !strings.Contains(page, "window.__musterDictate") {
			t.Errorf("%s: the microphone's script is not on the page", name)
		}
	}
}

// recorderParts is what one mic's recorder draws around its box: the mic's
// own tag, its waveform (box's place) and its ↑ (Send's place).
type recorderParts struct{ mic, wave, arrow string }

// recorderOf finds mic's recorder parts on page, failing the test when one is
// missing. Every attribute is pinned at its exact value: a part that is drawn
// but addresses another box would still pass a "contains data-dictate" check.
func recorderOf(t *testing.T, name, page, mic, input string, submit, hide []string) recorderParts {
	t.Helper()
	var r recorderParts
	r.mic = regexp.MustCompile(`<button id="` + mic + `" type="button"[^>]*>`).FindString(page)
	r.wave = regexp.MustCompile(`(?s)<div id="` + mic + `-wave"[^>]*>.*?</canvas><span[^>]*>0:00</span></div>`).FindString(page)
	r.arrow = regexp.MustCompile(`(?s)<button id="` + mic + `-send"[^>]*>.*?</button>`).FindString(page)
	for _, want := range []string{
		`data-dictate-input="` + input + `"`,
		`data-dictate-wave="` + mic + `-wave"`,
		`data-dictate-arrow="` + mic + `-send"`,
	} {
		if !strings.Contains(r.mic, want) {
			t.Errorf("%s: the mic lacks %s: %s", name, want, r.mic)
		}
	}
	// The waveform: hidden until a recording (the CLASS, as the mic's), for
	// THIS mic, with the × that discards, the canvas and the clock.
	if r.wave == "" {
		t.Errorf("%s: no waveform #%s-wave on the page", name, mic)
	}
	for _, want := range []string{
		`data-dictate-wave-for="` + mic + `" class="hidden `,
		`<button id="` + mic + `-discard" type="button" data-dictate-discard="` + mic + `" aria-label="Discard recording" title="Discard recording"`,
		`<canvas data-dictate-canvas="" aria-hidden="true"`,
		`<span data-dictate-clock="" aria-hidden="true"`,
	} {
		if !strings.Contains(r.wave, want) {
			t.Errorf("%s: the waveform lacks %s: %s", name, want, r.wave)
		}
	}
	// The ↑: hidden until a recording, for THIS mic, clicking and hiding
	// exactly the box's own Send.
	if r.arrow == "" {
		t.Errorf("%s: no ↑ #%s-send on the page", name, mic)
	}
	for _, want := range []string{
		`type="button" data-dictate-send="` + mic + `"`,
		`data-dictate-submit="` + strings.Join(submit, " ") + `"`,
		`data-dictate-hide="` + strings.Join(hide, " ") + `"`,
		`aria-label="Transcribe and send" title="Transcribe and send" class="group hidden `,
		`data-dictate-up`,
	} {
		if !strings.Contains(r.arrow, want) {
			t.Errorf("%s: the ↑ lacks %s: %s", name, want, r.arrow)
		}
	}
	// Order: the box, its waveform, the mic; the ↑ right before the Send it
	// stands in for.
	box, wave, m := strings.Index(page, `<textarea id="`+input+`"`), strings.Index(page, `id="`+mic+`-wave"`), strings.Index(page, `id="`+mic+`"`)
	if !(box >= 0 && box < wave && wave < m) {
		t.Errorf("%s: order textarea(%d) < waveform(%d) < mic(%d) does not hold", name, box, wave, m)
	}
	return r
}

// TestEveryMessageBoxTurnsIntoTheRecorder: the operator's 2026-11-12 decision,
// on every message box drawn in this package — the mirrored composer (both
// views), an agent's Terminal composer, the agent chat page and the chief
// panel's chat. While recording, the box's place is a waveform with a × and a
// clock, the mic is a ✓ named "Stop and insert transcript", and Send's place
// is a ↑ named "Transcribe and send" that clicks the box's own Send. RED at
// origin/main: none of the three parts existed.
func TestEveryMessageBoxTurnsIntoTheRecorder(t *testing.T) {
	for name, page := range dictateSurfaces(t, TranscribePath) {
		mic, input := "mv-dictate", "mv-input"
		submit, hide := []string{"mv-send-chief", "mv-send"}, []string{"mv-send-group", "mv-send", "mv-send-chief"}
		if strings.Contains(page, `id="chat-dictate"`) {
			mic, input = "chat-dictate", "chat-input"
			submit, hide = []string{"chat-send"}, []string{"chat-send"}
		}
		recorderOf(t, name, page, mic, input, submit, hide)
		// The Send the ↑ clicks is on the page, as a submit, right after it.
		send := submit[len(submit)-1]
		if !regexp.MustCompile(`<button (id="` + send + `" type="submit"|type="submit" id="` + send + `")`).MatchString(page) {
			t.Errorf("%s: the ↑ clicks #%s, which is no submit button on the page", name, send)
		}
		if a, s := strings.Index(page, `id="`+mic+`-send"`), strings.Index(page, `id="`+send+`"`); !(a >= 0 && a < s) {
			t.Errorf("%s: the ↑ (%d) is not before Send (%d)", name, a, s)
		}
		// The mic's recording glyph is the ✓, not a stop square.
		micFull := regexp.MustCompile(`(?s)<button id="` + mic + `" type="button".*?</button>`).FindString(page)
		if !strings.Contains(micFull, `group-data-[state=recording]:block" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" data-dictate-check>`) {
			t.Errorf("%s: the recording mic does not show the ✓: %s", name, micFull)
		}
		if strings.Contains(micFull, `<rect x="5" y="5" width="14" height="14"`) {
			t.Errorf("%s: the recording mic still draws the stop square", name)
		}
	}
	// The script names the ✓ and the ↑ as the markup does, and announces the
	// ↑'s armed state.
	script := DictateScriptHTML(TranscribePath)
	for _, want := range []string{`"stop":"Stop and insert transcript"`, `"send":"Transcribe and send"`, `"sending":"Transcribing, then sending…"`} {
		if !strings.Contains(script, want) {
			t.Errorf("the script lacks %s", want)
		}
	}
}

// TestNoServiceNoMicrophone: with no speech-to-text service (an empty path)
// no surface draws the button, its note or its script.
func TestNoServiceNoMicrophone(t *testing.T) {
	for name, page := range dictateSurfaces(t, "") {
		for _, absent := range []string{"data-dictate", "dictate-note", "__musterDictate", TranscribePath} {
			if strings.Contains(page, absent) {
				t.Errorf("%s: %q is drawn with dictation off", name, absent)
			}
		}
	}
}

// TestADisabledComposerHasNoMicrophone: a read-only host's box cannot send,
// so nothing dictated into it could go anywhere.
func TestADisabledComposerHasNoMicrophone(t *testing.T) {
	page := renderString(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "main", Interactive: false,
		StepUp: stepUpFixture(), Dictate: TranscribePath}))
	if strings.Contains(page, "data-dictate") || strings.Contains(page, "__musterDictate") {
		t.Error("a read-only host's composer draws a microphone")
	}
	page = renderString(t, AgentTerminalPage(AgentTerminalView{Name: "x",
		Composer: &ComposerView{SendPath: AgentSendPath("x"), Disabled: "not running", Dictate: TranscribePath}}))
	if strings.Contains(page, "data-dictate") {
		t.Error("a disabled agent composer draws a microphone")
	}
}

// TestTheMicrophoneSitsBesideSend: in the input row, between the box and Send,
// so the row's layout does not change when it appears (it only grows a 44px
// square the script reveals).
func TestTheMicrophoneSitsBesideSend(t *testing.T) {
	for name, page := range dictateSurfaces(t, TranscribePath) {
		input, mic, send := "mv-input", "mv-dictate", "mv-send"
		if strings.Contains(page, `id="chat-dictate"`) {
			input, mic, send = "chat-input", "chat-dictate", "chat-send"
		}
		i, m, s := strings.Index(page, `id="`+input+`"`), strings.Index(page, `id="`+mic+`"`), strings.Index(page, `id="`+send+`"`)
		if !(i >= 0 && i < m && m < s) {
			t.Errorf("%s: order textarea(%d) < mic(%d) < send(%d) does not hold", name, i, m, s)
		}
	}
}

// TestTheDeniedNoteNamesTheRightSettings: a refused microphone says where to
// allow it. In a browser that is the site's setting in the browser; inside the
// Android app's WebView (the bridge object AppBridgeName exists) it is the
// app's Android permission, which the WebView's page cannot reach through any
// browser settings. Both sentences are pinned whole, and every surface's
// script carries both and picks between them on the bridge.
func TestTheDeniedNoteNamesTheRightSettings(t *testing.T) {
	const browser = "Microphone access was blocked. Allow it for this site in the browser's settings, then tap the mic again."
	const app = "Microphone access is off for the muster app. Allow it in Android's Settings for muster, under Permissions, then Microphone, then tap the mic again. If Microphone isn't listed, update the muster app."
	if DictateDenied != browser {
		t.Errorf("DictateDenied = %q; want %q", DictateDenied, browser)
	}
	if DictateDeniedApp != app {
		t.Errorf("DictateDeniedApp = %q; want %q", DictateDeniedApp, app)
	}
	if AppBridgeName != "musterApp" {
		t.Fatalf("AppBridgeName = %q; the app injects window.musterApp", AppBridgeName)
	}
	for name, page := range dictateSurfaces(t, TranscribePath) {
		for _, want := range []string{
			`"denied":` + strconv.Quote(browser),
			`"deniedapp":` + strconv.Quote(app),
			`n === 'SecurityError' ? (window.musterApp ? M.deniedapp : M.denied)`,
		} {
			if !strings.Contains(page, want) {
				t.Errorf("%s: the mic's script lacks %s", name, want)
			}
		}
	}
}

// TestTheStringFormsAreTheSameControl: the answer boxes' string forms
// (DictateControlHTML, DictateNoteHTML, DictateScriptHTML — internal/api's
// Asks pages are raw HTML) are the gomponents themselves rendered, never a
// second copy of the markup; nothing at all with no service.
func TestTheStringFormsAreTheSameControl(t *testing.T) {
	if got, want := DictateControlHTML(TranscribePath, "in-7", "note-7", "btn-7"), renderString(t, dictateControl(TranscribePath, "in-7", "note-7", "btn-7")); got != want || !strings.Contains(got, `id="btn-7"`) ||
		!strings.Contains(got, `data-dictate-input="in-7"`) || !strings.Contains(got, `data-dictate-note="note-7"`) {
		t.Errorf("the control's string form:\n%s\nwant\n%s", got, want)
	}
	if got, want := DictateNoteHTML(TranscribePath, "note-7", "pb-2"), renderString(t, dictateNote(TranscribePath, "note-7", "pb-2")); got != want || !strings.Contains(got, `id="note-7"`) {
		t.Errorf("the note's string form:\n%s\nwant\n%s", got, want)
	}
	if got, want := DictateScriptHTML(TranscribePath), renderString(t, dictateScript(TranscribePath)); got != want || !strings.Contains(got, "window.__musterDictate") {
		t.Error("the script's string form is not the script")
	}
	if got, want := DictateWaveHTML(TranscribePath, "btn-7"), renderString(t, dictateWave(TranscribePath, "btn-7")); got != want || !strings.Contains(got, `id="btn-7-wave"`) {
		t.Errorf("the waveform's string form:\n%s\nwant\n%s", got, want)
	}
	if got, want := DictateSendHTML(TranscribePath, "btn-7", "ok-7", DictateSendWide),
		renderString(t, dictateSend(TranscribePath, "btn-7", DictateSendWide, []string{"ok-7"}, []string{"ok-7"})); got != want ||
		!strings.Contains(got, `id="btn-7-send"`) || !strings.Contains(got, `data-dictate-submit="ok-7" data-dictate-hide="ok-7"`) {
		t.Errorf("the ↑'s string form:\n%s\nwant\n%s", got, want)
	}
	for name, s := range map[string]string{"control": DictateControlHTML("", "a", "b", "c"), "note": DictateNoteHTML("", "b", ""), "script": DictateScriptHTML(""),
		"wave": DictateWaveHTML("", "c"), "send": DictateSendHTML("", "c", "d", DictateSendPill)} {
		if s != "" {
			t.Errorf("the %s with no service: %q", name, s)
		}
	}
}

// TestALiveRefreshRevealsTheMicsItDraws: the Asks list and a mirrored view's
// asks block are refreshed by AttentionLiveScript's innerHTML — no htmx swap,
// so no htmx:load — and every mic in the new content is drawn hidden. The
// refresh dispatches DictateRevealEvent on the list and the mic's script
// reveals on it; and the refresh counts a recording or transcribing mic as the
// owner typing (askBusyFunc), so it never swaps a box out from under one.
// RED at origin/main: neither the event nor the mic state was there. The click
// path is e2e/tests/dictation.spec.ts.
func TestALiveRefreshRevealsTheMicsItDraws(t *testing.T) {
	live := AttentionLiveScript("attention-items", "/attention/items", "attention_queue.changed")
	swap := strings.Index(live, "list.innerHTML = html;")
	reveal := strings.Index(live, `list.dispatchEvent(new Event("`+DictateRevealEvent+`", { bubbles: true }))`)
	if swap < 0 || reveal < swap {
		t.Errorf("the refresh does not dispatch %s after its swap:\n%s", DictateRevealEvent, live)
	}
	script := DictateScriptHTML(TranscribePath)
	if !strings.Contains(script, `document.addEventListener("`+DictateRevealEvent+`", function (e) {
    if (active && !active.p.btn.isConnected) abandon();
    reveal(e.target);
  });`) {
		t.Errorf("the mic's script does not reveal on %s, letting go of a replaced button's recording", DictateRevealEvent)
	}
	for _, s := range []string{live, askerAsksBusy} {
		if !strings.Contains(s, `[data-dictate][data-state="opening"], [data-dictate][data-state="recording"], [data-dictate][data-state="busy"]`) {
			t.Errorf("a refresh does not wait for an opening or recording mic:\n%s", s)
		}
	}
	// The mic says when it is opening, and when it is idle again; the refresh
	// it held back listens for the second.
	if !strings.Contains(script, "state(p, 'opening');") || !strings.Contains(script, `if (s === 'idle') { try { p.btn.dispatchEvent(new Event("`+DictateIdleEvent+`", { bubbles: true })); } catch (e) {} }`) {
		t.Error("the mic's script does not mark opening, or does not announce idle")
	}
	if !strings.Contains(live, `list.addEventListener("`+DictateIdleEvent+`", function () { if (pending) setTimeout(refresh, 0); });`) {
		t.Error("a held-back refresh does not run when the mic is idle again")
	}
}
