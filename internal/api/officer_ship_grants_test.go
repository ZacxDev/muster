package api

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// TestShipGrantsDescriptionNamesTheReadReposAtEveryStage: what approving a
// shipping officer's `github` request grants (the approve card,
// connector_requests_list's approving_grants, connector_request_decide's
// grants) names every one of its read_repos at EVERY ship stage — shadow,
// merge and release as well as draft — each as read with the deployment App's
// READ-ONLY token. The expected texts are written out literally. Without
// read_repos no stage's text mentions any.
func TestShipGrantsDescriptionNamesTheReadReposAtEveryStage(t *testing.T) {
	const read = "; reads of acme/other-repo, acme/third-repo with the deployment App's READ-ONLY token"
	nonDraft := func(stage string) string {
		return "officer engineering:example-web's OWN GitHub App (ID 9001), for acme/example-web only, behind muster's push gate: reads; " +
			"pushes ONLY to refs/heads/officer/* (never main, never a tag); opening and editing its own pull requests and commenting; " +
			"marking its own draft ready (officer_mark_ready). Merging and tagging happen only in muster, through the gated " +
			"officer_merge_pr and officer_tag_release tools (stage " + stage + ")"
	}
	draft := "officer engineering:example-web's OWN GitHub App (ID 9001), behind muster's push gate, stage draft: reads; DRAFT pull requests only, " +
		"in acme/example-web (pushes ONLY to refs/heads/officer/engineering-example-web/<item>-<slug>, never main); editing and commenting " +
		"on its own; every pull request checked for forbidden paths, removed volume/database/canary manifests and secret shapes. " +
		"It never marks ready, merges, tags or deletes a branch"
	for stage, bare := range map[string]string{
		officers.StageShadow:  nonDraft("shadow"),
		officers.StageMerge:   nonDraft("merge"),
		officers.StageRelease: nonDraft("release"),
		officers.StageDraft:   draft,
	} {
		t.Run(stage, func(t *testing.T) {
			o := officers.Officer{ID: "engineering:example-web", MaxInstances: 1, Repo: "acme/example-web",
				ReadRepos: []string{"acme/other-repo", "acme/third-repo"},
				Ship:      &officers.Ship{Stage: stage, Base: "main", BranchPrefix: "officer/", AppID: 9001}}
			if got, want := shipGrantsDescription(o), bare+read; got != want {
				t.Fatalf("with read_repos:\n got %q\nwant %q", got, want)
			}
			o.ReadRepos = nil
			if got := shipGrantsDescription(o); got != bare || strings.Contains(got, "reads of") {
				t.Fatalf("without read_repos:\n got %q\nwant %q", got, bare)
			}
		})
	}
}
