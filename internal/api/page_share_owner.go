package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/features"
	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/pages"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// THE OWNER'S SHARE BUTTON (docs/pages.md#sharing; ui/page_card.go draws it).
//
// One tap shares a page: POST /pages/{id}/share answers the page's ONE
// reusable link — the one it has, or a new one — and the button hands its
// address to the native share sheet or the clipboard. Every later Share
// answers the SAME address, because the reusable link's token is kept sealed
// (internal/pages seal.go, under a key derived from
// MUSTER_CONNECTOR_ENCRYPTION_KEY) beside the hash viewers are looked up by.
// The ⋯ menu's New link (…/share/rotate) revokes it and makes another in one
// transaction; Revoke link (…/share/revoke) takes it away.
//
// Without that key the store keeps no token: Share then makes a new
// hash-only link each time, its address shown in that answer only.
//
// 🔴 THESE ARE THE OWNER'S OWN DOOR: a signed-in session, same-origin writes
// (requireOperatorWrite), and row-level security confines every statement to
// the session's own pages. No agent reaches them.
//
// Each answers JSON to the page script (Accept: application/json) and, to a
// plain form post (no script), the page's own screen.
// ---------------------------------------------------------------------------

// wantsJSON reports whether the request is the page script's.
func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

// pageShareFail answers a refused share action in the request's own form.
func (s *Server) pageShareFail(w http.ResponseWriter, r *http.Request, id int64, status int, msg string) {
	if wantsJSON(r) {
		s.writeJSON(w, status, map[string]any{"error": msg})
		return
	}
	if status == http.StatusNotFound {
		s.renderPagesList(w, r, status, msg)
		return
	}
	s.renderPageDetail(w, r, id, status, msg, "", "")
}

// pageShareStoreFail is a store error as a share action's refusal.
func (s *Server) pageShareStoreFail(w http.ResponseWriter, r *http.Request, id int64, what string, err error) {
	switch {
	case errors.Is(err, pages.ErrNotFound):
		s.pageShareFail(w, r, id, http.StatusNotFound, "No such page.")
	case errors.Is(err, pages.ErrTooMany):
		s.pageShareFail(w, r, id, http.StatusConflict, strings.TrimPrefix(err.Error(), pages.ErrTooMany.Error()+": "))
	case errors.Is(err, pages.ErrLinkUnreadable):
		s.pageShareFail(w, r, id, http.StatusConflict, "This page's link can no longer be shown (the server's encryption key changed). Use New link from ⋯.")
	case errors.Is(err, pages.ErrNoLinkKey):
		s.pageShareFail(w, r, id, http.StatusConflict, "This server keeps no reusable links (MUSTER_CONNECTOR_ENCRYPTION_KEY is unset): Share makes a new link each time.")
	default:
		s.logger.Printf("pages: page %d: %s: %v", id, what, err)
		s.pageShareFail(w, r, id, http.StatusInternalServerError, "The link could not be "+what+". Try again.")
	}
}

// pageShareOn checks the feature and the id; false when it has answered.
func (s *Server) pageShareOn(w http.ResponseWriter, r *http.Request) (int64, bool) {
	if !s.pagesOn() {
		if wantsJSON(r) {
			s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "Shareable pages are off on this server."})
		} else {
			s.writeFeatureDisabled(w, r, features.Pages)
		}
		return 0, false
	}
	id, ok := pageIDParam(r)
	if !ok {
		s.pageShareFail(w, r, 0, http.StatusNotFound, "No such page.")
		return 0, false
	}
	return id, true
}

// handlePageShare is the Share button: the page's reusable link (made on the
// first tap), or — without a link key — a new hash-only link.
func (s *Server) handlePageShare(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pageShareOn(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	p, err := s.ext.Pages.Page(ctx, id)
	if err != nil {
		s.pageShareStoreFail(w, r, id, "made", err)
		return
	}
	var l pages.Link
	created := true
	if s.ext.Pages.LinksReusable() {
		l, created, err = s.ext.Pages.ShareLink(ctx, id, "you", false)
	} else {
		l, err = s.ext.Pages.CreateLink(ctx, id, pages.LinkLive, "you", false)
	}
	if err != nil {
		s.pageShareStoreFail(w, r, id, "made", err)
		return
	}
	if created {
		metrics.PageShareDecisions.WithLabelValues("minted").Inc()
	}
	if wantsJSON(r) {
		// no-store: the answer carries a bearer capability.
		w.Header().Set("Cache-Control", "no-store")
		s.writeJSON(w, http.StatusOK, map[string]any{"page_id": p.ID, "title": p.Title, "link_id": l.ID, "url": s.pageLinkURL(l),
			"reusable": l.Reusable, "created": created})
		return
	}
	if l.Reusable {
		// The page's screen shows its reusable link's address itself.
		http.Redirect(w, r, pagePath(id)+"#share", http.StatusSeeOther)
		return
	}
	s.renderPageDetail(w, r, id, http.StatusOK, "", "", s.pageLinkURL(l))
}

// handlePageShareRotate is ⋯ → New link: the old reusable link stops working,
// a new one is made, in one transaction.
func (s *Server) handlePageShareRotate(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pageShareOn(w, r)
	if !ok {
		return
	}
	l, old, err := s.ext.Pages.RotateShareLink(r.Context(), id, "you")
	if err != nil {
		s.pageShareStoreFail(w, r, id, "made", err)
		return
	}
	metrics.PageShareDecisions.WithLabelValues("minted").Inc()
	for range old {
		metrics.PageShareDecisions.WithLabelValues("revoked").Inc()
	}
	s.logger.Printf("pages: page %d: a new link %d made by its owner; %d old link(s) revoked", id, l.ID, len(old))
	if wantsJSON(r) {
		w.Header().Set("Cache-Control", "no-store")
		s.writeJSON(w, http.StatusOK, map[string]any{"page_id": id, "link_id": l.ID, "url": s.pageLinkURL(l), "reusable": true, "revoked": len(old)})
		return
	}
	http.Redirect(w, r, pagePath(id)+"#share", http.StatusSeeOther)
}

// handlePageShareRevoke is ⋯ → Revoke link: the page's reusable link stops
// working at once. A page with none answers revoked: false.
func (s *Server) handlePageShareRevoke(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pageShareOn(w, r)
	if !ok {
		return
	}
	// The page first: a page that is not the caller's is "no such page", never
	// the same "nothing to revoke" a page of theirs with no link answers.
	if _, err := s.ext.Pages.Page(r.Context(), id); err != nil {
		s.pageShareStoreFail(w, r, id, "revoked", err)
		return
	}
	l, err := s.ext.Pages.RevokeShareLink(r.Context(), id)
	revoked := err == nil
	if err != nil && !errors.Is(err, pages.ErrLinkNotFound) {
		s.pageShareStoreFail(w, r, id, "revoked", err)
		return
	}
	if revoked {
		metrics.PageShareDecisions.WithLabelValues("revoked").Inc()
		s.logger.Printf("pages: page %d: link %d revoked by its owner", id, l.ID)
	}
	if wantsJSON(r) {
		s.writeJSON(w, http.StatusOK, map[string]any{"page_id": id, "revoked": revoked})
		return
	}
	http.Redirect(w, r, pagePath(id), http.StatusSeeOther)
}

// handlePageCard is GET /pages/{id}/card?tool=&status=&version=: one page
// card (ui.PageCard), for the chat's live stream.
func (s *Server) handlePageCard(w http.ResponseWriter, r *http.Request) {
	id, ok := pageIDParam(r)
	if !s.pagesOn() || !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	p, err := s.ext.Pages.Page(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	v := ui.PageCardView{ID: p.ID, Title: p.Title, Version: p.CurrentVersion, Kind: ui.PageCardKind(q.Get("tool"), q.Get("status"))}
	if n, err := strconv.Atoi(q.Get("version")); err == nil && n > 0 {
		v.Version = n
	}
	if v.Kind == ui.PageCardShared {
		if links, err := s.ext.Pages.Links(ctx, id); err == nil {
			for _, l := range links {
				if l.State == pages.LinkLive {
					v.SharedBy = pageLinkMaker(l)
					break
				}
			}
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := ui.PageCard(v, "").Render(w); err != nil {
		s.logger.Printf("pages: card %d: %v", id, err)
	}
}
