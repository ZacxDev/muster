package ui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// ONE MARKDOWN RENDERER FOR /agents/{name} (#492's D1). The chat page used to
// carry a JS copy of markdown.go (mdRender / mdInline / mdUnderEm in
// agentChatScript), and the two disagreed. These hold that the copy is gone and
// that every message's HTML on that page is the Go renderer's.
// ---------------------------------------------------------------------------

// chatAnchor is the attributes markdown.go's mdAnchor gives every link.
const chatAnchor = `" target="_blank" rel="noopener" class="text-accent underline decoration-accent/40 underline-offset-2 hover:text-fg">`

// TestChatMessageHTMLOnTheCasesTheTwoRenderersSplitOn pins, as the WHOLE
// fragment, what the agent chat now shows for each input on which #492 measured
// the JS copy and the Go renderer disagreeing (or both wrong).
//
// At origin/main 1b029ed9 the Go renderer already drew every one of these
// right (#492 fixed it), so against markdown.go they are invariant guards; the
// regression they cover is the PAGE's, which drew them with the JS copy — held
// by TestAgentChatHTMLComesOnlyFromTheServer and the WS tests in internal/api.
// The three Unicode cases are red at d1a77724^ (before #492).
func TestChatMessageHTMLOnTheCasesTheTwoRenderersSplitOn(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"a bare URL ending in _ then a later _", "https://example.com/foo_ then x_",
			`<p class="my-1"><a href="https://example.com/foo_` + chatAnchor + `https://example.com/foo_</a> then x_</p>`},
		{"* inside a bare URL", "https://h.com/a*b*c",
			`<p class="my-1"><a href="https://h.com/a*b*c` + chatAnchor + `https://h.com/a*b*c</a></p>`},
		{"a decomposed é before _", "café_x_.txt", "<p class=\"my-1\">café_x_.txt</p>"},
		{"a Devanagari vowel sign (Mc) before _", "सूची_x_.", `<p class="my-1">सूची_x_.</p>`},
		{"a superscript digit before _", "x²_y_.", `<p class="my-1">x²_y_.</p>`},
		// The controls: real emphasis on the same line still renders, so the
		// cases above are literal because of the rule, not because nothing does.
		{"emphasis beside a URL", "see https://h.com/a_b and _this_",
			`<p class="my-1">see <a href="https://h.com/a_b` + chatAnchor + `https://h.com/a_b</a> and <em>this</em></p>`},
		{"a user message renders the same", "**bold** and route_id rt_ab_cd",
			`<p class="my-1"><strong>bold</strong> and route_id rt_ab_cd</p>`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, role := range []string{"assistant", "user"} {
				if got := ChatMessageHTML(role, c.in); got != c.want {
					t.Errorf("ChatMessageHTML(%s, %q)\n got: %s\nwant: %s", role, c.in, got, c.want)
				}
			}
		})
	}
}

// TestChatMessageHTMLEscapesAuthorMarkup: message text is author-controlled
// (the owner's, a model's, a pasted page's), and the page now assigns this
// HTML to innerHTML. Every output is the literal fragment: no author tag
// survives as a tag, no quote leaves an attribute, and only http(s) becomes an
// href.
func TestChatMessageHTMLEscapesAuthorMarkup(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"a script tag", "<script>alert(1)</script>",
			`<p class="my-1">&lt;script&gt;alert(1)&lt;/script&gt;</p>`},
		{"an img onerror", `<img src=x onerror="alert(1)">`,
			`<p class="my-1">&lt;img src=x onerror=&quot;alert(1)&quot;&gt;</p>`},
		{"a quote in link text", `[x" onmouseover="alert(1)](https://a.test/)`,
			`<p class="my-1"><a href="https://a.test/` + chatAnchor + `x&quot; onmouseover=&quot;alert(1)</a></p>`},
		{"a quote in a link URL", `[ok](https://a.test/"onerror="alert(1))`,
			`<p class="my-1"><a href="https://a.test/&quot;onerror=&quot;alert(1` + chatAnchor + `ok</a>)</p>`},
		{"markup after a bare URL", `https://a.test/"><img src=x onerror=alert(1)>`,
			`<p class="my-1"><a href="https://a.test/&quot;&gt;&lt;img` + chatAnchor + `https://a.test/&quot;&gt;&lt;img</a> src=x onerror=alert(1)&gt;</p>`},
		{"a tag as link text", `[<img src=x onerror=alert(1)>](https://a.test/)`,
			`<p class="my-1"><a href="https://a.test/` + chatAnchor + `&lt;img src=x onerror=alert(1)&gt;</a></p>`},
		{"a javascript: link stays text", `[x](javascript:alert(1))`,
			`<p class="my-1">[x](javascript:alert(1))</p>`},
		{"a single quote", `it's <b>`,
			`<p class="my-1">it&#39;s &lt;b&gt;</p>`},
		{"a tag inside code", "`<b onclick=x>`",
			`<p class="my-1"><code class="rounded bg-bg/60 px-1 py-0.5 font-mono text-[0.85em]">&lt;b onclick=x&gt;</code></p>`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, role := range []string{"assistant", "user"} {
				if got := ChatMessageHTML(role, c.in); got != c.want {
					t.Errorf("ChatMessageHTML(%s, %q)\n got: %s\nwant: %s", role, c.in, got, c.want)
				}
			}
		})
	}
}

// chatScriptJS is agentChatScript's executable source (comments stripped, so
// prose about a deleted function cannot satisfy or fail a guard).
func chatScriptJS(t *testing.T) string {
	t.Helper()
	js, err := stripJSComments(jsSource(t, agentChatScript("x")))
	if err != nil {
		t.Fatal(err)
	}
	return js
}

// TestAgentChatScriptBuildsNoMarkdown: the chat script cannot render markdown,
// because it can build none of markdown's elements.
//
// A renderer has two ways to put <em> on the page: write it as HTML (an HTML
// sink — held by TestAgentChatHTMLComesOnlyFromTheServer) or build it as DOM.
// This holds the second: the set of element tags the script creates is
// EXACTLY the one below — the chat's frames, chips and plain-text notes — and
// no em/strong/code/a/li/table can be made. The set fails when it grows or
// shrinks, so a new tag is a decision made here, in words.
//
// The functions it declares must also not include the deleted renderer's
// names (a second, weaker check: it cannot see a renderer renamed).
//
// 🔴 REGRESSION: red at 1b029ed9 — the script declared mdEsc, mdUnderWord,
// mdUnderSpace, mdUnderEm, mdInline, mdTableCells, mdIsTableSep, mdRender and
// mdConvert.
func TestAgentChatScriptBuildsNoMarkdown(t *testing.T) {
	js := chatScriptJS(t)
	tags := map[string]bool{}
	for _, m := range regexp.MustCompile(`createElement\(\s*([^)]*)\)`).FindAllStringSubmatch(js, -1) {
		tags[m[1]] = true
	}
	var got []string
	for k := range tags {
		got = append(got, k)
	}
	sort.Strings(got)
	want := []string{`'details'`, `'div'`, `'form'`, `'input'`, `'p'`, `'pre'`, `'span'`, `'summary'`, `'template'`}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("agentChatScript creates elements %v, want exactly %v", got, want)
	}
	deleted := map[string]bool{"mdEsc": true, "mdUnderWord": true, "mdUnderSpace": true, "mdUnderEm": true,
		"mdInline": true, "mdTableCells": true, "mdIsTableSep": true, "mdRender": true, "mdConvert": true}
	for _, m := range regexp.MustCompile(`function\s+([A-Za-z_$][\w$]*)\s*\(|([A-Za-z_$][\w$]*)\s*=\s*function\b`).FindAllStringSubmatch(js, -1) {
		name := m[1] + m[2]
		if deleted[name] || strings.HasPrefix(name, "md") {
			t.Errorf("agentChatScript declares %s: a client-side markdown renderer is back", name)
		}
	}
}

// htmlSinks lists every place src writes HTML into the document, as
// `<sink> <what is written>`, in source order.
var htmlSinkRe = regexp.MustCompile(`\.(?:inner|outer)HTML\s*\+?=\s*[^=][^;]*|\[\s*['"](?:inner|outer)HTML['"]\s*\]|insertAdjacentHTML\s*\([^;]*|setHTMLUnsafe|createContextualFragment|DOMParser|document\.write|srcdoc`)

func htmlSinks(src string) []string {
	var out []string
	for _, m := range htmlSinkRe.FindAllString(src, -1) {
		out = append(out, strings.Join(strings.Fields(m), " "))
	}
	return out
}

// TestAgentChatHTMLComesOnlyFromTheServer is the seam: on /agents/{name}, a
// message's HTML reaches the browser only from the Go renderer.
//
// Three halves, because the path has three ends:
//
//  1. THE PAGE (behaviour): a transcript bubble is drawn by the server, its
//     markdown already rendered — not raw text marked data-md for a script.
//  2. THE BROWSER (a ledger): every HTML sink in every inline script of the
//     page is listed below with what it writes. The chat script's are the
//     server's own HTML — the socket's "html" (a delta, a sent echo) and a
//     card fragment it fetched. The list fails when it grows (a sink fed by a
//     client renderer) or shrinks (the ledger has gone stale).
//  3. THE SERVER (a ledger): the socket's "html" is made only by
//     ChatMessageHTML, and ChatMessageHTML and chatBubble are the only users
//     of chatMessageBody — enumerated over the non-test Go of internal/ui and
//     internal/api, both directions.
//
// 🔴 REGRESSION: red at 1b029ed9 on (1) — the bubble was
// `data-md="">see **bold**` — and on (2): the chat script assigned
// mdRender(…) to innerHTML five times, and (3) did not exist.
func TestAgentChatHTMLComesOnlyFromTheServer(t *testing.T) {
	v := AgentDetailView{Name: "agent-md", Status: "running", Messages: []ChatLine{
		{Role: "user", Content: "see **bold** and route_id rt_ab_cd", ID: 1},
		{Role: "assistant", Content: "https://example.com/foo_ then x_", ID: 2},
	}}
	page := documentSource(t, AgentDetailPage(v, featuresAllOn))

	// 1. The page.
	for _, want := range []string{
		`data-msg-id="1" data-md-rendered=""><p class="my-1">see <strong>bold</strong> and route_id rt_ab_cd</p></div>`,
		`data-msg-id="2" data-md-rendered=""><p class="my-1"><a href="https://example.com/foo_` + chatAnchor + `https://example.com/foo_</a> then x_</p></div>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page does not draw the bubble rendered:\nwant %s", want)
		}
	}
	if strings.Contains(page, `data-md=""`) {
		t.Errorf("a bubble is still marked data-md for client-side rendering")
	}

	// 2. The browser.
	gotSinks := htmlSinks(page)
	wantSinks := []string{
		// agentChatScript: a dispatched agent's card, fetched from the server.
		".innerHTML = html.trim()",
		// agentChatScript: a sent message's echo, and a delta's reply so far.
		".innerHTML = msg.html",
		".innerHTML = msg.html",
		// Not the chat's: htmx's own server response, the app shell's constant
		// markup, a parse of a server page (restore), its copy, a toast glyph.
		".innerHTML = e.detail.serverResponse || ''",
		".innerHTML = SHELL",
		"DOMParser",
		".innerHTML = fresh.innerHTML",
		".innerHTML = TOAST_GLYPH",
	}
	if strings.Join(gotSinks, "\n") != strings.Join(wantSinks, "\n") {
		t.Errorf("the agent page's HTML sinks changed:\n got:\n  %s\nwant:\n  %s",
			strings.Join(gotSinks, "\n  "), strings.Join(wantSinks, "\n  "))
	}
	if js := chatScriptJS(t); strings.Count(js, "innerHTML") != 3 {
		t.Errorf("agentChatScript touches innerHTML %d times; the ledger above lists 3", strings.Count(js, "innerHTML"))
	}

	// 3. The server.
	got := goCallers(t, []string{".", "../api"}, "ChatMessageHTML", "chatMessageBody")
	want := []string{ // sorted
		"ChatMessageHTML <- api.wsChatStream.sendHTMLLocked",
		"ChatMessageHTML <- api.wsSentEcho",
		"chatMessageBody <- ui.ChatMessageHTML",
		"chatMessageBody <- ui.chatBubble",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the callers of the chat's renderer changed:\n got:\n  %s\nwant:\n  %s",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// goCallers is `<callee> <- <pkg>.<enclosing func>` for every call to one of
// names in the non-test Go files of dirs, sorted.
func goCallers(t *testing.T, dirs []string, names ...string) []string {
	t.Helper()
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	var out []string
	for _, dir := range dirs {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no Go files in %s: %v", dir, err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			fs := token.NewFileSet()
			file, err := parser.ParseFile(fs, f, src, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range file.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				encl := file.Name.Name + "." + fd.Name.Name
				if fd.Recv != nil && len(fd.Recv.List) == 1 {
					rt := fd.Recv.List[0].Type
					if s, ok := rt.(*ast.StarExpr); ok {
						rt = s.X
					}
					if id, ok := rt.(*ast.Ident); ok {
						encl = file.Name.Name + "." + id.Name + "." + fd.Name.Name
					}
				}
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					var callee string
					switch fn := call.Fun.(type) {
					case *ast.Ident:
						callee = fn.Name
					case *ast.SelectorExpr:
						callee = fn.Sel.Name
					}
					if want[callee] {
						out = append(out, callee+" <- "+encl)
					}
					return true
				})
			}
		}
	}
	sort.Strings(out)
	return out
}
