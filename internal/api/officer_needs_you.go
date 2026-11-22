package api

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/flows"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// AN OFFICER'S PAGE AS THE PLACE ITS OWNER DEALS WITH IT (officers UX, PR 3;
// docs/officers-builtin.md "An officer's page"):
//
//   - NEEDS YOU, at the top: every decision waiting on the viewer for this
//     officer, each one actionable in place — its pending post and reply
//     drafts and its outreach batches (THE Asks page's own cards and routes:
//     draftListHTMLIn), its open asks (THE answer form: attentionItemsHTMLTo),
//     the refusals that wait on the owner (re-file at a time, or drop), and
//     its budget and run controls (today's spend against its cap, held, Run
//     now, Pause / Resume);
//   - WHAT IT DID: its finished items newest first in plain words — what was
//     posted (with a link to the post where muster can build one), what was
//     refused and what came of it — and today's spend; the ids behind an ⓘ;
//   - the viewer's LOOK at the page (builtin.MarkSeen): the sidebar's and the
//     Officers list's "N new" count what finished since.
//
// 🔴 ONE COMPONENT, NOT A SECOND IMPLEMENTATION. A draft, a batch and an ask
// are decided here through EXACTLY the routes, checks and bindings the Asks
// page uses (decideDraft / decideDraftAs with approved_body_sha256 and
// approved_media_sha256, decideBatchAs with batch_sha256, answerAsk): the
// card is the same function drawn in another place (cardPlace), and that
// place adds only where the browser comes back to (`return`).
//
// 🔴 ONLY THE OPERATOR'S OWN REQUEST DECIDES. Every route here and every
// route these cards post to is a signed-in browser session's
// (requireSession / requireOperatorWrite): an officer's instance holds an
// agent or hook token, never a session cookie, so none of them can reach a
// decision — and the draft rules (draftDeciderRefusal, 0130's
// draft_approver) refuse a team principal deciding its own drafts again.
// ---------------------------------------------------------------------------

// cardPlace is where a draft or outreach card is drawn: the Asks page (the
// zero place) or an officer's page (back: that page, where each decision
// returns).
type cardPlace struct {
	back string
	// rejectLead: the place asks to REJECT the draft (a "Reject this draft?"
	// step-up card), so draftDecisionHTML draws Reject as the primary action
	// and Approve under it — the same forms, on the same guards.
	rejectLead bool
	// reviewHref, when set, says this face does NOT show the whole draft:
	// draftDecisionHTML draws a "Review and approve" link to it in place of
	// the Approve form (Reject stays).
	reviewHref string
}

// backField is the place's hidden `return` field ("" on the Asks page).
func (p cardPlace) backField() string {
	if p.back == "" {
		return ""
	}
	return `<input type="hidden" name="return" value="` + html.EscapeString(p.back) + `">`
}

// officerNeedsYouID is the officer page's Needs-you section, which every
// decision made in it returns to.
const officerNeedsYouID = "officer-needs-you"

// officerPageReturn is an officer page's address as a decision's `return`
// may name it: /officers/<id>, with owner= for a team's. Nothing else.
var officerPageReturn = regexp.MustCompile(`^` + officersPagePath + `/[a-z0-9-]+:[a-z0-9-]+(\?owner=[1-9][0-9]{0,18})?$`)

// officerReturn is the form's `return` when it is an officer's page on this
// origin (safeReturnTo, and officerPageReturn's shape), and whether it is.
func officerReturn(r *http.Request) (string, bool) {
	v := r.PostFormValue("return")
	if v == "" || safeReturnTo(v) != v || !officerPageReturn.MatchString(v) {
		return "", false
	}
	return v, true
}

// officerReturnDone is where a decision made on an officer's page sends the
// browser: that page, with notice, at its Needs-you section.
func officerReturnDone(back, notice string) string {
	return withNotice(back, notice) + "#" + officerNeedsYouID
}

// renderOfficerReturn draws the officer page back names with problem — a
// refused decision, said where it was made. ok false when back names no
// officer page the viewer may read (the caller then draws its own page).
func (s *Server) renderOfficerReturn(w http.ResponseWriter, r *http.Request, back string, code int, problem string) bool {
	u, err := url.Parse(back)
	if err != nil {
		return false
	}
	id, err := url.PathUnescape(strings.TrimPrefix(u.Path, officersPagePath+"/"))
	if err != nil {
		return false
	}
	sc, ok := s.scopeOf(r.Context(), u.Query().Get("owner"))
	if !ok {
		return false
	}
	s.renderOfficerDetail(w, r, sc, id, code, problem)
	return true
}

// --- what needs the viewer ------------------------------------------------------

// officerNeeds is everything waiting on the viewer for one officer.
type officerNeeds struct {
	// drafts are its PENDING drafts, an outreach batch's entries among them
	// (draftListHTMLIn draws one card per batch).
	drafts []plugins.DraftCard
	// asks are the open asks it raised (raisedByOfficer).
	asks []attention.Ask
	// refusals are its refusals waiting on the owner
	// (builtin.RefusalsAwaitingOwner).
	refusals []builtin.Refusal
}

// count is how many decisions wait: one per draft card, one per outreach
// batch, one per ask, one per refusal.
func (n officerNeeds) count() int {
	single, batches, _ := groupBatches(n.drafts)
	c := len(batches) + len(n.asks) + len(n.refusals)
	for _, d := range single {
		if d.Draft.Status == plugins.DraftPending {
			c++
		}
	}
	return c
}

// needsReads is what officerNeedsOf reads once for every officer on a page:
// the viewer's pending drafts and open asks.
type needsReads struct {
	drafts []plugins.DraftCard
	asks   []attention.Ask
}

// readNeeds reads the viewer's pending drafts (the Asks page's own read,
// outreach batches whole: readDraftViews) and open asks, once.
func (s *Server) readNeeds(ctx context.Context) needsReads {
	var n needsReads
	if s.ext.Plugins != nil {
		if pending, _, ok := s.readDraftViews(ctx, 0); ok {
			n.drafts = pending
		}
	}
	n.asks = s.openAsks(ctx)
	return n
}

// officerNeedsOf is what waits on the viewer for owner's officer id, from
// the page's reads and (a builtin officer) its refusals.
func (s *Server) officerNeedsOf(ctx context.Context, rd needsReads, owner int64, id string, builtinBackend bool) officerNeeds {
	var n officerNeeds
	for _, c := range rd.drafts {
		d := c.Draft
		if d.Status != plugins.DraftPending || d.Officer == nil || *d.Officer != id || d.OwnerID != owner {
			continue
		}
		n.drafts = append(n.drafts, c)
	}
	n.asks = raisedByOfficer(rd.asks, owner, id)
	if builtinBackend {
		list, err := s.officerStore.RefusalsAwaitingOwner(ctx, owner, id, s.officerNow().Add(-builtin.RefusalWindow))
		if err != nil {
			s.logger.Printf("officers: read %s's refusals for its page: %v", id, err)
		}
		n.refusals = list
	}
	return n
}

// needsYouHTML is the officer page's Needs-you section: run is its budget and
// run controls (officerRunHTML, drawn first), then each decision.
func (s *Server) needsYouHTML(ctx context.Context, sc officerScope, v declaredOfficerView, n officerNeeds, run string, controllable bool) string {
	back := officerHref(sc, v.ID, "")
	var b strings.Builder
	count := n.count()
	b.WriteString(`<section class="` + adminCard + `" id="` + officerNeedsYouID + `" data-needs-count="` + strconv.Itoa(count) + `">
<h2 class="text-base font-semibold text-fg">Needs you` + needsCountHTML(count) + `</h2>
`)
	b.WriteString(run)
	if count == 0 {
		b.WriteString(`<p class="text-sm text-fg2" id="officer-needs-none">Nothing from it waits on you.</p>`)
	}
	if len(n.drafts) > 0 {
		// THE Asks page's cards and their routes (draftListHTMLIn): a
		// decision posts where it does there, and comes back here.
		b.WriteString(`<ul class="flex flex-col gap-2" data-needs-drafts>` + draftListHTMLIn(n.drafts, "", cardPlace{back: back}) + `</ul>`)
	}
	if len(n.asks) > 0 {
		items := make([]attentionItem, 0, len(n.asks))
		for _, a := range n.asks {
			items = append(items, askItem(a))
		}
		sortAttentionItems(items)
		// THE answer form (askAnswerFormHTML, through the session pages'
		// item partial): answered here, it returns here (attentionReturnPage
		// admits an officer's page).
		s.withAskBoxes(ctx, items, false)
		b.WriteString(`<ul class="flex flex-col gap-2" data-needs-asks>` + attentionItemsHTMLTo(items, time.Now(), back) + `</ul>`)
	}
	for _, rf := range n.refusals {
		b.WriteString(refusalCardHTML(sc, v.ID, rf, controllable, s.officerNow()))
	}
	b.WriteString(`</section>`)
	return b.String()
}

// needsCountHTML is the Needs-you heading's count, nothing for none.
func needsCountHTML(n int) string {
	if n == 0 {
		return ""
	}
	return ` <span class="ml-1 inline-flex min-w-[1.5rem] items-center justify-center rounded-full bg-accent px-1.5 text-xs font-semibold text-on-accent" data-needs-badge>` + strconv.Itoa(n) + `</span>`
}

// refusalWhat is a refused draft's kind in words.
func refusalWhat(targetKind string) string {
	switch targetKind {
	case plugins.TargetReply:
		return "A reply"
	case plugins.TargetDM:
		return "A message"
	case plugins.TargetFollow:
		return "A follow"
	case plugins.TargetLike:
		return "A like"
	}
	return "A post"
}

// refusalBy is who refused it, in words.
func refusalBy(by string) string {
	if by == "muster" {
		return "muster"
	}
	return "its plugin"
}

// refusalCardHTML is one refusal waiting on its owner: what was refused, why
// (the plugin's or muster's own words, as filed), what the officer did with
// it, and — for a viewer who may write — Re-file (at a time) and Drop.
func refusalCardHTML(sc officerScope, officer string, rf builtin.Refusal, writable bool, now time.Time) string {
	esc := html.EscapeString
	item := strconv.FormatInt(rf.Item, 10)
	where := ""
	if rf.Venue != "" {
		where = " on " + venueWords(rf.Venue)
	}
	var b strings.Builder
	b.WriteString(`<article class="flex flex-col gap-2 rounded-xl bg-bg px-3 py-3 ring-1 ring-inset ring-edge" id="refusal-` + item + `" data-refusal="` + item + `">
<p class="text-sm font-semibold text-fg">` + esc(refusalWhat(rf.TargetKind)+where+" was not sent") + `</p>
<p class="break-words text-sm text-fg2" data-refusal-why>` + esc(capFirst(refusalBy(rf.By))) + ` refused it, saying «` + esc(flows.VisibleEscape(plugins.RefusalText(rf.Detail))) + `».</p>
`)
	switch {
	case rf.Status == officers.StatusOpen && rf.NotBefore != nil && rf.NotBefore.After(now):
		b.WriteString(`<p class="text-xs text-fg2" data-refusal-state>The officer picks it up after ` + esc(fmtTime(*rf.NotBefore)) + `.</p>`)
	case rf.Status == officers.StatusOpen:
		b.WriteString(`<p class="text-xs text-fg2" data-refusal-state>The officer has not picked it up yet: it redrafts it when it next runs, unless you re-file it for later or drop it.</p>`)
	case rf.Summary != "":
		b.WriteString(`<p class="break-words text-xs text-fg2" data-refusal-state>The officer: ` + esc(truncateRunes(rf.Summary, 240)) + `</p>`)
	}
	code := fmt.Sprintf("item %d · draft %d · %s · refused by %s", rf.Item, rf.Draft, rf.Venue, rf.By)
	if rf.RetryAt != nil {
		code += " · retry_at " + rf.RetryAt.UTC().Format(time.RFC3339)
	}
	b.WriteString(ui.InfoSheetHTML(ui.InfoSheet{ID: "refusal-" + item + "-info", Label: "Details of this refusal", Heading: "Details",
		Lead: "What muster records for this refusal:", Code: code}))
	if writable {
		action := esc(officersPagePath + "/" + url.PathEscape(officer) + "/refusals/" + item)
		b.WriteString(`<form method="POST" action="` + action + `/refile" class="flex flex-col gap-2" data-refusal-refile>` + sc.ownerField() + `
<label for="refile-when-` + item + `" class="text-xs font-medium text-fg">Re-file it</label>
<select id="refile-when-` + item + `" name="when" class="` + adminInput + `">`)
		// The plugin's retry_at, when it named one Re-file can take, is the
		// default: "at" chosen and its time filled in (refileDefault).
		atValue := ""
		retry, hasRetry := refileDefault(rf, now)
		if hasRetry {
			atValue = retry.Format(refileAtLayout)
			b.WriteString(`<option value="at" selected data-refile-retry>` + esc("When the plugin says it can go: "+fmtTime(retry)) + `</option>`)
		}
		for _, o := range refileChoices {
			if hasRetry && o.key == "at" {
				continue
			}
			b.WriteString(`<option value="` + o.key + `">` + esc(o.label) + `</option>`)
		}
		b.WriteString(`</select>
<label for="refile-at-` + item + `" class="text-xs text-fg2">At a time you choose (UTC)</label>
<input id="refile-at-` + item + `" name="at" type="datetime-local" value="` + esc(atValue) + `" class="` + adminInput + `">
<span class="text-xs text-fg2">The officer files it again then, and you approve the new draft before anything is sent.</span>
<div class="flex flex-wrap gap-2"><button type="submit" class="` + adminButton + `" data-refusal-refile-submit>Re-file</button>
<button type="submit" formaction="` + action + `/drop" class="` + adminQuiet + `" data-refusal-drop>Drop it</button></div></form>`)
	}
	b.WriteString(`</article>`)
	return b.String()
}

// capFirst is s with its first letter upper-case.
func capFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// venueWords is a venue's name in words ("x" is X).
func venueWords(v string) string {
	switch v {
	case plugins.VenueX:
		return "X"
	case plugins.VenueReddit:
		return "Reddit"
	}
	return v
}

// refileChoice is one of Re-file's offered times.
type refileChoice struct {
	key, label string
}

// refileChoices are Re-file's times, the first the default. "at" reads the
// form's own time.
var refileChoices = []refileChoice{
	{"1h", "In an hour"},
	{"6h", "In 6 hours"},
	{"tomorrow", "Tomorrow at 00:00 UTC (a new day)"},
	{"at", "At the time below"},
}

// refileAtLayout is Re-file's `at` field's layout (a datetime-local, UTC).
const refileAtLayout = "2006-01-02T15:04"

// refileDefault is the Re-file time refusal rf offers first: the plugin's
// retry_at, rounded UP to the minute (the field has no seconds, and earlier
// than the plugin said would be refused again) — when it named one that is
// still ahead of now and within builtin.MaxRefileAhead. ok false: the plain
// choices, the first of them the default, as before retry_at.
func refileDefault(rf builtin.Refusal, now time.Time) (time.Time, bool) {
	if rf.RetryAt == nil {
		return time.Time{}, false
	}
	t := rf.RetryAt.UTC()
	if r := t.Truncate(time.Minute); !r.Equal(t) {
		t = r.Add(time.Minute)
	}
	if !t.After(now) || t.After(now.Add(builtin.MaxRefileAhead)) {
		return time.Time{}, false
	}
	return t, true
}

// refileAt is when a Re-file's `when` (and `at`, for "at") asks for, at now;
// ok false for a choice or time muster does not read.
func refileAt(when, at string, now time.Time) (time.Time, bool) {
	switch when {
	case "1h":
		return now.Add(time.Hour), true
	case "6h":
		return now.Add(6 * time.Hour), true
	case "tomorrow":
		return now.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour), true
	case "at":
		t, err := time.ParseInLocation(refileAtLayout, strings.TrimSpace(at), time.UTC)
		return t, err == nil
	}
	return time.Time{}, false
}

// --- the refusal routes -----------------------------------------------------------

// controlTarget resolves the officer a run control or a refusal choice acts
// on: one of the scope's BUILTIN officers, declared in the database or by
// MUSTER_OFFICERS, that the viewer may write (their own, or a team's they
// manage). These write the officer's QUEUE and its on-off switch, never its
// declaration, so an env officer takes them too. ok false: answered already.
func (s *Server) controlTarget(w http.ResponseWriter, r *http.Request) (sc officerScope, id, key string, env, ok bool) {
	sc, ok = s.scopeFor(w, r)
	if !ok {
		return sc, "", "", false, false
	}
	id = r.PathValue("officer")
	key, env, err := s.controlTargetOf(r.Context(), sc, id)
	switch {
	case errors.Is(err, errQueueInHQ):
		s.renderNotice(w, r, http.StatusConflict, "Its queue is in its HQ", "This officer's queue and switch live in its HQ, not in muster.")
	case errors.Is(err, errManagerOnly):
		s.renderNotice(w, r, http.StatusForbidden, "Only a manager changes team officers", TeamOfficerManagerOnly)
	case err != nil:
		code, msg := officerPageRefusal(err)
		if code == http.StatusInternalServerError {
			s.logger.Printf("officers: read officer %s of %d: %v", id, sc.Owner, err)
		}
		s.renderNotice(w, r, code, "No such officer", msg)
	default:
		return sc, id, key, env, true
	}
	return sc, id, "", env, false
}

// Why controlTargetOf refuses an officer.
var (
	// errQueueInHQ: an HQ-backed officer: its queue and switch are its HQ's.
	errQueueInHQ = errors.New("this officer's queue and switch live in its HQ, not in muster")
	// errManagerOnly: a team's officer, and the person does not manage it.
	errManagerOnly = errors.New(TeamOfficerManagerOnly)
)

// controlTargetOf is controlTarget's rule over scope sc and officer id,
// answering nothing (the page and the chief's chief_officer_control share
// it): the officer's wake-loop key ("" for none) and whether MUSTER_OFFICERS
// declares it, or errQueueInHQ, errManagerOnly, or the store's error for an
// officer that is not declared (a def-only one included).
func (s *Server) controlTargetOf(ctx context.Context, sc officerScope, id string) (key string, env bool, err error) {
	if o, isEnv := s.officerRT.envOfficer(sc.Owner, id); isEnv {
		if !o.IsBuiltin() {
			return "", true, errQueueInHQ
		}
		if !sc.Manage {
			return "", true, errManagerOnly
		}
		return o.Key(), true, nil
	}
	if _, err := s.officerStore.GetDeclared(ctx, sc.Owner, id); err != nil {
		return "", false, err
	}
	if !sc.Manage {
		return "", false, errManagerOnly
	}
	if st, ok := s.officerRT.declaredView(sc.Owner, id); ok && st.officer != nil {
		key = st.officer.Key()
	}
	return key, false, nil
}

// refusalItem is the {item} path value, ok false for none.
func refusalItem(r *http.Request) (int64, bool) {
	n, err := strconv.ParseInt(r.PathValue("item"), 10, 64)
	return n, err == nil && n > 0
}

// refusalRefused words a refused refile or drop.
func refusalRefused(err error) (int, string) {
	var hq *officers.HQError
	switch {
	case errors.Is(err, builtin.ErrNotFound):
		return http.StatusNotFound, "That refusal is not this officer's, or it is gone."
	case errors.Is(err, builtin.ErrDecided):
		return http.StatusConflict, "That refusal was already re-filed or dropped."
	case errors.Is(err, builtin.ErrRedrafted):
		return http.StatusConflict, "The officer already filed a new draft for it: decide that draft instead."
	case errors.As(err, &hq) && hq.Status == http.StatusConflict:
		return http.StatusConflict, "The officer is working on it right now: try again once it is done."
	case errors.Is(err, builtin.ErrInvalid):
		return http.StatusBadRequest, strings.TrimPrefix(err.Error(), builtin.ErrInvalid.Error()+": ")
	}
	return http.StatusInternalServerError, "That could not be recorded. Try again in a moment."
}

// handleOfficerRefusalRefile is POST /officers/{officer}/refusals/{item}/refile
// {when, at?}: the refused work comes back to the officer at that time
// (builtin.RefileRefusal), as its owner's own item; the owner approves the
// draft it files then.
func (s *Server) handleOfficerRefusalRefile(w http.ResponseWriter, r *http.Request) {
	sc, id, key, _, ok := s.controlTarget(w, r)
	if !ok {
		return
	}
	item, ok := refusalItem(r)
	if !ok {
		s.renderOfficerDetail(w, r, sc, id, http.StatusNotFound, "No such refusal.")
		return
	}
	now := s.officerNow()
	at, ok := refileAt(r.FormValue("when"), r.FormValue("at"), now)
	if !ok {
		s.renderOfficerDetail(w, r, sc, id, http.StatusBadRequest, "Choose when to re-file it (a time is YYYY-MM-DD HH:MM, UTC).")
		return
	}
	carrier, err := s.officerStore.RefileRefusal(r.Context(), sc.Owner, id, item, at, now)
	if err != nil {
		code, msg := refusalRefused(err)
		if code == http.StatusInternalServerError {
			s.logger.Printf("officers: re-file refusal %d of %s (owner %d): %v", item, id, sc.Owner, err)
		}
		s.renderOfficerDetail(w, r, sc, id, code, msg)
		return
	}
	s.logger.Printf("officers: user %d re-filed refusal %d of %s (owner %d) at %s as item %d", authz.MustFrom(r.Context()).UserID, item, id, sc.Owner, at.UTC().Format(time.RFC3339), carrier)
	if sc.Team {
		s.auditTeam(r.Context(), actorOf(r), sc.Owner, "officer.refusal.refile", "officer_def:"+id, map[string]any{"item": item, "at": at.UTC(), "as": carrier})
	}
	if key != "" && s.officerRT.rec != nil && !at.After(now) {
		s.officerRT.rec.Due(key)
	}
	http.Redirect(w, r, officerReturnDone(officerHref(sc, id, ""), "refiled"), http.StatusSeeOther)
}

// handleOfficerRefusalDrop is POST /officers/{officer}/refusals/{item}/drop:
// the refusal is decided, and an open one finished, so it is never redrafted
// (builtin.DropRefusal).
func (s *Server) handleOfficerRefusalDrop(w http.ResponseWriter, r *http.Request) {
	sc, id, _, _, ok := s.controlTarget(w, r)
	if !ok {
		return
	}
	item, ok := refusalItem(r)
	if !ok {
		s.renderOfficerDetail(w, r, sc, id, http.StatusNotFound, "No such refusal.")
		return
	}
	if err := s.officerStore.DropRefusal(r.Context(), sc.Owner, id, item, s.officerNow()); err != nil {
		code, msg := refusalRefused(err)
		if code == http.StatusInternalServerError {
			s.logger.Printf("officers: drop refusal %d of %s (owner %d): %v", item, id, sc.Owner, err)
		}
		s.renderOfficerDetail(w, r, sc, id, code, msg)
		return
	}
	s.logger.Printf("officers: user %d dropped refusal %d of %s (owner %d)", authz.MustFrom(r.Context()).UserID, item, id, sc.Owner)
	if sc.Team {
		s.auditTeam(r.Context(), actorOf(r), sc.Owner, "officer.refusal.drop", "officer_def:"+id, map[string]any{"item": item})
	}
	http.Redirect(w, r, officerReturnDone(officerHref(sc, id, ""), "dropped"), http.StatusSeeOther)
}

// --- budget and run controls ------------------------------------------------------

// runningOfficer is owner's officer id as it runs (its declared budget
// included); ok false when it does not run, so no budget is known.
func (s *Server) runningOfficer(owner int64, id string) (officers.Officer, bool) {
	if o, ok := s.officerRT.envOfficer(owner, id); ok {
		return o, true
	}
	if st, ok := s.officerRT.declaredView(owner, id); ok && st.officer != nil {
		return *st.officer, true
	}
	return officers.Officer{}, false
}

// officerBudgetOf is the daily budget owner's officer id is held to on now's
// UTC day: as it runs, with its owner's raise for today applied
// (officers.Officer.WithRaisedCap, the cap's own rule), and whether today's is
// a raise. 0: none, or not known (it does not run). An unreadable raise is the
// declared budget, as the wake loop reads it.
func (s *Server) officerBudgetOf(ctx context.Context, owner int64, id string, now time.Time) (budget float64, raised bool) {
	o, ok := s.runningOfficer(owner, id)
	if !ok {
		return 0, false
	}
	declared := o.Budget.DailyUSD
	if declared > 0 {
		if m, err := s.officerStore.RaisedCapsOn(ctx, []int64{owner}, now); err != nil {
			s.logger.Printf("officers: read %s's raised cap for its page: %v", id, err)
		} else {
			o = o.WithRaisedCap(m[builtin.OfficerRef{Owner: owner, Officer: id}])
		}
	}
	return o.Budget.DailyUSD, o.Budget.DailyUSD > declared
}

// officerSpentToday is owner's officer id's spend on now's UTC day (the
// budget's own sum: builtin.Store.SpentOn), ok false when it cannot be read.
func (s *Server) officerSpentToday(ctx context.Context, owner int64, id string, now time.Time) (float64, bool) {
	m, err := s.officerStore.SpentOn(ctx, []int64{owner}, now)
	if err != nil {
		s.logger.Printf("officers: read %s's spend for its page: %v", id, err)
		return 0, false
	}
	return m[builtin.OfficerRef{Owner: owner, Officer: id}], true
}

// officerRunHTML is the Needs-you section's budget and run controls: today's
// spend against the cap and "held" once the cap stops it (the cap's own
// predicate, officers.Officer.OverBudget's rule over the same sum), and — for
// a viewer who may write — Run now and Pause / Resume.
func (s *Server) officerRunHTML(ctx context.Context, sc officerScope, v declaredOfficerView, controllable bool) string {
	esc := html.EscapeString
	if v.Backend != officers.BackendBuiltin {
		return ""
	}
	now := s.officerNow()
	budget, raised := s.officerBudgetOf(ctx, sc.Owner, v.ID, now)
	spent, read := s.officerSpentToday(ctx, sc.Owner, v.ID, now)
	var b strings.Builder
	b.WriteString(`<div class="flex flex-col gap-2 rounded-xl bg-bg px-3 py-3 ring-1 ring-inset ring-edge" id="officer-run">`)
	switch {
	case !read:
		b.WriteString(`<p class="text-sm text-fg2" data-run-spend>Today's spend could not be read just now.</p>`)
	case budget > 0:
		reset := officers.CapResetAt(now).UTC().Format("15:04 UTC")
		which := " daily budget."
		if raised {
			which = " budget for today (you raised it; it goes back at " + reset + ")."
		}
		b.WriteString(`<p class="text-sm text-fg" data-run-spend="` + esc(fmt.Sprintf("%.2f/%.2f", spent, budget)) + `">Spent today: ` + esc(fmt.Sprintf("$%.2f", spent)) + ` of its ` + esc(navUSDWords(budget)) + esc(which) + `</p>`)
		if spent >= budget {
			b.WriteString(`<p class="rounded-lg bg-st-warning-bg px-3 py-2 text-sm text-st-warning-fg" data-run-held>Held: today's budget is spent, so muster does not wake it for new work until ` +
				esc(reset) + `. Run now still wakes it: work you give it yourself gets past the cap.</p>`)
			b.WriteString(s.capRaiseFormHTML(ctx, sc, v.ID))
		}
	default:
		b.WriteString(`<p class="text-sm text-fg" data-run-spend="` + esc(fmt.Sprintf("%.2f", spent)) + `">Spent today: ` + esc(fmt.Sprintf("$%.2f", spent)) + ` (it has no daily budget).</p>`)
	}
	if controllable {
		form := func(suffix, label, cls string) string {
			return `<form method="POST" action="` + esc(officersPagePath+"/"+url.PathEscape(v.ID)+suffix) + `">` + sc.ownerField() +
				`<button type="submit" class="` + cls + `">` + label + `</button></form>`
		}
		b.WriteString(`<div class="flex flex-wrap gap-2" data-run-actions>`)
		b.WriteString(form("/run", "Run now", adminButton))
		if v.Paused != nil && *v.Paused {
			b.WriteString(form("/resume", "Resume", adminQuiet))
		} else {
			b.WriteString(form("/pause", "Pause", adminQuiet))
		}
		b.WriteString(`</div>`)
	}
	b.WriteString(`</div>`)
	return b.String()
}

// navUSDWords is a budget in dollars, whole dollars without cents ("$10").
func navUSDWords(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	return "$" + strings.TrimSuffix(s, ".00")
}

// --- what it did ------------------------------------------------------------------

// timelineShown is how many entries "What it did" draws.
const timelineShown = 30

// timelineEntry is one line of "What it did".
type timelineEntry struct {
	at   time.Time
	html string
}

// itemKindWords is an item's kind as a person reads it.
func itemKindWords(kind string) string {
	switch kind {
	case runNowKind:
		return "A run you asked for"
	case "scheduled.pass":
		return "Its scheduled pass"
	case builtin.DraftRefusedKind:
		return "A refused draft"
	case "plugin.reply":
		return "A reply to one of your posts"
	case "plugin.engagement":
		return "Someone engaged with your posts"
	case "plugin.note", "plugin.snapshot", "plugin.plan":
		return "A note from its plugin"
	case "session.stalled":
		return "A stopped session"
	}
	return "A piece of work"
}

// itemOutcomeWords is how an item ended, in words.
func itemOutcomeWords(it pageItem) string {
	switch {
	case it.Status == officers.StatusEscalated:
		return "handed to you"
	case it.Outcome == "no_action":
		return "nothing to do"
	}
	return "done"
}

// postURL is where a post muster sent can be read, built from its venue and
// the reference its plugin reported — only where muster knows the venue's
// address for it (an X post's numeric id); "" otherwise.
func postURL(venue, targetKind, ref string) string {
	if venue != plugins.VenueX || (targetKind != plugins.TargetPost && targetKind != plugins.TargetReply) {
		return ""
	}
	if n := len(ref); n < 5 || n > 25 {
		return ""
	}
	for _, c := range ref {
		if c < '0' || c > '9' {
			return ""
		}
	}
	return "https://x.com/i/web/status/" + ref
}

// sentWords is what a sent draft did, in words.
func sentWords(d plugins.Draft) string {
	where := venueWords(d.VenueText())
	switch d.Target().Kind {
	case plugins.TargetReply:
		return "Replied on " + where
	case plugins.TargetDM:
		return "Sent a message on " + where
	case plugins.TargetFollow:
		return "Followed " + flows.VisibleEscape(d.Target().Handle) + " on " + where
	case plugins.TargetLike:
		return "Liked a post on " + where
	}
	return "Posted on " + where
}

// timelineHTML is "What it did": owner's officer id's finished items and its
// sent drafts, newest first, at most timelineShown; seen is when the viewer
// last looked (an entry finished after it is marked new), nil for never.
// Today's spend heads it.
func (s *Server) timelineHTML(ctx context.Context, sc officerScope, v declaredOfficerView, seen *time.Time) string {
	esc := html.EscapeString
	var b strings.Builder
	b.WriteString(`<section class="` + adminCard + `" id="officer-timeline"><h2 class="text-base font-semibold text-fg">What it did</h2>`)
	if v.Backend != officers.BackendBuiltin {
		b.WriteString(`<p class="text-sm text-fg2">What it did is in its HQ.</p></section>`)
		return b.String()
	}
	items, err := s.officerItems(ctx, sc.Owner, v.ID, 100)
	if err != nil {
		s.logger.Printf("officers: read %s's items for its timeline: %v", v.ID, err)
		b.WriteString(`<p class="text-sm text-fg2">It could not be read just now.</p></section>`)
		return b.String()
	}
	// Its sent drafts, by the item each was filed for.
	sent := map[int64]plugins.Draft{}
	var loose []plugins.Draft
	if s.ext.Plugins != nil {
		if cards, err := s.ext.Plugins.DraftCards(ctx, 100); err == nil {
			for _, c := range cards {
				d := c.Draft
				if d.Status != plugins.DraftSent || d.Officer == nil || *d.Officer != v.ID || d.OwnerID != sc.Owner {
					continue
				}
				if d.OfficerItemID != nil {
					sent[*d.OfficerItemID] = d
				} else {
					loose = append(loose, d)
				}
			}
		}
	}
	isNew := func(t time.Time) bool { return seen == nil || t.After(*seen) }
	var entries []timelineEntry
	for _, it := range items {
		if it.Status != officers.StatusDone && it.Status != officers.StatusEscalated {
			continue
		}
		if d, ok := sent[it.ID]; ok {
			delete(sent, it.ID)
			entries = append(entries, timelineEntry{it.UpdatedAt, sentEntryHTML(d, &it, isNew(it.UpdatedAt))})
			continue
		}
		entries = append(entries, timelineEntry{it.UpdatedAt, itemEntryHTML(it, isNew(it.UpdatedAt))})
	}
	for _, d := range sent { // sent for an item the read did not reach
		loose = append(loose, d)
	}
	for _, d := range loose {
		at := d.CreatedAt
		if d.FinishedAt != nil {
			at = *d.FinishedAt
		}
		entries = append(entries, timelineEntry{at, sentEntryHTML(d, nil, isNew(at))})
	}
	// What the chief did to it (chief_officers.go): each of its acts is an
	// audit row, read back here.
	entries = append(entries, s.chiefActionEntries(ctx, sc, v.ID, isNew)...)
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].at.After(entries[j].at) })
	if len(entries) > timelineShown {
		entries = entries[:timelineShown]
	}
	if spent, ok := s.officerSpentToday(ctx, sc.Owner, v.ID, s.officerNow()); ok {
		b.WriteString(`<p class="text-sm text-fg2" data-timeline-spend>Today it has spent ` + esc(fmt.Sprintf("$%.2f", spent)) + `.</p>`)
	}
	if len(entries) == 0 {
		b.WriteString(`<p class="text-sm text-fg2" id="officer-timeline-empty">Nothing finished yet.</p></section>`)
		return b.String()
	}
	b.WriteString(`<ol class="flex flex-col divide-y divide-line" data-timeline>`)
	for _, e := range entries {
		b.WriteString(e.html)
	}
	b.WriteString(`</ol></section>`)
	return b.String()
}

// newMark is a timeline entry's "new" mark.
const newMark = `<span class="rounded-full bg-accent px-1.5 text-[11px] font-semibold text-on-accent" data-timeline-new>new</span>`

// entryHead is a timeline entry's first line: its title, when, and new.
func entryHead(title string, at time.Time, fresh bool) string {
	n := ""
	if fresh {
		n = newMark
	}
	return `<div class="flex flex-wrap items-center gap-2"><span class="min-w-0 flex-1 break-words text-sm font-semibold text-fg" data-timeline-title>` +
		html.EscapeString(title) + `</span><time class="text-xs text-fg2" datetime="` + at.UTC().Format(time.RFC3339) + `">` + html.EscapeString(fmtTime(at)) + `</time>` + n + `</div>`
}

// sentEntryHTML is a sent draft's entry: what it did and where, the text it
// sent (cut), a link to the post where muster can build one, and its ids
// behind an ⓘ. it is the item it was filed for, nil for none.
func sentEntryHTML(d plugins.Draft, it *pageItem, fresh bool) string {
	esc := html.EscapeString
	at := d.CreatedAt
	if d.FinishedAt != nil {
		at = *d.FinishedAt
	}
	if it != nil {
		at = it.UpdatedAt
	}
	id := "draft-" + strconv.FormatInt(d.ID, 10)
	var b strings.Builder
	b.WriteString(`<li class="flex flex-col gap-1 py-3" data-timeline-entry="sent" data-timeline-draft="` + strconv.FormatInt(d.ID, 10) + `">` + entryHead(sentWords(d), at, fresh))
	if body := strings.TrimSpace(d.Body); body != "" {
		b.WriteString(`<p class="whitespace-pre-wrap break-words text-sm text-fg2" data-timeline-body>` + esc(truncateRunes(flows.VisibleEscape(body), 280)) + `</p>`)
	}
	foot := ""
	if u := postURL(d.VenueText(), d.Target().Kind, d.ExternalRef); u != "" {
		foot = `<a href="` + esc(u) + `" target="_blank" rel="noopener noreferrer" class="text-sm text-fg underline underline-offset-2" data-timeline-post-link>See the post</a>`
	}
	code := "draft " + strconv.FormatInt(d.ID, 10)
	if it != nil {
		code = "item " + strconv.FormatInt(it.ID, 10) + " · " + code
	}
	if d.ExternalRef != "" {
		code += " · reference " + d.ExternalRef
	}
	b.WriteString(`<div class="flex items-center gap-2">` + foot + `<span class="flex-1"></span>` +
		ui.InfoSheetHTML(ui.InfoSheet{ID: "timeline-" + id + "-info", Label: "Details of this post", Heading: "Details", Lead: "What muster records for it:", Code: code}) + `</div></li>`)
	return b.String()
}

// itemEntryHTML is a finished item's entry: its kind and how it ended in
// words, the officer's own summary, and its ids behind an ⓘ.
func itemEntryHTML(it pageItem, fresh bool) string {
	esc := html.EscapeString
	id := strconv.FormatInt(it.ID, 10)
	title := itemKindWords(it.Kind) + ": " + itemOutcomeWords(it)
	var b strings.Builder
	b.WriteString(`<li class="flex flex-col gap-1 py-3" data-timeline-entry="item" data-timeline-item="` + id + `" data-timeline-kind="` + esc(it.Kind) + `">` + entryHead(title, it.UpdatedAt, fresh))
	if it.Summary != "" {
		b.WriteString(`<p class="break-words text-sm text-fg2" data-timeline-summary>` + esc(truncateRunes(it.Summary, 400)) + `</p>`)
	}
	b.WriteString(`<div class="flex justify-end">` + ui.InfoSheetHTML(ui.InfoSheet{ID: "timeline-item-" + id + "-info", Label: "Details of this entry", Heading: "Details",
		Lead: "What muster records for it:", Code: "item " + id + " · " + it.Kind + " · " + it.SubjectKey}) + `</div></li>`)
	return b.String()
}

// --- the Officers list's Needs you ---------------------------------------------------

// needsLink is one officer's line in the Officers list's combined Needs you.
type needsLink struct {
	name, href string
	n          int
}

// needsSummaryHTML is the Officers list's combined Needs you: each officer
// with something waiting on the viewer, how much, and a link to its section.
func needsSummaryHTML(links []needsLink) string {
	esc := html.EscapeString
	total := 0
	for _, l := range links {
		total += l.n
	}
	var b strings.Builder
	b.WriteString(`<section class="` + adminCard + `" id="officers-needs-you" data-needs-count="` + strconv.Itoa(total) + `"><h2 class="text-base font-semibold text-fg">Needs you` + needsCountHTML(total) + `</h2>`)
	if total == 0 {
		b.WriteString(`<p class="text-sm text-fg2">Nothing from your officers waits on you.</p></section>`)
		return b.String()
	}
	b.WriteString(`<ul class="flex flex-col gap-1">`)
	for _, l := range links {
		if l.n == 0 {
			continue
		}
		b.WriteString(`<li><a class="press flex min-h-[44px] items-center gap-2 rounded-lg px-2 text-sm text-fg hover:bg-s2" href="` + esc(l.href+"#"+officerNeedsYouID) + `" data-needs-officer><span class="min-w-0 flex-1 truncate">` +
			esc(l.name) + `</span><span class="text-xs text-fg2">` + esc(plural(l.n, "decision")) + `</span></a></li>`)
	}
	b.WriteString(`</ul></section>`)
	return b.String()
}

// unreadWords is a "new" count in words, "" for none.
func unreadWords(n int) string {
	if n <= 0 {
		return ""
	}
	return strconv.Itoa(n) + " new"
}
