package users

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// TEAM HOST TOKENS: THE ROTATE MARK (team sharing phase 2b.2;
// docs/design/team-sharing.md, "A member leaves while a host stays").
//
// A team host's mirror token (mirror_tokens.owner_id = the team) is portable:
// a person who held it keeps it after they leave. Operator decision D-1113 #6
// is "mark rotate, never auto-revoke" — a revoke cuts every team session at
// once — so the team's unrevoked host tokens are MARKED (rotate_at, 0133) in
// the SAME transaction as each of the three changes that leave such a token
// in the wrong hands:
//
//   - an operator or a manager is removed (removeMemberTx);
//   - an operator or a manager is set to viewer (setMemberTx);
//   - an operator or a manager is DISABLED (SetDisabledAs, which does not
//     touch memberships, so it needs its own trigger).
//
// A viewer never held a token through the team (minting is a manager's), so
// a viewer's removal or disable marks nothing. Nothing clears the mark: a
// token stays marked until it is revoked. A later trigger moves rotate_at
// forward, so the page names the latest reason.
//
// There is NO reason column: the reason is DERIVED from the audit row the
// triggering transaction wrote beside the mark (TeamRotateReason).
// ---------------------------------------------------------------------------

// markTeamTokensRotateTx marks every unrevoked host token of team rotate,
// inside tx.
//
// And, since 0134 (team sharing 2b.3b), the team's unrevoked viewer PLUGINS:
// a team plugin's token was shown to a manager once, and is kept as a host
// token is (plugins.rotate_at).
func markTeamTokensRotateTx(ctx context.Context, tx pgx.Tx, team int64) error {
	if _, err := tx.Exec(ctx, `UPDATE mirror_tokens SET rotate_at = now() WHERE owner_id = $1 AND revoked_at IS NULL`, team); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE plugins SET rotate_at = now() WHERE owner_id = $1 AND revoked_at IS NULL`, team)
	return err
}

// markRotateForDisableTx marks rotate the host tokens of every team person
// user operates or manages, inside the disable's tx.
func markRotateForDisableTx(ctx context.Context, tx pgx.Tx, user int64) error {
	for _, table := range []string{"mirror_tokens", "plugins"} {
		if _, err := tx.Exec(ctx, `UPDATE `+table+` SET rotate_at = now() WHERE revoked_at IS NULL AND owner_id IN (
			SELECT team_id FROM team_members WHERE user_id = $1 AND role IN ('operator', 'manager'))`, user); err != nil {
			return err
		}
	}
	return nil
}

// holdsTokenRole reports whether role is one a team host token is a concern
// for: an operator (who has a shell on the host) or a manager (who mints).
func holdsTokenRole(role string) bool { return role == "operator" || role == "manager" }

// The kinds of rotate reason, as /teams/{id}/hosts words them.
const (
	RotateLeft     = "left"
	RotateToViewer = "viewer"
	RotateDisabled = "disabled"
)

// RotateReason is why a team host token is marked rotate: the person whose
// change set the mark, what the change was, and when.
type RotateReason struct {
	UserID int64
	Name   string
	Kind   string // RotateLeft, RotateToViewer or RotateDisabled
	At     time.Time
}

// ErrNoRotateReason: no audit row explains the mark (a disable through a path
// that writes no audit row, SetDisabled with no actor).
var ErrNoRotateReason = errors.New("users: no audit row explains the rotate mark")

// TeamRotateReason is the reason for a rotate mark on team's tokens set at
// at: the triggering audit row written IN THE MARK'S OWN TRANSACTION — the
// team's team.member.remove of an operator or manager, its team.member.set
// from operator or manager to viewer, or a user.disable — with that person's
// name. rotate_at and audit_log.at are both now() of that one transaction, so
// the row is matched on at = rotate_at exactly, never "the newest row before
// it": a mark whose transaction wrote no audit row (a disable with no actor)
// has NO reason (ErrNoRotateReason), rather than a STALE row from another
// change (#685 round 1, G4: an older disable of someone who was a viewer
// then, and an operator now, was shown). The role each condition reads is
// the row's own (detail.role / detail.before, written at the change), never
// today's membership. A disable carries no role in its row; it is matched
// only by its transaction's time, and the disable trigger marks only for an
// operator's or manager's disable (markRotateForDisableTx).
//
// Read under System: audit_log's self_read admits a team's managers to the
// team's own rows but not to a user.disable row, which names no team. The
// caller (/teams/{id}/hosts) has checked the principal manages team.
func (s *PGStore) TeamRotateReason(ctx context.Context, team int64, at time.Time) (RotateReason, error) {
	var r RotateReason
	var action string
	err := s.pool.QueryRow(sys(ctx), `
		SELECT a.action, a.at, x.uid, u.display_name
		FROM audit_log a
		CROSS JOIN LATERAL (SELECT CASE
			WHEN a.action = 'user.disable' AND a.object ~ '^user:[0-9]{1,18}$' THEN substring(a.object FROM 6)::bigint
			WHEN a.action <> 'user.disable' AND a.detail->>'user' ~ '^[0-9]{1,18}$' THEN (a.detail->>'user')::bigint
			END AS uid) x
		JOIN users u ON u.id = x.uid
		WHERE a.at = $2 AND (
			(a.team_id = $1 AND a.action = 'team.member.remove' AND a.detail->>'role' IN ('operator', 'manager'))
			OR (a.team_id = $1 AND a.action = 'team.member.set' AND a.detail->>'before' IN ('operator', 'manager')
				AND a.detail->>'role' = 'viewer')
			OR a.action = 'user.disable')
		ORDER BY a.at DESC, a.id DESC LIMIT 1`, team, at).Scan(&action, &r.At, &r.UserID, &r.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return RotateReason{}, ErrNoRotateReason
	}
	if err != nil {
		return RotateReason{}, err
	}
	switch action {
	case "team.member.remove":
		r.Kind = RotateLeft
	case "team.member.set":
		r.Kind = RotateToViewer
	default:
		r.Kind = RotateDisabled
	}
	return r, nil
}
