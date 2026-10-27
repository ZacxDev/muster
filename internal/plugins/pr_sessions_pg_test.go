package plugins

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/prroute"
)

// session_prs (migration 0055) against a real Postgres: which session most
// recently authored a pull request, for the principal only.

const (
	prSessA = "7d1e5b2a-0000-4000-8000-0000000000a1"
	prSessB = "7d1e5b2a-0000-4000-8000-0000000000b2"
	prSessC = "7d1e5b2a-0000-4000-8000-0000000000c3"
)

func prStore(t *testing.T) (*PGStore, string) {
	t.Helper()
	s, _, dsn := pgStore(t)
	dbtest.OwnerExec(t, dsn, `TRUNCATE session_prs`)
	return s, dsn
}

var (
	createSig = prroute.Signal{Kind: prroute.KindPRCreate, Repo: "acme-labs/widget-api", PR: 4817}
	pushSig   = prroute.Signal{Kind: prroute.KindPush, Repo: "acme-labs/widget-api", HeadRef: "feat/gizmo-route"}
	refPR     = prroute.Ref{Repo: "acme-labs/widget-api", PR: 4817, HeadSHA: "3f2a9c0e7b14d6a85c3e9f01b2d47a6e8c5f1b09"}
)

func record(t *testing.T, s *PGStore, uid int64, sess, host string, bump bool, sigs ...prroute.Signal) {
	t.Helper()
	if err := s.RecordPRSessions(session(uid), sess, host, sigs, bump); err != nil {
		t.Fatalf("record %s for %d: %v", sess, uid, err)
	}
}

// age sets every row of sess to have been last seen ago.
func age(t *testing.T, dsn, sess string, ago time.Duration) {
	t.Helper()
	dbtest.OwnerExec(t, dsn, `UPDATE session_prs SET last_seen = now() - make_interval(secs => $2) WHERE claude_session = $1`, sess, ago.Seconds())
}

func resolve(t *testing.T, s *PGStore, uid int64, ref prroute.Ref) (PRSession, bool) {
	t.Helper()
	got, ok, err := s.ResolvePRSession(session(uid), ref)
	if err != nil {
		t.Fatalf("resolve for %d: %v", uid, err)
	}
	return got, ok
}

func TestThePRsMostRecentAuthorIsResolved(t *testing.T) {
	s, dsn := prStore(t)
	record(t, s, 2, prSessA, "lab-box-9", true, createSig)
	record(t, s, 2, prSessB, "desk-box-4", true, createSig)
	age(t, dsn, prSessA, 2*time.Hour)
	age(t, dsn, prSessB, time.Hour)
	got, ok := resolve(t, s, 2, refPR)
	if !ok || got.ClaudeSession != prSessB || got.Host != "desk-box-4" || got.Kind != prroute.KindPRCreate ||
		got.Repo != "acme-labs/widget-api" || got.PR != 4817 {
		t.Fatalf("resolved %+v, %v; want B, the more recent", got, ok)
	}
	// A seen again (bump) is now the most recent.
	record(t, s, 2, prSessA, "lab-box-9", true, createSig)
	if got, _ := resolve(t, s, 2, refPR); got.ClaudeSession != prSessA {
		t.Fatalf("after A was seen again: %+v", got)
	}
	var n int
	if err := s.pool.QueryRow(session(2), `SELECT count(*) FROM session_prs`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("%d rows (%v); want 2 — seeing a signal again updates its row", n, err)
	}
}

func TestARecordWithoutBumpKeepsLastSeen(t *testing.T) {
	s, dsn := prStore(t)
	record(t, s, 2, prSessA, "lab-box-9", true, createSig)
	record(t, s, 2, prSessB, "desk-box-4", true, createSig)
	age(t, dsn, prSessA, 2*time.Hour)
	age(t, dsn, prSessB, time.Hour)
	// A backfill re-reading A's old signal must not make it the newest.
	record(t, s, 2, prSessA, "lab-box-9", false, createSig)
	if got, _ := resolve(t, s, 2, refPR); got.ClaudeSession != prSessB {
		t.Fatalf("a re-read without bump moved A ahead: %+v", got)
	}
	// Control: the same record WITH bump does move it.
	record(t, s, 2, prSessA, "lab-box-9", true, createSig)
	if got, _ := resolve(t, s, 2, refPR); got.ClaudeSession != prSessA {
		t.Fatalf("control: a bump did not move A ahead: %+v", got)
	}
}

func TestAPushResolvesByItsHeadRefOnly(t *testing.T) {
	s, _ := prStore(t)
	record(t, s, 2, prSessC, "lab-box-9", true, pushSig)
	withRef := refPR
	withRef.HeadRef = "feat/gizmo-route"
	if got, ok := resolve(t, s, 2, withRef); !ok || got.ClaudeSession != prSessC || got.Kind != prroute.KindPush || got.HeadRef != "feat/gizmo-route" {
		t.Fatalf("by head_ref: %+v, %v", got, ok)
	}
	// No head_ref in the mail: a push alone names no pull request number.
	if got, ok := resolve(t, s, 2, refPR); ok {
		t.Fatalf("a mail with no head_ref resolved a push: %+v", got)
	}
	// Another branch, or the same branch on another repo, is not it.
	for _, r := range []prroute.Ref{{Repo: "acme-labs/widget-api", PR: 4817, HeadRef: "feat/other-branch"},
		{Repo: "acme-labs/gadget-api", PR: 4817, HeadRef: "feat/gizmo-route"}} {
		if got, ok := resolve(t, s, 2, r); ok {
			t.Fatalf("%+v resolved %+v", r, got)
		}
	}
	// A pull request number on another repo is not it either.
	record(t, s, 2, prSessA, "lab-box-9", true, createSig)
	if got, ok := resolve(t, s, 2, prroute.Ref{Repo: "acme-labs/gadget-api", PR: 4817}); ok {
		t.Fatalf("another repo's #4817 resolved %+v", got)
	}
}

func TestAnotherOwnersSessionIsNeverResolved(t *testing.T) {
	s, dsn := prStore(t)
	record(t, s, 2, prSessA, "lab-box-9", true, createSig)
	record(t, s, 3, prSessB, "desk-box-4", true, createSig)
	age(t, dsn, prSessA, 2*time.Hour) // the other owner's row is the newer one
	if got, ok := resolve(t, s, 2, refPR); !ok || got.ClaudeSession != prSessA {
		t.Fatalf("owner 2 resolved %+v, %v; want its own A", got, ok)
	}
	if got, ok := resolve(t, s, 3, refPR); !ok || got.ClaudeSession != prSessB {
		t.Fatalf("owner 3 resolved %+v, %v; want its own B", got, ok)
	}
	dbtest.OwnerExec(t, dsn, `DELETE FROM session_prs WHERE owner_id = 2`)
	if got, ok := resolve(t, s, 2, refPR); ok {
		t.Fatalf("owner 2, with no row of its own, resolved %+v", got)
	}
}

func TestPrunePRSessionsDeletesOnlyTheCallersOldRows(t *testing.T) {
	s, dsn := prStore(t)
	record(t, s, 2, prSessA, "lab-box-9", true, createSig)
	record(t, s, 2, prSessB, "lab-box-9", true, pushSig)
	record(t, s, 3, prSessC, "desk-box-4", true, createSig)
	age(t, dsn, prSessA, 40*24*time.Hour)
	age(t, dsn, prSessC, 40*24*time.Hour)
	n, err := s.PrunePRSessions(session(2), time.Now().Add(-30*24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("pruned %d (%v); want 1 (owner 2's old row only)", n, err)
	}
	if _, ok := resolve(t, s, 3, refPR); !ok {
		t.Fatal("owner 3's old row was pruned by owner 2")
	}
}

func TestPRSessionOwnersAreTheEnabledUsers(t *testing.T) {
	s, _ := prStore(t)
	owners, err := s.PRSessionOwners(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(owners, 2) || !slices.Contains(owners, 3) || slices.Contains(owners, 4) {
		t.Fatalf("owners %v; want 2 and 3, not the disabled 4", owners)
	}
}
