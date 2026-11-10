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
//     arrived, and otherwise says the browser blocks embedded previews.
//
// Every other request on p-<id> needs that cookie. 🔴 A preview that does not
// exist is 404 BEFORE anything else: an offer the owner never clicked Open on
// has no preview, so its would-be host serves nothing and opens no stream.
//
// ⚠ NOT HERE YET (G3b): the reverse proxy behind the cookie check (a
// placeholder answers instead), the Sec-Fetch-Site / Origin same-site
// defences, the cookie-bomb page and the lifecycle timers.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"
	"time"
)

const (
	// EntryCodeTTL is how long a gateway-minted entry code may be spent.
	EntryCodeTTL = 60 * time.Second
	// maxEntryCodes bounds the unspent entry codes held in memory. Codes are
	// minted only for a valid page token, and expired ones are swept on every
	// mint; a mint past the bound is refused rather than growing the map.
	maxEntryCodes = 4096
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
	for h, c := range g.codes {
		if !now.Before(c.expires) {
			delete(g.codes, h)
		}
	}
	if len(g.codes) >= maxEntryCodes {
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
// signed for its owner and unexpired.
func (g *Gateway) ownerAdmitted(r *http.Request, id string, owner int64) bool {
	for _, c := range r.Cookies() {
		if c.Name == OwnerCookie && g.sign.checkOwnerCookie(c.Value, id, owner, g.clock.Now()) {
			return true
		}
	}
	return false
}

// startPath is where /__muster/check sends an admitted owner: the preview's
// own start path, or "/". A path that a browser would read as another host
// ("//x", "/\x") is "/".
func startPath(p string) string {
	if p == "" || !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.HasPrefix(p, `/\`) {
		return "/"
	}
	return p
}

// previewHandler serves p-<id>.<zone>.
func (g *Gateway) previewHandler(id string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.gatewayPageHeaders(w)
		owner, path, ok := g.previewOwner(id)
		if !ok {
			// No such preview (never opened, closed, or its owner no longer
			// served): nothing here, and nothing is dialled.
			http.NotFound(w, r)
			return
		}
		switch r.URL.Path {
		case enterPath:
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			who, ok := g.spendEntryCode(r.URL.Query().Get("g"), id)
			if !ok {
				g.renderNotice(w, http.StatusForbidden, "This link has been used or has expired",
					"Open the preview again from its session page in muster.")
				return
			}
			v, err := g.sign.mintOwnerCookie(id, who, g.clock.Now())
			if err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			http.SetCookie(w, ownerCookieHeader(v, r.Header.Get("Sec-Fetch-Dest")))
			http.Redirect(w, r, checkPath, http.StatusSeeOther)
			return
		case checkPath:
			if !g.ownerAdmitted(r, id, owner) {
				g.renderNotice(w, http.StatusForbidden, "This browser blocks embedded previews",
					"Open it in a new tab from the session page in muster.")
				return
			}
			http.Redirect(w, r, startPath(path), http.StatusSeeOther)
			return
		}
		if !g.ownerAdmitted(r, id, owner) {
			g.renderNotice(w, http.StatusForbidden, "Session expired",
				"Open the preview again from muster.")
			return
		}
		// ⚠ PLACEHOLDER UNTIL G3b: the reverse proxy to the host's port goes
		// here. Nothing is dialled yet.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = w.Write([]byte("preview admitted; the proxy is not built yet\n"))
	})
}
