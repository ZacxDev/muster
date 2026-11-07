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
