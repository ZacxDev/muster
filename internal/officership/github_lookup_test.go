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
// (head=<owner>:<branch>) and by creator (the officer App's bot), every page,
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

// TestOfficerPRsAsksByCreatorAndSkipsIssues: the creator filter names the
// bot login; an issue (no pull_request key) is not a pull request; the body
// and author come through; past maxPages is an error.
func TestOfficerPRsAsksByCreatorAndSkipsIssues(t *testing.T) {
	login := BotLogin("ship-officer")
	var pages, full atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/repos/example-org/example-infra/issues" || q.Get("creator") != login || q.Get("state") != "open" {
			t.Errorf("asked %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		n := int(pages.Add(1))
		if n <= int(full.Load()) {
			var out []map[string]any
			for i := 0; i < 100; i++ {
				out = append(out, map[string]any{"number": i, "pull_request": map[string]any{}})
			}
			_ = json.NewEncoder(w).Encode(out)
			return
		}
		fmt.Fprintf(w, `[{"number":5,"state":"open","body":"first\nsecond","user":{"login":%q,"type":"Bot"},"pull_request":{"url":"x"}},
			{"number":6,"state":"open","body":"an issue","user":{"login":%q,"type":"Bot"}}]`, login, login)
	}))
	defer srv.Close()
	prs, err := restFor(srv).OfficerPRs(context.Background(), login, "open")
	if err != nil || len(prs) != 1 || prs[0].Number != 5 || prs[0].Body != "first\nsecond" || prs[0].Author != login || prs[0].AuthorType != "Bot" {
		t.Fatalf("got %+v, err %v; want #5 only, with its body and author", prs, err)
	}
	pages.Store(0)
	full.Store(maxPages + 1)
	if _, err := restFor(srv).OfficerPRs(context.Background(), login, "open"); err == nil || !strings.Contains(err.Error(), "too many") {
		t.Fatalf("past %d full pages: err = %v, want a refusal to vouch", maxPages, err)
	}
}

// TestDeleteBranchDeletesTheHeadsRef: DELETE git/refs/heads/<branch>, the
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
