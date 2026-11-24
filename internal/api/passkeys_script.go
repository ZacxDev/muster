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
		"If a password manager opened instead, choose its option to use your browser, a phone or a security key."
	// PasskeyDeviceFailedNote: any other error from the browser.
	PasskeyDeviceFailedNote = "This device could not use a passkey. Try again, or sign in with your password."
)

// The sign-in page's words for where a passkey sign-in has got to, in
// #passkey-signin-status (a polite live region): the browser's passkey sheet
// is open, muster is checking what it returned, and the sign-in is done and
// the app is loading. Without them the page sat unchanged while the check
// ran and the next page loaded, and read as stuck.
const (
	PasskeyWaitingNote   = "Waiting for your passkey\u2026"
	PasskeyVerifyingNote = "Checking your passkey\u2026"
	PasskeySigningInNote = "Signed in. Opening muster\u2026"
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
//     with the page's ONE prefetched challenge, which the buttons share: the
//     edge rate-limits /login and the passkey routes together with the
//     password POST, so a view makes one options request, not one per
//     ceremony. Sharing is safe because a challenge is spent only by a
//     verify, and only one ceremony ever verifies it (a button aborts the
//     autofill request before it starts; a verify takes the page's lock). The browser then lists the account's
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
//     WebAuthn). A new challenge is fetched only once a verify has spent the
//     old one or it is older than mpkMaxAge; a sheet dismissed before any
//     verify leaves it for the next ceremony.
//   - 🔴 SO DOES THE PASSWORD FORM'S SUBMIT: it aborts the pending autofill
//     request before the form posts, and none is started again on that page.
//   - The phone button is the same request with hints: ['hybrid'] (WebAuthn
//     Level 3; Chrome 128+), asking the browser to go straight to "use a phone
//     or tablet" (the QR code). ⚠ A password-manager extension that
//     intercepts navigator.credentials.get may ignore hints and show its own
//     window anyway; PasskeyNotUsedNote tells the person to pick its option
//     for the browser, a phone or a security key (managers label it
//     differently, so the note names no label).
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
  var waiting = ` + jsStr(PasskeyWaitingNote) + `, verifying = ` + jsStr(PasskeyVerifyingNote) + `, signingIn = ` + jsStr(PasskeySigningInNote) + `;
  var pre = mpkPrefetch(path), cond = null, busy = false, left = false;
  function lock(on) { busy = on; btn.disabled = on; if (phone) { phone.disabled = on; } }
  // toBuf is a COPY of options with its binary fields decoded: autofill and
  // the buttons may use one prefetched response, so it is never mutated.
  function toBuf(src) {
    var pk = JSON.parse(JSON.stringify(src));
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
    mpkSay(msg, verifying);
    var v = await mpkPost('` + passkeySignInVerifyPath + `', { challenge_id: o.data.challenge_id, credential: mpkCredJSON(cred), next: next });
    if (v.ok) { left = true; mpkSay(msg, signingIn); location.assign(v.data.redirect || '/'); return true; }
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
    if (!avail || cond || busy || left) { return; }
    var ac = new AbortController(), c = { ac: ac, done: null };
    cond = c;
    c.done = (async function () {
      try {
        // The prefetched challenge, shared with the buttons: a button aborts
        // this request before it starts its own, so only one ever verifies it.
        if (!mpkFresh(pre)) { pre = mpkPrefetch(path); }
        var p = pre, o = await p.promise;
        if (ac.signal.aborted) { return; }
        if (!o.ok) { if (pre === p) { pre = null; } return; }
        var rpId = o.data.options.rpId;
        var cred = await navigator.credentials.get({ mediation: 'conditional', publicKey: toBuf(o.data.options), signal: ac.signal });
        if (cond !== c || busy) { return; }
        cond = null; lock(true); mpkSay(msg, '');
        if (pre === p) { pre = null; } // the verify below spends it
        try {
          await finish(o, rpId, cred);
        } finally {
          // Signed in: the buttons stay locked while the next page loads.
          if (!left) { lock(false); }
          if (!left && !pre) { pre = mpkPrefetch(path); } // for the button
        }
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
      if (!o.ok) { pre = null; mpkSay(msg, o.data.error || 'Passkey sign-in is unavailable.'); return; }
      var rpId = o.data.options.rpId, pk = toBuf(o.data.options);
      if (hints) { pk.hints = hints; }
      if (c && c.done) { await Promise.race([c.done, new Promise(function (r) { setTimeout(r, 300); })]); }
      mpkSay(msg, waiting);
      var cred = await navigator.credentials.get({ publicKey: pk });
      // Only a verify spends the challenge: a sheet dismissed before this
      // point leaves it for the next ceremony (no new options request).
      pre = null;
      await finish(o, rpId, cred);
    } catch (e) {
      mpkSay(msg, mpkCancelled(e) ? notUsed : failed);
    } finally {
      // Signed in: the buttons stay locked while the next page loads.
      if (!left) {
        lock(false);
        if (!mpkFresh(pre)) { pre = mpkPrefetch(path); }
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
    pwForm.addEventListener('submit', function (e) {
      if (e.defaultPrevented) { return; }
      var c = cond;
      cond = null; left = true;
      if (c) { c.ac.abort(); }
      // The form is signing in (signInProgressJS): no passkey ceremony now.
      lock(true);
    });
  }
  // Back to a page that was restored from the back/forward cache after a
  // sign-in left it: it comes back locked, saying it is signing in.
  window.addEventListener('pageshow', function (e) {
    if (!e.persisted || !left) { return; }
    left = false; lock(false); mpkSay(msg, '');
    if (!mpkFresh(pre)) { pre = mpkPrefetch(path); }
    startAutofill();
  });
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
//
// When the server answers proof_required (passkey_enrol.go: the account
// already has a passkey, or a paired phone), the proof choices are shown
// instead: a passkey the account has — its prompt called IN the tap, with
// options fetched when the button is reached for (pointerdown) — or the
// paired phone, after which the page asks again every few seconds until the
// phone's Confirm lands. Either way the person then taps Add again: creating
// the passkey needs its own gesture.
const passkeyAddJS = ui.PasskeyPrefetchJS + `
(function () {
  var btn = document.getElementById('passkey-add'), msg = document.getElementById('passkey-status'),
      form = document.getElementById('passkey-reauth'), pw = document.getElementById('passkey-password'),
      proof = document.getElementById('passkey-proof'), byKey = document.getElementById('passkey-proof-passkey'),
      byPhone = document.getElementById('passkey-proof-phone');
  if (!btn) { return; }
  if (!mpkSupported()) { btn.disabled = true; mpkSay(msg, 'This browser cannot create passkeys.'); return; }
  var path = '` + accountPasskeysPath + `/options', pre = null, proofPre = null, polling = null;
  // ?confirm=1: the page was opened to confirm a removal (passkey_removal.go),
  // not to add a passkey: the proof is the point, and success says so.
  var confirming = new URLSearchParams(location.search).get('confirm') === '1';
  var confirmedText = 'Confirmed. For the next ten minutes you can remove a phone or a notification target on Account, or add one passkey here.';
  function needReauth(r) { return r.status === 401 && r.data.error === '` + PasskeyReauthRequired + `'; }
  function needProof(r) { return r.status === 403 && r.data.code === '` + PasskeyProofRequired + `'; }
  function showProof(r) {
    btn.hidden = true; proof.hidden = false;
    var p = r.data.proof || {};
    byKey.hidden = !p.passkey; byPhone.hidden = !p.phone;
  }
  function ready(text) {
    proof.hidden = true; btn.hidden = false; mpkSay(msg, text);
  }
  async function load() {
    pre = mpkPrefetch(path);
    var o = await pre.promise;
    if (needReauth(o)) { pre = null; btn.hidden = true; form.hidden = false; }
    else if (needProof(o)) { pre = null; showProof(o); }
    else if (confirming) { mpkSay(msg, 'Nothing to confirm: this account can remove a phone or a notification target as it is.'); }
  }
  btn.addEventListener('click', async function () {
    btn.disabled = true; mpkSay(msg, '');
    try {
      if (!mpkFresh(pre)) { pre = mpkPrefetch(path); }
      var o = await pre.promise;
      pre = null;
      if (needReauth(o)) { btn.hidden = true; form.hidden = false; pw.focus(); return; }
      if (needProof(o)) { showProof(o); return; }
      if (!o.ok) { mpkSay(msg, o.data.error || 'A passkey cannot be added right now.'); return; }
      var pk = o.data.options;
      pk.challenge = mpkB64ToBuf(pk.challenge);
      pk.user.id = mpkB64ToBuf(pk.user.id);
      (pk.excludeCredentials || []).forEach(function (c) { c.id = mpkB64ToBuf(c.id); });
      var cred = await navigator.credentials.create({ publicKey: pk });
      var v = await mpkPost('` + accountPasskeysPath + `/verify', { challenge_id: o.data.challenge_id, credential: mpkCredJSON(cred) });
      if (needReauth(v)) { btn.hidden = true; form.hidden = false; pw.focus(); return; }
      if (needProof(v)) { showProof(v); return; }
      if (!v.ok) { mpkSay(msg, v.data.error || 'That passkey could not be added.'); return; }
      location.assign('` + accountPasskeysPath + `');
    } catch (e) {
      mpkSay(msg, mpkCancelled(e) ? 'Adding a passkey was cancelled.' : 'This device could not create a passkey.');
    } finally {
      btn.disabled = false;
      if (!pre && !btn.hidden) { pre = mpkPrefetch(path); }
    }
  });
  // THE PASSKEY PROOF: options fetched when the button is reached for, the
  // prompt called in the tap itself.
  function fetchProof() {
    var p = proofPre = mpkPrefetch('` + accountPasskeysPath + `/proof/options');
    p.promise.then(function (o) { p.answer = o; }, function () { p.failed = true; });
  }
  byKey.addEventListener('pointerdown', function () { if (!proofPre || !mpkFresh(proofPre) || proofPre.failed) fetchProof(); });
  byKey.addEventListener('click', function () {
    var p = proofPre;
    if (!p || !mpkFresh(p) || p.failed) { fetchProof(); mpkSay(msg, 'Getting ready. Tap again.'); return; }
    if (!p.answer) { mpkSay(msg, 'Getting ready. Tap again.'); return; }
    proofPre = null;
    var o = p.answer;
    if (!o.ok || !o.data.available) { mpkSay(msg, (o.data && o.data.error) || 'No passkey of this account can be used here.'); return; }
    byKey.disabled = true;
    var ask;
    try { ask = navigator.credentials.get({ publicKey: mpkRequestOptions(o.data.options) }); }
    catch (e) { byKey.disabled = false; mpkSay(msg, 'The passkey could not be used.'); return; }
    ask.then(function (cred) {
      return mpkPost('` + accountPasskeysPath + `/proof/verify', { challenge_id: o.data.challenge_id, credential: mpkCredJSON(cred) }).then(function (v) {
        byKey.disabled = false;
        if (!v.ok) { mpkSay(msg, v.data.error || 'That passkey could not confirm it.'); return; }
        pre = mpkPrefetch(path);
        ready(confirming ? confirmedText : 'Confirmed. Now tap “Add a passkey on this device”.');
      });
    }, function (e) {
      byKey.disabled = false;
      mpkSay(msg, mpkCancelled(e) ? 'The passkey was cancelled.' : 'The passkey could not be used.');
    });
  });
  // THE PHONE PROOF: a card on the paired phone; then ask again until its
  // Confirm lands (the options stop answering proof_required).
  byPhone.addEventListener('click', async function () {
    byPhone.disabled = true;
    var r = await mpkPost('` + accountPasskeysPath + `/proof/phone', {});
    byPhone.disabled = false;
    if (!r.ok) { mpkSay(msg, r.data.error || 'The confirmation could not be sent to your phone.'); return; }
    mpkSay(msg, 'Confirm “Add a passkey to your account?” on your phone. This page waits for it.');
    if (polling) { clearInterval(polling); }
    var tries = 0;
    polling = setInterval(async function () {
      if (++tries > 200) { clearInterval(polling); polling = null; mpkSay(msg, 'No confirmation came from your phone. Try again.'); return; }
      var st = await mpkGet('` + accountPasskeysPath + `/proof/status');
      if (st.ok && st.data.ticket) {
        clearInterval(polling); polling = null; pre = mpkPrefetch(path);
        ready(confirming ? confirmedText : 'Confirmed on your phone. Now tap “Add a passkey on this device”.');
      }
    }, 3000);
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
  async function mpkGet(u) {
    var res = await fetch(u, { credentials: 'same-origin', headers: { 'Accept': 'application/json' } });
    var d = {};
    try { d = await res.json(); } catch (e) {}
    return { ok: res.ok, data: d };
  }
  load();
})();
`
