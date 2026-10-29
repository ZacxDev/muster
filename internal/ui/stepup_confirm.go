package ui

import (
	"strconv"
	"time"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/internal/gotrue"
	"github.com/ZacxDev/muster/web"
)

// "CONFIRM IT'S YOU" — the one dialog and the one script behind every step-up
// prompt (internal/stepup): the mirrored Terminal's Interactive switch, and the
// message composer on both the Terminal and the Rendered view.
//
// 🔴 ONE COPY. The terminal page and the rendered view each used to carry their
// own submit handler for this dialog; a second way to confirm (a passkey) would
// have had to be written twice and kept in step. Both pages now render
// confirmDialog and include stepUpScript, and reach it through
// window.musterStepUp.
//
// ⚠ THE PAGE'S VIEW OF FRESHNESS IS A CONVENIENCE, NOT THE GATE. musterStepUp
// only decides whether to ask BEFORE trying; the server refuses a stale write
// (403) whatever the page believed, and the page then asks.

// StepUpView is what a page needs to ask "Confirm it's you" before a gated
// action.
type StepUpView struct {
	// ConfirmPath is the same-origin route that takes the password again.
	ConfirmPath string
	// PasskeyPath is the same-origin route PREFIX of the passkey confirm
	// (<prefix>/options, <prefix>/verify); "" when this server has no passkeys
	// (password mode, or GoTrue's passkeys off), and the dialog is then
	// password-only. Whether THIS account has a passkey is asked when the dialog
	// opens — never on page load, which would spend a GoTrue call on every view.
	PasskeyPath string
	// FreshFor is how many milliseconds the current sign-in stays fresh, as
	// the SERVER measured it when it rendered the page (0 = not fresh). A
	// duration rather than an instant, so the browser's clock does not matter.
	FreshFor int64
	// Window is the freshness window as a person reads it ("12 hours") —
	// stepup.WindowText, passed in so this package states no window of its own.
	Window string
	// Why, when set, is the dialog's sentence about the gate in place of
	// StepUpWhy's (which names typing into a session): a page gating something
	// else says what. It should name Window too.
	Why string
}

// StepUpDialog is the dialog and its script (window.musterStepUp) for a page
// other than the terminal and the rendered view — today /plugins, whose Approve
// asks before it submits. Nothing for nil.
func StepUpDialog(s *StepUpView) g.Node {
	if s == nil {
		return nil
	}
	return g.Group{confirmDialog(s), stepUpScript()}
}

// stepUpAttrs carries a StepUpView to the terminal script on #term; nothing for
// nil.
func stepUpAttrs(s *StepUpView) g.Node {
	if s == nil {
		return nil
	}
	return g.Group{
		g.Attr("data-step-up", s.ConfirmPath),
		g.Attr("data-fresh-for", strconv.FormatInt(s.FreshFor, 10)),
	}
}

// StepUpWhy is the dialog's sentence about the gate, with the window in it.
func StepUpWhy(window string) string {
	return "Typing into this session needs a sign-in from the last " + window + "."
}

// stepUpWhyFor is s.Why when set, else StepUpWhy's sentence.
func stepUpWhyFor(s *StepUpView) string {
	if s.Why != "" {
		return s.Why
	}
	return StepUpWhy(s.Window)
}

// The dialog's "how" line, in its two states. The server renders the password
// one; stepUpScript swaps in the passkey one once it knows the account has a
// passkey this browser can use.
const (
	stepUpHowPassword = "Enter your password to continue."
	stepUpHowPasskey  = "Confirm with your passkey, or use your password."
)

const (
	stepUpPrimary   = "bg-accent text-on-accent hover:bg-accent/90"
	stepUpSecondary = "bg-bg text-fg ring-1 ring-inset ring-edge"
)

// confirmDialog is "Confirm it's you": a passkey or the password again before a
// gated action. stepUpScript posts to the StepUpView's routes (same origin,
// credentials included); the server's answer replaces the session's cookies
// with a freshly signed-in one.
//
// 🔴 THE PASSKEY IS THE ONE OPTION SHOWN; THE PASSWORD IS A COLLAPSE BEHIND IT
// (operator ask: "show only the passkey option with the password option inside
// a collapse closed by default"). The password sits in a <details>
// (stepUpPasswordBoxID), drawn SHUT with its summary ("Use your password
// instead") shown, so a passkey user never sees the password field flash or
// take focus (stepUpScript's offer):
//
//   - a passkey possible (this server has passkeys and this browser can run
//     WebAuthn): from the dialog's first frame the passkey button shows and has
//     focus, and the <details> stays SHUT — also while the options call is in
//     flight; its summary is the way to the password at any time;
//   - no passkey usable (none on this server, none on this account, no
//     WebAuthn here, or the options call failed), or a passkey device error:
//     the <details> OPENS and its summary is hidden — the password is then the
//     only working option, so it is never behind a tap.
//
// It is a plain <details>, so it opens and shuts with no script of its own.
func confirmDialog(s *StepUpView) g.Node {
	if s == nil {
		return nil
	}
	return Dialog(
		ID("term-confirm"),
		g.Attr("aria-labelledby", "term-confirm-title"),
		g.Attr("data-confirm", s.ConfirmPath),
		g.Attr("data-passkey", s.PasskeyPath),
		g.Attr("data-fresh-for", strconv.FormatInt(s.FreshFor, 10)),
		g.Attr("data-how-password", stepUpHowPassword),
		g.Attr("data-how-passkey", stepUpHowPasskey),
		g.Attr("data-options-timeout", stepUpNetTimeoutMS),
		Class("w-[calc(100%-2rem)] max-w-sm rounded-2xl bg-s1 p-0 text-fg ring-1 ring-line backdrop:bg-black/60"),
		Form(
			ID("term-confirm-form"),
			Class("flex flex-col gap-4 px-5 py-6"),
			H2(ID("term-confirm-title"), Class("text-base font-semibold text-fg"), g.Text("Confirm it's you")),
			P(ID("term-confirm-why"), Class("text-sm text-fg2"),
				g.Text(stepUpWhyFor(s)+" "), Span(ID("term-confirm-how"), g.Text(stepUpHowPassword))),
			Div(
				ID("term-confirm-passkey-box"),
				g.Attr("hidden", ""),
				Class("flex flex-col"),
				Button(ID("term-confirm-passkey"), Type("button"),
					g.Attr("data-label", stepUpPasskeyLabel),
					g.Attr("data-label-waiting", stepUpPasskeyWaiting),
					Class("press min-h-[44px] rounded-md px-3 py-2 text-base font-semibold transition aria-disabled:cursor-wait aria-disabled:opacity-60 "+stepUpPrimary),
					g.Text(stepUpPasskeyLabel)),
			),
			// Why no passkey is offered, when one should have been (stepUpScript's
			// unavailable): one muted line, hidden otherwise.
			P(ID("term-confirm-passkey-note"), g.Attr("hidden", ""), Class("text-xs text-muted")),
			Details(
				ID(stepUpPasswordBoxID),
				Class("group/pw"),
				// Hidden once the password is the only option (stepUpScript's
				// offer): then there is nothing to collapse it behind.
				Summary(
					ID(stepUpPasswordToggleID),
					Class("press flex min-h-[44px] cursor-pointer list-none items-center justify-center gap-1.5 rounded-md text-sm font-medium text-fg2 transition hover:text-fg group-open/pw:mb-2 [&::-webkit-details-marker]:hidden"),
					g.Text("Use your password instead"),
					g.Raw(`<svg class="h-4 w-4 shrink-0 text-muted transition-transform group-open/pw:rotate-180" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true"><path fill-rule="evenodd" d="M5.23 7.21a.75.75 0 011.06.02L10 11.168l3.71-3.938a.75.75 0 111.08 1.04l-4.25 4.5a.75.75 0 01-1.08 0l-4.25-4.5a.75.75 0 01.02-1.06z" clip-rule="evenodd"/></svg>`),
				),
				Div(
					Class("flex flex-col gap-3"),
					Label(For("term-confirm-password"), Class("text-sm font-medium text-fg"), g.Text("Password")),
					Input(
						ID("term-confirm-password"),
						Name("password"),
						Type("password"),
						g.Attr("autocomplete", "current-password"),
						g.Attr("required", ""),
						Class("min-h-[44px] rounded-md bg-bg px-3 py-2 text-base text-fg ring-1 ring-inset ring-edge focus:outline-none focus:ring-2 focus:ring-focus"),
					),
					Button(ID("term-confirm-submit"), Type("submit"),
						g.Attr("data-primary", stepUpPrimary),
						g.Attr("data-secondary", stepUpSecondary),
						Class("press min-h-[44px] rounded-md px-3 py-2 text-base font-semibold transition "+stepUpPrimary),
						g.Text(stepUpSubmitPassword)),
				),
			),
			P(ID("term-confirm-error"), g.Attr("role", "alert"), g.Attr("hidden", ""), Class("rounded-lg bg-st-error-bg px-3 py-2 text-sm text-st-error-fg ring-1 ring-inset ring-st-error-fg/40")),
			Button(ID("term-confirm-cancel"), Type("button"),
				Class("press min-h-[44px] rounded-md px-3 py-2 text-base font-semibold "+stepUpSecondary),
				g.Text("Cancel")),
		),
	)
}

// stepUpNetTimeout is how long the dialog waits on a passkey request
// (/options, /verify) before treating it as failed (stepUpScript's
// withTimeout). 🔴 DERIVED FROM THE SERVER'S OWN BOUNDS, AND ABOVE THEM: a
// passkey route may read GoTrue's /settings first (gotrue.SettingsTimeout)
// and then make up to two GoTrue calls in a row (/options lists the
// account's passkeys, then asks for sign-in options; each gotrue.HTTPTimeout),
// plus its own database work. A page that gave up sooner would abandon a
// /verify the server was still allowed to complete — blamed on the device,
// a second challenge spent, the waiting action dropped although the session
// had just been made fresh. The margin covers the database and the network.
const stepUpNetTimeout = gotrue.SettingsTimeout + 2*gotrue.HTTPTimeout + 5*time.Second

// stepUpNetTimeoutMS is stepUpNetTimeout as the script's milliseconds.
var stepUpNetTimeoutMS = strconv.FormatInt(stepUpNetTimeout.Milliseconds(), 10)

// The passkey button's label, and what it says while its challenge is being
// fetched (stepUpScript's pkWaiting).
const (
	stepUpPasskeyLabel   = "Confirm with passkey"
	stepUpPasskeyWaiting = "Checking for your passkey…"
)

// The password's collapse and its summary (confirmDialog).
const (
	stepUpPasswordBoxID    = "term-confirm-password-box"
	stepUpPasswordToggleID = "term-confirm-password-toggle"
)

// The password button's label in the dialog's two states: the only option
// ("Confirm"), and the one behind the passkey.
const (
	stepUpSubmitPassword = "Confirm"
	stepUpSubmitBehind   = "Confirm with password"
)

// StepUpFormView is the plain "Confirm it's you" page: the step-up for a send
// the BROWSER submitted (no script ran), which the dialog above cannot serve.
type StepUpFormView struct {
	// Action is the route the page posts to: it checks the password and, when
	// it is right, performs the send that was waiting.
	Action string
	// Fields are the pending send, carried as hidden inputs — its address, its
	// text, its nonce — so confirming completes it rather than asking for the
	// message again.
	Fields []ComposerField
	// BackHref is the view the send came from (Cancel, and where the result is
	// shown).
	BackHref string
	// Window is the freshness window as a person reads it ("12 hours").
	Window string
	// Error is why the last attempt was not accepted; "" on the first showing.
	Error string
}

// StepUpFormPage is a whole document with one form: the password again, and
// the pending send in hidden fields. No script is needed to use it — it exists
// for the page whose script did not run. (A passkey needs script; the dialog on
// a working page offers one.)
func StepUpFormPage(v StepUpFormView) g.Node {
	hidden := make([]g.Node, 0, len(v.Fields))
	for _, f := range v.Fields {
		hidden = append(hidden, Input(Type("hidden"), Name(f.Name), Value(f.Value)))
	}
	return Doctype(
		HTML(Class("dark"), Lang("en"),
			Head(
				Meta(Charset("utf-8")),
				Meta(Name("viewport"), Content("width=device-width, initial-scale=1, viewport-fit=cover")),
				Meta(Name("color-scheme"), Content("dark light")),
				ThemeHead(),
				manifestLink(),
				TitleEl(g.Text("Confirm it's you · muster")),
				Link(Rel("stylesheet"), Href(web.Asset("/static/app.css"))),
				faroHead(),
			),
			Body(
				Class("flex min-h-dvh items-center justify-center bg-bg px-4 text-fg antialiased"),
				Form(
					ID("stepup-form"),
					Method("post"),
					Action(v.Action),
					Class("flex w-full max-w-sm flex-col gap-4 rounded-2xl bg-s1 px-5 py-6 ring-1 ring-line"),
					g.Group(hidden),
					H1(Class("text-base font-semibold text-fg"), g.Text("Confirm it's you")),
					P(Class("text-sm text-fg2"), g.Text(StepUpWhy(v.Window)+" "+stepUpHowPassword+" Your message is sent once you have.")),
					Label(For("stepup-password"), Class("text-sm font-medium text-fg"), g.Text("Password")),
					Input(
						ID("stepup-password"),
						Name("password"),
						Type("password"),
						g.Attr("autocomplete", "current-password"),
						g.Attr("required", ""),
						g.Attr("autofocus", ""),
						Class("min-h-[44px] rounded-md bg-bg px-3 py-2 text-base text-fg ring-1 ring-inset ring-edge focus:outline-none focus:ring-2 focus:ring-focus"),
					),
					g.If(v.Error != "", P(ID("stepup-error"), g.Attr("role", "alert"),
						Class("rounded-lg bg-st-error-bg px-3 py-2 text-sm text-st-error-fg ring-1 ring-inset ring-st-error-fg/40"), g.Text(v.Error))),
					Div(
						Class("flex gap-2"),
						A(ID("stepup-cancel"), Href(v.BackHref),
							Class("press inline-flex min-h-[44px] flex-1 items-center justify-center rounded-md px-3 py-2 text-base font-semibold "+stepUpSecondary),
							g.Text("Cancel")),
						Button(ID("stepup-submit"), Type("submit"),
							Class("press min-h-[44px] flex-1 rounded-md px-3 py-2 text-base font-semibold transition "+stepUpPrimary),
							g.Text("Confirm and send")),
					),
				),
				// Like every document: the worker and the update toast. The form
				// above needs neither.
				pwaChrome(),
			),
		),
	)
}

// PasskeyJS is the WebAuthn helpers every passkey script on every page shares:
// the sign-in and account pages (internal/api) and the confirm dialog here.
// mpk* names keep them out of any other page script's way.
//
// ⚠ THE JSON <-> ArrayBuffer CONVERSION IS DONE BY HAND, not with
// PublicKeyCredential.parseRequestOptionsFromJSON / credential.toJSON(): those
// are missing from browsers muster's users still run (Safari before 18.x), and
// a page that works only where they exist would fail exactly on the phones the
// passkeys are for. GoTrue's options use base64url for every binary field.
const PasskeyJS = `
function mpkB64ToBuf(s) {
  s = String(s).replace(/-/g, '+').replace(/_/g, '/');
  while (s.length % 4) { s += '='; }
  var bin = atob(s), out = new Uint8Array(bin.length);
  for (var i = 0; i < bin.length; i++) { out[i] = bin.charCodeAt(i); }
  return out.buffer;
}
function mpkBufToB64(buf) {
  var b = new Uint8Array(buf), s = '';
  for (var i = 0; i < b.length; i++) { s += String.fromCharCode(b[i]); }
  return btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}
function mpkCredJSON(cred) {
  var r = cred.response, out = {
    id: cred.id, rawId: mpkBufToB64(cred.rawId), type: cred.type,
    authenticatorAttachment: cred.authenticatorAttachment || undefined,
    clientExtensionResults: cred.getClientExtensionResults ? cred.getClientExtensionResults() : {},
    response: { clientDataJSON: mpkBufToB64(r.clientDataJSON) }
  };
  if (r.attestationObject) {
    out.response.attestationObject = mpkBufToB64(r.attestationObject);
    if (r.getTransports) { out.response.transports = r.getTransports(); }
  } else {
    out.response.authenticatorData = mpkBufToB64(r.authenticatorData);
    out.response.signature = mpkBufToB64(r.signature);
    if (r.userHandle) { out.response.userHandle = mpkBufToB64(r.userHandle); }
  }
  return out;
}
async function mpkPost(path, body) {
  var res = await fetch(path, { method: 'POST', credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json', 'Accept': 'application/json' },
    body: JSON.stringify(body || {}) });
  var data = {};
  try { data = await res.json(); } catch (e) {}
  return { ok: res.ok, status: res.status, data: data };
}
// mpkMissing is why this document cannot run a WebAuthn ceremony, '' when it
// can: the first of no-publickeycredential, no-credentials, insecure-context.
function mpkMissing() {
  if (!window.PublicKeyCredential) return 'no-publickeycredential';
  if (!navigator.credentials) return 'no-credentials';
  if (!window.isSecureContext) return 'insecure-context';
  return '';
}
function mpkSupported() { return !mpkMissing(); }
function mpkSay(el, msg) { if (el) { el.textContent = msg; } }
function mpkCancelled(e) { return e && (e.name === 'NotAllowedError' || e.name === 'AbortError'); }
`

// PasskeyPrefetchJS: WebAuthn options are fetched BEFORE the tap and kept, so
// the tap's handler reaches navigator.credentials without a network round trip
// in between. WebKit grants WebAuthn only within a user gesture, and an await
// on fetch inside the click handler can outlive it (NotAllowedError on iOS). A
// prefetched challenge is single-use and GoTrue expires it after 5 minutes, so
// one older than mpkMaxAge, or one already spent, is fetched again. trigger,
// when given, is kept on the prefetch: why it was fetched (the confirm dialog's
// telemetry reports it).
const PasskeyPrefetchJS = `
var mpkMaxAge = 240000;
function mpkPrefetch(path, trigger) {
  var p = { at: Date.now(), trigger: trigger || '', promise: mpkPost(path, {}) };
  return p;
}
function mpkFresh(p) { return p && (Date.now() - p.at) < mpkMaxAge; }
`

// stepUpScript defines window.musterStepUp on a page that rendered
// confirmDialog (and leaves it undefined on one that did not):
//
//	fresh()        the page's belief that the sign-in is still fresh
//	stale()        the server just said otherwise
//	ask(why, done) open the dialog; done() runs after a successful confirm —
//	               the pending action (attach, or send the text again) — and
//	               never after a close without one (Cancel, Escape)
//	asking()       the dialog ask would use (the top window's, from the view
//	               switch's frame) is open now
//
// 🔴 IN THE VIEW SWITCH'S FRAME, ask OPENS THE TOP WINDOW'S DIALOG, not the
// frame's own (view_swap.go opens the other view in a same-origin frame over
// the page). Every caller — the Interactive switch, the composer's send, the
// send queue, an attention answer — goes through ask, so all of them follow.
// The top window's done receives the server's fresh_for, and the frame sets its
// own freshness from it: freshness is the cookie's, which both documents share.
// A top page with no dialog (no step-up on it) leaves the frame its own.
//
// 🔴 THE PASSKEY OPTIONS ARE FETCHED WHEN THE DIALOG OPENS, NOT ON PAGE LOAD.
// The sign-in page prefetches on load because signing in is what that page is
// for; these pages are opened all day by someone already fresh, and a challenge
// minted per page view would spend GoTrue's passkey rate budget on dialogs that
// never open. ONE options call per dialog open, and only when the account has
// a passkey: the server answers the same call with available:false, and no
// challenge, when it has none.
//
//   - 🔴 ask() while the dialog is ALREADY open fetches nothing and leaves the
//     offer as it is: the open's challenge (answered or still in flight) is
//     the one the tap uses. Before, every ask refetched and hid the passkey
//     button until the new answer — and a terminal sends an ask per key
//     pressed before the modal takes the keyboard.
//   - 🔴 A FAILED TAP KEEPS ITS CHALLENGE UNLESS GoTrue SAW IT. A challenge
//     is spent by GoTrue's verify, not by navigator.credentials.get: one the
//     person cancelled (NotAllowedError) or the device refused never reached
//     the server and is still good, so the next tap reuses it. So is one
//     muster's /verify refused BEFORE asking GoTrue — an unreadable assertion,
//     no user verification, the sign-in limiter — which the answer says with
//     challenge_unspent: true (internal/api stepUpRefuseUnspent; the status
//     cannot, since GoTrue's own refusals are also 401 and 429): it is kept
//     and nothing is fetched. A refusal GoTrue made fetches a replacement — at
//     once, NOT on the next tap, because a fetch inside the tap's handler is
//     exactly the gesture-outliving await PasskeyPrefetchJS exists to avoid (a
//     lazy fetch would fail every retry on iOS) — EXCEPT a 429, after which a
//     fetch would only meet the same limit: nothing is fetched until the next
//     tap. A challenge older than mpkMaxAge is fetched on the tap, as before.
//
// The tap on "Confirm with passkey" then reaches navigator.credentials on an
// already-resolved promise — no network between the gesture and WebAuthn (see
// PasskeyPrefetchJS). 🔴 THAT IS WHY THE BUTTON IS INERT WHILE A FETCH IS IN
// FLIGHT (pkWaiting): it is drawn, and focused, from the open's first frame,
// but aria-disabled — "Checking for your passkey…" — until the open's options
// answer, and again while a retry's replacement is fetched; a tap meanwhile
// does nothing (it would await that fetch inside the gesture: on iOS, the
// NotAllowedError read as "The passkey was cancelled"). aria-disabled, not
// disabled: a disabled button cannot hold the focus it is given. The one
// exception stays the one above: a challenge older than mpkMaxAge.
//
// Telemetry (window.cgTrack, Faro's custom events; nothing when telemetry is
// off), two events, both carrying open_id — a random id the page makes at each
// dialog open (16 hex digits from crypto.getRandomValues, derived from nothing
// about the person or the session), so every event of one open groups and a
// new open is a new id:
//
//   - stepup.passkey_options, ONE PER /options FETCH, sent when it answers:
//     trigger (open: the open's prefetch; stale: a tap found it older than
//     mpkMaxAge; retry: a refused /verify spent it, or the last answer was
//     unusable), status (the HTTP status, or network) and taps (how many taps
//     this open had made when it was fetched: 0 for the open's own). A burst of
//     fetches with no tap is visible as these with no stepup.passkey.
//   - stepup.passkey, ONE PER TAP: attempt (the tap's number in this open),
//     options_trigger (why the challenge it used was fetched: open, stale or
//     retry), options_status (that fetch's HTTP status, or network), get (how
//     navigator.credentials.get ended: ok, the DOMException's name, or
//     not-called) and verify (/verify's status, '' when not sent).
//
// No credential, challenge or account value is in either.
func stepUpScript() g.Node {
	return Script(g.Raw(PasskeyJS + PasskeyPrefetchJS + `
window.musterStepUp = (function () {
  var dlg = document.getElementById('term-confirm');
  if (!dlg) return null;
  var path = dlg.getAttribute('data-confirm');
  var pkBase = dlg.getAttribute('data-passkey') || '';
  var form = document.getElementById('term-confirm-form');
  var pw = document.getElementById('term-confirm-password');
  var err = document.getElementById('term-confirm-error');
  var submit = document.getElementById('term-confirm-submit');
  var how = document.getElementById('term-confirm-how');
  var pkBox = document.getElementById('term-confirm-passkey-box');
  var pkBtn = document.getElementById('term-confirm-passkey');
  var pkNote = document.getElementById('term-confirm-passkey-note');
  var pwBox = document.getElementById('` + stepUpPasswordBoxID + `');
  var pwToggle = document.getElementById('` + stepUpPasswordToggleID + `');
  var done = [], pre = null, freshUntil = 0, attempts = 0, openId = '';
  var ff = +dlg.getAttribute('data-fresh-for') || 0;
  if (ff > 0) freshUntil = performance.now() + ff;

  function fail(msg) { err.textContent = msg || ''; err.hidden = !msg; }
  function swap(el, from, to) {
    from.split(' ').forEach(function (c) { el.classList.remove(c); });
    to.split(' ').forEach(function (c) { el.classList.add(c); });
  }
  // offer shows (or hides) the passkey as THE action (confirmDialog): offered,
  // the password is behind its collapse, shut, and its button steps down to
  // secondary; not offered, the collapse is open with no summary — the password
  // is then the only option, never a tap away.
  function offer(on) {
    if (on) unavailable('');
    else pkWaiting(false);
    pkBox.hidden = !on;
    dlg.setAttribute('data-passkey-offered', on ? '1' : '0');
    how.textContent = dlg.getAttribute(on ? 'data-how-passkey' : 'data-how-password');
    pwToggle.hidden = !on;
    pwBox.open = !on;
    var pri = submit.getAttribute('data-primary'), sec = submit.getAttribute('data-secondary');
    if (on) { swap(submit, pri, sec); submit.textContent = '` + stepUpSubmitBehind + `'; }
    else { swap(submit, sec, pri); submit.textContent = '` + stepUpSubmitPassword + `'; }
  }
  // pkWaiting marks the passkey button inert, and says so, while the
  // challenge a tap would use is being fetched (see stepUpScript).
  function pkWaiting(on) {
    if (on) pkBtn.setAttribute('aria-disabled', 'true');
    else pkBtn.removeAttribute('aria-disabled');
    pkBtn.textContent = pkBtn.getAttribute(on ? 'data-label-waiting' : 'data-label');
  }
  // Opened by its summary, the password takes focus, so the keyboard is up.
  pwBox.addEventListener('toggle', function () { if (pwBox.open && dlg.open && !pwToggle.hidden) pw.focus(); });
  // 🔴 EVERY WAIT ON THE NETWORK HAS AN END. A request that never answers
  // (a dead mobile link; measured in CI, a fetch left paused when the test
  // harness took its request interception away) left the passkey button inert
  // — "Checking for your passkey…" — for the rest of the open. withTimeout
  // settles p within the dialog's limit (data-options-timeout, ms: Go's
  // stepUpNetTimeout, derived to sit ABOVE the server's own worst case for
  // these routes), rejecting with a TimeoutError: a prefetch that takes
  // longer is a failed one, and the password opens; a tap's says the
  // connection timed out.
  function netTimeout() { return +dlg.getAttribute('data-options-timeout') || ` + stepUpNetTimeoutMS + `; }
  function withTimeout(p) {
    return new Promise(function (resolve, reject) {
      var t = setTimeout(function () {
        var e = new Error('the request took too long'); e.name = 'TimeoutError'; reject(e);
      }, netTimeout());
      p.then(function (v) { clearTimeout(t); resolve(v); }, function (e) { clearTimeout(t); reject(e); });
    });
  }
  // prefetch resolves to '' when a passkey can be offered, else the reason it
  // cannot (an UNAVAILABLE value: options-failed:timeout when it took longer
  // than withTimeout allows), or null when a newer prefetch replaced it.
  function prefetch(trigger) {
    var mine = pre = fetchOptions(trigger);
    return withTimeout(mine.promise).then(function (o) {
      if (pre !== mine) return null;
      var d = o.data || {};
      var why = !o.ok ? 'options-failed:' + o.status : !d.available ? 'not-available' : !d.challenge_id ? 'no-challenge' : '';
      if (why) pre = null;
      return why;
    }, function (e) {
      optionsTimedOut(mine, e);
      if (pre !== mine) return null;
      pre = null;
      return e && e.name === 'TimeoutError' ? 'options-failed:timeout' : 'options-failed:network';
    });
  }
  // settled is a prefetch's answer applied to the button: inert no longer,
  // and — when no passkey can be had — the password opened. A prefetch that
  // a newer one replaced (null) leaves the button to that one; with none in
  // flight (pre null: the dialog closed, or nothing replaced it) the button is
  // live again, so there is no path on which it stays inert.
  function settled(r, focusPw) {
    if (r === null) { if (!pre) pkWaiting(false); return; }
    pkWaiting(false);
    if (!dlg.open || !r) return;
    offer(false); unavailable(r);
    if (focusPw) pw.focus();
  }
  // unavailable records why the passkey is NOT offered, on the dialog
  // (data-passkey-unavailable), so a dialog that offers only the password says
  // why in the page itself — and, as a muted line, on screen (a screenshot
  // carries it) — except where nothing is the right thing to show: no passkey
  // route on this server (no-path), or an account with no passkey
  // (not-available). '' clears both.
  function unavailable(why) {
    if (why) dlg.setAttribute('data-passkey-unavailable', why);
    else dlg.removeAttribute('data-passkey-unavailable');
    var shown = !!why && why !== 'no-path' && why !== 'not-available';
    pkNote.textContent = shown ? 'Passkey unavailable here (' + why + ')' : '';
    pkNote.hidden = !shown;
  }
  // finish takes the waiting callbacks, closes the dialog, then runs each, in
  // the order they asked, in its own try: one that throws is logged and does
  // not stop the next (each is a different caller's pending action). The close
  // listener empties done (a close is a Cancel unless finish made it), but the
  // close EVENT is queued as a task, so it fires after finish has taken them;
  // taking them first keeps that true whatever the event's timing (measured:
  // swapping the two lines is not caught, because Chromium queues the event).
  function finish(freshFor) {
    freshUntil = performance.now() + (+freshFor || 0);
    pw.value = '';
    var cbs = done; done = [];
    dlg.close();
    // The duration, not an instant: a framed view that handed its ask here
    // (topStepUp) keeps its own clock (performance.now() is per document).
    cbs.forEach(function (cb) {
      try { cb(+freshFor || 0); } catch (e) { try { console.error('musterStepUp: a confirmed action failed', e); } catch (e2) {} }
    });
  }
  // topStepUp is the TOP window's musterStepUp when this page is the view in
  // the Rendered | Terminal switch's frame (view_swap.go), else null.
  function topStepUp() {
    try {
      var fe = window.frameElement;
      if (!fe || !fe.hasAttribute('` + ViewFrameAttr + `')) return null;
      var t = window.parent.musterStepUp;
      return t && t !== window.musterStepUp && t.framedAsk === true && typeof t.ask === 'function' ? t : null;
    } catch (e) { return null; }
  }
  // The view swap makes the page under a revealed frame inert (every child of
  // the body, so maybe the dialog itself), and a dialog that is or is inside an
  // inert element cannot be used, modal or not: that inert is lifted while it
  // is open, and put back on close unless the swap took its own inert away
  // meanwhile (the frame went home).
  var lifted = [];
  function lift() {
    for (var el = dlg; el; el = el.parentElement) {
      if (el.inert) { el.inert = false; lifted.push({ el: el, swap: el.hasAttribute('data-view-inert') }); }
    }
  }
  // On close every element the swap marked (data-view-inert) is inert again —
  // also one the swap marked but left usable because this dialog was open in
  // it (view_swap.go's inertAll, a frame revealed while the dialog was open).
  function unlift() {
    lifted.forEach(function (l) { if (!l.swap) l.el.inert = true; });
    lifted = [];
    for (var el = dlg; el; el = el.parentElement) { if (el.hasAttribute('data-view-inert')) el.inert = true; }
  }
  // newOpenId is a random id for one dialog open: correlation only, made from
  // nothing identifying.
  function newOpenId() {
    var b = new Uint8Array(8), s = '';
    try { crypto.getRandomValues(b); } catch (e) { for (var i = 0; i < 8; i++) b[i] = Math.floor(Math.random() * 256); }
    for (var j = 0; j < 8; j++) s += (b[j] < 16 ? '0' : '') + b[j].toString(16);
    return s;
  }
  function ask(why, cb) {
    // 🔴 A FRAMED VIEW ASKS IN THE TOP WINDOW'S DIALOG. Reported from the
    // Android app (a WebView, WebAuthn as the app): the passkey was offered on
    // the rendered view loaded on its own, and the terminal opened in the
    // switch's frame asked for the password only — its dialog never requested
    // the passkey options, so the frame judged a passkey unusable there. WHY is
    // not measured: an emulator's WebView (133 and 145) gives a same-origin
    // frame WebAuthn as it gives the top document. The top window's dialog is
    // the one seen to offer and complete the passkey, so the frame's own
    // answer no longer matters. The server's answer re-signs the session's
    // cookies, which this frame shares, and this page's own belief in
    // freshness is set from the same answer.
    var t = topStepUp();
    if (t) {
      done = [];
      t.ask(why, function (ff) {
        if (ff !== undefined) freshUntil = performance.now() + (+ff || 0);
        if (cb) cb();
      });
      return;
    }
    // 🔴 ALREADY OPEN: the new caller's done runs after the ones already
    // waiting (all were waiting on this confirm; a caller with none drops
    // nothing), its why is shown, and nothing else changes — no refetch, no
    // offer(false), the password typed so far kept. The open's own options
    // call (answered or in flight) stands (see stepUpScript).
    if (dlg.open) {
      if (cb) done.push(cb);
      if (why) fail(why);
      return;
    }
    done = cb ? [cb] : [];
    attempts = 0;
    openId = newOpenId();
    // A tap of an earlier open may still hold the button (ours, in the tap):
    // a new open starts with it usable.
    pkBtn.disabled = false;
    fail(why);
    pw.value = '';
    // 🔴 DECIDED BEFORE THE DIALOG IS SHOWN, so its first frame is already
    // the right one: a passkey that CAN be used here leads (offered, password
    // shut, focus on the passkey) while the options call is in flight; one that
    // cannot (no route on this server, no WebAuthn) opens the password now.
    var missing = !pkBase ? 'no-path' : mpkMissing();
    offer(!missing);
    unavailable(missing);
    if (!dlg.open) { lift(); dlg.showModal(); }
    if (missing) { pw.focus(); return; }
    pkWaiting(true);
    pkBtn.focus();
    // The account has no passkey, or the options call failed or timed out:
    // the password opens, as the only option, and takes focus.
    prefetch('open').then(function (r) { settled(r, true); });
  }

  // 🔴 EVERY CLOSE BUT finish's IS A CANCEL: Cancel, Escape (the dialog's own
  // cancel, which closes it) or anything else that closes it drops the waiting
  // actions, so a password POST or passkey /verify still in flight when the
  // person walked away does not run them when it answers (finish took its
  // callbacks before closing, so a confirm that succeeded still runs them).
  document.getElementById('term-confirm-cancel').addEventListener('click', function () { dlg.close(); });
  // 🔴 A CLOSE EVENT IS A TASK, AND CAN LAND AFTER THE NEXT OPEN: Escape (or
  // close()) shuts the dialog at once but queues its 'close' event, and an ask
  // in between — measured in mirror-stepup.spec.ts's callback test, an ask
  // right after an Escape — already reopened it with its own callbacks and
  // its own options fetch. That late event then dropped both, and the new
  // open's answer was ignored (pre was no longer its fetch): the passkey sat
  // "Checking…" for good. An event for a dialog that is open again is the
  // old close's, and changes nothing.
  dlg.addEventListener('close', function () {
    if (dlg.open) return;
    done = []; pre = null; unlift(); unavailable(''); pkWaiting(false);
  });
  form.addEventListener('submit', function (ev) {
    ev.preventDefault();
    submit.disabled = true;
    fetch(path, {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Accept': 'application/json' },
      body: new URLSearchParams({ password: pw.value })
    }).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (j) { return { ok: r.ok, j: j }; });
    }).then(function (res) {
      submit.disabled = false;
      if (!res.ok) {
        fail(res.j.error || 'That could not be confirmed.');
        pw.value = '';
        pw.focus();
        return;
      }
      finish(res.j.fresh_for);
    }, function () {
      submit.disabled = false;
      fail('muster could not be reached. Try again.');
    });
  });
  // pkOptions is the WebAuthn request options from an options answer, with
  // its binary fields decoded — a COPY: the answer is kept, and reused by the
  // next tap while its challenge is unspent, so it must stay base64url.
  function pkOptions(src) {
    var pk = {};
    for (var k in src) { if (Object.prototype.hasOwnProperty.call(src, k)) pk[k] = src[k]; }
    pk.challenge = mpkB64ToBuf(src.challenge);
    if (src.allowCredentials) {
      pk.allowCredentials = src.allowCredentials.map(function (c) {
        var d = {};
        for (var k in c) { if (Object.prototype.hasOwnProperty.call(c, k)) d[k] = c[k]; }
        d.id = mpkB64ToBuf(c.id);
        return d;
      });
    }
    return pk;
  }
  // track sends one telemetry event (see stepUpScript). cgTrack is faro.go's,
  // defined only when telemetry is on.
  function track(name, ev) {
    try { if (typeof window.cgTrack === 'function') window.cgTrack(name, ev); } catch (e) {}
  }
  // fetchOptions is EVERY /options fetch this dialog makes (mpkPrefetch), and
  // sends its stepup.passkey_options event when it answers. The event's fields
  // are taken now: an answer that lands after the dialog closed is still this
  // open's.
  function fetchOptions(trigger) {
    var p = mpkPrefetch(pkBase + '/options', trigger);
    var ev = { open_id: openId, trigger: trigger, status: '', taps: attempts };
    // sent ONCE: on its answer, or — when withTimeout gave up on it first
    // (optionsTimedOut) — as status timeout, a late answer then sending none.
    p.report = function (status) {
      if (p.reported) return;
      p.reported = true;
      ev.status = status;
      track('stepup.passkey_options', ev);
    };
    p.promise.then(function (o) { p.report(o.status); }, function () { p.report('network'); });
    return p;
  }
  // optionsTimedOut reports p's fetch as timed out (fetchOptions) when e is
  // withTimeout's TimeoutError.
  function optionsTimedOut(p, e) {
    if (e && e.name === 'TimeoutError' && p && p.report) p.report('timeout');
  }
  pkBtn.addEventListener('click', async function () {
    // Inert while its challenge is fetched (pkWaiting): no gesture spent on a wait.
    if (pkBtn.getAttribute('aria-disabled') === 'true') return;
    pkBtn.disabled = true;
    fail('');
    var ev = { open_id: openId, attempt: ++attempts, options_trigger: '', options_status: '', get: 'not-called', verify: '' };
    // 🔴 A TAP BELONGS TO ITS OPEN. Its awaits can outlive it (Escape during a
    // /verify, then a new open): once the dialog was closed or opened again
    // since, the tap changes nothing on screen and fetches nothing — the new
    // open has its own state and its own challenge. (ask() re-enables the
    // button a new open shows.)
    var tapOpen = openId;
    function ours() { return dlg.open && openId === tapOpen; }
    // spent: /verify was sent, so GoTrue has used this challenge up.
    var spent = false;
    try {
      if (!mpkFresh(pre)) { pre = fetchOptions(pre ? 'stale' : 'retry'); }
      var mine = pre;
      ev.options_trigger = mine.trigger || '';
      var o;
      try { o = await withTimeout(mine.promise); } catch (e) { optionsTimedOut(mine, e); ev.options_status = e && e.name === 'TimeoutError' ? 'timeout' : 'network'; if (pre === mine) pre = null; throw e; }
      ev.options_status = o.status;
      if (!ours()) return;
      if (!o.ok || !o.data.available || !o.data.challenge_id) {
        if (pre === mine) pre = null;
        offer(false);
        fail(o.data.error || 'A passkey cannot be used right now. Use your password.');
        return;
      }
      var cred;
      try { cred = await navigator.credentials.get({ publicKey: pkOptions(o.data.options) }); ev.get = 'ok'; }
      catch (e) { ev.get = (e && e.name) || 'error'; throw e; }
      var body = { challenge_id: o.data.challenge_id, credential: mpkCredJSON(cred) };
      if (pre === mine) pre = null;
      spent = true;
      var v = await withTimeout(mpkPost(pkBase + '/verify', body));
      ev.verify = v.status;
      if (!ours()) {
        // The session is fresh whatever this page shows now; the open that
        // asked for it is gone, and the current one is left as it is.
        if (v.ok) freshUntil = performance.now() + (+v.data.fresh_for || 0);
        spent = false;
        return;
      }
      if (!v.ok) {
        fail(v.data.error || 'That passkey was not accepted.');
        if (v.data.challenge_unspent === true) {
          // muster refused it before GoTrue saw the challenge (an unreadable
          // assertion, no user verification, the sign-in limiter): it is still
          // good, so it is put back for the next tap and nothing is fetched.
          spent = false;
          if (!pre) pre = mine;
        } else if (v.status === 429) {
          // GoTrue's own limit: the challenge may be spent, but a fetch now
          // would only meet the same limit — the next tap fetches (stale/retry).
          spent = false;
        }
        return;
      }
      spent = false;
      finish(v.data.fresh_for);
    } catch (e) {
      var timedOut = e && e.name === 'TimeoutError';
      if (spent && !ev.verify) ev.verify = timedOut ? 'timeout' : 'network';
      if (!ours()) { spent = false; return; }
      // A request that did not answer is the connection's, not the device's:
      // said so, and the passkey stays offered (the summary has the password).
      // A device that cannot use the passkey leaves the password as the way
      // on, so it is opened; a cancel is the person's, who may tap again.
      if (timedOut) { fail('The connection timed out. Try again, or use your password.'); return; }
      if (!mpkCancelled(e)) pwBox.open = true;
      fail(mpkCancelled(e) ? 'The passkey was cancelled. Try again, or use your password.' : 'This device could not use a passkey. Use your password.');
    } finally {
      if (ours()) pkBtn.disabled = false;
      track('stepup.passkey', ev);
      // A challenge /verify was sent with is used up: the next tap needs a new
      // one, fetched NOW so that tap still has its gesture. One the tap never
      // sent (cancelled, refused by the device) is still good and still in
      // pre — nothing is fetched (see stepUpScript).
      if (spent && ours() && !pkBox.hidden) {
        pkWaiting(true);
        prefetch('retry').then(function (r) { settled(r, false); });
      }
    }
  });

  return {
    // A margin, so an action is not tried in the last seconds of freshness.
    fresh: function () { return performance.now() < freshUntil - 10000; },
    stale: function () { freshUntil = 0; },
    ask: ask,
    asking: function () {
      var t = topStepUp();
      if (t) return typeof t.asking === 'function' ? t.asking() : false;
      return dlg.open;
    },
    // framedAsk: this window's dialog can be asked from the view switch's
    // frame — it lifts the swap's inert while it is open (lift). A top page
    // without it (one loaded before this) would open its dialog inert,
    // unusable, over a blocked frame; topStepUp leaves such a frame its own.
    framedAsk: true
  };
})();
`))
}
