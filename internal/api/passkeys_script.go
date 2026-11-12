package api

// The passkey pages' scripts. Inline, like every auth page's (the CSP allows
// inline script; see securityHeaders), and plain ES2017 so an installed PWA on
// an older iOS runs them.
//
// The shared helpers (mpk*: the JSON <-> ArrayBuffer conversion, the options
// prefetch) live in internal/ui (ui.PasskeyJS, ui.PasskeyPrefetchJS), because
// the "Confirm it's you" dialog there uses them too — one copy.

import (
	"encoding/json"

	"github.com/ZacxDev/muster/internal/ui"
)

// The sign-in page's words for a ceremony that did not sign anyone in. A
// refusal by the server (the passkey was checked and turned down) shows the
// server's own answer instead (PasskeyRejectedRefusal, PasskeyNoUVRefusal,
// PasskeyUnknownRefusal).
const (
	// PasskeyNotUsedNote: the browser ended the ceremony with no passkey —
	// NotAllowedError or AbortError, which is the person closing the sheet,
	// no passkey for this site on the device, or a timeout: the page cannot
	// tell those apart (WebAuthn hides it on purpose), so it does not say
	// "cancelled" — often nobody cancelled anything.
	PasskeyNotUsedNote = "No passkey was used. You can try again, use a passkey from your phone, or sign in with your password. " +
		"If a password manager opened its own window, its “use another device” option hands over to the browser."
	// PasskeyDeviceFailedNote: any other error from the browser.
	PasskeyDeviceFailedNote = "This device could not use a passkey. Try again, or sign in with your password."
)

// jsStr is s as a JavaScript string literal.
func jsStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// passkeySignInJS drives the sign-in page's passkeys: autofill, the "Sign in
// with a passkey" button and the "Use a passkey from my phone" button.
//
//   - 🔴 AUTOFILL (conditional mediation). Where the browser offers it —
//     getClientCapabilities().conditionalGet, else
//     isConditionalMediationAvailable(); both feature-detected — the page
//     starts navigator.credentials.get({mediation: 'conditional'}) on load,
//     with its OWN options (a challenge of its own: a challenge is single-use,
//     and the buttons keep theirs). The browser then lists the account's
//     passkeys in the email field's suggestions (autocomplete="username
//     webauthn"); picking one signs in like the button. A failed autofill
//     sign-in shows why and is not restarted until the next button ceremony
//     ends (a browser that answers a conditional request at once — an
//     automated one — would otherwise loop).
//   - 🔴 A BUTTON ABORTS THE PENDING AUTOFILL REQUEST FIRST. A page may have
//     one WebAuthn request at a time, so the button aborts the conditional
//     one (its AbortController) and waits — briefly, so the tap's gesture is
//     not outlived — for it to end before starting the modal one. That abort
//     shows nothing: it is the page's own. The modal request uses the
//     prefetched challenge (PasskeyPrefetchJS: no network between the tap and
//     WebAuthn), and both ceremonies get new challenges when they end.
//   - 🔴 SO DOES THE PASSWORD FORM'S SUBMIT: it aborts the pending autofill
//     request before the form posts, and none is started again on that page.
//   - The phone button is the same request with hints: ['hybrid'] (WebAuthn
//     Level 3; Chrome 128+), asking the browser to go straight to "use a phone
//     or tablet" (the QR code). ⚠ A password-manager extension that
//     intercepts navigator.credentials.get may ignore hints and show its own
//     window anyway; its "use another device" option hands over to the
//     browser's, which is what PasskeyNotUsedNote tells the person.
//   - A refusal whose answer says credential_unknown — muster showed the
//     passkey is not this account's (passkey_signal.go) — tells the password
//     manager to stop offering it: PublicKeyCredential.signalUnknownCredential,
//     feature-detected, with the request's rpId and the credential's id.
var passkeySignInJS = ui.PasskeyPrefetchJS + `
(function () {
  var btn = document.getElementById('passkey-signin'), phone = document.getElementById('passkey-signin-phone'),
      msg = document.getElementById('passkey-signin-status');
  if (!btn) { return; }
  if (!mpkSupported()) { btn.hidden = true; if (phone) { phone.hidden = true; } return; }
  var path = '` + passkeySignInOptionsPath + `', notUsed = ` + jsStr(PasskeyNotUsedNote) + `, failed = ` + jsStr(PasskeyDeviceFailedNote) + `;
  var pre = mpkPrefetch(path), cond = null, busy = false, left = false;
  function lock(on) { busy = on; btn.disabled = on; if (phone) { phone.disabled = on; } }
  function toBuf(pk) {
    pk.challenge = mpkB64ToBuf(pk.challenge);
    (pk.allowCredentials || []).forEach(function (c) { c.id = mpkB64ToBuf(c.id); });
    return pk;
  }
  function signalUnknown(rpId, id) {
    var P = window.PublicKeyCredential;
    if (!rpId || !id || !P || typeof P.signalUnknownCredential !== 'function') { return; }
    try {
      var p = P.signalUnknownCredential({ rpId: rpId, credentialId: id });
      if (p && p.catch) { p.catch(function () {}); }
    } catch (e) {}
  }
  // finish verifies cred (from options o) and signs in; false when refused.
  async function finish(o, rpId, cred) {
    var next = (document.querySelector('input[name=next]') || {}).value || '/';
    var v = await mpkPost('` + passkeySignInVerifyPath + `', { challenge_id: o.data.challenge_id, credential: mpkCredJSON(cred), next: next });
    if (v.ok) { left = true; location.assign(v.data.redirect || '/'); return true; }
    if (v.data.credential_unknown) { signalUnknown(rpId, cred.id); }
    mpkSay(msg, v.data.error || 'That passkey was not accepted.');
    return false;
  }
  async function autofillAvailable() {
    var P = window.PublicKeyCredential;
    try {
      if (typeof P.getClientCapabilities === 'function') {
        var caps = await P.getClientCapabilities();
        if (caps && caps.conditionalGet === true) { return true; }
      }
      if (typeof P.isConditionalMediationAvailable === 'function') { return (await P.isConditionalMediationAvailable()) === true; }
    } catch (e) {}
    return false;
  }
  async function startAutofill() {
    if (cond || busy || left) { return; }
    var avail = await autofillAvailable();
    if (!avail || cond || busy) { return; }
    var ac = new AbortController(), c = { ac: ac, done: null };
    cond = c;
    c.done = (async function () {
      try {
        var o = await mpkPost(path, {});
        if (ac.signal.aborted || !o.ok) { return; }
        var rpId = o.data.options.rpId;
        var cred = await navigator.credentials.get({ mediation: 'conditional', publicKey: toBuf(o.data.options), signal: ac.signal });
        if (cond !== c || busy) { return; }
        cond = null; lock(true); mpkSay(msg, '');
        try { await finish(o, rpId, cred); } finally { lock(false); }
      } catch (e) {
        if (ac.signal.aborted) { return; }
        if (!mpkCancelled(e)) { mpkSay(msg, failed); }
      } finally {
        if (cond === c) { cond = null; }
      }
    })();
  }
  async function modal(hints) {
    if (busy) { return; }
    lock(true); mpkSay(msg, '');
    var c = cond;
    cond = null;
    if (c) { c.ac.abort(); }
    try {
      if (!mpkFresh(pre)) { pre = mpkPrefetch(path); }
      var o = await pre.promise;
      pre = null;
      if (!o.ok) { mpkSay(msg, o.data.error || 'Passkey sign-in is unavailable.'); return; }
      var rpId = o.data.options.rpId, pk = toBuf(o.data.options);
      if (hints) { pk.hints = hints; }
      if (c && c.done) { await Promise.race([c.done, new Promise(function (r) { setTimeout(r, 300); })]); }
      var cred = await navigator.credentials.get({ publicKey: pk });
      await finish(o, rpId, cred);
    } catch (e) {
      mpkSay(msg, mpkCancelled(e) ? notUsed : failed);
    } finally {
      lock(false);
      if (!left) {
        if (!pre) { pre = mpkPrefetch(path); }
        startAutofill();
      }
    }
  }
  btn.addEventListener('click', function () { modal(null); });
  if (phone) { phone.addEventListener('click', function () { modal(['hybrid']); }); }
  // The password form's submit ends the autofill request too: the person
  // chose the password, so the page abandons the passkey request (and starts
  // no other) rather than leave it pending while the form posts.
  var pwForm = (document.getElementById('muster-password') || {}).form;
  if (pwForm) {
    pwForm.addEventListener('submit', function () {
      var c = cond;
      cond = null; left = true;
      if (c) { c.ac.abort(); }
    });
  }
  startAutofill();
})();
`

// passkeySignalJS hands the #passkey-signal element's data (passkeySignal,
// passkey_signal.go) to the WebAuthn Signal API: the complete set of accepted
// credential ids when the page has it (allAcceptedCredentialIds non-null),
// and the account's name. Each call is feature-detected and its rejection
// ignored; a browser without the API is left as it was.
const passkeySignalJS = `
(function () {
  var el = document.getElementById('passkey-signal'), P = window.PublicKeyCredential;
  if (!el || !P) { return; }
  var d;
  try { d = JSON.parse(el.getAttribute('data-signal') || ''); } catch (e) { return; }
  if (!d || !d.rpId || !d.userId) { return; }
  function quiet(f, arg) {
    if (typeof f !== 'function') { return; }
    try { var p = f.call(P, arg); if (p && p.catch) { p.catch(function () {}); } } catch (e) {}
  }
  if (Array.isArray(d.allAcceptedCredentialIds)) {
    quiet(P.signalAllAcceptedCredentials, { rpId: d.rpId, userId: d.userId, allAcceptedCredentialIds: d.allAcceptedCredentialIds });
  }
  if (d.name) {
    quiet(P.signalCurrentUserDetails, { rpId: d.rpId, userId: d.userId, name: d.name, displayName: d.name });
  }
})();
`

// passkeyAddJS drives /account/passkeys: add a passkey. The registration
// options are prefetched on load; when the server answers reauth_required the
// password form is shown instead of the button, and a correct password
// prefetches again and brings the button back (the person taps it: creating a
// passkey needs its own gesture).
const passkeyAddJS = ui.PasskeyPrefetchJS + `
(function () {
  var btn = document.getElementById('passkey-add'), msg = document.getElementById('passkey-status'),
      form = document.getElementById('passkey-reauth'), pw = document.getElementById('passkey-password');
  if (!btn) { return; }
  if (!mpkSupported()) { btn.disabled = true; mpkSay(msg, 'This browser cannot create passkeys.'); return; }
  var path = '` + accountPasskeysPath + `/options', pre = null;
  function needReauth(r) { return r.status === 401 && r.data.error === '` + PasskeyReauthRequired + `'; }
  async function load() {
    pre = mpkPrefetch(path);
    var o = await pre.promise;
    if (needReauth(o)) { pre = null; btn.hidden = true; form.hidden = false; }
  }
  btn.addEventListener('click', async function () {
    btn.disabled = true; mpkSay(msg, '');
    try {
      if (!mpkFresh(pre)) { pre = mpkPrefetch(path); }
      var o = await pre.promise;
      pre = null;
      if (needReauth(o)) { btn.hidden = true; form.hidden = false; pw.focus(); return; }
      if (!o.ok) { mpkSay(msg, o.data.error || 'A passkey cannot be added right now.'); return; }
      var pk = o.data.options;
      pk.challenge = mpkB64ToBuf(pk.challenge);
      pk.user.id = mpkB64ToBuf(pk.user.id);
      (pk.excludeCredentials || []).forEach(function (c) { c.id = mpkB64ToBuf(c.id); });
      var cred = await navigator.credentials.create({ publicKey: pk });
      var v = await mpkPost('` + accountPasskeysPath + `/verify', { challenge_id: o.data.challenge_id, credential: mpkCredJSON(cred) });
      if (needReauth(v)) { btn.hidden = true; form.hidden = false; pw.focus(); return; }
      if (!v.ok) { mpkSay(msg, v.data.error || 'That passkey could not be added.'); return; }
      location.assign('` + accountPasskeysPath + `');
    } catch (e) {
      mpkSay(msg, mpkCancelled(e) ? 'Adding a passkey was cancelled.' : 'This device could not create a passkey.');
    } finally {
      btn.disabled = false;
      if (!pre && !btn.hidden) { pre = mpkPrefetch(path); }
    }
  });
  form.addEventListener('submit', async function (ev) {
    ev.preventDefault();
    // The shared "confirm it's you" route (stepup.go): the session user's
    // password again, form-encoded, answered with a fresh session.
    var res = await fetch('` + stepUpConfirmPath + `', { method: 'POST', credentials: 'same-origin',
      headers: { 'Accept': 'application/json' }, body: new URLSearchParams({ password: pw.value }) });
    var r = { ok: res.ok, data: {} };
    try { r.data = await res.json(); } catch (e) {}
    pw.value = '';
    if (!r.ok) { mpkSay(msg, r.data.error || 'That password is not right.'); return; }
    form.hidden = true; btn.hidden = false;
    mpkSay(msg, 'Password confirmed. Now tap “Add a passkey on this device”.');
    pre = mpkPrefetch(path);
  });
  load();
})();
`
