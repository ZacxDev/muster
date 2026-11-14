package agents

import "context"

// Secrets is an agent's two credentials: [Agent.HooksToken] (its muster
// identity) and [Agent.GatewaySecret] (its turn credential). They live in the
// agent_secrets table (migration 0128), apart from the agents row.
//
// 🔴 AN ORDINARY READ OF THE AGENTS ROW DOES NOT CARRY THEM. Get, GetByName,
// GetByHooksToken, List, AgentsByNoteIDs and every other read that scans
// agentCols return an Agent whose HooksToken and GatewaySecret are "". The
// agents table is the one a later slice makes readable by a team's members, and
// a member's request must never load another owner's agent credentials into
// memory. Code that genuinely needs a credential (the turn sender, the
// provisioner that ships it to the pod, the first-turn and delivery checks)
// asks for it through [SecretReader], by agent id, and nothing else does.
// internal/agents's secrets_ledger_test.go pins every query that names either
// column, so a new reader is a decision rather than an accident.
type Secrets struct {
	HooksToken    string
	GatewaySecret string
}

// SecretReader is the one read of an agent's credentials.
type SecretReader interface {
	// Secrets returns agent id's credentials, each "" when the agent has none
	// yet. An agent the caller cannot see is an error (pgx.ErrNoRows on the
	// Postgres store), not empty credentials.
	Secrets(ctx context.Context, id int64) (Secrets, error)
}

// WithSecrets returns a with its credentials read from r. They REPLACE whatever
// a carried: the store is the only source, so a row that came from an ordinary
// read (which carries none) and one that came from a fake are treated alike.
func WithSecrets(ctx context.Context, r SecretReader, a Agent) (Agent, error) {
	s, err := r.Secrets(ctx, a.ID)
	if err != nil {
		return a, err
	}
	a.HooksToken, a.GatewaySecret = s.HooksToken, s.GatewaySecret
	return a, nil
}
