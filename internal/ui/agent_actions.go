package ui

import (
	"io"
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// AN AGENT'S OWN PAGE CARRIES WHAT ITS CARD ON THE AGENTS LIST USED TO.
//
// The Agents list page is retired (operator decision: the sidebar and the
// session switcher's bar are how a person moves between agents), so what only
// its cards offered moved onto the agent's page (AgentDetailPage), built from
// the SAME card view (AgentCardView, internal/api's cardViewIndexed — the one
// composer), so the page and what the sidebar knows cannot disagree:
//
//   - the live status (the polled icon, with its "awaiting approval" badge),
//     the "kickoff owed" / "kickoff failed" badges and the work chip, at the
//     start of the header's second row (agentStrip);
//   - Rename, Start / Stop and Delete, in a ⋯ menu at that row's end
//     (agentActionsMenu) — 🔴 THE SECOND ROW, NOT THE FIRST: the first row is
//     the title's, and one more 44px button there would take its room (the
//     operator's rule e2e/tests/title-room.spec.ts pins: the title keeps 150px
//     on a 393px phone);
//   - the kickoff failure and its remedy, the "waiting for your github
//     approval" notice and a GitHub problem, as notices above the transcript
//     (agentNotices), where a long sentence has room.
//
// LIVE. The status icon polls (GET /ui/agents/{id}/status, every 10s), and
// when what the page was drawn with is no longer true the poll raises
// AgentsAwaitingEvent; that, a local lifecycle action's agents:changed and the
// agent.changed stream event each re-fetch the strip (AgentHeaderPath), whose
// answer also carries the menu and the notices out of band.

// The elements the agent page's actions live in.
const (
	AgentStripID   = "agent-strip"
	AgentMenuSlot  = "agent-menu-slot"
	AgentNoticesID = "agent-notices"
	AgentMenuID    = "agent-actions"
	AgentRenameID  = "agent-rename"
)

// AgentHeaderPath is the strip's refresh: GET /ui/agents/{id}/header.
func AgentHeaderPath(id int64) string { return "/ui/agents/" + strconv.FormatInt(id, 10) + "/header" }

// agentStripTrigger is the strip's hx-trigger: a local lifecycle action, the
// status poll's "what you drew is stale", and the agent stream event.
const agentStripTrigger = "agents:changed from:body, " + AgentsAwaitingEvent + " from:body, sse:agent.changed from:body delay:50ms"

// agentStrip is the header's live status: its element, refreshing itself.
func agentStrip(a AgentCardView) g.Node {
	return Span(
		ID(AgentStripID),
		hx("hx-get", AgentHeaderPath(a.ID)),
		hx("hx-trigger", agentStripTrigger),
		hx("hx-target", "this"),
		hx("hx-swap", "innerHTML"),
		Class("inline-flex shrink-0 items-center gap-2"),
		agentStripBody(a),
	)
}

// agentStripBody is what the strip holds: the polled status icon and,
// BESIDE it — never inside its polling span, which swaps its own innerHTML —
// the kickoff badges and the work chip.
func agentStripBody(a AgentCardView) g.Node {
	return g.Group{
		Span(
			ID("agent-status"),
			g.Attr("data-agent-status", a.Status),
			hx("hx-get", StatusPollPath(a.ID, a.AwaitingApproval, a.Work)),
			hx("hx-trigger", "every 10s"),
			hx("hx-target", "this"),
			hx("hx-swap", "innerHTML"),
			Class("inline-flex items-center"),
			statusIcon(a.Status),
			g.If(a.AwaitingApproval, AwaitingApprovalBadge()),
		),
		g.If(a.KickoffOwed, kickoffOwedBadge()),
		g.If(a.KickoffFailed, kickoffFailedBadge()),
		workChip(a.Work),
	}
}

// agentActionsMenu is the ⋯ menu: Rename, the agent's lifecycle action
// (Start, or Stop while it runs) and — for an agent the server lets you delete
// (not the chief, not a declared officer's instance) — a two-step Delete.
//
// A native popover (top layer: no ancestor clips it), placed beside its
// trigger by floatingMenuScript; popover="auto" closes it on an outside tap
// and on Escape. `hidden [&:popover-open]:flex`, not `flex`: any author
// display utility beats the UA's rule that hides a closed popover.
func agentActionsMenu(a AgentCardView) g.Node {
	ids := strconv.FormatInt(a.ID, 10)
	var lifecycle g.Node
	switch a.Status {
	case "running", "provisioning":
		lifecycle = agentActionItem("Stop", "/agents/"+ids+"/stop", "text-fg")
	default: // stopped/pending/error
		lifecycle = agentActionItem("Start", "/agents/"+ids+"/start", "text-accent")
	}
	deletable := !a.IsChief && !a.DeleteProtected
	return g.Group{
		Button(
			Type("button"),
			ID(AgentMenuID+"-open"),
			g.Attr("aria-label", "Agent actions"),
			g.Attr("title", "Agent actions"),
			g.Attr("aria-haspopup", "menu"),
			g.Attr("popovertarget", AgentMenuID),
			Class("press inline-flex h-11 w-11 shrink-0 cursor-pointer items-center justify-center rounded-lg text-fg2 transition hover:bg-s2"),
			g.Raw(`<svg class="h-5 w-5" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true"><circle cx="4" cy="10" r="1.6"/><circle cx="10" cy="10" r="1.6"/><circle cx="16" cy="10" r="1.6"/></svg>`),
		),
		Div(
			ID(AgentMenuID),
			g.Attr("popover", "auto"),
			g.Attr("role", "menu"),
			g.Attr("data-floating-menu", ""),
			Class("invisible hidden [&:popover-open]:flex m-0 w-48 flex-col gap-1 rounded-xl border border-line bg-s1 p-1.5 text-fg shadow-2xl ring-1 ring-black/20"),
			Button(
				Type("button"),
				g.Attr("role", "menuitem"),
				g.Attr("data-agent-action", "rename"),
				g.Attr("popovertarget", AgentRenameID),
				Class("press flex min-h-[44px] w-full items-center rounded-lg px-3 text-sm font-medium text-fg transition hover:bg-s2"),
				g.Text("Rename"),
			),
			lifecycle,
			g.If(deletable, Div(Class("my-0.5 border-t border-line"))),
			g.If(deletable, agentDeleteConfirm(ids)),
		),
	}
}

// agentActionItem is a lifecycle action (Start/Stop): it posts, and the answer
// (an empty 200 with agents:changed) re-fetches the strip and this menu.
func agentActionItem(label, postURL, tone string) g.Node {
	return Button(
		Type("button"),
		g.Attr("role", "menuitem"),
		g.Attr("data-agent-action", label),
		Class("press flex min-h-[44px] w-full items-center rounded-lg px-3 text-sm font-medium transition hover:bg-s2 disabled:opacity-60 "+tone),
		hx("hx-post", postURL),
		hx("hx-swap", "none"),
		hx("hx-disabled-elt", "this"),
		g.Text(label),
	)
}

// agentDeleteConfirm is a native-confirm-free two-step delete: a "Delete"
// whose nested <details> reveals an explicit "Confirm delete". The answer
// sends the page home (HX-Redirect): the agent it showed is going.
func agentDeleteConfirm(ids string) g.Node {
	return Details(
		Class("group/del"),
		Summary(
			g.Attr("data-agent-action", "delete"),
			Class("press flex min-h-[44px] cursor-pointer list-none items-center rounded-lg px-3 text-sm font-medium text-muted transition hover:bg-s2 hover:text-st-error-fg [&::-webkit-details-marker]:hidden"),
			g.Text("Delete"),
		),
		Button(
			Type("button"),
			g.Attr("data-agent-action", "confirm-delete"),
			Class("press mt-1 flex min-h-[44px] w-full items-center justify-center rounded-lg bg-st-error-bg px-3 text-sm font-semibold text-st-error-fg ring-1 ring-inset ring-st-error-fg/40 transition hover:bg-st-error-bg"),
			hx("hx-delete", "/agents/"+ids),
			hx("hx-swap", "none"),
			hx("hx-disabled-elt", "this"),
			g.Text("Confirm delete"),
		),
	)
}

// agentRenamePopover is the rename form, in a popover the menu's Rename opens:
// a name input (the display name; the slug is fixed) posting to
// POST /agents/{id}/name, whose answer reloads the page (HX-Refresh), so the
// header, the tab's title and the sidebar all read the new name.
func agentRenamePopover(a AgentCardView) g.Node {
	ids := strconv.FormatInt(a.ID, 10)
	return Div(
		ID(AgentRenameID),
		g.Attr("popover", "auto"),
		g.Attr("role", "dialog"),
		g.Attr("aria-label", "Rename agent"),
		Class("inset-x-0 bottom-auto top-[max(0.5rem,10vh)] mx-auto my-0 w-[min(28rem,calc(100vw-1rem))] rounded-xl border-0 bg-s1 p-0 text-fg shadow-2xl ring-1 ring-line backdrop:bg-black/40"),
		Form(
			Class("flex flex-col gap-3 p-4"),
			hx("hx-post", "/agents/"+ids+"/name"),
			hx("hx-swap", "none"),
			Label(
				Class("flex flex-col gap-1 text-xs font-medium text-muted"),
				Span(g.Text("Rename agent")),
				Input(
					Type("text"),
					Name("name"),
					Value(displayOr(a.DisplayName, a.Name)),
					g.Attr("aria-label", "Agent display name"),
					g.Attr("autocomplete", "off"),
					Placeholder(a.Name),
					Class("w-full rounded-lg border-0 bg-bg px-3 py-2 text-base text-fg ring-1 ring-inset ring-edge placeholder:text-muted focus:outline-none focus:ring-2 focus:ring-focus"),
				),
			),
			Span(Class("break-all text-[11px] text-muted"), g.Text("slug: "+a.Name+" (fixed)")),
			Div(
				Class("flex items-center gap-2"),
				Button(
					Type("submit"),
					Class("press inline-flex min-h-[44px] items-center justify-center rounded-xl bg-accent px-4 text-sm font-semibold text-on-accent transition hover:bg-accent/90 active:scale-[0.98] disabled:opacity-60"),
					g.Text("Save"),
				),
				Button(
					Type("button"),
					g.Attr("popovertarget", AgentRenameID),
					g.Attr("popovertargetaction", "hide"),
					Class("press inline-flex min-h-[44px] items-center justify-center rounded-xl px-4 text-sm font-medium text-fg2 ring-1 ring-inset ring-edge transition hover:bg-s2"),
					g.Text("Cancel"),
				),
			),
		),
	)
}

// agentNoticesBody is what the notices above the transcript hold: a failed
// kickoff with its remedy, a kickoff held for the owner's github approval, a
// GitHub App problem. Nothing for none.
func agentNoticesBody(a AgentCardView) g.Node {
	return g.Group{
		g.If(a.KickoffFailed, kickoffFailureDetail(a.KickoffFailure,
			KickoffFailedRemedy(a.KickoffResendSafe, a.Namespace, a.Name))),
		g.If(a.KickoffAwaitingGrant, kickoffAwaitingGrantNotice()),
		g.If(a.GitHubProblem != "", githubProblemDetail(a.GitHubProblem)),
	}
}

// agentNotices is the notices' element: empty, and taking no room, when there
// is nothing to say (empty:hidden).
func agentNotices(a AgentCardView) g.Node {
	return Div(ID(AgentNoticesID), Class("flex flex-col gap-2 empty:hidden"), agentNoticesBody(a))
}

// agentMenuHoldJS keeps an OPEN ⋯ menu (or the rename form it opened) from
// being replaced under the reader by the strip's out-of-band refresh: the
// refresh comes on any agent change (sse:agent.changed included), and a
// replaced popover closes — with a "Confirm delete" the reader was about to
// press. The skipped copy is not lost: when the menu closes, the strip is
// asked again (AgentsAwaitingEvent), so the menu catches up then.
var agentMenuHoldJS = `(function () {
  if (window.__musterAgentMenuHold) return;
  window.__musterAgentMenuHold = true;
  var held = false;
  function open(id) { var p = document.getElementById(id); try { return !!p && p.matches(':popover-open'); } catch (e) { return false; } }
  document.addEventListener('htmx:oobBeforeSwap', function (e) {
    var t = e.detail && e.detail.target;
    if (!t || t.id !== '` + AgentMenuSlot + `') return;
    if (open('` + AgentMenuID + `') || open('` + AgentRenameID + `')) { e.detail.shouldSwap = false; held = true; }
  });
  document.addEventListener('toggle', function (e) {
    var id = e.target && e.target.id;
    if (!held || e.newState !== 'closed' || (id !== '` + AgentMenuID + `' && id !== '` + AgentRenameID + `')) return;
    if (open('` + AgentMenuID + `') || open('` + AgentRenameID + `')) return;
    held = false;
    document.body.dispatchEvent(new CustomEvent('` + AgentsAwaitingEvent + `', { bubbles: true }));
  }, true);
})();`

// agentMenuHold is agentMenuHoldJS's tag.
func agentMenuHold() g.Node { return Script(g.Raw(agentMenuHoldJS)) }

// RenderAgentHeader writes the strip's refresh (GET AgentHeaderPath): the
// strip's body, and — out of band — the ⋯ menu (its lifecycle item follows the
// status) and the notices.
func RenderAgentHeader(w io.Writer, a AgentCardView) error {
	return g.Group{
		agentStripBody(a),
		Div(ID(AgentMenuSlot), hx("hx-swap-oob", "innerHTML"), agentActionsMenu(a)),
		Div(ID(AgentNoticesID), hx("hx-swap-oob", "innerHTML"), agentNoticesBody(a)),
	}.Render(w)
}
