package ui

import (
	"bytes"
	"encoding/json"
	"html"
	"io"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"testing"

	g "maragu.dev/gomponents"

	"github.com/ZacxDev/muster/web"
)

// THE SHELL DOCUMENTS CARRY THEIR SCRIPTS AS STATIC FILES (static_script.go).

// inlineScriptBudget is the most JavaScript one inline <script> on a shell
// document may hold, and inlineScriptTotal the most all of them together may.
// What stays inline is what must run inside a boosted swap's own task
// (tasksRefilterGuard, ~0.5 KB); anything bigger is code that belongs in a
// static file, re-sent with every navigation.
const (
	inlineScriptBudget = 1024
	inlineScriptTotal  = 2048
)

var anyScript = regexp.MustCompile(`(?s)<script([^>]*)>(.*?)</script>`)

// scriptedDocuments renders, as sent, every document that carries the shell's
// scripts, for one feature set: the shell on each tab it draws (the Tasks tab
// with its board drawn, as `/` serves it), the composer page, the task page
// and the agent page.
func scriptedDocuments(t *testing.T, f Features) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, tb := range visibleTabs(f) {
		data := ShellData{}
		if tb.Key == "tasks" {
			data = data.WithTasks(sampleTasksView(), "mark-1")
		}
		out["shell "+tb.Key] = renderRaw(t, PageWith(tb.Key, f, data))
	}
	out["compose"] = renderRaw(t, ComposePage(f, "a shared body"))
	out["task detail"] = renderRaw(t, g.NodeFunc(func(w io.Writer) error { return RenderTaskDetail(w, detailFixture(), f) }))
	out["agent detail"] = renderRaw(t, g.NodeFunc(func(w io.Writer) error { return RenderAgentDetail(w, sampleAgentDetailView(), f) }))
	return out
}

// TestTheShellCarriesNoInlineScriptOverTheBudget: the shell documents send
// their code by reference, not inline. Red before this change: the Tasks
// shell carried ~230 KB of inline JavaScript, appScript alone ~72 KB.
func TestTheShellCarriesNoInlineScriptOverTheBudget(t *testing.T) {
	f := Features{GitHub: true, Connectors: true, Admin: true, Push: true, Attention: true, SnapUser: 7, SnapBuild: "b1"}
	docs := scriptedDocuments(t, f)
	for _, name := range []string{"shell tasks", "shell agents", "compose"} {
		doc := docs[name]
		total, linked := 0, 0
		for _, m := range anyScript.FindAllStringSubmatch(doc, -1) {
			if strings.Contains(m[1], " src=") {
				if strings.Contains(m[1], `src="/static/js/`) {
					linked++
				}
				continue
			}
			n := len(m[2])
			total += n
			if n > inlineScriptBudget {
				head := strings.TrimSpace(m[2])
				if len(head) > 80 {
					head = head[:80]
				}
				t.Errorf("%s: an inline <script> of %d bytes (budget %d): %q…", name, n, inlineScriptBudget, head)
			}
		}
		if total > inlineScriptTotal {
			t.Errorf("%s: %d bytes of inline JavaScript in all (budget %d)", name, total, inlineScriptTotal)
		}
		// POSITIVE CONTROL: the code did not vanish, it moved. A document
		// that loaded no static script would pass the budget trivially.
		if linked < 10 {
			t.Errorf("%s: only %d static scripts linked — the budget above is met by a document that runs nothing", name, linked)
		}
		t.Logf("%s: %d bytes, %d bytes inline JS, %d static scripts", name, len(doc), total, linked)
	}
}

// TestStaticScriptsAreTheSameForEveryone: a static script's URL and bytes do
// not move with the viewer, the feature set or the host settings — only the
// tag's attributes do. Two deliberately different worlds are rendered; every
// script both link must be the same URL (so the same bytes), and no
// registered file may contain either world's own values.
func TestStaticScriptsAreTheSameForEveryone(t *testing.T) {
	a := Features{SnapUser: 1, SnapBuild: "build-alpha-1111"}
	b := Features{GitHub: true, Connectors: true, Admin: true, Push: true, Attention: true, SnapUser: 2, SnapBuild: "build-omega-9999"}

	withHostPrefs(t, HostPreferences{})
	docsA := scriptedDocuments(t, a)
	withHostPrefs(t, HostPreferences{Order: []string{"hostzulu", "hostyankee"}})
	docsB := scriptedDocuments(t, b)

	urls := func(doc string) map[string]string { // path → ?v=
		out := map[string]string{}
		for _, m := range staticScriptTag.FindAllStringSubmatch(doc, -1) {
			out[m[1]] = m[2]
		}
		return out
	}
	shared := 0
	for name, da := range docsA {
		db, ok := docsB[name]
		if !ok {
			continue
		}
		ua, ub := urls(da), urls(db)
		for p, va := range ua {
			vb, ok := ub[p]
			if !ok {
				continue
			}
			shared++
			if va != vb {
				t.Errorf("%s: %s is ?v=%s for one viewer and ?v=%s for another — its bytes depend on the request", name, p, va, vb)
			}
		}
		// POSITIVE CONTROL: the two worlds really do render different
		// documents, and the difference reaches the scripts' tags.
		if da == db {
			t.Errorf("%s: the two feature sets rendered the same document, so nothing here could differ", name)
		}
	}
	if shared < 15 {
		t.Fatalf("CONTROL: only %d script links shared across the two worlds", shared)
	}
	appA, appB := html.UnescapeString(docsA["shell tasks"]), html.UnescapeString(docsB["shell tasks"])
	for _, c := range []struct{ doc, want string }{
		{appA, `data-build="build-alpha-1111"`}, {appB, `data-build="build-omega-9999"`},
		{appB, `"repos"`}, {appB, `["hostzulu","hostyankee","*","cluster"]`},
	} {
		if !strings.Contains(c.doc, c.want) {
			t.Errorf("CONTROL: the per-document input %s is not on the page's tags", c.want)
		}
	}

	// No world's own value is in any served file.
	paths := web.Generated()
	if len(paths) < 15 {
		t.Fatalf("CONTROL: only %d generated files registered", len(paths))
	}
	for _, p := range paths {
		body, err := fs.ReadFile(web.Files, strings.TrimPrefix(p, "/"))
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		for _, own := range []string{"build-alpha-1111", "build-omega-9999", "hostzulu", "hostyankee"} {
			if bytes.Contains(body, []byte(own)) {
				t.Errorf("%s carries %q, a value of one request", p, own)
			}
		}
	}
}

// TestTheSwapInlinerKnowsEveryPageScript: a boosted swap can run a script
// inside the swap only if the window holds its text (swap_inline.go), so the
// inliner's list must be every other page script, at its current URL — and
// every text must be inlinable (no closing script tag inside it).
func TestTheSwapInlinerKnowsEveryPageScript(t *testing.T) {
	body, err := fs.ReadFile(web.Files, strings.TrimPrefix(swapInlineSrc, "/"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`var URLS = (\[[^\]]*\]);`).FindSubmatch(body)
	if m == nil {
		t.Fatal("swap-inline.js has no URLS list")
	}
	var urls []string
	if err := json.Unmarshal(m[1], &urls); err != nil {
		t.Fatal(err)
	}
	want := []string{}
	for _, p := range web.Generated() {
		if p != swapInlineSrc {
			want = append(want, web.Asset(p))
		}
	}
	if strings.Join(urls, " ") != strings.Join(want, " ") {
		t.Errorf("the inliner knows\n  %v\nwant every other page script\n  %v", urls, want)
	}
	for _, p := range web.Generated() {
		b, _ := fs.ReadFile(web.Files, strings.TrimPrefix(p, "/"))
		if bytes.Contains(bytes.ToLower(b), []byte("</script")) {
			t.Errorf("%s spells a closing script tag, so it cannot be inlined into a swap", p)
		}
	}
}

// TestEveryGeneratedScriptIsLinkedAndEveryLinkIsGenerated pins the ledger both
// ways: a registered file no document links is dead weight in every service
// worker's precache, and a linked path nothing registered is a 404 (which
// expandStaticScripts also refuses).
func TestEveryGeneratedScriptIsLinkedAndEveryLinkIsGenerated(t *testing.T) {
	f := Features{GitHub: true, Connectors: true, Admin: true, Push: true, Attention: true, SnapUser: 7, SnapBuild: "b1"}
	linked := map[string]bool{}
	for _, doc := range scriptedDocuments(t, f) {
		for _, m := range staticScriptTag.FindAllStringSubmatch(doc, -1) {
			linked[m[1]] = true
			if web.AssetHash(m[1]) != m[2] {
				t.Errorf("%s is linked at ?v=%s, not at its content hash %q", m[1], m[2], web.AssetHash(m[1]))
			}
		}
	}
	gen := web.Generated()
	for _, p := range gen {
		if !linked[p] {
			t.Errorf("%s is registered but no shell document links it", p)
		}
		delete(linked, p)
	}
	var stray []string
	for p := range linked {
		stray = append(stray, p)
	}
	sort.Strings(stray)
	if len(stray) > 0 {
		t.Errorf("linked but not registered: %v", stray)
	}
	if len(gen) < 15 {
		t.Fatalf("CONTROL: only %d generated files", len(gen))
	}
}
