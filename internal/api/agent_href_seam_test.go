package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// THE SEAM BETWEEN "A LINK TO AN AGENT'S PAGE" AND ui.AgentSessionHref.
//
// The bare /agents/{name} answers 303 → /agents/{name}?session=<id>, and that
// one hop was most of the cost of opening an agent on a phone (828/1203 ms
// first bubble through the bare link, 476/488 ms through the session URL, an
// emulated 350 ms RTT). Every in-app link therefore goes through ONE helper,
// ui.AgentSessionHref, and this test is what keeps it one: it walks every
// non-test Go file under internal/ and cmd/ and ledgers
//
//  1. every string concatenation that STARTS with the literal "/agents/" (and
//     every "/agents/%…" format string), by file, function and the path that
//     follows the name. A concatenation with nothing after the name, or only a
//     query, is a PAGE link, and the only function allowed to build one is
//     ui.AgentPath, which ui.AgentSessionHref calls;
//  2. every "'/agents/' +" built in inline JavaScript (Go string literals), by
//     what follows it, for the same reason;
//  3. every call of ui.AgentSessionHref, ui.AgentPath and
//     ui.AgentCardView.PageHref, by file and function.
//
// 🔴 EACH LEDGER FAILS WHEN ITS SET GROWS *OR* SHRINKS. A new link that builds
// "/agents/"+name itself is a new entry in (1) — and a PAGE-shaped one fails
// outright, naming the helper. A new link through the helper is a new entry in
// (3) and must be added here, which is the review point: does it name the
// session it knows? A removed site fails too, so the ledger never claims a
// caller that is gone.
//
// ⚠ IT SEES GO SOURCE ONLY. web/static/*.js holds no link to an agent page
// today (sw.js opens whatever URL a push carries, and the pushes are ledgered
// here at the Go side).

// agentHrefScan is what the walk found: ledger key → occurrences.
type agentHrefScan struct {
	concat, js, calls map[string]int
}

func scanAgentHrefs(t *testing.T) agentHrefScan {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	out := agentHrefScan{concat: map[string]int{}, js: map[string]int{}, calls: map[string]int{}}
	jsRe := regexp.MustCompile(`['"]/agents/['"]\s*\+([^;\n]*)`)
	jsSuffix := regexp.MustCompile(`\+\s*'(/[^']*)'`)
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, p, nil, 0)
			if err != nil {
				return err
			}
			pkgUI := f.Name.Name == "ui"
			for _, decl := range f.Decls {
				fn := "(package)"
				if fd, ok := decl.(*ast.FuncDecl); ok {
					fn = fd.Name.Name
					if fd.Recv != nil && len(fd.Recv.List) == 1 {
						fn = recvName(fd.Recv.List[0].Type) + "." + fn
					}
				}
				inner := map[ast.Node]bool{}
				ast.Inspect(decl, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.BinaryExpr:
						if x.Op != token.ADD || inner[x] {
							return true
						}
						ops := flattenAdd(x, inner)
						if s, ok := stringLit(ops[0]); ok && strings.HasPrefix(s, "/agents/") {
							out.concat[rel+":"+fn+":"+concatShape(s, ops[1:])]++
						}
					case *ast.BasicLit:
						s, ok := stringLit(x)
						if !ok {
							return true
						}
						if strings.Contains(s, "/agents/%") {
							out.concat[rel+":"+fn+":format"]++
						}
						for _, m := range jsRe.FindAllStringSubmatch(s, -1) {
							suffix := "(page)"
							if sm := jsSuffix.FindStringSubmatch(m[1]); sm != nil {
								suffix = sm[1]
							}
							out.js[rel+":"+fn+":"+suffix]++
						}
					case *ast.CallExpr:
						var name string
						switch fx := x.Fun.(type) {
						case *ast.SelectorExpr:
							if id, ok := fx.X.(*ast.Ident); ok && id.Name == "ui" {
								name = fx.Sel.Name
							} else if fx.Sel.Name == "PageHref" {
								name = "PageHref"
							}
						case *ast.Ident:
							if pkgUI {
								name = fx.Name
							}
						}
						switch name {
						case "AgentSessionHref", "AgentPath", "PageHref":
							out.calls[name+" ← "+rel+":"+fn]++
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
	}
	return out
}

func recvName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.StarExpr:
		return recvName(x.X)
	case *ast.Ident:
		return x.Name
	case *ast.IndexExpr:
		return recvName(x.X)
	}
	return "?"
}

// flattenAdd is the operands of a chain of `+`, left to right; every inner
// BinaryExpr is marked so it is not counted as a site of its own.
func flattenAdd(e ast.Expr, inner map[ast.Node]bool) []ast.Expr {
	if b, ok := e.(*ast.BinaryExpr); ok && b.Op == token.ADD {
		inner[b] = true
		return append(flattenAdd(b.X, inner), flattenAdd(b.Y, inner)...)
	}
	if p, ok := e.(*ast.ParenExpr); ok {
		return flattenAdd(p.X, inner)
	}
	return []ast.Expr{e}
}

func stringLit(e ast.Expr) (string, bool) {
	l, ok := e.(*ast.BasicLit)
	if !ok || l.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(l.Value)
	return s, err == nil
}

// concatShape is what a "/agents/"+… concatenation builds after the name: its
// first later literal that starts a PATH ("/terminal", "/send", …), or
// "(page)" when none does — the name alone, or the name plus a query, i.e. a
// link to the page itself.
func concatShape(first string, rest []ast.Expr) string {
	if tail := strings.TrimPrefix(first, "/agents/"); tail != "" {
		return "/agents/" + tail // a fixed path ("/agents/pins", "/agents/sessions/")
	}
	for _, e := range rest {
		if s, ok := stringLit(e); ok && strings.HasPrefix(s, "/") {
			return s
		}
	}
	return "(page)"
}

// TestEveryAgentPageLinkGoesThroughAgentSessionHref pins the three ledgers
// described above. Red before the helper: the card, pinned chip, Recent strip,
// task card, push target, ask target, drawer, terminal back link and
// redirects each built "/agents/"+name themselves — (page)-shaped entries
// outside ui.AgentPath.
func TestEveryAgentPageLinkGoesThroughAgentSessionHref(t *testing.T) {
	got := scanAgentHrefs(t)

	// (1) Every "/agents/"… concatenation. Only ui.AgentPath may build the page
	// (and the card menu's DELETE, which names the agent by id for a verb, not a
	// link).
	for k, n := range got.concat {
		if strings.HasSuffix(k, ":(page)") && !agentPageBuilders[k] {
			t.Errorf("%s builds a link to an agent's PAGE itself (%d×): use ui.AgentSessionHref, which names the session and skips the bare URL's 303", k, n)
		}
	}
	assertHrefLedger(t, "concatenations starting \"/agents/\"", got.concat, agentConcatLedger)
	assertHrefLedger(t, "inline-JS \"'/agents/' +\"", got.js, agentJSLedger)
	assertHrefLedger(t, "calls of the agent-page helpers", got.calls, agentHrefCallLedger)
}

func assertHrefLedger(t *testing.T, what string, got, want map[string]int) {
	t.Helper()
	keys := map[string]bool{}
	for k := range got {
		keys[k] = true
	}
	for k := range want {
		keys[k] = true
	}
	var sorted []string
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		if got[k] != want[k] {
			t.Errorf("%s: %s occurs %d×, the ledger says %d×", what, k, got[k], want[k])
		}
	}
	if t.Failed() {
		var lines []string
		for _, k := range sorted {
			if got[k] > 0 {
				lines = append(lines, strconv.Quote(k)+": "+strconv.Itoa(got[k])+",")
			}
		}
		t.Logf("%s as scanned now:\n%s", what, strings.Join(lines, "\n"))
	}
}

// agentPageBuilders are the (page)-shaped concatenations that are allowed.
var agentPageBuilders = map[string]bool{
	// THE one page builder; ui.AgentSessionHref adds the session to it.
	"internal/ui/terminal.go:AgentPath:(page)": true,
	// hx-delete "/agents/"+id: the destroy VERB on the agent's id, not a link.
	"internal/ui/agents.go:agentDeleteConfirm:(page)": true,
}

// agentConcatLedger: every "/agents/"… concatenation, by what follows the name.
var agentConcatLedger = map[string]int{
	"internal/api/chief_replace.go:Server.replaceChief:format":       1, // an error message naming POST /agents/%d/model
	"internal/api/session_runtime.go:Server.sessionViews:/sessions/": 1,
	"internal/ui/agents.go:agentDeleteConfirm:(page)":                1,
	"internal/ui/agents.go:agentMenu:/start":                         1,
	"internal/ui/agents.go:agentMenu:/stop":                          1,
	"internal/ui/agents.go:agentRenameForm:/name":                    1,
	"internal/ui/agents_detail.go:agentModelControl:/model":          1,
	"internal/ui/agents_detail.go:agentSessionsPath:/sessions":       1,
	"internal/ui/terminal.go:AgentPath:(page)":                       1,
	"internal/ui/terminal.go:AgentSendPath:/send":                    1,
	"internal/ui/terminal.go:AgentTerminalPath:/terminal":            1,
}

// agentJSLedger: every "'/agents/' +" in inline JavaScript — sockets, not links.
var agentJSLedger = map[string]int{
	"internal/ui/agents_detail.go:agentChatScript:/ws":          1,
	"internal/ui/terminal.go:agentTerminalScript:/terminal/ws?": 1,
}

// agentHrefCallLedger: every caller of the helpers — THE set of in-app links to
// an agent's page (and the redirects that must agree with them).
var agentHrefCallLedger = map[string]int{
	"AgentPath ← internal/ui/agent_href.go:AgentSessionHref": 2,
	// The server's own targets: a tap on an ask's push, the agent-ready push,
	// the terminal's Rendered switch (agentPageHref reads the latest session);
	// the 303 itself; HX-Redirects after opening or defaulting a session; the
	// no-JS send's way back; an answered ask's return.
	"AgentSessionHref ← internal/api/agent_href.go:Server.agentPageHref":                  1,
	"AgentSessionHref ← internal/api/agent_send.go:agentSendBack":                         1,
	"AgentSessionHref ← internal/api/agents.go:Server.answerSessionOpened":                1,
	"AgentSessionHref ← internal/api/agents.go:Server.handleAgentDetail":                  1,
	"AgentSessionHref ← internal/api/attention_asker.go:askerRef.page":                    1,
	"AgentSessionHref ← internal/api/session_runtime.go:Server.handleAgentSessionDefault": 1,
	// The views: a card (and through PageHref its pinned chip and Recent
	// entry), the session drawer, a task card's Open chat, the notification
	// panel's two row kinds, the terminal page's back link.
	"AgentSessionHref ← internal/ui/agents.go:AgentCardView.PageHref": 1,
	"AgentSessionHref ← internal/ui/agents_detail.go:sessionDrawer":   1,
	"AgentSessionHref ← internal/ui/notes.go:taskActionTrailing":      1,
	"AgentSessionHref ← internal/ui/notifications.go:activeRow":       1,
	"AgentSessionHref ← internal/ui/notifications.go:notifRow":        1,
	"AgentSessionHref ← internal/ui/terminal.go:AgentTerminalPage":    1,
	"PageHref ← internal/ui/agents.go:agentCard":                      2,
	"PageHref ← internal/ui/pinned_chips.go:pinnedChip":               1,
	"PageHref ← internal/ui/recents.go:recentItems":                   1,
}
