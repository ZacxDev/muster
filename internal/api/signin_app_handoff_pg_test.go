package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/signinreq"
)

// newHandoffRig is newAppRig with the Android app configured as pkg
// (MUSTER_ANDROID_PACKAGE; "" for none).
func newHandoffRig(t *testing.T, pkg string) *appRig {
	t.Helper()
	r := newDeviceRig(t, func(ext *Extensions, pool *pgxpool.Pool) {
		ext.SignInRequests = signinreq.NewPG(pool)
		ext.AndroidPackage = pkg
	})
	t.Cleanup(func() {
		_, _ = r.pool.Exec(authz.System(context.Background()), `DELETE FROM signin_requests WHERE approved_by = $1`, r.uid)
	})
	return &appRig{deviceRig: r, prefixed: true, h: r.edge}
}

// TestAppSignInPhonePageOffersTheAndroidApp: with the app configured, the page
// the QR opens carries the hand-off to it, hidden until its script shows it
// (on Android, outside the app), and the script builds the intent link the
// app's side of the contract expects. The template is pinned LITERALLY: the
// configured package, the <package>.QR_CODE string extra that carries the
// code, and a fallback with no code in it. What the script does in a browser
// (which User-Agents see it, the href it writes) is qr-signin.spec.ts's.
func TestAppSignInPhonePageOffersTheAndroidApp(t *testing.T) {
	if AppSignInOpenInApp != "Open in the muster app" || AppSignInContinueHere != "Continue in this browser" {
		t.Fatalf("the hand-off's words: %q / %q", AppSignInOpenInApp, AppSignInContinueHere)
	}
	a := newHandoffRig(t, "org.example.musterapp")
	w := a.do("GET", AppSignInPhonePath, nil, jar{})
	body := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d", AppSignInPhonePath, w.Code)
	}
	for _, want := range []string{
		// Hidden by default: a desktop, or a page whose script never ran, shows
		// the ordinary form. The package is the configured one.
		`<div id="qr-signin-handoff" class="space-y-2" data-package="org.example.musterapp" hidden>`,
		`<p class="text-sm text-fg2">Approve this sign-in in the muster app. Without the app, continue in this browser.</p>`,
		`<a id="qr-signin-open-app" href="#" rel="noreferrer" class="press inline-flex min-h-[44px] w-full items-center justify-center rounded-md bg-accent px-3 py-2 text-base font-semibold text-on-accent transition hover:bg-accent/90">Open in the muster app</a>`,
		`pkg = box ? box.getAttribute('data-package') || '' : '';`,
		// Only with the app configured, on Android, and not inside the app (its
		// WebView's bridge).
		`if (pkg && /Android/i.test(navigator.userAgent) && !window.musterApp && SHAPE.test(t.value)) {`,
		// The contract with the Android app.
		`open.setAttribute('href', 'intent://' + host + '/auth/qr#Intent;scheme=https;package=' + pkg + ';S.' + pkg + '.QR_CODE=' + encodeURIComponent(code) + ';S.browser_fallback_url=' + encodeURIComponent('https://' + host + '/auth/qr') + ';end');`,
		// The in-browser approval stays, beneath it.
		`here.textContent = "Continue in this browser";`,
		// Back from the fallback, the carried code goes on in the browser.
		`if (SHAPE.test(carried)) { t.value = carried; if (wrap) { wrap.style.display = 'none'; } f.submit(); }`,
		`KEY = 'muster.signin-app.code'`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("the phone page lacks %s\n%s", want, body)
		}
	}
	// The bootstrap no longer posts unconditionally: the page's script decides.
	if strings.Contains(body, `if (t.value) { f.submit(); }`) {
		t.Fatal("the fragment form still auto-submits before the hand-off is offered")
	}
}

// TestAppSignInPhonePageWithNoAppConfiguredOffersNoHandoff: with no
// MUSTER_ANDROID_PACKAGE there is no app to name, so the page carries no
// hand-off and its script posts the code as before.
func TestAppSignInPhonePageWithNoAppConfiguredOffersNoHandoff(t *testing.T) {
	a := newHandoffRig(t, "")
	w := a.do("GET", AppSignInPhonePath, nil, jar{})
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, `name="step" value="open"`) {
		t.Fatalf("GET %s: %d", AppSignInPhonePath, w.Code)
	}
	for _, absent := range []string{`id="qr-signin-handoff"`, `id="qr-signin-open-app"`, AppSignInOpenInApp, `data-package=`} {
		if strings.Contains(body, absent) {
			t.Fatalf("with no app configured the page still carries %s", absent)
		}
	}
}
