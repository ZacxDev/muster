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
// with tab label label: no Files tasks, linked to nothing.
func (f *srvFix) installLabelled(name, label string) (string, plugins.Plugin) {
	f.t.Helper()
	return f.installLinked(name, label, 0)
}

// installLinked installs it LINKED to server app linkTo (0: to nothing), as
// `app install --link-to` does, failing the test on an error.
func (f *srvFix) installLinked(name, label string, linkTo int64) (string, plugins.Plugin) {
	f.t.Helper()
	tok, p, err := f.tryInstall(name, label, linkTo)
	if err != nil {
		f.t.Fatalf("install %s: %v", name, err)
	}
	return tok, p
}

func (f *srvFix) tryInstall(name, label string, linkTo int64) (string, plugins.Plugin, error) {
	tok, p, err := f.store.InstallServerApp(f.sys(), plugins.ServerAppIn{Name: name + "-" + f.suffix, AppLabel: label, AppIcon: "book",
		Origin: hoZone, EntryURL: hoEntry, Origins: f.pats, LinkTo: linkTo}, users.Actor{Via: "cli"})
	if err == nil {
		f.appIDs = append(f.appIDs, p.ID)
		f.t.Cleanup(func() { _, _ = f.pool.Exec(f.sys(), `DELETE FROM plugin_server_pairs WHERE plugin_id = $1`, p.ID) })
	}
	return tok, p, err
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

// pairOf is app's partner in plugin_server_pairs, 0 with none.
func (f *srvFix) pairOf(app int64) int64 {
	f.t.Helper()
	var partner int64
	err := f.pool.QueryRow(f.sys(), `SELECT partner_id FROM plugin_server_pairs WHERE plugin_id = $1`, app).Scan(&partner)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return partner
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

// TestALinkedPairKnowsEachPersonByOneSubject: the adopter is installed
// --link-to the source. A opens the source first and then the adopter; B
// opens the adopter first and then the source (the subject the ADOPTER drew
// is what the source gets); C likewise. Each person has one subject under
// both apps, and the three people's subjects are pairwise distinct.
func TestALinkedPairKnowsEachPersonByOneSubject(t *testing.T) {
	f := newSrvFix(t)
	tokC, contacts := f.installLinked("srv-cards", "Quartz Ledger", f.app.ID)
	if f.pairOf(contacts.ID) != f.app.ID || f.pairOf(f.app.ID) != contacts.ID {
		t.Fatalf("install --link-to: pairs %d -> %d and %d -> %d; want each the other",
			contacts.ID, f.pairOf(contacts.ID), f.app.ID, f.pairOf(f.app.ID))
	}
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
	if again := f.open(f.b, contacts.ID, tokC); again != bAdo {
		t.Errorf("linked: B's second open of the adopter answered %s; want %s", again, bAdo)
	}
}

// TestALinkKeepsTheSourcesSubjects: A and B are known to the source before an
// app is installed linked to it. The install writes no plugin_server_users
// row for the new app, and after a redeem on it the new app answers A the
// source's existing subject, while every one of the source's rows is BYTE FOR
// BYTE what it was before (rows compared, not counted).
func TestALinkKeepsTheSourcesSubjects(t *testing.T) {
	f := newSrvFix(t)
	aSrc := f.open(f.a, f.app.ID, f.tok)
	f.open(f.b, f.app.ID, f.tok)
	before := f.grantRows(f.app.ID)
	if len(before) != 2 {
		t.Fatalf("control: the source has %d rows; want A's and B's", len(before))
	}
	tokC, contacts := f.installLinked("srv-cards", "Quartz Ledger", f.app.ID)
	if rows := f.grantRows(contacts.ID); len(rows) != 0 {
		t.Errorf("the source's subjects are kept: right after the install the new app has rows %v; want none", rows)
	}
	if got := f.open(f.a, contacts.ID, tokC); got != aSrc {
		t.Errorf("the source's subjects are kept: the new app answered A %s; want the source's existing %s", got, aSrc)
	}
	if after := f.grantRows(f.app.ID); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Errorf("the source's subjects are kept: the source's rows changed across the install and a redeem:\nbefore %v\nafter  %v", before, after)
	}
}

// TestInstallLinkToRefusalsInstallNothing: install --link-to is refused, each
// with its own words, for an unknown id, a personal plugin, an uninstalled
// app, and an app already in a pair; and 🔴 EACH REFUSAL IS ATOMIC: neither
// the new app's row (by its name, revoked or not) nor any pair row is left.
// Then the table, under a person's principal, shows nothing.
func TestInstallLinkToRefusalsInstallNothing(t *testing.T) {
	f := newSrvFix(t)
	refuse := func(name string, linkTo int64, want string) {
		t.Helper()
		_, _, err := f.tryInstall("srv-refused", "Ember Index", linkTo)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("refusals: %s: %v; want %q", name, err, want)
		}
		if n := f.count(`SELECT count(*) FROM plugins WHERE name = $1`, "srv-refused-"+f.suffix); n != 0 {
			t.Errorf("install --link-to is atomic: %s left %d app row(s)", name, n)
		}
	}
	pairs := func() int {
		return f.count(`SELECT count(*) FROM plugin_server_pairs WHERE plugin_id = ANY($1) OR partner_id = ANY($1)`, f.appIDs)
	}

	refuse("an unknown id", 999999999, "there is no app 999999999 to link to")
	_, personal, err := f.store.Create(srvAs(f.a), "srv-personal-"+f.suffix, "Example inbox", "example-inbox")
	if err != nil {
		t.Fatal(err)
	}
	refuse("a personal plugin", personal.ID, fmt.Sprintf("app %d is a personal plugin; only server apps can be linked", personal.ID))
	_, gone := f.installLabelled("srv-gone", "Basalt Tide")
	if _, err := f.store.UninstallServerApp(f.sys(), gone.ID, users.Actor{Via: "cli"}); err != nil {
		t.Fatal(err)
	}
	refuse("an uninstalled app", gone.ID, fmt.Sprintf("app %d is uninstalled", gone.ID))
	if n := pairs(); n != 0 {
		t.Fatalf("refusals linked %d pair row(s)", n)
	}
	_, linked := f.installLinked("srv-first", "Quartz Ledger", f.app.ID)
	refuse("the source already paired", f.app.ID, fmt.Sprintf("app %d is already linked with app %d; unlink it first", f.app.ID, linked.ID))
	refuse("the source already paired (as the new one)", linked.ID, fmt.Sprintf("app %d is already linked with app %d; unlink it first", linked.ID, f.app.ID))
	if n := pairs(); n != 2 {
		t.Errorf("after every refusal: %d pair rows; want the one pair's two", n)
	}
	// 🔴 Under a PERSON's principal the table shows nothing (its only policy
	// is scope_all): every read is the System's.
	var seen int
	if err := f.pool.QueryRow(srvAs(f.a), `SELECT count(*) FROM plugin_server_pairs`).Scan(&seen); err != nil || seen != 0 {
		t.Errorf("a person's principal reads %d pair rows (%v); want none", seen, err)
	}
}

// TestALinkToAnAppWhoseSubjectsAreSharedIsRefused: the pair (source, B), P
// opened both (one subject), then unlinked. 🔴 Installing C --link-to the
// source, or --link-to B, is refused (it would make C share P's subject with
// an app C was never linked to) and installs nothing. Once B is uninstalled,
// the source's subjects are its own again and C links. A source never paired
// links too (every other test's install --link-to).
func TestALinkToAnAppWhoseSubjectsAreSharedIsRefused(t *testing.T) {
	f := newSrvFix(t)
	tokB, b := f.installLinked("srv-cards", "Quartz Ledger", f.app.ID)
	sub := f.open(f.a, f.app.ID, f.tok)
	if got := f.open(f.a, b.ID, tokB); got != sub {
		t.Fatalf("control: linked, A has %s and %s", sub, got)
	}
	if _, err := f.store.UnlinkServerApp(context.Background(), b.ID, users.Actor{Via: "cli"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name         string
		source, with int64
	}{{"--link-to the former source", f.app.ID, b.ID}, {"--link-to the former adopter", b.ID, f.app.ID}} {
		_, _, err := f.tryInstall("srv-third", "Ember Index", c.source)
		want := fmt.Sprintf("app %d already shares its people's subjects with app %d from an earlier link, so a new app linked to it "+
			"would share them with app %d too; install the new app without --link-to instead", c.source, c.with, c.with)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("shared subjects: %s: %v; want %q", c.name, err, want)
		}
		if n := f.count(`SELECT count(*) FROM plugins WHERE name = $1`, "srv-third-"+f.suffix); n != 0 {
			t.Errorf("shared subjects: %s left %d app row(s)", c.name, n)
		}
	}
	if f.pairOf(f.app.ID) != 0 || f.pairOf(b.ID) != 0 {
		t.Errorf("shared subjects: a refused install made a pair")
	}
	if _, err := f.store.UninstallServerApp(f.sys(), b.ID, users.Actor{Via: "cli"}); err != nil {
		t.Fatal(err)
	}
	tokC, c := f.installLinked("srv-third", "Ember Index", f.app.ID)
	if got := f.open(f.a, c.ID, tokC); got != sub {
		t.Errorf("once the former partner is uninstalled, the new app answered A %s; want the source's %s", got, sub)
	}
}

// TestHideOnAnAdopterBeforeFirstOpen: A knows the source; an app is installed
// linked to it, and A hides it without ever opening it: the row Hide makes
// (Hide is the second subject writer) carries the source's subject for A.
// Show and open, and the new app answers that same subject. The other way: B
// hides the SOURCE before opening either, and the adopter then answers the
// subject that Hide drew.
func TestHideOnAnAdopterBeforeFirstOpen(t *testing.T) {
	f := newSrvFix(t)
	aSrc := f.open(f.a, f.app.ID, f.tok)
	tokC, contacts := f.installLinked("srv-cards", "Quartz Ledger", f.app.ID)
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
	tokC, contacts := f.installLinked("srv-cards", "Quartz Ledger", f.app.ID)
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
	tokC, contacts := f.installLinked("srv-cards", "Quartz Ledger", f.app.ID)
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
// opens either after it, gets two. Unlink deletes BOTH rows of the pair (one
// DELETE; the mutual key cascades to the mirror). Unlinking an app in no pair
// is refused.
func TestUnlinkKeepsWhatWasSharedAndSplitsNewPeople(t *testing.T) {
	f := newSrvFix(t)
	tokC, contacts := f.installLinked("srv-cards", "Quartz Ledger", f.app.ID)
	aSub := f.open(f.a, f.app.ID, f.tok)
	if got := f.open(f.a, contacts.ID, tokC); got != aSub {
		t.Fatalf("control: linked, A has %s and %s", aSub, got)
	}
	partner, err := f.store.UnlinkServerApp(context.Background(), contacts.ID, users.Actor{Via: "cli"})
	if err != nil || partner != f.app.ID {
		t.Fatalf("unlink: partner %d, %v; want %d", partner, err, f.app.ID)
	}
	if p, q := f.pairOf(contacts.ID), f.pairOf(f.app.ID); p != 0 || q != 0 {
		t.Errorf("unlink: pair rows left: %d -> %d, %d -> %d; want neither", contacts.ID, p, f.app.ID, q)
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
// both pair rows (in the uninstall's own transaction: no other write of the
// store could), so the survivor is linked to nothing. Uninstalling the
// adopter: a fresh install linked to the surviving source adopts the subjects
// the old adopter shared. Uninstalling the source: the survivor may be the
// source of a new pair at once.
func TestUninstallDeletesThePair(t *testing.T) {
	f := newSrvFix(t)
	uninstall := func(id int64) {
		t.Helper()
		if _, err := f.store.UninstallServerApp(f.sys(), id, users.Actor{Via: "cli"}); err != nil {
			t.Fatal(err)
		}
	}
	none := func(which string, ids ...int64) {
		t.Helper()
		if n := f.count(`SELECT count(*) FROM plugin_server_pairs WHERE plugin_id = ANY ($1) OR partner_id = ANY ($1)`, ids); n != 0 {
			t.Errorf("uninstall deletes the pair: uninstalling %s left %d pair row(s)", which, n)
		}
	}

	_, old := f.installLinked("srv-cards", "Quartz Ledger", f.app.ID)
	aSub := f.open(f.a, f.app.ID, f.tok)
	uninstall(old.ID)
	none("the adopter", old.ID, f.app.ID)
	tokR, again := f.installLinked("srv-cards-again", "Quartz Ledger", f.app.ID)
	if got := f.open(f.a, again.ID, tokR); got != aSub {
		t.Errorf("a reinstall linked to the surviving source answered A %s; want the survivor's %s", got, aSub)
	}
	uninstall(again.ID)

	_, src := f.installLabelled("srv-src", "Cobalt Atlas")
	_, ado := f.installLinked("srv-ado", "Ember Index", src.ID)
	uninstall(src.ID)
	none("the source", src.ID, ado.ID)
	if _, _, err := f.tryInstall("srv-next", "Basalt Tide", ado.ID); err != nil {
		t.Errorf("the survivor as the source of a new pair: %v", err)
	}
}

// TestLinkAndUnlinkAreAudited: the install's plugin.server.installed row
// records linked_to; plugin.server.unlinked records the partner; an uninstall
// that ended a pair records unlinked_from; each as the command line (no
// user, via cli).
func TestLinkAndUnlinkAreAudited(t *testing.T) {
	f := newSrvFix(t)
	_, contacts := f.installLinked("srv-cards", "Quartz Ledger", f.app.ID)
	if _, err := f.store.UnlinkServerApp(context.Background(), f.app.ID, users.Actor{Via: "cli"}); err != nil {
		t.Fatal(err)
	}
	_, src := f.installLabelled("srv-src", "Cobalt Atlas")
	_, ado := f.installLinked("srv-ado", "Ember Index", src.ID)
	if _, err := f.store.UninstallServerApp(f.sys(), ado.ID, users.Actor{Via: "cli"}); err != nil {
		t.Fatal(err)
	}
	// An uninstall that ended no pair names none (contacts was unlinked first).
	if _, err := f.store.UninstallServerApp(f.sys(), contacts.ID, users.Actor{Via: "cli"}); err != nil {
		t.Fatal(err)
	}
	var none string
	if err := f.pool.QueryRow(f.sys(), `SELECT coalesce(detail->>'unlinked_from', '') FROM audit_log WHERE action = 'plugin.server.uninstalled' AND object = $1`,
		plugins.Object(contacts.ID)).Scan(&none); err != nil || none != "" {
		t.Errorf("an uninstall that ended no pair: unlinked_from %q (%v); want none", none, err)
	}
	for _, c := range []struct{ action, object, key, want string }{
		{"plugin.server.installed", plugins.Object(contacts.ID), "linked_to", strconv.FormatInt(f.app.ID, 10)},
		{"plugin.server.unlinked", plugins.Object(f.app.ID), "partner", strconv.FormatInt(contacts.ID, 10)},
		{"plugin.server.uninstalled", plugins.Object(ado.ID), "unlinked_from", strconv.FormatInt(src.ID, 10)},
	} {
		var got, via string
		var user *int64
		err := f.pool.QueryRow(f.sys(), `SELECT coalesce(detail->>$3, ''), via, user_id FROM audit_log WHERE action = $1 AND object = $2`,
			c.action, c.object, c.key).Scan(&got, &via, &user)
		if err != nil || got != c.want || via != "cli" || user != nil {
			t.Errorf("%s on %s: %s %q via %q user %v (%v); want %s, via cli, no user", c.action, c.object, c.key, got, via, user, err, c.want)
		}
	}
}

// TestTheLinkedLineOnAccountApps: Account → Apps adds "<this> and <the other>
// know you as the same person." to each app of a pair, and an unlinked app has
// none. /admin/apps shows no linked line (`app list` is where an operator
// reads the pair). Labels are distinct from each other and every constant.
func TestTheLinkedLineOnAccountApps(t *testing.T) {
	f := newSrvFix(t)
	_, contacts := f.installLinked("srv-cards", "Quartz Ledger", f.app.ID)
	_, lone := f.installLabelled("srv-lone", "Cobalt Atlas")
	account := f.get(f.a, accountAppsPath).Body.String()
	for _, want := range []string{
		`data-app-linked="` + strconv.FormatInt(contacts.ID, 10) + `">` + html.EscapeString(srvLabel+" and Quartz Ledger know you as the same person.") + `</span>`,
		`data-app-linked="` + strconv.FormatInt(f.app.ID, 10) + `">` + html.EscapeString("Quartz Ledger and "+srvLabel+" know you as the same person.") + `</span>`,
	} {
		if !strings.Contains(account, want) {
			t.Errorf("Account → Apps lacks %s", want)
		}
	}
	if n := strings.Count(account, "data-app-linked="); n != 2 {
		t.Errorf("Account → Apps has %d linked lines; want 2 (the pair, not %s)", n, lone.Name)
	}
	if admin := f.get(f.admin, adminAppsPath).Body.String(); strings.Contains(admin, "Linked with") || !strings.Contains(admin, "Quartz Ledger") {
		t.Error("/admin/apps draws a linked line, or not the linked app at all")
	}
}
