package officers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// ---------------------------------------------------------------------------
// THE STALE-DRAFT AGE-OUT (docs/officers.md "The open-pull-request queue").
//
// A draft officer's open pull requests count against its open cap
// (MaxOpenPRsPerRepo) until a human closes or merges them, so a draft nobody
// looks at holds a slot for ever and the queue behind it never drains. With a
// `stale_drafts` block whose `enabled` is true, the draft sweep closes each of
// the officer's own DRAFT pull requests that has had no activity (GitHub's
// updated_at: a push, a comment, a review, an edit) for `days` days, after
// posting a comment that says why and that its branch is kept.
//
// 🔴 OFF BY DEFAULT, AND AN OUTWARD ACTION. Closing a pull request and
// commenting on it are writes on GitHub that people see. With no block, or
// `enabled: false`, muster never closes an idle draft (the cap then waits for
// a human, as before). Draft stage only: another stage's block is refused,
// not ignored.
// ---------------------------------------------------------------------------

// Bounds of a stale_drafts block.
const (
	DefaultStaleDraftDays = 3
	maxStaleDraftDays     = 90
)

// StaleDrafts is a draft officer's age-out policy (its `stale_drafts` block,
// enabled).
type StaleDrafts struct {
	// After is how long a draft may go without activity before the sweep
	// closes it: Days whole days.
	Days int
}

// After is the idle time past which a draft is closed.
func (s *StaleDrafts) After() time.Duration { return time.Duration(s.Days) * 24 * time.Hour }

type declaredStaleDrafts struct {
	Enabled bool `json:"enabled"`
	Days    int  `json:"days"`
}

// parseStaleDrafts reads a `stale_drafts` block of a ship block of stage:
// nil when it is absent or not enabled.
func parseStaleDrafts(stage string, raw json.RawMessage) (*StaleDrafts, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if stage != StageDraft {
		return nil, fmt.Errorf("ship: stale_drafts is enforced by the %s stage only; stage %s would ignore it — remove it", StageDraft, stage)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var d declaredStaleDrafts
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("ship: stale_drafts: %w", err)
	}
	days := d.Days
	if days == 0 {
		days = DefaultStaleDraftDays
	}
	if days < 1 || days > maxStaleDraftDays {
		return nil, fmt.Errorf("ship: stale_drafts.days %d is not 1..%d", d.Days, maxStaleDraftDays)
	}
	if !d.Enabled {
		return nil, nil
	}
	return &StaleDrafts{Days: days}, nil
}
