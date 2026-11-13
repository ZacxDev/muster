package officers_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

func noEnv(string) string { return "" }

// TestParseReadsAStandingReadOnABuiltinOfficer: a builtin officer may declare
// read_grant "standing" with read_repos and NO repo (it uses no App token);
// its read_repos are then its standing read and none is under a grant, and
// read_at "release" is kept for StampReleaseTag.
func TestParseReadsAStandingReadOnABuiltinOfficer(t *testing.T) {
	list, err := officers.Parse(`[{"id":"marketing:example","backend":"builtin","read_repos":["ZacxDev/Muster"],
	   "read_grant":"standing","read_at":"release"}]`, noEnv, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	o := list[0]
	if o.ReadGrant != officers.ReadGrantStanding || o.ReadAt != officers.ReadAtRelease || o.Repo != "" {
		t.Fatalf("parsed %+v", o)
	}
	if got := o.StandingReadRepos(); !reflect.DeepEqual(got, []string{"zacxdev/muster"}) {
		t.Fatalf("StandingReadRepos = %q", got)
	}
	if got := o.ApprovalReadRepos(); got != nil {
		t.Fatalf("ApprovalReadRepos = %q, want nil: a standing read is not under the grant", got)
	}
	// The same officer without "standing": read_repos needs repo again.
	if _, err := officers.Parse(`[{"id":"marketing:example","backend":"builtin","read_repos":["zacxdev/muster"]}]`, noEnv, "muster-agent-"); err == nil ||
		!strings.Contains(err.Error(), "read_repos needs `repo`") {
		t.Fatalf("read_repos without repo or standing: %v, want the needs-repo refusal", err)
	}
}

// TestParseRefusesAStandingReadOutsideItsScope: every declaration the standing
// read does not cover is a boot refusal naming it.
func TestParseRefusesAStandingReadOutsideItsScope(t *testing.T) {
	hqEnv := env(map[string]string{"MUSTER_OFFICER_TOKEN_TRIAGE_EXAMPLE_WEB": "x"})
	for _, c := range []struct {
		name, raw, want string
		getenv          func(string) string
	}{
		{"an HQ officer", `[{"id":"triage:example-web","hq_url":"http://h/api/v1","repo":"example-org/widget","read_repos":["example-org/docs"],"read_grant":"standing"}]`,
			`read_grant "standing" is for a builtin officer only`, hqEnv},
		{"an HQ officer (explicit backend)", `[{"id":"triage:example-web","backend":"hq","hq_url":"http://h/api/v1","repo":"example-org/widget","read_repos":["example-org/docs"],"read_grant":"standing"}]`,
			`read_grant "standing" is for a builtin officer only`, hqEnv},
		{"no read_repos", `[{"id":"marketing:example","backend":"builtin","read_grant":"standing"}]`,
			`read_grant "standing" needs read_repos`, noEnv},
		{"an unknown grant", `[{"id":"marketing:example","backend":"builtin","read_repos":["a/b"],"read_grant":"always"}]`,
			`read_grant "always" is not "standing"`, noEnv},
		{"read_at without standing", `[{"id":"marketing:example","backend":"builtin","repo":"a/c","read_repos":["a/b"],"read_at":"release"}]`,
			`read_at needs read_grant "standing"`, noEnv},
		{"an unknown read_at", `[{"id":"marketing:example","backend":"builtin","read_repos":["a/b"],"read_grant":"standing","read_at":"main"}]`,
			`read_at "main" is not "release"`, noEnv},
		{"a bad repository name", `[{"id":"marketing:example","backend":"builtin","read_repos":["not a repo"],"read_grant":"standing"}]`,
			`is not a GitHub owner/name`, noEnv},
		{"over the cap", `[{"id":"marketing:example","backend":"builtin","read_repos":["a/1","a/2","a/3","a/4","a/5","a/6"],"read_grant":"standing"}]`,
			`over the cap`, noEnv},
	} {
		_, err := officers.Parse(c.raw, c.getenv, "muster-agent-")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want a refusal containing %q", c.name, err, c.want)
		}
	}
}

// TestStandingReadReposIsTheOneRule: only a builtin officer declaring
// "standing" with no ship gets a standing read — an HQ officer or a shipping
// one whose value says "standing" (built outside Parse) gets none, and its
// read_repos stay under the grant.
func TestStandingReadReposIsTheOneRule(t *testing.T) {
	rr := []string{"example-org/docs"}
	for _, c := range []struct {
		name     string
		o        officers.Officer
		standing bool
	}{
		{"builtin standing", officers.Officer{Backend: officers.BackendBuiltin, ReadGrant: officers.ReadGrantStanding, ReadRepos: rr}, true},
		{"builtin, per-instance", officers.Officer{Backend: officers.BackendBuiltin, ReadRepos: rr}, false},
		{"HQ standing", officers.Officer{Backend: officers.BackendHQ, ReadGrant: officers.ReadGrantStanding, ReadRepos: rr}, false},
		{"no backend (HQ) standing", officers.Officer{ReadGrant: officers.ReadGrantStanding, ReadRepos: rr}, false},
		{"builtin standing, shipping", officers.Officer{Backend: officers.BackendBuiltin, ReadGrant: officers.ReadGrantStanding,
			ReadRepos: rr, Ship: &officers.Ship{Stage: officers.StageDraft}}, false},
	} {
		s, a := c.o.StandingReadRepos(), c.o.ApprovalReadRepos()
		if c.standing && (!reflect.DeepEqual(s, rr) || a != nil) || !c.standing && (s != nil || !reflect.DeepEqual(a, rr)) {
			t.Errorf("%s: standing %q, approval %q", c.name, s, a)
		}
	}
}

// TestReleaseTagOfAndStampReleaseTag: a release version becomes its tag; a
// non-release build has none; only read_at "release" officers are stamped.
func TestReleaseTagOfAndStampReleaseTag(t *testing.T) {
	for v, want := range map[string]string{"0.221.0": "v0.221.0", "v1.2.3": "v1.2.3", " 10.20.30 ": "v10.20.30",
		"dev": "", "": "", "0.221": "", "0.221.0-rc1": "", "0.221.0\nx": ""} {
		if got := officers.ReleaseTagOf(v); got != want {
			t.Errorf("ReleaseTagOf(%q) = %q, want %q", v, got, want)
		}
	}
	list := []officers.Officer{{ID: "a:b", ReadAt: officers.ReadAtRelease}, {ID: "c:d"}}
	got := officers.StampReleaseTag(list, "0.221.0")
	if got[0].ReleaseTag != "v0.221.0" || got[1].ReleaseTag != "" || list[0].ReleaseTag != "" {
		t.Fatalf("stamped %+v (input %+v)", got, list)
	}
}

// TestTheKickoffNamesTheStandingReadAtTheReleaseTag pins the standing read's
// kickoff text: the repositories, no approval, the tag, and both recipes with
// the proxy URL written out (git is not routed without a grant).
func TestTheKickoffNamesTheStandingReadAtTheReleaseTag(t *testing.T) {
	o := officers.Officer{ID: "marketing:example", MaxInstances: 1, Backend: officers.BackendBuiltin,
		ReadRepos: []string{"zacxdev/muster"}, ReadGrant: officers.ReadGrantStanding, ReadAt: officers.ReadAtRelease, ReleaseTag: "v0.221.0"}
	text := officers.KickoffText(o, 1)
	for _, want := range []string{
		"Public repositories you may READ with no approval: zacxdev/muster.",
		"do not connector_request for them",
		"Read them at tag v0.221.0: v0.221.0 is the release of muster this server runs",
		"Cite the file paths you read",
		"`git -c \"http.$MUSTER_CONNECT_URL/.extraHeader=X-Muster-Token: $MUSTER_HOOK_TOKEN\" -c \"http.$MUSTER_CONNECT_URL/.followRedirects=false\" clone --depth 1 --branch v0.221.0 \"$MUSTER_CONNECT_URL/connect/github/github.com/<owner>/<name>.git\"`",
		"/connect/github/api.github.com/repos/<owner>/<name>/contents/<path>?ref=v0.221.0\"`",
		"A push, or any other write, to them is refused.",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("the kickoff lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "You may also READ these repositories") {
		t.Fatal("a standing read is described as under the grant too")
	}
	// No release tag (a dev build): the default branch, said so.
	o.ReleaseTag = ""
	text = officers.KickoffText(o, 1)
	if !strings.Contains(text, "muster could not name its release tag") || strings.Contains(text, "--branch") || strings.Contains(text, "?ref=") {
		t.Fatalf("a dev build's kickoff:\n%s", text)
	}
	// An HQ officer whose value says standing is told nothing of it.
	o.Backend, o.ReleaseTag = officers.BackendHQ, "v0.221.0"
	if text = officers.KickoffText(o, 1); strings.Contains(text, "Public repositories you may READ") {
		t.Fatalf("an HQ officer was told of a standing read:\n%s", text)
	}
}
