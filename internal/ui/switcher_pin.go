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
// terminal — the bar holds a pin toggle for the page's OWN session, beside its
// search. It pins what a card's toggle pins: the agent (by id), or the
// mirrored window (host, session, window) — the two subjects internal/pins
// has; there is no other kind of pin.
//
// It is in the bar, not the header: the Rendered view's header keeps its
// title at least 150px wide on a 393px phone (e2e/tests/title-room.spec.ts,
// an operator ask), and one more 44px button there would take that from it.
// The bar is also where the Pins view it changes is.
//
// Posted by htmx, the answer is the toggle in its new state and the bar's
// rows and counts out of band (switcherRefresh), so the Pins view and its
// count follow at once. Without script the form posts and the server sends
// the browser back to the session's page (back).

// SwitcherPinID is the bar's pin toggle button.
const SwitcherPinID = "switcher-pin"

// SwitcherPinView is the form field value that asks the pin routes to answer
// with the bar's toggle (internal/api pins.go) rather than the Agents list.
const SwitcherPinView = "switcher"

// switcherPin is the page's own session as the bar's pin toggle posts it.
type switcherPin struct {
	pinned bool
	fields [][2]string
	back   string
}

// newSwitcherPin is e's pin toggle: its subject's fields (pinForm's) and the
// page to come back to after a post without script.
func newSwitcherPin(e listEntry) *switcherPin {
	if e.agent != nil {
		return &switcherPin{pinned: e.agent.Pinned, fields: agentPinFields(*e.agent), back: e.agent.PageHref()}
	}
	return &switcherPin{pinned: e.mirror.Pinned, fields: mirrorPinFields(*e.mirror), back: MirrorCardHref(*e.mirror)}
}

// switcherPinToggle is the bar's pin toggle for s's page; nothing when the
// page is no session (the Agents list), or the entries are not known yet.
func switcherPinToggle(s *Switcher) g.Node {
	if s == nil || s.unknown || s.pin == nil {
		return nil
	}
	return SwitcherPinToggle(s.pin.pinned, s.pin.fields, s.pin.back)
}

// SwitcherPinToggle is the bar's pin toggle: a form posting fields (the
// subject) to PinPath — UnpinPath when pinned — with back, the page to return
// to without script. Pressed (aria-pressed, the accent) when pinned. A 44px
// target around a 32px face, like the search beside it, in the bar's fixed
// h-11 row, so it moves nothing.
func SwitcherPinToggle(pinned bool, fields [][2]string, back string) g.Node {
	action, label := PinPath, "Pin this session"
	if pinned {
		action, label = UnpinPath, "Unpin this session"
	}
	nodes := []g.Node{
		Method("post"),
		Action(action),
		g.Attr("data-switcher-pin", ""),
		Class("shrink-0"),
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
			g.Attr("aria-pressed", strconv.FormatBool(pinned)),
			g.Attr("aria-label", label),
			g.Attr("title", label),
			hx("hx-disabled-elt", "this"),
			Class("press group inline-flex h-11 w-11 items-center justify-center disabled:opacity-60"),
			Span(
				Class("inline-flex h-8 w-8 items-center justify-center rounded-lg bg-s2 text-fg2 ring-1 ring-inset ring-line transition group-hover:bg-s3 group-aria-pressed:bg-accent/15 group-aria-pressed:text-accent group-aria-pressed:ring-accent/40"),
				g.Raw(pinIcon),
			),
		))...)
}

// RenderSwitcherPin writes the bar's pin toggle in its new state and, when s
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
