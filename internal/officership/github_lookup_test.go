package officership

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// ---------------------------------------------------------------------------
// The REST lookups the draft gate counts and re-checks from: by branch
// (head=<owner>:<branch>) and by author (the officer App's bot), every page,
// and an ERROR — never a truncated answer — past maxPages.
// ---------------------------------------------------------------------------

func restFor(srv *httptest.Server) *REST {
	return &REST{Base: srv.URL, Repo: dInfra, HTTP: srv.Client(),
		Token: func(context.Context) (string, error) { return "fixture-ship-token", nil }}
}

// TestPRsForBranchAsksByHeadAndReadsEveryPage: the query names the head as
// owner:branch and the state; a second page is read; past maxPages full
// pages is an error.
func TestPRsForBranchAsksByHeadAndReadsEveryPage(t *testing.T) {
	var pages, full atomic.Int32
	full.Store(2) // full pages before a short one
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/repos/example-org/example-infra/pulls" || q.Get("head") != "example-org:"+dBranch || q.Get("state") != "all" {
			t.Errorf("asked %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		n := int(pages.Add(1))
		count := 100
		if n > int(full.Load()) {
			count = 3
		}
		var out []map[string]any
		for i := 0; i < count; i++ {
			out = append(out, map[string]any{"number": n*1000 + i, "state": "closed", "head": map[string]any{"ref": dBranch, "sha": "s"}})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()
	prs, err := restFor(srv).PRsForBranch(context.Background(), dBranch, "all")
	if err != nil || len(prs) != 203 || pages.Load() != 3 {
		t.Fatalf("got %d pull requests in %d pages, err %v; want 203 in 3", len(prs), pages.Load(), err)
	}
	pages.Store(0)
	full.Store(maxPages + 5)
	if _, err := restFor(srv).PRsForBranch(context.Background(), dBranch, "all"); err == nil || !strings.Contains(err.Error(), "too many") {
		t.Fatalf("past %d full pages: err = %v, want a refusal to vouch", maxPages, err)
	}
}

// TestOfficerPRsListsPullsAndFiltersByAuthor: an officer App's token carries
// no Issues permission, so GitHub answers the issues list 403 ("Resource not
// accessible by integration"). OfficerPRs must read the pulls list (which the
// Pull requests permission covers), newest created first, every page, and
// keep only login's pull requests (login compared case-insensitively, as
// everywhere else); another user's are dropped. Past maxPages full pages is
// still a refusal, never a truncated answer.
func TestOfficerPRsListsPullsAndFiltersByAuthor(t *testing.T) {
	login := BotLogin("fixture-lister-7")
	const other = "unrelated-person"
	// mine maps a page to the indexes on it opened by login; the entry at
	// index 50 of page 1 spells the login in a different case.
	mine := map[int][]int{1: {3, 50}, 2: {7}, 3: {1}}
	number := func(page, i int) int { return 9000 - (page-1)*100 - i }
	var pages, full, issueCalls atomic.Int32
	full.Store(2) // full pages before a short third one
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch r.URL.Path {
		case "/repos/example-org/example-infra/issues":
			issueCalls.Add(1)
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"Resource not accessible by integration","documentation_url":"https://docs.github.com/rest/issues/issues#list-repository-issues","status":"403"}`)
			return
		case "/repos/example-org/example-infra/pulls":
		default:
			t.Errorf("asked %s?%s", r.URL.Path, r.URL.RawQuery)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if q.Get("state") != "all" || q.Get("sort") != "created" || q.Get("direction") != "desc" || q.Get("per_page") != "100" || q.Get("head") != "" {
			t.Errorf("asked %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		n := int(pages.Add(1))
		if got := q.Get("page"); got != fmt.Sprint(n) {
			t.Errorf("request %d asked page %q", n, got)
		}
		count := 100
		if n > int(full.Load()) {
			count = 4
		}
		ours := map[int]bool{}
		for _, i := range mine[n] {
			ours[i] = true
		}
		var out []map[string]any
		for i := 0; i < count; i++ {
			author, typ := other, "User"
			if ours[i] {
				author, typ = login, "Bot"
				if n == 1 && i == 50 {
					author = strings.ToUpper(login)
				}
			}
			out = append(out, map[string]any{
				"number": number(n, i), "state": "open", "draft": true, "title": fmt.Sprintf("t%d", number(n, i)),
				"body": fmt.Sprintf("body of %d\nsecond line", number(n, i)), "created_at": "2000-01-02T03:04:05Z",
				"user": map[string]any{"login": author, "type": typ},
				"head": map[string]any{"ref": fmt.Sprintf("head-%d", number(n, i)), "sha": "s"},
			})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()
	prs, err := restFor(srv).OfficerPRs(context.Background(), login, "all")
	if err != nil {
		t.Fatalf("OfficerPRs: %v (want the pulls list read, never the issues list)", err)
	}
	var got []int
	for _, p := range prs {
		got = append(got, p.Number)
	}
	want := []int{8997, 8950, 8893, 8799}
	if fmt.Sprint(got) != fmt.Sprint(want) || pages.Load() != 3 {
		t.Fatalf("got pull requests %v in %d pages; want exactly %v (login's only, newest first) in 3", got, pages.Load(), want)
	}
	if issueCalls.Load() != 0 {
		t.Errorf("read the issues list %d time(s); an officer App's token cannot", issueCalls.Load())
	}
	p := prs[2]
	if p.Body != "body of 8893\nsecond line" || p.Author != login || p.AuthorType != "Bot" || p.State != "open" ||
		p.Title != "t8893" || p.HeadRef != "head-8893" || !p.Draft || p.CreatedAt.Year() != 2000 {
		t.Fatalf("pull request #8893 = %+v; want its body, author, type, state, title, head and draft flag", p)
	}
	pages.Store(0)
	full.Store(maxPages + 1)
	if _, err := restFor(srv).OfficerPRs(context.Background(), login, "all"); err == nil || !strings.Contains(err.Error(), "too many") {
		t.Fatalf("past %d full pages: err = %v, want a refusal to vouch", maxPages, err)
	}
}

// TestDeleteBranchDeletesTheHeadsRef:DELETE git/refs/heads/<branch>, the
// branch's segments escaped one by one.
func TestDeleteBranchDeletesTheHeadsRef(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Method + " " + r.URL.EscapedPath()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if err := restFor(srv).DeleteBranch(context.Background(), dBranch); err != nil {
		t.Fatal(err)
	}
	if want := "DELETE /repos/example-org/example-infra/git/refs/heads/" + dBranch; got != want {
		t.Fatalf("sent %q, want %q", got, want)
	}
}
