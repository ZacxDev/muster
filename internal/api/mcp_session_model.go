package api

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/users"
)

// EVERY WRITE AN OPENROUTER SESSION MAKES NAMES ITS MODEL (operator decision).
//
// A chief side chat on the opencode runtime calls muster's MCP door with the
// chief's token like every CLI in its pod; ccd's generated MCP config adds
// agents.HeaderOCSession: <the session's key> to its calls. Such a call:
//
//   - runs with the session's model on its context (users.WithSessionModel),
//     so every audit_log row it writes — a route, a dispatch, a gated task, a
//     draft, a settle, a rule proposal, an answered question — carries
//     session_model and session_key in its detail (users.auditInsertFor);
//   - and leaves one row of its own, chief_session.tool_call {tool,
//     session_model, session_key, ok} (auditOpencodeToolCall), so a write
//     whose own record has no audit row (a task comment, a status change, a
//     cancelled ask) is still on the log with the model that made it.
//
// 🔴 A HEADER NAMING NO OPENCODE SESSION OF THIS CHIEF REFUSES THE CALL: the
// lane's writes are never left unstamped by a session that could not be
// read. ⚠ The header can be forged from inside the pod (any process there
// holds the chief's token): the worst a forgery buys is a call stamped with
// another of the same chief's OpenRouter sessions — a label, not a power.

// ChiefSessionToolCallAction is the per-call audit row's action.
const ChiefSessionToolCallAction = "chief_session.tool_call"

// mcpOpencodeSession is ctx stamped with the opencode session key names, or
// the refusal.
func (s *Server) mcpOpencodeSession(ctx context.Context, a agents.Agent, key string) (context.Context, users.SessionModel, string) {
	st, ok := s.sessionRuntimeStore()
	if !ok || len(key) > chiefSessionMaxKeyLen {
		return ctx, users.SessionModel{}, toolErr("this call names an opencode session (%s) muster cannot read", agents.HeaderOCSession)
	}
	sess, err := st.SessionByKey(ctx, a.ID, key)
	switch {
	case err != nil:
		s.logger.Printf("mcp: agent %d: the opencode session its call names: %v", a.ID, err)
		return ctx, users.SessionModel{}, toolErr("muster could not read the session this call names (%s); retry shortly", agents.HeaderOCSession)
	case sess.AgentID != a.ID || sess.Runtime != agents.SessionRuntimeOpencode || !agents.SessionOnOpenRouter(sess.Model):
		return ctx, users.SessionModel{}, toolErr("the session this call names (%s) is not one of your opencode sessions", agents.HeaderOCSession)
	}
	sm := users.SessionModel{SessionKey: sess.SessionKey, Model: sess.Model}
	return users.WithSessionModel(ctx, sm), sm, ""
}

// usersAuditor is the users store's audit writer (users.PGStore.Audit).
type usersAuditor interface {
	Audit(ctx context.Context, by users.Actor, action, object string, detail map[string]any) error
}

// auditOpencodeToolCall writes the call's own row. A row that cannot be
// written is logged: the call itself already ran.
func (s *Server) auditOpencodeToolCall(ctx context.Context, a agents.Agent, sm users.SessionModel, tool, out string) {
	au, ok := s.ext.Users.(usersAuditor)
	if !ok {
		return
	}
	by := users.Actor{UserID: agentOwnerID(a), Via: authz.ViaAgent}
	if p, ok := authz.From(ctx); ok && p.UserID > 0 {
		by = users.Actor{UserID: p.UserID, Via: p.Via}
	}
	detail := map[string]any{"tool": tool, "ok": !toolOutputFailed(out), "agent_id": a.ID}
	if err := au.Audit(ctx, by, ChiefSessionToolCallAction, "agent:"+strconv.FormatInt(a.ID, 10), detail); err != nil {
		s.logger.Printf("mcp: agent %d: audit %s of %s (%s): %v", a.ID, ChiefSessionToolCallAction, tool, sm.SessionKey, err)
	}
}

// toolOutputFailed reports whether a tool's output is toolErr's shape: a JSON
// object with an "error".
func toolOutputFailed(out string) bool {
	var m map[string]any
	if json.Unmarshal([]byte(out), &m) != nil {
		return false
	}
	_, failed := m["error"]
	return failed
}
