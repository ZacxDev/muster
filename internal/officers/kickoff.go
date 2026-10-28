package officers

import (
	"fmt"
	"strings"
)

// KickoffText is the first message instance n of officer o is given when the
// wake loop starts it.
//
// 🔴 IT CARRIES NO CHARTER AND NO CREDENTIAL. The charter lives in HQ and
// reaches the instance through officer_brief, so muster never holds a second
// copy that can drift from HQ's versioned one; and the officer token is spent
// by muster on the instance's behalf, so it has nothing to put here. Tests
// assert the token is absent from this text.
func KickoffText(o Officer, n int) string {
	tools := strings.Join([]string{ToolBrief, ToolItems, ToolClaim, ToolHeartbeat, ToolComplete, ToolRelease}, ", ")
	return fmt.Sprintf(`You are officer %[1]s, instance %[2]d of %[3]d (%[4]s). You are an officer: a standing agent that works a queue of items HQ produces for this officer. You have no muster task and no repository checkout; that is expected.

Your officer tools (MCP server "muster"): %[5]s. They act for officer %[1]s only; muster attaches the officer's HQ credential on your behalf, so you never hold or need a token — do not ask for one.

Start now:
1. Call %[6]s. It returns your charter (follow it: it is your standing instructions, versioned in HQ), the open items and the recent outcomes. Load it instead of looking for earlier conversation.
2. Work the queue one item at a time: %[7]s to see open items, %[8]s{item_id} to take one (a 409 means another instance has it — move on), %[9]s{item_id} at least every few minutes while you work it, and %[10]s{item_id, outcome: done|escalated|no_action, summary} when finished. If you cannot finish it, %[11]s{item_id} so another instance can.
3. Anything that needs a human decision: attention_ask, then attention_wait.
4. When %[7]s shows nothing open and you hold no claim, stop and wait. muster wakes you again when HQ has new work, and scales you to zero after a quiet spell.

You watch, triage, record and dispatch. You never merge, deploy or release, and you hold no production credentials.`,
		o.ID, n, o.MaxInstances, InstanceName(o.ID, n), tools,
		ToolBrief, ToolItems, ToolClaim, ToolHeartbeat, ToolComplete, ToolRelease) + releaseNote(o)
}

// releaseNote is the release officer's extra step: where its evaluation goes.
func releaseNote(o Officer) string {
	if !ToolOffered(o, ToolReleaseEvaluation) {
		return ""
	}
	return "\n\nAs a release officer, post each release evaluation with " + ToolReleaseEvaluation +
		"{evaluation} (the fields your charter names), then " + ToolComplete + " the release.evaluate item."
}

// The MCP tool names muster serves to officer instances.
const (
	ToolBrief     = "officer_brief"
	ToolItems     = "officer_items"
	ToolClaim     = "officer_claim"
	ToolHeartbeat = "officer_heartbeat"
	ToolComplete  = "officer_complete"
	ToolRelease   = "officer_release"
	// ToolReleaseEvaluation posts a release evaluation to HQ's captain route
	// (POST /releases/captain). Offered to `release:*` officers only
	// (ToolOffered).
	ToolReleaseEvaluation = "officer_release_evaluation"
)

// ReleaseOfficerType is the officer type whose instances evaluate releases.
const ReleaseOfficerType = "release"

// Type is the officer's type: the part of its id before the colon.
func (o Officer) Type() string {
	t, _, _ := strings.Cut(o.ID, ":")
	return t
}

// ToolOffered is THE rule for which officer tools officer o's instances get:
// the six queue tools for every officer, and the captain route's tool for a
// release officer only. internal/api's listing and dispatch both ask it.
func ToolOffered(o Officer, tool string) bool {
	switch tool {
	case ToolBrief, ToolItems, ToolClaim, ToolHeartbeat, ToolComplete, ToolRelease:
		return true
	case ToolReleaseEvaluation:
		return o.Type() == ReleaseOfficerType
	}
	return false
}
