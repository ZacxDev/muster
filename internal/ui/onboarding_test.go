package ui

import (
	"bytes"
	"fmt"
	"html"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// fencedText is every ```text block in a markdown file, in order.
var fencedText = regexp.MustCompile("(?s)```text\n(.*?)\n```")

// e2ePrompt is the PROMPT constant of e2e/tests/onboarding.spec.ts: its
// string literals, concatenated in order.
var (
	e2ePromptDecl = regexp.MustCompile(`(?s)\nconst PROMPT =\s*(.*?);\n`)
	e2eLiteral    = regexp.MustCompile(`'([^']*)'|"([^"]*)"`)
)

// TestTheOnePromptIsTheSameEverywhere: the prompt people copy is stated in
// five places — the Agents empty state (OnboardingPrompt), INSTALL.md's top,
// docs/onboarding.md, the README's Quickstart, which holds its own copy, and
// the e2e spec's PROMPT. Each file's FIRST ```text block (the spec's PROMPT)
// must be the constant, byte for byte, so a reword anywhere fails here until
// every copy agrees.
func TestTheOnePromptIsTheSameEverywhere(t *testing.T) {
	// The release's own copy, DOWNLOADED (tests/claude_plugin_test.py's Drift
	// pins that the prompt holds no facts and does not say "read <url>").
	if !strings.Contains(OnboardingPrompt, "curl -fsSL -o muster-install.md "+InstallGuideAssetURL) {
		t.Fatalf("OnboardingPrompt does not download the release's INSTALL.md: %q", OnboardingPrompt)
	}
	// The fallback (#728 round 0, Q1): a failed download is reported, never
	// worked around with another copy of the guide or from memory.
	if !strings.Contains(OnboardingPrompt, PromptDownloadFailed) || !strings.HasPrefix(PromptDownloadFailed, "If that download fails, stop") {
		t.Fatalf("OnboardingPrompt does not say what to do when the download fails: %q", OnboardingPrompt)
	}
	if got := specPrompt(t); got != OnboardingPrompt {
		t.Errorf("e2e/tests/onboarding.spec.ts's PROMPT differs from ui.OnboardingPrompt:\n spec: %q\n  Go:  %q", got, OnboardingPrompt)
	}
	for _, path := range []string{"../../INSTALL.md", "../../docs/onboarding.md", "../../README.md"} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		m := fencedText.FindSubmatch(b)
		if m == nil {
			t.Fatalf("%s has no ```text block; the prompt must be in one", path)
		}
		if got := string(m[1]); got != OnboardingPrompt {
			t.Errorf("%s's prompt differs from ui.OnboardingPrompt:\n file: %q\n  Go:  %q", path, got, OnboardingPrompt)
		}
	}
}

// TestTheEmptyAgentsListSaysHowToConnect: home with nothing to open (no
// agents and no mirrored windows: RenderAgentsHome) draws, inside the
// navigation, the connect steps in plain words for an admin (Features.Setup) —
// the exact prompt and the tmux one-liner each inside its ⓘ details sheet with
// a Copy button, the step's own Copy button copying the prompt, and the
// guide's link — and for anyone else only "ask your admin", the prompt and
// the command ABSENT from the markup. It offers no dispatch
// (docs/design/chief-dispatch.md phase 2; TestTheDispatchUIIsGone pins the
// absence).
func TestTheEmptyAgentsListSaysHowToConnect(t *testing.T) {
	admin, member := renderHome(t, true, AgentsHomeState{}), renderHome(t, false, AgentsHomeState{})
	for who, html := range map[string]string{"admin": admin, "member": member} {
		if !strings.Contains(html, `id="sidebar-root"`) || !strings.Contains(html, `id="agents-empty"`) {
			t.Errorf("%s: the onboarding page is not inside the nav:\n%s", who, html)
		}
	}
	// The expected texts are LITERALS, not ui's constants: the prompt is the
	// e2e spec's PROMPT (TestTheOnePromptIsTheSameEverywhere holds it equal to
	// the four other copies), the command is spelled here.
	prompt := specPrompt(t)
	// The admin's: each code inside its own sheet, copied whole. A sheet's Copy
	// carries no data-copy-text (#823): window.cgCopy copies the sheet's own
	// <code> as it reads, so that <code> must hold exactly the text.
	for id, code := range map[string]string{OnboardingPromptSheetID: prompt, OnboardingStartSheetID: "tmux new -A -s work claude"} {
		sheet := sheetOf(t, admin, id)
		if got := sheetCode(t, sheet); got != code {
			t.Errorf("admin: sheet %q's <code> holds %q, want %q", id, got, code)
		}
		copyTag := sheetCopyTag(t, sheet)
		if strings.Contains(copyTag, "data-copy-text") {
			t.Errorf("admin: sheet %q's Copy carries data-copy-text, so it would not copy the sheet's <code>: %s", id, copyTag)
		}
		if !strings.Contains(admin, `id="`+id+`-open" popovertarget="`+id+`"`) {
			t.Errorf("admin: no ⓘ trigger for %q", id)
		}
	}
	// The step's own Copy button copies exactly the prompt (its sheet's
	// <code>, pinned above, holds the same text).
	if got := tagAttr(t, admin, `id="onboarding-copy-prompt"`, "data-copy-text"); got != escapeForHTML(prompt) {
		t.Errorf("admin: #onboarding-copy-prompt copies %q, want the prompt %q", got, prompt)
	}
	if !strings.Contains(admin, `href="`+InstallGuideURL+`"`) || strings.Contains(admin, "data-onboarding-ask-admin") {
		t.Errorf("admin: no install guide link, or told to ask an admin:\n%s", admin)
	}
	// The member's: told to ask, and nothing of the setup in the markup.
	if !strings.Contains(member, "data-onboarding-ask-admin") || !strings.Contains(member, OnboardingAskAdminText) {
		t.Errorf("member: not told to ask their admin:\n%s", member)
	}
	for _, absent := range []string{"Set up muster for me", "muster-install.md", "tmux new", InstallGuideURL, "data-info-sheet", `id="` + OnboardingPromptSheetID} {
		if strings.Contains(member, absent) {
			t.Errorf("member: the page carries %q", absent)
		}
	}
}

// TestTheOnboardingPageLeadsWithWhatIsTrueForTheViewer: the page's first step
// follows the viewer's state.
//   - An admin with no computer connected: connect one first, then start an
//     agent (state "connect").
//   - An admin with a computer connected but nothing running: start an agent
//     first, connecting another computer second (state "start") — never sent
//     to set up again.
//   - The team's agents in the menu: said first, with a button that opens the
//     menu, for an admin and a member alike; neither page says "No agents yet".
func TestTheOnboardingPageLeadsWithWhatIsTrueForTheViewer(t *testing.T) {
	steps := func(html string) []string {
		var out []string
		for _, title := range []string{"Connect your first computer", "Start an agent on that computer", "Start an agent on your computer", "Connect another computer"} {
			if i := strings.Index(html, ">"+title+"<"); i >= 0 {
				out = append(out, fmt.Sprintf("%08d %s", i, title))
			}
		}
		sort.Strings(out)
		for i := range out {
			out[i] = out[i][9:]
		}
		return out
	}
	for _, c := range []struct {
		name  string
		setup bool
		st    AgentsHomeState
		state string
		steps []string
		team  bool
	}{
		{"admin, nothing connected", true, AgentsHomeState{}, "connect", []string{"Connect your first computer", "Start an agent on that computer"}, false},
		{"admin, a computer connected", true, AgentsHomeState{HostConnected: true}, "start", []string{"Start an agent on your computer", "Connect another computer"}, false},
		{"admin, team agents in the menu", true, AgentsHomeState{TeamEntries: true}, "connect", []string{"Connect your first computer", "Start an agent on that computer"}, true},
		{"member, team agents in the menu", false, AgentsHomeState{TeamEntries: true}, "ask-admin", nil, true},
		{"member, nothing", false, AgentsHomeState{}, "ask-admin", nil, false},
	} {
		html := renderHome(t, c.setup, c.st)
		if !strings.Contains(html, `data-onboarding-state="`+c.state+`"`) {
			t.Errorf("%s: not state %q", c.name, c.state)
		}
		if got := steps(html); strings.Join(got, "|") != strings.Join(c.steps, "|") {
			t.Errorf("%s: steps %q, want %q", c.name, got, c.steps)
		}
		team := strings.Contains(html, "data-onboarding-team") && strings.Contains(html, ">"+escapeForHTML(OnboardingTeamText)+"<") &&
			strings.Contains(html, `id="onboarding-open-menu"`)
		if team != c.team {
			t.Errorf("%s: says the team's agents are in the menu (with its button) = %v, want %v", c.name, team, c.team)
		}
		if c.team && strings.Contains(html, ">No agents yet<") {
			t.Errorf("%s: says \"No agents yet\" over a menu holding the team's agents", c.name)
		}
		if c.setup {
			if got := tagAttr(t, html, `id="onboarding-copy-prompt"`, "data-copy-text"); got != escapeForHTML(OnboardingPrompt) {
				t.Errorf("%s: #onboarding-copy-prompt copies %q", c.name, got)
			}
		}
	}
	// The menu button opens the sidebar the way its own button does.
	if html := renderHome(t, false, AgentsHomeState{TeamEntries: true}); !strings.Contains(html, `getElementById(&#39;sidebar-open&#39;)`) {
		t.Errorf("the menu button does not open the sidebar:\n%s", html)
	}
}

func renderHome(t *testing.T, setup bool, st AgentsHomeState) string {
	t.Helper()
	var b bytes.Buffer
	if err := RenderAgentsHome(&b, Features{Setup: setup}, st); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// sheetOf is the markup of the ⓘ sheet id in html, up to the next sheet.
func sheetOf(t *testing.T, html, id string) string {
	t.Helper()
	marker := `id="` + id + `" popover="auto"`
	i := strings.Index(html, marker)
	if i < 0 {
		t.Fatalf("no ⓘ sheet %q:\n%s", id, html)
	}
	sheet := html[i:]
	if j := strings.Index(sheet[len(marker):], `popover="auto"`); j >= 0 {
		sheet = sheet[:len(marker)+j]
	}
	return sheet
}

// specPrompt is the e2e spec's PROMPT literal (e2ePromptDecl).
func specPrompt(t *testing.T) string {
	t.Helper()
	spec, err := os.ReadFile("../../e2e/tests/onboarding.spec.ts")
	if err != nil {
		t.Fatal(err)
	}
	decl := e2ePromptDecl.FindSubmatch(spec)
	if decl == nil {
		t.Fatal("e2e/tests/onboarding.spec.ts has no `const PROMPT = …;`")
	}
	var got strings.Builder
	for _, m := range e2eLiteral.FindAllSubmatch(decl[1], -1) {
		got.Write(m[1])
		got.Write(m[2])
	}
	return got.String()
}

var sheetCodeRe = regexp.MustCompile(`<code [^>]*data-info-code=""[^>]*>([^<]*)</code>`)

// sheetCode is the unescaped text of the sheet's one data-info-code <code>.
func sheetCode(t *testing.T, sheet string) string {
	t.Helper()
	m := sheetCodeRe.FindAllStringSubmatch(sheet, -1)
	if len(m) != 1 {
		t.Fatalf("want exactly one data-info-code <code> in the sheet, got %d:\n%s", len(m), sheet)
	}
	return html.UnescapeString(m[0][1])
}

// sheetCopyTag is the opening tag of the sheet's one Copy (data-info-copy).
func sheetCopyTag(t *testing.T, sheet string) string {
	t.Helper()
	if n := strings.Count(sheet, `data-info-copy=""`); n != 1 {
		t.Fatalf("want exactly one data-info-copy in the sheet, got %d:\n%s", n, sheet)
	}
	i := strings.Index(sheet, `data-info-copy=""`)
	open := strings.LastIndex(sheet[:i], "<")
	return sheet[open : i+strings.Index(sheet[i:], ">")+1]
}

// tagAttr is attribute name's value on the tag holding marker.
func tagAttr(t *testing.T, html, marker, name string) string {
	t.Helper()
	i := strings.Index(html, marker)
	if i < 0 {
		t.Fatalf("no %s in:\n%s", marker, html)
	}
	open := strings.LastIndex(html[:i], "<")
	tag := html[open : i+strings.Index(html[i:], ">")]
	k := strings.Index(tag, " "+name+`="`)
	if k < 0 {
		return ""
	}
	v := tag[k+len(name)+3:]
	return v[:strings.Index(v, `"`)]
}

// escapeForHTML is how gomponents' g.Text writes s.
func escapeForHTML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;", "'", "&#39;")
	return r.Replace(s)
}
