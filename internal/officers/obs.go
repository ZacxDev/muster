package officers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// --- HQ's officer obs route ---------------------------------------------------
//
// civ-hq internal/api/officer_obs.go (docs/OFFICERS.md §6b):
//
//	GET  /officers/{id}/obs/queries   the named-query registry and the caps
//	POST /officers/{id}/obs/query     run ONE registry entry:
//	     {"query_id", "start", "end", "step"?, "tag"?, "procedures"?,
//	      "min_calls"?, "name"?, "path"?, "limit"?}
//
// 🔴 THE CALLER NEVER SENDS PromQL OR LogQL. It names a registry entry and
// supplies that entry's typed arguments; HQ escapes each into a reviewed
// template.
//
// 🔴 HQ'S DECODER IS THE ONE PLACE THE REQUEST SCHEMA LIVES. muster passes
// the tool's arguments object to HQ UNCHANGED — never decoded into a struct,
// which would silently DROP an unknown key (a `promql` the caller believes
// applied). HQ refuses an unknown field or a wrong type (400 invalid), an
// unknown id (400 unknown_query), a missing or malformed window and an
// argument the entry does not take or needs (400 invalid), and the refusal
// reaches the officer with HQ's code. muster keeps no copy of the schema or
// the registry to drift from them. What muster owns, and checks itself: the
// officer (the dispatcher refuses naming another), its token (Client), that
// the arguments are one JSON object, and the body's size.

// MaxObsQueryBody bounds the arguments object muster sends: HQ's own cap on
// an officer route's body (civ-hq internal/api MaxOfficerBody, 256 KiB). A
// larger one is refused before it leaves muster.
const MaxObsQueryBody = 256 << 10

// ObsQueryArgs checks the obs query tool's arguments for what muster owns —
// one JSON object, at most MaxObsQueryBody bytes — and returns them unchanged
// for HQ.
func ObsQueryArgs(raw []byte) (json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	var obj map[string]json.RawMessage
	if len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, &obj) != nil || obj == nil {
		return nil, fmt.Errorf("arguments must be one JSON object: {query_id, start, end, …} — a named query from %s and the arguments it lists", ToolObsQueries)
	}
	if len(raw) > MaxObsQueryBody {
		return nil, fmt.Errorf("arguments are %d bytes, over the %d-byte cap on an officer request", len(raw), MaxObsQueryBody)
	}
	return json.RawMessage(raw), nil
}

// obsDecisionCodes are the obs route's 5xx answers that are HQ DECISIONS, not
// outages, and FINAL (HQ's guide: never retry): the route switched off (503
// obs_unconfigured) and a backend answer of the wrong shape (502 wrong_shape,
// HQ's own defect). Each is passed back as *HQError with HQ's body — as "HQ
// is unavailable, try again" an instance would retry a switched-off route
// forever.
var obsDecisionCodes = []string{"obs_unconfigured", "wrong_shape"}

// obsRetryCodes are the obs route's 5xx answers HQ's guide says to RETRY:
// 502 upstream (the backend read failed: unreachable, timeout, non-200).
// Passed back as *RetryableError, naming the code.
var obsRetryCodes = []string{"upstream"}

// ObsQueries is GET /officers/{id}/obs/queries with the officer token.
func (c *Client) ObsQueries(ctx context.Context) ([]byte, error) {
	return c.doCoded(ctx, http.MethodGet, c.officerPath()+"/obs/queries", "", nil, obsDecisionCodes, obsRetryCodes)
}

// ObsQuery is POST /officers/{id}/obs/query with the officer token: args,
// byte for byte (ObsQueryArgs) — never re-encoded.
func (c *Client) ObsQuery(ctx context.Context, args json.RawMessage) ([]byte, error) {
	return c.doCoded(ctx, http.MethodPost, c.officerPath()+"/obs/query", "application/json", args, obsDecisionCodes, obsRetryCodes)
}
