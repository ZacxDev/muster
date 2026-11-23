package api

import (
	"context"
	"errors"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// "RAISE CAP FOR TODAY" (operator decision: "Yes, today only").
//
// On an officer's page, once today's spend reaches spendNearShare (80%) of
// its daily budget — and while that budget holds it — its OWNER sees one
// button (operator decision: "From 80%, for today") that raises
// that budget by one more day's worth (its declared
// daily_budget_usd) for the current UTC day only (builtin.Store.RaiseCapToday:
// a row keyed by the UTC day, so the raise ends at the next UTC midnight with
// no cleanup job). The wake loop reads the raise on its next pass
// (officers.Reconciler.observe, officers.Officer.WithRaisedCap), and the
// handler marks the officer due so that pass is now: the held officer resumes.
//
// 🔴 OWNER-ONLY. The officer's owner, signed in through their own browser
// (authz.ViaSession), and nobody else: a team's officer is refused (its owner
// is a team: a manager is not its owner), as is anyone who cannot see the
// officer at all (scopeFor's 404). Every agent and officer credential is
// refused before the handler, by requireOperatorWrite (a session and the same
// origin), and again here by Via. Every raise is in audit_log with its old
// and new cap and until when, written in the raise's own transaction.
// ---------------------------------------------------------------------------

// CapRaiseOwnerOnly is the refusal of a raise by anyone but the officer's own
// owner.
const CapRaiseOwnerOnly = "Only the officer's owner can raise its budget, and a team's officer's budget is not raised here."

// capRaiseAllowed reports whether ctx's principal may raise the daily budget
// of an officer in scope sc: the officer's owner themself, through a session.
func capRaiseAllowed(ctx context.Context, sc officerScope) bool {
	p, ok := authz.From(ctx)
	return ok && !sc.Team && p.Via == authz.ViaSession && p.UserID == sc.Owner
}

// capRaiseFormHTML is the "Raise today's limit" button the spend bar draws
// from spendNearShare of today's limit on (officerRunHTML), for its owner
// only; "" for anyone else, an officer with no budget, or one
// already raised MaxCapRaisesPerDay times today.
func (s *Server) capRaiseFormHTML(ctx context.Context, sc officerScope, id string) string {
	if !capRaiseAllowed(ctx, sc) {
		return ""
	}
	o, ok := s.runningOfficer(sc.Owner, id)
	step := o.Budget.DailyUSD
	if !ok || step <= 0 {
		return ""
	}
	now := s.officerNow()
	current, _ := s.officerBudgetOf(ctx, sc.Owner, id, now)
	if current >= step*float64(1+builtin.MaxCapRaisesPerDay)-1e-9 {
		return `<p class="text-xs text-fg2" data-cap-raise-spent>Today's budget was already raised as far as it goes; it goes back at ` +
			html.EscapeString(capResetWords(now)) + `.</p>`
	}
	esc := html.EscapeString
	label := "Raise today's limit to " + navUSDWords(current+step)
	return `<form method="POST" action="` + esc(officersPagePath+"/"+url.PathEscape(id)+"/raise-cap") + `" class="flex flex-col gap-1" data-cap-raise>` + sc.ownerField() +
		`<button type="submit" class="` + adminButton + ` ` + ui.OfficerRowButtonClass + `" data-cap-raise-submit>` + esc(label) + `</button>` +
		`<span class="text-xs text-fg2">+` + esc(navUSDWords(step)) + `, for today only: it goes back to ` + esc(navUSDWords(step)) + ` at ` + esc(capResetWords(now)) + `.</span></form>`
}

// capResetWords is when a raise made at now ends: the next UTC midnight.
func capResetWords(now time.Time) string {
	return officers.CapResetAt(now).UTC().Format("15:04 UTC")
}

// handleOfficerRaiseCap is POST /officers/{officer}/raise-cap: the owner's
// raise of its builtin officer's daily budget for today (capRaiseAllowed).
func (s *Server) handleOfficerRaiseCap(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.scopeFor(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	id := r.PathValue("officer")
	if !capRaiseAllowed(ctx, sc) {
		s.renderNotice(w, r, http.StatusForbidden, "Only its owner raises its budget", CapRaiseOwnerOnly)
		return
	}
	o, running := s.runningOfficer(sc.Owner, id)
	if !running {
		// The officer page's own refusal for an officer that does not run
		// (controlTarget's rule): none of the caller's is 404, while one of
		// theirs that is only a def is its own 409 — never the same answer.
		if _, err := s.officerStore.GetDeclared(ctx, sc.Owner, id); err != nil {
			code, msg := officerPageRefusal(err)
			if code == http.StatusInternalServerError {
				s.logger.Printf("officers: read officer %s of %d: %v", id, sc.Owner, err)
			}
			title := "It has no budget to raise"
			if code == http.StatusNotFound {
				title = "No such officer"
			}
			s.renderNotice(w, r, code, title, msg)
			return
		}
		s.renderOfficerDetail(w, r, sc, id, http.StatusConflict, "It is not running, so it has no budget to raise.")
		return
	}
	if !o.IsBuiltin() || o.Budget.DailyUSD <= 0 {
		s.renderOfficerDetail(w, r, sc, id, http.StatusConflict, "It has no daily budget, so there is nothing to raise.")
		return
	}
	p := authz.MustFrom(ctx)
	res, err := s.officerStore.RaiseCapToday(ctx, o, string(p.Via), s.officerNow())
	if err != nil {
		code, msg := http.StatusInternalServerError, "The raise could not be recorded. Try again in a moment."
		switch {
		case errors.Is(err, builtin.ErrInvalid):
			code, msg = http.StatusConflict, strings.TrimPrefix(err.Error(), builtin.ErrInvalid.Error()+": ")
		case errors.Is(err, builtin.ErrNoBudget):
			code, msg = http.StatusConflict, "It has no daily budget, so there is nothing to raise."
		default:
			s.logger.Printf("officers: user %d raising %s's cap: %v", p.UserID, id, err)
		}
		s.renderOfficerDetail(w, r, sc, id, code, msg)
		return
	}
	s.logger.Printf("officers: user %d raised officer %s's daily budget for today from $%.2f to $%.2f, until %s", p.UserID, id, res.Old, res.New, res.Until.UTC().Format("2006-01-02 15:04 UTC"))
	// The held officer resumes: its next pass reads the raise, now.
	if s.officerRT != nil && s.officerRT.rec != nil {
		s.officerRT.rec.Due(o.Key())
	}
	s.officerActionDone(w, r, sc, id, "cap-raised", nil)
}
