package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/askattach"
	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/pages"
	"github.com/ZacxDev/muster/internal/uploads"
)

// ---------------------------------------------------------------------------
// A PAGE'S AUTHOR ASSETS (internal/pages asset.go, migration 0164;
// docs/pages.md "Images and video"): images and video the page's author
// attaches, which its markdown and html blocks show.
//
//	POST /mirror/pages/{id}/assets        a mirror host sends a session's file (mirror token)
//	GET  /s/{token}/assets/{asset}        the link's shell reads one (the token)
//	GET  /pages/{id}/versions/{v}/assets/{asset}   the owner's shell reads one (session)
//
// A dispatched agent has no way to attach one yet (a follow-up): only a
// mirrored session's host can send a file from where it is.
//
// 🔴 ONLY THE PAGE'S AUTHOR ADDS ONE. The POST acts as a pages.Creator — the
// mirror token's HOST — and every store call is scoped to the
// pages that creator made (as the page tools are), under the owner's
// row-level security: another creator's page, the same owner's included,
// reads as missing.
//
// 🔴 THE BYTES ARE CHECKED HERE, WHATEVER SENT THEM: the type SNIFFED from them
// must be one of the six (pages.AssetTypeAllowed) and the type declared
// (Content-Type) when one is; at most pages.MaxAssetBytes (guard one, its
// own 413 code asset_too_large), and within the page's room under
// pages.MaxAssetBytesPerPage (guard two, page_assets_full) — both before a
// byte is read when the length is known, and held while it streams; and
// AddAsset counts the page's total again under the page row's lock.
//
// 🔴 READ BACK ONLY BY THE SHELL, AS THE VERSION'S: each GET serves an asset
// only when the version it names refers to it (Store.VersionAsset) — a
// link's, its page's CURRENT version; the owner's, version v — so a page
// reads no other page's asset and a revoked link reads none
// (requirePageLink). The bytes go out inert (uploads.SetServeHeaders) and
// same-origin only (Cross-Origin-Resource-Policy).
//
// CACHED, BECAUSE AN ID'S BYTES NEVER CHANGE: an asset is never rewritten (a
// new file is a new id), so each response may be kept by the browser for a
// year, immutable (pageAssetCacheControl) — private, never a shared cache's.
// A link's reads are also capped per link (pageAssetLimit), as its submits
// and uploads are.
// ---------------------------------------------------------------------------

// The two asset refusals a sender branches on, each its own guard's.
const (
	pageAssetTooLargeCode = "asset_too_large"
	pageAssetsFullCode    = "page_assets_full"
)

func (s *Server) registerPageAssetRoutes(mux Mux) {
	mux.HandleFunc("GET "+pages.SharePrefix+"{token}/assets/{asset}", s.requirePageLink(s.handlePageLinkAsset))
	mux.HandleFunc("GET "+pagesPath+"/{id}/versions/{v}/assets/{asset}", s.requireSession(s.handleOwnerPageAsset))
	mux.HandleFunc("POST /mirror/pages/{id}/assets", s.requireMirrorHostToken(s.handleMirrorPageAsset))
}

// pageAssetCacheControl is every asset response's Cache-Control: an id's bytes
// never change, so the browser keeps it; private, so no shared cache does.
const pageAssetCacheControl = "private, max-age=31536000, immutable"

// pageAssetThrottled is the answer to one viewer past their read cap on a
// link (pageAssetLimit, per link and client address).
const pageAssetThrottled = "Too many files read through this link for now. Try again in a minute."

// pageAssetBytesPerLinkMinute is how many bytes of assets one link may serve
// in a minute, all its viewers together: what bounds the bandwidth a link
// holder can draw (pageAssetLimit alone allows 600 reads of 15 MiB each).
const pageAssetBytesPerLinkMinute = 2 << 30

// pageAssetByteBudget is the per-link byte budget in force (a var so a test
// can lower it).
var pageAssetByteBudget = pageAssetBytesPerLinkMinute

// pageAssetOverBudget is the answer to a link past its byte budget.
const pageAssetOverBudget = "This page's files have been downloaded a lot through this link in the last minute. Try again in a minute."

// handlePageLinkAsset is GET /s/{token}/assets/{asset}: an asset the link's
// page's CURRENT version refers to.
func (s *Server) handlePageLinkAsset(w http.ResponseWriter, r *http.Request) {
	l, _ := pageLinkOf(r.Context())
	link := strconv.FormatInt(l.ID, 10)
	throttle := func(msg string) {
		metrics.PageLinkRequests.WithLabelValues("asset", "throttled").Inc()
		w.Header().Set("Retry-After", "60")
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, msg, http.StatusTooManyRequests)
	}
	// Guard one, the reads: per link AND client address, so one viewer's
	// reads never throttle another's (a cold view of a page reads each of its
	// assets once).
	if !s.pageState.assets.take(link+"|"+clientIP(r), s.now(), pageLimitWindow, pageAssetLimit) {
		throttle(pageAssetThrottled)
		return
	}
	a, err := s.ext.Pages.VersionAsset(r.Context(), l.PageID, 0, r.PathValue("asset"))
	if err != nil || s.ext.Uploads == nil {
		if err != nil && !errors.Is(err, pages.ErrNotFound) {
			s.logger.Printf("pages: link %d: read an asset: %v", l.ID, err)
		}
		refusePageLink(w, "asset")
		return
	}
	// Guard two, the bytes: per link, every viewer together, counted before
	// the asset is served (its size is the row's), so a link cannot draw more
	// than pageAssetByteBudget a minute however many addresses read it.
	if !s.pageState.assetBytes.spend(link, s.now(), pageLimitWindow, int(a.Size), pageAssetByteBudget) {
		throttle(pageAssetOverBudget)
		return
	}
	if s.servePageAsset(w, r, a) {
		metrics.PageLinkRequests.WithLabelValues("asset", "served").Inc()
	}
}

// handleOwnerPageAsset is GET /pages/{id}/versions/{v}/assets/{asset}: an
// asset version v of the caller's page refers to.
func (s *Server) handleOwnerPageAsset(w http.ResponseWriter, r *http.Request) {
	id, ok := pageIDParam(r)
	n, err := strconv.Atoi(r.PathValue("v"))
	if !s.pagesOn() || s.ext.Uploads == nil || !ok || err != nil || n <= 0 {
		http.NotFound(w, r)
		return
	}
	a, err := s.ext.Pages.VersionAsset(r.Context(), id, n, r.PathValue("asset"))
	if err != nil {
		if !errors.Is(err, pages.ErrNotFound) {
			s.logger.Printf("pages: page %d: read an asset of version %d: %v", id, n, err)
		}
		http.NotFound(w, r)
		return
	}
	s.servePageAsset(w, r, a)
}

// servePageAsset streams asset a from the upload store, inert.
func (s *Server) servePageAsset(w http.ResponseWriter, r *http.Request, a pages.Asset) bool {
	body, size, err := s.ext.Uploads.Objects.Get(r.Context(), uploads.PageObjectKeyFor(a.PageID, a.PublicID))
	if err != nil {
		s.logger.Printf("pages: page %d: open asset %s: %v", a.PageID, loggedID(a.PublicID), err)
		http.Error(w, "The file could not be read. Try again later.", http.StatusBadGateway)
		return false
	}
	defer body.Close()
	h := w.Header()
	uploads.SetServeHeaders(h, a.ContentType, a.Filename, size)
	h.Set("Cache-Control", pageAssetCacheControl)
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
	return true
}

// handleMirrorPageAsset is POST /mirror/pages/{id}/assets?filename=<name>: a
// mirror host's muster-mirror sends one file of the calling session's
// (X-Muster-Claude-Session, which this host must list running now) as one of
// page id's assets — a page made from this HOST.
func (s *Server) handleMirrorPageAsset(w http.ResponseWriter, r *http.Request) {
	t, _ := r.Context().Value(mirrorHostCtxKey{}).(mirror.Token)
	if _, ok := s.mirrorSessionOf(r.Context(), t.Host, strings.TrimSpace(r.Header.Get(mirrorSessionHeader))); !ok {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": "name the calling session (" + mirrorSessionHeader + "): one this host lists running now"})
		return
	}
	// The label is only the asset row's created_by (scoping is the host's):
	// the host's name, never a session's display name (no name rule here).
	s.storePageAsset(w, r, pages.Creator{MirrorHost: t.Host, Label: "a session on " + t.Host})
}

// pageAssetFull is the page cap's refusal in words: room bytes left.
func pageAssetFull(room int64) string {
	if room <= 0 {
		return "this page's assets are limited to " + strconv.FormatInt(pages.MaxAssetBytesPerPage>>20, 10) + " MiB in all, and it has no room left; delete one it no longer uses (page_asset_delete)"
	}
	return "this page's assets are limited to " + strconv.FormatInt(pages.MaxAssetBytesPerPage>>20, 10) + " MiB in all; it has room for " +
		uploads.HumanSize(room) + " more"
}

// storePageAsset stores the request body as one asset of c's page {id} and
// answers askattach.PageAsset with 201.
func (s *Server) storePageAsset(w http.ResponseWriter, r *http.Request, c pages.Creator) {
	refuse := func(status int, code, msg string) {
		s.writeJSON(w, status, map[string]any{"error": msg, "code": code})
	}
	if !s.pagesOn() {
		refuse(http.StatusNotFound, "pages_off", "shareable pages are not on for this server (MUSTER_PAGES)")
		return
	}
	if why := s.askUploadsOff(); why != "" {
		refuse(http.StatusNotFound, "uploads_off", "file uploads are off on this muster (its upload store is not configured), so a page cannot carry files")
		return
	}
	id, ok := pageIDParam(r)
	if !ok {
		refuse(http.StatusNotFound, "not_found", "no such page")
		return
	}
	svc := s.ext.Uploads
	limit := min(pages.MaxAssetBytes, svc.MaxBytes)
	tooLarge := askattach.ErrAssetTooLarge.Error()
	if limit < pages.MaxAssetBytes {
		tooLarge = "the file is over " + uploads.HumanSize(limit) + ", the most this server stores in one file"
	}
	// Guard one: the asset's own cap, before a byte is read when its length
	// is known (and held while it streams, below).
	if r.ContentLength > limit {
		refuse(http.StatusRequestEntityTooLarge, pageAssetTooLargeCode, tooLarge)
		return
	}
	declared, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if declared != "" && declared != "application/octet-stream" && !pages.AssetTypeAllowed(declared) {
		refuse(http.StatusUnsupportedMediaType, "unsupported_media_type", "the file was sent as "+declared+"; a page asset is PNG, JPEG, WebP, GIF, MP4 or WebM")
		return
	}
	ctx := r.Context()
	room, err := s.ext.Pages.AssetRoom(ctx, c, id)
	if err != nil {
		s.pageAssetStoreRefusal(w, id, err)
		return
	}
	// Guard two: the page's room under its cap.
	if room <= 0 || r.ContentLength > room {
		refuse(http.StatusRequestEntityTooLarge, pageAssetsFullCode, pageAssetFull(room))
		return
	}
	owner := requestOwner(ctx)
	leave, err := s.uploadLimits.enter(owner)
	if err != nil {
		w.Header().Set("Retry-After", "5")
		refuse(http.StatusTooManyRequests, "busy", UploadBusy)
		return
	}
	defer leave()
	deadline := time.Now().Add(s.uploadLimits.timeout())
	_ = http.NewResponseController(w).SetReadDeadline(deadline)
	ctx, cancel := context.WithDeadline(ctx, deadline.Add(30*time.Second))
	defer cancel()
	max := min(limit, room)
	st, err := svc.PutPageAsset(ctx, http.MaxBytesReader(w, r.Body, limit+1), r.URL.Query().Get("filename"), id, max,
		func(ct string) bool { return pages.CheckAssetType(declared, ct) == nil })
	if err != nil {
		var refused *uploads.RefusedTypeError
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &refused):
			msg := strings.TrimPrefix(pages.CheckAssetType(declared, refused.ContentType).Error(), pages.ErrAssetType.Error()+": ")
			refuse(http.StatusUnsupportedMediaType, "unsupported_media_type", msg)
		case errors.Is(err, uploads.ErrTooLarge) && room < limit:
			refuse(http.StatusRequestEntityTooLarge, pageAssetsFullCode, pageAssetFull(room))
		case errors.Is(err, uploads.ErrTooLarge), errors.As(err, &tooBig):
			refuse(http.StatusRequestEntityTooLarge, pageAssetTooLargeCode, tooLarge)
		case errors.Is(err, uploads.ErrEmpty):
			refuse(http.StatusBadRequest, "empty", UploadEmpty)
		case isTimeout(err):
			refuse(http.StatusRequestTimeout, "too_slow", UploadTooSlow)
		default:
			s.logger.Printf("pages: page %d: store an asset: %v", id, err)
			refuse(http.StatusBadGateway, "store_failed", UploadStoreFailed)
		}
		return
	}
	a, err := s.ext.Pages.AddAsset(ctx, c, id, pages.Asset{PublicID: st.PublicID, Filename: st.Filename, ContentType: st.ContentType, Size: st.Size})
	if err != nil {
		// Stored but not recorded: take it back, so no object outlives its row
		// (the page's own deletion lists its prefix, so a failure here is
		// still removed with it).
		if objErr, rerr := svc.Revoke(context.WithoutCancel(ctx), st.PublicID); rerr != nil || objErr != nil {
			metrics.PageUploadRollbackFailures.Inc()
			s.logger.Printf("pages: page %d: an unrecorded asset %s could not be taken back (row: %v; object: %v); it is removed with its page",
				id, loggedID(st.PublicID), rerr, objErr)
		}
		s.pageAssetStoreRefusal(w, id, err)
		return
	}
	s.logger.Printf("pages: page %d: %s attached asset %s type=%s bytes=%d", id, c.Label, loggedID(a.PublicID), a.ContentType, a.Size)
	s.writeJSON(w, http.StatusCreated, pageAssetRecord(a))
}

// pageAssetStoreRefusal answers a store refusal of an asset write.
func (s *Server) pageAssetStoreRefusal(w http.ResponseWriter, id int64, err error) {
	switch {
	case errors.Is(err, pages.ErrNotFound):
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such page: " + strconv.FormatInt(id, 10) + " (attach files only to a page you made)", "code": "not_found"})
	case errors.Is(err, pages.ErrClosing):
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "page " + strconv.FormatInt(id, 10) + " is being deleted", "code": "not_found"})
	case errors.Is(err, pages.ErrTooMany):
		s.writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": strings.TrimPrefix(err.Error(), pages.ErrTooMany.Error()+": "), "code": pageAssetsFullCode})
	case errors.Is(err, pages.ErrAssetType):
		s.writeJSON(w, http.StatusUnsupportedMediaType, map[string]any{"error": strings.TrimPrefix(err.Error(), pages.ErrAssetType.Error()+": "), "code": "unsupported_media_type"})
	default:
		s.logger.Printf("pages: page %d: record an asset: %v", id, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the asset could not be recorded; try again", "code": "error"})
	}
}

// pageAssetRecord is an asset as its sender and the tools are told of it.
func pageAssetRecord(a pages.Asset) askattach.PageAsset {
	return askattach.PageAsset{ID: a.PublicID, Ref: a.Ref(), Name: a.Filename, Type: a.ContentType, Size: a.Size}
}

// pageAssetUse is how a block shows asset a.
func pageAssetUse(a pages.Asset) string {
	if pages.IsVideo(a.ContentType) {
		return `markdown: ![what it shows](` + a.Ref() + `) — html: <video src="` + a.Ref() + `" controls playsinline></video>`
	}
	return `markdown: ![what it shows](` + a.Ref() + `) — html: <img src="` + a.Ref() + `" alt="what it shows">`
}

// --- the tools ----------------------------------------------------------------

const (
	pageAssetUploadTool = askattach.PageAssetTool
	pageAssetDeleteTool = "page_asset_delete"
)

// pageAssetToolDefs are the asset tools.
func pageAssetToolDefs() []agents.ToolDef {
	id := intProp("The page's id, as page_create returned it.", 1, 1<<53)
	mib := strconv.FormatInt(pages.MaxAssetBytes>>20, 10)
	all := strconv.FormatInt(pages.MaxAssetBytesPerPage>>20, 10)
	return []agents.ToolDef{
		{Type: "function", Name: pageAssetUploadTool,
			Description: "Attach an image or video file from your working directory to one of your pages, so the page can show it at full size: " +
				"PNG, JPEG, WebP, GIF, MP4 or WebM, at most " + mib + " MiB each and " + all + " MiB per page. Returns its ref (asset:<id>): use it in a " +
				"markdown block as ![what it shows](asset:<id>) (a video plays there too), or in an html block as <img src=\"asset:<id>\"> or " +
				"<video src=\"asset:<id>\" controls>; then publish a version that uses it with page_update. It is served with the page, to whoever can " +
				"open it, and kept while any version of the page uses it. Attach after page_create: a new page has no assets yet.",
			Parameters: objSchema(map[string]any{
				"page_id":      id,
				"path":         strProp("The file, relative to your working directory (or absolute inside it)."),
				"content_type": strProp("Its type (image/png, image/jpeg, image/webp, image/gif, video/mp4, video/webm); from the file's extension when omitted. It must be what the file's bytes are."),
				"asset_id":     strProp("Set by muster-mirror once it has sent the file; leave it out."),
			}, "page_id")},
		{Type: "function", Name: pageAssetDeleteTool,
			Description: "Delete one of a page's assets that no version of the page uses any more (an asset a version uses is kept with it). Frees its room under the page's " + all + " MiB.",
			Parameters: objSchema(map[string]any{
				"page_id":  id,
				"asset_id": strProp("The asset's id (the part of its ref after asset:)."),
			}, "page_id", "asset_id")},
	}
}

// pageAssetIn reads page_asset_upload's or page_asset_delete's arguments.
func pageAssetIn(arguments string) (id int64, assetID, path string, bad string) {
	var in struct {
		PageID  int64  `json:"page_id"`
		AssetID string `json:"asset_id"`
		Path    string `json:"path"`
	}
	if err := json.Unmarshal([]byte(orEmptyJSON(arguments)), &in); err != nil || in.PageID <= 0 {
		return 0, "", "", toolErr("page_id is required: the id page_create returned")
	}
	return in.PageID, strings.TrimPrefix(strings.TrimSpace(in.AssetID), pages.AssetScheme), strings.TrimSpace(in.Path), ""
}

// pageAssetsMirrorOnly is a dispatched agent's answer from either asset tool.
const pageAssetsMirrorOnly = "attaching images and video to a page is available from mirrored sessions only, for now; an image on your page must be inline (a data: URI)"

// toolPageAssetUpload is page_asset_upload as it reaches muster: with
// asset_id, the file muster-mirror already sent (POST
// /mirror/pages/{id}/assets) — reported, once it is checked to be an asset of
// a page this caller made. With only a path, nothing here can read the file:
// the session's muster-mirror is older than page assets.
//
// A dispatched agent is not offered the asset tools (mirrorTools offers them,
// agentTools does not); one that calls them anyway is told
// pageAssetsMirrorOnly.
func (s *Server) toolPageAssetUpload(ctx context.Context, caller toolCaller, c pages.Creator, arguments string) string {
	id, assetID, path, bad := pageAssetIn(arguments)
	if bad != "" {
		return bad
	}
	if assetID == "" {
		if path == "" {
			return toolErr("path is required: the file to attach, in your working directory")
		}
		if why := s.askUploadsOff(); why != "" {
			return toolErr("file uploads are off on this muster (its upload store is not configured), so a page cannot carry files; use data: URIs")
		}
		return toolErr("this host's muster-mirror did not send the file (it is older than page assets): update muster-mirror on this host")
	}
	a, err := s.ext.Pages.AssetFor(ctx, c, id, assetID)
	if err != nil {
		if errors.Is(err, pages.ErrNotFound) {
			if _, perr := s.ext.Pages.PageFor(ctx, c, id); perr != nil {
				return pageToolRefusal("read", id, perr)
			}
			return toolErr("no such asset on page %d: %s", id, assetID)
		}
		s.logger.Printf("pages: page %d: read asset %s: %v", id, loggedID(assetID), err)
		return toolErr("the asset could not be read")
	}
	out := map[string]any{"page_id": id, "asset_id": a.PublicID, "ref": a.Ref(), "name": a.Filename, "type": a.ContentType, "size": a.Size,
		"use":  pageAssetUse(a),
		"next": "Publish a version that uses " + a.Ref() + " with page_update{page_id: " + strconv.FormatInt(id, 10) + ", blocks}; until a version uses it, no one sees it."}
	return toolJSON(out)
}

// toolPageAssetDelete deletes an asset no version uses, and its bytes.
func (s *Server) toolPageAssetDelete(ctx context.Context, c pages.Creator, arguments string) string {
	id, assetID, _, bad := pageAssetIn(arguments)
	if bad != "" {
		return bad
	}
	if assetID == "" {
		return toolErr("asset_id is required")
	}
	a, err := s.ext.Pages.DeleteAsset(ctx, c, id, assetID)
	switch {
	case errors.Is(err, pages.ErrAssetInUse):
		return toolErr("%s", strings.TrimPrefix(err.Error(), pages.ErrAssetInUse.Error()+": "))
	case errors.Is(err, pages.ErrNotFound):
		if _, perr := s.ext.Pages.PageFor(ctx, c, id); perr != nil {
			return pageToolRefusal("read", id, perr)
		}
		return toolErr("no such asset on page %d: %s", id, assetID)
	case err != nil:
		s.logger.Printf("pages: page %d: delete asset %s: %v", id, loggedID(assetID), err)
		return toolErr("the asset could not be deleted")
	}
	if svc := s.ext.Uploads; svc != nil {
		if objErr, rerr := svc.Revoke(ctx, a.PublicID); rerr != nil || objErr != nil {
			// The asset's row is gone, so nothing serves it. Its upload row
			// (if the revoke failed) and its bytes go with the page: its delete
			// revokes every live upload under the page's prefix (Store.Files)
			// and sweeps the prefix.
			s.logger.Printf("pages: page %d: asset %s deleted, its bytes not (row: %v; object: %v)", id, loggedID(a.PublicID), rerr, objErr)
		}
	}
	return toolJSON(map[string]any{"page_id": id, "asset_id": a.PublicID, "deleted": true})
}

// pageAssetsView is page_get's view of a page's assets.
func pageAssetsView(list []pages.Asset) ([]map[string]any, int64) {
	out := make([]map[string]any, 0, len(list))
	var total int64
	for _, a := range list {
		total += a.Size
		out = append(out, map[string]any{"asset_id": a.PublicID, "ref": a.Ref(), "name": a.Filename, "type": a.ContentType, "size": a.Size})
	}
	return out, total
}
