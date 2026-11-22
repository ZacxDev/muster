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
// the QR opens carries the hand-off to it, naming the configured package and
// hidden until its script shows it. What the script does in a browser (which
// User-Agents see the hand-off, the intent link it writes, the fallback) is
// qr-signin.spec.ts's, which pins the generated href literally.
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
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("the phone page lacks %s\n%s", want, body)
		}
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
