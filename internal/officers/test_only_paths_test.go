package officers_test

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// TestParseReadsATestOnlyPathsFile: `test_only_paths_file` is kept on the one
// ship repository that declares it; a path that is absolute, climbs, is a
// directory or is blank is refused naming the key; the top-level
// (single-repository) form does not take it; and the shepherd's rule carries
// it for that repository (officers.ShepherdRules reads ShipRepo, so a parsed
// value is what reaches the wall).
func TestParseReadsATestOnlyPathsFile(t *testing.T) {
	m := draftShip()
	m["repos"].([]any)[0].(map[string]any)["test_only_paths_file"] = " .github/officer/money-paths.txt "
	list, err := parseDecl(t, draftDecl(m), shipEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	if r := list[0].Ship.Repos[0]; r.TestOnlyPathsFile != ".github/officer/money-paths.txt" {
		t.Fatalf("repo 0 = %+v, want the money-path file", r)
	}
	if list[0].Ship.Repos[1].TestOnlyPathsFile != "" {
		t.Fatal("the other repository got the file")
	}
	for _, bad := range []string{"/etc/list.txt", "../list.txt", "a/../b.txt", "  ", "dir/", "a/*.txt"} {
		m := draftShip()
		m["repos"].([]any)[0].(map[string]any)["test_only_paths_file"] = bad
		if _, err := parseDecl(t, draftDecl(m), shipEnv(t)); err == nil || !strings.Contains(err.Error(), "test_only_paths_file") {
			t.Errorf("test_only_paths_file %q: err = %v, want a refusal naming it", bad, err)
		}
	}
	top := draftShip()
	top["test_only_paths_file"] = ".github/officer/money-paths.txt"
	if _, err := parseDecl(t, draftDecl(top), shipEnv(t)); err == nil || !strings.Contains(err.Error(), "test_only_paths_file") {
		t.Errorf("a top-level test_only_paths_file: %v, want a refusal naming it", err)
	}
}

// TestAShepherdRuleCarriesTheMoneyPaths: a shepherd's rule for a repository
// holds the shepherd's own test_only_paths_file and the author officer's,
// sorted and unique, so a shepherd push meets the same money-path wall the
// author's own pull request does.
func TestAShepherdRuleCarriesTheMoneyPaths(t *testing.T) {
	sh := officers.Officer{ID: "shepherd:example", Repo: "example-org/web", Ship: &officers.Ship{Stage: officers.StageShepherd,
		Repos:    []officers.ShipRepo{{Repo: "example-org/web", TestOnlyPathsFile: "b/money.txt", ForbiddenPaths: []string{"x/**"}}},
		Shepherd: &officers.Shepherd{Authors: []officers.ShepherdAuthor{{Login: "author-bot[bot]", Officer: "author:example"}}}}}
	au := officers.Officer{ID: "author:example", Repo: "example-org/web", Ship: &officers.Ship{Stage: officers.StageDraft,
		Repos: []officers.ShipRepo{{Repo: "example-org/web", TestOnlyPathsFile: "a/money.txt"}}}}
	rules := officers.ShepherdRules(sh, []officers.Officer{sh, au})
	if len(rules) != 1 || strings.Join(rules[0].TestOnlyPathsFiles, ",") != "a/money.txt,b/money.txt" {
		t.Fatalf("rules = %+v, want one carrying both money-path files", rules)
	}
}
