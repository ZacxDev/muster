package focus

import (
	"context"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// SEARCHING FOCUS HISTORY IN A TIME WINDOW (migration 0022's rows): what a
// session was working on over the last few hours, not only what its current
// focus says.
//
// 🔴 WHY HISTORY, AND WHY A TIME WINDOW RATHER THAN THE NEWEST N ROWS. Focus is
// rewritten every refresh, so a session that has worked one long job for hours
// can have a CURRENT focus about some other subtask of it — a search of the
// current focus alone then misses it entirely. A count-capped tail misses it
// too, in a different way: a busy session writes many rows an hour, so its
// newest N reach back only as far as its own churn, and a quiet one's reach
// back days. Only a time window is the same span for every session. So every
// read here takes a `since`, and nothing here takes a row count per session
// until the rows are already inside the window and deduplicated.
//
// 🔴 THE LIVENESS FILTER IS IN SQL, BEFORE THE LIMIT, as Store.Search's is: a
// dead session's rows must neither be returned nor crowd a live one out of
// the result. Live names the sessions the caller may be handed — mirrored
// sessions by Claude session id, agents by id — and a row of any other
// session is never a hit.
//
// Like every Store method but the sweeps, these run under the caller's
// principal: agent_focus_history is under FORCE row-level security.

// HistoryWindow is how far back the chief's session hint and sessions_overview
// read focus history: six hours, the span that, replayed over a real miss,
// matched the session that was on the job and no other.
const HistoryWindow = 6 * time.Hour

// The bounds of a history read.
const (
	// MaxMatchTextBytes bounds the text MatchHistory matches against (an
	// inbound message can be long; its first few kilobytes name its subject).
	MaxMatchTextBytes = 8 << 10
	// MaxMatchTitles bounds the matching titles MatchHistory returns per session.
	MaxMatchTitles = 3
	// MaxRecentTitles bounds RecentTitles per session, AFTER the window and the
	// deduplication: a display bound, not a reach-back bound.
	MaxRecentTitles = 12
	// PaletteTitleRows is RecentTitles' row bound for the command palette:
	// each session's newest 64 history rows, its cost per session per render.
	PaletteTitleRows = 64
	// MinTitleQueryRunes is the shortest query SearchTitles answers: the
	// palette asks the server only once a query is this long.
	MinTitleQueryRunes = 3
	// MaxTitleQueryRunes is the longest query SearchTitles answers: a stored
	// title is at most MaxTitleRunes (migration 0023's CHECK), so a longer
	// query can match nothing — and it is answered without a read.
	MaxTitleQueryRunes = MaxTitleRunes
)

// Live is the sessions a history read may return: mirrored sessions by Claude
// session id, agents by id. A row of any other session is never returned.
type Live struct {
	Mirrors []string
	Agents  []int64
}

// TitleAt is one focus title and when it was (last) written.
type TitleAt struct {
	Title string
	At    time.Time
}

// HistoryHit is one session's best history row for a query.
type HistoryHit struct {
	Subject Subject
	Title   string
	Snippet string
	At      time.Time
	Rank    float32
}

// Match is one session whose recent history shares terms with a text: how
// many it shares (Terms: distinct identifiers plus distinct searchable words
// outside them, MatchHistory), and its titles that share any, newest first.
type Match struct {
	Subject Subject
	Terms   int
	Titles  []TitleAt
}

// liveHistorySQL is the liveness predicate over agent_focus_history h:
// ($m mirrored Claude session ids, $a agent ids). It is spelled ONCE so the
// three reads cannot disagree about which sessions are live.
func liveHistorySQL(m, a string) string {
	return `((h.subject_kind = 'mirror' AND h.subject_key = ANY(` + m + `::text[]))
		OR (h.subject_kind = 'agent' AND h.agent_id = ANY(` + a + `::bigint[])))`
}

// nonNil is live with nil slices as empty ones (pgx sends nil as NULL, and
// "= ANY(NULL)" is NULL — false, but say so explicitly).
func (l Live) nonNil() ([]string, []int64) {
	m, a := l.Mirrors, l.Agents
	if m == nil {
		m = []string{}
	}
	if a == nil {
		a = []int64{}
	}
	return m, a
}

// errNoWindow refuses a history read with no window: an unbounded read is not
// what any caller here means.
var errNoWindow = errf("a history read needs a since")

// SearchHistory is focus_search over history: each live session's best row
// written at or after since that matches query (websearch syntax, as Search),
// best first, at most limit (bounded by MaxSearchResults). Title weighs A and
// description B, as in agent_focus's generated column.
func (s *PGStore) SearchHistory(ctx context.Context, query string, live Live, since time.Time, limit int) ([]HistoryHit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return []HistoryHit{}, nil
	}
	if since.IsZero() {
		return nil, errNoWindow
	}
	if limit <= 0 || limit > MaxSearchResults {
		limit = MaxSearchResults
	}
	m, a := live.nonNil()
	rows, err := s.pool.Query(ctx, `SELECT subject_kind, subject_key, title, description, created_at, rank FROM (
			SELECT DISTINCT ON (h.subject_kind, h.subject_key) h.subject_kind, h.subject_key, h.title, h.description,
				h.created_at, ts_rank(h.v, q) AS rank
			FROM (SELECT h.*, setweight(to_tsvector('english', h.title), 'A') ||
					setweight(to_tsvector('english', h.description), 'B') AS v
				FROM agent_focus_history h
				WHERE h.created_at >= $2 AND `+liveHistorySQL("$3", "$4")+`) h,
				websearch_to_tsquery('english', $1) q
			WHERE h.v @@ q
			ORDER BY h.subject_kind, h.subject_key, rank DESC, h.created_at DESC, h.id DESC) best
		ORDER BY rank DESC, created_at DESC LIMIT $5`, query, since, m, a, limit)
	if err != nil {
		return nil, errf("search history: %w", err)
	}
	defer rows.Close()
	out := []HistoryHit{}
	for rows.Next() {
		var h HistoryHit
		var kind, desc string
		if err := rows.Scan(&kind, &h.Subject.Key, &h.Title, &desc, &h.At, &h.Rank); err != nil {
			return nil, errf("search history: %w", err)
		}
		h.Subject.Kind, h.Snippet = Kind(kind), Snippet(desc)
		out = append(out, h)
	}
	return out, rows.Err()
}

// MatchHistory is the live sessions whose history written at or after since
// shares at least minTerms terms with text — AT LEAST ONE OF THEM DISTINCTIVE
// (terms.go: an identifier, matched on its normalised key) — counting the
// distinctive terms shared plus the searchable words shared (english lexemes,
// stop words dropped) of the text with its identifiers blanked out, so an
// identifier counts once, not again as the words the parser splits it into.
// Any of them, not all: text is a whole message, not a query. Most
// distinctive terms first, then most terms, then most recently written; at
// most limit sessions, each with up to MaxMatchTitles of its titles that share
// any, newest first.
//
// minTerms < 1 is 1. The text contributes at most MaxDistinctiveTerms
// distinctive terms (the first ones), so stuffing it with identifiers cannot
// sweep every session.
//
// 🔴 GENERIC OVERLAP ALONE NAMES NOTHING. Measured: an ordinary mail ("quick
// update on our meeting next week, please review…") and a newsletter each
// shared two or more common words with unrelated sessions' focus — "update",
// "next", "review" — and named them. A message's sender can also add common
// words at will. So a match needs a shared identifier, which a mail about some
// other subject does not carry by accident.
//
// ⚠ WHY NOT "A TERM IN AT MOST ONE CANDIDATE'S HISTORY": in a small fleet a
// common word is often in exactly one session's history ("review" in the one
// reviewing PRs), so that rule calls it distinctive and names the session for
// "please review the update" (TestGenericOverlapNamesNothing's nudge case).
//
// HOW: SQL reads, for the live sessions with a row whose text CONTAINS some
// distinctive key's core (keyCore: a cheap superset), their rows in the window
// that share a word or contain a core, with the words they share; Go then runs
// the same extractor over each row (distinctiveTerms) to decide which keys it
// really carries, and counts, ranks and cuts. One extractor on both sides is
// what makes "PR-137" and "PR 137" the same term.
func (s *PGStore) MatchHistory(ctx context.Context, text string, live Live, since time.Time, minTerms, limit int) ([]Match, error) {
	if len(text) > MaxMatchTextBytes {
		text = strings.ToValidUTF8(text[:MaxMatchTextBytes], "")
	}
	if strings.TrimSpace(text) == "" {
		return []Match{}, nil
	}
	if since.IsZero() {
		return nil, errNoWindow
	}
	if minTerms < 1 {
		minTerms = 1
	}
	if limit <= 0 || limit > MaxSearchResults {
		limit = MaxSearchResults
	}
	keys, rest := distinctiveTerms(text, MaxDistinctiveTerms)
	if len(keys) == 0 {
		return []Match{}, nil // nothing distinctive: nothing can be named
	}
	want := make(map[string]bool, len(keys))
	cores := make([]string, 0, len(keys))
	for _, k := range keys {
		want[k] = true
		cores = append(cores, keyCore(k))
	}
	m, a := live.nonNil()
	rows, err := s.pool.Query(ctx, `WITH terms AS (
			SELECT DISTINCT t FROM unnest(tsvector_to_array(to_tsvector('english', $1))) t),
		rows AS (
			SELECT h.id, h.subject_kind, h.subject_key, h.title, h.description, h.created_at,
				lower(h.title || ' ' || h.description) AS low,
				tsvector_to_array(to_tsvector('english', h.title || ' ' || h.description)) AS lx
			FROM agent_focus_history h
			WHERE h.created_at >= $2 AND `+liveHistorySQL("$3", "$4")+`),
		cand AS (SELECT DISTINCT subject_kind, subject_key FROM rows r
			WHERE EXISTS (SELECT 1 FROM unnest($5::text[]) c WHERE strpos(r.low, c) > 0))
		SELECT r.subject_kind, r.subject_key, r.title, r.description, r.created_at,
			ARRAY(SELECT t FROM terms WHERE t = ANY(r.lx) ORDER BY t) AS shared
		FROM rows r JOIN cand USING (subject_kind, subject_key)
		WHERE EXISTS (SELECT 1 FROM terms WHERE terms.t = ANY(r.lx))
			OR EXISTS (SELECT 1 FROM unnest($5::text[]) c WHERE strpos(r.low, c) > 0)
		ORDER BY r.created_at DESC, r.id DESC`, rest, since, m, a, cores)
	if err != nil {
		return nil, errf("match history: %w", err)
	}
	defer rows.Close()
	type acc struct {
		sub      Subject
		keys     map[string]bool
		words    map[string]bool
		newest   time.Time
		titles   []TitleAt
		titleSet map[string]bool
	}
	by := map[Subject]*acc{}
	for rows.Next() {
		var kind, key, title, desc string
		var at time.Time
		var shared []string
		if err := rows.Scan(&kind, &key, &title, &desc, &at, &shared); err != nil {
			return nil, errf("match history: %w", err)
		}
		sub := Subject{Kind: Kind(kind), Key: key}
		rowKeys, _ := distinctiveTerms(title+" "+desc, 0)
		var hit []string
		for _, k := range rowKeys {
			if want[k] {
				hit = append(hit, k)
			}
		}
		if len(hit) == 0 && len(shared) == 0 {
			continue // contained a key's core, but not the key
		}
		x := by[sub]
		if x == nil {
			x = &acc{sub: sub, keys: map[string]bool{}, words: map[string]bool{}, titleSet: map[string]bool{}}
			by[sub] = x
		}
		for _, k := range hit {
			x.keys[k] = true
		}
		for _, w := range shared {
			x.words[w] = true
		}
		if at.After(x.newest) {
			x.newest = at
		}
		// Rows arrive newest first, so a title's first row is its newest.
		if !x.titleSet[title] && len(x.titles) < MaxMatchTitles {
			x.titleSet[title] = true
			x.titles = append(x.titles, TitleAt{Title: title, At: at})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, errf("match history: %w", err)
	}
	type ranked struct {
		Match
		nd int
		at time.Time
	}
	var out []ranked
	for _, x := range by {
		nd := len(x.keys)
		n := nd + len(x.words)
		if nd < 1 || n < minTerms {
			continue
		}
		out = append(out, ranked{Match: Match{Subject: x.sub, Terms: n, Titles: x.titles}, nd: nd, at: x.newest})
	}
	// Most distinctive terms first, then most terms, then most recently
	// written, then the subject (so the order is total).
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch {
		case a.nd != b.nd:
			return a.nd > b.nd
		case a.Terms != b.Terms:
			return a.Terms > b.Terms
		case !a.at.Equal(b.at):
			return a.at.After(b.at)
		case a.Subject.Kind != b.Subject.Kind:
			return a.Subject.Kind < b.Subject.Kind
		}
		return a.Subject.Key < b.Subject.Key
	})
	if len(out) > limit {
		out = out[:limit]
	}
	res := make([]Match, 0, len(out))
	for _, r := range out {
		res = append(res, r.Match)
	}
	return res, nil
}

// RecentTitles is each live session's distinct focus titles written at or
// after since, newest first (a title written twice is listed once, at its
// newest), at most MaxRecentTitles per session. maxRows > 0 also bounds the
// read to each session's newest maxRows rows in the window — a ROW bound, for
// the command palette, which reads the whole retention on every page render
// and must cost the same whatever a session's churn; <= 0 is no row bound (the
// chief's sessions_overview, over HistoryWindow).
//
// 🔴 IT NAMES THE OWNER ITSELF (h.owner_id = the principal's muster.uid), not
// only through row-level security. The policy's USING is "owner_id = uid OR
// scope = 'all'", and that OR keeps the planner off
// idx_agent_focus_history_subject, whose first column is owner_id. Measured
// (one owner's 31 live sessions; 3 runs each):
//
//   - 245k rows (5 owners × 30 sessions × 1500 rows over 30 days, plus one
//     20k-row session): the palette's read (30 days, 64 rows a session)
//     2.8–3.3 ms with the predicate, 21–22 ms without; sessions_overview's
//     (6 h, no row bound) 1.4–1.9 ms, against 1.3–1.4 ms for the flat
//     GROUP BY this replaced (the 6 h window is served by the created_at
//     index either way: no speed-up there, and no loss);
//   - 200k rows over 200 sessions of 5 owners: the palette's read 36 ms
//     with it, 2.3 s without — the planner walked the created_at index and
//     filtered 162k rows a session.
//
// Under the system scope with no uid it reads nothing; every caller acts as a
// user.
func (s *PGStore) RecentTitles(ctx context.Context, live Live, since time.Time, maxRows int) (map[Subject][]TitleAt, error) {
	if since.IsZero() {
		return nil, errNoWindow
	}
	if maxRows < 0 {
		maxRows = 0
	}
	m, a := live.nonNil()
	out := map[Subject][]TitleAt{}
	if len(m) == 0 && len(a) == 0 {
		return out, nil
	}
	// LIMIT NULL (maxRows 0) is no limit.
	rows, err := s.pool.Query(ctx, `SELECT s.kind, s.key, t.title, t.at
		FROM (SELECT 'mirror'::text AS kind, k AS key FROM unnest($2::text[]) k
			UNION SELECT 'agent', id::text FROM unnest($3::bigint[]) id) s
		CROSS JOIN LATERAL (
			SELECT r.title, max(r.created_at) AS at FROM (
				SELECT h.title, h.created_at FROM agent_focus_history h
				WHERE h.owner_id = nullif(current_setting('muster.uid', true), '')::bigint
					AND h.subject_kind = s.kind AND h.subject_key = s.key AND h.created_at >= $1
				ORDER BY h.created_at DESC, h.id DESC LIMIT nullif($4::int, 0)) r
			GROUP BY r.title) t
		ORDER BY s.kind, s.key, t.at DESC, t.title`, since, m, a, maxRows)
	if err != nil {
		return nil, errf("recent titles: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, key string
		var t TitleAt
		if err := rows.Scan(&kind, &key, &t.Title, &t.At); err != nil {
			return nil, errf("recent titles: %w", err)
		}
		sub := Subject{Kind: Kind(kind), Key: key}
		if len(out[sub]) < MaxRecentTitles {
			out[sub] = append(out[sub], t)
		}
	}
	return out, rows.Err()
}

// SearchTitles is the command palette's search of the WHOLE window: each live
// session's newest focus title written at or after since that CONTAINS query
// (case-folded: the query by Go's strings.ToLower, the title by Postgres's
// lower(); on the title only), newest first, at most limit (bounded by
// MaxSearchResults). A query (trimmed) of fewer than MinTitleQueryRunes or
// more than MaxTitleQueryRunes runes matches nothing, and is not read.
//
// 🔴 A SUBSTRING OF THE TITLE, NOT SearchHistory's full-text search. The
// palette matches what is typed as a fragment, in-page, as it is typed: "payout
// zeph" must find "Reconciling the payout zephyr", and websearch_to_tsquery
// matches whole (stemmed) words only. SearchHistory also matches the
// description, and a hit is shown as "Previously: <title>" — a title that does
// not contain what was typed would be an unexplained result. And it is
// cheaper: measured on the 245k-row set (one owner's 65k rows in 30 days),
// SearchHistory took 283 ms (203 ms with the owner predicate: computing the
// tsvector of every row dominates), this 53 ms.
//
// Owner-scoped like RecentTitles: row-level security, and the owner named in
// the query.
func (s *PGStore) SearchTitles(ctx context.Context, query string, live Live, since time.Time, limit int) ([]HistoryHit, error) {
	query = strings.TrimSpace(query)
	if n := utf8.RuneCountInString(query); n < MinTitleQueryRunes || n > MaxTitleQueryRunes {
		return []HistoryHit{}, nil
	}
	// Folded here, once, rather than by lower($1) in the statement.
	query = strings.ToLower(query)
	if since.IsZero() {
		return nil, errNoWindow
	}
	if limit <= 0 || limit > MaxSearchResults {
		limit = MaxSearchResults
	}
	m, a := live.nonNil()
	rows, err := s.pool.Query(ctx, `SELECT kind, key, title, at FROM (
			SELECT DISTINCT ON (h.subject_kind, h.subject_key) h.subject_kind AS kind, h.subject_key AS key,
				h.title, h.created_at AS at
			FROM agent_focus_history h
			WHERE h.owner_id = nullif(current_setting('muster.uid', true), '')::bigint
				AND h.created_at >= $2 AND `+liveHistorySQL("$3", "$4")+`
				AND strpos(lower(h.title), $1) > 0
			ORDER BY h.subject_kind, h.subject_key, h.created_at DESC, h.id DESC) best
		ORDER BY at DESC, kind, key LIMIT $5`, query, since, m, a, limit)
	if err != nil {
		return nil, errf("search titles: %w", err)
	}
	defer rows.Close()
	out := []HistoryHit{}
	for rows.Next() {
		var h HistoryHit
		var kind string
		if err := rows.Scan(&kind, &h.Subject.Key, &h.Title, &h.At); err != nil {
			return nil, errf("search titles: %w", err)
		}
		h.Subject.Kind = Kind(kind)
		out = append(out, h)
	}
	return out, rows.Err()
}
