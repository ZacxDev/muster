package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
)

// The Officers pages, round 1 of the O-C audit: who an escalation may reach
// (Q1), the one-choice step and the per-process pass (D1, D2), and the 🟡/🟢
// fixes — each through the real server, store and handler.

// The people of the reach tests. Owner 1 manages declTeam (whose viewer is
// declOther) and reachTeam2 (whose member is reachTeam2Member); reachOutsider
// is a person on no team with owner 1; reachNobody is no user at all.
const (
	reachOutsider    = int64(987303)
	reachTeam2       = int64(987304)
	reachTeam2Member = int64(987305)
	reachNobody      = int64(987399)
)

// reachOutWords is the one refusal of an out-of-reach escalation: pinned as a
// literal (the contract), not read from the constant it tests.
const reachOutWords = "escalate_to may name only people who can already see this officer"

// seedReach makes the teams and people above.
func seedReach(t *testing.T) {
	t.Helper()
	dsn := dbtest.DSN(t)
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, kind, role, display_name) OVERRIDING SYSTEM VALUE VALUES
		(987303, 'person', 'member', 'reach-outsider'), (987304, 'team', 'member', 'reach-team-two'), (987305, 'person', 'member', 'reach-team-two-member')
		ON CONFLICT DO NOTHING`)
	dbtest.OwnerExec(t, dsn, `INSERT INTO team_members (team_id, user_id, role, added_by) VALUES
		(987302, 1, 'manager', 1), (987302, 987301, 'viewer', 1), (987304, 1, 'manager', 1), (987304, 987305, 'operator', 1) ON CONFLICT DO NOTHING`)
	t.Cleanup(func() {
		dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE team_id IN (987302, 987304)`)
		dbtest.OwnerExec(t, dsn, `DELETE FROM team_members WHERE team_id IN (987302, 987304)`)
		dbtest.OwnerExec(t, dsn, `DELETE FROM users WHERE id IN (987303, 987304, 987305)`)
	})
}

// browserForm is what a browser submits for the form with id formID in body:
// every enabled named field — a radio or checkbox only when checked, a
// select's selected option (else its first), a textarea's text — and no
// submit button.
func browserForm(t *testing.T, body, formID string) url.Values {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	attr := func(n *html.Node, k string) (string, bool) {
		for _, a := range n.Attr {
			if a.Key == k {
				return a.Val, true
			}
		}
		return "", false
	}
	var form *html.Node
	var find func(*html.Node)
	find = func(n *html.Node) {
		if id, _ := attr(n, "id"); n.Type == html.ElementNode && n.Data == "form" && id == formID {
			form = n
		}
		for c := n.FirstChild; c != nil && form == nil; c = c.NextSibling {
			find(c)
		}
	}
	find(doc)
	if form == nil {
		t.Fatalf("no form #%s in the page", formID)
	}
	out := url.Values{}
	text := func(n *html.Node) string {
		var b strings.Builder
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.TextNode {
				b.WriteString(c.Data)
			}
		}
		return b.String()
	}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			name, named := attr(n, "name")
			_, disabled := attr(n, "disabled")
			switch {
			case !named || disabled:
			case n.Data == "input":
				typ, _ := attr(n, "type")
				_, checked := attr(n, "checked")
				v, _ := attr(n, "value")
				switch typ {
				case "submit", "button":
				case "radio", "checkbox":
					if checked {
						out.Add(name, v)
					}
				default:
					out.Add(name, v)
				}
			case n.Data == "textarea":
				out.Add(name, text(n))
			case n.Data == "select":
				first, picked := "", ""
				seen := false
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					if c.Type != html.ElementNode || c.Data != "option" {
						continue
					}
					v, _ := attr(c, "value")
					if !seen {
						first, seen = v, true
					}
					if _, sel := attr(c, "selected"); sel && picked == "" {
						picked = v
					}
				}
				if picked == "" {
					picked = first
				}
				if seen {
					out.Add(name, picked)
				}
			}
			if n.Data == "select" || n.Data == "textarea" {
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(form)
	return out
}

// TestEscalationsReachOnlyPeopleWhoSeeTheOfficer (Q1, operator decision): on
// EVERY door that writes escalate_to — the wizard, Edit, the JSON API's
// create and replace, and the team officers page — an id outside the officer's
// reach is refused, and an id naming nobody gets the SAME refusal, byte for
// byte once the id itself is set aside (no user-id enumeration). Personal
// officer: owner 1 and the people on owner 1's teams; team officer: the team
// and its members — not the members of ANOTHER team its manager manages.
// CONTROL on each door: an in-reach id is accepted.
func TestEscalationsReachOnlyPeopleWhoSeeTheOfficer(t *testing.T) {
	r := newPageRig(t, declRigOpts{pods: true})
	seedReach(t)
	sys := authz.System(context.Background())
	team := strconv.FormatInt(declTeam, 10)
	norm := func(body string, id int64) string { return strings.ReplaceAll(body, strconv.FormatInt(id, 10), "<id>") }
	// door is one write naming escalation id esc, answering the status and the
	// body.
	type door struct {
		name   string
		write  func(esc int64) (int, string)
		ok     int   // the control's status
		in     int64 // an in-reach id
		out    int64 // an out-of-reach person
		reset  func()
		stored func() []int64
	}
	escOf := func(owner int64, id string) func() []int64 {
		return func() []int64 {
			d, err := r.store.GetDef(sys, owner, id)
			if err != nil {
				t.Fatalf("read %d's %s: %v", owner, id, err)
			}
			return d.EscalateTo
		}
	}
	deleteDecl := func(owner int64) func() {
		return func() {
			_, _ = r.store.DeleteDeclared(sys, owner, declID)
			r.s.reloadAfterWrite(sys)
		}
	}
	jsonBody := func(esc int64) string {
		return `{"id":"` + declID + `","name":"Scout","charter":"c","runtime":"pod","mode":"shadow","paused":false,"escalate_to":[` + strconv.FormatInt(esc, 10) + `]}`
	}
	if err := r.store.PutType(sys, declTeam, builtin.Type{Key: "mk", Name: "Marketing", Charter: "c"}); err != nil {
		t.Fatal(err)
	}
	doors := []door{
		{name: "the wizard, a personal officer", ok: http.StatusSeeOther, in: declOther, out: reachOutsider, reset: deleteDecl(1), stored: escOf(1, declID),
			write: func(esc int64) (int, string) {
				f := wizardForm()
				f.Set("escalate_to", strconv.FormatInt(esc, 10))
				rec := r.page(t, 1, http.MethodPost, "/officers/new", f)
				return rec.Code, rec.Body.String()
			}},
		{name: "the wizard, a team officer", ok: http.StatusSeeOther, in: declOther, out: reachTeam2Member, reset: deleteDecl(declTeam), stored: escOf(declTeam, declID),
			write: func(esc int64) (int, string) {
				f := wizardForm()
				f.Set("owner", team)
				f.Set("escalate_to", strconv.FormatInt(esc, 10))
				rec := r.page(t, 1, http.MethodPost, "/officers/new", f)
				return rec.Code, rec.Body.String()
			}},
		{name: "the JSON API's create", ok: http.StatusCreated, in: reachTeam2Member, out: reachOutsider, reset: deleteDecl(1), stored: escOf(1, declID),
			write: func(esc int64) (int, string) {
				rec := r.hook(t, http.MethodPost, "/api/officers/declared", jsonBody(esc))
				return rec.Code, rec.Body.String()
			}},
	}
	for _, d := range doors {
		t.Run(d.name, func(t *testing.T) {
			for _, bad := range []int64{d.out, reachNobody} {
				code, body := d.write(bad)
				if code != http.StatusBadRequest || !strings.Contains(html.UnescapeString(body), reachOutWords) {
					t.Errorf("escalate_to [%d]: %d; want 400 with the out-of-reach refusal\n%.600s", bad, code, body)
				}
				if _, err := r.store.GetDeclared(sys, 1, declID); err == nil {
					d.reset()
					t.Errorf("escalate_to [%d] was stored", bad)
				}
				if _, err := r.store.GetDeclared(sys, declTeam, declID); err == nil {
					d.reset()
					t.Errorf("escalate_to [%d] was stored for the team", bad)
				}
			}
			oc, ob := d.write(d.out)
			nc, nb := d.write(reachNobody)
			if oc != nc || norm(ob, d.out) != norm(nb, reachNobody) {
				t.Errorf("an out-of-reach person (%d) and an id naming nobody (%d) are told apart: %d vs %d, bodies equal %v", d.out, reachNobody, oc, nc, norm(ob, d.out) == norm(nb, reachNobody))
			}
			if code, body := d.write(d.in); code != d.ok {
				t.Fatalf("control: an in-reach id %d: %d; want %d\n%.600s", d.in, code, d.ok, body)
			}
			if got := d.stored(); len(got) != 1 || got[0] != d.in {
				t.Fatalf("control: stored escalate_to %v; want [%d]", got, d.in)
			}
			d.reset()
		})
	}

	// Edit and the JSON replace: an officer in reach, then each change.
	if rec := r.page(t, 1, http.MethodPost, "/officers/new", wizardForm()); rec.Code != http.StatusSeeOther {
		t.Fatalf("seed: %d %.400s", rec.Code, rec.Body.String())
	}
	edit := func(esc int64) (int, string) {
		rec := r.page(t, 1, http.MethodPost, "/officers/"+declID+"/edit", url.Values{"name": {"Scout"}, "charter": {"c"}, "runtime": {"auto"}, "mode": {"shadow"},
			"escalate_to": {strconv.FormatInt(esc, 10)}})
		return rec.Code, rec.Body.String()
	}
	put := func(esc int64) (int, string) {
		rec := r.hook(t, http.MethodPut, "/api/officers/declared/"+declID, jsonBody(esc))
		return rec.Code, rec.Body.String()
	}
	for _, c := range []struct {
		name  string
		write func(int64) (int, string)
		ok    int
	}{{"Edit", edit, http.StatusSeeOther}, {"the JSON API's replace", put, http.StatusOK}} {
		t.Run(c.name, func(t *testing.T) {
			before := escOf(1, declID)()
			oc, ob := c.write(reachOutsider)
			nc, nb := c.write(reachNobody)
			if oc != http.StatusBadRequest || !strings.Contains(html.UnescapeString(ob), reachOutWords) {
				t.Errorf("an out-of-reach person: %d; want 400 with the out-of-reach refusal\n%.600s", oc, ob)
			}
			if oc != nc || norm(ob, reachOutsider) != norm(nb, reachNobody) {
				t.Errorf("an out-of-reach person and an id naming nobody are told apart: %d vs %d", oc, nc)
			}
			if got := escOf(1, declID)(); !slices.Equal(got, before) {
				t.Fatalf("a refused change stored escalate_to %v (it was %v)", got, before)
			}
			if code, body := c.write(reachTeam2Member); code != c.ok {
				t.Fatalf("control: an in-reach id: %d; want %d\n%.600s", code, c.ok, body)
			}
			if got := escOf(1, declID)(); len(got) != 1 || got[0] != reachTeam2Member {
				t.Fatalf("control: stored %v; want [%d]", got, reachTeam2Member)
			}
		})
	}

	// The team officers page: the team itself and its member are in reach;
	// the member of another team the manager manages, an outsider and nobody
	// are not, and the last two are told the same.
	t.Run("the team officers page", func(t *testing.T) {
		page := "/teams/" + team + "/officers/defs/mk:a"
		post := func(ids ...int64) (int, string) {
			var parts []string
			for _, id := range ids {
				parts = append(parts, strconv.FormatInt(id, 10))
			}
			rec := r.page(t, 1, http.MethodPost, page, url.Values{"mode": {"live"}, "enabled": {"1"}, "escalate_to": {strings.Join(parts, ",")}})
			return rec.Code, rec.Body.String()
		}
		for _, bad := range []int64{reachTeam2Member, reachTeam2, reachOutsider, reachNobody} {
			if code, _ := post(bad); code != http.StatusBadRequest {
				t.Errorf("the team page naming %d: %d; want 400", bad, code)
			}
		}
		if _, err := r.store.GetDef(sys, declTeam, "mk:a"); !errors.Is(err, builtin.ErrNotFound) {
			t.Errorf("a refused team-page write stored the def: %v", err)
		}
		oc, ob := post(reachOutsider)
		nc, nb := post(reachNobody)
		if oc != nc || ob != nb {
			t.Errorf("the team page tells an outsider from nobody: %d vs %d, bodies equal %v", oc, nc, ob == nb)
		}
		if code, body := post(declTeam, declOther); code != http.StatusSeeOther {
			t.Fatalf("control: the team and its viewer: %d %.400s", code, body)
		}
		if got := escOf(declTeam, "mk:a")(); len(got) != 2 || got[0] != declTeam || got[1] != declOther {
			t.Fatalf("control: stored %v; want the team and its viewer", got)
		}
	})
}

// TestTheEscalationPickerOffersExactlyTheReach: the wizard offers a personal
// officer the people on its owner's teams (not the owner, always asked; not
// an outsider), and a team officer the team and its members — not the member
// of another team the manager manages. Each offered id is one the save takes
// (TestEscalationsReachOnlyPeopleWhoSeeTheOfficer).
func TestTheEscalationPickerOffersExactlyTheReach(t *testing.T) {
	r := newPageRig(t, declRigOpts{pods: true})
	seedReach(t)
	offered := func(body string) map[string]bool {
		f := browserForm(t, strings.ReplaceAll(body, ` name="escalate_to" value="`, ` checked name="escalate_to" value="`), "officer-form")
		out := map[string]bool{}
		for _, v := range f["escalate_to"] {
			out[v] = true
		}
		return out
	}
	id := func(v int64) string { return strconv.FormatInt(v, 10) }
	mine := offered(r.page(t, 1, http.MethodGet, "/officers/new", nil).Body.String())
	if !mine[id(declOther)] || !mine[id(reachTeam2Member)] || mine[id(reachOutsider)] || mine["1"] || len(mine) != 2 {
		t.Errorf("a personal officer's picker offers %v; want exactly %d and %d", mine, declOther, reachTeam2Member)
	}
	theirs := offered(r.page(t, 1, http.MethodGet, "/officers/new?owner="+id(declTeam), nil).Body.String())
	if !theirs[id(declTeam)] || !theirs[id(declOther)] || !theirs["1"] || theirs[id(reachTeam2Member)] || len(theirs) != 3 {
		t.Errorf("a team officer's picker offers %v; want exactly the team, its viewer and its manager", theirs)
	}
}

// TestAStoredOutOfReachEscalationIsRefusedOnTheNextEdit: an escalate_to id
// stored before Q1's rule (any person was accepted) does not break a read —
// the list, the page and the Edit form draw, the form showing it ticked and
// marked — and Shadow/Live still switches (it writes only the mode); the next
// Edit save refuses it until the person unticks it. Nothing drops it
// silently.
func TestAStoredOutOfReachEscalationIsRefusedOnTheNextEdit(t *testing.T) {
	r := newPageRig(t, declRigOpts{pods: true})
	seedReach(t)
	if rec := r.page(t, 1, http.MethodPost, "/officers/new", wizardForm()); rec.Code != http.StatusSeeOther {
		t.Fatalf("seed: %d", rec.Code)
	}
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE officer_defs SET escalate_to = '{987303}' WHERE owner_id = 1 AND officer = '`+declID+`'`)
	r.s.reloadAfterWrite(authz.System(context.Background()))
	for _, p := range []string{"/officers", "/officers/" + declID} {
		if rec := r.page(t, 1, http.MethodGet, p, nil); rec.Code != http.StatusOK {
			t.Fatalf("GET %s with a stored out-of-reach escalation: %d", p, rec.Code)
		}
	}
	form := r.page(t, 1, http.MethodGet, "/officers/"+declID+"/edit", nil)
	if form.Code != http.StatusOK || !strings.Contains(form.Body.String(), `data-escalate-out`) {
		t.Fatalf("the edit form: %d, the stored id marked %v", form.Code, strings.Contains(form.Body.String(), `data-escalate-out`))
	}
	if rec := r.page(t, 1, http.MethodPost, "/officers/"+declID+"/mode", url.Values{"mode": {"live"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("Switch to Live with a stored out-of-reach escalation: %d %.400s", rec.Code, rec.Body.String())
	}
	sub := browserForm(t, form.Body.String(), "officer-form")
	if rec := r.page(t, 1, http.MethodPost, "/officers/"+declID+"/edit", sub); rec.Code != http.StatusBadRequest || !strings.Contains(html.UnescapeString(rec.Body.String()), reachOutWords) {
		t.Fatalf("the edit saved as drawn: %d; want 400 refusing the stored id", rec.Code)
	}
	sub.Del("escalate_to")
	if rec := r.page(t, 1, http.MethodPost, "/officers/"+declID+"/edit", sub); rec.Code != http.StatusSeeOther {
		t.Fatalf("control: the edit with it unticked: %d %.400s", rec.Code, rec.Body.String())
	}
	if d, _ := r.store.GetDef(authz.System(context.Background()), 1, declID); len(d.EscalateTo) != 0 || d.Mode != "live" {
		t.Fatalf("after the save: %+v; want no escalation and the Live switch kept", d)
	}
}

// TestTheWizardHasNoOneChoiceStep (D1) and draws no per-process pass (D2):
// with only "Blank officer" there is no "Start from" step (its value rides in
// a hidden field, and the wizard opens at the name), and neither the list nor
// an officer's page draws a "last pass" this server's wake loop remembers.
// CONTROL: the wizard still creates (the hidden template is submitted).
func TestTheWizardHasNoOneChoiceStep(t *testing.T) {
	r := newPageRig(t, declRigOpts{pods: true})
	body := r.page(t, 1, http.MethodGet, "/officers/new", nil).Body.String()
	if strings.Contains(body, `data-step="1"`) || strings.Contains(body, "Start from") || !strings.Contains(body, `data-first-step="2"`) {
		t.Errorf("the wizard with one template: a step 1 %v, its words %v, opens at step 2 %v", strings.Contains(body, `data-step="1"`),
			strings.Contains(body, "Start from"), strings.Contains(body, `data-first-step="2"`))
	}
	sub := browserForm(t, body, "officer-form")
	if sub.Get("template") != "blank" {
		t.Errorf("the wizard submits template %q; want blank", sub.Get("template"))
	}
	for k, v := range wizardForm() {
		sub[k] = v
	}
	if rec := r.page(t, 1, http.MethodPost, "/officers/new", sub); rec.Code != http.StatusSeeOther {
		t.Fatalf("control: the create: %d %.400s", rec.Code, rec.Body.String())
	}
	o := r.officerFor(t, 1, declID)
	r.enqueue(t, 1, declID, "first")
	r.pass(t, o) // a pass this process ran: what the old card drew
	for _, p := range []string{"/officers", "/officers/" + declID} {
		b := strings.ToLower(r.page(t, 1, http.MethodGet, p, nil).Body.String())
		if strings.Contains(b, "last pass") || strings.Contains(b, "no pass on this server") || strings.Contains(b, `id="officer-pass"`) {
			t.Errorf("%s draws a per-process last pass", p)
		}
	}
}

// TestAnEditKeepsAnOfflineHostOfficersMachine (round 0's 🟡): a host officer
// whose machine is not in the hub's list on this server — offline, or
// connected to another replica — keeps that machine through an Edit saved as
// the browser submits the drawn form: it is drawn selected (the host choice
// enabled), never another machine, never auto. Both probes: another host of
// the owner's listed; no host listed. And the server keeps the stored
// placement for a form that names no runtime, or no machine.
func TestAnEditKeepsAnOfflineHostOfficersMachine(t *testing.T) {
	good := func(name string) mirror.HostView {
		h := spawnableHost(name, true)
		h.AgentClose = true
		return h
	}
	for _, c := range []struct {
		name   string
		listed []mirror.HostView
	}{{"another host listed", []mirror.HostView{good(hostOffOther)}}, {"no host listed", []mirror.HostView{}}} {
		t.Run(c.name, func(t *testing.T) {
			r := newPageRig(t, declRigOpts{hosts: []mirror.HostView{good(hostOffHost), good(hostOffOther)}})
			f := wizardForm()
			f.Set("runtime", "host")
			f.Set("host", hostOffHost)
			if rec := r.page(t, 1, http.MethodPost, "/officers/new", f); rec.Code != http.StatusSeeOther {
				t.Fatalf("seed: %d %.400s", rec.Code, rec.Body.String())
			}
			// Its machine drops off this server's list; its token is still live.
			r.m.mu.Lock()
			r.m.hosts, r.m.tokenHosts = c.listed, []string{hostOffHost, hostOffOther}
			r.m.mu.Unlock()
			sys := authz.System(context.Background())
			wantHost := func(step string) {
				t.Helper()
				row, err := r.store.GetDeclared(sys, 1, declID)
				if err != nil || row.Fields.Runtime != officers.RuntimeHost || row.Fields.Host != hostOffHost {
					t.Fatalf("after %s: runtime %q host %q (%v); want host %s", step, row.Fields.Runtime, row.Fields.Host, err, hostOffHost)
				}
			}
			form := r.page(t, 1, http.MethodGet, "/officers/"+declID+"/edit", nil)
			sub := browserForm(t, form.Body.String(), "officer-form")
			if sub.Get("runtime") != officers.RuntimeHost || sub.Get("host") != hostOffHost {
				t.Errorf("the drawn form submits runtime %q host %q; want host %s", sub.Get("runtime"), sub.Get("host"), hostOffHost)
			}
			sub.Set("charter", "changed")
			if rec := r.page(t, 1, http.MethodPost, "/officers/"+declID+"/edit", sub); rec.Code != http.StatusSeeOther {
				t.Fatalf("the edit saved as drawn: %d %.600s", rec.Code, rec.Body.String())
			}
			wantHost("the edit saved as drawn")
			for _, drop := range []string{"runtime", "host"} {
				g := url.Values{"name": {"Scout"}, "charter": {"again"}, "mode": {"shadow"}, "runtime": {"host"}, "host": {hostOffHost}}
				g.Del(drop)
				if drop == "runtime" {
					g.Del("host")
				}
				if rec := r.page(t, 1, http.MethodPost, "/officers/"+declID+"/edit", g); rec.Code != http.StatusSeeOther {
					t.Fatalf("an edit with no %s: %d %.600s", drop, rec.Code, rec.Body.String())
				}
				wantHost("an edit with no " + drop)
			}
		})
	}
}

// TestATeamHostOfficersInstancesShowTheirWindows (round 0's 🟡): a team host
// officer's instance window, recorded by the fleet as the TEAM (its spawn row
// is the team's), is drawn to a member reading the officer's page — through
// the Postgres host spawn store, whose reads are confined to the acting
// principal's own rows. CONTROL: before the spawn is recorded the instance
// reads "not started" (so the window is the row's, not a fixed string).
func TestATeamHostOfficersInstancesShowTheirWindows(t *testing.T) {
	good := spawnableHost(hostOffHost, true)
	good.AgentClose = true
	r := newPageRig(t, declRigOpts{hosts: []mirror.HostView{good}, pgSpawns: true, hubOwner: declTeam})
	seedReach(t)
	team := strconv.FormatInt(declTeam, 10)
	f := wizardForm()
	f.Set("owner", team)
	f.Set("runtime", "host")
	f.Set("host", hostOffHost)
	f.Set("mode", "live")
	if rec := r.page(t, 1, http.MethodPost, "/officers/new", f); rec.Code != http.StatusSeeOther {
		t.Fatalf("seed: %d %.600s", rec.Code, rec.Body.String())
	}
	o := r.officerFor(t, declTeam, declID)
	if !o.OnHost() {
		t.Fatalf("the team officer %+v; want a host officer", o)
	}
	notStarted := `data-instance="` + o.InstanceName(1) + `"><span class="text-sm text-fg">` + o.InstanceName(1) + `</span><span class="text-xs text-fg2">not started`
	if body := r.page(t, declOther, http.MethodGet, "/officers/"+declID+"?owner="+team, nil).Body.String(); !strings.Contains(body, notStarted) {
		t.Fatalf("control: before any spawn the viewer does not read instance 1 as not started\n%.1500s", body)
	}
	octx := r.s.officerPrincipal(context.Background(), declTeam)
	hs := officers.NewPGHostSpawnStore(r.f.pool)
	row, err := hs.InsertHostSpawn(octx, declID, o.InstanceName(1), hostOffHost)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		dbtest.OwnerExec(t, dbtest.DSN(t), `DELETE FROM officer_host_spawns WHERE owner_id = 987302`)
	})
	if ok, err := hs.RecordHostSpawnWindow(octx, row.ID, hostOffHost, "@71", "sess-team"); err != nil || !ok {
		t.Fatalf("record the window: %v %v", ok, err)
	}
	for _, viewer := range []int64{declOther, 1} {
		body := r.page(t, viewer, http.MethodGet, "/officers/"+declID+"?owner="+team, nil).Body.String()
		if !strings.Contains(body, "window @71 on "+hostOffHost) {
			t.Errorf("user %d reading the team officer's page does not see instance 1's window", viewer)
		}
	}
}

// TestTheWizardRedrawsForTheOwnerChosen (round 0's 🟡): changing "Whose
// officer" redraws the rest of the wizard for that owner — its machines, what
// auto picks, whom its escalations can ask — keeping what was typed and
// creating nothing (the no-JS button, which the script submits on a change).
// CONTROL: the person's own wizard lists their machine.
func TestTheWizardRedrawsForTheOwnerChosen(t *testing.T) {
	good := spawnableHost(hostOffHost, true)
	good.AgentClose = true
	r := newPageRig(t, declRigOpts{hosts: []mirror.HostView{good}})
	seedReach(t)
	mine := r.page(t, 1, http.MethodGet, "/officers/new", nil).Body.String()
	if !strings.Contains(mine, `<option value="`+hostOffHost+`"`) || !strings.Contains(mine, `id="officer-owner-refresh"`) {
		t.Fatalf("control: the person's own wizard: their machine %v, the redraw button %v", strings.Contains(mine, `<option value="`+hostOffHost+`"`),
			strings.Contains(mine, `id="officer-owner-refresh"`))
	}
	f := browserForm(t, mine, "officer-form")
	f.Set("owner", strconv.FormatInt(declTeam, 10))
	f.Set("name", "Typed name")
	f.Set("escalate_to", strconv.FormatInt(reachTeam2Member, 10)) // the person's reach, not the team's
	f.Set("refresh_owner", "1") // the redraw button's name: the contract
	rec := r.page(t, 1, http.MethodPost, "/officers/new", f)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `value="Typed name"`) || !strings.Contains(body, `data-first-step="2"`) {
		t.Fatalf("the redraw: %d, the name kept %v, at step 2 %v\n%.600s", rec.Code, strings.Contains(body, `value="Typed name"`), strings.Contains(body, `data-first-step="2"`), body)
	}
	if strings.Contains(body, `<option value="`+hostOffHost+`"`) {
		t.Error("the team's wizard lists the person's own machine")
	}
	got := browserForm(t, body, "officer-form")
	if got.Get("owner") != strconv.FormatInt(declTeam, 10) || len(got["escalate_to"]) != 0 {
		t.Errorf("the redraw submits owner %q and escalate_to %v; want the team and none carried over", got.Get("owner"), got["escalate_to"])
	}
	if !strings.Contains(body, `name="escalate_to" value="`+strconv.FormatInt(declTeam, 10)+`"`) || strings.Contains(body, `name="escalate_to" value="`+strconv.FormatInt(reachTeam2Member, 10)+`"`) {
		t.Error("the redraw's picker is not the team's reach")
	}
	for _, o := range []int64{1, declTeam} {
		if _, err := r.store.GetDeclared(authz.System(context.Background()), o, "typed-name:main"); !errors.Is(err, builtin.ErrNotFound) {
			t.Errorf("the redraw created an officer for %d: %v", o, err)
		}
	}
}

// TestAnEditDoesNotUndoAModeSwitch (round 0's 🟢): an Edit form opened in
// Shadow, saved after the officer was switched Live from its page with its
// mode left as drawn, keeps it Live; a Pause made meanwhile stays too. And a
// mode switch writes only the mode. CONTROL: an Edit that changes the mode
// writes it.
func TestAnEditDoesNotUndoAModeSwitch(t *testing.T) {
	r := newPageRig(t, declRigOpts{pods: true})
	if rec := r.page(t, 1, http.MethodPost, "/officers/new", wizardForm()); rec.Code != http.StatusSeeOther {
		t.Fatalf("seed: %d", rec.Code)
	}
	sub := browserForm(t, r.page(t, 1, http.MethodGet, "/officers/"+declID+"/edit", nil).Body.String(), "officer-form")
	if sub.Get("mode") != "shadow" {
		t.Fatalf("the drawn form's mode %q; want shadow", sub.Get("mode"))
	}
	for _, p := range []struct {
		path string
		form url.Values
	}{{"/mode", url.Values{"mode": {"live"}}}, {"/pause", url.Values{}}} {
		if rec := r.page(t, 1, http.MethodPost, "/officers/"+declID+p.path, p.form); rec.Code != http.StatusSeeOther {
			t.Fatalf("POST %s: %d", p.path, rec.Code)
		}
	}
	sub.Set("charter", "edited later")
	if rec := r.page(t, 1, http.MethodPost, "/officers/"+declID+"/edit", sub); rec.Code != http.StatusSeeOther {
		t.Fatalf("the edit: %d %.400s", rec.Code, rec.Body.String())
	}
	sys := authz.System(context.Background())
	row, _ := r.store.GetDeclared(sys, 1, declID)
	ty, _ := r.store.GetType(sys, 1, row.TypeKey)
	if row.Mode != "live" || row.Enabled || ty.Charter != "edited later" {
		t.Fatalf("after the edit: mode %s, enabled %v, charter %q; want live, paused, the edit's charter", row.Mode, row.Enabled, ty.Charter)
	}
	sub2 := browserForm(t, r.page(t, 1, http.MethodGet, "/officers/"+declID+"/edit", nil).Body.String(), "officer-form")
	sub2.Set("mode", "shadow")
	if rec := r.page(t, 1, http.MethodPost, "/officers/"+declID+"/edit", sub2); rec.Code != http.StatusSeeOther {
		t.Fatalf("control: an edit to shadow: %d", rec.Code)
	}
	if row, _ := r.store.GetDeclared(sys, 1, declID); row.Mode != "shadow" || row.Enabled {
		t.Fatalf("control: after an edit changing the mode: %s, enabled %v; want shadow, still paused", row.Mode, row.Enabled)
	}
}

// TestTheOfficerPagesSmallFixes (round 0's 🟢): the list draws the delete's
// notice; Run now and a schedule's add and delete on a TEAM officer are in the
// team's audit; a read the core could not make (the hub's token list) is
// answered generically on the page and the JSON API, never in its own words.
func TestTheOfficerPagesSmallFixes(t *testing.T) {
	good := spawnableHost(hostOffHost, true)
	good.AgentClose = true
	r := newPageRig(t, declRigOpts{pods: true, hosts: []mirror.HostView{good}})
	seedReach(t)
	if rec := r.page(t, 1, http.MethodGet, "/officers?notice=deleted", nil); !strings.Contains(rec.Body.String(), `id="officer-notice"`) {
		t.Error("the list does not draw the delete's notice")
	}
	if rec := r.page(t, 1, http.MethodGet, "/officers?notice=bogus", nil); strings.Contains(rec.Body.String(), `id="officer-notice"`) {
		t.Error("control: the list draws a notice for an unknown key")
	}
	team := strconv.FormatInt(declTeam, 10)
	f := wizardForm()
	f.Set("owner", team)
	if rec := r.page(t, 1, http.MethodPost, "/officers/new", f); rec.Code != http.StatusSeeOther {
		t.Fatalf("seed: %d %.400s", rec.Code, rec.Body.String())
	}
	for _, p := range []struct {
		path string
		form url.Values
	}{{"/run", url.Values{}}, {"/schedules", url.Values{"schedule_preset": {"hourly"}, "name": {"nightly"}}}, {"/schedules/nightly/delete", url.Values{}}} {
		p.form.Set("owner", team)
		if rec := r.page(t, 1, http.MethodPost, "/officers/"+declID+p.path, p.form); rec.Code != http.StatusSeeOther {
			t.Fatalf("POST %s: %d %.400s", p.path, rec.Code, rec.Body.String())
		}
	}
	var actions []string
	rows, err := r.f.pool.Query(authz.System(context.Background()), `SELECT action FROM audit_log WHERE team_id = $1 ORDER BY id`, declTeam)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a string
		_ = rows.Scan(&a)
		actions = append(actions, a)
	}
	rows.Close()
	got := strings.Join(actions, ",")
	for _, want := range []string{"officer.create", "officer.run", "officer.schedule.put", "officer.schedule.delete"} {
		if !strings.Contains(","+got+",", ","+want+",") {
			t.Errorf("the team's audit %q lacks %s", got, want)
		}
	}
	r.m.mu.Lock()
	r.m.tokenErr = errors.New("hub-token-table-secret-words")
	r.m.mu.Unlock()
	hf := wizardForm()
	hf.Set("name", "Hosted")
	hf.Set("runtime", "host")
	hf.Set("host", hostOffHost)
	page := r.page(t, 1, http.MethodPost, "/officers/new", hf)
	api := r.hook(t, http.MethodPost, "/api/officers/declared", `{"id":"hosted:main","name":"Hosted","charter":"c","runtime":"host","host":"`+hostOffHost+`"}`)
	for name, rec := range map[string]struct {
		code int
		body string
	}{"the wizard": {page.Code, page.Body.String()}, "the JSON API": {api.Code, api.Body.String()}} {
		if rec.code != http.StatusInternalServerError || strings.Contains(rec.body, "hub-token-table-secret-words") {
			t.Errorf("%s when the hub's token read fails: %d, its words drawn %v; want a generic 500", name, rec.code, strings.Contains(rec.body, "hub-token-table-secret-words"))
		}
	}
	if !strings.Contains(r.logs.String(), "hub-token-table-secret-words") {
		t.Error("the failed read is not logged")
	}
}
