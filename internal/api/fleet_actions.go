package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/fleet"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/prroute"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// FLEET PROPOSALS (MUSTER_FLEET_ORCHESTRATION=propose; internal/fleet
// actions.go; docs/chief.md "Fleet proposals").
//
// The chief orchestrates its owner's fleet by PROPOSING: each write tool here
// files one fleet_actions row — a fixed muster-built text, or the chief's own
// words — and the owner's tap on its card in the fleet digest sends it. In
// this phase NOTHING is sent without that tap.
//
//   - session_instruct {subject, class, text?, reason}: continue, handoff,
//     stop_note (fixed texts) or custom (the chief's text, at most 4 KiB).
//   - session_wrap {subject, reason}: ONE card, "Write handoff, then close":
//     the handoff text, then the window closed once its handoff turn ended.
//   - work_reassign {task_id, from, to, reason}: ONE card, two steps: the task
//     routed to `to`, then `from` told it moved and asked for its handoff.
//   - fleet_actions_list {since?, state?, subject?}: the log, read-only.
//
// 🔴 OWNER-SCOPED, AND ONE ANSWER FOR EVERY OTHER SUBJECT. A subject is a
// session of the owner's OWN fleet (the fleet reader: own hosts, own agents),
// never the chief, never an officer's instance. Another owner's session, a
// team's window, the chief or an officer is `not_found` — the same answer for
// each, so ids are not probed.
//
// 🔴 A PROPOSAL IS FILED ON A STOP: the subject must have an open episode (a
// stop the session-state pass opened). The episode, its idle streak and the
// pull request's head are recorded, and re-read at the tap and at the paste
// (fleetStillOn): anything else is `suppressed:moved_on`.
//
// 🔴 OFF (the flag unset): no tool listed, no card, no fleet_actions write —
// every door below checks fleetProposeOn first.
// ---------------------------------------------------------------------------

// The tools' names.
const (
	sessionInstructTool  = "session_instruct"
	sessionWrapTool      = "session_wrap"
	workReassignTool     = "work_reassign"
	fleetActionsListTool = "fleet_actions_list"
)

// The machine-readable refusals every fleet write returns (`refused`).
const (
	fleetRefusedOff         = "off"
	fleetRefusedBadArgs     = "bad_arguments"
	fleetRefusedNotFound    = "not_found"
	fleetRefusedNotStopped  = "not_stopped"
	fleetRefusedViewOnly    = "view_only"
	fleetRefusedUnavailable = "unavailable"
	fleetRefusedAlready     = "already_proposed"
	fleetRefusedTooLong     = "too_long"
	fleetRefusedNotRoutable = "not_routable"
	fleetRefusedTask        = "task_not_found"
)

// fleetProposeOn reports whether fleet proposals are on: the flag, the digest
// (their cards are its), the action log, and session state (a proposal is
// filed on a stop: without episodes every call could only answer
// not_stopped, so there is no such mode — the server also refuses to boot
// with the flag and no MUSTER_SESSION_STATE).
func (s *Server) fleetProposeOn() bool {
	return s.ext.FleetPropose && s.ext.FleetDigest && s.ext.FleetActions != nil && s.sessionStateOn()
}

// fleetWriteToolDefs is the chief's fleet tools (offered only with
// fleetProposeOn; agentTools).
func fleetWriteToolDefs() []agents.ToolDef {
	subject := strProp("The session: its fleet id from fleet_list (\"mirror:<key>\" or \"agent:<id>\"). Only one of your owner's own " +
		"sessions that is STOPPED now (fleet_list state stalled, needs_you or waiting).")
	reason := strProp("Your one-line reason, shown on the card (at most 300 characters).")
	return []agents.ToolDef{
		{Type: "function", Name: sessionInstructTool,
			Description: "PROPOSE a message to one of your owner's stopped sessions. muster puts a card in the fleet digest showing " +
				"EXACTLY what will be sent; NOTHING is sent until your owner taps Approve (or edits it and sends it as their own). " +
				"class: continue (the fixed text \"" + fleet.ContinueText + "\"), handoff (the fixed handoff text, naming its " +
				"merged pull request when muster knows it), stop_note (\"" + fleet.StopNoteText + "\") or custom (your own text, " +
				"at most 4 KiB). Every pasted text starts with muster's line \"[Chief, approved by <owner> at HH:MM UTC]\". At most one " +
				"open proposal per session and class (a second answers refused: already_proposed). Returns {action_id, state, refused?, card?}.",
			Parameters: objSchema(map[string]any{
				"subject": subject,
				"class":   map[string]any{"type": "string", "enum": fleet.InstructClasses, "description": "What to send."},
				"text":    strProp("Your text, ONLY with class custom (at most 4 KiB)."),
				"reason":  reason,
			}, "subject", "class", "reason")},
		{Type: "function", Name: sessionWrapTool,
			Description: "PROPOSE wrapping up one of your owner's finished sessions: ONE card, \"Write handoff, then close\". On your " +
				"owner's tap muster sends the fixed handoff text; once the session's handoff turn has ended (it worked and stopped " +
				"again), muster closes its window (a Claude Code window gets /exit at an idle prompt; an agent is stopped and " +
				"removed) — unless it then has an open pull request, a queued message or background work, which leaves it open and " +
				"says so. Nothing happens without the tap. Returns {action_id, state, refused?, card?}.",
			Parameters: objSchema(map[string]any{"subject": subject, "reason": reason}, "subject", "reason")},
		{Type: "function", Name: workReassignTool,
			Description: "PROPOSE moving one of your owner's tasks from one session to another: ONE card, two steps. On your owner's " +
				"tap muster routes the task to `to` (a mirrored Claude Code session, as route_to_session does), then tells `from` " +
				"the task moved and asks it for its handoff. `from` must be stopped now. Nothing happens without the tap. Returns " +
				"{action_id, state, refused?, card?}.",
			Parameters: objSchema(map[string]any{
				"task_id": intProp("The task's id (one of your owner's tasks).", 1, 1<<62),
				"from":    strProp("The session the task leaves: its fleet id (\"mirror:<key>\" or \"agent:<id>\"), stopped now."),
				"to":      strProp("The session the task goes to: a mirrored Claude Code session's fleet id (\"mirror:<key>\"). For NEW work, use agent_dispatch's own card and a session_wrap or session_instruct handoff card instead."),
				"reason":  reason,
			}, "task_id", "from", "to", "reason")},
		sessionAnswerToolDef(),
		{Type: "function", Name: fleetActionsListTool,
			Description: "Read your fleet proposals and what became of them (the fleet's action log): each {action_id, class, " +
				"subject, state (proposed, approved, rejected, expired, queued, sent, failed, suppressed:<why>), outcome, reason, " +
				"text, created_at, decided_at}. Newest first. Read-only: use it to explain to your owner what you proposed.",
			Parameters: objSchema(map[string]any{
				"since":   strProp("RFC 3339 time; only actions created at or after it (default: the last 7 days)."),
				"state":   strProp("Only actions in this state."),
				"subject": strProp("Only actions on this session (its fleet id)."),
			})},
	}
}

// fleetToolResult is every fleet write's answer: {action_id, state, refused?,
// card?}, plus a sentence for the chief.
func fleetToolResult(a fleet.Action, refused, why string) string {
	m := map[string]any{}
	if refused != "" {
		m["state"] = "refused"
		m["refused"] = refused
		m["error"] = why
		return toolJSON(m)
	}
	m["action_id"] = a.ID
	m["state"] = a.State
	m["card"] = fleet.ActionObject(a.ID)
	m["next"] = "muster put the card in the fleet digest. NOTHING is sent until your owner taps Approve; tell them what you proposed and why."
	return toolJSON(m)
}

// dispatchFleetTool runs a fleet tool; ok false when name is not one.
func (s *Server) dispatchFleetTool(ctx context.Context, name, arguments string) (string, bool) {
	switch name {
	case sessionInstructTool, sessionWrapTool, workReassignTool, fleetActionsListTool, sessionAnswerTool:
	default:
		return "", false
	}
	// 🔴 RE-CHECKED AT THE CALL: a gateway runtime can name a tool it was
	// never offered. A TEAM's chief has none in this phase (the plan: "a
	// team's chief gets none of these tools in v1").
	if !s.fleetProposeOn() || s.ownerIsTeam(ctx, requestOwner(ctx)) {
		return fleetToolResult(fleet.Action{}, fleetRefusedOff, "fleet proposals are off on this server (MUSTER_FLEET_ORCHESTRATION)"), true
	}
	switch name {
	case sessionInstructTool:
		return s.toolSessionInstruct(ctx, arguments), true
	case sessionWrapTool:
		return s.toolSessionWrap(ctx, arguments), true
	case workReassignTool:
		return s.toolWorkReassign(ctx, arguments), true
	case sessionAnswerTool:
		return s.toolSessionAnswer(ctx, arguments), true
	}
	return s.toolFleetActionsList(ctx, arguments), true
}

// fleetNotFound is the one answer for a subject that is not a stopped session
// of the owner's own fleet's — another owner's, a team's, the chief, an
// officer's, or gone.
const fleetNotFoundWhy = "not one of your owner's sessions muster can see now (fleet_list names them)"

// fleetStoppedSubject is the owner's own session id, with its open stop: ok
// false (refused, why) for anything else.
func (s *Server) fleetStoppedSubject(ctx context.Context, id string) (fleetSubject, string, string) {
	sub, ok := s.fleetSubjectFor(ctx, strings.TrimSpace(id))
	if !ok || s.fleetSubjectGuarded(ctx, sub) {
		return fleetSubject{}, fleetRefusedNotFound, fleetNotFoundWhy
	}
	if sub.episode == nil {
		return fleetSubject{}, fleetRefusedNotStopped, "the session is not stopped now (fleet_list state working): muster proposes only on a stop"
	}
	return sub, "", ""
}

// fleetSubjectGuarded reports whether sub is never a fleet subject: an
// officer's instance — a dispatched one (an agent the destroy paths protect)
// or a HOST-SPAWNED one, which the fleet sees as a mirrored window: a window
// the officers' spawn store recorded (its id from the host's own spawn reply,
// never a tag; hostOfficerWindowsOpen's read). A read that fails guards.
func (s *Server) fleetSubjectGuarded(ctx context.Context, sub fleetSubject) bool {
	if sub.kind == chiefnudge.KindMirror {
		officer, err := s.fleetHostOfficerWindow(ctx, sub.view.Host, sub.view.Window)
		return err != nil || officer
	}
	if sub.kind != chiefnudge.KindAgent {
		return false
	}
	officer, err := s.fleetOfficerAgent(ctx, sub.agent.ID)
	return err != nil || officer
}

// fleetHostOfficerWindow reports whether host's window is one the officer
// runtime spawned (the spawn store's recorded window).
func (s *Server) fleetHostOfficerWindow(ctx context.Context, host, window string) (bool, error) {
	st := s.officerHostSpawns()
	if st == nil || window == "" {
		return false, nil
	}
	rows, err := st.HostSpawnsInWindows(s.officerPrincipal(ctx, requestOwner(ctx)), host, []string{window})
	return len(rows) > 0, err
}

// fleetOfficerAgent reports whether agent id is an officer's instance (or
// another agent the destroy paths protect), read from the agent table.
func (s *Server) fleetOfficerAgent(ctx context.Context, id int64) (bool, error) {
	a, protected, err := s.agentIsDeleteProtected(ctx, id)
	if err != nil || protected {
		return protected, err
	}
	_, _, officer := s.officerOf(a)
	return officer, nil
}

// fleetActionGuarded is fleetSubjectGuarded re-read for action a at the tap
// and the paste (fleetStillOn): an officer's window or instance is never sent
// to, even when it became one after the card was filed. A read that fails is
// unknown (retried), never a send.
func (s *Server) fleetActionGuarded(ctx context.Context, a fleet.Action) nudgeState {
	var officer bool
	var err error
	switch a.SubjectKind {
	case chiefnudge.KindMirror:
		officer, err = s.fleetHostOfficerWindow(ctx, a.Host, a.Window)
	case chiefnudge.KindAgent:
		officer, err = s.fleetOfficerAgent(ctx, a.AgentID)
	}
	switch {
	case err != nil:
		return nudgeUnknown
	case officer:
		return nudgeMovedAway
	}
	return nudgeStalled
}

// fleetProposalFor is the proposal of class with text on stopped subject sub.
func (s *Server) fleetProposalFor(sub fleetSubject, class, text, reason string) fleet.Proposal {
	ep := sub.episode
	p := fleet.Proposal{Class: class, Proposer: fleet.ProposerChief, SubjectKind: sub.kind, SubjectKey: ep.Key,
		Host: ep.Host, TmuxSession: ep.TmuxSession, Window: ep.Window, AgentID: ep.AgentID, ChatSessionID: ep.ChatSessionID,
		EpisodeID: ep.ID, StreakKey: ep.StreakKey, Reason: reason, Text: text,
		Evidence: fleet.Evidence{SubjectID: sub.row.ID, Label: cutRunes(firstNonBlank(sub.row.Name, sub.row.ID), 120), Runtime: fleetRuntime(sub)}}
	if len(sub.prs) > 0 {
		pr := sub.prs[0]
		p.Evidence.PRRepo, p.Evidence.PRNumber, p.Evidence.PRState, p.Evidence.PRHead = pr.Repo, pr.Number, pr.State, pr.HeadSHA12
	}
	return p
}

// fleetRuntime is sub's runtime as the evidence names it.
func fleetRuntime(sub fleetSubject) string {
	if sub.kind == chiefnudge.KindAgent {
		return "pod"
	}
	return sub.view.Runtime
}

// fleetFile writes p, after checking its subject can be delivered to at all
// (fleetTarget): a view-only pane is refused here, never carded.
func (s *Server) fleetFile(ctx context.Context, sub fleetSubject, p fleet.Proposal) string {
	if t := s.fleetTarget(ctx, fleetActionOf(p)); t.refused != "" {
		return fleetToolResult(fleet.Action{}, t.refused, t.why)
	}
	a, err := s.ext.FleetActions.Propose(ctx, p)
	switch {
	case errors.Is(err, fleet.ErrAlreadyProposed):
		return fleetToolResult(fleet.Action{}, fleetRefusedAlready, "an open "+p.Class+" proposal for this session is already waiting for your owner's tap")
	case err != nil:
		s.logger.Printf("fleet: propose %s on %s: %v", p.Class, sub.row.ID, err)
		return fleetToolResult(fleet.Action{}, fleetRefusedUnavailable, "the proposal could not be recorded")
	}
	s.fleetChanged(requestOwner(ctx))
	return fleetToolResult(a, "", "")
}

// fleetActionOf is p as the action it would be (for the target resolver).
func fleetActionOf(p fleet.Proposal) fleet.Action {
	return fleet.Action{Class: p.Class, SubjectKind: p.SubjectKind, SubjectKey: p.SubjectKey, Host: p.Host, TmuxSession: p.TmuxSession,
		Window: p.Window, AgentID: p.AgentID, ChatSessionID: p.ChatSessionID, EpisodeID: p.EpisodeID, StreakKey: p.StreakKey, Evidence: p.Evidence}
}

func (s *Server) toolSessionInstruct(ctx context.Context, arguments string) string {
	var in struct {
		Subject string  `json:"subject"`
		Class   string  `json:"class"`
		Text    *string `json:"text"`
		Reason  string  `json:"reason"`
	}
	dec := json.NewDecoder(strings.NewReader(orEmptyJSON(arguments)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || strings.TrimSpace(in.Subject) == "" {
		return fleetToolResult(fleet.Action{}, fleetRefusedBadArgs, "arguments are {subject, class, text?, reason}")
	}
	if (in.Class == fleet.ClassCustom) != (in.Text != nil) {
		return fleetToolResult(fleet.Action{}, fleetRefusedBadArgs, "text goes with class custom, and only with it")
	}
	sub, refused, why := s.fleetStoppedSubject(ctx, in.Subject)
	if refused != "" {
		return fleetToolResult(fleet.Action{}, refused, why)
	}
	var text string
	switch in.Class {
	case fleet.ClassContinue:
		text = fleet.ContinueText
	case fleet.ClassStopNote:
		text = fleet.StopNoteText
	case fleet.ClassHandoff:
		text = fleetHandoffText(sub)
	case fleet.ClassCustom:
		t, ok := cleanPrompt(*in.Text)
		switch {
		case !ok || len(t) > fleet.MaxCustomBytes:
			return fleetToolResult(fleet.Action{}, fleetRefusedTooLong, "text is at most 4 KiB")
		case t == "":
			return fleetToolResult(fleet.Action{}, fleetRefusedBadArgs, "text is empty")
		}
		text = t
	default:
		return fleetToolResult(fleet.Action{}, fleetRefusedBadArgs, "class is one of continue, handoff, stop_note, custom")
	}
	return s.fleetFile(ctx, sub, s.fleetProposalFor(sub, in.Class, text, in.Reason))
}

// fleetHandoffText is the handoff text for sub: naming its latest pull
// request when muster read it merged or closed.
func fleetHandoffText(sub fleetSubject) string {
	if len(sub.prs) > 0 && (sub.prs[0].State == fleet.PRMerged || sub.prs[0].State == fleet.PRClosed) {
		return fleet.HandoffText(sub.prs[0].Number, sub.prs[0].State)
	}
	return fleet.HandoffText(0, "")
}

func (s *Server) toolSessionWrap(ctx context.Context, arguments string) string {
	var in struct {
		Subject string `json:"subject"`
		Reason  string `json:"reason"`
	}
	dec := json.NewDecoder(strings.NewReader(orEmptyJSON(arguments)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || strings.TrimSpace(in.Subject) == "" {
		return fleetToolResult(fleet.Action{}, fleetRefusedBadArgs, "arguments are {subject, reason}")
	}
	sub, refused, why := s.fleetStoppedSubject(ctx, in.Subject)
	if refused != "" {
		return fleetToolResult(fleet.Action{}, refused, why)
	}
	return s.fleetFile(ctx, sub, s.fleetProposalFor(sub, fleet.ClassWrap, fleetHandoffText(sub), in.Reason))
}

func (s *Server) toolWorkReassign(ctx context.Context, arguments string) string {
	var in struct {
		TaskID int64  `json:"task_id"`
		From   string `json:"from"`
		To     string `json:"to"`
		Reason string `json:"reason"`
	}
	dec := json.NewDecoder(strings.NewReader(orEmptyJSON(arguments)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || in.TaskID <= 0 || strings.TrimSpace(in.From) == "" || strings.TrimSpace(in.To) == "" {
		return fleetToolResult(fleet.Action{}, fleetRefusedBadArgs, "arguments are {task_id, from, to, reason}")
	}
	chief, err := s.chiefAgent(ctx)
	if err != nil {
		return fleetToolResult(fleet.Action{}, fleetRefusedUnavailable, "your owner's chief could not be read")
	}
	if _, ok := s.ownersTask(ctx, chief, in.TaskID); !ok {
		return fleetToolResult(fleet.Action{}, fleetRefusedTask, "task_id "+strconv.FormatInt(in.TaskID, 10)+" is not one of your owner's tasks")
	}
	sub, refused, why := s.fleetStoppedSubject(ctx, in.From)
	if refused != "" {
		return fleetToolResult(fleet.Action{}, refused, why)
	}
	to, ok := s.fleetSubjectFor(ctx, strings.TrimSpace(in.To))
	if !ok || s.fleetSubjectGuarded(ctx, to) || to.row.ID == sub.row.ID {
		return fleetToolResult(fleet.Action{}, fleetRefusedNotFound, "to is "+fleetNotFoundWhy)
	}
	if !routesOn(s.ext) || to.kind != chiefnudge.KindMirror {
		return fleetToolResult(fleet.Action{}, fleetRefusedNotRoutable, "to must be a mirrored Claude Code session, with session routing on")
	}
	if _, refusedRoute := s.resolveRouteTarget(ctx, focus.Subject{Kind: focus.KindMirror, Key: to.key}); refusedRoute != "" {
		return fleetToolResult(fleet.Action{}, fleetRefusedNotRoutable, "to cannot take a routed task now")
	}
	p := s.fleetProposalFor(sub, fleet.ClassReassign, fleet.ReassignHandoffText(in.TaskID, firstNonBlank(to.row.Name, to.row.ID)), in.Reason)
	p.TaskID, p.ToSubject = in.TaskID, to.row.ID
	return s.fleetFile(ctx, sub, p)
}

func (s *Server) toolFleetActionsList(ctx context.Context, arguments string) string {
	var in struct {
		Since   string `json:"since"`
		State   string `json:"state"`
		Subject string `json:"subject"`
	}
	if err := json.Unmarshal([]byte(orEmptyJSON(arguments)), &in); err != nil {
		return toolErr("arguments are {since?, state?, subject?}")
	}
	since := s.now().Add(-7 * 24 * time.Hour)
	if in.Since != "" {
		t, err := time.Parse(time.RFC3339, in.Since)
		if err != nil {
			return toolErr("since is an RFC 3339 time")
		}
		since = t
	}
	// 🔴 THE SUBJECT IS MATCHED IN THE READ, EXACTLY, BEFORE ITS LIMIT
	// (fleet.ListFilter.Subject): a fleet id "mirror:<key>" or "agent:<id>"
	// is the log's own kind and key, and agent:4 is never agent:42.
	subject := strings.TrimSpace(in.Subject)
	if subject != "" {
		k, key, ok := strings.Cut(subject, ":")
		if !ok || key == "" || (k != chiefnudge.KindMirror && k != chiefnudge.KindAgent) {
			return toolErr("subject is a fleet id: mirror:<key> or agent:<id>")
		}
	}
	list, err := s.ext.FleetActions.ListActions(ctx, fleet.ListFilter{Since: since, State: strings.TrimSpace(in.State), Subject: subject, Limit: 100})
	if err != nil {
		return toolErr("the fleet's action log could not be read")
	}
	type row struct {
		ID        int64  `json:"action_id"`
		Class     string `json:"class"`
		Subject   string `json:"subject"`
		Label     string `json:"label,omitempty"`
		State     string `json:"state"`
		Outcome   string `json:"outcome,omitempty"`
		Reason    string `json:"reason,omitempty"`
		Text      string `json:"text"`
		TaskID    int64  `json:"task_id,omitempty"`
		To        string `json:"to,omitempty"`
		CreatedAt string `json:"created_at"`
		DecidedAt string `json:"decided_at,omitempty"`
	}
	out := []row{}
	for _, a := range list {
		id := firstNonBlank(a.Evidence.SubjectID, a.SubjectKind+":"+a.SubjectKey)
		r := row{ID: a.ID, Class: a.Class, Subject: id, Label: a.Evidence.Label, State: a.State, Outcome: a.Outcome, Reason: a.Reason,
			Text: a.Text, TaskID: a.TaskID, To: a.ToSubject, CreatedAt: a.CreatedAt.UTC().Format(time.RFC3339)}
		if !a.DecidedAt.IsZero() {
			r.DecidedAt = a.DecidedAt.UTC().Format(time.RFC3339)
		}
		out = append(out, r)
	}
	return toolJSON(map[string]any{"actions": out})
}

// --- the target resolver ----------------------------------------------------------

// How a fleet text reaches its session (fleetTarget).
const (
	fleetDeliverQueue = "queue" // the send queue (Claude Code; a launcher-bound opencode pane, through its gated send)
	fleetDeliverTurn  = "turn"  // a pod agent's turn (sendNudgeTurn's shape)
)

// fleetDelivery is where a fleet text goes: how, and to which window or
// agent; refused (a fleetRefused* code) and why when it cannot go at all.
type fleetDelivery struct {
	how     string
	host    mirror.HostView
	window  mirror.SessionView
	agent   agents.Agent
	refused string
	why     string
}

// 🔴 THE FLEET'S OWN TARGET RESOLVER, ONE PLACE. route_to_session's targets
// are Claude Code windows only (resolveRouteTarget); a fleet text reaches
// every runtime muster can send to, each through its own door:
//
//   - a mirrored Claude Code window (one a person opened, or one muster
//     dispatched on a host): the send queue, whose host types it through the
//     Claude prompt guard;
//   - a mirrored opencode pane its host sends to (launcher-bound, the host
//     running -opencode-send): the same queue row, sent through opencode's
//     API (the gated send);
//   - any other opencode pane, or a read-only host: refused view_only (the
//     card is never filed; the session stays a digest row);
//   - a dispatched pod agent: a turn, as sendNudgeTurn starts one.
//
// TestTheFleetTargetResolverPerSubjectKind holds one row per kind;
// TestTheFleetSeamLedger pins its callers.
func (s *Server) fleetTarget(ctx context.Context, a fleet.Action) fleetDelivery {
	if a.SubjectKind == chiefnudge.KindAgent {
		if s.ext.Agents == nil || s.ext.Gateway == nil {
			return fleetDelivery{refused: fleetRefusedUnavailable, why: "this server cannot start an agent's turn"}
		}
		ag, err := s.ext.Agents.Get(ctx, a.AgentID)
		if err != nil || ag.OwnerID != requestOwner(ctx) {
			return fleetDelivery{refused: fleetRefusedNotFound, why: fleetNotFoundWhy}
		}
		if !agentCanBeSent(s.liveStatus(ctx, ag)) {
			return fleetDelivery{refused: fleetRefusedUnavailable, why: "the agent is not running"}
		}
		return fleetDelivery{how: fleetDeliverTurn, agent: ag}
	}
	if s.ext.Mirror == nil || s.ext.SendQueue == nil {
		return fleetDelivery{refused: fleetRefusedUnavailable, why: "this server has no send queue"}
	}
	var e mirror.SessionView
	found := false
	for _, x := range s.mirrorFocusEntries(ctx) {
		if x.SessionKey() == a.SubjectKey && x.Host == a.Host && x.Session == a.TmuxSession && x.Window == a.Window {
			e, found = x, true
			break
		}
	}
	if !found {
		return fleetDelivery{refused: fleetRefusedNotFound, why: "the session is not in the window it was proposed in now"}
	}
	h, ok := s.hostView(ctx, e.Host)
	if !ok {
		return fleetDelivery{refused: fleetRefusedUnavailable, why: unprefixed(mirror.ErrHostOffline)}
	}
	return fleetMirrorDelivery(h, e)
}

// fleetMirrorDelivery is fleetTarget's mirrored half, on the host's and the
// window's own facts.
func fleetMirrorDelivery(h mirror.HostView, e mirror.SessionView) fleetDelivery {
	if !h.Interactive {
		return fleetDelivery{refused: fleetRefusedViewOnly, why: "its host is read-only (not -interactive): muster types nothing into it"}
	}
	if off := mirrorQueueOff(h, e); off != "" {
		if r, ok := e.Spec(); ok && r.GatedSend {
			// An opencode pane its host does not send to: view only.
			return fleetDelivery{refused: fleetRefusedViewOnly, why: "muster can only watch this " + e.Runtime + " pane: " + off}
		}
		return fleetDelivery{refused: fleetRefusedUnavailable, why: off}
	}
	return fleetDelivery{how: fleetDeliverQueue, host: h, window: e}
}

// --- the re-read -------------------------------------------------------------------

// fleetStillOn reads a's session now against the stop it was proposed on:
// the same open episode, the same idle streak (nudgeStateNow, the nudges' one
// "moved on" predicate) and — when one was recorded — the same pull request
// head. nudgeUnknown when any of it cannot be read: never moved on.
func (s *Server) fleetStillOn(ctx context.Context, a fleet.Action) nudgeState {
	if st := s.fleetActionGuarded(ctx, a); st != nudgeStalled {
		return st
	}
	switch {
	case a.Class == fleet.ClassAnswerPick:
		// A picker on screen is a WAITING window, not an idle one: the stop
		// and the held question itself are what must still hold.
		return s.fleetPickStillOn(ctx, a)
	case a.Evidence.AskLogID > 0:
		// An answer: the held question it answers is still open.
		if st := s.fleetAskStillOpen(ctx, a); st != nudgeStalled {
			return st
		}
	}
	d := chiefnudge.Decision{Kind: a.SubjectKind, Key: a.SubjectKey, Host: a.Host, TmuxSession: a.TmuxSession, Window: a.Window,
		AgentID: a.AgentID, ChatSessionID: a.ChatSessionID, StreakKey: a.StreakKey, EpisodeID: a.EpisodeID}
	// DecidedAt stays zero: an episode PUSHED after the proposal is not moved
	// on — the push is the owner's notice, the card their answer.
	st := s.nudgeStateNow(ctx, d)
	if st != nudgeStalled || a.Evidence.PRHead == "" || a.Evidence.PRRepo == "" {
		return st
	}
	reader := s.ext.FleetPRStates
	if reader == nil || s.githubAppForOwner(ctx, requestOwner(ctx)) == nil {
		return nudgeUnknown
	}
	pr, known, _ := s.fleetPRs.read(ctx, reader, prroute.Ref{Repo: a.Evidence.PRRepo, PR: a.Evidence.PRNumber}, s.now(), fleetPRReadTimeout, fleetPRTTL, fleetPRTTL)
	switch {
	case !known:
		return nudgeUnknown
	case len(pr.HeadSHA) < 12 || pr.HeadSHA[:12] != a.Evidence.PRHead:
		return nudgeMovedOn
	}
	return nudgeStalled
}

// --- delivery ----------------------------------------------------------------------

// The approve's answers (fleetApprove), each the card's note.
const (
	FleetSent           = "Sent."
	FleetQueued         = "Queued: it is pasted once the session is idle."
	FleetMovedOn        = "Not sent: the session moved on since the chief proposed it."
	FleetDecided        = "Not sent: this proposal was already decided."
	FleetExpiredNote    = "Not sent: this proposal expired."
	FleetUnreadable     = "Not sent: muster could not read the session's state just now. Try again."
	FleetNotDeliverable = "Not sent: "
	FleetNotEditable    = "Not sent: this proposal is approved or rejected, not edited."
)

// fleetEditable reports whether a's card may be sent as the owner's own
// edited text: never a reassign (its route is the point), an agent's turn,
// a close (nothing is typed — an edit would close the session and drop the
// owner's text) or a pick (an option is typed into a picker, not a text: an
// edit would type the chief's option and drop the owner's words). ONE
// predicate: the card's Edit form and the approve.
func fleetEditable(a fleet.Action) bool {
	return a.Class != fleet.ClassReassign && a.Class != fleet.ClassClose && a.Class != fleet.ClassAnswerPick && a.SubjectKind != chiefnudge.KindAgent
}

// fleetApproveResult is one approve: its note, and whether it moved on.
type fleetApproveResult struct {
	note    string
	sent    bool
	movedOn bool
}

// fleetApprove approves the principal's proposal id and sends it: the
// re-read first (moved on: suppressed, nothing sent), then the claim
// (proposed → approved, conditional: a second tap finds it decided), then the
// delivery. edited, when non-nil, is the owner's own text in place of the
// proposal's: sent as THEIR message, with no chief line (the caller checked
// the step-up).
func (s *Server) fleetApprove(ctx context.Context, id int64, via string, edited *string) fleetApproveResult {
	st := s.ext.FleetActions
	a, err := st.GetAction(ctx, id)
	if err != nil {
		return fleetApproveResult{note: FleetDecided}
	}
	if a.State != fleet.StateProposed {
		return fleetApproveResult{note: FleetDecided}
	}
	if edited != nil && !fleetEditable(a) {
		return fleetApproveResult{note: FleetNotEditable}
	}
	if fleet.Expired(a.CreatedAt, s.now()) {
		s.fleetMove(ctx, a, fleet.Move{State: fleet.StateExpired, Outcome: fleetTTLOutcome, Why: fleet.WhyTTL, Finish: true})
		return fleetApproveResult{note: FleetExpiredNote}
	}
	switch s.fleetStillOn(ctx, a) {
	case nudgeUnknown:
		return fleetApproveResult{note: FleetUnreadable}
	case nudgeMovedOn, nudgeMovedAway:
		s.fleetMove(ctx, a, fleet.Move{State: fleet.Suppressed(fleet.GuardMovedOn), Outcome: "the session moved on before the tap", Finish: true})
		return fleetApproveResult{note: FleetMovedOn, movedOn: true}
	}
	a, ok := s.fleetMove(ctx, a, fleet.Move{State: fleet.StateApproved, DecidedVia: via, DecidedBy: requestOwner(ctx)}, fleet.StateProposed)
	if !ok {
		return fleetApproveResult{note: FleetDecided}
	}
	if a.Class == fleet.ClassReassign {
		if why := s.fleetRouteTask(ctx, &a); why != "" {
			s.fleetMove(ctx, a, fleet.Move{State: fleet.StateFailed, Outcome: why, Finish: true}, fleet.StateApproved)
			return fleetApproveResult{note: FleetNotDeliverable + why}
		}
	}
	switch a.Class {
	case fleet.ClassAnswerPick:
		// Typed into the picker as the owner's tap types it: no text, no
		// attribution line (a picker takes an option, not a paste).
		return s.fleetDeliverPick(ctx, a)
	case fleet.ClassClose:
		return s.fleetDeliverClose(ctx, a)
	}
	body := fleet.Body(s.personName(ctx, requestOwner(ctx)), s.now(), a.Text)
	if edited != nil {
		body = *edited
	}
	return s.fleetDeliver(ctx, a, body, edited != nil)
}

// fleetDeliver sends approved action a's body through its target.
func (s *Server) fleetDeliver(ctx context.Context, a fleet.Action, body string, owners bool) fleetApproveResult {
	t := s.fleetTarget(ctx, a)
	switch t.how {
	case fleetDeliverQueue:
		it := sendqueue.Item{Target: sendqueue.Target{Host: t.window.Host, Session: t.window.Session, Window: t.window.Window}, Body: body}
		st := s.ext.Mirror.AgentState(ctx, t.window.Host, t.window.Session, t.window.Window)
		it.AfterSince = st.Pasted
		if owners {
			// 🔴 THE OWNER'S OWN TEXT: a person's row, gated as one — the
			// sign-in of the request that sent it (the caller checked it is
			// fresh), no origin, editable and retryable like any other.
			it.StepUpAt, _ = stepup.AuthTime(ctx)
		} else {
			// 🔴 A FLEET ROW: its origin gates it (sendQueueFleetGate), with
			// a server-stamped sign-in time; never edited, never retried.
			it.StepUpAt, it.FleetActionID = s.sendQueueNow(), a.ID
		}
		row, err := s.ext.SendQueue.Enqueue(ctx, it)
		if err != nil {
			why := "it could not be queued: " + unprefixed(err)
			s.fleetMove(ctx, a, fleet.Move{State: fleet.StateFailed, Outcome: why, Finish: true}, fleet.StateApproved)
			return fleetApproveResult{note: FleetNotDeliverable + why}
		}
		m := fleet.Move{State: fleet.StateQueued, SendQueueID: row.ID}
		if owners {
			m.Outcome = "edited: sent as the owner's own message"
		}
		s.fleetMove(ctx, a, m, fleet.StateApproved)
		s.kickSendQueue()
		return fleetApproveResult{note: FleetQueued, sent: true}
	case fleetDeliverTurn:
		// 🔴 A POD AGENT'S TURN STARTS ON THE PASS, NEVER ON THE TAP: the card's
		// routes do not reach the agent gateway (which a deployment may leave
		// off), so the approval queues the action and the session-state pass
		// starts the turn (fleetStartTurn), re-reading the stop first.
		if owners {
			s.fleetMove(ctx, a, fleet.Move{State: fleet.StateFailed, Outcome: "an agent's proposal is not edited", Finish: true}, fleet.StateApproved)
			return fleetApproveResult{note: FleetNotDeliverable + "an agent's proposal is approved or rejected, not edited"}
		}
		s.fleetMove(ctx, a, fleet.Move{State: fleet.StateQueued, Outcome: "its turn starts on the next session-state pass"}, fleet.StateApproved)
		return fleetApproveResult{note: FleetQueued, sent: true}
	}
	s.fleetMove(ctx, a, fleet.Move{State: fleet.StateFailed, Outcome: t.why, Finish: true}, fleet.StateApproved)
	return fleetApproveResult{note: FleetNotDeliverable + t.why}
}

// fleetRouteTask is a reassign's first step: its task routed to its `to`
// session, as route_to_session routes a task. "" when it was routed (a.RouteID
// set), else why not.
func (s *Server) fleetRouteTask(ctx context.Context, a *fleet.Action) string {
	if !routesOn(s.ext) {
		return "session routing is off on this server"
	}
	chief, err := s.chiefAgent(ctx)
	if err != nil {
		return "the chief could not be read"
	}
	n, ok := s.ownersTask(ctx, chief, a.TaskID)
	if !ok {
		return "the task is not one of the owner's now"
	}
	to, ok := s.fleetSubjectFor(ctx, a.ToSubject)
	if !ok || to.kind != chiefnudge.KindMirror {
		return "the session the task goes to is gone"
	}
	r, _, refused := s.routeItem(ctx, chief.ID, requestOwner(ctx), routedItem{task: n}, focus.Subject{Kind: focus.KindMirror, Key: to.key}, plugins.RouteOrigin{})
	if refused != "" {
		var m map[string]any
		_ = json.Unmarshal([]byte(refused), &m)
		return fmt.Sprintf("the task could not be routed (%v)", firstNonNil(m["error"], m["refused"]))
	}
	s.recordTaskRouted(ctx, n, workReassignTool)
	moved, ok := s.fleetMove(ctx, *a, fleet.Move{State: fleet.StateApproved, RouteID: r.RouteID}, fleet.StateApproved)
	if ok {
		*a = moved
	}
	return ""
}

// firstNonNil is the first of vs that is not nil.
func firstNonNil(vs ...any) any {
	for _, v := range vs {
		if v != nil {
			return v
		}
	}
	return ""
}

// fleetReject rejects the principal's proposal id.
func (s *Server) fleetReject(ctx context.Context, id int64, via string) bool {
	a, err := s.ext.FleetActions.GetAction(ctx, id)
	if err != nil || a.State != fleet.StateProposed {
		return false
	}
	_, ok := s.fleetMove(ctx, a, fleet.Move{State: fleet.StateRejected, DecidedVia: via, DecidedBy: requestOwner(ctx), Finish: true}, fleet.StateProposed)
	return ok
}

// fleetMove moves a (from from, default its own state) to m, logging a
// failure; the moved row and whether it moved.
//
// 🔴 A PROPOSAL THAT STOPS WAITING MOVES ITS NOTIFICATION (fleetPushSync):
// whatever took it out of `proposed` — an expiry, a tap, a reject, a
// suppression — the "N proposals waiting" push is withdrawn when none is
// left, or replaced quietly with the new count. A batch (the pass, a draw, a
// bulk decision: fleetBatch) syncs once, after it, instead.
func (s *Server) fleetMove(ctx context.Context, a fleet.Action, m fleet.Move, from ...string) (fleet.Action, bool) {
	if len(from) == 0 {
		from = []string{a.State}
	}
	moved, ok, err := s.ext.FleetActions.MoveAction(ctx, a.ID, from, m)
	if err != nil {
		s.logger.Printf("fleet: action %d: %s → %s: %v", a.ID, strings.Join(from, ","), m.State, err)
		return a, false
	}
	if ok {
		s.fleetChanged(a.Owner)
		if a.State == fleet.StateProposed && moved.State != fleet.StateProposed && !fleetBatched(ctx) {
			s.fleetPushSync(ctx, a.Owner)
		}
		return moved, true
	}
	return a, false
}

// fleetBatchKey marks a context whose caller syncs the fleet push once,
// after all its moves (fleetBatch).
type fleetBatchKey struct{}

// fleetBatch is ctx for a run of fleetMoves whose caller calls fleetPushSync
// itself once they are done.
func fleetBatch(ctx context.Context) context.Context {
	return context.WithValue(ctx, fleetBatchKey{}, true)
}

func fleetBatched(ctx context.Context) bool {
	b, _ := ctx.Value(fleetBatchKey{}).(bool)
	return b
}

// fleetChanged redraws owner's digest (the attention queue's event: the
// card's live refresh).
func (s *Server) fleetChanged(owner int64) {
	if owner > 0 {
		s.attentionChanged(owner, "fleet")
	}
}

// --- the send queue's gate ---------------------------------------------------------

// Why a fleet row is not pasted (sendQueueFleetGate).
const (
	SendQueueFleetOff     = "Not sent: fleet proposals are off on this server."
	SendQueueFleetUnknown = "Not sent: its fleet action could not be read."
	SendQueueFleetClosed  = "Not sent: its fleet action is no longer waiting to be sent"
	SendQueueFleetMoved   = "Not sent: the session moved on after its card was approved."
)

// sendQueueFleetGate is why fleet row it may not be pasted now, "" when it
// may: fleet proposals still on, its action still queued on THIS row, and
// the session still in the stop, idle streak and pull-request head the action
// was proposed on (fleetStillOn). A state that cannot be read is
// SendQueueNudgeWait — the row goes back to queued for a later pass. ctx is
// the row's owner.
func (s *Server) sendQueueFleetGate(ctx context.Context, it sendqueue.Item) string {
	if !s.fleetProposeOn() {
		if a, err := s.ext.FleetActions.GetAction(ctx, it.FleetActionID); err == nil && s.ext.FleetActions != nil {
			s.fleetMove(ctx, a, fleet.Move{State: fleet.Suppressed(fleet.GuardFlagOff), Outcome: "fleet proposals were turned off", Finish: true}, fleet.StateQueued)
		}
		return SendQueueFleetOff
	}
	a, err := s.ext.FleetActions.GetAction(ctx, it.FleetActionID)
	if err != nil {
		return SendQueueFleetUnknown
	}
	if a.State != fleet.StateQueued || a.SendQueueID != it.ID {
		return SendQueueFleetClosed + " (" + a.State + ")."
	}
	switch s.fleetStillOn(ctx, a) {
	case nudgeUnknown:
		return SendQueueNudgeWait
	case nudgeMovedOn, nudgeMovedAway:
		s.fleetMove(ctx, a, fleet.Move{State: fleet.Suppressed(fleet.GuardMovedOn), Outcome: "the session moved on before the paste", Finish: true}, fleet.StateQueued)
		return SendQueueFleetMoved
	}
	return ""
}

// --- the pass ----------------------------------------------------------------------

// fleetPass is one owner's fleet step in the session-state pass (its one
// caller, once per pass): settle the actions in flight (a queued row sent or
// failed; an approved agent action's turn started; a wrap whose handoff turn
// ended closed, or offered a close after 24 h), expire the proposals whose
// stop ended or that waited 12 h, propose "Write handoff, then close" for each
// finished session (fleetProposeWraps), and push "N proposals waiting" when
// one is due. octx is the owner's.
//
// 🔴 IT RUNS ALONGSIDE THE STOP JUDGE, WHOEVER THAT IS (the operator's answer
// to #796's round 0): the chief keeps judging stops (stopJudgeOf) until the
// fleet can judge (phase 3's triage), and the fleet's proposals — its own and
// the chief's — run beside it. So an action in flight settles whatever the
// judge is: nothing here asks stopJudgeOf. An owner with no chief has no
// proposals (the cards are the chief's panel's).
func (s *Server) fleetPass(octx context.Context, owner int64, facts []nudgeCandidate) {
	if !s.fleetProposeOn() {
		// 🔴 TURNED OFF, ITS NOTICE GOES TOO: an "N proposals waiting" item
		// filed while it was on would point at a Proposed group that is no
		// longer drawn — and so does its push.
		s.fleetNoticeWithdraw(octx)
		s.fleetPushHide(octx, owner)
		return
	}
	if _, err := s.chiefAgent(octx); err != nil {
		return
	}
	bctx := fleetBatch(octx)
	s.fleetSettle(bctx, facts)
	s.fleetProposeWraps(bctx, facts)
	s.fleetProposeCIResults(bctx, facts)
	s.fleetPushWaiting(octx, owner)
	// After the claim: a push it just sent recorded the set it shows, so only
	// a change it did not push is acted on.
	s.fleetPushSync(octx, owner)
}

// fleetSettle moves each open action of octx's owner on.
func (s *Server) fleetSettle(octx context.Context, facts []nudgeCandidate) {
	open, err := s.ext.FleetActions.OpenActions(octx)
	if err != nil {
		s.logger.Printf("fleet: open actions: %v", err)
		return
	}
	now := s.now()
	for _, a := range open {
		switch a.State {
		case fleet.StateProposed:
			s.fleetExpire(octx, a)
		case fleet.StateQueued:
			if a.Class == fleet.ClassClose {
				// 🔴 A CLOSE IS QUEUED WHILE fleetDeliverClose's BACKGROUND CLOSE
				// RUNS ("closing", no queue row): never an agent turn (that would
				// send the close's text, mark it sent and lose the close's
				// result), never the queue's settle. One the close never
				// finished (a restart mid-close) is repaired as an approved one is.
				if now.Sub(a.DecidedAt) > fleetCloseRepairAfter {
					s.fleetMove(octx, a, fleet.Move{State: fleet.StateFailed, Outcome: "its close was interrupted: the session may still be open", Finish: true})
				}
				continue
			}
			if a.SubjectKind == chiefnudge.KindAgent && a.SendQueueID == 0 {
				s.fleetStartTurn(octx, a)
				continue
			}
			s.fleetSettleQueued(octx, a)
		case fleet.StateSent:
			if a.Class == fleet.ClassWrap {
				s.fleetWrapClose(octx, a, facts)
			}
		case fleet.StateApproved:
			// Approved but neither queued nor failed: a stop between the two.
			if now.Sub(a.DecidedAt) > chiefnudge.QueueRepairAfter {
				s.fleetMove(octx, a, fleet.Move{State: fleet.StateFailed, Outcome: "its delivery was interrupted", Finish: true})
			}
		}
	}
}

// fleetStartTurn starts approved agent action a's turn (sendNudgeTurn's
// shape): the stop re-read first (moved on: suppressed, nothing sent), never
// overlapping one of the agent's turns. A state that cannot be read leaves it
// queued for the next pass.
func (s *Server) fleetStartTurn(octx context.Context, a fleet.Action) {
	if s.ext.Gateway == nil {
		s.fleetMove(octx, a, fleet.Move{State: fleet.StateFailed, Outcome: "the agent gateway is off on this server", Finish: true})
		return
	}
	t := s.fleetTarget(octx, a)
	if t.how != fleetDeliverTurn {
		s.fleetMove(octx, a, fleet.Move{State: fleet.StateFailed, Outcome: firstNonBlank(t.why, "the agent cannot take a turn"), Finish: true})
		return
	}
	switch s.fleetStillOn(octx, a) {
	case nudgeUnknown:
		return
	case nudgeMovedOn, nudgeMovedAway:
		s.fleetMove(octx, a, fleet.Move{State: fleet.Suppressed(fleet.GuardMovedOn), Outcome: "the conversation moved on before its turn", Finish: true})
		return
	}
	sess, err := s.ext.Agents.GetSession(octx, a.ChatSessionID)
	if err != nil || sess.AgentID != t.agent.ID {
		s.fleetMove(octx, a, fleet.Move{State: fleet.StateFailed, Outcome: "the agent's conversation is gone", Finish: true})
		return
	}
	if ok, _ := s.turns.enterIdle(t.agent.ID); !ok {
		return // a turn started this instant: the next pass re-reads (and finds it moved on)
	}
	if _, ok := s.fleetMove(octx, a, fleet.Move{State: fleet.StateSent, Finish: a.Class != fleet.ClassWrap, CloseAfter: a.StreakKey}); !ok {
		s.turns.leave(t.agent.ID)
		return
	}
	// The attribution line names the owner and the moment they approved it.
	s.startAgentTurn(octx, t.agent, sess, fleet.Body(s.personName(octx, a.Owner), a.DecidedAt, a.Text), notTyped, nil) // muster's relay, never the owner's typed words
}

// fleetSettleQueued settles a queued action by its send-queue row.
func (s *Server) fleetSettleQueued(octx context.Context, a fleet.Action) {
	if s.ext.SendQueue == nil || a.SendQueueID == 0 {
		return
	}
	it, err := s.ext.SendQueue.Get(octx, a.SendQueueID)
	if err != nil {
		return
	}
	switch it.State {
	case sendqueue.Sent:
		s.fleetMove(octx, a, fleet.Move{State: fleet.StateSent, Finish: a.Class != fleet.ClassWrap, CloseAfter: a.StreakKey})
	case sendqueue.Failed, sendqueue.Cancelled:
		if it.Detail == SendQueueFleetMoved || it.Detail == SendQueueFleetOff {
			return // the gate already recorded it
		}
		s.fleetMove(octx, a, fleet.Move{State: fleet.StateFailed, Outcome: it.Detail, Finish: true})
	}
}

// fleetCloseRepairAfter is how long a close stays queued ("closing") before
// the pass calls it interrupted: well past the close's own bound.
const fleetCloseRepairAfter = chiefnudge.QueueRepairAfter

// fleetWrapCloseWait bounds how long a wrap waits for its handoff turn.
const fleetWrapCloseWait = 24 * time.Hour

// fleetWrapClose is a wrap's second step: once its session worked after the
// handoff and stopped again (a stop on a streak other than CloseAfter), its
// window is closed — unless §5.2's refusals hold: an open pull request, a
// queued message, background work.
func (s *Server) fleetWrapClose(octx context.Context, a fleet.Action, facts []nudgeCandidate) {
	var c *nudgeCandidate
	for i := range facts {
		if facts[i].Kind == a.SubjectKind && facts[i].Key == a.SubjectKey {
			c = &facts[i]
		}
	}
	if c == nil || c.StreakKey == "" || c.StreakKey == a.CloseAfter || c.Status != "idle" {
		if s.now().Sub(a.DecidedAt) <= fleetWrapCloseWait {
			return
		}
		// 🔴 A8's FALLBACK: no handoff turn came within 24 h — the card's
		// close half is offered on its own, as a close card the owner taps
		// (still never closed without a tap).
		if c == nil {
			s.fleetMove(octx, a, fleet.Move{State: fleet.StateSent, Outcome: "not closed: the session is not listed now", Finish: true})
			return
		}
		if _, ok := s.fleetMove(octx, a, fleet.Move{State: fleet.StateSent, Outcome: "no handoff turn within 24 h: a close card is offered", Finish: true}); ok {
			s.fleetProposeClose(octx, a)
		}
		return
	}
	if why := s.fleetCloseRefusal(octx, a, *c); why != "" {
		s.fleetMove(octx, a, fleet.Move{State: fleet.StateSent, Outcome: "not closed: " + why, Finish: true})
		return
	}
	// 🔴 IN THE BACKGROUND, as fleetDeliverClose's: a close can take up to a
	// minute, and the session-state pass is every owner's pushes and nudges.
	// One close per wrap in flight (fleetWrapClosing); its finish is the
	// conditional sent → sent move, so a second pass after it finds it done.
	if _, busy := s.fleetWrapClosing.LoadOrStore(a.ID, struct{}{}); busy {
		return
	}
	bctx := context.WithoutCancel(octx)
	safeGo(s.logger, "fleet wrap close", func() {
		defer s.fleetWrapClosing.Delete(a.ID)
		outcome := s.fleetCloseSession(bctx, a)
		s.fleetMove(bctx, a, fleet.Move{State: fleet.StateSent, Outcome: outcome, Finish: true})
	})
}

// fleetProposeClose files the close card a wrap with no handoff turn falls
// back to: the wrap's own session and stop.
func (s *Server) fleetProposeClose(octx context.Context, w fleet.Action) {
	runtime := w.Evidence.Runtime
	p := fleet.Proposal{Class: fleet.ClassClose, Proposer: fleet.ProposerSweep, SubjectKind: w.SubjectKind, SubjectKey: w.SubjectKey,
		Host: w.Host, TmuxSession: w.TmuxSession, Window: w.Window, AgentID: w.AgentID, ChatSessionID: w.ChatSessionID,
		EpisodeID: w.EpisodeID, StreakKey: w.StreakKey, Evidence: w.Evidence, TaskID: 0,
		Reason: "No handoff turn came within 24 h of \"Write handoff, then close\": close it anyway?",
		Text:   fleetCloseText(runtime)}
	if _, err := s.ext.FleetActions.Propose(octx, p); err == nil {
		s.fleetChanged(requestOwner(octx))
	} else if !errors.Is(err, fleet.ErrAlreadyProposed) {
		s.logger.Printf("fleet: offer a close for %s: %v", w.SubjectKey, err)
	}
}

// fleetCloseText is a close card's text: nothing is typed, and how the
// session is closed.
func fleetCloseText(runtime string) string {
	return "(Nothing is typed. muster closes the session: " + endSessionHow(runtime) + ".)"
}

// fleetDeliverClose closes an approved close card's session — §5.2's refusals
// read at the tap (an open pull request, a queued message, background work) —
// in the background, as end_session's tap does, recording how it went.
func (s *Server) fleetDeliverClose(ctx context.Context, a fleet.Action) fleetApproveResult {
	var c nudgeCandidate
	found := false
	for _, f := range s.stallInputs(ctx, nil) {
		if f.Kind == a.SubjectKind && f.Key == a.SubjectKey {
			c, found = f, true
		}
	}
	if !found {
		s.fleetMove(ctx, a, fleet.Move{State: fleet.Suppressed(fleet.GuardMovedOn), Outcome: "the session is not listed now", Finish: true}, fleet.StateApproved)
		return fleetApproveResult{note: FleetMovedOn, movedOn: true}
	}
	if why := s.fleetCloseRefusal(ctx, a, c); why != "" {
		s.fleetMove(ctx, a, fleet.Move{State: fleet.StateFailed, Outcome: "not closed: " + why, Finish: true}, fleet.StateApproved)
		return fleetApproveResult{note: "Not closed: " + why + "."}
	}
	moved, ok := s.fleetMove(ctx, a, fleet.Move{State: fleet.StateQueued, Outcome: "closing"}, fleet.StateApproved)
	if !ok {
		return fleetApproveResult{note: FleetDecided}
	}
	bctx := context.WithoutCancel(ctx)
	safeGo(s.logger, "fleet close", func() {
		outcome := s.fleetCloseSession(bctx, moved)
		s.fleetMove(bctx, moved, fleet.Move{State: fleet.StateSent, Outcome: outcome, Finish: true}, fleet.StateQueued)
	})
	return fleetApproveResult{note: FleetClosing, sent: true}
}

// FleetClosing is an approved close card's note.
const FleetClosing = "Closing: muster closes the session now."

// fleetCloseRefusal is why a wrap's session is not closed now (§5.2), "" when
// it may be.
func (s *Server) fleetCloseRefusal(octx context.Context, a fleet.Action, c nudgeCandidate) string {
	if c.Shell {
		return "it has background work"
	}
	if c.OpenAsk || c.TurnError {
		return "it is waiting on you"
	}
	if a.SubjectKind == chiefnudge.KindMirror && s.ext.SendQueue != nil {
		items, err := s.ext.SendQueue.List(octx, sendqueue.Target{Host: a.Host, Session: a.TmuxSession, Window: a.Window})
		if err != nil {
			return "its queued messages could not be read"
		}
		for _, it := range items {
			if it.State.Open() {
				return "a message is queued to it"
			}
		}
	}
	if sub, ok := s.fleetSubjectFor(octx, firstNonBlank(a.Evidence.SubjectID, "mirror:"+a.SubjectKey)); ok {
		for _, p := range sub.prs {
			if p.State == fleet.PROpen {
				return "it has an open pull request"
			}
		}
	}
	return ""
}

// fleetCloseSession closes a wrap's session the way end_session's tap does
// (agent-close for a window; stop and remove for an agent) and says how it
// went.
func (s *Server) fleetCloseSession(octx context.Context, a fleet.Action) string {
	if a.SubjectKind == chiefnudge.KindAgent {
		if s.ext.Provisioner == nil {
			return "not closed: this muster cannot stop agents"
		}
		if _, protected, err := s.agentIsDeleteProtected(octx, a.AgentID); err != nil || protected {
			return "not closed: the agent is protected"
		}
		destroy := func(ctx context.Context, id int64) error { return s.ext.Provisioner.Destroy(ctx, id) }
		if _, err := s.destroyAgent(octx, a.AgentID, destroy, destroyHooks{done: s.reopenTaskAfterDestroy}); err != nil {
			return "not closed: " + err.Error()
		}
		return "closed: the agent was stopped and removed"
	}
	e, ok := s.mirrorFocusEntryFor(octx, a.SubjectKey)
	if !ok || e.Host != a.Host || e.Session != a.TmuxSession || e.Window != a.Window {
		return "not closed: the session is not in its window now"
	}
	if why := s.endable(octx, e); why != "" {
		return "not closed: " + why
	}
	closer, ok := s.ext.Mirror.(agentCloser)
	if !ok {
		return "not closed: this muster cannot reach hosts"
	}
	cctx, cancel := context.WithTimeout(octx, mirror.AgentCloseGrace+30*time.Second)
	defer cancel()
	r, err := closer.CloseAgent(cctx, e.Host, mirror.AgentCloseReq{Session: e.Session, Window: e.Window, Key: a.SubjectKey})
	if err != nil {
		return "not closed: " + err.Error()
	}
	return "closed: " + firstNonBlank(r.Reason, "its window was closed")
}

// fleetProposeWraps proposes "Write handoff, then close" (proposer sweep_code,
// no model; A8's one card) for
// each of octx's owner's sessions the digest counts Done — stopped, its latest
// pull request merged or closed, nothing queued to it — that the stall rule
// holds as a candidate (stalledSessions: no background work, no open ask, no
// turn error, an open episode).
func (s *Server) fleetProposeWraps(octx context.Context, facts []nudgeCandidate) {
	cands := map[string]bool{}
	for _, c := range s.stalledSessions(octx, facts) {
		cands[c.Kind+":"+c.Key] = true
	}
	if len(cands) == 0 {
		return
	}
	fr, err := s.readFleet(octx, fleetListFilter{})
	if err != nil || !fr.stateOn {
		return
	}
	for _, sub := range fr.subjects {
		if sub.chief || sub.episode == nil || !cands[sub.episode.Kind+":"+sub.episode.Key] || s.fleetSubjectGuarded(octx, sub) {
			continue
		}
		if fleet.GroupOf(sub.state, sub.prs, sub.inFlight) != fleet.GroupDone {
			continue
		}
		// ONE WRAP PROPOSAL PER STOP: a stop the owner already decided a wrap
		// on (approved, rejected, expired) is not proposed again.
		if done, err := s.ext.FleetActions.ProposedOn(octx, sub.episode.ID, fleet.ClassWrap); err != nil || done {
			continue
		}
		// 🔴 THE ONE CARD (A8): "Write handoff, then close" — never a
		// separate handoff card beside it.
		p := s.fleetProposalFor(sub, fleet.ClassWrap, fleetHandoffText(sub),
			"Its pull request "+sub.prs[0].Repo+"#"+strconv.FormatInt(sub.prs[0].Number, 10)+" is "+sub.prs[0].State+" and it stopped with nothing in flight.")
		p.Proposer = fleet.ProposerSweep
		if t := s.fleetTarget(octx, fleetActionOf(p)); t.refused != "" {
			continue
		}
		if _, err := s.ext.FleetActions.Propose(octx, p); err == nil {
			s.fleetChanged(requestOwner(octx))
		} else if !errors.Is(err, fleet.ErrAlreadyProposed) {
			s.logger.Printf("fleet: propose a wrap for %s: %v", sub.row.ID, err)
		}
	}
}

// --- the batched push (A4) ---------------------------------------------------------

// fleetPushTag is the "N proposals waiting" push's tag: each one replaces the
// last on a phone, never stacks.
const fleetPushTag = "fleet-proposals"

// fleetPushWaiting pushes owner "N proposals waiting" when one is due
// (fleet.Actions.ClaimPush: at most every fleet.PushEvery, and only when a
// proposal arrived since the last). It names the newest proposal, and its tap
// opens the digest's Proposed group on it; it carries NO action — 🔴 a
// phone's notification can never approve a proposal (the card's routes
// refuse a device principal too: fleetCardRefusal).
func (s *Server) fleetPushWaiting(octx context.Context, owner int64) {
	n, due, err := s.ext.FleetActions.ClaimPush(octx, s.now())
	if err != nil {
		s.logger.Printf("fleet: owner %d: the batched push: %v", owner, err)
		return
	}
	if due {
		s.goNotify(owner, push.EventAttention, fleetPushNotification(n, s.fleetNewestWaiting(octx)), nil)
	}
	s.fleetNoticeSync(octx, due, n)
}

// fleetPushSync keeps owner's "N proposals waiting" notification true to the
// proposals waiting now (fleet.Actions.SyncPush), acting only when the set it
// shows changed:
//
//   - none waits now: it is WITHDRAWN (a resolved push, its tag);
//   - some do: it is REPLACED IN PLACE — the new count and the newest still
//     waiting, linked — Update and Silent, so a device that already shows it
//     swaps it without sound, vibration or a second alert.
//
// 🔴 IT NEVER RAISES ONE: nothing showing (none pushed, or withdrawn) stays so,
// and it never moves ClaimPush's clock — a genuinely new proposal still gets
// its own alert on fleet.PushEvery's cadence. A pass where nothing changed
// sends nothing. octx is the owner's.
func (s *Server) fleetPushSync(octx context.Context, owner int64) {
	if s.ext.FleetActions == nil {
		return
	}
	r, err := s.ext.FleetActions.SyncPush(octx)
	if err != nil {
		s.logger.Printf("fleet: owner %d: the proposals push's sync: %v", owner, err)
		return
	}
	switch {
	case r.Withdraw:
		s.fleetPushWithdraw(owner)
	case r.Replace:
		newest, err := s.ext.FleetActions.GetAction(octx, r.Newest)
		if err != nil || newest.State != fleet.StateProposed {
			// Its link then names no proposal: the digest highlights the
			// newest waiting one when it opens (fleetFocus).
			newest = fleet.Action{}
		}
		n := fleetPushNotification(r.Count, newest)
		n.Update, n.Silent = true, true
		s.goNotify(owner, push.EventAttention, n, nil)
	}
}

// fleetPushWithdraw withdraws owner's "N proposals waiting" notification.
func (s *Server) fleetPushWithdraw(owner int64) {
	s.goNotify(owner, push.EventResolved, RouterNotification{Type: push.TypeResolved, ID: fleetPushTag, Tag: fleetPushTag}, nil)
}

// fleetPushHide withdraws owner's "N proposals waiting" notification if one is
// showing (fleet proposals turned off: its link opens a group no longer drawn).
func (s *Server) fleetPushHide(octx context.Context, owner int64) {
	if s.ext.FleetActions == nil {
		return
	}
	showing, err := s.ext.FleetActions.HidePush(octx)
	if err != nil {
		s.logger.Printf("fleet: owner %d: hide the proposals push: %v", owner, err)
		return
	}
	if showing {
		s.fleetPushWithdraw(owner)
	}
}

// fleetNoticeLabel is the "N proposals waiting" Attention item's asker label:
// muster's own (a System notice), how the pass finds the one it filed.
const fleetNoticeLabel = "Fleet proposals"

// fleetNoticeText is the Attention item for n waiting proposals.
func fleetNoticeText(n int) string {
	if n == 1 {
		return "1 fleet proposal is waiting for you in the chief panel's fleet digest. Nothing is sent until you approve it."
	}
	return strconv.Itoa(n) + " fleet proposals are waiting for you in the chief panel's fleet digest. Nothing is sent until you approve them."
}

// fleetNoticeSync keeps ONE "N proposals waiting" Attention item beside the
// batched push (A4) — UPDATED, never stacked: when a push went out (due) it
// replaces the open one with the new count, and once no proposal waits it is
// withdrawn. A notice (attention.KindNotice): nothing to answer, nothing
// relayed, never counted toward the open-ask cap; filed without a push of its
// own (the batched push is the push).
func (s *Server) fleetNoticeSync(octx context.Context, due bool, n int) {
	mine, ok := s.fleetNotices(octx)
	if !ok {
		return
	}
	waiting := n
	if !due {
		// Between pushes: only whether any proposal still waits matters.
		list, err := s.ext.FleetActions.OpenActions(octx)
		if err != nil {
			return
		}
		waiting = 0
		for _, a := range list {
			if a.State == fleet.StateProposed {
				waiting++
			}
		}
	}
	if !due && waiting > 0 {
		return
	}
	s.fleetNoticesCancel(octx, mine)
	if waiting == 0 {
		return
	}
	ask, err := s.ext.Attention.Create(octx, attention.New{Asker: attention.Asker{System: true, Label: fleetNoticeLabel},
		Question: fleetNoticeText(waiting), Notice: true})
	if err != nil {
		s.logger.Printf("fleet: the proposals notice: %v", err)
		return
	}
	s.attentionChanged(ask.Owner, attentionAskKey(ask.ID))
}

// fleetNotices is octx's owner's open "N proposals waiting" items; ok false
// when there is no Attention store or it could not be read.
func (s *Server) fleetNotices(octx context.Context) ([]attention.Ask, bool) {
	if s.ext.Attention == nil {
		return nil, false
	}
	open, err := s.ext.Attention.Open(octx)
	if err != nil {
		s.logger.Printf("fleet: the proposals notice: %v", err)
		return nil, false
	}
	var mine []attention.Ask
	for _, a := range open {
		if a.Kind == attention.KindNotice && a.Asker.System && a.Asker.Label == fleetNoticeLabel {
			mine = append(mine, a)
		}
	}
	return mine, true
}

// fleetNoticesCancel withdraws the notices mine.
func (s *Server) fleetNoticesCancel(octx context.Context, mine []attention.Ask) {
	for _, a := range mine {
		if _, err := s.cancelAsk(octx, a.ID); err != nil {
			s.logger.Printf("fleet: withdraw the proposals notice %d: %v", a.ID, err)
		}
	}
}

// fleetNoticeWithdraw withdraws octx's owner's "N proposals waiting" item,
// if one is open (fleet proposals turned off).
func (s *Server) fleetNoticeWithdraw(octx context.Context) {
	if mine, ok := s.fleetNotices(octx); ok {
		s.fleetNoticesCancel(octx, mine)
	}
}

// fleetPushNotification is the batched push for n waiting proposals, newest
// the newest of them (its zero value when it could not be read): muster's own
// words naming what is proposed and on which session (fleetPushBody) — never
// a message's or an instruction's text — and no actions. Its tap opens the
// digest's Proposed group on newest (ui.FleetProposalsURL).
func fleetPushNotification(n int, newest fleet.Action) RouterNotification {
	title := "1 proposal waiting"
	if n != 1 {
		title = strconv.Itoa(n) + " proposals waiting"
	}
	return RouterNotification{Type: attentionNotificationType, ID: fleetPushTag, Title: title,
		Body: fleetPushBody(n, newest), Tag: fleetPushTag, Data: map[string]string{"url": ui.FleetProposalsURL(newest.ID)}}
}

// fleetPushGeneric is the push's body when the newest proposal could not be
// read.
const fleetPushGeneric = "Your chief proposed what to send your sessions. Nothing is sent until you approve it in the fleet digest."

// fleetPushAsks is each class as the push asks it, %s the session's name.
// TestThePushNamesEveryClass holds it to fleet.ClassVerb's classes.
var fleetPushAsks = map[string]string{
	fleet.ClassContinue:       "Continue '%s'?",
	fleet.ClassHandoff:        "Ask '%s' for its handoff?",
	fleet.ClassStopNote:       "Stop '%s' after this step?",
	fleet.ClassCustom:         "Send the chief's message to '%s'?",
	fleet.ClassWrap:           "Wrap '%s'?",
	fleet.ClassReassign:       "Move a task off '%s'?",
	fleet.ClassAnswerRule:     "Answer '%s'?",
	fleet.ClassAnswerComposed: "Answer '%s'?",
	fleet.ClassAnswerPick:     "Pick an option for '%s'?",
	fleet.ClassCIResult:       "Tell '%s' its CI finished?",
	fleet.ClassClose:          "Close '%s'?",
}

// fleetPushNameRunes caps the session's name in the push.
const fleetPushNameRunes = 40

// fleetPushBody is the push's body: what the newest proposal is, on which
// session ("Wrap 'muster-asks-ux'?"), "+N more." for the others, and that
// nothing is sent until approved.
func fleetPushBody(n int, newest fleet.Action) string {
	if newest.ID <= 0 {
		return fleetPushGeneric
	}
	ask, ok := fleetPushAsks[newest.Class]
	if !ok {
		ask = "A proposal for '%s'."
	}
	body := strings.Replace(ask, "%s", fleetPushName(fleetLabel(newest)), 1)
	if n > 1 {
		return body + " +" + strconv.Itoa(n-1) + " more. Nothing is sent until you approve them."
	}
	return body + " Nothing is sent until you approve it."
}

// fleetPushName is a session's name as the push quotes it: the digest card's
// label on one line, with no control character and no straight quote (so the
// quoting stays unambiguous), cut to fleetPushNameRunes with "…".
func fleetPushName(label string) string {
	label = strings.Map(func(r rune) rune {
		switch {
		case r == '\'':
			return '’'
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			return ' '
		}
		return r
	}, label)
	label = strings.Join(strings.Fields(label), " ")
	if label == "" {
		return "a session"
	}
	if r := []rune(label); len(r) > fleetPushNameRunes {
		return strings.TrimSpace(string(r[:fleetPushNameRunes-1])) + "…"
	}
	return label
}

// fleetNewestWaiting is octx's owner's newest proposal still waiting for its
// tap; the zero Action when there is none or it could not be read.
func (s *Server) fleetNewestWaiting(octx context.Context) fleet.Action {
	list, err := s.ext.FleetActions.OpenActions(octx)
	if err != nil {
		s.logger.Printf("fleet: the batched push's newest proposal: %v", err)
		return fleet.Action{}
	}
	// Oldest first: the newest waiting is the last proposed.
	for i := len(list) - 1; i >= 0; i-- {
		if list[i].State == fleet.StateProposed {
			return list[i]
		}
	}
	return fleet.Action{}
}
