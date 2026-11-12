package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// `officer …` — the built-in officer store's admin API (/api/officers/…):
// define officer types (with their charters), officers and their items in
// muster's own database, as the caller (a personal token acts as its owner;
// the shared hook token as the operator, on an armed server only).
//
// 🔴 A CHARTER IS READ FROM A FILE OR STDIN (--charter-file, - for stdin),
// NEVER FROM ARGV: argv is world-readable through /proc and lands in shell
// history, and a charter is the operator's private instructions.

const (
	officerTypesRoute   = "/api/officers/types"
	officerTypeRoute    = "/api/officers/types/{key}"
	officerDefsRoute    = "/api/officers/defs"
	officerDefRoute     = "/api/officers/defs/{officer}"
	officerItemsRoute   = "/api/officers/defs/{officer}/items"
	officerResolveRoute = "/api/officers/defs/{officer}/items/{id}/resolve"
)

func newOfficerCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "officer",
		Short: "Define officers on muster's built-in backend: types (charters), officers, items",
		Long: `Administer the built-in officer backend's store (muster's own Postgres). An
officer runs on it when MUSTER_OFFICERS declares it with "backend": "builtin";
this defines what that officer IS — its type and charter, its switch, mode and
escalation recipients — and lets you queue and resolve its items.

Order: put the type first (officer type put), then the officer (officer def
put <type>:<area>); an officer's type is the part of its id before the colon.`,
	}
	cmd.AddCommand(newOfficerTypeCmd(a), newOfficerDefCmd(a), newOfficerItemCmd(a))
	return cmd
}

// officerCall sends req and emits the answer.
func (a *app) officerCall(cmd *cobra.Command, method, tmpl string, params map[string]string, query url.Values, body any) error {
	path, err := expandPath(tmpl, params)
	if err != nil {
		return err
	}
	raw, err := a.call(cmd.Context(), request{method: method, path: path, query: query, body: body, cred: credHook})
	if err != nil {
		return err
	}
	return a.emit(raw)
}

func newOfficerTypeCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{Use: "type", Short: "Officer types: a key, a name and a charter (versioned)"}
	ls := &cobra.Command{
		Use: "ls", Short: "GET " + officerTypesRoute + " — your types (no charters)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.officerCall(cmd, http.MethodGet, officerTypesRoute, nil, nil, nil)
		},
	}
	get := &cobra.Command{
		Use: "get <key>", Short: "GET " + officerTypeRoute + " — one type, charter included", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.officerCall(cmd, http.MethodGet, officerTypeRoute, map[string]string{"key": args[0]}, nil, nil)
		},
	}
	var name, charterFile, policy string
	put := &cobra.Command{
		Use:   "put <key>",
		Short: "PUT " + officerTypeRoute + " — create or replace a type (a changed charter bumps its version)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if charterFile == "" {
				return failf(exitUsage, "--charter-file is required (a path, or - for stdin); a charter is never taken from argv")
			}
			data, err := readBodyFile(charterFile, cmd.InOrStdin())
			if err != nil {
				return err
			}
			body := map[string]any{"name": name, "charter": string(data)}
			if strings.TrimSpace(policy) != "" {
				if !json.Valid([]byte(policy)) {
					return failf(exitUsage, "--policy is not JSON")
				}
				body["policy"] = json.RawMessage(policy)
			}
			return a.officerCall(cmd, http.MethodPut, officerTypeRoute, map[string]string{"key": args[0]}, nil, body)
		},
	}
	put.Flags().StringVar(&name, "name", "", "the type's display name")
	put.Flags().StringVar(&charterFile, "charter-file", "", "read the charter from this file, or - for stdin (required)")
	put.Flags().StringVar(&policy, "policy", "", "the type's policy, a JSON object (default {})")
	del := &cobra.Command{
		Use: "delete <key>", Short: "DELETE " + officerTypeRoute + " — refused while an officer names it", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.officerCall(cmd, http.MethodDelete, officerTypeRoute, map[string]string{"key": args[0]}, nil, nil)
		},
	}
	cmd.AddCommand(ls, get, put, del)
	return cmd
}

func newOfficerDefCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{Use: "def", Short: "Officers the built-in backend defines: switch, mode, escalation recipients"}
	ls := &cobra.Command{
		Use: "ls", Short: "GET " + officerDefsRoute + " — your officers (declared: MUSTER_OFFICERS runs it)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.officerCall(cmd, http.MethodGet, officerDefsRoute, nil, nil, nil)
		},
	}
	var enabled bool
	var mode, escalate string
	put := &cobra.Command{
		Use:   "put <type>:<area>",
		Short: "PUT " + officerDefRoute + " — create or replace an officer (its type must exist)",
		Long: `Create or replace an officer. PUT replaces: a flag left out takes its
default (--enabled=true, --mode live, no escalation recipients besides you).
--enabled=false switches it off: muster stops its instances and starts none.
--mode shadow records escalations without notifying anyone.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]any{"enabled": enabled, "mode": mode}
			var ids []int64
			for _, f := range strings.Split(escalate, ",") {
				if f = strings.TrimSpace(f); f == "" {
					continue
				}
				n, err := strconv.ParseInt(f, 10, 64)
				if err != nil || n <= 0 {
					return failf(exitUsage, "--escalate-to %q is not a list of user ids", escalate)
				}
				ids = append(ids, n)
			}
			body["escalate_to"] = ids
			return a.officerCall(cmd, http.MethodPut, officerDefRoute, map[string]string{"officer": args[0]}, nil, body)
		},
	}
	put.Flags().BoolVar(&enabled, "enabled", true, "the officer's switch")
	put.Flags().StringVar(&mode, "mode", "live", "live, or shadow (escalations recorded, never notified)")
	put.Flags().StringVar(&escalate, "escalate-to", "", "comma-separated user ids its escalations also reach")
	del := &cobra.Command{
		Use: "delete <type>:<area>", Short: "DELETE " + officerDefRoute + " — refused while it has items or schedules", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.officerCall(cmd, http.MethodDelete, officerDefRoute, map[string]string{"officer": args[0]}, nil, nil)
		},
	}
	cmd.AddCommand(ls, put, del)
	return cmd
}

func newOfficerItemCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{Use: "item", Short: "An officer's queue: list, enqueue, resolve"}
	var status string
	var limit int
	ls := &cobra.Command{
		Use: "ls <officer>", Short: "GET " + officerItemsRoute + " — newest first", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := url.Values{}
			if status != "" {
				q.Set("status", status)
			}
			if limit > 0 {
				q.Set("limit", strconv.Itoa(limit))
			}
			return a.officerCall(cmd, http.MethodGet, officerItemsRoute, map[string]string{"officer": args[0]}, q, nil)
		},
	}
	ls.Flags().StringVar(&status, "status", "", "open, claimed, waiting, done or escalated")
	ls.Flags().IntVar(&limit, "limit", 0, "at most this many (default 100, at most 500)")
	var kind, subject, payload, tier, priority, notBefore string
	enq := &cobra.Command{
		Use:   "enqueue <officer>",
		Short: "POST " + officerItemsRoute + " — queue an item (a live item of the same kind and subject is returned instead)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if kind == "" || subject == "" {
				return failf(exitUsage, "--kind and --subject are required")
			}
			body := map[string]any{"kind": kind, "subject_key": subject, "tier": tier, "priority": priority}
			if payload != "" {
				if !json.Valid([]byte(payload)) {
					return failf(exitUsage, "--payload is not JSON")
				}
				body["payload"] = json.RawMessage(payload)
			}
			if notBefore != "" {
				t, err := time.Parse(time.RFC3339, notBefore)
				if err != nil {
					return failf(exitUsage, "--not-before %q is not RFC 3339", notBefore)
				}
				body["not_before"] = t
			}
			return a.officerCall(cmd, http.MethodPost, officerItemsRoute, map[string]string{"officer": args[0]}, nil, body)
		},
	}
	enq.Flags().StringVar(&kind, "kind", "", "the item's kind (required)")
	enq.Flags().StringVar(&subject, "subject", "", "the item's subject key (required; one live item per kind and subject)")
	enq.Flags().StringVar(&payload, "payload", "", "a JSON object the instance reads")
	enq.Flags().StringVar(&tier, "tier", "", "routine or judgement (default judgement)")
	enq.Flags().StringVar(&priority, "priority", "", "urgent, high, normal or low (default normal)")
	enq.Flags().StringVar(&notBefore, "not-before", "", "not claimable before this RFC 3339 time")
	var outcome, summary string
	res := &cobra.Command{
		Use:   "resolve <officer> <item-id>",
		Short: "POST " + officerResolveRoute + " — finish an item by hand (refused while an instance holds it)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.officerCall(cmd, http.MethodPost, officerResolveRoute, map[string]string{"officer": args[0], "id": args[1]}, nil,
				map[string]any{"outcome": outcome, "summary": summary})
		},
	}
	res.Flags().StringVar(&outcome, "outcome", "no_action", "done, no_action or escalated")
	res.Flags().StringVar(&summary, "summary", "", "why")
	cmd.AddCommand(ls, enq, res)
	return cmd
}
