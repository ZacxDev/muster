package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/plugins"
)

// ---------------------------------------------------------------------------
// A STEP-UP CARD ABOUT A DRAFT CARRIES THE DRAFT'S OWN DECISION.
//
// A confirmation card a host session filed for plugin_draft_decide ("Post to
// X? · Draft #41", "Reject this draft? · Draft #41") is answered only from the
// owner's paired phone (hostDecisionForAnswer): no Confirm, no Refuse, no
// answer box in the browser, and that does not change here.
//
// What the Open feed's card and the ask page add is the DRAFT's own decision —
// the owner deciding the draft in muster's web UI, the path that was never
// step-up gated (host_stepup.go, "What is NOT gated") — drawn by THE function
// the Drafts tab draws it with (draftDecisionHTML), posting to the same
// /plugins/drafts/{id}/approve|reject routes with the same body and media
// binding, and a `return` that brings the browser back to the Asks page (or
// the card's page) instead of the Drafts tab's Archived view.
//
// A draft decided on ANY door settles every open step-up card about it
// (settleDraftStepUps, from decideDraftAs): the card answers with what was
// decided and where, its pending decision is dropped, and a later Confirm on
// the phone is refused as not open — and would find the draft no longer
// pending anyway (the store's own state check).
// ---------------------------------------------------------------------------

// stepUpDraftFor is the draft step-up card c is about, as ctx's principal may
// decide it (stepUpDraftsFor over c alone), nil for none.
func (s *Server) stepUpDraftFor(ctx context.Context, c *attention.StepUpCard) *plugins.DraftCard {
	if c == nil {
		return nil
	}
	decide, _ := s.stepUpDraftsFor(ctx, []int64{c.DraftID})
	return decide[c.DraftID]
}

// stepUpDraftsFor is, by draft id, the drafts the step-up cards naming ids
// are about, as ctx's principal may decide them — absent for a draft the
// principal cannot read (row-level security: another owner's reads as
// missing), one it may not decide (draftDeciderRefusal, officerDeciderRefusal:
// decideDraftAs's own refusals, so no control is drawn that the tap would
// refuse), and one time has decided (plugins.DraftTimeDecided: expired, which
// no sweep has settled yet).
//
// 🔴 ONE BATCH READ, NO SWEEP (plugins.DraftCardsByID): the Open feed draws a
// card per draft, and a DraftCard per card was a sweep — a write transaction —
// per card per render, live refreshes included.
//
// read is every draft the batch read could see, decidable or not: who raised
// it is still what the card's filter chip names (readAskSources), with no
// second read.
func (s *Server) stepUpDraftsFor(ctx context.Context, ids []int64) (decide map[int64]*plugins.DraftCard, read map[int64]plugins.DraftCard) {
	out := map[int64]*plugins.DraftCard{}
	read = map[int64]plugins.DraftCard{}
	if s.ext.Plugins == nil {
		return out, read
	}
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 {
		return out, read
	}
	var want []int64
	seen := map[int64]bool{}
	for _, id := range ids {
		if id > 0 && !seen[id] {
			seen[id] = true
			want = append(want, id)
		}
	}
	if len(want) == 0 {
		return out, read
	}
	cards, err := s.ext.Plugins.DraftCardsByID(ctx, want)
	if err != nil {
		s.logger.Printf("step-up: the cards' drafts: %v", err)
		return out, read
	}
	now := time.Now()
	refused := officerDeciderRefusal(ctx) != ""
	var keep []plugins.DraftCard
	for _, dc := range cards {
		read[dc.Draft.ID] = dc
		if refused || draftDeciderRefusal(p, dc.Draft, false) != "" || plugins.DraftTimeDecided(dc.Draft, now) {
			continue
		}
		keep = append(keep, dc)
	}
	// As the Drafts tab reads its cards (readDraftViews): whether a manual
	// retry is open is the server's, not the row's.
	s.withRetryOpen(ctx, keep)
	for i := range keep {
		out[keep[i].Draft.ID] = &keep[i]
	}
	return out, read
}

// stepUpRejects reports whether step-up card c asks to REJECT its draft (its
// title is stepUpRejectTitle's, as stepUpDraftCard files it).
func stepUpRejects(c *attention.StepUpCard) bool {
	return c != nil && strings.HasPrefix(stepUpTitleText(c), stepUpRejectTitle)
}

// The line a step-up card about a draft draws above the draft's own controls,
// in place of askStepUpPhoneLine.
const (
	askStepUpDraftLine       = "Approve here, or confirm on your phone."
	askStepUpDraftRejectLine = "Reject here, or confirm on your phone."
	askStepUpDraftReviewLine = "Review the whole post to approve it, or confirm on your phone."
)

// stepUpShowsWholeDraft reports whether step-up card c's face shows ALL of
// draft dc — the only face an Approve bound to the draft's whole body and
// media may be drawn on (🔴 what is approved is what is seen):
//   - the card is not an excerpt and not a changed-draft card;
//   - its preview IS the draft's text and its media the draft's media, as the
//     Drafts tab draws them (draftPostCard), so nothing differs from what the
//     approval binds;
//   - on the Open feed (feed), the draft is ONE post: the feed draws only a
//     thread's first post, clamped, and "+N more posts".
func stepUpShowsWholeDraft(c *attention.StepUpCard, dc *plugins.DraftCard, feed bool) bool {
	if c == nil || dc == nil || c.Excerpt != "" || c.Changed {
		return false
	}
	whole := draftPostCard(*dc)
	if c.Preview != whole.Preview || len(c.Media) != len(whole.Media) {
		return false
	}
	for i := range c.Media {
		if c.Media[i].SHA256 != whole.Media[i].SHA256 {
			return false
		}
	}
	if feed {
		if _, total := stepUpPostParts(stepUpWithVenue(c)); total != 1 {
			return false
		}
	}
	return true
}

// draftTabLink is draft id's card on the Asks page's Drafts tab, which draws
// its whole text.
func draftTabLink(id int64) string {
	n := strconv.FormatInt(id, 10)
	return attentionPath + "?" + asksTabParam + "=" + asksTabDrafts + "&" + asksDraftParam + "=" + n + "#" + draftCardAnchor(id)
}

// askStepUpDraftHTML is a step-up card's draft controls, drawn in back's place
// (attentionPath on the Open feed, the card's own page there), and the line
// above them; "" when dc is nil or the draft has nothing left to decide —
// the card then draws its phone line alone (askStepUpPhoneHTML). size is the
// line's text size class.
//
// review is where the whole draft can be read when this face does not show
// all of it (stepUpShowsWholeDraft false): its Approve becomes a "Review and
// approve" link there.
func askStepUpDraftHTML(c *attention.StepUpCard, dc *plugins.DraftCard, back, review, size string, feed bool, now time.Time) string {
	if dc == nil {
		return ""
	}
	rejects := stepUpRejects(c)
	place := cardPlace{back: back, rejectLead: rejects}
	if !stepUpShowsWholeDraft(c, dc, feed) {
		place.reviewHref = review
	}
	controls := draftDecisionHTML(*dc, now, place)
	if controls == "" {
		return ""
	}
	line := askStepUpDraftLine
	switch {
	case rejects:
		line = askStepUpDraftRejectLine
	case place.reviewHref != "" && strings.Contains(controls, "data-draft-review"):
		line = askStepUpDraftReviewLine
	}
	return `<div class="flex min-w-0 flex-col gap-2" data-asks-stepup-draft data-draft-id="` + strconv.FormatInt(dc.Draft.ID, 10) + `">` +
		`<p class="break-words ` + size + ` text-fg2" data-ask-stepup-phone>` + line + `</p>` + controls + `</div>`
}

// asksDraftReturn is the decision form's `return` when it is the Asks page
// (attentionPath) or one ask's page (askPagePath): where a decision made on a
// step-up card's draft controls sends the browser. Nothing else.
var asksDraftReturn = regexp.MustCompile(`^` + regexp.QuoteMeta(attentionPath) + `(/asks/[1-9][0-9]{0,18})?$`)

// asksReturn is that `return`, and whether the form named one.
func asksReturn(r *http.Request) (string, bool) {
	v := r.PostFormValue("return")
	if v == "" || safeReturnTo(v) != v || !asksDraftReturn.MatchString(v) {
		return "", false
	}
	return v, true
}

// stepUpDraftAnswer is what a step-up card about a draft records when the
// draft was decided on another door: the decision and where it was made.
func stepUpDraftAnswer(d plugins.Draft, via string) string {
	what := "Rejected"
	if d.Status == plugins.DraftApproved {
		what = "Approved"
	}
	where := "elsewhere"
	switch via {
	case "", plugins.DraftViaBrowser:
		where = "in the web UI"
	case plugins.DraftViaMCP:
		where = "through a session's request"
	}
	return what + " " + where
}

// settleDraftStepUps settles every open step-up card of ctx's principal that
// is about draft d, now decided (decideDraftAs, every door): each is answered
// with what was decided and where (stepUpDraftAnswer) by the store's own
// open-only update, and its pending decision is dropped — so it leaves the
// Open feed, and a Confirm on the phone afterwards is refused as not open
// (hostDecisionForAnswer). A card already answered (the phone's Confirm that
// is running this very decision) is not open and is left as it is.
func (s *Server) settleDraftStepUps(ctx context.Context, d plugins.Draft, via string) {
	if s.ext.Attention == nil || d.ID <= 0 {
		return
	}
	asks, err := s.ext.Attention.Search(ctx, attention.Search{DraftID: d.ID, Limit: attention.MaxSearchResults})
	if err != nil {
		s.logger.Printf("step-up: draft %d decided: read its cards: %v", d.ID, err)
		return
	}
	answer := stepUpDraftAnswer(d, via)
	for _, a := range asks {
		if a.Status != attention.Open || !a.Asker.System || a.Asker.Label != hostDecisionLabel || a.StepUp == nil || a.StepUp.DraftID != d.ID {
			continue
		}
		s.hostDecisions.take(a.ID)
		rec, err := s.ext.Attention.Answer(ctx, a.ID, answer)
		switch {
		case errors.Is(err, attention.ErrNotOpen), errors.Is(err, attention.ErrNotFound):
			continue
		case err != nil:
			s.logger.Printf("step-up: draft %d decided: settle card %d: %v", d.ID, a.ID, err)
			continue
		}
		s.logger.Printf("step-up: card %d settled: its draft %d was %s", a.ID, d.ID, strings.ToLower(answer))
		s.askSettled(rec)
	}
}
