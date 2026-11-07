package prroute

import (
	"fmt"
	"strings"
	"testing"
)

// Which pull requests and branches a text names (mentions.go). Invented names
// only: owner "acme", repos "infra-config" and "widget-api".

func TestMentionsReadsEveryQualifiedForm(t *testing.T) {
	for _, tc := range []struct {
		text string
		want string
	}{
		{"acme/infra-config#2855: Stage C test and a completed one-shot Job", "[{acme infra-config 2855}]"},
		{"see infra-config#2855 for the rollout", "[{ infra-config 2855}]"},
		{"https://github.com/Acme/Infra-Config/pull/2855/files", "[{acme infra-config 2855}]"},
		{"(acme/infra-config#2855), widget-api#7.", "[{acme infra-config 2855} { widget-api 7}]"},
		{"x/acme/infra-config#12", "[{acme infra-config 12}]"},
		{"acme/infra-config#2855 and again acme/infra-config#2855", "[{acme infra-config 2855}]"},
	} {
		if got := fmt.Sprint(Mentions(tc.text)); got != tc.want {
			t.Errorf("Mentions(%q) = %s; want %s", tc.text, got, tc.want)
		}
	}
}

// 🔴 A bare "#N" names nothing, however it is spelled around.
func TestABareNumberIsNoMention(t *testing.T) {
	for _, text := range []string{
		"#2855", "Re: #2855 is merged", "PR #2855", "pull request 2855", "the job (#2855)", "item 2855#", "acme/#2855",
		"infra-config#2855abc", "infra-config #2855",
	} {
		if got := Mentions(text); len(got) != 0 {
			t.Errorf("Mentions(%q) = %v; want none", text, got)
		}
	}
}

func TestMentionsIsBounded(t *testing.T) {
	var b strings.Builder
	// URLs and short forms together: each pattern alone stays under the cap.
	for i := 1; i <= MaxMentions-5; i++ {
		fmt.Fprintf(&b, "widget-api#%d https://github.com/acme/infra-config/pull/%d ", i, i)
	}
	if got := len(Mentions(b.String())); got != MaxMentions {
		t.Fatalf("%d mentions; want %d", got, MaxMentions)
	}
}

// A mention names a stored pull request only with the same name and number,
// and the same owner when it named one.
func TestAMentionNamesOnlyItsOwnPullRequest(t *testing.T) {
	const stored = "acme/infra-config"
	for _, tc := range []struct {
		m    Mention
		repo string
		pr   int64
		want bool
	}{
		{Mention{"acme", "infra-config", 2855}, stored, 2855, true},
		{Mention{"", "infra-config", 2855}, stored, 2855, true},
		{Mention{"other", "infra-config", 2855}, stored, 2855, false},
		{Mention{"acme", "infra-config", 2856}, stored, 2855, false},
		{Mention{"acme", "infra", 2855}, stored, 2855, false},
		{Mention{"acme", "infra-config", 2855}, "ACME/Infra-Config", 2855, true},
		{Mention{"acme", "infra-config", 2855}, "infra-config", 2855, false},
		{Mention{"acme", "infra-config", 0}, stored, 0, false},
	} {
		if got := tc.m.Names(tc.repo, tc.pr); got != tc.want {
			t.Errorf("%+v.Names(%q, %d) = %v; want %v", tc.m, tc.repo, tc.pr, got, tc.want)
		}
	}
}

func TestABranchIsNamedAsAWholeToken(t *testing.T) {
	const ref = "feat/stage-c-job"
	for text, want := range map[string]bool{
		"pushed feat/stage-c-job for review":     true,
		"branch feat/stage-c-job.":               true,
		"acme:feat/stage-c-job, then":            true,
		"`feat/stage-c-job`":                     true,
		"feat/stage-c-job":                       true,
		"feat/stage-c-job-2 is the next one":     false,
		"origin/feat/stage-c-job":                false,
		"xfeat/stage-c-job":                      false,
		"feat/stage-c-job.v2":                    false,
		"Feat/Stage-C-Job":                       false,
		"nothing here":                           false,
		"feat/stage-c-jo":                        false,
		"feat/stage-c-jobfeat/stage-c-job (dup)": false,
	} {
		if got := NamesBranch(text, ref); got != want {
			t.Errorf("NamesBranch(%q, %q) = %v; want %v", text, ref, got, want)
		}
	}
	// A later occurrence counts when an earlier one is inside a longer name.
	if !NamesBranch("feat/stage-c-job-2 replaced feat/stage-c-job", ref) {
		t.Error("the whole-token occurrence after a longer name was missed")
	}
}

// 🔴 A bare-word branch — what every trunk-deployed session pushes — is never
// named, however plainly the text spells it.
func TestAGenericBranchIsNeverNamed(t *testing.T) {
	for _, ref := range []string{"main", "master", "develop", "trunk", "a-b", "x_y"} {
		if NamesBranch("merged to "+ref+" today", ref) {
			t.Errorf("NamesBranch named the generic branch %q", ref)
		}
	}
	for _, ref := range []string{"feat/x", "fix-login", "wip_cache"} {
		if !NamesBranch("merged "+ref+" today", ref) {
			t.Errorf("NamesBranch missed the distinctive branch %q", ref)
		}
	}
}
