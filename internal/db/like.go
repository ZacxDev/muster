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
// ⚠ IT HAS EXACTLY ONE CALLER IN THIS REPOSITORY TODAY —
// agents.PGStore.SearchSessions. The second upstream caller was notes'
// directory search, which was NOT carried across (see the carve note in
// internal/notes/pgstore.go). One caller is not a reason to inline it back:
// the next searcher is the case this function exists for, and the cost of
// keeping it is one file.
//
// Behaviour is covered twice on purpose — directly by TestLikeEscape here, and
// from the caller's side by the agents package's pgstore_search_test.go, which
// drives it through real SQL.
func LikeEscape(q string) string {
	q = strings.ReplaceAll(q, `\`, `\\`)
	q = strings.ReplaceAll(q, "%", `\%`)
	q = strings.ReplaceAll(q, "_", `\_`)
	return q
}
