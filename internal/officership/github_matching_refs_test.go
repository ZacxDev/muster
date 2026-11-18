package officership

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// ---------------------------------------------------------------------------
// GitHub's git/matching-refs does NOT paginate: it ignores page and per_page
// and answers every matching ref on every call, with no Link header (measured
// against GitHub: a repo with 282 v* tags answered the same 282 refs, starting
// at the same first ref, for page 1, 2, 3 and 50). The fake below models that.
// A client that loops pages until a short page never sees one once a repo has
// 100 or more matches, re-reads the same list maxPages times and then refuses.
// ---------------------------------------------------------------------------

// matchingRefsLikeGitHub answers path with every ref in refs, whatever page
// or per_page the request names, and counts the requests.
func matchingRefsLikeGitHub(t *testing.T, path string, refs []map[string]any, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != path {
			t.Errorf("asked %s %s?%s, want GET %s", r.Method, r.URL.Path, r.URL.RawQuery, path)
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(refs)
	}))
}

// 282: past 100 (so a short-page loop never terminates) and not a multiple of
// 100 (so a correct count cannot be a page-size artifact).
const unpaginatedRefCount = 282

func TestTagsReadsTheUnpaginatedMatchingRefsOnce(t *testing.T) {
	var refs []map[string]any
	for i := 0; i < unpaginatedRefCount; i++ {
		refs = append(refs, map[string]any{
			"ref":    fmt.Sprintf("refs/tags/v1.%d.0", i),
			"object": map[string]any{"sha": fmt.Sprintf("%040x", i+1), "type": "tag"},
		})
	}
	var calls atomic.Int32
	srv := matchingRefsLikeGitHub(t, "/repos/"+dInfra+"/git/matching-refs/tags/v", refs, &calls)
	defer srv.Close()

	tags, err := restFor(srv).Tags(context.Background())
	if err != nil {
		t.Fatalf("Tags over %d refs: err = %v", unpaginatedRefCount, err)
	}
	seen := map[string]bool{}
	for _, tg := range tags {
		if seen[tg.Name] {
			t.Fatalf("tag %s returned twice", tg.Name)
		}
		seen[tg.Name] = true
	}
	if len(tags) != unpaginatedRefCount || len(seen) != unpaginatedRefCount {
		t.Fatalf("got %d tags (%d unique), want %d", len(tags), len(seen), unpaginatedRefCount)
	}
	if tags[7].Name != "v1.7.0" || tags[7].ObjectSHA != fmt.Sprintf("%040x", 8) || tags[7].ObjectType != "tag" {
		t.Fatalf("tags[7] = %+v", tags[7])
	}
	if calls.Load() != 1 {
		t.Fatalf("%d requests, want 1 (the endpoint answers every ref at once)", calls.Load())
	}
}

func TestBranchesReadsTheUnpaginatedMatchingRefsOnce(t *testing.T) {
	const prefix = "officer/"
	var refs []map[string]any
	for i := 0; i < unpaginatedRefCount; i++ {
		refs = append(refs, map[string]any{
			"ref":    fmt.Sprintf("refs/heads/officer/%d-x", i),
			"object": map[string]any{"sha": fmt.Sprintf("%040x", i+1)},
		})
	}
	var calls atomic.Int32
	srv := matchingRefsLikeGitHub(t, "/repos/"+dInfra+"/git/matching-refs/heads/"+prefix, refs, &calls)
	defer srv.Close()

	branches, err := restFor(srv).Branches(context.Background(), prefix)
	if err != nil {
		t.Fatalf("Branches over %d refs: err = %v", unpaginatedRefCount, err)
	}
	seen := map[string]bool{}
	for _, b := range branches {
		if seen[b.Name] {
			t.Fatalf("branch %s returned twice", b.Name)
		}
		seen[b.Name] = true
	}
	if len(branches) != unpaginatedRefCount || len(seen) != unpaginatedRefCount {
		t.Fatalf("got %d branches (%d unique), want %d", len(branches), len(seen), unpaginatedRefCount)
	}
	if branches[7].Name != "officer/7-x" || branches[7].SHA != fmt.Sprintf("%040x", 8) {
		t.Fatalf("branches[7] = %+v", branches[7])
	}
	if calls.Load() != 1 {
		t.Fatalf("%d requests, want 1 (the endpoint answers every ref at once)", calls.Load())
	}
}
