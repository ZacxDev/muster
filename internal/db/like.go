package db

import "strings"

// LikeEscape makes a user string safe as the BODY of a LIKE/ILIKE pattern. Use it
// with `ESCAPE '\'` on the SQL side:
//
//	WHERE col ILIKE '%' || $1 || '%' ESCAPE '\'
//
// 🔴 WITHOUT IT A QUERY OF "%" MATCHES EVERY ROW. `%` and `_` are LIKE
// metacharacters, so a search box that pasted its input straight into a pattern
// would answer "%" with the whole table and "a_b" with rows containing "axb" —
// wrong answers that look like working search. The backslash is escaped FIRST, or
// escaping the other two would double-escape the escape.
//
// 🔴 IT LIVES HERE, NOT IN ITS CALLER, BECAUSE A PREDICATE OPEN-CODED PER
// PACKAGE ENDS UP SUBTLY DIFFERENT AT EACH SITE. Upstream this began as a
// private `likeEscape` inside the agents package and was promoted when a second
// searcher needed exactly the same predicate.
//
// Its callers: agents.PGStore.SearchSessions, and the Asks page's search —
// attention.PGStore.Search and plugins.PGStore.SearchDraftCards. (Notes'
// directory search, an upstream caller, was NOT carried across: see the carve
// note in internal/notes/pgstore.go.)
//
// Behaviour is covered directly by TestLikeEscape here, and from each
// caller's side through real SQL: the agents package's
// pgstore_search_test.go, attention's search_pg_test.go and plugins'
// drafts_search_pg_test.go.
func LikeEscape(q string) string {
	q = strings.ReplaceAll(q, `\`, `\\`)
	q = strings.ReplaceAll(q, "%", `\%`)
	q = strings.ReplaceAll(q, "_", `\_`)
	return q
}
