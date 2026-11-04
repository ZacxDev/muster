package officers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
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
// template. So muster's tool has no field a query could be written in, and
// refuses any key HQ's body does not have BEFORE the request leaves muster:
// a `query`, `promql` or `expr` an agent sends anyway is refused, never
// dropped (a filter the caller believes applied and does not is the failure
// mode HQ's own decoder refuses for).
//
// Which arguments an entry takes is HQ's: GET /obs/queries lists each
// entry's `args`, and HQ refuses an argument the entry does not take (400
// invalid) and an unknown id (400 unknown_query). muster does not keep a copy
// of the registry to drift from it.

// ObsQuery is the POST /obs/query body, exactly HQ's OfficerObsQueryRequest.
type ObsQuery struct {
	QueryID string `json:"query_id"`
	// Start and End are RFC 3339, whole seconds; the window is (start, end].
	Start      string   `json:"start"`
	End        string   `json:"end"`
	Step       string   `json:"step,omitempty"`
	Tag        string   `json:"tag,omitempty"`
	Procedures []string `json:"procedures,omitempty"`
	MinCalls   *int     `json:"min_calls,omitempty"`
	Name       string   `json:"name,omitempty"`
	Path       string   `json:"path,omitempty"`
	Limit      int      `json:"limit,omitempty"`
}

// obsQueryIDRE is the shape of a registry id (HQ's are snake_case). It is a
// local sanity bound, not the registry: HQ answers unknown_query for an id
// it does not have.
var obsQueryIDRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// ParseObsQuery decodes and checks the obs query tool's arguments. Unknown
// keys are refused (a raw query under any name among them); query_id must be
// a registry-shaped id; start and end are required. Everything else —
// whether the entry takes an argument, the window's bounds, the step — is
// HQ's to judge.
func ParseObsQuery(raw []byte) (ObsQuery, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var q ObsQuery
	if err := dec.Decode(&q); err != nil {
		return ObsQuery{}, fmt.Errorf("arguments are {query_id, start, end, step?, tag?, procedures?, min_calls?, name?, path?, limit?} "+
			"— a named query from %s and its arguments, never a raw query: %v", ToolObsQueries, err)
	}
	if !obsQueryIDRE.MatchString(q.QueryID) {
		return ObsQuery{}, fmt.Errorf("query_id is required: one of the ids %s lists", ToolObsQueries)
	}
	if strings.TrimSpace(q.Start) == "" || strings.TrimSpace(q.End) == "" {
		return ObsQuery{}, errors.New("start and end are required (RFC 3339, whole seconds, e.g. 2006-01-02T15:04:05Z)")
	}
	return q, nil
}

// obsDecisionCodes are the obs route's 5xx answers that are HQ DECISIONS, not
// outages: the route switched off (503 obs_unconfigured), a backend answer of
// the wrong shape (502 wrong_shape) and a backend that failed (502 upstream).
// Each is passed back as *HQError with HQ's body, so the instance reads why —
// as "HQ is unavailable, try again" an instance would retry a switched-off
// route forever.
var obsDecisionCodes = []string{"obs_unconfigured", "wrong_shape", "upstream"}

// ObsQueries is GET /officers/{id}/obs/queries with the officer token.
func (c *Client) ObsQueries(ctx context.Context) ([]byte, error) {
	return c.do(ctx, http.MethodGet, c.officerPath()+"/obs/queries", nil, obsDecisionCodes)
}

// ObsQuery is POST /officers/{id}/obs/query with the officer token: q as is.
func (c *Client) ObsQuery(ctx context.Context, q ObsQuery) ([]byte, error) {
	return c.do(ctx, http.MethodPost, c.officerPath()+"/obs/query", q, obsDecisionCodes)
}
