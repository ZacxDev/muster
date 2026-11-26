package ui

import (
	"fmt"
	"strconv"
	"strings"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE OPENROUTER MODEL PICK: any OpenRouter model, found by a fuzzy search of
// OpenRouter's catalogue (the operator's decision), in every place a chief
// session's model is picked on the opencode runtime — New session, Continue in
// a new session (runtimeModelForms) and the Sessions tab's "+" menu
// (chiefNewChatChip).
//
// The field is a text input named `model`: what it holds is what is posted,
// and the server accepts it only as an exact catalogue id, or a case or name
// variant of exactly one model (internal/api ResolveChiefSessionModel), so a
// typed id works without script. Typing asks the server for the top matches
// (OpenRouterModelsOptionsPath, htmx), drawn as buttons under the field; a tap
// on one puts its id in the field (openRouterPickScript).
//
// 🔴 BEFORE ANYTHING IS TYPED, THE RECENT MODELS (an operator ask): the field
// asks with its empty value when it is focused, when it is emptied, and when
// the "+" menu it sits in opens, and the server answers the models the chief's
// sessions were opened on, most recently used first, under "Recent"
// (OpenRouterRecentModels) — or nothing, when there are none.

// OpenRouterModelsOptionsPath is the search's HTML answer (internal/api
// handleChiefSessionModelOptions): the same ranking as GET
// /api/chief/openrouter-models, as the buttons below.
const OpenRouterModelsOptionsPath = "/api/chief/openrouter-models/options"

// The pick's data attributes: what both test tiers read.
const (
	// openRouterSearchAttr marks the model field; openRouterResultsAttr the
	// matches' container; OpenRouterPickAttr each match, valued with the id
	// it puts in the field.
	openRouterSearchAttr  = "data-openrouter-model-search"
	openRouterResultsAttr = "data-openrouter-model-results"
	OpenRouterPickAttr    = "data-openrouter-model-pick"
)

// OpenRouterRuntime is the session runtime the pick is for (agents
// SessionRuntimeOpencode).
const OpenRouterRuntime = "opencode"

// openRouterModelField is the model field and its matches. id makes the
// results container's id unique in the document (one per form).
func openRouterModelField(id string) g.Node {
	results := "openrouter-models-" + id
	return Div(
		Class("flex min-w-0 flex-1 flex-col gap-1"),
		Input(
			Type("text"),
			Name("model"),
			g.Attr(openRouterSearchAttr, ""),
			// Its search fills the "+" menu it may sit in: the menu stays open.
			g.Attr(MenuKeepOpenAttr, ""),
			g.Attr("required", ""),
			g.Attr("autocomplete", "off"),
			g.Attr("spellcheck", "false"),
			g.Attr("placeholder", "Search OpenRouter models"),
			g.Attr("aria-label", "OpenRouter model"),
			hx("hx-get", OpenRouterModelsOptionsPath),
			// focus and the menu OPENING ask with the field as it is: empty, the
			// recent models; a value, its matches again. The popover's toggle
			// fires on close too, so the filter keeps it to the opening
			// (ToggleEvent.newState): a menu shutting asks nothing.
			hx("hx-trigger", "focus, input changed delay:250ms, toggle[newState=='open'] from:closest [popover]"),
			hx("hx-target", "#"+results),
			hx("hx-swap", "innerHTML"),
			hx("hx-sync", "this:replace"),
			Class("min-h-[40px] min-w-0 rounded-lg border-0 bg-bg px-2 text-sm text-fg ring-1 ring-inset ring-edge focus:outline-none focus:ring-2 focus:ring-focus disabled:opacity-60"),
		),
		Div(ID(results), g.Attr(openRouterResultsAttr, ""), g.Attr("role", "listbox"), Class("flex max-h-56 flex-col gap-0.5 overflow-y-auto")),
		openRouterPickScript(),
	)
}

// OpenRouterModelHit is one match the search draws.
type OpenRouterModelHit struct {
	// ID is the model as it is stored and posted ("openrouter/<catalogue id>").
	ID, Name string
	// ContextLength is its context window, 0 when not known; PromptUSD and
	// CompletionUSD its list price per token as the catalogue writes it.
	ContextLength            int64
	PromptUSD, CompletionUSD string
}

// OpenRouterModelOptions is the search's answer: one button per match, or a
// line saying there is none.
func OpenRouterModelOptions(hits []OpenRouterModelHit, q string) g.Node {
	if len(hits) == 0 {
		if strings.TrimSpace(q) == "" {
			return g.Group{}
		}
		return P(Class("px-2 py-1 text-xs text-muted"), g.Text("No OpenRouter model matches “"+q+"”."))
	}
	return openRouterModelButtons(hits)
}

// OpenRouterRecentModelsLabel is the recent list's heading.
const OpenRouterRecentModelsLabel = "Recent"

// OpenRouterRecentModels is the empty field's answer: hits — the chief's
// recent models, most recently used first — under OpenRouterRecentModelsLabel,
// the same buttons a search draws; nothing when there are none.
func OpenRouterRecentModels(hits []OpenRouterModelHit) g.Node {
	if len(hits) == 0 {
		return g.Group{}
	}
	return Div(
		g.Attr("role", "group"),
		g.Attr("aria-label", "Recent models"),
		g.Attr("data-openrouter-model-recent", ""),
		Class("flex flex-col gap-0.5"),
		Span(g.Attr("aria-hidden", "true"), Class("px-2 pt-1 text-[11px] font-semibold uppercase tracking-wide text-muted"), g.Text(OpenRouterRecentModelsLabel)),
		openRouterModelButtons(hits),
	)
}

// openRouterModelButtons is one button per hit: its name (its id without the
// route prefix when it has none) over its id, list price and context.
func openRouterModelButtons(hits []OpenRouterModelHit) g.Node {
	var out g.Group
	for _, h := range hits {
		id := strings.TrimPrefix(h.ID, OpenRouterModelPrefix)
		meta := id
		if p := perMillion(h.PromptUSD); p != "" {
			meta += " · " + p + " in"
			if c := perMillion(h.CompletionUSD); c != "" {
				meta += ", " + c + " out per M"
			}
		}
		if h.ContextLength > 0 {
			meta += " · " + strconv.FormatInt(h.ContextLength/1000, 10) + "k"
		}
		name := h.Name
		if name == "" {
			name = id
		}
		out = append(out, Button(
			Type("button"),
			g.Attr(OpenRouterPickAttr, h.ID),
			g.Attr("role", "option"),
			Class("press flex min-h-[40px] w-full flex-col items-start rounded-lg px-2 py-1 text-left transition hover:bg-s2 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus"),
			Span(Class("text-sm font-medium text-fg"), g.Text(name)),
			Span(Class("text-[11px] text-muted"), g.Text(meta)),
		))
	}
	return out
}

// OpenRouterModelUnavailable is the search's answer when the catalogue
// cannot be read: the reason, in the matches' place.
func OpenRouterModelUnavailable(why string) g.Node {
	return P(Class("px-2 py-1 text-xs text-muted"), g.Text(why))
}

// OpenRouterModelPrefix is the stored form's prefix (agents
// OpenRouterModelPrefix).
const OpenRouterModelPrefix = "openrouter/"

// perMillion is a per-token USD price as "$0.15" per million tokens, "" when
// it is not a price.
func perMillion(perToken string) string {
	var v float64
	if _, err := fmt.Sscanf(perToken, "%g", &v); err != nil || v < 0 {
		return ""
	}
	m := v * 1e6
	switch {
	case m == 0:
		return "free"
	case m < 0.01:
		return "<$0.01"
	}
	return "$" + strconv.FormatFloat(m, 'f', 2, 64)
}

// openRouterPickScript binds, once per document, a tap on a match: its id
// goes in its form's model field, the matches are cleared, and the form's
// submit button takes the focus.
func openRouterPickScript() g.Node {
	return Script(g.Raw(`
(function () {
  if (window.__musterOpenRouterPick) return;
  window.__musterOpenRouterPick = true;
  document.addEventListener('click', function (e) {
    var b = e.target && e.target.closest && e.target.closest('button[` + OpenRouterPickAttr + `]');
    if (!b) return;
    var form = b.closest('form');
    if (!form) return;
    var f = form.querySelector('[` + openRouterSearchAttr + `]');
    if (f) f.value = b.getAttribute('` + OpenRouterPickAttr + `');
    var r = b.closest('[` + openRouterResultsAttr + `]');
    if (r) r.innerHTML = '';
    var s = form.querySelector('button[type="submit"]');
    if (s) s.focus();
  });
})();
`))
}
