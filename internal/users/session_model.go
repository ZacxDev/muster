package users

import "context"

// THE MODEL BEHIND A WRITE (operator decision: every write an OpenRouter chief
// session makes records session_model on its audit row). internal/api's MCP
// door puts the calling session's model on the request's context when the
// call comes from an opencode lane (its MCP config names the session); every
// audit_log row this package writes under that context carries it
// (auditInsertFor).

// The detail keys a stamped row carries.
const (
	AuditSessionModelKey = "session_model"
	AuditSessionKeyKey   = "session_key"
)

// SessionModel is the session a call came from and the model it runs on.
type SessionModel struct {
	SessionKey, Model string
}

type sessionModelKey struct{}

// WithSessionModel returns ctx carrying sm.
func WithSessionModel(ctx context.Context, sm SessionModel) context.Context {
	return context.WithValue(ctx, sessionModelKey{}, sm)
}

// SessionModelFrom is the session model ctx carries, if any.
func SessionModelFrom(ctx context.Context) (SessionModel, bool) {
	sm, ok := ctx.Value(sessionModelKey{}).(SessionModel)
	return sm, ok && sm.Model != ""
}
