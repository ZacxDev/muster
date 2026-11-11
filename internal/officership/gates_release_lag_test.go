package officership

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// main_ci right after a merge, through the REAL REST client against a fake
// GitHub that answers in GitHub's own LIST shape: a pull request in
// commits/{sha}/pulls or pulls?… carries merged_at and merge_commit_sha but
// NO `merged` field (only GET pulls/{n} sends that).
//
// Observed: an officer merged its pull request and, 36 s later,
// officer_tag_release was refused by main_ci "it is no pull request's merge"
// on a base that gets no CI of its own: commits/{sha}/pulls still answered []
// (GitHub's association lags a merge). The merge is found by its
// merge_commit_sha among the most recently closed pull requests instead.
// ---------------------------------------------------------------------------

const (
	lagHead   = "5988daa1ca47aaaaaaaaaaaaaaaaaaaaaaaaaaaa" // the base's head: a squash merge
	lagPRHead = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" // the merged pull request's head
	lagOther  = "cccccccccccccccccccccccccccccccccccccccc" // another pull request's merge
	lagPR     = 525
)

// lagGitHub serves one repository. assoc is commits/{lagHead}/pulls'
// answer, closed the pulls list's; prTree is lagPRHead's tree (lagHead's is
// "tree-merged"). lagPRHead has every required check success; lagHead has
// none (the base gets no CI).
type lagGitHub struct {
	t       *testing.T
	assoc   []map[string]any
	closed  []map[string]any
	prTree  string
	listQry []string
}

// listPR is a pull request merged into main as a LIST answer spells it: no
// `merged`.
func listPR(n int, mergeSHA, head string) map[string]any {
	return listPRInto(n, mergeSHA, head, "main", "closed", true)
}

// listPRInto is listPR with its base, state and merged-ness chosen. An
// unmerged one has NO merged_at, but still a merge_commit_sha, as GitHub
// sends for a closed-unmerged pull request (its test merge).
func listPRInto(n int, mergeSHA, head, base, state string, merged bool) map[string]any {
	p := map[string]any{"number": n, "state": state, "merge_commit_sha": mergeSHA,
		"head": map[string]any{"ref": "officer/x", "sha": head}, "base": map[string]any{"ref": base}}
	if merged {
		p["merged_at"] = "2026-01-02T03:04:05Z"
	}
	return p
}

// pullsList is the pulls list's answer to q, as GitHub filters it: only
// state (unless "all") and base (when given), and at most per_page entries.
// closed is in "most recently updated first" order already.
func (f *lagGitHub) pullsList(q url.Values) []map[string]any {
	out := []map[string]any{}
	state := q.Get("state")
	if state == "" {
		state = "open" // GitHub's default
	}
	for _, p := range f.closed {
		if state != "all" && p["state"] != state {
			continue
		}
		if b := q.Get("base"); b != "" && p["base"].(map[string]any)["ref"] != b {
			continue
		}
		out = append(out, p)
	}
	if n, err := strconv.Atoi(q.Get("per_page")); err == nil && n < len(out) {
		out = out[:n]
	}
	return out
}

func (f *lagGitHub) serve(w http.ResponseWriter, r *http.Request) {
	repo := "/repos/example-org/example-infra"
	path := strings.TrimPrefix(r.URL.Path, repo)
	var out any
	switch path {
	case "/commits/" + lagHead + "/status":
		out = map[string]any{"statuses": []any{}}
	case "/commits/" + lagPRHead + "/status":
		var st []any
		for _, c := range fxChecks {
			st = append(st, map[string]any{"context": c, "state": "success"})
		}
		out = map[string]any{"statuses": st}
	case "/commits/" + lagHead + "/check-runs":
		out = map[string]any{"check_runs": []any{}}
	case "/commits/" + lagHead + "/pulls":
		out = append([]map[string]any{}, f.assoc...)
	case "/pulls":
		f.listQry = append(f.listQry, r.URL.RawQuery)
		out = f.pullsList(r.URL.Query())
	case "/git/commits/" + lagHead:
		out = map[string]any{"tree": map[string]any{"sha": "tree-merged"}, "parents": []any{map[string]any{"sha": fxPrevTag}}}
	case "/git/commits/" + lagPRHead:
		out = map[string]any{"tree": map[string]any{"sha": f.prTree}, "parents": []any{map[string]any{"sha": fxPrevTag}}}
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL)
		http.NotFound(w, r)
		return
	}
	_ = json.NewEncoder(w).Encode(out)
}

func lagGates(t *testing.T, f *lagGitHub) (*Gates, func()) {
	f.t = t
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	return &Gates{OfficerID: "engineering:example-web", Repo: dInfra, Ship: fxShip(), GH: restFor(srv)}, srv.Close
}

// TestMainCIFindsAJustMergedPullRequestTheAssociationDoesNotNameYet: the
// association answers [], the pulls list holds the merge (merge_commit_sha ==
// head, same tree, head's checks green): main_ci passes, and the list is
// asked for the closed pull requests into the base, most recently updated
// first.
func TestMainCIFindsAJustMergedPullRequestTheAssociationDoesNotNameYet(t *testing.T) {
	f := &lagGitHub{prTree: "tree-merged", closed: []map[string]any{
		listPR(lagPR+1, lagOther, "dddddddddddddddddddddddddddddddddddddddd"), listPR(lagPR, lagHead, lagPRHead)}}
	g, done := lagGates(t, f)
	defer done()
	if err := g.mainCIGreen(context.Background(), lagHead); err != nil {
		t.Fatalf("a just-merged pull request with green checks and the same tree was refused: %v", err)
	}
	if len(f.listQry) != 1 {
		t.Fatalf("pulls list read %d times, want once", len(f.listQry))
	}
	for _, want := range []string{"state=closed", "base=main", "sort=updated", "direction=desc", "per_page=30"} {
		if !strings.Contains(f.listQry[0], want) {
			t.Fatalf("pulls list asked %q, want %s in it", f.listQry[0], want)
		}
	}
}

// TestMainCIReadsMergedFromAListAnswersMergedAt: once the association names
// the pull request, in its LIST shape (no `merged` field), main_ci passes on
// it without reading the pulls list.
func TestMainCIReadsMergedFromAListAnswersMergedAt(t *testing.T) {
	f := &lagGitHub{prTree: "tree-merged", assoc: []map[string]any{listPR(lagPR, lagHead, lagPRHead)}}
	g, done := lagGates(t, f)
	defer done()
	if err := g.mainCIGreen(context.Background(), lagHead); err != nil {
		t.Fatalf("the associated merge (list-shaped, no `merged` field) was refused: %v", err)
	}
	if len(f.listQry) != 0 {
		t.Fatalf("pulls list read %d times; the association already named the merge", len(f.listQry))
	}
}

// TestMainCIStillRefusesWhenNoClosedPullRequestMergedAsTheHead: a closed
// pull request merged as ANOTHER sha does not vouch for head, and the
// refusal says both sources were asked.
func TestMainCIStillRefusesWhenNoClosedPullRequestMergedAsTheHead(t *testing.T) {
	f := &lagGitHub{prTree: "tree-merged", closed: []map[string]any{listPR(lagPR, lagOther, lagPRHead)}}
	g, done := lagGates(t, f)
	defer done()
	err := g.mainCIGreen(context.Background(), lagHead)
	if err == nil || err.Error() != lagNoMerge {
		t.Fatalf("err = %v\nwant %s", err, lagNoMerge)
	}
}

// lagNoMerge is main_ci's refusal when neither source names a merge of
// lagHead.
const lagNoMerge = "refused by gate main_ci: the checks are not green on main's head 5988daa1ca47, and it is no pull request's merge " +
	"(neither GitHub's pull requests of the commit nor the 30 most recently updated closed into main merged as it): " +
	"refused by gate checks: on 5988daa1ca47: ci / build=missing, ci / e2e=missing, ci / lint=missing"

// TestMainCIRefusesAClosedButUnmergedPullRequestWhoseTestMergeIsTheHead:
// a pull request closed WITHOUT merging still carries a merge_commit_sha
// (GitHub's test merge; merged_at null). Even with that sha equal to head,
// the same tree and green checks on its head, it vouches for nothing.
func TestMainCIRefusesAClosedButUnmergedPullRequestWhoseTestMergeIsTheHead(t *testing.T) {
	f := &lagGitHub{prTree: "tree-merged", closed: []map[string]any{listPRInto(lagPR, lagHead, lagPRHead, "main", "closed", false)}}
	g, done := lagGates(t, f)
	defer done()
	err := g.mainCIGreen(context.Background(), lagHead)
	if err == nil || err.Error() != lagNoMerge {
		t.Fatalf("a closed but UNMERGED pull request whose test merge is head was taken as head's merge: err = %v\nwant %s", err, lagNoMerge)
	}
}

// TestMainCIRefusesAMergeIntoAnotherBase: a pull request merged into ANOTHER
// base as head (same tree, green checks) is not on the closed list into the
// base, so it vouches for nothing. The fake filters by the query's base, so
// a list read without it would return the pull request.
func TestMainCIRefusesAMergeIntoAnotherBase(t *testing.T) {
	f := &lagGitHub{prTree: "tree-merged", closed: []map[string]any{listPRInto(lagPR, lagHead, lagPRHead, "release", "closed", true)}}
	g, done := lagGates(t, f)
	defer done()
	err := g.mainCIGreen(context.Background(), lagHead)
	if err == nil || err.Error() != lagNoMerge {
		t.Fatalf("a pull request merged into another base was taken as head's merge: err = %v\nwant %s", err, lagNoMerge)
	}
}

// TestMainCIFallbackReadsOnlyClosedPullRequests: a full page of open pull
// requests into the base, updated after the merge, does not push the merge
// off the one page read. The fake filters by the query's state and per_page,
// so a read of every state would see only the open ones.
func TestMainCIFallbackReadsOnlyClosedPullRequests(t *testing.T) {
	var prs []map[string]any
	for i := 0; i < 30; i++ {
		prs = append(prs, listPRInto(lagPR+10+i, lagOther, "dddddddddddddddddddddddddddddddddddddddd", "main", "open", false))
	}
	f := &lagGitHub{prTree: "tree-merged", closed: append(prs, listPR(lagPR, lagHead, lagPRHead))}
	g, done := lagGates(t, f)
	defer done()
	if err := g.mainCIGreen(context.Background(), lagHead); err != nil {
		t.Fatalf("the merge, behind 30 open pull requests updated after it, was refused: %v", err)
	}
}

// TestMainCIFallbackKeepsTheTreeCheck: the merge found through the pulls
// list is still refused when its tree is not the one its checks saw.
func TestMainCIFallbackKeepsTheTreeCheck(t *testing.T) {
	f := &lagGitHub{prTree: "tree-stale", closed: []map[string]any{listPR(lagPR, lagHead, lagPRHead)}}
	g, done := lagGates(t, f)
	defer done()
	err := g.mainCIGreen(context.Background(), lagHead)
	if r, ok := AsRefusal(err); !ok || r.Gate != GateMainCI || !strings.Contains(r.Reason, "has a tree its checks never saw") {
		t.Fatalf("err = %v, want main_ci's untested-tree refusal", err)
	}
}
