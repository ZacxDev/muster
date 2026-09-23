package notes

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// modulePath is this module's import path. The scans below use it to recognise
// an INTERNAL import; spelled once so a module rename is a one-line change
// rather than a guard that silently stops matching anything.
const modulePath = "github.com/ZacxDev/muster"

// ---------------------------------------------------------------------------
// THE SEPARATION GUARDS.
//
// This package was carved out of a larger service. The half it left behind
// still exists, still owns tables with familiar names — permission decisions,
// session/transcript records — and is still the system this one talks to most.
// So the dependency that gets re-introduced by accident is not an exotic one;
// it is the obvious, convenient one, and BOTH of its shapes are invisible to
// the type checker:
//
//   - a SQL read of a table this schema does not contain. Nothing in Go sees
//     it. It compiles, it vets, and it fails at runtime with an error naming a
//     relation rather than the decision that produced it.
//   - a Go import of a package that is not in this module's ledger. The
//     compiler sees it and is perfectly happy; nothing FAILS, because an import
//     only becomes a problem when the two halves are in different binaries —
//     which they now are.
//
// All three guards read SOURCE, because that is where the evidence is. None of
// them needs a database, so none can redden on a slow node.
// ---------------------------------------------------------------------------

// nonTestSources returns this package's non-test .go files, as (path, content).
func nonTestSources(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	out := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		out[name] = string(raw)
	}
	if len(out) == 0 {
		t.Fatalf("the file scan found NO non-test sources in %s — it is looking at the wrong place, "+
			"and every assertion below would be a confident pass over nothing", mustAbs(t))
	}
	return out
}

func mustAbs(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(".")
	if err != nil {
		return "."
	}
	return p
}

// TestNotesNeverNamesAnotherServicesTable refuses, in non-test source, every
// table name this package is known to have read before the carve and may not
// read now.
//
// 🔴 THE LIST IS A DENYLIST OF WHAT WE KNOW, NOT A PROOF OF WHAT IS LEFT, and
// saying so is the difference between a guard and a claim. It cannot see a
// table nobody has thought of; TestNoSQLInThisPackageJoinsAnotherServicesTable
// below is the ALLOWLIST half that can, for the JOIN shape. Two guards, two
// directions, because either alone is walkable.
//
// 🔴 IT COUNTS COMMENTS TOO, DELIBERATELY. A SQL reference is the thing that
// breaks; a COMMENT naming a table this package cannot reach is a claim that is
// simply false, and it sends the next reader looking for a join that is not
// there. Keeping the target at exactly zero is also what makes the check
// mechanical — "no SQL references, but prose is fine" needs a human to
// adjudicate every hit, which is how a guard stops being run.
//
// The prose that USED to name these has been re-worded, not deleted: the
// retention story and the three transcript states are still documented in
// threads.go, in terms of "the transcript record" rather than the table that
// holds it, and pgstore.go records what `Directories` did and why it went.
func TestNotesNeverNamesAnotherServicesTable(t *testing.T) {
	srcs := nonTestSources(t)

	// POSITIVE CONTROL. A scanner that reports zero is indistinguishable from a
	// scanner wired to nothing, so make it find something it MUST find first: this
	// package's own table is named all over pgstore_threads.go.
	control := 0
	for _, body := range srcs {
		control += strings.Count(body, "task_sessions")
	}
	if control == 0 {
		t.Fatalf("positive control failed: the scan found 0 mentions of `task_sessions` across %d "+
			"files, which cannot be true. The scan is not reading what it thinks it is, so its "+
			"verdict below means nothing", len(srcs))
	}

	// Tables the OTHER service owns. Every one of these was genuinely read from
	// this package at some point, which is why each is named rather than
	// guessed:
	//
	//   cc_sessions      the transcript/session record. Joined here until the
	//                    liveness composition moved to the API layer.
	//   requests         the permission-request WORKING QUEUE.
	//   request_history  the permission-decision ARCHIVE. `Directories` read it
	//                    for the task directory picker; see pgstore.go for
	//                    where that went and why.
	denied := []string{"cc_sessions", "requests", "request_history"}

	var hits []string
	for name, body := range srcs {
		for i, line := range strings.Split(body, "\n") {
			for _, tbl := range denied {
				if strings.Contains(line, tbl) {
					hits = append(hits, name+":"+strconv.Itoa(i+1)+": ["+tbl+"] "+strings.TrimSpace(line))
					break
				}
			}
		}
	}
	if len(hits) != 0 {
		t.Fatalf("internal/notes names a table owned by the other service in %d place(s):\n  %s\n\n"+
			"None of %v is in this module's schema. A SQL reference fails at RUNTIME with an "+
			"error naming a relation; a comment compiles and lies. Re-word it, or compose the "+
			"data at the API layer where the other service is reachable over HTTP.\n"+
			"(positive control: %d mentions of `task_sessions` found, so the scan works)",
			len(hits), strings.Join(hits, "\n  "), denied, control)
	}
}

// TestNotesImportsOnlyItsTwoDependencies pins the package's INTERNAL import
// ledger.
//
// 🔴 THE FAILURE IT GUARDS AGAINST IS NOT A COMPILE ERROR, WHICH IS WHY IT NEEDS
// A TEST. An import added here builds, vets and passes every behavioural test in
// the module; it becomes wrong only in what it COUPLES. The concrete shape to
// watch for: the obvious fix for a missing piece of composed data — session
// liveness, a directory list — is to have this package fetch it itself, and that
// is precisely what must not happen. Composition of anything this package does
// not own belongs at the API layer, where the other service is an HTTP call
// rather than an import.
//
// It fails when the set GROWS or SHRINKS. A shrink is not automatically wrong,
// but it means a dependency was removed and the ledger should record that it was.
func TestNotesImportsOnlyItsTwoDependencies(t *testing.T) {
	// 🔴 ONE ENTRY, AND THE SHORTNESS IS THE POINT — do not read it as a stub.
	// taskstatus is the task lifecycle vocabulary and imports nothing itself, so
	// this package's non-test source depends on no other package in this module
	// at all. `internal/db` is NOT here: the pool and the migration runner are
	// the TESTS' dependency, injected into the store as a *pgxpool.Pool by
	// whoever constructs it, and a store that opened its own connection would be
	// a store the API layer could not hand a transaction to.
	want := map[string]bool{
		"github.com/ZacxDev/muster/internal/taskstatus": true,
	}

	fset := token.NewFileSet()
	got := map[string]bool{}
	scanned := 0
	for name := range nonTestSources(t) {
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++
		for _, spec := range f.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("%s: bad import path %s", name, spec.Path.Value)
			}
			if strings.Contains(path, modulePath+"/internal/") {
				got[path] = true
			}
		}
	}
	// POSITIVE CONTROL on the parser: the walk must actually see imports, or an
	// empty `got` would read as "no internal dependencies" for any reason at all.
	if len(got) == 0 {
		t.Fatalf("the import walk found no internal imports across %d files. This package genuinely "+
			"has two, so the walk is broken and a passing ledger would prove nothing", scanned)
	}

	for p := range got {
		if !want[p] {
			t.Fatalf("internal/notes imports %q, which is not in the ledger.\n"+
				"This package is the task DOMAIN: it owns rows and the rules about them. A new "+
				"internal dependency here is a new thing tasks cannot be read without — and if "+
				"the dependency is really a reach for data owned elsewhere, it belongs at the API "+
				"layer instead. If it is genuinely a domain dependency, add it here with the "+
				"reason.\ngot: %v\nledger: %v", p, sortedSet(got), sortedSet(want))
		}
	}
	for p := range want {
		if !got[p] {
			t.Fatalf("the ledger expects internal/notes to import %q and it does not.\n"+
				"A dependency was removed — good, probably — but say so here rather than leaving "+
				"the ledger claiming a coupling that is gone.\ngot: %v\nledger: %v",
				p, sortedSet(got), sortedSet(want))
		}
	}
}

// TestNoSQLInThisPackageJoinsAnotherServicesTable walks every string literal in
// the package's source and refuses a JOIN whose right-hand side is not one of this
// package's own tables.
//
// 🔴 IT IS WIDER THAN THE DENYLIST ABOVE ON PURPOSE, AND NARROWER IN A DIFFERENT
// DIRECTION. The denylist catches the tables we KNOW about; this catches the
// next one, whatever it is called, because it is an ALLOWLIST. WHAT IT DOES NOT
// SEE, stated plainly so the name cannot be read as wider than the body:
//
//   - a plain `FROM`/subquery/CTE read with no JOIN keyword anywhere near it.
//     There is no such read of a foreign table in this package today — the one
//     that existed, `Directories`, went at the carve (see pgstore.go) — but this
//     guard would not catch its return. Widening it to every FROM was rejected:
//     the parse would have to distinguish a relation from a subquery alias in
//     string literals assembled across lines, and a guard that produces false
//     positives is a guard people switch off.
//   - anything assembled at runtime rather than written as a string literal.
func TestNoSQLInThisPackageJoinsAnotherServicesTable(t *testing.T) {
	// Every table this package may read, and all of them are in this module's
	// schema (internal/db/migrations). `agents` is here because a task carries
	// the agent that worked it; it is a table muster owns, not an exception.
	ours := map[string]bool{
		"notes": true, "note_attachments": true, "note_comments": true,
		"task_sessions": true, "agents": true,
	}

	fset := token.NewFileSet()
	var joins []string
	for name := range nonTestSources(t) {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			for _, tbl := range joinedTables(lit.Value) {
				if !ours[tbl] {
					joins = append(joins, name+": JOIN "+tbl)
				}
			}
			return true
		})
	}
	if len(joins) != 0 {
		t.Fatalf("internal/notes JOINs table(s) it does not own:\n  %s\n\n"+
			"Every table this package reads must be one this module's migrations create. A join "+
			"onto anything else fails at runtime with an error naming a relation, and nothing in "+
			"Go will have said so. If the table is genuinely muster's, add it to `ours` with the "+
			"migration that creates it.", strings.Join(joins, "\n  "))
	}

	// POSITIVE CONTROL — the finder must see a join when there is one to see.
	if got := joinedTables("`SELECT 1 FROM a LEFT JOIN elsewhere_sessions cs ON cs.x = a.x`"); len(got) != 1 || got[0] != "elsewhere_sessions" {
		t.Fatalf("positive control: joinedTables(<a real LEFT JOIN>) = %v, want [elsewhere_sessions]. "+
			"The finder cannot see a join, so its zero above is meaningless", got)
	}
	if got := joinedTables("`SELECT 1 FROM a JOIN notes n ON n.id = a.id`"); len(got) != 1 || got[0] != "notes" {
		t.Fatalf("positive control: a plain JOIN was not seen: %v", got)
	}
	// 🔴 THE LATERAL CONTROL. Without the descent this returns ["LATERAL"], which
	// is not a table, is in nobody's ledger, and would make the whole guard red for
	// a construct this package uses legitimately — or, if "LATERAL" were quietly
	// allowlisted, would hide every foreign table read through one.
	if got := joinedTables("`SELECT 1 FROM n LEFT JOIN LATERAL (SELECT max(x) FROM elsewhere_sessions c WHERE c.id = n.id) q ON true`"); len(got) != 1 || got[0] != "elsewhere_sessions" {
		t.Fatalf("positive control: a foreign table read through JOIN LATERAL was seen as %v, want [elsewhere_sessions]", got)
	}
	if got := joinedTables("`no sql here at all`"); len(got) != 0 {
		t.Fatalf("negative control: joinedTables found %v in a string with no JOIN", got)
	}
}

// joinedTables extracts the relation each JOIN in a SQL string literal reads.
// Case-insensitive; aliases dropped.
//
// `JOIN LATERAL (SELECT … FROM t …)` is DESCENDED INTO rather than skipped — the
// subquery's FROM is the relation actually joined, and this package uses that form
// three times. Treating it as "not a table" would have been a hole the size of the
// construct.
func joinedTables(lit string) []string {
	var out []string
	upper := strings.ToUpper(lit)
	for i := 0; ; {
		j := strings.Index(upper[i:], "JOIN ")
		if j < 0 {
			return out
		}
		i += j + len("JOIN ")
		rest := strings.TrimLeft(lit[i:], " \t\r\n")
		tok := leadingIdent(rest)
		if strings.EqualFold(tok, "LATERAL") {
			rest = strings.TrimLeft(rest[len(tok):], " \t\r\n")
			if strings.HasPrefix(rest, "(") {
				// The joined relation is whatever the subquery selects FROM.
				if k := strings.Index(strings.ToUpper(rest), "FROM "); k >= 0 {
					tok = leadingIdent(strings.TrimLeft(rest[k+len("FROM "):], " \t\r\n"))
				} else {
					tok = ""
				}
			} else {
				tok = leadingIdent(rest)
			}
		}
		if tok != "" {
			out = append(out, tok)
		}
	}
}

// leadingIdent returns the SQL identifier at the head of s ("" if there is none).
func leadingIdent(s string) string {
	end := strings.IndexFunc(s, func(r rune) bool {
		return !(r == '_' || r == '.' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
	})
	if end < 0 {
		return s
	}
	return s[:end]
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
