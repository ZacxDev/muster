package focus

import (
	"fmt"
	"testing"
)

// The repository-run rule of session_meta.go, without a database. Synthetic
// names only ("x-app-yt-thumbnail", "acme").

func TestARepoPartIsDistinctiveOnlyWhenItNamesSomething(t *testing.T) {
	for p, want := range map[string]bool{
		"thumbnail": true,  // not a dictionary word
		"acme":      true,  // not a dictionary word
		"k8s":       true,  // letters and digits
		"layout":    false, // a dictionary word
		"x":         false, // too short
		"yt":        false, // too short
		"2nd":       false, // an ordinal
		"2024":      false, // no letter
	} {
		if got := repoPartDistinctive(p); got != want {
			t.Errorf("repoPartDistinctive(%q) = %v; want %v", p, got, want)
		}
	}
}

func TestARepoRunNeedsTwoPartsOrOneThatNamesSomething(t *testing.T) {
	none := func(string) bool { return false }
	parts := repoParts("x-app-yt-thumbnail")
	if fmt.Sprint(parts) != "[x app yt thumbnail]" {
		t.Fatalf("parts %v", parts)
	}
	for _, c := range []struct {
		text    string
		covered string
		ok      bool
	}{
		{"feedback for yt-thumbnail: generator formats", "map[thumbnail:true yt:true]", true},
		{"the YT Thumbnail page", "map[thumbnail:true yt:true]", true},
		{"a thumbnail looks cramped", "map[thumbnail:true]", true}, // one distinctive part
		{"the x-app-yt-thumbnail repo", "map[app:true thumbnail:true x:true yt:true]", true},
		{"x marks the spot", "map[]", false},      // one short part
		{"yt is a short word", "map[]", false},    // one short part
		{"thumbnails everywhere", "map[]", false}, // not the part
	} {
		cov, ok := repoCover(repoParts(c.text), parts, none)
		if ok != c.ok || fmt.Sprint(cov) != c.covered {
			t.Errorf("%q: covered %v ok %v; want %s %v", c.text, cov, ok, c.covered, c.ok)
		}
	}
}

// A family of repos sharing a prefix is not all named by the prefix; one whose
// own part the text spells still is, and two windows in ONE repo share nothing.
func TestARunOfOnlySharedPartsNamesNoRepo(t *testing.T) {
	metas := []SessionMeta{{Repo: "acme-app-yt-thumbnail"}, {Repo: "acme-app-billing"}, {Repo: "acme-app-billing"}}
	shared := sharedParts(metas)
	if !shared("acme") || !shared("app") || shared("billing") || shared("thumbnail") {
		t.Fatalf("shared: acme %v app %v billing %v thumbnail %v", shared("acme"), shared("app"), shared("billing"), shared("thumbnail"))
	}
	words := repoParts("Acme app feedback")
	for _, name := range []string{"acme-app-yt-thumbnail", "acme-app-billing"} {
		if _, ok := repoCover(words, repoParts(name), shared); ok {
			t.Errorf("%s: named by the shared prefix alone", name)
		}
	}
	if cov, ok := repoCover(repoParts("Acme app feedback: the YT Thumbnail page"), repoParts("acme-app-yt-thumbnail"), shared); !ok ||
		fmt.Sprint(cov) != "map[thumbnail:true yt:true]" {
		t.Errorf("its own part: covered %v ok %v", cov, ok)
	}
	// Control: alone, the prefix run does name it.
	if _, ok := repoCover(words, repoParts("acme-app-billing"), sharedParts(metas[1:])); !ok {
		t.Error("control: one repo, the prefix run named nothing")
	}
}

func metaMatches(ms []MetaMatch) string {
	var out []string
	for _, m := range ms {
		out = append(out, fmt.Sprintf("%s/%d", m.Subject, m.Terms))
	}
	return fmt.Sprint(out)
}

func TestMatchReposFindsASessionByItsRepo(t *testing.T) {
	idle, other := MirrorSubject("0b2c3d4e-meta-idle"), MirrorSubject("0b2c3d4e-meta-other")
	metas := []SessionMeta{{Subject: idle, Repo: "x-app-yt-thumbnail", Label: "Mars:1"}, {Subject: other, Repo: "cache-quorum", Label: "Iris:2"}}
	if got := metaMatches(MatchRepos("Feedback: yt-thumbnail generator formats look cramped", metas, 2, 5)); got != "[mirror:0b2c3d4e-meta-idle/2]" {
		t.Fatalf("match %s; want the idle session, 2 parts", got)
	}
	// Generic overlap, and a codename spelled in prose, name nothing: the
	// label is never matched.
	for _, text := range []string{
		"Quick update: please review the desktop layout and the generator formats.",
		"Mars mission update: Iris and Mars rovers, see Mars 1 and Iris 2.",
		// One repo part, and the codename would have been the second term.
		"The thumbnail on the Mars page looks off.",
		"",
	} {
		if ms := MatchRepos(text, metas, 2, 5); len(ms) != 0 {
			t.Errorf("%q named %s", text, metaMatches(ms))
		}
	}
	// minTerms holds: "thumbnail" alone covers one part.
	if ms := MatchRepos("a thumbnail", metas, 2, 5); len(ms) != 0 {
		t.Errorf("one part named %s", metaMatches(ms))
	}
	if got := metaMatches(MatchRepos("a thumbnail", metas, 1, 5)); got != "[mirror:0b2c3d4e-meta-idle/1]" {
		t.Errorf("control, minTerms 1: %s", got)
	}
	// limit bounds it.
	many := make([]SessionMeta, 30)
	for i := range many {
		many[i] = SessionMeta{Subject: MirrorSubject(fmt.Sprintf("0b2c3d4e-meta-%02d", i)), Repo: "x-app-yt-thumbnail"}
	}
	if ms := MatchRepos("the yt-thumbnail app", many, 2, 3); len(ms) != 3 {
		t.Errorf("limit 3: %d matches", len(ms))
	}
}
