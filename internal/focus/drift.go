package focus

import (
	"context"
	"time"
	"unicode/utf8"
)

// DRIFT (docs/focus.md#when-the-work-moves-on): a session's name is frozen,
// but its work can move on. When its focus has stopped being about its name
// for long enough, muster OFFERS a rename to the current focus title — it
// never renames on its own (the name rule's guarantee). The offer is decided
// here, deterministically, from the session's focus history and its name.
//
// The constants, and why:
//
//   - DriftMinTitles: the newest DriftMinTitles history rows must ALL have
//     diverged from the name. A history row is one focus change (a refresh
//     whose title or description moved), so this is "N consecutive
//     refreshes that changed something" — one off-topic detour does not
//     count; three in a row is a new job. Three, not two: a session that
//     answers one side question and comes back writes two.
//   - DriftMinAge: and the OLDEST of those rows was written at least half an
//     hour ago, so the divergence is sustained, not a burst of refreshes in
//     one busy minute. Thirty minutes is two default refresh intervals
//     (DefaultInterval).
//   - DriftMaxOverlap: a title has diverged from the name when fewer than
//     half of the name's content words (driftWords: no filler or opener
//     word, a crude stem) appear in it. "Billing migration" vs "Fixing the
//     billing webhook" shares billing (1 of 2): not diverged. vs "Fixing
//     the Stripe webhook retry": 0 of 2, diverged.
const (
	DriftMinTitles  = 3
	DriftMinAge     = 30 * time.Minute
	DriftMaxOverlap = 0.5
)

// driftWords is s's content words, crudely stemmed: lower-cased words
// (words), less the filler and opener vocabularies, each cut to its stem
// (driftStem), so "migrating" and "migration" meet.
func driftWords(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range words(s) {
		if fillerWords[w] || openerWords[w] {
			continue
		}
		out[driftStem(w)] = true
	}
	return out
}

// driftStem is w's first driftStemRunes letters (all of it when shorter): a
// stem crude enough that "migration", "migrating" and "migrate" meet, and
// "billing" and "bills" — good enough to tell "the same job" from "another
// job", and no better. It is a threshold's input, not a search index.
func driftStem(w string) string {
	if r := []rune(w); len(r) > driftStemRunes {
		return string(r[:driftStemRunes])
	}
	return w
}

// driftStemRunes is driftStem's prefix length.
const driftStemRunes = 4

// Diverged reports whether title is no longer about name: fewer than
// DriftMaxOverlap of name's content words are in title. A name with no
// content word never diverges (there is nothing to measure against).
func Diverged(name, title string) bool {
	nw := driftWords(name)
	if len(nw) == 0 {
		return false
	}
	tw := driftWords(title)
	shared := 0
	for w := range nw {
		if tw[w] {
			shared++
		}
	}
	return float64(shared) < DriftMaxOverlap*float64(len(nw))
}

// DriftOffer is the rename a session's drift offers: the title to rename it
// to (its current focus title), "" for no offer. history is the session's
// newest history rows, newest first (at least DriftMinTitles of them to
// offer anything); dismissed is the focus title the owner dismissed an offer
// at ("" for none). The offer is made when:
//
//   - the newest DriftMinTitles rows ALL diverged from name (Diverged);
//   - the oldest of them was written at least DriftMinAge before now;
//   - the current title is one the bar would accept as a name
//     (NameableTitle) and is not name itself;
//   - and the owner has not dismissed an offer at this very title — a
//     dismissal holds until the focus title changes again.
func DriftOffer(name string, history []HistoryEntry, dismissed string, now time.Time) string {
	if name == "" || len(history) < DriftMinTitles {
		return ""
	}
	recent := history[:DriftMinTitles]
	for _, e := range recent {
		if !Diverged(name, e.Title) {
			return ""
		}
	}
	if now.Sub(recent[DriftMinTitles-1].CreatedAt) < DriftMinAge {
		return ""
	}
	cur := recent[0].Title
	if !NameableTitle(cur) || sameTitle(cur, name) || (dismissed != "" && dismissed == cur) {
		return ""
	}
	return cur
}

// DismissDrift records the caller's "not now" on sub's rename offer at focus
// title title (DriftOffer then holds it until the title changes). A subject
// with no row has no offer to dismiss: nothing is written.
func (s *PGStore) DismissDrift(ctx context.Context, sub Subject, title string) error {
	if err := sub.Valid(); err != nil {
		return err
	}
	if title == "" || utf8.RuneCountInString(title) > MaxDriftTitleRunes {
		return ErrBadName
	}
	if _, err := s.pool.Exec(ctx, `UPDATE agent_focus SET drift_dismissed = $3
		WHERE subject_kind = $1 AND subject_key = $2`, string(sub.Kind), sub.Key, title); err != nil {
		return errf("dismiss drift: %w", err)
	}
	return nil
}

// MaxDriftTitleRunes bounds a dismissed title: the stored title's own bound
// (agent_focus_title_cap, migration 0089's check).
const MaxDriftTitleRunes = 80
