package api

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attemptcode"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/users"
)

// codePageHub is heldPageHub that also answers code confirms: right is the
// one code it arms.
type codePageHub struct {
	heldPageHub
	right string
	typed []string
}

func (h *codePageHub) ConfirmCode(_ context.Context, code string) (string, string, error) {
	h.typed = append(h.typed, code)
	sum, ok := attemptcode.Hash(code)
	fp := ""
	if ok {
		fp = attemptcode.Fingerprint(sum)
	}
	if ok && code == h.right {
		return "build-01", fp, nil
	}
	return "", fp, mirror.ErrNoSuchCode
}

// TestTheCardTakesATypedCodeAndOffersNoRowButtonForACodedMachine: with a
// CODED held machine and a codeless one on the card, NO row has a Replace
// form and the card has the one code field; the codeless row says it sends
// no code, so it cannot be confirmed: upgrade muster-mirror there, or stop
// the live host. A typed code confirms (303) and is audited by its
// FINGERPRINT — the code itself is in no audit row or log line; a wrong code,
// or an address form's fields with no code, is refused (409), audited as
// refused, and arms nothing.
//
// Red at f8313e4f (#685 head: every row gets an address form and there is no
// code field) and at a21882d4 (this PR's first head: the codeless row keeps
// #685's address form).
func TestTheCardTakesATypedCodeAndOffersNoRowButtonForACodedMachine(t *testing.T) {
	const right = "M7Q2-W9XK-4HTR"
	w := newIsoWorld(t)
	sys := authz.System(context.Background())
	store := users.NewPG(w.pool)
	team, err := store.CreateTeam(sys, users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}, "code-team-"+w.suffix, w.a.uid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM team_members WHERE team_id = $1`, `DELETE FROM audit_log WHERE team_id = $1`, `DELETE FROM users WHERE id = $1`} {
			_, _ = w.pool.Exec(sys, q, team.ID)
		}
	})
	now := time.Now()
	hub := &codePageHub{heldPageHub: heldPageHub{Service: mirror.NewService(mirror.NewPG(w.pool), nil), held: []mirror.HeldReplacement{
		{Host: "build-01", Addr: "198.51.100.21", Coded: true, First: now, Last: now, Attempts: 3},
		{Host: "build-01", Addr: "198.51.100.29", First: now, Last: now, Attempts: 1},
	}}, right: right}
	w.srv.ext.MirrorTokens = mirror.NewPG(w.pool)
	w.srv.ext.Mirror = hub
	logs := &strings.Builder{}
	w.srv.logger = log.New(logs, "", 0)
	id := strconv.FormatInt(team.ID, 10)
	page := "/teams/" + id + "/hosts"
	ids := map[string]string{"id": id}
	body := teamHostsReq(w, w.srv.handleTeamHostsPage, "GET", page, nil, w.a.uid, false, ids).Body.String()
	if strings.Contains(body, `name="addr" value="198.51.100.21"`) {
		t.Fatal("the card offers a per-row Replace form for a CODED machine")
	}
	if !strings.Contains(body, `id="team-host-code"`) || !strings.Contains(body, `name="code"`) {
		t.Fatalf("the card has no code field:\n%.2500s", body)
	}
	if n := strings.Count(body, `action="/teams/`+id+`/hosts/replace"`); strings.Contains(body, `name="addr"`) || n != 1 {
		t.Fatalf("the card has %d replace form(s), or an address field; want only the one code form:\n%.2500s", n, body)
	}
	const noCode = "sends no attempt code (an older muster-mirror), so it cannot be confirmed: upgrade muster-mirror on that machine, or stop the live host"
	if n := strings.Count(body, noCode); n != 1 {
		t.Fatalf("%d rows say they send no code; want exactly the codeless one:\n%.2500s", n, body)
	}
	audits := func(action string) []string {
		t.Helper()
		rows, err := w.pool.Query(sys, `SELECT detail::text FROM audit_log WHERE team_id = $1 AND action = $2 AND user_id = $3`, team.ID, action, w.a.uid)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var d string
			if err := rows.Scan(&d); err != nil {
				t.Fatal(err)
			}
			out = append(out, d)
		}
		return out
	}
	wrong := "ZZZZ-ZZZZ-ZZZZ"
	if rec := teamHostsReq(w, w.srv.handleTeamHostReplace, "POST", page+"/replace", url.Values{"code": {wrong}}, w.a.uid, true, ids); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "No machine waiting for this team shows that code") {
		t.Fatalf("a wrong code: %d; want 409 saying no machine shows it", rec.Code)
	}
	if got := audits("team.host.replace"); len(got) != 0 {
		t.Fatalf("a wrong code was audited as a replace: %v", got)
	}
	wsum, _ := attemptcode.Hash(wrong)
	if got := audits("team.host.replace.refused"); len(got) != 1 || !strings.Contains(got[0], attemptcode.Fingerprint(wsum)) || strings.Contains(got[0], wrong) {
		t.Fatalf("the refused audit rows = %v; want one carrying the typed code's fingerprint and not the code", got)
	}
	if rec := teamHostsReq(w, w.srv.handleTeamHostReplace, "POST", page+"/replace", url.Values{"code": {right}}, w.a.uid, true, ids); rec.Code != http.StatusSeeOther {
		t.Fatalf("the right code: %d\n%.800s", rec.Code, rec.Body.String())
	}
	sum, _ := attemptcode.Hash(right)
	got := audits("team.host.replace")
	if len(got) != 1 || !strings.Contains(got[0], attemptcode.Fingerprint(sum)) || strings.Contains(got[0], right) || strings.Contains(got[0], strings.ReplaceAll(right, "-", "")) {
		t.Fatalf("the replace audit rows = %v; want one carrying the code's fingerprint %s and never the code", got, attemptcode.Fingerprint(sum))
	}
	if !strings.Contains(logs.String(), attemptcode.Fingerprint(sum)) {
		t.Fatalf("POSITIVE CONTROL: the server's log does not name the confirmed code's fingerprint:\n%s", logs.String())
	}
	for _, form := range []string{right, strings.ReplaceAll(right, "-", ""), wrong, strings.ReplaceAll(wrong, "-", "")} {
		if strings.Contains(logs.String(), form) {
			t.Fatalf("the server's log carries a typed code (%q):\n%s", form, logs.String())
		}
	}
	// #685's address form, posted anyway: no code, so nothing is confirmed.
	if rec := teamHostsReq(w, w.srv.handleTeamHostReplace, "POST", page+"/replace", url.Values{"host": {"build-01"}, "addr": {"198.51.100.29"}}, w.a.uid, true, ids); rec.Code != http.StatusConflict {
		t.Fatalf("an address confirm with no code: %d; want 409", rec.Code)
	}
	if got := audits("team.host.replace"); len(got) != 1 {
		t.Fatalf("an address confirm with no code was audited as a replace: %v", got)
	}
	// A stale sign-in confirms nothing: the step-up stays on the code path.
	n := len(hub.typed)
	if rec := teamHostsReq(w, w.srv.handleTeamHostReplace, "POST", page+"/replace", url.Values{"code": {right}}, w.a.uid, false, ids); rec.Code != http.StatusForbidden || len(hub.typed) != n {
		t.Fatalf("a stale code confirm: %d, hub asked %d more time(s); want 403 and none", rec.Code, len(hub.typed)-n)
	}
}
