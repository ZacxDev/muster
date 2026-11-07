package focus

import (
	"context"
	"regexp"
	"sort"
	"strings"
)

// MATCHING A LIVE SESSION BY WHAT ITS WINDOW SAYS, NOT ONLY BY ITS FOCUS: the
// repository it works in and its window label, as its mirror host reports
// them (mirror.SessionView.Repo / Label).
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
// 🔴 THE SAME TWO RULES AS MatchHistory. A match needs a DISTINCTIVE shared
// term and at least minTerms terms in all; generic overlap alone names
// nothing. What is distinctive here:
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
//     repository still has to be a run as above;
//   - an identifier (terms.go) the text and the label both carry.
//
// Every covered repository part counts as a term, and so does every english
// lexeme (stop words dropped, as MatchHistory's) the label shares with the
// text outside its identifiers.
//
// ⚠ THE REPO AND THE LABEL ARE UNTRUSTED, like titles: a host sends them.
// They decide only WHETHER a session the caller already holds is listed —
// never which sessions exist — and whoever renders them fences them.

// MaxMetaSessions bounds how many sessions one MatchMeta or SearchMeta call
// reads; the rest are not considered.
const MaxMetaSessions = 1024

// SessionMeta is what muster knows of one live session besides its focus: the
// short name of the repository its window works in and its window label ("" for
// either when its host did not say).
type SessionMeta struct {
	Subject     Subject
	Repo, Label string
}

// MetaMatch is one session whose repository name or window label shares terms
// with a text: how many in all (Terms) and how many distinctive (Distinctive).
type MetaMatch struct {
	Subject            Subject
	Terms, Distinctive int
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

// MatchMeta is the sessions of metas whose repository name or window label
// shares at least minTerms terms with text, at least one of them distinctive
// (see the top of this file): most distinctive terms first, then most terms,
// then the subject; at most limit (bounded by MaxSearchResults). minTerms < 1
// is 1. The text is cut as MatchHistory's is.
//
// It reads no table: Postgres is asked only for the english lexemes the labels
// share with the text (the same parser MatchHistory's word overlap uses), and
// only for sessions that already carry a distinctive term. Scoping is the
// caller's: metas are sessions it holds.
func (s *PGStore) MatchMeta(ctx context.Context, text string, metas []SessionMeta, minTerms, limit int) ([]MetaMatch, error) {
	if len(text) > MaxMatchTextBytes {
		text = strings.ToValidUTF8(text[:MaxMatchTextBytes], "")
	}
	if strings.TrimSpace(text) == "" || len(metas) == 0 {
		return []MetaMatch{}, nil
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
	keys, rest := distinctiveTerms(text, MaxDistinctiveTerms)
	want := make(map[string]bool, len(keys))
	for _, k := range keys {
		want[k] = true
	}
	words, shared := repoParts(text), sharedParts(metas)
	type cand struct {
		meta  SessionMeta
		terms map[string]bool
		nd    int
	}
	var cands []*cand
	var labels []string
	for _, m := range metas {
		c := &cand{meta: m, terms: map[string]bool{}}
		if m.Repo != "" {
			covered, ok := repoCover(words, repoParts(m.Repo), shared)
			if ok {
				for p := range covered {
					c.terms["repo:"+p] = true
				}
				c.nd += len(covered)
			}
		}
		if m.Label != "" {
			lk, _ := distinctiveTerms(m.Label, 0)
			for _, k := range lk {
				if want[k] && !c.terms["key:"+k] {
					c.terms["key:"+k] = true
					c.nd++
				}
			}
		}
		if c.nd == 0 {
			continue // nothing distinctive: nothing can be named
		}
		cands = append(cands, c)
		labels = append(labels, m.Label)
	}
	if len(cands) == 0 {
		return []MetaMatch{}, nil
	}
	lexemes, err := s.sharedLexemes(ctx, rest, labels)
	if err != nil {
		return nil, err
	}
	var out []MetaMatch
	for i, c := range cands {
		for _, w := range lexemes[i] {
			c.terms["word:"+w] = true
		}
		// A repository part and a label word that are the same word count once.
		n := len(c.terms)
		for t := range c.terms {
			if strings.HasPrefix(t, "word:") && c.terms["repo:"+strings.TrimPrefix(t, "word:")] {
				n--
			}
		}
		if n < minTerms {
			continue
		}
		out = append(out, MetaMatch{Subject: c.meta.Subject, Terms: n, Distinctive: c.nd})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch {
		case a.Distinctive != b.Distinctive:
			return a.Distinctive > b.Distinctive
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
	if out == nil {
		out = []MetaMatch{}
	}
	return out, nil
}

// sharedLexemes is, for each of docs in order, the english lexemes it shares
// with text (stop words dropped), sorted.
func (s *PGStore) sharedLexemes(ctx context.Context, text string, docs []string) ([][]string, error) {
	rows, err := s.pool.Query(ctx, `WITH terms AS (
			SELECT DISTINCT t FROM unnest(tsvector_to_array(to_tsvector('english', $1))) t)
		SELECT u.i, ARRAY(SELECT t FROM terms WHERE t = ANY(tsvector_to_array(to_tsvector('english', u.d))) ORDER BY t)
		FROM unnest($2::text[]) WITH ORDINALITY u(d, i) ORDER BY u.i`, text, docs)
	if err != nil {
		return nil, errf("match meta: %w", err)
	}
	defer rows.Close()
	out := make([][]string, len(docs))
	for rows.Next() {
		var i int64
		var shared []string
		if err := rows.Scan(&i, &shared); err != nil {
			return nil, errf("match meta: %w", err)
		}
		if i >= 1 && int(i) <= len(out) {
			out[i-1] = shared
		}
	}
	return out, rows.Err()
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
