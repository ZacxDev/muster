package api

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/features"
	"github.com/ZacxDev/muster/internal/plainscan"
)

// A TURNED-OFF FEATURE SAYS SO PLAINLY (the operator rule: muster's top layer
// shows no variable, .env, docs path or command). A person who opens a route
// whose feature is off gets the feature's plain name, "Turned off on this
// server." and the next step. A member's next step is to ask their admin, and
// their HTML carries no setup at all; an admin's is the ⓘ, which holds the
// exact line. Red at origin/main, where the page said
// "disabled: pages — set MUSTER_PAGES=1 in .env and restart (docs/features.md#pages)"
// to everyone (phone check of v0.250.0, F1).

// featureOffSet is a registry for every feature a route gates on, spelled the
// way the real catalog spells setup (MUSTER_ variables, a docs anchor). Names
// are synthetic; each PlainName differs from its Name, so a page that draws
// Name instead of PlainName fails.
func featureOffSet(t *testing.T) features.Set {
	t.Helper()
	one := func(name string) []features.Var { return []features.Var{{Name: name, Value: "1"}} }
	catalog := []features.Feature{
		{ID: features.SessionRouting, Name: "Session routing", PlainName: "Hand work to a session", Description: "Routes.", Enable: one("MUSTER_EXAMPLE_ROUTING")},
		{ID: features.Pages, Name: "Pages", PlainName: "Shareable pages", Description: "Pages.", Enable: one("MUSTER_EXAMPLE_PAGES")},
		{ID: features.PRMailRouting, Name: "Pull request routing", PlainName: "Code review mail", Description: "PR mail.", Enable: one("MUSTER_EXAMPLE_PR_MAIL")},
		{ID: features.Dictation, Name: "Dictation", PlainName: "Voice typing", Description: "A microphone.", Enable: one("MUSTER_EXAMPLE_STT")},
		{ID: features.Plugins, Name: "Plugin bots", PlainName: "Plugins", Description: "Bots.", Enable: one("MUSTER_EXAMPLE_PLUGINS")},
		{ID: features.PluginViewers, Name: "Plugin viewers", PlainName: "Plugin views", Description: "Viewers.", Enable: one("MUSTER_EXAMPLE_PLUGIN_ORIGINS")},
		{ID: features.AskLog, Name: "Ask log", PlainName: "Question history", Description: "Asks.", Enable: one("MUSTER_EXAMPLE_ASK_LOG")},
		{ID: features.Attachments, Name: "File attachments", PlainName: "Attach files", Description: "Files.", Enable: one("MUSTER_EXAMPLE_S3_BUCKET")},
	}
	st := map[string]features.Status{}
	for _, f := range catalog {
		st[f.ID] = features.Status{State: features.Off, Reason: "MUSTER_EXAMPLE_ is unset"}
	}
	// One misconfigured feature: its notice says it is not fully set up.
	st[features.Attachments] = features.Status{State: features.Misconfigured, Reason: "MUSTER_EXAMPLE_S3_BUCKET is not a bucket name"}
	set, err := features.Evaluate(catalog, st)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// featureOffSite is one call of writeFeatureDisabled: the function it is in,
// the feature it names, and how to reach it with a request.
type featureOffSite struct {
	fn, feature string
	viewers     bool // reach it with plugins on and no viewer origins
	call        func(s *Server, w http.ResponseWriter, r *http.Request)
}

func (c featureOffSite) key() string { return c.fn + "/" + c.feature }

// featureOffSites is EVERY call of writeFeatureDisabled in this package
// (TestEveryFeatureOffSiteIsListed holds it to the source, both ways).
var featureOffSites = []featureOffSite{
	{fn: "handleRouteReply", feature: features.SessionRouting, call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.handleRouteReply(w, r) }},
	{fn: "pageShareOn", feature: features.Pages, call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.pageShareOn(w, r) }},
	{fn: "handlePRSessionBind", feature: features.PRMailRouting, call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.handlePRSessionBind(w, r) }},
	{fn: "handlePRSessionList", feature: features.PRMailRouting, call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.handlePRSessionList(w, r) }},
	{fn: "handleTranscribe", feature: features.Dictation, call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.handleTranscribe(w, r) }},
	{fn: "requirePluginViewersOn", feature: features.Plugins, call: func(s *Server, w http.ResponseWriter, r *http.Request) {
		s.requirePluginViewersOn(func(http.ResponseWriter, *http.Request) { panic("reached past the gate") })(w, r)
	}},
	{fn: "requirePluginViewersOn", feature: features.PluginViewers, viewers: true, call: func(s *Server, w http.ResponseWriter, r *http.Request) {
		s.requirePluginViewersOn(func(http.ResponseWriter, *http.Request) { panic("reached past the gate") })(w, r)
	}},
	{fn: "handleAPIAsksLog", feature: features.AskLog, call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.handleAPIAsksLog(w, r) }},
	{fn: "handleAPIAsksSummary", feature: features.AskLog, call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.handleAPIAsksSummary(w, r) }},
	{fn: "handlePagesList", feature: features.Pages, call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.handlePagesList(w, r) }},
	{fn: "handlePageDetail", feature: features.Pages, call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.handlePageDetail(w, r) }},
	{fn: "handlePageView", feature: features.Pages, call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.handlePageView(w, r) }},
	{fn: "handlePageRevokeLink", feature: features.Pages, call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.handlePageRevokeLink(w, r) }},
	{fn: "handlePageRollback", feature: features.Pages, call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.handlePageRollback(w, r) }},
	{fn: "handlePageDelete", feature: features.Pages, call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.handlePageDelete(w, r) }},
	{fn: "requirePluginsOn", feature: features.Plugins, call: func(s *Server, w http.ResponseWriter, r *http.Request) {
		s.requirePluginsOn(func(http.ResponseWriter, *http.Request) { panic("reached past the gate") })(w, r)
	}},
	{fn: "handleUpload", feature: features.Attachments, call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.handleUpload(w, r) }},
}

// featureOffWant is what each feature's notice says, as literals.
var featureOffWant = map[string]struct{ name, line, memberNext, adminNext, code, docs string }{
	features.SessionRouting: {"Hand work to a session", "Turned off on this server.", "Ask your admin to turn this on.", "To turn it on, open its ⓘ.", "MUSTER_EXAMPLE_ROUTING=1", "docs/features.md#session-routing"},
	features.Pages:          {"Shareable pages", "Turned off on this server.", "Ask your admin to turn this on.", "To turn it on, open its ⓘ.", "MUSTER_EXAMPLE_PAGES=1", "docs/features.md#pages"},
	features.PRMailRouting:  {"Code review mail", "Turned off on this server.", "Ask your admin to turn this on.", "To turn it on, open its ⓘ.", "MUSTER_EXAMPLE_PR_MAIL=1", "docs/features.md#pr-mail-routing"},
	features.Dictation:      {"Voice typing", "Turned off on this server.", "Ask your admin to turn this on.", "To turn it on, open its ⓘ.", "MUSTER_EXAMPLE_STT=1", "docs/features.md#dictation"},
	features.Plugins:        {"Plugins", "Turned off on this server.", "Ask your admin to turn this on.", "To turn it on, open its ⓘ.", "MUSTER_EXAMPLE_PLUGINS=1", "docs/features.md#plugins"},
	features.PluginViewers:  {"Plugin views", "Turned off on this server.", "Ask your admin to turn this on.", "To turn it on, open its ⓘ.", "MUSTER_EXAMPLE_PLUGIN_ORIGINS=1", "docs/features.md#plugin-viewers"},
	features.AskLog:         {"Question history", "Turned off on this server.", "Ask your admin to turn this on.", "To turn it on, open its ⓘ.", "MUSTER_EXAMPLE_ASK_LOG=1", "docs/features.md#ask-log"},
	features.Attachments:    {"Attach files", "Not fully set up on this server.", "Ask your admin to finish setting it up.", "Something it needs is missing. Its ⓘ says what.", "MUSTER_EXAMPLE_S3_BUCKET=1", "docs/features.md#attachments"},
}

// featureOffJSON is each feature's typed 404 body, byte for byte, as it was
// before the plain page (origin/main f7f97c6e6): every key, reason included.
var featureOffJSON = map[string]string{
	features.SessionRouting: `{"docs":"docs/features.md#session-routing","enable":"set MUSTER_EXAMPLE_ROUTING=1 in .env and restart (docs/features.md#session-routing)","error":"feature_disabled","feature":"session-routing","message":"disabled: session-routing — set MUSTER_EXAMPLE_ROUTING=1 in .env and restart (docs/features.md#session-routing)","name":"Session routing","reason":"MUSTER_EXAMPLE_ is unset","state":"off"}` + "\n",
	features.Pages:          `{"docs":"docs/features.md#pages","enable":"set MUSTER_EXAMPLE_PAGES=1 in .env and restart (docs/features.md#pages)","error":"feature_disabled","feature":"pages","message":"disabled: pages — set MUSTER_EXAMPLE_PAGES=1 in .env and restart (docs/features.md#pages)","name":"Pages","reason":"MUSTER_EXAMPLE_ is unset","state":"off"}` + "\n",
	features.PRMailRouting:  `{"docs":"docs/features.md#pr-mail-routing","enable":"set MUSTER_EXAMPLE_PR_MAIL=1 in .env and restart (docs/features.md#pr-mail-routing)","error":"feature_disabled","feature":"pr-mail-routing","message":"disabled: pr-mail-routing — set MUSTER_EXAMPLE_PR_MAIL=1 in .env and restart (docs/features.md#pr-mail-routing)","name":"Pull request routing","reason":"MUSTER_EXAMPLE_ is unset","state":"off"}` + "\n",
	features.Dictation:      `{"docs":"docs/features.md#dictation","enable":"set MUSTER_EXAMPLE_STT=1 in .env and restart (docs/features.md#dictation)","error":"feature_disabled","feature":"dictation","message":"disabled: dictation — set MUSTER_EXAMPLE_STT=1 in .env and restart (docs/features.md#dictation)","name":"Dictation","reason":"MUSTER_EXAMPLE_ is unset","state":"off"}` + "\n",
	features.Plugins:        `{"docs":"docs/features.md#plugins","enable":"set MUSTER_EXAMPLE_PLUGINS=1 in .env and restart (docs/features.md#plugins)","error":"feature_disabled","feature":"plugins","message":"disabled: plugins — set MUSTER_EXAMPLE_PLUGINS=1 in .env and restart (docs/features.md#plugins)","name":"Plugin bots","reason":"MUSTER_EXAMPLE_ is unset","state":"off"}` + "\n",
	features.PluginViewers:  `{"docs":"docs/features.md#plugin-viewers","enable":"set MUSTER_EXAMPLE_PLUGIN_ORIGINS=1 in .env and restart (docs/features.md#plugin-viewers)","error":"feature_disabled","feature":"plugin-viewers","message":"disabled: plugin-viewers — set MUSTER_EXAMPLE_PLUGIN_ORIGINS=1 in .env and restart (docs/features.md#plugin-viewers)","name":"Plugin viewers","reason":"MUSTER_EXAMPLE_ is unset","state":"off"}` + "\n",
	features.AskLog:         `{"docs":"docs/features.md#ask-log","enable":"set MUSTER_EXAMPLE_ASK_LOG=1 in .env and restart (docs/features.md#ask-log)","error":"feature_disabled","feature":"ask-log","message":"disabled: ask-log — set MUSTER_EXAMPLE_ASK_LOG=1 in .env and restart (docs/features.md#ask-log)","name":"Ask log","reason":"MUSTER_EXAMPLE_ is unset","state":"off"}` + "\n",
	features.Attachments:    `{"docs":"docs/features.md#attachments","enable":"set MUSTER_EXAMPLE_S3_BUCKET=1 in .env and restart (docs/features.md#attachments)","error":"feature_disabled","feature":"attachments","message":"misconfigured: attachments — MUSTER_EXAMPLE_S3_BUCKET is not a bucket name; set MUSTER_EXAMPLE_S3_BUCKET=1 in .env and restart (docs/features.md#attachments)","name":"File attachments","reason":"MUSTER_EXAMPLE_S3_BUCKET is not a bucket name","state":"misconfigured"}` + "\n",
}

// featureOffPage reaches site as a document navigation by a signed-in reader
// of role, and returns the whole page.
func featureOffPage(t *testing.T, site featureOffSite, role authz.Role) string {
	t.Helper()
	ext := Extensions{}
	if site.viewers {
		ext.Plugins = stubPlugins{}
	}
	s, _ := featureServer(t, ext, featureOffSet(t))
	req := httptest.NewRequest(http.MethodGet, "/x", strings.NewReader("{}")).WithContext(asRole(role))
	req.Header.Set("Sec-Fetch-Dest", "document")
	rec := httptest.NewRecorder()
	site.call(s, rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("%s as %s: %d, want 404\n%.2000s", site.key(), role, rec.Code, rec.Body)
	}
	return rec.Body.String()
}

// featureOffRegion is the notice's own section.
func featureOffRegion(t *testing.T, page, id string) string {
	t.Helper()
	i := strings.Index(page, `<section data-feature-off="`+id+`"`)
	if i < 0 {
		t.Fatalf("no turned-off notice for %s; its <main> reads:\n%.1500s", id, page[max(0, strings.Index(page, "<main")):])
	}
	j := strings.Index(page[i:], `</section>`)
	if j < 0 {
		t.Fatalf("unterminated notice for %s", id)
	}
	return page[i : i+j+len(`</section>`)]
}

func topLayer(t *testing.T, markup string) string {
	t.Helper()
	texts, err := plainscan.TopLayerTexts(markup)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(texts, " | ")
}

var infoCodeRe = regexp.MustCompile(`data-info-code="">([^<]*)</code>`)

// TestAMemberSeesATurnedOffFeatureWithNoSetup: for EVERY route that answers a
// turned-off feature, a member's page is the plain name, the state and "Ask
// your admin to turn this on." (or "…to finish setting it up." for a feature
// that is not fully set up) — whole strings — and the document carries no
// variable, .env, docs path or ⓘ anywhere (absent, not hidden).
func TestAMemberSeesATurnedOffFeatureWithNoSetup(t *testing.T) {
	for _, site := range featureOffSites {
		t.Run(site.key(), func(t *testing.T) {
			want := featureOffWant[site.feature]
			page := featureOffPage(t, site, authz.RoleMember)
			// The whole document first: absent, not hidden.
			for _, bad := range []string{strings.TrimSuffix(want.code, "=1"), "MUSTER_EXAMPLE", ".env", "docs/features.md", "disabled:", "restart"} {
				if strings.Contains(page, bad) {
					t.Errorf("member's page carries %q", bad)
				}
			}
			region := featureOffRegion(t, page, site.feature)
			if got, w := topLayer(t, region), want.name+" | "+want.line+" | "+want.memberNext; got != w {
				t.Errorf("member's notice reads\n  %q\nwant\n  %q", got, w)
			}
			for _, bad := range []string{"data-info-sheet", "data-info-code", "popovertarget", "data-info-open"} {
				if strings.Contains(region, bad) {
					t.Errorf("member's notice carries an ⓘ (%q):\n%s", bad, region)
				}
			}
		})
	}
}

// TestAnAdminSeesATurnedOffFeatureWithTheSettingBehindTheInfo: the same
// routes for an admin: the plain top layer (no jargon), and an ⓘ whose code is
// exactly the line that turns the feature on, with the docs page in its sheet.
// It is also the member test's POSITIVE CONTROL: the strings the member's page
// must lack are here, in the same render path.
func TestAnAdminSeesATurnedOffFeatureWithTheSettingBehindTheInfo(t *testing.T) {
	for _, site := range featureOffSites {
		t.Run(site.key(), func(t *testing.T) {
			want := featureOffWant[site.feature]
			page := featureOffPage(t, site, authz.RoleAdmin)
			region := featureOffRegion(t, page, site.feature)
			label := "How to turn on " + want.name
			if got, w := topLayer(t, region), want.name+" | "+label+" | "+label+" | "+want.line+" | "+want.adminNext; got != w {
				t.Errorf("admin's notice reads\n  %q\nwant\n  %q", got, w)
			}
			texts, _ := plainscan.TopLayerTexts(region)
			for _, s := range texts {
				if m := plainscan.Jargon(s); m != "" {
					t.Errorf("admin's top layer says %q (%q)", s, m)
				}
			}
			codes := infoCodeRe.FindAllStringSubmatch(region, -1)
			if len(codes) != 1 || codes[0][1] != want.code {
				t.Errorf("admin's ⓘ code = %v, want exactly [%q]", codes, want.code)
			}
			if !strings.Contains(region, `data-info-sheet=""`) || !strings.Contains(region, ">"+want.docs+"</a>") {
				t.Errorf("admin's ⓘ has no sheet or no docs link %q:\n%s", want.docs, region)
			}
		})
	}
}

// TestASignedOutNavigationToATurnedOffFeatureIsSentToSignIn: the plugin gates
// run before any credential is examined, so a signed-out person opening
// /plugins used to get the notice — setup line and all. Now they are sent to
// sign in, like every other signed-in page; a signed-in admin through the real
// mux gets the notice.
func TestASignedOutNavigationToATurnedOffFeatureIsSentToSignIn(t *testing.T) {
	s, h := featureServer(t, Extensions{}, featureOffSet(t))
	for _, path := range []string{"/plugins", routingRulesPath} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Sec-Fetch-Dest", "document")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), loginPath) || strings.Contains(w.Body.String(), "MUSTER_EXAMPLE") {
			t.Errorf("signed-out GET %s: %d Location=%q\n%.1000s", path, w.Code, w.Header().Get("Location"), w.Body)
		}
		page := sessionGet(h, s, path, true)
		if page.Code != http.StatusNotFound || !strings.Contains(page.Body.String(), `<section data-feature-off="plugins"`) {
			t.Errorf("signed-in GET %s: %d\n%.2000s", path, page.Code, page.Body)
		}
	}
}

// TestATurnedOffFeaturesJSONKeepsItsSetup — an INVARIANT GUARD, not regression
// coverage: for EVERY site, the machine answer (the CLI reads `enable`) is the
// pre-change body BYTE FOR BYTE — every key, reason included. Green before
// and after; dropping or rewording any key turns it red.
func TestATurnedOffFeaturesJSONKeepsItsSetup(t *testing.T) {
	for _, site := range featureOffSites {
		t.Run(site.key(), func(t *testing.T) {
			ext := Extensions{}
			if site.viewers {
				ext.Plugins = stubPlugins{}
			}
			s, _ := featureServer(t, ext, featureOffSet(t))
			// No Accept: the share door answers its own JSON to an
			// Accept: application/json caller (pageShareOn), so a bare
			// request is the one every site hands to writeFeatureDisabled.
			req := httptest.NewRequest(http.MethodGet, "/x", strings.NewReader("{}")).WithContext(asRole(authz.RoleMember))
			w := httptest.NewRecorder()
			site.call(s, w, req)
			if w.Code != http.StatusNotFound || w.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("%d %q", w.Code, w.Header().Get("Content-Type"))
			}
			if got, want := w.Body.String(), featureOffJSON[site.feature]; got != want {
				t.Errorf("JSON body changed:\n got %s\nwant %s", got, want)
			}
		})
	}
}

// TestATokenCallerGetsTheJSONNeverASignInRedirect: a caller a TOKEN admitted
// (the hook token here) whose request looks like a page load (Accept:
// text/html) gets the unchanged JSON 404 — never a 303 to a sign-in form it
// cannot fill, and never the person's page (that is a browser session's).
func TestATokenCallerGetsTheJSONNeverASignInRedirect(t *testing.T) {
	_, h := featureServer(t, Extensions{}, featureOffSet(t))
	for _, c := range []struct{ method, path, feature string }{
		{http.MethodPost, "/api/routes/abc/reply", features.SessionRouting},
		{http.MethodGet, prSessionsPath, features.PRMailRouting},
	} {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+testHookToken)
		req.Header.Set("Accept", "text/html")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		var b map[string]any
		if w.Code != http.StatusNotFound || w.Header().Get("Content-Type") != "application/json" ||
			json.Unmarshal(w.Body.Bytes(), &b) != nil || b["error"] != FeatureDisabledError || b["feature"] != c.feature {
			t.Errorf("%s %s with a token and Accept: text/html: %d %q Location=%q\n%.600s",
				c.method, c.path, w.Code, w.Header().Get("Content-Type"), w.Header().Get("Location"), w.Body)
		}
	}
}

// TestEveryFeatureOffSiteIsListed: featureOffSites names every call of
// writeFeatureDisabled in this package's source, by enclosing function and
// feature — failing when a call is added (a new gated route the tests above
// would not reach) or removed (a stale row).
func TestEveryFeatureOffSiteIsListed(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, n, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(x ast.Node) bool {
				call, ok := x.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "writeFeatureDisabled" || len(call.Args) != 3 {
					return true
				}
				id := "?"
				if a, ok := call.Args[2].(*ast.SelectorExpr); ok {
					if v, ok := featureIDs[a.Sel.Name]; ok {
						id = v
					} else {
						id = "features." + a.Sel.Name
					}
				}
				got = append(got, fd.Name.Name+"/"+id)
				return true
			})
		}
	}
	var want []string
	for _, c := range featureOffSites {
		want = append(want, c.key())
	}
	sort.Strings(got)
	sort.Strings(want)
	got = dedupe(got)
	if len(got) < 10 {
		t.Fatalf("the scan found %d call(s) — it is not observing this package", len(got))
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("writeFeatureDisabled is called from\n  %v\nfeatureOffSites names\n  %v\nList each new call (and how to reach it) so the plain-page tests cover it.", got, want)
	}
}

// featureIDs maps the features package's constant names the call sites use
// to their ids.
var featureIDs = map[string]string{
	"SessionRouting": features.SessionRouting, "Pages": features.Pages, "PRMailRouting": features.PRMailRouting,
	"Dictation": features.Dictation, "Plugins": features.Plugins, "PluginViewers": features.PluginViewers,
	"AskLog": features.AskLog, "Attachments": features.Attachments,
}

func dedupe(xs []string) []string {
	var out []string
	for i, x := range xs {
		if i == 0 || x != xs[i-1] {
			out = append(out, x)
		}
	}
	return out
}
