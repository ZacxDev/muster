package notes

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// 🔴 FRAMING. Task threads are a NEW feature, so the tests in this file are
// INVARIANT GUARDS, not regression tests: they pin rules the old code never had a
// chance to violate (there was no link table to get wrong). They are labelled as
// such deliberately — calling them regression coverage would claim evidence that
// does not exist. What makes them worth having is the mutation sweep: each one is
// paired with a specific mutation of the rule it pins, and was watched to fail
// with ITS OWN assertion message when that mutation was applied.

// TestUpgradeRoleIsMonotonic is the invariant guard on the role rule, table-driven
// in BOTH directions — an upgrade must take, a downgrade must not, and `created`
// must survive everything.
//
// The both-directions part is the point. A one-directional table ("read → worked
// upgrades") is satisfied by an implementation that simply takes whatever arrives
// last, which is the exact bug the monotonicity rule exists to prevent.
func TestUpgradeRoleIsMonotonic(t *testing.T) {
	cases := []struct {
		name      string
		cur, next string
		want      string
	}{
		// Upgrades: must move.
		{"read to worked upgrades", RoleRead, RoleWorked, RoleWorked},
		{"read to created upgrades", RoleRead, RoleCreated, RoleCreated},
		{"worked to created upgrades", RoleWorked, RoleCreated, RoleCreated},
		// Downgrades: must NOT move.
		{"worked stays worked on a later read", RoleWorked, RoleRead, RoleWorked},
		{"created stays created on a later read", RoleCreated, RoleRead, RoleCreated},
		{"created stays created on a later worked", RoleCreated, RoleWorked, RoleCreated},
		// Idempotent.
		{"read to read", RoleRead, RoleRead, RoleRead},
		{"worked to worked", RoleWorked, RoleWorked, RoleWorked},
		{"created to created", RoleCreated, RoleCreated, RoleCreated},
		// An unrecognised incoming role ranks below everything and can never win.
		{"unknown never displaces a real role", RoleRead, "bogus", RoleRead},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := UpgradeRole(tc.cur, tc.next); got != tc.want {
				t.Fatalf("UpgradeRole(%q, %q) = %q, want %q — the role model is monotonic: read < worked < created, and `created` is terminal",
					tc.cur, tc.next, got, tc.want)
			}
		})
	}
}

// TestRoleRankMatchesSQLOrdering is a SEAM guard: roleRank (Go, used by the
// in-memory fake and by UpgradeRole) and the ARRAY literal inside roleUpgradeExpr
// (SQL, used by the only implementation that runs in production) are two spellings
// of one ordering, and nothing else forces them to agree.
//
// A CHECK constraint would not catch a reversed array — every value stays legal,
// the rule just silently inverts into "always downgrade". So the test reads the
// order out of the SQL text itself rather than restating it: a hand-written
// expected list here would be a THIRD copy that could drift with the other two.
func TestRoleRankMatchesSQLOrdering(t *testing.T) {
	m := regexp.MustCompile(`ARRAY\[([^\]]+)\]`).FindStringSubmatch(roleUpgradeExpr)
	if m == nil {
		t.Fatalf("could not find an ARRAY[...] literal in roleUpgradeExpr — the ordering seam is unreadable:\n%s", roleUpgradeExpr)
	}
	var sqlOrder []string
	for _, raw := range strings.Split(m[1], ",") {
		sqlOrder = append(sqlOrder, strings.Trim(strings.TrimSpace(raw), "'"))
	}
	// array_position is 1-based; roleRank is 0-based. The RELATIVE order is what
	// must match.
	for i, role := range sqlOrder {
		if got := roleRank(role); got != i {
			t.Fatalf("SQL ranks %q at position %d but roleRank says %d — the Go and SQL role orderings have drifted, which silently inverts the monotonic rule.\nSQL order: %v",
				role, i, got, sqlOrder)
		}
	}
	if len(sqlOrder) != 3 {
		t.Fatalf("SQL role array has %d entries (%v), want exactly the 3 roles — a missing entry ranks NULL and disables the upgrade for it", len(sqlOrder), sqlOrder)
	}
}

func TestValidRoleAcceptsExactlyTheThreeRoles(t *testing.T) {
	for _, ok := range []string{RoleRead, RoleWorked, RoleCreated} {
		if !ValidRole(ok) {
			t.Errorf("ValidRole(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "READ", "Worked", "owner", "deleted"} {
		if ValidRole(bad) {
			t.Errorf("ValidRole(%q) = true, want false — the role vocabulary is closed and matches the column's CHECK constraint", bad)
		}
	}
}

// TestMaxTaskSessionsMagnitude pins the LITERAL value of the cap.
//
// 🔴 IT IS NOT REDUNDANT WITH THE CAP TESTS, it is the thing they structurally
// cannot see. Every other cap test loops `for i := 0; i < MaxTaskSessions; i++`
// and builds its expectations from the same constant, so it passes identically at
// 1 and at 100000 — a whole class of mutation (change the number) survives the
// entire suite. At 1 a task keeps one breadcrumb; at 100000 the bound may as well
// not exist. Only a literal catches that.
//
// If you are deliberately retuning the cap, change the literal here in the same
// commit and say why in the message.
func TestMaxTaskSessionsMagnitude(t *testing.T) {
	if MaxTaskSessions != 50 {
		t.Fatalf("MaxTaskSessions = %d, want the literal 50 — every other cap test derives its expectations from this constant and cannot notice it moving", MaxTaskSessions)
	}
}

// TestErrTooManySessionsIsNotARequestFailure guards the DECISION, not the
// spelling: the cap sentinel exists to say "the breadcrumb was skipped", and
// nothing may map it to an HTTP status.
//
// The earlier design turned it into a 409, which produced a split write on
// PATCH …/status and a permanent 409 on plain GETs. Callers now log and count it,
// so the sentinel's job is to be RECOGNISABLE (errors.Is) and to read as a skip
// rather than a refusal — if someone re-words it as a refusal, that is the first
// step back toward failing requests on it.
func TestErrTooManySessionsIsNotARequestFailure(t *testing.T) {
	wrapped := fmt.Errorf("%w: task %d", ErrTooManySessions, 7)
	if !errors.Is(wrapped, ErrTooManySessions) {
		t.Fatalf("a wrapped cap error is not errors.Is-recognisable; callers key their log-and-continue on that")
	}
	if !strings.Contains(ErrTooManySessions.Error(), "skipped") {
		t.Fatalf("the cap sentinel reads %q — it must read as a SKIPPED breadcrumb, not as a refusal, because no route may fail on it", ErrTooManySessions)
	}
}

// TestGroupSessionLinksIsIdempotentAndDropsOrphans mirrors the existing
// groupNoteChildren guard: re-grouping must not double a task's thread, and a link
// whose note is not in the set must be dropped rather than panicking.
func TestGroupSessionLinksIsIdempotentAndDropsOrphans(t *testing.T) {
	list := []Note{{ID: 1}, {ID: 2}}
	links := []noteSessionLink{
		{NoteID: 1, Link: SessionLink{SessionID: "a", Role: RoleCreated}},
		{NoteID: 1, Link: SessionLink{SessionID: "b", Role: RoleRead}},
		{NoteID: 99, Link: SessionLink{SessionID: "orphan", Role: RoleRead}},
	}
	groupSessionLinks(list, links)
	groupSessionLinks(list, links) // twice: grouping must be idempotent
	if got := len(list[0].Sessions); got != 2 {
		t.Fatalf("the first task has %d links after two groupings, want 2 (re-grouping must reset, not append)", got)
	}
	if got := len(list[1].Sessions); got != 0 {
		t.Fatalf("the second task has %d links, want 0", got)
	}
	if list[0].Sessions[0].SessionID != "a" || list[0].Sessions[1].SessionID != "b" {
		t.Fatalf("input order not preserved: %+v", list[0].Sessions)
	}
}
