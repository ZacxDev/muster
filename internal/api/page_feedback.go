package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/pages"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// FEEDBACK ON A PAGE (migration 0191; docs/pages.md "Feedback";
// internal/pages feedback.go draws the pill).
//
// POST /s/{token}/feedback (a link's viewer: the link is the credential, as
// every share-link route) and POST /pages/{id}/versions/{v}/feedback (the
// page's owner from their own view: their session and muster's own Origin,
// requireOperatorWrite), each {text}:
//
//   - refused past pageFeedbackLimit messages per responder (a link, or the
//     owner on one page) per pageLimitWindow (429), past
//     pages.MaxFeedbackPerLink messages through one link (409; a budget of
//     its own, so feedback never uses up the page form's responses), over
//     pages.MaxFeedbackRunes characters (413, never cut) or blank (400) — and
//     everything a link's submit is refused for: a link that is revoked,
//     pending, denied or expired (the one 404, requirePageLink and the store's
//     live check under the link's lock);
//   - STORED as one of the page's responses (pages.KindFeedback,
//     pages.FeedbackComponent, {"text": …}) through the responder's own store
//     call, so page_responses reads it whatever happens next;
//   - and FILED as one page_feedback wake for the session that made the page
//     (pageFeedbackWake: the same session a response wakes) — one per
//     message, never folded into a count. The owner's message is their words
//     (held, like any, for a fresh sign-in); a link viewer's is a stranger's,
//     fenced as untrusted on the line (renderWake).
//
// The answer says which (pageFeedbackDelivery): sent, held for a sign-in, or
// not reachable — decided by fileWakeRow itself, the wakes' ONE filer, never
// by a second reading of its rules.
// ---------------------------------------------------------------------------

// pageFeedbackLimit is how many feedback messages one responder (a link, or
// the owner on one page) may send per pageLimitWindow (a minute): a burst of
// that many, then 429 until the window turns. A var so a test can lower it.
var pageFeedbackLimit = 5

// pageFeedbackBodyMax bounds a feedback request's body: a message of
// pages.MaxFeedbackRunes characters, JSON-escaped at its worst (\uXXXX, six
// bytes each), and its framing.
const pageFeedbackBodyMax = pages.MaxFeedbackRunes*6 + 1<<10

// Where a message went (the answer's "delivery").
const (
	pageFeedbackSent        = "sent"
	pageFeedbackHeld        = "held"
	pageFeedbackUnreachable = "unreachable"
)

// The answer's words for each.
const (
	PageFeedbackSentText        = "Sent to the session that made this page."
	PageFeedbackHeldText        = "Saved. It goes to the session that made this page once you sign in again."
	PageFeedbackUnreachableText = "Saved with this page's responses, but the session that made this page isn't reachable, so it was not sent to it."
	// PageFeedbackFullText: the link is at pages.MaxFeedbackPerLink.
	PageFeedbackFullText = "This link has sent as much feedback as one link may. Nothing more can be sent through it."
)

// pageFeedbackText is the answer's words for delivery d.
func pageFeedbackText(d string) string {
	switch d {
	case pageFeedbackSent:
		return PageFeedbackSentText
	case pageFeedbackHeld:
		return PageFeedbackHeldText
	}
	return PageFeedbackUnreachableText
}

// handlePageFeedback is POST /s/{token}/feedback: a message through the link,
// about the page's current version.
func (s *Server) handlePageFeedback(w http.ResponseWriter, r *http.Request) {
	l, _ := pageLinkOf(r.Context())
	s.pageFeedback(w, r, pageResponder{page: l.PageID, link: l})
}

// handleOwnerPageFeedback is POST /pages/{id}/versions/{v}/feedback: the
// OWNER's own message from their view of version v.
func (s *Server) handleOwnerPageFeedback(w http.ResponseWriter, r *http.Request) {
	if p, ok := s.ownerResponder(w, r); ok {
		s.pageFeedback(w, r, p)
	}
}

// pageFeedback is THE feedback route, as responder p.
func (s *Server) pageFeedback(w http.ResponseWriter, r *http.Request, p pageResponder) {
	ctx := r.Context()
	kind := string(pages.KindFeedback)
	if !s.pageState.feedback.take(p.rateKey(), s.now(), pageLimitWindow, pageFeedbackLimit) {
		metrics.PageResponses.WithLabelValues(kind, "throttled").Inc()
		w.Header().Set("Retry-After", "60")
		s.pageWriteError(w, http.StatusTooManyRequests, "That is a lot of feedback at once. Wait a minute, then send it again.")
		return
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(pageSubmitReadTimeout))
	r.Body = http.MaxBytesReader(w, r.Body, pageFeedbackBodyMax)
	var in struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		metrics.PageResponses.WithLabelValues(kind, "invalid").Inc()
		var tooBig *http.MaxBytesError
		switch {
		case errors.Is(err, os.ErrDeadlineExceeded):
			s.pageWriteError(w, http.StatusRequestTimeout, "The message took too long to arrive. Send it again.")
		case errors.As(err, &tooBig):
			s.pageWriteError(w, http.StatusRequestEntityTooLarge, pageFeedbackTooLong)
		default:
			s.pageWriteError(w, http.StatusBadRequest, "The request is not {text}.")
		}
		return
	}
	// A NUL cannot be stored (jsonb); nothing a person types is one.
	text := strings.ReplaceAll(in.Text, "\x00", "")
	if strings.TrimSpace(text) == "" {
		metrics.PageResponses.WithLabelValues(kind, "invalid").Inc()
		s.pageWriteError(w, http.StatusBadRequest, "Write a message first.")
		return
	}
	if utf8.RuneCountInString(text) > pages.MaxFeedbackRunes {
		metrics.PageResponses.WithLabelValues(kind, "invalid").Inc()
		s.pageWriteError(w, http.StatusRequestEntityTooLarge, pageFeedbackTooLong)
		return
	}
	v, err := s.ext.Pages.Version(ctx, p.page, p.version)
	if err != nil {
		status, msg, outcome := pageStoreRefusal(err)
		metrics.PageResponses.WithLabelValues(kind, outcome).Inc()
		s.pageWriteError(w, status, msg)
		return
	}
	resp, owner, err := s.storePageResponse(ctx, p, v.Version, pages.FeedbackComponent, pages.KindFeedback, pages.FeedbackData(text), "")
	if errors.Is(err, pages.ErrFeedbackFull) {
		metrics.PageResponses.WithLabelValues(kind, "full").Inc()
		s.pageWriteError(w, http.StatusConflict, PageFeedbackFullText)
		return
	}
	if err != nil {
		status, msg, outcome := pageStoreRefusal(err)
		if outcome == "error" {
			s.logger.Printf("pages: %s: record feedback: %v", p, err)
		}
		metrics.PageResponses.WithLabelValues(kind, outcome).Inc()
		s.pageWriteError(w, status, msg)
		return
	}
	metrics.PageResponses.WithLabelValues(kind, "recorded").Inc()
	d := s.wakePageFeedback(ctx, owner, p.page, resp, v)
	s.pageResponded(ctx, p, v, resp)
	w.Header().Set("Referrer-Policy", "no-referrer")
	s.writeJSON(w, http.StatusOK, map[string]any{"id": resp.ID, "delivery": d, "message": pageFeedbackText(d)})
}

// pageFeedbackTooLong is the refusal of a message over the bound.
var pageFeedbackTooLong = "That message is longer than " + strconv.Itoa(pages.MaxFeedbackRunes) + " characters. Shorten it and send it again."

// wakePageFeedback files the page_feedback wake feedback r on page id
// (version v) owes the session that made the page, and says where it went:
// pageFeedbackSent when a new wake was filed, pageFeedbackHeld when it was
// but waits for the owner's fresh sign-in (wakeNeedsSignIn, the one rule),
// pageFeedbackUnreachable when none was — the page was made by no mirrored
// Claude Code session here (a dispatched agent's, a team's, a host that does
// not mirror here: pageFeedbackWake and fileWakeRow's own rules), or the
// store refused it.
func (s *Server) wakePageFeedback(ctx context.Context, owner, id int64, r pages.Response, v pages.Version) string {
	if !s.wakesOn() || s.ext.Pages == nil || owner <= 0 {
		return pageFeedbackUnreachable
	}
	octx := s.queuedPrincipal(context.WithoutCancel(ctx), owner)
	pg, err := s.ext.Pages.Page(octx, id)
	if err != nil || pg.OwnerID != owner {
		return pageFeedbackUnreachable
	}
	n, ok := pageFeedbackWake(pg, r, v.Version)
	if !ok {
		return pageFeedbackUnreachable
	}
	wk, fresh := s.fileWakeRow(ctx, owner, n)
	if !fresh {
		return pageFeedbackUnreachable
	}
	if wakeNeedsSignIn(wk, s.sendQueueNow()) {
		return pageFeedbackHeld
	}
	return pageFeedbackSent
}

// pageFeedbackChrome is the Feedback pill for the owner's own view of page
// p: with the dictation microphone on a server with speech-to-text
// (dictatePath, the one predicate) — and nil, no pill, for a team member
// reading a team's page (teamPageNoFeedback refuses the route too). A link's
// viewer's pill is drawn without one (/transcribe is an operator's alone).
func (s *Server) pageFeedbackChrome(ctx context.Context, p pages.Page) *pages.FeedbackChrome {
	if memberOnTeamPage(ctx, p) {
		return nil
	}
	fc := &pages.FeedbackChrome{}
	if path := s.dictatePath(); path != "" {
		fc.Mic = ui.DictateControlHTML(path, pages.FeedbackInputID, pages.FeedbackNoteID, pages.FeedbackMicID)
		fc.MicNote = ui.DictateNoteHTML(path, pages.FeedbackNoteID, "fbn")
		fc.MicScript = ui.DictateScriptJS(path)
	}
	return fc
}
