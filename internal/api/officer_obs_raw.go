package api

import (
	"context"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// officer_obs_raw: an investigating judgement instance's own PromQL / LogQL,
// passed through to HQ's POST /officers/{id}/obs/raw (docs/officers.md
// "Raw metrics and logs: officer_obs_raw"; officers/obs.go ObsRawBody).
//
// The order of one call, each step refusing before the next:
//
//  1. the binding (dispatchOfficerTool): an instance of a JUDGEMENT pool
//     (officers.ToolOffered); a routine instance is refused there;
//  2. the item: the instance must hold one live claim in HQ (or name which of
//     its claims, item_id) — the same gate as officer_kube_read (heldItem);
//  3. the body: the arguments object passed through, plus the item and the
//     instance muster names (a key spelling either is refused);
//  4. the budget: one read of that item (spendRead, the count shared with
//     officer_kube_read and officer_obs_query), RESERVED before the call;
//  5. HQ: the officer's own token (the one the instance already spends for
//     every officer tool; no other credential), and HQ's answer or refusal
//     returned AS HQ SENT IT — HQ checks the query, redacts the lines and
//     writes the audit row (with the query text), so muster neither re-checks
//     nor rewrites any of it. settleRead then gives the reserved read back
//     when HQ's answer does not count (officers.ReadCounted: a 429, a 5xx,
//     or no answer).
//
// A call refused at step 1 reaches nothing; one refused at steps 2-3 reaches
// HQ only for the claim lookup. Neither spends a read.
// ---------------------------------------------------------------------------

// obsRawToolDef is officer_obs_raw's definition (offered by
// officers.ToolOffered: judgement instances only).
func obsRawToolDef() agents.ToolDef {
	return agents.ToolDef{Type: "function", Name: officers.ToolObsRaw,
		Description: "Run your OWN PromQL or LogQL (POST /officers/{id}/obs/raw) for the item you hold, when HQ's named queries (" +
			officers.ToolObsQuery + ") cannot answer the question. HQ answers it only when your officer's type investigates (otherwise 403 " +
			"not_investigating) and checks every argument itself — the window, the step, the series and line limits, its rate limit, and a Loki " +
			"stream selector (required; the apiserver audit stream is refused) — and its refusal names the limit to fix. It redacts every log " +
			"line and audits the query text. Prometheus is not pinned to a cluster: filter by cluster yourself. `empty: true` is NOT a zero nor " +
			"an absence of logs: run the positive control its `hint` names before you report none. A 429 means wait a minute (HQ's limit is " +
			"shared with " + officers.ToolObsQuery + "). muster names your instance and the item and returns HQ's answer as HQ sent it. Read budget: " +
			officers.ReadBudgetNote + ". Read-only.",
		// 🔴 AN OPEN OBJECT, as officer_obs_query's: HQ's decoder is the one
		// schema (officers.ObsRawBody passes the arguments through), so the
		// properties are guidance and carry none of HQ's limits — a copy
		// here would drift from HQ's and refuse what HQ accepts.
		Parameters: map[string]any{"type": "object", "additionalProperties": true, "required": []string{"backend", "query", "start", "end"},
			"properties": map[string]any{
				"backend": strProp("\"prometheus\" or \"loki\"."),
				"query":   strProp("The PromQL or LogQL text."),
				"start":   strProp("RFC 3339 in whole seconds, e.g. 2006-01-02T15:04:05Z; the window is (start, end]."),
				"end":     strProp("RFC 3339 in whole seconds."),
				"step":    strProp("Prometheus only, and required there: a duration, e.g. \"300s\"."),
				"limit":   map[string]any{"type": "integer", "description": "A Loki log query's line limit."},
				"item_id": strProp("Optional: the claimed item this read is for, when you hold more than one."),
			}}}
}

// officerObsRaw runs officer_obs_raw for instance a of pool o.
func (s *Server) officerObsRaw(ctx context.Context, a agents.Agent, o officers.Officer, c *officers.Client, arguments string) string {
	rest, named, _, err := splitItemID([]byte(orEmptyJSON(arguments)))
	if err != nil {
		return toolErr("%v", err)
	}
	item, err := s.heldItem(ctx, c, a.Name, named, true)
	if err != nil {
		return heldItemRefusal(officers.ToolObsRaw, err)
	}
	body, err := officers.ObsRawBody(rest, item, a.Name)
	if err != nil {
		return toolErr("%v", err)
	}
	if err := s.spendRead(o, item); err != nil {
		return toolErr("%v", err)
	}
	ans, err := c.ObsRaw(ctx, body)
	s.settleRead(o, item, err)
	return officerToolResult(officers.ToolObsRaw, ans, err)
}
