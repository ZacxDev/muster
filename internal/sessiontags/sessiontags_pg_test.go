package sessiontags

import (
	"context"
	"errors"
	"io"
	"log"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// Users in this package's scratch database: 1 is the seeded operator (an
// admin); 2 and 3 are members.
func pgStore(t *testing.T) (*PGStore, *pgxpool.Pool) {
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
	dbtest.OwnerExec(t, dsn, `TRUNCATE session_tags`)
	dbtest.OwnerExec(t, dsn, `DELETE FROM agents WHERE name LIKE 'tag-%'`)
	return NewPG(pool), pool
}

func as(uid int64) context.Context {
	role := authz.RoleMember
	if uid == authz.OperatorUserID {
		role = authz.RoleAdmin
	}
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: role, Via: authz.ViaSession})
}

func mustAgent(t *testing.T, pool *pgxpool.Pool, uid int64, name string) agents.Agent {
	t.Helper()
	a, err := agents.NewPG(pool).Create(as(uid), agents.Agent{Name: name, Namespace: "devpod-" + name, DisplayName: name,
		Status: agents.StatusRunning, HooksToken: "hooks-" + name})
	if err != nil {
		t.Fatalf("agent %s: %v", name, err)
	}
	return a
}

func mustFor(t *testing.T, s *PGStore, uid int64, tg Target) []string {
	t.Helper()
	got, err := s.For(as(uid), tg)
	if err != nil {
		t.Fatalf("for %+v as %d: %v", tg, uid, err)
	}
	return got
}

func systemCount(t *testing.T, pool *pgxpool.Pool, where string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM session_tags `+where, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

var sess = Target{Host: "laptop", Session: "scratch2"}

// TestAddListRemove: a tag added to an agent and to a mirrored session reads
// back from For and List, sorted; removing one leaves the other; the answer of
// each write is the subject's tags after it.
func TestAddListRemove(t *testing.T) {
	s, pool := pgStore(t)
	a := mustAgent(t, pool, 2, "tag-alr")
	agent := Target{AgentID: a.ID}
	for _, tag := range []string{"zeta", "alpha"} {
		if _, err := s.Add(as(2), agent, tag); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Add(as(2), sess, "  Needs Review ")
	if err != nil || !reflect.DeepEqual(got, []string{"needs-review"}) {
		t.Fatalf("add to a mirror: %q, %v; want the normalized tag", got, err)
	}
	if got := mustFor(t, s, 2, agent); !reflect.DeepEqual(got, []string{"alpha", "zeta"}) {
		t.Fatalf("agent's tags = %q, want sorted [alpha zeta]", got)
	}
	set, err := s.List(as(2))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(set.Agent(a.ID), []string{"alpha", "zeta"}) || !reflect.DeepEqual(set.Mirror("laptop", "scratch2"), []string{"needs-review"}) {
		t.Fatalf("list = %+v", set)
	}
	// Another session on the same host, and the same session on another host,
	// are other subjects.
	if got := mustFor(t, s, 2, Target{Host: "laptop", Session: "scratch3"}); len(got) != 0 {
		t.Fatalf("another session carries %q", got)
	}
	if got := mustFor(t, s, 2, Target{Host: "desk", Session: "scratch2"}); len(got) != 0 {
		t.Fatalf("the same session on another host carries %q", got)
	}
	got, err = s.Remove(as(2), agent, "ZETA")
	if err != nil || !reflect.DeepEqual(got, []string{"alpha"}) {
		t.Fatalf("remove (spelled in upper case): %q, %v; want [alpha]", got, err)
	}
	// Idempotent.
	if got, err := s.Remove(as(2), agent, "zeta"); err != nil || !reflect.DeepEqual(got, []string{"alpha"}) {
		t.Fatalf("second remove: %q, %v", got, err)
	}
	if got := mustFor(t, s, 2, sess); !reflect.DeepEqual(got, []string{"needs-review"}) {
		t.Fatalf("removing an agent's tag touched the mirror's: %q", got)
	}
}

// TestATagIsDedupedCaseInsensitively: the same tag in three spellings is one row.
func TestATagIsDedupedCaseInsensitively(t *testing.T) {
	s, pool := pgStore(t)
	for _, raw := range []string{"Deploy Blocker", "deploy-blocker", "DEPLOY   BLOCKER"} {
		if _, err := s.Add(as(2), sess, raw); err != nil {
			t.Fatalf("add %q: %v", raw, err)
		}
	}
	if got := mustFor(t, s, 2, sess); !reflect.DeepEqual(got, []string{"deploy-blocker"}) {
		t.Fatalf("tags = %q, want one deploy-blocker", got)
	}
	if n := systemCount(t, pool, `WHERE owner_id = 2`); n != 1 {
		t.Fatalf("%d rows, want 1", n)
	}
}

// TestTheLimitsHold: a tag over MaxTagRunes, an illegal character and an empty
// tag are refused with no row; the MaxPerTarget+1-th distinct tag is refused,
// re-adding one it already carries at the cap is not, and another subject is
// unaffected.
func TestTheLimitsHold(t *testing.T) {
	s, pool := pgStore(t)
	for _, raw := range []string{strings.Repeat("x", MaxTagRunes+1), "no;semicolons", "a:b:c", "   ", ""} {
		_, err := s.Add(as(2), sess, raw)
		if err == nil {
			t.Fatalf("add %q was accepted", raw)
		}
		if !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrEmpty) {
			t.Fatalf("add %q: %v, want ErrInvalid or ErrEmpty", raw, err)
		}
	}
	// The boundary itself is accepted (the control for the length refusal).
	if _, err := s.Add(as(2), sess, strings.Repeat("x", MaxTagRunes)); err != nil {
		t.Fatalf("a tag of exactly %d runes: %v", MaxTagRunes, err)
	}
	for i := 1; i < MaxPerTarget; i++ {
		if _, err := s.Add(as(2), sess, "t"+strconv.Itoa(i)); err != nil {
			t.Fatalf("tag %d: %v", i, err)
		}
	}
	if n := systemCount(t, pool, `WHERE owner_id = 2`); n != MaxPerTarget {
		t.Fatalf("%d rows at the cap, want %d", n, MaxPerTarget)
	}
	if _, err := s.Add(as(2), sess, "one-too-many"); !errors.Is(err, ErrTooMany) {
		t.Fatalf("the %d-th tag: %v, want ErrTooMany", MaxPerTarget+1, err)
	}
	if _, err := s.Add(as(2), sess, "T1"); err != nil {
		t.Fatalf("re-adding a tag it carries, at the cap: %v", err)
	}
	if n := systemCount(t, pool, `WHERE owner_id = 2`); n != MaxPerTarget {
		t.Fatalf("%d rows after the refusals, want %d", n, MaxPerTarget)
	}
	if _, err := s.Add(as(2), Target{Host: "laptop", Session: "other"}, "fine"); err != nil {
		t.Fatalf("another subject at the first's cap: %v", err)
	}
	if _, err := s.Add(as(3), sess, "fine"); err != nil {
		t.Fatalf("another owner, same subject: %v", err)
	}
}

// TestTheCapHoldsUnderConcurrentAdds: from one under the cap, eight distinct
// tags added at once land exactly one.
func TestTheCapHoldsUnderConcurrentAdds(t *testing.T) {
	s, pool := pgStore(t)
	for i := 1; i < MaxPerTarget; i++ {
		if _, err := s.Add(as(2), sess, "t"+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = s.Add(as(2), sess, "race"+strconv.Itoa(i))
		}(i)
	}
	wg.Wait()
	if n := systemCount(t, pool, `WHERE owner_id = 2`); n != MaxPerTarget {
		t.Fatalf("%d rows after concurrent adds, want exactly %d", n, MaxPerTarget)
	}
}

// TestTagsArePrivateToTheirOwner: user 2's tags — on an agent and on a
// mirrored session — are not in 3's reads or in the admin's (user 1); 3's
// remove of the same subject and tag leaves them; 3 cannot tag 2's agent; and
// a raw read through the application pool sees only the principal's rows. The
// positive control is 2's own read before and after.
func TestTagsArePrivateToTheirOwner(t *testing.T) {
	s, pool := pgStore(t)
	a := mustAgent(t, pool, 2, "tag-priv")
	agent := Target{AgentID: a.ID}
	if _, err := s.Add(as(2), agent, "mine"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(as(2), sess, "mine"); err != nil {
		t.Fatal(err)
	}
	if got := mustFor(t, s, 2, sess); !reflect.DeepEqual(got, []string{"mine"}) {
		t.Fatalf("2's own read = %q (the positive control)", got)
	}
	for _, uid := range []int64{3, authz.OperatorUserID} {
		if got := mustFor(t, s, uid, sess); len(got) != 0 {
			t.Fatalf("user %d reads 2's mirror tags: %q", uid, got)
		}
		if got := mustFor(t, s, uid, agent); len(got) != 0 {
			t.Fatalf("user %d reads 2's agent tags: %q", uid, got)
		}
		set, err := s.List(as(uid))
		if err != nil || len(set.Agents)+len(set.Mirrors) != 0 {
			t.Fatalf("user %d lists %+v (%v)", uid, set, err)
		}
		var n int
		if err := pool.QueryRow(as(uid), `SELECT count(*) FROM session_tags`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("user %d's raw read sees %d rows (%v)", uid, n, err)
		}
	}
	if _, err := s.Remove(as(3), sess, "mine"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Remove(as(3), agent, "mine"); err != nil {
		t.Fatal(err)
	}
	// A raw DELETE as 3 reaches none of 2's rows either.
	if tag, err := pool.Exec(as(3), `DELETE FROM session_tags`); err != nil || tag.RowsAffected() != 0 {
		t.Fatalf("3's raw delete: %v rows (%v)", tag.RowsAffected(), err)
	}
	if got := mustFor(t, s, 2, sess); !reflect.DeepEqual(got, []string{"mine"}) {
		t.Fatalf("3's removes reached 2's mirror tags: %q", got)
	}
	if got := mustFor(t, s, 2, agent); !reflect.DeepEqual(got, []string{"mine"}) {
		t.Fatalf("3's removes reached 2's agent tags: %q", got)
	}
	if _, err := s.Add(as(3), agent, "theirs"); !errors.Is(err, ErrNoSuchAgent) {
		t.Fatalf("3 tagging 2's agent: %v, want ErrNoSuchAgent", err)
	}
	// 3 cannot write a row owned by 2 either.
	if _, err := pool.Exec(as(3), `INSERT INTO session_tags (owner_id, kind, mirror_host, mirror_session, tag) VALUES (2, 'mirror', 'laptop', 'x', 'forged')`); err == nil {
		t.Fatal("3 inserted a row owned by 2")
	}
	if n := systemCount(t, pool, `WHERE owner_id = 3`); n != 0 {
		t.Fatalf("3 owns %d rows, want 0", n)
	}
	if n := systemCount(t, pool, ``); n != 2 {
		t.Fatalf("System sees %d rows, want 2", n)
	}
}

// TestDestroyingAnAgentDeletesItsTags: the FK cascade, as pins'. Another
// agent's tags and a mirror's stay.
func TestDestroyingAnAgentDeletesItsTags(t *testing.T) {
	s, pool := pgStore(t)
	gone := mustAgent(t, pool, 2, "tag-gone")
	kept := mustAgent(t, pool, 2, "tag-kept")
	for _, id := range []int64{gone.ID, kept.ID} {
		if _, err := s.Add(as(2), Target{AgentID: id}, "x"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Add(as(2), sess, "x"); err != nil {
		t.Fatal(err)
	}
	if err := agents.NewPG(pool).Delete(as(2), gone.ID); err != nil {
		t.Fatal(err)
	}
	if n := systemCount(t, pool, `WHERE agent_id = $1`, gone.ID); n != 0 {
		t.Fatalf("%d rows still name the deleted agent", n)
	}
	set, err := s.List(as(2))
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Agent(gone.ID)) != 0 || len(set.Agent(kept.ID)) != 1 || len(set.Mirror("laptop", "scratch2")) != 1 {
		t.Fatalf("after deleting agent %d: %+v", gone.ID, set)
	}
}

// TestABadTargetOrAbsentAgentIsRefused: no row lands for any of them.
func TestABadTargetOrAbsentAgentIsRefused(t *testing.T) {
	s, pool := pgStore(t)
	if _, err := s.Add(as(2), Target{AgentID: 987654321}, "x"); !errors.Is(err, ErrNoSuchAgent) {
		t.Fatalf("absent agent: %v, want ErrNoSuchAgent", err)
	}
	for _, tg := range []Target{
		{}, // nothing
		{AgentID: 1, Host: "laptop", Session: "s"}, // two subjects
		{AgentID: -1},
		{Host: "Laptop", Session: "s"}, // not a lowercase DNS label
		{Host: "laptop", Session: ""},
		{Host: "laptop", Session: "s\x01"},
	} {
		if _, err := s.Add(as(2), tg, "x"); !errors.Is(err, ErrBadTarget) {
			t.Errorf("add to %+v: %v, want ErrBadTarget", tg, err)
		}
	}
	if n := systemCount(t, pool, ``); n != 0 {
		t.Fatalf("refused adds left %d rows", n)
	}
}
