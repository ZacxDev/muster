package previewgw

// auth.go — the owner's way onto a preview's host, p-<id>.<zone>:
//
//  1. The session page (session.go) asks, with its page token, to open a
//     preview; the gateway mints an ENTRY CODE: random, 60 s, single use,
//     bound to that preview and owner, kept only in memory (hashed).
//  2. The browser goes to p-<id>.<zone>/__muster/enter?g=<code>. The code is
//     spent (whatever the outcome), and the owner cookie __Host-mp is set,
//     bound to that preview and owner for OwnerCookieTTL, with a SameSite
//     chosen from Sec-Fetch-Dest (cookie.go). It redirects to /__muster/check.
//  3. /__muster/check goes on to the preview's start path if the cookie
//     arrived, and otherwise says the browser blocks embedded previews. It
//     goes on with a page whose script navigates (location.replace), not a
//     redirect: a redirect would keep the app's request in w.'s same-site
//     chain, and an iframe navigation that is not same-origin passes only on
//     check itself (samesite.go). The page's own navigation is same-origin.
//
// Every other request on p-<id> needs that cookie. 🔴 A preview that does not
// exist is 404 BEFORE anything else: an offer the owner never clicked Open on
// has no preview, so its would-be host serves nothing and opens no stream.
//
// Behind the cookie: the same-site defences (samesite.go) and the reverse
// proxy (proxy.go).

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"path"
	"strings"
	"time"

	gm "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

const (
	// EntryCodeTTL is how long a gateway-minted entry code may be spent.
	EntryCodeTTL = 60 * time.Second
	// maxEntryCodes bounds the unspent entry codes held in memory. Codes are
	// minted only for a valid page token, and expired ones are swept on every
	// mint; a mint past the bound is refused rather than growing the map.
	maxEntryCodes = 4096
	// maxEntryCodesPerOwner bounds one owner's unspent entry codes, so one
	// owner minting codes cannot fill maxEntryCodes and refuse everyone else.
	maxEntryCodesPerOwner = 64
)

// Paths the gateway answers itself on p- and w. hosts.
const (
	enterPath = "/__muster/enter"
	checkPath = "/__muster/check"
)

type entryCode struct {
	preview string
	owner   int64
	expires time.Time
}

func codeHash(code string) string {
	s := sha256.Sum256([]byte(code))
	return hex.EncodeToString(s[:])
}

// mintEntryCode records a one-time code for (preview, owner) and returns it.
func (g *Gateway) mintEntryCode(preview string, owner int64) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	code := base64.RawURLEncoding.EncodeToString(b)
	now := g.clock.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	mine := 0
	for h, c := range g.codes {
		if !now.Before(c.expires) {
			delete(g.codes, h)
			continue
		}
		if c.owner == owner {
			mine++
		}
	}
	if len(g.codes) >= maxEntryCodes || mine >= maxEntryCodesPerOwner {
		return "", errTooManyCodes
	}
	g.codes[codeHash(code)] = entryCode{preview: preview, owner: owner, expires: now.Add(EntryCodeTTL)}
	return code, nil
}

// spendEntryCode spends code (it is gone after this call, whatever it
// answers) and reports the owner it admits to preview: it must be unexpired,
// minted for exactly that preview, and the preview must still be open and
// that owner's, with the owner still served.
func (g *Gateway) spendEntryCode(code, preview string) (int64, bool) {
	if code == "" || len(code) > 128 {
		return 0, false
	}
	now := g.clock.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	h := codeHash(code)
	c, ok := g.codes[h]
	delete(g.codes, h)
	if !ok || !now.Before(c.expires) || c.preview != preview {
		return 0, false
	}
	p, ok := g.previews[preview]
	if !ok || p.OwnerID != c.owner || !g.liveOwnerLocked(c.owner) {
		return 0, false
	}
	return c.owner, true
}

// previewIDOf is the preview id a Host names (p-<id>.<zone>), or "".
func (g *Gateway) previewIDOf(host string) string {
	label, ok := strings.CutSuffix(host, "."+g.cfg.Domain)
	if !ok {
		return ""
	}
	id, ok := strings.CutPrefix(label, "p-")
	if !ok || len(id) != 26 {
		return ""
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= '2' && c <= '7') {
			return ""
		}
	}
	return id
}

// previewOwner is the open preview id's owner and start path, if it is open
// and its owner is served.
func (g *Gateway) previewOwner(id string) (owner int64, path string, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	p, ok := g.previews[id]
	if !ok || !g.liveOwnerLocked(p.OwnerID) {
		return 0, "", false
	}
	return p.OwnerID, p.Path, true
}

// ownerAdmitted reports whether r carries an owner cookie for preview id,
// signed for its owner and unexpired, and when it expires.
func (g *Gateway) ownerAdmitted(r *http.Request, id string, owner int64) (time.Time, bool) {
	now := g.clock.Now()
	for _, c := range r.Cookies() {
		if c.Name != OwnerCookie {
			continue
		}
		if exp, ok := g.sign.checkOwnerCookie(c.Value, id, owner, now); ok {
			return exp, true
		}
	}
	return time.Time{}, false
}

// startPath is where /__muster/check sends an admitted owner: the preview's
// own start path, or "/". It holds the rule the host enforces on preview_open
// (internal/previewhost checkOpenArgs): a single leading "/", then printable
// ASCII without spaces or backslashes; anything else is "/". Without it a path
// could reach the browser as "//x", another host: http.Redirect passes a path
// it cannot parse ("/\t/x") through as is and the browser drops the tab, and it
// path.Cleans one it can ("/./\x" becomes "/\x") and the browser reads "\" as
// "/". The final check, that path.Clean does not start the path with "//", is
// defence in depth: path.Clean collapses repeated slashes, so with the
// character rule in front it does not fire today.
func startPath(p string) string {
	if p == "" || !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") ||
		strings.ContainsFunc(p, func(c rune) bool { return c <= ' ' || c >= 0x7f || c == '\\' }) {
		return "/"
	}
	clean := path.Clean(p)
	if strings.HasPrefix(clean, "//") {
		return "/"
	}
	return p
}

// previewHandler serves p-<id>.<zone>, in this order: 404 for a preview
// that is not open (nothing is dialled); the entry route (its one-time code
// is its credential, so neither check below applies); the cookie-bomb page;
// the same-site check (samesite.go); the owner cookie; then the proxy.
func (g *Gateway) previewHandler(id string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		owner, path, ok := g.previewOwner(id)
		if !ok {
			// No such preview (never opened, closed, or its owner no longer
			// served): nothing here, and nothing is dialled.
			g.gatewayPageHeaders(w)
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == enterPath {
			g.enterPreview(w, r, id)
			return
		}
		if cookieLen(r) > MaxCookieHeader {
			g.cookieBomb(w, r)
			return
		}
		if code := crossSiteRefusal(r, g.previewOrigin(id)); code != "" {
			g.renderCode(w, http.StatusForbidden, code)
			return
		}
		exp, admitted := g.ownerAdmitted(r, id, owner)
		if r.URL.Path == checkPath {
			if !admitted {
				g.renderCode(w, http.StatusForbidden, codeFrameBlocked)
				return
			}
			g.continueTo(w, startPath(path))
			return
		}
		if !admitted {
			g.renderCode(w, http.StatusForbidden, codeSessionExpired)
			return
		}
		g.serveAdmitted(w, r, id, exp)
	})
}

// enterPreview spends an entry code and sets the owner cookie.
func (g *Gateway) enterPreview(w http.ResponseWriter, r *http.Request, id string) {
	g.gatewayPageHeaders(w)
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	who, ok := g.spendEntryCode(r.URL.Query().Get("g"), id)
	if !ok {
		g.renderCode(w, http.StatusForbidden, codeEntryUsed)
		return
	}
	v, err := g.sign.mintOwnerCookie(id, who, g.clock.Now())
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, ownerCookieHeader(v, r.Header.Get("Sec-Fetch-Dest")))
	http.Redirect(w, r, checkPath, http.StatusSeeOther)
}

// continueTo is /__muster/check's answer for an admitted owner: a page whose
// script (under the page's nonce) replaces it with target, a path on this
// preview's host (startPath), plus a link for a browser without script. The
// navigation is the page's own, so the request reaching the app is
// same-origin.
func (g *Gateway) continueTo(w http.ResponseWriter, target string) {
	js, _ := json.Marshal(target) // escapes <, > and &: it cannot end the element
	nonce := newNonce()
	g.gatewayPageHeaders(w, nonce)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = page(nonce, "Opening the preview",
		P(A(Href(target), gm.Text("Continue to the preview"))),
		Script(gm.Attr("nonce", nonce), gm.Raw("location.replace("+string(js)+");")),
	).Render(w)
}
