package ui

import (
	"io"
	"strconv"
	"time"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// PrivilegeRequestView is the render-side projection of a pending privilege
// request (the api layer maps privilege.Request → this, mirroring AgentCardView).
type PrivilegeRequestView struct {
	ID        int64
	AgentName string
	Profile   string
	Reason    string
	CreatedAt time.Time
}

// PrivilegesPanel is the static shell for muster's Privileges tab; it
// lazy-loads /ui/privileges (the profile registry). The pending-request list is
// a SEPARATE partial (/ui/privilege-requests) that the loaded content mounts
// itself, so approving or denying a request does not re-fetch the registry.
//
// 🔴 NEW IN muster, NO UPSTREAM COUNTERPART — same shape as RunbooksPanel, and
// the same recorded seam applies: the two PARTIAL routes are on muster's side of
// the route partition manifest, but the DOCUMENT route `GET /privileges` this
// tab links to is owed by the API carve. See the `privileges` entry in
// musterTabs (components.go).
//
// 🔴 `privilege:changed` IS IN THE TRIGGER SET AND IT IS NOT DECORATION. A
// grant or revoke changes what an agent can reach; a registry that only
// refreshes on navigation shows a stale answer to exactly the question an
// operator opens this tab to ask. The 60s poll is the backstop for a dropped
// SSE connection, which is the case the event alone cannot cover.
func PrivilegesPanel(active bool) g.Node {
	return Div(
		ID(panelID("privileges")),
		g.Attr("role", "tabpanel"),
		// destructiveSwap=true: hx-target is this panel and hx-swap is innerHTML,
		// so a refresh discards every node inside it. See panelClass.
		Class(panelClass(active, true)),
		hx("hx-get", "/ui/privileges"),
		hx("hx-trigger", "load, privilege:changed from:body, every 60s"),
		hx("hx-target", "#"+panelID("privileges")),
		hx("hx-swap", "innerHTML"),
	)
}

// RenderPrivilegeRequests writes the /ui/privilege-requests partial: the list of
// pending privilege requests, morphed into #privilege-requests. Renders an empty
// node when there are none (the block collapses via `empty:mb-0`).
func RenderPrivilegeRequests(w io.Writer, list []PrivilegeRequestView) error {
	return PrivilegeRequests(list).Render(w)
}

// PrivilegeRequests is the pending privilege-request panel. Read-only in Phase
// 3.1 (granting + approve/deny arrive in 3.2).
func PrivilegeRequests(list []PrivilegeRequestView) g.Node {
	if len(list) == 0 {
		return g.Text("")
	}
	return Section(
		Class("rounded-2xl border border-amber-500/20 bg-amber-500/5 p-4 ring-1 ring-amber-500/10"),
		Div(
			Class("mb-3 flex items-center gap-2"),
			Span(Class("text-base"), g.Text("🔑")),
			H2(Class("text-sm font-semibold text-amber-200"), g.Text("Privilege requests")),
			Span(Class("rounded-full bg-amber-500/20 px-2 py-0.5 text-xs font-semibold text-amber-200"), g.Text(strconv.Itoa(len(list)))),
		),
		Div(Class("flex flex-col gap-2"), g.Map(list, privilegeRequestCard)),
	)
}

func privilegeRequestCard(pr PrivilegeRequestView) g.Node {
	ids := strconv.FormatInt(pr.ID, 10)
	return Div(
		Class("rounded-xl bg-slate-900/70 p-3 ring-1 ring-inset ring-white/5"),
		Div(
			Class("flex flex-wrap items-center gap-2"),
			Span(Class("text-sm font-semibold text-slate-100"), g.Text(pr.AgentName)),
			Span(Class("text-slate-400"), g.Text("wants")),
			Span(
				Class("inline-flex items-center rounded-full bg-amber-500/15 px-2.5 py-1 text-xs font-semibold text-amber-300 ring-1 ring-inset ring-amber-500/30"),
				g.Text(pr.Profile),
			),
			Span(Class("flex-1")),
			cardTime(pr.CreatedAt),
		),
		g.If(pr.Reason != "",
			P(Class("mt-2 whitespace-pre-wrap break-words text-xs leading-relaxed text-slate-400"), g.Text(pr.Reason)),
		),
		Div(
			Class("mt-3 flex items-center gap-2"),
			Button(
				Type("button"),
				Class("press inline-flex items-center justify-center rounded-lg bg-emerald-500 px-3 py-1.5 text-xs font-semibold text-emerald-950 transition hover:bg-emerald-400"),
				hx("hx-post", "/ui/privilege-requests/"+ids+"/approve"),
				hx("hx-target", "#privilege-requests"),
				hx("hx-swap", "morph:innerHTML"),
				hx("hx-confirm", "Grant "+pr.Profile+" to "+pr.AgentName+"? This applies live Kubernetes RBAC."),
				g.Text("Approve"),
			),
			Button(
				Type("button"),
				Class("press inline-flex items-center justify-center rounded-lg px-3 py-1.5 text-xs font-medium text-slate-300 ring-1 ring-inset ring-white/10 transition hover:bg-white/5 hover:text-rose-300"),
				hx("hx-post", "/ui/privilege-requests/"+ids+"/deny"),
				hx("hx-target", "#privilege-requests"),
				hx("hx-swap", "morph:innerHTML"),
				g.Text("Deny"),
			),
		),
	)
}

// --- profiles registry ---

// ProfileView is the render-side projection of a privilege profile.
type ProfileView struct {
	ID          int64
	Name        string
	DisplayName string
	Description string
	Summary     string // short human summary of what the spec grants
}

// RenderProfiles writes the /ui/privileges partial: the profiles list (read-only
// registry), morphed into #privilege-profiles. Profiles are now authored via the
// Operator (operator_create_profile) — there is no manual create form here.
func RenderProfiles(w io.Writer, list []ProfileView) error {
	return Profiles(list).Render(w)
}

// Profiles is the privilege-profiles registry section: a read-only list of
// reusable access bundles to grant to agents. Creation moved to the Operator
// (chat) — operator_create_profile — so this no longer renders an authoring form.
//
// 🔴 IT CARRIES id="privilege-profiles" AND THAT IS WHAT THE DELETE BUTTON
// TARGETS. Exactly the same correction as Runbooks in runbooks.go, for exactly
// the same reason: the id used to belong to a SECOND, hidden mount of this list
// inside the Agents panel's "Advanced" disclosure, so deleting a profile from
// the visible list swapped the server's response into the invisible copy and
// nothing on screen changed. One mount, and the id is on the thing the response
// replaces.
func Profiles(list []ProfileView) g.Node {
	return Section(
		ID("privilege-profiles"),
		Class("rounded-2xl border border-white/5 bg-slate-900/50 p-4 ring-1 ring-white/5"),
		Div(
			Class("mb-3 flex items-center gap-2"),
			Span(Class("text-base"), g.Text("🛡️")),
			H2(Class("text-sm font-semibold text-slate-200"), g.Text("Privilege profiles")),
			Span(Class("rounded-full bg-slate-700/50 px-2 py-0.5 text-xs font-semibold text-slate-300"), g.Text(strconv.Itoa(len(list)))),
		),
		g.If(len(list) == 0,
			P(Class("mb-3 text-xs text-slate-400"), g.Text("No profiles yet.")),
		),
		g.If(len(list) > 0,
			Div(Class("mb-3 flex flex-col gap-2"), g.Map(list, profileCard)),
		),
		P(Class("text-xs text-slate-400"), g.Text("Create via the Operator (chat) — ask it to define an access profile.")),
	)
}

func profileCard(p ProfileView) g.Node {
	ids := strconv.FormatInt(p.ID, 10)
	return Div(
		Class("flex items-start gap-2 rounded-xl bg-slate-950/50 p-3 ring-1 ring-inset ring-white/5"),
		Div(
			Class("min-w-0 flex-1"),
			Div(
				Class("flex flex-wrap items-center gap-2"),
				Span(Class("rounded-md bg-indigo-500/15 px-2 py-0.5 text-xs font-semibold text-indigo-300 ring-1 ring-inset ring-indigo-500/30"), g.Text(p.Name)),
				Span(Class("text-xs text-slate-400"), g.Text(p.Summary)),
			),
			g.If(p.Description != "",
				P(Class("mt-1 break-words text-xs text-slate-400"), g.Text(p.Description)),
			),
		),
		Button(
			Type("button"),
			g.Attr("aria-label", "Delete profile"),
			Class("press inline-flex h-7 w-7 shrink-0 items-center justify-center rounded-full text-slate-400 transition hover:bg-white/5 hover:text-rose-300"),
			hx("hx-delete", "/privileges/"+ids),
			// `closest section` rather than `#privilege-profiles`: the list this
			// card is IN, which is true wherever the partial is mounted. See the
			// note on Profiles above. outerHTML because the response IS the section.
			hx("hx-target", "closest section"),
			hx("hx-swap", "morph:outerHTML"),
			hx("hx-confirm", "Delete profile "+p.Name+"? Existing grants of it are removed."),
			g.Raw(`<svg class="h-4 w-4" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><path d="M5 5l10 10M15 5L5 15"/></svg>`),
		),
	)
}

// --- per-agent grants ---

// GrantView is a profile granted to an agent (render-side).
type GrantView struct {
	ProfileID   int64
	ProfileName string
}

// ProfileOption is a grantable profile for the agent grant selector.
type ProfileOption struct {
	ID   int64
	Name string
}

// RenderProfileGrantOptions writes the dispatch-modal privilege checklist
// (lazy-loaded into the dispatch form via /ui/agents/profiles).
func RenderProfileGrantOptions(w io.Writer, options []ProfileOption) error {
	return ProfileGrantChecklist(options).Render(w)
}

// ProfileGrantChecklist renders the dispatch-modal "grant privileges" multi-select
// as checkboxes (name="grant_profile", value=profile id) so a new agent can be
// granted least-privilege bundles at creation time. An empty list → a muted note.
func ProfileGrantChecklist(options []ProfileOption) g.Node {
	if len(options) == 0 {
		return P(Class("px-1 py-1.5 text-xs text-slate-400"), g.Text("No privilege profiles defined."))
	}
	rows := make([]g.Node, 0, len(options))
	for _, o := range options {
		rows = append(rows, Label(
			Class("flex cursor-pointer items-center gap-2 rounded-lg px-2 py-1.5 text-sm text-slate-200 transition hover:bg-white/5"),
			Input(
				Type("checkbox"), Name("grant_profile"), Value(strconv.FormatInt(o.ID, 10)),
				Class("h-4 w-4 rounded border-white/20 bg-slate-950 text-emerald-500 focus:ring-2 focus:ring-emerald-500/50"),
			),
			Span(Class("truncate"), g.Text(o.Name)),
		))
	}
	return Div(Class("flex flex-col gap-0.5"), g.Group(rows))
}

// profileGrantChecklistChecked is ProfileGrantChecklist with a set of pre-checked
// ids — the task-scoped dispatch confirm renders the task's privilege profiles
// pre-selected (name="grant_profile", value=id). Rendered inline (not lazy) so the
// pre-checks are server-resolved. An empty options list → the same muted note.
func profileGrantChecklistChecked(options []ProfileOption, selected []int64) g.Node {
	if len(options) == 0 {
		return P(Class("px-1 py-1.5 text-xs text-slate-400"), g.Text("No privilege profiles defined."))
	}
	checked := make(map[int64]bool, len(selected))
	for _, id := range selected {
		checked[id] = true
	}
	rows := make([]g.Node, 0, len(options))
	for _, o := range options {
		box := []g.Node{
			Type("checkbox"), Name("grant_profile"), Value(strconv.FormatInt(o.ID, 10)),
			Class("h-4 w-4 rounded border-white/20 bg-slate-950 text-emerald-500 focus:ring-2 focus:ring-emerald-500/50"),
		}
		if checked[o.ID] {
			box = append(box, g.Attr("checked", ""))
		}
		rows = append(rows, Label(
			Class("flex cursor-pointer items-center gap-2 rounded-lg px-2 py-1.5 text-sm text-slate-200 transition hover:bg-white/5"),
			Input(box...),
			Span(Class("truncate"), g.Text(o.Name)),
		))
	}
	return Div(Class("flex flex-col gap-0.5"), g.Group(rows))
}

// RenderAgentGrants writes the /ui/agents/{id}/grants partial: the granted
// profile chips + a grant selector, morphed into #agent-grants.
func RenderAgentGrants(w io.Writer, agentID int64, grants []GrantView, options []ProfileOption) error {
	return AgentGrants(agentID, grants, options).Render(w)
}

// AgentGrants renders an agent's privilege grants (chips + revoke) and a grant
// control for the not-yet-granted profiles.
func AgentGrants(agentID int64, grants []GrantView, options []ProfileOption) g.Node {
	ids := strconv.FormatInt(agentID, 10)
	return Div(
		Class("flex flex-col gap-3"),
		Div(
			Class("flex items-center gap-2"),
			Span(Class("text-base"), g.Text("🔑")),
			H3(Class("text-sm font-semibold text-slate-200"), g.Text("Privileges")),
		),
		g.If(len(grants) == 0,
			P(Class("text-xs text-slate-400"), g.Text("No privileges granted.")),
		),
		g.If(len(grants) > 0,
			Div(Class("flex flex-wrap gap-2"), g.Map(grants, func(gr GrantView) g.Node {
				return grantChip(ids, gr)
			})),
		),
		grantForm(ids, options),
	)
}

func grantChip(agentIDs string, gr GrantView) g.Node {
	pid := strconv.FormatInt(gr.ProfileID, 10)
	return Span(
		Class("inline-flex items-center gap-1.5 rounded-full bg-emerald-500/15 px-2.5 py-1 text-xs font-semibold text-emerald-300 ring-1 ring-inset ring-emerald-500/30"),
		g.Text(gr.ProfileName),
		Button(
			Type("button"),
			g.Attr("aria-label", "Revoke "+gr.ProfileName),
			Class("press -mr-1 inline-flex h-4 w-4 items-center justify-center rounded-full text-emerald-300/70 transition hover:bg-white/10 hover:text-rose-300"),
			hx("hx-delete", "/agents/"+agentIDs+"/grants/"+pid),
			hx("hx-target", "#agent-grants"),
			hx("hx-swap", "morph:innerHTML"),
			hx("hx-confirm", "Revoke "+gr.ProfileName+"? Its Kubernetes RBAC is removed."),
			g.Raw(`<svg class="h-3 w-3" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" aria-hidden="true"><path d="M5 5l10 10M15 5L5 15"/></svg>`),
		),
	)
}

func grantForm(agentIDs string, options []ProfileOption) g.Node {
	if len(options) == 0 {
		return g.Text("")
	}
	opts := make([]g.Node, 0, len(options)+1)
	opts = append(opts, Option(Value(""), g.Text("Grant a profile…")))
	for _, o := range options {
		opts = append(opts, Option(Value(strconv.FormatInt(o.ID, 10)), g.Text(o.Name)))
	}
	return Form(
		hx("hx-post", "/agents/"+agentIDs+"/grants"),
		hx("hx-target", "#agent-grants"),
		hx("hx-swap", "morph:innerHTML"),
		Class("flex items-center gap-2"),
		Select(
			Name("profile_id"),
			Required(),
			g.Attr("aria-label", "Profile to grant"),
			Class("rounded-lg border-0 bg-slate-950 px-2.5 py-1.5 text-xs text-slate-100 ring-1 ring-inset ring-white/10 focus:outline-none focus:ring-2 focus:ring-emerald-500/50"),
			g.Group(opts),
		),
		Button(Type("submit"),
			Class("press inline-flex items-center justify-center rounded-lg bg-slate-800 px-3 py-1.5 text-xs font-medium text-slate-200 ring-1 ring-inset ring-white/10 transition hover:bg-slate-700"),
			hx("hx-disabled-elt", "this"), g.Text("Grant")),
	)
}
