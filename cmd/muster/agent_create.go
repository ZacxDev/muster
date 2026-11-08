package main

import (
	"net/http"
	"strings"

	"github.com/spf13/cobra"
)

// envAgentCreateToken is the ONE credential `agent create` presents: a personal
// API token of scope agent-create (minted at /account/tokens, "Create agents").
//
// 🔴 IT IS NEVER envToken. The server refuses every other scope on POST
// /api/agents — a FULL token included (internal/api explicitScopeRoutes) — so
// that the ordinary token in this CLI's env file cannot create agents. Reading
// this variable instead means that token is never even sent to the route: with
// it unset the verb refuses here and sends nothing.
const envAgentCreateToken = "MUSTER_AGENT_CREATE_TOKEN"

// agentCreateRoute is the machine route that creates an agent
// (internal/api handleAPIAgentCreate).
const agentCreateRoute = "/api/agents"

// agentCreateBody is POST /api/agents' body: the fields of the web UI's
// dispatch form, which the route replaced (docs/design/chief-dispatch.md,
// phase 2). Start is a pointer so it is always sent: the server refuses a
// request that does not say whether to start the agent.
type agentCreateBody struct {
	Kind       string `json:"kind,omitempty"`
	Repo       string `json:"repo,omitempty"`
	RepoBranch string `json:"repo_branch,omitempty"`
	Model      string `json:"model,omitempty"`
	Prompt     string `json:"prompt,omitempty"`
	NoteID     *int64 `json:"note_id,omitempty"`
	CCAccount  string `json:"cc_account,omitempty"`
	CCFailover bool   `json:"cc_failover,omitempty"`
	WorkerPool string `json:"worker_pool,omitempty"`
	Start      *bool  `json:"start"`
}

// agentCreateNoTokenMsg is the local refusal when envAgentCreateToken is unset.
// It says what the server would: only an agent-create token reaches the route.
const agentCreateNoTokenMsg = "creating agents needs a token of scope agent-create: mint one at /account/tokens " +
	"(Create agents) and export it as " + envAgentCreateToken + ". This verb never sends " + envToken +
	" (the server refuses every other token on this route, a full one included), so nothing was sent."

// agentCreateNoTokenTenantsMsg is appended on a host with more than one tenant,
// where an exported token alone is not accepted (resolvePaired).
const agentCreateNoTokenTenantsMsg = " On a host with more than one tenant, put " + envAgentCreateToken +
	" in that tenant's file, or export it beside an " + envAPIURL + " + " + envToken + " pair."

func newAgentCreateCmd(a *app) *cobra.Command {
	var (
		b      agentCreateBody
		noteID int64
		start  bool
	)
	cmd := &cobra.Command{
		Use:   "create --start=<true|false>",
		Short: "POST /api/agents — create an agent as yourself (needs " + envAgentCreateToken + ")",
		Long: `Create an agent, acting as yourself, without the chief.

--start=true dispatches it now: its first turn is the prompt (or the task's
body with --note-id). --start=false creates it STOPPED and provisions nothing;
Start it later from its page in the web UI.

🔴 THE TOKEN IS ` + envAgentCreateToken + `, NOT ` + envToken + `. It must be a personal token of
scope agent-create, minted at /account/tokens (Create agents). The server
refuses every other token on this route, a full-access one included, so the
CLI's ordinary token cannot create agents; this verb never sends it.

The server applies the same checks the web UI's dispatch form did: the kind and
Claude account, the worker pool, that the repository is one of your connected
ones, the model credential, a task's gate: tag (for --start=true), and whether
this deployment can deliver a first turn at all (for --start=true; a refused
dispatch says to retry with --start=false, then Start).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.api()
			if err != nil {
				return err
			}
			if c.cfg.createTokRefusal != "" {
				return failf(exitUsage, "%s", c.cfg.createTokRefusal)
			}
			if strings.TrimSpace(c.cfg.createTok) == "" {
				msg := agentCreateNoTokenMsg
				if c.tenantNote != "" {
					msg += agentCreateNoTokenTenantsMsg
				}
				return failf(exitUsage, "%s", msg)
			}
			b.Start = &start
			if cmd.Flags().Changed("note-id") {
				if noteID <= 0 {
					return failf(exitUsage, "--note-id must be a positive task id, got %d.", noteID)
				}
				b.NoteID = &noteID
			}
			body, err := a.call(cmd.Context(), request{
				method: http.MethodPost, path: agentCreateRoute, body: b, cred: credAgentCreate,
			})
			if err != nil {
				return err
			}
			return a.emit(body)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&start, "start", false, "true dispatches the agent now; false creates it stopped (required)")
	f.StringVar(&b.Kind, "kind", "", "agent kind (claude-code or gateway; default: the server's)")
	f.StringVar(&b.Repo, "repo", "", "one of your connected repositories, owner/name")
	f.StringVar(&b.RepoBranch, "repo-branch", "", "branch (default: the repository's default branch)")
	f.StringVar(&b.Model, "model", "", "model (gateway kind only)")
	f.StringVar(&b.Prompt, "prompt", "", "the agent's first turn (ignored with --note-id: the task's body is used)")
	f.Int64Var(&noteID, "note-id", 0, "a task id: the agent works that task")
	f.StringVar(&b.CCAccount, "cc-account", "", "pin a Claude account (claude-code kind only)")
	f.BoolVar(&b.CCFailover, "cc-failover", false, "let a pinned Claude account fail over")
	f.StringVar(&b.WorkerPool, "worker-pool", "", "join a machine-job worker pool (claude-code kind only)")
	_ = cmd.MarkFlagRequired("start")
	return cmd
}
