package users

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ZacxDev/muster/internal/authz"
)

// Teams (docs/design/team-sharing.md, phase 1; migration 0125).
//
// A team is a users row of kind 'team': an OWNER that no one signs in as. A
// person belongs to it through team_members with a role (viewer, operator,
// manager). In this phase a team owns nothing and membership shares nothing:
// it is read into the request principal (Account) and is otherwise inert.
//
// 🔴 ONLY AN ADMIN MANAGES TEAMS IN THIS PHASE, from /admin/teams, whose
// admin-role check is the gate (internal/api admin_teams.go). Every write here
// runs under authz.System (inTx) and leaves an audit_log row naming the acting
// admin IN THE SAME TRANSACTION, so a change and its record commit together.

// Team is one team and its members.
type Team struct {
	ID        int64
	Name      string
	CreatedAt time.Time
	Members   []TeamMember
}

// TeamMember is one person's membership.
type TeamMember struct {
	UserID      int64
	Email       string
	DisplayName string
	Role        authz.TeamRole
	AddedBy     int64
	AddedAt     time.Time
}

var (
	// ErrTeamNameTaken: another team has that name (case-insensitively).
	ErrTeamNameTaken = errors.New("users: a team already has that name")
	// ErrTeamName: the name is empty or longer than TeamNameMax characters.
	ErrTeamName = errors.New("users: a team name is 1 to 64 characters")
	// ErrNotATeam: the id names no team.
	ErrNotATeam = errors.New("users: no such team")
	// ErrNotAPerson: the id names no person (a missing user, or a team).
	ErrNotAPerson = errors.New("users: no such person")
	// ErrNotAMember: the person is not a member of the team.
	ErrNotAMember = errors.New("users: not a member of that team")
	// ErrTeamRole: the role is not viewer, operator or manager.
	ErrTeamRole = errors.New("users: a team role is viewer, operator or manager")
)

// TeamNameMax is the longest team name (users_team_no_login).
const TeamNameMax = 64

// TeamObject is the audit_log object naming team id.
func TeamObject(id int64) string { return "team:" + strconv.FormatInt(id, 10) }

// CreateTeam creates team name with firstManager as its manager, and records
// both as by. One transaction: the team, its first membership and their two
// audit rows commit together or not at all.
func (s *PGStore) CreateTeam(ctx context.Context, by Actor, name string, firstManager int64) (Team, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > TeamNameMax {
		return Team{}, ErrTeamName
	}
	var t Team
	err := s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockPerson(ctx, tx, firstManager); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `INSERT INTO users (kind, role, display_name) VALUES ('team', 'member', $1)
			RETURNING id, display_name, created_at`, name).Scan(&t.ID, &t.Name, &t.CreatedAt)
		if isUniqueViolation(err, "users_team_name_key") {
			return ErrTeamNameTaken
		}
		if err != nil {
			return err
		}
		if err := auditInsertFor(ctx, tx, by, t.ID, "team.create", TeamObject(t.ID), map[string]any{"name": t.Name}); err != nil {
			return err
		}
		return setMemberTx(ctx, tx, by, t.ID, firstManager, authz.TeamManager)
	})
	if err != nil {
		return Team{}, err
	}
	return t, nil
}

// SetTeamMember adds person user to team with role, or changes their role,
// and records it as by. Setting the role a member already has changes nothing
// and records nothing.
func (s *PGStore) SetTeamMember(ctx context.Context, by Actor, team, user int64, role authz.TeamRole) error {
	if !role.Valid() {
		return ErrTeamRole
	}
	return s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockTeam(ctx, tx, team); err != nil {
			return err
		}
		if err := lockPerson(ctx, tx, user); err != nil {
			return err
		}
		return setMemberTx(ctx, tx, by, team, user, role)
	})
}

// ErrLastManager: the change would leave the team with no manager. A manager
// cannot make it (an admin can, on /admin/teams).
var ErrLastManager = errors.New("users: a team keeps at least one manager")

// SetTeamMemberAsManager is SetTeamMember for a team's own MANAGER
// (/teams/{id}): the same change and audit row, refused when it would leave
// the team with no manager. The caller has checked by is team's manager.
func (s *PGStore) SetTeamMemberAsManager(ctx context.Context, by Actor, team, user int64, role authz.TeamRole) error {
	if !role.Valid() {
		return ErrTeamRole
	}
	return s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockTeam(ctx, tx, team); err != nil {
			return err
		}
		if err := lockPerson(ctx, tx, user); err != nil {
			return err
		}
		if role != authz.TeamManager {
			if err := keepAManager(ctx, tx, team, user); err != nil {
				return err
			}
		}
		return setMemberTx(ctx, tx, by, team, user, role)
	})
}

// RemoveTeamMemberAsManager is RemoveTeamMember for a team's own manager,
// refused when it would leave the team with no manager.
func (s *PGStore) RemoveTeamMemberAsManager(ctx context.Context, by Actor, team, user int64) error {
	return s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockTeam(ctx, tx, team); err != nil {
			return err
		}
		if err := keepAManager(ctx, tx, team, user); err != nil {
			return err
		}
		return removeMemberTx(ctx, tx, by, team, user)
	})
}

// keepAManager refuses a change to user that would leave team with no
// manager: user is a manager and no other member is. The team row is locked
// (lockTeam), so two managers demoting each other at once cannot both pass.
func keepAManager(ctx context.Context, tx pgx.Tx, team, user int64) error {
	var others int
	var isManager bool
	if err := tx.QueryRow(ctx, `SELECT
			count(*) FILTER (WHERE user_id <> $2 AND role = 'manager'),
			coalesce(bool_or(user_id = $2 AND role = 'manager'), false)
		FROM team_members WHERE team_id = $1`, team, user).Scan(&others, &isManager); err != nil {
		return err
	}
	if isManager && others == 0 {
		return ErrLastManager
	}
	return nil
}

// TeamAudit is the newest audit rows that name team as their owner, read AS
// the caller: audit_log's self_read admits a team's MANAGERS to them
// (migration 0126), and nobody else but the rows' own actors.
func (s *PGStore) TeamAudit(ctx context.Context, team int64, limit int) ([]AuditEntry, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, at, COALESCE(user_id, 0), via, action, object, detail
		FROM audit_log WHERE team_id = $1 ORDER BY at DESC, id DESC LIMIT $2`, team, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var raw []byte
		if err := rows.Scan(&e.ID, &e.At, &e.UserID, &e.Via, &e.Action, &e.Object, &raw); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(raw, &e.Detail)
		out = append(out, e)
	}
	return out, rows.Err()
}

// TeamMembers is team's members with their roles, read under System: the
// caller has checked the principal manages team (/teams/{id}) or is an admin.
func (s *PGStore) TeamMembers(ctx context.Context, team int64) ([]TeamMember, error) {
	rows, err := s.pool.Query(sys(ctx), `SELECT m.user_id, COALESCE(u.email, ''), u.display_name, m.role, m.added_by, m.added_at
		FROM team_members m JOIN users u ON u.id = m.user_id WHERE m.team_id = $1 ORDER BY m.user_id`, team)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (TeamMember, error) {
		var m TeamMember
		var role string
		err := r.Scan(&m.UserID, &m.Email, &m.DisplayName, &role, &m.AddedBy, &m.AddedAt)
		m.Role = authz.TeamRole(role)
		return m, err
	})
}

// RemoveTeamMember removes person user from team and records it as by.
func (s *PGStore) RemoveTeamMember(ctx context.Context, by Actor, team, user int64) error {
	return s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockTeam(ctx, tx, team); err != nil {
			return err
		}
		return removeMemberTx(ctx, tx, by, team, user)
	})
}

// removeMemberTx deletes one membership and audits it, inside tx.
func removeMemberTx(ctx context.Context, tx pgx.Tx, by Actor, team, user int64) error {
	var role string
	err := tx.QueryRow(ctx, `DELETE FROM team_members WHERE team_id = $1 AND user_id = $2 RETURNING role`, team, user).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotAMember
	}
	if err != nil {
		return err
	}
	return auditInsertFor(ctx, tx, by, team, "team.member.remove", TeamObject(team), map[string]any{"user": user, "role": role})
}

// ListTeams returns every team with its members, oldest team first.
func (s *PGStore) ListTeams(ctx context.Context) ([]Team, error) {
	ctx = sys(ctx)
	rows, err := s.pool.Query(ctx, `SELECT id, display_name, created_at FROM users WHERE kind = 'team' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	teams, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Team, error) {
		var t Team
		err := r.Scan(&t.ID, &t.Name, &t.CreatedAt)
		return t, err
	})
	if err != nil || len(teams) == 0 {
		return teams, err
	}
	at := map[int64]int{}
	for i, t := range teams {
		at[t.ID] = i
	}
	rows, err = s.pool.Query(ctx, `SELECT m.team_id, m.user_id, COALESCE(u.email, ''), u.display_name, m.role, m.added_by, m.added_at
		FROM team_members m JOIN users u ON u.id = m.user_id ORDER BY m.team_id, m.user_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var team int64
		var m TeamMember
		var role string
		if err := rows.Scan(&team, &m.UserID, &m.Email, &m.DisplayName, &role, &m.AddedBy, &m.AddedAt); err != nil {
			return nil, err
		}
		m.Role = authz.TeamRole(role)
		if i, ok := at[team]; ok {
			teams[i].Members = append(teams[i].Members, m)
		}
	}
	return teams, rows.Err()
}

// setMemberTx upserts one membership and audits it, inside tx.
func setMemberTx(ctx context.Context, tx pgx.Tx, by Actor, team, user int64, role authz.TeamRole) error {
	var before *string
	err := tx.QueryRow(ctx, `SELECT role FROM team_members WHERE team_id = $1 AND user_id = $2 FOR UPDATE`, team, user).Scan(&before)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if before != nil && *before == string(role) {
		return nil
	}
	var adder any
	if by.UserID > 0 {
		adder = by.UserID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO team_members (team_id, user_id, role, added_by) VALUES ($1, $2, $3, $4)
		ON CONFLICT (team_id, user_id) DO UPDATE SET role = EXCLUDED.role`, team, user, string(role), adder); err != nil {
		return err
	}
	detail := map[string]any{"user": user, "role": string(role)}
	if before != nil {
		detail["before"] = *before
	}
	return auditInsertFor(ctx, tx, by, team, "team.member.set", TeamObject(team), detail)
}

// lockTeam locks team's users row, or answers ErrNotATeam.
func lockTeam(ctx context.Context, tx pgx.Tx, team int64) error {
	var id int64
	err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id = $1 AND kind = 'team' FOR UPDATE`, team).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotATeam
	}
	return err
}

// lockPerson locks person user's users row, or answers ErrNotAPerson.
func lockPerson(ctx context.Context, tx pgx.Tx, user int64) error {
	var id int64
	err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id = $1 AND kind = 'person' FOR SHARE`, user).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotAPerson
	}
	return err
}

func isUniqueViolation(err error, constraint string) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == constraint
}
