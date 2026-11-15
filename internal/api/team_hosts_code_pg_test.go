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
// one code it arms; limited answers every code confirm as rate-limited.
type codePageHub struct {
	heldPageHub
	right   string
	limited bool
	typed   []string
}

func (h *codePageHub) ConfirmCode(_ context.Context, code string) (string, string, error) {
	h.typed = append(h.typed, code)
	sum, ok := attemptcode.Hash(code)
	fp := ""
	if ok {
		fp = attemptcode.Fingerprint(sum)
	}
	switch {
	case h.limited:
		return "", "", mirror.ErrConfirmRateLimited
	case ok && code == h.right:
		return "build-01", fp, nil
	}
	return "", fp, mirror.ErrNoSuchCode
}

// TestTheCardTakesATypedCodeAndOffersNoRowButtonForACodedMachine: with a
// CODED held machine and an old muster-mirror's codeless one on the card, the
// coded row has no Replace form (it is named only by its code) and the card
// has the one code field; the codeless row is labelled "cannot be told apart
// from other machines (old muster-mirror)" and keeps #685's address form. A
// typed code confirms (303) and is audited by its FINGERPRINT — the code
// itself is in no audit row; a wrong code is refused (409), audited as
// refused, and arms nothing; a rate-limited confirm is 429.
//
// Red at the #685 head (f8313e4f): every row gets an address form and there
// is no code field.
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
	if !strings.Contains(body, `name="addr" value="198.51.100.29"`) || !strings.Contains(body, "cannot be told apart from other machines (old muster-mirror)") {
		t.Fatalf("the codeless row lost its fallback form or its old-muster-mirror label:\n%.2500s", body)
	}
	if n := strings.Count(body, "cannot be told apart from other machines (old muster-mirror)"); n != 1 {
		t.Fatalf("%d rows labelled old muster-mirror; want exactly the codeless one", n)
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
	if len(hub.confirmed) != 0 {
		t.Fatalf("a code confirm asked the hub for an ADDRESS confirm: %v", hub.confirmed)
	}
	hub.limited = true
	if rec := teamHostsReq(w, w.srv.handleTeamHostReplace, "POST", page+"/replace", url.Values{"code": {right}}, w.a.uid, true, ids); rec.Code != http.StatusTooManyRequests ||
		!strings.Contains(rec.Body.String(), "Too many wrong codes") {
		t.Fatalf("a rate-limited confirm: %d; want 429", rec.Code)
	}
	// A stale sign-in confirms nothing: the step-up stays on the code path.
	n := len(hub.typed)
	if rec := teamHostsReq(w, w.srv.handleTeamHostReplace, "POST", page+"/replace", url.Values{"code": {right}}, w.a.uid, false, ids); rec.Code != http.StatusForbidden || len(hub.typed) != n {
		t.Fatalf("a stale code confirm: %d, hub asked %d more time(s); want 403 and none", rec.Code, len(hub.typed)-n)
	}
}
