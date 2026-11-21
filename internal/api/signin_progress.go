package api

// signInProgressJS shows that a password sign-in is under way, on both sign-in
// pages (renderLoginPage, renderGoTrueLogin). Without it the page did nothing
// visible between the tap and the next page drawing: the form posts as a
// plain navigation, so the browser keeps the sign-in page on screen,
// unchanged, until the app's first page arrives — and a person reads an
// unchanged page as a broken one.
//
// On the form's submit it disables the Sign in button (a second tap or a
// second Enter cannot post again: implicit submission needs an enabled
// button), draws a spinner and "Signing in…" in it, marks the form
// aria-busy, and puts "Signing in…" in a polite live region screen readers
// announce. (The passkey buttons lock themselves on the same submit:
// passkeySignInJS.) It keeps that state until the page is replaced:
//
//   - a refused sign-in is a NEW page from the server (renderLoginPage /
//     renderGoTrueLogin with the problem), drawn with the form enabled, so
//     there is nothing to undo;
//   - a page restored from the back/forward cache comes back in the state it
//     left in, so pageshow with persisted puts the form back.
//
// Progressive enhancement: the form is an ordinary POST; with no script it
// signs in exactly as before, without the progress.
//
// Plain ES5 in one self-contained IIFE — no shared shell, no other script —
// for the independence reason at the top of login.go. Its own constant, so
// the sign-in markup it decorates stays as it was.
const signInProgressJS = `<script>
(function () {
  var pw = document.getElementById('muster-password');
  var form = pw && pw.form;
  var btn = form && form.querySelector('button[type=submit]');
  if (!btn) { return; }
  var label = btn.innerHTML;
  // The live region exists before anything is put in it, or a screen reader
  // may not announce the first message.
  var st = document.createElement('p');
  st.id = 'signin-status';
  st.className = 'sr-only';
  st.setAttribute('role', 'status');
  st.setAttribute('aria-live', 'polite');
  btn.insertAdjacentElement('afterend', st);
  function restore() {
    btn.disabled = false;
    btn.innerHTML = label;
    form.removeAttribute('aria-busy');
    st.textContent = '';
  }
  form.addEventListener('submit', function (e) {
    if (e.defaultPrevented) { return; }
    btn.disabled = true;
    btn.innerHTML = '<span class="inline-flex items-center justify-center gap-2">' +
      '<svg class="h-5 w-5 shrink-0 animate-spin" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><path d="M21 12a9 9 0 1 1-6.2-8.6"/></svg>' +
      '<span>Signing in…</span></span>';
    form.setAttribute('aria-busy', 'true');
    st.textContent = 'Signing in…';
  });
  window.addEventListener('pageshow', function (e) { if (e.persisted) { restore(); } });
})();
</script>`
