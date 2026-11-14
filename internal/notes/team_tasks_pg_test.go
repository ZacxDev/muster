package notes

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/users"
)

// Team tasks (docs/design/team-sharing.md phase 4, slice 4.3): filing a task
// in a team, and moving one of yours into it.

// teamTaskWorld is a team T managed by A, with B an operator and V a viewer,
// and N, who is in no team.
type teamTaskWorld struct {
	pool          *pgxpool.Pool
	dsn           string
	store         *PGStore
	a, b, v, n, t int64
}

func newTeamTaskWorld(t *testing.T) *teamTaskWorld {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	dsn := dbtest.DSN(t)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatal(err)
	}
	sys := authz.System(ctx)
	w := &teamTaskWorld{pool: pool, dsn: dsn, store: NewPG(pool)}
	person := func(name string) int64 {
		var id int64
		if err := pool.QueryRow(sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	suffix := time.Now().Format("150405.000000000")
	w.a, w.b, w.v, w.n = person("a-"+suffix), person("b-"+suffix), person("v-"+suffix), person("n-"+suffix)
	us := users.NewPG(pool)
	by := users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}
	team, err := us.CreateTeam(sys, by, "tt-"+suffix, w.a)
	if err != nil {
		t.Fatal(err)
	}
	w.t = team.ID
	if err := us.SetTeamMember(sys, by, w.t, w.b, authz.TeamOperator); err != nil {
		t.Fatal(err)
	}
	if err := us.SetTeamMember(sys, by, w.t, w.v, authz.TeamViewer); err != nil {
		t.Fatal(err)
	}
	return w
}

// as is person uid's browser-session principal, memberships read from the
// database as every request builds it.
func (w *teamTaskWorld) as(t *testing.T, uid int64) context.Context {
	t.Helper()
	acct, err := users.NewPG(w.pool).Account(authz.System(context.Background()), uid)
	if err != nil {
		t.Fatal(err)
	}
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: acct.Role, Via: authz.ViaSession, Teams: acct.Teams})
}

// asTeam is the team acting principal (slice 4.1): the team itself, as its
// chief will act.
func (w *teamTaskWorld) asTeam() context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: w.t, Role: authz.RoleMember, Via: authz.ViaAgent, Team: true})
}

func sqlState(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return ""
}

// TestATeamTaskIsFiledAtOperatorAndRefusedBelow: Create with a team Owner
// files the task IN the team for an operator and a manager — owner_id is the
// team, not the person — and Postgres refuses a viewer and a non-member with
// 42501, whatever any handler checked. CONTROL: the same people file their
// own task (no Owner) freely, owned by themselves.
func TestATeamTaskIsFiledAtOperatorAndRefusedBelow(t *testing.T) {
	w := newTeamTaskWorld(t)
	for who, uid := range map[string]int64{"the manager": w.a, "an operator": w.b} {
		n, err := w.store.Create(w.as(t, uid), Note{Title: "filed by " + who, Body: "b", Owner: w.t})
		if err != nil {
			t.Fatalf("%s filing a team task: %v", who, err)
		}
		if n.OwnerID != w.t {
			t.Errorf("%s filed a team task owned by %d; want the team %d", who, n.OwnerID, w.t)
		}
	}
	for who, uid := range map[string]int64{"a viewer": w.v, "a non-member": w.n} {
		_, err := w.store.Create(w.as(t, uid), Note{Title: "filed by " + who, Body: "b", Owner: w.t})
		if sqlState(err) != "42501" {
			t.Errorf("%s filing a team task: err %v; want SQLSTATE 42501 from Postgres", who, err)
		}
		own, err := w.store.Create(w.as(t, uid), Note{Title: "own", Body: "b"})
		if err != nil || own.OwnerID != uid {
			t.Errorf("control: %s filing their own task: owner %d, %v; want %d", who, own.OwnerID, err, uid)
		}
	}
	var teamRows int
	if err := w.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM notes WHERE owner_id = $1`, w.t).Scan(&teamRows); err != nil {
		t.Fatal(err)
	}
	if teamRows != 2 {
		t.Errorf("the team owns %d tasks; want exactly the manager's and the operator's 2", teamRows)
	}
}

// TestAPersonalTaskIsInvisibleToTheTeam: A's own task is seen by A and by no
// one else in A's team — not the operator B, not the viewer V, and not the
// team acting principal (the team chief's identity). CONTROL: each of them
// sees the team's own task, so the zero is about whose task it is.
func TestAPersonalTaskIsInvisibleToTheTeam(t *testing.T) {
	w := newTeamTaskWorld(t)
	mine, err := w.store.Create(w.as(t, w.a), Note{Title: "A's own canary", Body: "b"})
	if err != nil {
		t.Fatal(err)
	}
	team, err := w.store.Create(w.as(t, w.a), Note{Title: "team task", Body: "b", Owner: w.t})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.store.Get(w.as(t, w.a), mine.ID); err != nil {
		t.Fatalf("control: A reads A's own task: %v", err)
	}
	for who, ctx := range map[string]context.Context{"B (operator)": w.as(t, w.b), "V (viewer)": w.as(t, w.v), "the team acting principal": w.asTeam()} {
		if _, err := w.store.Get(ctx, mine.ID); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("%s reading A's personal task: %v; want no rows", who, err)
		}
		list, err := w.store.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		sawTeam := false
		for _, n := range list {
			if n.ID == mine.ID {
				t.Errorf("%s lists A's personal task %d", who, mine.ID)
			}
			sawTeam = sawTeam || n.ID == team.ID
		}
		if !sawTeam {
			t.Errorf("control: %s does not list the team's task %d", who, team.ID)
		}
	}
}

// TestMovingATaskMovesEveryChild: A's task with a comment, an attachment and
// a session link moves into the team, and afterwards the task and EVERY child
// row carry the team; the comment keeps its author (author_id A). The team's
// viewer reads it (and its children) through row-level security, and A still
// does — as the team's manager now. CONTROL: before the move V saw none of it.
func TestMovingATaskMovesEveryChild(t *testing.T) {
	w := newTeamTaskWorld(t)
	actx := w.as(t, w.a)
	n, err := w.store.Create(actx, Note{Title: "moving", Body: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.store.AddComment(actx, Comment{NoteID: n.ID, Author: "user", Body: "a comment"}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.store.AddAttachment(actx, n.ID, Attachment{Filename: "f.txt", ContentType: "text/plain", SizeBytes: 1, Data: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.store.LinkSession(actx, n.ID, SessionLink{SessionID: "sess-move", Role: "worked"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := w.store.Get(w.as(t, w.v), n.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("control: V read A's task before the move: %v", err)
	}
	if err := w.store.MoveToTeam(actx, n.ID, w.a, w.t); sqlState(err) != "42501" {
		t.Fatalf("MoveToTeam under A's own principal: %v; want 42501 (owner_id changes only under the system scope)", err)
	}
	if err := w.store.MoveToTeam(authz.System(context.Background()), n.ID, w.a, w.t); err != nil {
		t.Fatalf("MoveToTeam: %v; want the task and its children moved", err)
	}
	sys := authz.System(context.Background())
	for tbl, col := range map[string]string{"notes": "id", "note_comments": "note_id", "note_attachments": "note_id", "task_sessions": "note_id"} {
		var total, team int
		if err := w.pool.QueryRow(sys, `SELECT count(*), count(*) FILTER (WHERE owner_id = $2) FROM `+tbl+` WHERE `+col+` = $1`, n.ID, w.t).Scan(&total, &team); err != nil {
			t.Fatal(err)
		}
		if total == 0 || team != total {
			t.Errorf("after the move %s has %d of its %d rows owned by the team; want all of them", tbl, team, total)
		}
	}
	var author *int64
	if err := w.pool.QueryRow(sys, `SELECT author_id FROM note_comments WHERE note_id = $1`, n.ID).Scan(&author); err != nil || author == nil || *author != w.a {
		t.Errorf("the moved comment's author_id = %v (%v); want A %d", author, err, w.a)
	}
	got, err := w.store.Get(w.as(t, w.v), n.ID)
	if err != nil || len(got.Comments) != 1 || len(got.Attachments) != 1 || len(got.Sessions) != 1 {
		t.Errorf("V reading the moved task: %d comments, %d attachments, %d sessions, %v; want 1, 1, 1", len(got.Comments), len(got.Attachments), len(got.Sessions), err)
	}
	if _, err := w.store.Get(actx, n.ID); err != nil {
		t.Errorf("A reading the task A moved into A's team: %v", err)
	}
}

// TestAMoveIsRefusedNamingTheLink: a task an ask of A's points at
// (attention_asks.task_id) does not move, and the refusal names the link;
// nothing moved. A task that is not the mover's does not move either.
// CONTROL: once the ask is gone, the same move succeeds.
func TestAMoveIsRefusedNamingTheLink(t *testing.T) {
	w := newTeamTaskWorld(t)
	actx := w.as(t, w.a)
	n, err := w.store.Create(actx, Note{Title: "linked", Body: "b"})
	if err != nil {
		t.Fatal(err)
	}
	sys := authz.System(context.Background())
	var ask int64
	if err := w.pool.QueryRow(sys, `INSERT INTO attention_asks (owner_id, mirror_host, mirror_session, question, expires_at, task_id)
		VALUES ($1, 'h', 's', 'which?', now() + interval '1 hour', $2) RETURNING id`, w.a, n.ID).Scan(&ask); err != nil {
		t.Fatal(err)
	}
	sysCtx := authz.System(context.Background())
	err = w.store.MoveToTeam(sysCtx, n.ID, w.a, w.t)
	var blocked *MoveBlockedError
	if !errors.As(err, &blocked) || !strings.Contains(err.Error(), MoveRefusals["attention_asks.task_id"]) || !strings.Contains(err.Error(), "attention_asks: 1") {
		t.Fatalf("moving a task an ask points at: %v; want a refusal naming the ask link", err)
	}
	var owner int64
	_ = w.pool.QueryRow(sys, `SELECT owner_id FROM notes WHERE id = $1`, n.ID).Scan(&owner)
	if owner != w.a {
		t.Errorf("a refused move left the task owned by %d; want A %d", owner, w.a)
	}
	if err := w.store.MoveToTeam(sysCtx, n.ID, w.b, w.t); !errors.Is(err, ErrMoveNotYours) {
		t.Errorf("B moving A's task: %v; want ErrMoveNotYours", err)
	}
	if _, err := w.pool.Exec(sys, `DELETE FROM attention_asks WHERE id = $1`, ask); err != nil {
		t.Fatal(err)
	}
	if err := w.store.MoveToTeam(sysCtx, n.ID, w.a, w.t); err != nil {
		t.Errorf("control: the same move with no link: %v", err)
	}
}

// fksOntoNotes is every foreign key onto notes, as "table.column", read from
// the database's own catalog.
func fksOntoNotes(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT c.conrelid::regclass::text || '.' || a.attname
		FROM pg_constraint c
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = c.conkey[1]
		WHERE c.contype = 'f' AND c.confrelid = 'public.notes'::regclass ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// unclassifiedFKs is each key in neither ledger, and each ledger entry that
// is not a key.
func unclassifiedFKs(keys []string) (missing, stale []string) {
	known := map[string]bool{}
	for _, k := range MoveChildren() {
		known[k] = true
	}
	for k := range MoveRefusals {
		known[k] = true
	}
	have := map[string]bool{}
	for _, k := range keys {
		have[k] = true
		if !known[k] {
			missing = append(missing, k)
		}
	}
	for k := range known {
		if !have[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	return missing, stale
}

// TestEveryForeignKeyOntoNotesIsClassified is the move's seam ledger: every
// foreign key onto notes is in MoveChildren (moved with the task) or in
// MoveRefusals (a named refusal), and nothing in either is stale. It fails
// when a key appears (a new child table must say what a move does to it) and
// when one disappears. POSITIVE CONTROL: a new table with a key onto notes,
// created in this database, is reported unclassified.
func TestEveryForeignKeyOntoNotesIsClassified(t *testing.T) {
	w := newTeamTaskWorld(t)
	ctx := dbtest.AsOperator(context.Background())
	keys := fksOntoNotes(t, ctx, w.pool)
	if len(keys) < 9 {
		t.Fatalf("%d foreign keys onto notes; the ledger is reading nothing: %v", len(keys), keys)
	}
	if missing, stale := unclassifiedFKs(keys); len(missing) > 0 || len(stale) > 0 {
		t.Errorf("foreign keys onto notes with no move class (add to MoveChildren or MoveRefusals): %v; ledger entries that are no key: %v", missing, stale)
	}
	for _, k := range MoveChildren() {
		if _, both := MoveRefusals[k]; both {
			t.Errorf("%s is both moved and a refusal", k)
		}
	}
	// The login role creates it (the application role may not), and drops it.
	dbtest.OwnerExec(t, w.dsn, `CREATE TABLE public.probe_note_child (id bigint, note_id bigint REFERENCES public.notes (id))`)
	t.Cleanup(func() { dbtest.OwnerExec(t, w.dsn, `DROP TABLE IF EXISTS public.probe_note_child`) })
	rows, err := w.pool.Query(ctx, `SELECT c.conrelid::regclass::text || '.' || a.attname FROM pg_constraint c
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = c.conkey[1]
		WHERE c.contype = 'f' AND c.confrelid = 'public.notes'::regclass`)
	if err != nil {
		t.Fatal(err)
	}
	probe, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if missing, _ := unclassifiedFKs(probe); len(missing) != 1 || missing[0] != "probe_note_child.note_id" {
		t.Errorf("control: a new key onto notes is reported as %v; want [probe_note_child.note_id]", missing)
	}
}
