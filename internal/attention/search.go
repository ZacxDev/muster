package attention

import (
	"context"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/db"
)

// SEARCHING THE OWNER'S ASKS (the Asks page's search box, docs/attention.md
// "Searching your asks"): every ask the caller OWNS — open, answered,
// dismissed, expired, notices too — by its id, by the draft a step-up card
// confirms, or by a piece of its text.
//
// 🔴 THE CALLER'S OWN ASKS, NAMED IN THE QUERY. attention_asks is team-shared
// (0127): row-level security lets a team's member read the team's asks too,
// so "what RLS lets me read" is not "mine". Search names the owner itself
// (owner_id = muster.uid), as AnswersForTask and the open-ask cap do, and
// leaves out a question a session put to the chief (ToChief: no page of the
// owner's draws it).

// Search is what an ask search matches. An ask matches when ANY of its set
// parts does; a zero part matches nothing.
type Search struct {
	// AskID matches the ask with that id (0: none).
	AskID int64
	// DraftID matches the step-up cards that confirm a decision on that
	// plugin draft (StepUpCard.DraftID; 0: none).
	DraftID int64
	// Text matches, case-insensitively and as a plain substring (no
	// wildcards: % _ and \ are themselves), the question, the why line, any
	// option, the answer, who asked (the stored label, the mirrored session),
	// and a step-up card's title, why and post text ("": none), on every ask
	// whatever its age: attention_asks keeps an ask's text as long as the ask
	// (only the separate ask log forgets text, after MUSTER_ASK_LOG_TEXT_TTL).
	Text string
	// Limit is how many asks at most, newest first (≤ 0: MaxSearchResults).
	Limit int
	// OpenFirst orders the asks still open (not answered, cancelled or past
	// their expiry) before the rest, newest first within each, BEFORE the
	// limit cuts: an old open ask is never cut by newer closed ones.
	OpenFirst bool
}

// MaxSearchResults bounds one search.
const MaxSearchResults = 200

// MaxSearchTextRunes bounds a search's text.
const MaxSearchTextRunes = 200

// searchText is the text columns a Search's Text is matched against ($3 the
// pattern), one predicate.
const searchText = `(question ILIKE $3 ESCAPE '\'
		OR coalesce(answer, '') ILIKE $3 ESCAPE '\'
		OR asker_label ILIKE $3 ESCAPE '\'
		OR coalesce(mirror_session, '') ILIKE $3 ESCAPE '\'
		OR coalesce(why->>'text', '') ILIKE $3 ESCAPE '\'
		OR coalesce(why->>'focus', '') ILIKE $3 ESCAPE '\'
		OR coalesce(why->>'last', '') ILIKE $3 ESCAPE '\'
		OR EXISTS (SELECT 1 FROM jsonb_array_elements_text(options) o WHERE o ILIKE $3 ESCAPE '\')
		OR coalesce(step_up->>'title', '') ILIKE $3 ESCAPE '\'
		OR coalesce(step_up->>'why', '') ILIKE $3 ESCAPE '\'
		OR coalesce(step_up->>'preview', '') ILIKE $3 ESCAPE '\')`

// ownAsks is THE caller's-own-asks scope (the rule above): owned by the
// caller, never a question put to the chief. Search and ConfirmAsksForDrafts
// both read through it.
const ownAsks = `owner_id = nullif(current_setting('muster.uid', true), '')::bigint AND NOT to_chief`

// stepUpDraftID is the id, as text, of the plugin draft an ask's step-up card
// confirms (StepUpCard.DraftID); NULL for an ask with no step-up card. THE
// one spelling of "this ask confirms that draft" in SQL.
const stepUpDraftID = `(step_up->>'draft_id')`

// openFirstOrder is open asks (not answered, cancelled or past their expiry)
// first, newest first within each.
const openFirstOrder = `(status = 'open' AND expires_at > now()) DESC, id DESC`

// Search is the caller's own asks matching q, newest first (open ones first
// with q.OpenFirst).
func (s *PGStore) Search(ctx context.Context, q Search) ([]Ask, error) {
	text := truncateRunes(strings.TrimSpace(q.Text), MaxSearchTextRunes)
	if q.AskID <= 0 && q.DraftID <= 0 && text == "" {
		return nil, nil
	}
	limit := q.Limit
	if limit <= 0 || limit > MaxSearchResults {
		limit = MaxSearchResults
	}
	pattern := ""
	if text != "" {
		// 🔴 ESCAPED (db.LikeEscape): a search for "%" must not match every ask.
		pattern = "%" + db.LikeEscape(text) + "%"
	}
	// A constant ORDER BY, chosen here: never a caller's words.
	order := `id DESC`
	if q.OpenFirst {
		order = openFirstOrder
	}
	return collect(s.pool.Query(ctx, `SELECT `+cols+` FROM attention_asks
		WHERE `+ownAsks+`
		  AND (($1::bigint > 0 AND id = $1)
		    OR ($2::bigint > 0 AND `+stepUpDraftID+` = $2::bigint::text)
		    OR ($3::text <> '' AND `+searchText+`))
		ORDER BY `+order+` LIMIT $4`, q.AskID, q.DraftID, pattern, limit))
}

// DraftConfirmAsks is one draft's confirm asks: the caller's own asks whose
// step-up card is bound to it (StepUpCard.DraftID).
type DraftConfirmAsks struct {
	// Asks is at most the first n of them, open ones first, then newest.
	Asks []Ask
	// Total is how many there are in all, uncapped.
	Total int
}

// withExtra scans a row's columns into a scan's own, then extra.
type withExtra struct {
	pgx.Row
	extra []any
}

func (w withExtra) Scan(dest ...any) error { return w.Row.Scan(append(dest, w.extra...)...) }

// ConfirmAsksForDrafts is, for each of draftIDs that has any, the caller's
// OWN confirm asks (the ownAsks scope: never a team's, never one put to the
// chief) — at most n per draft (n ≤ 0: none listed, Total still counted),
// open first then newest, with the draft's uncapped count. ONE query for every
// draft, whatever their number.
func (s *PGStore) ConfirmAsksForDrafts(ctx context.Context, draftIDs []int64, n int) (map[int64]DraftConfirmAsks, error) {
	want := make([]string, 0, len(draftIDs))
	for _, id := range draftIDs {
		if id > 0 {
			want = append(want, strconv.FormatInt(id, 10))
		}
	}
	out := map[int64]DraftConfirmAsks{}
	if len(want) == 0 {
		return out, nil
	}
	if n < 0 {
		n = 0
	}
	rows, err := s.pool.Query(ctx, `SELECT `+cols+`, draft, rn, total FROM (
		SELECT *, `+stepUpDraftID+`::bigint AS draft,
		       row_number() OVER (PARTITION BY `+stepUpDraftID+` ORDER BY `+openFirstOrder+`) AS rn,
		       count(*) OVER (PARTITION BY `+stepUpDraftID+`) AS total
		  FROM attention_asks
		 WHERE `+ownAsks+`
		   AND `+stepUpDraftID+` = ANY($1::text[])
		) c
		WHERE rn <= greatest($2::int, 1)
		ORDER BY draft, rn`, want, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var draft, rn, total int64
		a, err := scan(withExtra{rows, []any{&draft, &rn, &total}})
		if err != nil {
			return nil, err
		}
		c := out[draft]
		c.Total = int(total)
		if rn <= int64(n) {
			c.Asks = append(c.Asks, a)
		}
		out[draft] = c
	}
	return out, rows.Err()
}
