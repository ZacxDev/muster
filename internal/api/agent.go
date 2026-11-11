package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agentkickoff"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefusage"
	"github.com/ZacxDev/muster/internal/features"
	"github.com/ZacxDev/muster/internal/flows"
	"github.com/ZacxDev/muster/internal/github"
	"github.com/ZacxDev/muster/internal/githubapp"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/plugins"
)

// agentCtxKey carries the authenticated agent through the request context.
type agentCtxKey struct{}

// requireAgentToken authenticates the agent self-service API by the caller's
// per-agent hooks token (Bearer or X-Muster-Token). The token is unique per
// agent, so a valid bearer both authenticates and identifies the calling agent;
// the resolved agent is attached to the request context for the handler.
func (s *Server) requireAgentToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := hookTokenFromRequest(r)
		if token == "" {
			s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing agent token"})
			return
		}
		a, err := s.agentByToken(r.Context(), token)
		if err != nil {
			// Unknown token (or no rows) → unauthorized. Don't leak which.
			s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid agent token"})
			return
		}
		p, ok := s.agentPrincipal(r.Context(), a, authz.ViaAgent)
		if !ok {
			s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid agent token"})
			return
		}
		ctx := context.WithValue(r.Context(), agentCtxKey{}, a)
		s.admit(w, r.WithContext(ctx), next, p)
	}
}

// agentFromContext returns the agent attached by requireAgentToken.
func agentFromContext(ctx context.Context) (agents.Agent, bool) {
	a, ok := ctx.Value(agentCtxKey{}).(agents.Agent)
	return a, ok
}

// agentTaskID returns the calling agent's bound task id, or writes a 404 and
// returns ok=false if the agent has no task assigned.
func (s *Server) agentTaskID(w http.ResponseWriter, a agents.Agent) (int64, bool) {
	if a.NoteID == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "no task assigned to this agent"})
		return 0, false
	}
	return *a.NoteID, true
}

// handleAgentTask serves GET /agent/task: the calling agent's bound task with
// its current status and comment thread.
func (s *Server) handleAgentTask(w http.ResponseWriter, r *http.Request) {
	a, _ := agentFromContext(r.Context())
	id, ok := s.agentTaskID(w, a)
	if !ok {
		return
	}
	note, err := s.ext.Notes.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "task not found"})
			return
		}
		s.logger.Printf("agent: get task %d: %v", id, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not load task"})
		return
	}
	s.writeJSON(w, http.StatusOK, note)
}

type agentCommentRequest struct {
	Body string `json:"body"`
}

// handleAgentTaskComment serves POST /agent/task/comment: append a comment to the
// agent's task, authored by the agent.
func (s *Server) handleAgentTaskComment(w http.ResponseWriter, r *http.Request) {
	a, _ := agentFromContext(r.Context())
	id, ok := s.agentTaskID(w, a)
	if !ok {
		return
	}
	var body agentCommentRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	if strings.TrimSpace(body.Body) == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "comment body required"})
		return
	}
	c, queued, err := s.addTaskComment(withRefOrigin(r.Context(), refOrigin{Agent: a.Name}), commentByActor,
		notes.Comment{NoteID: id, Author: a.Name, Body: body.Body})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "task not found"})
			return
		}
		s.logger.Printf("agent: comment task %d: %v", id, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not add comment"})
		return
	}
	if queued != nil {
		// A reference: the comment went to the upstream's queue (task_ref_queue.go).
		s.writeJSON(w, http.StatusAccepted, map[string]any{"queued": true, "write": queued, "message": refQueuedText})
		return
	}
	s.broadcast(r.Context(), EventTaskChanged, strconv.FormatInt(id, 10))
	s.writeJSON(w, http.StatusCreated, c)
}

type agentStatusRequest struct {
	Status string `json:"status"`
}

// handleAgentTaskStatus serves PATCH /agent/task/status: move the agent's task
// through the working states. Agents may NOT set `complete` — only the
// operator/human can declare a task done.
func (s *Server) handleAgentTaskStatus(w http.ResponseWriter, r *http.Request) {
	a, _ := agentFromContext(r.Context())
	id, ok := s.agentTaskID(w, a)
	if !ok {
		return
	}
	var body agentStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	if !notes.ValidStatus(body.Status) {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid status"})
		return
	}
	if !notes.StatusAllowedForAgent(body.Status) {
		s.writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "agents may not set this status; only the operator/human can mark a task complete",
		})
		return
	}
	// …nor move a task OUT of complete (agentStatusRefusal, the one rule). The
	// read FAILS CLOSED: a task whose status cannot be read is not written.
	cur, err := s.ext.Notes.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "task not found"})
			return
		}
		s.logger.Printf("agent: read task %d before a status write: %v", id, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not read the task"})
		return
	}
	if why := agentStatusRefusal(cur.Status, body.Status); why != "" {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": why})
		return
	}
	note, err := s.setNoteStatus(notes.ForAgent(withRefOrigin(r.Context(), refOrigin{Author: a.Name, Agent: a.Name})), writerAgentRoute, id, body.Status)
	if errors.Is(err, errQueuedUpstream) {
		s.writeJSON(w, http.StatusAccepted, struct {
			notes.Note
			Queued bool `json:"queued"`
		}{note, true})
		return
	}
	if err != nil {
		if errors.Is(err, notes.ErrCompleteLocked) {
			s.writeJSON(w, http.StatusForbidden, map[string]any{"error": agentStatusRefusal(notes.StatusComplete, body.Status)})
			return
		}
		if errors.Is(err, pgx.ErrNoRows) {
			s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "task not found"})
			return
		}
		s.logger.Printf("agent: set status task %d: %v", id, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not update status"})
		return
	}
	s.broadcast(r.Context(), EventTaskChanged, strconv.FormatInt(id, 10))
	s.writeJSON(w, http.StatusOK, note)
}

// --- worker self-service native tools (the /v1/responses mechanism) ---

// --- tool plumbing shared by every native-tool surface ---------------------
//
// 🔴 DELETION-TRACKING NOTE (upstream task #653, phase two). These five helpers
// LIVED IN internal/api/operator.go, which is deleted with the operator tier.
// They were never operator-specific — objSchema/strProp build a JSON-schema
// parameter block, toolJSON/toolErr encode a tool RESULT, and orEmptyJSON makes a
// no-argument tool call unmarshalable — and the worker tool set below has always
// used them. They moved rather than went because their only remaining consumer is
// this file.
//
// ⚠ THREE SIBLINGS DID NOT MOVE: intProp, strArray and policyRuleArraySchema had
// no consumer outside operatorToolDefs, so they went with it. Re-add one here if a
// worker tool ever needs an integer, a string array or an RBAC PolicyRule block —
// do not resurrect operator.go for it.

// objSchema builds a JSON-schema object node for a tool's parameters.
func objSchema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

// strProp is a described string property.
func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// toolJSON encodes a tool result, degrading to an error object rather than
// returning an empty string the model would read as success.
func toolJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `{"error":"could not encode result"}`
	}
	return string(b)
}

// toolErr is the tool-result spelling of an error.
func toolErr(format string, args ...any) string {
	return toolJSON(map[string]string{"error": fmt.Sprintf(format, args...)})
}

// orEmptyJSON returns "{}" for empty/blank args so json.Unmarshal never fails on
// a no-argument tool call.
func orEmptyJSON(s string) string {
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	return s
}

// agentSystemPrompt is the worker's system prompt for the tool loop. The tools
// carry their own schemas; this frames when to use them.
const agentSystemPrompt = `You are a muster worker agent. You have native tools to read and update YOUR assigned task. Use them: call agent_get_task to read your task before starting; agent_set_task_status to set "in_progress" when you begin and "ready_for_review" when done (you CANNOT set "complete", and you cannot change a task once it is complete — only the operator/human can; if you think a completed task needs reopening, ask with attention_ask); agent_comment_task to post progress notes. Then do the work.`

// agentTool is one entry of muster's ONE agent tool registry.
//
// 🔴 THERE IS ONE LIST AND ONE SWITCH. agentTools is the only place a tool is
// DECLARED; dispatchAgentTool is the only place one is EXECUTED. Both transports
// read them: the gateway kind gets [Server.AgentToolDefsFor] + [Server.AgentToolDispatch]
// on its /v1/responses turns, and POST /mcp (mcp.go) lists and calls the subset
// whose MCP flag is set. A tool added for one transport is therefore added for
// the other by setting a flag, never by copying a definition.
//
// 🔴 MCP IS AN EXPLICIT, PER-TOOL OPT-IN: THE CONNECTOR TOOLS AND THE TWO
// BROWSER-FLOW TOOLS HAVE IT (the flow tools only FILE a request the owner
// must approve; flow_tools.go), AND SO DO task_create_gated (it only files a
// task gated on the owner's approval; gated_task_tool.go), reply_draft (it
// only files a draft the owner must approve; reply_draft_tool.go) AND THE TWO
// FOCUS TOOLS (focus_tools.go: they read the owner's own focus rows and spend
// no credential the pod was not granted).
// /mcp is served on the connect listener — the ONE muster port a claude-code
// pod's network policy lets it reach — and that listener's contract is "the
// connector surface and nothing else" (OnlyConnectSurface). The other tools stay
// gateway-only on purpose: the task/status/comment tools
// would hand a claude-code pod muster's task API through a door that was opened
// for connectors only, and
// agent_list_pull_requests spends muster's OWN server-side GitHub connection,
// which would let a pod read through a credential it was never granted under the
// per-agent connector model. TestTheMCPToolsAreTheRegistrysMCPSubset pins both
// sets.
//
// 🔴 EXCEPT THE ATTENTION QUEUE'S TOOLS (attention_tools.go), WHICH ARE ON MCP BY
// OPERATOR DECISION: "all agents" ask their owner (attention_ask/wait/cancel)
// and work their OWN tasks — the dispatched one and the ones they created
// (tasks_list, task_get, task_comment, task_set_status; never into or out of
// `complete`) — and the chief alone lists the queue (read-only: only the
// operator answers) and works the whole
// board. That deliberately widens what a claude-code pod reaches on the connect
// listener from its connectors to its own task and its questions; the bound
// task's agent_* tools above stay gateway-only as they were.
//
// 🔴 chiefOnly IS A SECOND, ORTHOGONAL AXIS: WHO, NOT WHICH TRANSPORT. A
// chief-only tool is offered (on both transports) to the owner's CHIEF agent and
// to no other — the predicate is chiefPredicate's, the one every chief decision in
// this package uses. It is enforced twice: the listing ([Server.AgentToolDefsFor],
// [Server.MCPToolDefsFor]) leaves it out for anyone else, and dispatchAgentTool
// refuses it for anyone else, because a gateway runtime can name a tool it was
// never offered and the dispatch closure is the trust boundary.
// TestTheChiefOnlyToolsAreLedgered pins the set.
//
// notChief is the same axis turned round: a tool offered to every agent BUT the
// chief, enforced the same two ways. The chief's tool list is what its model
// carries on every turn, so a tool it has no use for is left off it:
// connector_request / connector_status (the chief's connectors are the owner's
// standing grants; connectors_list still shows them) and the browser-flow
// pair. TestTheChiefsToolListIsPinned pins the chief's whole list.
type agentTool struct {
	def agents.ToolDef
	// mcp: also served over POST /mcp to claude-code agents. See above before
	// setting it.
	mcp bool
	// chiefOnly: offered to, and callable by, the chief agent alone. See above.
	chiefOnly bool
	// notChief: offered to, and callable by, every agent but the chief. See
	// above.
	notChief bool
	// inProcess, when set, is the definition a GATEWAY-runtime agent is given
	// in place of def (AgentToolDefsFor): its calls run in muster's process, so
	// a form only an MCP caller can use is left out (route_to_session's text).
	inProcess *agents.ToolDef
	// officerOnly: offered to, and callable by, a declared officer's instance
	// agents alone (officers.go's officerOf — the binding rule). A THIRD axis,
	// orthogonal to the chief's: enforced in the listing (toolsFor) and again in
	// dispatchAgentTool, and the tool itself acts only for the caller's own
	// officer.
	officerOnly bool
}

// agentTools is the registry. The connector tools are present only when the
// connector store is configured, so an agent is never offered a tool that can
// only answer "unconfigured".
func (s *Server) agentTools() []agentTool {
	fn := func(name, desc string, params map[string]any) agentTool {
		return agentTool{def: agents.ToolDef{Type: "function", Name: name, Description: desc, Parameters: params}}
	}
	tools := []agentTool{
		fn("agent_get_task", "Read your assigned task: its text, status, and comments.", objSchema(map[string]any{})),
		fn("agent_comment_task", "Post a progress comment on your task.", objSchema(map[string]any{
			"body": strProp("comment text"),
		}, "body")),
		fn("agent_set_task_status", "Set your task's status. Allowed: open, in_progress, ready_for_review (you cannot set complete).", objSchema(map[string]any{
			"status": map[string]any{"type": "string", "enum": []string{"open", "in_progress", "ready_for_review"}},
		}, "status")),
		fn("agent_list_pull_requests", "List the pull requests on YOUR repo (the one cloned into your workspace). muster calls the GitHub API with its stored token server-side — do NOT ask for a token. Returns each PR's number, title, state, author, head→base branches, draft flag, url and updated time.", objSchema(map[string]any{
			"state": map[string]any{"type": "string", "description": "which PRs to list", "enum": []string{"open", "closed", "all"}},
		})),
	}
	// agent_report_done (agent_report_done.go): a started agent's EXPLICIT
	// "I finished" — the one completion signal besides moving its task. Every
	// agent but the chief, and on MCP so a claude-code agent has it too; only
	// with the outcome store that records it.
	if s.outcomesOn() {
		tools = append(tools, agentTool{def: agentReportDoneToolDef(), mcp: true, notChief: true})
	}
	// ask_owner (owner_ask.go): the chief's question card in its own chat.
	// Chief-only, and on MCP so a claude-code chief has it too. OFFERED WITH NO
	// GATE: it needs no store (the card is drawn from the stored call), every
	// chief has a chat, and both chiefs' standing instructions name it
	// unconditionally. ⚠ The cost: with a chief-only tool always in the
	// registry, toolsFor reads the agent list on every turn of every agent.
	tools = append(tools, agentTool{def: askOwnerToolDef(), mcp: true, chiefOnly: true})
	// 🔴 EACH GATE BELOW ALSO READS THE FEATURE REGISTRY (featureOff): a
	// feature it calls off has no tool offered, and a call to one explains
	// instead (features.go's toolFeatures, mcpDisabledTool). The two agree by
	// construction: same features, same predicate, same definitions.
	if s.ext.Connectors != nil && !s.featureOff(features.Connectors) {
		for _, d := range connectorToolDefs() {
			tools = append(tools, agentTool{def: d, mcp: true, notChief: d.Name != "connectors_list"})
		}
	}
	// Browser flows (flow_tools.go): MCP too — any agent may ASK; only its
	// owner's approval on /flows makes anything run.
	if s.ext.Flows != nil && !s.featureOff(features.BrowserFlows) {
		for _, d := range flowToolDefs() {
			tools = append(tools, agentTool{def: d, mcp: true, notChief: true})
		}
	}
	// The plugin interface (gated_task_tool.go): the chief may FILE a task its
	// owner must approve. MCP too, so a claude-code chief reaches it; chief-only,
	// because the chief is the one agent plugin messages are delivered to.
	if s.ext.Plugins != nil && !s.featureOff(features.Plugins) {
		tools = append(tools, agentTool{def: gatedTaskToolDef(), mcp: true, chiefOnly: true})
		// reply_draft (reply_draft_tool.go): the chief may DRAFT a reply its
		// owner must approve before the plugin sends it. MCP and chief-only for
		// task_create_gated's reasons.
		tools = append(tools, agentTool{def: replyDraftToolDef(), mcp: true, chiefOnly: true})
		// plugin_message_settle (unrouted.go): the chief records that a message
		// needs nothing more, so muster raises no ask for it. MCP and chief-only
		// for reply_draft's reasons.
		tools = append(tools, agentTool{def: pluginMessageSettleToolDef(), mcp: true, chiefOnly: true})
		// routing_rule_propose (routing_rules.go): the chief proposes a rule
		// that places later messages like one it was delivered without a turn;
		// only the owner's tap on its card makes it run. MCP and chief-only for
		// reply_draft's reasons; needs the attention store (the card).
		if s.rulesOffered() {
			tools = append(tools, agentTool{def: routingRuleProposeToolDef(), mcp: true, chiefOnly: true})
		}
	}
	// Session focus (focus_tools.go): the chief lists and searches its owner's
	// sessions' focus. MCP too, so a claude-code chief reaches them. Both are
	// chief-only: they reach the owner's whole fleet.
	//
	// ⚠ GATED ON THE STORE ONLY, NOT ON THE FOCUS FEATURE (#441 round 1): every
	// tool here READS what is stored — focus already written, live transcripts,
	// session history — and calls no model. "Focus off" (no OpenRouter key,
	// MUSTER_FOCUS_INTERVAL=0) means nothing NEW is summarized; the stored
	// history and the overview still work, so they stay offered.
	if s.ext.Focus != nil {
		for _, d := range focusToolDefs() {
			tools = append(tools, agentTool{def: d, mcp: true, chiefOnly: true})
		}
		// sessions_overview (sessions_overview.go): what every live session is
		// doing now, transcripts included. Chief-only and on MCP for the focus
		// tools' reasons; it reads the owner's whole fleet.
		tools = append(tools, agentTool{def: sessionsOverviewToolDef(), mcp: true, chiefOnly: true})
		// session_context (session_context.go): one mirrored session in detail,
		// with a tail long enough to hold a kickoff — the session the owner asked
		// about from its view (view_chief.go). Chief-only and on MCP for the
		// overview's reasons; read-only.
		tools = append(tools, agentTool{def: sessionContextToolDef(), mcp: true, chiefOnly: true})
		// fleet_report (fleet_report.go): every session with focus activity in
		// a window, ended ones included. Chief-only and on MCP for the same
		// reasons; it reads the owner's whole fleet.
		tools = append(tools, agentTool{def: fleetReportToolDef(), mcp: true, chiefOnly: true})
	}
	// Session routing (session_routes.go), only with MUSTER_SESSION_ROUTING on,
	// with or without plugins: the chief hands a mirrored session the owner's
	// own request, a task or (with plugins) a plugin message (chief-only, like
	// reply_draft). The session answers with route_reply.
	if routesOn(s.ext) && !s.featureOff(features.SessionRouting) {
		gw := routeToolDefInProcess()
		tools = append(tools, agentTool{def: routeToolDef(), inProcess: &gw, mcp: true, chiefOnly: true})
	}
	// Task routing (task_routing.go), only with MUSTER_TASK_ROUTING on: the
	// chief decides what happens to a task a plugin handed it — the attention
	// queue, a forward, or (MUSTER_CHIEF_DISPATCH_OWNERS) a new agent. Chief-only and
	// on MCP, like route_to_session.
	if taskRoutingOn(s.ext) && !s.featureOff(features.TaskRouting) {
		for _, d := range taskRoutingToolDefs(chiefDispatchOn(s.ext)) {
			tools = append(tools, agentTool{def: d, mcp: true, chiefOnly: true})
		}
		// repos_list (repos_list.go): the owner's connected repositories, the
		// only ones agent_dispatch starts in. Read-only; offered with
		// agent_dispatch, chief-only and on MCP like it.
		if chiefDispatchOn(s.ext) {
			tools = append(tools, agentTool{def: reposListToolDef(), mcp: true, chiefOnly: true})
		}
		// succession_propose (succession.go): the chief proposes a session that
		// continues the one its owner asked about from its view; only the owner's
		// tap on the card starts it. Chief-only and on MCP, with agent_dispatch.
		if successionOffered(s.ext) {
			tools = append(tools, agentTool{def: successionProposeToolDef(), mcp: true, chiefOnly: true})
		}
		// end_session_propose (end_session.go): the chief proposes ending one
		// of its owner's sessions; only the owner's tap ends it.
		if endSessionOffered(s.ext) {
			tools = append(tools, agentTool{def: endSessionToolDef(), mcp: true, chiefOnly: true})
		}
	}
	// The attention queue (attention_tools.go), present with its store: every
	// agent may ask its owner and work the owner's tasks; the chief alone lists
	// the queue (read-only). All on MCP too.
	// attention_answer is NOT here: it is the mirrored sessions' alone
	// (mirror_mcp.go's mirrorTools), and its handler refuses any agent. Nor are
	// connector_requests_list / connector_request_decide, for the same reason
	// (connector_decide_tools.go), nor agent_destroy (agent_destroy_tool.go).
	if s.attentionToolsOn() {
		for _, d := range attentionToolDefs() {
			tools = append(tools, agentTool{def: d, mcp: true})
		}
		for _, d := range taskToolDefs() {
			tools = append(tools, agentTool{def: d, mcp: true})
		}
		for _, d := range chiefAttentionToolDefs() {
			tools = append(tools, agentTool{def: d, mcp: true, chiefOnly: true})
		}
		// A mirrored session's question to the chief (chief_ask_tool.go): the
		// chief answers it. Chief-only, on MCP.
		tools = append(tools, agentTool{def: answerSessionQuestionToolDef(), mcp: true, chiefOnly: true})
	}
	// The officer runtime (officers.go), present when officers are declared:
	// an officer's instances reach their HQ queue through muster, which holds
	// the officer's token. On MCP (officer instances are claude-code agents);
	// officer-only.
	if s.officerRT != nil && !s.featureOff(features.Officers) {
		for _, d := range officerToolDefs() {
			tools = append(tools, agentTool{def: d, mcp: true, officerOnly: true})
		}
	}
	return tools
}

// toolsFor is the registry as agent a may see it: every tool, minus the
// chief-only ones unless a is its owner's chief, and minus the not-chief ones if
// it is. It reads the agent list only when the registry HAS a tool on either
// axis. ⚠ Since ask_owner (offered with no gate) that is every registry, so the
// read is paid on every turn.
//
// 🔴 THE CHIEF-ONLY AXIS FAILS CLOSED: an agent list that cannot be read offers
// no chief-only tool, the same direction agentIsChief's other callers take for
// the reverse question. ⚠ The notChief axis therefore errs the OTHER way: the
// same unreadable list counts the chief as "not the chief", so it is offered the
// notChief tools. That is harmless — every worker has them, and they only file
// requests the owner must approve — and it only lengthens a list.
func (s *Server) toolsFor(ctx context.Context, a agents.Agent) []agentTool {
	all := s.agentTools()
	chief, acting, decided := false, false, false
	decide := func() {
		if !decided {
			// acting: the chief AND on. A chief its owner turned off keeps
			// no chief-only tool (chief_toggle.go), and is still the chief
			// for the notChief axis.
			chief, decided = s.isChiefAgent(ctx, a), true
			acting = chief && s.ownersChiefOn(ctx, a)
		}
	}
	out := make([]agentTool, 0, len(all))
	for _, t := range all {
		// The Compose chief is offered exactly its pre-allowed tools
		// (composeChiefRefuses' rule, with the chief decided once per listing).
		if s.ext.ComposeChief != nil && !agentspec.ComposeChiefAllowed(t.def.Name) {
			if decide(); chief {
				continue
			}
		}
		if t.officerOnly {
			o, _, officer := s.officerOf(a)
			if !officer || !officers.ToolOffered(o, t.def.Name) {
				continue
			}
			// the definition as o's pool is offered it (officer_complete's
			// outcomes and investigation differ per pool)
			t.def = officerToolDefFor(o, t.def)
		}
		if t.chiefOnly || t.notChief {
			decide()
			if (t.chiefOnly && !acting) || (t.notChief && chief) {
				continue
			}
		}
		out = append(out, t)
	}
	return out
}

// composeChiefRefuses reports whether name is refused to agent a because a is
// the chief on the Docker Compose install (Extensions.ComposeChief) and name is
// not one of its pre-allowed tools (agentspec.ComposeChiefAllowedMCPTools).
//
// 🔴 ONE LIST, THREE CONSUMERS. In the Compose container every permission
// prompt is denied, so a tool outside that list is one the chief can never
// run. The spec pre-allows the list (agentspec.Build), the listing offers
// nothing outside it (toolsFor), and this re-check refuses a call by name, so
// the chief is never offered a tool it cannot use. What it is TOLD derives from
// the same list: its CLAUDE.md (agentspec.ChiefInstructionsCompose), the MCP
// instructions (mcpInstructionsFor) and every turn muster sends it
// (ChiefTurnTools; TestNoComposeChiefTurnTextNamesAToolItIsNotOffered).
func (s *Server) composeChiefRefuses(ctx context.Context, a agents.Agent, name string) bool {
	return !agentspec.ComposeChiefAllowed(name) && s.isComposeChief(ctx, a)
}

// isComposeChief reports whether a is the chief on the Docker Compose install
// (Extensions.ComposeChief).
func (s *Server) isComposeChief(ctx context.Context, a agents.Agent) bool {
	return s.ext.ComposeChief != nil && s.isChiefAgent(ctx, a)
}

// ChiefTurnTools is the muster tools chief is offered when that set is
// restricted — the Compose chief's: exactly what toolsFor lists it, read from
// that one listing — or nil (unrestricted) for every other chief. Every turn
// text muster sends the chief that names a tool is rendered against it
// (plugins.ChiefTools): the plugin turn (DelivererConfig.Tools), a route's
// hand-back (handBackChoices), and a session's question (toolChiefAsk, which
// does not put one to a chief without answer_session_question).
func (s *Server) ChiefTurnTools(ctx context.Context, chief agents.Agent) plugins.ChiefTools {
	if s.ext.ComposeChief == nil {
		return nil
	}
	set := plugins.ChiefTools{}
	for _, d := range s.MCPToolDefsFor(ctx, chief) {
		set[d.Name] = true
	}
	return set
}

// composeChiefFallbackTools is the Compose chief's pre-allowed set as
// plugins.ChiefTools: what a turn is rendered for when the chief's own listing
// cannot be read, so it still names no tool outside that set.
func composeChiefFallbackTools() plugins.ChiefTools {
	set := plugins.ChiefTools{}
	for _, n := range agentspec.ComposeChiefAllowedMCPTools {
		set[n] = true
	}
	return set
}

// composeChiefRefusal is the Compose chief's answer for a tool outside its
// pre-allowed set.
func composeChiefRefusal(name string) string {
	return name + " is not available to the chief on this install (Docker Compose). " + agentspec.ComposeChiefUnavailable
}

// toolAxis is name's two who-axis flags in the registry (both false for a name
// it does not declare). It reads the registry rather than a second list, so the
// flags are the one place the decision is written.
func (s *Server) toolAxis(name string) (chiefOnly, notChief bool) {
	for _, t := range s.agentTools() {
		if t.def.Name == name {
			return t.chiefOnly, t.notChief
		}
	}
	return false, false
}

// isChiefAgent answers chiefPredicate for a's CURRENT row (a caller's copy of the
// row may predate a relabel). A read error is "not the chief".
func (s *Server) isChiefAgent(ctx context.Context, a agents.Agent) bool {
	_, chief, err := s.agentIsChief(ctx, a.ID)
	if err != nil {
		s.logger.Printf("agent tools: agent %d: could not decide whether it is the chief: %v", a.ID, err)
		return false
	}
	return chief
}

// AgentToolDefsFor returns the native function tools given to gateway agent a:
// every tool in the registry a may see (toolsFor).
//
// 🔴 THERE IS NO AGENT-LESS VARIANT, ON PURPOSE. An unfiltered listing is how a
// chief-only tool would reach a worker; every consumer names the agent.
func (s *Server) AgentToolDefsFor(ctx context.Context, a agents.Agent) []agents.ToolDef {
	tools := s.toolsFor(ctx, a)
	defs := make([]agents.ToolDef, 0, len(tools))
	for _, t := range tools {
		if t.inProcess != nil {
			defs = append(defs, *t.inProcess)
			continue
		}
		defs = append(defs, t.def)
	}
	return defs
}

// MCPToolDefsFor returns the MCP subset of what agent a may see, in registry
// order.
func (s *Server) MCPToolDefsFor(ctx context.Context, a agents.Agent) []agents.ToolDef {
	var defs []agents.ToolDef
	for _, t := range s.toolsFor(ctx, a) {
		if t.mcp {
			defs = append(defs, t.def)
		}
	}
	return defs
}

// AgentInstructions returns the worker's system prompt (used by the kickoff loop
// AND every interactive chat turn). It grounds the agent in its ACTUAL
// environment — its repo and, when configured, the connector store — and tells it to describe only its real capabilities. The
// base image prompt otherwise advertises a generic assistant feature set (Signal/
// Telegram messaging, paired devices, image/video, calendar/email) this sandboxed
// worker does not have, so "what can you do" answers confabulate without this.
func (s *Server) AgentInstructions(a agents.Agent) string {
	b := agentSystemPrompt
	if a.Repo != "" {
		repo := a.Repo
		if i := strings.LastIndex(repo, "/"); i >= 0 && i+1 < len(repo) {
			b += "\n\nYour workspace is the GitHub repo " + a.Repo + ", cloned at /data/repos/" + repo[i+1:] + " — read/write/edit files there and run git + shell commands."
		}
		b += "\n\nTo see this repo's pull requests, call agent_list_pull_requests{state: open|closed|all} — do NOT ask for a GitHub token, it's handled server-side."
		// Close-out protocol: only when the agent is working a dispatched TASK
		// (NoteID) on a repo. Without this, agents complete the edit + tests and
		// STOP — the change is trapped in the ephemeral workspace and the task never
		// closes. This turns the loop into a reviewable artifact + a status report.
		//
		// 🔴 NO GITHUB CREDENTIAL IS IN THE POD. This text used to say "the git
		// credential helper + GITHUB_TOKEN are already configured"; agentspec's
		// buildSecrets emits no GITHUB_TOKEN (nor any GIT_CONFIG_* helper), so that
		// was false for every agent this binary provisions. Pushing goes through the
		// connector store's proxy when it is offered, and is otherwise reported as
		// impossible rather than retried against a token that is not there.
		if a.NoteID != nil && s.githubAppReadOnly() {
			b += "\n\nCLOSE THE LOOP when the task's change is complete and verified — do NOT just stop after editing/testing:" +
				readOnlyCloseOut
		} else if a.NoteID != nil {
			b += "\n\nCLOSE THE LOOP when the task's change is complete and verified — do NOT just stop after editing/testing:" +
				s.pushSteps(a.Repo) +
				"\n3. Call agent_comment_task with the PR URL, then agent_set_task_status{status:\"ready_for_review\"}." +
				"\nOnly finish once the change is pushed, a PR is open, and the task is marked ready_for_review. If you genuinely cannot (e.g. no change is needed), comment on the task explaining why and set it ready_for_review."
		}
	}
	if s.ext.Connectors != nil {
		b += "\n\nTo use an external platform's API (e.g. GitHub), ask for its connector with connector_request, wait with connector_status, then call it through muster's proxy: " +
			ProxyRecipe("<connector>", "<host>") + " You never see the credential."
	}
	if s.ext.Flows != nil {
		b += "\n\nTo have your owner's logged-in browser run a named flow (" + strings.Join(flows.Names(), ", ") +
			"), call browser_flow_request; your owner approves each run in muster, then poll browser_flow_status."
	}
	if s.attentionToolsOn() {
		b += "\n\n" + s.attentionInstructionFor(a)
	}
	b += "\n\nWhen asked what you can do, describe ONLY these real capabilities: your task tools (above), reading/writing files + git + shell in your workspace" +
		". Do NOT claim messaging (Signal/Telegram/WhatsApp/Discord), paired devices/cameras/location, image/video generation, web browsing, or calendar/email — you do not have those here."
	return b
}

// githubAppReadOnly reports whether this deployment's GitHub App mints
// read-only agent tokens (MUSTER_GITHUB_APP_ACCESS=read).
func (s *Server) githubAppReadOnly() bool {
	return s.ext.GitHubApp != nil && s.ext.GitHubApp.Access() == githubapp.AccessRead
}

// readOnlyCloseOut is the close-out protocol when the GitHub App mints
// read-only tokens. 🔴 IT NEVER TELLS THE AGENT TO PUSH OR OPEN A PULL REQUEST:
// GitHub refuses both with such a token, and an instruction to do it anyway is
// a claim of write access the deployment does not grant.
const readOnlyCloseOut = "\n1. Commit your work on a branch in your workspace. This deployment's GitHub access is READ ONLY " +
	"(MUSTER_GITHUB_APP_ACCESS=read): you cannot push or open a pull request — do NOT try, and do NOT ask for a token." +
	"\n2. Call agent_comment_task naming the branch and summarising the change (what, why, how you verified it), then agent_set_task_status{status:\"ready_for_review\"}." +
	"\nOnly finish once that comment is posted and the task is marked ready_for_review."

// pushSteps is steps 1-2 of the close-out protocol: how to push and open a PR.
// With the connector store configured the agent goes through the `github`
// connector; without it muster provides no GitHub credential at all.
func (s *Server) pushSteps(repo string) string {
	if s.ext.Connectors == nil {
		return "\n1. Create a branch and commit your work, then `git push` it. muster provides no GitHub credential here — do NOT ask for a token; if the push is refused, name your branch in step 3 and explain." +
			"\n2. If the push succeeded, open a pull request against the repo's default branch if this environment lets you."
	}
	return "\n1. Create a branch and commit your work. You hold no GitHub token — do NOT ask for one. Get the `github` connector: connector_request{name:\"github\", reason:\"push <branch> to " + repo + "\"}, then connector_status{name:\"github\", wait_seconds:50} until it is approved. Push through muster's proxy: " +
		ProxyRecipe("github", "github.com") + " For git: `" + GitPushRecipe("github", "github.com/"+repo+".git") + " <branch>`." +
		"\n2. Open a pull request against the repo's default branch through the same connector: POST $MUSTER_CONNECT_URL/connect/github/api.github.com/repos/" + repo + "/pulls with the same header."
}

// GitPushRecipe is the git command line (up to the refspec) that pushes through
// a connector's proxy.
//
// 🔴 BOTH -c KEYS ARE SCOPED TO THE CONNECTOR'S PROXY URL, AND REDIRECTS ARE OFF.
// An unscoped `http.extraHeader` goes to every URL git talks to, and git's
// default redirect policy follows the first request's redirect carrying that
// header to whatever host it names — measured. Scoped to
// `$MUSTER_CONNECT_URL/connect/<connector>/` the token goes to the proxy only,
// and `followRedirects=false` keeps git from following even a redirect the
// proxy itself would never hand out (it answers 502 instead).
func GitPushRecipe(connector, path string) string {
	proxy := "$MUSTER_CONNECT_URL/connect/" + connector + "/"
	return `git -c "http.` + proxy + `.extraHeader=X-Muster-Token: $MUSTER_HOOK_TOKEN" -c "http.` + proxy +
		`.followRedirects=false" push ` + proxy + path
}

// turnTools is what one in-process turn of agent a carries — its system prompt
// (AgentInstructions), the native function tools it may see (AgentToolDefsFor)
// and the dispatch that runs them (AgentToolDispatch) — and the context the turn
// runs under: ask_owner ends the turn it is called in (owner_ask.go), and the
// tool loop reads that mark after each round of calls.
//
// 🔴 ONE ASSEMBLY FOR EVERY TOOL-ENABLED TURN: a chat turn (chatTurn) and a
// gateway agent's first turn (KickoffTurnTools). The first turn once had none of
// it, so a dispatched gateway agent — which does its whole job in that turn —
// was told to call agent_report_done and never offered it.
func (s *Server) turnTools(ctx context.Context, a agents.Agent) (context.Context, agentkickoff.TurnTools) {
	ctx = agents.WithTurnEnd(ctx)
	return ctx, agentkickoff.TurnTools{Instructions: s.AgentInstructions(a), Tools: s.AgentToolDefsFor(ctx, a),
		Dispatch: s.AgentToolDispatch(ctx, a)}
}

// KickoffTurnTools is agentkickoff.Config.TurnTools: a GATEWAY-kind agent's first
// turn carries its muster tools (gatewayTurnTools), as each of its chat turns does.
func (s *Server) KickoffTurnTools(ctx context.Context, a agents.Agent) (context.Context, agentkickoff.TurnTools, bool) {
	return s.gatewayTurnTools(ctx, a, authz.ViaKickoff)
}

// gatewayTurnTools is what a turn of agent a that muster starts OUTSIDE a
// session of the owner's carries: for a GATEWAY-kind agent, its muster tools
// (turnTools) under a context acting as its owner through via; for any other
// agent, or a row with no owner, ok false and the turn goes out toolless.
//
// 🔴 ONE DECISION FOR EVERY SUCH DOOR: the kickoff deliverer's first turn
// (KickoffTurnTools, authz.ViaKickoff) and the machine message door
// (handleAPIAgentSendMessage via gatewayTurn, authz.ViaAgentMessage). Each once
// open-coded a toolless turn of its own, so a gateway agent reached through it
// was told to call agent_report_done or attention_ask and never offered either.
// gateway_turn_ledger_test.go pins every call site that starts a turn.
//
// 🔴 THE TURN'S TOOLS RUN AS THE AGENT'S OWNER (turnPrincipal), NEVER AS THE
// CALLER'S PRINCIPAL — the deliverer's authz.System, or the machine door's
// operator. Under System row-level security sees every owner's rows, so an
// agent's tools would reach across owners; and an insert that takes its owner
// from the principal (attention_ask, connector_request) fails outright —
// measured: both answered an error under System and succeeded as the owner.
//
// ⚠ NOT FOR A CLAUDE-CODE AGENT (ok false): its CLI reaches muster's tools over
// MCP from inside its pod (MCPToolDefsFor), and ccd accepts and ignores a
// request's instructions and tools (cmd/ccd/server.go responsesRequest), so its
// turn stays the toolless one it always was.
func (s *Server) gatewayTurnTools(ctx context.Context, a agents.Agent, via authz.Via) (context.Context, agentkickoff.TurnTools, bool) {
	if agents.ResolveKind(a.Kind) != agents.KindGateway {
		return ctx, agentkickoff.TurnTools{}, false
	}
	pctx, ok := s.turnPrincipal(ctx, a, via)
	if !ok {
		s.logger.Printf("agents: %s names no owner; its %s turn goes out WITHOUT tools (they act as the owner)", a.Name, via)
		return ctx, agentkickoff.TurnTools{}, false
	}
	ctx, tt := s.turnTools(pctx, a)
	return ctx, tt, true
}

// turnVias is the closed set of principals turnPrincipal mints: one per door
// gatewayTurnTools serves.
var turnVias = map[authz.Via]bool{authz.ViaKickoff: true, authz.ViaAgentMessage: true}

// turnPrincipal is ctx acting as agent a's owner, through via (a turnVias
// member): how a gateway agent's turn started outside an owner's session runs
// its tools, so row-level security confines them to that owner's rows.
//
// 🔴 FALSE FOR A ROW THAT NAMES NO OWNER (agentPrincipal's refusal), NEVER A
// PANIC: authz.WithPrincipal panics on a principal with no user, and the kickoff
// deliverer has no recover, so one such row would crash muster. The caller sends
// that agent's turn toolless instead. False, too, for a via outside turnVias:
// this is not a general "act as the owner" door.
func (s *Server) turnPrincipal(ctx context.Context, a agents.Agent, via authz.Via) (context.Context, bool) {
	if !turnVias[via] {
		return ctx, false
	}
	p, ok := s.agentPrincipal(ctx, a, via)
	if !ok {
		return ctx, false
	}
	return authz.WithPrincipal(ctx, p), true
}

// gatewayTurn runs one turn of agent a that muster starts outside a session of
// the owner's (the machine message door): with its muster tools when
// gatewayTurnTools answers ok for it (runTurnWithTools, which owns the toolless
// fallback for an image without /v1/responses), else the toolless Chat.
func (s *Server) gatewayTurn(ctx context.Context, a agents.Agent, sessionKey, text string, via authz.Via) (reply string, err error) {
	// 🔴 THE CHIEF'S TOKEN TELEMETRY on this door too (chief_usage.go): a
	// chief turn started here — the machine message door, the kickoff — is
	// recorded as chatTurn records every other, tagged with its door.
	trigger := chiefusage.TriggerMachine
	if via == authz.ViaKickoff {
		trigger = chiefusage.TriggerKickoff
	}
	ctx = chiefusage.WithTrigger(ctx, chiefusage.Trigger{Kind: trigger})
	var emit agents.StreamEmit
	if ctx2, usage := s.beginChiefTurnUsage(ctx, a); usage != nil {
		ctx, emit = ctx2, usage.observe(nil)
		defer func() { usage.finish(ctx, err) }()
	}
	tctx, tt, ok := s.gatewayTurnTools(withTurnSession(ctx, sessionKey), a, via)
	if !ok {
		return s.ext.Gateway.Chat(ctx, a, sessionKey, text, nil)
	}
	return s.runTurnWithTools(tctx, a, sessionKey, text, tt, emit)
}

// AgentToolDispatch returns the dispatch closure executing a worker's tool calls
// in-process, scoped to that agent (its bound task). Used by both the kickoff
// (via the provisioner hook) and the interactive chat.
func (s *Server) AgentToolDispatch(ctx context.Context, a agents.Agent) agents.ToolDispatch {
	// In-process: a gateway-runtime session's turn, never a CLI over MCP —
	// which agent_dispatch's owner_asked tells apart (chief_owner_release.go).
	ctx = withInProcessToolCall(ctx)
	return func(name, arguments string) string {
		return s.dispatchAgentTool(ctx, a, name, arguments)
	}
}

func (s *Server) dispatchAgentTool(ctx context.Context, a agents.Agent, name, arguments string) string {
	arg := func(v any) error { return json.Unmarshal([]byte(orEmptyJSON(arguments)), v) }
	// 🔴 THE CHIEF-ONLY RE-CHECK. The listing already leaves a chief-only tool out
	// for any other agent, but a gateway runtime can call a name it was not
	// offered, and this closure runs whatever it is handed — so the decision is
	// made again here, against the agent's current row.
	switch chiefOnly, notChief := s.toolAxis(name); {
	case s.composeChiefRefuses(ctx, a, name):
		return toolErr("%s", composeChiefRefusal(name))
	case chiefOnly && !s.isChiefAgent(ctx, a):
		return toolErr("%s is available to your owner's chief agent only", name)
	case chiefOnly && !s.chiefActing(ctx, a):
		return toolErr("%s: your owner turned the chief off", name)
	case notChief && s.isChiefAgent(ctx, a):
		return toolErr("%s is not one of your owner's chief agent's tools", name)
	}
	// The officer tools: refused inside to any agent that is not a declared
	// officer's instance, and bound to the caller's own officer (officers.go).
	if text, ok := s.dispatchOfficerTool(ctx, a, name, arguments); ok {
		return text
	}
	switch name {
	case agentReportDoneTool:
		return s.toolAgentReportDone(ctx, a, arguments)
	case "agent_get_task":
		if a.NoteID == nil {
			return toolErr("no task assigned to you")
		}
		n, err := s.ext.Notes.Get(ctx, *a.NoteID)
		if err != nil {
			return toolErr("could not load task: %v", err)
		}
		return toolJSON(n)
	case "agent_comment_task":
		if a.NoteID == nil {
			return toolErr("no task assigned to you")
		}
		var in struct{ Body string }
		_ = arg(&in)
		if strings.TrimSpace(in.Body) == "" {
			return toolErr("body required")
		}
		c, queued, err := s.addTaskComment(withRefOrigin(ctx, refOrigin{Agent: a.Name}), commentByActor,
			notes.Comment{NoteID: *a.NoteID, Author: a.Name, Body: in.Body})
		if err != nil {
			return toolErr("could not comment: %v", err)
		}
		if queued != nil {
			return toolJSON(map[string]any{"queued": true, "write_id": queued.ID, "message": refQueuedText})
		}
		s.broadcast(ctx, EventTaskChanged, strconv.FormatInt(*a.NoteID, 10))
		return toolJSON(c)
	case "agent_set_task_status":
		if a.NoteID == nil {
			return toolErr("no task assigned to you")
		}
		var in struct{ Status string }
		_ = arg(&in)
		cur, err := s.ext.Notes.Get(ctx, *a.NoteID)
		if err != nil {
			return toolErr("could not load task: %v", err)
		}
		if why := agentStatusRefusal(cur.Status, in.Status); why != "" {
			return toolErr("%s", why)
		}
		n, err := s.setNoteStatus(notes.ForAgent(withRefOrigin(ctx, refOrigin{Author: a.Name, Agent: a.Name})), writerAgentTool, *a.NoteID, in.Status)
		if errors.Is(err, errQueuedUpstream) {
			return toolJSON(map[string]any{"queued": true, "task_id": n.ID, "message": refQueuedText})
		}
		if errors.Is(err, notes.ErrCompleteLocked) {
			return toolErr("%s", agentStatusRefusal(notes.StatusComplete, in.Status))
		}
		if err != nil {
			return toolErr("could not set status: %v", err)
		}
		s.broadcast(ctx, EventTaskChanged, strconv.FormatInt(*a.NoteID, 10))
		return toolJSON(n)
	case "agent_list_pull_requests":
		return s.listAgentPullRequests(ctx, a, arguments)
	case askOwnerTool:
		return s.toolAskOwner(ctx, a, arguments)
	case "connectors_list":
		return s.toolConnectorsList(ctx, a)
	case "connector_request":
		var in struct{ Name, Reason string }
		_ = arg(&in)
		return s.toolConnectorRequest(ctx, a, in.Name, in.Reason)
	case "browser_flow_request":
		return s.toolFlowRequest(ctx, a, arguments)
	case "browser_flow_status":
		return s.toolFlowStatus(ctx, a, arguments)
	case gatedTaskToolName:
		return s.toolTaskCreateGated(ctx, a, arguments)
	case replyDraftToolName:
		return s.toolReplyDraft(ctx, a, arguments)
	case pluginMessageSettleToolName:
		return s.toolPluginMessageSettle(ctx, a, arguments)
	case routingRuleProposeTo:
		if !s.rulesOffered() {
			return toolErr("unknown tool %q", name)
		}
		return s.toolRoutingRulePropose(ctx, a, arguments)
	case fleetListToolName:
		return s.toolFleetList(ctx)
	case focusSearchToolName:
		return s.toolFocusSearch(ctx, arguments)
	case sessionsOverviewToolName:
		return s.toolSessionsOverview(ctx, a, arguments)
	case sessionContextToolName:
		return s.toolSessionContext(ctx, arguments)
	case reposListTool:
		// Re-checked at the call: a gateway runtime can name a tool it was
		// never offered.
		if !chiefDispatchOn(s.ext) || !taskRoutingOn(s.ext) {
			return toolErr("unknown tool %q", name)
		}
		return s.toolReposList(ctx, arguments)
	case successionProposeTool:
		if !successionOffered(s.ext) {
			return toolErr("unknown tool %q", name)
		}
		return s.toolSuccessionPropose(ctx, a, arguments)
	case endSessionProposeTool:
		if !endSessionOffered(s.ext) {
			return toolErr("unknown tool %q", name)
		}
		return s.toolEndSessionPropose(ctx, a, arguments)
	case fleetReportToolName:
		return s.toolFleetReport(ctx, a, arguments)
	case routeToSessionToolName:
		return s.toolRouteToSession(ctx, a, arguments)
	case answerSessionQuestionTool:
		// Registered only with the attention store (agentTools).
		if !s.attentionToolsOn() {
			return toolErr("unknown tool %q", name)
		}
		return s.toolAnswerSessionQuestion(ctx, a, arguments)
	case postgresQueryToolName:
		return s.toolPostgresQuery(ctx, a, arguments)
	case "connector_status":
		var in struct {
			Name        string `json:"name"`
			WaitSeconds int    `json:"wait_seconds"`
		}
		_ = arg(&in)
		return s.toolConnectorStatus(ctx, a, in.Name, in.WaitSeconds)
	default:
		if text, ok := s.dispatchTaskRoutingTool(ctx, a, name, arguments); ok {
			return text
		}
		// dispatchCallerTool runs nothing without the attention store (it
		// answers false before touching it).
		if text, ok := s.dispatchCallerTool(ctx, callerOfAgent(a), name, arguments); ok {
			return text
		}
		return toolErr("unknown tool %q", name)
	}
}

// listAgentPullRequests backs the agent_list_pull_requests worker tool: it
// resolves the connected GitHub token (server-side, never surfaced to the LLM),
// lists the agent's OWN repo's PRs, and returns a compact JSON array. The repo
// is implicit (a.Repo) — an agent only reads its own repo.
func (s *Server) listAgentPullRequests(ctx context.Context, a agents.Agent, arguments string) string {
	if strings.TrimSpace(a.Repo) == "" {
		return toolErr("agent has no repo")
	}
	var in struct{ State string }
	_ = json.Unmarshal([]byte(orEmptyJSON(arguments)), &in)
	state := strings.TrimSpace(in.State)
	switch state {
	case "open", "closed", "all":
	default:
		state = "open"
	}
	var prs []github.PullRequest
	var err error
	app := s.githubApp(ctx)
	if app == nil {
		return toolErr("no GitHub connection")
	}
	// The App's token for THIS agent's repository (githubapp.App.ListPullRequests).
	prs, err = app.ListPullRequests(ctx, a.ID, a.Repo, state)
	if err != nil {
		return toolErr("could not list pull requests: %v", err)
	}
	if len(prs) > 50 {
		prs = prs[:50]
	}
	// A compact view: drop nested noise, keep what an agent needs to reason.
	type prView struct {
		Number    int    `json:"number"`
		Title     string `json:"title"`
		State     string `json:"state"`
		Author    string `json:"author"`
		Head      string `json:"head"`
		Base      string `json:"base"`
		Draft     bool   `json:"draft"`
		URL       string `json:"url"`
		UpdatedAt string `json:"updated"`
	}
	out := make([]prView, 0, len(prs))
	for _, p := range prs {
		out = append(out, prView{
			Number: p.Number, Title: p.Title, State: p.State, Author: p.User,
			Head: p.HeadRef, Base: p.BaseRef, Draft: p.Draft, URL: p.HTMLURL, UpdatedAt: p.UpdatedAt,
		})
	}
	return toolJSON(out)
}
