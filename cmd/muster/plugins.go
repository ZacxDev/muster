package main

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// `plugins retry <message-id>` — the /plugins page's Retry button, for a session
// without a browser: hand a plugin message the chief never confirmed (its turn
// was cut off, so it settled `unconfirmed`) to the CURRENT chief again. It posts
// POST /api/plugins/messages/{id}/retry with the hook credential — a personal
// token acts as its owner; the shared hook token as the operator, on an armed
// server only.

const pluginRetryRoute = "/api/plugins/messages/{id}/retry"

func newPluginsCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugins",
		Short: "Act on plugin messages",
	}
	cmd.AddCommand(newPluginsRetryCmd(a), newPluginsDeliverToCmd(a))
	return cmd
}

const pluginDeliverToRoute = "/api/plugins/{id}/deliver-to"

// `plugins deliver-to <plugin-id> officer:<id>` (or --clear): bind a plugin
// to a builtin officer (docs/plugins.md "A plugin bound to an officer").
func newPluginsDeliverToCmd(a *app) *cobra.Command {
	var clear bool
	cmd := &cobra.Command{
		Use:   "deliver-to <plugin-id> [officer:<id>]",
		Short: "PUT " + pluginDeliverToRoute + " — bind a plugin to a builtin officer, or --clear it",
		Long: `Bind a plugin to a builtin officer: its messages become items in that
officer's queue instead of chief turns, and the officer's outbound drafts are
sent through it. Refused (exit 5) unless the officer is defined in the built-in
store for the plugin's owner. --clear gives the plugin back to your chief.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(strings.TrimSpace(args[0]), 10, 64)
			if err != nil || id <= 0 {
				return failf(exitUsage, "plugin id %q is not a positive number (it is on /plugins)", args[0])
			}
			to := ""
			switch {
			case clear && len(args) == 2:
				return failf(exitUsage, "give an officer or --clear, not both")
			case !clear && len(args) != 2:
				return failf(exitUsage, "give officer:<id>, or --clear")
			case !clear:
				to = strings.TrimSpace(args[1])
			}
			path, err := expandPath(pluginDeliverToRoute, map[string]string{"id": strconv.FormatInt(id, 10)})
			if err != nil {
				return err
			}
			raw, err := a.call(cmd.Context(), request{method: http.MethodPut, path: path, cred: credHook,
				body: map[string]any{"deliver_to": to}})
			if err != nil {
				return err
			}
			return a.emit(raw)
		},
	}
	cmd.Flags().BoolVar(&clear, "clear", false, "unbind the plugin: its messages go to your chief again")
	return cmd
}

func newPluginsRetryCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "retry <message-id>",
		Short: "POST " + pluginRetryRoute + " — hand an unconfirmed plugin message to the chief again",
		Long: `Retry a plugin message whose delivery turn was cut off (status unconfirmed):
it goes back in the queue and is delivered to your CURRENT chief the way every
message is. Refused (exit 5, nothing changed) when the message is queued,
being delivered or delivered, when the chief already drafted a reply to it that
is pending, approved or sent, when it was routed to a session, or when you have
no chief. The answer is 202 {id, status: queued, retrying: true}; the turn runs
after it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(strings.TrimSpace(args[0]), 10, 64)
			if err != nil || id <= 0 {
				return failf(exitUsage, "message id %q is not a positive number (it is on /plugins)", args[0])
			}
			path, err := expandPath(pluginRetryRoute, map[string]string{"id": strconv.FormatInt(id, 10)})
			if err != nil {
				return err
			}
			raw, err := a.call(cmd.Context(), request{method: http.MethodPost, path: path, cred: credHook})
			if err != nil {
				return err
			}
			return a.emit(raw)
		},
	}
}
