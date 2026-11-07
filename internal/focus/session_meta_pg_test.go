package focus

import (
	"fmt"
	"testing"
)

// MatchMeta and SearchMeta (session_meta.go) against a real Postgres — the
// english parser decides the label's words. Synthetic names only.

const (
	metaIdle  = "0b2c3d4e-meta-idle"
	metaOther = "0b2c3d4e-meta-other"
)

func metaFixture() []SessionMeta {
	return []SessionMeta{
		{Subject: MirrorSubject(metaIdle), Repo: "x-app-yt-thumbnail", Label: "Update desktop layout for generator formats"},
		{Subject: MirrorSubject(metaOther), Repo: "cache-quorum", Label: "Reading the cache dashboards"},
	}
}

func metaSubjects(ms []MetaMatch) string {
	var out []string
	for _, m := range ms {
		out = append(out, fmt.Sprintf("%s/%d/%d", m.Subject, m.Terms, m.Distinctive))
	}
	return fmt.Sprint(out)
}

func TestMatchMetaFindsASessionByItsRepoAndLabel(t *testing.T) {
	s, _ := pgStore(t)
	ms, err := s.MatchMeta(as(2), "Feedback: yt-thumbnail generator formats look cramped", metaFixture(), 2, 5)
	// yt + thumbnail (the repo run) and generat + format (the label's words).
	if err != nil || metaSubjects(ms) != "[mirror:"+metaIdle+"/4/2]" {
		t.Fatalf("match %s %v; want the idle session, 4 terms, 2 distinctive", metaSubjects(ms), err)
	}
	// The generic-overlap rule holds: words the label shares, and a repo
	// part that names nothing on its own, name nothing.
	for _, text := range []string{
		"Quick update on our desktop layout and the generator formats, please.",
		"Quick update: please review the cache dashboards before the meeting.",
		"",
	} {
		if ms, err := s.MatchMeta(as(2), text, metaFixture(), 2, 5); err != nil || len(ms) != 0 {
			t.Errorf("%q named %s %v", text, metaSubjects(ms), err)
		}
	}
	// minTerms holds: "thumbnail" alone is one distinctive term.
	if ms, _ := s.MatchMeta(as(2), "a thumbnail", metaFixture(), 2, 5); len(ms) != 0 {
		t.Errorf("one term named %s", metaSubjects(ms))
	}
	if ms, _ := s.MatchMeta(as(2), "a thumbnail", metaFixture(), 1, 5); metaSubjects(ms) != "[mirror:"+metaIdle+"/1/1]" {
		t.Errorf("control, minTerms 1: %s", metaSubjects(ms))
	}
	// limit bounds it.
	many := make([]SessionMeta, 30)
	for i := range many {
		many[i] = SessionMeta{Subject: MirrorSubject(fmt.Sprintf("0b2c3d4e-meta-%02d", i)), Repo: "x-app-yt-thumbnail"}
	}
	if ms, _ := s.MatchMeta(as(2), "the yt-thumbnail app", many, 2, 3); len(ms) != 3 {
		t.Errorf("limit 3: %d matches", len(ms))
	}
	// An identifier the label carries is distinctive, as in a focus title
	// (#4711; review and releas are its words).
	label := []SessionMeta{{Subject: MirrorSubject(metaOther), Label: "Reviewing PR #4711 for the release"}}
	if ms, _ := s.MatchMeta(as(2), "Please review pull request 4711 before the release.", label, 2, 5); metaSubjects(ms) != "[mirror:"+metaOther+"/3/1]" {
		t.Errorf("label identifier: %s", metaSubjects(ms))
	}
}

func TestSearchMetaFindsASessionByAWordOfItsRepo(t *testing.T) {
	s, _ := pgStore(t)
	for q, want := range map[string]string{
		"thumbnail":          "[mirror:" + metaIdle + "]",
		"yt-thumbnail":       "[mirror:" + metaIdle + "]",
		"x-app-yt-thumbnail": "[mirror:" + metaIdle + "]",
		"generator formats":  "[mirror:" + metaIdle + "]",
		"cache":              "[mirror:" + metaOther + "]",
		"thumbnail invoice":  "[]", // websearch: every word
		"zeppelin":           "[]",
		"   ":                "[]",
	} {
		subs, err := s.SearchMeta(as(2), q, metaFixture(), 5)
		if err != nil || fmt.Sprint(subs) != want {
			t.Errorf("%q: %v %v; want %s", q, subs, err, want)
		}
	}
	if subs, _ := s.SearchMeta(as(2), "thumbnail", nil, 5); len(subs) != 0 {
		t.Errorf("no sessions: %v", subs)
	}
}
