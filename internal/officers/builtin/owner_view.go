package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// WHAT AN OFFICER'S OWNER DEALS WITH ON ITS PAGE (internal/api
// officer_needs_you.go; docs/officers-builtin.md "An officer's page"):
//
//   - the REFUSALS that wait on the owner: a draft its plugin (or muster)
//     refused that the officer has not redrafted, and the owner's choice on
//     it — re-file it at a time (the officer files it again then, and the
//     owner approves the re-filed draft), or drop it;
//   - when the viewer last LOOKED at the officer (officer_seen, 0153), and how
//     many of its items finished since: the "N new" of the sidebar and the
//     Officers list.
// ---------------------------------------------------------------------------

// DraftRefusedKind is the kind of the item an officer gets when one of its
// outbound drafts ends unsent (refused, rejected, expired): where it redrafts.
// THE ONE SPELLING — internal/api's draftRefusedKind is this.
const DraftRefusedKind = "draft.refused"

// RefusalWindow is how far back a refusal is still offered to its owner: an
// older one is history (the "What it did" timeline), not a decision.
const RefusalWindow = 7 * 24 * time.Hour

// MaxRefileAhead bounds how far ahead a refusal may be re-filed.
const MaxRefileAhead = 30 * 24 * time.Hour

// Refusal is one draft.refused item waiting on its owner's choice.
type Refusal struct {
	Item      int64
	Status    string
	Outcome   string
	Summary   string
	NotBefore *time.Time
	Blocked   bool
	CreatedAt time.Time
	UpdatedAt time.Time
	// From the item's payload (internal/api draftEndFor): the draft that was
	// refused, where it was for, who refused it ("plugin" or "muster"), and
	// why — the plugin's or muster's own words, sanitised when filed.
	Draft      int64
	Venue      string
	TargetKind string
	By         string
	Detail     string
}

// refusalPayload is the part of a draft.refused item's payload a Refusal
// reads.
type refusalPayload struct {
	DraftID int64  `json:"draft_id"`
	Venue   string `json:"venue"`
	By      string `json:"by"`
	Detail  string `json:"detail"`
	Target  struct {
		Kind string `json:"kind"`
	} `json:"target"`
}

var (
	// ErrDecided: the owner already re-filed or dropped this refusal.
	ErrDecided = errors.New("builtin: the owner already decided this refusal")
	// ErrRedrafted: the officer already filed a new draft for this refusal.
	ErrRedrafted = errors.New("builtin: the officer already redrafted it")
)

// refusalAwaitsOwner is THE predicate for "a refusal waits on its owner", in
// SQL over officer_items aliased i: a draft.refused item whose refusal was
// the plugin's or muster's (an owner's own rejection is a decision already
// made), that no instance is working on, that the owner has not decided
// (owner_decision in its payload), and for which the officer has filed no
// new draft (a draft naming it as its item).
const refusalAwaitsOwner = `i.kind = '` + DraftRefusedKind + `'
	AND coalesce(i.payload->>'by', '') <> 'owner'
	AND i.status <> 'claimed'
	AND NOT (i.payload ? 'owner_decision')
	AND NOT EXISTS (SELECT 1 FROM plugin_reply_drafts d WHERE d.owner_id = i.owner_id AND d.officer_item_id = i.id)`

// RefusalsAwaitingOwner is owner's officer's refusals waiting on the owner
// (refusalAwaitsOwner) filed since since, newest first, at most 20.
func (s *Store) RefusalsAwaitingOwner(ctx context.Context, owner int64, officer string, since time.Time) ([]Refusal, error) {
	rows, err := s.pool.Query(ctx, `SELECT i.id, i.status, coalesce(i.outcome, ''), coalesce(i.summary, ''), i.payload, i.not_before, i.blocked, i.created_at, i.updated_at
		FROM officer_items i WHERE i.owner_id = $1 AND i.officer = $2 AND i.created_at >= $3 AND `+refusalAwaitsOwner+`
		ORDER BY i.id DESC LIMIT 20`, owner, officer, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Refusal
	for rows.Next() {
		var r Refusal
		var raw []byte
		if err := rows.Scan(&r.Item, &r.Status, &r.Outcome, &r.Summary, &raw, &r.NotBefore, &r.Blocked, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		var p refusalPayload
		_ = json.Unmarshal(raw, &p)
		r.Draft, r.Venue, r.By, r.Detail, r.TargetKind = p.DraftID, p.Venue, p.By, p.Detail, p.Target.Kind
		out = append(out, r)
	}
	return out, rows.Err()
}

// RefileNote is what a re-filed refusal's payload tells the officer
// (owner_note): file it again, and the owner decides again.
const RefileNote = "Your owner chose to re-file this refused draft now. File it again with officer_post_draft and this item's id: " +
	"the same text and media, unless the refusal's detail says what must change. Your owner approves it again before anything is sent."

// lockRefusal reads owner's officer's draft.refused item for update and
// refuses what neither choice may touch: none (ErrNotFound), already decided
// (ErrDecided), redrafted by the officer (ErrRedrafted), or held by an
// instance under a live lease (a 409 held).
func lockRefusal(ctx context.Context, tx pgx.Tx, owner int64, officer string, id int64, now time.Time) (itemRow, error) {
	it, err := scanItem(tx.QueryRow(ctx, `SELECT `+itemCols+` FROM officer_items WHERE owner_id = $1 AND officer = $2 AND id = $3 AND kind = $4 FOR UPDATE`,
		owner, officer, id, DraftRefusedKind))
	if errors.Is(err, pgx.ErrNoRows) {
		return it, ErrNotFound
	}
	if err != nil {
		return it, err
	}
	var marks struct {
		Decision string `json:"owner_decision"`
	}
	_ = json.Unmarshal(it.Payload, &marks)
	if marks.Decision != "" {
		return it, ErrDecided
	}
	var redrafted bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM plugin_reply_drafts WHERE owner_id = $1 AND officer_item_id = $2)`, owner, id).Scan(&redrafted); err != nil {
		return it, err
	}
	if redrafted {
		return it, ErrRedrafted
	}
	if it.live(now) {
		return it, refuse(http.StatusConflict, "held", "an instance is working on it now: wait for it", map[string]any{"held_by": it.ClaimedBy})
	}
	return it, nil
}

// RefileRefusal is the OWNER re-filing refusal id of owner's officer at at:
// the same work comes back to the officer then, as its owner's own item
// (officers.OriginOwner: a spent daily budget does not hold it back), with
// RefileNote — and the owner approves whatever it files, as every draft.
//
//   - still OPEN (the officer has not taken it): that item, its not_before
//     moved to at, so the officer takes it then and not before;
//   - FINISHED (the officer closed it, e.g. "do not re-file now"): a new
//     draft.refused item, its payload the old one's plus the note, not_before
//     at; the old item records which (refiled_as).
//
// Either way the refusal is marked decided (owner_decision "refile"), in the
// same transaction, so it leaves the owner's list and a second choice is
// ErrDecided. at is at most MaxRefileAhead from now (an earlier at is now).
// Answers the id of the item that carries the re-filed work.
func (s *Store) RefileRefusal(ctx context.Context, owner int64, officer string, id int64, at, now time.Time) (int64, error) {
	if at.After(now.Add(MaxRefileAhead)) {
		return 0, fmt.Errorf("%w: a refusal is re-filed at most 30 days ahead", ErrInvalid)
	}
	if at.Before(now) {
		at = now
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	it, err := lockRefusal(ctx, tx, owner, officer, id, now)
	if err != nil {
		return 0, err
	}
	mark := map[string]any{"owner_decision": "refile", "owner_refile_at": at.UTC(), "owner_note": RefileNote}
	carrier := it.ID
	if it.Status == officers.StatusOpen || it.Status == officers.StatusClaimed {
		raw, _ := json.Marshal(mark)
		if _, err := tx.Exec(ctx, `UPDATE officer_items SET not_before = $2, payload = payload || $3::jsonb, origin = $4, updated_at = $5 WHERE id = $1`,
			it.ID, at, raw, officers.OriginOwner, now); err != nil {
			return 0, classify(err)
		}
	} else {
		var payload map[string]any
		if err := json.Unmarshal(it.Payload, &payload); err != nil || payload == nil {
			payload = map[string]any{}
		}
		payload["owner_note"] = RefileNote
		payload["refiled_from"] = it.ID
		raw, _ := json.Marshal(payload)
		carrier, err = s.EnqueueTx(ctx, tx, owner, NewItem{Officer: officer, Kind: DraftRefusedKind, SubjectKey: it.SubjectKey, Payload: raw,
			NotBefore: &at, Origin: officers.OriginOwner}, now)
		if err != nil {
			return 0, classify(err)
		}
		mark["refiled_as"] = carrier
		raw, _ = json.Marshal(mark)
		if _, err := tx.Exec(ctx, `UPDATE officer_items SET payload = payload || $2::jsonb, updated_at = $3 WHERE id = $1`, it.ID, raw, now); err != nil {
			return 0, classify(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return carrier, nil
}

// DroppedSummary is the summary of a refusal its owner dropped while it was
// still open.
const DroppedSummary = "Dropped by your owner: not re-filed."

// DropRefusal is the OWNER dropping refusal id of owner's officer: marked
// decided (owner_decision "drop"); still open, it is finished no_action with
// DroppedSummary, so the officer never redrafts it.
func (s *Store) DropRefusal(ctx context.Context, owner int64, officer string, id int64, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	it, err := lockRefusal(ctx, tx, owner, officer, id, now)
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(map[string]any{"owner_decision": "drop"})
	if it.Status == officers.StatusOpen || it.Status == officers.StatusClaimed {
		_, err = tx.Exec(ctx, `UPDATE officer_items SET status = $2, outcome = 'no_action', summary = $3, claimed_by = NULL, lease_expires_at = NULL,
			payload = payload || $4::jsonb, updated_at = $5 WHERE id = $1`, it.ID, officers.StatusDone, DroppedSummary, raw, now)
	} else {
		_, err = tx.Exec(ctx, `UPDATE officer_items SET payload = payload || $2::jsonb, updated_at = $3 WHERE id = $1`, it.ID, raw, now)
	}
	if err != nil {
		return classify(err)
	}
	return tx.Commit(ctx)
}

// --- what the viewer has seen -------------------------------------------------

// UnseenWindow is how far back an officer the viewer has never opened counts
// its finished items as new.
const UnseenWindow = 7 * 24 * time.Hour

// finishedStatuses are the item statuses of a "What it did" entry: work the
// officer finished (done, with any outcome) or handed to a person.
var finishedStatuses = []string{officers.StatusDone, officers.StatusEscalated}

// MarkSeen records that ctx's principal (the viewer) looked at officer of
// officerOwner at at, answering when they had last looked before (nil: never).
// A look never moves the mark back.
func (s *Store) MarkSeen(ctx context.Context, officerOwner int64, officer string, at time.Time) (*time.Time, error) {
	var prev *time.Time
	err := s.pool.QueryRow(ctx, `WITH prev AS (SELECT seen_at FROM officer_seen
			WHERE owner_id = nullif(current_setting('muster.uid', true), '')::bigint AND officer_owner = $1 AND officer = $2),
		up AS (INSERT INTO officer_seen (officer_owner, officer, seen_at) VALUES ($1, $2, $3)
			ON CONFLICT (owner_id, officer_owner, officer) DO UPDATE SET seen_at = greatest(officer_seen.seen_at, EXCLUDED.seen_at))
		SELECT (SELECT seen_at FROM prev)`, officerOwner, officer, at).Scan(&prev)
	if err != nil {
		return nil, classify(err)
	}
	return prev, nil
}

// UnreadCounts is, in ONE query, how many items of each officer of owners
// finished after ctx's principal last looked at that officer (MarkSeen), or
// within UnseenWindow of now for an officer they never looked at. An officer
// absent from the map has none. Read under ctx's principal (row-level
// security: the items its teams' too, the looks only its own).
func (s *Store) UnreadCounts(ctx context.Context, owners []int64, now time.Time) (map[OfficerRef]int, error) {
	out := map[OfficerRef]int{}
	if len(owners) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT i.owner_id, i.officer, count(*) FROM officer_items i
		LEFT JOIN officer_seen v ON v.officer_owner = i.owner_id AND v.officer = i.officer
			AND v.owner_id = nullif(current_setting('muster.uid', true), '')::bigint
		WHERE i.owner_id = ANY($1::bigint[]) AND i.status = ANY($2::text[]) AND i.updated_at > coalesce(v.seen_at, $3)
		GROUP BY i.owner_id, i.officer`, owners, finishedStatuses, now.Add(-UnseenWindow))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r OfficerRef
		var n int
		if err := rows.Scan(&r.Owner, &r.Officer, &n); err != nil {
			return nil, err
		}
		out[r] = n
	}
	return out, rows.Err()
}
