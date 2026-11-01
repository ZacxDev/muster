package main

import (
	"net/http"

	"github.com/spf13/cobra"
)

// The attention verb: `attention summary` over GET /api/attention/summary —
// what is waiting on you, your mirror hosts' busy/idle counts and your chief
// nudge mode, as one JSON document (docs/attention.md#the-summary-api).
//
// 🔴 IT WORKS ONLY WITH A PERSONAL TOKEN IN envToken, like `flow …`: the route
// (api.requireAPIToken) refuses the shared hook secret, because the summary is
// one person's queue and the hook secret names nobody. A read-only token is
// enough. A poller with its own token keeps it in its own file and passes
// --env-file, so that token never has to live in the default env file.

const attentionSummaryRoute = "/api/attention/summary"

func newAttentionCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "attention",
		Short: "Read what is waiting on you (needs a personal API token; read-only is enough)",
	}
	cmd.AddCommand(newAttentionSummaryCmd(a))
	return cmd
}

func newAttentionSummaryCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "summary",
		Short: "GET /api/attention/summary — what waits on you, your hosts, your nudge mode",
		Long: `Print what is waiting on you — the same items as the Asks page — with each
item's title, why (the text its notification shows), age and link; each of your
mirror hosts with whether it is online and its busy and idle Claude windows;
and your chief nudge mode (off, shadow or live). The output is the server's
JSON document. "partial": true means part of the queue could not be read, so
the list may be short.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			body, err := a.call(cmd.Context(), request{method: http.MethodGet, path: attentionSummaryRoute, cred: credHook})
			if err != nil {
				return err
			}
			return a.emit(body)
		},
	}
}
