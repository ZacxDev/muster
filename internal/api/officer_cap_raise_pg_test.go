package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/officers/builtin"
)

// "RAISE CAP FOR TODAY" (officer_cap_raise.go), through the real server, the
// real built-in store and the real wake loop's pass, on the runtime's clock
// seam (declRigOpts.now): the owner raises a held officer's daily budget, the
// next pass wakes it for the work the budget held back, the raise is in
// audit_log, nobody else can raise it, and on the next UTC day the declared
// budget is back with nothing cleaned up.

const (
	capOfficer     = "scout:capped"
	capTeamOfficer = "scout:teamcap"
	// The literal amounts (never derived from the code): a $10 budget, $12
	// spent, so held; one raise is +$10, to $20.
	capButtonWords = "Raise today&#39;s limit to $20"
)

const capEnv = `[{"id":"` + capOfficer + `","backend":"builtin","daily_budget_usd":10},
 {"id":"` + capTeamOfficer + `","backend":"builtin","owner":987302,"daily_budget_usd":10}]`

// capRig is a page rig whose clock the test moves, with two budgeted builtin
// env officers — user 1's and team declTeam's (user 1 its manager, declOther
// its viewer) — each $12 into its $10 budget on the clock's day.
type capRig struct {
	*declRig
	now *time.Time
}

func newCapRig(t *testing.T) capRig {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Hour)
	if now.Hour() == 23 {
		now = now.Add(-time.Hour) // the "next day" step must not be this day
	}
	clock := &now
	r := newPageRig(t, declRigOpts{pods: true, env: capEnv, now: func() time.Time { return *clock }})
	dsn := dbtest.DSN(t)
	dbtest.OwnerExec(t, dsn, `INSERT INTO team_members (team_id, user_id, role, added_by) VALUES (987302, 1, 'manager', 1), (987302, 987301, 'viewer', 1) ON CONFLICT DO NOTHING`)
	t.Cleanup(func() {
		dbtest.OwnerExec(t, dsn, `DELETE FROM officer_cap_raises WHERE officer IN ('`+capOfficer+`', '`+capTeamOfficer+`')`)
		dbtest.OwnerExec(t, dsn, `DELETE FROM officer_llm_usage WHERE officer IN ('`+capOfficer+`', '`+capTeamOfficer+`')`)
		dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE action = '`+builtin.CapRaiseAuditAction+`' OR team_id = 987302`)
		dbtest.OwnerExec(t, dsn, `DELETE FROM team_members WHERE team_id = 987302`)
	})
	sys := authz.System(context.Background())
	for _, owner := range []int64{1, declTeam} {
		if err := r.store.PutType(sys, owner, builtin.Type{Key: "scout", Name: "Scout", Charter: "Scan the queue; never post."}); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []struct {
		owner int64
		id    string
	}{{1, capOfficer}, {declTeam, capTeamOfficer}} {
		if err := r.store.PutDef(sys, d.owner, builtin.Def{Officer: d.id, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	c := capRig{declRig: r, now: clock}
	c.spend(t, 1, capOfficer, 12)
	c.spend(t, declTeam, capTeamOfficer, 12)
	return c
}

// spend books usd as owner's officer's spend on the clock's UTC day.
func (c capRig) spend(t *testing.T, owner int64, id string, usd float64) {
	t.Helper()
	dbtest.OwnerExec(t, dbtest.DSN(t), `INSERT INTO officer_llm_usage (owner_id, officer, pool, instance, day, requests, input_tokens, output_tokens,
		cache_read_tokens, cache_write_tokens, cost_usd, estimated_requests) VALUES ($1, $2, 'judgement', 1, $3::date, 1, 0, 0, 0, 0, $4, 1)`,
		owner, id, c.now.Format("2006-01-02"), usd)
}

// held runs one wake pass of owner's officer and answers how many items its
// budget held back and how many were claimable.
func (c capRig) held(t *testing.T, owner int64, id string) (budgetWaiting, claimable int) {
	t.Helper()
	d := c.pass(t, c.officerFor(t, owner, id)).Decision
	return d.BudgetWaiting, d.Claimable
}

// raises is the officer_cap_raises rows and the officer.cap_raise audit rows.
func (c capRig) raises(t *testing.T) (rows, audits int) {
	t.Helper()
	sys := authz.System(context.Background())
	if err := c.f.pool.QueryRow(sys, `SELECT count(*) FROM officer_cap_raises`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := c.f.pool.QueryRow(sys, `SELECT count(*) FROM audit_log WHERE action = $1`, builtin.CapRaiseAuditAction).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	return rows, audits
}

// TestTheOwnerRaisesTodaysCapAndTheHeldOfficerResumes: user 1's officer, $12
// into its $10 budget, holds its (non-owner) item back; its page says held and
// offers "Raise today's limit to $20". The owner's raise writes the row
// and ONE audit row (who, which officer, $10 -> $20, until the next 00:00
// UTC); the next pass wakes it for the item, and the page reads $12 of $20.
// On the next UTC day (the clock seam moved past midnight, no cleanup run)
// the budget is $10 again: $12 that day holds it, and there is no raise.
func TestTheOwnerRaisesTodaysCapAndTheHeldOfficerResumes(t *testing.T) {
	c := newCapRig(t)
	c.enqueue(t, 1, capOfficer, "held-work")
	if w, n := c.held(t, 1, capOfficer); w != 1 || n != 0 {
		t.Fatalf("control: $12 of $10: %d held back, %d claimable; want 1 and 0", w, n)
	}
	page := c.page(t, 1, http.MethodGet, "/officers/"+capOfficer, nil).Body.String()
	if !strings.Contains(page, "data-run-held") || !strings.Contains(page, capButtonWords) || !strings.Contains(page, `data-run-spend="12.00/10.00"`) {
		t.Fatalf("the held officer's page: held %v, button %v, spend %v", strings.Contains(page, "data-run-held"),
			strings.Contains(page, capButtonWords), strings.Contains(page, `data-run-spend="12.00/10.00"`))
	}

	rec := c.page(t, 1, http.MethodPost, "/officers/"+capOfficer+"/raise-cap", url.Values{})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/officers/"+capOfficer+"?notice=cap-raised" {
		t.Fatalf("the owner's raise: %d %q %.400s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	midnight := c.now.Truncate(24 * time.Hour).Add(24 * time.Hour)
	var uid int64
	var via, object string
	var raw []byte
	if err := c.f.pool.QueryRow(authz.System(context.Background()), `SELECT user_id, via, object, detail FROM audit_log WHERE action = $1`,
		builtin.CapRaiseAuditAction).Scan(&uid, &via, &object, &raw); err != nil {
		t.Fatalf("the raise's audit row: %v", err)
	}
	var detail map[string]any
	_ = json.Unmarshal(raw, &detail)
	if uid != 1 || via != "session" || object != "officer_def:"+capOfficer || detail["officer"] != capOfficer ||
		detail["old_daily_usd"] != 10.0 || detail["new_daily_usd"] != 20.0 || detail["until"] != midnight.Format(time.RFC3339) {
		t.Fatalf("the audit row: user %d via %q object %q detail %s; want user 1, session, $10 -> $20 until %s", uid, via, object, raw, midnight.Format(time.RFC3339))
	}
	if w, n := c.held(t, 1, capOfficer); w != 0 || n != 1 {
		t.Fatalf("after the raise: %d held back, %d claimable; want 0 and 1 (it resumes)", w, n)
	}
	page = c.page(t, 1, http.MethodGet, "/officers/"+capOfficer, nil).Body.String()
	if strings.Contains(page, "data-run-held") || !strings.Contains(page, `data-run-spend="12.00/20.00"`) || strings.Contains(page, "data-cap-raise-submit") {
		t.Fatalf("the page after the raise still reads held, or not $12 of $20:\n%.1500s", section(page, "officer-run", "officer-timeline"))
	}

	// The next UTC day: nothing ran, the raise is simply not today's.
	*c.now = midnight.Add(time.Hour)
	c.spend(t, 1, capOfficer, 12)
	if w, n := c.held(t, 1, capOfficer); w != 1 || n != 0 {
		t.Fatalf("the next UTC day at $12: %d held back, %d claimable; want 1 and 0 (the $10 budget is back)", w, n)
	}
	page = c.page(t, 1, http.MethodGet, "/officers/"+capOfficer, nil).Body.String()
	if !strings.Contains(page, `data-run-spend="12.00/10.00"`) || !strings.Contains(page, capButtonWords) {
		t.Fatalf("the next day's page does not read $12 of $10 with the button back")
	}
}

// TestOnlyTheOwnerRaisesTheCap: every caller but the officer's own owner is
// refused, and nothing is written — no raise row, no audit row:
//   - ANOTHER user (declOther) naming user 1's officer: it is not theirs to see
//     (404);
//   - a team's officer, by its team's MANAGER (user 1) — a manager is not the
//     officer's owner, so the button is not drawn either — and by a viewer;
//   - the shared hook token and the service token: refused before the
//     handler (requireOperatorWrite; an officer instance's own token is
//     TestTheOfficerCannotDecideItsOwnWork's, which posts here too);
//   - user 1 through a door that is not their browser — a personal API token
//     and an agent token, as the handler meets them (the Via guard);
//
// then the owner's own raise is the control that writes both.
func TestOnlyTheOwnerRaisesTheCap(t *testing.T) {
	c := newCapRig(t)
	own := "/officers/" + capOfficer + "/raise-cap"
	team := "/officers/" + capTeamOfficer + "/raise-cap"
	teamQ := url.Values{"owner": {"987302"}}
	if rec := c.page(t, declOther, http.MethodPost, own, url.Values{"owner": {"1"}}); rec.Code != http.StatusNotFound {
		t.Errorf("another user's raise of user 1's officer: %d, want 404", rec.Code)
	}
	teamPage := c.page(t, 1, http.MethodGet, "/officers/"+capTeamOfficer+"?owner=987302", nil).Body.String()
	if !strings.Contains(teamPage, "data-run-held") || strings.Contains(teamPage, "data-cap-raise") {
		t.Errorf("the team officer's page for its manager: held %v (want true), raise drawn %v (want false)",
			strings.Contains(teamPage, "data-run-held"), strings.Contains(teamPage, "data-cap-raise"))
	}
	for _, uid := range []int64{1, declOther} {
		rec := c.page(t, uid, http.MethodPost, team, teamQ)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "Only the officer&#39;s owner can raise its budget") {
			t.Errorf("team member %d's raise of the team's officer: %d, want 403 saying only its owner", uid, rec.Code)
		}
	}
	for name, cred := range map[string]string{"the shared hook token": connTestHookToken, "the service token": connTestServiceToken} {
		req := capPost(own)
		req.Header.Set("Authorization", "Bearer "+cred)
		rec := httptest.NewRecorder()
		c.h.ServeHTTP(rec, req)
		if rec.Code < 400 {
			t.Errorf("%s: %d, want a refusal", name, rec.Code)
		}
	}
	for _, via := range []authz.Via{authz.ViaToken, authz.ViaAgent, authz.ViaHook} {
		req := capPost(own)
		req.SetPathValue("officer", capOfficer)
		req = req.WithContext(authz.WithPrincipal(req.Context(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: via}))
		rec := httptest.NewRecorder()
		c.s.handleOfficerRaiseCap(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("user 1 through %s, at the handler: %d, want 403", via, rec.Code)
		}
	}
	if rows, audits := c.raises(t); rows != 0 || audits != 0 {
		t.Fatalf("a refused raise wrote %d raise row(s) and %d audit row(s)", rows, audits)
	}
	if rec := c.page(t, 1, http.MethodPost, own, url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("control: the owner's raise: %d %.300s\nlogs:\n%s", rec.Code, rec.Body.String(), c.logs)
	}
	if rows, audits := c.raises(t); rows != 1 || audits != 1 {
		t.Fatalf("control: the owner's raise wrote %d raise row(s) and %d audit row(s), want 1 and 1", rows, audits)
	}
	if got, _ := c.s.officerBudgetOf(authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaSession}), 1, capOfficer, *c.now); got != 20 {
		t.Fatalf("control: the owner's budget after the raise is $%v, want $20", got)
	}
}

// TestNoPrincipalWhoseIDIsTheTeamsRaisesItsOfficer pins why `!sc.Team` is
// not the only thing between a "user whose id equals a team's id" and that
// team's officer. A team IS a users row (0126: users.kind 'team', the same
// users.id primary key a person has), so:
//   - no PERSON can hold a team's id: inserting one is refused by the key;
//   - the only principal with that id is the TEAM ROW itself, and a team never
//     signs in: its browser session is refused at admit (TeamSignInRefusal),
//     before the handler — where, with owner = its own id, scopeOf would read
//     the team officer as the caller's OWN (sc.Team false), so admit is the
//     guard that holds here, not `!sc.Team`.
//
// Nothing is written. CONTROL: user 1 raises their own officer.
func TestNoPrincipalWhoseIDIsTheTeamsRaisesItsOfficer(t *testing.T) {
	c := newCapRig(t)
	if _, err := c.f.pool.Exec(authz.System(context.Background()),
		`INSERT INTO users (id, kind, role, display_name) OVERRIDING SYSTEM VALUE VALUES (987302, 'person', 'member', 'same id as the team')`); err == nil ||
		!strings.Contains(err.Error(), "23505") {
		t.Fatalf("a person with the team's id: %v, want a unique violation (23505): one id space", err)
	}
	rec := c.page(t, declTeam, http.MethodPost, "/officers/"+capTeamOfficer+"/raise-cap", url.Values{"owner": {"987302"}})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "team") {
		t.Fatalf("the team row's own session raising its officer: %d %.300s; want 403 (a team never signs in)", rec.Code, rec.Body.String())
	}
	if rows, audits := c.raises(t); rows != 0 || audits != 0 {
		t.Fatalf("the team's refused raise wrote %d raise row(s) and %d audit row(s)", rows, audits)
	}
	if rec := c.page(t, 1, http.MethodPost, "/officers/"+capOfficer+"/raise-cap", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("control: user 1's raise of their own officer: %d %.300s", rec.Code, rec.Body.String())
	}
	if rows, _ := c.raises(t); rows != 1 {
		t.Fatalf("control: user 1's raise wrote %d row(s), want 1", rows)
	}
}

// capPost is a same-origin form POST to path with no credential of its own.
func capPost(path string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(""))
	req.Host = "muster.test"
	req.Header.Set("Origin", "http://muster.test")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}
