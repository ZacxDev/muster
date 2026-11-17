package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
)

// The Officers pages (officers redesign O-C), through the real server, the
// real built-in store and the real wake loop's pass: a signed-in person's
// browser session, played by sessionUserOverride (as the isolation walk does).

// pageUserHeader names the user a test's browser session acts as.
const pageUserHeader = "X-Officers-Page-Test-User"

// sessions makes r's browser sessions act as the user pageUserHeader names
// (user 1 when unset).
func (r *declRig) sessions() {
	r.s.sessionUserOverride = func(req *http.Request) int64 {
		if v, err := strconv.ParseInt(req.Header.Get(pageUserHeader), 10, 64); err == nil && v > 0 {
			return v
		}
		return authz.OperatorUserID
	}
}

// page is a request through the real handler as uid's signed-in browser: a
// document navigation (GET) or a same-origin form post.
func (r *declRig) page(t *testing.T, uid int64, method, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	body := ""
	if form != nil {
		body = form.Encode()
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "muster.test"
	req.Header.Set("Cookie", sessionCookieFor(r.s))
	req.Header.Set("Origin", "http://muster.test")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set(pageUserHeader, strconv.FormatInt(uid, 10))
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	return rec
}

// wizardForm is a valid wizard submission for a blank officer named Scout
// (id scout:main), given work by hand only, in Shadow.
func wizardForm() url.Values {
	return url.Values{"template": {"blank"}, "name": {"Scout"}, "charter": {"Scan the queue; never post."}, "runtime": {"auto"},
		"wake": {"manual"}, "mode": {"shadow"}}
}

func newPageRig(t *testing.T, o declRigOpts) *declRig {
	t.Helper()
	r := newDeclRig(t, o)
	r.sessions()
	return r
}

// TestTheWizardCreatesAnOfficerTheNextWakeRuns: the wizard's POST lands a
// DB-declared officer in Shadow, with the schedule its wake step chose, in
// the LIVE registry with no restart — and the next wake pass starts its
// instance for an item. The list then shows it, and its page shows it.
// CONTROL: before the create the list draws the empty state and the registry
// has no such officer.
func TestTheWizardCreatesAnOfficerTheNextWakeRuns(t *testing.T) {
	r := newPageRig(t, declRigOpts{pods: true})
	list := r.page(t, 1, http.MethodGet, "/officers", nil)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `id="officers-empty"`) {
		t.Fatalf("control: the empty list: %d, empty state drawn %v", list.Code, strings.Contains(list.Body.String(), `id="officers-empty"`))
	}
	if _, ok := r.s.officerRT.reg.ForOwner(1, declID); ok {
		t.Fatal("control: scout:main is in the registry before any create")
	}
	form := wizardForm()
	form.Set("wake", "schedule")
	form.Set("schedule_preset", "daily")
	rec := r.page(t, 1, http.MethodPost, "/officers/new", form)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/officers/"+declID+"?notice=created" {
		t.Fatalf("the wizard's create: %d %q\n%.600s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	row, err := r.store.GetDeclared(authz.System(context.Background()), 1, declID)
	if err != nil || row.Mode != "shadow" || !row.Enabled || row.Fields.Runtime != officers.RuntimeAuto || row.TypeName != "Scout" {
		t.Fatalf("the stored officer: %+v, %v; want Scout, shadow, enabled, auto", row, err)
	}
	scheds, err := r.store.ListSchedules(authz.System(context.Background()), 1, declID)
	if err != nil || len(scheds) != 1 || scheds[0].Cron != "0 9 * * *" || !scheds[0].Enabled {
		t.Fatalf("its schedules: %+v, %v; want the wizard's daily one", scheds, err)
	}
	o := r.officerFor(t, 1, declID) // live, with no restart
	r.enqueue(t, 1, declID, "first")
	if res := r.pass(t, o); len(res.Decision.Start) != 1 {
		t.Fatalf("the next wake pass started %v; want instance 1", res.Decision.Start)
	}
	if _, ok := r.agentNamed(o.InstanceName(1)); !ok {
		t.Fatalf("the pass did not create instance %s", o.InstanceName(1))
	}
	list = r.page(t, 1, http.MethodGet, "/officers", nil)
	body := list.Body.String()
	if !strings.Contains(body, `data-officer="`+declID+`"`) || !strings.Contains(body, `data-officer-status="shadow"`) || strings.Contains(body, `id="officers-empty"`) {
		t.Errorf("the list after the create does not show scout:main in Shadow:\n%.1500s", body)
	}
	detail := r.page(t, 1, http.MethodGet, "/officers/"+declID, nil)
	db := detail.Body.String()
	for _, want := range []string{`id="officer-actions"`, `data-schedule="pass"`, `data-item-status="open"`, "Last pass", "Switch to Live"} {
		if !strings.Contains(db, want) {
			t.Errorf("the officer's page lacks %q", want)
		}
	}
}

// TestTheWizardRefusesAnOfficerNothingWakes (F5): a create that chose
// neither a schedule nor "only when I give it work" is refused on the wake
// step, and stores nothing. CONTROL: the same form with "manual" is created.
func TestTheWizardRefusesAnOfficerNothingWakes(t *testing.T) {
	r := newPageRig(t, declRigOpts{pods: true})
	form := wizardForm()
	form.Del("wake")
	rec := r.page(t, 1, http.MethodPost, "/officers/new", form)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `data-field-error="wake"`) || !strings.Contains(rec.Body.String(), `data-first-step="4"`) {
		t.Fatalf("a create with no wake: %d, wake refused %v; want 400 on the wake step\n%.600s", rec.Code, strings.Contains(rec.Body.String(), `data-field-error="wake"`), rec.Body.String())
	}
	if _, err := r.store.GetDeclared(authz.System(context.Background()), 1, declID); !errors.Is(err, builtin.ErrNotFound) {
		t.Fatalf("the refused create stored an officer: %v", err)
	}
	// The typed values come back, so nothing is retyped.
	if !strings.Contains(rec.Body.String(), "Scan the queue; never post.") {
		t.Error("the refused form lost its instructions")
	}
	form.Set("wake", "schedule")
	form.Set("schedule_preset", "custom")
	form.Set("cron", "every day please")
	if rec := r.page(t, 1, http.MethodPost, "/officers/new", form); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `data-field-error="cron"`) {
		t.Fatalf("a create with an unreadable cron: %d; want 400 on cron", rec.Code)
	}
	form.Set("wake", "manual")
	if rec := r.page(t, 1, http.MethodPost, "/officers/new", form); rec.Code != http.StatusSeeOther {
		t.Fatalf("control: the same form with manual: %d %.400s", rec.Code, rec.Body.String())
	}
}

// TestTheWizardShowsTheCoresRefusalsOnTheirFields: a declaration the core
// refuses (O-A's one check) comes back on the form beside the field it names
// — a host that is none of the owner's machines on "host", an id that is not
// <type>:<area> on "id" — with nothing stored.
func TestTheWizardShowsTheCoresRefusalsOnTheirFields(t *testing.T) {
	good := spawnableHost(hostOffHost, true)
	good.AgentClose = true
	r := newPageRig(t, declRigOpts{hosts: []mirror.HostView{good}})
	form := wizardForm()
	form.Set("runtime", "host")
	form.Set("host", "host-nowhere")
	rec := r.page(t, 1, http.MethodPost, "/officers/new", form)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `data-field-error="host"`) || !strings.Contains(rec.Body.String(), "is not one of its owner") {
		t.Fatalf("a host the owner does not have: %d\n%.800s", rec.Code, rec.Body.String())
	}
	form = wizardForm()
	form.Set("id", "Not An Id")
	rec = r.page(t, 1, http.MethodPost, "/officers/new", form)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `data-field-error="id"`) {
		t.Fatalf("an id that is not <type>:<area>: %d\n%.800s", rec.Code, rec.Body.String())
	}
	if list, _ := r.store.ListDeclaredFor(authz.System(context.Background()), 1); len(list) != 0 {
		t.Fatalf("refused creates stored %d officers", len(list))
	}
	// CONTROL: the owner's own host is taken.
	form = wizardForm()
	form.Set("runtime", "host")
	form.Set("host", hostOffHost)
	if rec := r.page(t, 1, http.MethodPost, "/officers/new", form); rec.Code != http.StatusSeeOther {
		t.Fatalf("control: the owner's own host: %d %.600s", rec.Code, rec.Body.String())
	}
}

// TestAnotherOwnerCannotReadOrChangeAnOfficer: owner 1's officer is not
// another person's to read or change on any page route — each answers what
// an officer nobody has answers (same status, same words, nothing of the
// officer's), naming owner 1 in owner= included — and nothing changes.
// CONTROL: owner 1's own pause lands.
func TestAnotherOwnerCannotReadOrChangeAnOfficer(t *testing.T) {
	r := newPageRig(t, declRigOpts{pods: true})
	form := wizardForm()
	form.Set("charter", "owner one's secret charter")
	if rec := r.page(t, 1, http.MethodPost, "/officers/new", form); rec.Code != http.StatusSeeOther {
		t.Fatalf("seed: %d %.400s", rec.Code, rec.Body.String())
	}
	sys := authz.System(context.Background())
	routes := []struct {
		method, suffix string
		form           url.Values
	}{
		{http.MethodGet, "", nil},
		{http.MethodGet, "/edit", nil},
		{http.MethodPost, "/edit", wizardForm()},
		{http.MethodPost, "/pause", url.Values{}},
		{http.MethodPost, "/resume", url.Values{}},
		{http.MethodPost, "/mode", url.Values{"mode": {"live"}}},
		{http.MethodPost, "/run", url.Values{}},
		{http.MethodPost, "/delete", url.Values{"confirm": {declID}}},
		{http.MethodPost, "/schedules", url.Values{"schedule_preset": {"hourly"}, "name": {"x"}}},
		{http.MethodPost, "/schedules/pass/delete", url.Values{}},
	}
	for _, c := range routes {
		for _, q := range []string{"", "?owner=1"} {
			got := r.page(t, declOther, c.method, "/officers/"+declID+c.suffix+q, c.form)
			ghost := r.page(t, declOther, c.method, "/officers/ghost:none"+c.suffix+q, c.form)
			if got.Code != http.StatusNotFound || got.Code != ghost.Code || got.Body.String() != ghost.Body.String() {
				t.Errorf("another owner's %s %s%s: %d (a ghost %d), bodies equal %v; want the ghost's 404", c.method, c.suffix, q, got.Code, ghost.Code, got.Body.String() == ghost.Body.String())
			}
			if strings.Contains(got.Body.String(), "secret charter") || strings.Contains(got.Body.String(), "Scout") {
				t.Errorf("another owner's %s %s%s shows owner 1's officer", c.method, c.suffix, q)
			}
		}
	}
	if list := r.page(t, declOther, http.MethodGet, "/officers", nil); strings.Contains(list.Body.String(), declID) {
		t.Error("another owner's list shows owner 1's officer")
	}
	row, err := r.store.GetDeclared(sys, 1, declID)
	if err != nil || !row.Enabled || row.Mode != "shadow" || row.TypeName != "Scout" {
		t.Fatalf("after another owner's writes owner 1's officer is %+v, %v", row, err)
	}
	if items, _ := r.store.ListItems(sys, 1, declID, "", 10); len(items) != 0 {
		t.Fatalf("another owner's Run now queued %d items", len(items))
	}
	if rec := r.page(t, 1, http.MethodPost, "/officers/"+declID+"/pause", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("control: the owner's pause: %d", rec.Code)
	}
	if row, _ := r.store.GetDeclared(sys, 1, declID); row.Enabled {
		t.Fatal("control: the owner's pause did not land")
	}
}

// TestEnvOfficersAreReadOnlyOnThePages: an officer MUSTER_OFFICERS declares
// is listed read-only and its page draws no actions; every write the pages
// have is refused (409) and changes nothing — the wizard included, for its
// id. CONTROL: the list and page show it (so the refusals are about it).
func TestEnvOfficersAreReadOnlyOnThePages(t *testing.T) {
	r := newPageRig(t, declRigOpts{pods: true, env: `[{"id":"` + declID + `","backend":"builtin"}]`})
	sys := authz.System(context.Background())
	if err := r.store.PutType(sys, 1, builtin.Type{Key: "scout", Name: "Scout", Charter: "env charter"}); err != nil {
		t.Fatal(err)
	}
	if err := r.store.PutDef(sys, 1, builtin.Def{Officer: declID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	list := r.page(t, 1, http.MethodGet, "/officers", nil).Body.String()
	if !strings.Contains(list, `data-officer="`+declID+`" data-managed-by="env"`) || !strings.Contains(list, "data-read-only") {
		t.Fatalf("control: the list does not show the env officer read-only:\n%.1200s", list)
	}
	detail := r.page(t, 1, http.MethodGet, "/officers/"+declID, nil).Body.String()
	if !strings.Contains(detail, `id="officer-read-only"`) || strings.Contains(detail, `id="officer-actions"`) || strings.Contains(detail, `id="officer-delete"`) {
		t.Fatalf("the env officer's page: read-only note %v, actions %v, delete %v", strings.Contains(detail, `id="officer-read-only"`),
			strings.Contains(detail, `id="officer-actions"`), strings.Contains(detail, `id="officer-delete"`))
	}
	for _, c := range []struct {
		suffix string
		form   url.Values
	}{{"/edit", wizardForm()}, {"/pause", url.Values{}}, {"/mode", url.Values{"mode": {"live"}}}, {"/run", url.Values{}},
		{"/delete", url.Values{"confirm": {declID}}}, {"/schedules", url.Values{"schedule_preset": {"hourly"}}}} {
		if rec := r.page(t, 1, http.MethodPost, "/officers/"+declID+c.suffix, c.form); rec.Code != http.StatusConflict {
			t.Errorf("POST %s on an env officer: %d; want 409", c.suffix, rec.Code)
		}
	}
	if rec := r.page(t, 1, http.MethodPost, "/officers/new", wizardForm()); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "read-only here") {
		t.Errorf("the wizard creating the env officer's id: %d; want 409 read-only", rec.Code)
	}
	d, err := r.store.GetDef(sys, 1, declID)
	if err != nil || !d.Enabled || d.Mode != "live" {
		t.Fatalf("after the refused writes the env officer's def is %+v, %v", d, err)
	}
	if items, _ := r.store.ListItems(sys, 1, declID, "", 10); len(items) != 0 {
		t.Fatalf("a refused Run now queued %d items", len(items))
	}
}

// TestATeamsOfficerIsChangedOnlyByItsManager: a manager creates a team
// officer in the wizard (owned by the team); a viewer of the team sees it in
// the team's section and on its page, with no actions, and every write of
// theirs is refused (403) and changes nothing. CONTROL: the manager's pause
// lands.
func TestATeamsOfficerIsChangedOnlyByItsManager(t *testing.T) {
	r := newPageRig(t, declRigOpts{pods: true})
	dsn := dbtest.DSN(t)
	dbtest.OwnerExec(t, dsn, `INSERT INTO team_members (team_id, user_id, role, added_by) VALUES (987302, 1, 'manager', 1), (987302, 987301, 'viewer', 1) ON CONFLICT DO NOTHING`)
	t.Cleanup(func() {
		// The manager's writes are audited against the team (auditTeam): those
		// rows go before the rig removes the team.
		dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE team_id = 987302`)
		dbtest.OwnerExec(t, dsn, `DELETE FROM team_members WHERE team_id = 987302`)
	})
	team := strconv.FormatInt(declTeam, 10)
	form := wizardForm()
	form.Set("owner", team)
	rec := r.page(t, 1, http.MethodPost, "/officers/new", form)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/officers/"+declID+"?owner="+team+"&notice=created" {
		t.Fatalf("the manager's create: %d %q\n%.800s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	sys := authz.System(context.Background())
	if _, err := r.store.GetDeclared(sys, declTeam, declID); err != nil {
		t.Fatalf("the team does not own the created officer: %v", err)
	}
	if _, err := r.store.GetDeclared(sys, 1, declID); !errors.Is(err, builtin.ErrNotFound) {
		t.Fatalf("the manager personally owns a copy: %v", err)
	}
	list := r.page(t, declOther, http.MethodGet, "/officers", nil).Body.String()
	if !strings.Contains(list, `data-officers-team="`+team+`"`) || !strings.Contains(list, `data-officer="`+declID+`"`) {
		t.Fatalf("the viewer's list does not show the team's officer:\n%.1500s", list)
	}
	detail := r.page(t, declOther, http.MethodGet, "/officers/"+declID+"?owner="+team, nil)
	if detail.Code != http.StatusOK || strings.Contains(detail.Body.String(), `id="officer-actions"`) || !strings.Contains(detail.Body.String(), TeamOfficerManagerOnly) {
		t.Fatalf("the viewer's page: %d, actions drawn %v", detail.Code, strings.Contains(detail.Body.String(), `id="officer-actions"`))
	}
	for _, c := range []struct {
		suffix string
		form   url.Values
	}{{"/pause", url.Values{}}, {"/mode", url.Values{"mode": {"live"}}}, {"/run", url.Values{}}, {"/edit", wizardForm()},
		{"/delete", url.Values{"confirm": {declID}}}, {"/schedules", url.Values{"schedule_preset": {"hourly"}}}} {
		c.form.Set("owner", team)
		if rec := r.page(t, declOther, http.MethodPost, "/officers/"+declID+c.suffix, c.form); rec.Code != http.StatusForbidden {
			t.Errorf("the viewer's POST %s: %d; want 403", c.suffix, rec.Code)
		}
	}
	vf := wizardForm()
	vf.Set("owner", team)
	vf.Set("name", "Viewer's")
	if rec := r.page(t, declOther, http.MethodPost, "/officers/new", vf); rec.Code != http.StatusForbidden {
		t.Errorf("the viewer's create of a team officer: %d; want 403", rec.Code)
	}
	row, err := r.store.GetDeclared(sys, declTeam, declID)
	if err != nil || !row.Enabled || row.Mode != "shadow" {
		t.Fatalf("after the viewer's writes the team officer is %+v, %v", row, err)
	}
	if rec := r.page(t, 1, http.MethodPost, "/officers/"+declID+"/pause", url.Values{"owner": {team}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("control: the manager's pause: %d %.400s", rec.Code, rec.Body.String())
	}
	if row, _ := r.store.GetDeclared(sys, declTeam, declID); row.Enabled {
		t.Fatal("control: the manager's pause did not land")
	}
}

// TestTheWizardSaysWhereAnOfficerCanRunAndWhyNot: the "where it runs" step
// and the list's panel draw the server's own answer — with no agent plane the
// pod choice is disabled naming why, a host that cannot close windows is
// "not ready" naming why, and auto names that nothing qualifies; with a
// provisioner the pod choice is enabled and auto picks a pod.
func TestTheWizardSaysWhereAnOfficerCanRunAndWhyNot(t *testing.T) {
	f := newConnFixture(t)
	noClose := spawnableHost(hostOffOther, true)
	for _, c := range []struct {
		name    string
		opts    declRigOpts
		podOff  bool
		want    []string
		notWant []string
	}{
		{"no agent plane, a host that cannot close", declRigOpts{hosts: []mirror.HostView{noClose}}, true,
			[]string{"no agent provisioner that dispatches agents", hostOffOther + " — not ready: it does not close windows", "Right now: nowhere yet"}, []string{"Right now: in a pod"}},
		{"a provisioner", declRigOpts{pods: true}, false,
			[]string{"Right now: in a pod", "no mirror hub"}, []string{"no agent provisioner"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newDeclRigOn(t, f, c.opts)
			r.sessions()
			body := r.page(t, 1, http.MethodGet, "/officers/new", nil).Body.String()
			podOff := strings.Contains(body, `name="runtime" value="pod" disabled`)
			if podOff != c.podOff {
				t.Errorf("the pod choice disabled = %v; want %v", podOff, c.podOff)
			}
			for _, w := range c.want {
				if !strings.Contains(body, w) {
					t.Errorf("the wizard does not say %q", w)
				}
			}
			for _, w := range c.notWant {
				if strings.Contains(body, w) {
					t.Errorf("the wizard says %q", w)
				}
			}
			list := r.page(t, 1, http.MethodGet, "/officers", nil).Body.String()
			if !strings.Contains(list, `id="officers-runtimes"`) || !strings.Contains(list, `data-runtime="pod" data-available="`+strconv.FormatBool(!c.podOff)+`"`) {
				t.Errorf("the list's runtime panel does not say pods available = %v", !c.podOff)
			}
		})
	}
}

// TestTheModeSwitchChangesOnlyTheMode: Switch to Live on a paused Shadow
// officer leaves it paused, its instructions, runtime and escalations as they
// were; Run now queues one item and a second tap queues none. A replace
// whose form left paused out would resume it (O-A's F6): this is the page
// door stating it.
func TestTheModeSwitchChangesOnlyTheMode(t *testing.T) {
	r := newPageRig(t, declRigOpts{pods: true})
	if rec := r.page(t, 1, http.MethodPost, "/officers/new", wizardForm()); rec.Code != http.StatusSeeOther {
		t.Fatalf("seed: %d", rec.Code)
	}
	if rec := r.page(t, 1, http.MethodPost, "/officers/"+declID+"/pause", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("pause: %d", rec.Code)
	}
	if rec := r.page(t, 1, http.MethodPost, "/officers/"+declID+"/mode", url.Values{"mode": {"live"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("switch to live: %d %.400s", rec.Code, rec.Body.String())
	}
	sys := authz.System(context.Background())
	row, err := r.store.GetDeclared(sys, 1, declID)
	if err != nil || row.Mode != "live" || row.Enabled || row.Fields.Runtime != officers.RuntimeAuto {
		t.Fatalf("after Switch to Live: %+v, %v; want live and still paused", row, err)
	}
	ty, err := r.store.GetType(sys, 1, row.TypeKey)
	if err != nil || ty.Charter != "Scan the queue; never post." || ty.Name != "Scout" {
		t.Fatalf("after Switch to Live its type is %+v, %v", ty, err)
	}
	for i, want := range []string{"notice=queued", "notice=already-queued"} {
		rec := r.page(t, 1, http.MethodPost, "/officers/"+declID+"/run", url.Values{})
		if rec.Code != http.StatusSeeOther || !strings.HasSuffix(rec.Header().Get("Location"), want) {
			t.Fatalf("Run now %d: %d %q; want %s", i+1, rec.Code, rec.Header().Get("Location"), want)
		}
	}
	if items, _ := r.store.ListItems(sys, 1, declID, "", 10); len(items) != 1 {
		t.Fatalf("two taps of Run now queued %d items; want 1", len(items))
	}
}

// TestTheDeleteNeedsItsIDTyped: a delete whose confirmation is not the
// officer's id deletes nothing; with it, the officer and its schedules go.
func TestTheDeleteNeedsItsIDTyped(t *testing.T) {
	r := newPageRig(t, declRigOpts{pods: true})
	form := wizardForm()
	form.Set("wake", "schedule")
	form.Set("schedule_preset", "hourly")
	if rec := r.page(t, 1, http.MethodPost, "/officers/new", form); rec.Code != http.StatusSeeOther {
		t.Fatalf("seed: %d", rec.Code)
	}
	sys := authz.System(context.Background())
	if rec := r.page(t, 1, http.MethodPost, "/officers/"+declID+"/delete", url.Values{"confirm": {"scout"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("a delete with the wrong confirmation: %d; want 400", rec.Code)
	}
	if _, err := r.store.GetDeclared(sys, 1, declID); err != nil {
		t.Fatalf("the unconfirmed delete removed it: %v", err)
	}
	if rec := r.page(t, 1, http.MethodPost, "/officers/"+declID+"/delete", url.Values{"confirm": {declID}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("the confirmed delete: %d", rec.Code)
	}
	if _, err := r.store.GetDeclared(sys, 1, declID); !errors.Is(err, builtin.ErrNotFound) {
		t.Fatalf("after the delete: %v", err)
	}
	if _, ok := r.s.officerRT.reg.ForOwner(1, declID); ok {
		t.Fatal("the deleted officer is still in the live registry")
	}
}
