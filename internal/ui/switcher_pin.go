package ui

import (
	"io"
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE SESSION SWITCHER'S PIN (operator ask: "a way to pin the current
// session, the current agent, from within the rendered view"): on a session's
// page — a dispatched agent's chat, a mirrored window's Rendered view or
// terminal — the bar's ⋯ menu (switcherMoreMenu) holds Pin / Unpin for the
// page's OWN session. It pins one of the two subjects: the agent (by id), or
// the mirrored window (host, session, window) — the two subjects internal/pins
// has; there is no other kind of pin.
//
// It is in the bar, not the header: the Rendered view's header keeps its
// title at least 150px wide on a 393px phone (e2e/tests/title-room.spec.ts,
// an operator ask), and one more 44px button there would take that from it.
// The bar is also where the Pins view it changes is. And it is in a ⋯ menu,
// not a button of its own (the operator asked for the pin to move into a
// three-dots menu), so the bar's width goes to the pills.
//
// Posted by htmx, the answer is the menu item in its new state and the bar's
// rows and counts out of band (switcherRefresh), so the Pins view and its
// count follow at once. Without script the ⋯ still opens (a popover the
// button opens itself), the form posts and the server sends the browser back
// to the session's page (back).

// SwitcherPinID is the pin's menu item (a submit button): one per page.
const SwitcherPinID = "switcher-pin"

// The ⋯ menu and its button.
const (
	switcherMoreID    = "switcher-more"
	switcherMoreBtnID = "switcher-more-btn"
	// SwitcherMoreLabel is the ⋯ button's accessible name.
	SwitcherMoreLabel = "More for this session"
)

// SwitcherPinView is the form field value that asks the pin routes to answer
// with the bar's pin item (internal/api pins.go) rather than an empty answer.
const SwitcherPinView = "switcher"

// switcherPin is the page's own session as the bar's pin item posts it.
type switcherPin struct {
	pinned bool
	fields [][2]string
	back   string
}

// newSwitcherPin is e's pin item: its subject's fields (pins.go) and the page
// to come back to after a post without script.
func newSwitcherPin(e listEntry) *switcherPin {
	if e.agent != nil {
		return &switcherPin{pinned: e.agent.Pinned, fields: agentPinFields(*e.agent), back: e.agent.PageHref()}
	}
	return &switcherPin{pinned: e.mirror.Pinned, fields: mirrorPinFields(*e.mirror), back: MirrorCardHref(*e.mirror)}
}

// switcherMoreButton is the bar's ⋯ button, at its right end, opening
// switcherMoreMenu; nothing when the page is no session (or its session is not
// among the entries): the pin is all the menu holds.
//
// IT WORKS WITHOUT SCRIPT, like the send queue's row menus
// (sendQueueRowMenu): the menu is a POPOVER the button opens itself
// (popovertarget), which closes on Escape and on a tap outside with no script,
// and its item is a real form. The bar's script (switcherJS) opens it on an
// arrow key too, closes it on Escape (focus back on the ⋯) and Tab, places it
// above the button and keeps aria-expanded.
func switcherMoreButton(s *Switcher) g.Node {
	if s == nil || s.pin == nil {
		return nil
	}
	return Button(
		ID(switcherMoreBtnID),
		Type("button"),
		g.Attr("popovertarget", switcherMoreID),
		g.Attr("aria-haspopup", "menu"),
		g.Attr("aria-expanded", "false"),
		g.Attr("aria-controls", switcherMoreID),
		g.Attr("aria-label", SwitcherMoreLabel),
		g.Attr("title", SwitcherMoreLabel),
		Class(switcherIconButtonClass),
		Span(
			Class(switcherIconFaceClass+" group-aria-expanded:bg-s3 group-aria-expanded:text-fg"),
			g.Raw(`<svg class="h-4 w-4 shrink-0" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true"><circle cx="4" cy="10" r="1.75"/><circle cx="10" cy="10" r="1.75"/><circle cx="16" cy="10" r="1.75"/></svg>`),
		),
	)
}

// switcherMoreMenu is the ⋯ menu: a popover (top layer, out of flow — opening
// it moves nothing) holding the pin item. Nothing when switcherMoreButton is.
// Its open and close are animated in CSS (web/css/input.css, #switcher-more),
// as the view menu's are.
func switcherMoreMenu(s *Switcher) g.Node {
	if s == nil || s.pin == nil {
		return nil
	}
	return Div(
		ID(switcherMoreID),
		g.Attr("popover", ""),
		g.Attr("role", "menu"),
		g.Attr("aria-label", "This session"),
		Class("m-0 min-w-[12rem] rounded-xl border-0 bg-s1 p-1 text-fg shadow-lg ring-1 ring-line"),
		SwitcherPinToggle(s.pin.pinned, s.pin.fields, s.pin.back),
	)
}

// SwitcherPinToggle is the ⋯ menu's pin item: a form posting fields (the
// subject) to PinPath — UnpinPath when pinned — with back, the page to return
// to without script. Its button is the menu item, named by its words: "Pin
// this session", or "Unpin this session" (data-pinned="true", in the accent)
// when pinned.
func SwitcherPinToggle(pinned bool, fields [][2]string, back string) g.Node {
	action, label := PinPath, "Pin this session"
	if pinned {
		action, label = UnpinPath, "Unpin this session"
	}
	nodes := []g.Node{
		Method("post"),
		Action(action),
		g.Attr("data-switcher-pin", ""),
		hx("hx-post", action),
		hx("hx-target", "this"),
		hx("hx-swap", "outerHTML"),
		Input(Type("hidden"), Name("view"), Value(SwitcherPinView)),
		Input(Type("hidden"), Name("back"), Value(back)),
	}
	for _, f := range fields {
		nodes = append(nodes, Input(Type("hidden"), Name(f[0]), Value(f[1])))
	}
	return Form(append(nodes,
		Button(
			ID(SwitcherPinID),
			Type("submit"),
			g.Attr("role", "menuitem"),
			g.Attr("data-pinned", strconv.FormatBool(pinned)),
			hx("hx-disabled-elt", "this"),
			Class("press flex min-h-[44px] w-full items-center gap-2 rounded-lg px-3 text-left text-sm font-semibold text-fg transition hover:bg-s2 focus:bg-s2 focus:outline-none disabled:opacity-60 data-[pinned=true]:text-accent"),
			g.Raw(pinIcon),
			Span(g.Text(label)),
		))...)
}

// RenderSwitcherPin writes the ⋯ menu's pin item in its new state and, when s
// is not nil, the bar's rows and counts out of band (switcherRefresh).
func RenderSwitcherPin(w io.Writer, pinned bool, fields [][2]string, back string, s *Switcher) error {
	if err := SwitcherPinToggle(pinned, fields, back).Render(w); err != nil {
		return err
	}
	if s == nil {
		return nil
	}
	return switcherRefresh(s).Render(w)
}
