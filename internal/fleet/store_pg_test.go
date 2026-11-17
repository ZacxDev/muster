package fleet

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// The fleet's log and marks on a real Postgres, AS muster_app (db.Connect's
// pool sets that role on every connection): the append-only trigger, the
// owner scope, the one notify per episode, the settle and the look. Every id
// and key is invented.

func pgFleet(t *testing.T) (*PG, *pgxpool.Pool, string) {
	t.Helper()
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(dbtest.AsOperator(ctx), pool, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, role) OVERRIDING SYSTEM VALUE VALUES (2, 'member'), (3, 'member') ON CONFLICT (id) DO NOTHING`)
	dbtest.OwnerExec(t, dsn, `ALTER TABLE fleet_actions DISABLE TRIGGER fleet_actions_append_only`)
	dbtest.OwnerExec(t, dsn, `DELETE FROM fleet_actions`)
	dbtest.OwnerExec(t, dsn, `ALTER TABLE fleet_actions ENABLE TRIGGER fleet_actions_append_only`)
	dbtest.OwnerExec(t, dsn, `DELETE FROM fleet_owner_marks`)
	dbtest.OwnerExec(t, dsn, `DELETE FROM session_episodes WHERE session_key LIKE 'fleet-pg-%'`)
	return NewPG(pool), pool, dsn
}

func asUser(uid int64) context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: authz.RoleMember, Via: authz.ViaSession})
}

// openEpisode writes an open stop for uid and returns its id.
func openEpisode(t *testing.T, pool *pgxpool.Pool, uid int64, key string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(asUser(uid), `INSERT INTO session_episodes (session_kind, session_key, seq, state) VALUES ('mirror', $1, 1, 'stalled') RETURNING id`,
		key).Scan(&id); err != nil {
		t.Fatalf("episode %s: %v", key, err)
	}
	return id
}

// TestTheLogIsAppendOnlyForTheAppRole: a DELETE is refused, an UPDATE of a
// column that is not state/outcome/finished_at is refused, a finished row's
// finished_at cannot be moved EARLIER (nor later, nor cleared), and a
// finish before creation is refused. The positive control: the one update
// the store makes (Settle's finish) goes through, so the refusals are the
// trigger's and not a role that can write nothing.
func TestTheLogIsAppendOnlyForTheAppRole(t *testing.T) {
	p, pool, _ := pgFleet(t)
	ctx := asUser(2)
	ep := openEpisode(t, pool, 2, "fleet-pg-append")
	if ok, err := p.RecordNotify(ctx, Notify{SubjectKind: "mirror", SubjectKey: "fleet-pg-append", Host: "bench", Window: "@4",
		EpisodeID: ep, Evidence: map[string]any{"episode": ep, "jev": "neither"}, Reason: "pushed: the stop needs its owner"}); err != nil || !ok {
		t.Fatalf("record: %v, %v", ok, err)
	}
	var id int64
	if err := pool.QueryRow(ctx, `SELECT id FROM fleet_actions WHERE episode_id = $1`, ep).Scan(&id); err != nil {
		t.Fatal(err)
	}
	refused := func(what, sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, sql, args...)
		if err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: err = %v; want the append-only refusal", what, err)
		}
	}
	refused("DELETE", `DELETE FROM fleet_actions WHERE id = $1`, id)
	refused("moving the subject", `UPDATE fleet_actions SET subject_key = 'fleet-pg-elsewhere' WHERE id = $1`, id)
	refused("rewriting the reason", `UPDATE fleet_actions SET reason = 'something else' WHERE id = $1`, id)
	refused("back-dating the row", `UPDATE fleet_actions SET created_at = created_at - interval '1 day' WHERE id = $1`, id)
	refused("finishing before it was created", `UPDATE fleet_actions SET finished_at = created_at - interval '1 hour', outcome = 'owner' WHERE id = $1`, id)

	// The control: the episode ends and Settle finishes the row.
	if _, err := pool.Exec(ctx, `UPDATE session_episodes SET ended_at = now(), end_reason = 'owner' WHERE id = $1`, ep); err != nil {
		t.Fatal(err)
	}
	if n, err := p.Settle(ctx); err != nil || n != 1 {
		t.Fatalf("settle: %d, %v; want 1", n, err)
	}
	var outcome string
	var finished *time.Time
	if err := pool.QueryRow(ctx, `SELECT outcome, finished_at FROM fleet_actions WHERE id = $1`, id).Scan(&outcome, &finished); err != nil ||
		outcome != "owner" || finished == nil {
		t.Fatalf("settled row: outcome %q finished %v, %v; want owner and a time", outcome, finished, err)
	}
	// A finished row is fixed: its finish cannot move earlier (the
	// decreasing update), later, or be cleared; its outcome cannot change.
	refused("moving the finish earlier", `UPDATE fleet_actions SET finished_at = finished_at - interval '1 minute' WHERE id = $1`, id)
	refused("moving the finish later", `UPDATE fleet_actions SET finished_at = finished_at + interval '1 minute' WHERE id = $1`, id)
	refused("re-opening it", `UPDATE fleet_actions SET finished_at = NULL, outcome = '' WHERE id = $1`, id)
	refused("rewriting the outcome", `UPDATE fleet_actions SET outcome = 'worked' WHERE id = $1`, id)
	if n, err := p.Settle(ctx); err != nil || n != 0 {
		t.Errorf("a second settle: %d, %v; want 0", n, err)
	}
}

// TestOneNotifyPerEpisodeAndOwnerScoped: a replayed claim writes nothing; the
// other owner sees none of these rows, writes none under this owner, and its
// own open count is its own. Open counts are per session, and drop once the
// episode ends and the row is settled.
func TestOneNotifyPerEpisodeAndOwnerScoped(t *testing.T) {
	p, pool, _ := pgFleet(t)
	mine, theirs := asUser(2), asUser(3)
	ep := openEpisode(t, pool, 2, "fleet-pg-mine")
	other := openEpisode(t, pool, 3, "fleet-pg-theirs")
	n := Notify{SubjectKind: "mirror", SubjectKey: "fleet-pg-mine", EpisodeID: ep}
	if ok, err := p.RecordNotify(mine, n); err != nil || !ok {
		t.Fatalf("first: %v, %v", ok, err)
	}
	if ok, err := p.RecordNotify(mine, n); err != nil || ok {
		t.Fatalf("replayed: %v, %v; want nothing written", ok, err)
	}
	if ok, err := p.RecordNotify(theirs, Notify{SubjectKind: "mirror", SubjectKey: "fleet-pg-theirs", EpisodeID: other}); err != nil || !ok {
		t.Fatalf("theirs: %v, %v", ok, err)
	}
	if got, err := p.OpenCounts(mine); err != nil || len(got) != 1 || got["mirror:fleet-pg-mine"] != 1 {
		t.Errorf("my open counts = %v, %v; want only mirror:fleet-pg-mine 1", got, err)
	}
	if got, err := p.OpenCounts(theirs); err != nil || len(got) != 1 || got["mirror:fleet-pg-theirs"] != 1 {
		t.Errorf("their open counts = %v, %v; want only their own", got, err)
	}
	// The other owner cannot settle mine: my episode ends, their Settle finds
	// nothing of mine.
	if _, err := pool.Exec(mine, `UPDATE session_episodes SET ended_at = now(), end_reason = 'worked' WHERE id = $1`, ep); err != nil {
		t.Fatal(err)
	}
	if k, err := p.Settle(theirs); err != nil || k != 0 {
		t.Errorf("their settle touched %d of mine, %v", k, err)
	}
	if k, err := p.Settle(mine); err != nil || k != 1 {
		t.Errorf("my settle: %d, %v; want 1", k, err)
	}
	if got, _ := p.OpenCounts(mine); len(got) != 0 {
		t.Errorf("after the settle, my open counts = %v; want none", got)
	}
	if _, err := p.RecordNotify(mine, Notify{SubjectKind: "pane", SubjectKey: "x", EpisodeID: ep}); err != ErrBadNotify {
		t.Errorf("a bad kind: %v; want ErrBadNotify", err)
	}
}

// TestTheLookMovesTheBaselineOnlyAfterAGap: no look → no baseline; a look
// LookGap or more after the previous one makes that previous one the
// baseline; a re-open inside the gap keeps it. Per owner.
func TestTheLookMovesTheBaselineOnlyAfterAGap(t *testing.T) {
	p, _, _ := pgFleet(t)
	ctx := asUser(2)
	at := time.Date(2000, 1, 2, 9, 0, 0, 0, time.UTC)
	if b, err := p.Look(ctx, at); err != nil || !b.IsZero() {
		t.Fatalf("first look: %v, %v; want no baseline", b, err)
	}
	if b, err := p.Look(ctx, at.Add(3*time.Minute)); err != nil || !b.IsZero() {
		t.Fatalf("a re-open inside the gap: %v, %v; want still none", b, err)
	}
	// 3m + 10m: the previous look (at+3m) is LookGap before → the baseline.
	if b, err := p.Look(ctx, at.Add(13*time.Minute)); err != nil || !b.Equal(at.Add(3*time.Minute)) {
		t.Fatalf("after the gap: %v, %v; want %v", b, err, at.Add(3*time.Minute))
	}
	if b, err := p.Baseline(ctx); err != nil || !b.Equal(at.Add(3*time.Minute)) {
		t.Fatalf("baseline read: %v, %v", b, err)
	}
	if b, err := p.Look(ctx, at.Add(14*time.Minute)); err != nil || !b.Equal(at.Add(3*time.Minute)) {
		t.Fatalf("a re-open a minute later: %v, %v; want the baseline kept", b, err)
	}
	if b, err := p.Baseline(asUser(3)); err != nil || !b.IsZero() {
		t.Fatalf("the other owner's baseline: %v, %v; want none", b, err)
	}
	if id, err := p.FleetChat(asUser(3)); err != nil || id != 0 {
		t.Fatalf("no fleet chat yet: %d, %v", id, err)
	}
}
