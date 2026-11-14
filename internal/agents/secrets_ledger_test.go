package agents

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// secretColumn matches an agent credential column named in SQL.
var secretColumn = regexp.MustCompile(`\b(hooks_token|gateway_secret)\b`)

// secretQueryLedger is EVERY string literal in the module's non-test Go source
// that names hooks_token or gateway_secret, keyed by file and enclosing
// function (with a count when one function holds several), and why each may.
//
// 🔴 A LEDGER, NOT A SPOT CHECK (team sharing phase 4, slice 4.2). The
// credentials moved to agent_secrets so that an ordinary read of an agents row
// — the one a later slice makes readable by a team's members — never carries
// them. That holds only while every query naming them is one of the narrow
// ones below; a new SELECT that reads either column back into a row, wherever
// it lives, would quietly undo it. So this fails when the set GROWS (a new
// reader or writer: decide, then add it here with its reason) and when it
// SHRINKS (a stale entry would vouch for a query that no longer exists).
// It sees Go string literals only; migrations (.sql) are the schema, pinned by
// internal/db's own ledgers.
var secretQueryLedger = map[string]string{
	// The store's narrow paths (internal/agents pgstore.go).
	"internal/agents/pgstore.go:Create":           "WRITE: a create's token, into agent_secrets and (dual-write) agents",
	"internal/agents/pgstore.go:GetByHooksToken":  "LOOKUP by the presented token: agent_secrets first, the old column as fallback; selects agentCols, so returns no credential",
	"internal/agents/pgstore.go:Secrets":          "READ: the one read of an agent's credentials, by id",
	"internal/agents/pgstore.go:SetHooksToken":    "WRITE: the mint, into agent_secrets and (dual-write) agents",
	"internal/agents/pgstore.go:SetGatewaySecret": "WRITE: the mint, into agent_secrets and (dual-write) agents",
	// e2e fixtures seed rows straight into the old column; the store's fallback
	// reads it. They run against a throwaway database only.
	"e2e/ccfix/main.go:run":       "FIXTURE WRITE: seeds the old column (e2e only)",
	"e2e/heldfix/main.go:run":     "FIXTURE WRITE: seeds the old column (e2e only)",
	"e2e/pluginfix/main.go:run#2": "FIXTURE WRITE: seeds the old column (e2e only), twice",
}

// TestEveryQueryNamingAnAgentCredentialIsClassified walks the module and
// compares what it finds with secretQueryLedger.
func TestEveryQueryNamingAnAgentCredentialIsClassified(t *testing.T) {
	root := moduleRoot(t)
	found := map[string]int{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); path != root && (strings.HasPrefix(n, ".") || n == "vendor" || n == "node_modules" || n == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		for _, decl := range f.Decls {
			name := "<package>"
			if fd, ok := decl.(*ast.FuncDecl); ok {
				name = fd.Name.Name
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					s = lit.Value
				}
				if secretColumn.MatchString(s) {
					found[rel+":"+name]++
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	// Positive control: the walk can see a query at all — the store's own
	// reader must be among what it found.
	if found["internal/agents/pgstore.go:Secrets"] == 0 {
		t.Fatalf("instrument check FAILED: the walk did not find PGStore.Secrets's query (found %v)", found)
	}
	got := make([]string, 0, len(found))
	for k, n := range found {
		if n > 1 {
			k += "#" + strconv.Itoa(n)
		}
		got = append(got, k)
	}
	want := make([]string, 0, len(secretQueryLedger))
	for k := range secretQueryLedger {
		want = append(want, k)
	}
	sort.Strings(got)
	sort.Strings(want)
	for _, k := range got {
		if _, ok := secretQueryLedger[k]; !ok {
			t.Errorf("UNCLASSIFIED: %s names hooks_token or gateway_secret in a string literal. An agent's "+
				"credentials are read only through agents.Store.Secrets (agent_secrets); if this query must "+
				"name them, add it to secretQueryLedger with the reason", k)
		}
	}
	for _, k := range want {
		if !slices.Contains(got, k) {
			t.Errorf("STALE: secretQueryLedger names %s, which no longer names either column — remove it", k)
		}
	}
}

// TestAgentColsNamesNoCredential: the column list every ordinary read selects
// names neither credential, and scanAgent leaves both fields empty.
func TestAgentColsNamesNoCredential(t *testing.T) {
	if m := secretColumn.FindString(agentCols); m != "" {
		t.Fatalf("agentCols selects %s: every ordinary read of an agents row would carry the agent's credential", m)
	}
}

// moduleRoot is the directory holding go.mod, walking up from this package.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's directory")
		}
		dir = parent
	}
}
