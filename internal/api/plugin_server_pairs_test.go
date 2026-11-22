package api

import (
	"go/ast"
	"go/token"
	"regexp"
	"strings"
	"testing"
)

// LINKED SERVER APPS: THE STRUCTURAL PINS (no database; docs/design/
// contacts.md, "The code seams"). Each is a LEDGER over the module's non-test
// Go, failing when its set GROWS or SHRINKS.

// TestOneFunctionChoosesASubject: newSubject has ONE caller, subjectFor, and
// subjectFor has exactly the two writers of a plugin_server_users row as its
// callers (the redeem and Hide). A third writer calling newSubject directly
// would skip the pair; a writer dropped from subjectFor would too.
func TestOneFunctionChoosesASubject(t *testing.T) {
	newSubject, subjectFor := map[string]bool{}, map[string]bool{}
	moduleFuncs(t, func(site string, n ast.Node) {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return
		}
		if id, ok := call.Fun.(*ast.Ident); ok {
			switch id.Name {
			case "newSubject":
				newSubject[site] = true
			case "subjectFor":
				subjectFor[site] = true
			}
		}
	})
	if got := setOf(newSubject); strings.Join(got, ",") != "internal/plugins subjectFor" {
		t.Errorf("callers of newSubject: %v; want only internal/plugins subjectFor", got)
	}
	if got, want := setOf(subjectFor), []string{"internal/plugins PGStore.RedeemServerHandoff", "internal/plugins PGStore.SetServerAppHidden"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("callers of subjectFor: %v; want %v", got, want)
	}
}

// TestThePairsTableIsReadOnlyUnderTheSystem: every SQL literal naming
// plugin_server_pairs is in one of the listed functions, and each of those
// either calls authz.System itself or is handed a System transaction by a
// caller that does (subjectFor by the redeem and Hide, linkAtInstall by
// InstallServerApp). The table's only policy is scope_all, so a read under any
// other principal sees nothing (TestLinkRefusals reads it as a person).
func TestThePairsTableIsReadOnlyUnderTheSystem(t *testing.T) {
	sites, system, linkers := map[string]bool{}, map[string]bool{}, map[string]bool{}
	moduleFuncs(t, func(site string, n ast.Node) {
		switch x := n.(type) {
		case *ast.CallExpr:
			if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "linkAtInstall" {
				linkers[site] = true
			}
		case *ast.BasicLit:
			if x.Kind == token.STRING && strings.Contains(x.Value, "plugin_server_pairs") {
				sites[site] = true
			}
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok && id.Name == "authz" && x.Sel.Name == "System" {
				system[site] = true
			}
		}
	})
	want := []string{
		"internal/plugins PGStore.UninstallServerApp",
		"internal/plugins PGStore.UnlinkServerApp",
		"internal/plugins PGStore.serverPairs",
		"internal/plugins linkAtInstall",
		"internal/plugins subjectFor",
	}
	got := setOf(sites)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("functions spelling plugin_server_pairs:\n got %v\nwant %v", got, want)
	}
	handed := map[string]bool{"internal/plugins subjectFor": true, "internal/plugins linkAtInstall": true}
	// linkAtInstall is handed the install's System transaction, by the
	// install alone: a pair is made nowhere else.
	if got := setOf(linkers); strings.Join(got, ",") != "internal/plugins PGStore.InstallServerApp" {
		t.Errorf("callers of linkAtInstall: %v; want only internal/plugins PGStore.InstallServerApp", got)
	}
	for _, s := range got {
		if !system[s] && !handed[s] {
			t.Errorf("%s spells plugin_server_pairs but never calls authz.System", s)
		}
	}
}

// subjectRewrite matches a statement on plugin_server_users that SETs its
// subject: the one write a link must never make.
var subjectRewrite = regexp.MustCompile(`(?is)plugin_server_users\b.*\bSET\b[^;]*?\bsubject\s*=`)

// TestNothingRewritesASubject: no SQL literal in the module sets
// plugin_server_users.subject (a subject is written once, when its row is
// inserted, and never moved). Positive controls first: the pattern must
// catch an UPDATE and an upsert that do.
func TestNothingRewritesASubject(t *testing.T) {
	for _, bad := range []string{
		"UPDATE plugin_server_users SET subject = $1 WHERE plugin_id = $2",
		"INSERT INTO plugin_server_users (plugin_id, owner_id, subject) VALUES ($1, $2, $3) ON CONFLICT (plugin_id, owner_id) DO UPDATE SET act_until = now(), subject = EXCLUDED.subject",
	} {
		if !subjectRewrite.MatchString(bad) {
			t.Fatalf("control: the pattern misses %q", bad)
		}
	}
	if subjectRewrite.MatchString("INSERT INTO plugin_server_users (plugin_id, subject) VALUES ($1, $2) ON CONFLICT DO UPDATE SET act_until = now() RETURNING subject, act_until") {
		t.Fatal("control: the pattern matches an upsert that only RETURNS the subject")
	}
	found := 0
	moduleFuncs(t, func(site string, n ast.Node) {
		if x, ok := n.(*ast.BasicLit); ok && x.Kind == token.STRING && strings.Contains(x.Value, "plugin_server_users") {
			found++
			if subjectRewrite.MatchString(x.Value) {
				t.Errorf("%s rewrites a subject: %s", site, x.Value)
			}
		}
	})
	if found < 5 {
		t.Errorf("positive control: %d literals naming plugin_server_users were read; want the store's", found)
	}
}
