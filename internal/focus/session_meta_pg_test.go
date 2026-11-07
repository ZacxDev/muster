package focus

import (
	"fmt"
	"testing"
)

// SearchMeta (session_meta.go) against a real Postgres — the english parser
// decides the words. Synthetic names only; labels are host codenames, as a
// mirror host sends them ("<codename>:<n>").

const (
	metaIdle  = "0b2c3d4e-meta-idle"
	metaOther = "0b2c3d4e-meta-other"
)

func metaFixture() []SessionMeta {
	return []SessionMeta{
		{Subject: MirrorSubject(metaIdle), Repo: "x-app-yt-thumbnail", Label: "Mars:1"},
		{Subject: MirrorSubject(metaOther), Repo: "cache-quorum", Label: "Iris:2"},
	}
}

func TestSearchMetaFindsASessionByAWordOfItsRepoOrItsCodename(t *testing.T) {
	s, _ := pgStore(t)
	for q, want := range map[string]string{
		"thumbnail":          "[mirror:" + metaIdle + "]",
		"yt-thumbnail":       "[mirror:" + metaIdle + "]",
		"x-app-yt-thumbnail": "[mirror:" + metaIdle + "]",
		"Mars":               "[mirror:" + metaIdle + "]", // the codename, typed on purpose
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
