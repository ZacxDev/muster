package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/users"
	"github.com/jackc/pgx/v5"
)

// Team sharing slice 4.3, audit round 1: who may retract a comment on a team
// task, the role gate's read errors, and the move handler's owner check.

// addTeamPerson adds a person to tw's team at role (cleaned up with the walk).
func addTeamPerson(t *testing.T, tw *teamTaskWalk, name string, team int64, role authz.TeamRole) isoUser {
	t.Helper()
	w := tw.w
	var id int64
	if err := w.pool.QueryRow(w.sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, name+"-"+w.suffix).Scan(&id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		_, _ = w.pool.Exec(bg, `DELETE FROM team_members WHERE user_id = $1`, id)
		_, _ = w.pool.Exec(bg, `DELETE FROM notes WHERE owner_id = $1`, id)
		_, _ = w.pool.Exec(bg, `DELETE FROM users WHERE id = $1`, id)
	})
	by := users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}
	if err := users.NewPG(w.pool).SetTeamMember(w.sys, by, team, id, role); err != nil {
		t.Fatal(err)
	}
	return isoUser{name: name, uid: id}
}

// TestATeamTaskCommentIsRetractedOnlyByItsAuthorOrAManager: on a team task,
// operator B cannot retract operator O's comment (403, the comment stays);
// O can; manager A can retract another of O's. A comment with no author_id
// (written under the system scope, or before 0127) only the manager may
// retract. CONTROL: on B's own personal task B retracts an authorless
// comment, as before team tasks.
func TestATeamTaskCommentIsRetractedOnlyByItsAuthorOrAManager(t *testing.T) {
	tw := newTeamTaskWalk(t)
	w := tw.w
	o := addTeamPerson(t, tw, "tt-operator2", tw.team, authz.TeamOperator)
	id := strconv.FormatInt(tw.task, 10)

	comment := func(uid int64, body string) int64 {
		t.Helper()
		if rec := tw.post(t, uid, "POST", "/tasks/"+id+"/comments", url.Values{"body": {body}}.Encode()); !is2xx(rec.Code) {
			t.Fatalf("commenting %q: %d %s", body, rec.Code, truncate(rec.Body.String(), 200))
		}
		var cid, author int64
		if err := w.pool.QueryRow(w.sys, `SELECT id, author_id FROM note_comments WHERE note_id = $1 AND body = $2`, tw.task, body).Scan(&cid, &author); err != nil || author != uid {
			t.Fatalf("comment %q: author_id %d, %v; want %d", body, author, err, uid)
		}
		return cid
	}
	retracted := func(cid int64) bool {
		t.Helper()
		var gone bool
		if err := w.pool.QueryRow(w.sys, `SELECT deleted_at IS NOT NULL FROM note_comments WHERE id = $1`, cid).Scan(&gone); err != nil {
			t.Fatal(err)
		}
		return gone
	}
	retract := func(uid int64, task string, cid int64) int {
		return tw.post(t, uid, "DELETE", "/tasks/"+task+"/comments/"+strconv.FormatInt(cid, 10), "").Code
	}
	legacy := func(task int64, owner int64) int64 {
		t.Helper()
		var cid int64
		var author *int64
		if err := w.pool.QueryRow(w.sys, `INSERT INTO note_comments (note_id, owner_id, author, body) VALUES ($1, $2, 'user', 'legacy')
			RETURNING id, author_id`, task, owner).Scan(&cid, &author); err != nil || author != nil {
			t.Fatalf("legacy comment: author_id %v, %v; want NULL", author, err)
		}
		return cid
	}

	oFirst, oSecond := comment(o.uid, "by O one "+tw.canary), comment(o.uid, "by O two "+tw.canary)
	if code := retract(w.b.uid, id, oFirst); code != http.StatusForbidden || retracted(oFirst) {
		t.Errorf("B (operator) retracting O's comment on the team task: %d, retracted %v; want 403 and the comment kept", code, retracted(oFirst))
	}
	if code := retract(o.uid, id, oFirst); !is2xx(code) || !retracted(oFirst) {
		t.Errorf("O (operator) retracting O's own comment: %d, retracted %v; want 2xx", code, retracted(oFirst))
	}
	if code := retract(w.a.uid, id, oSecond); !is2xx(code) || !retracted(oSecond) {
		t.Errorf("A (manager) retracting O's comment: %d, retracted %v; want 2xx", code, retracted(oSecond))
	}

	old := legacy(tw.task, tw.team)
	if code := retract(w.b.uid, id, old); code != http.StatusForbidden || retracted(old) {
		t.Errorf("B (operator) retracting an authorless comment on the team task: %d, retracted %v; want 403", code, retracted(old))
	}
	if code := retract(w.a.uid, id, old); !is2xx(code) || !retracted(old) {
		t.Errorf("A (manager) retracting an authorless comment on the team task: %d, retracted %v; want 2xx", code, retracted(old))
	}

	// CONTROL: a personal task is unchanged — its owner retracts any comment.
	var mine int64
	if err := w.pool.QueryRow(w.sys, `INSERT INTO notes (owner_id, title) VALUES ($1, 'b own') RETURNING id`, w.b.uid).Scan(&mine); err != nil {
		t.Fatal(err)
	}
	own := legacy(mine, w.b.uid)
	if code := retract(w.b.uid, strconv.FormatInt(mine, 10), own); !is2xx(code) || !retracted(own) {
		t.Errorf("control: B retracting an authorless comment on B's own task: %d, retracted %v; want 2xx", code, retracted(own))
	}
}

// TestTheStoreRefusesRetractingAnotherMembersTeamComment is the store's
// backstop under the handler: SoftDeleteComment as operator B on operator O's
// team-task comment is pgx.ErrNoRows and stamps nothing; as O, and as manager
// A, it retracts.
func TestTheStoreRefusesRetractingAnotherMembersTeamComment(t *testing.T) {
	tw := newTeamTaskWalk(t)
	w := tw.w
	o := addTeamPerson(t, tw, "tt-operator2", tw.team, authz.TeamOperator)
	as := func(uid int64) context.Context {
		acct, err := users.NewPG(w.pool).Account(w.sys, uid)
		if err != nil {
			t.Fatal(err)
		}
		return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: acct.Role, Via: authz.ViaSession, Teams: acct.Teams})
	}
	store := notes.NewPG(w.pool)
	c1, err := store.AddComment(as(o.uid), notes.Comment{NoteID: tw.task, Author: "user", Body: "store one"})
	if err != nil {
		t.Fatal(err)
	}
	c2, err := store.AddComment(as(o.uid), notes.Comment{NoteID: tw.task, Author: "user", Body: "store two"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SoftDeleteComment(as(w.b.uid), tw.task, c1.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("store: B (operator) retracting O's team comment: %v; want pgx.ErrNoRows", err)
	}
	if err := store.SoftDeleteComment(as(o.uid), tw.task, c1.ID); err != nil {
		t.Errorf("store: O retracting O's own team comment: %v", err)
	}
	if err := store.SoftDeleteComment(as(w.a.uid), tw.task, c2.ID); err != nil {
		t.Errorf("store: A (manager) retracting O's team comment: %v", err)
	}
}

// failingGet is the real notes store whose Get fails with an error that is
// not "not found" (a dropped connection, a timeout).
type failingGet struct{ notes.Store }

func (failingGet) Get(context.Context, int64) (notes.Note, error) {
	return notes.Note{}, errors.New("connection reset")
}

// TestTheTeamTaskRoleGateFailsClosedOnAReadError: when the role gate cannot
// read the task, operator B's dismiss of the team's task is a 500 and the
// task stays live. (Postgres admits a dismiss at operator, so a gate that
// passed the error through would let B dismiss it.)
func TestTheTeamTaskRoleGateFailsClosedOnAReadError(t *testing.T) {
	tw := newTeamTaskWalk(t)
	w := tw.w
	prev := w.srv.ext.Notes
	w.srv.ext.Notes = failingGet{prev}
	t.Cleanup(func() { w.srv.ext.Notes = prev })
	rec := tw.post(t, w.b.uid, "DELETE", "/tasks/"+strconv.FormatInt(tw.task, 10), "")
	var gone bool
	if err := w.pool.QueryRow(tw.sys, `SELECT deleted_at IS NOT NULL FROM notes WHERE id = $1`, tw.task).Scan(&gone); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError || gone {
		t.Errorf("B (operator) dismissing the team task while the gate's read fails: %d, dismissed %v; want 500 and the task kept", rec.Code, gone)
	}
}

// TestOnlyATasksOwnerMovesItEvenBetweenTeamsTheyOperate: B operates in team
// T and team U and can see T's task, but it is T's, not B's: moving it into
// U is the HANDLER's 403 ("only the task's owner …"), not the store's
// not-yours refusal (which answers 404), and the task stays T's.
func TestOnlyATasksOwnerMovesItEvenBetweenTeamsTheyOperate(t *testing.T) {
	tw := newTeamTaskWalk(t)
	w := tw.w
	by := users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}
	store := users.NewPG(w.pool)
	u, err := store.CreateTeam(w.sys, by, "second team "+w.suffix, w.a.uid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		_, _ = w.pool.Exec(bg, `DELETE FROM notes WHERE owner_id = $1`, u.ID)
		_, _ = w.pool.Exec(bg, `DELETE FROM team_members WHERE team_id = $1`, u.ID)
		_, _ = w.pool.Exec(bg, `DELETE FROM audit_log WHERE object = $1 OR team_id = $1::bigint`, users.TeamObject(u.ID))
		_, _ = w.pool.Exec(bg, `DELETE FROM users WHERE id = $1`, u.ID)
	})
	if err := store.SetTeamMember(w.sys, by, u.ID, w.b.uid, authz.TeamOperator); err != nil {
		t.Fatal(err)
	}
	rec := tw.post(t, w.b.uid, "POST", "/tasks/"+strconv.FormatInt(tw.task, 10)+"/move", url.Values{"team": {strconv.FormatInt(u.ID, 10)}}.Encode())
	var owner int64
	if err := w.pool.QueryRow(w.sys, `SELECT owner_id FROM notes WHERE id = $1`, tw.task).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "only the task's owner can move it into a team") || owner != tw.team {
		t.Errorf("B (operator of both teams) moving team T's task into team U: %d %q, owner %d; want the handler's 403 (only the task's owner …) and owner T %d",
			rec.Code, strings.TrimSpace(rec.Body.String()), owner, tw.team)
	}
}
