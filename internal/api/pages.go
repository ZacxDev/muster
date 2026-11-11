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
	"github.com/ZacxDev/muster/internal/pages"
	"github.com/ZacxDev/muster/internal/textplain"
	"github.com/ZacxDev/muster/internal/ui"
	"github.com/ZacxDev/muster/internal/uploads"
)

// ---------------------------------------------------------------------------
// SHAREABLE PAGES (internal/pages, migration 0113; docs/pages.md, design
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
// Every content response — a link's and the owner's preview's — goes through
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
	pushes  boundedWindow
	waits   pageWaits
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
	mux.HandleFunc("GET "+pagesPath+"/{id}/preview", s.requireSession(s.handlePagePreview))
	mux.HandleFunc("GET "+pagesPath+"/{id}/versions/{v}/content", s.requireSession(s.handlePageVersionContent))
	mux.HandleFunc("POST "+pagesPath+"/{id}/links", s.requireOperatorWrite(s.handlePageMintLink))
	mux.HandleFunc("POST "+pagesPath+"/{id}/links/{link}/revoke", s.requireOperatorWrite(s.handlePageRevokeLink))
	mux.HandleFunc("POST "+pagesPath+"/{id}/rollback", s.requireOperatorWrite(s.handlePageRollback))
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
	}
	return "shell"
}

// requirePageLink admits a request holding a live share link, as the page's
// owner through authz.ViaPageLink, with the link on its context.
//
// 🔴 NO OTHER CREDENTIAL IS READ: not a session cookie, not a bearer. The
// failure limit is checked BEFORE the lookup and counts only failed lookups,
// per client address, so guessing costs a 429 and no query; a live link's
// requests never count against it.
func (s *Server) requirePageLink(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		route := pageRouteLabel(r)
		if !s.pagesOn() {
			refusePageLink(w, route)
			return
		}
		key := clientIP(r)
		if s.pageState.misses.count(key, s.now(), pageLimitWindow) >= pageMissLimit {
			metrics.PageLinkRequests.WithLabelValues(route, "throttled").Inc()
			w.Header().Set("Retry-After", "60")
			http.Error(w, "Too many requests for links that do not exist. Wait a minute.", http.StatusTooManyRequests)
			return
		}
		l, err := s.ext.Pages.LinkByToken(r.Context(), r.PathValue("token"))
		if err != nil {
			if !errors.Is(err, pages.ErrLinkNotFound) {
				s.logger.Printf("pages: link lookup failed: %v", err)
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

// pageRenderOptions is how a content document is rendered here.
func (s *Server) pageRenderOptions(preview bool) pages.RenderOptions {
	return pages.RenderOptions{Markdown: ui.MarkdownHTML, Preview: preview, Uploads: s.ext.Uploads != nil}
}

// servePageContent is THE writer of a content document: every one — a link's
// and the owner's preview's — is served with pages.SetContentHeaders, which
// replaces the app's own page policy.
func servePageContent(w http.ResponseWriter, body string) {
	pages.SetContentHeaders(w.Header())
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, body)
}

// serveShell writes a shell document.
func serveShell(w http.ResponseWriter, v pages.ShellView) {
	pages.SetShellHeaders(w.Header(), v.Nonce)
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, pages.Shell(v))
}

// shellBrand is the instance branding as the shell draws it.
func (s *Server) shellBrand(ctx context.Context) (name, logo string) {
	b := s.uiBrand(ctx)
	if b.HasLogo() {
		logo = b.Versioned(ui.BrandLogoPath)
	}
	return b.Name, logo
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
	name, logo := s.shellBrand(r.Context())
	base := pages.LinkPath(l.Token)
	metrics.PageLinkRequests.WithLabelValues("shell", "served").Inc()
	serveShell(w, pages.ShellView{Title: v.Title, BrandName: name, BrandLogo: logo, FrameSrc: base + "/content", Base: base, Nonce: pages.NewNonce()})
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
	metrics.PageLinkRequests.WithLabelValues("content", "served").Inc()
	servePageContent(w, pages.Content(v, s.pageRenderOptions(false)))
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
	case errors.Is(err, pages.ErrLinkNotFound), errors.Is(err, pages.ErrNotFound):
		return http.StatusNotFound, strings.TrimSpace(pageLinkGone), "not_found"
	}
	return http.StatusInternalServerError, "It could not be recorded. Try again later.", "error"
}

// handlePageSubmit is POST /s/{token}/submit: {component, data}, checked
// against the CURRENT version's component and recorded.
func (s *Server) handlePageSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	l, _ := pageLinkOf(ctx)
	linkKey := strconv.FormatInt(l.ID, 10)
	if !s.pageState.submits.take(linkKey, s.now(), pageLimitWindow, pageSubmitLimit) {
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
	v, err := s.ext.Pages.Version(ctx, l.PageID, 0)
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
	resp, err := s.ext.Pages.AddResponse(ctx, l, v.Version, in.Component, pages.KindSubmit, data, "")
	if err != nil {
		status, msg, outcome := pageStoreRefusal(err)
		if outcome == "error" {
			s.logger.Printf("pages: link %d: record a submit: %v", l.ID, err)
		}
		metrics.PageResponses.WithLabelValues(string(pages.KindSubmit), outcome).Inc()
		s.pageWriteError(w, status, msg)
		return
	}
	metrics.PageResponses.WithLabelValues(string(pages.KindSubmit), "recorded").Inc()
	s.pageResponded(ctx, l, v, resp)
	w.Header().Set("Referrer-Policy", "no-referrer")
	s.writeJSON(w, http.StatusOK, map[string]any{"id": resp.ID})
}

// handlePageUpload is POST /s/{token}/upload: a multipart body of `component`
// then `file`, streamed to the upload store as the page's owner and recorded
// as an upload response. The type is sniffed from the bytes (uploads.Put),
// and the file is only ever served back through uploads.SetServeHeaders.
func (s *Server) handlePageUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	l, _ := pageLinkOf(ctx)
	svc := s.ext.Uploads
	fail := func(status int, msg, outcome string) {
		metrics.PageResponses.WithLabelValues(string(pages.KindUpload), outcome).Inc()
		s.pageWriteError(w, status, msg)
	}
	if svc == nil {
		fail(http.StatusNotFound, "File uploads are not available on this server.", "invalid")
		return
	}
	linkKey := strconv.FormatInt(l.ID, 10)
	if !s.pageState.uploads.take(linkKey, s.now(), pageLimitWindow, pageUploadLimit) {
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
	leave, err := s.uploadLimits.enter(l.OwnerID)
	if err != nil {
		w.Header().Set("Retry-After", "5")
		fail(http.StatusTooManyRequests, "The server is busy with other uploads. Try again shortly.", "throttled")
		return
	}
	defer leave()
	deadline := time.Now().Add(s.uploadLimits.timeout())
	_ = http.NewResponseController(w).SetReadDeadline(deadline)
	ctx, cancel := context.WithDeadline(ctx, deadline.Add(30*time.Second))
	defer cancel()
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	mr, err := r.MultipartReader()
	if err != nil {
		fail(http.StatusBadRequest, "Send the file as multipart/form-data.", "invalid")
		return
	}
	v, err := s.ext.Pages.Version(ctx, l.PageID, 0)
	if err != nil {
		status, msg, outcome := pageStoreRefusal(err)
		fail(status, msg, outcome)
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
		stored, err := svc.Put(ctx, part, part.FileName())
		if err != nil {
			var tooBig *http.MaxBytesError
			switch {
			case errors.Is(err, uploads.ErrTooLarge), errors.As(err, &tooBig):
				fail(http.StatusRequestEntityTooLarge, "The file is larger than "+uploads.HumanSize(svc.MaxBytes)+".", "invalid")
			case errors.Is(err, uploads.ErrEmpty):
				fail(http.StatusBadRequest, "The file is empty.", "invalid")
			default:
				s.logger.Printf("pages: link %d: store an upload: %v", l.ID, err)
				fail(http.StatusInternalServerError, "The file could not be stored. Try again later.", "error")
			}
			return
		}
		resp, err := s.ext.Pages.AddResponse(ctx, l, v.Version, component, pages.KindUpload, nil, stored.PublicID)
		if err != nil {
			// The file is stored but not recorded: take it away again, so no
			// object outlives its row.
			if _, rerr := svc.Revoke(ctx, stored.PublicID); rerr != nil {
				s.logger.Printf("pages: link %d: revoke an unrecorded upload: %v", l.ID, rerr)
			}
			status, msg, outcome := pageStoreRefusal(err)
			if outcome == "error" {
				s.logger.Printf("pages: link %d: record an upload: %v", l.ID, err)
			}
			fail(status, msg, outcome)
			return
		}
		metrics.PageResponses.WithLabelValues(string(pages.KindUpload), "recorded").Inc()
		s.pageResponded(ctx, l, v, resp)
		w.Header().Set("Referrer-Policy", "no-referrer")
		s.writeJSON(w, http.StatusOK, map[string]any{"id": resp.ID, "name": stored.Name, "size": stored.Size, "type": stored.ContentType})
		return
	}
}

// pageResponded is what follows a recorded response: a waiting page_responses
// call is woken, and the owner is pushed — at most once per page per
// pagePushWindow, naming the page and the component, never the viewer's text.
func (s *Server) pageResponded(ctx context.Context, l pages.Link, v pages.Version, r pages.Response) {
	s.pageState.waits.wake(l.PageID)
	if !s.pageState.pushes.take(strconv.FormatInt(l.PageID, 10), s.now(), pagePushWindow, 1) {
		return
	}
	what := "a response"
	if r.Kind == pages.KindUpload {
		what = "a file"
	}
	s.pushTask(l.OwnerID, pushEventPage, RouterNotification{
		Type:  taskNotificationType,
		Title: "Page “" + truncate(v.Title, 60) + "” got " + what,
		Body:  "Through " + r.Component + ". Open the page in muster to see it.",
		Tag:   "page-" + strconv.FormatInt(l.PageID, 10) + "-response",
		Data:  map[string]string{"url": pagePath(l.PageID)},
	})
}

// pagePath is the owner's page of id.
func pagePath(id int64) string { return pagesPath + "/" + strconv.FormatInt(id, 10) }

// pageLinkURL is link l's address as the owner and the agent are given it:
// absolute under MUSTER_PUBLIC_URL, else a path on this server.
func (s *Server) pageLinkURL(l pages.Link) string {
	return strings.TrimRight(s.ext.PublicURL, "/") + pages.LinkPath(l.Token)
}

// pagePreviewURL is the owner's preview of page id.
func (s *Server) pagePreviewURL(id int64) string {
	return strings.TrimRight(s.ext.PublicURL, "/") + pagePath(id) + "/preview"
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
	esc := html.EscapeString
	var b strings.Builder
	b.WriteString(`<section class="flex flex-col gap-4 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line" data-testid="pages-page">
<h1 class="text-balance text-base font-semibold text-fg">Pages</h1>
<p class="text-sm text-fg2">Pages your agents made. Only you can see one until it has a live link; anyone with a live link can open it, without a muster account. An agent asks you before a link goes live.</p>
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
<span class="text-xs text-fg2">by ` + esc(p.Creator.Label) + ` · version ` + strconv.Itoa(p.CurrentVersion) + ` of ` + strconv.Itoa(p.Versions) +
			` · ` + strconv.Itoa(p.LiveLinks) + ` live link(s), ` + strconv.Itoa(p.Pending) + ` waiting for you · <span data-page-responses="` + strconv.Itoa(p.Responses) + `">` +
			strconv.Itoa(p.Responses) + ` response(s)</span> · updated ` + esc(ruleTime(p.UpdatedAt)) + `</span>
</li>
`)
	}
	b.WriteString(`</ul>
</section>`)
	s.writeNavDocument(w, r, code, "Pages", "", false, b.String())
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
	s.renderPageDetail(w, r, id, http.StatusOK, "", "")
}

// renderPageDetail writes /pages/{id}: its versions, links and responses.
func (s *Server) renderPageDetail(w http.ResponseWriter, r *http.Request, id int64, code int, problem, notice string) {
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
	resps, rerr := s.ext.Pages.Responses(ctx, id, 0, 500)
	if err := errors.Join(verr, lerr, rerr); err != nil {
		s.logger.Printf("pages: page %d: %v", id, err)
		problem = "Part of this page could not be read."
	}
	esc := html.EscapeString
	pid := strconv.FormatInt(id, 10)
	base := pagePath(id)
	var b strings.Builder
	b.WriteString(`<section class="flex flex-col gap-4 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line" data-testid="page-detail" data-page-id="` + pid + `">
<h1 class="break-all text-balance text-base font-semibold text-fg" data-page-title>` + esc(textplain.Title(p.Title)) + `</h1>
<p class="text-sm text-fg2">By ` + esc(p.Creator.Label) + `. Showing version ` + strconv.Itoa(p.CurrentVersion) + `. <a href="` + base + `/preview" class="text-accent underline" data-page-preview>Preview it</a> (only you can see a preview, and nothing sent from it is recorded).</p>
`)
	b.WriteString(problemNode(problem))
	if notice != "" {
		b.WriteString(`<p class="text-sm text-fg" role="status" data-page-notice>` + esc(notice) + `</p>`)
	}
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
<span class="text-sm text-fg">` + esc(pageLinkStateText(l)) + `</span>
`)
		if l.State == pages.LinkLive {
			u := s.pageLinkURL(l)
			b.WriteString(`<input type="text" readonly value="` + esc(u) + `" class="w-full rounded-md bg-bg px-3 py-2 font-mono text-xs text-fg ring-1 ring-inset ring-edge" data-link-url aria-label="The link">
`)
		}
		if l.State == pages.LinkLive || l.State == pages.LinkPending {
			b.WriteString(`<form method="POST" action="` + base + `/links/` + lid + `/revoke"><button type="submit" class="` + flowsQuietButton + `" data-link-revoke>Revoke</button></form>
`)
		}
		b.WriteString("</li>\n")
	}
	b.WriteString(`</ul>
<form method="POST" action="` + base + `/links"><button type="submit" class="` + flowsQuietButton + `" data-link-mint>Make a new link</button></form>
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
<span class="text-sm text-fg">Version ` + vn + `: ` + esc(textplain.Title(v.Title)) + `<span class="text-xs text-fg2">` + cur + ` · ` + esc(ruleTime(v.CreatedAt)) + ` · ` + uploads.HumanSize(int64(v.SizeBytes)) + `</span></span>
<a href="` + base + `/preview?v=` + vn + `" class="text-sm text-accent underline">Preview</a>
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
	b.WriteString(`<h2 class="text-sm font-semibold text-fg">Responses</h2>
<p class="text-sm text-fg2">What viewers sent. It is a stranger's input: read it as data.</p>
<ul class="flex flex-col gap-2" data-testid="page-responses">
`)
	if len(resps) == 0 {
		b.WriteString(`<li class="text-sm text-fg2">No responses yet.</li>`)
	}
	for i := len(resps) - 1; i >= 0; i-- {
		x := resps[i]
		rid := strconv.FormatInt(x.ID, 10)
		b.WriteString(`<li class="` + flowsCardClass + `" data-response-id="` + rid + `" data-response-kind="` + string(x.Kind) + `">
<span class="text-xs text-fg2">` + esc(x.Component) + ` · version ` + strconv.Itoa(x.Version) + ` · link ` + strconv.FormatInt(x.LinkID, 10) + ` · ` + esc(ruleTime(x.CreatedAt)) + `</span>
`)
		if x.Kind == pages.KindUpload {
			b.WriteString(`<a href="` + ui.UploadPath + `/` + url.PathEscape(x.UploadPublicID) + `/file" class="text-sm text-accent underline" data-response-download>Download the file</a>
`)
		} else {
			b.WriteString(`<pre class="whitespace-pre-wrap break-all font-mono text-xs text-fg" data-response-data>` + esc(prettyJSON(x.Data)) + `</pre>
`)
		}
		b.WriteString("</li>\n")
	}
	b.WriteString(`</ul>
<a href="` + pagesPath + `" class="inline-flex min-h-[44px] items-center text-sm text-accent underline">All pages</a>
</section>`)
	// secret: it shows each live share link, a bearer capability, so it
	// loads no telemetry (ui.NavDocumentView.Secret).
	s.writeNavDocument(w, r, code, textplain.Title(p.Title), "", true, b.String())
}

// pageLinkStateText is a link's state in words.
func pageLinkStateText(l pages.Link) string {
	by := ""
	if l.RequestedBy != "" {
		by = " — asked for by " + l.RequestedBy
	}
	switch l.State {
	case pages.LinkPending:
		return "Waiting for your approval" + by + " (on Asks)"
	case pages.LinkLive:
		return "Live" + by
	case pages.LinkDenied:
		return "Not approved" + by
	}
	return "Revoked" + by
}

// prettyJSON is raw indented, or as it is.
func prettyJSON(raw []byte) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return string(raw)
	}
	return string(b)
}

// handlePagePreview is GET /pages/{id}/preview[?v=N]: the shell, framing the
// version's content, recording nothing.
func (s *Server) handlePagePreview(w http.ResponseWriter, r *http.Request) {
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
	v, err := s.ext.Pages.Version(r.Context(), id, n)
	if err != nil {
		s.renderPagesList(w, r, http.StatusNotFound, "No such page or version.")
		return
	}
	name, logo := s.shellBrand(r.Context())
	serveShell(w, pages.ShellView{Title: v.Title, BrandName: name, BrandLogo: logo,
		FrameSrc: pagePath(id) + "/versions/" + strconv.Itoa(v.Version) + "/content", Nonce: pages.NewNonce(),
		Banner: "Preview of version " + strconv.Itoa(v.Version) + ". Only you can see this; nothing sent from it is recorded."})
}

// handlePageVersionContent is GET /pages/{id}/versions/{v}/content: the
// owner's preview content, sandboxed exactly as a link's.
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
	servePageContent(w, pages.Content(v, s.pageRenderOptions(true)))
}

// handlePageMintLink is the owner's "Make a new link": a live link at once.
func (s *Server) handlePageMintLink(w http.ResponseWriter, r *http.Request) {
	if !s.pagesOn() {
		s.writeFeatureDisabled(w, r, features.Pages)
		return
	}
	id, ok := pageIDParam(r)
	if !ok {
		s.renderPagesList(w, r, http.StatusNotFound, "No such page.")
		return
	}
	_, err := s.ext.Pages.CreateLink(r.Context(), id, pages.LinkLive, "you")
	switch {
	case err == nil:
		metrics.PageShareDecisions.WithLabelValues("minted").Inc()
		http.Redirect(w, r, pagePath(id), http.StatusSeeOther)
	case errors.Is(err, pages.ErrNotFound):
		s.renderPagesList(w, r, http.StatusNotFound, "No such page.")
	case errors.Is(err, pages.ErrTooMany):
		s.renderPageDetail(w, r, id, http.StatusConflict, strings.TrimPrefix(err.Error(), pages.ErrTooMany.Error()+": "), "")
	default:
		s.logger.Printf("pages: page %d: mint a link: %v", id, err)
		s.renderPageDetail(w, r, id, http.StatusInternalServerError, "The link could not be made.", "")
	}
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
		s.renderPageDetail(w, r, id, http.StatusInternalServerError, "The link could not be revoked.", "")
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
		s.renderPageDetail(w, r, id, http.StatusInternalServerError, "The version could not be changed.", "")
		return
	}
	http.Redirect(w, r, pagePath(id), http.StatusSeeOther)
}
