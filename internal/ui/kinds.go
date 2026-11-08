package ui

// KindChoices is what a caller may create, by agent kind: the kinds the
// deployment enables (agents.Kinds order) and, for claude-code, the configured
// Claude account NAMES the caller may pin. Never a token. agent_dispatch's
// no-target refusal lists its targets from it (internal/api kindChoices); the
// dispatch form that drew it as a picker was deleted
// (docs/design/chief-dispatch.md phase 2).
//
// 🔴 Accounts ARE THE CALLER'S OWN. A member's list is the accounts they
// entered in Account → Credentials; the deployment's environment pool is the
// admin's and never appears for a member.
//
// Unavailable maps a kind the caller has no credential for to the sentence
// saying what to add; the server refuses such a kind regardless
// (api.resolveKindAccount).
type KindChoices struct {
	Kinds       []string
	Accounts    []string
	Unavailable map[string]string
}

// CredentialsPath is the page where a user adds their own credentials.
const CredentialsPath = "/account/credentials"
