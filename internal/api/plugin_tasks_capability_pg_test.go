package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/plugins"
)

// THE `tasks` CAPABILITY through the real server against a real Postgres
// (docs/design/meetings.md decision 11; plugin_tasks_capability.go): a token
// minted on /plugins with Files tasks alone files and comments on
// POST /plugin/v1/tasks and is refused everything else; a messages token is
// unchanged; the owner turns it on and off from the plugin's card, audited.

// ptMint mints a plugin from the /plugins form with the given extra
// fields, as the fixture's owner, and returns its token.
func (f *ptFixture) ptMint(name string, extra url.Values) string {
	f.t.Helper()
	form := url.Values{"name": {name}, "label": {"Example filer"}}
	for k, v := range extra {
		form[k] = v
	}
	rec := f.do("POST", "/plugins", form.Encode(), f.session())
	m := mintedPluginRE.FindStringSubmatch(rec.Body.String())
	if rec.Code != http.StatusOK || m == nil {
		f.t.Fatalf("mint %s: %d", name, rec.Code)
	}
	return m[1]
}

// ptPluginTask posts body to POST /plugin/v1/tasks with tok.
func (f *ptFixture) ptPluginTask(tok string, body map[string]any) (int, map[string]any) {
	f.t.Helper()
	raw, _ := json.Marshal(body)
	rec := f.do("POST", "/plugin/v1/tasks", string(raw), func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) })
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// ptOwnPlugin is the owner's plugin named name, read through the store.
func (f *ptFixture) ptOwnPlugin(name string) plugins.Plugin {
	f.t.Helper()
	list, err := f.srv.ext.Plugins.List(f.asOwner)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, p := range list {
		if p.Name == name && p.RevokedAt == nil {
			return p
		}
	}
	f.t.Fatalf("no live plugin %q", name)
	return plugins.Plugin{}
}

// TestATasksOnlyTokenFilesTasksAndNothingElse: the /plugins form with Files
// tasks ticked and nothing else mints a token whose capabilities are exactly
// [tasks] (audited plugin.created with them). It files a task with a comment
// (201) and comments on it again (200), as a messages token does. It is 400
// on route_to_chief and on a reference (messages powers), and 403 naming the
// capability on the messages, drafts, task listing, task-writes and chief
// instructions routes. The control: the messages token minted by the fixture
// still files (201), and is not refused route_to_chief for the capability.
func TestATasksOnlyTokenFilesTasksAndNothingElse(t *testing.T) {
	f := newPTFixture(t)
	tok := f.ptMint("example-filer", url.Values{"tasks": {"1"}})
	p := f.ptOwnPlugin("example-filer")
	if !slices.Equal(p.Capabilities, []plugins.Capability{plugins.CapTasks}) || p.DataScope != plugins.ViewerOnlyDataScope {
		t.Fatalf("the Files tasks mint: caps %v scope %q; want [tasks], none", p.Capabilities, p.DataScope)
	}
	if msgs := f.ptOwnPlugin("example-inbox"); !slices.Equal(msgs.Capabilities, []plugins.Capability{plugins.CapMessages}) {
		t.Fatalf("CONTROL: the form without Files tasks minted %v; want [messages]", msgs.Capabilities)
	}

	key := "tracker:tk" + f.suffix
	code, out := f.ptPluginTask(tok, map[string]any{"key": key, "create": map[string]any{"title": "Heron ledger", "body": "Count the herons."},
		"comment": "evt-1 heron count opened"})
	if code != http.StatusCreated || out["created"] != true || out["commented"] != true {
		t.Fatalf("a tasks token filing: %d %v; want 201 created+commented", code, out)
	}
	if code, out := f.ptPluginTask(tok, map[string]any{"key": key, "comment": "evt-2 heron count closed"}); code != http.StatusOK || out["commented"] != true {
		t.Fatalf("a tasks token commenting: %d %v; want 200 commented", code, out)
	}
	if live := f.live(key); len(live) != 1 || len(f.comments(live[0].ID)) != 2 {
		t.Fatalf("after the tasks token's two calls: %d task(s); want 1 with 2 comments", len(live))
	}

	if code, out := f.ptPluginTask(tok, map[string]any{"key": key, "comment": "evt-3 assigned", "route_to_chief": true, "notice": "assigned"}); code != http.StatusBadRequest || out["error"] != PluginTasksOnlyRouteRefusal {
		t.Fatalf("a tasks token's route_to_chief: %d %v; want 400 %q", code, out, PluginTasksOnlyRouteRefusal)
	}
	if code, out := f.ptPluginTask(tok, map[string]any{"key": "tracker:ref" + f.suffix, "event_id": "e-1",
		"reference": map[string]any{"title": "Egret ledger", "body": "Count the egrets.", "status": "open", "updated_at": "2030-01-02T03:04:05Z"}}); code != http.StatusBadRequest || out["error"] != PluginTasksOnlyReferenceRefusal {
		t.Fatalf("a tasks token's reference: %d %v; want 400 %q", code, out, PluginTasksOnlyReferenceRefusal)
	}

	for _, rt := range []struct{ method, path, body string }{
		{"POST", "/plugin/v1/messages", pgnMsg},
		{"POST", "/plugin/v1/drafts/claim", `{}`},
		{"POST", "/plugin/v1/drafts/5/result", `{}`},
		{"GET", "/plugin/v1/tasks?namespace=tracker&live=true", ""},
		{"POST", "/plugin/v1/task-writes/claim", `{}`},
		{"POST", "/plugin/v1/task-writes/5/result", `{}`},
		{"PUT", "/plugin/v1/chief/instructions", `{"markdown":"x"}`},
	} {
		rec := f.do(rt.method, rt.path, rt.body, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) })
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "capability this route needs") {
			t.Errorf("a tasks token on %s %s: %d %s; want 403 naming the capability", rt.method, rt.path, rec.Code, rec.Body)
		}
	}

	ctrlKey := "tracker:mc" + f.suffix
	if code, out := f.ptPluginTask(f.tok, map[string]any{"key": ctrlKey, "create": map[string]any{"title": "Ibis ledger", "body": "Count the ibises."}}); code != http.StatusCreated {
		t.Fatalf("CONTROL: the messages token filing: %d %v; want 201", code, out)
	}
	if code, out := f.ptPluginTask(f.tok, map[string]any{"key": ctrlKey, "comment": "evt-9 assigned", "route_to_chief": true, "notice": "assigned"}); code == http.StatusBadRequest || out["error"] == PluginTasksOnlyRouteRefusal {
		t.Fatalf("CONTROL: the messages token's route_to_chief: %d %v; want it not refused for the capability", code, out)
	}
}

// TestTheOwnerTurnsFilesTasksOnAndOff: a chief_extension-only plugin's token
// is 403 on POST /plugin/v1/tasks; the owner's card draws Turn on Files tasks
// (and none on the messages plugin), the same-origin form turns it on (303),
// the SAME token then files (201), and the change is audited
// plugin.capability.changed with the old and new sets. A cross-site form and
// another owner's session change nothing; Turn off makes the token 403 again.
func TestTheOwnerTurnsFilesTasksOnAndOff(t *testing.T) {
	f := newPTFixture(t)
	ctx := context.Background()
	pool, err := db.Connect(ctx, dbtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	tok, p, err := f.srv.ext.Plugins.CreatePlugin(f.asOwner, plugins.NewPlugin{Name: "example-guide", Label: "Example guide", DataScope: "none",
		Capabilities: []plugins.Capability{plugins.CapChiefExtension}})
	if err != nil {
		t.Fatal(err)
	}
	pid := strconv.FormatInt(p.ID, 10)
	key := "tracker:tg" + f.suffix
	file := func() int {
		code, _ := f.ptPluginTask(tok, map[string]any{"key": key, "create": map[string]any{"title": "Plover ledger", "body": "Count the plovers."}})
		return code
	}
	if code := file(); code != http.StatusForbidden {
		t.Fatalf("control: before the grant the plugin's task: %d; want 403", code)
	}
	page := f.do("GET", "/plugins", "", f.session()).Body.String()
	if !strings.Contains(page, `action="/plugins/`+pid+`/tasks" data-plugin-tasks>`) || !strings.Contains(page, "Turn on Files tasks") {
		t.Fatalf("the guide plugin's card draws no Turn on Files tasks")
	}
	if inbox := f.ptOwnPlugin("example-inbox"); strings.Contains(page, `action="/plugins/`+strconv.FormatInt(inbox.ID, 10)+`/tasks"`) {
		t.Fatalf("the messages plugin's card draws a Files tasks toggle; messages already files tasks")
	}
	if !strings.Contains(page, `data-testid="plugin-tasks"`) {
		t.Fatalf("the create form draws no Files tasks box")
	}
	toggle := func(on string, set func(*http.Request)) int {
		return f.do("POST", "/plugins/"+pid+"/tasks", url.Values{"on": {on}}.Encode(), set).Code
	}
	caps := func() []string {
		var c []string
		if err := pool.QueryRow(authz.System(ctx), `SELECT capabilities FROM plugins WHERE id = $1`, p.ID).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	crossSite := func(r *http.Request) {
		f.session()(r)
		r.Header.Set("Origin", "http://elsewhere.example.invalid")
		r.Header.Set("Sec-Fetch-Site", "cross-site")
	}
	if code := toggle("1", crossSite); code == http.StatusSeeOther || slices.Contains(caps(), "tasks") {
		t.Fatalf("a cross-site Turn on: %d, caps %v; want refused, unchanged", code, caps())
	}
	var stranger int64
	if err := pool.QueryRow(authz.System(ctx), `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`,
		"ptr2"+f.suffix).Scan(&stranger); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(authz.System(context.Background()), `DELETE FROM users WHERE id = $1`, stranger)
	})
	if code := toggle("1", func(r *http.Request) {
		f.session()(r)
		r.Header.Set("X-Plugin-Test-User", strconv.FormatInt(stranger, 10))
	}); code != http.StatusNotFound || slices.Contains(caps(), "tasks") {
		t.Fatalf("another owner's Turn on: %d, caps %v; want 404, unchanged", code, caps())
	}

	if code := toggle("1", f.session()); code != http.StatusSeeOther {
		t.Fatalf("the owner's Turn on: %d; want 303", code)
	}
	if got := caps(); !slices.Equal(got, []string{"chief_extension", "tasks"}) {
		t.Fatalf("after the grant: caps %v; want [chief_extension tasks]", got)
	}
	if code := file(); code != http.StatusCreated {
		t.Fatalf("after the grant the SAME token's task: %d; want 201", code)
	}
	var n int
	var detail string
	if err := pool.QueryRow(authz.System(ctx), `SELECT count(*), coalesce(max(detail::text), '') FROM audit_log
		WHERE action = 'plugin.capability.changed' AND object = $1 AND user_id = $2`, plugins.Object(p.ID), f.uid).Scan(&n, &detail); err != nil {
		t.Fatal(err)
	}
	if n != 1 || !strings.Contains(detail, `"capability": "tasks"`) || !strings.Contains(detail, `"old": ["chief_extension"]`) ||
		!strings.Contains(detail, `"new": ["chief_extension", "tasks"]`) {
		t.Fatalf("the audit: %d row(s), %s; want one naming tasks, old and new", n, detail)
	}
	if page := f.do("GET", "/plugins", "", f.session()).Body.String(); !strings.Contains(page, "Turn off Files tasks") {
		t.Fatalf("after the grant the card draws no Turn off Files tasks")
	}

	if code := toggle("0", f.session()); code != http.StatusSeeOther {
		t.Fatalf("the owner's Turn off: %d; want 303", code)
	}
	if code := file(); code != http.StatusForbidden {
		t.Fatalf("after Turn off the token's task: %d; want 403", code)
	}
}
