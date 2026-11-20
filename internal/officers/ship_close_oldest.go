package officers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// ---------------------------------------------------------------------------
// THE CAP-TRIGGERED CLOSE OF THE OLDEST IDLE DRAFT (docs/officers.md "The
// open-pull-request queue").
//
// A draft officer's open pull requests count against its open cap
// (MaxOpenPRsPerRepo) until a human closes or merges them, so a draft nobody
// looks at holds a slot and the queue behind it never drains. When a create
// is queued BECAUSE the cap is full, muster closes the officer's ONE oldest
// draft in that repository — by last activity (GitHub's updated_at: a push, a
// comment, a review, an edit) — if it has been idle IdleDays or more, with a
// comment, and the freed slot drains the queue at once. Only the officer's
// own drafts (internal/officership's ownership rule, as its rate limits count
// them), never one marked ready for review.
//
// ON BY DEFAULT for the draft stage (IdleDays DefaultIdleDraftDays). A
// `close_oldest_draft` block tunes it: `{"enabled": false}` turns it off,
// `idle_days` (1..90) moves the bound. Any other stage refuses the block.
// ---------------------------------------------------------------------------

// Bounds of a close_oldest_draft block.
const (
	DefaultIdleDraftDays = 2
	maxIdleDraftDays     = 90
)

// CloseOldestDraft is a draft officer's cap-triggered close, enabled.
type CloseOldestDraft struct {
	// IdleDays is how many whole days without activity the oldest draft must
	// have before it is closed.
	IdleDays int
}

// After is the idle time at or past which the oldest draft is closed.
func (c *CloseOldestDraft) After() time.Duration { return time.Duration(c.IdleDays) * 24 * time.Hour }

type declaredCloseOldestDraft struct {
	Enabled  *bool `json:"enabled"`
	IdleDays int   `json:"idle_days"`
}

// parseCloseOldestDraft reads a `close_oldest_draft` block of a ship block of
// stage: the draft stage's default (on, DefaultIdleDraftDays) when absent, nil
// when it says enabled false, and nil on every other stage (whose block is
// refused).
func parseCloseOldestDraft(stage string, raw json.RawMessage) (*CloseOldestDraft, error) {
	present := len(raw) > 0 && string(raw) != "null"
	if stage != StageDraft {
		if present {
			return nil, fmt.Errorf("ship: close_oldest_draft is enforced by the %s stage only; stage %s would ignore it — remove it", StageDraft, stage)
		}
		return nil, nil
	}
	if !present {
		return &CloseOldestDraft{IdleDays: DefaultIdleDraftDays}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var d declaredCloseOldestDraft
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("ship: close_oldest_draft: %w", err)
	}
	days := d.IdleDays
	if days == 0 {
		days = DefaultIdleDraftDays
	}
	if days < 1 || days > maxIdleDraftDays {
		return nil, fmt.Errorf("ship: close_oldest_draft.idle_days %d is not 1..%d", d.IdleDays, maxIdleDraftDays)
	}
	if d.Enabled != nil && !*d.Enabled {
		return nil, nil
	}
	return &CloseOldestDraft{IdleDays: days}, nil
}
