// Package pages is shareable pages (migration 0116; docs/pages.md, design
// docs/design/pages.md): a page an agent writes for its owner, its immutable
// versions, the share links that let someone without a muster account open it,
// and what those viewers send back.
//
// 🔴 THIS PACKAGE IMPORTS NOTHING OF internal/api OR internal/ui. It is the
// model (spec, validation, limits), the store and the renderer (the content
// document, the shell, the SDK). The routes, tools and the owner's approval
// live in internal/api as a thin layer over Store, so serving could move to a
// separate host later without touching this package (the design's "core, not
// a plugin").
//
// 🔴 PAGE CONTENT IS UNTRUSTED, WHOEVER RENDERS IT. Blocks are an agent's
// words and script; responses are a stranger's. The content document is
// served with ContentHeaders (an opaque origin by HTTP header, no network),
// and what muster itself draws from either is HTML-escaped.
package pages

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"regexp"
	"time"
)

// The limits (docs/design/pages.md#limits-and-quotas). Vars where a test
// lowers them.
const (
	// MaxVersionBytes bounds one version's blocks as JSON (0116's CHECK).
	MaxVersionBytes = 512 << 10
	// MaxBlocksInputBytes bounds the blocks as a REQUEST carries them, before
	// they are parsed (Parse): twice MaxVersionBytes, because the caller's
	// encoder is not Go's. The stored size is Go's re-encoding (Validate),
	// which writes non-ASCII as raw UTF-8 but escapes <, > and & as \u003c (6
	// bytes each), so JavaScript's JSON.stringify, which escapes less, is no
	// larger than the stored form for well-formed text. An encoder that writes
	// non-ASCII as \uXXXX (Python's json.dumps default) can be larger: at
	// worst 3x, a 2-byte UTF-8 letter as a 6-byte escape (measured: "é" is 2
	// bytes in Go's encoding and 6 in Python's; an emoji 4 and 12, a surrogate
	// pair; "<" 6 in Go's and 1 in Python's and JSON.stringify's). Base64
	// data: URIs are ASCII and the same size in every encoder, so a page of
	// inline screenshots is ~1x. Past this the blocks are refused unread with
	// the page's own error ("larger than 512 KiB"). Every MCP hop's body
	// bound is derived from it (mirror.MaxMCPRequestBytes).
	MaxBlocksInputBytes = 2 * MaxVersionBytes
	// MaxBlocks bounds a version's block count.
	MaxBlocks = 100
	// MaxHTMLBytes bounds one html block.
	MaxHTMLBytes = 256 << 10
	// MaxMarkdownBytes bounds one markdown block.
	MaxMarkdownBytes = 64 << 10
	// MaxSubmitBytes bounds a submit's data as JSON.
	MaxSubmitBytes = 32 << 10
	// MaxTitleRunes bounds a page's title (0116's CHECK).
	MaxTitleRunes = 200
)

// DefaultUploadBytesPerPage bounds the files one page may hold in all
// (MUSTER_PAGES_UPLOAD_BYTES overrides it): 500 MiB.
const DefaultUploadBytesPerPage int64 = 500 << 20

var (
	// MaxVersionsPerPage bounds a page's versions.
	MaxVersionsPerPage = 100
	// MaxPagesPerOwner bounds an owner's pages.
	MaxPagesPerOwner = 200
	// MaxOpenLinksPerPage bounds a page's pending and live links together.
	MaxOpenLinksPerPage = 10
	// MaxResponsesPerLink bounds what one link may record, uploads included.
	MaxResponsesPerLink = 1000
	// MaxUploadsPerLink bounds the files one link may record.
	MaxUploadsPerLink = 50
)

// The errors a caller turns into words. Every "not yours" reads as
// ErrNotFound, exactly as a missing id.
var (
	ErrNotFound = errors.New("pages: no such page")
	// ErrLinkNotFound is a link that is unknown, not live, or expired: one
	// answer for all of them.
	ErrLinkNotFound = errors.New("pages: no such link")
	ErrInvalid      = errors.New("pages: invalid")
	// ErrTooMany is a cap reached (pages, versions, links, responses).
	ErrTooMany = errors.New("pages: too many")
	// ErrNotPending is a decision on a link that is no longer pending.
	ErrNotPending = errors.New("pages: that link is no longer waiting for a decision")
	// ErrLinkUnreadable is a page's reusable link whose sealed token does not
	// open (the server's key changed): it still serves, but its address cannot
	// be shown again — make a new link (RotateShareLink).
	ErrLinkUnreadable = errors.New("pages: this page's link can no longer be shown (the server's encryption key changed); make a new link")
	// ErrClosing is a new link asked of a page whose delete has begun
	// (CloseLinks ran): it gets none — press Delete again to finish it.
	ErrClosing = errors.New("pages: this page is being deleted; it takes no new link")
)

// Creator is who made a page, and so who a tool may act on it as: a
// dispatched agent (AgentID) or a mirrored session's HOST (MirrorHost — what a
// mirror token proves; the session id is the caller's claim). Exactly one is
// set. Label is the name the owner sees.
type Creator struct {
	AgentID    int64
	MirrorHost string
	Label      string
}

// Valid reports whether c names exactly one creator.
func (c Creator) Valid() bool { return (c.AgentID > 0) != (c.MirrorHost != "") }

// Page is one page.
type Page struct {
	ID             int64
	OwnerID        int64
	Title          string
	CurrentVersion int
	Creator        Creator
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Summary is a page as a list shows it: the page and its counts.
type Summary struct {
	Page
	Versions  int
	LiveLinks int
	Pending   int
	Responses int
}

// Version is one immutable version of a page.
type Version struct {
	ID        int64
	PageID    int64
	Version   int
	Title     string
	Blocks    []Block
	SizeBytes int
	CreatedBy string
	CreatedAt time.Time
}

// LinkState is a share link's state.
type LinkState string

const (
	// LinkPending: an agent asked; the owner has not decided. Serves nothing.
	LinkPending LinkState = "pending"
	// LinkLive: approved (or minted by the owner). Serves the current version.
	LinkLive LinkState = "live"
	// LinkDenied: the owner said no. Serves nothing, ever.
	LinkDenied LinkState = "denied"
	// LinkRevoked: was live, and was revoked. Serves nothing, ever.
	LinkRevoked LinkState = "revoked"
)

// Link is one share link.
//
// 🔴 ITS TOKEN IS NOT STORED IN THE CLEAR. Token is set only on the Link
// CreateLink returns — for a hash-only link the one moment the token exists
// outside a request — on the Link LinkByToken returns (the presented token,
// already matched), and on a REUSABLE link ShareLink, RotateShareLink or
// ShareLinks returns (opened from its sealed copy, seal.go); every other read
// leaves it "". TokenSHA256 is the lookup key.
type Link struct {
	ID          int64
	OwnerID     int64
	PageID      int64
	Token       string
	TokenSHA256 string
	State       LinkState
	AskID       int64
	RequestedBy string
	// Reusable: the page's one link the owner's Share hands out every time
	// (migration 0135). Its token is kept sealed while it is live, so
	// ShareLink can show the address again; a link that is not reusable is
	// hash-only, and its address was shown once.
	Reusable bool
	// ByAgent: an agent made it (its request), not the owner.
	ByAgent   bool
	CreatedAt time.Time
	DecidedAt *time.Time
}

// Label is a link's short, non-secret name for its owner: when it was made
// and the first 8 hex digits of its token's hash ("created 2026-11-11 14:02
// UTC · 3fa9c2e1"). The hash prefix tells two links apart; it is no part of
// the token and opens nothing.
func (l Link) Label() string {
	h := l.TokenSHA256
	if len(h) > 8 {
		h = h[:8]
	}
	return "created " + l.CreatedAt.UTC().Format("2006-01-02 15:04") + " UTC · " + h
}

// ResponseKind is what a response is.
type ResponseKind string

const (
	// KindSubmit is a component's (or custom script's) submit: Data.
	KindSubmit ResponseKind = "submit"
	// KindUpload is a file: UploadPublicID names the uploads row.
	KindUpload ResponseKind = "upload"
)

// Response is one thing a viewer — or the page's owner — sent.
type Response struct {
	ID     int64
	PageID int64
	// LinkID is the link it came through; 0 for the owner's (ByOwner).
	LinkID int64
	// ByOwner: the page's owner sent it from their own view of the page,
	// signed in (migration 0149), not a viewer through a link.
	ByOwner        bool
	Version        int
	Component      string
	Kind           ResponseKind
	Data           []byte // JSON; "{}" for an upload
	UploadPublicID string
	CreatedAt      time.Time
}

// --- the token -------------------------------------------------------------

// tokenBytes is a link token's entropy: 256 bits.
const tokenBytes = 32

var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// NewToken is a fresh link token: 32 random bytes, base64url without padding
// (43 characters).
func NewToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// TokenShapeOK reports whether s is shaped like a link token. A request whose
// token is not is refused before any lookup.
func TokenShapeOK(s string) bool { return tokenPattern.MatchString(s) }

// TokenHash is the lookup key of token: its sha256, lower-case hex.
func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// LinkPath is the shell's path for token.
func LinkPath(token string) string { return SharePrefix + token }

// SharePrefix begins every share-link path (the shell, its content and its two
// writes). The access log writes it as SharePrefix + "[redacted]".
const SharePrefix = "/s/"
