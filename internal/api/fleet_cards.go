package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/fleet"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// THE PROPOSAL CARDS' ROUTES (fleet proposals; internal/ui fleet_digest.go):
//
//	POST /ui/chief/fleet/actions/{id}/approve   send it (step-up)
//	POST /ui/chief/fleet/actions/{id}/edit      send text= as the owner's own (step-up)
//	POST /ui/chief/fleet/actions/{id}/reject    reject it
//	POST /ui/chief/fleet/bulk                   op=approve|reject, id=… each row shown
//	GET  /ui/chief/fleet/stepup                 the "Confirm it's you" dialog
//
// Each answers with the card, its Proposed group open and a note.
//
// 🔴 404 UNLESS FLEET PROPOSALS ARE ON (fleetProposeOn).
//
// 🔴 A PHONE'S NOTIFICATION CAN NEVER APPROVE (fleetCardRefusal): a request
// a paired device's signed action makes (authz.ViaDevice) is refused before
// anything is read — and the batched push carries no action at all.
//
// 🔴 SENDING NEEDS A FRESH SIGN-IN (stepup.Fresh) OF THE TAPPING BROWSER
// SESSION: an approval types into a session, as a Send does; an edit sends
// the owner's own words. A reject sends nothing and needs none. A stale one is
// 403 with X-Muster-Step-Up (refuseStale): the panel's script asks "Confirm
// it's you" and taps again.
// ---------------------------------------------------------------------------

func (s *Server) registerFleetCardRoutes(mux Mux) {
	mux.HandleFunc("POST "+ui.FleetActionPathPrefix+"{id}/{verb}", s.requireOperatorWrite(s.handleFleetCardAction))
	mux.HandleFunc("POST "+ui.FleetBulkPath, s.requireOperatorWrite(s.handleFleetBulk))
	mux.HandleFunc("GET "+ui.FleetStepUpPath, s.requireSession(s.handleFleetStepUp))
}

// FleetNotFromPhone is the refusal of a card action from a phone's
// notification (a paired device's signed request).
const FleetNotFromPhone = "a fleet proposal is approved only on its card in muster, never from a phone's notification"

// fleetCardRefusal is why ctx may not act on a card at all ("" when it may):
// only a person signed in to muster in a browser — never a device's signed
// action, never a token.
func fleetCardRefusal(ctx context.Context) string {
	p, ok := authz.From(ctx)
	switch {
	case !ok || p.Team || p.All():
		return "not your card"
	case p.Via == authz.ViaDevice:
		return FleetNotFromPhone
	case p.Via != authz.ViaSession:
		return "a fleet proposal is decided only by a person signed in to muster in a browser"
	}
	return ""
}

// The bulk's note.
func fleetBulkNote(sent, skipped, failed int) string {
	note := strconv.Itoa(sent) + " sent"
	if skipped > 0 {
		note += ", " + strconv.Itoa(skipped) + " skipped (moved on)"
	}
	if failed > 0 {
		note += ", " + strconv.Itoa(failed) + " not sent"
	}
	return note + "."
}

// fleetCardGate is the routes' common gate: on, a person in a browser, and —
// when send — a fresh sign-in. false: it answered.
func (s *Server) fleetCardGate(w http.ResponseWriter, r *http.Request, send bool) bool {
	if !s.fleetProposeOn() {
		http.NotFound(w, r)
		return false
	}
	if why := fleetCardRefusal(r.Context()); why != "" {
		http.Error(w, why, http.StatusForbidden)
		return false
	}
	if send && !stepup.Fresh(r.Context(), s.stepUpNow()) {
		refuseStale(w)
		return false
	}
	return true
}

// handleFleetCardAction serves POST /ui/chief/fleet/actions/{id}/{verb}.
func (s *Server) handleFleetCardAction(w http.ResponseWriter, r *http.Request) {
	verb := r.PathValue("verb")
	if verb != "approve" && verb != "edit" && verb != "reject" {
		http.NotFound(w, r)
		return
	}
	if !s.fleetCardGate(w, r, verb != "reject") {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	var note string
	switch verb {
	case "approve":
		note = s.fleetApprove(ctx, id, fleet.ViaWeb, nil).note
	case "edit":
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "the text is too large", http.StatusRequestEntityTooLarge)
			return
		}
		text, ok := cleanPrompt(r.PostForm.Get("text"))
		switch {
		case !ok || len(text) > fleet.MaxCustomBytes:
			note = "Not sent: the text is at most 4 KiB."
		case text == "":
			note = "Not sent: the text is empty."
		default:
			note = s.fleetApprove(ctx, id, fleet.ViaWebEdit, &text).note
		}
	case "reject":
		note = "Rejected."
		if !s.fleetReject(ctx, id, fleet.ViaWeb) {
			note = FleetDecided
		}
	}
	s.writeFleetCard(w, r, note)
}

// handleFleetBulk serves POST /ui/chief/fleet/bulk: every listed proposal
// approved (each re-read on its own; one that moved on is skipped and
// counted) or rejected.
func (s *Server) handleFleetBulk(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	op := r.PostForm.Get("op")
	if op != "approve" && op != "reject" {
		http.Error(w, "op is approve or reject", http.StatusBadRequest)
		return
	}
	if !s.fleetCardGate(w, r, op == "approve") {
		return
	}
	// One sync of the proposals push after every decision (fleetPushSync).
	owner := requestOwner(r.Context())
	ctx := fleetBatch(r.Context())
	defer s.fleetPushSync(r.Context(), owner)
	var ids []int64
	for _, v := range r.PostForm["id"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil && id > 0 && len(ids) < 100 {
			ids = append(ids, id)
		}
	}
	sent, skipped, failed, rejected := 0, 0, 0, 0
	for _, id := range ids {
		if op == "reject" {
			if s.fleetReject(ctx, id, fleet.ViaWebBulk) {
				rejected++
			}
			continue
		}
		res := s.fleetApprove(ctx, id, fleet.ViaWebBulk, nil)
		switch {
		case res.sent:
			sent++
		case res.movedOn:
			skipped++
		default:
			failed++
		}
	}
	note := fleetBulkNote(sent, skipped, failed)
	if op == "reject" {
		note = strconv.Itoa(rejected) + " rejected."
	}
	s.writeFleetCard(w, r, note)
}

// handleFleetStepUp serves GET /ui/chief/fleet/stepup: the "Confirm it's you"
// dialog and its script, for a page that has none.
func (s *Server) handleFleetStepUp(w http.ResponseWriter, r *http.Request) {
	if !s.fleetProposeOn() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(s.stepUpFormsHTML(r, FleetStepUpWhy())))
}

// FleetStepUpWhy is the dialog's sentence for a card action.
func FleetStepUpWhy() string {
	return "Approving a proposal types into one of your sessions, so it needs a sign-in from the last " + stepup.WindowText() + "."
}

// writeFleetCard answers a card action with the card: its Proposed group open
// and note on it.
func (s *Server) writeFleetCard(w http.ResponseWriter, r *http.Request, note string) {
	ctx := r.Context()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	var baseline time.Time
	if s.ext.Fleet != nil {
		baseline, _ = s.ext.Fleet.Baseline(ctx)
	}
	v := s.fleetDigestViewFor(ctx, baseline)
	v.Note, v.OpenGroup = note, ui.FleetGroupProposed
	if err := ui.RenderFleetDigest(w, v); err != nil {
		s.logger.Printf("fleet card: render: %v", err)
	}
}

// fleetDigestViewFor is the card as ctx's principal sees it: the digest, and —
// with fleet proposals on — the Proposed group after Needs you.
func (s *Server) fleetDigestViewFor(ctx context.Context, baseline time.Time) ui.FleetDigestView {
	d, unavailable, subs := s.fleetDigestRead(ctx, baseline)
	if unavailable != "" {
		v := ui.FleetDigestView{Unavailable: unavailable}
		if s.fleetProposeOn() {
			v.Groups = []ui.FleetGroupView{{Key: ui.FleetGroupProposed}}
		}
		return v
	}
	now := s.now()
	v := fleetDigestView(d, now)
	if !s.fleetProposeOn() {
		return v
	}
	proposed := ui.FleetGroupView{Key: ui.FleetGroupProposed, Label: ui.FleetProposedLabel,
		Note: "Nothing is sent until you approve it. Each is checked again when you tap: a session that moved on is skipped."}
	proposed.Proposals = s.fleetProposalViews(ctx, subs, now)
	var groups []ui.FleetGroupView
	for _, gr := range v.Groups {
		groups = append(groups, gr)
		if gr.Key == fleet.GroupNeedsYou {
			groups = append(groups, proposed)
		}
	}
	v.Groups = groups
	return v
}

// fleetProposalViews is the principal's open proposals as cards, oldest
// first. 🔴 EXPIRY IS READ HERE TOO (fleetExpire), so a card the pass has not
// reached yet is never drawn after its stop ended or its 12 h passed.
func (s *Server) fleetProposalViews(ctx context.Context, subs map[string]fleetSubject, now time.Time) []ui.FleetProposalView {
	open, err := s.ext.FleetActions.OpenActions(ctx)
	if err != nil {
		s.logger.Printf("fleet card: open actions: %v", err)
		return nil
	}
	owner := s.personName(ctx, requestOwner(ctx))
	// Every expiry this draw makes moves the notification once, after it
	// (fleetPushSync), not once per card.
	bctx, expired := fleetBatch(ctx), false
	defer func() {
		if expired {
			s.fleetPushSync(ctx, requestOwner(ctx))
		}
	}()
	var out []ui.FleetProposalView
	for _, a := range open {
		if a.State != fleet.StateProposed {
			continue
		}
		if s.fleetExpire(bctx, a) {
			expired = true
			continue
		}
		p := ui.FleetProposalView{ID: a.ID, Subject: a.Evidence.SubjectID, Session: firstNonBlank(a.Evidence.Label, firstNonBlank(a.Evidence.SubjectID, a.SubjectKey)),
			Verb: fleet.ClassVerb[a.Class], Reason: a.Reason, Text: fleet.Body(owner, now, a.Text), EditText: a.Text,
			Age: fleetAge(now.Sub(a.CreatedAt)), Editable: fleetEditable(a),
			ExpiresIn: fleetExpiresIn(fleet.ExpiresAt(a.CreatedAt).Sub(now)), ExpiresAt: fleet.ExpiresAt(a.CreatedAt)}
		if a.Class == fleet.ClassReassign {
			p.Text = "1. Route task #" + strconv.FormatInt(a.TaskID, 10) + " to " + a.ToSubject + " (as route_to_session does).\n2. Then send:\n" + p.Text
		}
		if a.Class == fleet.ClassWrap {
			p.Text += "\n\n(Then, once its handoff turn has ended, muster closes the session — unless it has an open pull request, a queued message or background work.)"
		}
		if sub, ok := subs[a.Evidence.SubjectID]; ok {
			p.Href, p.Where = fleetSessionHref(sub), sub.row.Where
		}
		out = append(out, p)
	}
	return out
}

// fleetTTLOutcome is the outcome of a proposal expired by its 12 h
// (fleet.Expired), recorded with fleet.WhyTTL; fleetMovedOnOutcome one whose
// session moved on, with fleet.WhyMovedOn. fleetGone reads the reason, never
// these words.
const (
	fleetTTLOutcome     = "12 h passed"
	fleetMovedOnOutcome = "the session moved on (its stop ended, or a new message or pull-request head)"
)

// fleetExpire expires proposal a when its 12 h passed or its stop moved on
// (the stop ended, a new message, a new pull-request head): true when it is
// no longer a proposal. A state that cannot be read keeps it.
func (s *Server) fleetExpire(ctx context.Context, a fleet.Action) bool {
	if fleet.Expired(a.CreatedAt, s.now()) {
		s.fleetMove(ctx, a, fleet.Move{State: fleet.StateExpired, Outcome: fleetTTLOutcome, Why: fleet.WhyTTL, Finish: true})
		return true
	}
	if s.fleetStillOn(ctx, a).movedOn() {
		s.fleetMove(ctx, a, fleet.Move{State: fleet.StateExpired, Outcome: fleetMovedOnOutcome, Why: fleet.WhyMovedOn, Finish: true})
		return true
	}
	return false
}

// fleetExpiresIn is d, the time left before a proposal expires, as its card
// says it: "in 3h", "in 45m", "in under a minute".
func fleetExpiresIn(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "in under a minute"
	case d < time.Hour:
		return "in " + strconv.Itoa(int(d.Minutes())) + "m"
	}
	return "in " + strconv.Itoa(int(d.Hours())) + "h"
}

// --- a fleet link's arrival (ui.FleetProposalsURL) ------------------------------

// The lines a fleet link's card draws when the proposal it named no longer
// waits (ui.FleetGroupView.Gone; " at <clock>." follows when it says when).
const (
	// FleetGoneTTL: its 12 hours passed with no tap (fleet.WhyTTL).
	FleetGoneTTL = "Expired after 12 hours"
	// FleetGoneNoLongerNeeded: its session moved on before the tap — the stop
	// ended, a new message, a new pull-request head (fleet.WhyMovedOn).
	FleetGoneNoLongerNeeded = "No longer needed: the session moved on"
	// FleetGoneExpired: expired before muster recorded why (migration 0192).
	FleetGoneExpired  = "That proposal expired"
	FleetGoneApproved = "That proposal was already approved"
	FleetGoneRejected = "That proposal was already rejected"
	FleetGoneMovedOn  = "That proposal was not sent: its session moved on"
	FleetGoneUnknown  = "That proposal is no longer waiting"
	// FleetNoneWaiting: the link named no proposal (the Attention notice's)
	// and none waits now.
	FleetNoneWaiting = "No proposal is waiting now"
)

// fleetFocus is v as a fleet link asked for it (?fleet=proposed&proposal=id):
// its Proposed group open (on a phone, its sheet), and the proposal id
// highlighted — or, when id no longer waits, the line saying so and the newest
// waiting one highlighted. id 0 highlights the newest. A card with no
// Proposed group (fleet proposals off, no digest) is left as it is.
func (s *Server) fleetFocus(ctx context.Context, v *ui.FleetDigestView, id int64) {
	for i := range v.Groups {
		gr := &v.Groups[i]
		if gr.Key != ui.FleetGroupProposed {
			continue
		}
		v.OpenGroup = ui.FleetGroupProposed
		at := -1
		for j, p := range gr.Proposals {
			if id > 0 && p.ID == id {
				at = j
			}
		}
		if at < 0 && id > 0 {
			gr.Gone, gr.GoneAt = s.fleetGone(ctx, id)
		}
		// The cards are oldest first: the newest is the last.
		if at < 0 && len(gr.Proposals) > 0 {
			at = len(gr.Proposals) - 1
		}
		if at >= 0 {
			gr.Proposals[at].Highlight = true
		} else if gr.Gone == "" {
			gr.Gone = FleetNoneWaiting
		}
		return
	}
}

// fleetGone is why proposal id, not drawn as waiting, no longer waits, and
// when (zero when the row cannot be read as the principal's).
func (s *Server) fleetGone(ctx context.Context, id int64) (string, time.Time) {
	a, err := s.ext.FleetActions.GetAction(ctx, id)
	if err != nil || a.Owner != requestOwner(ctx) {
		return FleetGoneUnknown, time.Time{}
	}
	switch {
	case a.State == fleet.StateExpired && a.ExpiredWhy == fleet.WhyTTL:
		// Its 12 h passed: then, not when a draw next noticed and marked it.
		return FleetGoneTTL, fleet.ExpiresAt(a.CreatedAt)
	case a.State == fleet.StateExpired && a.ExpiredWhy == fleet.WhyMovedOn:
		// Its session moved on: noticed (and marked) by a draw or the pass.
		return FleetGoneNoLongerNeeded, a.FinishedAt
	case a.State == fleet.StateExpired:
		return FleetGoneExpired, a.FinishedAt
	case a.State == fleet.StateRejected:
		return FleetGoneRejected, a.DecidedAt
	case a.State == fleet.StateApproved || a.State == fleet.StateQueued || a.State == fleet.StateSent || a.State == fleet.StateFailed:
		return FleetGoneApproved, a.DecidedAt
	case a.State == fleet.Suppressed(fleet.GuardMovedOn):
		return FleetGoneMovedOn, a.FinishedAt
	}
	return FleetGoneUnknown, time.Time{}
}

// fleetLabel is a proposal's session as a short line (for logs and notes).
func fleetLabel(a fleet.Action) string {
	return strings.TrimSpace(firstNonBlank(a.Evidence.Label, firstNonBlank(a.Evidence.SubjectID, a.SubjectKey)))
}
