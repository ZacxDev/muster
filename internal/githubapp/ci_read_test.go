package githubapp

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/cichecks"
)

// THE CI READ (CommitChecks), against the fake GitHub, whose repositories are
// private: a commit's statuses and check runs are a 404 to a token not minted
// with checks AND statuses read for that repository. Invented fixtures.

const ciSHA = "c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1"

func TestCommitChecksReadsStatusesAndRunsWithItsOwnToken(t *testing.T) {
	app, f, _ := newTestApp(t)
	f.mu.Lock()
	f.ci = map[string]string{
		"example-org/widget/" + ciSHA + "/status": `{"statuses":[{"context":"lint","state":"success","updated_at":"2000-01-02T03:04:05Z"},` +
			`{"context":"lint","state":"pending","updated_at":"2000-01-02T03:00:00Z"}]}`,
		"example-org/widget/" + ciSHA + "/check-runs": `{"check_runs":[{"id":71,"name":"unit","status":"completed","conclusion":"failure",` +
			`"completed_at":"2000-01-02T03:09:00Z"},{"id":72,"name":"e2e","status":"in_progress","conclusion":null,"completed_at":null}]}`,
	}
	f.mu.Unlock()
	got, err := app.CommitChecks(context.Background(), "example-org/widget", ciSHA)
	if err != nil {
		t.Fatal(err)
	}
	want := cichecks.Checks{
		Statuses: map[string]cichecks.Status{"lint": {State: "success", UpdatedAt: time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC)}},
		Runs: []cichecks.Run{{ID: 71, Name: "unit", Status: "completed", Conclusion: "failure", CompletedAt: time.Date(2000, 1, 2, 3, 9, 0, 0, time.UTC)},
			{ID: 72, Name: "e2e", Status: "in_progress"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("checks %+v\nwant %+v", got, want)
	}
	ms := f.mintsSoFar()
	if len(ms) != 1 {
		t.Fatalf("%d mints, want 1", len(ms))
	}
	var body struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	_ = json.Unmarshal([]byte(ms[0].body), &body)
	if !reflect.DeepEqual(body.Repositories, []string{"widget"}) ||
		!reflect.DeepEqual(body.Permissions, map[string]string{"metadata": "read", "checks": "read", "statuses": "read"}) {
		t.Fatalf("the CI read's token: %+v", body)
	}
	// The pull-request read keeps its own, narrower token.
	f.setPull("example-org/widget", 91, pull{body: `{"number":91,"state":"open","merged":false}`})
	if _, err := app.PullRequestState(context.Background(), "example-org/widget", 91); err != nil {
		t.Fatal(err)
	}
	if ms := f.mintsSoFar(); len(ms) != 2 || !strings.Contains(ms[1].body, `"pull_requests":"read"`) || strings.Contains(ms[1].body, "checks") {
		t.Fatalf("the PR read's token: %+v", ms)
	}
}

func TestCommitChecksRefusesAnythingButARepoAndAFullCommit(t *testing.T) {
	app, _, _ := newTestApp(t)
	for _, c := range [][2]string{{"example-org", ciSHA}, {"example-org/widget", "c1c1c1"}, {"example-org/widget/x", ciSHA}} {
		if _, err := app.CommitChecks(context.Background(), c[0], c[1]); err == nil {
			t.Errorf("%q %q: read", c[0], c[1])
		}
	}
}

// runsPage is a check-runs page of n green runs, ids from first.
func runsPage(first, n int) string {
	var b strings.Builder
	b.WriteString(`{"check_runs":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"id":%d,"name":"job-%d","status":"completed","conclusion":"success","completed_at":"2000-01-02T03:09:00Z"}`, first+i, first+i)
	}
	b.WriteString(`]}`)
	return b.String()
}

// TestCommitChecksReadsEveryPageAndATruncatedReadIsNeverDone: a commit with
// 450 check runs (four full pages and a short fifth) is read whole — past the
// old three-page cap — and is done and green; one whose every page is full (more
// than the cap can read) is read as TRUNCATED, so it is never done nor green,
// however green every run read is.
func TestCommitChecksReadsEveryPageAndATruncatedReadIsNeverDone(t *testing.T) {
	app, f, _ := newTestApp(t)
	const many = "d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2"
	f.mu.Lock()
	f.ci = map[string]string{
		"example-org/widget/" + ciSHA + "/status":    `{"statuses":[]}`,
		"example-org/widget/" + many + "/status":     `{"statuses":[]}`,
		"example-org/widget/" + many + "/check-runs": runsPage(90000, 100),
	}
	for p := 1; p <= 4; p++ {
		f.ci["example-org/widget/"+ciSHA+"/check-runs#"+strconv.Itoa(p)] = runsPage(p*1000, 100)
	}
	f.ci["example-org/widget/"+ciSHA+"/check-runs#5"] = runsPage(5000, 50)
	f.mu.Unlock()
	got, err := app.CommitChecks(context.Background(), "example-org/widget", ciSHA)
	if err != nil {
		t.Fatal(err)
	}
	if sum := cichecks.Summarize(got); len(got.Runs) != 450 || got.Truncated || !sum.Done() || !sum.AllGreen() {
		t.Fatalf("450 runs over five pages: read %d, truncated %v, done %v", len(got.Runs), got.Truncated, sum.Done())
	}
	got, err = app.CommitChecks(context.Background(), "example-org/widget", many)
	if err != nil {
		t.Fatal(err)
	}
	if sum := cichecks.Summarize(got); !got.Truncated || sum.Done() || sum.AllGreen() {
		t.Fatalf("more runs than the cap reads: truncated %v, done %v, green %v; want truncated, not done", got.Truncated, sum.Done(), sum.AllGreen())
	}
}
