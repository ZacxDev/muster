package api

import (
	"context"
	"errors"
	"fmt"
	"html"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/users"
)

// LINKED SERVER APPS (docs/design/contacts.md, "The core change: linked server
// apps", its "Tests (in muster)" and "Mutation rows (core)"): two server apps
// an admin linked know each person by ONE subject; every other app stays
// pairwise. The real door, redeem, Hide, uninstall and the real Postgres, on
// srvFix. Every name is invented; each app's label is its own, distinct from
// srvLabel and from every other.

// installLabelled installs a server app named name (+ the fixture's suffix)
// with tab label label: no Files tasks.
func (f *srvFix) installLabelled(name, label string) (string, plugins.Plugin) {
	f.t.Helper()
	tok, p, err := f.store.InstallServerApp(f.sys(), plugins.ServerAppIn{Name: name + "-" + f.suffix, AppLabel: label, AppIcon: "book",
		Origin: hoZone, EntryURL: hoEntry, Origins: f.pats}, users.Actor{UserID: f.admin, Via: authz.ViaSession})
	if err != nil {
		f.t.Fatalf("install %s: %v", name, err)
	}
	f.appIDs = append(f.appIDs, p.ID)
	return tok, p
}

// link links source to adopter as the command line does, failing the test on
// an error.
func (f *srvFix) link(source, adopter int64) plugins.ServerPair {
	f.t.Helper()
	pair, err := f.store.LinkServerApps(context.Background(), source, adopter, users.Actor{Via: "cli"})
	if err != nil {
		f.t.Fatalf("link %d -> %d: %v", source, adopter, err)
	}
	f.t.Cleanup(func() {
		_, _ = f.pool.Exec(f.sys(), `DELETE FROM plugin_server_pairs WHERE $1 IN (low_id, high_id)`, source)
	})
	return pair
}

// subjectOf is uid's plugin_server_users subject under app, or "" with none.
func (f *srvFix) subjectOf(app, uid int64) string {
	f.t.Helper()
	var sub string
	err := f.pool.QueryRow(f.sys(), `SELECT subject FROM plugin_server_users WHERE plugin_id = $1 AND owner_id = $2`, app, uid).Scan(&sub)
	if errors.Is(err, pgx.ErrNoRows) {
		return ""
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return sub
}

// grantRows is every column of app's plugin_server_users rows, one string a
// row, by id: what "kept byte for byte" compares.
func (f *srvFix) grantRows(app int64) []string {
	f.t.Helper()
	rows, err := f.pool.Query(f.sys(), `SELECT g::text FROM plugin_server_users g WHERE plugin_id = $1 ORDER BY id`, app)
	if err != nil {
		f.t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

// TestUnlinkedServerAppsStayPairwise is an INVARIANT GUARD (it pins today's
// behaviour; no bug ever violated it): two unlinked server apps, three people
// who each open both, in both orders, and each person has a different subject
// under each app, and no two people share one.
func TestUnlinkedServerAppsStayPairwise(t *testing.T) {
	f := newSrvFix(t)
	tokB, appB := f.installLabelled("srv-ledger", "Quartz Ledger")
	c := f.person("Cyra Tamsin", "member", "")
	seen := map[string]string{}
	for _, uid := range []int64{f.a, f.b, c} {
		var x, y string
		if uid == f.b {
			y, x = f.open(uid, appB.ID, tokB), f.open(uid, f.app.ID, f.tok)
		} else {
			x, y = f.open(uid, f.app.ID, f.tok), f.open(uid, appB.ID, tokB)
		}
		if x == y {
			t.Errorf("user %d has the same subject %s under two UNLINKED apps: not pairwise", uid, x)
		}
		for _, s := range []string{x, y} {
			if prev, dup := seen[s]; dup {
				t.Errorf("subject %s is both %s and user %d's", s, prev, uid)
			}
			seen[s] = "user " + strconv.FormatInt(uid, 10) + "'s"
		}
	}
}

// TestALinkedPairKnowsEachPersonByOneSubject: A opens the source first and
// then the adopter; B opens the adopter first and then the source (the
// subject the ADOPTER drew is what the source gets); C opens only the
// adopter, then the source. Each person has one subject under both apps, and
// the three people's subjects are pairwise distinct.
func TestALinkedPairKnowsEachPersonByOneSubject(t *testing.T) {
	f := newSrvFix(t)
	tokC, contacts := f.installLabelled("srv-cards", "Quartz Ledger")
	f.link(f.app.ID, contacts.ID)
	c := f.person("Cyra Tamsin", "member", "")

	aSrc := f.open(f.a, f.app.ID, f.tok)
	aAdo := f.open(f.a, contacts.ID, tokC)
	bAdo := f.open(f.b, contacts.ID, tokC)
	bSrc := f.open(f.b, f.app.ID, f.tok)
	cAdo := f.open(c, contacts.ID, tokC)
	cSrc := f.open(c, f.app.ID, f.tok)
	for _, p := range []struct {
		who      string
		src, ado string
	}{{"A (source first)", aSrc, aAdo}, {"B (adopter first)", bSrc, bAdo}, {"C (adopter first)", cSrc, cAdo}} {
		if p.src != p.ado {
			t.Errorf("linked: %s has subject %s under the source and %s under the adopter; want one subject", p.who, p.src, p.ado)
		}
	}
	if aSrc == bSrc || aSrc == cSrc || bSrc == cSrc {
		t.Errorf("linked: three people's subjects %s %s %s are not pairwise distinct", aSrc, bSrc, cSrc)
	}
	// A second open keeps the subject (the row is there; the upsert keeps it).
	if again := f.open(f.b, contacts.ID, tokC); again != bAdo {
		t.Errorf("linked: B's second open of the adopter answered %s; want %s", again, bAdo)
	}
}

// TestALinkKeepsTheSourcesSubjects: A and B are known to the source before the
// link. The link writes no plugin_server_users row (the adopter has none right
// after it), and after a redeem on the adopter the adopter answers A the
// source's existing subject, while every one of the source's rows is
// BYTE FOR BYTE what it was before the link (rows compared, not counted).
func TestALinkKeepsTheSourcesSubjects(t *testing.T) {
	f := newSrvFix(t)
	tokC, contacts := f.installLabelled("srv-cards", "Quartz Ledger")
	aSrc := f.open(f.a, f.app.ID, f.tok)
	f.open(f.b, f.app.ID, f.tok)
	before := f.grantRows(f.app.ID)
	if len(before) != 2 {
		t.Fatalf("control: the source has %d rows; want A's and B's", len(before))
	}
	pair := f.link(f.app.ID, contacts.ID)
	if pair.Known != 2 || pair.Source.ID != f.app.ID || pair.Adopter.ID != contacts.ID {
		t.Errorf("the link answered %d known, source %d, adopter %d; want 2, %d, %d", pair.Known, pair.Source.ID, pair.Adopter.ID, f.app.ID, contacts.ID)
	}
	if rows := f.grantRows(contacts.ID); len(rows) != 0 {
		t.Errorf("the source's subjects are kept: right after the link the adopter has rows %v; want none (a link writes no grant)", rows)
	}
	if got := f.open(f.a, contacts.ID, tokC); got != aSrc {
		t.Errorf("the source's subjects are kept: the adopter answered A %s; want the source's existing %s", got, aSrc)
	}
	if after := f.grantRows(f.app.ID); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Errorf("the source's subjects are kept: the source's rows changed across the link and an adopter redeem:\nbefore %v\nafter  %v", before, after)
	}
}

// TestLinkRefusals: each refusal with its own words, and nothing linked by
// any of them: the adopter already knows someone; the source, then the
// adopter, uninstalled; a personal plugin on either side; an app linked to
// itself; an unknown id; and an app already in a pair, in every column it can
// collide in (the low index, the high index, and the other column of another
// row, the trigger).
func TestLinkRefusals(t *testing.T) {
	f := newSrvFix(t)
	// Ids in install order: f.app < p1 < p2 < p3.
	_, p1 := f.installLabelled("srv-one", "Quartz Ledger")
	_, p2 := f.installLabelled("srv-two", "Cobalt Atlas")
	_, p3 := f.installLabelled("srv-three", "Ember Index")
	refuse := func(name string, source, adopter int64, want string) {
		t.Helper()
		_, err := f.store.LinkServerApps(context.Background(), source, adopter, users.Actor{Via: "cli"})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("refusals: %s: %v; want %q", name, err, want)
		}
	}
	pairs := func() int {
		return f.count(`SELECT count(*) FROM plugin_server_pairs WHERE low_id = ANY($1) OR high_id = ANY($1)`, f.appIDs)
	}

	// The adopter already knows someone: a hidden-only row counts too.
	if _, err := f.store.SetServerAppHidden(srvAs(f.b), p1.ID, true, f.now); err != nil {
		t.Fatal(err)
	}
	refuse("adopter already knows someone", f.app.ID, p1.ID,
		"Quartz Ledger already knows 1 people; uninstall and reinstall it, or link it before anyone opens it")
	f.exec(`DELETE FROM plugin_server_users WHERE plugin_id = $1`, p1.ID)

	refuse("self-link", p1.ID, p1.ID, "an app cannot be linked to itself")
	refuse("an unknown id", f.app.ID, 999999999, "there is no app 999999999")

	_, personal, err := f.store.Create(srvAs(f.a), "srv-personal-"+f.suffix, "Example inbox", "example-inbox")
	if err != nil {
		t.Fatal(err)
	}
	refuse("personal plugin as the adopter", f.app.ID, personal.ID, fmt.Sprintf("app %d is a personal plugin; only server apps can be linked", personal.ID))
	refuse("personal plugin as the source", personal.ID, p1.ID, fmt.Sprintf("app %d is a personal plugin; only server apps can be linked", personal.ID))

	_, gone := f.installLabelled("srv-gone", "Basalt Tide")
	if _, err := f.store.UninstallServerApp(f.sys(), gone.ID, users.Actor{Via: "cli"}); err != nil {
		t.Fatal(err)
	}
	refuse("the source uninstalled", gone.ID, p1.ID, fmt.Sprintf("app %d (%s) is uninstalled", gone.ID, gone.Name))
	refuse("the adopter uninstalled", f.app.ID, gone.ID, fmt.Sprintf("app %d (%s) is uninstalled", gone.ID, gone.Name))
	if n := pairs(); n != 0 {
		t.Fatalf("refusals linked %d pair(s)", n)
	}

	// Already paired: the pair (p1, p2), so p1 is a low and p2 a high; f.app
	// is the lowest id of all, p3 the highest.
	f.link(p1.ID, p2.ID)
	already := func(id, partner int64) string {
		return fmt.Sprintf("app %d is already linked with app %d; unlink it first", id, partner)
	}
	refuse("already paired: a low again (the low index)", p1.ID, p3.ID, already(p1.ID, p2.ID))
	refuse("already paired: a high again (the high index)", f.app.ID, p2.ID, already(p2.ID, p1.ID))
	refuse("already paired: a high as a low (the trigger)", p2.ID, p3.ID, already(p2.ID, p1.ID))
	refuse("already paired: a low as a high (the trigger)", f.app.ID, p1.ID, already(p1.ID, p2.ID))
	refuse("already paired: the same pair again", p2.ID, p1.ID, already(p2.ID, p1.ID))
	if n := pairs(); n != 1 {
		t.Errorf("after every refusal: %d pairs; want the one made", n)
	}
	// 🔴 The table, under a PERSON's principal, shows and admits nothing (its
	// only policy is scope_all): every read is the System's.
	var seen int
	if err := f.pool.QueryRow(srvAs(f.a), `SELECT count(*) FROM plugin_server_pairs`).Scan(&seen); err != nil || seen != 0 {
		t.Errorf("a person's principal reads %d pair rows (%v); want none", seen, err)
	}
}

// TestHideOnAnAdopterBeforeFirstOpen: A knows the source; after the link A
// hides the adopter without ever opening it, and the row Hide makes (Hide is
// the second subject writer) carries the source's subject for A. Show and
// open, and the adopter answers that same subject.
func TestHideOnAnAdopterBeforeFirstOpen(t *testing.T) {
	f := newSrvFix(t)
	tokC, contacts := f.installLabelled("srv-cards", "Quartz Ledger")
	aSrc := f.open(f.a, f.app.ID, f.tok)
	f.link(f.app.ID, contacts.ID)
	if changed, err := f.store.SetServerAppHidden(srvAs(f.a), contacts.ID, true, f.now); err != nil || !changed {
		t.Fatalf("hide: %v %v", changed, err)
	}
	if got := f.subjectOf(contacts.ID, f.a); got != aSrc {
		t.Errorf("hide on an adopter: the row Hide made has subject %q; want the source's %s", got, aSrc)
	}
	if _, err := f.store.SetServerAppHidden(srvAs(f.a), contacts.ID, false, f.now); err != nil {
		t.Fatal(err)
	}
	if got := f.open(f.a, contacts.ID, tokC); got != aSrc {
		t.Errorf("hide on an adopter: after Show, the adopter answered %s; want %s", got, aSrc)
	}
	// And the other way: B hides the SOURCE before opening either; the
	// adopter then answers the subject that Hide drew.
	if _, err := f.store.SetServerAppHidden(srvAs(f.b), f.app.ID, true, f.now); err != nil {
		t.Fatal(err)
	}
	if bHidden, got := f.subjectOf(f.app.ID, f.b), f.open(f.b, contacts.ID, tokC); got != bHidden {
		t.Errorf("hide on a source: the adopter answered B %s; want the subject Hide drew under the source, %s", got, bHidden)
	}
}

// TestTwoConcurrentFirstOpensOfAPairGiveOneSubject: 50 people, each opening
// both apps of a pair AT THE SAME MOMENT for the first time. A barrier makes
// the two redeems start together: this test holds both codes' rows FOR
// UPDATE until both redeems wait on them, then lets them go at once. Every
// person ends with one subject under both apps.
func TestTwoConcurrentFirstOpensOfAPairGiveOneSubject(t *testing.T) {
	f := newSrvFix(t)
	tokC, contacts := f.installLabelled("srv-cards", "Quartz Ledger")
	f.link(f.app.ID, contacts.ID)
	split := 0
	for i := range 50 {
		uid := f.person("Race Person "+strconv.Itoa(i), "member", "")
		_, codeM := f.mintAt(uid, f.app.ID, f.signin)
		_, codeC := f.mintAt(uid, contacts.ID, f.signin)
		if codeM == "" || codeC == "" {
			t.Fatalf("run %d: mint", i)
		}
		tx, err := f.pool.Begin(f.sys())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(f.sys(), `SELECT 1 FROM plugin_handoffs WHERE code_hash = ANY ($1) FOR UPDATE`,
			[]string{plugins.HashToken(codeM), plugins.HashToken(codeC)}); err != nil {
			t.Fatal(err)
		}
		// This transaction's id: each redeem's UPDATE of its code's row waits
		// on it (pg_locks is readable by any role; pg_stat_activity's query
		// column is not, to the application role).
		var xid string
		if err := tx.QueryRow(f.sys(), `SELECT (pg_current_xact_id()::text::bigint % 4294967296)::text`).Scan(&xid); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var subM, subC string
		var codeMOK, codeCOK int
		wg.Add(2)
		go func() {
			defer wg.Done()
			rec, out := f.redeem(f.tok, codeM)
			codeMOK, subM = rec.Code, out.Owner.Subject
		}()
		go func() {
			defer wg.Done()
			rec, out := f.redeem(tokC, codeC)
			codeCOK, subC = rec.Code, out.Owner.Subject
		}()
		deadline := time.Now().Add(10 * time.Second)
		for {
			var waiting int
			if err := f.pool.QueryRow(f.sys(), `SELECT count(DISTINCT pid) FROM pg_locks
				WHERE locktype = 'transactionid' AND NOT granted AND transactionid::text = $1`, xid).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting >= 2 {
				break
			}
			if time.Now().After(deadline) {
				_ = tx.Rollback(f.sys())
				t.Fatalf("run %d: the two redeems never both reached the barrier (%d waiting)", i, waiting)
			}
			time.Sleep(5 * time.Millisecond)
		}
		if err := tx.Rollback(f.sys()); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		if codeMOK != 200 || codeCOK != 200 {
			t.Fatalf("run %d: redeems answered %d and %d", i, codeMOK, codeCOK)
		}
		if subM != subC {
			split++
			t.Errorf("the race: run %d: concurrent first opens drew two subjects, %s and %s", i, subM, subC)
		}
	}
	if split > 0 {
		t.Errorf("the race: %d of 50 people ended with two subjects", split)
	}
}

// TestCrossAppActingIsStillRefused: a shared subject does not let one app act
// through the other's grant. A opened only the source: the ADOPTER's token
// knocking for A's shared subject is the one 404 while the source's grant is
// live and the adopter has none (the source's own knock, the control, is
// 202). The reverse: B opened only the adopter, and the source's token is
// refused for B's subject while the adopter's knock is 202.
func TestCrossAppActingIsStillRefused(t *testing.T) {
	f := newSrvFix(t)
	tokC, contacts := f.installLabelled("srv-cards", "Quartz Ledger")
	f.link(f.app.ID, contacts.ID)
	aSub := f.open(f.a, f.app.ID, f.tok)
	if rec := f.knock(f.tok, aSub, "pair-src-a"); rec.Code != 202 {
		t.Fatalf("control: the source knocks for A: %d %s; want 202", rec.Code, rec.Body.String())
	}
	if rec := f.knock(tokC, aSub, "pair-ado-a"); rec.Code != 404 || strings.TrimSpace(rec.Body.String()) != srvNotFound {
		t.Errorf("cross-app acting is still refused: the adopter knocks with A's shared subject: %d %s; want 404 %s", rec.Code, rec.Body.String(), srvNotFound)
	}
	bSub := f.open(f.b, contacts.ID, tokC)
	if rec := f.knock(tokC, bSub, "pair-ado-b"); rec.Code != 202 {
		t.Fatalf("control: the adopter knocks for B: %d %s; want 202", rec.Code, rec.Body.String())
	}
	if rec := f.knock(f.tok, bSub, "pair-src-b"); rec.Code != 404 || strings.TrimSpace(rec.Body.String()) != srvNotFound {
		t.Errorf("cross-app acting is still refused: the source knocks with B's shared subject: %d %s; want 404 %s", rec.Code, rec.Body.String(), srvNotFound)
	}
	_, states := f.check(tokC, aSub, bSub)
	if states[aSub] != string(plugins.GrantRevoked) || states[bSub] != string(plugins.GrantLive) {
		t.Errorf("the adopter's owners/check: %v; want A revoked (it never redeemed A) and B live", states)
	}
}

// TestUnlinkKeepsWhatWasSharedAndSplitsNewPeople: A, known to both apps
// before the unlink, keeps one subject under both after it; B, who first
// opens either after it, gets two. Unlinking an app in no pair is refused.
func TestUnlinkKeepsWhatWasSharedAndSplitsNewPeople(t *testing.T) {
	f := newSrvFix(t)
	tokC, contacts := f.installLabelled("srv-cards", "Quartz Ledger")
	f.link(f.app.ID, contacts.ID)
	aSub := f.open(f.a, f.app.ID, f.tok)
	if got := f.open(f.a, contacts.ID, tokC); got != aSub {
		t.Fatalf("control: linked, A has %s and %s", aSub, got)
	}
	partner, err := f.store.UnlinkServerApp(context.Background(), contacts.ID, users.Actor{Via: "cli"})
	if err != nil || partner != f.app.ID {
		t.Fatalf("unlink: partner %d, %v; want %d", partner, err, f.app.ID)
	}
	if n := f.count(`SELECT count(*) FROM plugin_server_pairs WHERE $1 IN (low_id, high_id)`, f.app.ID); n != 0 {
		t.Errorf("unlink: the pair row is still there (%d)", n)
	}
	if x, y := f.open(f.a, f.app.ID, f.tok), f.open(f.a, contacts.ID, tokC); x != aSub || y != aSub {
		t.Errorf("unlink: A, known to both, has %s and %s; want %s under both", x, y, aSub)
	}
	if x, y := f.open(f.b, contacts.ID, tokC), f.open(f.b, f.app.ID, f.tok); x == y {
		t.Errorf("unlink: B, new after the unlink, has one subject %s under both; want two", x)
	}
	if _, err := f.store.UnlinkServerApp(context.Background(), contacts.ID, users.Actor{Via: "cli"}); err == nil ||
		!strings.Contains(err.Error(), fmt.Sprintf("app %d is not linked", contacts.ID)) {
		t.Errorf("unlinking an app in no pair: %v; want \"app %d is not linked\"", err, contacts.ID)
	}
}

// TestUninstallDeletesThePair: an uninstall of either app of a pair deletes
// the pair row (in the uninstall's own transaction: no other write of the
// store could), so the survivor is linked to nothing and may be linked again
// at once. Uninstalling the adopter: a fresh install linked to the surviving
// source adopts the subjects the old adopter shared. Uninstalling the source:
// the surviving adopter may become the source of a new pair.
func TestUninstallDeletesThePair(t *testing.T) {
	f := newSrvFix(t)
	none := func(which string, ids ...int64) {
		t.Helper()
		if n := f.count(`SELECT count(*) FROM plugin_server_pairs WHERE low_id = ANY ($1) OR high_id = ANY ($1)`, ids); n != 0 {
			t.Errorf("uninstall deletes the pair: uninstalling %s left %d pair row(s)", which, n)
		}
	}
	uninstall := func(id int64) {
		t.Helper()
		if _, err := f.store.UninstallServerApp(f.sys(), id, users.Actor{Via: "cli"}); err != nil {
			t.Fatal(err)
		}
	}

	_, old := f.installLabelled("srv-cards", "Quartz Ledger")
	f.link(f.app.ID, old.ID)
	aSub := f.open(f.a, f.app.ID, f.tok)
	uninstall(old.ID)
	none("the adopter", old.ID, f.app.ID)
	tokR, again := f.installLabelled("srv-cards-again", "Quartz Ledger")
	f.link(f.app.ID, again.ID)
	if got := f.open(f.a, again.ID, tokR); got != aSub {
		t.Errorf("a reinstall linked to the surviving source answered A %s; want the survivor's %s", got, aSub)
	}
	uninstall(again.ID)

	_, src := f.installLabelled("srv-src", "Cobalt Atlas")
	_, ado := f.installLabelled("srv-ado", "Ember Index")
	f.link(src.ID, ado.ID)
	uninstall(src.ID)
	none("the source", src.ID, ado.ID)
	_, next := f.installLabelled("srv-next", "Basalt Tide")
	if _, err := f.store.LinkServerApps(context.Background(), ado.ID, next.ID, users.Actor{Via: "cli"}); err != nil {
		t.Errorf("the surviving adopter as the source of a new pair: %v", err)
	}
	_, _ = f.pool.Exec(f.sys(), `DELETE FROM plugin_server_pairs WHERE $1 IN (low_id, high_id)`, ado.ID)
}

// TestLinkAndUnlinkAreAudited: plugin.server.linked on the adopter with both
// ids, and plugin.server.unlinked on the unlinked app with both, each as the
// command line (no user, via cli).
func TestLinkAndUnlinkAreAudited(t *testing.T) {
	f := newSrvFix(t)
	_, contacts := f.installLabelled("srv-cards", "Quartz Ledger")
	f.link(f.app.ID, contacts.ID)
	if _, err := f.store.UnlinkServerApp(context.Background(), f.app.ID, users.Actor{Via: "cli"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ action, object, detail string }{
		{"plugin.server.linked", plugins.Object(contacts.ID), fmt.Sprintf(`{"source": %d, "adopter": %d}`, f.app.ID, contacts.ID)},
		{"plugin.server.unlinked", plugins.Object(f.app.ID), fmt.Sprintf(`{"apps": [%d, %d], "source": %d}`, f.app.ID, contacts.ID, f.app.ID)},
	} {
		var detail, via string
		var user *int64
		err := f.pool.QueryRow(f.sys(), `SELECT detail::text, via, user_id FROM audit_log WHERE action = $1 AND object = $2`, c.action, c.object).Scan(&detail, &via, &user)
		if err != nil || detail != c.detail || via != "cli" || user != nil {
			t.Errorf("%s on %s: detail %s via %q user %v (%v); want %s via cli, no user", c.action, c.object, detail, via, user, err, c.detail)
		}
	}
}

// TestTheLinkedLinesOnAdminAppsAndAccountApps: /admin/apps says "Linked with
// <the other's label>." on each app of a pair, and Account → Apps adds "<this>
// and <the other> know you as the same person." to each; an unlinked app has
// neither. Labels are distinct from each other and from every constant.
func TestTheLinkedLinesOnAdminAppsAndAccountApps(t *testing.T) {
	f := newSrvFix(t)
	_, contacts := f.installLabelled("srv-cards", "Quartz Ledger")
	_, lone := f.installLabelled("srv-lone", "Cobalt Atlas")
	f.link(f.app.ID, contacts.ID)
	admin := f.get(f.admin, adminAppsPath).Body.String()
	account := f.get(f.a, accountAppsPath).Body.String()
	for _, want := range []string{
		`data-linked-with="` + strconv.FormatInt(contacts.ID, 10) + `">` + html.EscapeString("Linked with Quartz Ledger.") + `</span>`,
		`data-linked-with="` + strconv.FormatInt(f.app.ID, 10) + `">` + html.EscapeString("Linked with "+srvLabel+".") + `</span>`,
	} {
		if !strings.Contains(admin, want) {
			t.Errorf("/admin/apps lacks %s", want)
		}
	}
	for _, want := range []string{
		`data-app-linked="` + strconv.FormatInt(contacts.ID, 10) + `">` + html.EscapeString(srvLabel+" and Quartz Ledger know you as the same person.") + `</span>`,
		`data-app-linked="` + strconv.FormatInt(f.app.ID, 10) + `">` + html.EscapeString("Quartz Ledger and "+srvLabel+" know you as the same person.") + `</span>`,
	} {
		if !strings.Contains(account, want) {
			t.Errorf("Account → Apps lacks %s", want)
		}
	}
	if n := strings.Count(admin, "data-linked-with="); n != 2 {
		t.Errorf("/admin/apps has %d linked lines; want 2 (the pair, not %s)", n, lone.Name)
	}
	if n := strings.Count(account, "data-app-linked="); n != 2 {
		t.Errorf("Account → Apps has %d linked lines; want 2 (the pair, not %s)", n, lone.Name)
	}
	if strings.Contains(account, "Cobalt Atlas and") || strings.Contains(account, "and Cobalt Atlas") {
		t.Error("Account → Apps names the unlinked app in a linked line")
	}
}
