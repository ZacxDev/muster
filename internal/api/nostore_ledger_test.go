package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE no-store LEDGER. Back and Forward may restore exactly four documents
// from a cache — an agent's chat page, the mirrored Rendered view, a mirrored
// window's terminal and an agent's terminal (viewDocument) — and NOTHING that
// shows or takes a secret: the sign-in pages, step-up confirms, the account,
// token, credential, passkey, plugin, flow and admin pages (the pages that
// load no telemetry, ui.NavDocumentView.Secret), connectors, devices, and every
// response that carries a minted value. Those keep `Cache-Control: no-store`.
//
// Two halves, because either alone has a hole:
//
//   - STRUCTURAL: every function in this package that marks a response
//     no-store — a call to noStore, formDocument or writeDocumentHeaders, or a
//     "no-store" literal — is named below with what it serves, and viewDocument
//     has exactly its four callers. A writer that stops saying no-store leaves
//     the set and the test fails; so does a new one nobody looked at, and a
//     fifth page made cacheable.
//   - BEHAVIOURAL: the secret pages are requested through the whole handler
//     chain and must answer exactly no-store, and the four views exactly
//     viewDocumentCacheControl — so a helper that keeps its name and loses its
//     header (formDocument without noStore, a later Set overriding it) is red
//     too.

// noStoreWriters is every function that marks a response no-store through a
// helper (noStore, formDocument, writeDocumentHeaders), with what it serves.
var noStoreWriters = map[string]string{
	"writeAuthDocument":   "the bare pages: sign-in, claim, invite, reset, notices",
	"writeNavDocument":    "every signed-in server-written page: account, tokens, credentials, passkeys, plugins, flows, admin, Asks",
	"formDocument":        "the helper the no-script confirm pages share",
	"mirrorStepUpPage":    "the Rendered view's no-script step-up confirm, carrying the message",
	"sendQueueStepUpPage": "the send queue's no-script step-up confirm, carrying the message",
	"sendAgainPage":       "the no-script Send again for a message that may already have been delivered, carrying the message",
	"sendQueueRetry":      "the send queue's Retry confirm page",
	"sendQueueEdit":       "the send queue's edit form, the queued message in it",
	"handleSendQueueList": "the send queue: the queued messages",
}

// noStoreLiterals is every function holding a "no-store" literal of its own.
var noStoreLiterals = map[string]string{
	"noStore":                  "the helper itself",
	"writeDocumentHeaders":     "the header set of both document writers",
	"handleAccountDevicesLive": "the paired devices, live",
	"handleDeviceConfig":       "a paired phone's config",
	"handleDeviceChallenge":    "a signed action's one-time nonce",
	"handleDoctor":             "`muster doctor`'s report: live state (which hosts are connected now)",
	"handleMirrorHost":         "a machine's own connection state, live",
	"handleDevicePair":         "a pairing, with its one-time secret",
	"handleDevicePairCode":     "a pairing code",
	"handlePushKey":            "the VAPID public key",
	"renderConnectorsContent":  "the connectors list",
	"staticCacheControl":       "another build's /static/ bytes, or a page script's bare path (a CDN rewrites no-cache)",
	"staticHandler":            "a missing /static/ file (an edge must not keep a script's 404)",
	"handleAgentChatLog":       "the chat-log partial",
	"handleAsksDrafts":         "an ask's draft answers",
	"handleAttentionAsker":     "a page's open asks",
	"handleAttentionBadge":     "the Asks badge",
	"handleAttentionItems":     "the Asks list",
	"handleAttentionOpen":      "an ask opened from a push",
	"renderAskPage":            "an ask page's live region (its options, suggestions and state)",
	"writeAskLive":             "an ask's live answer",
	"handleFocusHistory":       "an agent's focus history",
	"handleMirrorAsks":         "a window's open questions",
	"handleMirrorComplete":     "the composer's completions",
	"handleMirrorSubagent":     "a subagent's steps",
	"handleMirrorSuggest":      "a window's suggested replies",
	"handleReviewCount":        "the review count",
	"handleShare":              "the share target",
	"handleTranscribe":         "a dictation's transcript",
	"handleUpload":             "an upload's answer",
	"handleUploadLink":         "an upload's signed link",
	"handleNavAgents":          "the sidebar's agent list",
	"redirectTo":               "home's redirect, from `/` and GET /agents (it follows who the chief is now)",
	"handleAgentsHome":         "GET /agents: home's redirect, or the onboarding page when there is nowhere to go",
	"handleAgentHeader":        "an agent page's status strip, ⋯ menu and notices, live",
}

// viewDocumentCallers are the four handlers whose documents Back may restore.
var viewDocumentCallers = []string{
	"handleAgentDetail",        // /agents/{name}
	"handleAgentTerminalPage",  // /agents/{name}/terminal
	"handleMirrorTerminalPage", // /mirror/terminal
	"handleMirrorViewPage",     // /mirror/view
}

// TestTheNoStoreLedger is the structural half (see the top of this file).
func TestTheNoStoreLedger(t *testing.T) {
	writers, literals, views := noStoreScan(t, ".")
	// POSITIVE CONTROL: a scan wired to nothing would find nothing.
	if len(writers) < 5 || len(literals) < 5 || len(views) == 0 {
		t.Fatalf("the source scan found %d writers, %d literals, %d view callers — it is not observing this package", len(writers), len(literals), len(views))
	}
	if got, want := strings.Join(writers, ","), strings.Join(sortedKeys(noStoreWriters), ","); got != want {
		t.Errorf("the functions marking a response no-store through a helper are\n  %s\nthe ledger names\n  %s\n"+
			"🔴 A page that shows or takes a secret must stay no-store. A writer that left this set stopped "+
			"saying it; a new one goes in noStoreWriters with what it serves.", got, want)
	}
	if got, want := strings.Join(literals, ","), strings.Join(sortedKeys(noStoreLiterals), ","); got != want {
		t.Errorf("the functions with a no-store literal are\n  %s\nthe ledger names\n  %s\n"+
			"A response that stopped saying no-store, or a new one: name it in noStoreLiterals.", got, want)
	}
	if strings.Join(views, ",") != strings.Join(viewDocumentCallers, ",") {
		t.Errorf("viewDocument (cacheable: Back may restore it) is called from %v; the ledger names %v.\n"+
			"Only the agent page, the Rendered view and the terminals are restorable. A page with a "+
			"secret on it never is.", views, viewDocumentCallers)
	}
}

// TestTheSecretPagesAreNoStoreAndOnlyTheViewsAreRestorable is the
// behavioural half: each page, through the handler chain, says what the ledger
// says it does.
func TestTheSecretPagesAreNoStoreAndOnlyTheViewsAreRestorable(t *testing.T) {
	f := newGoTrueFixtureWith(t, &Extensions{
		Attention:   emptyAttention{},
		Plugins:     &countingPlugins{},
		Credentials: &memCreds{},
	})
	f.fake.EnablePasskeys()
	f.s.gt.Accounts = &memAccounts{rows: f.store}
	sub := f.member("admin@example.com", authz.OperatorUserID, authz.RoleAdmin)
	at, rt, _ := f.fake.Mint(sub)
	signedIn := []string{"/attention", "/account", "/account/tokens", ui.CredentialsPath, "/account/passkeys",
		"/plugins", "/flows", "/admin/users", "/admin/invites"}
	for _, path := range signedIn {
		w := do(f.edge, "GET", path, nil, withCookies(at, rt), asNavigation)
		if w.Code != http.StatusOK {
			t.Errorf("fixture: GET %s = %d, not the page", path, w.Code)
			continue
		}
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("GET %s: Cache-Control = %q, want no-store — it shows or takes a secret", path, got)
		}
	}
	for _, path := range []string{"/login", ClaimPath, InvitePath, ResetPath} {
		w := do(f.edge, "GET", path, nil, asNavigation)
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("GET %s (signed out) = %d: Cache-Control = %q, want no-store", path, w.Code, got)
		}
	}

	// The four views: restorable, never shared.
	ms, mts := viewServer(t, goldHost())
	ss, sts := sendServer(t, newSendAgents(), &turnGateway{})
	views := []struct {
		s    *Server
		ts   *httptest.Server
		path string
	}{
		{ms, mts, ref("scratch2", "@7").ViewPath()},
		{ms, mts, ref("scratch2", "@4").TerminalPath()},
		{ss, sts, "/agents/keen-wren?session=41"},
		{ss, sts, ui.AgentTerminalPath("keen-wren")},
	}
	for _, v := range views {
		resp, _ := mirrorGet(t, v.s, v.ts, v.path)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("fixture: GET %s = %d", v.path, resp.StatusCode)
			continue
		}
		if got := resp.Header.Get("Cache-Control"); got != viewDocumentCacheControl {
			t.Errorf("GET %s: Cache-Control = %q, want %q", v.path, got, viewDocumentCacheControl)
		}
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// noStoreScan reads dir's non-test Go files and returns, sorted and unique: the
// functions calling noStore, formDocument or writeDocumentHeaders (and those
// helpers themselves); the functions holding a "no-store" string literal; and
// the functions calling viewDocument.
func noStoreScan(t *testing.T, dir string) (writers, literals, views []string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	w, l, v := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					if id, ok := x.Fun.(*ast.Ident); ok {
						switch id.Name {
						case "noStore", "formDocument", "writeDocumentHeaders":
							w[fd.Name.Name] = true
						case "viewDocument":
							v[fd.Name.Name] = true
						}
					}
				case *ast.BasicLit:
					if x.Kind == token.STRING && strings.Contains(x.Value, "no-store") {
						l[fd.Name.Name] = true
					}
				}
				return true
			})
		}
	}
	keys := func(m map[string]bool) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	return keys(w), keys(l), keys(v)
}

// THE OTHER HALF OF THE LEDGER: every handler that writes text/html, and what
// it tells caches. The no-store ledger above only sees a function that MENTIONS
// no-store; a page that sets no Cache-Control at all would be invisible to it.
// So every function that sets Content-Type to text/html is enumerated here and
// must be one of: no-store (it, or a helper it calls, says so), one of the four
// views (viewDocument), or named below with why it may be kept by a cache.

// htmlFragment is the reason an htmx partial is allowed no Cache-Control: it
// is fetched by script, never navigated to, so neither Back nor the HTTP
// cache's history path ever shows it as a page; it carries no validator, so
// the HTTP cache never serves it fresh; and no secret is in it.
const htmlFragment = "an htmx fragment: fetched by script, never a document; no validator, so never served fresh; no secret"

var htmlWithoutNoStore = map[string]string{
	"handleIndex": "the shell (/tasks, /connectors, …): restorable before this change, no secret on it; its appScript " +
		"carries restoreJS, so a restore or a history load from the HTTP cache gets the build and refusal checks",
	"handleTaskDetail":   "a task's page: no secret on it; its appScript carries restoreJS (as the shell's)",
	"renderTaskNotFound": "a task's not-found page: nothing on it but the id the reader typed",

	"answerSwitcherPin":         htmlFragment,
	"handleAgentNewModal":       htmlFragment,
	"handleAgentRepoOptions":    htmlFragment,
	"handleAgentNoteOptions":    htmlFragment,
	"handleAgentStatusIcon":     htmlFragment,
	"handleNotifications":       htmlFragment,
	"handleAgentTaskModal":      htmlFragment,
	"writeChiefPanelBody":       htmlFragment,
	"writeChiefPanelBodyFor":    htmlFragment,
	"handleConnectorRequests":   htmlFragment,
	"handleConnectorNewForm":    htmlFragment + " (the form a secret is typed INTO; it holds none)",
	"handleConnectorRotateForm": htmlFragment + " (the form a secret is typed INTO; it holds none)",
	"handleChiefThreadSearch":   htmlFragment,
	"handleNotesContent":        htmlFragment,
	"handleNoteNewModal":        htmlFragment,
	"handleNoteDelete":          htmlFragment,
	"handleNoteEditModal":       htmlFragment,
	"renderNoteCard":            htmlFragment,
	"handleUITagVocabulary":     htmlFragment,
}

// TestEveryHTMLWriterIsClassified enumerates the functions that set
// Content-Type: text/html and fails when one is neither no-store, a view, nor
// named in htmlWithoutNoStore — and when that map names a function that no
// longer needs it (gone, or now no-store or a view).
func TestEveryHTMLWriterIsClassified(t *testing.T) {
	html := htmlWriters(t, ".")
	writers, literals, views := noStoreScan(t, ".")
	if len(html) < 20 {
		t.Fatalf("the scan found %d text/html writers — it is not observing this package", len(html))
	}
	in := func(xs []string, x string) bool {
		for _, y := range xs {
			if y == x {
				return true
			}
		}
		return false
	}
	var unclassified, stale []string
	seen := map[string]bool{}
	for _, fn := range html {
		seen[fn] = true
		classified := in(writers, fn) || in(literals, fn) || in(views, fn)
		_, allowed := htmlWithoutNoStore[fn]
		switch {
		case !classified && !allowed:
			unclassified = append(unclassified, fn)
		case classified && allowed:
			stale = append(stale, fn+" (now no-store or a view)")
		}
	}
	for fn := range htmlWithoutNoStore {
		if !seen[fn] {
			stale = append(stale, fn+" (writes no text/html any more)")
		}
	}
	sort.Strings(unclassified)
	sort.Strings(stale)
	if len(unclassified) > 0 {
		t.Errorf("text/html writers that say nothing to caches and are not named in htmlWithoutNoStore: %v.\n"+
			"🔴 A page that shows or takes a secret must be no-store; one that may be kept goes in the map with why.", unclassified)
	}
	if len(stale) > 0 {
		t.Errorf("htmlWithoutNoStore names functions it should not: %v", stale)
	}
	// The four views are text/html writers too: the enumeration sees them.
	for _, v := range viewDocumentCallers {
		if !in(html, v) {
			t.Errorf("%s is a view but the text/html scan does not see it — the scan is narrower than the views", v)
		}
	}
}

// htmlWriters is every function in dir's non-test files with a call
// `….Set("Content-Type", "<…text/html…>")`, sorted.
func htmlWriters(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	set := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				c, ok := n.(*ast.CallExpr)
				if !ok || len(c.Args) != 2 {
					return true
				}
				sel, ok := c.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Set" {
					return true
				}
				k, ok1 := c.Args[0].(*ast.BasicLit)
				v, ok2 := c.Args[1].(*ast.BasicLit)
				if ok1 && ok2 && k.Value == `"Content-Type"` && strings.Contains(v.Value, "text/html") {
					set[fd.Name.Name] = true
				}
				return true
			})
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
