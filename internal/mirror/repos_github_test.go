package mirror_test

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/github"
	"github.com/ZacxDev/muster/internal/mirror"
)

// TestValidGitHubRepoAgreesWithGitHubs: mirror's own copy of the owner/name
// rule (it may not import internal/github, which muster-mirror would then
// link with a redirect-following HTTP client) answers exactly as
// github.ValidRepoName on every boundary: owner 1 and 39 and 40 characters, a
// leading hyphen, name 1, 100 and 101 characters, each allowed and a refused
// character, no slash, two slashes.
func TestValidGitHubRepoAgreesWithGitHubs(t *testing.T) {
	o39, n100 := strings.Repeat("o", 39), strings.Repeat("n", 100)
	cases := []string{"a/b", o39 + "/x", o39 + "o/x", "-a/b", "a-/b", "a/" + n100, "a/" + n100 + "n", "a/b.c_d-e", "a/b c",
		"a/b/c", "ab", "", "a/", "/b", "A9/Z_9", "a_b/c", "a/é"}
	seenTrue, seenFalse := false, false
	for _, c := range cases {
		got, want := mirror.ValidGitHubRepo(c), github.ValidRepoName(c)
		if got != want {
			t.Errorf("%q: mirror %v, github %v", c, got, want)
		}
		seenTrue, seenFalse = seenTrue || want, seenFalse || !want
	}
	if !seenTrue || !seenFalse {
		t.Fatal("instrument check: the cases must include both valid and invalid names")
	}
}

// TestAReposMessagesGitHubReposLandOnTheEntries: the GitHub repository a v2
// host sends beside a window's folder name (Msg.GitHubRepos) lands on that
// window's entry as GitHubRepo, its folder name staying Repo — only for listed
// windows, only an owner/name, lowercased; a CHANGE of the GitHub repository
// alone (the same folder names) nudges the views and is kept; it survives the
// host going offline. Red before Msg.GitHubRepos: no entry had one.
func TestAReposMessagesGitHubReposLandOnTheEntries(t *testing.T) {
	r := newRig(t)
	r.svc.SetNotifyDebounce(time.Millisecond)
	tok, _ := r.store.mint(t, 9, "devbox")
	h := hello("devbox", false)
	h.Windows = []mirror.Window{
		{Session: "work", ID: "@5", Index: 2, Active: true, Claude: true, ClaudeSession: "sess-gh"},
		{Session: "work", ID: "@6", Index: 3},
	}
	host, err := r.rawConnect(t, tok, h)
	if err != nil {
		t.Fatal(err)
	}
	entry := func(window string) (repo, gh string) {
		for _, hv := range r.svc.Hub().Hosts(9) {
			for _, e := range hv.Entries() {
				if e.Window == window {
					return e.Repo, e.GitHubRepo
				}
			}
		}
		return "<unlisted>", "<unlisted>"
	}
	eventually(t, "the host listed", func() bool { r, _ := entry("@5"); return r == "" })
	send := func(m mirror.Msg) {
		b, _ := json.Marshal(m)
		if err := host.ws.Write(context.Background(), websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
	}
	send(mirror.Msg{Type: mirror.TypeRepos, Repos: map[string]string{"@5": "clone-fixture", "@6": "plain-dir"},
		GitHubRepos: map[string]string{"@5": "Fixture-Org/Real-Name", "@6": "not a repo", "@77": "fixture-org/ghost"}})
	eventually(t, "the GitHub repository", func() bool { _, gh := entry("@5"); return gh == "fixture-org/real-name" })
	if repo, _ := entry("@5"); repo != "clone-fixture" {
		t.Fatalf("Repo = %q, want the folder's name kept beside the GitHub repository", repo)
	}
	if got := r.svc.Hub().Hosts(9)[0].GitHubRepos; !maps.Equal(got, map[string]string{"@5": "fixture-org/real-name"}) {
		t.Fatalf("GitHubRepos = %v, want only the listed window's owner/name", got)
	}
	// Only the GitHub repository changes: kept.
	send(mirror.Msg{Type: mirror.TypeRepos, Repos: map[string]string{"@5": "clone-fixture", "@6": "plain-dir"},
		GitHubRepos: map[string]string{"@5": "fixture-org/renamed-repo"}})
	eventually(t, "the changed GitHub repository", func() bool { _, gh := entry("@5"); return gh == "fixture-org/renamed-repo" })
	host.ws.CloseNow()
	eventually(t, "the host offline", func() bool { hs := r.svc.Hub().Hosts(9); return len(hs) == 1 && !hs[0].Online })
	if _, gh := entry("@5"); gh != "fixture-org/renamed-repo" {
		t.Fatalf("an offline host's entry has GitHubRepo %q, want its last", gh)
	}
}
