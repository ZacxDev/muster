package officers_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// SHIP v2: `ship.repos` and the `draft` stage. Fixture values are pairwise
// distinct where the config lets them be: two repositories with different
// bases (main, trunk) and forbidden lists, so a rule that read the wrong entry
// cannot pass. The branch prefix cannot differ: the draft stage pins it to
// officer/ (TestADraftShipsBranchPrefixIsPinned).
// ---------------------------------------------------------------------------

const (
	v2Web   = "example-org/example-web"
	v2Infra = "example-org/example-infra"
)

// draftShip is a valid draft-stage declaration with two repositories.
func draftShip() map[string]any {
	return map[string]any{
		"stage": "draft",
		"repos": []any{
			map[string]any{"repo": v2Infra, "base": "trunk", "branch_prefix": "officer/",
				"github_app":      map[string]any{"id": 9001, "account": "example-org:4242"},
				"forbidden_paths": []string{"**/secrets/**", ".sops.yaml", "clusters/prod/apps/{agent-identity,officer-identity}/**"}},
			map[string]any{"repo": v2Web, "github_app": map[string]any{"id": 9001, "account": "example-org:4242"}},
		},
		"notice": map[string]any{"channel": "ch/example-alerts"},
	}
}

func parseDecl(t *testing.T, decl map[string]any, envm map[string]string) ([]officers.Officer, error) {
	t.Helper()
	raw, _ := json.Marshal([]any{decl})
	return officers.Parse(string(raw), env(envm), "muster-agent-")
}

func draftDecl(ship map[string]any) map[string]any {
	return map[string]any{"id": shipID, "hq_url": "https://hq.example.com/api/v1", "repo": v2Web, "ship": ship}
}

// TestParseReadsADraftShipWithSeveralRepos: both entries, in order, each with
// its own base, prefix and forbidden paths; the Ship's own fields are the
// officer `repo`'s entry (example-web: main, officer/), not the first one's;
// no required_checks or review_check is needed; nothing gated is allowed.
func TestParseReadsADraftShipWithSeveralRepos(t *testing.T) {
	list, err := parseDecl(t, draftDecl(draftShip()), shipEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	s := list[0].Ship
	if s == nil || s.Stage != officers.StageDraft || !s.IsDraft() || s.MayMerge() || s.MayRelease() || s.MayMarkReady() {
		t.Fatalf("parsed %v (draft %v merge %v release %v ready %v)", s, s.IsDraft(), s.MayMerge(), s.MayRelease(), s.MayMarkReady())
	}
	if len(s.Repos) != 2 || s.Repos[0].Repo != v2Infra || s.Repos[0].Base != "trunk" || s.Repos[0].BranchPrefix != "officer/" ||
		len(s.Repos[0].ForbiddenPaths) != 3 || s.Repos[1].Repo != v2Web || s.Repos[1].Base != "main" || s.Repos[1].BranchPrefix != "officer/" ||
		len(s.Repos[1].ForbiddenPaths) != 0 {
		t.Fatalf("repos = %+v", s.Repos)
	}
	if s.Base != "main" || s.BranchPrefix != "officer/" || s.AppID != 9001 || s.AppAccount.ID != 4242 {
		t.Fatalf("the Ship's own fields are not the officer repo's entry: base %q prefix %q app %d account %d", s.Base, s.BranchPrefix, s.AppID, s.AppAccount.ID)
	}
	if r, ok := s.RepoFor(v2Web, "EXAMPLE-ORG/example-infra"); !ok || r.Base != "trunk" {
		t.Fatalf("RepoFor (case-folded) = %+v, %v", r, ok)
	}
	if _, ok := s.RepoFor(v2Web, "example-org/elsewhere"); ok {
		t.Fatal("RepoFor answers a repository the officer does not ship to")
	}
}

// TestTheSingleRepoShipFormIsUnchanged: the pre-v2 declaration parses to the
// same policy it did, now with exactly one Repos entry mirroring it.
func TestTheSingleRepoShipFormIsUnchanged(t *testing.T) {
	list, err := parseShip(t, fullShip(), shipEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	s := list[0].Ship
	want := []officers.ShipRepo{{Repo: v2Web, Base: "main", BranchPrefix: "officer/", AppID: 9001, AppAccount: s.AppAccount}}
	if !reflect.DeepEqual(s.Repos, want) || s.AppAccount.Login != "example-org" || s.AppAccount.ID != 4242 {
		t.Fatalf("repos = %+v, want %+v", s.Repos, want)
	}
	if !s.MayMarkReady() || s.IsDraft() {
		t.Fatal("a release-stage officer lost officer_mark_ready")
	}
}

// TestParseRefusesABadDraftShip: each refusal names what is wrong. The
// control is draftShip() itself, which parses (the test above).
func TestParseRefusesABadDraftShip(t *testing.T) {
	repoEntry := func(repo string, app int64) map[string]any {
		return map[string]any{"repo": repo, "github_app": map[string]any{"id": app, "account": "example-org:4242"}}
	}
	for name, c := range map[string]struct {
		edit func(m map[string]any)
		want string
	}{
		"four repos": {func(m map[string]any) {
			m["repos"] = []any{repoEntry(v2Web, 9001), repoEntry("example-org/a", 9001), repoEntry("example-org/b", 9001), repoEntry("example-org/c", 9001)}
		}, "repos lists 4 repositories; 1..3"},
		"no repos entries": {func(m map[string]any) { m["repos"] = []any{} }, "repos lists 0 repositories"},
		"repo not listed":  {func(m map[string]any) { m["repos"] = []any{repoEntry(v2Infra, 9001)} }, "does not list the officer's `repo`"},
		"a repo twice": {func(m map[string]any) {
			m["repos"] = []any{repoEntry(v2Web, 9001), repoEntry("Example-Org/Example-Web", 9001)}
		}, "twice"},
		"two apps": {func(m map[string]any) {
			m["repos"] = []any{repoEntry(v2Web, 9001), repoEntry(v2Infra, 9002)}
		}, "an officer has ONE App"},
		"top-level base beside repos": {func(m map[string]any) { m["base"] = "main" }, "remove the top-level base"},
		"top-level app beside repos": {func(m map[string]any) {
			m["github_app"] = map[string]any{"id": 9001, "account": "example-org:4242"}
		}, "remove the top-level github_app"},
		"an unknown repos key": {func(m map[string]any) {
			e := repoEntry(v2Web, 9001)
			e["protected"] = true
			m["repos"] = []any{e}
		}, "unknown field"},
		"a bad glob": {func(m map[string]any) {
			e := repoEntry(v2Web, 9001)
			e["forbidden_paths"] = []string{"clusters/{a,b"}
			m["repos"] = []any{e}
		}, "unbalanced"},
		"a class glob": {func(m map[string]any) {
			e := repoEntry(v2Web, 9001)
			e["forbidden_paths"] = []string{"nodes/[ab]*"}
			m["repos"] = []any{e}
		}, "not supported"},
		"draft with a rollout": {func(m map[string]any) {
			m["rollout"] = map[string]any{"health_url": "https://h.example.test/", "rollback": "off"}
		}, "watches no rollout"},
		"draft without a notice": {func(m map[string]any) { delete(m, "notice") }, "needs a notice block"},
	} {
		m := draftShip()
		c.edit(m)
		e := shipEnv(t)
		if _, err := parseDecl(t, draftDecl(m), e); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to name %q", name, err, c.want)
		}
	}
}

// TestADraftShipsBranchPrefixIsPinned: a draft-stage officer's branches are
// exactly officer/…, in both the repos and the single-repository form,
// because the target repositories' own walls (their CI) key on that literal:
// any other prefix is refused at boot, naming the key and the value it must
// be. Controls: officer/ spelled out, and the default, parse; another stage
// keeps its configurable prefix.
func TestADraftShipsBranchPrefixIsPinned(t *testing.T) {
	const want = `branch_prefix "bot/" must be exactly "officer/" on the draft stage`
	m := draftShip()
	m["repos"].([]any)[0].(map[string]any)["branch_prefix"] = "bot/"
	if _, err := parseDecl(t, draftDecl(m), shipEnv(t)); err == nil || !strings.Contains(err.Error(), want) ||
		!strings.Contains(err.Error(), "repos[0].") {
		t.Errorf("repos form: err = %v, want it to name repos[0]. and %q", err, want)
	}
	single := map[string]any{"stage": "draft", "branch_prefix": "bot/", "github_app": map[string]any{"id": 9001, "account": "example-org:4242"},
		"notice": map[string]any{"channel": "ch/example-alerts"}}
	if _, err := parseDecl(t, draftDecl(single), shipEnv(t)); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("single form: err = %v, want %q", err, want)
	}
	for _, p := range []string{"officer/", ""} {
		s := map[string]any{"stage": "draft", "github_app": map[string]any{"id": 9001, "account": "example-org:4242"},
			"notice": map[string]any{"channel": "ch/example-alerts"}}
		if p != "" {
			s["branch_prefix"] = p
		}
		list, err := parseDecl(t, draftDecl(s), shipEnv(t))
		if err != nil || list[0].Ship.BranchPrefix != "officer/" {
			t.Errorf("control %q: err = %v", p, err)
		}
	}
	rel := fullShip()
	rel["branch_prefix"] = "bot/"
	if list, err := parseShip(t, rel, shipEnv(t)); err != nil || list[0].Ship.BranchPrefix != "bot/" {
		t.Errorf("control: a release-stage prefix bot/ no longer parses: %v", err)
	}
}

// TestTheV2KeysAreRefusedOutsideTheDraftStage: forbidden_paths and several
// repositories would be ignored by a shadow/merge/release officer (its rules
// are unchanged), so each is a refusal, not an inert key. Control: the same
// release block without them parses.
func TestTheV2KeysAreRefusedOutsideTheDraftStage(t *testing.T) {
	if _, err := parseShip(t, fullShip(), shipEnv(t)); err != nil {
		t.Fatalf("control: %v", err)
	}
	m := fullShip()
	m["forbidden_paths"] = []string{".sops.yaml"}
	if _, err := parseShip(t, m, shipEnv(t)); err == nil || !strings.Contains(err.Error(), "forbidden_paths is enforced by the draft stage only") {
		t.Errorf("forbidden_paths on a release officer: %v", err)
	}
	m = fullShip()
	for _, k := range []string{"github_app"} {
		delete(m, k)
	}
	m["repos"] = []any{
		map[string]any{"repo": v2Web, "github_app": map[string]any{"id": 9001, "account": "example-org:4242"}},
		map[string]any{"repo": v2Infra, "github_app": map[string]any{"id": 9001, "account": "example-org:4242"}},
	}
	if _, err := parseShip(t, m, shipEnv(t)); err == nil || !strings.Contains(err.Error(), "several repos are supported on the draft stage only") {
		t.Errorf("two repos on a release officer: %v", err)
	}
}

// TestADraftOfficerIsOfferedNoMergeReadyTagOrRolloutTool: the four tools are
// not offered to a draft officer (so not listed, pre-allowed or dispatched);
// a merge-stage officer is offered them as before (control).
func TestADraftOfficerIsOfferedNoMergeReadyTagOrRolloutTool(t *testing.T) {
	list, err := parseDecl(t, draftDecl(draftShip()), shipEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	merge := officers.Officer{ID: "engineering:x", Ship: &officers.Ship{Stage: officers.StageMerge}}
	for _, tool := range []string{officers.ToolMergePR, officers.ToolTagRelease, officers.ToolRolloutStatus, officers.ToolMarkReady} {
		if officers.ToolOffered(list[0], tool) {
			t.Errorf("a draft officer is offered %s", tool)
		}
		if !officers.ToolOffered(merge, tool) {
			t.Errorf("control: a merge-stage officer is not offered %s", tool)
		}
	}
	text := officers.KickoffText(list[0], 1)
	for _, want := range []string{"DRAFT pull requests", officers.BodyFirstLine(shipID, 0)[:len("Officer: "+shipID+" · item ")],
		"## Root cause, ## Evidence, ## Test, ## Rollback", v2Infra + " (branches officer/engineering-example-web/<item>-<slug> off origin/trunk)",
		v2Web + " (branches officer/engineering-example-web/<item>-<slug> off origin/main)",
		"after every push to a branch with an open pull request, muster reads that comparison again and CLOSES the pull request"} {
		if !strings.Contains(text, want) {
			t.Errorf("the draft officer's kickoff does not say %q:\n%s", want, text)
		}
	}
	for _, never := range []string{officers.ToolMergePR + "{pr}", officers.ToolMarkReady + "{pr}", officers.ToolTagRelease} {
		if strings.Contains(text, never) {
			t.Errorf("the draft officer's kickoff teaches %q", never)
		}
	}
}

// TestShipCoexistsWithTiersAndOnlyTheJudgementPoolShips: tiers and llm beside
// ship parse; the judgement pool holds the Ship (so the gate, the App token
// and the ship tools), the routine pool holds none.
func TestShipCoexistsWithTiersAndOnlyTheJudgementPoolShips(t *testing.T) {
	decl := draftDecl(draftShip())
	decl["tiers"] = map[string]any{"routine": map[string]any{"model": "vendor/model-flash", "max_instances": 2},
		"judgement": map[string]any{"model": "opus", "max_instances": 1}}
	decl["llm"] = map[string]any{"daily_budget_usd": 1}
	e := shipEnv(t)
	e[officers.LLMKeyEnvPrefix+officers.TokenEnvSuffix(shipID)] = "sk-fixture-llm"
	list, err := parseDecl(t, decl, e)
	if err != nil {
		t.Fatalf("tiers and llm beside ship: %v", err)
	}
	judge, routine := pools(t, list[0])
	if judge.Ship == nil || !judge.Ship.IsDraft() {
		t.Fatal("the judgement pool lost the ship policy")
	}
	if routine.Ship != nil || routine.CILogs != nil {
		t.Fatalf("the routine pool holds a ship policy: %v", routine.Ship)
	}
	// The routine pool is told the read-only recipe, not the ship one.
	if txt := officers.KickoffText(routine, 1); strings.Contains(txt, "DRAFT pull requests") || !strings.Contains(txt, "You may READ it") {
		t.Fatalf("the routine pool's kickoff:\n%s", txt)
	}
}

// TestReadReposMayStandBesideShip: read_repos parses beside ship; an entry
// that is one of ship.repos is refused (it would have two answers).
func TestReadReposMayStandBesideShip(t *testing.T) {
	decl := draftDecl(draftShip())
	decl["read_repos"] = []string{"example-org/flags-state"}
	list, err := parseDecl(t, decl, shipEnv(t))
	if err != nil {
		t.Fatalf("read_repos beside ship: %v", err)
	}
	if !reflect.DeepEqual(list[0].ReadRepos, []string{"example-org/flags-state"}) || list[0].Ship == nil {
		t.Fatalf("read_repos = %v, ship %v", list[0].ReadRepos, list[0].Ship)
	}
	decl["read_repos"] = []string{"example-org/flags-state", v2Infra}
	if _, err := parseDecl(t, decl, shipEnv(t)); err == nil || !strings.Contains(err.Error(), "is one of ship.repos") {
		t.Fatalf("a read_repos entry that is a ship repo: %v", err)
	}
}

// TestMatchForbidden pins the glob shapes on the paths the plan forbids, with
// a near miss beside each hit.
func TestMatchForbidden(t *testing.T) {
	for _, c := range []struct {
		pat, path string
		want      bool
	}{
		{"**/secrets/**", "clusters/prod/apps/x/secrets/db.yaml", true},
		{"**/secrets/**", "secrets/db.yaml", true},
		{"**/secrets/**", "clusters/prod/apps/x/secretsauce/db.yaml", false},
		{"**/*.enc.yaml", "a/b/c.enc.yaml", true},
		{"**/*.enc.yaml", "c.enc.yaml", true},
		{"**/*.enc.yaml", "a/b/c.enc.yaml.bak", false},
		{".sops.yaml", ".sops.yaml", true},
		{".sops.yaml", "sub/.sops.yaml", true},
		{".sops.yaml", "sub/x.sops.yaml", false},
		{"clusters/production/flux-system/**", "clusters/production/flux-system/gotk-sync.yaml", true},
		{"clusters/production/flux-system/**", "clusters/production/flux-systemx/a.yaml", false},
		{"nodes/**", "nodes/cp-1/config.yaml", true},
		{"nodes/**", "a/nodes/x.yaml", false},
		{"clusters/production/apps/{agent-identity,officer-identity,civ-hq-pool}/**", "clusters/production/apps/officer-identity/role.yaml", true},
		{"clusters/production/apps/{agent-identity,officer-identity,civ-hq-pool}/**", "clusters/production/apps/officer-identityx/role.yaml", false},
		{"clusters/production/apps/tekton-builds/gitops-ci*", "clusters/production/apps/tekton-builds/gitops-ci-pipeline.yaml", true},
		{"clusters/production/apps/tekton-builds/gitops-ci*", "clusters/production/apps/tekton-builds/sub/gitops-ci.yaml", false},
		{"k0sctl.*.yaml", "k0sctl.submodel-a.yaml", true},
		{"k0sctl.*.yaml", "k0sctl.yaml", false},
		{".claude/**", ".claude/skills/x/SKILL.md", true},
		{".claude/**", "docs/.claude", false},
		{".githooks/", ".githooks/pre-push", true},
		{"scripts/gitops-delta-gate.sh", "scripts/gitops-delta-gate.sh", true},
		{"scripts/gitops-delta-gate.sh", "scripts/gitops-delta-gate.shx", false},
		// The shapes a second repository's list needs (an overlay or a Flux
		// file can widen a role without touching the app's own directory).
		{"overlays/*/read-role/**", "overlays/site-a/read-role/patch.yaml", true},
		{"overlays/*/read-role/**", "overlays/site-a/sub/read-role/patch.yaml", false},
		{"clusters/*-overlay/*read-role*", "clusters/site-a-overlay/apps-read-role.yaml", true},
		{"clusters/*-overlay/*read-role*", "clusters/site-a-overlay/apps.yaml", false},
		{".github/**", ".github/workflows/ci.yml", true},
		{"scripts/ci/**", "scripts/ci/check.sh", true},
		{"scripts/ci/**", "scripts/cid/check.sh", false},
		{"?.yaml", "a.yaml", true},
		{"?.yaml", "ab.yaml", false},
	} {
		if got := officers.MatchForbidden(c.pat, c.path); got != c.want {
			t.Errorf("MatchForbidden(%q, %q) = %v, want %v", c.pat, c.path, got, c.want)
		}
	}
	// A pattern that does not compile matches everything (a wall, not a gap).
	if !officers.MatchForbidden("a/{b", "anything.go") {
		t.Error("a broken pattern let a path through")
	}
}

// TestBranchItem: the draft branch shape and its item number.
func TestBranchItem(t *testing.T) {
	for _, c := range []struct {
		branch string
		item   int
		ok     bool
	}{
		{"officer/engineering-example-web/134-fix-oom", 134, true},
		{"officer/engineering-example-web/7-x", 7, true},
		{"officer/engineering-example-web/0-x", 0, false},
		{"officer/engineering-example-web/134", 0, false},
		{"officer/engineering-example-web/fix-134", 0, false},
		{"officer/other-officer/134-fix", 0, false},
		{"officer/engineering-example-web/134-fix/more", 0, false},
		{"officer/134-fix", 0, false},
		{"officer/engineering-example-web/134-fix.lock", 0, false},
	} {
		n, ok := officers.BranchItem("officer/", shipID, c.branch)
		if n != c.item || ok != c.ok {
			t.Errorf("BranchItem(%q) = %d, %v; want %d, %v", c.branch, n, ok, c.item, c.ok)
		}
	}
}

// TestParseReadsAForbiddenPathsFile: `forbidden_paths_file` is kept per
// repository beside the inline list; a path that is absolute, climbs, or is
// empty after trimming is refused, and so is the key outside the draft stage.
func TestParseReadsAForbiddenPathsFile(t *testing.T) {
	m := draftShip()
	repos := m["repos"].([]any)
	repos[0].(map[string]any)["forbidden_paths_file"] = "scripts/ci/officer-forbidden-paths.txt"
	list, err := parseDecl(t, draftDecl(m), shipEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	if r := list[0].Ship.Repos[0]; r.ForbiddenPathsFile != "scripts/ci/officer-forbidden-paths.txt" || len(r.ForbiddenPaths) != 3 {
		t.Fatalf("repo 0 = %+v, want the file beside the three inline globs", r)
	}
	if list[0].Ship.Repos[1].ForbiddenPathsFile != "" {
		t.Fatal("the other repository got the file")
	}
	for _, bad := range []string{"/etc/list.txt", "../list.txt", "a/../b.txt", "  ", "dir/"} {
		m := draftShip()
		m["repos"].([]any)[0].(map[string]any)["forbidden_paths_file"] = bad
		if _, err := parseDecl(t, draftDecl(m), shipEnv(t)); err == nil || !strings.Contains(err.Error(), "forbidden_paths_file") {
			t.Errorf("forbidden_paths_file %q: err = %v, want a refusal naming it", bad, err)
		}
	}
	rel := fullShip()
	rel["forbidden_paths_file"] = "scripts/ci/list.txt"
	if _, err := parseShip(t, rel, shipEnv(t)); err == nil || !strings.Contains(err.Error(), "draft stage only") {
		t.Errorf("forbidden_paths_file on a release officer: %v", err)
	}
}
