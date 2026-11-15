package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sendqueue"
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
//   - CONTROL: a row the team queued itself (queued_by = the team) is the
//     team's own: pasted.
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
	own := o.teamRow(t, "@32", "ops: the team's own", o.team)
	o.tick(t)
	if st, d := o.rowState(t, own); st != "sent" || !o.pastedBody("ops: the team's own") {
		t.Fatalf("CONTROL: the team's own row: %s %q, pasted %q", st, d, o.pasted())
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
	o.srv.cancelQueuedFor(context.Background(), o.ben, SendQueueCancelDisabled)
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
	t.Cleanup(func() { _, _ = w.pool.Exec(authz.System(context.Background()), `DELETE FROM send_queue WHERE owner_id = $1`, v.team) })
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
