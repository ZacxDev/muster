package officership

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// THE SHEPHERD'S LOOP THROUGH HQ (docs/officers.md "The review loop through
// HQ"): the audit verdict mark-ready reads off the held item, the shadow every
// write honours, and the merge ask a shepherd files.
// ---------------------------------------------------------------------------

// GateShadow is the refusal of a write the held item (or the officer) runs in
// shadow; GateMergeAsk is the merge-ask tool's own checks.
const (
	GateShadow   = "shadow"
	GateMergeAsk = "merge_ask"
)

// LoopPayload is the part of an HQ loop item's payload muster reads.
type LoopPayload struct {
	// PRLoop is `payload.pr_loop`: the pull request the loop is about, and the
	// audit round the item premises (absent on an HQ item that is no loop's).
	PRLoop *PRLoop `json:"pr_loop"`
	// From is `payload.from` on a findings item: the item it came from and
	// that item's report (Summary), which HQ writes verbatim — the shepherd
	// can neither write nor edit it.
	From *LoopFrom `json:"from"`
}

// PRLoop is `payload.pr_loop`.
type PRLoop struct {
	Repo    string `json:"repo"`
	PR      int    `json:"pr"`
	HeadSHA string `json:"head_sha"`
	Round   *int   `json:"round"`
	HeadRef string `json:"head_ref"`
	Base    string `json:"base"`
	Author  string `json:"author"`
}

// LoopFrom is `payload.from`.
type LoopFrom struct {
	OfficerID string          `json:"officer_id"`
	ItemID    officers.ItemID `json:"item_id"`
	Kind      string          `json:"kind"`
	Summary   string          `json:"summary"`
}

// ParseLoopPayload reads raw (an item's payload). An absent payload is the
// zero LoopPayload; one that is not a JSON object is an error.
func ParseLoopPayload(raw json.RawMessage) (LoopPayload, error) {
	var p LoopPayload
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return p, nil
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return LoopPayload{}, fmt.Errorf("the item's payload is not the loop's JSON: %v", err)
	}
	return p, nil
}

// VerdictRE is the auditor report's first line, exactly as HQ reads it:
// verdict, round, and the first 12 hex of the head audited.
var VerdictRE = regexp.MustCompile(`^VERDICT: (proceed|questioned|delete|close|red|no-red|superseded|not-run|exit [0-9]{1,3}) · round ([0-9]{1,4}) · head ([0-9a-f]{12})$`)

// redFindings counts the report's 🔴 finding lines (a line, trimmed, that
// starts with 🔴).
func redFindings(summary string) int {
	n := 0
	for _, l := range strings.Split(summary, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "🔴") {
			n++
		}
	}
	return n
}

// AuditVerdictClear is mark-ready's audit gate, over the item the instance
// holds for pull request pr (the one Admit's item gate found): its
// payload.from.summary — the auditor's report, written by HQ — must open
// with a VerdictRE line whose verdict is `no-red`, whose round is
// payload.pr_loop.round (at least 1) and whose head is the live head's first
// 12 hex; payload.pr_loop.head_sha must be the live head; and the report
// must carry no 🔴 line. It is the predicate HQ's merge-ask gate
// (audit_not_stopped) applies over its own records; HQ checks again at the
// merge ask, so this is the early refusal.
//
// 🔴 THE REPORT MUST BE ANOTHER OFFICER'S: payload.from names the item the
// report came from, and its officer must be of another TYPE than self (the
// shepherd's officer id), as must its item kind's type (the kind's prefix
// before its first '.', HQ's `<type>.<what>`). An officer's id is
// `<type>:<area>`, so a report from the shepherd itself — or from any
// officer of its type — never clears its own pull request.
func AuditVerdictClear(item officers.Item, pr PR, self string) error {
	p, err := ParseLoopPayload(item.Payload)
	if err != nil {
		return refuse(GateAudit, "item %s: %v", item.ID, err)
	}
	// Read nil-safe, so each check below stands on its own (none relies on
	// an earlier one to keep a nil away).
	summary := ""
	if p.From != nil {
		summary = p.From.Summary
	}
	var loop PRLoop
	if p.PRLoop != nil {
		loop = *p.PRLoop
	}
	if strings.TrimSpace(summary) == "" {
		return refuse(GateAudit, "item %s carries no auditor's report (payload.from.summary): mark ready follows an audit round HQ recorded, not the shepherd's own word", item.ID)
	}
	var fromID, fromKind string
	if p.From != nil {
		fromID, fromKind = p.From.OfficerID, p.From.Kind
	}
	selfType, _, _ := strings.Cut(self, ":")
	fromType, _, _ := strings.Cut(fromID, ":")
	kindType, _, _ := strings.Cut(fromKind, ".")
	switch {
	case strings.TrimSpace(fromID) == "" || strings.TrimSpace(fromKind) == "":
		return refuse(GateAudit, "item %s's report names no source officer or kind (payload.from.officer_id, .kind): it cannot be read as an auditor's", item.ID)
	case fromID == self || strings.EqualFold(fromType, selfType):
		return refuse(GateAudit, "item %s's report is from officer %s, of this shepherd's own type %s: a shepherd's own word never clears its pull request", item.ID, fromID, selfType)
	case strings.EqualFold(kindType, selfType):
		return refuse(GateAudit, "item %s's report came from an item of kind %s, one of this shepherd's own type %s: it is not an auditor's", item.ID, fromKind, selfType)
	}
	round, haveRound := 0, false
	if loop.Round != nil {
		round, haveRound = *loop.Round, true
	}
	if !haveRound {
		return refuse(GateAudit, "item %s: no round on record (payload.pr_loop.round)", item.ID)
	}
	if round < 1 {
		return refuse(GateAudit, "item %s: premise round %d: an audit round is 1 or more", item.ID, round)
	}
	if loop.PR != 0 && loop.PR != pr.Number {
		return refuse(GateAudit, "item %s's loop is about #%d, not #%d", item.ID, loop.PR, pr.Number)
	}
	if loop.HeadSHA != pr.HeadSHA {
		return refuse(GateAudit, "item %s audited another head: its loop is at %s, #%d is at %s", item.ID, short(loop.HeadSHA), pr.Number, short(pr.HeadSHA))
	}
	line, _, _ := strings.Cut(summary, "\n")
	m := VerdictRE.FindStringSubmatch(strings.TrimRight(line, "\r"))
	if len(m) != 4 {
		return refuse(GateAudit, "item %s: the auditor's verdict line is unreadable (line 1 of the report is not `VERDICT: <verdict> · round <n> · head <12 hex>`)", item.ID)
	}
	vround, _ := strconv.Atoi(m[2])
	if vround != round || m[3] != head12(pr.HeadSHA) {
		return refuse(GateAudit, "item %s: the verdict names round %d at head %s, but the item is round %d and #%d is at %s", item.ID, vround, m[3], round, pr.Number, short(pr.HeadSHA))
	}
	if m[1] != "no-red" {
		return refuse(GateAudit, "item %s: the verdict is %s, not no-red: the ladder has not stopped", item.ID, m[1])
	}
	if n := redFindings(summary); n > 0 {
		return refuse(GateAudit, "item %s: the auditor's report has %d 🔴 findings", item.ID, n)
	}
	return nil
}

// ShadowRule is THE shadow rule, one for every write muster refuses in
// shadow: the shepherd's five writes and a draft officer's push under a
// pull-request fix item (fixpush.go). Officer is the officer's id;
// OfficerShadow reads HQ's officer row `shadow`; Shadowed records a refused
// write ("would have …") against its item (appended to that item's
// officer_complete summary); nil records nothing.
type ShadowRule struct {
	Officer       string
	OfficerShadow func(ctx context.Context) (bool, error)
	Shadowed      func(item officers.Item, line string)
}

// shadowed decides whether a write under item runs in shadow: the item's own
// `shadow` when HQ sends one, else the officer row's. 🔴 FAIL CLOSED: a row
// that cannot be read, or no way to read it, is shadow. It returns why (the
// refusal's tail).
func (r ShadowRule) shadowed(ctx context.Context, item officers.Item) (bool, string) {
	if item.Shadow != nil {
		if *item.Shadow {
			return true, "HQ runs item " + string(item.ID) + " in shadow"
		}
		return false, ""
	}
	if r.OfficerShadow == nil {
		return true, "muster cannot read HQ's officer row for its shadow, so the write is treated as shadow"
	}
	on, err := r.OfficerShadow(ctx)
	if err != nil {
		return true, fmt.Sprintf("HQ's officer row could not be read for its shadow (%v), so the write is treated as shadow", err)
	}
	if on {
		return true, "HQ runs officer " + r.Officer + " in shadow"
	}
	return false, ""
}

// refusal is "", false when the write may go ahead; else it records "would
// have <would>" for item and returns the refusal text `shadow: would have
// <would>` with why.
func (r ShadowRule) refusal(ctx context.Context, item officers.Item, would string) (string, bool) {
	on, why := r.shadowed(ctx, item)
	if !on {
		return "", false
	}
	line := "would have " + would
	if r.Shadowed != nil {
		r.Shadowed(item, line)
	}
	return "shadow: " + line + " (" + why + "; nothing was sent)", true
}

// shadowRefusal is the shepherd's writes asking ShadowRule.
func (s *Shepherd) shadowRefusal(ctx context.Context, item officers.Item, would string) (string, bool) {
	return ShadowRule{Officer: s.OfficerID, OfficerShadow: s.OfficerShadow, Shadowed: s.Shadowed}.refusal(ctx, item, would)
}

// head12 is a sha's first 12 characters (all of it when shorter).
func head12(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// MergeAskArgs are officer_shepherd_merge_ask's arguments: everything of HQ's
// body but `instance`, which muster sets from the caller.
type MergeAskArgs struct {
	ItemID   officers.ItemID `json:"item_id"`
	Repo     string          `json:"repo"`
	PR       int             `json:"pr"`
	HeadSHA  string          `json:"head_sha"`
	BaseRef  string          `json:"base_ref"`
	BaseSHA  string          `json:"base_sha"`
	Summary  string          `json:"summary"`
	Purpose  string          `json:"purpose"`
	How      string          `json:"how"`
	Evidence MergeEvidence   `json:"evidence"`
	To       string          `json:"to,omitempty"`
}

// MergeEvidence is the merge ask's `evidence`, field for field HQ's — and
// nothing more: HQ reads the audit ladder and CI from its OWN records and
// refuses `ladder` and `checks` (400 unknown field), so the strict decoder
// (ParseMergeAskArgs) refuses them here first.
type MergeEvidence struct {
	// OfficerItem names the AUTHORING officer's item: Item is HQ's integer
	// item id (HQ decodes it as one, strictly; a string is a 400 there and
	// refused here).
	OfficerItem struct {
		Officer string `json:"officer"`
		Item    int64  `json:"item"`
	} `json:"officer_item"`
	OpenYellows []struct {
		Where string `json:"where"`
		Text  string `json:"text"`
	} `json:"open_yellows"`
	ProtectedIdentity struct {
		Result string `json:"result"`
		Detail string `json:"detail,omitempty"`
	} `json:"protected_identity"`
	SettledAt string `json:"settled_at"`
	Rollback  string `json:"rollback"`
}

// mergeAskBody is HQ's body: the arguments with `instance` added.
type mergeAskBody struct {
	Instance string        `json:"instance"`
	To       string        `json:"to,omitempty"`
	Repo     string        `json:"repo"`
	PR       int           `json:"pr"`
	HeadSHA  string        `json:"head_sha"`
	BaseRef  string        `json:"base_ref"`
	BaseSHA  string        `json:"base_sha"`
	Summary  string        `json:"summary"`
	Purpose  string        `json:"purpose"`
	How      string        `json:"how"`
	Evidence MergeEvidence `json:"evidence"`
}

// ParseMergeAskArgs decodes officer_shepherd_merge_ask's arguments, refusing
// a field HQ's body does not have (HQ refuses unknown fields) and refusing
// `instance` by name: muster sets it from the calling agent.
func ParseMergeAskArgs(raw []byte) (MergeAskArgs, error) {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return MergeAskArgs{}, refuse(GateMergeAsk, "arguments must be one JSON object")
	}
	for k := range keys {
		if strings.EqualFold(k, "instance") {
			return MergeAskArgs{}, refuse(GateMergeAsk, "instance is not an argument: muster names the calling instance itself")
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var a MergeAskArgs
	if err := dec.Decode(&a); err != nil {
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) && te.Field == "evidence.officer_item.item" {
			return MergeAskArgs{}, refuse(GateMergeAsk, "evidence.officer_item.item must be an integer: the authoring officer's HQ item id (got a %s)", te.Value)
		}
		return MergeAskArgs{}, refuse(GateMergeAsk, "arguments: %v", err)
	}
	return a, nil
}

// check is the merge ask's STRUCTURE, all muster checks itself: an item id
// it can put in HQ's path. Every bound (texts, lists, shas) is HQ's to check,
// and HQ's 400 comes back verbatim — a second copy here would drift.
func (a MergeAskArgs) check() error {
	if strings.TrimSpace(string(a.ItemID)) == "" || strings.ContainsAny(string(a.ItemID), "/?#") || len(a.ItemID) > 64 {
		return refuse(GateMergeAsk, "item_id is required: the HQ item you hold for the pull request")
	}
	return nil
}

// MergeAsk is officer_shepherd_merge_ask: instance's request that HQ open a
// merge ask for pull request a.PR of a.Repo at a.HeadSHA, under the item it
// holds. In order: the officer is a shepherd and a.Repo one of its
// repositories; the item id (the only structure muster needs: every bound is
// HQ's, its 400 passed back verbatim); the item gate — HQ shows instance holding a.ItemID under a
// live lease, its subject that pull request (ItemByID); shadow; then the
// POST, with `instance` set here. It returns HQ's answer, or HQ's error
// (*officers.HQError / *officers.RetryableError) for the caller to pass back
// verbatim; a local refusal is a *Refusal.
func (g *ShepherdGates) MergeAsk(ctx context.Context, instance string, a MergeAskArgs) ([]byte, error) {
	if g == nil || g.Shepherd == nil || !g.Shepherd.Ship.IsShepherd() {
		return nil, refuse(GateStage, "this officer is not a shepherd")
	}
	sh := g.Shepherd
	if _, ok := sh.Ship.RepoFor(sh.Repo, a.Repo); !ok {
		return nil, refuse(GateBase, "%q is not one of this shepherd's repositories", a.Repo)
	}
	if err := a.check(); err != nil {
		return nil, err
	}
	if instance == "" {
		return nil, refuse(GateItem, "a request from an unknown instance: its hold on item %s cannot be checked", a.ItemID)
	}
	if g.ItemByID == nil || g.PostMergeAsk == nil {
		return nil, refuse(GateItem, "muster cannot reach HQ's items for this shepherd")
	}
	item, err := g.ItemByID(ctx, instance, a.ItemID, a.Repo, a.PR)
	if err != nil {
		return nil, refuse(GateItem, "HQ does not show %s holding item %s for %s: %v", instance, a.ItemID, officers.ShepherdSubject(a.Repo, a.PR), err)
	}
	if why, on := sh.shadowRefusal(ctx, item, fmt.Sprintf("asked HQ for a merge ask of %s#%d at %s", a.Repo, a.PR, head12(a.HeadSHA))); on {
		return nil, refuse(GateShadow, "%s", why)
	}
	body, err := json.Marshal(mergeAskBody{Instance: instance, To: a.To, Repo: a.Repo, PR: a.PR, HeadSHA: a.HeadSHA, BaseRef: a.BaseRef,
		BaseSHA: a.BaseSHA, Summary: a.Summary, Purpose: a.Purpose, How: a.How, Evidence: a.Evidence})
	if err != nil {
		return nil, fmt.Errorf("encoding the merge ask: %v", err)
	}
	return g.PostMergeAsk(ctx, item.ID, body)
}
