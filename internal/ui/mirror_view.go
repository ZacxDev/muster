package ui

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/web"
)

// THE RENDERED VIEW of a mirrored tmux session whose pane runs Claude Code:
// prompts and replies as chat bubbles, each tool call as ONE collapsed row
// (tool, target, ok / error / running), thinking never shown, live while a turn
// runs. It is the DEFAULT for such a session; the Terminal is one tap away.
//
// 🔴 IT DRAWS WITH THE AGENT CHAT'S OWN COMPONENTS (chatBubbleOf, toolChip), so
// a tool row reads the same in an agent's chat and in a mirrored session — the
// rows are rendered HERE, on the server, and sent to the page as HTML; the page
// only places them.

// MirrorViewPath is a mirrored session's rendered view (its active window).
func MirrorViewPath(host, session string) string {
	return MirrorRef{Host: host, Session: session}.ViewPath()
}

// MirrorViewWSPath is that page's transcript socket, before the page adds its
// resume point.
func MirrorViewWSPath(host, session string) string {
	return MirrorRef{Host: host, Session: session}.ViewWSPath()
}

// MirrorViewView is the data the rendered view's page renders.
type MirrorViewView struct {
	// DraftUser is the signed-in user, the first part of the message box's
	// draft key (composer_draft.go).
	DraftUser int64
	Host      string
	// HostIcon is Host's icon key (HostIconFor), the one its cards draw.
	HostIcon string
	Session  string
	// Window is the tmux window id the view is of; "" = the session's active
	// window (a per-session host). With Session it is the page's ADDRESS: its
	// socket and its composer name the same window.
	Window string
	// Title is what an entry with no Label is called ("<session>:<window
	// index>"); "" = the session name.
	Title string
	// Label is the host's display name for the session ("mango:1"), "" for
	// none: the page's title. Session stays the address.
	Label string
	// Qualifier is what tells this entry apart from another listed entry of the
	// same name ("" = none) — the SAME qualifier its Agents card shows
	// (mirror.Qualified): part of the header's title and of <title>.
	Qualifier string
	// Interactive: the host advertised -interactive, so the composer is offered
	// (each send is still gated on a fresh sign-in, server-side). False draws
	// it disabled, saying why.
	Interactive bool
	// Hermes: the window runs Hermes (docs/design/hermes.md). Its composer
	// sends only when HermesSend — the host advertised hermesSend and is
	// -interactive (Phase 3) — and is drawn disabled saying why
	// (MirrorHermesNoSendNote), with no queue, otherwise.
	Hermes bool
	// HermesSend: a Hermes window whose host sends to Hermes.
	HermesSend bool
	// ViewOnly is the Kind of a runtime muster VIEWS but never sends to
	// (opencode); "" otherwise. Its composer is drawn disabled saying why
	// (MirrorViewOnlyNote), with no queue.
	ViewOnly string
	// StepUp is the "Confirm it's you" wiring (stepup_confirm.go): a send
	// refused as stale asks there — passkey or password — and retries. Unused
	// (no dialog is drawn) on a read-only host, which cannot be sent to at all.
	StepUp *StepUpView
	// Notice, NoticeOK and Nonce are the composer's (ComposerView).
	Notice   string
	NoticeOK bool
	Nonce    string
	// Dictate is the composer's (ComposerView.Dictate).
	Dictate string
	// Upload is the composer's (ComposerView.Upload).
	Upload string
	// Queue is the composer's send queue (ComposerView.Queue); nil for none.
	Queue *ComposerQueue
	// FocusHistory is the session's focus history, the newest page
	// (FocusHistoryPath); "" when the host reports no Claude session for the
	// window, which then has none. The index sheet's second tab.
	FocusHistory string
	// Asks and AsksSrc are the window's Claude sessions' OPEN attention asks
	// and their fragment URL (AgentDetailView.Asks).
	Asks    string
	AsksSrc string
	// Nav is the navigation the page draws: an overlay sidebar (sidebarIn's
	// overlay — this page is sized to the window) and its hamburger, framed
	// by the view swap or not (nav_layout.go says why).
	Nav NavChrome
	// Switcher is the session switcher below the composer (switcher.go); nil
	// for none.
	Switcher *Switcher
	// Suggest is the suggested replies' URL (ComposerView.Suggest); "" for
	// none — a Hermes window, a host with suggestions not wired.
	Suggest string
	// SuggestPaused: the viewer's suggestions are auto-paused here
	// (ComposerView.SuggestPaused).
	SuggestPaused bool
}

// MirrorReadOnlyNote is what a read-only host's composer says instead of
// accepting text.
const MirrorReadOnlyNote = "This host is read-only — its muster-mirror was not started with -interactive, so nothing can be sent from here."

// MirrorHermesNoSendNote is a Hermes window's rendered view's composer, drawn
// disabled on a host that does not send to Hermes (docs/design/hermes.md).
const MirrorHermesNoSendNote = "This host does not send to Hermes: its muster-mirror runs without -hermes-send, because Enter there can answer a prompt Hermes is showing, such as a command approval. Type in the terminal."

// MirrorViewOnlyNote is the disabled composer of a window of kind, a runtime
// muster only views (opencode, docs/design/opencode.md).
func MirrorViewOnlyNote(kind string) string {
	return "Sending to " + kind + " isn't supported yet: muster shows this session read-only. Type in the terminal."
}

// MirrorViewPage is the rendered view's document.
//
// ⚠ ITS OWN DOCUMENT, like the terminal page: a real load (the links into it
// opt out of boost), with its own socket and script.
func MirrorViewPage(v MirrorViewView) g.Node {
	ref := MirrorRef{Host: v.Host, Session: v.Session, Window: v.Window}
	composer := ComposerView{SendPath: MirrorSendPath, Host: v.Host, Session: v.Session, Window: v.Window,
		Placeholder: "Message Claude…", Aria: "Message Claude in this session", DisabledPlaceholder: "Read-only host",
		Notice: v.Notice, NoticeOK: v.NoticeOK, Nonce: v.Nonce, Dictate: v.Dictate, Upload: v.Upload, Queue: v.Queue,
		DraftKey: MirrorDraftKey(v.DraftUser, "view", v.Host, v.Session, v.Window),
		// Pinned to the visible viewport, not in flow at the bottom of the body:
		// on a phone the body's bottom can be under the browser's chrome, and
		// this page cannot be scrolled to bring it back (composerPinScript).
		Pinned: true, Switcher: v.Switcher}
	composer.Suggest, composer.SuggestPaused = v.Suggest, v.SuggestPaused
	shell := pinnedShellClass
	if v.Switcher != nil {
		shell = pinnedShellSwitcherClass
	}
	working := "Claude is working…"
	switch {
	case v.ViewOnly != "":
		working = v.ViewOnly + " is working…"
		composer.Placeholder, composer.Aria = "Message "+v.ViewOnly+"…", "Message "+v.ViewOnly+" in this session"
		composer.Disabled, composer.DisabledPlaceholder, composer.Queue = MirrorViewOnlyNote(v.ViewOnly), "Sending to "+v.ViewOnly+" is not available", nil
		composer.Suggest, composer.SuggestPaused = "", false
	case v.Hermes && !v.HermesSend:
		working = "Hermes is working…"
		composer.Placeholder, composer.Aria = "Message Hermes…", "Message Hermes in this session"
		composer.Disabled, composer.DisabledPlaceholder, composer.Queue = MirrorHermesNoSendNote, "Sending to Hermes is not available", nil
		// No suggested replies on a Hermes window (Claude Code only).
		composer.Suggest, composer.SuggestPaused = "", false
	case v.Hermes:
		working = "Hermes is working…"
		composer.Placeholder, composer.Aria = "Message Hermes…", "Message Hermes in this session"
		composer.Suggest, composer.SuggestPaused = "", false
		if !v.Interactive {
			composer.Disabled = MirrorReadOnlyNote
		}
	case !v.Interactive:
		composer.Disabled = MirrorReadOnlyNote
	}
	return Doctype(
		HTML(Class("dark"), Lang("en"),
			Head(
				Meta(Charset("utf-8")),
				Meta(Name("viewport"), Content("width=device-width, initial-scale=1, viewport-fit=cover, interactive-widget=resizes-content")),
				Meta(Name("color-scheme"), Content("dark light")),
				ThemeHead(),
				manifestLink(),
				TitleEl(g.Text(mirrorViewName(v)+" · Mirrored · muster")),
				UserMarker(v.Nav.Feat),
				Link(Rel("stylesheet"), Href(web.Asset("/static/app.css"))),
				overlayNavHead(),
				faroHead(),
			),
			Body(
				// The app shell (only #mv-log scrolls), sized to the visible
				// viewport and ending above the pinned composer.
				Class(shell+" bg-bg text-fg antialiased"),
				// The navigation: position:fixed, so it takes no room in the
				// column (nothing below moves), and an overlay at every width.
				overlayNav(v.Nav, "agents"),
				Header(
					Class("flex items-center gap-2 border-b border-line px-2 py-1.5"),
					overlayNavButton(),
					A(
						ID("mv-back"),
						Href("/agents"),
						g.Attr("aria-label", "Back to the list"),
						Class("press inline-flex h-11 w-11 shrink-0 items-center justify-center rounded-lg text-fg2 transition hover:bg-s2"),
						g.Raw(`<svg class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M15 18l-6-6 6-6"/></svg>`),
					),
					Div(
						// relative: the title button's 44px hit area
						// (mirrorTitleHitClass) is placed on this column.
						Class("relative min-w-0 flex-1"),
						mirrorViewTitle(v),
						Div(
							// relative: the Updating… chip is positioned in this row
							// (mirrorUpdatingChip), taking none of its width.
							Class("relative flex min-w-0 items-center gap-2"),
							P(ID("mv-status"), Class("truncate text-xs text-muted"), g.Attr("aria-live", "polite"), g.Text("Connecting…")),
							// The context readout (TakeContext): hidden until a turn
							// has reported usage, and again after a reset.
							Span(ID("mv-context"), g.Attr("hidden", ""), g.Attr("title", "Context used"),
								Class("shrink-0 rounded-full bg-s2 px-2 py-0.5 text-[11px] tabular-nums text-fg2 ring-1 ring-inset ring-line")),
							mirrorUpdatingChip(),
						),
					),
					chatIndexButton("mv-log"),
					viewToggle(true, ref.ViewPath(), ref.TerminalPath()),
				),
				Div(
					ID("mv-error"),
					g.Attr("role", "alert"),
					g.Attr("hidden", ""),
					Class("border-b border-line bg-danger/15 px-3 py-2 text-xs text-fg break-words"),
					Span(ID("mv-error-text")),
				),
				Main(
					ID("mv-log"),
					g.Attr("data-ws", ref.ViewWSPath()),
					// Where an opened subagent card reads its steps
					// (subagent_cards.go), before it adds `agent`.
					g.Attr("data-subagent-src", ref.SubagentPath()),
					g.Attr("aria-label", "Conversation"),
					Class("flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto overflow-x-hidden px-3 py-3"),
				),
				// The questions the window's ask hook is holding (mirror_ask.go).
				mirrorAsksRegion(ref),
				// The window's sessions' attention asks (attention_ask), answered here.
				g.If(v.AsksSrc != "", AskerAsksBlock(v.Asks, v.AsksSrc, false)),
				Div(
					ID("mv-working"),
					g.Attr("hidden", ""),
					g.Attr("role", "status"),
					Class("px-3 pb-1 text-xs text-muted"),
					Span(Class("animate-pulse"), g.Text(working)),
				),
				mirrorComposer(composer),
				g.If(v.Interactive && (!v.Hermes || v.HermesSend) && v.ViewOnly == "", confirmDialog(v.StepUp)),
				g.If(v.Interactive && (!v.Hermes || v.HermesSend) && v.ViewOnly == "", stepUpScript()),
				// The snapshot store, PARSER-BLOCKING here (SnapScriptNow): it runs
				// just before mirrorViewScript, which starts reading this window's
				// snapshot — and opens the socket — while the rest of the document
				// is still loading.
				SnapScriptNow(v.Nav.Feat),
				mirrorViewScript(),
				// Places the header's full-title popover (mirrorViewTitle) by its
				// title and keeps the title's aria-expanded in step.
				floatingMenuScript(),
				// Enables the title while it is clipped (mirrorViewTitle).
				agentTitleFitScript(),
				// The question cards' poll and submit (mirror_ask.go).
				askScript(),
				// The attention asks block's live refresh.
				AskerAsksLive(v.AsksSrc),
				// The tool groups' open/close animation (tool_groups.go).
				toolGroupScript(),
				bubbleClampScript(),
				// The subagent cards' tails and elapsed time.
				subagentCardScript(),
				chatIndexSheet(v.FocusHistory, "mirror-view:"+ref.query()),
				chatIndexScript(),
				mirrorComposerScript(),
				overlayNavScript(v.Nav),
				pwaChrome(),
			),
		),
	)
}

// viewToggle is the Rendered | Terminal switch in the header of anything that
// has both views — a mirrored Claude Code window, a claude-code agent: the
// current view (aria-current, not a link) and a link to the other. The current
// one's href is unused.
//
// The link is marked data-view-swap with the view it brings: the page's
// viewFrameScript opens that view in a frame over this one and keeps both, so
// neither view's socket is torn down by a switch (view_swap.go). It stays a
// real link — what a switch falls back to — and so does it without script.
//
// 🔴 BELOW `sm` (640px, the app's own phone/desktop breakpoint) THE SWITCH IS
// ONE 44×44 ICON BUTTON: the link to the OTHER view. An operator ask: at 393px
// the header row held three 44px buttons and this 147px switch, leaving the
// rendered view's title 70px. The current view's segment is `max-sm:hidden`
// (display:none — no width, not in the accessibility tree) and the link drops
// its word for an icon, its name ("Show terminal" / "Show rendered view")
// carried by an sr-only span that is itself display:none at `sm` and up, so a
// desktop reads exactly the two segments it did. It is CSS only: one element
// per action at every width, the same ids and the same data-view-swap link,
// no script deciding the layout.
func viewToggle(rendered bool, renderedHref, terminalHref string) g.Node {
	seg := func(id, view, label, href string, current bool) g.Node {
		cls := viewToggleSegClass
		if current {
			return Span(ID(id), g.Attr("aria-current", "page"), Class(cls+"max-sm:hidden bg-accent text-on-accent"), g.Text(label))
		}
		icon, name := viewIconTerminal, "Show terminal"
		if view == ViewRendered {
			icon, name = viewIconRendered, "Show rendered view"
		}
		return A(ID(id), Href(href), hx("hx-boost", "false"), g.Attr("data-view-swap", view), g.Attr("data-view-toggle", ""),
			// The icon's tooltip for a mouse below `sm` (a 480–639px window).
			// The name stays the link's text: "Terminal" from `sm` up.
			g.Attr("title", name),
			Class(cls+viewToggleCompactClass+" text-fg2 hover:bg-s3"),
			g.Raw(icon),
			Span(Class("sr-only sm:hidden"), g.Text(name)),
			Span(Class("max-sm:hidden"), g.Text(label)),
		)
	}
	return Nav(
		g.Attr("aria-label", "View"),
		Class("inline-flex shrink-0 overflow-hidden rounded-lg bg-s2 ring-1 ring-inset ring-line"),
		seg("view-rendered", ViewRendered, "Rendered", renderedHref, rendered),
		seg("view-terminal", ViewTerminal, "Terminal", terminalHref, !rendered),
		// Every page that draws the switch carries what makes it keep both views.
		viewFrameScript(),
	)
}

// mirrorViewTitle is the rendered view's header title: the window's name (its
// label, qualifier and session, drawn as its card draws them) and its
// "Mirrored · <host>" badge, on ONE line that clips with an ellipsis.
//
// 🔴 THE TITLE IS A BUTTON THAT OPENS THE WHOLE TITLE IN A POPOVER (an
// operator ask: "allow clicking on the ellipses to see the full title"). At
// 390px the header leaves the title 67px (measured; 51px before the switch's
// padding was cut) — a handful of characters — so a realistic title is
// clipped on the operator's main device whatever the header's padding. The
// popover is fullTitlePopover, the agent header's own: popover="auto" (top
// layer; a tap outside it or Escape closes it), placed by floatingMenuScript
// beside its trigger.
//
// The button is the line itself, not a "…" beside it (the agent header's
// titleMoreButton): a separate 44px "…" would take back more room than the
// switch's padding gave (viewToggleSegClass). It is block and truncating, so
// it draws exactly the box the <h1> drew before (no new height, no shift).
//
// Whether it is clipped is decided by agentTitleFitScript — the agent
// header's rule, not a copy of it: the <h1> is its [data-agent-title] box and
// the button both its [data-agent-title-text] and its [data-title-more].
// data-title-fit="disable" makes that script DISABLE the button while the
// title fits (a tap on a whole title opens nothing) rather than hide it, as
// it hides the agent header's "…". Without script the button stays enabled
// and the popover works natively.
//
// The popover is OUTSIDE the <h1>, so the heading's text is still only the
// title (specs read `header h1` and its [data-mirror-name]).
func mirrorViewTitle(v MirrorViewView) g.Node {
	return g.Group{
		H1(
			g.Attr("data-agent-title", ""),
			g.Attr("data-title-fit", "disable"),
			Class("min-w-0 text-sm font-semibold"),
			Button(
				ID(mirrorTitleID),
				Type("button"),
				g.Attr("data-mv-title", ""),
				g.Attr("data-agent-title-text", ""),
				g.Attr("data-title-more", ""),
				g.Attr("popovertarget", mirrorTitlePopoverID),
				g.Attr("aria-haspopup", "dialog"),
				g.Attr("aria-controls", mirrorTitlePopoverID),
				g.Attr("aria-expanded", "false"),
				Class("block w-full min-w-0 truncate rounded text-left focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus "+mirrorTitleHitClass),
				mirrorEntryTitle(v.Label, v.Title, v.Session, v.Qualifier, ""),
				Span(g.Attr("data-term-badge", ""), Class("ml-2"), mirrorBadge("Mirrored · "+v.Host, v.Host, v.HostIcon)),
			),
		),
		fullTitlePopover(mirrorTitlePopoverID, mirrorViewHeading(v)),
	}
}

// mirrorViewName is the window's name as the rendered view's header and
// <title> call it: mirrorDisplayName — entryTitle's rule, not a copy — and its
// qualifier.
func mirrorViewName(v MirrorViewView) string {
	name, _ := mirrorDisplayName(v.Label, v.Title, v.Session)
	return mirrorQualified(name, v.Qualifier)
}

// mirrorViewHeading is the header line as text: the name, then its badge's
// "Mirrored · <host>". The full-title popover shows exactly this, so when only
// the badge is clipped the popover still says what the header did not.
func mirrorViewHeading(v MirrorViewView) string {
	return mirrorViewName(v) + " · Mirrored · " + v.Host
}

// mirrorTitleHitClass gives the title button a 44px-tall hit area (a thumb
// target, like every other control in the header) WITHOUT changing the
// header's layout: the button's line is 20px, so its ::after — positioned on
// the title column (the button itself is not positioned, and its truncating
// overflow does not clip a box whose containing block is outside it) — spans
// the column's width and is 44px tall, centred on the title and status rows.
// z-10 puts it over the status row (itself positioned, so later in paint
// order). Not while the button is disabled (the title fits): the rows below
// keep their own hover and selection then.
const mirrorTitleHitClass = "after:absolute after:inset-x-0 after:top-1/2 after:z-10 after:h-11 after:-translate-y-1/2 disabled:after:hidden"

// The rendered view's header title (mirrorViewTitle) and its popover. One of
// each per document.
const (
	mirrorTitleID        = "mv-title"
	mirrorTitlePopoverID = "mv-title-full"
)

// viewToggleSegClass is each Rendered | Terminal segment's class, before its
// state's colours. 44px tall: a thumb target, like every other control in
// these headers. px-1.5 (6px a side), not the px-2.5 it was: an operator ask —
// the header's title was clipped too tightly beside the switch, and the
// segments' padding was the cheapest room to give it (16px across the two;
// each segment is still well over 44px wide).
const viewToggleSegClass = "press inline-flex min-h-[44px] items-center px-1.5 text-xs font-semibold transition "

// viewToggleCompactClass makes the switch's link a 44×44 icon button below
// `sm` (viewToggle); from `sm` up it adds nothing.
const viewToggleCompactClass = "max-sm:h-11 max-sm:w-11 max-sm:justify-center max-sm:px-0"

// The switch's phone icons (viewToggle): a prompt for the terminal, a speech
// bubble for the rendered conversation. Decorative; the link's name is its
// sr-only text.
const (
	viewIconTerminal = `<svg class="h-5 w-5 sm:hidden" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><path d="M4 17l6-5-6-5"/><path d="M12 19h8"/></svg>`
	viewIconRendered = `<svg class="h-5 w-5 sm:hidden" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/></svg>`
)

// The two views' names, as data-view-swap and the view frame spell them.
const (
	ViewRendered = "rendered"
	ViewTerminal = "terminal"
)

// MirrorOp is one DOM operation the rendered view's page applies:
//
//   - append: add HTML to the conversation — or, with Into, to the element of
//     that id (a tool-call group's body);
//   - replace: put HTML in place of the element with ID (a running tool row
//     whose result arrived; a group's summary as its count ticks up);
//   - wrap: put HTML (a tool-call group) in place of the element with ID and
//     move that element into the group's body — the second call of a run;
//   - attr: set attribute Name to Value on the element with ID (a group's state);
//   - result: a tool row's result for a call an EARLIER socket drew (Resumed):
//     the row with ID takes HTML's status and output and keeps the rest; with
//     no such row on the page, it is an append;
//   - patch: put HTML in place of the element with ID when the page has one,
//     and do nothing when it does not (a subagent card's summary: never drawn
//     anywhere but inside its card).
type MirrorOp struct {
	Op    string `json:"op"` // "append" | "replace" | "wrap" | "attr" | "result" | "patch"
	ID    string `json:"id,omitempty"`
	HTML  string `json:"html,omitempty"`
	Into  string `json:"into,omitempty"`
	Name  string `json:"name,omitempty"`
	Value string `json:"value,omitempty"`
}

// MirrorTranscript is one rendered-view socket's drawing state: the tool calls
// on screen, so a result can redraw its call's row in place.
//
// 🔴 IN MEMORY, PER SOCKET, AND BOUNDED: it is dropped with the socket and
// reset with the view, and holds at most maxDrawnCalls calls (the oldest are
// forgotten; a result for one draws as its own row).
type MirrorTranscript struct {
	calls map[string]ChatLine
	order []string
	// The tool-call grouping (tool_groups.go): each drawn row's state, the
	// group a row was drawn into, and the run the conversation currently ends
	// in — the rows since the last thing that was not a tool row, and their
	// group once there are two. Bounded with calls (forget).
	states  map[string]string
	groupOf map[string]string
	groups  map[string][]string
	run     []string
	runGrp  string
	// Resumed says the page this socket draws for already holds a conversation
	// drawn by an EARLIER socket (it reconnected with gen and offset, and no
	// Reset came). This transcript has none of that page's calls, so a result
	// for one of them is not an orphan to draw at the end: it is redrawn where
	// the page has it (a `replace`), and no run is started from it.
	//
	// 🔴 WITHOUT IT A RESUME SCRAMBLED A RUN: calls a, b, c drawn as one group,
	// the socket dropped and resumed, then results a, b, c arrived — each taken
	// for an orphan and appended by the grouping rule, so b and c were MOVED out
	// of the group into a new one at the end (the client's dedupe removes the old
	// row before an append) under the SAME group id as the first: a duplicate id
	// and a split, reordered run. The page refreshes a redrawn row's group count
	// and state itself (toolGroupScript's refresh).
	Resumed bool
	// usage is the latest turn's context use, and fresh whether it changed
	// since TakeContext last reported it.
	usage *cctranscript.Usage
	fresh bool
	// asks is the AskUserQuestion calls drawn as question cards (mirror_ask.go),
	// with their questions: a result for one redraws its card. Bounded with
	// calls (forget).
	asks map[string][]askq.Question
	// Subagents: the host reports subagents (the stream's SubagentList), so
	// an Agent call is drawn as a subagent card (subagent_cards.go) — never
	// for an older host, whose Agent calls stay plain tool rows. cards are
	// the cards drawn, by tool id, and subs the latest summary of each (a
	// summary can arrive before its card). Bounded with calls (forget).
	Subagents bool
	cards     map[string]subagentCall
	subs      map[string]mirror.Subagent
}

const maxDrawnCalls = 2048

// MirrorPromptID is a mirrored prompt's element id, from its Event.Line: the
// ONE spelling both its bubble (Ops) and its prompt-index entry
// (NewMirrorIndex) use, so the message index finds the bubble a prompt has.
func MirrorPromptID(line int64) string { return "mv-p-" + strconv.FormatInt(line, 10) }

// MirrorIndex is the prompt index as the view's socket sends it: every prompt
// the host's transcript holds (the newest it kept), each with its bubble's
// element id, and how many earlier ones the host left out.
type MirrorIndex struct {
	Items []MirrorIndexItem `json:"items"`
	More  int               `json:"more"`
}

// MirrorIndexItem is one prompt: its bubble's id and its first line.
type MirrorIndexItem struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// NewMirrorIndex is refs as the view's index.
func NewMirrorIndex(refs []cctranscript.PromptRef, more int) *MirrorIndex {
	idx := &MirrorIndex{Items: make([]MirrorIndexItem, 0, len(refs)), More: max(more, 0)}
	for _, r := range refs {
		idx.Items = append(idx.Items, MirrorIndexItem{ID: MirrorPromptID(r.Line), Text: r.Text})
	}
	return idx
}

// Reset forgets every drawn call (the view starts over).
func (t *MirrorTranscript) Reset() {
	t.calls, t.order, t.asks = nil, nil, nil
	t.Subagents, t.cards, t.subs = false, nil, nil
	t.states, t.groupOf, t.groups, t.run, t.runGrp = nil, nil, nil, nil, ""
	t.Resumed = false
	t.usage, t.fresh = nil, false
}

// endRun records that something other than a tool row was drawn: the next
// call starts a run of its own.
func (t *MirrorTranscript) endRun() { t.run, t.runGrp = nil, "" }

// forget drops a call evicted past maxDrawnCalls from every map that names it.
func (t *MirrorTranscript) forget(toolID string) {
	delete(t.calls, toolID)
	delete(t.asks, toolID)
	delete(t.cards, toolID)
	delete(t.subs, toolID)
	delete(t.states, toolID)
	if gid, ok := t.groupOf[toolID]; ok {
		delete(t.groupOf, toolID)
		rows := t.groups[gid]
		for i, r := range rows {
			if r == toolID {
				rows = append(rows[:i:i], rows[i+1:]...)
				break
			}
		}
		if len(rows) == 0 {
			delete(t.groups, gid)
		} else {
			t.groups[gid] = rows
		}
	}
}

// groupStateOps redraws group gid's summary and state from its rows' states.
func (t *MirrorTranscript) groupStateOps(gid string) []MirrorOp {
	rows := t.groups[gid]
	states := make([]string, 0, len(rows))
	for _, r := range rows {
		states = append(states, t.states[r])
	}
	st := toolGroupState(states)
	return []MirrorOp{
		{Op: "replace", ID: gid + "-s", HTML: render(toolGroupSummary(gid, len(rows), st))},
		{Op: "attr", ID: gid, Name: "data-tool-status", Value: st},
	}
}

// appendRow draws a NEW tool row at the end of the conversation, by the
// grouping rule (tool_groups.go): the first of a run is a plain row; the second
// wraps the first in a group and joins it; every later one joins the group, and
// the group's count and state are redrawn.
func (t *MirrorTranscript) appendRow(toolID, state string, row g.Node) []MirrorOp {
	id := toolRowID(toolID)
	t.states[toolID] = state
	html := render(row)
	switch len(t.run) {
	case 0:
		t.run = []string{toolID}
		return []MirrorOp{{Op: "append", ID: id, HTML: html}}
	case 1:
		first := t.run[0]
		gid := toolGroupID(first)
		t.run = append(t.run, toolID)
		t.runGrp = gid
		t.groups[gid] = []string{first, toolID}
		t.groupOf[first], t.groupOf[toolID] = gid, gid
		return []MirrorOp{
			{Op: "wrap", ID: toolRowID(first), HTML: render(toolGroup(gid, []string{t.states[first], state}))},
			{Op: "append", Into: gid + "-b", ID: id, HTML: html},
		}
	default:
		gid := t.runGrp
		t.run = append(t.run, toolID)
		t.groups[gid] = append(t.groups[gid], toolID)
		t.groupOf[toolID] = gid
		return append([]MirrorOp{{Op: "append", Into: gid + "-b", ID: id, HTML: html}}, t.groupStateOps(gid)...)
	}
}

// ContextView is the header's context-usage readout (mirrorViewBatch.Context):
// the latest turn's context, as a count ("124k context").
type ContextView struct {
	Tokens int64  `json:"tokens"`
	Label  string `json:"label"`
}

// TakeContext is the context readout when Ops has drawn a newer one since the
// last call, else nil — so a batch carries it only when it changed.
func (t *MirrorTranscript) TakeContext() *ContextView {
	if !t.fresh || t.usage == nil {
		return nil
	}
	t.fresh = false
	return &ContextView{Tokens: t.usage.Tokens, Label: tokenCount(t.usage.Tokens) + " context"}
}

// tokenCount is n tokens, short: 950, 12k, 124k, 1M, 1.2M.
func tokenCount(n int64) string {
	switch {
	case n < 1000:
		return strconv.FormatInt(n, 10)
	case n < 999_500: // past it, rounding to k would print "1000k"
		return strconv.FormatInt((n+500)/1000, 10) + "k"
	default:
		s := strconv.FormatFloat(float64(n)/1_000_000, 'f', 1, 64)
		return strings.TrimSuffix(s, ".0") + "M"
	}
}

// toolRowID is a tool row's element id: the tool id hashed, since it is the
// host's text.
func toolRowID(toolID string) string {
	sum := sha256.Sum256([]byte(toolID))
	return "mt-" + hex.EncodeToString(sum[:8])
}

func render(n g.Node) string {
	var b bytes.Buffer
	_ = n.Render(&b)
	return b.String()
}

// Ops renders events as the page's DOM operations, in order.
func (t *MirrorTranscript) Ops(events []cctranscript.Event) []MirrorOp {
	if t.calls == nil {
		t.calls = map[string]ChatLine{}
	}
	if t.states == nil {
		t.states, t.groupOf, t.groups = map[string]string{}, map[string]string{}, map[string][]string{}
	}
	var ops []MirrorOp
	for _, ev := range events {
		if ev.Usage != nil {
			t.usage, t.fresh = ev.Usage, true
		}
		switch ev.Kind {
		case cctranscript.KindPrompt:
			// The bubble's id is the prompt's place in its transcript, the id the
			// prompt index names it by (MirrorPromptID). A host that sends no Line
			// gets none here, and the message index gives the bubble a
			// page-unique one (chatIndexScript).
			t.endRun()
			ops = append(ops, MirrorOp{Op: "append", HTML: render(chatBubbleOf("user",
				g.If(ev.Line > 0, ID(MirrorPromptID(ev.Line))), g.Attr("data-md-rendered", ""), promptBody(ev.Text)))})
		case cctranscript.KindText:
			t.endRun()
			ops = append(ops, MirrorOp{Op: "append", HTML: render(chatBubbleOf("assistant", g.Attr("data-md-rendered", ""), renderMarkdown(ev.Text)))})
		case cctranscript.KindError:
			t.endRun()
			ops = append(ops, MirrorOp{Op: "append", HTML: render(Div(
				g.Attr("data-mirror-error", ""),
				Class("max-w-[85%] self-start break-words rounded-2xl bg-danger/15 px-3 py-2 text-sm text-fg ring-1 ring-inset ring-danger/40"),
				g.Text("⚠ "+ev.Text)))})
		case cctranscript.KindNotice:
			// A notice is drawn in the tool rows' frame but is not a call: it ends
			// a run like text does.
			t.endRun()
			ops = append(ops, MirrorOp{Op: "append", HTML: render(noticeRow(ev))})
		case cctranscript.KindToolCall:
			if ev.Ask != nil && len(ev.Ask.Questions) > 0 {
				ops = append(ops, t.askCall(ev)...)
				continue
			}
			if t.Subagents && mirror.IsSubagentTool(ev.Tool) {
				ops = append(ops, t.subagentCallOps(ev)...)
				continue
			}
			call := ChatLine{Kind: "tool_call", ToolID: ev.ToolID, ToolName: ev.Tool, ToolTarget: ev.Target, Content: ev.Input}
			id := toolRowID(ev.ToolID)
			if _, seen := t.calls[ev.ToolID]; seen {
				// The same call again: redrawn where it is, run and group untouched.
				t.calls[ev.ToolID] = call
				ops = append(ops, MirrorOp{Op: "replace", ID: id, HTML: render(toolChip(call, nil, id))})
				t.states[ev.ToolID] = toolStateRunning
				if gid, ok := t.groupOf[ev.ToolID]; ok {
					ops = append(ops, t.groupStateOps(gid)...)
				}
				continue
			}
			t.order = append(t.order, ev.ToolID)
			if len(t.order) > maxDrawnCalls {
				t.forget(t.order[0])
				t.order = t.order[1:]
			}
			t.calls[ev.ToolID] = call
			ops = append(ops, t.appendRow(ev.ToolID, toolStateRunning, toolChip(call, nil, id))...)
		case cctranscript.KindToolResult:
			if qs, isAsk := t.asks[ev.ToolID]; isAsk || (ev.Ask != nil && len(ev.Ask.Questions) > 0) {
				ops = append(ops, t.askResult(ev, qs, isAsk)...)
				continue
			}
			if _, isCard := t.cards[ev.ToolID]; isCard {
				ops = append(ops, t.subagentResultOps(ev)...)
				continue
			}
			result := ChatLine{Kind: "tool_result", ToolID: ev.ToolID, ToolOK: ev.OK, Content: ev.Output}
			id := toolRowID(ev.ToolID)
			call, ok := t.calls[ev.ToolID]
			if !ok && t.Resumed {
				// Its call was drawn by the socket before this one: the result is
				// put INTO the row where the page has it (a `result`: its status and
				// output; the row keeps the name, target and args only the earlier
				// socket knew), appended only if the page has none, and no run is
				// started from it — see Resumed.
				ops = append(ops, MirrorOp{Op: "result", ID: id,
					HTML: render(toolChip(ChatLine{Kind: "tool_call", ToolID: ev.ToolID}, &result, id))})
				continue
			}
			if !ok {
				// Its call fell before the backfill window: a row of its own, drawn
				// at the end like any new row (and bounded with the calls).
				t.order = append(t.order, ev.ToolID)
				if len(t.order) > maxDrawnCalls {
					t.forget(t.order[0])
					t.order = t.order[1:]
				}
				ops = append(ops, t.appendRow(ev.ToolID, toolChipState(&result),
					toolChip(ChatLine{Kind: "tool_call", ToolID: ev.ToolID}, &result, id))...)
				continue
			}
			ops = append(ops, MirrorOp{Op: "replace", ID: id, HTML: render(toolChip(call, &result, id))})
			t.states[ev.ToolID] = toolChipState(&result)
			if gid, ok := t.groupOf[ev.ToolID]; ok {
				ops = append(ops, t.groupStateOps(gid)...)
			}
		}
	}
	return ops
}

// track records toolID as drawn (bounded with every other call).
func (t *MirrorTranscript) track(toolID string) {
	if _, seen := t.calls[toolID]; seen {
		return
	}
	t.order = append(t.order, toolID)
	if len(t.order) > maxDrawnCalls {
		t.forget(t.order[0])
		t.order = t.order[1:]
	}
}

// askCall draws an AskUserQuestion call as its question card: NOT a tool row,
// so it ends a run (a card is never folded into a collapsed group) and is
// never grouped itself.
func (t *MirrorTranscript) askCall(ev cctranscript.Event) []MirrorOp {
	if t.asks == nil {
		t.asks = map[string][]askq.Question{}
	}
	id := toolRowID(ev.ToolID)
	html := render(MirrorAskCall(id, ev.Ask.Questions))
	_, seen := t.calls[ev.ToolID]
	t.track(ev.ToolID)
	t.calls[ev.ToolID] = ChatLine{Kind: "tool_call", ToolID: ev.ToolID, ToolName: ev.Tool}
	t.asks[ev.ToolID] = ev.Ask.Questions
	t.endRun()
	if seen {
		return []MirrorOp{{Op: "replace", ID: id, HTML: html}}
	}
	return []MirrorOp{{Op: "append", ID: id, HTML: html}}
}

// askResult draws an AskUserQuestion call's result as its card, read-only: the
// questions and answers the result records (Event.Ask, from the transcript's
// toolUseResult), else the call's own questions with no answer and the
// result's text (a picker dismissed in the terminal). In place of the call's
// card when it is on the page — a resumed socket's included: a "replace" of a
// row the page does not have is an append.
func (t *MirrorTranscript) askResult(ev cctranscript.Event, callQs []askq.Question, drawn bool) []MirrorOp {
	qs, answers, note := callQs, map[string]string(nil), ""
	if ev.Ask != nil && len(ev.Ask.Questions) > 0 {
		qs, answers = ev.Ask.Questions, ev.Ask.Answers
	}
	if len(answers) == 0 {
		note = ev.Output
	}
	id := toolRowID(ev.ToolID)
	html := render(MirrorAskResult(id, qs, answers, note))
	if drawn || t.Resumed {
		return []MirrorOp{{Op: "replace", ID: id, HTML: html}}
	}
	if t.asks == nil {
		t.asks = map[string][]askq.Question{}
	}
	t.track(ev.ToolID)
	t.calls[ev.ToolID] = ChatLine{Kind: "tool_call", ToolID: ev.ToolID, ToolName: askq.ToolName}
	t.asks[ev.ToolID] = qs
	t.endRun()
	return []MirrorOp{{Op: "append", ID: id, HTML: html}}
}

// noticeRow draws a harness notice (cctranscript.KindNotice) as a compact
// collapsed row in the tool rows' frame — never as the human's bubble: an icon
// for its source, its one-line summary, and a task's status; expanding shows
// the detail (a task's result, a local command's output).
//
// ⚠ ONLY THE MIRRORED VIEW DRAWS ONE. A dispatched agent's chat is muster's own
// persisted messages plus ccd's reply stream, which carries a turn's text and
// tool calls and nothing the harness injects, so no notice ever reaches it.
func noticeRow(ev cctranscript.Event) g.Node {
	icon, label := "🔔", "task"
	switch ev.Notice {
	case cctranscript.NoticeCommandOutput:
		icon, label = "▸", "output"
	case cctranscript.NoticeHermes:
		icon, label = "☤", "hermes"
	case cctranscript.NoticeOpenCode:
		icon, label = "◇", "opencode"
	}
	// The status as the harness wrote it, drawn the tool rows' way: a known
	// good one ✓, a known bad one ✗, anything else by name.
	var status g.Node
	switch ev.Status {
	case "":
	case "completed", "success":
		status = Span(Class("ml-auto shrink-0 font-semibold text-accent"), g.Attr("aria-label", "status: "+ev.Status), g.Text("✓"))
	case "failed", "error":
		status = Span(Class("ml-auto shrink-0 font-semibold text-st-error-fg"), g.Attr("aria-label", "status: "+ev.Status), g.Text("✗"))
	default:
		status = Span(Class("ml-auto shrink-0 text-muted"), g.Text(ev.Status))
	}
	var body g.Node
	if strings.TrimSpace(ev.Output) != "" && ev.Output != ev.Text {
		body = Div(Class("flex flex-col gap-1 px-3 pb-2 pt-0"), toolPre("detail", ev.Output))
	}
	return Details(
		g.Attr("data-notice-row", ev.Notice),
		g.If(ev.Status != "", g.Attr("data-notice-status", ev.Status)),
		Class("w-full max-w-[92%] min-w-0 self-start rounded-xl bg-bg/50 text-xs ring-1 ring-inset ring-line"),
		Summary(
			Class("flex min-h-[44px] min-w-0 cursor-pointer select-none items-center gap-2 px-3 py-2 font-medium text-fg2 hover:text-fg"),
			Span(Class("shrink-0"), g.Attr("aria-hidden", "true"), g.Text(icon)),
			Span(Class("shrink-0 font-mono text-muted"), g.Text(label)),
			Span(g.Attr("data-notice-text", ""), Class("min-w-0 truncate"), g.Text(ev.Text)),
			status,
		),
		body,
	)
}

// mirrorViewScript connects the transcript socket and applies its batches, and
// reconnects resuming from the last offset. The composer is its own script
// (mirrorComposerScript), shared with the Terminal view.
//
// Every node it inserts is server-rendered HTML (MirrorTranscript.Ops), whose
// text was escaped by gomponents and renderMarkdown; the script adds none of
// its own from transcript text.
func mirrorViewScript() g.Node {
	return Script(g.Raw(`
(function () {
  var log = document.getElementById('mv-log');
  if (!log) return;
  var statusEl = document.getElementById('mv-status');
  var errEl = document.getElementById('mv-error');
  var errText = document.getElementById('mv-error-text');
  var working = document.getElementById('mv-working');
  var ctxEl = document.getElementById('mv-context');
  var gen = '', offset = 0, retry = 0, closed = false, sock = null;
  // The snapshot (web/static/mirror-snap.js): what this window's view drew,
  // replayed at open, then resumed from its offset. snapRef stays null — no
  // snapshot is read or written — when the store is unavailable. Set in
  // start(); the store's script ran just before this one (SnapScriptNow).
  var snap = null, snapRef = null;
  // 🔴 THE SOCKET OPENS BEFORE THE DOCUMENT HAS FINISHED LOADING; WHAT IT SENDS
  // IS DRAWN ONLY ONCE IT HAS. The open used to wait for DOMContentLoaded (the
  // deferred scripts), then for the snapshot read, then for the snapshot's
  // paint, and only then create the socket — so the host's whole round trip
  // started after all of it. Now start() runs as this script is parsed: the
  // snapshot is read at once, the socket is created the moment its resume
  // point is known, and the paint and every socket event wait for parsed
  // (DOMContentLoaded), queued in order, so every script that decorates the
  // conversation is still listening before anything is drawn.
  var parsed = document.readyState !== 'loading', queued = [];
  var chip = document.getElementById('mv-updating');
  // opLog is the batches drawn since the last reset (what a snapshot stores);
  // provisional: the page shows a snapshot no socket has confirmed yet.
  // opBytes is opLog's size (JSON characters), held to twice the store's
  // bound in memory; firstAt is when this conversation was last read in full
  // from the host (a reset), which ages the snapshot (mirror-snap.js).
  var opLog = [], opBytes = 0, firstAt = 0, provisional = false, dirty = false, saveTimer = 0, chipTimer = 0;
  // The open, measured (faro.go's musterOpen; absent while telemetry is off):
  // opening is the socket's open until its first batch, lastOpen the one a
  // later disconnect belongs to, and endReason the closed word the server
  // gave for ending the stream, if it gave one.
  var opening = null, lastOpen = null, endReason = '';
  // drawnFor is the open whose first batch has been drawn.
  var drawnFor = null;

  function showChip(on) {
    if (!chip) return;
    chip.classList.toggle('invisible', !on);
    chip.setAttribute('aria-hidden', on ? 'false' : 'true');
  }
  function remember(m, size) {
    if (!snapRef) return;
    if (m.reset) { opLog = []; opBytes = 0; firstAt = Date.now(); }
    if (!firstAt) firstAt = Date.now();
    if (snap.worth(m)) { opLog.push(m); opBytes += size; }
    if (opBytes > 2 * snap.maxBytes) { var b = snap.bound(opLog, snap.maxBytes); opLog = b.batches; opBytes = b.bytes; }
    dirty = true;
    clearTimeout(saveTimer);
    saveTimer = setTimeout(save, 800);
  }
  function save() {
    clearTimeout(saveTimer);
    if (!snapRef || !dirty || provisional || !gen) return;
    dirty = false;
    snap.put(snapRef, { gen: gen, offset: offset, batches: opLog, firstAt: firstAt });
  }
  // settled: the socket answered, so what is on the page is current.
  function settled() {
    provisional = false;
    clearTimeout(chipTimer);
    showChip(false);
    log.removeAttribute('data-snapshot');
  }
  // discard: a resume that failed or was refused before its first batch. What
  // the snapshot painted is taken off the page and out of storage, and the
  // next socket starts from the beginning.
  function discard() {
    settled();
    log.innerHTML = '';
    window.muMirrorIndex = null;
    ctxEl.hidden = true; ctxEl.textContent = '';
    document.dispatchEvent(new CustomEvent('muster:chat-index'));
    gen = ''; offset = 0; opLog = []; opBytes = 0; firstAt = 0; dirty = false;
    if (snap && snapRef) snap.drop(snapRef);
  }
  // paint draws a snapshot through apply() — the path a live batch takes —
  // and leaves the page provisional until the socket speaks.
  function paint(rec) {
    if (opening) { opening.mark('snapshot_paint_start'); opening.count('snapshot_batches', rec.batches.length); }
    // 🔴 ONE LAYOUT FOR THE WHOLE SNAPSHOT, NOT ONE PER BATCH. apply() reads
    // the log's scroll position before a batch and writes it after, which
    // forces a layout of everything drawn so far; a snapshot is tens of
    // batches, so that was a layout per batch over a growing conversation
    // (measured: 170–200 ms of a throttled open for 50 batches). A snapshot
    // opens at its end, so the batches are drawn without reading, and the log
    // is scrolled once.
    bulk = true;
    try { rec.batches.forEach(function (b) { apply(b); }); } finally { bulk = false; }
    log.scrollTop = log.scrollHeight;
    if (opening) opening.mark('snapshot_painted');
    gen = rec.gen; offset = rec.offset;
    opLog = rec.batches.slice(); opBytes = rec.bytes || 0; firstAt = rec.firstAt;
    // A snapshot's last "busy" is not a claim about now.
    working.hidden = true;
    provisional = true;
    log.setAttribute('data-snapshot', '');
    // Catching up past a second shows "Updating…" in its reserved slot.
    chipTimer = setTimeout(function () { if (provisional) showChip(true); }, 1000);
  }

  function showError(msg) { errText.textContent = msg; errEl.hidden = false; }
  function clearError() { errEl.hidden = true; errText.textContent = ''; }
  function nearBottom() { return log.scrollHeight - log.scrollTop - log.clientHeight < 80; }
  function frag(html) { var t = document.createElement('template'); t.innerHTML = html; return t.content; }

  var bulk = false;
  function apply(m) {
    var stick = bulk ? false : (nearBottom() || m.reset);
    // The prompt index (chatIndexScript reads it): a Reset starts a transcript
    // over, so its index is gone until the host sends the new one — and a host
    // that predates it never does.
    if (m.reset) { log.innerHTML = ''; window.muMirrorIndex = null; ctxEl.hidden = true; ctxEl.textContent = ''; }
    if (m.index) { window.muMirrorIndex = m.index; }
    if (m.reset || m.index) document.dispatchEvent(new CustomEvent('muster:chat-index'));
    // Context usage: only a batch whose usage changed carries it; an older
    // muster or host sends none and the readout stays hidden.
    if (m.context && m.context.label) {
      ctxEl.textContent = m.context.label;
      ctxEl.hidden = false;
    }
    (m.ops || []).forEach(function (op) {
      // A group's state (tool_groups.go): one attribute, on the group.
      if (op.op === 'attr') {
        var at = op.id && document.getElementById(op.id);
        if (at) at.setAttribute(op.name, op.value);
        return;
      }
      // The second call of a run: the group takes the first row's place and the
      // row moves into its body (the call itself is the next op, appended into it).
      if (op.op === 'wrap' && op.id) {
        var first = document.getElementById(op.id);
        if (first) {
          var grp = frag(op.html).firstElementChild;
          first.replaceWith(grp);
          grp.querySelector('[data-tool-group-body]').appendChild(first);
          return;
        }
      }
      // Only where it is: a subagent card's summary (subagent_cards.go).
      if (op.op === 'patch') {
        var pt = op.id && document.getElementById(op.id);
        if (pt) pt.replaceWith(frag(op.html));
        return;
      }
      if (op.op === 'result' && op.id) {
        var row = document.getElementById(op.id);
        if (row) {
          var got = frag(op.html).firstElementChild;
          row.setAttribute('data-tool-status', got.getAttribute('data-tool-status') || '');
          var st = row.querySelector(':scope > summary [aria-label^="status:"]');
          var nst = got.querySelector(':scope > summary [aria-label^="status:"]');
          if (st && nst) st.replaceWith(nst);
          var body = row.querySelector(':scope > div'), nbody = got.querySelector(':scope > div');
          if (body && nbody) while (nbody.firstChild) body.appendChild(nbody.firstChild);
          if (window.musterToolGroups) window.musterToolGroups.refresh(row);
          return;
        }
      }
      if (op.op === 'replace' && op.id) {
        var old = document.getElementById(op.id);
        if (old) {
          var f = frag(op.html), el = f.firstElementChild;
          if (el && old.open) el.open = true; // keep a row the reader expanded
          old.replaceWith(f);
          // A row redrawn inside a group: the group's count and state follow it
          // (a resumed socket cannot redraw a group it never drew).
          if (el && window.musterToolGroups) window.musterToolGroups.refresh(el);
          return;
        }
      }
      if (op.id) { var dup = document.getElementById(op.id); if (dup) dup.remove(); }
      // Into a group's body when it is on the page; else at the end, as before.
      ((op.into && document.getElementById(op.into)) || log).appendChild(frag(op.html));
    });
    if (m.gen) gen = m.gen;
    if (typeof m.offset === 'number') offset = m.offset;
    working.hidden = !m.busy;
    // A turn is running: no suggestion is for it (suggest_row.go).
    if (m.busy && window.musterSuggest) window.musterSuggest({ state: 'busy' });
    if (!log.firstElementChild) {
      log.innerHTML = '<p data-mirror-empty class="m-auto text-sm text-muted">No conversation yet.</p>';
    } else {
      var empty = log.querySelector('[data-mirror-empty]'); if (empty && log.children.length > 1) empty.remove();
    }
    if (stick) log.scrollTop = log.scrollHeight;
  }

  // painted calls fn once the browser has painted what was just drawn: the
  // next animation frame, then a task after it (the frame's paint runs
  // between the two).
  function nextTask(fn) {
    if (typeof MessageChannel === 'undefined') { setTimeout(fn, 0); return; }
    var ch = new MessageChannel();
    ch.port1.onmessage = function () { fn(); };
    ch.port2.postMessage(0);
  }
  function painted(fn) { requestAnimationFrame(function () { nextTask(fn); }); }
  // A socket's events, in order: as they come once the document is parsed,
  // queued until then. An event of a socket that has since been replaced is
  // dropped.
  function deliver(s, fn) {
    if (parsed) { fn(); return; }
    queued.push(function () { if (s === sock) fn(); });
  }
  function connect() {
    // Left (pagehide): a pending retry opens nothing.
    if (closed) return;
    var base = log.getAttribute('data-ws');
    // A reconnect is an open of its own (the page's first began in start()).
    if (!opening && window.musterOpen) opening = window.musterOpen.begin('mirror_view', { resume: !!gen });
    var tp = opening ? opening.tp() : '';
    endReason = '';
    var url = (location.protocol === 'https:' ? 'wss:' : 'ws:') + '//' + location.host + base +
      (gen ? '&gen=' + encodeURIComponent(gen) + '&offset=' + offset : '') + (tp ? '&tp=' + encodeURIComponent(tp) : '');
    statusEl.textContent = 'Connecting…';
    var s = sock = new WebSocket(url);
    if (opening) opening.mark('socket_created');
    var first = true;
    s.onopen = function () { retry = 0; if (opening) opening.mark('stream_connected'); };
    s.onmessage = function (ev) {
      if (first && opening) { first = false; opening.mark('first_frame'); }
      deliver(s, function () { onMessage(ev); });
    };
    s.onclose = function (ev) { deliver(s, function () { onClose(ev); }); };
  }
  function onMessage(ev) {
      var m; try { m = JSON.parse(ev.data); } catch (e) { return; }
      if (m.type === 'batch') {
        // The open's FIRST batch (a later one before the paint below is not).
        var o = opening && opening !== drawnFor ? (drawnFor = opening) : null;
        if (o) { o.mark('batch_insert_start'); o.count('first_batch_ops', (m.ops || []).length); }
        if (provisional) settled(); clearError(); statusEl.textContent = 'Live'; apply(m); remember(m, ev.data.length); caughtUp();
        if (o) { o.mark('batch_insert_end'); lastOpen = o; }
        // The open ends when the browser has PAINTED the batch, not when the
        // script has inserted it: style, layout and paint of what was
        // inserted come after, and on a long conversation they are a large
        // part of it. A disconnect before then ends it with its reason.
        if (o || !usableSent) painted(function () {
          if (o && opening === o) { o.mark('first_paint'); o.mark('usable'); o.end('ok'); opening = null; }
          viewUsable();
        });
      }
      else if (m.type === 'error') {
        endReason = m.reason || 'error';
        if (provisional) discard(); showError(m.message || 'The view ended.'); caughtUp();
      }
      // The window's questions changed (mirror_ask.go): read them now.
      else if (m.type === 'asks') { if (window.musterAskPoll) window.musterAskPoll(); }
      // Suggested replies (suggest_row.go): pending, ready or none.
      else if (m.type === 'suggest') { if (window.musterSuggest) window.musterSuggest(m); }
  }
  function onClose(ev) {
      statusEl.textContent = 'Disconnected — reconnecting…';
      working.hidden = true;
      if (closed) return;
      // Why, reported (faro.go): the server's closed word when it said one,
      // else the socket's close code; and whether it happened before the view
      // had drawn anything (opening) or while it was live.
      var o = opening || lastOpen, why = endReason || ('closed_' + (ev && ev.code));
      if (opening) opening.end(why);
      if (o) o.disconnected(why, { phase: opening ? 'opening' : 'live', code: ev && ev.code, retry: retry });
      opening = null;
      // Closed before its first batch (refused, signed out, gone): the
      // snapshot is not shown as current.
      if (provisional) discard();
      retry = Math.min(retry + 1, 6);
      setTimeout(connect, 500 * Math.pow(2, retry - 1));
  }
  // viewUsable tells the page's other scripts that the open is over (the
  // snapshot store's warming waits for it on this page: mirror-snap.js).
  var usableSent = false;
  function viewUsable() {
    if (usableSent) return;
    usableSent = true;
    document.dispatchEvent(new CustomEvent('muster:view-usable'));
  }
  // The log's box shrinks when the keyboard opens or the composer grows: a
  // reader at the end stays at the end, rather than the last message sliding
  // under the box's bottom edge.
  var atEnd = true;
  log.addEventListener('scroll', function () { atEnd = nearBottom(); }, { passive: true });
  if (typeof ResizeObserver !== 'undefined') {
    new ResizeObserver(function () { if (atEnd) log.scrollTop = log.scrollHeight; }).observe(log);
  }
  // 🔴 NO STORAGE WRITE MAY BE IN FLIGHT AS THE PAGE GOES INTO THE BACK/FORWARD
  // CACHE. A save is a chain of storage transactions, and one still running
  // when the page is frozen there evicts it (measured, Chromium 153: Back
  // reloaded the view, CDP reason IgnoreEventAndEvict, whenever the page was
  // left within the save delay of a batch; with the store's put stubbed out it
  // was restored). So a page that MAY be kept (pagehide persisted) saves
  // nothing as it goes; one that is going for good saves, as before. Skipping
  // it costs only freshness: the stored snapshot is the last one saved, whole
  // at its own offset, and a later open resumes from there.
  var leaving = false;
  window.addEventListener('pagehide', function (e) { closed = true; leaving = !!e.persisted; if (!e.persisted) save(); if (sock) sock.close(); });
  // Back and Forward (restore.go): the pagehide above closed the socket; a
  // restore opens it again, resuming from the offset the page already drew,
  // so nothing is painted twice. Caught up is the socket's first answer.
  var restoring = null;
  function caughtUp() { if (restoring) { var r = restoring; restoring = null; r(); } }
  ` + restoreJoinJS(`{
    show: function () {
      closed = false; leaving = false; retry = 0;
      return new Promise(function (resolve) {
        restoring = resolve;
        if (!sock || sock.readyState > 1) connect();
      });
    }
  }`) + `
  // Leaving fires visibilitychange BEFORE pagehide, so the hidden save waits
  // a quarter second and is skipped once pagehide has said the page may be kept:
  // a tab that was only switched away gets no pagehide and saves then.
  document.addEventListener('visibilitychange', function () {
    if (document.hidden) setTimeout(function () { if (!leaving) save(); }, 250);
  });
  // Open: read this window's snapshot, if there is one this user may see,
  // and connect — resuming from its offset — as soon as the read is done.
  // Storage that is slow or absent costs at most readWaitMs, then the socket
  // opens without it. The snapshot is painted once the document is parsed
  // (whenParsed), before any socket event is applied.
  var toPaint = null;
  function start() {
    // The page's first open begins HERE, before the snapshot read: faro-init
    // loads the telemetry once no open is running (musterOpen.quiet), and an
    // open begun only when the read had ended could be missed by that.
    if (window.musterOpen) {
      var o = opening = window.musterOpen.begin('mirror_view');
      // The document's milestones, as they come.
      var nav = function () {
        try {
          var n = performance.getEntriesByType('navigation')[0];
          if (n && n.domContentLoadedEventStart > 0) o.markAt('dom_content_loaded', n.domContentLoadedEventStart);
          if (n && n.loadEventStart > 0) o.markAt('load', n.loadEventStart);
        } catch (e) {}
      };
      if (document.readyState === 'complete') nav(); else window.addEventListener('load', nav);
      document.addEventListener('DOMContentLoaded', nav);
    }
    snap = window.musterSnap && window.musterSnap.enabled ? window.musterSnap : null;
    snapRef = snap ? snap.refOf(log.getAttribute('data-ws')) : null;
    if (!snapRef) { if (opening) { opening.attr('snapshot', 'off'); opening.flag('resume', false); } connect(); return; }
    var started = false;
    if (opening) opening.mark('snapshot_read_start');
    function go(rec, outcome) {
      if (started) return;
      started = true;
      // The read's outcome: hit, miss or timeout (a closed word).
      if (opening) { opening.mark('snapshot_read_end'); opening.attr('snapshot', outcome); opening.flag('resume', !!rec); }
      // The resume point is known: the socket opens now, and the snapshot is
      // painted in this same task when the document is parsed (no socket
      // event can come in between), else once it is.
      if (rec) { gen = rec.gen; offset = rec.offset; }
      connect();
      if (rec) { if (parsed) paintSnapshot(rec); else toPaint = rec; }
    }
    var wait = setTimeout(function () { go(null, 'timeout'); }, snap.readWaitMs);
    snap.get(snapRef).then(function (rec) { clearTimeout(wait); go(rec, rec ? 'hit' : 'miss'); }, function () { clearTimeout(wait); go(null, 'miss'); });
  }
  // paintSnapshot paints rec, which the socket is already resuming from. A
  // snapshot that cannot be painted is discarded, and the socket replaced by
  // one from the beginning.
  function paintSnapshot(rec) {
    try { paint(rec); } catch (e) {
      discard();
      var old = sock;
      sock = null;
      if (old) { old.onclose = null; old.onmessage = null; try { old.close(); } catch (x) {} }
      connect();
    }
  }
  function whenParsed() {
    if (toPaint) { var rec = toPaint; toPaint = null; paintSnapshot(rec); }
    parsed = true;
    queued.splice(0).forEach(function (fn) { fn(); });
  }
  start();
  if (parsed) whenParsed(); else document.addEventListener('DOMContentLoaded', whenParsed);

})();
`))
}
