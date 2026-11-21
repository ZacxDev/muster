package plugins

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// RETRY CHAINS (migration 0175; docs/plugins.md "Retries"): the owner approves
// an outbound draft ONCE, and a send that failed for a cause that clears by
// itself is tried again without asking them again.
//
//	approved ──claim──▶ claimed ──result ok:false, nothing_sent, transient──▶ failed
//	                                                                     │
//	                                    successor: approved, not_before ◀┘
//
// 🔴 NO CLAIM IS EVER CLEARED AND NO ROW IS RE-OFFERED. The failed row is
// settled `failed` exactly as before — `claimed_at IS NULL` stays the
// at-most-once rule (drafts.go's header). What goes again is a NEW row, the
// SUCCESSOR: the same owner, plugin, venue, target, body, media, officer and
// item, born `approved` with the root's approved hashes (the owner's one
// approval covers it: the bytes are identical), retry_of naming the chain's
// ROOT, attempt n+1, not_before now + RetryBackoff(n). The claim skips it
// until then (ClaimDraft), and re-checks its binding like any other
// (BindingHolds) before it hands it out. 0175's trigger refuses, in Postgres,
// a successor that is not exactly that, or that follows anything but a
// failed attempt whose plugin asserted nothing was sent.
//
// A successor is filed ONLY when the result asserted nothing_sent AND
// transient (DraftResult), for an outbound draft, within its lease, and only
// while its backoff ends before the chain's expiry (the ROOT's expires_at,
// which every successor copies): past it the chain ends with that failure
// (RetryDecision.EndedAtExpiry). An old plugin sends neither flag, so its
// results are recorded exactly as before.
//
// ONE LIVE ATTEMPT PER CHAIN: at most one successor is pending or approved
// (0175's plugin_reply_drafts_retry_one_live), and one successor per attempt
// number (plugin_reply_drafts_retry_attempt).
//
// THE OFFICER'S ITEM SETTLES ONCE, with the chain's final outcome: while a
// successor is approved the item is not settled (builtin.DraftEndedTx leaves
// an item alone while another of its drafts is pending or approved, and
// internal/api files no draft.refused follow-up for a failure that has a
// successor); the last attempt's end — posted, failed for good, expired —
// settles it as any draft's does.
//
// The owner may ALSO retry by hand (RetryDraft: plugin_draft_retry, or the
// Asks card's Retry): the same successor, not_before now, allowed only when
// the chain's latest attempt failed with nothing_sent and the chain has not
// expired.

// MaxAutoAttempts caps the AUTOMATIC attempts of one budget of a chain: the
// first and five retries after it (1, 5, 15, 30 and 60 minutes). After the
// sixth fails the chain stops retrying on its own (RetryDecision.Exhausted)
// — hourly attempts against a venue's security wall for days would risk the
// account — and the owner is told. A manual retry (RetryDraft) starts a new
// budget of MaxAutoAttempts from the attempt it files (Draft.AutoFrom).
const MaxAutoAttempts = 6

// autoAttempts is how many attempts d's current automatic budget has made,
// d included.
func (d Draft) autoAttempts() int {
	from := d.AutoFrom
	if from < 1 {
		from = 1
	}
	return d.Attempt - from + 1
}

// RetryBackoff is how long after the n-th attempt of a budget failed its
// successor may be claimed: 1 minute, 5, 15, 30, then 60 (FinishDraft files
// none after the MaxAutoAttempts-th).
func RetryBackoff(attempt int) time.Duration {
	switch attempt {
	case 1:
		return time.Minute
	case 2:
		return 5 * time.Minute
	case 3:
		return 15 * time.Minute
	case 4:
		return 30 * time.Minute
	}
	return time.Hour
}

// RetryOwnerPushAttempt is the attempt whose failure pushes the owner once,
// while its chain goes on retrying (internal/api): the first two failures are
// quiet, the third is worth a look ("X wants a security check").
const RetryOwnerPushAttempt = 3

// retryAuto is the audit row's `via` for a successor the result filed.
const retryAuto = "auto"

// RetryRefusal is why a manual retry (RetryDraft) was refused; it Is
// ErrRetryRefused.
type RetryRefusal struct{ Code, Why string }

func (e RetryRefusal) Error() string { return "plugins: retry refused: " + e.Why }

func (e RetryRefusal) Is(target error) bool { return target == ErrRetryRefused }

// ErrRetryRefused is every RetryRefusal.
var ErrRetryRefused = errors.New("plugins: retry refused")

// The RetryRefusal codes.
const (
	RetryNotOutbound = "not_outbound"
	RetryLive        = "live"
	RetrySent        = "sent"
	RetryRejected    = "rejected"
	RetryExpired     = "expired"
	RetryUnknown     = "unknown_outcome"
	RetryNotFailed   = "not_failed"
	RetryUnbound     = "binding"
	RetryRedrafted   = "redrafted"
)

// ErrRedrafted is OfficerLink.RetryTakeOverTx's refusal: the chain's officer
// has taken up its hand-back (RetryDraft).
var ErrRedrafted = errors.New("plugins: the officer has taken up this draft's hand-back")

// fileRetryTx files failed draft's successor inside tx, claimable after
// `after`, audited with via. It runs under a SAVEPOINT: a database refusal
// rolls back the successor alone (Err), never the failure the caller is
// recording. EndedAtExpiry when the backoff would end at or past the chain's
// expiry.
//
// manual: the owner's retry, which starts a new automatic budget at the
// attempt it files (auto_from); an automatic one keeps its predecessor's.
func (s *PGStore) fileRetryTx(ctx context.Context, tx pgx.Tx, failed Draft, after time.Duration, manual bool, via string) (RetryDecision, error) {
	switch {
	case failed.AgentID == nil:
		// 0016's owner trigger requires the writer's agent row.
		return RetryDecision{Err: "the agent that wrote it is gone"}, errors.New("the agent that wrote it is gone")
	case !failed.BindingHolds():
		return RetryDecision{Err: BindingBrokenDetail}, errors.New(BindingBrokenDetail)
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return RetryDecision{Err: err.Error()}, err
	}
	next, err := scanDraft(sp.QueryRow(ctx, `
		INSERT INTO plugin_reply_drafts (owner_id, message_id, agent_id, agent_name, body, status, expires_at, decided_at, approved_at,
		                                 check_state, checked_at, plugin_id, venue, target_kind, target_ref, media_refs, media, officer,
		                                 officer_item_id, approved_body_sha256, approved_media_sha256, because_ref, contact_id, outreach_reason,
		                                 retry_of, attempt, not_before, retry_last_detail, auto_from)
		SELECT f.owner_id, NULL, f.agent_id, f.agent_name, f.body, 'approved', f.expires_at, f.decided_at, f.approved_at,
		       f.check_state, f.checked_at, f.plugin_id, f.venue, f.target_kind, f.target_ref, f.media_refs, f.media, f.officer,
		       f.officer_item_id, f.approved_body_sha256, f.approved_media_sha256, f.because_ref, f.contact_id, f.outreach_reason,
		       coalesce(f.retry_of, f.id), f.attempt + 1, now() + make_interval(secs => $2), f.detail,
		       CASE WHEN $3 THEN f.attempt + 1 ELSE f.auto_from END
		  FROM plugin_reply_drafts f
		 WHERE f.id = $1 AND f.status = 'failed' AND f.nothing_sent AND now() + make_interval(secs => $2) < f.expires_at
		RETURNING `+draftCols, failed.ID, after.Seconds(), manual))
	if errors.Is(err, pgx.ErrNoRows) {
		_ = sp.Rollback(ctx)
		return RetryDecision{EndedAtExpiry: true}, nil
	}
	if err == nil && !next.BindingHolds() {
		err = errors.New(BindingBrokenDetail)
	}
	if err == nil {
		err = audit(ctx, sp, "plugin.draft.retry_scheduled", DraftObject(next.ID), map[string]any{
			"retry_of": next.ChainRoot(), "after_draft": failed.ID, "attempt": next.Attempt, "not_before": next.NotBefore, "via": via,
			"auto_from": next.AutoFrom})
	}
	if err == nil {
		err = sp.Commit(ctx)
	}
	if err != nil {
		_ = sp.Rollback(ctx)
		return RetryDecision{Err: err.Error()}, err
	}
	return RetryDecision{Next: &next}, nil
}

// RetryDraft is the owner's MANUAL retry of outbound draft id's chain
// (plugin_draft_retry over MCP, or the Asks card's Retry), recorded on its
// audit row as made via via: a successor of the chain's LATEST attempt,
// claimable now. It relays the owner's word and re-sends exactly the bytes
// they already approved, so it needs no new approval. Refused (RetryRefusal)
// unless that attempt FAILED with nothing_sent and the chain has not expired;
// so for an unknown outcome (a failure without nothing_sent, an unconfirmed
// claim), a sent, rejected, pending or expired draft, a reply draft, and
// while an attempt of the chain is still approved (ONE LIVE ATTEMPT).
func (s *PGStore) RetryDraft(ctx context.Context, id int64, via DraftDecisionSource) (Draft, error) {
	switch via.Via {
	case "", DraftViaBrowser:
		if via.Host != "" || via.Session != "" {
			return Draft{}, invalid("only an MCP retry names a host and session")
		}
	case DraftViaMCP:
		if via.Host == "" || via.Session == "" {
			return Draft{}, invalid("an MCP retry names its host and session")
		}
	default:
		return Draft{}, invalid("unknown retry source %q", via.Via)
	}
	if err := s.sweep(ctx); err != nil {
		return Draft{}, err
	}
	var out Draft
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		cur, err := scanDraft(tx.QueryRow(ctx, `SELECT `+draftCols+` FROM plugin_reply_drafts WHERE id = $1`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		out = cur
		if !cur.IsOutbound() {
			return RetryRefusal{RetryNotOutbound, "only an officer's outbound draft is retried; a reply draft is answered by a new one"}
		}
		latest := func(lock string) (Draft, error) {
			return scanDraft(tx.QueryRow(ctx, `SELECT `+draftCols+` FROM plugin_reply_drafts WHERE id = $1 OR retry_of = $1
				ORDER BY attempt DESC LIMIT 1 `+lock, cur.ChainRoot()))
		}
		// The chain's latest attempt, under its row lock (a result being
		// recorded for it holds the same lock); then read again, in a new
		// statement, for a successor that result may have filed meanwhile.
		locked, err := latest("FOR UPDATE")
		if err != nil {
			return err
		}
		last, err := latest("")
		if err != nil {
			return err
		}
		out = last
		if last.ID != locked.ID {
			return RetryRefusal{RetryLive, "an attempt of this draft is already on its way"}
		}
		switch {
		case last.Status == DraftApproved:
			return RetryRefusal{RetryLive, "an attempt of this draft is already on its way; it is retried only once it has failed"}
		case last.Status == DraftSent:
			return RetryRefusal{RetrySent, "it was sent"}
		case last.Status == DraftRejected:
			return RetryRefusal{RetryRejected, "it was rejected"}
		case last.Status == DraftExpired, last.Status == DraftFailed && !last.ExpiresAt.After(time.Now()):
			return RetryRefusal{RetryExpired, "it expired: it was not sent within 7 days of being written"}
		case last.Status == DraftUnconfirmed, last.Status == DraftFailed && !last.NothingSent:
			return RetryRefusal{RetryUnknown, "its plugin did not say that nothing was sent, so it may have been posted: retrying it could post it twice"}
		case last.Status != DraftFailed:
			return RetryRefusal{RetryNotFailed, "it is " + string(last.Status) + ", not a failed send"}
		case !last.BindingHolds():
			return RetryRefusal{RetryUnbound, BindingBrokenDetail}
		}
		// 🔴 A CHAIN HANDED BACK TO ITS OFFICER IS NOT RETRIED ONCE THE
		// OFFICER HAS TAKEN IT UP (OfficerLink.RetryTakeOverTx: its
		// draft.refused follow-up is claimed or finished, or a newer draft
		// was filed for its item): the officer may have redrafted it, and
		// a retry would post it twice. Otherwise the open follow-up is
		// closed here, in this transaction, so it cannot be redrafted after.
		if s.officers != nil && last.Officer != nil {
			if err := s.officers.RetryTakeOverTx(ctx, tx, last); errors.Is(err, ErrRedrafted) {
				return RetryRefusal{RetryRedrafted, "it went back to its officer, which has taken it up (it may have written a new draft): retrying it could post it twice"}
			} else if err != nil {
				return err
			}
		}
		dec, cause := s.fileRetryTx(ctx, tx, last, 0, true, via.String())
		var pg *pgconn.PgError
		switch {
		case dec.EndedAtExpiry:
			return RetryRefusal{RetryExpired, "it expired: it was not sent within 7 days of being written"}
		case errors.As(cause, &pg) && pg.Code == "23505":
			return RetryRefusal{RetryLive, "an attempt of this draft is already on its way"}
		case cause != nil:
			return fmt.Errorf("plugins: retry draft %d: %w", last.ID, cause)
		}
		out = *dec.Next
		return nil
	})
	return out, err
}
