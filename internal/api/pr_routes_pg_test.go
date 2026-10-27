package api

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/prroute"
)

// PULL-REQUEST → SESSION ROUTING (pr_routes.go, migration 0055), through the
// real server against a real Postgres: a review mail from a listed, verified
// sender goes to the session that authored its pull request — or, when that
// session's window is gone, to the chief with the session named; anything
// else is handed on exactly as before.
//
// Fixture values are invented and pairwise distinct: the sender is not the
// one a deployment configures, and the repo, number, sha and branch differ
// from every other test's.

const (
	prSender  = "review-gate/pr-check"
	prRepo    = "acme-labs/widget-api"
	prNumber  = 4817
	prSHA     = "3f2a9c0e7b14d6a85c3e9f01b2d47a6e8c5f1b09"
	prBranch  = "feat/gizmo-route"
	prOtherID = "5e55a1d0-0000-4000-8000-0000000b0b0b"
)

type prFixture struct {
	*threadFixture
}

func newPRFixture(t *testing.T) *prFixture {
	t.Helper()
	f := &prFixture{threadFixture: newThreadFixture(t)}
	senders, err := plugins.ParsePRMailSenders(prSender)
	if err != nil {
		t.Fatal(err)
	}
	f.srv.ext.PRMail = &PRMail{Senders: senders}
	f.srv.ext.PRSessions = f.plugins
	// The chief's turn carries the hints production wires (UseExtensions:
	// pluginHints), which is where the author block comes from.
	d, err := plugins.NewDeliverer(plugins.DelivererConfig{Store: f.plugins, Agents: f.srv.ext.Agents, Gateway: f.turns,
		Turn: f.turns.turn, Routing: true, Hints: f.srv.pluginHints(f.srv.ext)})
	if err != nil {
		t.Fatal(err)
	}
	f.srv.pluginDeliverer = d
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM session_prs WHERE owner_id = $1`, f.owner)
	})
	return f
}

// reviewBody is a gate mail: prose, a decoy block, then the machine block.
func reviewBody(fields string) string {
	return "Review of the pull request: 2 findings.\n\n```\nfinding 1: a nil map write\n```\n\n```json\n{" + fields + "}\n```\n"
}

func fullFields() string {
	return `"repo":"` + prRepo + `","pr":` + strconv.Itoa(prNumber) + `,"head_sha":"` + prSHA + `","head_ref":"` + prBranch + `"`
}

// review stores one review mail through the agent-mail plugin and hands it on
// (dispatchPluginMessage), waited for.
func (f *prFixture) review(t *testing.T, external, sender string, verified bool, body string) plugins.Message {
	t.Helper()
	m, created, err := f.plugins.Submit(f.asOwner, f.agentMail.ID, plugins.MessageIn{ExternalID: external, Sender: sender,
		Subject: "review: Gizmo route", Body: body, SenderVerified: verified})
	if err != nil || !created {
		t.Fatalf("submit: %v %v", created, err)
	}
	f.dispatch(t, m)
	return m
}

// authored records sess as having created the pull request (or pushed its
// branch), for owner.
func (f *prFixture) authored(t *testing.T, ctx context.Context, sess string, sig prroute.Signal) {
	t.Helper()
	if _, err := f.plugins.RecordPRSession(ctx, sess, "tower", sig); err != nil {
		t.Fatal(err)
	}
}

func prCreated() prroute.Signal {
	return prroute.Signal{Kind: prroute.KindPRCreate, Repo: prRepo, PR: prNumber}
}

// lastTurn is the chief's most recent delivered turn ("" for none).
func (f *prFixture) lastTurn() string {
	f.turns.mu.Lock()
	defer f.turns.mu.Unlock()
	if len(f.turns.sent) == 0 {
		return ""
	}
	return f.turns.sent[len(f.turns.sent)-1]
}

func TestAReviewOfAnOnlineSessionsPRIsRoutedToIt(t *testing.T) {
	f := newPRFixture(t)
	f.authored(t, f.asOwner, routeSID, prCreated())
	m := f.review(t, "pr-online-1", prSender, true, reviewBody(fullFields()))
	rs := f.routesOf(t, m)
	if len(rs) != 1 || rs[0].State != plugins.RouteRouted || rs[0].TargetKind != "mirror" || rs[0].TargetKey != routeSID ||
		rs[0].AgentID == nil || *rs[0].AgentID != f.chief.ID {
		t.Fatalf("routes: %+v", rs)
	}
	items := f.queued(t)
	if len(items) != 1 || rs[0].SendQueueID == nil || *rs[0].SendQueueID != items[0].ID ||
		!strings.Contains(items[0].Body, "Routed to you by "+routedByPR+": route_id "+rs[0].RouteID) ||
		!strings.Contains(items[0].Body, "finding 1: a nil map write") {
		t.Fatalf("queued: %+v", items)
	}
	if got, _ := f.plugins.Message(f.asOwner, m.ID); got.Status != plugins.StatusDelivered || got.AgentID == nil || *got.AgentID != f.chief.ID {
		t.Fatalf("the message: %+v; want delivered to the chief", got)
	}
	if n := f.turns.count(); n != 0 {
		t.Fatalf("the chief took %d turn(s); a route by pull request takes none", n)
	}
	if note := f.chiefDefaultSessionText(t); !strings.Contains(note, "finding 1: a nil map write") || !strings.Contains(note, "Route "+rs[0].RouteID) {
		t.Fatalf("the chief's conversation does not hold the message and its route: %q", note)
	}
	var via string
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT detail->>'routed_via' FROM audit_log
		WHERE action = 'session_route.routed' AND object = $1`, plugins.RouteObject(rs[0].RouteID)).Scan(&via); err != nil ||
		via != "pr "+prRepo+"#"+strconv.Itoa(prNumber)+" authored by "+routeSID {
		t.Fatalf("routed_via %q (%v)", via, err)
	}
	// The row is marked as a chief route marks it: no unrouted ask is due.
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE plugin_messages SET delivered_at = now() - interval '1 hour' WHERE id = `+strconv.FormatInt(m.ID, 10))
	due, err := f.plugins.UnroutedDue(context.Background(), time.Now(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range due {
		if d.ID == m.ID {
			t.Fatal("a message routed by pull request is due an unrouted ask")
		}
	}
}

func TestAReviewMatchedByItsPushedBranchIsRouted(t *testing.T) {
	f := newPRFixture(t)
	f.authored(t, f.asOwner, routeSID, prroute.Signal{Kind: prroute.KindPush, Repo: prRepo, HeadRef: prBranch})
	m := f.review(t, "pr-push-1", prSender, true, reviewBody(fullFields()))
	if rs := f.routesOf(t, m); len(rs) != 1 || rs[0].TargetKey != routeSID || f.turns.count() != 0 {
		t.Fatalf("%+v, %d chief turns; want one route to the pushing session and no turn", rs, f.turns.count())
	}
}

func TestTheMostRecentAuthorOfTheOwnerIsChosen(t *testing.T) {
	f := newPRFixture(t)
	f.mirror.set(func(m *routeMirror) { m.otherSession = prOtherID })
	f.authored(t, f.asOwner, routeSID, prCreated())
	f.authored(t, f.asOwner, prOtherID, prCreated())
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE session_prs SET last_seen = now() - interval '2 hours' WHERE claude_session = $1`, routeSID)
	m := f.review(t, "pr-recent-1", prSender, true, reviewBody(fullFields()))
	if rs := f.routesOf(t, m); len(rs) != 1 || rs[0].TargetKey != prOtherID {
		t.Fatalf("routes %+v; want the more recent author %s", rs, prOtherID)
	}
}

// 🔴 Another owner's session is never chosen, even when it is the more recent
// author and its window is one this owner's mirror shows.
func TestAnotherOwnersAuthorIsNeverChosen(t *testing.T) {
	f := newPRFixture(t)
	other := f.newMember(t)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM session_prs WHERE owner_id = $1`, other)
	})
	f.mirror.set(func(m *routeMirror) { m.otherSession = prOtherID })
	f.authored(t, f.asOwner, routeSID, prCreated())
	f.authored(t, f.as(other), prOtherID, prCreated())
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE session_prs SET last_seen = now() - interval '2 hours' WHERE claude_session = $1`, routeSID)
	m := f.review(t, "pr-owner-1", prSender, true, reviewBody(fullFields()))
	if rs := f.routesOf(t, m); len(rs) != 1 || rs[0].TargetKey != routeSID {
		t.Fatalf("routes %+v; want the owner's own author %s, never the other owner's %s", rs, routeSID, prOtherID)
	}
}

func TestAReviewOfAnOfflineSessionsPRGoesToTheChiefNamingIt(t *testing.T) {
	f := newPRFixture(t)
	f.authored(t, f.asOwner, routeSID, prCreated())
	// The window now holds another conversation: no online window shows the author.
	f.mirror.set(func(m *routeMirror) { m.claudeSession = "5e55a1d0-0000-4000-8000-0000000c0ffe" })
	m := f.review(t, "pr-offline-1", prSender, true, reviewBody(fullFields()))
	if rs := f.routesOf(t, m); len(rs) != 0 || f.turns.count() != 1 || len(f.queued(t)) != 0 {
		t.Fatalf("%d routes, %d chief turns, %d queued; want 0, 1, 0", len(rs), f.turns.count(), len(f.queued(t)))
	}
	turn := f.lastTurn()
	at := strings.Index(turn, "\n"+plugins.AuthorHeader)
	if at < 0 {
		t.Fatalf("the chief's turn does not name the author:\n%s", turn)
	}
	block := turn[at:]
	for _, want := range []string{"| mirror:" + routeSID + " ", "@ tower", "gh-pr-create " + prRepo + "#" + strconv.Itoa(prNumber),
		"window: offline", "No online mirrored window shows that session now"} {
		if !strings.Contains(block, want) {
			t.Errorf("the author block lacks %q:\n%s", want, block)
		}
	}
}

// The no-match cases: the chief gets the message exactly as before — one turn,
// no route, no author block.
func TestAReviewMusterDoesNotRouteGoesToTheChiefUnchanged(t *testing.T) {
	cases := []struct {
		name     string
		sender   string
		verified bool
		body     string
		senders  string // "" leaves the fixture's list
		off      bool   // no PRMail at all
	}{
		{name: "no author recorded", sender: prSender, verified: true,
			body: reviewBody(`"repo":"acme-labs/gadget-api","pr":4817,"head_sha":"` + prSHA + `"`)},
		{name: "an unverified sender", sender: prSender, verified: false, body: reviewBody(fullFields())},
		{name: "an unlisted sender", sender: "elsewhere/pr-check", verified: true, body: reviewBody(fullFields())},
		{name: "today's mail, no repo", sender: prSender, verified: true,
			body: reviewBody(`"pr":` + strconv.Itoa(prNumber) + `,"head_sha":"` + prSHA + `"`)},
		{name: "an empty list", sender: prSender, verified: true, body: reviewBody(fullFields()), senders: "-"},
		{name: "the feature off", sender: prSender, verified: true, body: reviewBody(fullFields()), off: true},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newPRFixture(t)
			f.authored(t, f.asOwner, routeSID, prCreated())
			switch {
			case c.off:
				f.srv.ext.PRMail = nil
			case c.senders == "-":
				f.srv.ext.PRMail = &PRMail{}
			}
			m := f.review(t, "pr-nomatch-"+strconv.Itoa(i), c.sender, c.verified, c.body)
			if rs := f.routesOf(t, m); len(rs) != 0 || f.turns.count() != 1 || len(f.queued(t)) != 0 {
				t.Fatalf("%d routes, %d chief turns, %d queued; want 0, 1, 0", len(rs), f.turns.count(), len(f.queued(t)))
			}
			if turn := f.lastTurn(); strings.Contains(turn, plugins.AuthorHeader) || strings.Contains(turn, "pr-author") {
				t.Fatalf("an unmatched review's turn names an author:\n%s", turn)
			}
		})
	}
	// Positive control: the same fixture and a matching, verified, listed mail IS routed.
	f := newPRFixture(t)
	f.authored(t, f.asOwner, routeSID, prCreated())
	if rs := f.routesOf(t, f.review(t, "pr-nomatch-control", prSender, true, reviewBody(fullFields()))); len(rs) != 1 {
		t.Fatalf("control: %d routes", len(rs))
	}
}

// --- the hook's report (POST /api/pr-sessions) ---------------------------------

// report POSTs body to the report route with token tok (the owner's personal
// token when ""): the status and the response body.
func (f *prFixture) report(t *testing.T, tok, body string) (int, string) {
	t.Helper()
	if tok == "" {
		tok = f.token
	}
	w := f.call(t, "POST", prSessionsPath, strings.NewReader(body), map[string]string{"Authorization": "Bearer " + tok})
	return w.Code, w.Body.String()
}

func reportBody(fields map[string]any) string {
	b, _ := json.Marshal(fields)
	return string(b)
}

func createReport(sess string) map[string]any {
	return map[string]any{"repo": prRepo, "pr": prNumber, "head_ref": nil, "claude_session": sess, "kind": "gh-pr-create"}
}

// rowsFor is how many session_prs rows of any owner name sess.
func (f *prFixture) rowsFor(t *testing.T, sess string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM session_prs WHERE claude_session = $1`, sess).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAHookReportIsStoredForTheTokensOwnerAndRoutesTheReview(t *testing.T) {
	f := newPRFixture(t)
	if code, body := f.report(t, "", reportBody(createReport(routeSID))); code != 201 || body != "" {
		t.Fatalf("first report: %d %q; want 201 and no body", code, body)
	}
	if code, body := f.report(t, "", reportBody(createReport(routeSID))); code != 200 || body != "" {
		t.Fatalf("the same report again: %d %q; want 200 and no body", code, body)
	}
	got, ok, err := f.plugins.ResolvePRSession(f.asOwner, prroute.Ref{Repo: prRepo, PR: prNumber, HeadSHA: prSHA})
	if err != nil || !ok || got.ClaudeSession != routeSID || got.Host != "tower" || got.Kind != prroute.KindPRCreate {
		t.Fatalf("stored: %+v, %v, %v; want the owner's row, host from the online window", got, ok, err)
	}
	var owner int64
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT owner_id FROM session_prs WHERE claude_session = $1`, routeSID).Scan(&owner); err != nil || owner != f.owner {
		t.Fatalf("row owner %d (%v); want the token's owner %d", owner, err, f.owner)
	}
	// End to end: the review of that pull request now goes to that session.
	m := f.review(t, "pr-hook-e2e-1", prSender, true, reviewBody(fullFields()))
	if rs := f.routesOf(t, m); len(rs) != 1 || rs[0].TargetKey != routeSID || f.turns.count() != 0 {
		t.Fatalf("routes %+v, %d chief turns; want one route to the reporting session", rs, f.turns.count())
	}
}

// 🔴 Stored with no window (unlike a thread bind's 409): the offline path
// names this session to the chief, which a window check would have dropped.
func TestAReportForASessionNoWindowShowsIsStoredAndNamedOffline(t *testing.T) {
	f := newPRFixture(t)
	const offline = "5e55a1d0-0000-4000-8000-0000000d0d0d"
	if code, _ := f.report(t, "", reportBody(createReport(offline))); code != 201 {
		t.Fatalf("an offline session's report: %d, want 201", code)
	}
	got, ok, _ := f.plugins.ResolvePRSession(f.asOwner, prroute.Ref{Repo: prRepo, PR: prNumber, HeadSHA: prSHA})
	if !ok || got.ClaudeSession != offline || got.Host != "" {
		t.Fatalf("stored %+v, %v; want the session with no host", got, ok)
	}
	f.review(t, "pr-hook-offline-1", prSender, true, reviewBody(fullFields()))
	if f.turns.count() != 1 || !strings.Contains(f.lastTurn(), "| mirror:"+offline+" (host unknown") || !strings.Contains(f.lastTurn(), "window: offline") {
		t.Fatalf("%d chief turns; the turn does not name the offline author:\n%s", f.turns.count(), f.lastTurn())
	}
}

func TestAPushReportNamesItsBranch(t *testing.T) {
	f := newPRFixture(t)
	body := reportBody(map[string]any{"repo": prRepo, "pr": nil, "head_ref": prBranch, "claude_session": routeSID, "kind": "git-push"})
	if code, _ := f.report(t, "", body); code != 201 {
		t.Fatalf("push report: %d", code)
	}
	if got, ok, _ := f.plugins.ResolvePRSession(f.asOwner, prroute.Ref{Repo: prRepo, PR: prNumber, HeadSHA: prSHA, HeadRef: prBranch}); !ok || got.Kind != prroute.KindPush {
		t.Fatalf("stored %+v, %v", got, ok)
	}
}

func TestAMalformedReportIsRefusedAndStoresNothing(t *testing.T) {
	f := newPRFixture(t)
	with := func(k string, v any) string {
		m := createReport(routeSID)
		if v == "<drop>" {
			delete(m, k)
		} else {
			m[k] = v
		}
		return reportBody(m)
	}
	// Control: the base report is accepted, so every refusal below is the field's.
	if code, _ := f.report(t, "", with("kind", "gh-pr-create")); code != 201 {
		t.Fatalf("control: %d", code)
	}
	dbtest.OwnerExec(t, dbtest.DSN(t), `DELETE FROM session_prs WHERE claude_session = $1`, routeSID)
	cases := map[string]string{
		"not json":                    "{",
		"repo with a space":           with("repo", "acme labs/widget-api"),
		"repo with three parts":       with("repo", "acme-labs/widget-api/x"),
		"no repo":                     with("repo", "<drop>"),
		"pr zero":                     with("pr", 0),
		"pr negative":                 with("pr", -4817),
		"pr as a string":              with("pr", "4817"),
		"pr fractional":               with("pr", 4817.5),
		"create with no pr":           with("pr", nil),
		"a push with a pr":            with("kind", "git-push"),
		"an unknown kind":             with("kind", "git-fetch"),
		"no kind":                     with("kind", "<drop>"),
		"head_ref empty":              with("head_ref", ""),
		"head_ref with a space":       with("head_ref", "feat/gizmo route"),
		"head_ref with ..":            with("head_ref", "feat/../main"),
		"no session":                  with("claude_session", "<drop>"),
		"a session with a slash":      with("claude_session", "../../etc"),
		"a runtime-qualified session": with("claude_session", "hermes:20000101_000000_abcdef"),
	}
	for name, body := range cases {
		if code, _ := f.report(t, "", body); code != 400 {
			t.Errorf("%s: %d, want 400", name, code)
		}
	}
	push := reportBody(map[string]any{"repo": prRepo, "head_ref": nil, "claude_session": routeSID, "kind": "git-push"})
	if code, _ := f.report(t, "", push); code != 400 {
		t.Errorf("a push with no head_ref: %d, want 400", code)
	}
	if n := f.rowsFor(t, routeSID); n != 0 {
		t.Fatalf("%d rows after refused reports", n)
	}
}

func TestTheReportRouteRefusesAnyOtherCredential(t *testing.T) {
	f := newPRFixture(t)
	body := reportBody(createReport(routeSID))
	if code, _ := f.report(t, "mst_not-a-real-token-at-all-000000000000", body); code != 401 {
		t.Fatalf("a bad token: %d, want 401", code)
	}
	if code, _ := f.report(t, f.mailTok, body); code != 401 {
		t.Fatalf("a plugin token: %d, want 401", code)
	}
	if n := f.rowsFor(t, routeSID); n != 0 {
		t.Fatalf("%d rows after refused credentials", n)
	}
	// Positive control: the owner's own token is accepted.
	if code, _ := f.report(t, "", body); code != 201 {
		t.Fatalf("control: %d", code)
	}
}

func TestTheReportRouteIsOffWithPRRoutingOff(t *testing.T) {
	for _, off := range []string{"no senders", "routing off"} {
		t.Run(off, func(t *testing.T) {
			f := newPRFixture(t)
			if off == "no senders" {
				f.srv.ext.PRMail = &PRMail{}
			} else {
				f.srv.ext.Routing = nil
			}
			if code, _ := f.report(t, "", reportBody(createReport(routeSID))); code != 404 || f.rowsFor(t, routeSID) != 0 {
				t.Fatalf("%s: %d, %d rows; want 404 and none", off, code, f.rowsFor(t, routeSID))
			}
		})
	}
}

func TestAnotherOwnersReportIsInvisible(t *testing.T) {
	f := newPRFixture(t)
	other := f.newMember(t)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM session_prs WHERE owner_id = $1`, other)
	})
	if _, err := f.plugins.RecordPRSession(f.as(other), routeSID, "tower", prCreated()); err != nil {
		t.Fatal(err)
	}
	if got, ok, _ := f.plugins.ResolvePRSession(f.asOwner, prroute.Ref{Repo: prRepo, PR: prNumber, HeadSHA: prSHA}); ok {
		t.Fatalf("the owner resolved another owner's report: %+v", got)
	}
	m := f.review(t, "pr-hook-other-1", prSender, true, reviewBody(fullFields()))
	if rs := f.routesOf(t, m); len(rs) != 0 || f.turns.count() != 1 {
		t.Fatalf("%d routes, %d chief turns; want 0 and 1", len(rs), f.turns.count())
	}
}
