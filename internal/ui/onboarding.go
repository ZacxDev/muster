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
// 🔴 IT IS STATED IN FOUR PLACES AND MUST BE BYTE-IDENTICAL IN ALL OF THEM:
// here (home's onboarding page, agentsEmptyState), the fenced block at the top of
// INSTALL.md, the one in docs/onboarding.md, and the README's own copy in its
// Quickstart. TestTheOnePromptIsTheSameEverywhere pins that, so a reword in
// one place fails until all four agree. Changing it is a public-facing change: people
// will have copied the old one.
const OnboardingPrompt = "Set up muster for me: read https://raw.githubusercontent.com/ZacxDev/muster/main/INSTALL.md " +
	"and follow it step by step. Ask me everything you need in one go up front, then do the rest yourself."

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
