package githubapp

import (
	"context"
	"net/http"
	"reflect"
	"testing"
)

// TestPullRequestCommitsListsTheShasInOrder: GET .../pulls/<n>/commits with the
// repository-scoped pull-requests read token (the fake 404s any other), the
// shas oldest first; a failure is an error (the head order is then unknown).
func TestPullRequestCommitsListsTheShasInOrder(t *testing.T) {
	app, f, _ := newTestApp(t)
	f.mu.Lock()
	if f.pulls == nil {
		f.pulls = map[string]pull{}
	}
	f.pulls["example-org/widget#31/commits"] = pull{body: `[{"sha":"aaaa0001"},{"sha":"bbbb0002"},{"sha":"cccc0003"}]`}
	f.pulls["example-org/widget#32/commits"] = pull{status: http.StatusBadGateway, body: `{"message":"Server Error"}`}
	f.mu.Unlock()
	got, err := app.PullRequestCommits(context.Background(), "example-org/widget", 31)
	if err != nil || !reflect.DeepEqual(got, []string{"aaaa0001", "bbbb0002", "cccc0003"}) {
		t.Fatalf("commits: %v, %v", got, err)
	}
	if _, err := app.PullRequestCommits(context.Background(), "example-org/widget", 32); err == nil {
		t.Fatal("a 502 listing answered no error")
	}
	if _, err := app.PullRequestCommits(context.Background(), "not-a-repo", 31); err == nil {
		t.Fatal("a malformed repository answered no error")
	}
}
