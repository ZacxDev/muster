package ui

import (
	"io"
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

// agentsEmptyState is what home shows when there is nothing to open: no
// dispatched agent and no mirrored window (internal/api handleAgentsHome — the
// retired Agents list's empty state, now the page /agents draws when home has
// nowhere to go). On a fresh install that is either no machine connected yet
// or a connected machine with nothing in tmux, so it says how to fix both —
// never a blank screen. It offers no dispatch button: the chief starts agents
// (docs/design/chief-dispatch.md phase 2 deleted "Dispatch an agent").
func agentsEmptyState() g.Node {
	return Div(
		ID(HomeEmptyID),
		Class("mx-auto mt-12 flex w-full max-w-xl flex-col items-center gap-3 px-1 text-center"),
		Div(Class("text-4xl"), g.Text("🧩")),
		H2(Class("text-lg font-medium text-fg2"), g.Text("No agents yet")),
		P(Class("text-sm text-muted"),
			g.Text("Your coding agents show up in the menu when they run inside tmux on a connected machine. On a connected machine, start one:")),
		Pre(Class("w-full select-all overflow-x-auto rounded-lg bg-s2 px-3 py-2 text-left font-mono text-xs text-fg ring-1 ring-inset ring-line"),
			Code(g.Text(TmuxOneLiner))),
		P(Class("text-sm text-muted"),
			g.Text("To connect a machine (or set muster up from scratch), paste this into the coding agent on it:")),
		Pre(ID("onboarding-prompt"),
			Class("w-full select-all whitespace-pre-wrap break-words rounded-lg bg-s2 px-3 py-2 text-left font-mono text-xs text-fg ring-1 ring-inset ring-line"),
			Code(g.Text(OnboardingPrompt))),
		P(Class("text-sm text-muted"),
			g.Text("Or follow "),
			A(Href(InstallGuideURL), Target("_blank"), Rel("noopener"), Class("text-accent underline"), g.Text("INSTALL.md")),
			g.Text(" yourself."),
		),
	)
}

// RenderAgentsHome writes the page home draws when it has nothing to open
// (agentsEmptyState), inside the navigation (NavDocument).
func RenderAgentsHome(w io.Writer, feat Features) error {
	var b strings.Builder
	if err := agentsEmptyState().Render(&b); err != nil {
		return err
	}
	return RenderNavDocument(w, NavDocumentView{Title: "Agents", Feat: feat, Main: b.String()})
}
