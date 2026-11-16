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

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/users"
)

// "CONFIRM IT'S YOU" IN PLACE ON A FORM PAGE (stepUpFormsHTML): a page whose
// writes are step-up gated draws the confirm dialog and marks every one of
// those forms data-step-up-form, so a stale sign-in confirms where it is and
// the form submits — instead of a 403 page saying "Confirm it's you" with no
// way to (found on v0.232.0's team pages by a live browser check: only signing
// out and in again got past it).

// formTagRE is one opening <form> tag.
var formTagRE = regexp.MustCompile(`<form\b[^>]*>`)

// gatedForms checks body's step-up wiring: the dialog with why, the form
// script, and EVERY POST form whose action gated reports marked
// data-step-up-form. It returns the actions of those forms, so the caller
// pins the set (a form that stopped being drawn, or a new one, changes it).
func gatedForms(t *testing.T, who, body string, gated func(action string) bool, why string) []string {
	t.Helper()
	if !strings.Contains(body, `id="term-confirm"`) {
		t.Errorf("%s: no confirm dialog (#term-confirm) on the page", who)
	}
	if !strings.Contains(body, html.EscapeString(why)) {
		t.Errorf("%s: the dialog does not say the page's own gate %q", who, why)
	}
	if !strings.Contains(body, "window.musterStepUpForms") {
		t.Errorf("%s: no form script (ui.StepUpFormScript) on the page", who)
	}
	var actions []string
	for _, tag := range formTagRE.FindAllString(body, -1) {
		m := regexp.MustCompile(`action="([^"]*)"`).FindStringSubmatch(tag)
		if m == nil || !gated(m[1]) || !strings.Contains(tag, `method="POST"`) {
			continue
		}
		actions = append(actions, m[1])
		if !strings.Contains(tag, "data-step-up-form") {
			t.Errorf("%s: the gated form %s is not marked data-step-up-form: %s", who, m[1], tag)
		}
	}
	return actions
}

// under is gatedForms' filter for every form posting under prefix.
func under(prefix string) func(string) bool {
	return func(a string) bool { return strings.HasPrefix(a, prefix) }
}

func sameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]int{}
	for _, g := range got {
		seen[g]++
	}
	for _, w := range want {
		if seen[w] == 0 {
			return false
		}
		seen[w]--
	}
	return true
}

// TestTheTeamPluginsPageConfirmsInPlace: a stale MANAGER on
// /teams/{id}/plugins is drawn the confirm dialog, and both its gated forms
// (mint, and a live plugin's revoke) ask before they submit. The 403 page a
// stale mint is answered with (the form submitted before the page knew) draws
// it too, so the manager confirms there and tries again.
//
// 🔴 RED at ff1295f8: the page drew no dialog and no form was marked.
func TestTheTeamPluginsPageConfirmsInPlace(t *testing.T) {
	f := newTPlug(t)
	w := f.w
	id := strconv.FormatInt(f.team, 10)
	page := "/teams/" + id + "/plugins"
	ids := map[string]string{"id": id}
	_, p := f.viewerPlugin(t, f.teamCtx(), "tplug-confirm-"+w.suffix, "https://c.viewer.example.net/e")
	pid := strconv.FormatInt(p.ID, 10)

	rec := teamHostsReq(w, w.srv.handleTeamPluginsPage, "GET", page, nil, w.a.uid, false, ids)
	if rec.Code != http.StatusOK {
		t.Fatalf("a stale manager's GET: %d; want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data-fresh-for="0"`) {
		t.Errorf("CONTROL: the stale page's dialog does not know it is stale (data-fresh-for 0)")
	}
	got := gatedForms(t, "the stale page", body, under(page), TeamPluginsStepUpWhy())
	if want := []string{page, page + "/" + pid + "/revoke"}; !sameSet(got, want) {
		t.Errorf("the page's gated forms %v; want exactly %v", got, want)
	}

	form := url.Values{"name": {"tplug-stale-" + w.suffix}, "origin": {tplugZone}, "entry_url": {"https://s.viewer.example.net/e"}}
	rec = teamHostsReq(w, w.srv.handleTeamPluginMint, "POST", page, form, w.a.uid, false, ids)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), html.EscapeString(TeamPluginsStepUp)) {
		t.Fatalf("a stale manager's mint: %d; want 403 saying %q", rec.Code, TeamPluginsStepUp)
	}
	gatedForms(t, "the stale mint's 403 page", rec.Body.String(), under(page), TeamPluginsStepUpWhy())
}

// TestTheTeamHostsPageConfirmsInPlace: as the plugins page, for
// /teams/{id}/hosts — mint, a live token's revoke and (with a coded machine
// held) the replace form are every gated form, and each asks first.
//
// 🔴 RED at ff1295f8: the page drew no dialog and no form was marked.
func TestTheTeamHostsPageConfirmsInPlace(t *testing.T) {
	w := newIsoWorld(t)
	sys := authz.System(context.Background())
	store := users.NewPG(w.pool)
	admin := users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}
	team, err := store.CreateTeam(sys, admin, "hosts-confirm-"+w.suffix, w.a.uid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM mirror_tokens WHERE owner_id = $1`, `DELETE FROM team_members WHERE team_id = $1`,
			`DELETE FROM audit_log WHERE team_id = $1`, `DELETE FROM users WHERE id = $1`} {
			_, _ = w.pool.Exec(sys, q, team.ID)
		}
	})
	toks := mirror.NewPG(w.pool)
	w.srv.ext.MirrorTokens = toks
	w.srv.ext.Mirror = &heldPageHub{Service: mirror.NewService(mirror.NewPG(w.pool), nil),
		held: []mirror.HeldReplacement{{Host: "build-01", Addr: "192.0.2.7", Attempts: 1, First: time.Now(), Last: time.Now(), Coded: true}}}
	tctx, err := w.srv.teamAdminContext(authz.WithPrincipal(context.Background(), w.srv.principalFor(context.Background(), w.a.uid, authz.ViaSession)), team.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, tok, err := toks.CreateToken(tctx, "build-01")
	if err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(team.ID, 10)
	page := "/teams/" + id + "/hosts"
	ids := map[string]string{"id": id}

	body := teamHostsReq(w, w.srv.handleTeamHostsPage, "GET", page, nil, w.a.uid, false, ids).Body.String()
	got := gatedForms(t, "the stale page", body, under(page), TeamHostsStepUpWhy())
	if want := []string{page, page + "/" + strconv.FormatInt(tok.ID, 10) + "/revoke", page + "/replace"}; !sameSet(got, want) {
		t.Errorf("the page's gated forms %v; want exactly %v", got, want)
	}
	rec := teamHostsReq(w, w.srv.handleTeamHostMint, "POST", page, url.Values{"host": {"build-02"}}, w.a.uid, false, ids)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), html.EscapeString(TeamHostsStepUp)) {
		t.Fatalf("a stale manager's mint: %d; want 403 saying %q", rec.Code, TeamHostsStepUp)
	}
	gatedForms(t, "the stale mint's 403 page", rec.Body.String(), under(page), TeamHostsStepUpWhy())
}

// TestThePluginsPagePairingApproveConfirmsInPlace: /plugins' pairing
// Approve was already marked data-step-up-form (decidePairForm is step-up
// gated), but the page drew no dialog for the mark to use — the same defect
// as the team pages. With a request pending, the dialog is drawn.
//
// 🔴 RED at ff1295f8: no dialog on /plugins.
func TestThePluginsPagePairingApproveConfirmsInPlace(t *testing.T) {
	w := newOutboundWorld(t, true)
	if _, rec := w.pair(pairBody); rec.Code != http.StatusAccepted {
		t.Fatalf("POST /plugin/v1/pair: %d %s", rec.Code, rec.Body.String())
	}
	rec := w.do("GET", "/plugins", "", w.operatorForm)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "data-pair-approve") {
		t.Fatalf("CONTROL: /plugins with a pending pairing request: %d, approve drawn %v", rec.Code, strings.Contains(rec.Body.String(), "data-pair-approve"))
	}
	// Only the Approve is gated (a denial needs no fresh sign-in).
	approve := func(a string) bool {
		return strings.HasPrefix(a, pluginsPath+"/pair/") && strings.HasSuffix(a, "/approve")
	}
	if got := gatedForms(t, "/plugins", rec.Body.String(), approve, PairStepUpWhy()); len(got) != 1 {
		t.Errorf("the pairing approve forms %v; want the one request's", got)
	}
}
