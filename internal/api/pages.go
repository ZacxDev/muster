package api

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/features"
	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/pages"
	"github.com/ZacxDev/muster/internal/textplain"
	"github.com/ZacxDev/muster/internal/theme"
	"github.com/ZacxDev/muster/internal/ui"
	"github.com/ZacxDev/muster/internal/uploads"

	g "maragu.dev/gomponents"
)

// ---------------------------------------------------------------------------
// SHAREABLE PAGES (internal/pages, migration 0116; docs/pages.md, design
// docs/design/pages.md). This file is the routes: the four share-link routes a
// viewer reaches (/s/{token}…) and the owner's Pages screen. page_tools.go is
// the agents' tools; page_share.go the owner's approval of a link.
//
// 🔴 THE SHARE-LINK ROUTES READ NO COOKIE AND WRITE NONE. requirePageLink is
// their ONLY credential check: the token in the path, looked up once under
// System (pages.PGStore.LinkByToken), then the request acts as the page's
// OWNER (authz.ViaPageLink) — so row-level security confines it to that
// owner's rows, and every read below is also bound to the link's page. A
// session cookie the browser sends along is never consulted, so nothing on
// these routes is reachable by an ambient credential, and there is nothing to
// forge across sites. That is also why the two writes do not check Origin: a
// sandboxed page's requests carry `Origin: null`, and a token holder may post
// from anywhere, by design.
//
// 🔴 THE CONTENT DOCUMENT IS SERVED SANDBOXED BY HTTP HEADER
// (pages.SetContentHeaders): an opaque origin even when opened directly.
// Every content response — a link's and the owner's view's — goes through
// servePageContent, the one writer.
//
// 🔴 A REFUSED LINK IS ONE ANSWER: unknown, pending, denied, revoked, expired
// or malformed all get pageLinkGone's 404, so nothing says which.
// ---------------------------------------------------------------------------

// The owner's routes.
const (
	pagesPath = "/pages"
)

// pageLinkGone is the one body every refused share link gets.
const pageLinkGone = "This link does not exist, or no longer works.\n"

// The share-link rate limits (docs/design/pages.md#limits-and-quotas). Vars so
// a test can lower them.
var (
	// pageMissLimit: failed lookups per client address per pageLimitWindow,
	// then 429. On the public pages listener the 429 comes BEFORE any lookup,
	// so a guesser there cannot make the database work; on muster's own
	// listeners the lookup comes first (requirePageLink says why).
	pageMissLimit = 60
	// pageSubmitLimit and pageUploadLimit: per link per pageLimitWindow.
	pageSubmitLimit = 20
	pageUploadLimit = 10
	// pageAssetLimit: one viewer's reads of a page's images and video through
	// a link (page_assets.go), keyed by link and client address. A view reads
	// each asset once, and a repeat view comes from the browser's cache, so
	// this is room for many views of a page of 200 assets and a bound on one
	// viewer looping; the bytes all viewers of a link draw are bounded apart
	// (pageAssetByteBudget).
	pageAssetLimit  = 600
	pageLimitWindow = time.Minute
	// pagePushWindow: at most one owner push per page per window.
	pagePushWindow = time.Minute
)

// pageSubmitBodyMax bounds a submit's body: the data cap plus its framing.
const pageSubmitBodyMax = pages.MaxSubmitBytes + 4<<10

// pageState is the feature's process state; the zero value is ready.
type pageState struct {
	misses  boundedWindow
	submits boundedWindow
	uploads boundedWindow
	// feedback counts feedback messages per responder (page_feedback.go).
	feedback boundedWindow
	assets   boundedWindow
	// assetBytes counts BYTES, per link (spend), not requests.
	assetBytes boundedWindow
	pushes     boundedWindow
	waits      pageWaits
	// uploadSlots is the page uploads' in-flight slots, keyed by LINK — never
	// the composer's (Server.uploadLimits, keyed by user).
	uploadSlots pageSlots
}

// pagesOn reports whether pages are served here: a store, and the registry
// not calling the feature off.
func (s *Server) pagesOn() bool { return s.ext.Pages != nil && !s.featureOff(features.Pages) }

func (s *Server) registerPageRoutes(mux Mux) {
	// The viewer's four: the link is the credential (requirePageLink).
	mux.HandleFunc("GET "+pages.SharePrefix+"{token}", s.requirePageLink(s.handlePageShell))
	mux.HandleFunc("GET "+pages.SharePrefix+"{token}/content", s.requirePageLink(s.handlePageContent))
	mux.HandleFunc("POST "+pages.SharePrefix+"{token}/submit", s.requirePageLink(s.handlePageSubmit))
	mux.HandleFunc("POST "+pages.SharePrefix+"{token}/upload", s.requirePageLink(s.handlePageUpload))
	// The Feedback pill's message through the link (page_feedback.go).
	mux.HandleFunc("POST "+pages.SharePrefix+"{token}"+pages.FeedbackPathSuffix, s.requirePageLink(s.handlePageFeedback))
	// The owner's: a signed-in session; writes same-origin too.
	mux.HandleFunc("GET "+pagesPath, s.requireSession(s.handlePagesList))
	mux.HandleFunc("GET "+pagesPath+"/{id}", s.requireSession(s.handlePageDetail))
	mux.HandleFunc("GET "+pagesPath+"/{id}/view", s.requireSession(s.handlePageView))
	// The view's former address, still in older notifications and cards.
	mux.HandleFunc("GET "+pagesPath+"/{id}/preview", s.requireSession(s.handlePagePreview))
	mux.HandleFunc("GET "+pagesPath+"/{id}/versions/{v}/content", s.requireSession(s.handlePageVersionContent))
	// An admin's download of one response's stored form (its raw ⓘ, cut).
	mux.HandleFunc("GET "+pagesPath+"/{id}/responses/{rid}/raw", s.requireSession(s.handlePageResponseRaw))
	// The owner's own responses from their view: their session and muster's
	// own Origin (requireOperatorWrite), never a link's token.
	mux.HandleFunc("POST "+pagesPath+"/{id}/versions/{v}/submit", s.requireOperatorWrite(s.handleOwnerPageSubmit))
	mux.HandleFunc("POST "+pagesPath+"/{id}/versions/{v}/upload", s.requireOperatorWrite(s.handleOwnerPageUpload))
	mux.HandleFunc("POST "+pagesPath+"/{id}/versions/{v}"+pages.FeedbackPathSuffix, s.requireOperatorWrite(s.handleOwnerPageFeedback))
	mux.HandleFunc("POST "+pagesPath+"/{id}/links/{link}/revoke", s.requireOperatorWrite(s.handlePageRevokeLink))
	// The Share button and its ⋯ menu (page_card.go): the page's one reusable
	// link. And the chat's page card, for the live stream.
	mux.HandleFunc("POST "+pagesPath+"/{id}/share", s.requireOperatorWrite(s.handlePageShare))
	mux.HandleFunc("POST "+pagesPath+"/{id}/share/rotate", s.requireOperatorWrite(s.handlePageShareRotate))
	mux.HandleFunc("POST "+pagesPath+"/{id}/share/revoke", s.requireOperatorWrite(s.handlePageShareRevoke))
	mux.HandleFunc("GET "+pagesPath+"/{id}/card", s.requireSession(s.handlePageCard))
	// The owner's own opt-in to their agents sharing without asking
	// (page_share_pref.go).
	mux.HandleFunc("POST "+accountPagesAgentSharePath, s.requireOperatorWrite(s.handleAccountPagesAgentShare))
	mux.HandleFunc("POST "+pagesPath+"/{id}/rollback", s.requireOperatorWrite(s.handlePageRollback))
	mux.HandleFunc("POST "+pagesPath+"/{id}/delete", s.requireOperatorWrite(s.handlePageDelete))
	// The author's images and video (page_assets.go).
	s.registerPageAssetRoutes(mux)
}

// pageLinkCtxKey carries the admitted share link to its handler.
type pageLinkCtxKey struct{}

// pageLinkOf is the share link requirePageLink admitted.
func pageLinkOf(ctx context.Context) (pages.Link, bool) {
	l, ok := ctx.Value(pageLinkCtxKey{}).(pages.Link)
	return l, ok
}

// refusePageLink is the one refusal of a share link.
func refusePageLink(w http.ResponseWriter, route string) {
	metrics.PageLinkRequests.WithLabelValues(route, "not_found").Inc()
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, pageLinkGone)
}

// pageRouteLabel is the metric label of a share-link request's route.
func pageRouteLabel(r *http.Request) string {
	switch {
	case strings.HasSuffix(r.URL.Path, "/content"):
		return "content"
	case strings.HasSuffix(r.URL.Path, "/submit"):
		return "submit"
	case strings.HasSuffix(r.URL.Path, "/upload"):
		return "upload"
	case strings.HasSuffix(r.URL.Path, pages.FeedbackPathSuffix):
		return "feedback"
	case strings.Contains(r.URL.Path, "/assets/"):
		return "asset"
	}
	return "shell"
}

// requirePageLink admits a request holding a live share link, as the page's
// owner through authz.ViaPageLink, with the link on its context.
//
// 🔴 NO OTHER CREDENTIAL IS READ: not a session cookie, not a bearer. The
// failure limit counts only failed lookups, per client address. WHERE it is
// checked depends on the listener:
//   - on the PUBLIC pages listener (PagesPublicSurface), which can face the
//     internet, BEFORE the lookup: an address past it is answered 429 for the
//     window without the database being asked anything, whatever token it
//     sends. The cost, accepted there: a viewer behind the same address as a
//     guesser (a shared NAT) cannot open even a live link until it passes.
//   - on muster's own listeners, AFTER it, and only for a token that does not
//     resolve: an address that is throttled still opens a live link, and only
//     its misses are answered 429 (TestAThrottledAddressStillOpensALiveLink).
func (s *Server) requirePageLink(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		route := pageRouteLabel(r)
		if !s.pagesOn() {
			refusePageLink(w, route)
			return
		}
		key := clientIP(r)
		throttled := func() bool {
			if s.pageState.misses.count(key, s.now(), pageLimitWindow) < pageMissLimit {
				return false
			}
			metrics.PageLinkRequests.WithLabelValues(route, "throttled").Inc()
			w.Header().Set("Retry-After", "60")
			http.Error(w, "Too many requests for links that do not exist. Wait a minute.", http.StatusTooManyRequests)
			return true
		}
		if viaPagesPublic(r) && throttled() {
			return
		}
		l, err := s.ext.Pages.LinkByToken(r.Context(), r.PathValue("token"))
		if err != nil {
			if !errors.Is(err, pages.ErrLinkNotFound) {
				s.logger.Printf("pages: link lookup failed: %v", err)
			}
			if throttled() {
				return
			}
			s.pageState.misses.add(key, s.now(), pageLimitWindow)
			refusePageLink(w, route)
			return
		}
		if l.OwnerID <= 0 {
			refusePageLink(w, route)
			return
		}
		ctx := context.WithValue(r.Context(), pageLinkCtxKey{}, l)
		s.admit(w, r.WithContext(ctx), next, s.principalFor(r.Context(), l.OwnerID, authz.ViaPageLink))
	}
}

// pageRenderOptions is how version v of a page is rendered here: with the
// author's assets it refers to (asset.go), which the SDK asks the shell for.
// A failed read is logged and renders with none (each reference left as it
// was written, which loads nothing).
func (s *Server) pageRenderOptions(ctx context.Context, v pages.Version) pages.RenderOptions {
	o := pages.RenderOptions{Markdown: ui.MarkdownHTML, Uploads: s.ext.Uploads != nil, ThemeCSS: s.pageThemeCSS(ctx)}
	if s.ext.Uploads != nil {
		a, err := s.ext.Pages.VersionAssets(ctx, v.PageID, v.Version)
		if err != nil {
			s.logger.Printf("pages: page %d: the assets of version %d: %v", v.PageID, v.Version, err)
		}
		o.Assets = a
	}
	return o
}

// servePageContent is THE writer of a content document: every one — a link's
// and the owner's view's — is served with pages.SetContentHeaders, which
// replaces the app's own page policy.
//
// 🔴 ONLY INTO A FRAME. A browser that says where the document is going
// (Sec-Fetch-Dest) gets it only for an iframe: opened on its own, outside the
// shell's frame, a page's script could still navigate (its CSP cannot stop a
// top-level navigation), so a top-level open is refused with the one 404.
// frame-ancestors 'self' then keeps every frame but muster's own. A client
// that sends no Sec-Fetch-Dest runs no script of the page, and is served.
func servePageContent(w http.ResponseWriter, r *http.Request, body string) bool {
	if d := r.Header.Get("Sec-Fetch-Dest"); d != "" && d != "iframe" {
		refusePageLink(w, "content")
		return false
	}
	pages.SetContentHeaders(w.Header())
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, body)
	return true
}

// serveShell writes a shell document: framed by no page, but the owner's
// own view in a session's panel (v.Embed) by muster's own pages only.
func serveShell(w http.ResponseWriter, v pages.ShellView) {
	if v.Embed && v.Owner != nil {
		pages.SetEmbedShellHeaders(w.Header(), v.Nonce)
	} else {
		pages.SetShellHeaders(w.Header(), v.Nonce)
	}
	if v.Owner != nil && v.Feedback != nil && v.Feedback.Mic != "" {
		// The owner's Feedback microphone runs in the shell itself
		// (muster's own origin). The content frame stays without one: its own
		// header says microphone=(), and its opaque origin is not 'self'.
		w.Header().Set("Permissions-Policy", pages.FeedbackMicPermissionsPolicy)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, pages.Shell(v))
}

// shellBrand is the instance branding as the shell draws it — by ui.Brand's
// own rules, as every branded surface does: the name only when it is drawn
// beside the logo (DrawsName), the logo's alt text (LogoAlt), the app icon.
func (s *Server) shellBrand(ctx context.Context, v pages.ShellView) pages.ShellView {
	b := s.uiBrand(ctx)
	if b.HasLogo() {
		v.BrandLogo, v.BrandLogoAlt = b.Versioned(ui.BrandLogoPath), b.LogoAlt()
	}
	if b.DrawsName() {
		v.BrandName = b.Name
	}
	v.Icon = b.Icon192()
	v.ThemeCSS = s.pageThemeCSS(ctx)
	return v
}

// pageThemeCSS is the server's theme block as a page's documents inline it
// (the content's CSP loads no stylesheet): the same rules /ui-kit/v1/theme.css
// serves (theme.CSS of serverTheme), or "" for the default theme, whose
// palette the documents already embed.
func (s *Server) pageThemeCSS(ctx context.Context) string {
	c, t := s.serverTheme(ctx)
	if c.IsDefault() {
		return ""
	}
	return theme.CSS(t)
}

// handlePageShell is GET /s/{token}: the shell of the link's page.
func (s *Server) handlePageShell(w http.ResponseWriter, r *http.Request) {
	l, _ := pageLinkOf(r.Context())
	v, err := s.ext.Pages.Version(r.Context(), l.PageID, 0)
	if err != nil {
		s.logger.Printf("pages: link %d: read its page: %v", l.ID, err)
		refusePageLink(w, "shell")
		return
	}
	base := pages.LinkPath(l.Token)
	metrics.PageLinkRequests.WithLabelValues("shell", "served").Inc()
	serveShell(w, s.shellBrand(r.Context(), pages.ShellView{Title: v.Title, FrameSrc: base + "/content", Base: base, Nonce: pages.NewNonce(), Keeps: pages.KeepKeys(v),
		Feedback: &pages.FeedbackChrome{}}))
}

// handlePageContent is GET /s/{token}/content: the current version, sandboxed.
func (s *Server) handlePageContent(w http.ResponseWriter, r *http.Request) {
	l, _ := pageLinkOf(r.Context())
	v, err := s.ext.Pages.Version(r.Context(), l.PageID, 0)
	if err != nil {
		s.logger.Printf("pages: link %d: read its page: %v", l.ID, err)
		refusePageLink(w, "content")
		return
	}
	if servePageContent(w, r, pages.Content(v, s.pageRenderOptions(r.Context(), v))) {
		metrics.PageLinkRequests.WithLabelValues("content", "served").Inc()
	}
}

// pageWriteError answers a share-link write's refusal as JSON the shell hands
// back to the page.
func (s *Server) pageWriteError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	s.writeJSON(w, status, map[string]any{"error": msg})
}

// pageStoreRefusal is a store error as a viewer reads it, and its status.
func pageStoreRefusal(err error) (int, string, string) {
	switch {
	case errors.Is(err, pages.ErrInvalid):
		return http.StatusBadRequest, strings.TrimPrefix(err.Error(), pages.ErrInvalid.Error()+": "), "invalid"
	case errors.Is(err, pages.ErrTooMany):
		return http.StatusConflict, "This page is not taking any more responses.", "full"
	case errors.Is(err, pages.ErrResponseTooLarge):
		return http.StatusRequestEntityTooLarge, "That is too much to send at once. Send less.", "invalid"
	case errors.Is(err, pages.ErrLinkNotFound), errors.Is(err, pages.ErrNotFound), errors.Is(err, pages.ErrClosing):
		return http.StatusNotFound, strings.TrimSpace(pageLinkGone), "not_found"
	}
	return http.StatusInternalServerError, "It could not be recorded. Try again later.", "error"
}

// pageResponder is who a response is recorded as: a share link's viewer, or
// the page's signed-in owner from their own view (migration 0152). Both go
// through the SAME submit and upload code (pageSubmit, pageUpload), with the
// same checks, rate limits, caps and size bounds; only the identity, the
// version and the store call differ.
type pageResponder struct {
	page int64
	// link is the viewer's link; zero for the owner.
	link pages.Link
	// owner: the page's owner, signed in (requireOperatorWrite: the session
	// and muster's own Origin, as every owner write).
	owner bool
	// version is the version answered: 0, a link's, is the page's current
	// one; the owner answers the version their view shows (?v=N included).
	version int
}

// String names the responder in a log line (ids only).
func (p pageResponder) String() string {
	if p.owner {
		return "page " + strconv.FormatInt(p.page, 10) + "'s owner"
	}
	return "link " + strconv.FormatInt(p.link.ID, 10)
}

// rateKey is the responder's key in the submit and upload windows: per link,
// and for the owner per page — the same limits either way.
func (p pageResponder) rateKey() string {
	if p.owner {
		return "owner-" + strconv.FormatInt(p.page, 10)
	}
	return strconv.FormatInt(p.link.ID, 10)
}

// slotKey is the responder's key in the page upload slots: a link id, or for
// the owner the page id negated (link and page ids are both positive, so the
// two never meet).
func (p pageResponder) slotKey() int64 {
	if p.owner {
		return -p.page
	}
	return p.link.ID
}

// record records one response as the responder.
//
// A response on a page a mirrored session made wakes that session
// (session_wake.go): the owner's own response with its content, anyone
// else's as one more in a count, never their words.
func (s *Server) recordPageResponse(ctx context.Context, p pageResponder, v int, component string, kind pages.ResponseKind, data []byte, uploadID string) (pages.Response, error) {
	r, owner, err := s.storePageResponse(ctx, p, v, component, kind, data, uploadID)
	if err == nil {
		s.wakePageResponse(ctx, owner, p.page, r)
	}
	return r, err
}

// storePageResponse stores one response as the responder, waking nothing, and
// names the page's owner: THE store call both a response (recordPageResponse)
// and a feedback message (page_feedback.go) make.
func (s *Server) storePageResponse(ctx context.Context, p pageResponder, v int, component string, kind pages.ResponseKind, data []byte, uploadID string) (pages.Response, int64, error) {
	var r pages.Response
	var err error
	owner := p.link.OwnerID
	if p.owner {
		r, err = s.ext.Pages.AddOwnerResponse(ctx, p.page, v, component, kind, data, uploadID)
		if pg, perr := s.ext.Pages.Page(ctx, p.page); err == nil && perr == nil {
			owner = pg.OwnerID
		}
	} else {
		r, err = s.ext.Pages.AddResponse(ctx, p.link, v, component, kind, data, uploadID)
	}
	return r, owner, err
}

// handlePageSubmit is POST /s/{token}/submit: {component, data}, checked
// against the CURRENT version's component and recorded through the link.
func (s *Server) handlePageSubmit(w http.ResponseWriter, r *http.Request) {
	l, _ := pageLinkOf(r.Context())
	s.pageSubmit(w, r, pageResponder{page: l.PageID, link: l})
}

// handlePageUpload is POST /s/{token}/upload: a file through the link.
func (s *Server) handlePageUpload(w http.ResponseWriter, r *http.Request) {
	l, _ := pageLinkOf(r.Context())
	s.pageUpload(w, r, pageResponder{page: l.PageID, link: l})
}

// ownerResponder is the owner's POST /pages/{id}/versions/{v}/submit or
// /upload as a responder: false (answered 404, the one "missing") for a bad
// id or version, or one that is not the caller's — the store's read is under
// their row-level security, so another owner's page reads as missing — before
// any byte of the body is read.
func (s *Server) ownerResponder(w http.ResponseWriter, r *http.Request) (pageResponder, bool) {
	id, ok := pageIDParam(r)
	n, err := strconv.Atoi(r.PathValue("v"))
	if s.pagesOn() && ok && err == nil && n > 0 {
		_, err = s.ext.Pages.Version(r.Context(), id, n)
		if err == nil {
			return pageResponder{page: id, owner: true, version: n}, true
		}
		if !errors.Is(err, pages.ErrNotFound) {
			s.logger.Printf("pages: page %d: the owner's response: read version %d: %v", id, n, err)
		}
	}
	s.pageWriteError(w, http.StatusNotFound, "No such page or version.")
	return pageResponder{}, false
}

// handleOwnerPageSubmit is POST /pages/{id}/versions/{v}/submit: the OWNER's
// own response from their view of version v, recorded as theirs (by_owner).
func (s *Server) handleOwnerPageSubmit(w http.ResponseWriter, r *http.Request) {
	if p, ok := s.ownerResponder(w, r); ok {
		s.pageSubmit(w, r, p)
	}
}

// handleOwnerPageUpload is POST /pages/{id}/versions/{v}/upload: the owner's
// own file.
func (s *Server) handleOwnerPageUpload(w http.ResponseWriter, r *http.Request) {
	if p, ok := s.ownerResponder(w, r); ok {
		s.pageUpload(w, r, p)
	}
}

// pageSubmit is THE submit: {component, data}, checked against the
// responder's version's component and recorded as the responder.
//
// 🔴 AN OLD VERSION'S SUBMIT (the owner viewing ?v=N) IS RECORDED AGAINST
// THAT VERSION, checked against ITS blocks: a response always names the
// version it answered, as a link's always has.
func (s *Server) pageSubmit(w http.ResponseWriter, r *http.Request, p pageResponder) {
	ctx := r.Context()
	if !s.pageState.submits.take(p.rateKey(), s.now(), pageLimitWindow, pageSubmitLimit) {
		metrics.PageResponses.WithLabelValues(string(pages.KindSubmit), "throttled").Inc()
		w.Header().Set("Retry-After", "60")
		s.pageWriteError(w, http.StatusTooManyRequests, "Too many responses through this link for now. Try again in a minute.")
		return
	}
	// 🔴 A BOUNDED READ: a body that trickles in is cut off at its deadline
	// (408), on every listener, rather than holding this goroutine and its
	// connection for as long as the sender likes.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(pageSubmitReadTimeout))
	r.Body = http.MaxBytesReader(w, r.Body, pageSubmitBodyMax)
	var in struct {
		Component string          `json:"component"`
		Data      json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		metrics.PageResponses.WithLabelValues(string(pages.KindSubmit), "invalid").Inc()
		if errors.Is(err, os.ErrDeadlineExceeded) {
			s.pageWriteError(w, http.StatusRequestTimeout, "The response took too long to arrive. Send it again.")
			return
		}
		s.pageWriteError(w, http.StatusBadRequest, "The request is not {component, data}, or is too large.")
		return
	}
	if !pages.ValidComponent(in.Component) {
		metrics.PageResponses.WithLabelValues(string(pages.KindSubmit), "invalid").Inc()
		s.pageWriteError(w, http.StatusBadRequest, "That is not a component of this page.")
		return
	}
	v, err := s.ext.Pages.Version(ctx, p.page, p.version)
	if err != nil {
		status, msg, outcome := pageStoreRefusal(err)
		metrics.PageResponses.WithLabelValues(string(pages.KindSubmit), outcome).Inc()
		s.pageWriteError(w, status, msg)
		return
	}
	data, err := pages.CheckSubmit(v.Blocks, in.Component, in.Data)
	if err != nil {
		status, msg, outcome := pageStoreRefusal(err)
		metrics.PageResponses.WithLabelValues(string(pages.KindSubmit), outcome).Inc()
		s.pageWriteError(w, status, msg)
		return
	}
	resp, err := s.recordPageResponse(ctx, p, v.Version, in.Component, pages.KindSubmit, data, "")
	if err != nil {
		status, msg, outcome := pageStoreRefusal(err)
		if outcome == "error" {
			s.logger.Printf("pages: %s: record a submit: %v", p, err)
		}
		metrics.PageResponses.WithLabelValues(string(pages.KindSubmit), outcome).Inc()
		s.pageWriteError(w, status, msg)
		return
	}
	metrics.PageResponses.WithLabelValues(string(pages.KindSubmit), "recorded").Inc()
	s.pageResponded(ctx, p, v, resp)
	w.Header().Set("Referrer-Policy", "no-referrer")
	s.writeJSON(w, http.StatusOK, map[string]any{"id": resp.ID})
}

// pageUpload is THE upload: a multipart body of `component` then `file`,
// streamed to the upload store as the page's owner and recorded as an upload
// response of the responder. The type is sniffed from the bytes
// (uploads.Put), and the file is only ever served back through
// uploads.SetServeHeaders.
func (s *Server) pageUpload(w http.ResponseWriter, r *http.Request, p pageResponder) {
	ctx := r.Context()
	svc := s.ext.Uploads
	fail := func(status int, msg, outcome string) {
		metrics.PageResponses.WithLabelValues(string(pages.KindUpload), outcome).Inc()
		s.pageWriteError(w, status, msg)
	}
	if svc == nil {
		fail(http.StatusNotFound, "File uploads are not available on this server.", "invalid")
		return
	}
	if !s.pageState.uploads.take(p.rateKey(), s.now(), pageLimitWindow, pageUploadLimit) {
		w.Header().Set("Retry-After", "60")
		fail(http.StatusTooManyRequests, "Too many files through this link for now. Try again in a minute.", "throttled")
		return
	}
	limit := svc.MaxBytes + 64<<10
	if r.ContentLength > limit {
		fail(http.StatusRequestEntityTooLarge, "The file is larger than "+uploads.HumanSize(svc.MaxBytes)+".", "invalid")
		return
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "multipart/form-data" {
		fail(http.StatusBadRequest, "Send the file as multipart/form-data.", "invalid")
		return
	}
	// 🔴 A STRANGER'S UPLOAD TAKES A PAGE SLOT, NEVER THE OWNER'S: the slots
	// are per link (per page for the owner's own view) and a server-wide page
	// pool (pageState.uploadSlots), apart from the composer's per-user slots,
	// with a shorter read deadline — so a link holder trickling bodies cannot
	// make the owner's own attach busy.
	leave, err := s.pageState.uploadSlots.enter(p.slotKey())
	if err != nil {
		w.Header().Set("Retry-After", "5")
		fail(http.StatusTooManyRequests, "Too many files are arriving right now. Try again shortly.", "throttled")
		return
	}
	defer leave()
	deadline := time.Now().Add(pageUploadReadTimeout)
	_ = http.NewResponseController(w).SetReadDeadline(deadline)
	ctx, cancel := context.WithDeadline(ctx, deadline.Add(30*time.Second))
	defer cancel()
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	mr, err := r.MultipartReader()
	if err != nil {
		fail(http.StatusBadRequest, "Send the file as multipart/form-data.", "invalid")
		return
	}
	v, err := s.ext.Pages.Version(ctx, p.page, p.version)
	if err != nil {
		status, msg, outcome := pageStoreRefusal(err)
		fail(status, msg, outcome)
		return
	}
	// The page's byte quota: what its files may still add (MUSTER_PAGES_UPLOAD_BYTES).
	quota := s.pageUploadQuota()
	used, err := s.ext.Pages.UploadBytes(ctx, p.page)
	if err != nil {
		s.logger.Printf("pages: %s: read its page's file total: %v", p, err)
		fail(http.StatusInternalServerError, "The file could not be stored. Try again later.", "error")
		return
	}
	room := quota - used
	if room <= 0 {
		fail(http.StatusRequestEntityTooLarge, pageQuotaFull(quota), "full")
		return
	}
	component := ""
	for {
		part, err := mr.NextPart()
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				fail(http.StatusRequestEntityTooLarge, "The file is larger than "+uploads.HumanSize(svc.MaxBytes)+".", "invalid")
				return
			}
			fail(http.StatusBadRequest, "No file arrived.", "invalid")
			return
		}
		switch part.FormName() {
		case "component":
			b, _ := io.ReadAll(io.LimitReader(part, 128))
			component = strings.TrimSpace(string(b))
			continue
		case "file":
		default:
			continue
		}
		// 🔴 THE COMPONENT COMES FIRST AND IS CHECKED BEFORE A BYTE IS STORED.
		if !pages.ValidComponent(component) || !pages.UploadTarget(v.Blocks, component) {
			fail(http.StatusBadRequest, "That is not an upload of this page.", "invalid")
			return
		}
		// Under the page's own prefix (pages/<id>/…), which the bucket's
		// uploads/ lifecycle rule does not reach: kept until the page is
		// deleted (handlePageDelete deletes them).
		stored, err := svc.PutForPage(ctx, part, part.FileName(), p.page, room)
		if err != nil {
			var tooBig *http.MaxBytesError
			switch {
			case errors.Is(err, uploads.ErrTooLarge) && room < svc.MaxBytes:
				fail(http.StatusRequestEntityTooLarge, pageQuotaRoom(quota, room), "full")
			case errors.Is(err, uploads.ErrTooLarge), errors.As(err, &tooBig):
				fail(http.StatusRequestEntityTooLarge, "The file is larger than "+uploads.HumanSize(svc.MaxBytes)+".", "invalid")
			case errors.Is(err, uploads.ErrEmpty):
				fail(http.StatusBadRequest, "The file is empty.", "invalid")
			default:
				s.logger.Printf("pages: %s: store an upload: %v", p, err)
				fail(http.StatusInternalServerError, "The file could not be stored. Try again later.", "error")
			}
			return
		}
		resp, err := s.recordPageResponse(ctx, p, v.Version, component, pages.KindUpload, nil, stored.PublicID)
		if err != nil {
			// The file is stored but not recorded: take it away again, so no
			// object outlives its row.
			s.dropUnrecordedUpload(ctx, p, stored.PublicID)
			status, msg, outcome := pageStoreRefusal(err)
			if outcome == "error" {
				s.logger.Printf("pages: %s: record an upload: %v", p, err)
			}
			fail(status, msg, outcome)
			return
		}
		metrics.PageResponses.WithLabelValues(string(pages.KindUpload), "recorded").Inc()
		s.pageResponded(ctx, p, v, resp)
		w.Header().Set("Referrer-Policy", "no-referrer")
		s.writeJSON(w, http.StatusOK, map[string]any{"id": resp.ID, "name": stored.Name, "size": stored.Size, "type": stored.ContentType})
		return
	}
}

// pageResponded is what follows a recorded response: a waiting page_responses
// call is woken, and — for a VIEWER's response — the owner is pushed, at most
// once per page per pagePushWindow, naming the page and the component, never
// the viewer's text. The owner's own response pushes nobody: they sent it.
func (s *Server) pageResponded(ctx context.Context, p pageResponder, v pages.Version, r pages.Response) {
	s.pageState.waits.wake(p.page)
	if p.owner || !s.pageState.pushes.take(strconv.FormatInt(p.page, 10), s.now(), pagePushWindow, 1) {
		return
	}
	what := "a response"
	switch r.Kind {
	case pages.KindUpload:
		what = "a file"
	case pages.KindFeedback:
		what = "feedback"
	}
	s.pushTask(p.link.OwnerID, pushEventPage, RouterNotification{
		Type:  taskNotificationType,
		Title: "Page “" + truncate(v.Title, 60) + "” got " + what,
		Body:  pageResponsePushBody(v.Blocks, r),
		Tag:   "page-" + strconv.FormatInt(p.page, 10) + "-response",
		Data:  map[string]string{"url": pagePath(p.page)},
	})
}

// pageResponsePushBody is a response's push body: the part it came through
// by the page's own label (pages.DescribeAnswer's heading, UploadHeading for
// a file) — never the part's id, never the viewer's words.
func pageResponsePushBody(blocks []pages.Block, r pages.Response) string {
	if r.Component == pages.CustomComponent {
		return "From the page's own script. Open the page in muster to see it."
	}
	if r.Kind == pages.KindFeedback {
		return "From the Feedback button. Open the page in muster to read it."
	}
	part := pages.UploadHeading(blocks, r.Component)
	if r.Kind != pages.KindUpload {
		part = pages.DescribeAnswer(blocks, r.Component, nil).Heading
	}
	return "Through “" + truncate(part, 60) + "”. Open the page in muster to see it."
}

// pagePath is the owner's page of id.
func pagePath(id int64) string { return pagesPath + "/" + strconv.FormatInt(id, 10) }

// pageLinkURL is a NEW link's address, the once it is shown (the link
// CreateLink just returned holds its token; no stored link does): absolute
// under MUSTER_PAGES_PUBLIC_URL when the public pages listener is on, else
// under MUSTER_PUBLIC_URL, else a path on this server. It is the one place a
// link's address is made, so the owner's Share, an agent's tools and the
// page's screen all hand on the same one.
func (s *Server) pageLinkURL(l pages.Link) string {
	base := s.ext.PublicURL
	if s.ext.PagesPublicURL != "" {
		base = s.ext.PagesPublicURL
	}
	return strings.TrimRight(base, "/") + pages.LinkPath(l.Token)
}

// pageViewPath is the owner's own view of page id (ui.PageViewPath).
func pageViewPath(id int64) string { return ui.PageViewPath(id) }

// pageViewURL is the owner's own view of page id, absolute under
// MUSTER_PUBLIC_URL.
func (s *Server) pageViewURL(id int64) string {
	return strings.TrimRight(s.ext.PublicURL, "/") + pageViewPath(id)
}

// --- the owner's Pages screen ------------------------------------------------

// pageIDParam is the {id} path value.
func pageIDParam(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

func (s *Server) handlePagesList(w http.ResponseWriter, r *http.Request) {
	if !s.pagesOn() {
		s.writeFeatureDisabled(w, r, features.Pages)
		return
	}
	s.renderPagesList(w, r, http.StatusOK, "")
}

// THE OWNER'S PAGES SCREENS: the list (/pages) and one page's screen
// (/pages/{id}).
//
// 🔴 THE TOP LAYER IS PLAIN (the operator rule; internal/plainscan). Each
// screen says only what the owner acts on: a section with nothing in it is not
// drawn (no "No links."), a count that is zero is not drawn (no "0
// responses"), and every explanation is behind an ⓘ (ui.InfoSheet). Times are
// in words ("today at 9:14", ui.PlainWhenHTML), the exact one in a tooltip.
// TestThePagesScreensArePlain holds both to it.

func (s *Server) renderPagesList(w http.ResponseWriter, r *http.Request, code int, problem string) {
	list, err := s.ext.Pages.Pages(r.Context())
	if err != nil {
		s.logger.Printf("pages: list: %v", err)
		problem = "The pages could not be read."
	}
	shared, serr := s.ext.Pages.ShareLinks(r.Context())
	if serr != nil {
		s.logger.Printf("pages: list: share links: %v", serr)
	}
	// The screen's one explanation, behind the ⓘ beside its heading: who can
	// see a page, and the Account setting that lets agents share at once.
	acct := `<a href="` + accountPath + "#" + pagesAgentShareID + `" class="text-accent underline">Account</a>`
	how := "An agent asks you before a link goes live. You can let your agents share without asking, on " + acct + "."
	if s.pageAgentShareOn(r.Context(), requestOwner(r.Context())) {
		how = "You let your agents share without asking: an agent's link goes live at once; you are told, and can revoke it (" + acct + ")."
	}
	ctx := r.Context()
	// A team's pages (0165) are in the list beside the person's own, each
	// marked with its team's name.
	var teamIDs []int64
	seen := map[int64]bool{}
	for _, p := range list {
		if memberOnTeamPage(ctx, p.Page) && !seen[p.OwnerID] {
			seen[p.OwnerID] = true
			teamIDs = append(teamIDs, p.OwnerID)
		}
	}
	teams := s.teamNames(ctx, teamIDs)
	var b strings.Builder
	b.WriteString(`<section class="flex flex-col gap-3" data-testid="pages-page">
<div class="flex items-center gap-1"><h1 class="text-lg font-semibold text-fg">Pages</h1>` +
		ui.InfoSheetHTML(ui.InfoSheet{ID: "pages-about", Label: "About pages", Heading: "Pages",
			Lead:  "Pages your agents made. Only you can see a page until you share it; then anyone with its link can open it, without signing in. Share gives you the page's link to send." + teamPagesLead(len(teamIDs) > 0),
			Extra: g.Raw(`<p class="text-sm text-fg2">` + how + `</p>`)}) + `</div>
`)
	b.WriteString(problemNode(problem))
	if len(list) == 0 && problem == "" {
		b.WriteString(`<p class="text-sm text-fg2" data-pages-empty>No pages yet.</p>
`)
	}
	if len(list) > 0 {
		b.WriteString(`<ul class="flex flex-col divide-y divide-line" data-testid="pages">
`)
		for _, p := range list {
			v := ui.PageRowView{ID: p.ID, Title: p.Title, Maker: pageMakerName(p.Creator.Label, p.Creator.MirrorHost), Updated: p.UpdatedAt, Now: s.now(),
				Versions: p.Versions, Current: p.CurrentVersion, Pending: p.Pending, Replies: p.Responses, Live: p.LiveLinks > 0, Reusable: s.ext.Pages.LinksReusable(),
				NoRevoke: !pageRevocable(ctx, p.Page)}
			if memberOnTeamPage(ctx, p.Page) {
				v.TeamID, v.Team = p.OwnerID, teamPageLabel(teams[p.OwnerID])
			}
			if l := shared[p.ID]; l.Token != "" {
				v.ShareURL = s.pageLinkURL(l)
			}
			_ = ui.PageRow(v).Render(&b)
			b.WriteString("\n")
		}
		b.WriteString(`</ul>
`)
	}
	b.WriteString(`</section>` + ui.FloatingMenuScriptHTML())
	// secret: each row's Share carries its page's live link, a bearer
	// capability, so the screen loads no telemetry (ui.NavDocumentView.Secret).
	s.writeNavDocument(w, r, code, "Pages", ui.NavPages, true, b.String(), navWide)
}

// teamPageLabel is a team page's mark in the list and on its screen, in
// plain words: "Team: <name>".
func teamPageLabel(team string) string { return "Team: " + team }

// teamPagesLead is the list's sentence about a team's pages, when it shows
// any.
func teamPagesLead(any bool) string {
	if !any {
		return ""
	}
	return " Pages your teams' agents made are seen by everyone on that team: anyone on it can open one, answer it and share it; " +
		"only the person who started the agent that made it can delete it or change it."
}

func (s *Server) handlePageDetail(w http.ResponseWriter, r *http.Request) {
	if !s.pagesOn() {
		s.writeFeatureDisabled(w, r, features.Pages)
		return
	}
	id, ok := pageIDParam(r)
	if !ok {
		s.renderPagesList(w, r, http.StatusNotFound, "No such page.")
		return
	}
	s.renderPageDetail(w, r, id, http.StatusOK, "", "", "")
}

// renderPageDetail writes /pages/{id}: who made it, Open and Share, and —
// each only when there is one — its open links, its other versions and its
// responses; then Delete.
//
// shown, when not "", is a link's address shown ONCE — the one-off link the
// owner's Share just made on a server with no link key (handlePageShare's
// no-script answer). Only the token's hash is stored, so this response is the
// only place the owner sees it.
func (s *Server) renderPageDetail(w http.ResponseWriter, r *http.Request, id int64, code int, problem, notice, shown string) {
	ctx := r.Context()
	p, err := s.ext.Pages.Page(ctx, id)
	if err != nil {
		if !errors.Is(err, pages.ErrNotFound) {
			s.logger.Printf("pages: page %d: %v", id, err)
		}
		s.renderPagesList(w, r, http.StatusNotFound, "No such page.")
		return
	}
	versions, verr := s.ext.Pages.Versions(ctx, id)
	links, lerr := s.ext.Pages.Links(ctx, id)
	// The responses, a screen at a time, newest first: ?responses_before=<id>
	// is the next (older) screen. One more than a screen is read to know
	// whether there is one.
	before, _ := strconv.ParseInt(r.URL.Query().Get("responses_before"), 10, 64)
	resps, rerr := s.ext.Pages.ResponsesBefore(ctx, id, before, pageResponsesPerScreen+1)
	olderFrom := int64(0)
	if len(resps) > pageResponsesPerScreen {
		resps = resps[:pageResponsesPerScreen]
		olderFrom = resps[len(resps)-1].ID
	}
	if err := errors.Join(verr, lerr, rerr); err != nil {
		s.logger.Printf("pages: page %d: %v", id, err)
		problem = "Part of this page could not be read."
	}
	// The page's one reusable link (ui/page_card.go): Share hands it out, its
	// row in Links copies it, the ⋯ beside Share revokes or replaces it.
	shared, serr := s.ext.Pages.ShareLinks(ctx)
	if serr != nil {
		s.logger.Printf("pages: page %d: share link: %v", id, serr)
	}
	cur := shared[id]
	curURL := ""
	if cur.Token != "" {
		curURL = s.pageLinkURL(cur)
	}
	reusable := s.ext.Pages.LinksReusable()
	esc := html.EscapeString
	pid := strconv.FormatInt(id, 10)
	base := pagePath(id)
	now := s.now()
	// Setup details (the setting that turns reusable links on, a response's
	// stored form) are an admin's (ui.Features.Setup's predicate).
	admin := authz.IsAdmin(ctx)
	// A team's page, read by a member (0165): marked with its team, and its
	// managing controls drawn only for the one who may manage it (manage:
	// pages.MayManage — Delete, Make current) or revoke its links (revoke:
	// pages.MayRevoke — each link's Revoke, and the ⋯'s Revoke link and New
	// link, a revoke and a Share). Share is every reader's.
	manage, revoke := pageManageable(ctx, p), pageRevocable(ctx, p)
	teamNote := ""
	if memberOnTeamPage(ctx, p) {
		team := s.teamNames(ctx, []int64{p.OwnerID})[p.OwnerID]
		teamNote = `<p class="text-sm text-fg2" data-page-team="` + strconv.FormatInt(p.OwnerID, 10) + `"><span class="font-semibold text-fg">` + esc(teamPageLabel(team)) +
			`.</span> Everyone on the team sees this page, can answer it and can share it.`
		if !manage {
			teamNote += ` Only the person who started the agent that made it can delete it or change its version; a manager of the team can revoke its links.`
		}
		teamNote += "</p>\n"
	}
	sessHref, sessLabel := s.pageSessionLink(ctx, p)
	var b strings.Builder
	b.WriteString(`<section class="flex flex-col gap-6" data-testid="page-detail" data-page-id="` + pid + `">
<div class="flex flex-col gap-2">
<h1 class="min-w-0 break-words text-lg font-semibold text-fg" data-page-title>` + esc(textplain.Title(p.Title)) + `</h1>
<p class="text-sm text-fg2" data-page-meta>` + pageMadeBy(esc(pageMakerName(p.Creator.Label, p.Creator.MirrorHost)), sessHref, esc(sessLabel)) + ` · v` + strconv.Itoa(p.CurrentVersion) + `</p>
` + teamNote + `<div id="share" class="flex flex-wrap items-center gap-2 pt-1" data-page-share-box data-page-id="` + pid + `">`)
	_ = ui.PageOpenButton(id).Render(&b)
	_ = ui.PageShareQuietButton(id, p.Title, curURL, reusable).Render(&b)
	if m := ui.PageLinkMenu(id, p.Title, revoke && cur.Token != ""); m != nil {
		_ = m.Render(&b)
	}
	if !reusable {
		// No link key: each Share mints a one-off link. Why, and the setting
		// (an admin's), behind an ⓘ.
		b.WriteString(ui.InfoSheetHTML(pageShareSetupSheet(admin)))
	}
	b.WriteString(`</div>
</div>
`)
	b.WriteString(problemNode(problem))
	if notice != "" {
		b.WriteString(`<p class="text-sm text-fg" role="status" data-page-notice>` + esc(notice) + `</p>`)
	}
	if shown != "" {
		// 🔴 THE ADDRESS IS IN THE ⓘ AND ON THE COPY BUTTON, NEVER DRAWN in the
		// top layer: this answer is still the only place the owner sees it.
		b.WriteString(`<div class="` + flowsCardClass + `" data-link-new>
<span class="text-sm font-semibold text-fg">Your new link is ready. Copy it now: it is shown only this once.</span>
<div class="flex flex-wrap items-center gap-2"><button type="button" class="` + pageQuietButton + `" data-copy-text="` + esc(shown) + `" onclick="window.cgCopy &amp;&amp; window.cgCopy(this)" data-link-copy>Copy link</button>` +
			ui.InfoSheetHTML(ui.InfoSheet{ID: "page-link-new", Label: "The new link", Heading: "Your new link",
				Lead: "Anyone with this link can open the page. Copy it now:", Code: shown}) + `</div>
</div>
`)
	}
	// Links: only when the page has one. The open ones (live, or waiting for
	// the owner) are listed; the past ones (revoked, not approved) are under
	// a collapsed "Past links (n)".
	var open, past []pages.Link
	for _, l := range links {
		if l.State == pages.LinkLive || l.State == pages.LinkPending {
			open = append(open, l)
		} else {
			past = append(past, l)
		}
	}
	if len(links) > 0 {
		b.WriteString(`<section class="flex flex-col gap-1" data-page-links>
<div class="flex items-center gap-1"><h2 class="text-sm font-semibold text-fg">Links</h2>` +
			ui.InfoSheetHTML(ui.InfoSheet{ID: "page-links-about", Label: "About links", Heading: "Links",
				Lead: "Anyone with a live link can open this page and send responses. Revoking one stops it at once."}) + `</div>
<ul class="flex flex-col divide-y divide-line" data-testid="page-links">
`)
		for _, l := range open {
			lid := strconv.FormatInt(l.ID, 10)
			b.WriteString(`<li class="flex flex-wrap items-center gap-x-3 gap-y-1 py-2" data-link-id="` + lid + `" data-link-state="` + string(l.State) + `">
<span class="min-w-0 flex-1"><span class="block text-sm text-fg" data-link-state-text>` + esc(pageLinkStateText(l, p.Creator.MirrorHost)) + `</span>
<span class="block text-xs text-fg2" data-link-made>` + ui.PlainWhenTitledHTML("Made ", l.CreatedAt, now, "") + `</span></span>
`)
			// Copy: the page's Share link, the one link whose address muster
			// keeps (an older, hash-only one's was shown once, when it was
			// made). The address is on the button, never drawn.
			if l.ID == cur.ID && curURL != "" {
				b.WriteString(`<button type="button" class="` + pageQuietButton + `" data-copy-text="` + esc(curURL) + `" data-link-copy onclick="window.cgCopy &amp;&amp; window.cgCopy(this)">Copy</button>
`)
			}
			if revoke {
				b.WriteString(`<form method="POST" action="` + base + `/links/` + lid + `/revoke"><button type="submit" class="` + pageQuietButton + `" data-link-revoke>Revoke</button></form>
`)
			}
			b.WriteString(`</li>
`)
		}
		b.WriteString(`</ul>
`)
		if len(past) > 0 {
			b.WriteString(`<details data-page-links-past><summary class="press flex min-h-[44px] cursor-pointer list-none items-center text-sm text-fg2 [&::-webkit-details-marker]:hidden" data-page-links-past-summary>Past links (` + strconv.Itoa(len(past)) + `)</summary>
<ul class="flex flex-col divide-y divide-line" data-testid="page-links-past">
`)
			for _, l := range past {
				b.WriteString(`<li class="py-2" data-link-id="` + strconv.FormatInt(l.ID, 10) + `" data-link-state="` + string(l.State) + `">
<span class="block text-sm text-fg2" data-link-state-text>` + esc(pageLinkStateText(l, p.Creator.MirrorHost)) + `</span>
<span class="block text-xs text-fg2" data-link-made>` + ui.PlainWhenTitledHTML("Made ", l.CreatedAt, now, "") + `</span>
</li>
`)
			}
			b.WriteString(`</ul>
</details>
`)
		}
		b.WriteString(`</section>
`)
	}
	// Versions: only when there is more than one, newest first, each row
	// opening that version in the owner's view; an older one's ⋯ makes it the
	// current one (what the page's links show).
	if len(versions) > 1 {
		b.WriteString(`<section class="flex flex-col gap-1" data-page-versions>
<h2 class="text-sm font-semibold text-fg">Versions</h2>
<ul class="flex flex-col divide-y divide-line" data-testid="page-versions">
`)
		for _, v := range pageVersionsNewestFirst(versions) {
			vn := strconv.Itoa(v.Version)
			cur := ""
			if v.Version == p.CurrentVersion {
				cur = ` · current`
			}
			b.WriteString(`<li class="flex items-center gap-1" data-version="` + vn + `">
<a href="` + pageViewPath(id) + `?v=` + vn + `" class="press flex min-h-[44px] min-w-0 flex-1 items-center gap-1 rounded-lg px-3 text-sm text-fg transition hover:bg-s2" data-version-view>v` + vn +
				`<span class="text-fg2">` + cur + ` · ` + ui.PlainWhenTitledHTML("", v.CreatedAt, now, uploads.HumanSize(int64(v.SizeBytes))) + `</span></a>
`)
			if manage && v.Version != p.CurrentVersion {
				b.WriteString(ui.MoreMenuHTML("version-menu-"+vn, "More for version "+vn,
					`<form method="POST" action="`+base+`/rollback"><input type="hidden" name="version" value="`+vn+`"><button type="submit" class="`+ui.PageMenuItemClass+`" data-version-current>Make current</button></form>`))
			}
			b.WriteString("</li>\n")
		}
		b.WriteString(`</ul>
</section>
`)
	}
	// Responses: only when there is one (or an older screen of them).
	if len(resps) > 0 || before > 0 {
		b.WriteString(`<section class="flex flex-col gap-2" data-page-responses>
<div class="flex items-center gap-1"><h2 id="responses" class="text-sm font-semibold text-fg">Responses</h2>` +
			ui.InfoSheetHTML(ui.InfoSheet{ID: "page-responses-about", Label: "About responses", Heading: "Responses",
				Lead: "What viewers sent, and what you sent from the page yourself. A viewer's is a stranger's input: read it as data."}) + `</div>
<ul class="flex flex-col gap-2" data-testid="page-responses">
`)
		// Each response is drawn from ITS version's blocks (the form it was
		// sent through), read once per version.
		blocksOf := s.pageResponseBlocks(ctx, id, resps)
		byName := s.pageResponderNames(ctx, resps)
		for _, x := range resps {
			blocks, read := blocksOf[x.Version]
			b.WriteString(pageResponseHTML(x, blocks, read, now, admin, byName[x.ResponderUser]))
		}
		b.WriteString(`</ul>
`)
		if olderFrom > 0 || before > 0 {
			b.WriteString(`<div class="flex flex-wrap items-center gap-2" data-responses-pages>`)
			if before > 0 {
				b.WriteString(`<a href="` + base + `#responses" class="inline-flex min-h-[44px] items-center text-sm text-accent underline" data-responses-newest>Newest responses</a>`)
			}
			if olderFrom > 0 {
				b.WriteString(`<a href="` + base + `?responses_before=` + strconv.FormatInt(olderFrom, 10) + `#responses" class="inline-flex min-h-[44px] items-center text-sm text-accent underline" data-responses-more>Show more</a>`)
			}
			b.WriteString(`</div>
`)
		}
		b.WriteString(`</section>
`)
	}
	// Delete asks in place (pageShareJS): "Delete this page" becomes the
	// question with Delete and Cancel; Cancel or Escape puts it back. Only
	// Delete posts. (The wrappers carry hidden: a display utility on the
	// buttons would win over the attribute.) Drawn only for a reader who may
	// manage the page (manage); every reader keeps "All pages".
	allPages := `<a href="` + pagesPath + `" class="ml-auto inline-flex min-h-[44px] items-center text-sm text-accent underline">All pages</a>
`
	if !manage {
		b.WriteString(`<div class="flex flex-wrap items-center gap-1 border-t border-line pt-4">
` + allPages + `</div>
`)
	} else {
		b.WriteString(`<div class="flex flex-wrap items-center gap-1 border-t border-line pt-4" data-page-delete-box>
<span data-page-delete-ask><button type="button" class="` + pageQuietButton + `">Delete this page</button></span>
<div hidden data-page-delete-confirm><div class="flex flex-wrap items-center gap-2" role="group" aria-label="Delete this page?">
<span class="text-sm font-semibold text-fg">Delete this page and its responses?</span>
<form method="POST" action="` + base + `/delete"><button type="submit" class="press inline-flex min-h-[44px] items-center rounded-lg bg-st-error-bg px-4 text-sm font-semibold text-st-error-fg ring-1 ring-inset ring-st-error-fg/40" data-page-delete>Delete</button></form>
<button type="button" class="` + pageQuietButton + `" data-page-delete-cancel>Cancel</button>
</div></div>` +
			ui.InfoSheetHTML(ui.InfoSheet{ID: "page-delete-about", Label: "About deleting", Heading: "Delete this page",
				Lead: "Deletes the page, every version, link and response, and every file sent through it. It cannot be undone."}) + `
` + allPages + `</div>
`)
	}
	b.WriteString(`</section>` + ui.FloatingMenuScriptHTML())
	// secret: its Share and its Links carry the page's live link, a bearer
	// capability, so it loads no telemetry (ui.NavDocumentView.Secret).
	s.writeNavDocument(w, r, code, textplain.Title(p.Title), ui.NavPages, true, b.String(), navWide)
}

// pageVersionsNewestFirst is versions, the highest version first (the store
// reads them so; this holds the screen to it whatever the store's order).
func pageVersionsNewestFirst(versions []pages.Version) []pages.Version {
	out := slices.Clone(versions)
	slices.SortStableFunc(out, func(a, b pages.Version) int { return b.Version - a.Version })
	return out
}

// pageLinkStateText is a link's state in words: who made it (an agent that
// made it live WITHOUT asking — its owner opted in — says so), and
// what kind it is (the page's reusable Share link, or an earlier one whose
// address was shown once, when it was made).
func pageLinkStateText(l pages.Link, host string) string {
	by := ""
	if l.RequestedBy != "" {
		by = " — asked for by " + pageMakerName(l.RequestedBy, host)
	}
	if l.ByAgent && l.AskID == 0 && l.State != pages.LinkPending && l.RequestedBy != "" {
		by = " — made live by " + pageMakerName(l.RequestedBy, host) + " without asking you"
	}
	kind := ""
	switch {
	case l.Reusable && l.State == pages.LinkLive:
		kind = " · the page's Share link"
	case l.State == pages.LinkLive:
		kind = " · an earlier link, which can't be copied again"
	}
	switch l.State {
	case pages.LinkPending:
		return "Waiting for your approval" + by + " (on Asks)"
	case pages.LinkLive:
		return "Live" + by + kind
	case pages.LinkDenied:
		return "Not approved" + by
	}
	return "Revoked" + by
}

// pageShellTimes gives the owner's view's version popover its times in words,
// as the Pages screens say them: the day ("today"), which the shell's script
// redraws in the reader's own zone with its time ("today at 9:14"). list is
// handlePageView's, built from versions in the same order, one per version.
func pageShellTimes(list []pages.ShellVersion, versions []pages.Version, now time.Time) {
	for i := range list {
		if i < len(versions) && versions[i].Version == list[i].Version {
			t := versions[i].CreatedAt
			list[i].Made, list[i].AtMS = ui.PlainWhen(t, now), t.UnixMilli()
		}
	}
}

// --- the page screen's plain words (the operator rule: no address, id,
// hash, timestamp, JSON or setting name in the top layer; what is needed is
// behind an ⓘ, and a setup detail is an admin's) ------------------------------

// pageMakerName is who made a page or asked for a link, as the owner reads
// it: an agent's name as it is; a mirrored session's stored label through
// mirror.PlainStoredLabel ("work:1 @ atlas" is "work on atlas"), host being
// the page's creator host. The stored label is unchanged (the agents' tools
// give it).
func pageMakerName(label, host string) string { return mirror.PlainStoredLabel(label, host) }

// pageShareSetupKey is the setting that gives each page one reusable link.
const pageShareSetupKey = "MUSTER_CONNECTOR_ENCRYPTION_KEY"

// pageShareSetupSheet is the ⓘ beside Share on a server with no link key:
// what Share does there, and the setting, for an admin; anyone else is told
// to ask theirs (ui.InfoSheet.Withheld: the code is absent, not hidden).
func pageShareSetupSheet(admin bool) ui.InfoSheet {
	const what = "Sharing isn't set up on this server, so the page has no lasting link: Share makes a new one-off link and copies it, and you can copy each one only when it is made. Revoke the ones you no longer need under Links."
	s := ui.InfoSheet{ID: "page-share-setup", Label: "How sharing works here", Heading: "Sharing",
		Lead: what + " To keep one lasting link per page, set this on the server (it also turns on Connectors):", Code: pageShareSetupKey}
	if !admin {
		s.Lead, s.Withheld = what, true
	}
	return s
}

// pageResponseBlocks is the blocks of each version resps were sent through
// (a submit's form, an upload's label), read once per version; a version
// that does not exist is nil, and its responses are "A response to an older
// version" (pages.DescribeAnswer, UploadHeading); one that could not be read
// for another reason is absent.
func (s *Server) pageResponseBlocks(ctx context.Context, id int64, resps []pages.Response) map[int][]pages.Block {
	out := map[int][]pages.Block{}
	for _, x := range resps {
		if _, ok := out[x.Version]; ok {
			continue
		}
		v, err := s.ext.Pages.Version(ctx, id, x.Version)
		switch {
		case errors.Is(err, pages.ErrNotFound):
			out[x.Version] = nil
			continue
		case err != nil:
			// Not read now (a database error): absent, so its responses are
			// drawn from their data, never called an older version's.
			s.logger.Printf("pages: page %d: version %d for its responses: %v", id, x.Version, err)
			continue
		}
		out[x.Version] = v.Blocks
	}
	return out
}

// pageRawRunes bounds a response's raw ⓘ: past it, the rest is the full
// response's download (handlePageResponseRaw), never drawn — a stranger's
// 32 KiB submit, hundreds of them, would otherwise be the screen's bulk.
const pageRawRunes = 2048

// pageResponsesPerScreen is how many responses the page's screen draws at a
// time; "Show more" is the next, older screen.
const pageResponsesPerScreen = 50

// pageResponseRawSheet is a response's "Show raw" ⓘ, an ADMIN's only: the
// part's id, the version, the link and the exact time it came through, and
// the data as stored — at most pageRawRunes of it; cut says it was cut (the
// caller then offers the whole as a download).
func pageResponseRawSheet(x pages.Response) (sheet ui.InfoSheet, cut bool) {
	from := "link " + strconv.FormatInt(x.LinkID, 10)
	if x.ByOwner {
		from = "sent by the owner"
	}
	code := "part " + x.Component + " · version " + strconv.Itoa(x.Version) + " · " + from + " · " + ruleTime(x.CreatedAt)
	if x.Kind == pages.KindUpload {
		code += " · file " + x.UploadPublicID
	} else {
		code += " · " + string(x.Data)
	}
	lead := "Exactly what was stored, for troubleshooting:"
	if short := pages.CutRunes(code, pageRawRunes); short != code {
		code, cut, lead = short, true, "The start of what was stored, for troubleshooting (the whole of it is the download beside this):"
	}
	return ui.InfoSheet{ID: "page-response-raw-" + strconv.FormatInt(x.ID, 10), Label: "Show raw", Heading: "Raw response",
		Lead: lead, Code: code}, cut
}

// handlePageResponseRaw is GET /pages/{id}/responses/{rid}/raw: one
// response's stored form, whole, as a download — what an admin's raw ⓘ
// offers when it is too long to draw. It is the OWNER's own data, so the
// door is the owner's session, as the page's screen (row-level security:
// another owner's response is the one 404); only the LINK to it is an
// admin's, with the ⓘ it belongs to (the top-layer rule, not a boundary).
func (s *Server) handlePageResponseRaw(w http.ResponseWriter, r *http.Request) {
	id, ok := pageIDParam(r)
	rid, err := strconv.ParseInt(r.PathValue("rid"), 10, 64)
	if !s.pagesOn() || !ok || err != nil || rid <= 0 {
		http.NotFound(w, r)
		return
	}
	got, err := s.ext.Pages.Responses(r.Context(), id, rid-1, 1)
	if err != nil || len(got) != 1 || got[0].ID != rid {
		http.NotFound(w, r)
		return
	}
	x := got[0]
	body, _ := json.Marshal(map[string]any{"response_id": x.ID, "page_id": x.PageID, "link_id": x.LinkID, "by_owner": x.ByOwner,
		"version": x.Version, "part": x.Component, "kind": x.Kind, "file": x.UploadPublicID,
		"created_at": x.CreatedAt.UTC().Format(time.RFC3339), "data": json.RawMessage(orJSONNull(x.Data))})
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Disposition", `attachment; filename="response-`+strconv.FormatInt(x.ID, 10)+`.json"`)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

// orJSONNull is raw, or JSON null when it is empty.
func orJSONNull(raw []byte) []byte {
	if len(raw) == 0 {
		return []byte("null")
	}
	return raw
}

// pageResponseHTML is one response as its owner reads it: the part it came
// through (by its label), when, and its fields labelled from the version's
// own form (pages.DescribeAnswer); a file is a download. Every value is a
// stranger's words, escaped here. The stored form is an admin's ⓘ.
//
// read is false when its version could not be read now (pageResponseBlocks):
// it is then drawn from its data alone, under a plain "A response" / "A file".
//
// by is the sender's name for an owner-view response someone OTHER than the
// reader sent (a team member's, on a team's page: pageResponderNames); ""
// draws it as the reader's own ("Sent by you").
func pageResponseHTML(x pages.Response, blocks []pages.Block, read bool, now time.Time, admin bool, by string) string {
	esc := html.EscapeString
	rid := strconv.FormatInt(x.ID, 10)
	var a pages.Answer
	if x.Kind == pages.KindFeedback {
		// A Feedback pill's message: its words, whatever its version's form.
		a = pages.DescribeFeedback(x.Data)
	} else if x.Kind == pages.KindUpload {
		a.Heading = pages.UploadHeading(blocks, x.Component)
		if !read {
			a.Heading = "A file"
		}
	} else {
		a = pages.DescribeAnswer(blocks, x.Component, x.Data)
		if !read {
			a.Heading = "A response"
		}
	}
	var b strings.Builder
	// Who sent it: a viewer, through a link (which one is the raw ⓘ's), or
	// the owner from their own view of the page.
	when := ui.PlainWhenHTML("Sent ", x.CreatedAt, now)
	if x.ByOwner {
		when = `<span data-response-by="you">` + ui.PlainWhenHTML("Sent by you ", x.CreatedAt, now) + `</span>`
		if by != "" {
			when = `<span data-response-by="member" data-response-by-user="` + strconv.FormatInt(x.ResponderUser, 10) + `">` +
				ui.PlainWhenHTML("Sent by "+textplain.Title(by)+" ", x.CreatedAt, now) + `</span>`
		}
	}
	b.WriteString(`<li class="` + flowsCardClass + `" data-response-id="` + rid + `" data-response-kind="` + string(x.Kind) + `"` + pageResponseByAttr(x) + `>
<div class="flex items-start gap-2"><div class="flex min-w-0 flex-1 flex-col gap-1">
<span class="break-words text-sm font-semibold text-fg" data-response-heading>` + esc(a.Heading) + `</span>
<span class="text-xs text-fg2">` + when + `</span>
</div>`)
	if admin {
		sheet, cut := pageResponseRawSheet(x)
		b.WriteString(ui.InfoSheetHTML(sheet))
		if cut {
			b.WriteString(`<a href="` + pagePath(x.PageID) + `/responses/` + rid + `/raw" class="inline-flex min-h-[44px] shrink-0 items-center text-xs text-accent underline" data-response-raw-download>Download the full response</a>`)
		}
	}
	b.WriteString("</div>\n")
	if x.Kind == pages.KindUpload {
		b.WriteString(`<a href="` + ui.UploadPath + `/` + url.PathEscape(x.UploadPublicID) + `/file" class="inline-flex min-h-[44px] items-center self-start text-sm text-accent underline" data-response-download>Download the file</a>
`)
	} else {
		b.WriteString(`<dl class="flex flex-col gap-1" data-response-fields>`)
		for _, l := range a.Lines {
			b.WriteString(`<div><dt class="text-xs text-fg2">` + esc(l.Label) + `</dt><dd class="whitespace-pre-wrap break-words text-sm text-fg" data-response-value>` + esc(l.Value) + `</dd></div>`)
		}
		b.WriteString("</dl>\n")
	}
	b.WriteString("</li>\n")
	return b.String()
}

// pageResponderNames names each owner-view response's sender in resps who is
// NOT the reader (a team member's response on a team's page), by user id; the
// reader's own and a link's are absent (drawn "you" and "Sent").
func (s *Server) pageResponderNames(ctx context.Context, resps []pages.Response) map[int64]string {
	me := requestOwner(ctx)
	out := map[int64]string{}
	for _, x := range resps {
		if !x.ByOwner || x.ResponderUser <= 0 || x.ResponderUser == me {
			continue
		}
		if _, ok := out[x.ResponderUser]; !ok {
			out[x.ResponderUser] = s.personName(ctx, x.ResponderUser)
		}
	}
	return out
}

// pageSessionLink is where the session that made page p is NOW, for the
// signed-in owner viewing it (ctx is their request), and its short label: a
// dispatched agent's own page (agentPageHref: the agent's latest session, as
// every in-app agent link names it); a mirrored Claude Code session's
// CURRENT window, resolved from
// the session id recorded at create to wherever it runs now
// (claudeSessionEntry, claudeSessionWindow's own lookup — the resolver the
// Asks' "View in session" uses — over the viewer's OWN hosts only,
// s.ext.Mirror.Hosts(ctx)), labelled by its CURRENT name (mirrorLabel).
//
// 🔴 THE LABEL IS PLAINED HERE (textplain.Title), its one builder: pages.Shell
// and the page's screen only escape it. internal/textplain's title-sink
// ledger scans this function (apiLabelBuilders) for exactly that.
//
// 🔴 "" — no link, never a guess — when it cannot be resolved: no session
// recorded (a mirrored window's page made before 0152), the session no longer
// running on any of the owner's windows, the host offline, or an agent that
// is not the page owner's. Another owner's hosts are never searched (Hosts
// is the principal's own), so their session is never resolved.
func (s *Server) pageSessionLink(ctx context.Context, p pages.Page) (href, label string) {
	switch {
	case p.Creator.AgentID > 0:
		if s.ext.Agents == nil {
			return "", ""
		}
		a, err := s.ext.Agents.Get(ctx, p.Creator.AgentID)
		if err != nil || a.OwnerID != p.OwnerID {
			return "", ""
		}
		return s.agentPageHref(ctx, a), textplain.Title(a.Name)
	case p.Creator.MirrorHost != "" && p.Creator.MirrorSession != "":
		if s.ext.Mirror == nil {
			return "", ""
		}
		// claudeSessionEntry: claudeSessionWindow's own lookup, the window
		// the session runs in NOW.
		hosts := s.ext.Mirror.Hosts(ctx)
		e, ok := claudeSessionEntry(hosts, p.Creator.MirrorHost, p.Creator.MirrorSession)
		if !ok {
			return "", ""
		}
		href = ui.MirrorRef{Host: e.Host, Session: e.Session, Window: e.Window}.ViewPath()
		// Its name as the rest of muster names it NOW (mirrorLabel: its
		// human name, else its window and host), keyed by the session — the
		// entry is the window's, which does not carry it — and plained.
		// Nothing resolved: the name stored when the page was made.
		e.Claude, e.ClaudeSession = true, p.Creator.MirrorSession
		// Its human name, else the window as the session lists name it NOW
		// (windowDisplayName over every window: "main · window 3" when a
		// sibling would read the same) and where ("on workstation") — the
		// top layer's words, never its tmux address.
		if n := s.mirrorName(ctx, e); n.Human() {
			label = textplain.Title(n.Text)
		} else if d := windowDisplayName(hosts, mirror.Place{Host: e.Host, Addr: e.Address()}); d != "" {
			label = textplain.Title(d) + " " + mirror.PlaceWords(e.Host)
		}
		if label == "" {
			label = textplain.Title(pageMakerName(p.Creator.Label, p.Creator.MirrorHost))
		}
		return href, label
	}
	return "", ""
}

// pageMadeBy is the page screen's "by …": the session, as a link to it
// (pageSessionLink) when it resolves, else its label alone. creator and
// label come escaped; href is escaped here.
func pageMadeBy(creator, href, label string) string {
	if href == "" {
		return "by " + creator
	}
	return `by <a href="` + html.EscapeString(href) + `" class="inline-flex min-h-[44px] items-center text-accent underline" data-page-session>` + label + `</a>`
}

// pageResponseByAttr marks the owner's own response's row.
func pageResponseByAttr(x pages.Response) string {
	if x.ByOwner {
		return ` data-response-by-owner`
	}
	return ""
}

// handlePageView is GET /pages/{id}/view[?v=N]: the OWNER's own view of their
// page — the same shell and content a link's viewer gets, with the version
// chip — where what they send is recorded as theirs (pageSubmit, pageUpload:
// POST /pages/{id}/versions/{v}/…, their session). Without ?v it shows the
// page's current version; with it, version N, and a response from there
// answers N.
func (s *Server) handlePageView(w http.ResponseWriter, r *http.Request) {
	if !s.pagesOn() {
		s.writeFeatureDisabled(w, r, features.Pages)
		return
	}
	id, ok := pageIDParam(r)
	if !ok {
		s.renderPagesList(w, r, http.StatusNotFound, "No such page.")
		return
	}
	n := 0
	// ?embed=1: framed in a session's panel (ui.SessionPageEmbedPath).
	embed := r.URL.Query().Get("embed") == "1"
	if q := r.URL.Query().Get("v"); q != "" {
		var err error
		if n, err = strconv.Atoi(q); err != nil || n <= 0 {
			s.renderPagesList(w, r, http.StatusNotFound, "No such version.")
			return
		}
	}
	ctx := r.Context()
	p, err := s.ext.Pages.Page(ctx, id)
	if err != nil {
		s.renderPagesList(w, r, http.StatusNotFound, "No such page or version.")
		return
	}
	v, err := s.ext.Pages.Version(ctx, id, n)
	if err != nil {
		s.renderPagesList(w, r, http.StatusNotFound, "No such page or version.")
		return
	}
	versions, err := s.ext.Pages.Versions(ctx, id)
	if err != nil {
		s.logger.Printf("pages: page %d: versions: %v", id, err)
	}
	list := make([]pages.ShellVersion, 0, len(versions))
	for _, x := range versions {
		href := pageViewPath(id) + "?v=" + strconv.Itoa(x.Version)
		if embed {
			// A version picked in the panel's frame stays in the frame.
			href += "&embed=1"
		}
		list = append(list, pages.ShellVersion{Version: x.Version, Made: ruleTime(x.CreatedAt), Current: x.Version == p.CurrentVersion, Href: href})
	}
	now := s.now()
	pageShellTimes(list, versions, now)
	// Share, in the header, only for a reader who may MANAGE the page
	// (pages.MayManage: its owner; on a team's page its creator) — a team
	// member who may only view it is drawn none.
	var share strings.Builder
	if pageManageable(ctx, p) {
		shareURL := ""
		if shared, serr := s.ext.Pages.ShareLinks(ctx); serr != nil {
			s.logger.Printf("pages: page %d: share link: %v", id, serr)
		} else if l := shared[id]; l.Token != "" {
			shareURL = s.pageLinkURL(l)
		}
		_ = ui.PageShareButton(id, p.Title, shareURL, s.ext.Pages.LinksReusable()).Render(&share)
	}
	// The breadcrumbs: Pages, and on a team's page its team (no screen of
	// a team's own pages exists, so that crumb is words, not a link).
	crumbs := []pages.Crumb{{Label: "Pages", Href: ui.PagesPath}}
	if memberOnTeamPage(ctx, p) {
		crumbs = append(crumbs, pages.Crumb{Label: s.teamNames(ctx, []int64{p.OwnerID})[p.OwnerID]})
	}
	sessHref, sessLabel := s.pageSessionLink(ctx, p)
	if embed {
		// Framed in the session's own panel: no button back to the session
		// it is already in.
		sessHref, sessLabel = "", ""
	}
	vb := pagePath(id) + "/versions/" + strconv.Itoa(v.Version)
	serveShell(w, s.shellBrand(ctx, pages.ShellView{Title: v.Title, FrameSrc: vb + "/content", Base: vb, Nonce: pages.NewNonce(), Embed: embed, Keeps: pages.KeepKeys(v),
		Feedback: s.pageFeedbackChrome(ctx, p),
		Owner: &pages.OwnerChrome{PageID: id, Crumbs: crumbs, Updated: ui.PlainWhen(p.UpdatedAt, now), UpdatedMS: p.UpdatedAt.UnixMilli(),
			Version: v.Version, Versions: list, DetailHref: pagePath(id),
			SessionHref: sessHref, SessionLabel: sessLabel,
			ShareButton: share.String(), ShareScript: ui.PageShareScript(),
			// The version shown, by its own blocks: no html block, no bar.
			ScriptFree: !pages.HasHTML(v.Blocks)}}))
}

// handlePagePreview is GET /pages/{id}/preview[?v=N], the view's former
// address (older notifications, cards and Asks name it): a redirect to
// /pages/{id}/view, its ?v kept.
func (s *Server) handlePagePreview(w http.ResponseWriter, r *http.Request) {
	id, ok := pageIDParam(r)
	if !ok {
		s.renderPagesList(w, r, http.StatusNotFound, "No such page.")
		return
	}
	to := pageViewPath(id)
	if q := r.URL.Query().Get("v"); q != "" {
		to += "?v=" + url.QueryEscape(q)
	}
	http.Redirect(w, r, to, http.StatusFound)
}

// handlePageVersionContent is GET /pages/{id}/versions/{v}/content: the
// owner's view's content, sandboxed exactly as a link's (it is the same
// document).
func (s *Server) handlePageVersionContent(w http.ResponseWriter, r *http.Request) {
	if !s.pagesOn() {
		http.NotFound(w, r)
		return
	}
	id, ok := pageIDParam(r)
	n, err := strconv.Atoi(r.PathValue("v"))
	if !ok || err != nil || n <= 0 {
		http.NotFound(w, r)
		return
	}
	v, err := s.ext.Pages.Version(r.Context(), id, n)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	opts := s.pageRenderOptions(r.Context(), v)
	// A team member's own view of a TEAM's page takes no file yet (0165;
	// team_pages.go teamPageNoFile refuses the upload too): its file fields
	// are drawn off, saying so.
	if p, err := s.ext.Pages.Page(r.Context(), id); err == nil && memberOnTeamPage(r.Context(), p) {
		opts.Uploads, opts.UploadsOff = false, pages.TeamUploadsOff
	}
	_ = servePageContent(w, r, pages.Content(v, opts))
}

// handlePageRevokeLink is the owner's Revoke.
func (s *Server) handlePageRevokeLink(w http.ResponseWriter, r *http.Request) {
	if !s.pagesOn() {
		s.writeFeatureDisabled(w, r, features.Pages)
		return
	}
	id, ok := pageIDParam(r)
	lid, err := strconv.ParseInt(r.PathValue("link"), 10, 64)
	if !ok || err != nil || lid <= 0 {
		s.renderPagesList(w, r, http.StatusNotFound, "No such link.")
		return
	}
	l, err := s.ext.Pages.RevokeLink(r.Context(), id, lid)
	switch {
	case err == nil:
		metrics.PageShareDecisions.WithLabelValues("revoked").Inc()
		s.withdrawLinkCard(r.Context(), l)
		http.Redirect(w, r, pagePath(id), http.StatusSeeOther)
	case errors.Is(err, pages.ErrLinkNotFound), errors.Is(err, pages.ErrNotFound):
		s.renderPagesList(w, r, http.StatusNotFound, "No such link.")
	default:
		s.logger.Printf("pages: page %d: revoke link %d: %v", id, lid, err)
		s.renderPageDetail(w, r, id, http.StatusInternalServerError, "The link could not be revoked.", "", "")
	}
}

// handlePageRollback is the owner's "Show this version".
func (s *Server) handlePageRollback(w http.ResponseWriter, r *http.Request) {
	if !s.pagesOn() {
		s.writeFeatureDisabled(w, r, features.Pages)
		return
	}
	id, ok := pageIDParam(r)
	n, err := strconv.Atoi(r.PostFormValue("version"))
	if !ok || err != nil || n <= 0 {
		s.renderPagesList(w, r, http.StatusNotFound, "No such page or version.")
		return
	}
	if _, err := s.ext.Pages.SetCurrent(r.Context(), id, n); err != nil {
		if errors.Is(err, pages.ErrNotFound) {
			s.renderPagesList(w, r, http.StatusNotFound, "No such page or version.")
			return
		}
		s.logger.Printf("pages: page %d: roll back to %d: %v", id, n, err)
		s.renderPageDetail(w, r, id, http.StatusInternalServerError, "The version could not be changed.", "", "")
		return
	}
	http.Redirect(w, r, pagePath(id), http.StatusSeeOther)
}

// handlePageDelete is the owner's "Delete this page".
//
// 🔴 THE ORDER IS WHAT LEAVES NO FILE BEHIND:
//  1. every pending and live link is revoked (CloseLinks, one transaction
//     under the page row's lock): from then on no upload is RECORDED — an
//     upload already streaming finds its link revoked at AddResponse and
//     takes its own object back (dropUnrecordedUpload);
//  2. every file row of the page is revoked (its object deleted);
//  3. every object under pages/<id>/ is deleted BY LISTING THE BUCKET
//     (DeletePageFiles), so an object no row names — an upload that landed
//     in the window — goes too;
//  4. the page, with every version, link and response, is deleted.
//
// A failure before step 4 keeps the page (its links already closed), and
// Delete can be pressed again: nothing is left that no page owns.
func (s *Server) handlePageDelete(w http.ResponseWriter, r *http.Request) {
	if !s.pagesOn() {
		s.writeFeatureDisabled(w, r, features.Pages)
		return
	}
	id, ok := pageIDParam(r)
	if !ok {
		s.renderPagesList(w, r, http.StatusNotFound, "No such page.")
		return
	}
	ctx := r.Context()
	closed, err := s.ext.Pages.CloseLinks(ctx, id)
	if errors.Is(err, pages.ErrNotFound) {
		s.renderPagesList(w, r, http.StatusNotFound, "No such page.")
		return
	}
	if err != nil {
		s.logger.Printf("pages: page %d: close its links: %v", id, err)
		s.renderPageDetail(w, r, id, http.StatusInternalServerError, "The page could not be deleted.", "", "")
		return
	}
	for _, l := range closed {
		s.withdrawLinkCard(ctx, l)
	}
	files, err := s.ext.Pages.Files(ctx, id)
	if err != nil {
		s.logger.Printf("pages: page %d: list its files: %v", id, err)
		s.renderPageDetail(w, r, id, http.StatusInternalServerError, "The page could not be deleted.", "", "")
		return
	}
	if len(files) > 0 && s.ext.Uploads == nil {
		s.renderPageDetail(w, r, id, http.StatusServiceUnavailable, "The page's files cannot be deleted while file uploads are off, so the page is kept.", "", "")
		return
	}
	for _, f := range files {
		objErr, err := s.ext.Uploads.Revoke(ctx, f)
		if errors.Is(err, uploads.ErrNotFound) {
			err = nil
		}
		if err = errors.Join(err, objErr); err != nil {
			s.logger.Printf("pages: page %d: delete file %s: %v", id, f, err)
			s.renderPageDetail(w, r, id, http.StatusBadGateway, "A file of the page could not be deleted, so the page is kept. Try again.", "", "")
			return
		}
	}
	// A TEAM's page deleted by a member (its creator or a manager, whatever
	// their role): its uploads rows are the team's, which 0127 lets only an
	// operator revoke, so the loop above may have revoked none of them. 0165's
	// narrow door revokes every row under the page's prefix.
	if pg, perr := s.ext.Pages.Page(ctx, id); perr == nil && memberOnTeamPage(ctx, pg) {
		if rv, ok := s.ext.Pages.(teamPageUploadRevoker); ok {
			if _, err := rv.RevokeTeamPageUploads(ctx, id); err != nil {
				s.logger.Printf("pages: page %d: revoke the team's file rows: %v", id, err)
				s.renderPageDetail(w, r, id, http.StatusBadGateway, "The page's files could not all be deleted, so the page is kept. Try again.", "", "")
				return
			}
		}
	}
	swept := 0
	if s.ext.Uploads != nil {
		if swept, err = s.ext.Uploads.DeletePageFiles(ctx, id); err != nil {
			s.logger.Printf("pages: page %d: delete what is under its prefix: %v", id, err)
			s.renderPageDetail(w, r, id, http.StatusBadGateway, "The page's files could not all be deleted, so the page is kept. Try again.", "", "")
			return
		}
	}
	if _, err := s.ext.Pages.Delete(ctx, id); err != nil {
		if errors.Is(err, pages.ErrNotFound) {
			s.renderPagesList(w, r, http.StatusNotFound, "No such page.")
			return
		}
		s.logger.Printf("pages: page %d: delete: %v", id, err)
		s.renderPageDetail(w, r, id, http.StatusInternalServerError, "The page could not be deleted.", "", "")
		return
	}
	s.logger.Printf("pages: page %d deleted by its owner (%d file row(s); %d object(s) under its prefix)", id, len(files), swept)
	http.Redirect(w, r, pagesPath, http.StatusSeeOther)
}

// teamPageUploadRevoker is the pages store's revoke of a team page's uploads
// rows by its prefix (pages.PGStore.RevokeTeamPageUploads).
type teamPageUploadRevoker interface {
	RevokeTeamPageUploads(ctx context.Context, id int64) (int, error)
}

// pageUploadReadTimeout is how long a stranger's upload has to deliver its
// whole body: shorter than the composer's (a var so a test can shorten it).
var pageUploadReadTimeout = time.Minute

// pageSubmitReadTimeout is how long a submit's body (at most
// pageSubmitBodyMax) has to arrive. A var so a test can shorten it.
var pageSubmitReadTimeout = 15 * time.Second

// pageUploadQuota is the bytes one page's files may hold in all.
func (s *Server) pageUploadQuota() int64 {
	if s.ext.PagesUploadBytes > 0 {
		return s.ext.PagesUploadBytes
	}
	return pages.DefaultUploadBytesPerPage
}

// pageQuotaFull and pageQuotaRoom are the viewer's words for the quota.
func pageQuotaFull(quota int64) string {
	return "This page cannot take any more files: they are limited to " + uploads.HumanSize(quota) + " in all."
}

func pageQuotaRoom(quota, room int64) string {
	return "The file is too large for this page: it has room for " + uploads.HumanSize(room) + " more (its files are limited to " +
		uploads.HumanSize(quota) + " in all)."
}

// dropUnrecordedUpload takes back a file that was stored but could not be
// recorded (its link revoked meanwhile, a cap reached). 🔴 A FAILURE HERE
// LEAVES AN OBJECT UNDER pages/<id>/ THAT NO ROW NAMES: it is logged (ids
// only) and counted, and the page's own deletion lists its prefix, so that
// still removes it.
func (s *Server) dropUnrecordedUpload(ctx context.Context, p pageResponder, publicID string) {
	objErr, err := s.ext.Uploads.Revoke(context.WithoutCancel(ctx), publicID)
	if err == nil && objErr == nil {
		return
	}
	metrics.PageUploadRollbackFailures.Inc()
	s.logger.Printf("pages: %s of page %d: an unrecorded upload %s could not be taken back (row: %v; object: %v); "+
		"it is removed with its page", p, p.page, publicID, err, objErr)
}

// pageSlots is shareable pages' own upload slots (pageState.uploadSlots).
type pageSlots = uploadLimits
