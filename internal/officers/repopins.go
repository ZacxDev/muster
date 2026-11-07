package officers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/ZacxDev/muster/internal/github"
)

// RepoRef is one repository a pinned officer's held item lets it read, and the
// one commit it may read it at (Officer.PinRepoReadsToItem).
type RepoRef struct {
	Repo string `json:"repo"`
	SHA  string `json:"sha"`
}

// MaxRepoRefs bounds an item's pins.
const MaxRepoRefs = 8

var repoRefSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ItemRepoPins reads a held item's repository pins from its payload:
// `replay.repo_refs`, a list of {"repo": "owner/name", "sha": "<40 hex>"}.
//
// 🔴 IT FAILS CLOSED. No payload, no `replay`, no `repo_refs`, an empty list, a
// bad repository or commit, a repository named twice, or more than
// MaxRepoRefs entries is an error, and the caller refuses every read: a
// pinned officer reads nothing it was not pinned to.
func ItemRepoPins(payload json.RawMessage) ([]RepoRef, error) {
	if len(bytes.TrimSpace(payload)) == 0 || string(bytes.TrimSpace(payload)) == "null" {
		return nil, errors.New("the item you hold has no payload, so it pins no repository (replay.repo_refs)")
	}
	var env struct {
		Replay *struct {
			RepoRefs []RepoRef `json:"repo_refs"`
		} `json:"replay"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, fmt.Errorf("the item's payload does not decode: %v", err)
	}
	if env.Replay == nil || len(env.Replay.RepoRefs) == 0 {
		return nil, errors.New("the item you hold pins no repository (its payload has no replay.repo_refs)")
	}
	if len(env.Replay.RepoRefs) > MaxRepoRefs {
		return nil, fmt.Errorf("the item pins %d repositories, over the cap of %d", len(env.Replay.RepoRefs), MaxRepoRefs)
	}
	seen := map[string]bool{}
	out := make([]RepoRef, 0, len(env.Replay.RepoRefs))
	for _, r := range env.Replay.RepoRefs {
		repo, sha := strings.ToLower(strings.TrimSpace(r.Repo)), strings.ToLower(strings.TrimSpace(r.SHA))
		if !github.ValidRepoName(repo) {
			return nil, fmt.Errorf("replay.repo_refs entry %q is not a GitHub owner/name", r.Repo)
		}
		if !repoRefSHA.MatchString(sha) {
			return nil, fmt.Errorf("replay.repo_refs entry %s: sha %q is not a full 40-hex commit id", repo, r.SHA)
		}
		if seen[repo] {
			return nil, fmt.Errorf("replay.repo_refs names %s twice", repo)
		}
		seen[repo] = true
		out = append(out, RepoRef{Repo: repo, SHA: sha})
	}
	return out, nil
}
