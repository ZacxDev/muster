package mirror_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// runtimeConst reports whether an identifier is one of internal/mirror's
// names for a runtime: the runtime constants.
func runtimeConst(name string) bool {
	return name == "RuntimeClaude" || name == "RuntimeOpenCode"
}

// ledgerScope is the mirror surface the ledger reads: the server and host
// halves, the API and UI that act on mirrored windows, focus keys, and the
// host binary.
var ledgerScope = []string{"internal/mirror/", "internal/mirrorhost/", "internal/api/", "internal/ui/", "internal/focus/", "cmd/muster-mirror/"}

// runtimeSites is every occurrence, in the non-test Go source of
// ledgerScope, of a runtime constant (runtimeConst: mirror.X, or bare inside
// internal/mirror), counted per function: "<file> <func> ×<n>". It also
// returns how many files it parsed.
func runtimeSites(t *testing.T, root string) ([]string, int) {
	t.Helper()
	counts := map[string]int{}
	files := 0
	mirrorDir := filepath.Join("internal", "mirror") + string(filepath.Separator)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", ".git", "testdata", "e2e", "web":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		relPath, _ := filepath.Rel(root, path)
		if !slices.ContainsFunc(ledgerScope, func(d string) bool { return strings.HasPrefix(filepath.ToSlash(relPath), d) }) {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		files++
		rel, _ := filepath.Rel(root, path)
		inMirror := strings.HasPrefix(rel, mirrorDir)
		for _, decl := range f.Decls {
			where := "(declarations)"
			if fd, ok := decl.(*ast.FuncDecl); ok {
				where = fd.Name.Name
				if fd.Recv != nil && len(fd.Recv.List) == 1 {
					switch r := fd.Recv.List[0].Type.(type) {
					case *ast.Ident:
						where = r.Name + "." + where
					case *ast.StarExpr:
						if id, ok := r.X.(*ast.Ident); ok {
							where = id.Name + "." + where
						}
					}
				}
			}
			key := rel + " " + where
			// A selector's Sel and a key's Ident are counted once, there, not
			// again as bare identifiers.
			done := map[*ast.Ident]bool{}
			ast.Inspect(decl, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.SelectorExpr:
					done[x.Sel] = true
					pkg, _ := x.X.(*ast.Ident)
					if pkg != nil && pkg.Name == "mirror" && runtimeConst(x.Sel.Name) {
						counts[key]++
					}
				case *ast.Ident:
					if inMirror && !done[x] && runtimeConst(x.Name) {
						counts[key]++
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var sites []string
	for k, n := range counts {
		sites = append(sites, fmt.Sprintf("%s ×%d", k, n))
	}
	sort.Strings(sites)
	return sites, files
}

// TestEveryPlaceThatNamesARuntimeIsListed is a SEAM LEDGER over the whole
// module's non-test source: every OCCURRENCE of a runtime's name or of a
// value that is one runtime's own (runtimeConst: the runtime constants) is
// counted per function below. The ledger fails when a
// function's count GROWS or SHRINKS, or a function appears or goes. Per-
// runtime decisions belong to the runtime interface (mirror.RuntimeSpec, the
// mirrorhost Runtime); what names a runtime outside it is one of:
//   - the interface's own tables (runtime.go, both halves);
//   - a runtime's implementation checking that a window is its own, or
//     speaking its own wire (opencode's locator, stream, listing, asks and
//     answer);
//   - Claude Code's wire, which is Window.Claude and per-session hosts, never
//     Window.Runtime;
//   - per-runtime WORDS (the queue's copy), keyed by name.
//
// A new site that branches on a runtime's name fails here: route the
// decision through the interface, or add it on purpose.
func TestEveryPlaceThatNamesARuntimeIsListed(t *testing.T) {
	sites, files := runtimeSites(t, filepath.Join("..", ".."))
	if files < 60 {
		t.Fatalf("instrument check: parsed %d source files; is the test running in internal/mirror?", files)
	}
	want := []string{
		"cmd/muster-mirror/opencode_list.go printOpenCode ×1",
		// The chief's stall nudges judge opencode panes — and only those of the
		// non-Claude runtimes — by the owner's choice (chief_nudge.go).
		"internal/api/chief_nudge.go nudgeWindowKey ×1",
		// The queue's per-runtime words: Claude Code's and opencode's (the
		// opencode API send, docs/design/opencode-first-class.md).
		"internal/api/send_queue.go (declarations) ×2",
		"internal/api/send_queue.go queueWordsOf ×1",
		// A chief subject's runtime LABEL (view_chief.go): a Claude Code window
		// has no RuntimeSpec (Spec is false for it), so it is named "claude"
		// here; every other runtime's name comes from its spec. It decides
		// nothing — it is the turn header's and session_context's word.
		"internal/api/view_chief.go Server.sessionSubject ×1",
		"internal/mirror/agentstate.go Hub.AgentState ×1",
		// The host view's opencode flags: OpenCodeAnswer and OpenCodeSend.
		"internal/mirror/hub.go Hub.Hosts ×2",
		"internal/mirror/hub.go hostConn.target ×1",
		"internal/mirror/hub.go hostConn.targetRuntime ×1",
		"internal/mirror/opencode_answer.go Hub.AnswerOpenCodeAsk ×3",
		// An opencode dispatch's final reply rides only an opencode window: it
		// is opencode's own wire (outcomes.go), the runtime with no muster tool.
		"internal/mirror/outcomes.go NormalizeOutcomes ×1",
		"internal/mirror/runtime.go (declarations) ×4",
		"internal/mirror/transcript.go hostConn.claudeWaiting ×1",
		"internal/mirror/turnerror.go boundTurnError ×1",
		"internal/mirror/wire.go (declarations) ×1",
		"internal/mirror/wire.go Window.AgentRuntime ×1",
		"internal/mirrorhost/opencode.go ResolveOpenCodePaneStarted ×1",
		"internal/mirrorhost/opencode.go openCodeRuntime.LocatePanes ×1",
		"internal/mirrorhost/opencode_asks.go hostSide.answerOpenCode ×1",
		// The host reading an opencode dispatch window's final reply checks the
		// window is opencode's (opencode_outcome.go).
		"internal/mirrorhost/opencode_outcome.go OpenCodeSource.dispatchOutcomes ×1",
		// The gated send checking the window is opencode's NOW.
		"internal/mirrorhost/opencode_send.go hostSide.sendOpenCode ×1",
		"internal/mirrorhost/opencode_stream.go hostSide.openCodeGone ×1",
		"internal/mirrorhost/opencode_stream.go hostSide.openCodeWindow ×1",
		"internal/mirrorhost/runtime.go claudeRuntime.Name ×1",
		"internal/mirrorhost/runtime.go openCodeRuntime.Name ×1",
		"internal/mirrorhost/transcript.go hostSide.watch ×1",
	}
	if !slices.Equal(sites, want) {
		t.Fatalf("the functions that name a runtime are\n  %s\nwant exactly\n  %s\nA per-runtime decision goes through the runtime interface (mirror.RuntimeSpec / mirrorhost.Runtime); update this ledger only on purpose",
			strings.Join(sites, "\n  "), strings.Join(want, "\n  "))
	}
}
