package focus

import (
	"context"
	"regexp"
	"sort"
	"strings"
)

// MATCHING A LIVE SESSION BY WHAT ITS WINDOW SAYS, NOT ONLY BY ITS FOCUS: the
// repository it works in (the hint and focus_search) and its window label
// (focus_search only), as its mirror host reports them
// (mirror.SessionView.Repo / Label).
//
// 🔴 WHY. Focus is written only when a session's transcript moves
// (refresher.go), so a session that is open on a job but has not had a turn
// for days has NO focus row in any recent window — and its last titles may not
// name the job at all. Measured: an outside message about an app whose
// repository was "<org>-app-yt-thumbnail" reached the chief while a live
// window sat in that repository, idle for ten days; the hint named an
// unrelated session and focus_search for "thumbnail" found nothing, because
// every row the session had was older than the window and none said
// "thumbnail". Its repository name did.
//
// 🔴 THE HINT MATCHES THE REPO ONLY (MatchRepos), NEVER THE LABEL. A Claude
// window's label is its host's codename and index ("<codename>:<n>",
// mirror.Window.Label), not a task title — so a label term in a message is a
// word of prose that happens to spell a codename, and naming a session for it
// is the generic overlap MatchHistory refuses. focus_search (SearchMeta) does
// search the label: there the chief typed the codename on purpose.
//
// 🔴 THE SAME TWO RULES AS MatchHistory. A repo match needs at least minTerms
// covered parts, all from a DISTINCTIVE run; generic overlap alone names
// nothing. A run counts when it is:
//
//   - a RUN of the repository name's parts (split on anything not a letter
//     or digit) that the text spells consecutively in any case and with any
//     separators — two or more parts ("yt-thumbnail", "YT Thumbnail" for
//     "acme-app-yt-thumbnail"), or one part that is distinctive on its own
//     (repoPartDistinctive: a word with a digit, or of 3+ letters that is not
//     a dictionary word — "acme", "thumbnail", never "layout" alone) — that
//     holds a part NO OTHER live repository's name has. So a family of repos
//     sharing a prefix ("acme-app-one", "acme-app-two") is not all named by a
//     message that says "acme app": only one whose own part it spells. The
//     names are counted distinct, so two windows in ONE repository share
//     nothing by it. This only ever REMOVES a match: a part unique to one
//     repository still has to be a run as above.
//
// Every covered repository part counts as one term.
//
// ⚠ THE REPO AND THE LABEL ARE UNTRUSTED, like titles: a host sends them.
// They decide only WHETHER a session the caller already holds is listed —
// never which sessions exist — and whoever renders them fences them.

// MaxMetaSessions bounds how many sessions one MatchRepos or SearchMeta call
// reads; the rest are not considered.
const MaxMetaSessions = 1024

// SessionMeta is what muster knows of one live session besides its focus: the
// short name of the repository its window works in and its window label ("" for
// either when its host did not say).
type SessionMeta struct {
	Subject     Subject
	Repo, Label string
}

// MetaMatch is one session whose repository name shares terms with a text:
// how many of its parts the text covers (Terms).
type MetaMatch struct {
	Subject Subject
	Terms   int
}

// repoSplitRE splits a repository name, and a text, into the words a run is
// made of.
var repoSplitRE = regexp.MustCompile(`[A-Za-z0-9]+`)

// repoParts is name's words, lower-cased.
func repoParts(name string) []string {
	ps := repoSplitRE.FindAllString(name, -1)
	for i := range ps {
		ps[i] = strings.ToLower(ps[i])
	}
	return ps
}

// repoPartDistinctive reports whether one repository part names something on
// its own: it holds a letter and a digit (not an ordinal or a clock time), or
// it is 3+ letters and not a dictionary word.
func repoPartDistinctive(p string) bool {
	hasLetter, hasDigit := false, false
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z':
			hasLetter = true
		case r >= '0' && r <= '9':
			hasDigit = true
		}
	}
	switch {
	case !hasLetter:
		return false
	case hasDigit:
		return !proseNumberRE.MatchString(p)
	}
	return len(p) >= 3 && !isDictWord(p)
}

// repoCover is which of parts the text's words (words, lower-cased) spell in a
// qualifying run (see the top of this file) — one holding a part shared
// reports false for — and whether any run qualified.
func repoCover(words, parts []string, shared func(string) bool) (covered map[string]bool, distinctive bool) {
	covered = map[string]bool{}
	for i := range words {
		for j := range parts {
			n := 0
			for i+n < len(words) && j+n < len(parts) && words[i+n] == parts[j+n] {
				n++
			}
			if n == 0 || (n == 1 && !repoPartDistinctive(parts[j])) {
				continue
			}
			run, own := parts[j:j+n], false
			for _, p := range run {
				own = own || !shared(p)
			}
			if !own {
				continue
			}
			distinctive = true
			for _, p := range run {
				covered[p] = true
			}
		}
	}
	return covered, distinctive
}

// sharedParts is, for metas, whether a repository part is in two or more of
// their DISTINCT repository names.
func sharedParts(metas []SessionMeta) func(string) bool {
	names := map[string]bool{}
	n := map[string]int{}
	for _, m := range metas {
		name := strings.ToLower(m.Repo)
		if name == "" || names[name] {
			continue
		}
		names[name] = true
		seen := map[string]bool{}
		for _, p := range repoParts(name) {
			if !seen[p] {
				seen[p] = true
				n[p]++
			}
		}
	}
	return func(p string) bool { return n[p] >= 2 }
}

// MatchRepos is the sessions of metas whose repository name the text covers
// in at least minTerms parts, from qualifying runs (see the top of this file):
// most parts first, then the subject; at most limit (bounded by
// MaxSearchResults). minTerms < 1 is 1. The text is cut as MatchHistory's is.
// Labels are not read. Pure: scoping is the caller's (metas are sessions it
// holds).
func MatchRepos(text string, metas []SessionMeta, minTerms, limit int) []MetaMatch {
	if len(text) > MaxMatchTextBytes {
		text = strings.ToValidUTF8(text[:MaxMatchTextBytes], "")
	}
	out := []MetaMatch{}
	if strings.TrimSpace(text) == "" || len(metas) == 0 {
		return out
	}
	if len(metas) > MaxMetaSessions {
		metas = metas[:MaxMetaSessions]
	}
	if minTerms < 1 {
		minTerms = 1
	}
	if limit <= 0 || limit > MaxSearchResults {
		limit = MaxSearchResults
	}
	words, shared := repoParts(text), sharedParts(metas)
	for _, m := range metas {
		if m.Repo == "" {
			continue
		}
		covered, ok := repoCover(words, repoParts(m.Repo), shared)
		if !ok || len(covered) < minTerms {
			continue
		}
		out = append(out, MetaMatch{Subject: m.Subject, Terms: len(covered)})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch {
		case a.Terms != b.Terms:
			return a.Terms > b.Terms
		case a.Subject.Kind != b.Subject.Kind:
			return a.Subject.Kind < b.Subject.Kind
		}
		return a.Subject.Key < b.Subject.Key
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// metaDoc is the text a session's repository name and label are searched as:
// the name's parts apart (so "thumbnail" or "yt thumbnail" match
// "acme-app-yt-thumbnail"), then the label.
func metaDoc(m SessionMeta) string {
	return strings.Join(repoParts(m.Repo), " ") + " " + m.Label
}

// joinedRE is a '-', '_' or '.' run between two letters or digits.
var joinedRE = regexp.MustCompile(`([A-Za-z0-9])[-_.]+([A-Za-z0-9])`)

// metaQuery is query with its joined words apart ("yt-thumbnail" → "yt
// thumbnail"), as metaDoc writes a repository name: the parser reads
// "yt-thumbnail" as a phrase of a compound and its parts, which a name split
// into parts does not hold. A leading '-' (websearch's NOT) is not between two
// words and stays.
func metaQuery(query string) string {
	for {
		next := joinedRE.ReplaceAllString(query, "$1 $2")
		if next == query {
			return query
		}
		query = next
	}
}

// SearchMeta is focus_search over the sessions of metas by repository name and
// window label (metaDoc): the sessions whose text matches query (websearch
// syntax, as Search), best first, at most limit (bounded by
// MaxSearchResults). It reads no table; scoping is the caller's.
func (s *PGStore) SearchMeta(ctx context.Context, query string, metas []SessionMeta, limit int) ([]Subject, error) {
	query = strings.TrimSpace(query)
	if query == "" || len(metas) == 0 {
		return []Subject{}, nil
	}
	if len(metas) > MaxMetaSessions {
		metas = metas[:MaxMetaSessions]
	}
	if limit <= 0 || limit > MaxSearchResults {
		limit = MaxSearchResults
	}
	docs := make([]string, len(metas))
	for i, m := range metas {
		docs[i] = metaDoc(m)
	}
	rows, err := s.pool.Query(ctx, `SELECT u.i FROM unnest($2::text[]) WITH ORDINALITY u(d, i),
			websearch_to_tsquery('english', $1) q
		WHERE to_tsvector('english', u.d) @@ q
		ORDER BY ts_rank(to_tsvector('english', u.d), q) DESC, u.i LIMIT $3`, metaQuery(query), docs, limit)
	if err != nil {
		return nil, errf("search meta: %w", err)
	}
	defer rows.Close()
	out := []Subject{}
	for rows.Next() {
		var i int64
		if err := rows.Scan(&i); err != nil {
			return nil, errf("search meta: %w", err)
		}
		if i >= 1 && int(i) <= len(metas) {
			out = append(out, metas[i-1].Subject)
		}
	}
	return out, rows.Err()
}
