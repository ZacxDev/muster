package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// THE SESSION_SUCCESSIONS WRITER LEDGER (migration 0099; docs/design/
// chief-in-agent-view.md 3.2): asserted over the Go source, failing when a set
// GROWS or SHRINKS.
//
//   - in the store, the SQL that writes session_successions is in exactly
//     these functions, and in no other file;
//   - in internal/api, each writer has its one named caller: the proposal
//     (succession_propose), the decline (the card's Not now), supersede (the
//     successor running), the push mark (the 30 s timer) and the seen mark
//     (the successor's view);
//   - the link is reached only inside ReserveHostDispatch's transaction.
func TestTheSuccessionWritersAreALedger(t *testing.T) {
	assertCallers(t, ".", "CreateSuccession", []string{"succession.go:toolSuccessionPropose"})
	assertCallers(t, ".", "DeclineSuccession", []string{"succession.go:successionForAnswer"})
	assertCallers(t, ".", "MarkSuperseded", []string{"succession.go:successionDispatchMoved"})
	assertCallers(t, ".", "SetSuccessionNotified", []string{"succession.go:armSuccessionPush"})
	assertCallers(t, ".", "SetSuccessorSeen", []string{"succession.go:predecessorForView"})
	assertCallers(t, ".", "successionDispatchMoved", []string{"host_dispatch.go:endHostDispatch", "host_dispatch_reconcile.go:reconcileListing"})
	assertCallers(t, filepath.Join("..", "plugins"), "linkSuccessionTx", []string{"host_dispatches.go:ReserveHostDispatch"})

	src, err := os.ReadFile(filepath.Join("..", "plugins", "session_successions.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, "session_successions.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var writers []string
	for _, d := range af.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		body := string(src[fset.Position(fd.Body.Pos()).Offset:fset.Position(fd.Body.End()).Offset])
		if strings.Contains(body, "INSERT INTO session_successions") || strings.Contains(body, "UPDATE session_successions") {
			writers = append(writers, fd.Name.Name)
		}
	}
	slices.Sort(writers)
	want := []string{"CreateSuccession", "DeclineSuccession", "MarkSuperseded", "SetSuccessionNotified", "SetSuccessorSeen", "linkSuccessionTx"}
	if !slices.Equal(writers, want) {
		t.Errorf("session_successions is written by %v\nwant exactly %v", writers, want)
	}
	files, _ := filepath.Glob(filepath.Join("..", "plugins", "*.go"))
	files2, _ := filepath.Glob("*.go")
	for _, f := range append(files, files2...) {
		if strings.HasSuffix(f, "_test.go") || filepath.Base(f) == "session_successions.go" {
			continue
		}
		b, _ := os.ReadFile(f)
		if strings.Contains(string(b), "INSERT INTO session_successions") || strings.Contains(string(b), "UPDATE session_successions") {
			t.Errorf("%s writes session_successions", f)
		}
	}
}

// TestTheViewHeaderNamesSuccessionOnlyWhenOffered: the chief's header for a
// view line says how to continue the session — the tool, the binding, and
// that nothing starts until the owner taps — only when the tool is offered.
func TestTheViewHeaderNamesSuccessionOnlyWhenOffered(t *testing.T) {
	about := &chatSubject{SessionKey: "opencode:ses_0hdrfixture", Runtime: "opencode", Host: "ws", Tmux: "main:3"}
	off := viewChiefTurnText(about, "continue it")
	if strings.Contains(off, "succession_propose") {
		t.Errorf("the header names succession_propose while it is not offered:\n%s", off)
	}
	about.CanSucceed = true
	on := viewChiefTurnText(about, "continue it")
	want := "If your owner asks for a new session that continues this one (copy its kickoff and run it), call succession_propose with " +
		"successor_of: \"opencode:ses_0hdrfixture\", succession: replace, spinoff or ? (when they did not say which), and prompt_from: " +
		"{ref, block} naming the kickoff in session_context's tail. muster copies the kickoff and shows your owner a card; nothing " +
		"starts until they tap it.\n---\ncontinue it"
	if !strings.HasSuffix(on, want) {
		t.Errorf("the header with succession offered:\n%s\nwant it to end with:\n%s", on, want)
	}
}
