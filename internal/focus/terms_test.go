package focus

import (
	"fmt"
	"strings"
	"testing"
)

// THE DISTINCTIVE-TERM EXTRACTOR (terms.go), without a database.

func TestWhatIsDistinctive(t *testing.T) {
	for _, c := range []struct {
		text string
		want []string
	}{
		// A number counts only written as a reference, and every spelling of
		// one reference is ONE key.
		{"PR 137", []string{"#137"}},
		{"see #137.", []string{"#137"}},
		{"PR-137 and pr137 and PR #137 and pull request 137", []string{"#137"}},
		{"137", nil},
		{"build 137 of 4711 passed", nil},
		{"&#137; is an entity, a/#137 a fragment", nil},
		// GitHub's cross-repo form owner/repo#N is the reference #N (the
		// repo is not in the key: a focus title says "PR #N"). The owner and
		// repo stay ordinary text.
		{"[ask] pr-review acme/widgets#7239 @841aa42 r1", []string{"#7239", "841aa42", "r1"}},
		{"# Review acme/widgets#7239 @841aa42", []string{"#7239", "841aa42"}},
		{"see my-org/repo.name_2#45, then #46", []string{"my-org", "repo.name_2", "#45", "#46"}},
		{"(acme/widgets#7)", []string{"#7"}},
		// Not a reference: a '#' glued to a word, a fragment of a URL or a
		// path (two slashes, or an owner with a dot — a domain), an entity.
		{"abc#12 and x_y#12", []string{"x_y"}},
		{"https://example.com/acme/widgets#12", []string{"example.com"}},
		{"see example.com/page#12 and docs/a/b#12", []string{"example.com"}},
		{"acme/widgets#x and acme/w&#39;", nil},
		{"?q=1&acme/widgets#12", nil},
		// Versions.
		{"v1.2 then 1.2.3", []string{"v1.2", "v1.2.3"}},
		{"1.2 is a number", nil},
		// Letters and digits together, but not prose numbers.
		{"drain node-7k, then k8s", []string{"node-7k", "k8s"}},
		{"the 2nd of May, the 1990s, at 5pm", nil},
		// Hyphenated: only with a part that is not a dictionary word.
		{"Quick follow-up on the sign-up form", nil},
		{"a self-hosted, well-known, built-in, up-to-date check-in", nil},
		{"merge muster-followups today", []string{"muster-followups"}},
		// Code identifiers and file names, which prose does not write.
		{"is match_history done?", []string{"match_history"}},
		{"did matchHistory or MatchHistory land?", []string{"matchhistory"}},
		{"see history_search.go and README.md", []string{"history_search.go", "readme.md"}},
		{"e.g. i.e. U.S. a.m.", nil},
		// Plain words never — nor acronyms.
		{"Please review the update before the release", nil},
		{"The HTTP API and OAuth", nil},
	} {
		got, _ := distinctiveTerms(c.text, MaxDistinctiveTerms)
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%q: %v; want %v", c.text, got, c.want)
		}
	}
}

// TestAMessageContributesAtMostTheFirstTwentyTerms: a message stuffed with
// references contributes only the first MaxDistinctiveTerms distinct ones, in
// text order — and every one of them is still blanked from rest.
func TestAMessageContributesAtMostTheFirstTwentyTerms(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&b, "#%d #%d ", i, i) // each twice: distinct ones are counted
	}
	keys, rest := distinctiveTerms(b.String()+"please review", MaxDistinctiveTerms)
	if len(keys) != 20 || keys[0] != "#1" || keys[19] != "#20" {
		t.Fatalf("keys %v; want #1 … #20", keys)
	}
	if strings.TrimSpace(rest) != "please review" {
		t.Fatalf("rest %q; want only the plain words", rest)
	}
	if all, _ := distinctiveTerms(b.String(), 0); len(all) != 40 {
		t.Fatalf("with no cap: %d keys; want 40", len(all))
	}
}

// TestRestBlanksIdentifiersOnly: the generic overlap is counted on rest, which
// keeps the plain words and drops every identifier's span.
func TestRestBlanksIdentifiersOnly(t *testing.T) {
	_, rest := distinctiveTerms("Re: node-7k upgrade (PR-137), v1.2.3 follow-up", 0)
	if got := strings.Join(strings.Fields(rest), " "); got != "Re: upgrade ( ), follow-up" {
		t.Fatalf("rest %q", got)
	}
	// A cross-repo reference blanks only its "#N": the owner and repo stay
	// words of the generic overlap.
	if _, rest := distinctiveTerms("Review acme/widgets#7239 today", 0); rest != "Review acme/widgets      today" {
		t.Fatalf("cross-repo rest %q", rest)
	}
}
