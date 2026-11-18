// Package github is muster's small GitHub REST client: the repository and
// pull-request shapes, the pull-request listing the GitHub App reads with an
// installation token (internal/githubapp), and the API base both follow.
//
// ⚠ IT HELD THE PER-USER OAUTH CONNECTION TOO — the encrypted token store, the
// OAuth web flow and the token-based listings the Repos tab used. All of that
// was removed with the Repos tab: the GitHub App is muster's GitHub identity.
// The `github_connection` table that store wrote is dropped by migration 0059.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// httpClient bounds GitHub API/OAuth calls.
var httpClient = &http.Client{Timeout: 15 * time.Second}

// apiBaseURL is the GitHub REST API base. A package var so tests can point it at
// an httptest server.
var apiBaseURL = "https://api.github.com"

// UseAPIBaseURL points every GitHub call at another API base. Only the e2eterm
// browser-test build calls it (its fake GitHub); a deployable binary never does.
func UseAPIBaseURL(u string) { apiBaseURL = u }

// APIBaseURL is the API base every GitHub call uses (UseAPIBaseURL's value), so
// another client (internal/githubapp) follows the same override.
func APIBaseURL() string { return apiBaseURL }

// Repo is a repository as listed for the dispatch dialog's repository picker.
type Repo struct {
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
	HTMLURL  string `json:"html_url"`
	CloneURL string `json:"clone_url"`
}

// PullRequest is a repository pull request, flattened for the worker tool. The
// GitHub API nests author/branch under user/head/base; ListPullRequestsAt maps
// those into the flat fields below.
type PullRequest struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	State     string `json:"state"`
	HTMLURL   string `json:"html_url"`
	User      string `json:"user"`
	HeadRef   string `json:"head_ref"`
	BaseRef   string `json:"base_ref"`
	Draft     bool   `json:"draft"`
	UpdatedAt string `json:"updated_at"`
}

// ListPullRequestsAt returns a repo's pull requests from the API at base
// (newest-updated first, up to 50). repo is "owner/name". state is one of
// "open", "closed", "all"; an empty or unrecognized value defaults to "open".
// The token stays server-side — it is never returned to the caller.
func ListPullRequestsAt(ctx context.Context, base, token, repo, state string) ([]PullRequest, error) {
	switch state {
	case "open", "closed", "all":
	default:
		state = "open"
	}
	// raw mirrors the GitHub PR JSON shape we care about, before flattening.
	var raw []struct {
		Number    int    `json:"number"`
		Title     string `json:"title"`
		State     string `json:"state"`
		HTMLURL   string `json:"html_url"`
		Draft     bool   `json:"draft"`
		UpdatedAt string `json:"updated_at"`
		User      struct {
			Login string `json:"login"`
		} `json:"user"`
		Head struct {
			Ref string `json:"ref"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	q := url.Values{}
	q.Set("state", state)
	q.Set("per_page", "50")
	q.Set("sort", "updated")
	q.Set("direction", "desc")
	u := base + "/repos/" + repo + "/pulls?" + q.Encode()
	if err := apiGet(ctx, token, u, &raw); err != nil {
		return nil, err
	}
	out := make([]PullRequest, 0, len(raw))
	for _, p := range raw {
		out = append(out, PullRequest{
			Number:    p.Number,
			Title:     p.Title,
			State:     p.State,
			HTMLURL:   p.HTMLURL,
			User:      p.User.Login,
			HeadRef:   p.Head.Ref,
			BaseRef:   p.Base.Ref,
			Draft:     p.Draft,
			UpdatedAt: p.UpdatedAt,
		})
	}
	return out, nil
}

func apiGet(ctx context.Context, token, urlStr string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("github api %s: status %d", urlStr, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
