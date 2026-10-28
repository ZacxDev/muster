package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/ZacxDev/muster/internal/agents"
)

// ---------------------------------------------------------------------------
// THE CHIEF IS OPTIONAL, AND OFF UNLESS YOU HAVE ONE
//
// The chief is an agent that reports on and manages the rest of the fleet. Not
// everyone wants one, so each user has a switch (Account → Chief, stored in
// chief_preferences, migration 0060):
//
//   - a user who never touched the switch has the chief ON exactly when they
//     already have a chief agent. A fresh install and a new user have none, so
//     they have no chief; a deployment that runs one keeps it, unchanged, with
//     no data migration — the agent row is the state that turned it on.
//   - a stored choice wins either way.
//
// OFF stops the chief from acting and hides it from navigation; it deletes
// nothing (the agent, its sessions and its history stay):
//
//   - chiefAgent — everything that TALKS to the chief: plugin deliveries,
//     `chief ask`, a session's chief_ask, the panel, threads — answers
//     ErrChiefOff, which is an ErrNoChief, so every one of those paths takes
//     the branch it already had for "no chief";
//   - `/` opens the Agents list, and the agents list and sidebar leave the
//     chief out (homeTarget, agentsListCardsLive);
//   - a mention's ask is raised by muster, not the chief (mentionAsker);
//   - the chief agent is offered none of its chief-only tools, and a call to
//     one is refused (toolsFor, dispatchAgentTool), so a chief that is still
//     running cannot route, dispatch or draft;
//   - mirrored sessions are not offered chief_ask (mirrorToolsFor);
//   - with an agent provisioner, the chief's instance is stopped (and started
//     again when it is turned back on).
//
// ⚠ THE DELETE PROTECTION IS NOT PART OF IT. chiefPredicate still calls an off
// chief the chief, so it still cannot be deleted by a stray tap: off is
// reversible and a delete is not.
// ---------------------------------------------------------------------------

// ErrChiefOff reports that the user has a chief agent and turned it off. It
// IS an ErrNoChief (errors.Is), so every caller that already handles "no
// chief" treats an off chief the same way.
var ErrChiefOff = fmt.Errorf("%w: the chief is turned off", ErrNoChief)

// chiefChoice is owner's stored choice (chose false: none). No store (no
// database) is "never chose".
func (s *Server) chiefChoice(ctx context.Context, owner int64) (chose, on bool, err error) {
	if s.ext.ChiefPrefs == nil {
		return false, false, nil
	}
	return s.ext.ChiefPrefs.Choice(ctx, owner)
}

// chiefOn is THE rule: whether owner's chief is on, given an agent list that
// holds owner's agents (any other owner's rows in it are ignored). A stored
// choice wins; with none, the chief is on exactly when owner has a chief agent
// (primaryChief among owner's agents).
//
// 🔴 A READ ERROR IS OFF. A user who turned the chief off must never have it
// act because one read failed; a chief that is on and skipped for one failed
// read loses nothing it cannot be sent again.
func (s *Server) chiefOn(ctx context.Context, owner int64, list []agents.Agent) (bool, error) {
	if owner <= 0 {
		return false, nil
	}
	chose, on, err := s.chiefChoice(ctx, owner)
	if err != nil {
		return false, err
	}
	if chose {
		return on, nil
	}
	_, has := primaryChief(ownedBy(list, owner))
	return has, nil
}

// chiefOnFor is chiefOn reading owner's agents itself.
func (s *Server) chiefOnFor(ctx context.Context, owner int64) (bool, error) {
	if s.ext.Agents == nil {
		return s.chiefOn(ctx, owner, nil)
	}
	list, err := s.ext.Agents.List(ctx)
	if err != nil {
		return false, err
	}
	return s.chiefOn(ctx, owner, list)
}

// ownedBy is list's agents owned by owner.
func ownedBy(list []agents.Agent, owner int64) []agents.Agent {
	out := make([]agents.Agent, 0, len(list))
	for _, a := range list {
		if a.OwnerID == owner {
			out = append(out, a)
		}
	}
	return out
}

// withoutOffChief is list without the viewer's chief when the viewer turned it
// off (or the switch could not be read: off, as chiefOn). A principal-less or
// scope-all ctx has no viewer, and its list is returned as is.
func (s *Server) withoutOffChief(ctx context.Context, list []agents.Agent) []agents.Agent {
	owner := requestOwner(ctx)
	if owner <= 0 {
		return list
	}
	on, err := s.chiefOn(ctx, owner, list)
	if err != nil {
		s.logger.Printf("agents: owner %d: read the chief toggle: %v (leaving the chief out)", owner, err)
	}
	if on {
		return list
	}
	isChief := chiefPredicate(list)
	out := make([]agents.Agent, 0, len(list))
	for _, a := range list {
		if a.OwnerID == owner && isChief(a) {
			continue
		}
		out = append(out, a)
	}
	return out
}

// chiefActing reports whether agent a is its owner's chief AND that chief is
// on: the predicate the chief-only tools are offered and served on. A read
// error is "not acting" (logged), the chief-only axis's fail-closed direction.
func (s *Server) chiefActing(ctx context.Context, a agents.Agent) bool {
	return s.isChiefAgent(ctx, a) && s.ownersChiefOn(ctx, a)
}

// ownersChiefOn is chiefOnFor(a's owner), a read error logged and read as off.
func (s *Server) ownersChiefOn(ctx context.Context, a agents.Agent) bool {
	on, err := s.chiefOnFor(ctx, a.OwnerID)
	if err != nil {
		s.logger.Printf("agent tools: agent %d: could not read whether the chief is on: %v", a.ID, err)
		return false
	}
	return on
}

// --- the Account page's switch ------------------------------------------------

// accountChiefPath is the switch's POST (on=1|0).
const accountChiefPath = "/account/chief"

// chiefToggleAvailable reports whether the switch can do anything for owner
// with the agents in list: a database to keep the choice in, and either an
// agent provisioner (on can create the chief) or a chief agent already there
// (on and off gate it). A public install with no Kubernetes and no chief has
// neither, and the page says so instead of drawing a switch that cannot work.
func (s *Server) chiefToggleAvailable(owner int64, list []agents.Agent) bool {
	if s.ext.ChiefPrefs == nil {
		return false
	}
	if s.ext.Provisioner != nil {
		return true
	}
	_, has := primaryChief(ownedBy(list, owner))
	return has
}

// accountChiefSection is the Account page's "Chief" section: on or off, one
// line on what it does, and the switch — or, where it cannot work, why not.
func (s *Server) accountChiefSection(r *http.Request) string {
	ctx := r.Context()
	owner := requestOwner(ctx)
	const head = `<section id="chief" data-testid="chief-settings" class="flex flex-col gap-3 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line">
<h2 class="text-base font-semibold text-fg">Chief</h2>
<p class="text-sm text-fg2">Optional. The chief is an agent that reports on and manages the rest of your agents: plugin messages and your sessions' questions go to it, and home opens its chat. Off, nothing is sent to it and it is hidden; its history is kept.</p>
`
	state := func(id, text string) string {
		return `<p id="chief-state" data-state="` + id + `" class="text-sm text-fg">` + text + `</p>
`
	}
	form := func(on bool, label string) string {
		v := "0"
		if on {
			v = "1"
		}
		return `<form method="POST" action="` + accountChiefPath + `"><input type="hidden" name="on" value="` + v + `">
<button type="submit" id="chief-toggle" class="press min-h-[44px] w-full rounded-md bg-bg px-3 py-2 text-base font-semibold text-fg ring-1 ring-inset ring-edge">` + label + `</button></form>
`
	}
	var list []agents.Agent
	if s.ext.Agents != nil && owner > 0 {
		l, err := s.ext.Agents.List(ctx)
		if err != nil {
			s.logger.Printf("chief: account: list agents: %v", err)
			return head + state("unknown", "Your chief setting could not be read. Reload the page to try again.") + `</section>`
		}
		list = l
	}
	if owner <= 0 || !s.chiefToggleAvailable(owner, list) {
		return head + state("unavailable", "Not available on this install. The chief runs as an agent in its own Kubernetes pod, and this muster has no agent provisioner (MUSTER_AGENT_PROVISIONER). Everything else works without it.") + `</section>`
	}
	notice := ""
	switch r.URL.Query().Get("chief") {
	case "saved":
		notice = `<p role="status" class="text-sm font-semibold text-fg">Saved.</p>
`
	case "refused":
		notice = `<p role="alert" class="text-sm font-semibold text-fg">The chief could not be created: add an OpenRouter key or a Claude account under Credentials, then turn it on again.</p>
`
	case "failed":
		notice = `<p role="alert" class="text-sm font-semibold text-fg">The chief could not be created. It stays off; try again.</p>
`
	case "startfailed":
		notice = `<p role="alert" class="text-sm font-semibold text-fg">The chief could not be started. It stays off; try again.</p>
`
	case "stopfailed":
		notice = `<p role="alert" class="text-sm font-semibold text-fg">The chief is off and gets no work, but its pod could not be stopped. Turn it off again to retry.</p>
`
	}
	on, err := s.chiefOn(ctx, owner, list)
	if err != nil {
		s.logger.Printf("chief: account: read the toggle: %v", err)
		return head + state("unknown", "Your chief setting could not be read. Reload the page to try again.") + `</section>`
	}
	if on {
		return head + notice + state("on", "On.") + form(false, "Turn the chief off") + `</section>`
	}
	return head + notice + state("off", "Off.") + form(true, "Turn the chief on") + `</section>`
}

// chiefSwitchLock is owner's switch lock: every POST /account/chief of one
// owner runs its read, its provisioner call and its store under it, one at a
// time, so the last request to take it decides both the stored choice and the
// instance's state (see handleAccountChief). Per process: muster runs one.
func (s *Server) chiefSwitchLock(owner int64) *sync.Mutex {
	m, _ := s.chiefSwitch.LoadOrStore(owner, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// handleAccountChief is POST /account/chief (on=1|0): the caller's own switch,
// back to the Account page.
//
// 🔴 THE PROVISIONER CALL IS MADE IN THE REQUEST, UNDER THE OWNER'S SWITCH
// LOCK, AND THAT IS WHAT MAKES THE LAST CHOICE WIN. It used to be a background
// goroutine per click, with Start only when the row read `stopped`: Off then
// On quickly left the chief on but stopped (On read the row before Stop wrote
// it, so started nothing), and On then Off could land the slow Start after the
// Stop, leaving a pod running with the chief off (audit round 1, #1 and #3).
// Serialised, each request finishes its call before the next one reads.
//
//   - off: stored first (the gates stop the chief acting at once), then, with
//     a provisioner, its instance is stopped. A Stop that fails is said, not
//     hidden: the choice stands and the page says the pod is still up.
//   - on: with a chief agent, its instance is started — UNCONDITIONALLY, since
//     a Start of a running instance only re-applies it — and the choice is
//     stored only once that succeeded; a Start that fails leaves it off and
//     says so, as a refused create does. With no chief agent, it is created
//     (provisionChief) and stored only once that succeeded.
//
// Nothing is deleted either way.
func (s *Server) handleAccountChief(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := requestOwner(ctx)
	if owner <= 0 {
		http.Error(w, "no user", http.StatusForbidden)
		return
	}
	var on bool
	switch r.PostFormValue("on") {
	case "1":
		on = true
	case "0":
	default:
		http.Error(w, "on is 1 or 0", http.StatusBadRequest)
		return
	}
	back := func(code string) { http.Redirect(w, r, accountPath+"?chief="+code+"#chief", http.StatusSeeOther) }
	mu := s.chiefSwitchLock(owner)
	mu.Lock()
	defer mu.Unlock()
	// A client that goes away mid-call must not cut a Start or Stop in half;
	// the principal is kept (see Provisioner).
	opCtx := context.WithoutCancel(ctx)
	var list []agents.Agent
	if s.ext.Agents != nil {
		l, err := s.ext.Agents.List(ctx)
		if err != nil {
			s.logger.Printf("chief: owner %d: list agents: %v", owner, err)
			http.Error(w, "could not read your agents", http.StatusInternalServerError)
			return
		}
		list = l
	}
	if !s.chiefToggleAvailable(owner, list) {
		http.Error(w, "the chief is not available on this install", http.StatusConflict)
		return
	}
	chief, has := primaryChief(ownedBy(list, owner))
	save := func() bool {
		if err := s.ext.ChiefPrefs.SetEnabled(ctx, owner, on); err != nil {
			s.logger.Printf("chief: owner %d: save the toggle: %v", owner, err)
			http.Error(w, "could not save the setting", http.StatusInternalServerError)
			return false
		}
		s.logger.Printf("chief: owner %d turned the chief %s", owner, map[bool]string{true: "on", false: "off"}[on])
		return true
	}
	switch {
	case on && !has:
		// chiefToggleAvailable already said a provisioner exists when there is
		// no chief; checked here too, at the call that needs it.
		if s.ext.Provisioner == nil {
			back("failed")
			return
		}
		if _, _, err := s.provisionChief(ctx, owner, ""); err != nil {
			code := "failed"
			if errors.Is(err, ErrDispatchRefused) || errors.Is(err, agents.ErrModelNotCredentialed) {
				code = "refused"
			}
			back(code)
			return
		}
		if !save() {
			return
		}
	case on:
		if s.ext.Provisioner != nil {
			if err := s.ext.Provisioner.Start(opCtx, chief.ID); err != nil {
				s.logger.Printf("chief: owner %d: start %d: %v (left off)", owner, chief.ID, err)
				back("startfailed")
				return
			}
		}
		if !save() {
			return
		}
	default:
		if !save() {
			return
		}
		if has && s.ext.Provisioner != nil {
			if err := s.ext.Provisioner.Stop(opCtx, chief.ID); err != nil {
				s.logger.Printf("chief: owner %d: stop %d: %v (off, still running)", owner, chief.ID, err)
				s.agentChangedFor(owner, chief.Name)
				back("stopfailed")
				return
			}
		}
	}
	// Other open pages' sidebars refetch (they read the switch on each load).
	if has {
		s.agentChangedFor(owner, chief.Name)
	}
	back("saved")
}
