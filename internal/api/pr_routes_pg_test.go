package api

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/mirror"
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
	if err := f.plugins.RecordPRSessions(ctx, sess, "tower", []prroute.Signal{sig}, true); err != nil {
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

// --- the scan -----------------------------------------------------------------

// scanMirror is routeMirror whose window's transcript stream serves a
// backfill and, on a resume at its end, the events after it.
type scanMirror struct {
	*routeMirror
	mu sync.Mutex
	// gen names the stream's conversation (routeSID unless set).
	gen string
	// backfill and next are the events before and after offset 100.
	backfill, next []cctranscript.Event
	// asked is every (gen, offset) a watch asked for.
	asked []string
}

func (m *scanMirror) WatchWindow(ctx context.Context, host, session, window, gen string, offset int64, out func(mirror.Msg) error) error {
	m.mu.Lock()
	m.asked = append(m.asked, gen+"@"+strconv.FormatInt(offset, 10))
	g := m.gen
	if g == "" {
		g = routeSID
	}
	backfill, next := m.backfill, m.next
	m.mu.Unlock()
	var msgs []mirror.Msg
	switch {
	case gen == g && offset == 100:
		msgs = []mirror.Msg{{Gen: g, Offset: 100}, {Gen: g, Offset: 250, Events: next}}
	case gen == g && offset == 250:
		msgs = []mirror.Msg{{Gen: g, Offset: 250}}
	default:
		msgs = []mirror.Msg{{Gen: g, Reset: true, Offset: 100, Events: backfill}, {Gen: g, Offset: 100, PromptIndex: true}}
	}
	for _, msg := range msgs {
		if err := out(msg); err != nil {
			return err
		}
	}
	<-ctx.Done()
	return nil
}

func scanBash(id, command string) cctranscript.Event {
	return cctranscript.Event{Kind: cctranscript.KindToolCall, ToolID: id, Tool: "Bash", Target: command,
		Input: `{"command":` + strconv.Quote(command) + `}`}
}

func scanResult(id, output string) cctranscript.Event {
	return cctranscript.Event{Kind: cctranscript.KindToolResult, ToolID: id, OK: true, Output: output}
}

func TestTheScanRecordsWhatASessionAuthoredAndResumesWhereItStopped(t *testing.T) {
	f := newPRFixture(t)
	sm := &scanMirror{routeMirror: f.mirror,
		backfill: []cctranscript.Event{
			scanBash("toolu_s1view", "gh pr view 7001"), scanResult("toolu_s1view", "https://github.com/acme-labs/widget-api/pull/7001"),
			scanBash("toolu_s1create", "gh pr create --fill"), scanResult("toolu_s1create", "https://github.com/"+prRepo+"/pull/"+strconv.Itoa(prNumber)+"\n"),
		},
		next: []cctranscript.Event{
			scanBash("toolu_s2push", "git push -u origin "+prBranch),
			scanResult("toolu_s2push", "To github.com:"+prRepo+".git\n * [new branch]      "+prBranch+" -> "+prBranch+"\n"),
		}}
	f.srv.ext.Mirror = sm
	clock := time.Now()
	f.srv.prScan.now = func() time.Time { return clock }

	f.srv.prScanOwner(f.asOwner, f.owner)
	got, ok, err := f.plugins.ResolvePRSession(f.asOwner, prroute.Ref{Repo: prRepo, PR: prNumber, HeadSHA: prSHA})
	if err != nil || !ok || got.ClaudeSession != routeSID || got.Host != "tower" || got.Kind != prroute.KindPRCreate {
		t.Fatalf("after the first scan: %+v, %v, %v", got, ok, err)
	}
	if _, ok, _ := f.plugins.ResolvePRSession(f.asOwner, prroute.Ref{Repo: prRepo, PR: 7001, HeadSHA: prSHA}); ok {
		t.Fatal("a gh pr view was recorded as authorship")
	}
	// The next scan resumes at the backfill's end and reads the push after it.
	clock = clock.Add(time.Hour)
	f.srv.prScanOwner(f.asOwner, f.owner)
	got, ok, err = f.plugins.ResolvePRSession(f.asOwner, prroute.Ref{Repo: prRepo, PR: 1, HeadSHA: prSHA, HeadRef: prBranch})
	if err != nil || !ok || got.Kind != prroute.KindPush {
		t.Fatalf("after the resumed scan: %+v, %v, %v", got, ok, err)
	}
	sm.mu.Lock()
	asked := append([]string(nil), sm.asked...)
	sm.mu.Unlock()
	if len(asked) != 2 || asked[0] != "@0" || asked[1] != routeSID+"@100" {
		t.Fatalf("watches asked %q; want a fresh one, then a resume at the first one's end", asked)
	}
}

func TestTheScanStoresNothingFromAnotherConversation(t *testing.T) {
	f := newPRFixture(t)
	sm := &scanMirror{routeMirror: f.mirror, gen: "5e55a1d0-0000-4000-8000-00000000d1ff",
		backfill: []cctranscript.Event{scanBash("toolu_s3create", "gh pr create --fill"),
			scanResult("toolu_s3create", "https://github.com/"+prRepo+"/pull/"+strconv.Itoa(prNumber))}}
	f.srv.ext.Mirror = sm
	f.srv.prScanOwner(f.asOwner, f.owner)
	if got, ok, _ := f.plugins.ResolvePRSession(f.asOwner, prroute.Ref{Repo: prRepo, PR: prNumber, HeadSHA: prSHA}); ok {
		t.Fatalf("a stream of another conversation was recorded as %s's: %+v", routeSID, got)
	}
	// Control: the same stream naming the window's conversation is recorded.
	sm.mu.Lock()
	sm.gen = ""
	sm.mu.Unlock()
	f.srv.prScan.now = func() time.Time { return time.Now().Add(time.Hour) }
	f.srv.prScanOwner(f.asOwner, f.owner)
	if _, ok, _ := f.plugins.ResolvePRSession(f.asOwner, prroute.Ref{Repo: prRepo, PR: prNumber, HeadSHA: prSHA}); !ok {
		t.Fatal("control: the window's own conversation was not recorded")
	}
}
