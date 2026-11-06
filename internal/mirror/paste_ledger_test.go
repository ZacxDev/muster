package mirror_test

import (
	"errors"
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

// TestEveryServerPasteGoesThroughPasteWindow is a SEAM LEDGER over this
// package's source, not a behaviour test: the behaviour is the Claude prompt
// guard's server half (TestASendToAClaudeWindowWaitingOnThePersonIsRefusedBeforeTheHost), and
// this is what keeps it complete. The guard sits in Hub.PasteWindow, so:
//
//  1. a paste message (a Msg whose Type is TypePaste) is BUILT in exactly one
//     function, Hub.pasteTo;
//  2. pasteTo is CALLED from exactly one, Hub.PasteWindow — and Hub.Paste,
//     the other entry point, only delegates to PasteWindow;
//  3. inside PasteWindow the guard (claudeWaiting) runs BEFORE pasteTo;
//  4. outside internal/mirror and the host (internal/mirrorhost, which
//     RECEIVES pastes), no source file names TypePaste: no other package can
//     write one onto a host's connection.
//
// A new way to paste fails here until it is routed through PasteWindow.
func TestEveryServerPasteGoesThroughPasteWindow(t *testing.T) {
	fset := token.NewFileSet()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var builds, calls, delegates []string
	gateBeforePaste := false
	files := 0
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files++
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			where := fd.Name.Name
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
			var gate, paste token.Pos
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CompositeLit:
					for _, el := range x.Elts {
						kv, ok := el.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Type" {
							if v, ok := kv.Value.(*ast.Ident); ok && v.Name == "TypePaste" {
								builds = append(builds, where)
							}
						}
					}
				case *ast.CallExpr:
					sel, ok := x.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					switch sel.Sel.Name {
					case "pasteTo":
						calls = append(calls, where)
						paste = x.Pos()
					case "PasteWindow":
						delegates = append(delegates, where)
					case "claudeWaiting":
						gate = x.Pos()
					}
				}
				return true
			})
			if where == "Hub.PasteWindow" && gate.IsValid() && paste.IsValid() && gate < paste {
				gateBeforePaste = true
			}
		}
	}
	if files < 10 {
		t.Fatalf("instrument check: parsed %d source files; is the test running in the package directory?", files)
	}
	if want := []string{"Hub.pasteTo"}; !slices.Equal(builds, want) {
		t.Errorf("a paste message is built in %q; want exactly %q", builds, want)
	}
	if want := []string{"Hub.PasteWindow"}; !slices.Equal(calls, want) {
		t.Errorf("pasteTo is called from %q; want exactly %q — every paste goes through PasteWindow and its Claude prompt guard", calls, want)
	}
	slices.Sort(delegates)
	if want := []string{"Hub.Paste", "Service.Paste", "Service.PasteWindow"}; !slices.Equal(delegates, want) {
		t.Errorf("PasteWindow is called from %q; want exactly %q (the entry points that delegate to it)", delegates, want)
	}
	if !gateBeforePaste {
		t.Error("Hub.PasteWindow does not run claudeWaiting before pasteTo")
	}

	// 4: no other package names TypePaste.
	root := filepath.Join("..", "..")
	var named []string
	scanned := 0
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			if strings.HasPrefix(rel, filepath.Join("internal", "mirror")+string(filepath.Separator)) ||
				strings.HasPrefix(rel, filepath.Join("internal", "mirrorhost")+string(filepath.Separator)) {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			scanned++
			if strings.Contains(string(b), "TypePaste") {
				named = append(named, rel)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if scanned < 100 {
		t.Fatalf("instrument check: scanned %d source files outside internal/mirror; is the walk rooted at the module?", scanned)
	}
	if len(named) > 0 {
		t.Errorf("TypePaste is named outside internal/mirror and the host: %q", named)
	}
}

// TestAgentStateNamesTheRuntime: AgentState says which runtime it read —
// also when the status is unknown — and a window of a runtime muster no
// longer knows (an older host's "hermes") is no agent at all.
func TestAgentStateNamesTheRuntime(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "ws")
	connectJSON(t, r, tok, `{"type":"hello","version":2,"host":"ws","interactive":true,"statusReport":true,"windows":[`+
		`{"session":"work","id":"@1","index":0,"active":true,"runtime":"opencode","agentSession":"ses_0fixture"},`+
		`{"session":"work","id":"@2","index":1,"claude":true},`+
		`{"session":"work","id":"@3","index":2,"runtime":"opencode","agentSession":"ses_1fixture","status":"idle","statusSince":1792000001111},`+
		`{"session":"work","id":"@4","index":3,"runtime":"hermes","status":"idle","statusSince":1792000001111}]}`)
	r.online(t, 1, "ws", "work")
	st := r.svc.AgentState(asUser(1), "ws", "work", "@1")
	if st.Runtime != mirror.RuntimeOpenCode || !errors.Is(st.Unknown, mirror.ErrNoStatus) {
		t.Fatalf("an opencode window with no status = %+v; want Runtime opencode, ErrNoStatus", st)
	}
	if st := r.svc.AgentState(asUser(1), "ws", "work", "@2"); st.Runtime != mirror.RuntimeClaude || !errors.Is(st.Unknown, mirror.ErrNoStatus) {
		t.Fatalf("a Claude window with no status = %+v; want Runtime claude, ErrNoStatus", st)
	}
	if st := r.svc.AgentState(asUser(1), "ws", "work", "@3"); st.Runtime != mirror.RuntimeOpenCode || st.Unknown != nil || st.Status != mirror.StatusIdle {
		t.Fatalf("an idle opencode window = %+v", st)
	}
	if st := r.svc.AgentState(asUser(1), "ws", "work", "@4"); st.Runtime != "" || !errors.Is(st.Unknown, mirror.ErrNotClaude) {
		t.Fatalf("an older host's Hermes window = %+v; want no runtime, ErrNotClaude", st)
	}
}

// TestNormalizeWindowsKeepsRuntimesApart: on the wire a window is Claude Code,
// opencode, or neither — never both (opencode, the more restrictive, wins) and
// never a runtime muster does not mirror (an older host's "hermes" among
// them); an opencode window keeps its session and status. An id of 128
// characters is kept, one over the bound is not, and a plain window has none;
// a Claude window's status and id are untouched.
func TestNormalizeWindowsKeepsRuntimesApart(t *testing.T) {
	longID := "ses_" + strings.Repeat("0123456789abcdef", 7) + "0123456789ab"
	got := mirror.NormalizeWindows([]mirror.Window{
		{Session: "s", ID: "@1", Runtime: "hermes", AgentSession: "local-" + strings.Repeat("0123456789abcdef", 4), Status: mirror.StatusIdle, StatusSince: 5},
		{Session: "s", ID: "@2", Runtime: mirror.RuntimeOpenCode, Claude: true, ClaudeSession: "abc", AgentSession: longID, Pending: &mirror.Pending{Summary: "x"}, Status: mirror.StatusWaiting},
		{Session: "s", ID: "@3", Runtime: "aider", AgentSession: "abc", Status: mirror.StatusIdle},
		{Session: "s", ID: "@4", Runtime: mirror.RuntimeOpenCode, AgentSession: strings.Repeat("a", 129)},
		{Session: "s", ID: "@5", Claude: true, ClaudeSession: "abc", AgentSession: "def", Status: mirror.StatusBusy, StatusSince: 7},
		{Session: "s", ID: "@6", Runtime: mirror.RuntimeClaude, Status: mirror.StatusBusy},
		{Session: "s", ID: "@7", Runtime: mirror.RuntimeOpenCode, AgentSession: "ses_0fixture", Status: mirror.StatusBusy, StatusSince: 9},
	}, 0)
	if len(got) != 7 {
		t.Fatalf("%+v", got)
	}
	if w := got[0]; w.Runtime != "" || w.AgentSession != "" || w.Status != "" || w.StatusSince != 0 || w.AgentRuntime() != "" || w.SessionKey() != "" {
		t.Errorf("an older host's Hermes window: %+v; want a plain window", w)
	}
	if w := got[1]; w.Claude || w.ClaudeSession != "" || w.Pending != nil || w.AgentRuntime() != mirror.RuntimeOpenCode || w.Status != mirror.StatusWaiting ||
		len(longID) != 128 || w.AgentSession != longID {
		t.Errorf("a window claiming both: %+v; want opencode alone, its 128-character id kept", w)
	}
	if w := got[2]; w.Runtime != "" || w.AgentSession != "" || w.Status != "" || w.AgentRuntime() != "" {
		t.Errorf("an unknown runtime: %+v; want a plain window", w)
	}
	if w := got[3]; w.AgentSession != "" || w.AgentRuntime() != mirror.RuntimeOpenCode || w.SessionKey() != "" {
		t.Errorf("an over-long id: %+v; want the window, without the id", w)
	}
	if w := got[4]; !w.Claude || w.ClaudeSession != "abc" || w.AgentSession != "" || w.Status != mirror.StatusBusy || w.SessionKey() != "abc" {
		t.Errorf("a Claude window: %+v", w)
	}
	if w := got[5]; w.Runtime != "" || w.AgentRuntime() != "" || w.Status != "" {
		t.Errorf(`Runtime "claude" without Claude: %+v; want a plain window (Claude Code is Window.Claude)`, w)
	}
	// An opencode window is on the wire (its view, by title): its session, keyed
	// "opencode:<id>", and its status.
	if w := got[6]; w.AgentRuntime() != mirror.RuntimeOpenCode || w.AgentSession != "ses_0fixture" || w.Status != mirror.StatusBusy ||
		w.StatusSince != 9 || w.SessionKey() != "opencode:ses_0fixture" {
		t.Errorf("an opencode window: %+v (key %q)", w, w.SessionKey())
	}
	for key, ok := range map[string]bool{
		// A key stored before Hermes support was removed is still a key
		// (legacyKeyPrefixes: the CHECKs still admit it); a bare id of its
		// length is not.
		"0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b": true, "hermes:local-" + strings.Repeat("0123456789abcdef", 4): true,
		"local-" + strings.Repeat("0123456789abcdef", 4): false,
		"hermes:": false, "claude:abc": false, strings.Repeat("c", 65): false,
		"opencode:ses_0fixture": true, "opencode:": false,
	} {
		if mirror.ValidSessionKey(key) != ok {
			t.Errorf("ValidSessionKey(%q) = %t, want %t", key, !ok, ok)
		}
	}
}
