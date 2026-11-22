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
// decide it — nil for a card about no draft, a draft the principal cannot
// read (row-level security: another owner's reads as missing), or one it may
// not decide (draftDeciderRefusal, officerDeciderRefusal: decideDraftAs's own
// refusals, so no control is drawn that the tap would refuse).
func (s *Server) stepUpDraftFor(ctx context.Context, c *attention.StepUpCard) *plugins.DraftCard {
	if c == nil || c.DraftID <= 0 || s.ext.Plugins == nil {
		return nil
	}
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 {
		return nil
	}
	dc, err := s.ext.Plugins.DraftCard(ctx, c.DraftID)
	if err != nil {
		return nil
	}
	if draftDeciderRefusal(p, dc.Draft, false) != "" || officerDeciderRefusal(ctx) != "" {
		return nil
	}
	// As the Drafts tab reads its focused card (readDraftViews): whether a
	// manual retry is open is the server's, not the row's.
	one := []plugins.DraftCard{dc}
	s.withRetryOpen(ctx, one)
	return &one[0]
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
)

// askStepUpDraftHTML is a step-up card's draft controls, drawn in back's place
// (attentionPath on the Open feed, the card's own page there), and the line
// above them; "" when dc is nil or the draft has nothing left to decide —
// the card then draws its phone line alone (askStepUpPhoneHTML). size is the
// line's text size class.
func askStepUpDraftHTML(c *attention.StepUpCard, dc *plugins.DraftCard, back, size string, now time.Time) string {
	if dc == nil {
		return ""
	}
	rejects := stepUpRejects(c)
	controls := draftDecisionHTML(*dc, now, cardPlace{back: back, rejectLead: rejects})
	if controls == "" {
		return ""
	}
	line := askStepUpDraftLine
	if rejects {
		line = askStepUpDraftRejectLine
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
