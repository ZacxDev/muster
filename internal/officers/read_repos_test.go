package officers_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// TestParseReadsReadReposCanonically: `read_repos` is kept trimmed,
// lowercased and SORTED, so two declarations of one set in another order or
// case parse to the same value (and so to the same grant scope,
// connectors.RepoScope). Absent and empty are nil. The fixture's entries are
// declared out of order and in mixed case, so a parse that kept the input
// order or case would not produce the literal below.
func TestParseReadsReadReposCanonically(t *testing.T) {
	tok := env(map[string]string{"MUSTER_OFFICER_TOKEN_RELEASE_EXAMPLE_WEB": "x"})
	parse := func(rr string) []string {
		t.Helper()
		raw := `[{"id":"release:example-web","hq_url":"http://h/api/v1","repo":"example-org/widget"` + rr + `}]`
		list, err := officers.Parse(raw, tok, "muster-agent-")
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		return list[0].ReadRepos
	}
	want := []string{"example-org/flags-state", "other-org/docs"}
	a := parse(`,"read_repos":[" Other-Org/Docs ","example-org/flags-state"]`)
	b := parse(`,"read_repos":["example-org/FLAGS-state","other-org/docs"]`)
	if !reflect.DeepEqual(a, want) || !reflect.DeepEqual(b, want) {
		t.Fatalf("read_repos parsed to %q and %q, want both %q", a, b, want)
	}
	for _, rr := range []string{``, `,"read_repos":[]`, `,"read_repos":null`} {
		if got := parse(rr); got != nil {
			t.Errorf("%q: read_repos = %q, want nil", rr, got)
		}
	}
	// The cap itself is accepted (a boundary of the refusal below).
	five := `,"read_repos":["example-org/a","example-org/b","example-org/c","example-org/d","example-org/e"]`
	if got := parse(five); len(got) != officers.MaxReadRepos || officers.MaxReadRepos != 5 {
		t.Fatalf("five read_repos parsed to %q (cap %d)", got, officers.MaxReadRepos)
	}
}

// TestParseRefusesABadReadRepos: each bad `read_repos` is a boot refusal that
// names the officer and the reason — the same shape check as `repo`, a
// duplicate in any case, the officer's own `repo` in any case, more than
// MaxReadRepos, and read_repos without `repo`. The full error text is pinned.
func TestParseRefusesABadReadRepos(t *testing.T) {
	tok := env(map[string]string{"MUSTER_OFFICER_TOKEN_RELEASE_EXAMPLE_WEB": "x"})
	const prefix = `MUSTER_OFFICERS[0]: officer release:example-web: `
	for name, c := range map[string]struct{ repo, rr, want string }{
		"not owner/name": {"example-org/widget", `["widget"]`, `read_repos entry "widget" is not a GitHub owner/name`},
		"a URL":          {"example-org/widget", `["https://github.com/example-org/x"]`, `read_repos entry "https://github.com/example-org/x" is not a GitHub owner/name`},
		"extra segment":  {"example-org/widget", `["example-org/x/y"]`, `read_repos entry "example-org/x/y" is not a GitHub owner/name`},
		"duplicate":      {"example-org/widget", `["example-org/x","Example-Org/X"]`, `read_repos lists "Example-Org/X" twice`},
		"is repo":        {"example-org/widget", `["example-org/a","Example-Org/Widget"]`, `read_repos entry "Example-Org/Widget" is the officer's own ` + "`repo`" + `; remove it`},
		"over the cap": {"example-org/widget", `["example-org/a","example-org/b","example-org/c","example-org/d","example-org/e","example-org/f"]`,
			`read_repos lists 6 repositories, over the cap of 5`},
		"no repo": {"", `["example-org/a"]`, "read_repos needs `repo`: the officer's own repository is what a request that names " +
			"no repository is served for, and the GitHub App refuses an agent without one"},
	} {
		t.Run(name, func(t *testing.T) {
			decl := `{"id":"release:example-web","hq_url":"http://h/api/v1","read_repos":` + c.rr
			if c.repo != "" {
				decl += `,"repo":"` + c.repo + `"`
			}
			_, err := officers.Parse(`[`+decl+`}]`, tok, "muster-agent-")
			if err == nil || err.Error() != prefix+c.want {
				t.Fatalf("err = %v, want %q", err, prefix+c.want)
			}
		})
	}
}

// TestParseAcceptsReadReposOnAShippingOfficer: since ship v2 a shipping
// officer may declare read_repos (its push gate admits only reads there, served
// with the deployment App's read-only token; TestReadReposMayStandBesideShip
// pins the overlap refusal). Before v2 this was a boot refusal.
func TestParseAcceptsReadReposOnAShippingOfficer(t *testing.T) {
	decl := map[string]any{"id": shipID, "hq_url": "https://hq.example.com/api/v1", "repo": "example-org/example-web", "ship": fullShip(),
		"read_repos": []string{"example-org/flags-state"}}
	raw, _ := json.Marshal([]any{decl})
	list, err := officers.Parse(string(raw), env(shipEnv(t)), "muster-agent-")
	if err != nil {
		t.Fatalf("read_repos on a shipping officer: %v", err)
	}
	if len(list[0].ReadRepos) != 1 || list[0].Ship == nil {
		t.Fatalf("parsed read_repos %v, ship %v", list[0].ReadRepos, list[0].Ship)
	}
}

// TestTheKickoffNamesTheReadRepos: an officer with read_repos is told it may
// read each of them through the same connector, read-only; an officer without
// any gets exactly the text it got before (no read_repos sentence).
func TestTheKickoffNamesTheReadRepos(t *testing.T) {
	o := officers.Officer{ID: "triage:example-web", MaxInstances: 1, Repo: "example-org/widget"}
	before := officers.KickoffText(o, 1)
	if strings.Contains(before, "You may also READ these repositories") {
		t.Fatalf("an officer with no read_repos is told about some:\n%s", before)
	}
	o.ReadRepos = []string{"example-org/flags-state", "other-org/docs"}
	text := officers.KickoffText(o, 1)
	if !strings.Contains(text, "You may also READ these repositories through the same \"github\" connector") ||
		!strings.Contains(text, "example-org/flags-state, other-org/docs.") || !strings.Contains(text, "READ-ONLY token limited to that one repository") {
		t.Fatalf("the kickoff does not name the read_repos:\n%s", text)
	}
	// The read_repos sentence is one added line; without it the text is
	// byte-identical to the officer's without read_repos.
	i := strings.Index(text, "\nYou may also READ these repositories")
	j := strings.Index(text[i+1:], "\n")
	stripped := text[:i]
	if j >= 0 {
		stripped += text[i+1+j:]
	}
	if stripped != before {
		t.Fatalf("adding read_repos changed more than its own line:\n%s\n---\n%s", text, before)
	}
}
