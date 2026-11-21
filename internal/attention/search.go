package attention

import (
	"context"
	"strings"

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

// Search is the caller's own asks matching q, newest first.
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
	return collect(s.pool.Query(ctx, `SELECT `+cols+` FROM attention_asks
		WHERE owner_id = nullif(current_setting('muster.uid', true), '')::bigint
		  AND NOT to_chief
		  AND (($1::bigint > 0 AND id = $1)
		    OR ($2::bigint > 0 AND step_up IS NOT NULL AND step_up->>'draft_id' = $2::bigint::text)
		    OR ($3::text <> '' AND `+searchText+`))
		ORDER BY id DESC LIMIT $4`, q.AskID, q.DraftID, pattern, limit))
}
