package officers_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// TestParseReadsPinRepoReadsToItem: `pin_repo_reads_to_item` parses to
// Officer.PinRepoReadsToItem on BOTH pools of a tiered officer (the routine
// pool reads repositories too), is false when absent or false, and is a boot
// refusal — full text pinned — without `repo` or beside `ship`.
func TestParseReadsPinRepoReadsToItem(t *testing.T) {
	tok := env(map[string]string{"MUSTER_OFFICER_TOKEN_ANOMALY_REPLAY_PROD": "x"})
	parse := func(extra string) ([]officers.Officer, error) {
		raw := `[{"id":"anomaly-replay:prod","hq_url":"http://h/api/v1"` + extra + `}]`
		return officers.Parse(raw, tok, "muster-agent-")
	}
	list, err := parse(`,"repo":"example-org/widget","read_repos":["example-org/docs"],"pin_repo_reads_to_item":true,` +
		`"tiers":{"routine":{"model":"haiku","max_instances":1},"judgement":{"model":"opus","max_instances":1}}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range list[0].Pools() {
		if !p.PinRepoReadsToItem {
			t.Errorf("pool %q is not pinned", p.Tier)
		}
	}
	for _, extra := range []string{`,"repo":"example-org/widget"`, `,"repo":"example-org/widget","pin_repo_reads_to_item":false`} {
		l, err := parse(extra)
		if err != nil {
			t.Fatal(err)
		}
		if l[0].PinRepoReadsToItem {
			t.Errorf("%s: pinned, want not", extra)
		}
	}
	const prefix = `MUSTER_OFFICERS[0]: officer anomaly-replay:prod: `
	if _, err := parse(`,"pin_repo_reads_to_item":true`); err == nil || err.Error() != prefix+
		"pin_repo_reads_to_item needs `repo`: the pinned reads go through the GitHub App, which serves an officer with a repository only" {
		t.Errorf("pin without repo: %v", err)
	}
	if _, err := parse(`,"repo":"example-org/widget","pin_repo_reads_to_item":true,"ship":{"stage":"draft"}`); err == nil || err.Error() != prefix+
		"pin_repo_reads_to_item is refused beside `ship`: a shipping officer's requests are its push gate's, which a pinned read cannot narrow" {
		t.Errorf("pin beside ship: %v", err)
	}
}

// TestItemRepoPinsReadsReplayRepoRefsAndFailsClosed: a held item's pins are
// its payload's replay.repo_refs, lowercased; every malformed or missing form
// is an error (a pinned officer then reads nothing), never an empty success.
func TestItemRepoPinsReadsReplayRepoRefsAndFailsClosed(t *testing.T) {
	const a = "3F9C1E0A7B2D4C6E8F0A1B3C5D7E9F1A2B4C6D8E"
	const b = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"
	got, err := officers.ItemRepoPins(json.RawMessage(`{"alertname":"x","replay":{"case":3,"repo_refs":[` +
		`{"repo":"Example-Org/Widget","sha":"` + a + `"},{"repo":"example-org/docs","sha":" ` + b + ` "}]}}`))
	want := []officers.RepoRef{{Repo: "example-org/widget", SHA: strings.ToLower(a)}, {Repo: "example-org/docs", SHA: b}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("ItemRepoPins = %+v, %v; want %+v", got, err, want)
	}
	for name, payload := range map[string]string{
		"no payload":          ``,
		"null payload":        `null`,
		"not an object":       `[1]`,
		"no replay":           `{"alertname":"x"}`,
		"no repo_refs":        `{"replay":{"case":1}}`,
		"empty repo_refs":     `{"replay":{"repo_refs":[]}}`,
		"a short sha":         `{"replay":{"repo_refs":[{"repo":"example-org/widget","sha":"3f9c1e0"}]}}`,
		"a branch, not a sha": `{"replay":{"repo_refs":[{"repo":"example-org/widget","sha":"main"}]}}`,
		"a bad repository":    `{"replay":{"repo_refs":[{"repo":"widget","sha":"` + b + `"}]}}`,
		"a repository twice":  `{"replay":{"repo_refs":[{"repo":"example-org/widget","sha":"` + b + `"},{"repo":"Example-Org/Widget","sha":"` + strings.ToLower(a) + `"}]}}`,
		"over the cap": `{"replay":{"repo_refs":[` + strings.Repeat(`{"repo":"example-org/x","sha":"`+b+`"},`, officers.MaxRepoRefs) +
			`{"repo":"example-org/y","sha":"` + b + `"}]}}`,
	} {
		if got, err := officers.ItemRepoPins(json.RawMessage(payload)); err == nil {
			t.Errorf("%s: %+v, want an error", name, got)
		}
	}
}

// TestAPinnedOfficersKickoffTeachesOnlyThePinnedRecipe: a pinned officer is
// told the two reads the proxy serves it, at the item's sha, and that git and
// history are refused — and is NOT taught the clone or history recipes, which
// the proxy would refuse. An unpinned officer's text is unchanged.
func TestAPinnedOfficersKickoffTeachesOnlyThePinnedRecipe(t *testing.T) {
	o := officers.Officer{ID: "anomaly-replay:prod", MaxInstances: 1, Repo: "acme/example-web",
		ReadRepos: []string{"acme/other-repo"}, PinRepoReadsToItem: true}
	o.Tiers = &officers.Tiers{Routine: officers.TierPool{Model: "haiku", MaxInstances: 1}, Judgement: officers.TierPool{Model: "opus", MaxInstances: 1}}
	for _, p := range o.Pools() {
		text := officers.KickoffText(p, 1)
		for _, want := range []string{"acme/example-web, acme/other-repo", "replay.repo_refs",
			"/repos/<owner>/<name>/contents/<path>?ref=<sha>", "/repos/<owner>/<name>/git/trees/<sha>?recursive=1",
			"refuses everything else — git clone, commits, compare, pull requests"} {
			if !strings.Contains(text, want) {
				t.Errorf("pool %q: the kickoff does not carry %q:\n%s", p.Tier, want, text)
			}
		}
		for _, banned := range []string{"git clone --depth", "ccd connect-git", "You may also READ these repositories", "compare/<base>"} {
			if strings.Contains(text, banned) {
				t.Errorf("pool %q: the pinned kickoff still teaches %q", p.Tier, banned)
			}
		}
	}
	o.PinRepoReadsToItem = false
	if text := officers.KickoffText(o.Pool(officers.TierJudgement), 1); strings.Contains(text, "replay.repo_refs") ||
		!strings.Contains(text, "git clone --depth 50 https://github.com/acme/example-web.git") {
		t.Errorf("the unpinned kickoff changed:\n%s", text)
	}
}
