package ui

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"
)

// fencedText is every ```text block in a markdown file, in order.
var fencedText = regexp.MustCompile("(?s)```text\n(.*?)\n```")

// TestTheOnePromptIsTheSameEverywhere: the prompt people copy is stated in
// four places — the Agents empty state (OnboardingPrompt), INSTALL.md's top,
// docs/onboarding.md and the README's Quickstart, which holds its own copy.
// Each file's FIRST ```text block must be the constant, byte for byte, so a
// reword anywhere fails here until every copy agrees.
func TestTheOnePromptIsTheSameEverywhere(t *testing.T) {
	if !strings.Contains(OnboardingPrompt, "https://raw.githubusercontent.com/ZacxDev/muster/main/INSTALL.md") {
		t.Fatalf("OnboardingPrompt does not name the raw INSTALL.md URL: %q", OnboardingPrompt)
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
// agents and no mirrored windows: RenderAgentsHome) draws the connect
// instructions — the tmux one-liner, the exact prompt and the guide's link —
// inside the navigation. It offers no dispatch (docs/design/chief-dispatch.md
// phase 2; TestTheDispatchUIIsGone pins the absence).
func TestTheEmptyAgentsListSaysHowToConnect(t *testing.T) {
	var empty bytes.Buffer
	if err := RenderAgentsHome(&empty, Features{}); err != nil {
		t.Fatal(err)
	}
	html := empty.String()
	if !strings.Contains(html, `id="sidebar-root"`) {
		t.Errorf("the onboarding page is not inside the nav:\n%s", html)
	}
	for _, want := range []string{`id="agents-empty"`, `id="onboarding-prompt"`} {
		if !strings.Contains(html, want) {
			t.Errorf("the empty state lacks %s:\n%s", want, html)
		}
	}
	for _, want := range []string{TmuxOneLiner, OnboardingPrompt, InstallGuideURL} {
		if !strings.Contains(html, escapeForHTML(want)) {
			t.Errorf("the empty state lacks %q:\n%s", want, html)
		}
	}
}

// escapeForHTML is how gomponents' g.Text writes s.
func escapeForHTML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;", "'", "&#39;")
	return r.Replace(s)
}
