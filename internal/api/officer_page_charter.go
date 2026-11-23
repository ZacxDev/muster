package api

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// THE OFFICER PAGE'S "INSTRUCTIONS" SECTION (operator: "a way to
// see the officer charter and also be able to edit it and refresh the
// officer").
//
// It shows a builtin officer's charter (its type's: builtin.Charter), its
// version and the version the officer last read — so a stale officer is
// visible ("It is still working from version 2") — and, for a person who may
// change the officer, an editor with two buttons:
//
//   - Save: the charter is written (builtin.Store.SetCharter); its version
//     goes up only when the text changed. A running instance reads it the
//     next time it claims or completes work: officer_claim and
//     officer_complete refuse an instance briefed on an older version until
//     it calls officer_brief (builtin.Backend.CharterCurrent).
//   - Save and refresh (operator decision: "Restart, fresh conversation"):
//     the same write, then every up instance is stopped and started again in
//     a NEW conversation (officers.Reconciler.Refresh, OfficerTurnContext's
//     unbriefedFirstTurn), whose first turn loads the new charter and brief.
//
// 🔴 THE CHARTER IS THE TYPE'S. When other officers share the type, the
// section says how many, beside the editor: a change reaches all of them.
// ---------------------------------------------------------------------------

// officerCharterID is the section's id.
const officerCharterID = "officer-charter"

// charterRead is what an officer's instances last read of its charter: the
// lowest version any of them read (the one a stale instance works from), and
// whether any has read one at all.
type charterRead struct {
	min int
	any bool
}

// charterReadOf is what instances (an officer's instance names; none: every
// name recorded) last read, from c.Briefed.
func charterReadOf(c builtin.Charter, instances []string) charterRead {
	var r charterRead
	consider := func(v int) {
		if !r.any || v < r.min {
			r.min = v
		}
		r.any = true
	}
	if len(instances) == 0 {
		for _, v := range c.Briefed {
			consider(v)
		}
		return r
	}
	for _, n := range instances {
		if v, ok := c.Briefed[n]; ok {
			consider(v)
		}
	}
	return r
}

// charterDraft is a Save the server refused or could not make: the text the
// person SUBMITTED and why, redrawn in the editor (never the stored charter,
// which would throw their edit away).
type charterDraft struct {
	text, problem string
}

type charterDraftKey struct{}

// withCharterDraft is ctx carrying d for the page drawn as the answer to it.
func withCharterDraft(ctx context.Context, d charterDraft) context.Context {
	return context.WithValue(ctx, charterDraftKey{}, d)
}

// charterDraftFrom is ctx's refused Save, ok false for none.
func charterDraftFrom(ctx context.Context) (charterDraft, bool) {
	d, ok := ctx.Value(charterDraftKey{}).(charterDraft)
	return d, ok
}

// charterSectionHTML is the Instructions section for officer v, whose
// charter is c; editable draws the editor (a person who may change it), and
// draft, when set, is a refused Save: its text in the editor, its problem
// beside it, and the editor open.
func charterSectionHTML(sc officerScope, v declaredOfficerView, c builtin.Charter, editable bool, draft *charterDraft) string {
	esc := html.EscapeString
	read := charterReadOf(c, v.Instances)
	stale := read.any && read.min < c.Version
	var b strings.Builder
	b.WriteString(`<section class="` + adminCard + `" id="` + officerCharterID + `" data-charter-version="` + strconv.Itoa(c.Version) +
		`" data-charter-read="` + strconv.Itoa(read.min) + `" data-charter-stale="` + strconv.FormatBool(stale) + `">
<div class="flex flex-wrap items-center gap-2"><h2 class="min-w-0 flex-1 text-base font-semibold text-fg">Instructions</h2>` +
		`<span class="` + ui.OfficerVersionChipClass + `" data-charter-version-chip>Version ` + strconv.Itoa(c.Version) + `</span>` +
		ui.InfoSheetHTML(charterSheet(v, c)) + `</div>
`)
	switch {
	case stale:
		b.WriteString(`<p class="` + ui.OfficerStaleClass + `" data-charter-stale-line>It is still working from version ` + strconv.Itoa(read.min) +
			`. Save and refresh restarts it on version ` + strconv.Itoa(c.Version) + `.</p>`)
	case read.any:
		b.WriteString(`<p class="text-sm text-fg2" data-charter-read-line>It has read this version.</p>`)
	default:
		b.WriteString(`<p class="text-sm text-fg2" data-charter-read-line>It has not read them yet: it does when it next starts.</p>`)
	}
	if c.SharedBy > 1 {
		b.WriteString(`<p class="text-sm text-fg2" data-charter-shared="` + strconv.Itoa(c.SharedBy) + `">` +
			esc(fmt.Sprintf("%d officers share these instructions: a change here changes them for all %d.", c.SharedBy, c.SharedBy)) + `</p>`)
	}
	if !editable {
		b.WriteString(`<div class="` + ui.OfficerCharterTextClass + `" data-charter-text>` + esc(c.Text) + `</div></section>`)
		return b.String()
	}
	open := ""
	text, problem := c.Text, ""
	if stale {
		open = " open"
	}
	if draft != nil {
		open, text, problem = " open", draft.text, draft.problem
	}
	b.WriteString(`<details class="flex flex-col gap-3" data-charter-body` + open + `><summary class="` + ui.OfficerDisclosureClass + `">Read or change them</summary>
<form method="POST" action="` + esc(officersPagePath+"/"+url.PathEscape(v.ID)+"/charter") + `" class="flex flex-col gap-3" data-charter-form>` + sc.ownerField() + `
<label for="officer-charter-text" class="text-sm font-medium text-fg">Its standing instructions</label>` + charterProblemHTML(problem) + `
<textarea id="officer-charter-text" name="charter" rows="8" required maxlength="` + strconv.Itoa(builtin.MaxCharterRunes) + `" class="` + ui.OfficerCharterInputClass + `" data-charter-input>` + esc(text) + `</textarea>
<div class="` + ui.OfficerButtonRowClass + `">` +
		`<button type="submit" name="refresh" value="1" class="` + adminButton + ` ` + ui.OfficerRowButtonClass + `" data-charter-save-refresh>Save and refresh</button>` +
		`<button type="submit" class="` + adminQuiet + ` ` + ui.OfficerRowButtonClass + `" data-charter-save>Save</button></div>
<p class="text-xs text-fg2">Save and refresh restarts it in a new conversation that starts from these instructions. Save alone: it switches to them the next time it picks up or finishes work.</p>
</form></details></section>`)
	return b.String()
}

// charterProblemHTML is a refused Save's problem beside the editor ("" for
// none).
func charterProblemHTML(problem string) string {
	if problem == "" {
		return ""
	}
	return `<p class="` + ui.OfficerStaleClass + `" role="alert" data-charter-error>` + html.EscapeString(problem) + `</p>`
}

// charterSheet is the section's ⓘ: the type and what each instance read.
func charterSheet(v declaredOfficerView, c builtin.Charter) ui.InfoSheet {
	names := make([]string, 0, len(c.Briefed))
	for n := range c.Briefed {
		names = append(names, n)
	}
	sort.Strings(names)
	read := make([]string, 0, len(names))
	for _, n := range names {
		read = append(read, fmt.Sprintf("%s=v%d", n, c.Briefed[n]))
	}
	code := fmt.Sprintf("type %s · charter_version %d · officers on this type %d", c.TypeKey, c.Version, c.SharedBy)
	if len(read) > 0 {
		code += " · read: " + strings.Join(read, ", ")
	}
	return ui.InfoSheet{ID: "officer-charter-info", Label: "Details of its instructions", Heading: "Details",
		Lead: "Its instructions belong to its type; each running copy records the version it last read.", Code: code}
}

// officerCharterHTML is the Instructions section for officer v on its page,
// or "" for an officer whose charter muster does not keep (an HQ officer) or
// cannot read just now (logged; the rest of the page is drawn).
func (s *Server) officerCharterHTML(ctx context.Context, sc officerScope, v declaredOfficerView) string {
	if v.Backend != officers.BackendBuiltin || s.officerStore == nil {
		return ""
	}
	c, err := s.officerStore.CharterOf(ctx, sc.Owner, v.ID)
	if err != nil {
		if !errors.Is(err, builtin.ErrNotFound) {
			s.logger.Printf("officers: read %s's charter for its page: %v", v.ID, err)
		}
		return ""
	}
	var draft *charterDraft
	if d, ok := charterDraftFrom(ctx); ok {
		draft = &d
	}
	return charterSectionHTML(sc, v, c, sc.Manage, draft)
}

// charterNotices are the section's notice codes (officerNotices holds them).
var charterNotices = map[string]string{
	"charter-saved":     "Saved. It switches to these instructions the next time it picks up or finishes work.",
	"charter-unchanged": "Nothing changed: those are already its instructions.",
	"refreshed":         "Saved. It is restarting in a new conversation that starts from these instructions.",
	"refreshed-idle":    "Saved. It is not running now: its next start is a new conversation that starts from these instructions.",
	// Every copy restarted; only work one was holding could not be handed
	// back at once (officers.RefreshResult.Unreleased).
	"refreshed-unreleased": "Saved. It is restarting in a new conversation; some work it was holding goes back to its queue in a few minutes.",
}

// refreshBudget bounds Save and refresh's restart, which runs detached from
// the request (handleOfficerCharter): stopping and starting each instance.
const refreshBudget = 2 * time.Minute

// handleOfficerCharter is POST /officers/{officer}/charter {charter,
// refresh?}: the charter written, and with refresh=1 the officer restarted in
// a new conversation. The officer is one the person may change
// (controlTarget: a builtin officer, theirs or a team's they manage).
func (s *Server) handleOfficerCharter(w http.ResponseWriter, r *http.Request) {
	sc, id, key, _, ok := s.controlTarget(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	// A browser submits a textarea's line breaks as CRLF: stored as typed,
	// every Save of unchanged text would read as a change (a new version).
	text := strings.ReplaceAll(r.FormValue("charter"), "\r\n", "\n")
	// A refused or failed Save redraws the editor with what was SUBMITTED,
	// and why (charterDraft) — never the stored charter.
	refused := func(code int, problem string) {
		r = r.WithContext(withCharterDraft(ctx, charterDraft{text: text, problem: problem}))
		s.renderOfficerDetail(w, r, sc, id, code, problem)
	}
	version, changed, err := s.officerStore.SetCharter(ctx, sc.Owner, id, text)
	if err != nil {
		switch {
		case errors.Is(err, builtin.ErrInvalid):
			refused(http.StatusBadRequest, strings.TrimPrefix(err.Error(), builtin.ErrInvalid.Error()+": "))
		case errors.Is(err, builtin.ErrNotFound):
			s.renderNotice(w, r, http.StatusNotFound, "No such officer", officerNotFound)
		default:
			s.logger.Printf("officers: user %d saving %s's charter (owner %d): %v", authz.MustFrom(ctx).UserID, id, sc.Owner, err)
			refused(http.StatusInternalServerError, "The instructions could not be saved. Try again in a moment.")
		}
		return
	}
	refresh := r.FormValue("refresh") == "1"
	s.logger.Printf("officers: user %d saved officer %s's charter (owner %d): version %d, changed %v, refresh %v", authz.MustFrom(ctx).UserID, id, sc.Owner, version, changed, refresh)
	if sc.Team {
		s.auditTeam(ctx, actorOf(r), sc.Owner, "officer.charter.put", "officer_def:"+id, map[string]any{"charter_version": version, "changed": changed, "refresh": refresh})
	}
	if !refresh {
		notice := "charter-saved"
		if !changed {
			notice = "charter-unchanged"
		}
		s.officerActionDone(w, r, sc, id, notice, nil)
		return
	}
	// 🔴 DETACHED FROM THE REQUEST (the pattern of accounts.go's refresh and
	// chief_restart_tool.go's release): a browser that goes away mid-refresh
	// must not cancel it half way — instances stopped and never started, or
	// started without their briefs forgotten. Bounded by refreshBudget.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshBudget)
	res, err := s.refreshOfficer(rctx, sc.Owner, id, key)
	cancel()
	notice, problem := refreshOutcome(res, err)
	if problem != "" {
		s.logger.Printf("officers: user %d refreshing %s (owner %d): %+v, %v", authz.MustFrom(ctx).UserID, id, sc.Owner, res, err)
		s.renderOfficerDetail(w, r, sc, id, http.StatusInternalServerError, problem)
		return
	}
	s.officerActionDone(w, r, sc, id, notice, nil)
}

// refreshOutcome is THE rule for what a Save and refresh tells the person:
// a notice when every running copy restarted (refreshed; refreshed-idle when
// none was running; refreshed-unreleased when work one held could not be
// handed back at once — the restart itself succeeded, so it is NOT "could
// not be restarted"), else a problem saying exactly what did not happen
// (refreshProblem).
func refreshOutcome(res officers.RefreshResult, err error) (notice, problem string) {
	if err != nil || len(res.NotStopped) > 0 || len(res.NotStarted) > 0 {
		return "", refreshProblem(res, err)
	}
	switch {
	case len(res.Restarted) == 0:
		return "refreshed-idle", ""
	case len(res.Unreleased) > 0:
		return "refreshed-unreleased", ""
	}
	return "refreshed", ""
}

// refreshProblem says exactly what of a Save and refresh did not happen (the
// instructions are saved either way), in plain words: how many of its copies
// restarted, which could not be stopped (they keep their old conversation),
// and which were stopped and not started again (they start with their next
// work). err is a failure before any of that: nothing was restarted.
func refreshProblem(res officers.RefreshResult, err error) string {
	parts := []string{"Saved."}
	total := len(res.Restarted) + len(res.NotStopped) + len(res.NotStarted)
	if err != nil && len(res.NotStarted) == 0 && len(res.NotStopped) == 0 {
		return "Saved, but it could not be restarted just now: try Save and refresh again in a moment."
	}
	parts = append(parts, fmt.Sprintf("%d of its %d running copies restarted in a new conversation.", len(res.Restarted), total))
	if n := len(res.NotStopped); n > 0 {
		parts = append(parts, fmt.Sprintf("%d could not be stopped and keep their old conversation.", n))
	}
	if n := len(res.NotStarted); n > 0 {
		parts = append(parts, fmt.Sprintf("%d stopped and did not start again; they start with their next work.", n))
	}
	if n := len(res.Unreleased); n > 0 {
		parts = append(parts, fmt.Sprintf("%d piece(s) of work they held go back to the queue in a few minutes.", n))
	}
	parts = append(parts, "Try Save and refresh again to restart the rest.")
	return strings.Join(parts, " ")
}

// refreshOfficer is "Save and refresh"'s restart of owner's officer id,
// whose wake-loop key is key ("" when it does not run here): every up
// instance stopped, every instance's brief forgotten, the stopped ones
// started again (officers.Reconciler.Refresh) — so each starts a NEW
// conversation (OfficerTurnContext). An officer with no wake loop here has
// its briefs forgotten only: its next start is new. Answers exactly what it
// did (officers.RefreshResult).
func (s *Server) refreshOfficer(ctx context.Context, owner int64, id, key string) (officers.RefreshResult, error) {
	forget := func(ctx context.Context) error { return s.officerStore.ForgetBriefs(ctx, owner, id) }
	if key == "" || s.officerRT == nil || s.officerRT.rec == nil {
		return officers.RefreshResult{}, forget(ctx)
	}
	return s.officerRT.rec.Refresh(ctx, key, forget)
}
