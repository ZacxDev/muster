package api

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/officers/builtin"
)

// THE OFFICER PAGE REDESIGN (the operator), through the real server,
// the real built-in store and the real wake loop: the Instructions section
// (charter text, "Version N", what it read, the stale line, the shared-type
// line), Save (a version only for a changed text), Save and refresh (a stop,
// a start, and a first turn that opens a NEW conversation), the spend bar
// with Raise from 80%, no machine-user warning and no Shadow / Live button on
// the page (the warning kept in the wizard).

const (
	rdA = "scout:alpha"
	rdB = "scout:beta"
	// Pairwise-distinct texts, none of them a constant the code writes.
	rdCharter1 = "Read the triage queue at nine; never post."
	rdCharter2 = "Read the triage queue at ten; label each item."
)

// rdEnv is two builtin officers of user 1 on ONE type ("scout"), so the
// type's charter is shared by two, with a $10 daily budget.
const rdEnv = `[{"id":"` + rdA + `","backend":"builtin","daily_budget_usd":10},{"id":"` + rdB + `","backend":"builtin","daily_budget_usd":10}]`

// rdRig is a page rig with rdEnv's two officers declared on type "scout".
func rdRig(t *testing.T) *declRig {
	t.Helper()
	r := newPageRig(t, declRigOpts{pods: true, env: rdEnv})
	sys := authz.System(context.Background())
	if err := r.store.PutType(sys, 1, builtin.Type{Key: "scout", Name: "Scout", Charter: rdCharter1}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{rdA, rdB} {
		if err := r.store.PutDef(sys, 1, builtin.Def{Officer: id, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

// briefed records instance names of officer as having read version v.
func briefed(t *testing.T, officer string, read map[string]int) {
	t.Helper()
	parts := []string{}
	args := []any{officer}
	for n, v := range read {
		args = append(args, n, v)
		parts = append(parts, "$"+strconv.Itoa(len(args)-1)+"::text, $"+strconv.Itoa(len(args))+"::int")
	}
	obj := "'{}'::jsonb"
	if len(parts) > 0 {
		obj = "jsonb_build_object(" + strings.Join(parts, ", ") + ")"
	}
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE officer_defs SET charter_briefed = `+obj+` WHERE owner_id = 1 AND officer = $1`, args...)
}

// charterSection is the page's Instructions section.
func charterSection(t *testing.T, page string) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)<section [^>]*id="officer-charter".*?</section>`).FindString(page)
	if m == "" {
		t.Fatalf("the page has no Instructions section:\n%.2000s", page)
	}
	return m
}

// TestTheInstructionsSectionShowsTheCharterAndWhatItRead (PR A (1)): the
// section draws the type's charter, "Version 2", and — with instance 1 having
// read version 1 — the stale line naming both versions; with it having read
// version 2, "It has read this version." and no stale line; with nothing
// read, "not read them yet". Two officers on the type: the shared line says 2.
func TestTheInstructionsSectionShowsTheCharterAndWhatItRead(t *testing.T) {
	r := rdRig(t)
	sys := authz.System(context.Background())
	// Version 2: the charter changed once.
	if err := r.store.PutType(sys, 1, builtin.Type{Key: "scout", Name: "Scout", Charter: rdCharter2}); err != nil {
		t.Fatal(err)
	}
	inst := r.officerFor(t, 1, rdA).InstanceName(1)

	briefed(t, rdA, map[string]int{inst: 1})
	sec := charterSection(t, r.page(t, 1, http.MethodGet, "/officers/"+rdA, nil).Body.String())
	for _, want := range []string{rdCharter2, `data-charter-version="2"`, `data-charter-read="1"`, `data-charter-stale="true"`,
		`>Version 2</span>`, "It is still working from version 1. Save and refresh restarts it on version 2.", `data-charter-shared="2"`,
		"2 officers share these instructions: a change here changes them for all 2."} {
		if !strings.Contains(sec, want) {
			t.Errorf("stale: the section lacks %q:\n%s", want, sec)
		}
	}
	if strings.Contains(sec, rdCharter1) {
		t.Errorf("the section shows the OLD charter text:\n%s", sec)
	}

	briefed(t, rdA, map[string]int{inst: 2})
	sec = charterSection(t, r.page(t, 1, http.MethodGet, "/officers/"+rdA, nil).Body.String())
	if !strings.Contains(sec, `data-charter-stale="false"`) || strings.Contains(sec, "data-charter-stale-line") || !strings.Contains(sec, "It has read this version.") {
		t.Errorf("current: the section still reads stale, or lacks its read line:\n%s", sec)
	}

	briefed(t, rdA, nil)
	sec = charterSection(t, r.page(t, 1, http.MethodGet, "/officers/"+rdA, nil).Body.String())
	if !strings.Contains(sec, "It has not read them yet") || strings.Contains(sec, "data-charter-stale-line") {
		t.Errorf("unread: %s", sec)
	}
}

// postCharter posts the Instructions form.
func postCharter(t *testing.T, r *declRig, id, text string, refresh bool) (code int, location string) {
	t.Helper()
	form := url.Values{"charter": {text}}
	if refresh {
		form.Set("refresh", "1")
	}
	rec := r.page(t, 1, http.MethodPost, "/officers/"+id+"/charter", form)
	return rec.Code, rec.Header().Get("Location")
}

// TestSaveBumpsTheVersionOnlyWhenTheTextChanged (PR A (2)): Save of a new
// text (CRLF, as a browser sends a textarea) stores it with LF and version 2;
// Save of the same text again — CRLF again — is "unchanged" at version 2; a
// blank text is refused with nothing written. The other officer on the type
// reads the change (the charter is the type's).
func TestSaveBumpsTheVersionOnlyWhenTheTextChanged(t *testing.T) {
	r := rdRig(t)
	ctx := dbtest.AsOperator(context.Background())
	typed := "Read the queue.\r\nLabel each item."
	if code, loc := postCharter(t, r, rdA, typed, false); code != http.StatusSeeOther || loc != "/officers/"+rdA+"?notice=charter-saved" {
		t.Fatalf("Save of a new text: %d %q", code, loc)
	}
	ty, err := r.store.GetType(ctx, 1, "scout")
	if err != nil || ty.CharterVersion != 2 || ty.Charter != "Read the queue.\nLabel each item." {
		t.Fatalf("after Save: version %d text %q (%v); want 2 and the LF text", ty.CharterVersion, ty.Charter, err)
	}
	if code, loc := postCharter(t, r, rdA, typed, false); code != http.StatusSeeOther || loc != "/officers/"+rdA+"?notice=charter-unchanged" {
		t.Fatalf("Save of the same text: %d %q", code, loc)
	}
	if ty, _ := r.store.GetType(ctx, 1, "scout"); ty.CharterVersion != 2 {
		t.Fatalf("Save of the same text moved the version to %d", ty.CharterVersion)
	}
	if rec := r.page(t, 1, http.MethodPost, "/officers/"+rdA+"/charter", url.Values{"charter": {"  \r\n "}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("a blank charter: %d, want 400", rec.Code)
	}
	if ty, _ := r.store.GetType(ctx, 1, "scout"); ty.CharterVersion != 2 || ty.Charter != "Read the queue.\nLabel each item." {
		t.Fatalf("a refused Save wrote: %+v", ty)
	}
	if sec := charterSection(t, r.page(t, 1, http.MethodGet, "/officers/"+rdB, nil).Body.String()); !strings.Contains(sec, "Label each item.") {
		t.Errorf("the other officer on the type does not read the saved charter:\n%s", sec)
	}
	if rec := r.page(t, declOther, http.MethodPost, "/officers/"+rdA+"/charter", url.Values{"charter": {"x"}, "owner": {"1"}}); rec.Code != http.StatusNotFound {
		t.Errorf("another user's Save of user 1's charter: %d, want 404", rec.Code)
	}
}

// TestSaveAndRefreshStartsAFreshConversation (PR A (3)): a running, briefed
// instance's first turn RESUMES (no new-conversation route — the control);
// Save and refresh stops it, forgets every brief, and starts it again with
// its first turn; that first turn now asks its ccd for a NEW conversation
// (agents.HeaderCCNewConversation "1"); once it reads its charter again
// (officer_brief records it), a later first turn resumes that conversation.
func TestSaveAndRefreshStartsAFreshConversation(t *testing.T) {
	r := newPageRig(t, declRigOpts{pods: true})
	if rec := r.hook(t, http.MethodPost, "/api/officers/declared", declPodBody); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	o := r.officerFor(t, 1, declID)
	r.enqueue(t, 1, declID, "first")
	if res := r.pass(t, o); len(res.Decision.Start) != 1 {
		t.Fatalf("control: the pass started %v; want instance 1", res.Decision.Start)
	}
	name := o.InstanceName(1)
	a, ok := r.agentNamed(name)
	if !ok {
		t.Fatalf("no instance %s", name)
	}
	newConv := func() string {
		t.Helper()
		cur, _ := r.agentNamed(name)
		rt, ok := agents.TurnRouteFrom(r.s.OfficerTurnContext(context.Background(), cur))
		if !ok {
			return ""
		}
		return rt.Header.Get(agents.HeaderCCNewConversation)
	}
	// It delivered its first turn and read its charter (version 1).
	r.ag.mu.Lock()
	for i := range r.ag.rows {
		if r.ag.rows[i].ID == a.ID {
			r.ag.rows[i].KickedOff, r.ag.rows[i].PendingNote = true, ""
		}
	}
	r.ag.mu.Unlock()
	briefed(t, declID, map[string]int{name: 1})
	// CONTROL: a first turn re-owed to a briefed instance resumes.
	if err := r.ag.RearmKickoff(context.Background(), a.ID, "again"); err != nil {
		t.Fatal(err)
	}
	if v := newConv(); v != "" {
		t.Fatalf("control: a briefed instance's first turn asks for a new conversation (%q)", v)
	}
	r.prov.mu.Lock()
	stops0, starts0 := len(r.prov.stops), len(r.prov.starts)
	r.prov.mu.Unlock()

	code, loc := postCharter(t, r, declID, "Scan the queue; label it; never post.", true)
	if code != http.StatusSeeOther || loc != "/officers/"+declID+"?notice=refreshed" {
		t.Fatalf("Save and refresh: %d %q\nlogs:\n%s", code, loc, r.logs)
	}
	r.prov.mu.Lock()
	stops, starts := r.prov.stops[stops0:], r.prov.starts[starts0:]
	r.prov.mu.Unlock()
	if len(stops) != 1 || stops[0] != a.ID || len(starts) != 1 || starts[0] != a.ID {
		t.Fatalf("Save and refresh: stopped %v, started %v; want instance %d stopped then started", stops, starts, a.ID)
	}
	var raw string
	if err := r.f.pool.QueryRow(authz.System(context.Background()), `SELECT charter_briefed::text FROM officer_defs WHERE owner_id = 1 AND officer = $1`, declID).Scan(&raw); err != nil || raw != "{}" {
		t.Fatalf("after Save and refresh charter_briefed = %q (%v); want {}", raw, err)
	}
	cur, _ := r.agentNamed(name)
	if cur.KickedOff || cur.PendingNote == "" || !strings.Contains(cur.PendingNote, "officer_brief") {
		t.Fatalf("the restarted instance is not owed its kickoff: kicked_off %v note %q", cur.KickedOff, cur.PendingNote)
	}
	if v := newConv(); v != "1" {
		t.Fatalf("the restarted instance's first turn asks for a new conversation: %q, want \"1\"", v)
	}
	// It reads its charter in the new conversation: later starts resume it.
	briefed(t, declID, map[string]int{name: 2})
	if v := newConv(); v != "" {
		t.Fatalf("after its new brief, a first turn still asks for a new conversation (%q)", v)
	}
}

// rdSpend books usd as user 1's officer's spend today, on instance n (one
// row per officer, pool, instance and day).
func rdSpend(t *testing.T, officer string, n int, usd float64) {
	t.Helper()
	dbtest.OwnerExec(t, dbtest.DSN(t), `INSERT INTO officer_llm_usage (owner_id, officer, pool, instance, day, requests, input_tokens, output_tokens,
		cache_read_tokens, cache_write_tokens, cost_usd, estimated_requests) VALUES (1, $1, 'judgement', $3, (now() AT TIME ZONE 'UTC')::date, 1, 0, 0, 0, 0, $2, 1)`, officer, usd, n)
	t.Cleanup(func() {
		dbtest.OwnerExec(t, dbtest.DSN(t), `DELETE FROM officer_llm_usage WHERE owner_id = 1 AND officer = $1`, officer)
	})
}

// spendOf is the page's spend bar.
func spendOf(t *testing.T, page string) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)<div class="flex flex-col gap-2" id="officer-spend".*?</details></div>`).FindString(page)
	if m == "" {
		t.Fatalf("the page has no spend bar:\n%.3000s", page)
	}
	return m
}

// TestRaiseIsOfferedFrom80PercentAndRaisesTodayOnly (PR A (4) on the page,
// and (5)): at $7.90 of $10 the bar is plain and offers no Raise; at $8.00 it
// is amber and offers "Raise today's limit to $20" — the raise-cap route
// (officer_cap_raise.go) — whose post raises TODAY's limit only: today's
// officer_cap_raises row is $20, tomorrow has none, and the page reads $8.00
// of $20 with no Raise.
func TestRaiseIsOfferedFrom80PercentAndRaisesTodayOnly(t *testing.T) {
	r := rdRig(t)
	t.Cleanup(func() {
		dbtest.OwnerExec(t, dbtest.DSN(t), `DELETE FROM officer_cap_raises WHERE owner_id = 1 AND officer IN ($1, $2)`, rdA, rdB)
	})
	rdSpend(t, rdA, 1, 7.9)
	bar := spendOf(t, r.page(t, 1, http.MethodGet, "/officers/"+rdA, nil).Body.String())
	if !strings.Contains(bar, `data-spend-level="under"`) || strings.Contains(bar, "data-cap-raise") {
		t.Fatalf("79%%: the bar is not plain, or offers Raise:\n%s", bar)
	}
	rdSpend(t, rdA, 2, 0.1)
	bar = spendOf(t, r.page(t, 1, http.MethodGet, "/officers/"+rdA, nil).Body.String())
	for _, want := range []string{`data-spend-level="near"`, `data-progress-tone="warn"`, `action="/officers/` + rdA + `/raise-cap"`, "Raise today&#39;s limit to $20"} {
		if !strings.Contains(bar, want) {
			t.Fatalf("80%%: the bar lacks %q:\n%s", want, bar)
		}
	}
	rec := r.page(t, 1, http.MethodPost, "/officers/"+rdA+"/raise-cap", url.Values{})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/officers/"+rdA+"?notice=cap-raised" {
		t.Fatalf("the raise: %d %q %.300s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	sys := authz.System(context.Background())
	now := r.s.officerNow()
	today, err := r.store.RaisedCapsOn(sys, []int64{1}, now)
	if err != nil || today[builtin.OfficerRef{Owner: 1, Officer: rdA}] != 20 {
		t.Fatalf("today's raise: %v (%v); want $20", today, err)
	}
	tomorrow, err := r.store.RaisedCapsOn(sys, []int64{1}, now.Add(24*time.Hour))
	if err != nil || len(tomorrow) != 0 {
		t.Fatalf("tomorrow's raises: %v (%v); want none (today only)", tomorrow, err)
	}
	if _, ok := today[builtin.OfficerRef{Owner: 1, Officer: rdB}]; ok {
		t.Fatal("the raise reached the other officer on the type")
	}
	bar = spendOf(t, r.page(t, 1, http.MethodGet, "/officers/"+rdA, nil).Body.String())
	if !strings.Contains(bar, `data-run-spend="8.00/20.00"`) || !strings.Contains(bar, `data-spend-level="under"`) || strings.Contains(bar, "data-cap-raise") {
		t.Fatalf("after the raise the bar is not $8.00 of $20, plain, without Raise:\n%s", bar)
	}
}

// TestTheMachineUserWarningIsOnlyInTheWizard (PR A (6)): the wizard draws the
// host warning; the page of a host officer it creates does not.
func TestTheMachineUserWarningIsOnlyInTheWizard(t *testing.T) {
	good := spawnableHost(hostOffHost, true)
	good.AgentClose = true
	r := newPageRig(t, declRigOpts{hosts: []mirror.HostView{good}})
	wizard := r.page(t, 1, http.MethodGet, "/officers/new", nil).Body.String()
	if !strings.Contains(wizard, "data-host-risk") || !strings.Contains(wizard, html.EscapeString(hostRiskLine)) {
		t.Fatalf("the wizard lost its host warning")
	}
	form := wizardForm()
	form.Set("runtime", "host")
	form.Set("host", hostOffHost)
	if rec := r.page(t, 1, http.MethodPost, "/officers/new", form); rec.Code != http.StatusSeeOther {
		t.Fatalf("create a host officer: %d %.600s", rec.Code, rec.Body.String())
	}
	if row, err := r.store.GetDeclared(authz.System(context.Background()), 1, declID); err != nil || row.Fields.Runtime != "host" {
		t.Fatalf("control: the officer is not a host officer: %+v %v", row.Fields, err)
	}
	page := r.page(t, 1, http.MethodGet, "/officers/"+declID, nil).Body.String()
	if strings.Contains(page, "data-host-risk") || strings.Contains(page, html.EscapeString(hostRiskLine)) {
		t.Fatalf("the host officer's page still draws the machine-user warning")
	}
}

// TestThePageHasNoShadowButton (PR A (7)): a Shadow officer's page draws no
// Shadow / Live button (no form posting to /mode), and Shadow stays a setting
// the route still takes: a post to /mode switches it, and the page still has
// no button.
func TestThePageHasNoShadowButton(t *testing.T) {
	r := newPageRig(t, declRigOpts{pods: true})
	if rec := r.page(t, 1, http.MethodPost, "/officers/new", wizardForm()); rec.Code != http.StatusSeeOther {
		t.Fatalf("seed: %d", rec.Code)
	}
	sys := authz.System(context.Background())
	for _, step := range []string{"shadow", "live"} {
		page := r.page(t, 1, http.MethodGet, "/officers/"+declID, nil).Body.String()
		if row, _ := r.store.GetDeclared(sys, 1, declID); row.Mode != step {
			t.Fatalf("control: mode %q, want %q", row.Mode, step)
		}
		if strings.Contains(page, `/mode"`) || strings.Contains(page, "Switch to Live") || strings.Contains(page, "Switch to Shadow") || !strings.Contains(page, `id="officer-edit"`) {
			t.Fatalf("%s: the page draws a Shadow / Live button (or lost Edit settings)", step)
		}
		if step == "shadow" {
			if rec := r.page(t, 1, http.MethodPost, "/officers/"+declID+"/mode", url.Values{"mode": {"live"}}); rec.Code != http.StatusSeeOther {
				t.Fatalf("the /mode route: %d", rec.Code)
			}
		}
	}
}
