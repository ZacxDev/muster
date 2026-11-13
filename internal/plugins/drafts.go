package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/fence"
	"github.com/ZacxDev/muster/internal/flows"
)

// REPLY DRAFTS (plugin_reply_drafts, migration 0016): the chief drafts a
// reply to a plugin message (the reply_draft tool), its OWNER approves or rejects
// the exact text on /plugins, and the plugin then CLAIMS an approved draft, sends
// it upstream, and reports the result. muster never sends anything itself.
//
// The life of a draft:
//
//	pending ──approve──▶ approved ──claim──▶ (approved, claimed, leased)
//	   │  ╲                  │                    │            ╲
//	reject  superseded    7 days               result         lease lapses
//	   ▼      (newer          ▼                    ▼               ▼
//	rejected   draft)      expired          sent / failed     unconfirmed
//
// and, from pending or approved-unclaimed: superseded, when ANOTHER draft of the
// same message is reported sent (FinishDraft).
//
// 🔴 AT MOST ONCE, AND THAT IS WHY A LAPSED LEASE IS NOT RE-OFFERED. A plugin that
// claimed a draft and then went quiet may have sent it upstream before it died.
// Offering the draft again would risk the sender receiving it twice; so a claim
// whose lease lapses without a result becomes `unconfirmed` and is never offered
// again. The claim query's `claimed_at IS NULL` is the rule; the lapse is
// bookkeeping. What the owner may do instead is approve a NEW draft for the same
// message (the chief files one; its card warns that an earlier reply may already
// have reached the sender) — and if the unconfirmed one is then reported sent
// after all, that new draft is superseded before it can go (FinishDraft).

// DraftStatus is a draft's state. The set is closed (the 0015 CHECK).
type DraftStatus string

const (
	DraftPending     DraftStatus = "pending"
	DraftApproved    DraftStatus = "approved"
	DraftRejected    DraftStatus = "rejected"
	DraftSuperseded  DraftStatus = "superseded"
	DraftSent        DraftStatus = "sent"
	DraftFailed      DraftStatus = "failed"
	DraftUnconfirmed DraftStatus = "unconfirmed"
	DraftExpired     DraftStatus = "expired"
)

const (
	// MaxDraftBytes caps a draft's body, the same as an inbound message's.
	MaxDraftBytes = 16 << 10
	// MaxPendingDraftsPerAgent: drafts one agent may have awaiting its owner.
	MaxPendingDraftsPerAgent = 20
	// DraftTTL: a draft not sent within this of being written expires, whether
	// it was still pending or approved and never claimed.
	DraftTTL = 7 * 24 * time.Hour
	// LeaseDuration: how long a claiming plugin has to report a result.
	LeaseDuration = 2 * time.Minute
	// MaxClaimWait bounds one long-poll of the claim route.
	MaxClaimWait = 25 * time.Second
	// MaxExternalRefRunes and MaxDetailBytes cap what a result may carry.
	MaxExternalRefRunes = 200
	MaxDetailBytes      = 4096
	// MaxResultRequestBytes bounds what the result route reads at all; the caps
	// above are checked after decoding. 🔴 IT IS THE WORST-CASE ESCAPED SIZE OF A
	// VALID RESULT (MaxRequestBytes' reasoning): detail is capped in BYTES and any
	// byte may arrive as a 6-byte `\u00XX` escape (encoding/json spells `<` so),
	// external_ref in RUNES and a 4-byte rune may arrive as a 12-byte surrogate
	// pair; 1 KiB covers the keys, `ok` and indentation. A smaller cap refused a
	// valid detail of 4,096 `<` (audit round 1).
	MaxResultRequestBytes = 6*MaxDetailBytes + 12*MaxExternalRefRunes + 1024
)

// Draft is one row of plugin_reply_drafts.
type Draft struct {
	ID          int64       `json:"id"`
	OwnerID     int64       `json:"-"`
	MessageID   int64       `json:"messageId"`
	AgentID     *int64      `json:"-"`
	AgentName   string      `json:"agentName"`
	Body        string      `json:"body"`
	Status      DraftStatus `json:"status"`
	CreatedAt   time.Time   `json:"createdAt"`
	ExpiresAt   time.Time   `json:"expiresAt"`
	DecidedAt   *time.Time  `json:"decidedAt,omitempty"`
	ApprovedAt  *time.Time  `json:"approvedAt,omitempty"`
	ClaimedAt   *time.Time  `json:"claimedAt,omitempty"`
	LeaseUntil  *time.Time  `json:"leaseUntil,omitempty"`
	FinishedAt  *time.Time  `json:"finishedAt,omitempty"`
	ExternalRef string      `json:"externalRef,omitempty"`
	Detail      string      `json:"detail,omitempty"`
	// RouteID is the session route this draft was relayed from (0021), when
	// it is one: a routed session's answer that fell back to the owner.
	RouteID *string `json:"-"`
	// CheckState is the plugin's pre-approval check (0112): "" when none is
	// asked for (its plugin lacks CapDraftChecks), else CheckAwaiting,
	// CheckSendable or CheckRefused. CheckedAt is when the verdict came.
	CheckState DraftCheckState `json:"checkState,omitempty"`
	CheckedAt  *time.Time      `json:"checkedAt,omitempty"`
	// Refusal is what the store DECIDED, in the transaction that recorded a
	// refusal of this draft (refuseRouteDraftTx): set only on the Draft that
	// FinishDraft (a fresh "failed") or CheckDraft (a fresh "refused")
	// returns; never stored, never read back. internal/api acts on it — the
	// push's wording, the sweep, the chief's note — and reads nothing again.
	Refusal RefusalOutcome `json:"-"`
	// An OUTBOUND draft's (migration 0121, outbound.go): MessageID is 0, and
	// PluginID names its plugin. Venue, TargetKind/TargetRef, MediaRefs and
	// Officer/OfficerItemID are what the officer filed; Media is what the
	// plugin uploaded for the refs; the Approved* hashes are what the owner
	// approved (set with ApprovedAt, cleared by a withdrawal). All nil/empty
	// on a reply draft.
	PluginID            *int64       `json:"pluginId,omitempty"`
	Venue               *string      `json:"venue,omitempty"`
	TargetKind          *string      `json:"targetKind,omitempty"`
	TargetRef           *string      `json:"targetRef,omitempty"`
	MediaRefs           []string     `json:"mediaRefs,omitempty"`
	Media               []DraftMedia `json:"media,omitempty"`
	Officer             *string      `json:"officer,omitempty"`
	OfficerItemID       *int64       `json:"officerItemId,omitempty"`
	ApprovedBodySHA256  *string      `json:"-"`
	ApprovedMediaSHA256 []string     `json:"-"`
	// DecisionReason is the reason its OWNER gave with a rejection (or a
	// withdrawal), when they gave one (migration 0123, DraftDecision.Reason):
	// shown on the card's history, and an outbound draft's officer is told it
	// (internal/api outboundSettled). nil otherwise.
	DecisionReason *string `json:"-"`
}

// RefusalOutcome is where a refused draft goes (Draft.Refusal).
type RefusalOutcome string

const (
	// RefusalNone: not a fresh refusal.
	RefusalNone RefusalOutcome = ""
	// RefusalAnsweredElsewhere: another reply to the message was sent or is in
	// flight (OtherReplyOut): recorded on the draft only, nothing goes back.
	RefusalAnsweredElsewhere RefusalOutcome = "answered_elsewhere"
	// RefusalRedraft: its route ended send_failed owing a hand-back to its
	// SESSION (Route.Redraft).
	RefusalRedraft RefusalOutcome = "redraft"
	// RefusalRouteToChief: its route ended send_failed owing a hand-back to
	// the CHIEF (past MaxRouteRedrafts, or no session to go back to).
	RefusalRouteToChief RefusalOutcome = "route_to_chief"
	// RefusalNoRoute: no route took it (the chief filed it, or its route had
	// moved on): the chief is to be told directly.
	RefusalNoRoute RefusalOutcome = "no_route"
	// RefusalOutbound: an officer's outbound draft (outbound.go): its officer
	// is told, by a draft.refused item (internal/api).
	RefusalOutbound RefusalOutcome = "outbound"
)

// DraftCheckState is a draft's pre-approval check by its plugin (0112's
// check_state); "" is none asked for.
type DraftCheckState string

const (
	// CheckAwaiting: filed for a plugin with CapDraftChecks and not yet
	// checked. The owner cannot approve it (ErrDraftUnchecked), and is not
	// asked to until it is sendable.
	CheckAwaiting DraftCheckState = "awaiting"
	// CheckSendable: the plugin said it can send it; the owner decides.
	CheckSendable DraftCheckState = "sendable"
	// CheckRefused: the plugin said it cannot send it. The draft is rejected
	// and its Detail is the plugin's reason.
	CheckRefused DraftCheckState = "refused"
	// CheckLapsed: the plugin gave no verdict within DraftCheckWait
	// (LapseDraftCheck). The draft is an ordinary pending one the owner may
	// approve; the card and the push say the plugin did not check it.
	CheckLapsed DraftCheckState = "lapsed"
)

// Scan reads check_state, NULL as "".
func (c *DraftCheckState) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*c = ""
	case string:
		*c = DraftCheckState(v)
	case []byte:
		*c = DraftCheckState(v)
	default:
		return fmt.Errorf("plugins: check_state is %T", src)
	}
	return nil
}

// AwaitingCheck reports whether d is pending its plugin's verdict: THE ONE
// PREDICATE for "the owner cannot approve this yet" — the decision's refusal
// (DecideDraftFrom), the Asks card, the attention queue and the owner's push
// all branch on it.
func (d Draft) AwaitingCheck() bool { return d.Status == DraftPending && d.CheckState == CheckAwaiting }

// RefusedByCheck reports whether d's plugin refused it before approval.
func (d Draft) RefusedByCheck() bool { return d.CheckState == CheckRefused }

// Claim is what the claim route hands a plugin: an approved draft to send.
type Claim struct {
	DraftID           int64     `json:"draft_id"`
	MessageExternalID string    `json:"message_external_id"`
	Body              string    `json:"body"`
	ApprovedAt        time.Time `json:"approved_at"`
	// Kind is "reply" (a reply draft) or "outbound" (outbound.go). Only an
	// outbound claim carries the fields below (Claim.MarshalJSON).
	Kind       string         `json:"kind"`
	BodySHA256 string         `json:"body_sha256,omitempty"`
	Venue      string         `json:"venue,omitempty"`
	Target     OutboundTarget `json:"target"`
	Media      []ClaimMedia   `json:"media"`
}

// The draft kinds a claim or a check listing names.
const (
	DraftKindReply    = "reply"
	DraftKindOutbound = "outbound"
)

// MarshalJSON is the claim's wire form: a reply draft's four fields plus its
// kind; an outbound draft's also its body hash, venue, target and media.
func (c Claim) MarshalJSON() ([]byte, error) {
	out := map[string]any{"draft_id": c.DraftID, "message_external_id": c.MessageExternalID, "body": c.Body,
		"approved_at": c.ApprovedAt, "kind": c.Kind}
	if c.Kind == DraftKindOutbound {
		out["body_sha256"], out["venue"], out["target"], out["media"] = c.BodySHA256, c.Venue, c.Target, c.Media
	}
	return json.Marshal(out)
}

// DraftResult is what a plugin reports after trying to send a claimed draft.
type DraftResult struct {
	OK          bool   `json:"ok"`
	ExternalRef string `json:"external_ref"`
	Detail      string `json:"detail"`
}

// DraftCard is a draft with the message it answers and its plugin, for /plugins.
type DraftCard struct {
	Draft       Draft
	Message     Message
	PluginName  string
	PluginLabel string
	// EarlierUnconfirmed: another draft for the same message is unconfirmed —
	// it may already have reached the sender. The card says so.
	EarlierUnconfirmed bool
	// Route is the session route the draft came from (Draft.RouteID's row), or
	// the zero RouteTarget for a draft the chief filed itself.
	Route RouteTarget
	// ReplyTo is, for an outbound draft replying on a thread, the message its
	// plugin delivered with that venue and thread — the human reply it
	// answers (untrusted) — when one arrived; nil otherwise. Message is the
	// zero Message on every outbound card.
	ReplyTo *Message
}

var (
	// ErrNoReply: the message was marked no_reply by its plugin.
	ErrNoReply = errors.New("plugins: this message may not be replied to (no_reply)")
	// ErrTooManyPendingDrafts: the agent has MaxPendingDraftsPerAgent awaiting its owner.
	ErrTooManyPendingDrafts = errors.New("plugins: too many reply drafts awaiting the owner")
	// ErrDraftState: the draft is not in a state that allows this (409).
	ErrDraftState = errors.New("plugins: the draft is not in a state that allows this")
	// ErrReplyInFlight: another reply to the same message is approved and not yet
	// settled; a second one would risk two replies reaching the sender.
	ErrReplyInFlight = errors.New("plugins: another reply to this message is already approved and on its way")
	// ErrResultConflict: the draft already settled with the OTHER result (409).
	ErrResultConflict = errors.New("plugins: the draft already settled with a different result")
	// ErrDraftBodyMismatch: the decision named a body_sha256 that is not the
	// draft's body (DraftDecision.BodySHA256).
	ErrDraftBodyMismatch = errors.New("plugins: the draft's body is not the one the decision names")
	// ErrDraftUnchecked: an approval of a draft its plugin has not checked yet
	// (Draft.AwaitingCheck).
	ErrDraftUnchecked = errors.New("plugins: the plugin has not checked this draft yet")
	// ErrCheckConflict: the draft already has the OTHER verdict (409).
	ErrCheckConflict = errors.New("plugins: the draft already has a different verdict")
)

// Where an owner's decision on a draft was made: the decided_via its
// plugin.draft.* audit row records.
const (
	// DraftViaBrowser: the Asks page's Reply drafts tab.
	DraftViaBrowser = "browser"
	// DraftViaMCP: plugin_draft_decide, from one of the owner's mirrored Claude
	// Code sessions (internal/api/plugin_draft_tools.go).
	DraftViaMCP = "mcp"
)

// DraftDecisionSource is where a decision was made: Via (DraftViaBrowser,
// DraftViaMCP, or "" for none recorded) and, for DraftViaMCP, the mirror host
// (proven by its token) and the Claude session the caller named (a claim).
type DraftDecisionSource struct {
	Via, Host, Session string
}

// String is the source as the audit row names it: "mcp from ws/<session>".
func (src DraftDecisionSource) String() string {
	if src.Via == DraftViaMCP {
		return src.Via + " from " + src.Host + "/" + src.Session
	}
	return src.Via
}

// DraftDecision is one owner decision on a draft (DecideDraftFrom).
type DraftDecision struct {
	// Approve sends it; otherwise it is rejected (or, on the Asks page, an
	// unclaimed approval withdrawn).
	Approve bool
	// Via is where the decision was made, recorded on its audit row.
	Via DraftDecisionSource
	// BodySHA256, when set, must be the lowercase hex SHA-256 of the draft's
	// stored body (ErrDraftBodyMismatch): the decision is on exactly the text
	// the decider read. Required for DraftViaMCP, and for any approval of an
	// outbound draft.
	BodySHA256 string
	// MediaSHA256, for an OUTBOUND draft, is the SHA-256 of each medium the
	// decider saw, in the draft's ref order (Draft.MediaSHA256): an approval
	// must carry it (empty, not nil, for a draft with no media), and it is
	// refused unless it is exactly the uploaded media (ErrDraftMediaMismatch).
	// The approval records it, with BodySHA256, as the binding the claim
	// re-checks. Ignored for a reply draft.
	MediaSHA256 []string
	// Reason is the owner's optional reason for a REJECTION (a withdrawal
	// included): plain text, at most MaxDecisionReasonRunes characters
	// (ScreenDecisionReason), stored on the draft (0123) and its audit row.
	// "" is none. An approval with a reason is refused.
	Reason string
}

// MaxDecisionReasonRunes bounds an owner's rejection reason (0123's CHECK
// says the same).
const MaxDecisionReasonRunes = 1000

// ScreenDecisionReason is an owner's rejection reason as it is stored: CRLF
// as LF, trimmed, "" for none; or an ErrInvalid. 🔴 PLAIN TEXT: valid UTF-8,
// no NUL, no control or invisible character but a newline or a tab
// (fence.Unsafe), at most MaxDecisionReasonRunes — it is repeated to an
// officer as an item's detail and drawn on the card.
func ScreenDecisionReason(reason string) (string, error) {
	reason = strings.TrimSpace(strings.ReplaceAll(reason, "\r\n", "\n"))
	if reason == "" {
		return "", nil
	}
	if !utf8.ValidString(reason) {
		return "", invalid("reason must be UTF-8 text")
	}
	if n := utf8.RuneCountInString(reason); n > MaxDecisionReasonRunes {
		return "", invalid("reason is %d characters; at most %d", n, MaxDecisionReasonRunes)
	}
	for _, r := range reason {
		if r != '\n' && r != '\t' && fence.Unsafe(r) {
			return "", invalid("reason is plain text: no control or invisible characters (found U+%04X)", r)
		}
	}
	return reason, nil
}

// DraftBodySHA256 is the lowercase hex SHA-256 of a draft body: what
// DraftDecision.BodySHA256 must equal.
func DraftBodySHA256(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// DraftObject is the audit_log `object` naming a reply draft.
func DraftObject(id int64) string { return "plugin_draft:" + strconv.FormatInt(id, 10) }

// ScreenDraft is the screen an agent's reply text passes before it is stored:
// the form it is stored, shown and sent in (NFKC — flows.NormalizeAgentText), or
// an ErrInvalid naming why it is refused.
//
// 🔴 THE CONTROL IS THE OWNER'S APPROVAL OF THE EXACT VISIBLE TEXT, as for a
// browser-flow request (internal/flows/screen.go). So: no character the approval
// card cannot show (flows.HiddenCharRefusal), and the deny patterns
// (flows.AgentTextRefusal) — a heuristic against an agent talked into sending an
// address, a credential or the deployment's own names to an outside party. It is
// never the gate; the approval is.
func ScreenDraft(body string, denyWords []string) (string, error) {
	return screenDraft(body, denyWords, flows.AgentTextRefusal)
}

// screenDraft is ScreenDraft with the deny screen refusal (ScreenOutbound's
// differs: public host names pass).
func screenDraft(body string, denyWords []string, refusal func(string, []string) string) (string, error) {
	if !utf8.ValidString(body) || strings.ContainsRune(body, 0) {
		return "", invalid("body must be UTF-8 text without NUL")
	}
	body = flows.NormalizeAgentText(body)
	switch {
	case strings.TrimSpace(body) == "":
		return "", invalid("body is required")
	case len(body) > MaxDraftBytes:
		return "", invalid("body is over %d bytes", MaxDraftBytes)
	}
	if why := flows.HiddenCharRefusal(body); why != "" {
		return "", invalid("body: %s", why)
	}
	if why := refusal(body, denyWords); why != "" {
		return "", invalid("body: %s", why)
	}
	return body, nil
}

// Validate checks a result's free-text fields.
func (r DraftResult) Validate() error {
	switch {
	case !utf8.ValidString(r.ExternalRef) || strings.ContainsRune(r.ExternalRef, 0) ||
		utf8.RuneCountInString(r.ExternalRef) > MaxExternalRefRunes:
		return invalid("external_ref is at most %d characters of UTF-8 text", MaxExternalRefRunes)
	case !utf8.ValidString(r.Detail) || strings.ContainsRune(r.Detail, 0) || len(r.Detail) > MaxDetailBytes:
		return invalid("detail is at most %d bytes of UTF-8 text", MaxDetailBytes)
	}
	return nil
}

// WithAgentDenyWords sets the deployment's own infrastructure names a reply
// draft may not carry (MUSTER_FLOW_AGENT_DENY_WORDS, the browser-flow screen's
// list): configuration, because this repository is public.
func (s *PGStore) WithAgentDenyWords(words []string) *PGStore {
	s.denyWords = append([]string(nil), words...)
	return s
}

const draftCols = `id, owner_id, message_id, agent_id, agent_name, body, status, created_at, expires_at, decided_at,
	approved_at, claimed_at, lease_until, finished_at, external_ref, detail, route_id, check_state, checked_at,
	plugin_id, venue, target_kind, target_ref, media_refs, media, officer, officer_item_id, approved_body_sha256, approved_media_sha256,
	decision_reason`

func draftDest(d *Draft, st *string) []any {
	return []any{&d.ID, &d.OwnerID, (*zeroIfNull)(&d.MessageID), &d.AgentID, &d.AgentName, &d.Body, st, &d.CreatedAt, &d.ExpiresAt,
		&d.DecidedAt, &d.ApprovedAt, &d.ClaimedAt, &d.LeaseUntil, &d.FinishedAt, &d.ExternalRef, &d.Detail, &d.RouteID,
		&d.CheckState, &d.CheckedAt,
		&d.PluginID, &d.Venue, &d.TargetKind, &d.TargetRef, &d.MediaRefs, &d.Media, &d.Officer, &d.OfficerItemID,
		&d.ApprovedBodySHA256, &d.ApprovedMediaSHA256, &d.DecisionReason}
}

// zeroIfNull scans a nullable bigint as 0 for NULL: an outbound draft's
// message_id (0121), so Draft.MessageID stays an int64 and 0 means "none".
type zeroIfNull int64

func (z *zeroIfNull) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*z = 0
	case int64:
		*z = zeroIfNull(v)
	default:
		return fmt.Errorf("plugins: message_id is %T", src)
	}
	return nil
}

// checkDueSQL is the check_due_at a NEW draft is filed with, given the SQL
// parameter holding its check_state: now + DraftCheckWait for an awaiting
// one, NULL otherwise (0112's plugin_reply_drafts_check_due).
func checkDueSQL(param string) string {
	return fmt.Sprintf(`CASE WHEN %s::text = 'awaiting' THEN now() + make_interval(secs => %d) END`, param, int(DraftCheckWait/time.Second))
}

// checkStateFor is the check_state a NEW pending draft of message messageID
// is filed with: CheckAwaiting when the message's plugin has CapDraftChecks,
// else nil (none asked for). Read in the filing's transaction.
//
// 🔴 THE PLUGIN ROW IS LOCKED (FOR SHARE) UNTIL THE FILING COMMITS, and the
// caller takes it BEFORE it locks any draft. SetDraftChecks turning the
// capability off takes the same row FOR UPDATE, then the drafts: so the two
// are serialised — a filing either sees the capability gone, or is committed
// before the turn-off's lapse runs and is lapsed by it — and they take their
// locks in one order (the plugin, then drafts), so they cannot deadlock.
// Without it a filing racing a turn-off was left awaiting a check nobody
// would make (audit round 2).
func checkStateFor(ctx context.Context, tx pgx.Tx, messageID int64) (*string, error) {
	var checks bool
	if err := tx.QueryRow(ctx, `SELECT $2 = ANY (p.capabilities) FROM plugin_messages m JOIN plugins p ON p.id = m.plugin_id
		WHERE m.id = $1 FOR SHARE OF p`, messageID, string(CapDraftChecks)).Scan(&checks); err != nil {
		return nil, err
	}
	if !checks {
		return nil, nil
	}
	st := string(CheckAwaiting)
	return &st, nil
}

func scanDraft(row pgx.Row) (Draft, error) {
	var d Draft
	var st string
	err := row.Scan(draftDest(&d, &st)...)
	d.Status = DraftStatus(st)
	return d, err
}

// sweep runs sweepDrafts in a transaction of its own.
//
// 🔴 IT IS NEVER RUN INSIDE A TRANSACTION THAT HOLDS A MESSAGE OR DRAFT LOCK.
// It updates rows across every message the principal can see, so inside such a
// transaction it could wait on a row another writer holds while that writer
// waits on this one's (a deadlock, as a 500). Run first and on its own, it
// holds nothing it waits for; and its rows are taken in id order
// (sweepDrafts), so two sweeps cannot deadlock each other either.
func (s *PGStore) sweep(ctx context.Context) error {
	return s.inTx(ctx, func(tx pgx.Tx) error { return sweepDrafts(ctx, tx) })
}

// sweepDrafts settles what time has decided, for the principal's rows: a draft
// pending or approved-but-unclaimed past its expiry is `expired`, and a claim
// whose lease lapsed without a result is `unconfirmed` — the at-most-once rule
// acting. Each is audited, one row per draft, as whichever principal's request
// ran the sweep.
func sweepDrafts(ctx context.Context, tx pgx.Tx) error {
	for _, step := range []struct{ q, action, why string }{
		{`UPDATE plugin_reply_drafts SET status = 'expired', decided_at = coalesce(decided_at, now())
			WHERE id IN (SELECT id FROM plugin_reply_drafts
			              WHERE status IN ('pending', 'approved') AND claimed_at IS NULL AND expires_at <= now()
			              ORDER BY id FOR UPDATE)
			RETURNING id`,
			"plugin.draft.expired", "not sent within 7 days of being written"},
		{`UPDATE plugin_reply_drafts SET status = 'unconfirmed'
			WHERE id IN (SELECT id FROM plugin_reply_drafts
			              WHERE status = 'approved' AND claimed_at IS NOT NULL AND lease_until <= now()
			              ORDER BY id FOR UPDATE)
			RETURNING id`,
			"plugin.draft.unconfirmed", "lease lapsed without a result"},
	} {
		rows, err := tx.Query(ctx, step.q)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := audit(ctx, tx, step.action, DraftObject(id), map[string]any{"why": step.why}); err != nil {
				return err
			}
		}
	}
	return nil
}

// lockMessage locks message id's row and answers ErrReplyInFlight when another
// draft for it (not except) is approved and unsettled: not yet claimed, or
// claimed within its lease. (A lapsed lease is unconfirmed, whether or not the
// sweep has said so yet: it no longer blocks, and its card warns instead.)
//
// 🔴 ONE REPLY IN FLIGHT PER MESSAGE, AND THE MESSAGE ROW IS THE LOCK. Filing,
// approving and settling all take it first, so an owner approving draft 1
// while the chief files draft 2 for the same message are serialised, and the
// second never becomes a second approved reply the sender would receive too.
// A reply that has SETTLED (sent, failed) does not block a new one: answering
// again after a send is the owner's decision to make on the new card.
func lockMessage(ctx context.Context, tx pgx.Tx, id, except int64) (Message, error) {
	m, err := scanMessage(tx.QueryRow(ctx, `SELECT `+messageCols+` FROM plugin_messages WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Message{}, ErrNotFound
	}
	if err != nil {
		return Message{}, err
	}
	var inFlight bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM plugin_reply_drafts
		WHERE message_id = $1 AND status = 'approved' AND id <> $2
		  AND (claimed_at IS NULL OR lease_until > now()))`, id, except).Scan(&inFlight); err != nil {
		return Message{}, err
	}
	if inFlight {
		return m, ErrReplyInFlight
	}
	return m, nil
}

// FileDraft stores agent agentID's reply to message messageID: the message must
// be one delivered to that agent and its owner (DeliveredTo — checked here, so
// it holds whatever store is wired) and not marked no_reply; the body passes
// ScreenDraft. A pending draft already filed for the message is superseded
// (superseded counts them). At most MaxPendingDraftsPerAgent may be pending per
// agent, not counting the one this replaces.
func (s *PGStore) FileDraft(ctx context.Context, agentID, ownerID int64, agentName string, messageID int64, body string) (Draft, int, error) {
	if ownerID <= 0 || agentID <= 0 {
		return Draft{}, 0, invalid("the agent has no owner")
	}
	body, err := ScreenDraft(body, s.denyWords)
	if err != nil {
		return Draft{}, 0, err
	}
	if err := s.sweep(ctx); err != nil {
		return Draft{}, 0, err
	}
	var d Draft
	superseded := 0
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		// 🔴 THE MESSAGE ROW IS LOCKED FIRST (lockMessage): two drafts for one
		// message are filed one at a time, so "supersede the pending one, insert
		// mine" cannot leave two pending (the one-pending index would refuse it, as
		// a 500), and none is filed while another reply to it is in flight.
		m, inFlight := lockMessage(ctx, tx, messageID, 0)
		if inFlight != nil && !errors.Is(inFlight, ErrReplyInFlight) {
			return inFlight
		}
		// Who may draft is decided before whether they may draft NOW, so the
		// in-flight answer says nothing about a message that is not theirs.
		if !DeliveredTo(m, agentID, ownerID) {
			return ErrNotFound
		}
		if m.NoReply {
			return ErrNoReply
		}
		if inFlight != nil {
			return inFlight
		}
		// One agent's filings are serialised so the pending count cannot be raced.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('plugin_reply_drafts.file'), $1::int)`, int32(agentID)); err != nil {
			return err
		}
		var pending int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM plugin_reply_drafts
			WHERE agent_id = $1 AND status = 'pending' AND message_id <> $2`, agentID, messageID).Scan(&pending); err != nil {
			return err
		}
		if pending >= MaxPendingDraftsPerAgent {
			return fmt.Errorf("%w: %d await your owner (the limit is %d); wait for a decision",
				ErrTooManyPendingDrafts, pending, MaxPendingDraftsPerAgent)
		}
		// The plugin row before any draft (checkStateFor's lock order).
		check, err := checkStateFor(ctx, tx, messageID)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE plugin_reply_drafts SET status = 'superseded', decided_at = now()
			WHERE message_id = $1 AND status = 'pending'`, messageID)
		if err != nil {
			return err
		}
		superseded = int(tag.RowsAffected())
		d, err = scanDraft(tx.QueryRow(ctx, `
			INSERT INTO plugin_reply_drafts (owner_id, message_id, agent_id, agent_name, body, expires_at, check_state, check_due_at)
			VALUES ($1, $2, $3, $4, $5, now() + make_interval(secs => $6), $7, `+checkDueSQL("$7")+`) RETURNING `+draftCols,
			ownerID, messageID, agentID, cutRunes(agentName, 100), body, DraftTTL.Seconds(), check))
		if err != nil {
			return err
		}
		return audit(ctx, tx, "plugin.draft.filed", DraftObject(d.ID), map[string]any{
			"message_id": messageID, "agent_id": agentID, "superseded": superseded,
		})
	})
	if err != nil {
		return Draft{}, 0, err
	}
	return d, superseded, nil
}

// Draft reads one of the principal's drafts.
func (s *PGStore) Draft(ctx context.Context, id int64) (Draft, error) {
	d, err := scanDraft(s.pool.QueryRow(ctx, `SELECT `+draftCols+` FROM plugin_reply_drafts WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Draft{}, ErrNotFound
	}
	return d, err
}

// DraftCards is the principal's drafts — every one awaiting a decision
// (pending), then the most recent others — at most limit, after a sweep.
func (s *PGStore) DraftCards(ctx context.Context, limit int) ([]DraftCard, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	return s.draftCards(ctx, `ORDER BY (d.status = 'pending') DESC, d.id DESC LIMIT $1`, limit, limit)
}

// DraftCard is one of the principal's drafts as a card (after a sweep):
// what the Asks page draws for a just-decided draft that DraftCards' cut
// left out. ErrNotFound for none (or another owner's: row-level security).
func (s *PGStore) DraftCard(ctx context.Context, id int64) (DraftCard, error) {
	cards, err := s.draftCards(ctx, `WHERE d.id = $1`, id, 1)
	if err != nil {
		return DraftCard{}, err
	}
	if len(cards) == 0 {
		return DraftCard{}, ErrNotFound
	}
	return cards[0], nil
}

// draftCards is DraftCards' and DraftCard's two queries — the reply drafts
// (each with its message) and the outbound drafts (outbound.go: its plugin,
// and the human reply it answers) — tail their WHERE / ORDER BY / LIMIT,
// merged in DraftCards' order and cut to limit.
func (s *PGStore) draftCards(ctx context.Context, tail string, arg any, limit int) ([]DraftCard, error) {
	if err := s.sweep(ctx); err != nil {
		return nil, err
	}
	out, err := s.replyDraftCards(ctx, tail, arg)
	if err != nil {
		return nil, err
	}
	outbound, err := s.outboundDraftCards(ctx, tail, arg)
	if err != nil {
		return nil, err
	}
	out = append(out, outbound...)
	sort.SliceStable(out, func(i, j int) bool {
		pi, pj := out[i].Draft.Status == DraftPending, out[j].Draft.Status == DraftPending
		if pi != pj {
			return pi
		}
		return out[i].Draft.ID > out[j].Draft.ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *PGStore) replyDraftCards(ctx context.Context, tail string, arg any) ([]DraftCard, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+prefixed("d", draftCols)+`, `+prefixed("m", messageCols)+`, p.name, p.label,
		       EXISTS (SELECT 1 FROM plugin_reply_drafts o
		                WHERE o.message_id = d.message_id AND o.id <> d.id AND o.status = 'unconfirmed'),
		       coalesce(r.target_kind, ''), coalesce(r.target_key, ''), coalesce(r.target_label, '')
		  FROM plugin_reply_drafts d
		  JOIN plugin_messages m ON m.id = d.message_id
		  JOIN plugins p ON p.id = m.plugin_id
		  LEFT JOIN session_routes r ON r.route_id = d.route_id
		 `+tail, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DraftCard{}
	for rows.Next() {
		var c DraftCard
		var dst, mst string
		dest := draftDest(&c.Draft, &dst)
		m := &c.Message
		dest = append(dest, messageDest(m, &mst)...)
		dest = append(dest, &c.PluginName, &c.PluginLabel, &c.EarlierUnconfirmed, &c.Route.Kind, &c.Route.Key, &c.Route.Label)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		c.Draft.Status, m.Status = DraftStatus(dst), Status(mst)
		out = append(out, c)
	}
	return out, rows.Err()
}

// outboundDraftCards is the outbound half of draftCards: each card's plugin,
// and — for a reply target — the human reply its plugin delivered with that
// venue and thread (OutboundReplyTo), when one arrived.
func (s *PGStore) outboundDraftCards(ctx context.Context, tail string, arg any) ([]DraftCard, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+prefixed("d", draftCols)+`, p.name, p.label
		  FROM plugin_reply_drafts d
		  JOIN plugins p ON p.id = d.plugin_id
		 `+outboundTail(tail), arg)
	if err != nil {
		return nil, err
	}
	out := []DraftCard{}
	for rows.Next() {
		var c DraftCard
		d, err := scanDraftWith(rows, &c.PluginName, &c.PluginLabel)
		if err != nil {
			rows.Close()
			return nil, err
		}
		c.Draft = d
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		d := out[i].Draft
		if t := d.Target(); t.Kind == TargetReply {
			m, ok, err := s.OutboundReplyTo(ctx, *d.PluginID, d.VenueText(), t.Ref)
			if err != nil {
				return nil, err
			}
			if ok {
				out[i].ReplyTo = &m
			}
		}
	}
	return out, nil
}

// outboundTail is draftCards' tail restricted to outbound drafts.
func outboundTail(tail string) string {
	if rest, ok := strings.CutPrefix(tail, "WHERE "); ok {
		return "WHERE d.message_id IS NULL AND " + rest
	}
	return "WHERE d.message_id IS NULL " + tail
}

// lockedDraft locks draft id after its message (lockMessage, the lock order
// every draft write takes), answering the message's in-flight check as well.
func lockedDraft(ctx context.Context, tx pgx.Tx, id int64) (Draft, error) {
	var messageID *int64
	err := tx.QueryRow(ctx, `SELECT message_id FROM plugin_reply_drafts WHERE id = $1`, id).Scan(&messageID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Draft{}, ErrNotFound
	}
	if err != nil {
		return Draft{}, err
	}
	// An outbound draft (no message, outbound.go) has no message to lock and
	// no sibling reply in flight: its own row is the lock.
	var inFlight error
	if messageID != nil {
		_, inFlight = lockMessage(ctx, tx, *messageID, id)
		if inFlight != nil && !errors.Is(inFlight, ErrReplyInFlight) {
			return Draft{}, inFlight
		}
	}
	cur, err := scanDraft(tx.QueryRow(ctx, `SELECT `+draftCols+` FROM plugin_reply_drafts WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Draft{}, ErrNotFound
	}
	if err != nil {
		return Draft{}, err
	}
	return cur, inFlight
}

// DecideDraftFrom approves or rejects one of the principal's PENDING, unexpired
// drafts (ErrDraftState otherwise), recording where the decision was made
// (dec.Via) on its audit row. An approved draft is what the plugin's next claim
// may hand out. Approving while another reply to the same message is in flight
// is ErrReplyInFlight; rejecting is always allowed.
//
// Rejecting an APPROVED draft that no plugin has claimed yet WITHDRAWS the
// approval (rejected, its approval time cleared — the audit row keeps it): an
// owner who spots a mistake, or whose plugin is offline, is not left with a
// reply that blocks the message for its whole 7 days. A claimed draft cannot be
// withdrawn: the plugin may already be sending it. A withdrawal is the Asks
// page's alone: an MCP decision acts only on a PENDING draft, the one
// plugin_drafts_list showed it.
//
// 🔴 A BodySHA256 THAT IS NOT THE STORED BODY'S IS REFUSED HERE, under the row
// lock beside the state (ErrDraftBodyMismatch), so every door gets it; an MCP
// decision must carry one. The body is immutable (0016's trigger), so the hash
// names one text for the draft's whole life.
func (s *PGStore) DecideDraftFrom(ctx context.Context, id int64, dec DraftDecision) (Draft, error) {
	switch dec.Via.Via {
	case "", DraftViaBrowser:
		if dec.Via.Host != "" || dec.Via.Session != "" {
			return Draft{}, invalid("only an MCP decision names a host and session")
		}
	case DraftViaMCP:
		if dec.Via.Host == "" || dec.Via.Session == "" {
			return Draft{}, invalid("an MCP decision names its host and session")
		}
		if dec.BodySHA256 == "" {
			return Draft{}, invalid("an MCP decision names the body_sha256 of the text it decides")
		}
	default:
		return Draft{}, invalid("unknown decision source %q", dec.Via.Via)
	}
	reason, err := ScreenDecisionReason(dec.Reason)
	if err != nil {
		return Draft{}, err
	}
	if dec.Approve && reason != "" {
		return Draft{}, invalid("a reason goes with a rejection, not an approval")
	}
	var reasonArg *string
	if reason != "" {
		reasonArg = &reason
	}
	if err := s.sweep(ctx); err != nil {
		return Draft{}, err
	}
	approve := dec.Approve
	var d Draft
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		cur, err := lockedDraft(ctx, tx, id)
		if errors.Is(err, ErrReplyInFlight) && (!approve || cur.Status != DraftPending) {
			err = nil
		}
		if err != nil && !errors.Is(err, ErrReplyInFlight) {
			d = cur
			return err
		}
		inFlight := err
		if dec.BodySHA256 != "" && dec.BodySHA256 != DraftBodySHA256(cur.Body) {
			d = cur
			return ErrDraftBodyMismatch
		}
		if inFlight != nil {
			d = cur
			return inFlight
		}
		detail := map[string]any{}
		if dec.Via.Via != "" {
			detail["decided_via"] = dec.Via.String()
		}
		if reasonArg != nil {
			detail["reason"] = *reasonArg
		}
		if !approve && cur.Status == DraftApproved && cur.ClaimedAt == nil && dec.Via.Via != DraftViaMCP {
			if d, err = scanDraft(tx.QueryRow(ctx, `UPDATE plugin_reply_drafts
				SET status = 'rejected', decided_at = now(), approved_at = NULL, approved_body_sha256 = NULL, approved_media_sha256 = NULL,
				    decision_reason = $2
				WHERE id = $1 RETURNING `+draftCols, id, reasonArg)); err != nil {
				return err
			}
			detail["message_id"], detail["approved_at"] = d.MessageID, cur.ApprovedAt.UTC().Format(time.RFC3339)
			return audit(ctx, tx, "plugin.draft.withdrawn", DraftObject(id), detail)
		}
		if cur.Status != DraftPending {
			d = cur
			return ErrDraftState
		}
		if approve && cur.AwaitingCheck() {
			// 🔴 ITS PLUGIN HAS NOT SAID IT CAN SEND IT (CheckDraft): the owner is
			// never asked to approve what the plugin may refuse. Rejecting it is
			// still allowed.
			d = cur
			return ErrDraftUnchecked
		}
		q, action := `UPDATE plugin_reply_drafts SET status = 'rejected', decided_at = now(), decision_reason = $2 WHERE id = $1 RETURNING `+draftCols, "plugin.draft.rejected"
		args := []any{id, reasonArg}
		if approve {
			q, action = `UPDATE plugin_reply_drafts SET status = 'approved', decided_at = now(), approved_at = now() WHERE id = $1 RETURNING `+draftCols, "plugin.draft.approved"
			args = []any{id}
		}
		if approve && cur.IsOutbound() {
			// 🔴 AN OUTBOUND APPROVAL BINDS THE BODY AND EVERY MEDIUM (outbound.go):
			// refused while a ref has no bytes, or unless the decider names exactly
			// the stored body's and media's hashes; the hashes are recorded and the
			// claim re-checks them (ClaimDraft).
			if missing := cur.MissingMedia(); len(missing) > 0 {
				d = cur
				return MissingMediaError{Missing: missing}
			}
			if dec.BodySHA256 == "" || dec.MediaSHA256 == nil {
				d = cur
				return invalid("an approval of an outbound draft names the body_sha256 and media_sha256 of what it approves")
			}
			if !slices.Equal(dec.MediaSHA256, cur.MediaSHA256()) {
				d = cur
				return ErrDraftMediaMismatch
			}
			q = `UPDATE plugin_reply_drafts SET status = 'approved', decided_at = now(), approved_at = now(),
				approved_body_sha256 = $2, approved_media_sha256 = $3 WHERE id = $1 RETURNING ` + draftCols
			args = append(args, DraftBodySHA256(cur.Body), cur.MediaSHA256())
		}
		if d, err = scanDraft(tx.QueryRow(ctx, q, args...)); err != nil {
			return err
		}
		detail["message_id"] = d.MessageID
		return audit(ctx, tx, action, DraftObject(id), detail)
	})
	return d, err
}

// ClaimDraft hands plugin pluginID ONE of its approved, unclaimed, unexpired
// drafts — the oldest approval first — and leases it for LeaseDuration. ok is
// false when there is none.
//
// 🔴 `claimed_at IS NULL` IS THE AT-MOST-ONCE RULE (see the file header): a
// draft is claimed once and its claim is never cleared. SKIP LOCKED lets two
// claims race without both taking one draft.
func (s *PGStore) ClaimDraft(ctx context.Context, pluginID int64) (Claim, bool, error) {
	if err := s.sweep(ctx); err != nil {
		return Claim{}, false, err
	}
	// An outbound draft whose binding broke is withdrawn and the next one
	// tried; a bounded number per call, so one claim cannot spin.
	for range maxClaimTries {
		c, found, retry, err := s.claimOne(ctx, pluginID)
		if err != nil || found || !retry {
			return c, found, err
		}
	}
	return Claim{}, false, nil
}

// maxClaimTries bounds how many broken outbound drafts one claim withdraws
// before it answers "nothing".
const maxClaimTries = 5

// claimOne is one attempt of ClaimDraft. retry: it withdrew an outbound
// draft whose body or media no longer hash to what was approved.
//
// 🔴 THE BINDING IS RE-CHECKED UNDER THE ROW LOCK, BEFORE THE LEASE (Draft.
// BindingHolds): the body is immutable and the media cannot change once
// approved (0121's triggers), so this fires only if something got past both
// — and then the owner's approval no longer describes what would be sent, so
// nothing is handed out and the approval is withdrawn, audited.
func (s *PGStore) claimOne(ctx context.Context, pluginID int64) (c Claim, found, retry bool, err error) {
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var ext string
		d, err := scanDraftWith(tx.QueryRow(ctx, `
			SELECT `+prefixed("d", draftCols)+`, coalesce(m.external_id, '')
			  FROM plugin_reply_drafts d
			  LEFT JOIN plugin_messages m ON m.id = d.message_id AND m.owner_id = d.owner_id
			 WHERE coalesce(m.plugin_id, d.plugin_id) = $1 AND d.status = 'approved' AND d.claimed_at IS NULL
			   AND d.expires_at > now() AND NOT coalesce(m.no_reply, false)
			 ORDER BY d.approved_at, d.id
			 LIMIT 1
			   FOR UPDATE OF d SKIP LOCKED`, pluginID), &ext)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if d.IsOutbound() && !d.BindingHolds() {
			if _, err := tx.Exec(ctx, `UPDATE plugin_reply_drafts SET status = 'rejected', decided_at = now(), approved_at = NULL,
				approved_body_sha256 = NULL, approved_media_sha256 = NULL, detail = $2 WHERE id = $1`, d.ID, BindingBrokenDetail); err != nil {
				return err
			}
			retry = true
			return audit(ctx, tx, "plugin.draft.binding_broken", DraftObject(d.ID), map[string]any{"plugin_id": pluginID, "why": BindingBrokenDetail})
		}
		if _, err := tx.Exec(ctx, `UPDATE plugin_reply_drafts
			SET claimed_at = now(), lease_until = now() + make_interval(secs => $2) WHERE id = $1`,
			d.ID, LeaseDuration.Seconds()); err != nil {
			return err
		}
		c = Claim{DraftID: d.ID, MessageExternalID: ext, Body: d.Body, ApprovedAt: d.ApprovedAt.UTC(), Kind: DraftKindReply}
		if d.IsOutbound() {
			c.Kind, c.BodySHA256, c.Venue, c.Target, c.Media = DraftKindOutbound, *d.ApprovedBodySHA256, d.VenueText(), d.Target(), claimMediaOf(d)
		}
		found = true
		return audit(ctx, tx, "plugin.draft.claimed", DraftObject(d.ID), map[string]any{"plugin_id": pluginID})
	})
	if err != nil {
		return Claim{}, false, false, err
	}
	return c, found, retry, nil
}

// BindingBrokenDetail is an outbound draft's detail when the claim withdrew
// it (claimOne).
const BindingBrokenDetail = "the body or media changed after approval"

// scanDraftWith scans draftCols then extra destinations.
func scanDraftWith(row pgx.Row, extra ...any) (Draft, error) {
	var d Draft
	var st string
	err := row.Scan(append(draftDest(&d, &st), extra...)...)
	d.Status = DraftStatus(st)
	return d, err
}

// FinishDraft records plugin pluginID's result for a draft IT claimed: sent when
// r.OK, failed otherwise. The same result again answers the stored draft
// unchanged (fresh false); the other result is ErrResultConflict. A draft of
// another plugin is ErrNotFound; one never claimed is ErrDraftState.
//
// 🔴 A "FAILED" ENDS THE DRAFT'S SESSION ROUTE send_failed, IN THE SAME
// TRANSACTION (refuseRouteDraftTx): the route stops reading as relayed, and it
// owes a hand-back — to the session that wrote the reply, to redraft it, or
// past MaxRouteRedrafts to the chief. A draft the chief filed itself has no
// route; internal/api tells the chief and the owner.
//
// A result arriving after the lease lapsed (the draft is `unconfirmed`) is still
// recorded: it is exactly the fact the owner would otherwise have to find out
// upstream.
//
// 🔴 A "SENT" SUPERSEDES EVERY OTHER UNDECIDED OR UNCLAIMED REPLY TO THE SAME
// MESSAGE, IN THE SAME TRANSACTION. The sender has the reply now, so another draft
// of that message must not go too: every OTHER draft of it that is pending, or
// approved and not yet claimed, becomes `superseded` (audited, naming the sent
// draft). This is the MESSAGE-scoped rule, not a per-draft chain: it is what
// closes the case where a draft's lease lapses (unconfirmed), the chief files a
// new draft for the message, the owner approves it — and the first is then
// reported sent after all. A claimed sibling is left alone (its plugin may be
// sending it; its own result will settle it), and a "failed" supersedes nothing.
func (s *PGStore) FinishDraft(ctx context.Context, pluginID, draftID int64, r DraftResult) (Draft, bool, error) {
	if err := r.Validate(); err != nil {
		return Draft{}, false, err
	}
	fresh := false
	want := DraftFailed
	if r.OK {
		want = DraftSent
	}
	var d Draft
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		// 🔴 THE MESSAGE ROW FIRST, the lock order every draft write takes
		// (lockMessage): a result and a filing, approval or withdrawal of the same
		// message's drafts are serialised rather than deadlocking, and the
		// supersede below sees every sibling committed before it.
		var messageID *int64
		err := tx.QueryRow(ctx, `SELECT d.message_id FROM plugin_reply_drafts d
			  LEFT JOIN plugin_messages m ON m.id = d.message_id AND m.owner_id = d.owner_id
			 WHERE d.id = $1 AND coalesce(m.plugin_id, d.plugin_id) = $2`, draftID, pluginID).Scan(&messageID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if messageID != nil {
			if _, err := lockMessage(ctx, tx, *messageID, draftID); err != nil && !errors.Is(err, ErrReplyInFlight) {
				return err
			}
		}
		cur, err := scanDraft(tx.QueryRow(ctx, `SELECT `+prefixed("d", draftCols)+`
			  FROM plugin_reply_drafts d
			  LEFT JOIN plugin_messages m ON m.id = d.message_id AND m.owner_id = d.owner_id
			 WHERE d.id = $1 AND coalesce(m.plugin_id, d.plugin_id) = $2
			   FOR UPDATE OF d`, draftID, pluginID))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		d = cur
		switch {
		case cur.Status == DraftSent || cur.Status == DraftFailed:
			if cur.Status != want {
				return ErrResultConflict
			}
			return nil // the same result again: the stored draft is the answer
		case cur.ClaimedAt == nil:
			return ErrDraftState
		case cur.Status != DraftApproved && cur.Status != DraftUnconfirmed:
			return ErrDraftState
		}
		var late bool
		if err := tx.QueryRow(ctx, `SELECT lease_until <= now() FROM plugin_reply_drafts WHERE id = $1`, draftID).Scan(&late); err != nil {
			return err
		}
		if d, err = scanDraft(tx.QueryRow(ctx, `UPDATE plugin_reply_drafts
			SET status = $2, finished_at = now(), external_ref = $3, detail = $4
			WHERE id = $1 RETURNING `+draftCols, draftID, string(want), r.ExternalRef, r.Detail)); err != nil {
			return err
		}
		if err := audit(ctx, tx, "plugin.draft."+string(want), DraftObject(draftID), map[string]any{
			"plugin_id": pluginID, "external_ref": r.ExternalRef, "late": late || cur.Status == DraftUnconfirmed,
		}); err != nil {
			return err
		}
		fresh = true
		if want == DraftFailed {
			d.Refusal, err = refuseRouteDraftTx(ctx, tx, d, r.Detail)
			return err
		}
		// 🔴 THE MESSAGE-SCOPED SUPERSEDE (see the doc comment). Lock order: the
		// message (above), this row, then its siblings in id order. This row is
		// `sent` by now, so the status filter already excludes it.
		if want == DraftSent && !cur.IsOutbound() {
			rows, err := tx.Query(ctx, `UPDATE plugin_reply_drafts SET status = 'superseded', decided_at = now()
				WHERE id IN (SELECT id FROM plugin_reply_drafts
				              WHERE message_id = $1
				                AND (status = 'pending' OR (status = 'approved' AND claimed_at IS NULL))
				              ORDER BY id FOR UPDATE)
				RETURNING id`, cur.MessageID)
			if err != nil {
				return err
			}
			ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
			if err != nil {
				return err
			}
			for _, id := range ids {
				if err := audit(ctx, tx, "plugin.draft.superseded", DraftObject(id), map[string]any{
					"why": "another reply to the same message was reported sent", "sent_draft_id": draftID}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return d, fresh && err == nil, err
}

// MaxRouteRedrafts is how many times one message's refused routed reply goes
// back to the session that wrote it to be written again (refuseRouteDraftTx):
// the refusal after that many goes to the chief instead, so a reply that keeps
// being refused cannot loop between the session and the plugin.
const MaxRouteRedrafts = 2

// MaxRefusalRunes bounds a plugin's refusal as muster repeats it on one line
// (RefusalText): in a push, a hand-back, a note to the chief, a routed paste.
const MaxRefusalRunes = 300

// RefusalText is a plugin's refusal detail as muster repeats it on one line:
// sanitised (fence.Sanitize — it may carry an upstream's words) and cut to
// MaxRefusalRunes, or muster's own words when the plugin gave none.
func RefusalText(detail string) string {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return "the plugin gave no reason"
	}
	return fence.Cut(fence.Sanitize(detail), MaxRefusalRunes)
}

// RouteSendFailedHandBack is the hand-back a send_failed route owes: why its
// reply (draft draftID) is back.
func RouteSendFailedHandBack(draftID int64, detail string) string {
	return "its reply (draft " + strconv.FormatInt(draftID, 10) + ") was not sent: the plugin refused it, saying «" +
		RefusalText(detail) + "»"
}

// rowQuerier is what OtherReplyOut reads with: a transaction or the pool.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// OtherReplyOut reports whether a reply to message messageID other than draft
// draftID was SENT, or is approved and in flight (unclaimed, or claimed within
// its lease: lockMessage's in-flight rule). THE ONE PREDICATE for "the sender
// has, or is getting, another answer": refuseRouteDraftTx reads it, and its
// decision (Draft.Refusal) is what internal/api acts on.
func OtherReplyOut(ctx context.Context, q rowQuerier, messageID, draftID int64) (bool, error) {
	var out bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM plugin_reply_drafts
		WHERE message_id = $1 AND id <> $2
		  AND (status = 'sent' OR (status = 'approved' AND (claimed_at IS NULL OR lease_until > now()))))`,
		messageID, draftID).Scan(&out)
	return out, err
}

// DraftCheckWait is how long a draft awaits its plugin's check before it
// LAPSES (LapseDraftCheck): it then becomes an ordinary pending draft the
// owner may approve, and the owner is asked, told the plugin did not check
// it — so a plugin that stops checking never leaves a reply to expire unseen.
const DraftCheckWait = time.Hour

// DueDraftCheck is a draft whose check is past DraftCheckWait, and whose.
type DueDraftCheck struct{ ID, OwnerID int64 }

// DraftChecksDue is EVERY owner's drafts awaiting a check past
// DraftCheckWait (system scope; ids and owners only): each is then lapsed AS
// ITS OWNER (LapseDraftCheck), by internal/api's attention sweep.
func (s *PGStore) DraftChecksDue(ctx context.Context) ([]DueDraftCheck, error) {
	rows, err := s.pool.Query(authz.System(ctx), `SELECT id, owner_id FROM plugin_reply_drafts
		WHERE status = 'pending' AND check_state = 'awaiting' AND expires_at > now()
		  AND check_due_at <= now() ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (DueDraftCheck, error) {
		var d DueDraftCheck
		err := r.Scan(&d.ID, &d.OwnerID)
		return d, err
	})
}

// LapseDraftCheck ends draft id's wait for its plugin's check when it is still
// awaiting one past DraftCheckWait: check_state becomes CheckLapsed, and the
// draft is an ordinary pending draft the owner may approve. lapsed is false —
// and nothing changes — otherwise (a verdict came, it left pending, it is not
// yet due). A verdict after a lapse is ErrDraftState (CheckDraft): the owner
// decides it now.
func (s *PGStore) LapseDraftCheck(ctx context.Context, id int64) (Draft, bool, error) {
	var d Draft
	lapsed := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		cur, err := lockedDraft(ctx, tx, id)
		if err != nil && !errors.Is(err, ErrReplyInFlight) {
			return err
		}
		d = cur
		var due bool
		if err := tx.QueryRow(ctx, `SELECT coalesce(check_due_at <= now(), false) AND expires_at > now()
			FROM plugin_reply_drafts WHERE id = $1`, id).Scan(&due); err != nil {
			return err
		}
		if !cur.AwaitingCheck() || !due {
			return nil
		}
		if d, err = scanDraft(tx.QueryRow(ctx, `UPDATE plugin_reply_drafts SET check_state = 'lapsed', checked_at = now()
			WHERE id = $1 RETURNING `+draftCols, id)); err != nil {
			return err
		}
		lapsed = true
		return audit(ctx, tx, "plugin.draft.check_lapsed", DraftObject(id), map[string]any{
			"why": "the plugin did not check it within " + DraftCheckWait.String()})
	})
	return d, lapsed, err
}

// refuseRouteDraftTx is the route half of a refused draft d (FinishDraft's
// "failed", CheckDraft's "refused"), in the caller's transaction, which holds
// d's message and then d (the lock order every draft write takes; the route
// comes last). When d is the reply of a session route still relayed or
// fallback, that route ends send_failed, owing a hand-back, and is marked
// redraft — the hand-back goes back to its SESSION, with the plugin's reason —
// while fewer than MaxRouteRedrafts of the message's routes ended send_failed
// before it (and it went to a mirrored session, for a chief that still
// exists). A draft with no route, or whose route moved on, changes nothing here.
func refuseRouteDraftTx(ctx context.Context, tx pgx.Tx, d Draft, detail string) (RefusalOutcome, error) {
	if d.IsOutbound() {
		// No message, no route, no sibling reply: its officer is told.
		return RefusalOutbound, nil
	}
	// 🔴 ANOTHER REPLY TO THE MESSAGE WENT OUT, OR IS ON ITS WAY: the sender
	// has (or is getting) an answer, so this refusal is recorded on the draft
	// only. A route is left as it is — no send_failed, no hand-back — or a
	// redraft would tell the session "the sender has no answer yet" and its new
	// reply could reach the sender as a second one (an auto-approved relay).
	// The case: this draft's lease lapsed (unconfirmed), another was sent, and
	// this one's failure arrived late.
	if out, err := OtherReplyOut(ctx, tx, d.MessageID, d.ID); err != nil || out {
		return RefusalAnsweredElsewhere, err
	}
	if d.RouteID == nil {
		return RefusalNoRoute, nil
	}
	cur, err := lockRoute(ctx, tx, *d.RouteID)
	if errors.Is(err, ErrRouteNotFound) {
		return RefusalNoRoute, nil
	}
	if err != nil {
		return RefusalNone, err
	}
	if cur.DraftID == nil || *cur.DraftID != d.ID || (cur.State != RouteRelayed && cur.State != RouteFallback) {
		return RefusalNoRoute, nil
	}
	var before int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM session_routes WHERE plugin_message_id = $1 AND state = 'send_failed'`,
		cur.MessageID).Scan(&before); err != nil {
		return RefusalNone, err
	}
	redraft := before < MaxRouteRedrafts && cur.TargetKind == RouteTargetMirror && cur.AgentID != nil
	if _, err := tx.Exec(ctx, `UPDATE session_routes SET state = 'send_failed', error = $2, handback = $3, redraft = $4
		WHERE route_id = $1`, cur.RouteID, clampRouteError(RefusalText(detail)), clampHandBack(RouteSendFailedHandBack(d.ID, detail)),
		redraft); err != nil {
		return RefusalNone, err
	}
	out := RefusalRouteToChief
	if redraft {
		out = RefusalRedraft
	}
	return out, audit(ctx, tx, "session_route.send_failed", RouteObject(cur.RouteID), map[string]any{
		"from": string(cur.State), "draft_id": d.ID, "redraft": redraft, "refusals": before + 1})
}

// AwaitingCheck is one draft a plugin with CapDraftChecks is asked to check
// (DraftsAwaitingCheck): the identity a claim hands out, and the text.
type AwaitingCheck struct {
	DraftID           int64     `json:"draft_id"`
	MessageExternalID string    `json:"message_external_id"`
	Body              string    `json:"body"`
	CreatedAt         time.Time `json:"created_at"`
	// Kind is DraftKindReply or DraftKindOutbound; only an outbound entry
	// carries the fields below (AwaitingCheck.MarshalJSON).
	Kind   string         `json:"kind"`
	Venue  string         `json:"venue,omitempty"`
	Target OutboundTarget `json:"target"`
	Media  []CheckMedia   `json:"media"`
}

// MarshalJSON is the checks listing's wire form (docs/plugins.md).
func (a AwaitingCheck) MarshalJSON() ([]byte, error) {
	out := map[string]any{"draft_id": a.DraftID, "message_external_id": a.MessageExternalID, "body": a.Body,
		"created_at": a.CreatedAt, "kind": a.Kind}
	if a.Kind == DraftKindOutbound {
		out["venue"], out["target"], out["media"] = a.Venue, a.Target, a.Media
	}
	return json.Marshal(out)
}

// MaxChecksListed bounds one GET /plugin/v1/drafts/checks.
const MaxChecksListed = 20

// DraftsAwaitingCheck is plugin pluginID's pending, unexpired drafts awaiting
// its verdict (Draft.AwaitingCheck), oldest first, at most limit. Reading one
// leases nothing: a check sends nothing, so checking a draft twice is
// harmless, and the first verdict recorded is the one kept (CheckDraft).
func (s *PGStore) DraftsAwaitingCheck(ctx context.Context, pluginID int64, limit int) ([]AwaitingCheck, error) {
	if limit <= 0 || limit > MaxChecksListed {
		limit = MaxChecksListed
	}
	if err := s.sweep(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+prefixed("d", draftCols)+`, coalesce(m.external_id, '')
		  FROM plugin_reply_drafts d
		  LEFT JOIN plugin_messages m ON m.id = d.message_id AND m.owner_id = d.owner_id
		 WHERE coalesce(m.plugin_id, d.plugin_id) = $1 AND d.status = 'pending' AND d.check_state = 'awaiting' AND d.expires_at > now()
		 ORDER BY d.id LIMIT $2`, pluginID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AwaitingCheck, error) {
		var ext string
		d, err := scanDraftWith(r, &ext)
		a := AwaitingCheck{DraftID: d.ID, MessageExternalID: ext, Body: d.Body, CreatedAt: d.CreatedAt.UTC(), Kind: DraftKindReply}
		if d.IsOutbound() {
			a.Kind, a.Venue, a.Target, a.Media = DraftKindOutbound, d.VenueText(), d.Target(), checkMediaOf(d)
		}
		return a, err
	})
}

// DraftCheck is a plugin's verdict on a draft awaiting its check.
type DraftCheck struct {
	Sendable bool
	Detail   string
}

// CheckDraft records plugin pluginID's verdict on draft draftID, a draft of
// one of ITS messages awaiting the check (Draft.AwaitingCheck): sendable
// leaves it pending for the owner to decide; refused rejects it — it can never
// be approved — with the plugin's reason as its detail, and ends its session
// route send_failed (refuseRouteDraftTx), as a failed send does. The same
// verdict again answers the stored draft (fresh false); the other is
// ErrCheckConflict. A draft of another plugin is ErrNotFound; one not awaiting
// a check (none was asked, or it left pending first — superseded, expired,
// rejected by the owner) is ErrDraftState.
func (s *PGStore) CheckDraft(ctx context.Context, pluginID, draftID int64, v DraftCheck) (Draft, bool, error) {
	if err := (DraftResult{Detail: v.Detail}).Validate(); err != nil {
		return Draft{}, false, err
	}
	if err := s.sweep(ctx); err != nil {
		return Draft{}, false, err
	}
	want := CheckRefused
	if v.Sendable {
		want = CheckSendable
	}
	var d Draft
	fresh := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var mine bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM plugin_reply_drafts d
			  LEFT JOIN plugin_messages m ON m.id = d.message_id AND m.owner_id = d.owner_id
			 WHERE d.id = $1 AND coalesce(m.plugin_id, d.plugin_id) = $2)`, draftID, pluginID).Scan(&mine); err != nil {
			return err
		}
		if !mine {
			return ErrNotFound
		}
		// The message first, then the draft (lockedDraft's order, every draft
		// write's).
		cur, err := lockedDraft(ctx, tx, draftID)
		if err != nil && !errors.Is(err, ErrReplyInFlight) {
			return err
		}
		d = cur
		switch {
		case cur.CheckState == CheckSendable || cur.CheckState == CheckRefused:
			if cur.CheckState != want {
				return ErrCheckConflict
			}
			return nil // the same verdict again: the stored draft is the answer
		case !cur.AwaitingCheck():
			return ErrDraftState
		}
		if v.Sendable && cur.IsOutbound() {
			// 🔴 THE OWNER IS NEVER ASKED TO APPROVE A POST WHOSE MEDIA MUSTER
			// HAS NOT SEEN (outbound.go): sendable waits for every ref's bytes.
			if missing := cur.MissingMedia(); len(missing) > 0 {
				return MissingMediaError{Missing: missing}
			}
		}
		action, q, args := "plugin.draft.checked", `UPDATE plugin_reply_drafts SET check_state = 'sendable', checked_at = now()
			WHERE id = $1 RETURNING `+draftCols, []any{draftID}
		if !v.Sendable {
			action, q, args = "plugin.draft.check_refused", `UPDATE plugin_reply_drafts SET check_state = 'refused', checked_at = now(),
				status = 'rejected', decided_at = now(), detail = $2 WHERE id = $1 RETURNING `+draftCols, []any{draftID, v.Detail}
		}
		if d, err = scanDraft(tx.QueryRow(ctx, q, args...)); err != nil {
			return err
		}
		if err := audit(ctx, tx, action, DraftObject(draftID), map[string]any{"plugin_id": pluginID, "sendable": v.Sendable}); err != nil {
			return err
		}
		fresh = true
		if !v.Sendable {
			d.Refusal, err = refuseRouteDraftTx(ctx, tx, d, v.Detail)
			return err
		}
		return nil
	})
	return d, fresh && err == nil, err
}

// QueuedForOwner is ownerID's undelivered messages across the owner's LIVE
// plugins, oldest first, at most limit — what the claim route retries — less
// any a mail resume holds (resumeHolds: the resume delivers it itself).
func (s *PGStore) QueuedForOwner(ctx context.Context, ownerID int64, limit int) ([]Message, error) {
	if limit <= 0 || ownerID <= 0 {
		return []Message{}, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+prefixed("m", messageCols)+`
		  FROM plugin_messages m JOIN plugins p ON p.id = m.plugin_id
		 WHERE m.owner_id = $1 AND m.status = 'queued' AND p.revoked_at IS NULL AND NOT `+resumeHolds+`
		 ORDER BY m.id LIMIT $2`, ownerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// QueuedCounts is how many undelivered messages each of the principal's plugins
// has, by plugin id (a plugin with none is absent).
func (s *PGStore) QueuedCounts(ctx context.Context) (map[int64]int, error) {
	rows, err := s.pool.Query(ctx, `SELECT plugin_id, count(*) FROM plugin_messages WHERE status = 'queued' GROUP BY plugin_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// PendingDraft is a pending draft's identity and deadline: what the attention
// queue needs to withdraw its notification when it expires unseen.
type PendingDraft struct {
	ID, OwnerID, MessageID int64
	ExpiresAt              time.Time
}

// PendingDrafts is EVERY owner's pending drafts (system scope): read once by
// the attention sweep when a process starts, so a draft notified before a
// restart still has its notification withdrawn when it expires. Ids, owners
// and deadlines only — no body is read.
func (s *PGStore) PendingDrafts(ctx context.Context) ([]PendingDraft, error) {
	rows, err := s.pool.Query(authz.System(ctx), `SELECT id, owner_id, coalesce(message_id, 0), expires_at FROM plugin_reply_drafts WHERE status = 'pending'`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (PendingDraft, error) {
		var d PendingDraft
		err := r.Scan(&d.ID, &d.OwnerID, &d.MessageID, &d.ExpiresAt)
		return d, err
	})
}

// cutRunes keeps at most n runes of s.
func cutRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
