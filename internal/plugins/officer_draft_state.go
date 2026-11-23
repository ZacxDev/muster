package plugins

import (
	"context"
	"strconv"
	"time"
)

// AN OFFICER'S OUTBOUND DRAFTS AS MUSTER RECORDS THEM NOW, for the instance
// that just claimed an item (internal/api officerDraftState). A builtin
// officer can resume one long conversation across wakes; what it remembers of
// an earlier draft ("still awaiting approval") can be stale by the time the
// next item arrives — the draft may have been refused seconds after it was
// filed. So the facts travel WITH the work: the claim answer carries this
// read, and the officer decides from it rather than from its memory.

// DraftStateLiveStatuses are the statuses of an outbound draft that can still
// go out: pending (awaiting its plugin's check or its owner's decision) and
// approved (waiting for its plugin's claim, claimed and being posted under a
// lease, or a retry chain's queued next attempt, which is filed approved:
// draft_retry.go).
var DraftStateLiveStatuses = []DraftStatus{DraftPending, DraftApproved}

// DraftStateEndedStatuses are the ENDED statuses reported: the recent
// refusals — rejected (by its owner, by its plugin's check, or withdrawn at
// its claim) and failed (its plugin's send reported failure). Sent,
// unconfirmed, superseded and expired drafts are not reported: the question
// the snapshot answers is "was this refused, and why?" (an operator
// decision).
var DraftStateEndedStatuses = []DraftStatus{DraftRejected, DraftFailed}

const (
	// DraftStateWindow is how far back an ENDED draft is still reported.
	DraftStateWindow = 24 * time.Hour
	// DraftStateMaxEnded bounds the ended drafts reported, newest first.
	DraftStateMaxEnded = 20
	// DraftStateMaxLive bounds the live drafts reported, newest first. Far
	// above what one officer can have out (MaxPendingDraftsPerAgent pending,
	// plus its approved ones), so in practice every live draft is listed;
	// DraftState says when it is not.
	DraftStateMaxLive = 60
)

// DraftState is officer's outbound drafts of owner ownerID, as read at
// ReadAt: every live one (up to DraftStateMaxLive) and the rejected or failed
// ones that ended within DraftStateWindow (up to DraftStateMaxEnded), each
// newest first.
type DraftState struct {
	ReadAt time.Time
	Live   []Draft
	Ended  []Draft
	// LiveTruncated, EndedTruncated: more rows matched than were returned.
	LiveTruncated, EndedTruncated bool
}

// draftEndedAt is when an ended draft ended, in SQL: its result's time, else
// its decision's (a rejection, an expiry, a supersession), else its filing.
const draftEndedAt = `coalesce(finished_at, decided_at, created_at)`

// OfficerDraftState reads officer's outbound drafts of owner ownerID
// (DraftState) as the principal in ctx — the officer's owner: row-level
// security admits that owner's rows only, and the query keeps to the one
// officer. The sweep runs first, so a draft past its expiry reads expired,
// not pending.
func (s *PGStore) OfficerDraftState(ctx context.Context, ownerID int64, officer string, now time.Time) (DraftState, error) {
	if err := s.sweep(ctx); err != nil {
		return DraftState{}, err
	}
	live, ended := statusArg(DraftStateLiveStatuses), statusArg(DraftStateEndedStatuses)
	out := DraftState{ReadAt: now}
	read := func(where, order string, max int, args ...any) ([]Draft, bool, error) {
		rows, err := s.pool.Query(ctx, `SELECT `+draftCols+` FROM plugin_reply_drafts
			WHERE owner_id = $1 AND officer = $2 AND message_id IS NULL AND `+where+`
			ORDER BY `+order+` LIMIT `+strconv.Itoa(max+1), append([]any{ownerID, officer}, args...)...)
		if err != nil {
			return nil, false, err
		}
		defer rows.Close()
		var ds []Draft
		for rows.Next() {
			d, err := scanDraft(rows)
			if err != nil {
				return nil, false, err
			}
			ds = append(ds, d)
		}
		if err := rows.Err(); err != nil {
			return nil, false, err
		}
		if len(ds) > max {
			return ds[:max], true, nil
		}
		return ds, false, nil
	}
	var err error
	if out.Live, out.LiveTruncated, err = read(`status = ANY($3::text[])`, `id DESC`, DraftStateMaxLive, live); err != nil {
		return DraftState{}, err
	}
	if out.Ended, out.EndedTruncated, err = read(`status = ANY($3::text[]) AND `+draftEndedAt+` >= $4`, draftEndedAt+` DESC, id DESC`, DraftStateMaxEnded,
		ended, now.Add(-DraftStateWindow)); err != nil {
		return DraftState{}, err
	}
	return out, nil
}

// statusArg is ss as a query argument.
func statusArg(ss []DraftStatus) []string {
	out := make([]string, len(ss))
	for i, st := range ss {
		out[i] = string(st)
	}
	return out
}
