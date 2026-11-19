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
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/features"
	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/pages"
	"github.com/ZacxDev/muster/internal/textplain"
	"github.com/ZacxDev/muster/internal/ui"
	"github.com/ZacxDev/muster/internal/uploads"
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
	// then 429 without touching the database.
	pageMissLimit = 60
	// pageSubmitLimit and pageUploadLimit: per link per pageLimitWindow.
	pageSubmitLimit = 20
	pageUploadLimit = 10
	// pageAssetLimit: a link's reads of its page's images and video
	// (page_assets.go). A view reads each asset once, and a repeat view comes
	// from the browser's cache, so this is room for many views of a page of
	// 200 assets and a bound on a link holder looping.
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
	assets  boundedWindow
	pushes  boundedWindow
	waits   pageWaits
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
	case strings.Contains(r.URL.Path, "/assets/"):
		return "asset"
	}
	return "shell"
}

// requirePageLink admits a request holding a live share link, as the page's
// owner through authz.ViaPageLink, with the link on its context.
//
// 🔴 NO OTHER CREDENTIAL IS READ: not a session cookie, not a bearer. The
// failure limit counts only failed lookups, per client address, and is
// applied only to a token that does NOT resolve: the lookup comes first, so
// an address that is throttled (a shared NAT, one guesser behind it) still
// opens a live link, and only its misses are answered 429.
func (s *Server) requirePageLink(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		route := pageRouteLabel(r)
		if !s.pagesOn() {
			refusePageLink(w, route)
			return
		}
		key := clientIP(r)
		l, err := s.ext.Pages.LinkByToken(r.Context(), r.PathValue("token"))
		if err != nil {
			if !errors.Is(err, pages.ErrLinkNotFound) {
				s.logger.Printf("pages: link lookup failed: %v", err)
			}
			if s.pageState.misses.count(key, s.now(), pageLimitWindow) >= pageMissLimit {
				metrics.PageLinkRequests.WithLabelValues(route, "throttled").Inc()
				w.Header().Set("Retry-After", "60")
				http.Error(w, "Too many requests for links that do not exist. Wait a minute.", http.StatusTooManyRequests)
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
	o := pages.RenderOptions{Markdown: ui.MarkdownHTML, Uploads: s.ext.Uploads != nil}
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

// serveShell writes a shell document.
func serveShell(w http.ResponseWriter, v pages.ShellView) {
	pages.SetShellHeaders(w.Header(), v.Nonce)
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
	return v
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
	serveShell(w, s.shellBrand(r.Context(), pages.ShellView{Title: v.Title, FrameSrc: base + "/content", Base: base, Nonce: pages.NewNonce()}))
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
func (s *Server) recordPageResponse(ctx context.Context, p pageResponder, v int, component string, kind pages.ResponseKind, data []byte, uploadID string) (pages.Response, error) {
	if p.owner {
		return s.ext.Pages.AddOwnerResponse(ctx, p.page, v, component, kind, data, uploadID)
	}
	return s.ext.Pages.AddResponse(ctx, p.link, v, component, kind, data, uploadID)
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
	r.Body = http.MaxBytesReader(w, r.Body, pageSubmitBodyMax)
	var in struct {
		Component string          `json:"component"`
		Data      json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		metrics.PageResponses.WithLabelValues(string(pages.KindSubmit), "invalid").Inc()
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
	if r.Kind == pages.KindUpload {
		what = "a file"
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
// under MUSTER_PUBLIC_URL, else a path on this server.
func (s *Server) pageLinkURL(l pages.Link) string {
	return strings.TrimRight(s.ext.PublicURL, "/") + pages.LinkPath(l.Token)
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
	esc := html.EscapeString
	now := s.now()
	how := "An agent asks you before a link goes live (you can let your agents share without asking, on <a href=\"" + accountPath + "#" + pagesAgentShareID + "\" class=\"text-accent underline\">Account</a>)."
	if s.pageAgentShareOn(r.Context(), requestOwner(r.Context())) {
		how = "You let your agents share without asking: an agent's link goes live at once; you are told, and can revoke it (<a href=\"" + accountPath + "#" + pagesAgentShareID + "\" class=\"text-accent underline\">Account</a>)."
	}
	var b strings.Builder
	b.WriteString(`<section class="flex flex-col gap-4 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line" data-testid="pages-page">
<h1 class="text-balance text-base font-semibold text-fg">Pages</h1>
<p class="text-sm text-fg2">Pages your agents made. Only you can see a page until you share it; then anyone with its link can open it, without signing in. ` + how + ` Share gives you the page's link to send.</p>
`)
	b.WriteString(problemNode(problem))
	b.WriteString(`<ul class="flex flex-col gap-2" data-testid="pages">
`)
	if len(list) == 0 && problem == "" {
		b.WriteString(`<li class="text-sm text-fg2">No pages yet.</li>`)
	}
	for _, p := range list {
		id := strconv.FormatInt(p.ID, 10)
		b.WriteString(`<li class="` + flowsCardClass + `" data-page-id="` + id + `">
<a href="` + pagePath(p.ID) + `" class="break-all text-sm font-semibold text-accent underline" data-page-title>` + esc(textplain.Title(p.Title)) + `</a>
<span class="text-xs text-fg2">by ` + esc(pageMakerName(p.Creator.Label, p.Creator.MirrorHost)) + ` · version ` + strconv.Itoa(p.CurrentVersion) + ` of ` + strconv.Itoa(p.Versions) +
			` · ` + pageLinksWords(p.LiveLinks, p.Pending) + ` · <span data-page-responses="` + strconv.Itoa(p.Responses) + `">` +
			pageCount(p.Responses, "response", "responses") + `</span> · ` + ui.PlainWhenHTML("updated ", p.UpdatedAt, now) + `</span>
<div class="flex flex-wrap items-center gap-2">` + s.pageShareControls(p.ID, p.Title, shared[p.ID]) + `</div>
</li>
`)
	}
	b.WriteString(`</ul>
</section>`)
	// secret: each row's Share button carries its page's live link, a bearer
	// capability, so the screen loads no telemetry (ui.NavDocumentView.Secret).
	s.writeNavDocument(w, r, code, "Pages", ui.NavPages, true, b.String())
}

// pageShareControls is a page's Share button and ⋯ menu (ui/page_card.go),
// carrying its live reusable link's address when it has one (l).
func (s *Server) pageShareControls(id int64, title string, l pages.Link) string {
	url := ""
	if l.Token != "" {
		url = s.pageLinkURL(l)
	}
	var b strings.Builder
	_ = ui.PageShareButton(id, title, url, s.ext.Pages.LinksReusable()).Render(&b)
	_ = ui.PageShareMenu(id, title, false).Render(&b)
	return b.String()
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

// renderPageDetail writes /pages/{id}: its versions, links and responses.
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
	esc := html.EscapeString
	pid := strconv.FormatInt(id, 10)
	base := pagePath(id)
	now := s.now()
	// Setup details (the setting that turns reusable links on, a response's
	// stored form) are an admin's (ui.Features.Setup's predicate).
	admin := authz.IsAdmin(ctx)
	sessHref, sessLabel := s.pageSessionLink(ctx, p)
	var b strings.Builder
	b.WriteString(`<section class="flex flex-col gap-4 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line" data-testid="page-detail" data-page-id="` + pid + `">
<h1 class="break-all text-balance text-base font-semibold text-fg" data-page-title>` + esc(textplain.Title(p.Title)) + `</h1>
<p class="text-sm text-fg2">` + pageMadeBy(esc(pageMakerName(p.Creator.Label, p.Creator.MirrorHost)), sessHref, esc(sessLabel)) + ` Showing version ` + strconv.Itoa(p.CurrentVersion) + `, ` + ui.PlainWhenHTML("updated ", p.UpdatedAt, now) + `. <a href="` + pageViewPath(id) + `" class="text-accent underline" data-page-view>Open the page</a> (what you send from it is recorded as yours).</p>
`)
	b.WriteString(problemNode(problem))
	if notice != "" {
		b.WriteString(`<p class="text-sm text-fg" role="status" data-page-notice>` + esc(notice) + `</p>`)
	}
	if shown != "" {
		// 🔴 THE ADDRESS IS IN THE ⓘ AND ON THE COPY BUTTON, NEVER DRAWN in the
		// top layer: this answer is still the only place the owner sees it.
		b.WriteString(`<div class="` + flowsCardClass + `" data-link-new>
<span class="text-sm font-semibold text-fg">Your new link is ready — copy it now. It is shown only this once; if you lose it, make a new one and revoke this one.</span>
<div class="flex flex-wrap items-center gap-2"><button type="button" class="` + pageQuietButton + `" data-copy-text="` + esc(shown) + `" onclick="window.cgCopy &amp;&amp; window.cgCopy(this)" data-link-copy>Copy link</button>` +
			ui.InfoSheetHTML(ui.InfoSheet{ID: "page-link-new", Label: "The new link", Heading: "Your new link",
				Lead: "Anyone with this link can open the page. Copy it now:", Code: shown}) + `</div>
</div>
`)
	}
	// Share: the page's one reusable link, one tap (ui/page_card.go).
	shared, serr := s.ext.Pages.ShareLinks(ctx)
	if serr != nil {
		s.logger.Printf("pages: page %d: share link: %v", id, serr)
	}
	cur := shared[id]
	b.WriteString(`<section id="share" class="flex flex-col gap-2" data-page-share-box data-page-id="` + pid + `">
<h2 class="text-sm font-semibold text-fg">Share</h2>
`)
	curURL := ""
	if cur.Token != "" {
		curURL = s.pageLinkURL(cur)
	}
	switch {
	case cur.Token != "":
		b.WriteString(`<p class="text-sm text-fg2">Anyone with its link can open the page. Share gives you the same link every time; New link (⋯) replaces it, and the old one stops working.</p>
`)
	case s.ext.Pages.LinksReusable():
		b.WriteString(`<p class="text-sm text-fg2">No link yet. Share makes the page's link — the same one every time after — and hands it to your phone's share sheet, or copies it.</p>
`)
	default:
		// The ⓘ is the paragraph's sibling: a popover sheet is not phrasing
		// content, so it may not sit inside the <p>.
		b.WriteString(`<div class="flex items-start gap-1"><p class="min-w-0 flex-1 text-sm text-fg2" data-page-share-setup>Sharing isn't set up on this server, so the page has no lasting link. Share makes a new one-off link and copies it; you can copy each one only when it is made. Revoke the ones you no longer need below.</p>` +
			ui.InfoSheetHTML(pageShareSetupSheet(admin)) + `</div>
`)
	}
	b.WriteString(`<div class="flex flex-wrap items-center gap-2">` + s.pageShareControls(id, p.Title, cur))
	// With a link key: Copy link (shown by the page script only where the
	// browser has a share sheet — elsewhere Share itself copies) and the
	// link's ⓘ, drawn hidden while the page has no link so the script can
	// fill it on the first Share and hide it again on Revoke.
	if s.ext.Pages.LinksReusable() {
		var c strings.Builder
		_ = ui.PageCopyButton(id, p.Title, curURL, true).Render(&c)
		_ = ui.PageLinkSheet(id, curURL).Render(&c)
		b.WriteString(c.String())
	}
	b.WriteString(`</div>
</section>
`)
	// Links.
	b.WriteString(`<h2 class="text-sm font-semibold text-fg">Links</h2>
<p class="text-sm text-fg2">Anyone with a live link can open this page and send responses. Revoking one stops it at once.</p>
<ul class="flex flex-col gap-2" data-testid="page-links">
`)
	if len(links) == 0 {
		b.WriteString(`<li class="text-sm text-fg2">No links.</li>`)
	}
	for _, l := range links {
		lid := strconv.FormatInt(l.ID, 10)
		b.WriteString(`<li class="` + flowsCardClass + `" data-link-id="` + lid + `" data-link-state="` + string(l.State) + `">
<div class="flex items-start gap-2"><div class="flex min-w-0 flex-1 flex-col gap-1">
<span class="text-sm text-fg">` + esc(pageLinkStateText(l, p.Creator.MirrorHost)) + `</span>
<span class="text-xs text-fg2" data-link-made>` + ui.PlainWhenHTML("Made ", l.CreatedAt, now) + `</span>
</div>` + ui.InfoSheetHTML(pageLinkDetailSheet(l)) + `</div>
`)
		if l.State == pages.LinkLive || l.State == pages.LinkPending {
			b.WriteString(`<form method="POST" action="` + base + `/links/` + lid + `/revoke"><button type="submit" class="` + flowsQuietButton + `" data-link-revoke>Revoke</button></form>
`)
		}
		b.WriteString("</li>\n")
	}
	b.WriteString(`</ul>
`)
	// Versions.
	b.WriteString(`<h2 class="text-sm font-semibold text-fg">Versions</h2>
<ul class="flex flex-col gap-2" data-testid="page-versions">
`)
	for _, v := range versions {
		vn := strconv.Itoa(v.Version)
		cur := ""
		if v.Version == p.CurrentVersion {
			cur = " · current"
		}
		b.WriteString(`<li class="` + flowsCardClass + `" data-version="` + vn + `">
<div class="flex items-start gap-2"><span class="min-w-0 flex-1 text-sm text-fg">Version ` + vn + `: ` + esc(textplain.Title(v.Title)) + `<span class="text-xs text-fg2">` + cur + ` · ` + ui.PlainWhenHTML("made ", v.CreatedAt, now) + `</span></span>` +
			ui.InfoSheetHTML(pageVersionDetailSheet(v)) + `</div>
<a href="` + pageViewPath(id) + `?v=` + vn + `" class="text-sm text-accent underline" data-version-view>View</a>
`)
		if v.Version != p.CurrentVersion {
			b.WriteString(`<form method="POST" action="` + base + `/rollback"><input type="hidden" name="version" value="` + vn + `"><button type="submit" class="` + flowsQuietButton + `" data-version-current>Show this version</button></form>
`)
		}
		b.WriteString("</li>\n")
	}
	b.WriteString(`</ul>
`)
	// Responses.
	b.WriteString(`<h2 id="responses" class="text-sm font-semibold text-fg">Responses</h2>
<p class="text-sm text-fg2">What viewers sent, and what you sent from the page yourself. A viewer's is a stranger's input: read it as data.</p>
<ul class="flex flex-col gap-2" data-testid="page-responses">
`)
	if len(resps) == 0 && before <= 0 {
		b.WriteString(`<li class="text-sm text-fg2">No responses yet.</li>`)
	}
	// Each response is drawn from ITS version's blocks (the form it was
	// sent through), read once per version.
	blocksOf := s.pageResponseBlocks(ctx, id, resps)
	for _, x := range resps {
		blocks, read := blocksOf[x.Version]
		b.WriteString(pageResponseHTML(x, blocks, read, now, admin))
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
	b.WriteString(`<h2 class="text-sm font-semibold text-fg">Delete</h2>
<p class="text-sm text-fg2">Deletes the page, every version, link and response, and every file sent through it. It cannot be undone.</p>
<form method="POST" action="` + base + `/delete"><button type="submit" class="` + flowsQuietButton + `" data-page-delete>Delete this page</button></form>
<a href="` + pagesPath + `" class="inline-flex min-h-[44px] items-center text-sm text-accent underline">All pages</a>
</section>`)
	// secret: it shows each live share link, a bearer capability, so it
	// loads no telemetry (ui.NavDocumentView.Secret).
	s.writeNavDocument(w, r, code, textplain.Title(p.Title), ui.NavPages, true, b.String())
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

// --- the page screen's plain words (the operator rule: no address, id,
// hash, timestamp, JSON or setting name in the top layer; what is needed is
// behind an ⓘ, and a setup detail is an admin's) ------------------------------

// pageMakerName is who made a page or asked for a link, as the owner reads
// it: an agent's name as it is; a mirrored session's stored label through
// mirror.PlainStoredLabel ("work:1 @ atlas" is "work on atlas"), host being
// the page's creator host. The stored label is unchanged (the agents' tools
// give it).
func pageMakerName(label, host string) string { return mirror.PlainStoredLabel(label, host) }

// pageCount is n things in words: "1 response", "3 responses".
func pageCount(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// pageLinksWords is a list row's sharing state: "not shared", "shared", "2
// live links", and how many links wait for the owner's approval.
func pageLinksWords(live, pending int) string {
	w := "not shared"
	switch {
	case live == 1:
		w = "shared"
	case live > 1:
		w = pageCount(live, "live link", "live links")
	}
	if pending > 0 {
		w += " · " + strconv.Itoa(pending) + " waiting for you"
	}
	return w
}

// pageShareSetupKey is the setting that gives each page one reusable link.
const pageShareSetupKey = "MUSTER_CONNECTOR_ENCRYPTION_KEY"

// pageShareSetupSheet is the ⓘ beside "Sharing isn't set up on this server":
// the setting, for an admin; anyone else is told to ask theirs
// (ui.InfoSheet.Withheld: the code is absent, not hidden).
func pageShareSetupSheet(admin bool) ui.InfoSheet {
	s := ui.InfoSheet{ID: "page-share-setup", Label: "How to set up sharing", Heading: "Set up sharing", Inline: true,
		Lead: "To keep one lasting link per page, set this on the server (it also turns on Connectors):", Code: pageShareSetupKey}
	if !admin {
		s.Lead, s.Withheld = "Sharing links that last is not set up on this server.", true
	}
	return s
}

// pageLinkDetailSheet is a link row's ⓘ: its exact label (when it was made, in
// UTC, and its token hash's first 8 hex digits — pages.Link.Label, which
// tells two links apart and opens nothing).
func pageLinkDetailSheet(l pages.Link) ui.InfoSheet {
	return ui.InfoSheet{ID: "page-link-detail-" + strconv.FormatInt(l.ID, 10), Label: "Details of this link", Heading: "This link",
		Lead: "What tells this link apart from the page's others:", Code: l.Label()}
}

// pageVersionDetailSheet is a version row's ⓘ: its exact time and its size.
func pageVersionDetailSheet(v pages.Version) ui.InfoSheet {
	vn := strconv.Itoa(v.Version)
	return ui.InfoSheet{ID: "page-version-detail-" + vn, Label: "Details of version " + vn, Heading: "Version " + vn,
		Lead: "When it was made, and its size:", Code: ruleTime(v.CreatedAt) + " · " + uploads.HumanSize(int64(v.SizeBytes))}
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
func pageResponseHTML(x pages.Response, blocks []pages.Block, read bool, now time.Time, admin bool) string {
	esc := html.EscapeString
	rid := strconv.FormatInt(x.ID, 10)
	var a pages.Answer
	if x.Kind == pages.KindUpload {
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
		b.WriteString(`<a href="` + ui.UploadPath + `/` + url.PathEscape(x.UploadPublicID) + `/file" class="text-sm text-accent underline" data-response-download>Download the file</a>
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

// pageMadeBy is the page screen's "Made by …": the session, as a link to it
// (pageSessionLink) when it resolves, else its label alone. creator and
// label come escaped; href is escaped here.
func pageMadeBy(creator, href, label string) string {
	if href == "" {
		return "Made by " + creator + "."
	}
	return `Made by <a href="` + html.EscapeString(href) + `" class="text-accent underline" data-page-session>` + label + `</a>.`
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
		list = append(list, pages.ShellVersion{Version: x.Version, Made: ruleTime(x.CreatedAt), Current: x.Version == p.CurrentVersion,
			Href: pageViewPath(id) + "?v=" + strconv.Itoa(x.Version)})
	}
	shareURL := ""
	if shared, serr := s.ext.Pages.ShareLinks(ctx); serr != nil {
		s.logger.Printf("pages: page %d: share link: %v", id, serr)
	} else if l := shared[id]; l.Token != "" {
		shareURL = s.pageLinkURL(l)
	}
	var share strings.Builder
	_ = ui.PageShareButton(id, p.Title, shareURL, s.ext.Pages.LinksReusable()).Render(&share)
	sessHref, sessLabel := s.pageSessionLink(ctx, p)
	vb := pagePath(id) + "/versions/" + strconv.Itoa(v.Version)
	serveShell(w, s.shellBrand(ctx, pages.ShellView{Title: v.Title, FrameSrc: vb + "/content", Base: vb, Nonce: pages.NewNonce(),
		Owner: &pages.OwnerChrome{Version: v.Version, Versions: list, DetailHref: pagePath(id),
			SessionHref: sessHref, SessionLabel: sessLabel,
			ShareButton: share.String(), ShareScript: ui.PageShareScript()}}))
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
	_ = servePageContent(w, r, pages.Content(v, s.pageRenderOptions(r.Context(), v)))
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

// pageUploadReadTimeout is how long a stranger's upload has to deliver its
// whole body: shorter than the composer's (a var so a test can shorten it).
var pageUploadReadTimeout = time.Minute

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
