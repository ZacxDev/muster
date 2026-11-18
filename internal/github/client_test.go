package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestListPullRequests(t *testing.T) {
	const body = `[
		{"number":42,"title":"Add CI","state":"open","html_url":"https://github.com/owner/app/pull/42",
		 "draft":false,"updated_at":"2026-06-09T10:00:00Z",
		 "user":{"login":"alice"},"head":{"ref":"feature/ci"},"base":{"ref":"trunk"}},
		{"number":40,"title":"WIP refactor","state":"open","html_url":"https://github.com/owner/app/pull/40",
		 "draft":true,"updated_at":"2026-06-08T12:00:00Z",
		 "user":{"login":"bob"},"head":{"ref":"refactor"},"base":{"ref":"main"}}
	]`
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/app/pulls" {
			http.Error(w, "wrong path: "+r.URL.Path, http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok123" {
			http.Error(w, "missing bearer", http.StatusUnauthorized)
			return
		}
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	prs, err := ListPullRequestsAt(context.Background(), srv.URL, "tok123", "owner/app", "open")
	if err != nil {
		t.Fatalf("ListPullRequestsAt: %v", err)
	}
	if len(prs) != 2 {
		t.Fatalf("got %d PRs, want 2", len(prs))
	}
	p := prs[0]
	if p.Number != 42 || p.Title != "Add CI" || p.State != "open" {
		t.Fatalf("pr[0] basics = %+v", p)
	}
	if p.User != "alice" || p.HeadRef != "feature/ci" || p.BaseRef != "trunk" {
		t.Fatalf("pr[0] nested mapping wrong: user=%q head=%q base=%q", p.User, p.HeadRef, p.BaseRef)
	}
	if p.Draft || p.HTMLURL != "https://github.com/owner/app/pull/42" || p.UpdatedAt != "2026-06-09T10:00:00Z" {
		t.Fatalf("pr[0] fields = %+v", p)
	}
	if !prs[1].Draft || prs[1].User != "bob" {
		t.Fatalf("pr[1] = %+v, want draft + bob", prs[1])
	}
	for _, want := range []string{"state=open", "per_page=50", "sort=updated", "direction=desc"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q missing %q", gotQuery, want)
		}
	}
}

func TestListPullRequestsDefaultsState(t *testing.T) {
	var gotState string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotState = r.URL.Query().Get("state")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	if _, err := ListPullRequestsAt(context.Background(), srv.URL, "t", "owner/app", "bogus"); err != nil {
		t.Fatalf("ListPullRequestsAt: %v", err)
	}
	if gotState != "open" {
		t.Fatalf("invalid state not defaulted: got %q, want open", gotState)
	}
	if _, err := ListPullRequestsAt(context.Background(), srv.URL, "t", "owner/app", ""); err != nil {
		t.Fatalf("ListPullRequestsAt empty: %v", err)
	}
	if gotState != "open" {
		t.Fatalf("empty state not defaulted: got %q, want open", gotState)
	}
}

func TestListPullRequestsErrorsOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := ListPullRequestsAt(context.Background(), srv.URL, "t", "owner/app", "open"); err == nil {
		t.Fatal("expected error on 500")
	}
}
