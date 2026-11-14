package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// OUTREACH (migration 0131; docs/plugins.md "Outreach drafts", docs/
// officers-builtin.md "People and outreach"): an officer's outbound draft
// that reaches a PERSON — a DM {handle}, a follow {handle}, a like {ref}, or
// a reply on one of a person's recorded engagements.
//
//   - 🔴 THE FILING IS GATED (FileOutbound → OfficerLink.OutreachTx, which
//     internal/api wires to internal/officers/builtin OutreachGuardTx): a
//     do-not-contact, bot or hostile person, a dm/follow/like without a
//     qualifying because_ref of THAT person, a second outreach while one is
//     out, a second contact with no engagement of theirs in between, and an
//     outreach inside the cooldown that answers nothing newer, are refused in
//     the filing's transaction.
//   - 🔴 A TEXT IS APPROVED ON ITS EXACT TEXT (a dm, like a post or a reply:
//     the body's SHA-256). A FOLLOW OR A LIKE CARRIES NO TEXT and is approved
//     only in its day's OUTREACH BATCH (DecideOutreachBatch): one card, the
//     owner ticks entries, the approval binds the exact set of draft ids the
//     card showed (BatchSHA256), and every un-ticked entry is rejected "not
//     selected". An individual approval of a follow or a like is refused
//     (ErrBatchOnly).
//   - The person is re-checked at the approval and at the claim
//     (ContactBlockedTx): one who said "stop", blocked the owner or was
//     marked since is never reached.
//   - A sent outreach is recorded on the person in the result's transaction
//     (OfficerLink.OutreachSentTx).

// Outreach is one outreach as the officer store's gate reads it.
type Outreach struct {
	Officer, Venue string
	// Kind is the target kind (reply, dm, follow, like).
	Kind string
	// Handle is a dm's or a follow's person, or the person a like or a reply
	// names (NewOutbound.Contact).
	Handle string
	// TargetRef is a like's or a reply's upstream reference.
	TargetRef  string
	BecauseRef string
}

// OutreachResolved is who an outreach reaches: their contact id and handle as
// their contact stores it (nil and "" for a reply to no recorded person).
type OutreachResolved struct {
	Contact *int64
	Handle  string
}

// Bounds of an outreach draft (0131's CHECKs say the same).
const (
	// MaxHandleBytes bounds a person's handle.
	MaxHandleBytes = 100
	// MaxReasonRunes bounds an officer's one-line reason.
	MaxReasonRunes = 200
)

// IsBatchKind reports whether target kind k is decided in an outreach batch:
// a follow or a like, which carry no text.
func IsBatchKind(k string) bool { return k == TargetFollow || k == TargetLike }

// NeedsBecause reports whether target kind k must cite because_ref.
func NeedsBecause(k string) bool { return k == TargetDM || IsBatchKind(k) }

// Errors of an outreach decision.
var (
	// ErrBatchOnly: an individual approval of a follow or a like.
	ErrBatchOnly = errors.New("plugins: a follow or a like is approved in its outreach batch only")
	// ErrContactBlocked: the person is do-not-contact, a bot or hostile.
	ErrContactBlocked = errors.New("plugins: the person may not be contacted")
	// ErrBatchChanged: the batch's decidable entries are not the set the
	// decision names (batch_sha256).
	ErrBatchChanged = errors.New("plugins: the outreach batch is not the one the decision names")
)

// ContactBlockedError is ErrContactBlocked naming why (do_not_contact, bot,
// hostile) and, in a batch, which draft.
type ContactBlockedError struct {
	Why   string
	Draft int64
}

func (e ContactBlockedError) Error() string {
	if e.Draft > 0 {
		return fmt.Sprintf("%s: draft %d's person is %s", ErrContactBlocked, e.Draft, BlockedText(e.Why))
	}
	return ErrContactBlocked.Error() + ": the person is " + BlockedText(e.Why)
}

func (e ContactBlockedError) Is(target error) bool { return target == ErrContactBlocked }

// BlockedText words why a person may not be contacted.
func BlockedText(why string) string {
	switch why {
	case "do_not_contact":
		return "on the do-not-contact list"
	case "bot":
		return "marked a bot"
	case "hostile":
		return "marked hostile"
	case "unreadable":
		return "no longer on record (erased, or not yours)"
	}
	return why
}

// ContactBlockedDetail is an outreach draft's detail when the claim withdrew
// it because its person may no longer be contacted.
func ContactBlockedDetail(why string) string {
	return "the person is now " + BlockedText(why) + ": not sent"
}

// contactBlockedTx is why draft d's person may not be contacted now, or "".
func (s *PGStore) contactBlockedTx(ctx context.Context, tx pgx.Tx, d Draft) (string, error) {
	if d.ContactID == nil || s.officers == nil {
		return "", nil
	}
	return s.officers.ContactBlockedTx(ctx, tx, d.OwnerID, *d.ContactID)
}

// --- the outreach batch -----------------------------------------------------------------

// NotSelectedReason is the reason an un-ticked batch entry is rejected with.
const NotSelectedReason = "not selected"

// OutreachBatch is one officer's follows and likes filed on one UTC day,
// still pending: the Entries its owner may decide now (checked by their
// plugin and unexpired, oldest first), how many still await their plugin's
// check (Waiting: not yet on the card), and SHA256, the binding of exactly
// the Entries' ids a decision must name.
type OutreachBatch struct {
	ID      string  `json:"batch_id"`
	Officer string  `json:"officer"`
	Day     string  `json:"day"`
	Entries []Draft `json:"-"`
	Waiting int     `json:"waiting"`
	SHA256  string  `json:"batch_sha256"`
}

var batchIDRE = regexp.MustCompile(`^([a-z0-9]+(?:-[a-z0-9]+)*:[a-z0-9]+(?:-[a-z0-9]+)*)@([0-9]{4}-[0-9]{2}-[0-9]{2})$`)

// BatchID is officer's batch of UTC day ("2006-01-02"): "<officer>@<day>".
func BatchID(officer, day string) string { return officer + "@" + day }

// ParseBatchID splits a BatchID.
func ParseBatchID(id string) (officer, day string, ok bool) {
	m := batchIDRE.FindStringSubmatch(id)
	if m == nil {
		return "", "", false
	}
	if _, err := time.Parse("2006-01-02", m[2]); err != nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// BatchSHA256 is the binding of a batch's decidable draft ids: the SHA-256 of
// the ids in ascending order, comma-separated. Every entry is immutable
// (0121/0131's trigger), so its id names what the card showed.
func BatchSHA256(ids []int64) string {
	sorted := append([]int64(nil), ids...)
	slices.Sort(sorted)
	parts := make([]string, len(sorted))
	for i, id := range sorted {
		parts[i] = strconv.FormatInt(id, 10)
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, ",")))
	return hex.EncodeToString(sum[:])
}

// batchWhere selects the principal's pending follow and like drafts; $1 and
// $2 narrow it to an officer and a UTC day when the caller names them.
const batchWhere = `message_id IS NULL AND target_kind IN ('follow', 'like') AND status = 'pending' AND expires_at > now()`

// batchOf groups ds (pending follows and likes, oldest first) into batches.
func batchOf(ds []Draft) []OutreachBatch {
	var out []OutreachBatch
	at := map[string]int{}
	for _, d := range ds {
		if d.Officer == nil {
			continue
		}
		day := d.CreatedAt.UTC().Format("2006-01-02")
		id := BatchID(*d.Officer, day)
		i, ok := at[id]
		if !ok {
			i = len(out)
			at[id] = i
			out = append(out, OutreachBatch{ID: id, Officer: *d.Officer, Day: day})
		}
		if d.AwaitingCheck() {
			out[i].Waiting++
			continue
		}
		out[i].Entries = append(out[i].Entries, d)
	}
	for i := range out {
		ids := make([]int64, 0, len(out[i].Entries))
		for _, d := range out[i].Entries {
			ids = append(ids, d.ID)
		}
		out[i].SHA256 = BatchSHA256(ids)
	}
	return out
}

// OutreachBatches is the principal's open outreach batches, oldest day first
// (a batch with nothing decidable yet included: Waiting says why).
func (s *PGStore) OutreachBatches(ctx context.Context) ([]OutreachBatch, error) {
	if err := s.sweep(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+draftCols+` FROM plugin_reply_drafts WHERE `+batchWhere+` ORDER BY created_at, id LIMIT 500`)
	if err != nil {
		return nil, err
	}
	ds, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Draft, error) { return scanDraft(r) })
	if err != nil {
		return nil, err
	}
	return batchOf(ds), nil
}

// OutreachBatch is the principal's batch id (ErrNotFound when it holds no
// pending follow or like).
func (s *PGStore) OutreachBatch(ctx context.Context, id string) (OutreachBatch, error) {
	officer, day, ok := ParseBatchID(id)
	if !ok {
		return OutreachBatch{}, invalid("batch_id is <officer>@<YYYY-MM-DD>")
	}
	if err := s.sweep(ctx); err != nil {
		return OutreachBatch{}, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+draftCols+` FROM plugin_reply_drafts WHERE `+batchWhere+`
		AND officer = $1 AND (created_at AT TIME ZONE 'UTC')::date = $2::date ORDER BY created_at, id`, officer, day)
	if err != nil {
		return OutreachBatch{}, err
	}
	ds, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Draft, error) { return scanDraft(r) })
	if err != nil {
		return OutreachBatch{}, err
	}
	bs := batchOf(ds)
	if len(bs) == 0 {
		return OutreachBatch{}, ErrNotFound
	}
	return bs[0], nil
}

// BatchDecision is the owner's decision on an outreach batch.
type BatchDecision struct {
	// BatchSHA256 must be the batch's SHA256 as the card or
	// plugin_outbound_batch_get showed it (ErrBatchChanged otherwise).
	BatchSHA256 string
	// Approve are the ticked entries' draft ids: each one of the batch's
	// decidable entries. Every other decidable entry is rejected, reason
	// NotSelectedReason. Empty rejects them all.
	Approve []int64
	Via     DraftDecisionSource
}

// BatchDecided is what DecideOutreachBatch did.
type BatchDecided struct {
	Batch              string
	Approved, Rejected []Draft
}

// DecideOutreachBatch is THE decision on outreach batch id, in ONE
// transaction under every entry's row lock: the batch's decidable entries
// must be exactly the set dec.BatchSHA256 names — a follow or a like filed
// or checked after the owner looked is never decided unseen — each approved
// id one of them; a ticked entry whose person may no longer be contacted
// refuses the whole decision (ContactBlockedError naming it). Then the ticked
// entries are approved (the binding: an empty body and no media) and every
// other entry rejected NotSelectedReason, each audited with the batch.
func (s *PGStore) DecideOutreachBatch(ctx context.Context, id string, dec BatchDecision) (BatchDecided, error) {
	officer, day, ok := ParseBatchID(id)
	if !ok {
		return BatchDecided{}, invalid("batch_id is <officer>@<YYYY-MM-DD>")
	}
	if !sha256RE.MatchString(dec.BatchSHA256) {
		return BatchDecided{}, invalid("batch_sha256 is the batch's, 64 lower-case hex digits, as the card or plugin_outbound_batch_get showed it")
	}
	switch dec.Via.Via {
	case "", DraftViaBrowser:
		if dec.Via.Host != "" || dec.Via.Session != "" {
			return BatchDecided{}, invalid("only an MCP decision names a host and session")
		}
	case DraftViaMCP:
		if dec.Via.Host == "" || dec.Via.Session == "" {
			return BatchDecided{}, invalid("an MCP decision names its host and session")
		}
	default:
		return BatchDecided{}, invalid("unknown decision source %q", dec.Via.Via)
	}
	seen := map[int64]bool{}
	for _, a := range dec.Approve {
		if seen[a] {
			return BatchDecided{}, invalid("draft %d is ticked twice", a)
		}
		seen[a] = true
	}
	if err := s.sweep(ctx); err != nil {
		return BatchDecided{}, err
	}
	out := BatchDecided{Batch: id}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+draftCols+` FROM plugin_reply_drafts WHERE `+batchWhere+`
			AND officer = $1 AND (created_at AT TIME ZONE 'UTC')::date = $2::date ORDER BY id FOR UPDATE`, officer, day)
		if err != nil {
			return err
		}
		ds, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Draft, error) { return scanDraft(r) })
		if err != nil {
			return err
		}
		var decidable []Draft
		ids := []int64{}
		for _, d := range ds {
			if !d.AwaitingCheck() {
				decidable = append(decidable, d)
				ids = append(ids, d.ID)
			}
		}
		if len(decidable) == 0 {
			return ErrNotFound
		}
		if BatchSHA256(ids) != dec.BatchSHA256 {
			return ErrBatchChanged
		}
		for _, a := range dec.Approve {
			if !slices.Contains(ids, a) {
				return invalid("draft %d is not one of batch %s's entries", a, id)
			}
		}
		for _, d := range decidable {
			if !seen[d.ID] {
				continue
			}
			why, err := s.contactBlockedTx(ctx, tx, d)
			if err != nil {
				return err
			}
			if why != "" {
				return ContactBlockedError{Why: why, Draft: d.ID}
			}
		}
		empty := DraftBodySHA256("")
		for _, d := range decidable {
			q, action := `UPDATE plugin_reply_drafts SET status = 'rejected', decided_at = now(), decision_reason = $2 WHERE id = $1 RETURNING `+draftCols,
				"plugin.draft.rejected"
			args := []any{d.ID, NotSelectedReason}
			if seen[d.ID] {
				if d.Body != "" || len(d.MediaRefs) > 0 {
					// a follow or a like is never text (0131's shape): a second check
					return invalid("draft %d carries text or media; it cannot be approved in a batch", d.ID)
				}
				q, action = `UPDATE plugin_reply_drafts SET status = 'approved', decided_at = now(), approved_at = now(),
					approved_body_sha256 = $2, approved_media_sha256 = '{}' WHERE id = $1 RETURNING `+draftCols, "plugin.draft.approved"
				args = []any{d.ID, empty}
			}
			nd, err := scanDraft(tx.QueryRow(ctx, q, args...))
			if err != nil {
				return err
			}
			if seen[d.ID] {
				out.Approved = append(out.Approved, nd)
			} else {
				out.Rejected = append(out.Rejected, nd)
			}
			detail := map[string]any{"batch": id}
			if dec.Via.Via != "" {
				detail["decided_via"] = dec.Via.String()
			}
			if !seen[d.ID] {
				detail["reason"] = NotSelectedReason
			}
			if err := audit(ctx, tx, action, DraftObject(d.ID), detail); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return BatchDecided{}, err
	}
	return out, nil
}
