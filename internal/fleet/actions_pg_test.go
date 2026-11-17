package fleet

import (
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// The fleet's action log on a real Postgres, AS muster_app (db.Connect's pool
// sets that role on every connection): append-only by trigger, one open
// action per session and class, the owner scope, one audit row per change with
// ids only, and the batched push's claim. Every id and text is invented.

func pgActions(t *testing.T) *PG {
	t.Helper()
	p := pgFleet(t)
	dbtest.OwnerExec(t, dbtest.DSN(t), `TRUNCATE fleet_actions`) // no row trigger: the log refuses a DELETE
	dbtest.OwnerExec(t, dbtest.DSN(t), `DELETE FROM audit_log WHERE object LIKE 'fleet_action:%'`)
	return p
}

// proposal is a distinct proposal on session key in episode ep.
func proposal(key string, ep int64, class, text string) Proposal {
	return Proposal{Class: class, Proposer: ProposerChief, SubjectKind: "mirror", SubjectKey: key, Host: "rig-north",
		TmuxSession: "work", Window: "@31", EpisodeID: ep, StreakKey: "since:4242", Reason: "it offered to go on",
		Text: text, Evidence: Evidence{SubjectID: "mirror:" + key, Label: "parser rework"}}
}

// TestTheActionLogIsAppendOnlyAsTheAppRole: as muster_app (positive control:
// the pool's current_user IS the app role), a DELETE is refused, an UPDATE of
// an action's own fields (its text, its session) is refused, a finished
// action cannot change at all — and its progress (state, outcome) may move
// while it is open.
func TestTheActionLogIsAppendOnlyAsTheAppRole(t *testing.T) {
	p := pgActions(t)
	ctx := asUser(2)
	var who string
	if err := p.pool.QueryRow(ctx, `SELECT current_user::text`).Scan(&who); err != nil || who != db.AppRole {
		t.Fatalf("instrument: the pool runs as %q (%v), want %q", who, err, db.AppRole)
	}
	a, err := p.Propose(ctx, proposal("a11ce5e5-0001", 9001, ClassContinue, ContinueText))
	if err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string]string{
		`DELETE FROM fleet_actions WHERE id = $1`:                          "cannot be deleted",
		`UPDATE fleet_actions SET text = 'something else' WHERE id = $1`:   "only an action's progress may change",
		`UPDATE fleet_actions SET subject_key = 'other' WHERE id = $1`:     "only an action's progress may change",
		`UPDATE fleet_actions SET evidence = '{"x":1}' WHERE id = $1`:      "only an action's progress may change",
		`UPDATE fleet_actions SET created_at = created_at - interval '1 day' WHERE id = $1`: "only an action's progress may change",
	} {
		_, err := p.pool.Exec(ctx, q, a.ID)
		if err == nil || !strings.Contains(err.Error(), "append-only") || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want the append-only refusal (%s)", q, err, want)
		}
	}
	// Its progress moves (positive control for the refusals above).
	moved, ok, err := p.MoveAction(ctx, a.ID, []string{StateProposed}, Move{State: StateRejected, DecidedVia: ViaWeb, DecidedBy: 2, Finish: true})
	if err != nil || !ok || moved.State != StateRejected || moved.FinishedAt.IsZero() {
		t.Fatalf("reject: %+v %v %v", moved, ok, err)
	}
	if _, err := p.pool.Exec(ctx, `UPDATE fleet_actions SET outcome = 'changed later' WHERE id = $1`, a.ID); err == nil ||
		!strings.Contains(err.Error(), "a finished action cannot change") {
		t.Errorf("a finished action's outcome moved: %v", err)
	}
	// A move from a state it is not in moves nothing.
	if _, ok, err := p.MoveAction(ctx, a.ID, []string{StateProposed}, Move{State: StateApproved}); err != nil || ok {
		t.Errorf("a decided action moved again: %v %v", ok, err)
	}
}

// TestOneOpenActionPerSessionAndClass: a second proposal of the same class on
// the same session while one is open is ErrAlreadyProposed; another class,
// another session, or the same class after the first finished, are not.
func TestOneOpenActionPerSessionAndClass(t *testing.T) {
	p := pgActions(t)
	ctx := asUser(2)
	first, err := p.Propose(ctx, proposal("a11ce5e5-0002", 9002, ClassContinue, ContinueText))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Propose(ctx, proposal("a11ce5e5-0002", 9002, ClassContinue, ContinueText)); err != ErrAlreadyProposed {
		t.Fatalf("the second continue: %v, want ErrAlreadyProposed", err)
	}
	if _, err := p.Propose(ctx, proposal("a11ce5e5-0002", 9002, ClassStopNote, StopNoteText)); err != nil {
		t.Fatalf("another class on the same session: %v", err)
	}
	if _, err := p.Propose(ctx, proposal("a11ce5e5-0003", 9003, ClassContinue, ContinueText)); err != nil {
		t.Fatalf("the same class on another session: %v", err)
	}
	if _, err := p.Propose(asUser(3), proposal("a11ce5e5-0002", 9002, ClassContinue, ContinueText)); err != nil {
		t.Fatalf("another owner's session of the same key: %v", err)
	}
	if _, _, err := p.MoveAction(ctx, first.ID, []string{StateProposed}, Move{State: StateExpired, Finish: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Propose(ctx, proposal("a11ce5e5-0002", 9002, ClassContinue, ContinueText)); err != nil {
		t.Fatalf("after the first finished: %v", err)
	}
	if ok, err := p.ProposedOn(ctx, 9002, ClassContinue); err != nil || !ok {
		t.Fatalf("ProposedOn the episode: %v %v", ok, err)
	}
	if ok, err := p.ProposedOn(ctx, 9002, ClassHandoff); err != nil || ok {
		t.Fatalf("ProposedOn another class: %v %v", ok, err)
	}
}

// TestAnActionIsItsOwnersAlone: another owner reads none of it and moves none
// of it (forced row-level security).
func TestAnActionIsItsOwnersAlone(t *testing.T) {
	p := pgActions(t)
	a, err := p.Propose(asUser(2), proposal("a11ce5e5-0004", 9004, ClassHandoff, HandoffText(77, PRMerged)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.GetAction(asUser(3), a.ID); err != ErrActionNotFound {
		t.Fatalf("another owner's read: %v, want ErrActionNotFound", err)
	}
	if _, ok, err := p.MoveAction(asUser(3), a.ID, []string{StateProposed}, Move{State: StateApproved, DecidedVia: ViaWeb}); err != nil || ok {
		t.Fatalf("another owner's move: %v %v", ok, err)
	}
	if got, err := p.GetAction(asUser(2), a.ID); err != nil || got.State != StateProposed || got.Text != "Your PR #77 is merged and nothing is in flight. Write your handoff (what is done, what is left, where), then stop." {
		t.Fatalf("the owner's own: %+v %v", got, err)
	}
}

// TestEveryStateChangeWritesOneAuditRowWithIdsOnly: the proposal and each
// move write exactly one fleet.<class>.<state> row; none carries the text.
func TestEveryStateChangeWritesOneAuditRowWithIdsOnly(t *testing.T) {
	p := pgActions(t)
	ctx := asUser(2)
	const text = "Please rebase onto the release branch quickly-quokka."
	a, err := p.Propose(ctx, proposal("a11ce5e5-0005", 9005, ClassCustom, text))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.MoveAction(ctx, a.ID, []string{StateProposed}, Move{State: StateApproved, DecidedVia: ViaWeb, DecidedBy: 2}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.MoveAction(ctx, a.ID, []string{StateApproved}, Move{State: Suppressed(GuardMovedOn), Outcome: "moved", Finish: true}); err != nil {
		t.Fatal(err)
	}
	rows, err := p.pool.Query(asUser(2), `SELECT action, detail::text FROM audit_log WHERE object = $1 ORDER BY id`, ActionObject(a.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var action, detail string
		if err := rows.Scan(&action, &detail); err != nil {
			t.Fatal(err)
		}
		got = append(got, action)
		if strings.Contains(detail, "quokka") || strings.Contains(detail, "rebase") {
			t.Errorf("%s: the audit detail carries the text: %s", action, detail)
		}
	}
	want := []string{"fleet.custom.proposed", "fleet.custom.approved", "fleet.custom.suppressed_moved_on"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("audit rows %v, want %v", got, want)
	}
}

// TestTheBatchedPushIsClaimedAtMostEveryThirtyMinutesAndOnlyForNewProposals:
// no proposal → none; a proposal → one; again within 30 min → none; after 30
// min with nothing new → none; a new proposal after it → one.
func TestTheBatchedPushIsClaimedAtMostEveryThirtyMinutesAndOnlyForNewProposals(t *testing.T) {
	p := pgActions(t)
	ctx := asUser(3)
	t0 := time.Now().Add(time.Hour)
	if _, due, err := p.ClaimPush(ctx, t0); err != nil || due {
		t.Fatalf("no proposals: due %v %v", due, err)
	}
	if _, err := p.Propose(ctx, proposal("a11ce5e5-0006", 9006, ClassContinue, ContinueText)); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Propose(ctx, proposal("a11ce5e5-0007", 9007, ClassStopNote, StopNoteText)); err != nil {
		t.Fatal(err)
	}
	if n, due, err := p.ClaimPush(ctx, t0); err != nil || !due || n != 2 {
		t.Fatalf("two new proposals: %d %v %v; want 2, due", n, due, err)
	}
	if _, due, err := p.ClaimPush(ctx, t0.Add(29*time.Minute)); err != nil || due {
		t.Fatalf("29 minutes later: due %v %v", due, err)
	}
	// 31 minutes on, nothing new since the push (t0 is an hour ahead of every
	// created_at): not due.
	if _, due, err := p.ClaimPush(ctx, t0.Add(31*time.Minute)); err != nil || due {
		t.Fatalf("31 minutes later, nothing new: due %v %v", due, err)
	}
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE fleet_owner_marks SET proposals_pushed_at = now() - interval '2 hours' WHERE owner_id = 3`)
	if n, due, err := p.ClaimPush(ctx, time.Now()); err != nil || !due || n != 2 {
		t.Fatalf("proposals newer than the last push, 30 min on: %d %v %v; want 2, due", n, due, err)
	}
}
