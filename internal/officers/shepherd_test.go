package officers_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// THE SHEPHERD STAGE's configuration. Two officers: a draft-stage AUTHOR
// shipping to example-infra (App 7001, its own forbidden list and list file)
// and the SHEPHERD (App 7002) over example-infra, with its own repository
// list too, so a wall that dropped either source cannot pass. Every value is
// synthetic and pairwise distinct.
// ---------------------------------------------------------------------------

const (
	shepID     = "shepherd:example-infra"
	shepAuthor = "investigate:example-infra"
	shepRepo   = "example-org/example-infra"
	shepLogin  = "example-investigator[bot]"
	shepOther  = "example-other-bot[bot]"
)

func shepEnv(t *testing.T) map[string]string {
	k := testAppKey(t)
	return map[string]string{
		"MUSTER_OFFICER_TOKEN_SHEPHERD_EXAMPLE_INFRA":             "tok-shepherd",
		"MUSTER_OFFICER_GITHUB_APP_KEY_SHEPHERD_EXAMPLE_INFRA":    k,
		"MUSTER_OFFICER_TOKEN_INVESTIGATE_EXAMPLE_INFRA":          "tok-author",
		"MUSTER_OFFICER_GITHUB_APP_KEY_INVESTIGATE_EXAMPLE_INFRA": k,
	}
}

func shepAuthorDecl() map[string]any {
	return map[string]any{"id": shepAuthor, "hq_url": "https://hq.example.com/api/v1", "repo": shepRepo, "ship": map[string]any{
		"stage": "draft",
		"repos": []any{map[string]any{"repo": shepRepo, "base": "trunk", "github_app": map[string]any{"id": 7001, "account": "example-org:4242"},
			"forbidden_paths": []string{"nodes/**", ".sops.yaml"}, "forbidden_paths_file": "scripts/ci/officer-forbidden.txt"}},
		"notice": map[string]any{"channel": "ch/example-alerts"},
	}}
}

func shepShip() map[string]any {
	return map[string]any{
		"stage": "shepherd",
		"repos": []any{map[string]any{"repo": shepRepo, "base": "trunk", "github_app": map[string]any{"id": 7002, "account": "example-org:4242"},
			"forbidden_paths": []string{".github/**"}}},
		"shepherd": map[string]any{"authors": []any{map[string]any{"login": shepLogin, "officer": shepAuthor}, shepOther}},
		"notice":   map[string]any{"channel": "ch/example-alerts"},
	}
}

func shepDecl(ship map[string]any) map[string]any {
	return map[string]any{"id": shepID, "hq_url": "https://hq.example.com/api/v1", "repo": shepRepo, "ship": ship}
}

func parseShep(t *testing.T, decls ...map[string]any) ([]officers.Officer, error) {
	t.Helper()
	all := make([]any, len(decls))
	for i, d := range decls {
		all[i] = d
	}
	raw, _ := json.Marshal(all)
	return officers.Parse(string(raw), env(shepEnv(t)), "muster-agent-")
}

func byID(t *testing.T, list []officers.Officer, id string) officers.Officer {
	t.Helper()
	for _, o := range list {
		if o.ID == id {
			return o
		}
	}
	t.Fatalf("no officer %s in %v", id, list)
	return officers.Officer{}
}

// TestParseReadsAShepherd is the allow half: the stage, both author forms
// (an object naming an officer, a bare login), no required_checks or
// review_check needed, nothing merged, tagged or marked ready through the
// own-PR tool.
func TestParseReadsAShepherd(t *testing.T) {
	list, err := parseShep(t, shepAuthorDecl(), shepDecl(shepShip()))
	if err != nil {
		t.Fatal(err)
	}
	s := byID(t, list, shepID).Ship
	if !s.IsShepherd() || s.IsDraft() || s.MayMerge() || s.MayRelease() || s.MayMarkReady() {
		t.Fatalf("parsed %v (shepherd %v draft %v merge %v release %v ready %v)", s, s.IsShepherd(), s.IsDraft(), s.MayMerge(), s.MayRelease(), s.MayMarkReady())
	}
	want := []officers.ShepherdAuthor{{Login: shepLogin, Officer: shepAuthor}, {Login: shepOther}}
	if s.Shepherd == nil || !reflect.DeepEqual(s.Shepherd.Authors, want) {
		t.Fatalf("authors = %+v, want %+v", s.Shepherd, want)
	}
	if a, ok := s.Shepherd.Author("EXAMPLE-INVESTIGATOR[BOT]"); !ok || a.Officer != shepAuthor {
		t.Fatalf("Author (case-folded) = %+v, %v", a, ok)
	}
	if _, ok := s.Shepherd.Author("someone-else[bot]"); ok {
		t.Fatal("Author answers a login that is not listed")
	}
	if r := s.Repos[0]; r.Base != "trunk" || r.BranchPrefix != "officer/" {
		t.Fatalf("repo entry = %+v, want base trunk and the default prefix", r)
	}
}

// TestTheShepherdsWallIsTheAuthorsLookedUpPlusItsOwn pins the one rule: for
// the author naming an officer, the wall is the author officer's own globs and
// list file for the repository PLUS the shepherd's; for a bare login, the
// shepherd's alone. Literal expectations, not derived from the code.
func TestTheShepherdsWallIsTheAuthorsLookedUpPlusItsOwn(t *testing.T) {
	list, err := parseShep(t, shepAuthorDecl(), shepDecl(shepShip()))
	if err != nil {
		t.Fatal(err)
	}
	got := officers.ShepherdRules(byID(t, list, shepID), list)
	want := []officers.ShepherdRule{
		{Repo: shepRepo, Login: shepLogin, AuthorOfficer: shepAuthor, ForbiddenPaths: []string{".github/**", ".sops.yaml", "nodes/**"},
			ForbiddenPathsFiles: []string{"scripts/ci/officer-forbidden.txt"}},
		{Repo: shepRepo, Login: shepOther, ForbiddenPaths: []string{".github/**"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rules =\n%+v\nwant\n%+v", got, want)
	}
	// The lookup is live: editing the AUTHOR's list changes the shepherd's
	// wall (one rule, one place: nothing copied into the shepherd).
	ad := shepAuthorDecl()
	ad["ship"].(map[string]any)["repos"].([]any)[0].(map[string]any)["forbidden_paths"] = []string{"clusters/prod/**"}
	list, err = parseShep(t, ad, shepDecl(shepShip()))
	if err != nil {
		t.Fatal(err)
	}
	if got := officers.ShepherdRules(byID(t, list, shepID), list)[0].ForbiddenPaths; !reflect.DeepEqual(got, []string{".github/**", "clusters/prod/**"}) {
		t.Fatalf("after editing the author's list the wall is %v", got)
	}
}

// TestParseRefusesABadShepherd: every boot refusal, each naming its reason.
// The control is TestParseReadsAShepherd's declaration, which parses.
func TestParseRefusesABadShepherd(t *testing.T) {
	ship := func(edit func(m map[string]any)) map[string]any { m := shepShip(); edit(m); return m }
	authors := func(a ...any) func(m map[string]any) {
		return func(m map[string]any) { m["shepherd"] = map[string]any{"authors": a} }
	}
	for name, c := range map[string]struct {
		decls []map[string]any
		want  string
	}{
		"no shepherd block": {[]map[string]any{shepAuthorDecl(), shepDecl(ship(func(m map[string]any) { delete(m, "shepherd") }))},
			"needs a shepherd block"},
		"no authors":    {[]map[string]any{shepAuthorDecl(), shepDecl(ship(authors()))}, "shepherd.authors is empty"},
		"a login twice": {[]map[string]any{shepAuthorDecl(), shepDecl(ship(authors(shepOther, "Example-Other-Bot[bot]")))}, "twice"},
		"not a login":   {[]map[string]any{shepAuthorDecl(), shepDecl(ship(authors("not a login")))}, "is not a GitHub login"},
		"an unknown author key": {[]map[string]any{shepAuthorDecl(), shepDecl(ship(authors(map[string]any{"login": shepOther, "role": "x"})))},
			"unknown field"},
		"itself as an author": {[]map[string]any{shepAuthorDecl(), shepDecl(ship(authors(map[string]any{"login": shepOther, "officer": shepID})))},
			"never acts on its own work"},
		"an undeclared author officer": {[]map[string]any{shepDecl(shepShip())}, "is not declared in MUSTER_OFFICERS"},
		"an author officer with no ship": {[]map[string]any{{"id": shepAuthor, "hq_url": "https://hq.example.com/api/v1", "repo": shepRepo},
			shepDecl(shepShip())}, "has no ship block"},
		"an author officer that is a shepherd": {[]map[string]any{func() map[string]any {
			d := shepAuthorDecl()
			d["ship"] = map[string]any{"stage": "shepherd", "repos": []any{map[string]any{"repo": shepRepo, "github_app": map[string]any{"id": 7001, "account": "example-org:4242"},
				"forbidden_paths": []string{"x/**"}}}, "shepherd": map[string]any{"authors": []any{"someone[bot]"}}, "notice": map[string]any{"channel": "ch/example-alerts"}}
			return d
		}(), shepDecl(shepShip())}, "is itself a shepherd"},
		"the author's App is the shepherd's": {[]map[string]any{shepAuthorDecl(), shepDecl(ship(func(m map[string]any) {
			m["repos"].([]any)[0].(map[string]any)["github_app"] = map[string]any{"id": 7001, "account": "example-org:4242"}
		}))}, "is the shepherd's own"},
		"an author officer shipping elsewhere": {[]map[string]any{func() map[string]any {
			d := shepAuthorDecl()
			d["repo"] = "example-org/elsewhere"
			d["ship"].(map[string]any)["repos"].([]any)[0].(map[string]any)["repo"] = "example-org/elsewhere"
			return d
		}(), shepDecl(shepShip())}, "ships to none of the shepherd's repositories"},
		"a bare login with no wall": {[]map[string]any{shepAuthorDecl(), shepDecl(ship(func(m map[string]any) {
			delete(m["repos"].([]any)[0].(map[string]any), "forbidden_paths")
		}))}, "would pass no forbidden-path check"},
		"review_check":     {[]map[string]any{shepAuthorDecl(), shepDecl(ship(func(m map[string]any) { m["review_check"] = "pr-review" }))}, "would ignore review_check"},
		"sensitive_paths":  {[]map[string]any{shepAuthorDecl(), shepDecl(ship(func(m map[string]any) { m["sensitive_paths"] = []string{"x/"} }))}, "would ignore sensitive_paths"},
		"releases_per_day": {[]map[string]any{shepAuthorDecl(), shepDecl(ship(func(m map[string]any) { m["releases_per_day"] = 2 }))}, "would ignore releases_per_day"},
		"a rollout": {[]map[string]any{shepAuthorDecl(), shepDecl(ship(func(m map[string]any) {
			m["rollout"] = map[string]any{"health_url": "https://svc.example.test/healthz", "rollback": "off"}
		}))}, "would ignore rollout"},
		"no notice": {[]map[string]any{shepAuthorDecl(), shepDecl(ship(func(m map[string]any) { delete(m, "notice") }))}, "needs a notice block"},
		"a ci_logs block": {[]map[string]any{shepAuthorDecl(), func() map[string]any {
			d := shepDecl(shepShip())
			d["ci_logs"] = map[string]any{"namespace": "ci", "pipeline": "gate", "commit_label": "x/commit", "service_account": "ci-reader"}
			return d
		}()}, "has no pull request of its own"},
		"a shepherd block on another stage": {[]map[string]any{shepAuthorDecl(), func() map[string]any {
			d := shepAuthorDecl()
			d["id"] = "other:example-infra"
			d["ship"].(map[string]any)["shepherd"] = map[string]any{"authors": []any{shepOther}}
			return d
		}()}, "is read by stage shepherd only"},
		"two shepherds of one author in one repository": {[]map[string]any{shepAuthorDecl(), shepDecl(shepShip()), func() map[string]any {
			d := shepDecl(shepShip())
			d["id"] = "shepherd:example-two"
			d["ship"].(map[string]any)["repos"].([]any)[0].(map[string]any)["github_app"] = map[string]any{"id": 7003, "account": "example-org:4242"}
			return d
		}()}, "two writers would race"},
	} {
		t.Run(name, func(t *testing.T) {
			envm := shepEnv(t)
			envm["MUSTER_OFFICER_TOKEN_OTHER_EXAMPLE_INFRA"] = "tok-other"
			envm["MUSTER_OFFICER_GITHUB_APP_KEY_OTHER_EXAMPLE_INFRA"] = envm["MUSTER_OFFICER_GITHUB_APP_KEY_SHEPHERD_EXAMPLE_INFRA"]
			envm["MUSTER_OFFICER_TOKEN_SHEPHERD_EXAMPLE_TWO"] = "tok-two"
			envm["MUSTER_OFFICER_GITHUB_APP_KEY_SHEPHERD_EXAMPLE_TWO"] = envm["MUSTER_OFFICER_GITHUB_APP_KEY_SHEPHERD_EXAMPLE_INFRA"]
			all := make([]any, len(c.decls))
			for i, d := range c.decls {
				all[i] = d
			}
			raw, _ := json.Marshal(all)
			_, err := officers.Parse(string(raw), env(envm), "muster-agent-")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
		})
	}
}

// TestAShepherdIsOfferedItsOwnToolAndNoMergeTool: officer_shepherd_mark_ready
// and nothing that merges, tags or marks its own; and no other stage gets the
// shepherd's tool.
func TestAShepherdIsOfferedItsOwnToolAndNoMergeTool(t *testing.T) {
	list, err := parseShep(t, shepAuthorDecl(), shepDecl(shepShip()))
	if err != nil {
		t.Fatal(err)
	}
	sh, author := byID(t, list, shepID), byID(t, list, shepAuthor)
	if !officers.ToolOffered(sh, officers.ToolShepherdReady) {
		t.Fatal("a shepherd is not offered officer_shepherd_mark_ready")
	}
	for _, tool := range []string{officers.ToolMergePR, officers.ToolTagRelease, officers.ToolRolloutStatus, officers.ToolMarkReady, officers.ToolCILogs} {
		if officers.ToolOffered(sh, tool) {
			t.Errorf("a shepherd is offered %s", tool)
		}
	}
	if officers.ToolOffered(author, officers.ToolShepherdReady) {
		t.Fatal("a draft officer is offered the shepherd's tool")
	}
	if officers.ToolOffered(officers.Officer{ID: "x:y"}, officers.ToolShepherdReady) {
		t.Fatal("an officer with no ship block is offered the shepherd's tool")
	}
	k := officers.KickoffText(sh, 1)
	for _, want := range []string{officers.ToolShepherdReady, shepLogin, "update-branch", "FAST-FORWARD"} {
		if !strings.Contains(k, want) {
			t.Errorf("the shepherd's first message does not say %q", want)
		}
	}
	if strings.Contains(k, officers.ToolMergePR) {
		t.Errorf("the shepherd's first message names %s", officers.ToolMergePR)
	}
}

// TestShepherdSubject: the item subject is <owner>/<name>#<n>, the repository
// compared without case and the number exactly.
func TestShepherdSubject(t *testing.T) {
	if got := officers.ShepherdSubject("Example-Org/Example-Infra", 42); got != "example-org/example-infra#42" {
		t.Fatalf("ShepherdSubject = %q", got)
	}
	for subject, want := range map[string]bool{
		"example-org/example-infra#42":   true,
		"EXAMPLE-ORG/example-infra#42":   true,
		" example-org/example-infra#42 ": true,
		"example-org/example-infra#420":  false,
		"example-org/example-infra#4":    false,
		"example-org/example-web#42":     false,
		"example-infra#42":               false,
		"example-org/example-infra":      false,
		"example-org/example-infra#042":  false,
	} {
		if got := officers.SameShepherdSubject(subject, shepRepo, 42); got != want {
			t.Errorf("SameShepherdSubject(%q) = %v, want %v", subject, got, want)
		}
	}
}
