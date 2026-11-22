package attention

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/users"
)

// A SESSION'S ASKS (session_asks.go), on a real Postgres. Every fixture's
// host, session key and question is distinct from every other's, so a
// predicate the query drops lets in exactly one row the test names.

// sessionAskIDs is what SessionAsks returned for q as ctx's principal, as ids
// in order.
func sessionAskIDs(t *testing.T, s *PGStore, ctx context.Context, q SessionAsksQuery) []int64 {
	t.Helper()
	got, err := s.SessionAsks(ctx, q)
	if err != nil {
		t.Fatalf("session asks %+v: %v", q, err)
	}
	ids := make([]int64, 0, len(got))
	for _, a := range got {
		ids = append(ids, a.ID)
	}
	return ids
}

// The session under test runs two keys on host "desk"; another session runs a
// third on the same host; the same key on host "attic" is another host's.
const (
	sessHost  = "desk"
	sessKeyA  = "0c1d2e3f-4a5b-4c6d-8e7f-0a1b2c3d4e01"
	sessKeyB  = "0c1d2e3f-4a5b-4c6d-8e7f-0a1b2c3d4e02"
	sessOther = "0c1d2e3f-4a5b-4c6d-8e7f-0a1b2c3d4e03"
	otherHost = "attic"
)

func sessAsker(host, key string) Asker {
	return Asker{MirrorHost: host, MirrorSession: key, Label: "work:2 @ " + host}
}

// TestSessionAsksListsOneSessionsOpenThenAnsweredNewestFirst: open asks
// first, newest first; then the answered ones (on the page, and by a message
// in the session), newest first. Not listed: another session's on the same
// host, the same key on another host, a dismissed and an expired one, a
// question to the chief, a notice, and an agent's.
func TestSessionAsksListsOneSessionsOpenThenAnsweredNewestFirst(t *testing.T) {
	s, pool, dsn := pgStore(t)
	const owner = int64(2)
	ctx := as(owner)
	a, b := sessAsker(sessHost, sessKeyA), sessAsker(sessHost, sessKeyB)
	// Created in this order; created_at is moved below so each is distinct.
	answeredOld := mustAsk(t, s, owner, New{Asker: a, Question: "the oldest, answered on the page"})
	openOld := mustAsk(t, s, owner, New{Asker: b, Question: "an old open one, from the second key"})
	inSession := mustAsk(t, s, owner, New{Asker: b, Question: "answered by a message in the session"})
	openNew := mustAsk(t, s, owner, New{Asker: a, Question: "the newest open one"})
	answeredNew := mustAsk(t, s, owner, New{Asker: a, Question: "the newest answered"})
	dismissed := mustAsk(t, s, owner, New{Asker: a, Question: "dismissed"})
	expired := mustAsk(t, s, owner, New{Asker: a, Question: "expired"})
	chief := mustAsk(t, s, owner, New{Asker: a, Question: "to the chief", ToChief: true})
	// ⚠ AN INVARIANT GUARD, NOT REGRESSION COVERAGE: a notice is muster's own
	// (attention_asks_kind_check: system_asker), so it can carry no session
	// and no query by session could list it.
	notice := mustNotice(t, s, owner, "a notice")
	otherSess := mustAsk(t, s, owner, New{Asker: sessAsker(sessHost, sessOther), Question: "another session's"})
	otherHosts := mustAsk(t, s, owner, New{Asker: sessAsker(otherHost, sessKeyA), Question: "the same key on another host"})
	agentAsk := mustAsk(t, s, owner, New{Asker: Asker{AgentID: agentFor(t, pool, owner)}, Question: "an agent's"})

	for i, x := range []Ask{answeredOld, openOld, inSession, openNew, answeredNew, dismissed, expired, chief, notice, otherSess, otherHosts, agentAsk} {
		dbtest.OwnerExec(t, dsn, `UPDATE attention_asks SET created_at = now() - make_interval(mins => `+strconv.Itoa(100-i)+`) WHERE id = `+strconv.FormatInt(x.ID, 10))
	}
	if _, err := s.Answer(ctx, answeredOld.ID, "yes, the old one"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Answer(ctx, answeredNew.ID, "yes, the new one"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.AnswerInSession(ctx, b, "typed into the session", 0, func(x Ask) bool { return x.ID != inSession.ID }); err != nil || len(got) != 1 || got[0].ID != inSession.ID {
		t.Fatalf("answer in session: %v %v", got, err)
	}
	if _, err := s.Cancel(ctx, dismissed.ID); err != nil {
		t.Fatal(err)
	}
	dbtest.OwnerExec(t, dsn, `UPDATE attention_asks SET expires_at = created_at + interval '1 second' WHERE id = `+strconv.FormatInt(expired.ID, 10))

	q := SessionAsksQuery{MirrorHost: sessHost, MirrorSessions: []string{sessKeyA, sessKeyB}}
	want := []int64{openNew.ID, openOld.ID, answeredNew.ID, inSession.ID, answeredOld.ID}
	if got := sessionAskIDs(t, s, ctx, q); !slices.Equal(got, want) {
		t.Fatalf("the session's asks: %v, want %v (open newest first, then answered newest first)", got, want)
	}
	// One key of the two: only that key's.
	if got := sessionAskIDs(t, s, ctx, SessionAsksQuery{MirrorHost: sessHost, MirrorSessions: []string{sessKeyB}}); !slices.Equal(got, []int64{openOld.ID, inSession.ID}) {
		t.Errorf("key B alone: %v, want [%d %d]", got, openOld.ID, inSession.ID)
	}
	// The other session and the other host each list their own (controls:
	// the rows exist and are readable).
	if got := sessionAskIDs(t, s, ctx, SessionAsksQuery{MirrorHost: sessHost, MirrorSessions: []string{sessOther}}); !slices.Equal(got, []int64{otherSess.ID}) {
		t.Errorf("control: the other session's: %v, want [%d]", got, otherSess.ID)
	}
	if got := sessionAskIDs(t, s, ctx, SessionAsksQuery{MirrorHost: otherHost, MirrorSessions: []string{sessKeyA}}); !slices.Equal(got, []int64{otherHosts.ID}) {
		t.Errorf("control: the other host's: %v, want [%d]", got, otherHosts.ID)
	}
	// An agent's, by its id alone.
	if got := sessionAskIDs(t, s, ctx, SessionAsksQuery{AgentID: agentAsk.Asker.AgentID}); !slices.Equal(got, []int64{agentAsk.ID}) {
		t.Errorf("the agent's: %v, want [%d]", got, agentAsk.ID)
	}
	// A query that names no session lists nothing (not every ask).
	for _, bad := range []SessionAsksQuery{{}, {MirrorHost: sessHost}, {MirrorSessions: []string{sessKeyA}}, {AgentID: 1, MirrorHost: sessHost, MirrorSessions: []string{sessKeyA}}} {
		if got := sessionAskIDs(t, s, ctx, bad); len(got) != 0 {
			t.Errorf("%+v listed %v, want none", bad, got)
		}
	}
}

// TestSessionAsksIsCapped: more than the limit lists the limit, the open ones
// kept ahead of every answered one; no limit is MaxSessionAsks.
func TestSessionAsksIsCapped(t *testing.T) {
	s, _, _ := pgStore(t)
	const owner = int64(2)
	ctx := as(owner)
	a := sessAsker(sessHost, sessKeyA)
	var opens []int64
	for i := range 3 {
		x := mustAsk(t, s, owner, New{Asker: a, Question: "open " + strconv.Itoa(i)})
		opens = append(opens, x.ID)
	}
	for i := range MaxSessionAsks + 2 {
		x := mustAsk(t, s, owner, New{Asker: a, Question: "answered " + strconv.Itoa(i)})
		if _, err := s.Answer(ctx, x.ID, "ok"); err != nil {
			t.Fatal(err)
		}
	}
	q := SessionAsksQuery{MirrorHost: sessHost, MirrorSessions: []string{sessKeyA}}
	got := sessionAskIDs(t, s, ctx, q)
	if len(got) != MaxSessionAsks {
		t.Fatalf("no limit listed %d, want MaxSessionAsks (%d)", len(got), MaxSessionAsks)
	}
	slices.Reverse(opens)
	if !slices.Equal(got[:3], opens) {
		t.Errorf("the first three are %v, want the open ones newest first %v", got[:3], opens)
	}
	q.Limit = 4
	if got := sessionAskIDs(t, s, ctx, q); len(got) != 4 || !slices.Equal(got[:3], opens) {
		t.Errorf("limit 4 listed %v, want 4 starting with %v", got, opens)
	}
	q.Limit = 10_000
	if got := sessionAskIDs(t, s, ctx, q); len(got) != MaxSessionAsks {
		t.Errorf("limit 10000 listed %d, want MaxSessionAsks (%d)", len(got), MaxSessionAsks)
	}
}

// TestSessionAsksIsTheOwnersOwnOnly: a team's ask from the SAME host and
// session key is readable to its member (the control proves row-level
// security lets it through, so it is the query's owner condition that keeps
// it out), and another person's from the same host and key is not theirs;
// the member's list holds only their own. RED with the owner condition
// removed: the team's ask is listed.
func TestSessionAsksIsTheOwnersOwnOnly(t *testing.T) {
	s, pool, dsn := pgStore(t)
	ctx := context.Background()
	dbtest.OwnerExec(t, dsn, `SELECT setval(pg_get_serial_sequence('public.users', 'id'), (SELECT max(id) FROM public.users))`)
	us := users.NewPG(pool)
	by := users.Actor{UserID: 1, Via: authz.ViaSession}
	var member int64
	if err := pool.QueryRow(authz.System(ctx), `INSERT INTO users (role, display_name) VALUES ('member', 'Sam Session') RETURNING id`).Scan(&member); err != nil {
		t.Fatal(err)
	}
	team, err := us.CreateTeam(ctx, by, "Sessioners "+strconv.FormatInt(time.Now().UnixNano()%1e9, 36), member)
	if err != nil {
		t.Fatal(err)
	}
	acct, err := us.Account(authz.System(ctx), member)
	if err != nil {
		t.Fatal(err)
	}
	me := authz.WithPrincipal(ctx, authz.Principal{UserID: member, Role: acct.Role, Via: authz.ViaSession, Teams: acct.Teams})
	asTeam := authz.WithPrincipal(ctx, authz.Principal{UserID: team.ID, Role: authz.RoleMember, Via: authz.ViaAgent, Team: true})
	a := sessAsker(sessHost, sessKeyA)

	mine, err := s.Create(me, New{Asker: a, Question: "the member's own"})
	if err != nil {
		t.Fatal(err)
	}
	teams, err := s.Create(asTeam, New{Asker: a, Question: "the team's, same host and key"})
	if err != nil {
		t.Fatal(err)
	}
	others := mustAsk(t, s, 3, New{Asker: a, Question: "another person's, same host and key"})

	if x, err := s.Get(me, teams.ID); err != nil || x.ID != teams.ID {
		t.Fatalf("control: the member reads the team's ask %d: %+v %v", teams.ID, x, err)
	}
	q := SessionAsksQuery{MirrorHost: sessHost, MirrorSessions: []string{sessKeyA}}
	if got := sessionAskIDs(t, s, asTeam, q); !slices.Equal(got, []int64{teams.ID}) {
		t.Fatalf("control: the team's own list: %v, want [%d]", got, teams.ID)
	}
	if got := sessionAskIDs(t, s, as(3), q); !slices.Equal(got, []int64{others.ID}) {
		t.Fatalf("control: the other person's own list: %v, want [%d]", got, others.ID)
	}
	if got := sessionAskIDs(t, s, me, q); !slices.Equal(got, []int64{mine.ID}) {
		t.Errorf("the member's list: %v, want only their own %d (not the team's %d or the other person's %d)", got, mine.ID, teams.ID, others.ID)
	}
}
