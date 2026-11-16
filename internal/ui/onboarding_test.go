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
	if got.String() != OnboardingPrompt {
		t.Errorf("e2e/tests/onboarding.spec.ts's PROMPT differs from ui.OnboardingPrompt:\n spec: %q\n  Go:  %q", got.String(), OnboardingPrompt)
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
