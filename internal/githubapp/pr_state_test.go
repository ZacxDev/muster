package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"
)

// PULL-REQUEST STATE (PullRequestState), against the fake GitHub, whose every
// repository is PRIVATE: a pull request is a 404 to a token not minted with
// pull_requests read for that repository, as on GitHub. Fixtures are invented
// (example-org/widget, numbers distinct per case).

const (
	mergedBody   = `{"number":11,"state":"closed","merged":true}`
	unmergedBody = `{"number":12,"state":"closed","merged":false}`
	openBody     = `{"number":13,"state":"open","merged":false}`
)

func TestPullRequestStateReadsClosedMergedUnmergedAndOpen(t *testing.T) {
	app, f, _ := newTestApp(t)
	f.setPull("example-org/widget", 11, pull{body: mergedBody})
	f.setPull("example-org/widget", 12, pull{body: unmergedBody})
	f.setPull("example-org/widget", 13, pull{body: openBody})
	for _, tc := range []struct {
		n    int64
		want PRState
	}{
		{11, PRState{State: "closed", Merged: true}},
		{12, PRState{State: "closed", Merged: false}},
		{13, PRState{State: "open", Merged: false}},
	} {
		got, err := app.PullRequestState(context.Background(), "example-org/widget", tc.n)
		if err != nil || got != tc.want {
			t.Fatalf("#%d: %+v, %v; want %+v", tc.n, got, err, tc.want)
		}
	}
}

// 🔴 REGRESSION (audit of #417, 🟡-1): the read's token must carry
// pull_requests read for THAT repository only. The listing token (metadata
// read) gets a 404 for a private repository's pull request, so a read made
// with it fails — and the feature is inert for every private repository.
func TestPullRequestStateMintsARepoScopedPullRequestsReadToken(t *testing.T) {
	app, f, _ := newTestApp(t)
	f.setPull("example-org/widget", 11, pull{body: mergedBody})
	if _, err := app.PullRequestState(context.Background(), "example-org/widget", 11); err != nil {
		t.Fatalf("read: %v", err)
	}
	ms := f.mintsSoFar()
	if len(ms) != 1 {
		t.Fatalf("%d mint(s); want 1", len(ms))
	}
	var body struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(ms[0].body), &body); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(body.Repositories, []string{"widget"}) {
		t.Errorf("mint repositories %v; want exactly [widget]", body.Repositories)
	}
	if want := map[string]string{"metadata": "read", "pull_requests": "read"}; !reflect.DeepEqual(body.Permissions, want) {
		t.Errorf("mint permissions %v; want exactly %v", body.Permissions, want)
	}
	// Cached: a second read of the same repository mints nothing more.
	if _, err := app.PullRequestState(context.Background(), "example-org/widget", 11); err != nil {
		t.Fatal(err)
	}
	if n := len(f.mintsSoFar()); n != 1 {
		t.Fatalf("%d mints after a second read; want the token reused (1)", n)
	}
}

func TestPullRequestStateFailuresAreErrors(t *testing.T) {
	app, f, _ := newTestApp(t)
	f.setPull("example-org/widget", 21, pull{status: http.StatusBadGateway, body: `{"message":"Server Error"}`})
	f.setPull("example-org/widget", 22, pull{body: `{"number":22,"state":"","merged":false}`})
	for _, tc := range []struct {
		name, repo string
		n          int64
	}{
		{"404", "example-org/widget", 20},
		{"5xx", "example-org/widget", 21},
		{"no state", "example-org/widget", 22},
		{"no installation", "other-org/widget", 1},
	} {
		got, err := app.PullRequestState(context.Background(), tc.repo, tc.n)
		if err == nil {
			t.Errorf("%s: %+v, nil error; want an error", tc.name, got)
		}
	}
}

// The caller's deadline bounds the read: a GitHub that answers late is an
// error well before its answer would arrive.
func TestPullRequestStateHonoursTheDeadline(t *testing.T) {
	app, f, _ := newTestApp(t)
	f.setPull("example-org/widget", 31, pull{body: mergedBody, delay: 3 * time.Second})
	// Mint first, so the deadline below covers the pull read itself.
	f.setPull("example-org/widget", 30, pull{body: openBody})
	if _, err := app.PullRequestState(context.Background(), "example-org/widget", 30); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := app.PullRequestState(ctx, "example-org/widget", 31)
	if err == nil {
		t.Fatal("a read past the deadline returned no error")
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("the read took %s; want it cut at the 150ms deadline", el)
	}
}

// 🟢 (audit of #417): a mint in progress elsewhere is not waited for — the
// read fails at once with ErrMintBusy, so the caller's deadline holds.
func TestPullRequestStateDoesNotWaitForABusyMintLock(t *testing.T) {
	app, f, _ := newTestApp(t)
	f.setPull("example-org/widget", 41, pull{body: mergedBody})
	app.mintMu.Lock()
	start := time.Now()
	_, err := app.PullRequestState(context.Background(), "example-org/widget", 41)
	app.mintMu.Unlock()
	if !errors.Is(err, ErrMintBusy) {
		t.Fatalf("err %v; want ErrMintBusy", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("waited %s on the busy lock", el)
	}
	// Control: with the lock free, the same read succeeds.
	if got, err := app.PullRequestState(context.Background(), "example-org/widget", 41); err != nil || got.State != "closed" {
		t.Fatalf("control: %+v, %v", got, err)
	}
}
