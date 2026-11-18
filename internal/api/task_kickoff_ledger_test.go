package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// THE TASK-KICKOFF SEAM LEDGER (task_kickoff.go).
//
// 🔴 THE HAZARD. Task text a plugin wrote must reach an agent only inside the
// untrusted fence. The rule is one function, taskKickoffText (with its
// byte-budgeted twin taskKickoffTextWithin), and it only holds if EVERY door
// that starts an agent on a task builds the first message through it. Four
// doors once pasted task.Body raw; a fifth added tomorrow would too, and each
// door's own tests would stay green.
//
// So this pins a RELATIONSHIP, two ways, by type (go/types), not by spelling:
//
//   - every top-level function in this package that reads a task's text — the
//     Body, Title or Comments field of a notes.Note, by any variable name — is
//     in expectedTaskTextReaders with WHY it may: it builds a kickoff through
//     the helper, or the text it reads never becomes an agent's first message
//     (owner-facing UI, push titles, a route that fences every task anyway). A
//     new reader fails until someone decides which it is; a reader that is
//     gone fails too, so the ledger cannot rot into a superset.
//   - the helper's callers are exactly the kickoff readers, and each kickoff
//     reader calls it.
//
// ⚠ WHAT THIS DOES NOT CHECK, so nobody over-reads it:
//   - that a function scored "not a kickoff" really never hands the text to an
//     agent. That is a judgement the ledger makes reviewable, not one it proves.
//   - a function that passes a WHOLE notes.Note on without selecting a field:
//     toolTaskGet marshals the note into task_get's answer (a tool result, not
//     a first message: docs/limits.md says so), and a helper in another package
//     handed a whole Note would read its fields out of this scan's sight.
//   - a reader one call away: taskNameRaw is ledgered, its callers
//     (taskLabel, spawnName, …) are not.
// The per-door tests (task_kickoff_sites_test.go, task_kickoff_sites_pg_test.go)
// drive the four kickoffs for real.
// ---------------------------------------------------------------------------

// kickoffViaHelper marks a reader that builds an agent's first message from a
// task, and must do it through taskKickoffText / taskKickoffTextWithin.
const kickoffViaHelper = "kickoff"

// expectedTaskTextReaders is every function in this package that reads a
// notes.Note's Body, Title or Comments, mapped to kickoffViaHelper or to why the
// text it reads is not an agent's first message.
var expectedTaskTextReaders = map[string]string{
	// The doors that start an agent on a task — the text goes through the rule.
	"toolAgentDispatch":    kickoffViaHelper, // the chief's agent_dispatch (it also names the agent from the body)
	"toolDispatchApprove":  kickoffViaHelper, // a session's dispatch_request, approved (and the agent's name)
	"handleAPIAgentCreate": kickoffViaHelper, // POST /api/agents with note_id (and the agent's name)
	// The rule itself.
	"taskKickoffTextWithin": "the one rule: fences a plugin's text, returns the owner's unchanged",
	// Text that reaches an agent, but fenced for EVERY task, whoever wrote it.
	"renderRoutedTask": "a routed task: the session's paste fences every task's title and body (renderRoutedBlock)",
	// Text that never becomes an agent's first message.
	"taskNameRaw": "a task's one-line name: pushes, cards, the attention queue, a host window's name, and the " +
		"chief's task events (stored as TaskEvent.Title and rendered fenced by plugins.renderTaskEvent)",
	"approvalQuestionFor":   "the owner's approval card (attention), shown to a person",
	"handleAgentTaskModal":  "the owner's task modal (HTML)",
	"handlePluginTaskList":  "GET /plugin/v1/tasks: answers the plugin, never an agent",
	"taskEventHintText":     "word matching for a task event's session hints; the text itself is never sent",
	"refEditRefusal":        "compares an edit with a reference's current text; nothing is sent",
	"commentRetractAllowed": "reads comment authors to decide whether a retraction is allowed",
	"apiTasks":              "GET /api/tasks JSON for the CLI and producers: a read, not a first message",
}

// expectedKickoffBuilders is every function that calls the rule: the kickoff
// readers above, plus the host spawn, which reads no text itself (it hands the
// task to taskKickoffTextWithin with its byte budget).
var expectedKickoffBuilders = map[string]bool{
	"toolAgentDispatch":    true,
	"toolDispatchApprove":  true,
	"handleAPIAgentCreate": true,
	"spawnPromptFor":       true, // host_dispatch.go: a host-spawned session's prompt, cut to fit
	"taskKickoffText":      true, // the unbudgeted spelling, over taskKickoffTextWithin
}

// taskTextFields are the notes.Note fields that carry text someone wrote.
var taskTextFields = map[string]bool{"Body": true, "Title": true, "Comments": true}

// taskKickoffHelpers are the one rule's two spellings.
var taskKickoffHelpers = map[string]bool{"taskKickoffText": true, "taskKickoffTextWithin": true}

func TestEveryTaskTextReaderIsClassifiedAndEveryKickoffIsFenced(t *testing.T) {
	readers, callers := scanTaskTextReaders(t, nil)
	if len(readers) == 0 || len(callers) == 0 {
		t.Fatalf("positive control FAILED: %d reader(s), %d caller(s) of the rule found; the scan sees nothing", len(readers), len(callers))
	}
	for name := range readers {
		if _, ok := expectedTaskTextReaders[name]; !ok {
			t.Errorf("%s reads a task's text (Body/Title/Comments) and is not in expectedTaskTextReaders: if it puts the text "+
				"in an agent's first message, build that through taskKickoffText (task_kickoff.go) and ledger it %q; "+
				"otherwise ledger why it never reaches an agent", name, kickoffViaHelper)
		}
	}
	for name, why := range expectedTaskTextReaders {
		if !readers[name] {
			t.Errorf("%s is ledgered (%q) but no longer reads a task's text: remove it from expectedTaskTextReaders", name, why)
		}
		if why == kickoffViaHelper && !callers[name] {
			t.Errorf("%s builds a kickoff from a task but does not call taskKickoffText / taskKickoffTextWithin", name)
		}
	}
	for name := range callers {
		if !expectedKickoffBuilders[name] {
			t.Errorf("%s calls the kickoff rule and is not in expectedKickoffBuilders", name)
		}
	}
	for name := range expectedKickoffBuilders {
		if !callers[name] {
			t.Errorf("%s is ledgered as a kickoff builder but no longer calls taskKickoffText / taskKickoffTextWithin", name)
		}
	}
}

// TestTheTaskTextLedgerSeesANewReader is the scan's own negative control: a
// synthetic function that pastes a task's body into a first message, under a
// variable name no ledgered function uses, is found — and, not calling the
// helper, would be refused.
func TestTheTaskTextLedgerSeesANewReader(t *testing.T) {
	readers, callers := scanTaskTextReaders(t, map[string]string{"zz_synthetic_kickoff.go": `package api

import "github.com/ZacxDev/muster/internal/notes"

func syntheticKickoff(job notes.Note) string { return job.Body + chiefDispatchRules }

func syntheticFencedKickoff(job notes.Note) string { return taskKickoffText(job) }
`})
	if !readers["syntheticKickoff"] || callers["syntheticKickoff"] {
		t.Errorf("syntheticKickoff: reader=%v caller=%v; want a reader that does not call the helper", readers["syntheticKickoff"], callers["syntheticKickoff"])
	}
	if !callers["syntheticFencedKickoff"] {
		t.Error("syntheticFencedKickoff calls taskKickoffText and the scan did not see it")
	}
}

// scanTaskTextReaders type-checks this package's non-test files (plus extra,
// name -> source) and returns the top-level functions that read a task-text
// field of notes.Note, and those that call a kickoff helper.
func scanTaskTextReaders(t *testing.T, extra map[string]string) (readers, callers map[string]bool) {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, f := range paths {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	for name, src := range extra {
		file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	info := &types.Info{Selections: map[*ast.SelectorExpr]*types.Selection{}, Uses: map[*ast.Ident]types.Object{}}
	conf := types.Config{Importer: taskKickoffExportImporter(t, fset)}
	if _, err := conf.Check("github.com/ZacxDev/muster/internal/api", fset, files, info); err != nil {
		t.Fatalf("type-check: %v", err)
	}
	isNote := func(typ types.Type) bool {
		if p, ok := typ.(*types.Pointer); ok {
			typ = p.Elem()
		}
		named, ok := typ.(*types.Named)
		return ok && named.Obj().Name() == "Note" && named.Obj().Pkg() != nil &&
			named.Obj().Pkg().Path() == "github.com/ZacxDev/muster/internal/notes"
	}
	readers, callers = map[string]bool{}, map[string]bool{}
	for _, file := range files {
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			name := fn.Name.Name
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.SelectorExpr:
					if sel := info.Selections[n]; sel != nil && sel.Kind() == types.FieldVal &&
						taskTextFields[sel.Obj().Name()] && isNote(sel.Recv()) {
						readers[name] = true
					}
				case *ast.CallExpr:
					if id, ok := n.Fun.(*ast.Ident); ok && taskKickoffHelpers[id.Name] {
						if obj, ok := info.Uses[id].(*types.Func); ok && obj.Pkg() != nil && obj.Pkg().Path() == "github.com/ZacxDev/muster/internal/api" {
							callers[name] = true
						}
					}
				}
				return true
			})
		}
	}
	return readers, callers
}

// taskKickoffExportImporter imports this package's dependencies from the compiler's
// export data (`go list -export`, the build cache), not from source: a source
// import type-checks every dependency again and took minutes here.
func taskKickoffExportImporter(t *testing.T, fset *token.FileSet) types.Importer {
	t.Helper()
	// 🔴 NEVER REPLAYED FROM GO'S TEST CACHE (scanGatewayTurns has the why).
	_ = os.Getenv("MUSTER_TEST_NOCACHE")
	cmd := exec.Command("go", "list", "-export", "-deps", "-json=ImportPath,Export", ".")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v: %s", err, stderr.String())
	}
	exports := map[string]string{}
	for dec := json.NewDecoder(bytes.NewReader(out)); ; {
		var lp struct{ ImportPath, Export string }
		if err := dec.Decode(&lp); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		exports[lp.ImportPath] = lp.Export
	}
	return importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		f, ok := exports[path]
		if !ok || f == "" {
			return nil, errors.New("no export data for " + path)
		}
		return os.Open(f)
	})
}
