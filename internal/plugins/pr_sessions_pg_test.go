package plugins

import (
	"errors"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/prroute"
)

// session_prs (migration 0055) against a real Postgres: a host hook's report
// stored, and which session most recently authored a pull request, for the
// principal only.

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

func record(t *testing.T, s *PGStore, uid int64, sess, host string, sig prroute.Signal) bool {
	t.Helper()
	created, err := s.RecordPRSession(session(uid), sess, host, sig)
	if err != nil {
		t.Fatalf("record %s for %d: %v", sess, uid, err)
	}
	return created
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
	if !record(t, s, 2, prSessA, "lab-box-9", createSig) {
		t.Fatal("a first report was not created")
	}
	record(t, s, 2, prSessB, "desk-box-4", createSig)
	age(t, dsn, prSessA, 2*time.Hour)
	age(t, dsn, prSessB, time.Hour)
	got, ok := resolve(t, s, 2, refPR)
	if !ok || got.ClaudeSession != prSessB || got.Host != "desk-box-4" || got.Kind != prroute.KindPRCreate ||
		got.Repo != "acme-labs/widget-api" || got.PR != 4817 {
		t.Fatalf("resolved %+v, %v; want B, the more recent", got, ok)
	}
	// A reported again is now the most recent, and its row is the same row.
	if record(t, s, 2, prSessA, "lab-box-9", createSig) {
		t.Fatal("a repeat report created a second row")
	}
	if got, _ := resolve(t, s, 2, refPR); got.ClaudeSession != prSessA {
		t.Fatalf("after A was reported again: %+v", got)
	}
	var n int
	if err := s.pool.QueryRow(session(2), `SELECT count(*) FROM session_prs`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("%d rows (%v); want 2", n, err)
	}
}

func TestAReportWithNoHostKeepsAndLacksOne(t *testing.T) {
	s, _ := prStore(t)
	record(t, s, 2, prSessA, "", createSig)
	if got, ok := resolve(t, s, 2, refPR); !ok || got.Host != "" {
		t.Fatalf("no host: %+v, %v", got, ok)
	}
	record(t, s, 2, prSessA, "lab-box-9", createSig)
	record(t, s, 2, prSessA, "", createSig) // a later report with no window keeps the known host
	if got, _ := resolve(t, s, 2, refPR); got.Host != "lab-box-9" {
		t.Fatalf("host %q; want the last known one kept", got.Host)
	}
}

func TestAPushResolvesByItsHeadRefOnly(t *testing.T) {
	s, _ := prStore(t)
	record(t, s, 2, prSessC, "lab-box-9", pushSig)
	withRef := refPR
	withRef.HeadRef = "feat/gizmo-route"
	if got, ok := resolve(t, s, 2, withRef); !ok || got.ClaudeSession != prSessC || got.Kind != prroute.KindPush || got.HeadRef != "feat/gizmo-route" {
		t.Fatalf("by head_ref: %+v, %v", got, ok)
	}
	// No head_ref in the mail: a push alone names no pull request number.
	if got, ok := resolve(t, s, 2, refPR); ok {
		t.Fatalf("a mail with no head_ref resolved a push: %+v", got)
	}
	for _, r := range []prroute.Ref{{Repo: "acme-labs/widget-api", PR: 4817, HeadRef: "feat/other-branch"},
		{Repo: "acme-labs/gadget-api", PR: 4817, HeadRef: "feat/gizmo-route"}} {
		if got, ok := resolve(t, s, 2, r); ok {
			t.Fatalf("%+v resolved %+v", r, got)
		}
	}
	record(t, s, 2, prSessA, "lab-box-9", createSig)
	if got, ok := resolve(t, s, 2, prroute.Ref{Repo: "acme-labs/gadget-api", PR: 4817}); ok {
		t.Fatalf("another repo's #4817 resolved %+v", got)
	}
}

func TestACreateNamingItsHeadMatchesByEither(t *testing.T) {
	s, _ := prStore(t)
	sig := createSig
	sig.HeadRef = "feat/headed-create"
	record(t, s, 2, prSessB, "lab-box-9", sig)
	if got, ok := resolve(t, s, 2, prroute.Ref{Repo: "acme-labs/widget-api", PR: 9999, HeadRef: "feat/headed-create"}); !ok || got.ClaudeSession != prSessB {
		t.Fatalf("by its head branch: %+v, %v", got, ok)
	}
	if got, ok := resolve(t, s, 2, refPR); !ok || got.ClaudeSession != prSessB {
		t.Fatalf("by its number: %+v, %v", got, ok)
	}
}

func TestAMalformedReportIsRefused(t *testing.T) {
	s, _ := prStore(t)
	for _, sig := range []prroute.Signal{
		{Kind: prroute.KindPRCreate, Repo: "acme-labs/widget-api"},
		{Kind: prroute.KindPush, Repo: "acme-labs/widget-api", PR: 4817, HeadRef: "feat/x"},
		{Kind: prroute.KindPush, Repo: "acme-labs/widget-api"},
		{Kind: "git-fetch", Repo: "acme-labs/widget-api", PR: 4817},
		{Kind: prroute.KindPRCreate, Repo: "widget-api", PR: 4817},
	} {
		if _, err := s.RecordPRSession(session(2), prSessA, "", sig); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v; want ErrInvalid", sig, err)
		}
	}
}

func TestAnotherOwnersSessionIsNeverResolved(t *testing.T) {
	s, dsn := prStore(t)
	record(t, s, 2, prSessA, "lab-box-9", createSig)
	record(t, s, 3, prSessB, "desk-box-4", createSig)
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

func TestAReportPrunesOnlyTheCallersOldRows(t *testing.T) {
	s, dsn := prStore(t)
	record(t, s, 2, prSessA, "lab-box-9", createSig)
	record(t, s, 3, prSessC, "desk-box-4", createSig)
	age(t, dsn, prSessA, 40*24*time.Hour)
	age(t, dsn, prSessC, 40*24*time.Hour)
	record(t, s, 2, prSessB, "lab-box-9", pushSig)
	var n int
	if err := s.pool.QueryRow(session(2), `SELECT count(*) FROM session_prs WHERE claude_session = $1`, prSessA).Scan(&n); err != nil || n != 0 {
		t.Fatalf("owner 2's 40-day-old row: %d (%v); want pruned", n, err)
	}
	if _, ok := resolve(t, s, 3, refPR); !ok {
		t.Fatal("owner 3's old row was pruned by owner 2's report")
	}
}
