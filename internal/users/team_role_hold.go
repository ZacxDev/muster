package users

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/authz"
)

// ---------------------------------------------------------------------------
// A MEMBER'S ROLE, HELD FOR ONE SEND (issue #724; team sharing slice 4.4).
//
// A member's message to a team's agent is re-checked on every send attempt
// (internal/api turnActingContext): a member removed — or made a viewer —
// while it waited is not sent. A plain read is not enough for that: the read
// and the runtime taking the text are two steps, and a removal committed
// BETWEEN them returned to the manager while the attempt that had just read
// "operator" went on to be typed into the team's session. Measured: 2/200
// under -race at af368efa, and every time when the attempt is parked between
// the two (TestARemovalThatReturnedIsNeverFollowedByTheMembersMessage).
//
// So the role is read under a SHARE lock on the TEAM'S users row, held until
// the caller releases it — when the runtime has taken the text or refused it.
// Every writer of team_members takes that row FOR UPDATE first (lockTeam:
// SetTeamMember, ReroleTeamMemberAsManager, AddTeamMemberByEmailAsManager,
// RemoveTeamMember, RemoveTeamMemberAsManager), so a membership change either
// commits BEFORE the read (and the attempt is dropped) or waits for the
// attempt to be accepted or refused (and was made while the member still held
// the role). A removal that has returned is never followed by the removed
// member's message.
//
// 🔴 THE TEAM ROW, NOT THE MEMBERSHIP ROW. A removal locks the team row and
// THEN deletes the membership row; a hold on the membership row would make it
// wait holding the team row FOR UPDATE, and any write the attempt makes
// before the runtime answers to a row whose foreign key names the team (an FK
// check is FOR KEY SHARE on the team row) would wait for the removal — which waits for the
// hold the attempt only lets go of after those writes. Postgres sees no cycle
// (the holding transaction is idle, not waiting), so it would hang, not fail.
// FOR SHARE on the team row is taken in lockTeam's order, and FOR KEY SHARE is
// compatible with it.
// ---------------------------------------------------------------------------

// HoldTeamRole reads person user's role in team under a share lock on team's
// row, and keeps the lock until release is called (idempotent, any
// goroutine). in is false when user is not a member, or team is not a team;
// the lock is then already released. On err nothing is held.
func (s *PGStore) HoldTeamRole(ctx context.Context, team, user int64) (role authz.TeamRole, in bool, release func(), err error) {
	ctx = sys(ctx)
	none := func() {}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", false, none, err
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			// The turn's ctx may already be over; the lock must still go.
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			_ = tx.Rollback(rctx)
		})
	}
	var id int64
	err = tx.QueryRow(ctx, `SELECT id FROM users WHERE id = $1 AND kind = 'team' FOR SHARE`, team).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		release()
		return "", false, none, nil
	}
	if err != nil {
		release()
		return "", false, none, err
	}
	var r string
	err = tx.QueryRow(ctx, `SELECT role FROM team_members WHERE team_id = $1 AND user_id = $2`, team, user).Scan(&r)
	if errors.Is(err, pgx.ErrNoRows) {
		release()
		return "", false, none, nil
	}
	if err != nil {
		release()
		return "", false, none, err
	}
	return authz.TeamRole(r), true, release, nil
}
