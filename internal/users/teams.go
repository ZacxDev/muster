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
	"github.com/ZacxDev/muster/internal/branding"
)

// Teams (docs/design/team-sharing.md, phase 1; migration 0126).
//
// A team is a users row of kind 'team': an OWNER that no one signs in as. A
// person belongs to it through team_members with a role (viewer, operator,
// manager). Membership is read into the request principal (Account), and
// Postgres decides from it what a member may read and write of the team's
// rows (docs/auth.md#teams).
//
// 🔴 AN ADMIN MANAGES TEAMS from /admin/teams, whose admin-role check is the
// gate (internal/api admin_teams.go); a manager manages their own team from
// /teams/<id> (internal/api teams_pages.go). Every write here
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
	// A team's name is a users.display_name: the same character rule as a
	// person's (a *branding.NameCharError).
	if err := branding.CheckNameText(name); err != nil {
		return Team{}, err
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

// ReroleTeamMemberAsManager changes the role of an EXISTING member of team,
// for the team's own MANAGER (/teams/{id}), and records it as by. It never
// ADDS anyone: a person who is not a member, and an id that names no one,
// both answer ErrNotAMember — one answer, so the id form is neither a way to
// add a person without their consent nor an oracle for which ids exist. It
// refuses a change that would leave the team with no manager. The caller has
// checked by manages team.
func (s *PGStore) ReroleTeamMemberAsManager(ctx context.Context, by Actor, team, user int64, role authz.TeamRole) error {
	if !role.Valid() {
		return ErrTeamRole
	}
	return s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockTeam(ctx, tx, team); err != nil {
			return err
		}
		var cur string
		err := tx.QueryRow(ctx, `SELECT role FROM team_members WHERE team_id = $1 AND user_id = $2 FOR UPDATE`, team, user).Scan(&cur)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotAMember
		}
		if err != nil {
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

// AddTeamMemberByEmailAsManager adds the person who signs in with email to
// team as role, for the team's own manager, and records it as by. It reports
// NOTHING about whether such a person exists: no person with that email, a
// DISABLED one, or one already in the team, is a nil answer with no change
// and no audit row,
// in the same one transaction a real add takes. The caller has checked by
// manages team.
func (s *PGStore) AddTeamMemberByEmailAsManager(ctx context.Context, by Actor, team int64, email string, role authz.TeamRole) error {
	if !role.Valid() {
		return ErrTeamRole
	}
	return s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockTeam(ctx, tx, team); err != nil {
			return err
		}
		var user int64
		var member bool
		err := tx.QueryRow(ctx, `SELECT u.id, EXISTS (SELECT 1 FROM team_members m WHERE m.team_id = $2 AND m.user_id = u.id)
			FROM users u WHERE lower(u.email) = lower($1) AND u.kind = 'person' AND u.disabled_at IS NULL FOR SHARE OF u`, strings.TrimSpace(email), team).Scan(&user, &member)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && member {
			return nil
		}
		if err != nil {
			return err
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
// (migration 0127), and nobody else but the rows' own actors.
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

// TeamPushTargets is who a notification owned by owner reaches when owner
// is a TEAM (team sharing phase 2b, part C: the one team fan-out, in
// internal/api goNotify). It is read NOW, at send time, under System:
// team_members and users are self-read, and the caller is a background push
// naming the owner, not a person.
type TeamPushTargets struct {
	// Team: owner is a team. False for a person (whose push is their own) and
	// for an id that names no one.
	Team bool
	// Name is the team's name, for the title's prefix.
	Name string
	// Notify is who a push reaches: the team's OPERATORS and MANAGERS whose
	// own notify switch (team_members.notify, 0133) is on — persons only, not
	// disabled. A viewer, someone who has left, a disabled person and a member
	// who turned the switch off get nothing.
	Notify []int64
	// Managers is the MANAGERS among Notify (switch on): who a push only a
	// manager can act on reaches (a team agent's connector request, team
	// sharing 4.8: only a manager approves it).
	Managers []int64
	// Withdraw is who a withdrawal (a "resolved" notification) reaches: the
	// same operators and managers WHATEVER their switch says (operator
	// decision G1: "the team's current operators and managers"), so a member
	// who turns the switch off after a push still has it closed.
	Withdraw []int64
}

// TeamPushTargets reads owner's TeamPushTargets: one query for a person,
// two for a team. Both member lists are sorted by id.
func (s *PGStore) TeamPushTargets(ctx context.Context, owner int64) (TeamPushTargets, error) {
	var t TeamPushTargets
	var kind string
	err := s.pool.QueryRow(sys(ctx), `SELECT kind, display_name FROM users WHERE id = $1`, owner).Scan(&kind, &t.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return TeamPushTargets{}, nil
	}
	if err != nil || kind != "team" {
		return TeamPushTargets{}, err
	}
	t.Team = true
	rows, err := s.pool.Query(sys(ctx), `SELECT m.user_id, m.notify, m.role FROM team_members m JOIN users u ON u.id = m.user_id
		WHERE m.team_id = $1 AND m.role IN ('operator', 'manager') AND u.kind = 'person' AND u.disabled_at IS NULL
		ORDER BY m.user_id`, owner)
	if err != nil {
		return TeamPushTargets{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var notify bool
		var role string
		if err := rows.Scan(&id, &notify, &role); err != nil {
			return TeamPushTargets{}, err
		}
		t.Withdraw = append(t.Withdraw, id)
		if notify {
			t.Notify = append(t.Notify, id)
			if authz.TeamRole(role) == authz.TeamManager {
				t.Managers = append(t.Managers, id)
			}
		}
	}
	return t, rows.Err()
}

// MyTeam is one of a person's teams as Account -> Teams lists it.
type MyTeam struct {
	ID     int64
	Name   string
	Role   authz.TeamRole
	Notify bool
}

// MyTeams is person user's teams, by name, with their role in each and their
// notify switch. Read under System: the caller passes the request
// principal's own id.
func (s *PGStore) MyTeams(ctx context.Context, user int64) ([]MyTeam, error) {
	rows, err := s.pool.Query(sys(ctx), `SELECT t.id, t.display_name, m.role, m.notify FROM team_members m
		JOIN users t ON t.id = m.team_id AND t.kind = 'team' WHERE m.user_id = $1 ORDER BY lower(t.display_name), t.id`, user)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (MyTeam, error) {
		var m MyTeam
		var role string
		err := r.Scan(&m.ID, &m.Name, &role, &m.Notify)
		m.Role = authz.TeamRole(role)
		return m, err
	})
}

// MembershipsAmong is every team membership of each of persons, in ONE query:
// the team stream sweep's read (internal/api team_live.go), every
// DisableSweepInterval, of the people who hold an open team stream. A person
// with no row in the answer is in no team now. Under System: team_members is
// self-read, and the sweep reads other people's rows.
func (s *PGStore) MembershipsAmong(ctx context.Context, persons []int64) (map[int64][]authz.Membership, error) {
	rows, err := s.pool.Query(sys(ctx), `SELECT user_id, team_id, role FROM team_members WHERE user_id = ANY($1) ORDER BY user_id, team_id`, persons)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]authz.Membership{}
	for rows.Next() {
		var user int64
		var m authz.Membership
		var role string
		if err := rows.Scan(&user, &m.Team, &role); err != nil {
			return nil, err
		}
		m.Role = authz.TeamRole(role)
		out[user] = append(out[user], m)
	}
	return out, rows.Err()
}

// SetTeamNotify sets person user's OWN notify switch in team (0133): it
// updates only team_members.notify, only on the caller's own row, and only
// while they are a member — ErrNotAMember otherwise, the same answer for a
// team that does not exist. Under System: team_members is self-read, and the
// caller passes the request principal's own id (it is in the System ledger).
// A manager does not set it for anyone else; there is no other writer.
func (s *PGStore) SetTeamNotify(ctx context.Context, user, team int64, on bool) error {
	tag, err := s.pool.Exec(sys(ctx), `UPDATE team_members SET notify = $3 WHERE team_id = $1 AND user_id = $2`, team, user, on)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotAMember
	}
	return nil
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
	if err := endLends(ctx, tx, team, user); err != nil {
		return err
	}
	// Team sharing 2b.4: what they queued into the team's windows is
	// cancelled with them (team_queue.go).
	if _, err := CancelQueuedByTx(ctx, tx, team, user, time.Time{}, QueueCancelLeftTeam); err != nil {
		return err
	}
	// Team sharing 2b.2: an operator or a manager leaving marks the team's
	// host tokens rotate (team_hosts.go), in this transaction.
	if holdsTokenRole(role) {
		if err := markTeamTokensRotateTx(ctx, tx, team); err != nil {
			return err
		}
	}
	return auditInsertFor(ctx, tx, by, team, "team.member.remove", TeamObject(team), map[string]any{"user": user, "role": role})
}

// endLends takes back every host spawn user LENT to team (migration 0130, team
// sharing phase 3), inside tx: a lend is admitted only to a team the person
// operates, so it ends when they leave the team or drop to viewer — their
// rows stop showing to the team in the same transaction as the change.
func endLends(ctx context.Context, tx pgx.Tx, team, user int64) error {
	_, err := tx.Exec(ctx, `UPDATE officer_host_spawns SET shared_team_id = NULL WHERE owner_id = $1 AND shared_team_id = $2`, user, team)
	return err
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

// TeamNames is the display name of each of ids that is a team (a person's
// id is left out). It names the teams a caller's own memberships list — the
// task board's owner groups and the Owner select — so the caller is a member
// of each; it returns nothing else about them.
func (s *PGStore) TeamNames(ctx context.Context, ids []int64) (map[int64]string, error) {
	out := map[int64]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(sys(ctx), `SELECT id, display_name FROM users WHERE kind = 'team' AND id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
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
	if role == authz.TeamViewer {
		if err := endLends(ctx, tx, team, user); err != nil {
			return err
		}
		// Team sharing 2b.4: a viewer queues nothing, so what they queued as
		// an operator is cancelled (team_queue.go).
		if _, err := CancelQueuedByTx(ctx, tx, team, user, time.Time{}, QueueCancelToViewer); err != nil {
			return err
		}
		// Team sharing 2b.2: an operator or a manager dropping to viewer
		// marks the team's host tokens rotate (team_hosts.go).
		if before != nil && holdsTokenRole(*before) {
			if err := markTeamTokensRotateTx(ctx, tx, team); err != nil {
				return err
			}
		}
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
