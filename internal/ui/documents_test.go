package ui

import (
	"io"
	"sort"
	"strings"
	"testing"

	g "maragu.dev/gomponents"

	"github.com/ZacxDev/muster/internal/notes"
)

// The registry of every full HTML document muster serves, and the derivation
// that turns it into the sweep input every document-level guard iterates.
//
// 🔴 THE REGISTRY IS CHECKED AGAINST THE SOURCE, NOT MAINTAINED BESIDE IT.
// TestEveryStandaloneDocumentIsRegistered parses this package and compares the
// functions that actually emit a Doctype against the rows below. That is the
// whole point: upstream shipped two documents unguarded because the registry was
// hand-written, so their absence from it was indistinguishable from their not
// existing, while every guard's docstring went on claiming coverage over "every
// document this app serves".

// namedDoc is one rendering, with a label for failure messages.
type namedDoc struct {
	name string
	node g.Node
}

// documentRenderer is one standalone document: the function that emits its
// Doctype, a human label for failures, and a way to render it with real data.
type documentRenderer struct {
	// fn is the function in this package that returns the Doctype — the key the
	// source-derived check matches on.
	fn string
	// label is how the document is named in a failure message.
	label string
	// render is the POPULATED rendering: real, non-empty, pairwise-distinct data,
	// so every list/card renderer inside the page actually executes.
	render func(io.Writer) error
	// renderEmpty is the SAME document at its EMPTY state, and it is not optional
	// polish. Upstream measured it: enriching `render` moved every sweep off the
	// empty-state markup, and a deliberately injected defect in an empty-state
	// renderer then SURVIVED a fully green suite that had killed it against the
	// previous bare fixture. The two states share no markup, so neither is ever a
	// sample of the other.
	renderEmpty func(io.Writer) error
}

// standaloneDocuments is every full HTML document muster serves.
//
// 🔴 EVERY `render` BELOW MUST PASS NON-EMPTY DATA. A bare fixture leaves every
// list-item and card renderer inside the page UNEXECUTED, so the sweeps inspect
// the empty state of exactly the renderers a defect would live in — which is a
// green run that examined almost nothing.
var standaloneDocuments = []documentRenderer{
	{
		fn: "Page", label: "the app shell (/)",
		render:      func(w io.Writer) error { return RenderPage(w, "tasks") },
		renderEmpty: func(w io.Writer) error { return RenderPage(w, "") },
	},
	{
		fn: "AgentDetailPage", label: "the agent detail page (/agents/<name>)",
		render: func(w io.Writer) error { return RenderAgentDetail(w, sampleAgentDetailView()) },
		// A freshly-created agent: no sessions yet, so the session list renders its
		// empty state instead of its rows. It keeps a NAME — the page's <h1> is the
		// agent's name and the route cannot resolve without one, so a nameless view
		// is not an empty state this document has, it is a document that cannot
		// exist.
		renderEmpty: func(w io.Writer) error {
			return RenderAgentDetail(w, AgentDetailView{
				ID: 8, Name: "worker-nova", DisplayName: "worker-nova", Status: "pending",
			})
		},
	},
	{
		// DELIBERATELY the richest fixture in the list: the detail shape is the
		// only place the markdown body, the attachment list, the session thread,
		// the comment boxes and the add-comment form render at all.
		fn: "TaskDetailPage", label: "the task detail page (/tasks/<id>)",
		render: func(w io.Writer) error { return RenderTaskDetail(w, sampleTaskDetailView()) },
		// A task with nothing on it. Every "…nothing yet" branch renders here.
		//
		// 🔴 UpdatedAt IS SET, and a bare notes.Note{ID: 1} is WRONG here even
		// though it reads as the natural "empty" fixture. "Empty" means the task
		// has no content; it does NOT mean the row has no timestamps —
		// notes.updated_at is NOT NULL with a default, so no note the store can
		// produce has a zero one. A zero UpdatedAt makes the card omit its
		// revision attribute, which silently drops this document OUT of the reach
		// of the stale-swap guard.
		renderEmpty: func(w io.Writer) error {
			return RenderTaskDetail(w, TaskCardView{
				Note:   notes.Note{ID: 1, UpdatedAt: fixedNow},
				Detail: true,
			})
		},
	},
	{
		// A real document a reader can LAND on — a legacy deeplink to a dismissed
		// task arrives here — so it owes the same contract as any other page.
		fn: "TaskNotFoundPage", label: "the task 404 page (/tasks/<id>, dismissed)",
		render: func(w io.Writer) error { return RenderTaskNotFound(w, taskNotFoundFixtureID) },
		// The id is the page's ONLY input, and it is not always present: a
		// /tasks/{id} whose id never parsed reaches this document with nothing to
		// name.
		renderEmpty: func(w io.Writer) error { return RenderTaskNotFound(w, "") },
	},
}

// taskNotFoundFixtureID is the 404 page's whole input.
const taskNotFoundFixtureID = "613"

// documentsFrom turns a document registry into the namedDoc list the sweeps
// iterate. It is the DERIVATION that matters: upstream's sweep list was once a
// second, hand-written copy of the registry, so a document could be correctly
// registered and still be skipped by every document-level guard in the package.
//
// Each row contributes BOTH of its renderings when it declares an empty state.
func documentsFrom(rows []documentRenderer) []namedDoc {
	docs := make([]namedDoc, 0, 2*len(rows))
	for _, r := range rows {
		r := r
		docs = append(docs, namedDoc{
			name: r.label,
			node: g.NodeFunc(func(w io.Writer) error { return r.render(w) }),
		})
		if r.renderEmpty == nil {
			continue
		}
		docs = append(docs, namedDoc{
			name: r.label + " — empty state",
			node: g.NodeFunc(func(w io.Writer) error { return r.renderEmpty(w) }),
		})
	}
	return docs
}

// allDocuments is the sweep input: the registry, plus the shell rendered once
// per tab.
func allDocuments() []namedDoc { return allDocumentsFrom(standaloneDocuments) }

// allDocumentsFrom is the sweep input built from a GIVEN registry. The parameter
// is the seam that lets a regression test hand this function an extra document
// and assert the sweeps then see it; asserting that against a copy of this logic
// would prove nothing about the list allDocuments() actually returns.
//
// 🔴 THE PER-TAB EXPANSION IS DERIVED FROM musterTabs, NOT SPELLED. The shell is
// ONE registered function but one document per tab to a reader, and upstream
// listed those tabs by hand here — a fifth place its tab set was duplicated. A
// tab added to the registry without being added here would simply never be
// swept, silently.
func allDocumentsFrom(rows []documentRenderer) []namedDoc {
	docs := documentsFrom(rows)
	for _, t := range musterTabs {
		t := t
		docs = append(docs, namedDoc{
			name: "shell:" + t.Key,
			node: g.NodeFunc(func(w io.Writer) error { return RenderPage(w, t.Key) }),
		})
	}
	return docs
}

// listFragments is every LIST/CARD partial the shell lazy-loads over htmx rather
// than rendering inline. They are unreachable from Page() at ANY fixture — every
// panel ships a skeleton and hx-gets its rows — so a page-only sweep can never
// see a task card, a privilege row, a profile, a runbook or an agent card.
func listFragments() []namedDoc {
	return []namedDoc{
		{"tasks-list", NotesCards(sampleTasksView())},
		{"privilege-requests", PrivilegeRequests(samplePrivilegeRequests())},
		{"profiles", Profiles(sampleProfiles())},
		{"runbooks", Runbooks(sampleRunbooks())},
		{"agents-cards", AgentsCards(sampleAgentCards())},
		{"repos-connected", ReposConnected("octo-owner", sampleRepos())},
		{"notes-edit-modal", NotesEditModalBody(sampleNoteEditView())},
	}
}

// allRenderables is allDocuments PLUS the lazy-loaded fragments. Use it for
// every assertion about an ELEMENT; those rules do not care whether the markup
// arrived as a full document or as an htmx swap body, and the fragments are
// where most measured defects actually lived. Use allDocuments for assertions
// about a DOCUMENT (exactly one <h1>, a body-attribute ledger): a fragment
// legitimately has neither.
func allRenderables() []namedDoc {
	return append(allDocuments(), listFragments()...)
}

func sampleAgentDetailView() AgentDetailView {
	return AgentDetailView{
		ID:              7,
		Name:            "worker-1",
		DisplayName:     "worker-1",
		Status:          "running",
		Sessions:        []SessionView{{ID: 1, Title: "First chat", Active: true}, {ID: 2}},
		ActiveSessionID: 1,
	}
}

// TestEveryStandaloneDocumentIsRegistered compares the registry against the
// package's actual Doctype emitters.
func TestEveryStandaloneDocumentIsRegistered(t *testing.T) {
	derived := doctypeEmitters(t)

	// POSITIVE CONTROL for the instrument. A parser wired to nothing — wrong
	// directory, wrong filter, a renamed helper — returns an empty set, and an
	// empty set makes the comparison below pass vacuously against an empty
	// registry and fail unhelpfully against a full one. Assert it found real
	// functions before believing anything it says.
	if len(derived) < 2 {
		t.Fatalf("the source scan found %d Doctype-emitting functions (%v) — it is not observing this "+
			"package, so nothing below is a claim about the code", len(derived), derived)
	}

	registered := make([]string, 0, len(standaloneDocuments))
	for _, d := range standaloneDocuments {
		registered = append(registered, d.fn)
	}
	sort.Strings(registered)

	if strings.Join(registered, ",") != strings.Join(derived, ",") {
		t.Errorf("standaloneDocuments registers %v; the package actually emits a Doctype from %v.\n"+
			"🔴 Every document-level guard in this package iterates the registry. A document missing from "+
			"it is not checked by ANY of them, while their docstrings keep claiming coverage over 'every "+
			"document this app serves'. Add a row (with real data), or delete the stale one.\n"+
			"⚠ If the DERIVED side names a helper you do not recognise, the Doctype has probably moved "+
			"into a shared shell function: the scan attributes it to the function whose body holds the "+
			"call, so the pages would vanish from the derived set and the helper would appear. That is a "+
			"scan limit, not a stale registry — see doctypeEmittersIn.",
			registered, derived)
	}
}

// TestEveryDocumentHasExactlyOneH1 is the document-level accessibility floor.
// A page with none is unnavigable by heading; a page with several has no single
// subject.
func TestEveryDocumentHasExactlyOneH1(t *testing.T) {
	docs := allDocuments()
	if len(docs) == 0 {
		t.Fatal("allDocuments() is empty — the sweep is wired to nothing")
	}
	for _, d := range docs {
		d := d
		t.Run(d.name, func(t *testing.T) {
			// documentSource, not renderString: every document embeds appScript,
			// whose comments quote `<h1 id="page-heading">`. Counting over the raw
			// render read that sentence as a second heading on EVERY document.
			html := documentSource(t, d.node)
			if n := strings.Count(html, "<h1"); n != 1 {
				t.Errorf("%s renders %d <h1> elements, want exactly 1", d.name, n)
			}
		})
	}
}

// TestTheShellRendersOneTabpanelPerRegisteredTab is the nav registry's
// behavioural half.
//
// 🔴 IT PINS A RELATIONSHIP — the SET of rendered tabpanel ids against the SET
// derived from musterTabs — rather than checking that some particular panel is
// present. A membership check passes while a panel is missing; an equality
// check fails in BOTH directions, which is what catches the two real shapes: a
// tab added to the registry whose panel nobody rendered (a nav entry that shows
// a blank page), and a panel rendered for a tab the registry does not carry (a
// surface reachable by URL and invisible in the nav).
func TestTheShellRendersOneTabpanelPerRegisteredTab(t *testing.T) {
	if len(musterTabs) == 0 {
		t.Fatal("musterTabs is empty — every assertion below would pass vacuously")
	}
	html := renderString(t, Page(defaultTab))

	want := make([]string, 0, len(musterTabs))
	for _, tb := range musterTabs {
		want = append(want, panelID(tb.Key))
	}
	sort.Strings(want)

	// Read the ids back out of the markup rather than asking whether each
	// expected one is present: only the full set can see an EXTRA panel.
	var got []string
	for _, id := range want {
		if strings.Contains(html, `id="`+id+`"`) {
			got = append(got, id)
		}
	}
	// Any `role="tabpanel"` whose id is not in want is an extra.
	extras := 0
	for i := 0; ; {
		j := strings.Index(html[i:], `role="tabpanel"`)
		if j < 0 {
			break
		}
		extras++
		i += j + 1
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the shell renders tabpanels %v; musterTabs declares %v.\n"+
			"A tab in the registry with no panel shows a blank page when selected; a panel with no "+
			"registry entry is reachable by URL and invisible in the nav.", got, want)
	}
	if extras != len(want) {
		t.Errorf("the shell renders %d elements with role=\"tabpanel\" but musterTabs declares %d tabs — "+
			"there is a tabpanel whose id is not panelID(<a registered tab key>)", extras, len(want))
	}
}

// TestTheBrowserScriptsTabRegistryMatchesGos is the nav registry's OTHER half,
// and the one upstream could not make.
//
// 🔴 A GO-SIDE TAB MISSING FROM THE BROWSER SCRIPT IS SILENT IN EVERY WAY THAT
// MATTERS: the server paints a deep-linked tab correctly, so the page looks
// right. Only an in-page switch onto it misbehaves — the heading keeps
// announcing the PREVIOUS tab's subject, and the swipe gesture skips it.
//
// 🔴 IT READS THE THREE EMITTED VALUES SEPARATELY, AND A MUTANT IS WHY. The
// first version searched the WHOLE script for each tab key. That is walkable
// across the emitted values: dropping a key from TABS alone left it present in
// the script anyway, because HEADINGS is keyed by the same strings — a guard
// about one structure, satisfied by a different one. Each value is now extracted
// and asserted on its own.
func TestTheBrowserScriptsTabRegistryMatchesGos(t *testing.T) {
	js := mustStripJSComments(t, jsSource(t, appScript()))

	tabsJS := jsValueAfter(t, js, "var TABS =")
	headingsJS := jsValueAfter(t, js, "var HEADINGS =")
	panelsJS := jsValueAfter(t, js, "var PANEL_SELECTOR =")

	// POSITIVE CONTROL: the extractor must have found real content, or every
	// assertion below is a comparison against an empty string.
	for name, v := range map[string]string{"TABS": tabsJS, "HEADINGS": headingsJS, "PANEL_SELECTOR": panelsJS} {
		if len(v) < 3 {
			t.Fatalf("extracted %q for %s — the script no longer emits it under that name, so nothing "+
				"below is a claim about the browser's copy of the registry", v, name)
		}
	}

	for _, tb := range musterTabs {
		if !strings.Contains(tabsJS, `"`+tb.Key+`"`) {
			t.Errorf("tab %q is in musterTabs but not in the script's TABS array (%s) — switchTab and the "+
				"swipe gesture both index into that array, so the tab is unreachable in-page", tb.Key, tabsJS)
		}
		if !strings.Contains(headingsJS, `"`+tb.Heading+`"`) {
			t.Errorf("heading %q for tab %q is not in the script's HEADINGS map — an SPA switch onto that "+
				"tab leaves the PREVIOUS tab's <h1> in place, announcing the wrong subject",
				tb.Heading, tb.Key)
		}
		if !strings.Contains(panelsJS, "#"+panelID(tb.Key)) {
			t.Errorf("PANEL_SELECTOR does not cover %s — the swipe gesture will not arm over that panel",
				panelID(tb.Key))
		}
	}

	// The SHRINK arm: a tab the shell no longer serves must not survive in the
	// script, or the swipe gesture walks onto a panel that is not rendered.
	for _, gone := range []string{"requests", "attention", "suggestions", "tmux", "layout"} {
		if strings.Contains(tabsJS, `"`+gone+`"`) {
			t.Errorf("the script's TABS array still names the upstream-only tab %q — switchTab would "+
				"toggle #panel-%s, which muster does not render", gone, gone)
		}
	}
}

// jsValueAfter returns the text between a `var X =` and its terminating `;`.
//
// ⚠ A TEXT EXTRACT, NOT A JS PARSE. It is adequate because every value it reads
// is emitted by navRegistryJS as a single-line JSON literal containing no
// semicolon; a value that stops being emitted that way makes the extract short
// or empty, and the caller's positive control fails rather than the assertions
// silently passing against "".
func jsValueAfter(t *testing.T, js, decl string) string {
	t.Helper()
	i := strings.Index(js, decl)
	if i < 0 {
		return ""
	}
	rest := js[i+len(decl):]
	j := strings.Index(rest, ";")
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:j])
}

// TestTheWordmarkSpellsMuster pins the RENDERED, tag-stripped wordmark as a
// WHOLE STRING.
//
// 🔴 THE FIRST VERSION OF THIS TEST WAS WALKABLE, AND A MUTANT PROVED IT. It
// asked whether the shell's rendered text CONTAINED "muster" anywhere. Reverting
// just the HEADER wordmark to the upstream brand left the SIDEBAR one intact, so
// the document still contained the right name and the test stayed green while
// half the app showed the wrong one — a substring check answering a question
// about presence when the question is about identity.
//
// Two changes close it. The wordmark is now ONE function (see wordmark()), so
// there is no half to rename; and this asserts equality against the whole
// normalised string rather than membership, so any edit to either syllable
// fails. Pinning the whole string means a deliberate rename must also edit this
// line — which is the cost, paid on purpose for a machine-readable claim.
func TestTheWordmarkSpellsMuster(t *testing.T) {
	if got := strippedText(renderString(t, wordmark())); got != "muster" {
		t.Errorf("the wordmark reassembles to %q, want %q.\n"+
			"It is split across two text nodes so the first syllable can be coloured, so the name is "+
			"never spelled whole in the source and no search for it can find this.", got, "muster")
	}
}

// TestBothWordmarkSitesGoThroughTheOneRenderer is the other half: the helper
// being correct is worth nothing if a call site stops using it.
//
// 🔴 IT IS A COUNT LEDGER, AND THE NUMBER IS THE POINT. Two is the header and
// the sidebar. A third site added by hand — the shape that produced the original
// half-renamed state — renders markup this count does not predict and fails
// here, as does a site that stops calling wordmark(). A "contains it at least
// once" check sees neither.
func TestBothWordmarkSitesGoThroughTheOneRenderer(t *testing.T) {
	mark := renderString(t, wordmark())
	if mark == "" {
		t.Fatal("wordmark() renders nothing — every count below would be vacuous")
	}
	shell := renderString(t, Page(defaultTab))
	if n := strings.Count(shell, mark); n != 2 {
		t.Errorf("the shell renders the wordmark markup %d times, want 2 (the header and the sidebar).\n"+
			"A site that spells the wordmark inline instead of calling wordmark() is how one of the two "+
			"was left showing the upstream brand after the carve.", n)
	}
}

// strippedText returns a node's rendered text with all tags removed, which is
// the only form in which adjacent text nodes concatenate the way a reader sees
// them.
func strippedText(html string) string {
	var text strings.Builder
	depth := 0
	for i := 0; i < len(html); i++ {
		switch html[i] {
		case '<':
			depth++
		case '>':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				text.WriteByte(html[i])
			}
		}
	}
	return strings.TrimSpace(text.String())
}

// TestTheShellCarriesNoPermissionRouterSurface is a LEAK check on the carve, and
// it is labelled as one rather than dressed up as a behavioural guard.
//
// ⚠ WHAT IT WOULD STILL ACCEPT: a router surface introduced under a name not in
// this list. It pins the known set, which is the set the carve actually had to
// remove, and nothing more. Its value is that these particular ids were wired
// into the shell's script and markup in a dozen places, so a partial revert is a
// realistic way for them to come back.
func TestTheShellCarriesNoPermissionRouterSurface(t *testing.T) {
	// Comment-stripped: the shell's script carries a comment NAMING the
	// attributes this sweep looks for, recording that the listener reading them
	// was deleted. Over the raw render that sentence is itself a finding.
	html := documentSource(t, Page(defaultTab))

	// POSITIVE CONTROL: the sweep must be able to find something in this
	// document at all, or a clean result says nothing.
	if !strings.Contains(html, panelID("tasks")) {
		t.Fatalf("the rendered shell does not contain %q — this sweep is not reading the document",
			panelID("tasks"))
	}

	for _, s := range []string{
		"panel-requests",
		"auto-approve-banner",
		"auto-approve-toggle",
		"auto-approve-popover",
		"auto-approve-persist-error",
		"badge-popover",
		"pending-badge",
		"queued-indicator",
		"data-action-url",
		"data-aa-project",
	} {
		if strings.Contains(html, s) {
			t.Errorf("the shell still renders %q, a permission-router surface that stays with the "+
				"upstream project. muster has no source for its state, so it can only ever show a "+
				"value that never changes.", s)
		}
	}
}
