package cctranscript

import "time"

// HOURLY CLAUDE USAGE, as an officer instance pod's ccd answers it on
// GET /v1/usage (cmd/ccd/usage_route.go) and muster-server pushes it to the
// officer's HQ (internal/officers/usage, officers.Client.PutUsage).
//
// ONE SHAPE, TWO WIRES: a bucket is what ccd counted from the instance's own
// transcripts and what HQ stores, field for field. HQ upserts a bucket by
// (officer, instance, session, hour) and REPLACES its counts, so every bucket
// sent must be the whole of its hour — which is why ccd truncates `since` down
// to the hour (UsagePage.Since) and never answers a partial oldest hour.

// UsagePage is ccd's answer to GET /v1/usage.
//
//   - Since is the window's start as ccd applied it: the caller's `since`
//     truncated DOWN to the UTC hour, and clamped to at most 48 hours ago.
//   - Buckets are the window's hours, sorted by Hour then Session. A bucket
//     with no API call and no turn is left out; an empty window is [], never
//     null.
type UsagePage struct {
	Since   time.Time     `json:"since"`
	Buckets []UsageBucket `json:"buckets"`
}

// UsageBucket is one session's Claude usage in one UTC hour.
//
//   - Session is the CLI session the transcript belongs to: a subagent's
//     transcript (nested under its parent session's directory) counts on the
//     parent session.
//   - Hour is the UTC hour, on the hour.
//   - Turns: distinct prompts (promptId) of the main conversation, at the
//     hour each was first seen. A subagent's prompts are not turns.
//   - APICalls: API calls, each counted ONCE however many block records the
//     CLI wrote for it, at the hour of its first record — subagent calls
//     included (they are this instance's spend).
//   - The tokens are those calls' usage, each call's largest value of each
//     field.
type UsageBucket struct {
	Session             string    `json:"session"`
	Hour                time.Time `json:"hour"`
	Turns               int64     `json:"turns"`
	APICalls            int64     `json:"api_calls"`
	InputTokens         int64     `json:"input_tokens"`
	OutputTokens        int64     `json:"output_tokens"`
	CacheReadTokens     int64     `json:"cache_read_tokens"`
	CacheCreationTokens int64     `json:"cache_creation_tokens"`
}
