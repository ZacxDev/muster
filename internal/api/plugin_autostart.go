package api

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/fence"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// AUTO-START (docs/plugins.md#auto-start, migration 0152): the owner opts ONE
// plugin in, on its /plugins card behind a fresh sign-in, and a task that
// plugin files starts an agent on it with no tap.
//
// 🔴 IT IS A THIRD DOOR ONTO THE CHIEF'S LEDGER, NOT A SECOND SET OF GUARDS.
// The start goes through ReserveChiefDispatch and startReserved exactly as
// agent_dispatch and dispatch_approve do, and is judged by THE SAME predicates:
//
//   - chiefDispatchDeploymentRefusal — the owner is in
//     MUSTER_CHIEF_DISPATCH_OWNERS and this deployment can start an agent (so a
//     Docker Compose install, whose chief refuses dispatch, never auto-starts);
//   - chiefDispatchTaskRefusal, under the owner's reservation lock — the task
//     is not gated, open, unclaimed, with no live agent (a GATED task is never
//     auto-started: this is the predicate task_route_attention's buttons and
//     agent_dispatch itself refuse on, task_actions.go);
//   - connectedRepo — the opt-in's repository is still one of the owner's;
//   - the chief's budget (plugins.ChiefBudget's LiveFull/DailyFull), counted by
//     the reservation itself: an auto-start spends the chief's day like any
//     start, and a full budget refuses it.
//
// On top: the plugin's own per-day cap (plugins.AutostartsInWindow, counted
// under the same lock). Any refusal falls back to what a task gets today —
// muster puts it in front of the owner (task_route_attention, the one-tap
// ask) — and the reason is recorded (an audit row and a task comment).
//
// 🔴 THE TASK CHOOSES NOTHING. The target and repository are the opt-in's;
// a task naming another repository is refused, never followed; the body is
// handed to the agent INSIDE the plugin-message fence
// (plugins.RenderAutostartTask), never as instructions.
//
// 🔴 EVERY AUTO-START IS SEEN AND STOPPABLE: a comment on the task, and an ask
// from muster on the Attention view and the owner's phone — its push is
// "Started an agent on #N … from <plugin>" — whose one option, Stop the agent,
// stops it (autostartStopForAnswer).
// ---------------------------------------------------------------------------

// autostartStore is the plugin store's auto-start half (plugins.PGStore). A
// store without it (a test fake) auto-starts nothing and draws no control.
type autostartStore interface {
	SetAutostart(ctx context.Context, pluginID int64, target, repo string, dailyCap int) (plugins.Autostart, error)
	ClearAutostart(ctx context.Context, pluginID int64) (bool, error)
	AutostartOf(ctx context.Context, pluginID int64) (plugins.Autostart, error)
	AutostartsInWindow(ctx context.Context, pluginID int64) (int, error)
	RecordAutostartStarted(ctx context.Context, r plugins.AutostartRun) error
	RecordAutostartSkipped(ctx context.Context, pluginID, taskID int64, reason string) error
	AutostartByAsk(ctx context.Context, askID int64) (plugins.AutostartRun, bool, error)
}

func (s *Server) autostarts() autostartStore {
	as, _ := s.ext.Plugins.(autostartStore)
	return as
}

// The auto-start's own refusals, in the owner's words (the task comment, the
// fallback ask's note, the audit row).
const (
	autostartCapFull   = "this plugin already auto-started its most agents for today (%d in 24 hours)"
	autostartBudget    = "your chief's allowance for starting agents is used up for now"
	autostartTaskRepo  = "the task names another repository (%s) than the one auto-start is set to (%s)"
	autostartPrior     = "an agent was already started on this task"
	autostartBusy      = "another start held your allowance too long; nothing was spent"
	autostartTeam      = "auto-start is for a person's plugins, not a team's"
	autostartReadError = "muster could not read what it needed to start one"
	autostartNoStart   = "the agent could not be started"
)

// autostartStopLabel is the Stop ask's one option: the button, the phone chip
// and the answer.
const autostartStopLabel = "Stop the agent"

// autostartRequest is what the plugin's request says about routing: whether
// it also hands an ASSIGNED notice to the chief, which then places a task
// that was not auto-started itself (today's route) — so muster's fallback ask
// is not filed twice.
type autostartRequest struct {
	routesAssigned bool
}

// autostartPluginTask runs auto-start for task n, just filed by plugin p. ctx
// acts as the owner (the plugin's principal). It reports whether an agent was
// started; nothing happens at all when p is not opted in.
func (s *Server) autostartPluginTask(ctx context.Context, p plugins.Plugin, n notes.Note, req autostartRequest) bool {
	store := s.autostarts()
	if store == nil || s.ext.ChiefDispatch == nil || s.ext.Notes == nil {
		return false
	}
	cfg, err := store.AutostartOf(ctx, p.ID)
	if err != nil {
		s.logger.Printf("auto-start: plugin %d: read its setting: %v", p.ID, err)
		return false
	}
	if !cfg.On {
		return false
	}
	ctx = context.WithoutCancel(ctx)
	rec, why := s.autostartStart(ctx, store, p, cfg, n)
	if why != "" {
		s.autostartSkipped(ctx, store, p, n, why, req)
		return false
	}
	s.autostartStarted(ctx, store, p, cfg, n, rec)
	return true
}

// autostartStart is the start itself: the shared predicates, then the chief's
// ledger. It answers the agent, or why none was started (the owner's words).
func (s *Server) autostartStart(ctx context.Context, store autostartStore, p plugins.Plugin, cfg plugins.Autostart, n notes.Note) (agents.Agent, string) {
	owner := n.OwnerID
	if p.OwnerTeam || owner != p.OwnerID {
		return agents.Agent{}, autostartTeam
	}
	// The opt-in and the deployment: agent_dispatch's own first refusal.
	if why := s.chiefDispatchDeploymentRefusal(owner); why != "" {
		return agents.Agent{}, why
	}
	// The target the OWNER chose (never the task's), as agent_dispatch reads one.
	kindArg, why := s.podKindOf(ctx, cfg.Target)
	if why != "" {
		return agents.Agent{}, why
	}
	kind, err := s.resolveKind(ctx, kindArg)
	if err != nil {
		return agents.Agent{}, err.Error()
	}
	// The repository the OWNER chose, still one of theirs (guard 12b's check).
	if why := s.connectedRepo(ctx, cfg.Repo); why != "" {
		return agents.Agent{}, why
	}
	detail := map[string]any{"kind": kind, "target": cfg.Target, "repo": cfg.Repo, "door": "plugin_autostart",
		plugins.DetailAutostartPlugin: p.ID, DetailRequestedByUser: owner}
	var task notes.Note
	// 🔴 UNDER THE OWNER'S RESERVATION LOCK: the task re-read, judged by THE
	// task predicate agent_dispatch uses (chiefDispatchTaskRefusal: gated, not
	// open, claimed, a live agent), and the plugin's own cap — each a refusal
	// that reserves nothing.
	check := func() error {
		cur, err := s.ext.Notes.Get(ctx, n.ID)
		if err != nil {
			return err
		}
		if cur.OwnerID != owner {
			return errTaskDispatch(chiefDispatchNoTask)
		}
		if cur.Repo != "" && !strings.EqualFold(cur.Repo, cfg.Repo) {
			return errTaskDispatch(fmt.Sprintf(autostartTaskRepo, cur.Repo, cfg.Repo))
		}
		if why, err := s.chiefDispatchTaskRefusal(ctx, owner, cur); err != nil {
			return err
		} else if why != "" {
			return errTaskDispatch(why)
		}
		used, err := store.AutostartsInWindow(ctx, p.ID)
		if err != nil {
			return err
		}
		if used >= cfg.DailyCap {
			return errTaskDispatch(fmt.Sprintf(autostartCapFull, cfg.DailyCap))
		}
		task = cur
		return nil
	}
	res, err := s.ext.ChiefDispatch.ReserveChiefDispatch(ctx, plugins.NoteObject(n.ID), detail, check)
	var refusal errTaskDispatch
	switch {
	case err == nil:
	case errors.As(err, &refusal):
		return agents.Agent{}, string(refusal)
	case errors.Is(err, plugins.ErrChiefLiveCap), errors.Is(err, plugins.ErrChiefDailyCap):
		return agents.Agent{}, autostartBudget
	case reservationBusy(err):
		return agents.Agent{}, autostartBusy
	default:
		s.logger.Printf("auto-start: task %d: reserve a start: %v", n.ID, err)
		return agents.Agent{}, autostartReadError
	}
	if res.Prior {
		return agents.Agent{}, autostartPrior
	}
	dp := dispatchParams{Kind: kind, Repo: cfg.Repo, Model: task.Model, Kickoff: true, TrackOutcome: true,
		DispatchedBy: s.ownersChiefID(ctx), NoteText: autostartKickoff(p, task), NoteID: &task.ID,
		DisplayName: dispatchDisplayName(task.ID, task.Body)}
	if kind == agents.KindClaudeCode {
		dp.Model = "" // claudeCodeNoModel
	}
	st := s.startReserved(ctx, plugins.NoteObject(n.ID), res.ReservationID, dp)
	switch {
	case st.failReason != "":
		return agents.Agent{}, st.failReason
	case st.refused != nil:
		return agents.Agent{}, autostartNoStart + ": " + st.refused.Error()
	case st.failed, st.failUnrecorded:
		return agents.Agent{}, autostartNoStart
	}
	return st.agent, ""
}

// autostartKickoff is an auto-started agent's first message: the task the
// plugin filed, title and body INSIDE the plugin-message fence
// (plugins.RenderAutostartTask), then muster's rules for a started agent,
// LAST (chiefDispatchRules: a forward folded into the first turn goes before
// them, forwardToTaskAgent).
func autostartKickoff(p plugins.Plugin, n notes.Note) string {
	return plugins.RenderAutostartTask(p, n.ID, n.Title, n.Body) + chiefDispatchRules
}

// autostartStarted makes a start seen and stoppable: in progress, a comment on
// the task, and muster's ask — pushed as "Started an agent on #N … from
// <plugin>" — whose one option stops it.
func (s *Server) autostartStarted(ctx context.Context, store autostartStore, p plugins.Plugin, cfg plugins.Autostart, n notes.Note, rec agents.Agent) {
	id := strconv.FormatInt(n.ID, 10)
	s.advanceTaskToInProgress(ctx, n.ID)
	label := firstNonBlank(p.Label, p.Name)
	if _, _, err := s.addTaskComment(ctx, commentByMuster, notes.Comment{NoteID: n.ID, Author: ownerReleaseAuthor,
		Body: "Auto-start: muster started an agent (" + rec.Name + ") on this task with no tap from you, because " + label +
			" is set to start agents on its tasks (" + autostartTargetWords(cfg.Target) + ", in " + cfg.Repo + "). " +
			"Stop it from the ask on your Attention view, or from the agent's page."}); err != nil {
		s.logger.Printf("auto-start: task %d: comment: %v", n.ID, err)
	}
	var askID int64
	if s.ext.Attention != nil {
		q := autostartAskQuestion(n, label)
		ask, err := s.ext.Attention.Create(ctx, attention.New{Asker: attention.Asker{System: true, Label: attention.SystemAskerLabel},
			Question: fence.Cut(q, attention.MaxQuestionRunes), Options: []string{autostartStopLabel}, TaskID: n.ID})
		if err != nil {
			s.logger.Printf("auto-start: task %d: file the stop ask: %v", n.ID, err)
		} else {
			askID = ask.ID
			s.askFiled(ask) // the push
		}
	}
	if err := store.RecordAutostartStarted(ctx, plugins.AutostartRun{PluginID: p.ID, TaskID: n.ID, AgentID: rec.ID, AskID: askID}); err != nil {
		s.logger.Printf("auto-start: task %d: record the start: %v", n.ID, err)
	}
	s.attentionChanged(n.OwnerID, "task-routed-"+id)
	s.logger.Printf("auto-start: task %d of plugin %d: started agent %d (%s, %s)", n.ID, p.ID, rec.ID, cfg.Target, cfg.Repo)
}

// autostartAskQuestion is the Stop ask's question, which is also the push's
// text: what was started, on which task, from which plugin.
func autostartAskQuestion(n notes.Note, label string) string {
	return "Started an agent on #" + strconv.FormatInt(n.ID, 10) + " " + taskLabel(n) + " from " + label +
		". It runs now with no tap from you; stop it here if you did not want it."
}

// autostartTargetWords is a target in the owner's words.
func autostartTargetWords(target string) string {
	switch target {
	case "pod/claude-code":
		return "Claude Code"
	case "pod/gateway":
		return "a Standard agent on your OpenRouter model"
	}
	return target
}

// autostartSkipped is a task of an opted-in plugin that was NOT auto-started:
// the reason recorded (an audit row, a task comment), and — unless the
// plugin's own notice already hands the task to the chief — today's one-tap
// ask, filed by muster with the chief's own task_route_attention.
func (s *Server) autostartSkipped(ctx context.Context, store autostartStore, p plugins.Plugin, n notes.Note, why string, req autostartRequest) {
	s.logger.Printf("auto-start: task %d of plugin %d: not started: %s", n.ID, p.ID, why)
	if err := store.RecordAutostartSkipped(ctx, p.ID, n.ID, why); err != nil {
		s.logger.Printf("auto-start: task %d: record the skip: %v", n.ID, err)
	}
	if _, _, err := s.addTaskComment(ctx, commentByMuster, notes.Comment{NoteID: n.ID, Author: ownerReleaseAuthor,
		Body: "Auto-start: muster did not start an agent on this task: " + why + ". It waits for you, as any task does."}); err != nil {
		s.logger.Printf("auto-start: task %d: comment: %v", n.ID, err)
	}
	if req.routesAssigned && taskRoutingOn(s.ext) {
		return // the notice's own route places it (task triage, or the chief)
	}
	chief, err := s.chiefAgent(ctx)
	if err != nil || chief.ID == 0 || agentOwnerID(chief) != n.OwnerID || !s.chiefActing(ctx, chief) {
		return // no chief to file the ask: the task is on the board, with its push
	}
	if out := s.toolTaskRouteAttention(ctx, chief, n.ID, autostartFallbackNote(why), ""); toolRefused(out) {
		s.logger.Printf("auto-start: task %d: the fallback ask was refused: %s", n.ID, out)
	}
}

// autostartFallbackNote is the fallback ask's note: muster's words.
func autostartFallbackNote(why string) string {
	return "muster did not auto-start an agent on this task: " + why + "."
}

// The Stop answer's refusals.
const (
	autostartStopFailed = "Not stopped: muster could not stop the agent. Try again, or stop it from its page."
	autostartStopGone   = "Not stopped: that agent is no longer this task's."
)

// autostartStopForAnswer stops the agent an auto-start started, when answer is
// its Stop ask's one option — BEFORE the answer is recorded, so a stop that
// failed leaves the ask open with its sentence. nil for every other answer and
// ask (the ordinary answer path, untouched).
//
// 🔴 THE ASK IS IDENTIFIED BY STRUCTURE, NEVER BY ITS WORDS: muster's own asker
// (System), a task link, the tapped answer one of its stored options, and an
// auto-start recorded with this ask's id. Stopping is the safe direction, so
// any of the owner's answer doors may do it.
func (s *Server) autostartStopForAnswer(ctx context.Context, id int64, answer string) error {
	store := s.autostarts()
	// The provisioner barrier (provisioner_seam_test.go): this body calls the
	// provisioner, so it nil-checks it.
	if store == nil || s.ext.Attention == nil || s.ext.Agents == nil || s.ext.Provisioner == nil {
		return nil
	}
	if sameWords(answer) != autostartStopLabel {
		return nil
	}
	a, err := s.ext.Attention.Get(ctx, id)
	if err != nil || a.Status != attention.Open || !a.Asker.System || a.TaskID <= 0 {
		return nil
	}
	offered := false
	for _, o := range a.Options {
		offered = offered || sameWords(o) == autostartStopLabel
	}
	if !offered {
		return nil
	}
	run, ok, err := store.AutostartByAsk(ctx, id)
	if err != nil {
		s.logger.Printf("auto-start: ask %d: read its start: %v", id, err)
		return errReleaseRefused(autostartStopFailed)
	}
	if !ok || run.TaskID != a.TaskID {
		return nil
	}
	ag, err := s.ext.Agents.Get(ctx, run.AgentID)
	if err != nil || ag.OwnerID != a.Owner || ag.NoteID == nil || *ag.NoteID != run.TaskID {
		return errReleaseRefused(autostartStopGone)
	}
	if agentLive(ag.Status) {
		if err := s.ext.Provisioner.Stop(context.WithoutCancel(ctx), ag.ID); err != nil {
			s.logger.Printf("auto-start: ask %d: stop agent %d (%s): %v", id, ag.ID, ag.Name, err)
			return errReleaseRefused(autostartStopFailed)
		}
		s.clearAgentWork(ag.ID)
		s.agentChangedFor(ag.OwnerID, ag.Name)
	}
	if _, _, err := s.addTaskComment(ctx, commentByMuster, notes.Comment{NoteID: run.TaskID, Author: ownerReleaseAuthor,
		Body: "Auto-start: you stopped the agent (" + ag.Name + ") muster had started on this task. Start it again from its page if you change your mind."}); err != nil {
		s.logger.Printf("auto-start: task %d: comment the stop: %v", run.TaskID, err)
	}
	s.logger.Printf("auto-start: ask %d: stopped agent %d (%s) on task %d", id, ag.ID, ag.Name, run.TaskID)
	return nil
}

// --- the owner's card ----------------------------------------------------------

// AutostartStepUpWhy is the confirm dialog's sentence for turning auto-start on.
func AutostartStepUpWhy() string {
	return "Turning on auto-start lets this plugin's tasks start agents with no tap from you, so it needs a sign-in from the last " +
		stepup.WindowText() + "."
}

// autostartDefaultCap is the cap the card's form proposes.
const autostartDefaultCap = 3

// autostartAvailable is why auto-start cannot be turned on here, "" when it
// can: agent_dispatch's deployment refusal for the owner (the same
// predicate the start runs, so the card and the start cannot disagree).
func (s *Server) autostartUnavailable(ctx context.Context) string {
	p, _ := authz.From(ctx)
	return s.chiefDispatchDeploymentRefusal(p.UserID)
}

// pluginAutostartHTML is a live plugin's Auto-start control on its /plugins
// card, and whether it draws a form that needs a fresh sign-in (Turn on).
func (s *Server) pluginAutostartHTML(r *http.Request, p plugins.Plugin) (string, bool) {
	store := s.autostarts()
	if store == nil || !p.Has(plugins.CapMessages) {
		return "", false
	}
	ctx := r.Context()
	esc := html.EscapeString
	id := strconv.FormatInt(p.ID, 10)
	cfg, err := store.AutostartOf(ctx, p.ID)
	if err != nil {
		s.logger.Printf("plugins: page auto-start %d: %v", p.ID, err)
		return problemNode("This plugin's auto-start could not be read."), false
	}
	if cfg.On {
		return `<div class="flex flex-col gap-1" data-plugin-autostart="on">
<span class="text-xs text-fg2">Auto-start is on: each task this plugin files starts an agent right away (` + esc(autostartTargetWords(cfg.Target)) +
			`, in ` + esc(cfg.Repo) + `), at most ` + strconv.Itoa(cfg.DailyCap) + ` a day and within your chief's allowance. You are told each time, and can stop it.</span>
<form method="POST" action="` + pluginsPath + `/` + id + `/autostart" data-plugin-autostart-off><input type="hidden" name="on" value="0"><button type="submit" class="` + flowsQuietButton + `">Turn off auto-start</button></form>
</div>
`, false
	}
	if why := s.autostartUnavailable(ctx); why != "" {
		sheet := ui.InfoSheet{ID: "plugin-autostart-setup-" + id, Label: "Why auto-start is not available", Heading: "Auto-start",
			Lead:     "Auto-start needs muster on Kubernetes, with your chief allowed to start agents for you (" + why + ").",
			Code:     "MUSTER_AGENT_PROVISIONER=kubernetes\nMUSTER_TASK_ROUTING=1\nMUSTER_CHIEF_DISPATCH_OWNERS=<your user id>",
			Withheld: !s.shellFeatures(ctx).Setup, Inline: true}
		return `<div class="flex items-center gap-1" data-plugin-autostart="unavailable"><span class="text-xs text-fg2">Auto-start is not available on this server.</span>` +
			ui.InfoSheetHTML(sheet) + `</div>
`, false
	}
	targets := s.dispatchTargetsNow(ctx).Pods
	repos, rerr := s.githubRepos(ctx)
	if len(targets) == 0 || rerr != nil || len(repos) == 0 {
		return `<span class="text-xs text-fg2" data-plugin-autostart="needs-setup">Auto-start is off. To use it, connect GitHub with at least one repository and have an agent kind you can run.</span>
`, false
	}
	var b strings.Builder
	b.WriteString(`<details class="flex flex-col gap-2" data-plugin-autostart="off">
<summary class="min-h-[44px] cursor-pointer text-sm text-fg">Auto-start is off: start agents on this plugin's tasks automatically</summary>
<form method="POST" action="` + pluginsPath + `/` + id + `/autostart" class="flex flex-col gap-2" data-step-up-form data-plugin-autostart-on>
<input type="hidden" name="on" value="1">
<p class="text-xs text-fg2">When this plugin files a task, muster starts an agent on it right away, with no tap from you. The task's text is treated as outside data, never as instructions. Each start counts against your chief's allowance, and you get a notice you can stop it from.</p>
<label for="plugin-autostart-target-` + id + `" class="text-sm font-medium text-fg">Run it with</label>
<select id="plugin-autostart-target-` + id + `" name="target" class="` + flowsInputClass + `">`)
	for _, t := range targets {
		b.WriteString(`<option value="` + esc(t) + `">` + esc(autostartTargetWords(t)) + `</option>`)
	}
	b.WriteString(`</select>
<label for="plugin-autostart-repo-` + id + `" class="text-sm font-medium text-fg">In the repository</label>
<select id="plugin-autostart-repo-` + id + `" name="repo" class="` + flowsInputClass + `">`)
	for _, rp := range repos {
		b.WriteString(`<option value="` + esc(rp.FullName) + `">` + esc(rp.FullName) + `</option>`)
	}
	b.WriteString(`</select>
<label for="plugin-autostart-cap-` + id + `" class="text-sm font-medium text-fg">At most this many a day</label>
<input id="plugin-autostart-cap-` + id + `" name="daily_cap" type="number" inputmode="numeric" min="1" max="` + strconv.Itoa(plugins.MaxAutostartDailyCap) +
		`" value="` + strconv.Itoa(autostartDefaultCap) + `" class="` + flowsInputClass + `">
<button type="submit" class="` + flowsButtonClass + `">Turn on auto-start</button>
</form>
</details>
`)
	return b.String(), true
}

// handlePluginAutostart is POST /plugins/{id}/autostart {on: "1" turns it on
// with target, repo and daily_cap; anything else off}. Turning it ON needs a
// fresh sign-in (step-up), the deployment's ability to start agents for the
// owner (autostartUnavailable), a target it can start now and a repository the
// owner connected; turning it OFF needs only the signed-in session. 404 as
// revoke's.
func (s *Server) handlePluginAutostart(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	store := s.autostarts()
	if err != nil || id <= 0 || store == nil {
		s.renderPluginsPage(w, r, http.StatusNotFound, "", "No such plugin.")
		return
	}
	ctx := r.Context()
	if r.PostFormValue("on") != "1" {
		changed, err := store.ClearAutostart(ctx, id)
		if !s.autostartWriteOK(w, r, err) {
			return
		}
		if changed {
			s.logger.Printf("plugins: plugin %d: auto-start turned off by its owner", id)
		}
		http.Redirect(w, r, pluginsPath, http.StatusSeeOther)
		return
	}
	if !stepup.Fresh(ctx, s.stepUpNow()) {
		if isDocumentNavigation(r) {
			w.Header().Set("X-Muster-Step-Up", "required")
			s.renderPluginsPage(w, r, http.StatusForbidden, "", "Turning on auto-start needs a recent sign-in. Confirm it's you, then turn it on again.")
			return
		}
		refuseStale(w)
		return
	}
	if why := s.autostartUnavailable(ctx); why != "" {
		s.renderPluginsPage(w, r, http.StatusConflict, "", "Auto-start is not available on this server: "+why+".")
		return
	}
	target := strings.TrimSpace(r.PostFormValue("target"))
	known := false
	for _, t := range s.dispatchTargetsNow(ctx).Pods {
		known = known || t == target
	}
	if !known {
		s.renderPluginsPage(w, r, http.StatusBadRequest, "", "Pick one of the agent kinds offered.")
		return
	}
	repo, why := s.connectedRepoName(ctx, strings.TrimSpace(r.PostFormValue("repo")))
	if why != "" {
		s.renderPluginsPage(w, r, http.StatusBadRequest, "", "Pick one of your connected repositories.")
		return
	}
	dailyCap, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("daily_cap")))
	if err != nil || dailyCap < 1 || dailyCap > plugins.MaxAutostartDailyCap {
		s.renderPluginsPage(w, r, http.StatusBadRequest, "", "The daily limit must be 1 to "+strconv.Itoa(plugins.MaxAutostartDailyCap)+".")
		return
	}
	_, err = store.SetAutostart(ctx, id, target, repo, dailyCap)
	if !s.autostartWriteOK(w, r, err) {
		return
	}
	s.logger.Printf("plugins: plugin %d: auto-start turned on by its owner (%s, %s, %d a day)", id, target, repo, dailyCap)
	http.Redirect(w, r, pluginsPath, http.StatusSeeOther)
}

// autostartWriteOK answers a failed auto-start write, reporting whether err
// was nil.
func (s *Server) autostartWriteOK(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, plugins.ErrNotFound):
		s.renderPluginsPage(w, r, http.StatusNotFound, "", "No such plugin.")
	case errors.Is(err, plugins.ErrAutostartDoor):
		s.renderPluginsPage(w, r, http.StatusForbidden, "", "Only you, signed in, can change auto-start.")
	case errors.Is(err, plugins.ErrInvalid):
		s.renderPluginsPage(w, r, http.StatusBadRequest, "", strings.TrimPrefix(err.Error(), plugins.ErrInvalid.Error()+": "))
	default:
		s.logger.Printf("plugins: auto-start write: %v", err)
		s.renderPluginsPage(w, r, http.StatusInternalServerError, "", "Auto-start could not be changed.")
	}
	return false
}
