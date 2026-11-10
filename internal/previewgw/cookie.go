package previewgw

// cookie.go — what the gateway signs with PREVIEW_SIGNING_KEY: the owner
// cookie on a preview's host (__Host-mp) and the session page's page token.
// Both are HMAC-SHA256 over a small JSON payload, with a KIND mixed into the
// MAC, so a page token is never accepted as a cookie or the reverse.
//
// 🔴 THE OWNER COOKIE IS NEVER RENEWED BY ITSELF. OwnerCookieTTL from the
// entry code it was minted for, then the owner goes back through muster's chip
// (the operator's decision: 8 h, renewed only by a fresh hand-off). Nothing
// here re-issues it on use.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

const (
	// OwnerCookie is the owner cookie's name. The __Host- prefix makes the
	// browser require Secure, Path=/ and no Domain, so a cookie another preview
	// tosses at the zone cannot overwrite or shadow it.
	OwnerCookie = "__Host-mp"
	// OwnerCookieTTL is how long an owner cookie admits its owner.
	OwnerCookieTTL = 8 * time.Hour
	// PageTokenTTL is how long a session page's token works; a reload or an
	// expired token means clicking muster's chip again.
	PageTokenTTL = 15 * time.Minute
	// MinSigningKey is PREVIEW_SIGNING_KEY's minimum length, in bytes.
	MinSigningKey = 32
)

// MAC kinds: domain separation between the two things the key signs.
const (
	kindOwnerCookie = "owner-cookie/1"
	kindPageToken   = "page-token/1"
)

type signer struct{ key []byte }

func (s signer) mac(kind string, payload []byte) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(kind))
	m.Write([]byte{0})
	m.Write(payload)
	return m.Sum(nil)
}

// seal is base64url(JSON(v)) "." base64url(MAC).
func (s signer) seal(kind string, v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString(b) + "." + enc.EncodeToString(s.mac(kind, b)), nil
}

// open checks tok's MAC for kind and decodes its payload into v.
func (s signer) open(kind, tok string, v any) bool {
	p, m, ok := strings.Cut(tok, ".")
	if !ok || len(tok) > 2048 {
		return false
	}
	enc := base64.RawURLEncoding
	b, err := enc.DecodeString(p)
	if err != nil {
		return false
	}
	got, err := enc.DecodeString(m)
	if err != nil || !hmac.Equal(got, s.mac(kind, b)) {
		return false
	}
	return json.Unmarshal(b, v) == nil
}

// ownerClaim is the owner cookie's payload: the preview it admits to, its
// owner, and when it stops (unix seconds).
type ownerClaim struct {
	Preview string `json:"p"`
	Owner   int64  `json:"o"`
	Expires int64  `json:"e"`
}

// mintOwnerCookie is the signed value of an owner cookie for (preview, owner),
// expiring OwnerCookieTTL after now.
func (s signer) mintOwnerCookie(preview string, owner int64, now time.Time) (string, error) {
	return s.seal(kindOwnerCookie, ownerClaim{Preview: preview, Owner: owner, Expires: now.Add(OwnerCookieTTL).Unix()})
}

// checkOwnerCookie reports whether v admits owner to preview at now: signed
// with this key as an owner cookie, naming exactly that preview and owner, and
// not yet expired (an expiry equal to now is expired).
func (s signer) checkOwnerCookie(v, preview string, owner int64, now time.Time) bool {
	var c ownerClaim
	if !s.open(kindOwnerCookie, v, &c) {
		return false
	}
	return c.Preview == preview && c.Owner == owner && now.Unix() < c.Expires
}

// ownerCookieHeader is the Set-Cookie for an owner cookie. Its SameSite
// follows how the preview is being opened (the design's "Previews are
// same-site with each other", part 2):
//
//   - Sec-Fetch-Dest: iframe (Show here, inside muster): SameSite=None;
//     Partitioned. Lax or Strict would never be sent inside a frame whose
//     ancestor is another site; partitioning keys it to muster's top-level
//     site, so a page a preview opens on its own never carries it.
//   - anything else (document: a new tab; or a browser that sends no
//     Fetch Metadata): SameSite=Strict, not partitioned.
func ownerCookieHeader(value, secFetchDest string) *http.Cookie {
	c := &http.Cookie{Name: OwnerCookie, Value: value, Path: "/", MaxAge: int(OwnerCookieTTL / time.Second),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode}
	if secFetchDest == "iframe" {
		c.SameSite = http.SameSiteNoneMode
		c.Partitioned = true
	}
	return c
}

// pageClaim is a page token's payload: who and which window the session page
// was entered for, and when the token stops (unix seconds).
type pageClaim struct {
	Owner   int64  `json:"o"`
	Host    string `json:"h"`
	Window  string `json:"w"`
	Session string `json:"s,omitempty"`
	Expires int64  `json:"e"`
	Nonce   string `json:"n"`
}

func (s signer) mintPageToken(c pageClaim, now time.Time) (string, error) {
	c.Expires = now.Add(PageTokenTTL).Unix()
	return s.seal(kindPageToken, c)
}

// checkPageToken is the claim tok carries, if it is a page token signed with
// this key and not yet expired.
func (s signer) checkPageToken(tok string, now time.Time) (pageClaim, bool) {
	var c pageClaim
	if !s.open(kindPageToken, tok, &c) || now.Unix() >= c.Expires || c.Owner <= 0 || c.Host == "" || c.Window == "" {
		return pageClaim{}, false
	}
	return c, true
}
