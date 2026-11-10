package officers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// --- HQ's officer PR context read ---------------------------------------------
//
// civ-hq internal/api/officer_pr_context.go (web-performance author
// attribution P3), read-only, for an officer whose type policy sets
// pr_context_read: true:
//
//	GET /officers/{id}/prs/{number}/context?item={item}
//	    {pr, pr_key, url, title, author, merged_at, head_sha, items, releases,
//	     releases_total, reviews, why, use}
//
// What HQ holds about ONE PR — its release row, the release captain's notes,
// its review rounds and its why entries — for ATTRIBUTION EVIDENCE only,
// never a review verdict. HQ answers only for a PR in a compare range one of
// the officer's live items names (here: the item the instance names), 404
// pr_not_visible otherwise, and refuses a type without the policy (403
// pr_context_read_off). Both are 4xx, so they reach the instance as final
// refusals (HQError) and are never retried. A 429 rate_limited is HQ's
// per-officer limit, shared with the obs and task reads. The route contract
// (its limits and its other codes) is HQ's, in HQ's officer docs; it is not
// copied here.
//
// WHO MAY READ: muster offers the tool by officer TYPE NAME and POOL
// (ToolOffered: the judgement pool of a web-performance officer, as
// officer_flood_block_propose); HQ enforces the type POLICY. Neither stands
// in for the other.
//
// 🔴 A 404 THAT IS NOT pr_not_visible IS NOT AN INVISIBLE PR. An HQ older
// than the route answers it with its router's catch-all 404 not_found; read
// as "not in range" it would make every PR look unattributable when muster
// is deployed before HQ. The client turns any such 404 into
// *PRContextRouteUnavailableError (pr_context_route_unavailable).
//
// 🔴 MUSTER BUILDS THE URL, SO IT DECODES STRICTLY: {item_id, pr} and
// nothing else, pr a positive JSON integer. A dropped or mistyped argument
// would otherwise be a narrowing the caller believes applied and HQ never saw.

// ToolPRContext is the PR context read tool.
const ToolPRContext = "officer_pr_context"

// PRContextNotVisibleCode is HQ's 404 code for a PR outside every range.
const PRContextNotVisibleCode = "pr_not_visible"

// PRContextRouteUnavailableCode names the refusal of a 404 that is NOT
// PRContextNotVisibleCode: this HQ has no PR context route.
const PRContextRouteUnavailableCode = "pr_context_route_unavailable"

// maxPRNumber bounds a PR number (HQ's route takes at most ten digits).
const maxPRNumber = 9999999999

// PRContextArgs is officer_pr_context's arguments: {item_id, pr}.
type PRContextArgs struct {
	Item ItemID
	PR   int64
}

// ParsePRContextArgs decodes officer_pr_context's arguments strictly.
func ParsePRContextArgs(raw []byte) (PRContextArgs, error) {
	obj, err := strictObject(raw, ToolPRContext, "{item_id, pr}", "item_id", "pr")
	if err != nil {
		return PRContextArgs{}, err
	}
	item, err := itemArg(obj)
	if err != nil {
		return PRContextArgs{}, err
	}
	d := json.NewDecoder(bytes.NewReader(obj["pr"]))
	d.UseNumber()
	var v any
	if len(obj["pr"]) == 0 || d.Decode(&v) != nil {
		return PRContextArgs{}, fmt.Errorf("pr is required: the PR number (an integer)")
	}
	n, ok := v.(json.Number)
	if !ok {
		return PRContextArgs{}, fmt.Errorf("pr must be the PR number as an integer, e.g. 7251")
	}
	pr, err := strconv.ParseInt(n.String(), 10, 64)
	if err != nil || pr < 1 || pr > maxPRNumber {
		return PRContextArgs{}, fmt.Errorf("pr must be a PR number from 1 to %d", int64(maxPRNumber))
	}
	return PRContextArgs{Item: item, PR: pr}, nil
}

// PRContextRouteUnavailableError: HQ answered the PR context route 404
// without PRContextNotVisibleCode (an HQ older than the route). Final:
// retrying will not help until HQ is upgraded; it says nothing about the PR.
type PRContextRouteUnavailableError struct{ HQ *HQError }

func (e *PRContextRouteUnavailableError) Error() string {
	return "officers: HQ has no officer PR context route (404 " + e.HQ.Code() + "), an HQ older than the read: not an invisible PR"
}

// PRContext is GET /officers/{id}/prs/{pr}/context?item={item} with the
// officer token, HQ's JSON passed through.
func (c *Client) PRContext(ctx context.Context, a PRContextArgs) ([]byte, error) {
	if a.PR < 1 || a.PR > maxPRNumber {
		return nil, fmt.Errorf("officers: %d is not a PR number", a.PR)
	}
	path := c.officerPath() + "/prs/" + strconv.FormatInt(a.PR, 10) + "/context?" + url.Values{"item": {string(a.Item)}}.Encode()
	body, err := c.do(ctx, http.MethodGet, path, "", nil)
	var he *HQError
	if errors.As(err, &he) && he.Status == http.StatusNotFound && he.Code() != PRContextNotVisibleCode {
		return nil, &PRContextRouteUnavailableError{HQ: he}
	}
	return body, err
}
