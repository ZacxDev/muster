// Package pages is shareable pages (migration 0113; docs/pages.md, design
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
	// MaxVersionBytes bounds one version's blocks as JSON (0113's CHECK).
	MaxVersionBytes = 512 << 10
	// MaxBlocks bounds a version's block count.
	MaxBlocks = 100
	// MaxHTMLBytes bounds one html block.
	MaxHTMLBytes = 256 << 10
	// MaxMarkdownBytes bounds one markdown block.
	MaxMarkdownBytes = 64 << 10
	// MaxSubmitBytes bounds a submit's data as JSON.
	MaxSubmitBytes = 32 << 10
	// MaxTitleRunes bounds a page's title (0113's CHECK).
	MaxTitleRunes = 200
)

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
type Link struct {
	ID          int64
	OwnerID     int64
	PageID      int64
	Token       string
	State       LinkState
	AskID       int64
	RequestedBy string
	CreatedAt   time.Time
	DecidedAt   *time.Time
	ExpiresAt   *time.Time
}

// Live reports whether l serves at now.
func (l Link) Live(now time.Time) bool {
	return l.State == LinkLive && (l.ExpiresAt == nil || l.ExpiresAt.After(now))
}

// ResponseKind is what a response is.
type ResponseKind string

const (
	// KindSubmit is a component's (or custom script's) submit: Data.
	KindSubmit ResponseKind = "submit"
	// KindUpload is a file: UploadPublicID names the uploads row.
	KindUpload ResponseKind = "upload"
)

// Response is one thing a viewer sent.
type Response struct {
	ID             int64
	PageID         int64
	LinkID         int64
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
