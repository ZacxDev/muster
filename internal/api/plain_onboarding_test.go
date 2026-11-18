package api

import (
	"bytes"
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE FIRST-RUN SCREEN AND THE EMPTY STATES IN PLAIN WORDS (operator rule:
// muster is for people who are not engineers, so its own chrome shows no
// command, URL, flag, file name, path or id in its top layer). The onboarding
// page says what to do in plain steps; the setup prompt and the command that
// starts an agent are each behind an ⓘ details sheet (ui.InfoSheet) with a
// Copy button, and only for a viewer who may see setup details
// (ui.Features.Setup) — anyone else is told to ask their admin.

// onboardingJargon is what the onboarding page's and the task board's top
// layer must not hold, beyond #806's address scan (jargonInTopLayer) and
// #814's flag scan (hostJargon): a URL, a shell command (curl, tmux), a file
// name (INSTALL.md), a filesystem path, the shell's "working directory", the
// "repos" abbreviation and a task's id ("Task #12").
var onboardingJargon = regexp.MustCompile(`(?i)https?://|\bcurl\b|\btmux\b|\b[\w-]+\.md\b|(^|\s)/[\w.-]+/|\bworking director|\brepos\b|\btask #\d`)

// assertNoOnboardingJargon fails for every top-layer text of markup
// (topLayerTexts) that any of the three scans matches.
func assertNoOnboardingJargon(t *testing.T, what, markup string) {
	t.Helper()
	texts := topLayerTexts(t, markup)
	if len(texts) == 0 {
		t.Fatalf("%s: CONTROL: no top-layer text at all — the scan sees nothing", what)
	}
	for _, s := range texts {
		if onboardingJargon.MatchString(s) || jargonInTopLayer.MatchString(s) || hostJargon.MatchString(s) {
			t.Errorf("%s: the top layer says %q", what, s)
		}
	}
}

// TestTheOnboardingJargonScanSeesACommandAURLAndAPath: the scan's own
// controls. POSITIVE: every finding's old text — in a text node, a tooltip, an
// aria-label and a placeholder — is caught. NEGATIVE: the plain words that
// replace them pass, and the exact prompt and command inside an ⓘ sheet, a
// data attribute (the Copy button's data-copy-text) or a script are not read.
func TestTheOnboardingJargonScanSeesACommandAURLAndAPath(t *testing.T) {
	for _, bad := range []string{
		`<p>Your coding agents show up in the menu when they run inside tmux on a connected machine.</p>`,
		`<pre><code>tmux new -A -s work claude</code></pre>`,
		`<pre><code>` + html.EscapeString(ui.OnboardingPrompt) + `</code></pre>`,
		`<p>Or follow <a href="x">INSTALL.md</a> yourself.</p>`,
		`<p>Tap + to capture a task for a working directory.</p>`,
		`<input placeholder="/path/to/project (optional)">`,
		`<input placeholder="Search repos… (optional)">`,
		`<h1>Task #1</h1>`,
		`<button aria-label="Run curl -fsSL">x</button>`,
		`<span title="See https://example.test/guide">x</span>`,
	} {
		hit := false
		for _, s := range topLayerTexts(t, bad) {
			hit = hit || onboardingJargon.MatchString(s) || jargonInTopLayer.MatchString(s) || hostJargon.MatchString(s)
		}
		if !hit {
			t.Errorf("the scan missed the jargon in %s", bad)
		}
	}
	ok := `<p>Your agents appear here once a computer is connected.</p><p>Connect your first computer</p>` +
		`<p>Tap + to add something for an agent to do.</p><input placeholder="Pick a folder (optional)">` +
		`<input placeholder="Search repositories… (optional)"><h1>Untitled task</h1><p>Or read the <a href="https://x.test/INSTALL.md">install guide</a> yourself.</p>` +
		`<button data-copy-text="` + html.EscapeString(ui.OnboardingPrompt) + `">Copy setup instructions</button>` +
		`<div data-info-sheet=""><p>Setup instructions</p><code>` + html.EscapeString(ui.OnboardingPrompt) + `</code><code>tmux new -A -s work claude</code></div>` +
		`<script>var c="curl https://x /path/to/y INSTALL.md"</script>`
	texts := topLayerTexts(t, ok)
	if len(texts) < 8 {
		t.Fatalf("CONTROL: the negative fixture's plain texts were not read: %q", texts)
	}
	for _, s := range texts {
		if onboardingJargon.MatchString(s) || jargonInTopLayer.MatchString(s) || hostJargon.MatchString(s) {
			t.Errorf("the scan flagged %q in %s", s, ok)
		}
	}
}

// TestNoTopLayerOfTheOnboardingPageSpellsACommand: GET /agents with nothing
// to open (the first-run screen, handleAgentsHome) — for an admin and for a
// member — says what to do in plain words: no command, URL, flag or file name
// in its top layer. The admin's steps each carry an ⓘ whose sheet holds the
// exact prompt or command and copies it; the member is told to ask their admin
// and the page carries neither.
//
// REGRESSION TEST for the jargon audit's F101/F102/F103/F104: red at
// origin/main, whose first screen printed "tmux new -A -s work claude", the
// whole curl setup prompt and "Or follow INSTALL.md yourself." in the top
// layer, to every viewer.
func TestNoTopLayerOfTheOnboardingPageSpellsACommand(t *testing.T) {
	s, h := homeServer(t, &homeAgents{})
	// The admin (the password operator, the person who installed muster).
	rec := homeGet(s, h, "/agents", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="`+ui.HomeEmptyID+`"`) {
		t.Fatalf("CONTROL: GET /agents = %d, want the onboarding page:\n%.400s", rec.Code, rec.Body.String())
	}
	admin := mainOf(t, rec.Body.String())
	assertNoOnboardingJargon(t, "the admin's first-run screen", admin)
	for open, code := range map[string]string{"onboarding-prompt": ui.OnboardingPrompt, "onboarding-start": ui.TmuxOneLiner} {
		if !strings.Contains(admin, `id="`+open+`-open" popovertarget="`+open+`"`) {
			t.Errorf("the admin's first-run screen has no ⓘ %q", open)
		}
		if !strings.Contains(admin, `data-info-code="">`+html.EscapeString(code)+`<`) {
			t.Errorf("the admin's first-run screen holds no ⓘ with %q", code)
		}
	}

	// A member (an invited account with nothing to open).
	req := httptest.NewRequest(http.MethodGet, "/agents", nil)
	req = req.WithContext(authz.WithPrincipal(context.Background(), authz.Principal{UserID: 7, Role: authz.RoleMember, Via: authz.ViaSession}))
	mrec := httptest.NewRecorder()
	s.handleAgentsHome(mrec, req)
	if mrec.Code != http.StatusOK || !strings.Contains(mrec.Body.String(), `id="`+ui.HomeEmptyID+`"`) {
		t.Fatalf("CONTROL: a member's GET /agents = %d, want the onboarding page:\n%.400s", mrec.Code, mrec.Body.String())
	}
	member := mainOf(t, mrec.Body.String())
	assertNoOnboardingJargon(t, "a member's first-run screen", member)
	if !strings.Contains(member, "Ask your admin to connect a computer.") {
		t.Errorf("a member's first-run screen does not say to ask their admin")
	}
	for _, absent := range []string{html.EscapeString(ui.OnboardingPrompt), html.EscapeString(ui.TmuxOneLiner), "muster-install.md", `popovertarget="onboarding-`} {
		if strings.Contains(member, absent) {
			t.Errorf("a member's first-run screen carries %q", absent)
		}
	}
}

// mainOf is a page's <main>: the onboarding page itself, without the
// navigation around it (the sidebar and the server switcher are other
// surfaces' to guard).
func mainOf(t *testing.T, page string) string {
	t.Helper()
	i, j := strings.Index(page, "<main"), strings.LastIndex(page, "</main>")
	if i < 0 || j < i {
		t.Fatalf("no <main> in:\n%.600s", page)
	}
	return page[i : j+len("</main>")]
}

// TestNoTopLayerOfTheTaskBoardsEmptyStateSpellsJargon: the task board's
// empty state, the new-task form (its Advanced fields), the edit form, and the
// page of a task with no title, folder or text say it in plain words — no
// "working directory", no path placeholder, no "repos", no "Task #<id>"
// heading, no "Edit task #<id>".
//
// REGRESSION TEST for the jargon audit's F041/U-16/F042/F043/F123: red at
// origin/main ("Tap + to capture a task for a working directory.",
// "/path/to/project (optional)", "Search repos… (optional)", "Task #1",
// "Edit task #3").
func TestNoTopLayerOfTheTaskBoardsEmptyStateSpellsJargon(t *testing.T) {
	render := func(what string, f func(*bytes.Buffer) error) string {
		t.Helper()
		var b bytes.Buffer
		if err := f(&b); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		return b.String()
	}
	surfaces := map[string]string{
		"the empty board": render("board", func(b *bytes.Buffer) error { return ui.NotesCards(ui.TasksView{}).Render(b) }),
		"the new-task form": render("new", func(b *bytes.Buffer) error {
			return ui.NotesModalBodyFor([]string{"alpha"}, false, "", nil).Render(b)
		}),
		"the edit form": render("edit", func(b *bytes.Buffer) error {
			return ui.NotesEditModalBody(ui.NoteEditView{Note: notes.Note{ID: 3, Title: "Rotate the key", Status: notes.StatusOpen}}).Render(b)
		}),
	}
	for what, markup := range surfaces {
		assertNoOnboardingJargon(t, what, markup)
	}
	// The untitled task's page: its <h1>, the one line naming it.
	page := render("detail", func(b *bytes.Buffer) error {
		return ui.RenderTaskDetail(b, ui.TaskCardView{Note: notes.Note{ID: 1, Status: notes.StatusOpen}}, ui.Features{})
	})
	i := strings.Index(page, "<h1")
	j := strings.Index(page[i:], "</h1>")
	if i < 0 || j < 0 {
		t.Fatalf("CONTROL: the task page has no <h1>")
	}
	assertNoOnboardingJargon(t, "an untitled task's heading", page[i:i+j+len("</h1>")])
}
