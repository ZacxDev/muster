package ui

import (
	"io"
	"strconv"
	"strings"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// OnboardingPrompt is THE one prompt: what a person pastes into their coding
// agent to have it set muster up (INSTALL.md), or connect another machine to
// one that runs.
//
// 🔴 IT IS STATED IN FIVE PLACES AND MUST BE BYTE-IDENTICAL IN ALL OF THEM:
// here (home's onboarding page, agentsEmptyState), the fenced block at the top of
// INSTALL.md, the one in docs/onboarding.md, the README's own copy in its
// Quickstart, and e2e/tests/onboarding.spec.ts's PROMPT.
// TestTheOnePromptIsTheSameEverywhere pins that, so a reword in one place
// fails until all five agree. Changing it is a public-facing change: people
// will have copied the old one.
//
// 🔴 IT HOLDS NO FACTS, ONLY REFERENCES (tests/claude_plugin_test.py, Drift): no
// version, port, image tag or setting, only where facts live — the guide's
// URL, the repository, `muster doctor` and `--help`. A fact in a prompt people
// have pasted somewhere cannot be corrected when the code changes.
const OnboardingPrompt = "Set up muster for me. Download its install guide with " +
	"`curl -fsSL -o muster-install.md " + InstallGuideAssetURL + "`, read that whole file and follow it step by step. " +
	PromptDownloadFailed + " " +
	"Ask me everything you need in one go up front, then do the rest yourself. " +
	"Where the guide and the machine disagree, trust `muster doctor`, each command's `--help` and the code at " +
	"https://github.com/ZacxDev/muster over memory."

// PromptDownloadFailed is the prompt's fallback: a download that fails (the
// newest release has no INSTALL.md, the network is down) is reported to the
// person, never worked around with main's copy of the guide, which installs
// whatever main describes rather than the release, or with memory.
const PromptDownloadFailed = "If that download fails, stop and tell me what it printed; never work from another copy " +
	"of the guide, such as the one on the main branch, or from memory."

// InstallGuideAssetURL is the newest release's own copy of INSTALL.md
// (scripts/release-build.sh attaches it, stamped with that release), so the
// guide an agent downloads installs the release it came with. The prompt has
// the agent DOWNLOAD it (curl -o) rather than "read" it: a fetch tool may hand
// an agent a summary of a page, and this file is a program to run.
const InstallGuideAssetURL = "https://github.com/ZacxDev/muster/releases/latest/download/INSTALL.md"

// InstallGuideURL is INSTALL.md on GitHub, for a person to read.
const InstallGuideURL = "https://github.com/ZacxDev/muster/blob/main/INSTALL.md"

// TmuxOneLiner is how a coding agent is started so muster shows it: inside
// tmux, on a machine whose muster-mirror is connected.
const TmuxOneLiner = "tmux new -A -s work claude"

// HomeEmptyID is the onboarding page's content (agentsEmptyState).
const HomeEmptyID = "agents-empty"

// OnboardingPromptSheetID and OnboardingStartSheetID are the onboarding page's
// two ⓘ details sheets: the setup prompt (OnboardingPrompt) and the command
// that starts an agent (TmuxOneLiner). Their triggers are "<id>-open".
const (
	OnboardingPromptSheetID = "onboarding-prompt"
	OnboardingStartSheetID  = "onboarding-start"
)

// Words of the onboarding page's top layer. The page says what to do in plain
// steps; the exact prompt and command are behind each step's ⓘ.
const (
	OnboardingLead         = "Your agents appear here once a computer is connected."
	OnboardingAskAdminText = "Ask your admin to connect a computer."
)

// agentsEmptyState is what home shows when there is nothing to open: no
// dispatched agent and no mirrored window (internal/api handleAgentsHome — the
// retired Agents list's empty state, now the page /agents draws when home has
// nowhere to go). On a fresh install that is either no machine connected yet
// or a connected machine with nothing in tmux, so it says how to fix both —
// never a blank screen. It offers no dispatch button: the chief starts agents
// (docs/design/chief-dispatch.md phase 2 deleted "Dispatch an agent").
//
// PLAIN STEPS UP FRONT, THE PROMPT AND THE COMMAND BEHIND AN ⓘ (the operator
// rule, info_sheet.go): the top layer names no command, URL, flag or file. The
// setup prompt and the tmux command are SETUP details, so only a viewer who
// may see them (setup: Features.Setup, an admin — usually whoever installed
// muster) gets the steps; anyone else is told to ask their admin, and the page
// carries neither the prompt nor the command (absent, not hidden).
func agentsEmptyState(setup bool) g.Node {
	head := g.Group{
		Div(Class("text-4xl"), g.Text("🧩")),
		H2(Class("text-lg font-medium text-fg2"), g.Text("No agents yet")),
		P(Class("text-sm text-muted"), g.Text(OnboardingLead)),
	}
	if !setup {
		return Div(
			ID(HomeEmptyID),
			Class("mx-auto mt-12 flex w-full max-w-xl flex-col items-center gap-3 px-1 text-center"),
			head,
			P(g.Attr("data-onboarding-ask-admin", ""), Class("text-sm text-fg2"), g.Text(OnboardingAskAdminText)),
		)
	}
	prompt := InfoSheet{
		ID: OnboardingPromptSheetID, Label: "Show the setup instructions", Heading: "Setup instructions",
		Lead: "Paste this into the coding agent on the computer you want to connect. It sets muster up there, or connects that computer to this one.",
		Code: OnboardingPrompt,
	}
	start := InfoSheet{
		ID: OnboardingStartSheetID, Label: "How to start an agent", Heading: "Start an agent",
		Lead: "In a terminal on that computer, run this. muster shows agents that run inside tmux.",
		Code: TmuxOneLiner,
	}
	return Div(
		ID(HomeEmptyID),
		Class("mx-auto mt-12 flex w-full max-w-xl flex-col items-center gap-3 px-1 text-center"),
		head,
		Ol(
			Class("flex w-full flex-col gap-2 text-left"),
			onboardingStep(1, "Connect your first computer",
				"Copy the setup instructions and paste them into the coding agent on that computer.",
				prompt,
				// The step's own action: the prompt is copied without opening
				// anything; the ⓘ beside it shows it first.
				Button(
					Type("button"),
					ID("onboarding-copy-prompt"),
					g.Attr("data-copy-text", OnboardingPrompt),
					g.Attr("onclick", "window.cgCopy && window.cgCopy(this)"),
					Class("press mt-2 inline-flex min-h-[44px] items-center rounded-lg bg-accent px-4 text-sm font-semibold text-on-accent transition hover:bg-accent/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus"),
					g.Text("Copy setup instructions"),
				),
			),
			onboardingStep(2, "Start an agent on that computer",
				"It shows up here as soon as it starts.", start, nil),
		),
		P(Class("text-sm text-muted"),
			g.Text("Or read the "),
			A(Href(InstallGuideURL), Target("_blank"), Rel("noopener"), Class("text-accent underline"), g.Text("install guide")),
			g.Text(" yourself."),
		),
	)
}

// onboardingStep is one numbered step of the onboarding page: its title and a
// plain sentence, the step's ⓘ beside them (a sibling of every control, as
// InfoSheet requires) and, optionally, an action under the sentence.
func onboardingStep(n int, title, text string, info InfoSheet, action g.Node) g.Node {
	return Li(
		Class("flex items-start gap-3 rounded-lg bg-s1 px-3 py-3 ring-1 ring-inset ring-line"),
		Span(g.Attr("aria-hidden", "true"),
			Class("mt-0.5 inline-flex h-6 w-6 shrink-0 items-center justify-center rounded-full bg-s2 text-xs font-semibold text-fg2"),
			g.Text(strconv.Itoa(n))),
		Div(
			Class("min-w-0 flex-1"),
			P(Class("text-sm font-medium text-fg"), g.Text(title)),
			P(Class("text-sm text-muted"), g.Text(text)),
			g.If(action != nil, action),
		),
		info.Node(),
	)
}

// RenderAgentsHome writes the page home draws when it has nothing to open
// (agentsEmptyState), inside the navigation (NavDocument). feat.Setup decides
// whether the viewer gets the setup steps or is told to ask their admin.
func RenderAgentsHome(w io.Writer, feat Features) error {
	var b strings.Builder
	if err := agentsEmptyState(feat.Setup).Render(&b); err != nil {
		return err
	}
	return RenderNavDocument(w, NavDocumentView{Title: "Agents", Feat: feat, Main: b.String()})
}
