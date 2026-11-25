package api

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/users"
)

// ROUND 7 (migration 0189): every SQL that deletes a step-up phone marks its
// account in the same statement, so the password's delete-everything paths
// (Sign out everywhere, a disable) cannot empty an account's factors without
// leaving it gated. Red at the previous head.

// unmarkedStepUpPhone is the enrolment rig with a step-up phone whose account
// is NOT marked: a failed best-effort mark at pairing, or a phone paired by an
// image older than 0188 after its backfill.
func unmarkedStepUpPhone(t *testing.T) *enrolRig {
	t.Helper()
	r := newEnrolRig(t, true)
	if _, err := r.pool.Exec(authz.System(context.Background()), `UPDATE users SET had_strong_factor_at = NULL WHERE id = $1`, r.uid); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *enrolRig) refusesAKeyedPairing(t *testing.T, after string) {
	t.Helper()
	if n := r.stepUpPhones(t); n != 0 {
		t.Fatalf("fixture: %s left %d phone(s)", after, n)
	}
	r.at, r.rt = r.login() // the password, again
	code, allowed := r.mintCode(t)
	if allowed {
		t.Fatalf("after %s, an unmarked account's password-only code may pair a step-up key", after)
	}
	if res := r.redeemKeyed(t, code); res.code != http.StatusForbidden {
		t.Fatalf("a password-only keyed pairing after %s: %d %s, want 403", after, res.code, res.body)
	}
}

// TestSignOutEverywhereMarksAnUnmarkedStepUpPhonesAccount: 🔴 R7-1, the
// reproduced chain: an account whose step-up phone was never marked signs out
// everywhere with the password; the phone goes, and the account is marked in
// the same statement, so the password cannot pair the next phone with a key.
func TestSignOutEverywhereMarksAnUnmarkedStepUpPhonesAccount(t *testing.T) {
	r := unmarkedStepUpPhone(t)
	if w := do(r.edge, "POST", "/logout", url.Values{"scope": {"global"}}, withCookies(r.at, r.rt)); w.Code != http.StatusSeeOther {
		t.Fatalf("sign out everywhere: %d", w.Code)
	}
	r.refusesAKeyedPairing(t, "sign out everywhere")
}

// TestADisableMarksAnUnmarkedStepUpPhonesAccount: the same through the
// disable path, which deletes every phone too.
func TestADisableMarksAnUnmarkedStepUpPhonesAccount(t *testing.T) {
	r := unmarkedStepUpPhone(t)
	st := users.NewPG(r.pool)
	for _, off := range []bool{true, false} {
		if _, err := st.SetDisabledAs(context.Background(), &users.Actor{Via: "cli"}, r.uid, off); err != nil {
			t.Fatal(err)
		}
	}
	r.refusesAKeyedPairing(t, "a disable and an enable")
}

// normaliseSQL is s as the devices-writers ledger matches it: comments
// stripped (-- and /* */), lower case, double-quoted identifiers unquoted,
// the public. schema and ONLY dropped, whitespace collapsed. So a spelling of
// the same statement cannot walk around the matcher.
func normaliseSQL(s string) string {
	s = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(s, " ")
	s = regexp.MustCompile(`--[^\n]*`).ReplaceAllString(s, " ")
	s = strings.ToLower(s)
	s = regexp.MustCompile(`"([a-z_][a-z0-9_]*)"`).ReplaceAllString(s, "$1")
	s = regexp.MustCompile(`\bpublic\s*\.\s*`).ReplaceAllString(s, "")
	s = regexp.MustCompile(`\s+`).ReplaceAllString(s, " ")
	s = regexp.MustCompile(`\b(from|update|into|truncate|table) only\b`).ReplaceAllString(s, "$1")
	return s
}

var (
	devicesDeleteRE   = regexp.MustCompile(`\bdelete from devices\b`)
	devicesMergeRE    = regexp.MustCompile(`\bmerge into devices\b`)
	devicesUpdateRE   = regexp.MustCompile(`\bupdate devices\b(?: as)?(?: [a-z_]+)? set (.*?)(?: where |$| returning | from )`)
	devicesTruncateRE = regexp.MustCompile(`\btruncate(?: table)? [^;]*\b(devices|users)\b`)
)

// writesDevices: SQL that deletes from devices, merges into it, changes a
// phone's step-up key or its owner, or truncates devices or users (a
// truncate fires no row trigger: 0189's header).
func writesDevices(raw string) bool {
	s := normaliseSQL(raw)
	if devicesDeleteRE.MatchString(s) || devicesMergeRE.MatchString(s) || devicesTruncateRE.MatchString(s) {
		return true
	}
	for _, m := range devicesUpdateRE.FindAllStringSubmatch(s, -1) {
		if strings.Contains(m[1], "stepup") || strings.Contains(m[1], "owner_id") {
			return true
		}
	}
	return false
}

// TestTheDevicesWritersAreALedger: every SQL in the module's non-test Go that
// deletes a phone, changes its step-up key or owner, merges into devices, or
// truncates devices or users. Each row-level one is covered by migration
// 0189's trigger, which is what marks the account; a truncate would not be,
// so there must be none. The ledger is how a new writer gets looked at: it
// fails when the set grows or shrinks.
func TestTheDevicesWritersAreALedger(t *testing.T) {
	root := filepath.Join("..", "..")
	got := moduleSQLSites(t, root, writesDevices)
	want := []string{
		"internal/devices/devices.go:Remove",      // a phone removed (by itself, its owner, or an admin's page)
		"internal/users/users.go:deleteDevicesTx", // every phone: sign out everywhere, a disable, a reset, the recovery
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("SQL writing devices (or truncating devices/users): %v\nwant exactly %v", got, want)
	}
	// The trigger the ledger leans on is in the schema's migrations, for each
	// of the statements and both columns.
	mig, err := os.ReadFile(filepath.Join(root, "internal", "db", "migrations", "0189_devices_mark_strong_factor.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`AFTER INSERT OR DELETE OR UPDATE OF stepup_pubkey, owner_id ON public\.devices`).Match(mig) {
		t.Fatal("0189's trigger does not cover INSERT, DELETE and UPDATE OF stepup_pubkey, owner_id on devices")
	}
	// CONTROLS: every spelling measured to walk around a plain match is
	// caught, and the near misses are not.
	for _, q := range []string{
		"DELETE FROM devices WHERE id = $1",
		`DELETE FROM "devices" WHERE id = $1`,
		`DELETE FROM public."devices" WHERE id = $1`,
		"DELETE FROM ONLY devices WHERE id = $1",
		"DELETE/**/FROM devices",
		"DELETE -- a comment\n FROM\n\tdevices",
		"delete from public . devices",
		"MERGE INTO devices d USING x ON d.id = x.id WHEN MATCHED THEN DELETE",
		"UPDATE devices SET owner_id = $2 WHERE id = $1",
		"UPDATE devices SET stepup_pubkey = NULL WHERE id = $1",
		`UPDATE ONLY "devices" AS d SET name = 'x', stepup_key_type = NULL`,
		"TRUNCATE devices",
		"TRUNCATE TABLE ONLY public.devices",
		"TRUNCATE users CASCADE",
		"TRUNCATE audit_log, users RESTART IDENTITY CASCADE",
	} {
		if !writesDevices(q) {
			t.Errorf("control: the matcher misses %q", q)
		}
	}
	for _, q := range []string{
		"DELETE FROM device_pairing_codes WHERE owner_id = $1",
		"UPDATE devices SET last_seen_at = now() WHERE id = $1",
		"SELECT owner_id, stepup_pubkey FROM devices WHERE id = $1",
		"UPDATE users SET had_strong_factor_at = NULL WHERE id = $1",
	} {
		if writesDevices(q) {
			t.Errorf("control: the matcher flags %q", q)
		}
	}
}

// moduleSQLSites is every "<path>:<func>" (or "<path>:<file scope>") of the
// module's non-test, non-migration Go whose folded string constants satisfy
// match.
func moduleSQLSites(t *testing.T, root string, match func(string) bool) []string {
	t.Helper()
	var out []string
	for _, rel := range moduleGoFiles(t, root) {
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		for _, site := range literalSites(t, rel, src) {
			if match(site.text) {
				out = append(out, rel+":"+site.fn)
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

type literalSite struct{ fn, text string }

// literalSites is every string constant expression in src, with the function
// it sits in ("" at file scope). 🔴 CONSTANT-FOLDED: a chain of string
// literals joined with +, and an identifier naming a string constant declared
// in the same file, fold into the one string they make — so a split literal
// ("had_strong_" + "factor_at") or a constant holding one half is read whole.
// Every literal on its own is reported too.
func literalSites(t *testing.T, name string, src []byte) []literalSite {
	t.Helper()
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	consts := map[string]string{}
	for _, d := range af.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, sp := range g.Specs {
			vs := sp.(*ast.ValueSpec)
			for i, n := range vs.Names {
				if i < len(vs.Values) {
					if s, ok := foldString(vs.Values[i], consts); ok {
						consts[n.Name] = s
					}
				}
			}
		}
	}
	var out []literalSite
	visit := func(fn string, root ast.Node) {
		ast.Inspect(root, func(n ast.Node) bool {
			e, ok := n.(ast.Expr)
			if !ok {
				return true
			}
			switch e.(type) {
			case *ast.BasicLit, *ast.BinaryExpr, *ast.Ident:
				if s, ok := foldString(e, consts); ok {
					out = append(out, literalSite{fn, s})
				}
			}
			return true
		})
	}
	for _, d := range af.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok {
			visit(fd.Name.Name, fd)
		} else {
			visit("", d)
		}
	}
	return out
}

func foldString(e ast.Expr, consts map[string]string) (string, bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(x.Value)
		return s, err == nil
	case *ast.Ident:
		s, ok := consts[x.Name]
		return s, ok
	case *ast.ParenExpr:
		return foldString(x.X, consts)
	case *ast.BinaryExpr:
		if x.Op != token.ADD {
			return "", false
		}
		l, ok1 := foldString(x.X, consts)
		r, ok2 := foldString(x.Y, consts)
		return l + r, ok1 && ok2
	}
	return "", false
}

// strongFactorColumn names the sticky record's column in any spelling a
// query could use; strongFactorHalves are the pieces a split literal could
// carry on its own.
const strongFactorColumn = "had_strong_factor_at"

var strongFactorHalves = []string{"had_strong", "strong_factor"}

func mentionsStrongFactor(s string) bool {
	l := strings.ToLower(s)
	if strings.Contains(l, strongFactorColumn) {
		return true
	}
	for _, h := range strongFactorHalves {
		if strings.Contains(l, h) {
			return true
		}
	}
	return false
}

// readsStrongFactor: SQL that READS the column (a SELECT naming it, or a
// RETURNING of it), as opposed to writing it.
func readsStrongFactor(s string) bool {
	l := strings.ToLower(s)
	return mentionsStrongFactor(l) && (regexp.MustCompile(`select\b[^;]*had_strong`).MatchString(l) || regexp.MustCompile(`returning\b[^;]*had_strong`).MatchString(l))
}

// TestTheStrongFactorColumnIsALedger: 🟡 the SQL level of the one-rule
// ledger. In the module's non-test Go, the sticky record's column is READ by
// one store method — users.PGStore.HadStrongFactor, whose only caller is
// hadStrongFactor, whose only caller is accountStrongFactors
// (TestOnlyOneFunctionReadsTheStrongFactorRecord) — and WRITTEN by the listed
// methods alone. The scan folds string constants (literalSites), so a split
// literal or a renamed method is caught by design; the controls below build
// both and watch the scan see them.
func TestTheStrongFactorColumnIsALedger(t *testing.T) {
	root := filepath.Join("..", "..")
	reads := moduleSQLSites(t, root, readsStrongFactor)
	if want := []string{"internal/users/users.go:HadStrongFactor"}; !slices.Equal(reads, want) {
		t.Errorf("SQL reading had_strong_factor_at: %v\nwant exactly %v", reads, want)
	}
	all := moduleSQLSites(t, root, mentionsStrongFactor)
	want := []string{
		"e2e/seed/main.go:",                         // the e2e seed's reset of user 1
		"internal/users/users.go:ClearStrongFactor", // a completed password reset
		"internal/users/users.go:HadStrongFactor",   // the one read
		"internal/users/users.go:MarkStrongFactor",  // the gate's marks
		"internal/users/users.go:ResetFactors",      // the server recovery
	}
	if !slices.Equal(all, want) {
		t.Errorf("SQL naming had_strong_factor_at: %v\nwant exactly %v", all, want)
	}
	assertCallers(t, ".", "HadStrongFactor", []string{"passkey_enrol.go:hadStrongFactor"})

	// CONTROLS, by construction: the same scan over sources made to hide a
	// reader.
	for _, tc := range []struct{ name, src, wantSite string }{
		{"a renamed store method", `package x
func (s *S) WasEverStrong() { s.q("SELECT had_strong_factor_at FROM users") }`, "WasEverStrong"},
		{"a split literal", `package x
func (s *S) Sneaky() { s.q("SELECT had_strong_" + "factor_at FROM users") }`, "Sneaky"},
		{"a constant holding the column", `package x
const col = "had_strong_" + "factor_at"
func (s *S) ViaConst() { s.q("SELECT " + col + " FROM users") }`, "ViaConst"},
	} {
		var sites []string
		for _, s := range literalSites(t, tc.name+".go", []byte(tc.src)) {
			if readsStrongFactor(s.text) {
				sites = append(sites, s.fn)
			}
		}
		if !slices.Contains(sites, tc.wantSite) {
			t.Errorf("control (%s): the scan did not see the reader %s: %v", tc.name, tc.wantSite, sites)
		}
	}
}
