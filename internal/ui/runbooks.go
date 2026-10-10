package ui

import (
	"io"
	"strconv"
	"time"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// RunbookParam is a render-side projection of a runbook parameter (the input the
// dispatch form renders for it).
type RunbookParam struct {
	Name        string
	Label       string
	Description string
	Required    bool
	Default     string
	Enum        []string
}

// RunbookView is the render-side projection of a runbook.
type RunbookView struct {
	ID          int64
	Name        string
	DisplayName string
	Description string
	Summary     string // short human summary of what it dispatches
	Repo        string // default repo (pre-fills the dispatch form override)
	Params      []RunbookParam
	Runs        int       // dispatch count (audit history)
	LastRunAt   time.Time // most recent dispatch (zero = never)
}

// RunbooksPanel is the static shell for muster's Runbooks tab; it lazy-loads
// /ui/runbooks.
//
// 🔴 THIS PANEL IS NEW IN muster AND HAS NO UPSTREAM COUNTERPART. Upstream
// renders the runbooks section inside another view rather than as a top-level
// tab, so there was nothing to carry across — muster needs one because it owns
// the runbook surface outright and a service you can only navigate by typing
// URLs is not independently usable. See the `runbooks` entry in musterTabs
// (components.go) for the recorded seam: the PARTIAL route below is on muster's
// side of the route partition manifest, but the DOCUMENT route `GET /runbooks`
// this tab links to does not exist yet and is owed by the API carve.
//
// The trigger set is deliberately narrower than the tasks/agents panels': a
// runbook changes only when someone creates, deletes or dispatches one, and
// muster broadcasts `runbook:changed` for exactly those. There is no poll,
// because a registry that changes a handful of times a week does not earn one.
func RunbooksPanel(active bool) g.Node {
	return Div(
		ID(panelID("runbooks")),
		g.Attr("role", "tabpanel"),
		// destructiveSwap=true: hx-target is this panel and hx-swap is innerHTML,
		// so a refresh discards every node inside it. See panelClass.
		Class(panelClass(active, true)),
		hx("hx-get", "/ui/runbooks"),
		hx("hx-trigger", "load, runbook:changed from:body"),
		hx("hx-target", "#"+panelID("runbooks")),
		hx("hx-swap", "innerHTML"),
	)
}

// RenderRunbooks writes the /ui/runbooks partial: the runbooks list (run them
// with one click), morphed into #runbooks. Runbooks are now authored via the
// Operator (operator_create_runbook) — there is no manual create form here.
func RenderRunbooks(w io.Writer, list []RunbookView) error {
	return Runbooks(list).Render(w)
}

// Runbooks is the runbooks registry section: a list of reusable dispatch
// templates you run with one click. Creation moved to the Operator (chat) —
// operator_create_runbook — so this no longer renders an authoring form.
// 🔴 IT CARRIES id="runbooks" AND THAT IS WHAT THE CARD ACTIONS TARGET. Every
// runbook card's Delete and Dispatch swaps this whole section (morph:outerHTML)
// for the partial the server re-renders. The id used to belong to a SECOND,
// hidden mount of this same list inside the Agents panel — so the actions on the
// visible list swapped their response into the invisible copy and the operator
// saw nothing happen. There is one mount now, and the id is on the thing the
// response replaces.
func Runbooks(list []RunbookView) g.Node {
	return Section(
		ID("runbooks"),
		Class("rounded-2xl border border-line bg-s1/50 p-4 ring-1 ring-line"),
		Div(
			Class("mb-3 flex items-center gap-2"),
			Span(Class("text-base"), g.Text("📒")),
			H2(Class("text-sm font-semibold text-fg"), g.Text("Runbooks")),
			Span(Class("rounded-full bg-s3/50 px-2 py-0.5 text-xs font-semibold text-fg2"), g.Text(strconv.Itoa(len(list)))),
		),
		g.If(len(list) == 0,
			P(Class("mb-3 text-xs text-muted"), g.Text("No runbooks yet.")),
		),
		g.If(len(list) > 0,
			Div(Class("mb-3 flex flex-col gap-2"), g.Map(list, runbookCard)),
		),
		P(Class("text-xs text-muted"), g.Text("Create via the Operator (chat) — ask it to define a runbook.")),
	)
}

func runbookCard(rb RunbookView) g.Node {
	ids := strconv.FormatInt(rb.ID, 10)
	return Div(
		Class("rounded-xl bg-bg/50 p-3 ring-1 ring-inset ring-line"),
		Div(
			Class("flex items-start gap-2"),
			Div(
				Class("min-w-0 flex-1"),
				Div(
					Class("flex flex-wrap items-center gap-2"),
					Span(Class("rounded-md bg-st-progress-bg px-2 py-0.5 text-xs font-semibold text-st-progress-fg ring-1 ring-inset ring-st-progress-fg/40"), g.Text(rb.Name)),
					Span(Class("text-xs text-muted"), g.Text(rb.Summary)),
					runbookRunsBadge(rb),
				),
				g.If(rb.Description != "",
					P(Class("mt-1 break-words text-xs text-muted"), g.Text(rb.Description)),
				),
			),
			Button(
				Type("button"),
				g.Attr("aria-label", "Delete runbook"),
				Class("press inline-flex h-11 w-11 shrink-0 items-center justify-center rounded-full text-muted transition hover:bg-s2 hover:text-st-error-fg"),
				hx("hx-delete", "/runbooks/"+ids),
				// `closest section` rather than `#runbooks`: the target is the list
				// this card is IN, which is true wherever the partial is mounted. The
				// id form sent the response to whichever mount happened to own the id
				// — and for a while that was a hidden second copy. outerHTML because
				// the response IS the section, id included.
				hx("hx-target", "closest section"),
				hx("hx-swap", "morph:outerHTML"),
				hx("hx-confirm", "Delete runbook "+rb.Name+"?"),
				g.Raw(`<svg class="h-4 w-4" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><path d="M5 5l10 10M15 5L5 15"/></svg>`),
			),
		),
		runbookRunForm(rb),
	)
}

// runbookRunsBadge shows the dispatch count + how long ago the last run was.
// Renders nothing for a runbook that has never run.
func runbookRunsBadge(rb RunbookView) g.Node {
	if rb.Runs == 0 {
		return g.Text("")
	}
	label := strconv.Itoa(rb.Runs) + "×"
	if !rb.LastRunAt.IsZero() {
		label += " · last " + relTimeString(rb.LastRunAt) + " ago"
	}
	return Span(
		Class("inline-flex items-center rounded-full bg-s3/40 px-2 py-0.5 text-[11px] font-medium text-muted ring-1 ring-inset ring-line"),
		g.Text(label),
	)
}

// runbookRunForm is the collapsible "Run ▸" panel: a field per declared param
// (plus a repo override), posting to the dispatch endpoint.
func runbookRunForm(rb RunbookView) g.Node {
	ids := strconv.FormatInt(rb.ID, 10)
	fields := make([]g.Node, 0, len(rb.Params)+2)
	for _, p := range rb.Params {
		fields = append(fields, runbookParamField(p))
	}
	repoPlaceholder := "repo override (owner/name)"
	if rb.Repo != "" {
		repoPlaceholder = "repo (default: " + rb.Repo + ")"
	}
	fields = append(fields,
		Input(Type("text"), Name("repo"), Placeholder(repoPlaceholder),
			Class("w-full rounded-lg border-0 bg-bg px-3 py-2 text-sm text-fg ring-1 ring-inset ring-edge placeholder:text-muted focus:outline-none focus:ring-2 focus:ring-focus")),
		Button(Type("submit"),
			Class("press inline-flex items-center justify-center rounded-lg bg-accent px-3 py-2 text-xs font-semibold text-on-accent transition hover:bg-accent/90"),
			hx("hx-disabled-elt", "this"), g.Text("Dispatch")),
	)
	return g.El("details",
		Class("group/run mt-2 rounded-lg bg-bg/40 ring-1 ring-inset ring-line"),
		// 🔴 list-none IS REQUIRED, NOT COSMETIC PADDING. A <summary> is
		// `display: list-item` with `list-style-type: disclosure-closed`, so the
		// browser draws its OWN triangle — and this markup drew a second one as
		// literal text. The rendered control read "▸ ▸ Run". The "Advanced"
		// summary on the agents view already set list-style:none for exactly this
		// reason; matching it means one triangle, and it is the one that ROTATES
		// on open (group-open/run) so the control says which state it is in.
		// marker:content-[''] covers the WebKit/Blink ::marker spelling the way
		// the fenced-code <summary> in markdown.go does.
		g.El("summary",
			Class("flex cursor-pointer select-none list-none items-center gap-1.5 px-3 py-2 text-xs font-medium text-fg2 marker:content-['']"),
			Span(Class("transition-transform group-open/run:rotate-90"), g.Text("▸")),
			g.Text("Run"),
		),
		Form(
			hx("hx-post", "/runbooks/"+ids+"/dispatch"),
			// Same reasoning as the Delete button above: the list this card is in,
			// not whatever owns the id.
			hx("hx-target", "closest section"),
			hx("hx-swap", "morph:outerHTML"),
			hx("hx-on::after-request", "if(event.target===this && event.detail.successful){try{window.cgTrack('runbook.run',{name:"+jsonString(rb.Name)+"});}catch(e){}}"),
			Class("flex flex-col gap-2 p-3 pt-0"),
			g.Group(fields),
		),
	)
}

func runbookParamField(p RunbookParam) g.Node {
	label := p.Label
	if p.Required {
		label += " *"
	}
	inputClass := "w-full rounded-lg border-0 bg-bg px-3 py-2 text-sm text-fg ring-1 ring-inset ring-edge placeholder:text-muted focus:outline-none focus:ring-2 focus:ring-focus"
	var control g.Node
	if len(p.Enum) > 0 {
		opts := make([]g.Node, 0, len(p.Enum))
		for _, e := range p.Enum {
			opts = append(opts, Option(Value(e), g.If(e == p.Default, Selected()), g.Text(e)))
		}
		control = Select(Name("param_"+p.Name), Class(inputClass), g.Group(opts))
	} else {
		attrs := []g.Node{
			Type("text"), Name("param_" + p.Name), Placeholder(p.Description), Class(inputClass),
		}
		if p.Default != "" {
			attrs = append(attrs, Value(p.Default))
		}
		if p.Required {
			attrs = append(attrs, Required())
		}
		control = Input(attrs...)
	}
	return Label(
		Class("flex flex-col gap-1 text-xs text-muted"),
		Span(g.Text(label)),
		control,
	)
}
