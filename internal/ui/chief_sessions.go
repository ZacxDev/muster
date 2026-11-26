package ui

import (
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE CHIEF'S SESSIONS ON MORE THAN ONE RUNTIME (internal/api/session_runtime.go):
// a session is started on a runtime and a model, fixed for its life — from the
// Sessions tab's "+ New session" menu (chiefNewChatChip), or by "Continue in a
// new session" (chiefContinuePicker, runtimeModelForms below) — its row's model
// tag names the model, and one chat is the PRIMARY (where plugin messages and
// `chief ask` go), made so from another row's ⋯ menu (Make primary).
//
// The panel's own thread list, which held a "New session" runtime picker, a
// runtime line on each row and a "Make default" button beside it, is gone: the
// Sessions tab is the one list of the chief's chats (an operator decision), and
// its "+" menu offers the same runtimes and models (ChiefChatBar.NewChatModels
// and .OpenRouter come from the same gate, internal/api chiefChatBarFor).

// sessionDefaultAttr marks the primary chat's ★ (and the switcher's badge).
const sessionDefaultAttr = "data-session-default"

// SessionRuntimeView names a runtime the picker offers.
type SessionRuntimeView struct {
	Runtime string // "claude-code"
	Label   string
}

// sessionDefaultBadge marks the default thread.
func sessionDefaultBadge(sess SessionView) g.Node {
	if !sess.IsDefault {
		return nil
	}
	return Span(
		g.Attr(sessionDefaultAttr, ""),
		g.Attr("title", "The primary chat: every delivery to the chief — plugin mail, hand-backs, notes, chief ask — goes here"),
		Class("shrink-0 rounded px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-accent ring-1 ring-inset ring-accent/50"),
		g.Text("Primary"),
	)
}

// runtimeModelForms is THE runtime + model form, one per offered runtime:
// Continue in a new session's (chiefContinuePicker). Each form posts runtime +
// model to path on surface (chiefChatPost: HX-Redirect on the page, the panel
// body re-rendered in the panel); formAttr (valued with the runtime) marks it,
// button labels its submit, and extra adds attributes to the form (a confirm,
// say). The form's controls are disabled while its request runs
// (hx-disabled-elt).
//
// The runtimes and models are the ones the "+ New session" menu offers
// (internal/api chiefChatBarFor fills both from one gate), so a session is
// continued on exactly what New session would start.
func runtimeModelForms(runtimes []SessionRuntimeView, claudeModels []string, surface ChatSurface, path, formAttr, button string, extra ...g.Node) []g.Node {
	var forms []g.Node
	for _, rt := range runtimes {
		// claude-code's model is one of the Claude aliases; opencode's is any
		// OpenRouter model, found by search (openRouterModelField).
		var field g.Node
		switch rt.Runtime {
		case "claude-code":
			opts := make([]g.Node, 0, len(claudeModels))
			for _, m := range claudeModels {
				opts = append(opts, Option(Value(m), g.Text(m)))
			}
			field = Select(
				Name("model"),
				g.Attr("aria-label", rt.Label+" model"),
				Class("min-h-[40px] min-w-0 flex-1 rounded-lg border-0 bg-bg px-2 text-sm text-fg ring-1 ring-inset ring-edge focus:outline-none focus:ring-2 focus:ring-focus disabled:opacity-60"),
				g.Group(opts),
			)
		case OpenRouterRuntime:
			field = openRouterModelField(formAttr[len("data-"):] + "-" + strconv.Itoa(len(forms)))
		default:
			continue
		}
		align := "items-center"
		if rt.Runtime == OpenRouterRuntime {
			align = "items-start"
		}
		forms = append(forms, Form(
			g.Attr(formAttr, rt.Runtime),
			chiefChatPost(surface, onSurface(surface, path)),
			g.Group(extra),
			hx("hx-disabled-elt", "find button, find select"),
			Class("flex "+align+" gap-2"),
			Input(Type("hidden"), Name("runtime"), Value(rt.Runtime)),
			Span(Class("w-20 shrink-0 pt-3 text-xs leading-none text-fg2"), g.Text(rt.Label)),
			field,
			Button(
				Type("submit"),
				Class("press min-h-[40px] shrink-0 rounded-lg bg-accent px-3 text-xs font-semibold text-on-accent transition hover:bg-accent/90 disabled:opacity-60"),
				g.Text(button),
			),
		))
	}
	return forms
}
