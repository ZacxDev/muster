package api

import (
	"fmt"
	"html"
	"image/color"
	"strings"

	"github.com/boombuler/barcode/qr"
)

// The sign-in page's half of "Sign in with the muster app" (signin_app.go):
// the button, the QR panel and its script. Kept apart from login.go and
// login_gotrue.go so the sign-in pages carry ONE insertion point each
// (appSignInHTML), whatever else changes in them.

// The status lines the page shows, named so the tests pin them whole. The
// script carries the same strings (appSignInJS reads them from here).
const (
	AppSignInWaiting  = "Scan this code with your phone's camera. Your phone must be signed in to muster."
	AppSignInScanned  = "Scanned. Approve the sign-in on your phone."
	AppSignInRedeem   = "Approved. Signing you in…"
	AppSignInDeniedJS = "The sign-in was denied on your phone."
	AppSignInExpired  = "This code has expired."
)

// appSignInHTML is the option on the sign-in page, or "" when the server
// does not offer it. next is where a successful sign-in goes (the form's own
// next, sanitised again by the redeem).
func (s *Server) appSignInHTML(next string) string {
	if !s.appSignInServed() {
		return ""
	}
	return `<section id="qr-signin" class="mt-4 flex flex-col gap-2" data-next="` + html.EscapeString(safeReturnTo(next)) + `">
<button type="button" id="qr-signin-start" class="press min-h-[44px] w-full rounded-md bg-bg px-3 py-2 text-base font-semibold text-fg ring-1 ring-inset ring-edge">Sign in with the muster app</button>
<div id="qr-signin-panel" class="flex flex-col items-center gap-2 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line" hidden>
<div id="qr-signin-code" class="self-center" data-state="" data-qr-url=""></div>
<p class="text-center text-sm text-fg2" id="qr-signin-status" role="status" aria-live="polite"></p>
<p class="text-center text-xs text-muted">No camera? Open this link on your phone:</p>
<a id="qr-signin-link" href="#" class="break-all text-center text-xs text-accent underline" rel="noreferrer"></a>
<button type="button" id="qr-signin-new" class="press min-h-[44px] w-full rounded-md bg-accent px-3 py-2 text-base font-semibold text-on-accent transition hover:bg-accent/90" hidden>Get a new code</button>
</div>
</section>
<script>` + appSignInJS() + `</script>`
}

// appSignInJS starts a request, draws its QR, polls its state (with the
// server's interval, backing off on a 429 or an error, RFC 8628's slow_down)
// and redeems an approval. A generation counter drops the answers of a code
// that "Get a new code" replaced.
func appSignInJS() string {
	js := func(s string) string { return fmt.Sprintf("%q", s) }
	return `(function () {
  var root = document.getElementById('qr-signin');
  if (!root || !window.fetch) { return; }
  var start = document.getElementById('qr-signin-start'), panel = document.getElementById('qr-signin-panel'),
      box = document.getElementById('qr-signin-code'), status = document.getElementById('qr-signin-status'),
      link = document.getElementById('qr-signin-link'), again = document.getElementById('qr-signin-new');
  var next = root.getAttribute('data-next') || '/';
  var code = '', timer = 0, delay = 2000, gen = 0;
  var WAITING = ` + js(AppSignInWaiting) + `, SCANNED = ` + js(AppSignInScanned) + `, REDEEM = ` + js(AppSignInRedeem) + `,
      DENIED = ` + js(AppSignInDeniedJS) + `, EXPIRED = ` + js(AppSignInExpired) + `;
  function say(t) { status.textContent = t; }
  function stop() { if (timer) { clearTimeout(timer); timer = 0; } }
  function end(state, t) { stop(); box.setAttribute('data-state', state); say(t); again.hidden = false; }
  function json(r) { return r.json().then(function (j) { return { status: r.status, ok: r.ok, j: j || {} }; }, function () { return { status: r.status, ok: false, j: {} }; }); }
  function backoff(res) {
    var floor = (res && res.j && res.j.interval ? res.j.interval : 0) * 1000;
    delay = Math.min(Math.max(delay * 2, floor), 10000);
  }
  function begin() {
    stop(); gen++; var mine = gen;
    start.hidden = true; panel.hidden = false; again.hidden = true;
    box.innerHTML = ''; box.setAttribute('data-state', 'starting'); box.setAttribute('data-qr-url', '');
    link.textContent = ''; link.setAttribute('href', '#');
    say('Getting a code…');
    fetch('/login/app', { method: 'POST', credentials: 'same-origin', headers: { 'Accept': 'application/json' } }).then(json).then(function (res) {
      if (mine !== gen) { return; }
      if (!res.ok) { end('error', res.j.error || 'Could not get a code. Try again.'); return; }
      code = res.j.code;
      box.innerHTML = res.j.svg;
      box.setAttribute('data-qr-url', res.j.url);
      box.setAttribute('data-state', 'pending');
      link.setAttribute('href', res.j.url); link.textContent = res.j.url;
      delay = (res.j.interval || 2) * 1000;
      say(WAITING);
      poll(mine);
    }, function () { if (mine === gen) { end('error', 'Could not get a code. Check your connection and try again.'); } });
  }
  function poll(mine) {
    timer = setTimeout(function () {
      timer = 0;
      fetch('/login/app/status', { credentials: 'same-origin', headers: { 'Accept': 'application/json' } }).then(json).then(function (res) {
        if (mine !== gen) { return; }
        if (!res.ok) { backoff(res); poll(mine); return; }
        var st = res.j.state;
        if (res.j.interval) { delay = res.j.interval * 1000; }
        box.setAttribute('data-state', st);
        if (st === 'pending') { say(WAITING); poll(mine); }
        else if (st === 'scanned') { say(SCANNED); poll(mine); }
        else if (st === 'approved') { say(REDEEM); redeem(mine); }
        else if (st === 'denied') { end('denied', DENIED); }
        else { end('expired', EXPIRED); }
      }, function () { if (mine === gen) { backoff(null); poll(mine); } });
    }, delay);
  }
  function redeem(mine) {
    var body = new URLSearchParams();
    body.set('code', code); body.set('next', next);
    fetch('/login/app/redeem', { method: 'POST', credentials: 'same-origin', headers: { 'Accept': 'application/json' }, body: body }).then(json).then(function (res) {
      if (mine !== gen) { return; }
      if (res.ok) { box.setAttribute('data-state', 'redeemed'); location.assign(res.j.redirect || '/'); return; }
      if (res.j.state === 'pending') { poll(mine); return; }
      end(res.j.state || 'error', res.j.error || 'Could not sign you in. Get a new code and try again.');
    }, function () { if (mine === gen) { end('error', 'Could not sign you in. Check your connection and try again.'); } });
  }
  start.addEventListener('click', begin);
  again.addEventListener('click', begin);
})();`
}

// qrSize is the QR's drawn size in CSS pixels.
const qrSize = 232

// qrSVG draws text as a QR code (error correction M) in an SVG: one path of
// dark modules on a white square with the standard four-module quiet zone.
// White whatever the theme: a camera needs the contrast, and some readers
// refuse an inverted code.
func qrSVG(text string) (string, error) {
	code, err := qr.Encode(text, qr.M, qr.Auto)
	if err != nil {
		return "", err
	}
	n := code.Bounds().Dx()
	const quiet = 4
	dark := func(x, y int) bool {
		r, g, b, _ := code.At(x, y).RGBA()
		return color.GrayModel.Convert(color.RGBA64{R: uint16(r), G: uint16(g), B: uint16(b), A: 0xffff}).(color.Gray).Y < 128
	}
	var d strings.Builder
	for y := 0; y < n; y++ {
		for x := 0; x < n; {
			if !dark(x, y) {
				x++
				continue
			}
			run := 1
			for x+run < n && dark(x+run, y) {
				run++
			}
			fmt.Fprintf(&d, "M%d %dh%dv1h-%dz", x+quiet, y+quiet, run, run)
			x += run
		}
	}
	side := n + 2*quiet
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d" role="img" aria-label="QR code to sign in with the muster app" shape-rendering="crispEdges">`+
		`<rect width="%d" height="%d" fill="#ffffff"/><path fill="#000000" d="%s"/></svg>`,
		side, side, qrSize, qrSize, side, side, d.String()), nil
}
