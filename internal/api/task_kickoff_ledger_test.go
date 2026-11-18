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
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// THE TASK-TEXT SEAM LEDGER (task_kickoff.go).
//
// 🔴 THE HAZARD. Task text a plugin wrote must reach an agent only inside the
// untrusted fence. The rule is a handful of functions in task_kickoff.go —
// taskKickoffText / taskKickoffTextWithin for an agent's first message,
// taskToolText / taskListTitle / taskListText for a task tool's answer — and
// it only holds if EVERY path that hands a task to an agent goes through them.
// Four kickoff doors and three tool answers once handed the text over raw; a
// new one added tomorrow would too, and its own tests would stay green.
//
// So this pins RELATIONSHIPS, by type (go/types), not by spelling, over every
// top-level function AND every package-level var initialiser (a func literal
// in a var is code too):
//
//   - READERS: every unit that reads task text — Body/Title/Comments of a
//     notes.Note, Body/Title/Comments of a notes.RefSnapshot, the Body of a
//     notes.Comment or notes.RefComment — is in expectedTaskTextReaders with
//     its class: a kickoff (must call a kickoff helper), a tool answer (must
//     call a read helper), or WHY its text never reaches an agent.
//   - BUILDERS: the callers of the kickoff helpers and of the read helpers are
//     exactly expectedKickoffBuilders and expectedToolAnswerBuilders.
//   - NO LAUNDERING: every notes.Note composite literal that sets Body, Title or
//     Comments is ledgered (expectedNoteLiterals) — a fresh Note built from a
//     plugin task's fields has no CreatedBy and would pass the rule unfenced —
//     and no helper is ever called with a composite literal argument at all.
//   - NO WHOLE TASK IN AN ANSWER: every call handing a whole notes.Note (or a
//     pointer or slice of them) to toolJSON, writeJSON or json.Marshal is
//     ledgered (expectedWholeNoteAnswers): that is how task_get once answered
//     with a plugin's text unfenced, out of a field-read scan's sight.
//
// Growing OR shrinking any set fails.
//
// ⚠ WHAT THIS DOES NOT CHECK, so nobody over-reads it:
//   - that a unit scored "not to an agent" really never hands the text to one.
//     That is a judgement the ledger makes reviewable, not one it proves.
//   - a whole Note handed to a function in ANOTHER package other than the
//     three encoders above (that package reads the fields out of sight).
//   - a reader one call away: taskNameRaw is ledgered, its callers (taskLabel,
//     spawnName, …) are not.
// The per-door tests (task_kickoff_sites_test.go, task_kickoff_sites_pg_test.go,
// task_tools_fence_pg_test.go) drive every path for real.
// ---------------------------------------------------------------------------

// The reader classes beside a free-text reason.
const (
	// kickoffViaHelper: builds an agent's first message from a task, through
	// taskKickoffText / taskKickoffTextWithin.
	kickoffViaHelper = "kickoff"
	// toolAnswerViaHelper: answers an agent's task tool with a task, through
	// taskToolText / taskListTitle / taskListText.
	toolAnswerViaHelper = "tool answer"
)

// expectedTaskTextReaders is every unit in this package that reads task text,
// mapped to its class or to why the text it reads is not handed to an agent.
var expectedTaskTextReaders = map[string]string{
	// The doors that start an agent on a task — the text goes through the rule.
	"toolAgentDispatch":    kickoffViaHelper, // the chief's agent_dispatch (it also names the agent from the body)
	"toolDispatchApprove":  kickoffViaHelper, // a session's dispatch_request, approved (and the agent's name)
	"autostartStart":       "names the auto-started agent from the body (dispatchDisplayName); its first message is autostartKickoff",
	"handleAPIAgentCreate": kickoffViaHelper, // POST /api/agents with note_id (and the agent's name)
	// The rule itself.
	"taskKickoffTextWithin": "the one kickoff rule: fences a plugin's text, returns the owner's unchanged",
	"taskPluginText":        "the one read rule: takes a plugin's text out of a task and blanks it there",
	// Text that reaches an agent, but fenced for EVERY task, whoever wrote it.
	"renderRoutedTask": "a routed task: the session's paste fences every task's title and body (renderRoutedBlock)",
	// Text that never becomes an agent's message or tool answer.
	"taskNameRaw": "a task's one-line name: pushes, cards, the attention queue, a host window's name, and the " +
		"chief's task events (stored as TaskEvent.Title and rendered fenced by plugins.renderTaskEvent)",
	"approvalQuestionFor":   "the owner's approval card (attention), shown to a person",
	"handleAgentTaskModal":  "the owner's task modal (HTML)",
	"handlePluginTaskList":  "GET /plugin/v1/tasks: answers the plugin, never an agent",
	"taskEventHintText":     "word matching for a task event's session hints; the text itself is never sent",
	"refEditRefusal":        "compares an edit with a reference's current text; nothing is sent",
	"commentRetractAllowed": "reads comment authors to decide whether a retraction is allowed",
	"apiTasks":              "GET /api/tasks JSON for the CLI and producers: not an agent tool (docs/limits.md)",
	"addTaskComment":        "writes a comment (the store), and queues it on a reference; nothing is sent to an agent",
	"addTaskCommentOnce":    "writes a comment once (the store); nothing is sent to an agent",
	"snapshot":              "builds a reference's snapshot from the plugin's request, to store it",
	"refNoticeChange":       "a reference notice's change line: plugins.TaskEvent.Change, which the task event renders fenced",
	"notifyTaskCommented":   "a push to the owner's phone about a new comment, shown to a person",
	"morningTasksChanged":   "the morning report's task titles: stored for, and drawn escaped on, the owner's own page; never sent to an agent",
}

// expectedKickoffBuilders is every unit that calls a kickoff helper: the
// kickoff readers above, plus the host spawn, which reads no text itself.
var expectedKickoffBuilders = map[string]bool{
	"toolAgentDispatch":    true,
	"toolDispatchApprove":  true,
	"autostartKickoff":     true, // an auto-started agent's first message (plugin_autostart.go)
	"handleAPIAgentCreate": true,
	"spawnPromptFor":       true, // host_dispatch.go: a host-spawned session's prompt, cut to fit
	"taskKickoffText":      true, // the unbudgeted spelling, over taskKickoffTextWithin
}

// expectedToolAnswerBuilders is every unit that calls a read helper.
var expectedToolAnswerBuilders = map[string]bool{
	"toolTaskGet":           true, // task_get (mirrored sessions, the chief)
	"handleAgentTask":       true, // GET /agent/task (an agent's bound task, JSON)
	"handleAgentTaskStatus": true, // its status write's answer
	"taskAgentJSON":         true, // over taskPluginText
	"toolTaskSetStatus":     true, // task_set_status answers with the task
	"toolTasksList":         true, // tasks_list: titles
	"dispatchAgentTool":     true, // agent_get_task and agent_set_task_status (pod agents)
	"taskToolText":          true, // over taskPluginText
}

// expectedNoteLiterals is every unit that builds a notes.Note literal setting
// Body, Title or Comments, and why that is not a plugin task's text laundered
// out of its CreatedBy.
var expectedNoteLiterals = map[string]string{
	"createTaskOnce":   "files a task from its creator's own input, with the creator's CreatedBy (a plugin's is CreatedByPlugin)",
	"handleNoteCreate": "the owner's new-task form: the owner's own text",
}

// expectedWholeNoteAnswers is every unit that hands a whole task to toolJSON,
// writeJSON or json.Marshal, and why that is not a plugin's text unfenced.
var expectedWholeNoteAnswers = map[string]string{
	"taskToolText": "the one read rule's answer: the JSON with the plugin's fields blanked",
}

// taskTextFields are the fields that carry text someone (possibly a plugin)
// wrote, per notes type.
var taskTextFields = map[string]map[string]bool{
	"Note":        {"Body": true, "Title": true, "Comments": true},
	"RefSnapshot": {"Body": true, "Title": true, "Comments": true},
	"Comment":     {"Body": true},
	"RefComment":  {"Body": true},
}

var (
	kickoffHelpers = map[string]bool{"taskKickoffText": true, "taskKickoffTextWithin": true}
	readHelpers    = map[string]bool{"taskToolText": true, "taskListTitle": true, "taskListText": true, "taskPluginText": true, "taskAgentJSON": true}
	answerEncoders = map[string]bool{"toolJSON": true, "writeJSON": true, "Marshal": true}
)

func TestEveryTaskTextReaderIsClassifiedAndEveryKickoffIsFenced(t *testing.T) {
	sc := scanTaskText(t, nil)
	if len(sc.readers) == 0 || len(sc.kickoffCallers) == 0 || len(sc.readCallers) == 0 || len(sc.wholeNote) == 0 {
		t.Fatalf("positive control FAILED: %d reader(s), %d kickoff and %d read caller(s), %d whole-task answer(s); the scan sees nothing",
			len(sc.readers), len(sc.kickoffCallers), len(sc.readCallers), len(sc.wholeNote))
	}
	for name := range sc.readers {
		if _, ok := expectedTaskTextReaders[name]; !ok {
			t.Errorf("%s reads task text and is not in expectedTaskTextReaders: if it hands the text to an agent, build that "+
				"through task_kickoff.go and ledger it %q or %q; otherwise ledger why it never reaches an agent",
				name, kickoffViaHelper, toolAnswerViaHelper)
		}
	}
	for name, why := range expectedTaskTextReaders {
		if !sc.readers[name] {
			t.Errorf("%s is ledgered (%q) but no longer reads task text: remove it from expectedTaskTextReaders", name, why)
		}
		if why == kickoffViaHelper && !sc.kickoffCallers[name] {
			t.Errorf("%s builds a kickoff from a task but does not call taskKickoffText / taskKickoffTextWithin", name)
		}
		if why == toolAnswerViaHelper && !sc.readCallers[name] {
			t.Errorf("%s answers a task tool but does not call taskToolText / taskListTitle / taskListText", name)
		}
	}
	taskTextSameSet(t, "calls a kickoff helper", sc.kickoffCallers, expectedKickoffBuilders)
	taskTextSameSet(t, "calls a read helper", sc.readCallers, expectedToolAnswerBuilders)
	taskTextSameSet(t, "builds a notes.Note literal with task text (laundering: no CreatedBy)", sc.literals, taskTextKeys(expectedNoteLiterals))
	taskTextSameSet(t, "hands a whole task to toolJSON / writeJSON / json.Marshal", sc.wholeNote, taskTextKeys(expectedWholeNoteAnswers))
	for _, at := range sc.literalArgs {
		t.Errorf("%s calls a task_kickoff.go helper with a composite literal: a fresh Note has no CreatedBy, so a plugin's "+
			"text would pass the rule unfenced; pass the task as read", at)
	}
}

// TestTheTaskTextLedgerSeesEverySneak is the scan's own negative control, one
// synthetic unit per hazard, under names and variable names no ledgered unit
// uses: a raw paste, a package-level var's func literal, a laundering literal
// (in a unit that ALSO calls the helper, the shape a field-read scan misses), a
// snapshot body read, a comment body read and a whole task in an answer.
func TestTheTaskTextLedgerSeesEverySneak(t *testing.T) {
	sc := scanTaskText(t, map[string]string{"zz_synthetic_sneaks.go": `package api

import (
	"encoding/json"

	"github.com/ZacxDev/muster/internal/notes"
)

func syntheticKickoff(job notes.Note) string { return job.Body + chiefDispatchRules }

var syntheticVarKickoff = func(job notes.Note) string { return job.Title }

func syntheticLaundered(job notes.Note) string {
	return taskKickoffText(notes.Note{Body: job.Body}) + taskKickoffText(job)
}

func syntheticSnapshot(r notes.RefSnapshot) string { return r.Body }

func syntheticComment(c notes.Comment) string { return c.Body }

func syntheticAnswer(job *notes.Note) []byte { b, _ := json.Marshal(job); return b }

func syntheticFencedKickoff(job notes.Note) string { return taskKickoffText(job) }
`})
	for name, want := range map[string]bool{
		"syntheticKickoff": true, "syntheticVarKickoff": true, "syntheticLaundered": true,
		"syntheticSnapshot": true, "syntheticComment": true,
	} {
		if sc.readers[name] != want {
			t.Errorf("%s: reader=%v, want %v", name, sc.readers[name], want)
		}
	}
	if sc.kickoffCallers["syntheticKickoff"] || !sc.kickoffCallers["syntheticFencedKickoff"] {
		t.Errorf("kickoff callers %v: want syntheticFencedKickoff and not syntheticKickoff", sc.kickoffCallers)
	}
	if !sc.literals["syntheticLaundered"] {
		t.Error("the laundering literal in syntheticLaundered was not seen")
	}
	if !sc.wholeNote["syntheticAnswer"] {
		t.Error("json.Marshal of a whole *notes.Note in syntheticAnswer was not seen")
	}
	found := false
	for _, at := range sc.literalArgs {
		if strings.HasPrefix(at, "syntheticLaundered ") {
			found = true
		}
	}
	if !found {
		t.Errorf("the helper call with a composite literal in syntheticLaundered was not seen: %v", sc.literalArgs)
	}
}

type taskTextScan struct {
	readers, kickoffCallers, readCallers, literals, wholeNote map[string]bool
	literalArgs                                               []string
}

// scanTaskText type-checks this package's non-test files (plus extra, name ->
// source) and scans every top-level function and package-level var
// initialiser (see the ledger's doc).
func scanTaskText(t *testing.T, extra map[string]string) taskTextScan {
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
	info := &types.Info{Selections: map[*ast.SelectorExpr]*types.Selection{}, Uses: map[*ast.Ident]types.Object{},
		Types: map[ast.Expr]types.TypeAndValue{}}
	conf := types.Config{Importer: taskKickoffExportImporter(t, fset)}
	if _, err := conf.Check("github.com/ZacxDev/muster/internal/api", fset, files, info); err != nil {
		t.Fatalf("type-check: %v", err)
	}
	const notesPkg = "github.com/ZacxDev/muster/internal/notes"
	const apiPkg = "github.com/ZacxDev/muster/internal/api"
	// notesNamed is the notes type typ names (through one pointer), or "".
	notesNamed := func(typ types.Type) string {
		if p, ok := typ.(*types.Pointer); ok {
			typ = p.Elem()
		}
		named, ok := typ.(*types.Named)
		if !ok || named.Obj().Pkg() == nil || named.Obj().Pkg().Path() != notesPkg {
			return ""
		}
		return named.Obj().Name()
	}
	// wholeNote: a Note, *Note or []Note / []*Note.
	wholeNote := func(typ types.Type) bool {
		if s, ok := typ.(*types.Slice); ok {
			typ = s.Elem()
		}
		return notesNamed(typ) == "Note"
	}
	sc := taskTextScan{readers: map[string]bool{}, kickoffCallers: map[string]bool{}, readCallers: map[string]bool{},
		literals: map[string]bool{}, wholeNote: map[string]bool{}}
	scan := func(name string, root ast.Node) {
		ast.Inspect(root, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if sel := info.Selections[n]; sel != nil && sel.Kind() == types.FieldVal &&
					taskTextFields[notesNamed(sel.Recv())][sel.Obj().Name()] {
					sc.readers[name] = true
				}
			case *ast.CompositeLit:
				if tv, ok := info.Types[n]; ok && notesNamed(tv.Type) == "Note" {
					for _, e := range n.Elts {
						if kv, ok := e.(*ast.KeyValueExpr); ok {
							if k, ok := kv.Key.(*ast.Ident); ok && taskTextFields["Note"][k.Name] {
								sc.literals[name] = true
							}
						}
					}
				}
			case *ast.CallExpr:
				var callee *ast.Ident
				switch f := n.Fun.(type) {
				case *ast.Ident:
					callee = f
				case *ast.SelectorExpr:
					callee = f.Sel
				}
				if callee == nil {
					return true
				}
				obj, _ := info.Uses[callee].(*types.Func)
				if obj == nil || obj.Pkg() == nil {
					return true
				}
				own := obj.Pkg().Path() == apiPkg
				if own && (kickoffHelpers[obj.Name()] || readHelpers[obj.Name()]) {
					if kickoffHelpers[obj.Name()] {
						sc.kickoffCallers[name] = true
					} else {
						sc.readCallers[name] = true
					}
					for _, a := range n.Args {
						if lit, ok := ast.Unparen(a).(*ast.CompositeLit); ok && notesNamed(info.Types[lit].Type) == "Note" {
							sc.literalArgs = append(sc.literalArgs, name+" "+fset.Position(a.Pos()).String())
						}
					}
				}
				if answerEncoders[obj.Name()] && (own || obj.Pkg().Path() == "encoding/json") {
					for _, a := range n.Args {
						if tv, ok := info.Types[a]; ok && wholeNote(tv.Type) {
							sc.wholeNote[name] = true
						}
					}
				}
			}
			return true
		})
	}
	for _, file := range files {
		for _, d := range file.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Body != nil {
					scan(d.Name.Name, d.Body)
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, v := range vs.Values {
						name := vs.Names[0].Name
						if i < len(vs.Names) {
							name = vs.Names[i].Name
						}
						scan(name, v)
					}
				}
			}
		}
	}
	return sc
}

func taskTextSameSet(t *testing.T, what string, got, want map[string]bool) {
	t.Helper()
	for _, n := range taskTextSorted(got) {
		if !want[n] {
			t.Errorf("%s %s and is not ledgered", n, what)
		}
	}
	for _, n := range taskTextSorted(want) {
		if !got[n] {
			t.Errorf("%s is ledgered as one that %s, and no longer does", n, what)
		}
	}
}

func taskTextKeys(m map[string]string) map[string]bool {
	out := map[string]bool{}
	for k := range m {
		out[k] = true
	}
	return out
}

func taskTextSorted(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// taskKickoffExportImporter imports this package's dependencies from the
// compiler's export data (`go list -export`, the build cache), not from
// source: a source import type-checks every dependency again and took minutes.
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
