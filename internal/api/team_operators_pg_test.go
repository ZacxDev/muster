package api

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/users"
)

// TEAM SHARING 2b.4, OPERATORS (docs/design/team-sharing.md A, "Who can send,
// and the step-up"; E). Synthetic fixtures: team "ops-<n>", whose members are
// the operator user 1 (its first manager) and "ops-ben" (an operator); the
// team's host is the fake mirror's "ws" (fakeMirror.teamHost).
//
// The rows a test sends are written STRAIGHT INTO send_queue as the migration
// role (owner_id the team, queued_by ben), and ben is changed straight in the
// database: never through Enqueue's API, SetDisabledAs or removeMemberTx,
// whose cancels would end the rows before the delivery check is reached
// (E, "delivery checks queued_by").

// opsTeam is the fixture: the send-queue rig with real accounts, a team, its
// operator ben, and the team's host.
type opsTeam struct {
	*sqFixture
	store *users.PGStore
	team  int64
	ben   int64
	dsn   string
}

func newOpsTeam(t *testing.T) *opsTeam {
	t.Helper()
	o := &opsTeam{}
	o.sqFixture = &sqFixture{mirror: queueMirror()}
	o.connFixture = newConnFixtureWith(t, func(ext *Extensions, pool *pgxpool.Pool) {
		o.queue = sendqueue.NewPG(pool)
		o.store = users.NewPG(pool)
		ext.Notes, ext.SessionLiveness, ext.Provisioner, ext.Gateway = stubNotesStore{}, stubLiveness{}, newFakeTerminal(), &turnGateway{}
		ext.Mirror, ext.SendQueue, ext.Users = o.mirror, o.queue, o.store
	})
	o.dsn = dbtest.DSN(t)
	dbtest.OwnerExec(t, o.dsn, `TRUNCATE send_queue`)
	o.mirror.agentState = &mirror.AgentState{Status: mirror.StatusIdle, Since: time.Now().Add(-time.Hour).UnixMilli(),
		Observed: time.Now().Add(-time.Hour)}
	sys := authz.System(context.Background())
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := o.pool.QueryRow(sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, "ops-ben-"+suffix).Scan(&o.ben); err != nil {
		t.Fatal(err)
	}
	admin := users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}
	tm, err := o.store.CreateTeam(sys, admin, "ops-"+suffix, authz.OperatorUserID)
	if err != nil {
		t.Fatal(err)
	}
	o.team = tm.ID
	if err := o.store.SetTeamMember(sys, admin, o.team, o.ben, authz.TeamOperator); err != nil {
		t.Fatal(err)
	}
	o.mirror.teamHost = o.team
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		for _, q := range []string{`DELETE FROM send_queue WHERE owner_id = $1`, `DELETE FROM team_members WHERE team_id = $1`,
			`DELETE FROM audit_log WHERE team_id = $1 OR object = 'team:' || $1::text`, `DELETE FROM users WHERE id = $1`} {
			_, _ = o.pool.Exec(bg, q, o.team)
		}
		_, _ = o.pool.Exec(bg, `DELETE FROM team_members WHERE user_id = $1`, o.ben)
		_, _ = o.pool.Exec(bg, `DELETE FROM users WHERE id = $1`, o.ben)
	})
	return o
}

// teamRow writes a queued row into the team's window `window` of "ws"/"main",
// queued by queuedBy, straight into the table (the migration role: no
// trigger, no row-level security), signed in a minute ago. Its id.
func (o *opsTeam) teamRow(t *testing.T, window, body string, queuedBy int64) int64 {
	t.Helper()
	dbtest.OwnerExec(t, o.dsn, `INSERT INTO send_queue (owner_id, queued_by, mirror_host, mirror_session, mirror_window, body, stepup_at)
		VALUES ($1, $2, 'ws', 'main', $3, $4, now() - interval '1 minute')`, o.team, queuedBy, window, body)
	var id int64
	if err := o.pool.QueryRow(authz.System(context.Background()), `SELECT id FROM send_queue WHERE body = $1`, body).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// rowState is row id's state and detail, read under the system scope.
func (o *opsTeam) rowState(t *testing.T, id int64) (string, string) {
	t.Helper()
	var state, detail string
	if err := o.pool.QueryRow(authz.System(context.Background()), `SELECT state, detail FROM send_queue WHERE id = $1`, id).Scan(&state, &detail); err != nil {
		t.Fatal(err)
	}
	return state, detail
}

func (o *opsTeam) pastedBody(body string) bool {
	for _, p := range o.pasted() {
		if p == body {
			return true
		}
	}
	return false
}

// The delivery check's words, pinned literally (not read from the code under
// test).
const (
	opsNotOperator = "Not sent: the person who queued it is no longer an operator of the team that owns this session."
	opsLeftCancel  = "Cancelled: the person who queued it is no longer a member of the team."
	opsTeamQueued  = "Not sent: it was queued by the team itself, not by a member, and muster sends a team's queued message only for a member who is still its operator."
)

// TestATeamSendIsCheckedAgainstWhoQueuedIt: the dispatcher delivers a TEAM's
// row as the team, and checks the person who QUEUED it — fresh, at delivery.
//
//   - ben disabled straight in the database: not pasted, failed with the
//     queuer's refusal;
//   - ben's membership deleted straight in the database: the same;
//   - ben removed through the real removal (removeMemberTx): cancelled by the
//     CANCEL path, whose words differ, so the two guards are told apart;
//   - CONTROL: ben still an operator: pasted;
//   - a row the team queued itself (queued_by = the team): no writer makes
//     one yet, so it is refused, failing closed (round 0, D1).
//
// Red at base: the dispatcher panics at queuedPrincipal (ViaQueue is not a
// team door). Red on the half-applied mutant (ViaQueue admitted, the check
// still reading the row's OWNER, the team): ben's rows are pasted.
func TestATeamSendIsCheckedAgainstWhoQueuedIt(t *testing.T) {
	o := newOpsTeam(t)

	ok := o.teamRow(t, "@31", "ops: ben, still an operator", o.ben)
	o.tick(t)
	if st, d := o.rowState(t, ok); st != "sent" || !o.pastedBody("ops: ben, still an operator") {
		t.Fatalf("CONTROL: ben's row while he is an operator: %s %q, pasted %q", st, d, o.pasted())
	}
	// (1) disabled, in the database only.
	disabled := o.teamRow(t, "@33", "ops: ben, disabled", o.ben)
	dbtest.OwnerExec(t, o.dsn, `UPDATE users SET disabled_at = now() WHERE id = $1`, o.ben)
	o.tick(t)
	if st, d := o.rowState(t, disabled); st != "failed" || d != opsNotOperator || o.pastedBody("ops: ben, disabled") {
		t.Fatalf("a disabled member's team row: %s %q, pasted %q; want failed %q and nothing pasted", st, d, o.pasted(), opsNotOperator)
	}
	dbtest.OwnerExec(t, o.dsn, `UPDATE users SET disabled_at = NULL WHERE id = $1`, o.ben)

	// (2) removed, in the database only.
	removed := o.teamRow(t, "@34", "ops: ben, removed", o.ben)
	dbtest.OwnerExec(t, o.dsn, `DELETE FROM team_members WHERE team_id = $1 AND user_id = $2`, o.team, o.ben)
	o.tick(t)
	if st, d := o.rowState(t, removed); st != "failed" || d != opsNotOperator || o.pastedBody("ops: ben, removed") {
		t.Fatalf("a removed member's team row: %s %q, pasted %q; want failed %q and nothing pasted", st, d, o.pasted(), opsNotOperator)
	}

	// (3) through the real removal: the cancel path's own words.
	admin := users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}
	sys := authz.System(context.Background())
	if err := o.store.SetTeamMember(sys, admin, o.team, o.ben, authz.TeamOperator); err != nil {
		t.Fatal(err)
	}
	cancelled := o.teamRow(t, "@35", "ops: ben, removed by a manager", o.ben)
	if err := o.store.RemoveTeamMember(sys, admin, o.team, o.ben); err != nil {
		t.Fatal(err)
	}
	o.tick(t)
	if st, d := o.rowState(t, cancelled); st != "cancelled" || d != opsLeftCancel || o.pastedBody("ops: ben, removed by a manager") {
		t.Fatalf("a member removed through the removal: %s %q, pasted %q; want cancelled %q", st, d, o.pasted(), opsLeftCancel)
	}

	// A row the TEAM queued as itself (no person): refused, failing closed,
	// until a team-acting writer exists with its own check.
	own := o.teamRow(t, "@32", "ops: queued by the team itself", o.team)
	o.tick(t)
	if st, d := o.rowState(t, own); st != "failed" || d != opsTeamQueued || o.pastedBody("ops: queued by the team itself") {
		t.Fatalf("a row the team queued itself: %s %q, pasted %q; want failed %q and nothing pasted", st, d, o.pasted(), opsTeamQueued)
	}
}

// TestDisablingAMemberCancelsWhatTheyQueuedIntoTheirTeams: a disable's (and
// a sign-out everywhere's) cancel reaches the rows ben queued into his team's
// windows — rows the TEAM owns, which the owner's cancel never touched — and
// leaves another member's row on the same team queued (control). Red at base:
// ben's team row stays queued (only the delivery check would stop it).
func TestDisablingAMemberCancelsWhatTheyQueuedIntoTheirTeams(t *testing.T) {
	o := newOpsTeam(t)
	ben := o.teamRow(t, "@41", "ops: ben's, to be cancelled", o.ben)
	other := o.teamRow(t, "@42", "ops: the manager's, kept", authz.OperatorUserID)
	// ben's OWN row (his queue, not the team's): the same one call cancels it.
	dbtest.OwnerExec(t, o.dsn, `INSERT INTO send_queue (owner_id, queued_by, mirror_host, mirror_session, mirror_window, body, stepup_at)
		VALUES ($1, $1, 'ws', 'main', '@43', 'ops: ben''s own', now())`, o.ben)
	var personal int64
	if err := o.pool.QueryRow(authz.System(context.Background()), `SELECT id FROM send_queue WHERE body = 'ops: ben''s own'`).Scan(&personal); err != nil {
		t.Fatal(err)
	}
	o.srv.cancelQueuedFor(context.Background(), o.ben, SendQueueCancelDisabled)
	if st, d := o.rowState(t, personal); st != "cancelled" || d != "Cancelled: the account was disabled." {
		t.Fatalf("ben's OWN row after his disable's cancel: %s %q", st, d)
	}
	if st, d := o.rowState(t, ben); st != "cancelled" || d != "Cancelled: the account was disabled." {
		t.Fatalf("ben's team row after his disable's cancel: %s %q", st, d)
	}
	if st, _ := o.rowState(t, other); st != "queued" {
		t.Fatalf("CONTROL: another member's row on the team: %s; want queued", st)
	}
}

// TestAPhoneAnswersATeamAskAtOperator: the device answer route on a TEAM's
// question (one of a team the phone's owner is a member of). A viewer's
// signed answer is refused by name (403 not_operator) and the question stays
// open; made an operator, the same person answers it, and the answer records
// them. Red at base: the viewer's answer is answered as if the question were
// closed or missing (row-level security filters the write), never refused.
func TestAPhoneAnswersATeamAskAtOperator(t *testing.T) {
	a := newActionRig(t)
	sys := authz.System(context.Background())
	us := users.NewPG(a.pool)
	admin := users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}
	tm, err := us.CreateTeam(sys, admin, "ops-phone-"+strconv.FormatInt(time.Now().UnixNano(), 36), authz.OperatorUserID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		for _, q := range []string{`DELETE FROM attention_asks WHERE owner_id = $1`, `DELETE FROM team_members WHERE team_id = $1`,
			`DELETE FROM audit_log WHERE team_id = $1 OR object = 'team:' || $1::text`, `DELETE FROM users WHERE id = $1`} {
			_, _ = a.pool.Exec(bg, q, tm.ID)
		}
	})
	if err := us.SetTeamMember(sys, admin, tm.ID, a.uid, authz.TeamViewer); err != nil {
		t.Fatal(err)
	}
	asTeam := authz.WithPrincipal(context.Background(), authz.Principal{UserID: tm.ID, Team: true, Role: authz.RoleMember, Via: authz.ViaAttention})
	ask, err := a.attn.Create(asTeam, attention.New{Asker: attention.Asker{System: true, Label: attention.SystemAskerLabel},
		Question: "Roll the team's build forward?", Options: []string{"yes", "no"}})
	if err != nil {
		t.Fatal(err)
	}
	answer := "yes"
	signed := func() map[string]string {
		nonce, msg := a.challenge(t, a.tok, actionAskAnswer, ask.ID, &answer)
		return map[string]string{"nonce": nonce, "signature": sign(t, a.key, msg), "answer": answer}
	}
	if c, code := a.act(t, a.tok, "answer", ask.ID, signed()); c != http.StatusForbidden || code != "not_operator" {
		t.Fatalf("a viewer's phone answering the team's question: %d %q; want 403 not_operator", c, code)
	}
	if got, err := a.attn.Get(asTeam, ask.ID); err != nil || got.Status != attention.Open {
		t.Fatalf("the team's question after a viewer's answer: %+v %v; want open", got, err)
	}
	if err := us.SetTeamMember(sys, admin, tm.ID, a.uid, authz.TeamOperator); err != nil {
		t.Fatal(err)
	}
	if c, code := a.act(t, a.tok, "answer", ask.ID, signed()); c != http.StatusOK {
		t.Fatalf("an operator's phone answering the team's question: %d %q; want 200", c, code)
	}
	got, err := a.attn.Get(asTeam, ask.ID)
	if err != nil || got.Status != attention.Answered || got.Answer != answer || got.AnsweredByUser != a.uid {
		t.Fatalf("the team's question after the operator's answer: %+v %v; want answered %q by user %d", got, err, answer, a.uid)
	}
}

// TestOnlyItsQueuerOrAManagerChangesATeamSend: the queue routes on a TEAM
// window's rows (team_host_view's fixture: A manages the team, B operates
// it, cal views it).
//
//   - B cancels the row he queued, naming the team (owner=): done;
//   - B cannot cancel A's row (403), and A — a manager — cancels B's: done;
//   - A cannot EDIT B's row (403): a manager cancels, never edits;
//   - cal, a viewer, is refused at the gate (403);
//   - 🔴 a request that does NOT name the team reaches no team row by its id
//     (404), though the member's principal reads and — at operator —
//     writes the team's rows.
//
// Red at base: B's untargeted cancel of a team row succeeds; every targeted
// one is refused to everyone (typing was own-only).
func TestOnlyItsQueuerOrAManagerChangesATeamSend(t *testing.T) {
	v := newTeamView(t)
	w := v.w
	dsn := dbtest.DSN(t)
	row := func(body string, by int64) int64 {
		dbtest.OwnerExec(t, dsn, `INSERT INTO send_queue (owner_id, queued_by, mirror_host, mirror_session, mirror_window, body, stepup_at)
			VALUES ($1, $2, 'build-01', $3, '', $4, now())`, v.team, by, v.shared, body)
		var id int64
		if err := w.pool.QueryRow(w.sys, `SELECT id FROM send_queue WHERE body = $1`, body).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	t.Cleanup(func() {
		_, _ = w.pool.Exec(authz.System(context.Background()), `DELETE FROM send_queue WHERE owner_id = $1`, v.team)
	})
	state := func(id int64) string {
		var s string
		if err := w.pool.QueryRow(w.sys, `SELECT state FROM send_queue WHERE id = $1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	owner := "?owner=" + strconv.FormatInt(v.team, 10)
	post := func(path string, uid int64) (int, string) {
		rec := w.do(w.h, "POST", path, "", cred{sessionUser: uid})
		return rec.Code, rec.Body.String()
	}
	path := func(id int64, verb string) string { return "/send-queue/" + strconv.FormatInt(id, 10) + "/" + verb }

	bens, anas, bens2, bens3 := row("ops-view: B's", w.b.uid), row("ops-view: A's", w.a.uid), row("ops-view: B's second", w.b.uid), row("ops-view: B's third", w.b.uid)
	if c, body := post(path(bens, "cancel"), w.b.uid); c != http.StatusNotFound || state(bens) != "queued" {
		t.Fatalf("B's cancel of a team row NOT naming the team: %d %q, row %s; want 404 and still queued", c, truncate(body, 120), state(bens))
	}
	if c, body := post(path(bens, "cancel")+owner, w.b.uid); c != http.StatusNoContent || state(bens) != "cancelled" {
		t.Fatalf("B's cancel of his own team row: %d %q, row %s; want 204 cancelled", c, truncate(body, 120), state(bens))
	}
	if c, body := post(path(anas, "cancel")+owner, w.b.uid); c != http.StatusForbidden || !strings.Contains(body, SendQueueNotYours) || state(anas) != "queued" {
		t.Fatalf("B's cancel of A's team row: %d %q, row %s; want 403 and still queued", c, truncate(body, 120), state(anas))
	}
	if c, body := post(path(bens2, "cancel")+owner, w.a.uid); c != http.StatusNoContent || state(bens2) != "cancelled" {
		t.Fatalf("A's (manager) cancel of B's team row: %d %q, row %s; want 204 cancelled", c, truncate(body, 120), state(bens2))
	}
	if c, body := post(path(bens3, "edit")+owner, w.a.uid); c != http.StatusForbidden || !strings.Contains(body, SendQueueNotYours) {
		t.Fatalf("A's (manager) edit of B's team row: %d %q; want 403", c, truncate(body, 120))
	}
	if c, body := post(path(bens3, "cancel")+owner, v.cal); c != http.StatusForbidden || !strings.Contains(body, MirrorTeamViewerRefusal) || state(bens3) != "queued" {
		t.Fatalf("cal's (viewer) cancel of a team row: %d %q, row %s; want 403 and still queued", c, truncate(body, 120), state(bens3))
	}
}

// TestEveryQueueTargetNamesItsOwner: every function in internal/api that
// builds a sendqueue.Target (a queue's address) is listed with whose queue it
// addresses. A page's queue post (a queue, a Retry, a card's "Send as a
// message") builds it ONLY through queueTargetOf, which carries the team
// entry's owner: a literal elsewhere is the copy that drops it (round 1 on
// #702, F1: "Send as a message" queued a team card's answer into the
// member's own queue). Fails when the set grows or shrinks.
//
// 🔴 RED at c6df10bf: handleMirrorAskMessage built its own literal.
func TestEveryQueueTargetNamesItsOwner(t *testing.T) {
	want := map[string]string{
		"queueTargetOf":                "a mirrored entry's queue: its address and its owner (a team entry's is the team's)",
		"Server.sendQueueNotePaste":    "the window ctx's mirror calls touch (windowOwner)",
		"Server.forwardToHostDispatch": "a chief's forward, as the chief's owner (the caller's own)",
		"Server.queueNudgeText":        "a chief nudge, as the nudge's owner (the caller's own)",
		"Server.injectRoute":           "a routed message, as the route's owner (the caller's own)",
		"Server.askLogProvenance":      "the ask log's read of the caller's OWN sent bodies",
		"Server.attachFleetInFlight":   "the fleet reader's read of the caller's OWN windows (Mirror.Hosts; owner 0 = own)",
	}
	found := map[string]int{}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			name := fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) == 1 {
				rt := fn.Recv.List[0].Type
				if st, ok := rt.(*ast.StarExpr); ok {
					rt = st.X
				}
				if id, ok := rt.(*ast.Ident); ok {
					name = id.Name + "." + name
				}
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if cl, ok := n.(*ast.CompositeLit); ok {
					if sel, ok := cl.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Target" {
						if x, ok := sel.X.(*ast.Ident); ok && x.Name == "sendqueue" {
							found[name]++
						}
					}
				}
				return true
			})
		}
	}
	if len(found) < 3 {
		t.Fatalf("instrument check FAILED: the walk found %d builders", len(found))
	}
	ledgerMatches(t, "sendqueue.Target literals", found, want)
}

// TestATeamCardMessageQueuesIntoTheTeamsQueue: the target a team window's
// card builds (queueTargetOf, the one constructor) is the TEAM's, and a queue
// post through it, by an operator aimed at the team, lands with owner_id the
// team and queued_by the operator. CONTROL: the member's own entry (Owner 0)
// queues into the member's own.
func TestATeamCardMessageQueuesIntoTheTeamsQueue(t *testing.T) {
	o := newOpsTeam(t)
	m, err := authz.NewMemberships([]authz.Membership{{Team: o.team, Role: authz.TeamOperator}})
	if err != nil {
		t.Fatal(err)
	}
	ben := stepup.WithAuthTime(authz.WithPrincipal(context.Background(), authz.Principal{UserID: o.ben, Role: authz.RoleMember,
		Via: authz.ViaSession, Teams: m}), time.Now())
	teamHost := mirror.HostView{Owner: o.team, Host: "ws", Online: true, Interactive: true}
	e := mirror.SessionView{Owner: o.team, Host: "ws", Session: "main", Window: "@61"}
	q := sendQueueTarget{t: queueTargetOf(teamHost, e), h: teamHost, e: e, listed: true}
	it, err := o.srv.enqueueSend(mirror.ForOwner(ben, o.team), q, "ops: a card's answer, as a message")
	if err != nil {
		t.Fatal(err)
	}
	if it.Owner != o.team || it.QueuedBy != o.ben {
		t.Fatalf("a team card's message: owner %d queued_by %d; want the team %d, queued by ben %d", it.Owner, it.QueuedBy, o.team, o.ben)
	}
	own := queueTargetOf(mirror.HostView{Host: "ws"}, mirror.SessionView{Host: "ws", Session: "main", Window: "@62"})
	if own.Owner != 0 {
		t.Fatalf("CONTROL: an own entry's target names owner %d; want 0 (the caller's own)", own.Owner)
	}
}

// opsCutoff is the queuer's sign-in refusal, pinned literally.
const opsCutoff = "Not sent: the account that queued it was signed out of every session (a disable, a password reset or a sign-out everywhere) after it was queued. Queue it again."

// TestATeamRowIsCheckedAgainstItsQueuersSignIn: with accounts (GoTrue's
// user store wired), a TEAM row is refused when its QUEUER's sessions were
// cut off (users.sessions_not_before) or signed out everywhere
// (users.signed_out_at) after the sign-in that queued it — the cancel is
// bypassed (the cutoffs are written straight into the database), so only the
// delivery's account check of the queuer stands between the row and the
// pane. CONTROL: ben with no cutoff is pasted.
//
// Round 1 on #702 (F3): both mutants — the check reading the TEAM's account,
// and the check skipped — survived the suite before this test.
func TestATeamRowIsCheckedAgainstItsQueuersSignIn(t *testing.T) {
	o := newOpsTeam(t)
	o.srv.gt = &gotrueState{GoTrueAuth: GoTrueAuth{Users: o.store}, now: time.Now}

	ok := o.teamRow(t, "@51", "ops: ben, signed in", o.ben)
	o.tick(t)
	if st, d := o.rowState(t, ok); st != "sent" {
		t.Fatalf("CONTROL: ben's row with no cutoff: %s %q", st, d)
	}
	for _, c := range []struct{ name, column string }{
		{"signed out everywhere", "signed_out_at"},
		{"sessions cut off", "sessions_not_before"},
	} {
		body := "ops: ben, " + c.name
		id := o.teamRow(t, "@5"+strconv.Itoa(len(c.column)), body, o.ben)
		dbtest.OwnerExec(t, o.dsn, `UPDATE users SET `+c.column+` = now() WHERE id = $1`, o.ben)
		o.tick(t)
		if st, d := o.rowState(t, id); st != "failed" || d != opsCutoff || o.pastedBody(body) {
			t.Errorf("ben's team row, %s since he queued it: %s %q, pasted %q; want failed %q", c.name, st, d, o.pasted(), opsCutoff)
		}
		dbtest.OwnerExec(t, o.dsn, `UPDATE users SET `+c.column+` = NULL WHERE id = $1`, o.ben)
	}
}

// TestAPhoneDismissesATeamAskAtOperator: the device DISMISSAL route on a
// TEAM's question (round 1 on #702, F4). A viewer's signed dismissal is
// refused by name (403 not_operator) and the question stays open; an
// operator's dismisses it. A team's DISPATCH CARD is not dismissed from a
// phone even by an operator (TeamCardNotFromPhone) and stays open.
func TestAPhoneDismissesATeamAskAtOperator(t *testing.T) {
	a := newActionRig(t)
	sys := authz.System(context.Background())
	us := users.NewPG(a.pool)
	pl := plugins.NewPG(a.pool)
	a.s.ext.ChiefDispatch = pl
	admin := users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}
	tm, err := us.CreateTeam(sys, admin, "ops-dismiss-"+strconv.FormatInt(time.Now().UnixNano(), 36), authz.OperatorUserID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		for _, q := range []string{`DELETE FROM attention_asks WHERE owner_id = $1`, `DELETE FROM team_members WHERE team_id = $1`,
			`DELETE FROM audit_log WHERE user_id = $1 OR team_id = $1 OR object = 'team:' || $1::text`, `DELETE FROM users WHERE id = $1`} {
			_, _ = a.pool.Exec(bg, q, tm.ID)
		}
	})
	if err := us.SetTeamMember(sys, admin, tm.ID, a.uid, authz.TeamViewer); err != nil {
		t.Fatal(err)
	}
	asTeam := authz.WithPrincipal(context.Background(), authz.Principal{UserID: tm.ID, Team: true, Role: authz.RoleMember, Via: authz.ViaAttention})
	file := func(q string, card bool) int64 {
		ask, err := a.attn.Create(asTeam, attention.New{Asker: attention.Asker{System: true, Label: attention.SystemAskerLabel},
			Question: q, Options: []string{"yes", "no"}, OwnerCard: card})
		if err != nil {
			t.Fatal(err)
		}
		return ask.ID
	}
	open := func(id int64) bool {
		got, err := a.attn.Get(asTeam, id)
		return err == nil && got.Status == attention.Open
	}
	dismiss := func(id int64) (int, string) {
		nonce, msg := a.challenge(t, a.tok, actionAskDismiss, id, nil)
		return a.act(t, a.tok, "cancel", id, map[string]string{"nonce": nonce, "signature": sign(t, a.key, msg)})
	}

	plain := file("Skip the team's nightly build?", false)
	if c, code := dismiss(plain); c != 403 || code != "not_operator" || !open(plain) {
		t.Fatalf("a viewer's phone dismissing the team's question: %d %q, open %v; want 403 not_operator and still open", c, code, open(plain))
	}
	if err := us.SetTeamMember(sys, admin, tm.ID, a.uid, authz.TeamOperator); err != nil {
		t.Fatal(err)
	}
	card := file("Start an agent on the team's task?", true)
	if _, err := pl.FileDispatchHold(asTeam, map[string]any{"door": "card", "ask_id": card}); err != nil {
		t.Fatal(err)
	}
	if c, code := dismiss(card); c == 200 || !open(card) {
		t.Fatalf("an operator's phone dismissing the team's DISPATCH CARD: %d %q, open %v; want refused and still open", c, code, open(card))
	}
	if c, code := dismiss(plain); c != 200 || open(plain) {
		t.Fatalf("CONTROL: an operator's phone dismissing the team's question: %d %q, open %v; want 200 and closed", c, code, open(plain))
	}
}

// TestATeamConfirmPageComesBackToTheTeamsWindow: the plain confirm pages a
// browser with no script gets for a stale (or, here, a wrong-password)
// confirm of a TEAM window's send, queue post and Send now post back to the
// TEAM's window: their action names the team (owner=), which is what aims a
// request (mirrorTeamGate). Round 1 on #702 (F2): they named none, so the
// confirm resolved against the member's own hosts. CONTROL: B's confirm with
// no owner= is 404 — B has no build-01 of his own.
func TestATeamConfirmPageComesBackToTheTeamsWindow(t *testing.T) {
	v := newTeamViewWith(t, true)
	w := v.w
	team := strconv.FormatInt(v.team, 10)
	dbtest.OwnerExec(t, dbtest.DSN(t), `INSERT INTO send_queue (owner_id, queued_by, mirror_host, mirror_session, mirror_window, body, stepup_at)
		VALUES ($1, $2, 'build-01', $3, '', 'ops-confirm: B''s', now())`, v.team, w.b.uid, v.shared)
	t.Cleanup(func() {
		_, _ = w.pool.Exec(authz.System(context.Background()), `DELETE FROM send_queue WHERE owner_id = $1`, v.team)
	})
	var row int64
	if err := w.pool.QueryRow(w.sys, `SELECT id FROM send_queue WHERE body = 'ops-confirm: B''s'`).Scan(&row); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"host": {"build-01"}, "session": {v.shared}, "text": {"hello"}, "password": {"not the password"}}
	send := url.Values{"kind": {"view"}}
	for k, vs := range form {
		send[k] = vs
	}
	sendNow := "/send-queue/" + strconv.FormatInt(row, 10) + "/send-now/confirm"
	for _, c := range []struct{ name, path, body string }{
		{"a send", "/mirror/confirm-send", send.Encode()},
		{"a queue post", "/send-queue/confirm", form.Encode()},
		{"a Send now", sendNow, url.Values{"password": {"not the password"}}.Encode()},
	} {
		rec := w.do(w.h, "POST", c.path+"?owner="+team, c.body, cred{sessionUser: w.b.uid})
		want := `action="` + c.path + `?owner=` + team + `"`
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s: B's confirm page for the team's window: %d, carries %s: %v\n%s", c.name, rec.Code, want,
				strings.Contains(rec.Body.String(), want), truncate(rec.Body.String(), 300))
		}
	}
	if rec := w.do(w.h, "POST", "/mirror/confirm-send", send.Encode(), cred{sessionUser: w.b.uid}); rec.Code != 404 {
		t.Errorf("CONTROL: B's confirm naming no team: %d; want 404", rec.Code)
	}
}
