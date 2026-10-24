package mirror_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestEveryPlaceThatNamesARuntimeIsListed is a SEAM LEDGER over the whole
// module's non-test source: every function that names a runtime
// (RuntimeClaude, RuntimeHermes, RuntimeOpenCode or HermesKeyPrefix) is
// listed below, and the list fails when it GROWS or SHRINKS. Per-runtime
// decisions belong to the runtime interface (mirror.RuntimeSpec, the
// mirrorhost Runtime); what names a runtime outside it is one of:
//   - the interface's own tables (runtime.go, both halves);
//   - a runtime's implementation checking that a window is its own (Hermes's
//     send, asks, stream, answer, listing; its e2e fake);
//   - Claude Code's wire, which is Window.Claude and per-session hosts, never
//     Window.Runtime;
//   - per-runtime WORDS (the queue's and Send now's copy), keyed by name.
//
// A new site that branches on a runtime's name fails here: route the
// decision through the interface, or add it on purpose.
func TestEveryPlaceThatNamesARuntimeIsListed(t *testing.T) {
	root := filepath.Join("..", "..")
	names := map[string]bool{"RuntimeClaude": true, "RuntimeHermes": true, "RuntimeOpenCode": true, "HermesKeyPrefix": true}
	var sites []string
	files := 0
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
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		files++
		rel, _ := filepath.Rel(root, path)
		seen := map[string]bool{}
		note := func(where string) {
			if !seen[where] {
				seen[where] = true
				sites = append(sites, rel+" "+where)
			}
		}
		for _, decl := range f.Decls {
			where := ""
			switch d := decl.(type) {
			case *ast.FuncDecl:
				where = d.Name.Name
				if d.Recv != nil && len(d.Recv.List) == 1 {
					switch r := d.Recv.List[0].Type.(type) {
					case *ast.Ident:
						where = r.Name + "." + where
					case *ast.StarExpr:
						if id, ok := r.X.(*ast.Ident); ok {
							where = id.Name + "." + where
						}
					}
				}
			case *ast.GenDecl:
				where = "(declarations)"
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.SelectorExpr:
					if names[x.Sel.Name] {
						if id, ok := x.X.(*ast.Ident); ok && id.Name == "mirror" {
							note(where)
						}
					}
				case *ast.Ident:
					if names[x.Name] && strings.HasPrefix(rel, filepath.Join("internal", "mirror")+string(filepath.Separator)) {
						note(where)
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
	if files < 100 {
		t.Fatalf("instrument check: parsed %d source files; is the test running in internal/mirror?", files)
	}
	slices.Sort(sites)
	want := []string{
		"cmd/muster-mirror/e2ehermes.go hermesWindowsE2E",
		"cmd/muster-mirror/hermes_list.go printHermes",
		"internal/api/send_queue.go (declarations)",
		"internal/api/send_queue.go queueWordsOf",
		"internal/api/send_queue_now.go (declarations)",
		"internal/focus/focus.go HermesSubject",
		"internal/mirror/hermes_answer.go Hub.AnswerHermesApproval",
		"internal/mirror/hub.go Hub.Hosts",
		"internal/mirror/hub.go hostConn.target",
		"internal/mirror/hub.go hostConn.targetRuntime",
		"internal/mirror/order.go SessionView.HermesView",
		"internal/mirror/runtime.go (declarations)",
		"internal/mirror/wire.go (declarations)",
		"internal/mirror/wire.go Window.AgentRuntime",
		"internal/mirrorhost/hermes_asks.go hermesAsks.attachPending",
		"internal/mirrorhost/hermes_asks.go hostSide.answerHermes",
		"internal/mirrorhost/hermes_send.go hermesStillIdle",
		"internal/mirrorhost/hermes_send.go hostSide.hermesPaste",
		"internal/mirrorhost/hermes_stream.go hostSide.hermesWindow",
		"internal/mirrorhost/runtime.go claudeRuntime.Name",
		"internal/mirrorhost/runtime.go hermesRuntime.LocatePanes",
		"internal/mirrorhost/runtime.go hermesRuntime.Name",
		"internal/mirrorhost/runtime.go openCodeRuntime.Name",
		"internal/mirrorhost/transcript.go hostSide.watch",
	}
	if !slices.Equal(sites, want) {
		t.Fatalf("the functions that name a runtime are\n  %s\nwant exactly\n  %s\nA per-runtime decision goes through the runtime interface (mirror.RuntimeSpec / mirrorhost.Runtime); update this ledger only on purpose",
			strings.Join(sites, "\n  "), strings.Join(want, "\n  "))
	}
}

// TestTheRuntimeTableIsWhatTheRefactorPromised: the three runtimes, Claude
// Code first, with the capabilities each has today — and the opencode stub
// a runtime of "view: no, send: no" that the wire does not carry and no key
// names. The session key pattern built from the table is the one migration
// 0036 spells (internal/db pins the migration side).
func TestTheRuntimeTableIsWhatTheRefactorPromised(t *testing.T) {
	var got []string
	for _, r := range mirror.Runtimes() {
		got = append(got, r.Name)
	}
	if want := []string{mirror.RuntimeClaude, mirror.RuntimeHermes, mirror.RuntimeOpenCode}; !slices.Equal(got, want) {
		t.Fatalf("runtimes %q, want %q", got, want)
	}
	for name, want := range map[string]mirror.Capabilities{
		mirror.RuntimeClaude:   {View: true, Send: true, Prompts: true},
		mirror.RuntimeHermes:   {View: true, Send: true, Prompts: true},
		mirror.RuntimeOpenCode: {},
	} {
		r, ok := mirror.LookupRuntime(name)
		if !ok || r.Caps != want {
			t.Errorf("%s: %+v (%v), want capabilities %+v", name, r.Caps, ok, want)
		}
	}
	oc, _ := mirror.LookupRuntime(mirror.RuntimeOpenCode)
	if oc.OnWire || oc.KeyPrefix != "" || oc.GatedSend || oc.Kind != mirror.KindOpenCode || oc.HasView("ses_x") {
		t.Fatalf("the opencode stub does more than report itself: %+v", oc)
	}
	if mirror.SessionKeyPattern != `^([0-9A-Za-z][0-9A-Za-z_-]{0,63}|hermes:[0-9A-Za-z][0-9A-Za-z_-]{0,127})$` {
		t.Fatalf("the session key pattern moved: %s", mirror.SessionKeyPattern)
	}
	if _, ok := mirror.LookupRuntime("bogus"); ok {
		t.Fatal("an unknown runtime name was found")
	}
	if got := mirror.SessionIDOf("hermes:local-1"); got != "local-1" {
		t.Errorf("SessionIDOf(hermes key) = %q", got)
	}
	if got := mirror.SessionIDOf("c-1"); got != "c-1" {
		t.Errorf("SessionIDOf(claude key) = %q", got)
	}
}
