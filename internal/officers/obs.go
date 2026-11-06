package officers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
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
// the arguments are one JSON object, and the body's size. The one key muster
// removes is a judgement instance's item_id, which is muster's (the item its
// read budget is charged to, internal/api officer_reads.go), never HQ's.

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

// --- HQ's raw obs route (officer_obs_raw) -------------------------------------
//
// civ-hq internal/api/officer_obs_raw.go (docs/OFFICERS.md §6b "Raw queries"):
//
//	POST /officers/{id}/obs/raw   an investigating officer's own PromQL / LogQL
//	     {"item_id", "instance", "backend", "query", "start", "end",
//	      "step"?, "limit"?}
//
// HQ owns every check on the query (its caps, the stream selector, the
// refused audit stream, the redaction) and audits it with its text; muster
// passes the tool's arguments through UNCHANGED, as ObsQueryArgs does, and
// HQ's decoder is the one schema. What muster adds is what only muster knows:
// the instance (the caller's own name) and the item (the live claim the read
// is counted against, internal/api). The caller may name neither another way:
// a key spelling either (in any case — HQ's decoder matches names without
// case) is refused, never overwritten or forwarded.

// ObsRawBody is the POST /obs/raw body: the tool's arguments object args
// (item_id already split off by the caller), each value as sent (re-encoded
// compactly, never decoded into a struct), plus item_id (HQ's int64) and
// instance.
func ObsRawBody(args []byte, item ItemID, instance string) (json.RawMessage, error) {
	args = bytes.TrimSpace(args)
	var obj map[string]json.RawMessage
	if len(args) == 0 || args[0] != '{' || json.Unmarshal(args, &obj) != nil || obj == nil {
		return nil, fmt.Errorf("arguments must be one JSON object: {backend, query, start, end, step?, limit?, item_id?}")
	}
	for k := range obj {
		if strings.EqualFold(k, "instance") || strings.EqualFold(k, "item_id") {
			return nil, fmt.Errorf("%q is muster's to send, not yours: muster names your instance and the item you hold (pick one with item_id)", k)
		}
	}
	n, err := strconv.ParseInt(string(item), 10, 64)
	if err != nil || n <= 0 {
		return nil, fmt.Errorf("item %q is not one HQ's raw obs route can name (a positive integer id)", item)
	}
	obj["item_id"] = json.RawMessage(strconv.FormatInt(n, 10))
	inst, _ := json.Marshal(instance)
	obj["instance"] = inst
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("arguments: %v", err)
	}
	if len(out) > MaxObsQueryBody {
		return nil, fmt.Errorf("arguments are %d bytes, over the %d-byte cap on an officer request", len(out), MaxObsQueryBody)
	}
	return out, nil
}

// ObsRaw is POST /officers/{id}/obs/raw with the officer token: body
// (ObsRawBody) as given. The answer is HQ's, unchanged.
func (c *Client) ObsRaw(ctx context.Context, body json.RawMessage) ([]byte, error) {
	return c.doCoded(ctx, http.MethodPost, c.officerPath()+"/obs/raw", "application/json", body, obsDecisionCodes, obsRetryCodes)
}
