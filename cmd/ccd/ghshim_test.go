package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// THE READ-ONLY gh SHIM, against a fake connector proxy that answers the
// GitHub REST shapes the shim reads. Values are synthetic and pairwise
// distinct (head and base repositories differ: the fixture is a fork PR, so
// a shim that read the head as the base, or the reverse, cannot pass).
// ---------------------------------------------------------------------------

const (
	ghTok  = "fixture-agent-token-7e3a"
	ghRepo = "example-org/example-infra"
	ghFork = "contrib/example-infra"
	ghPR   = 900
	ghHead = "abcdef0123456789abcdef0123456789abcdef01"
	ghBase = "1234567890abcdef1234567890abcdef12345678"
)

type fakeProxy struct {
	*httptest.Server
	mu   sync.Mutex
	seen []string // "METHOD path?query accept"
	toks []string
}

func newFakeProxy(t *testing.T) *fakeProxy {
	t.Helper()
	p := &fakeProxy{}
	prefix := "/connect/github/api.github.com/"
	page := func(w http.ResponseWriter, r *http.Request, all []any, path string) {
		// Two items per page, so --paginate and the list fields must follow
		// the Link header to see them all.
		pg := 1
		fmt.Sscanf(r.URL.Query().Get("page"), "%d", &pg)
		lo, hi := (pg-1)*2, pg*2
		if hi > len(all) {
			hi = len(all)
		}
		if hi < len(all) {
			w.Header().Set("Link", fmt.Sprintf(`<https://api.github.com/%s?per_page=100&page=%d>; rel="next", <https://api.github.com/%s?page=9>; rel="last"`, path, pg+1, path))
		}
		_ = json.NewEncoder(w).Encode(all[lo:hi])
	}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.seen = append(p.seen, r.Method+" "+strings.TrimPrefix(r.URL.Path, prefix)+"?"+r.URL.RawQuery+" "+r.Header.Get("Accept"))
		p.toks = append(p.toks, r.Header.Get("X-Muster-Token")+"|"+r.Header.Get("Authorization"))
		p.mu.Unlock()
		if r.Method != http.MethodGet {
			http.Error(w, "fake proxy: a write reached it", http.StatusTeapot)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, prefix)
		pr := fmt.Sprintf("repos/%s/pulls/%d", ghRepo, ghPR)
		switch path {
		case pr:
			if strings.Contains(r.Header.Get("Accept"), "diff") {
				_, _ = fmt.Fprint(w, "diff --git a/x.yaml b/x.yaml\n-a: 1\n+a: 2\n")
				return
			}
			if strings.Contains(r.Header.Get("Accept"), "patch") {
				_, _ = fmt.Fprint(w, "From abcdef Mon Sep 17 00:00:00 2001\nSubject: [PATCH] fix\n")
				return
			}
			_, _ = fmt.Fprintf(w, `{"number":%d,"node_id":"PR_node900","html_url":"https://github.com/%s/pull/%d","title":"Raise the limit","body":"Officer: x",
				"state":"open","draft":true,"merged":false,"merged_at":null,"mergeable":true,"additions":3,"deletions":1,"changed_files":3,
				"user":{"login":"example-investigator[bot]","node_id":"BOT_1","type":"Bot"},"labels":[{"node_id":"LA_1","name":"officer","description":"","color":"ededed"}],
				"head":{"ref":"officer/x/17-fix","sha":"%s","repo":{"node_id":"R_fork","name":"example-infra","full_name":"%s","owner":{"login":"contrib","node_id":"U_contrib","type":"User"}}},
				"base":{"ref":"trunk","sha":"%s","repo":{"node_id":"R_base","name":"example-infra","full_name":"%s","owner":{"login":"example-org","node_id":"O_org","type":"Organization"}}}}`,
				ghPR, ghRepo, ghPR, ghHead, ghFork, ghBase, ghRepo)
		case pr + "/files":
			page(w, r, []any{map[string]any{"filename": "a.yaml", "additions": 1, "deletions": 0}, map[string]any{"filename": "b.yaml", "additions": 1, "deletions": 1},
				map[string]any{"filename": "c.yaml", "additions": 1, "deletions": 0}}, path)
		case pr + "/commits":
			page(w, r, []any{map[string]any{"sha": ghHead, "author": nil, "commit": map[string]any{"message": "Raise the limit\n\nClaude-Session-Id: s-1",
				"author": map[string]any{"name": "Bot", "email": "bot@example.test", "date": "2000-01-05T15:00:00Z"}, "committer": map[string]any{"date": "2000-01-05T15:01:00Z"}}}}, path)
		case fmt.Sprintf("repos/%s/issues/%d/comments", ghRepo, ghPR):
			var cs []any
			for i := 1; i <= 3; i++ {
				cs = append(cs, map[string]any{"node_id": fmt.Sprintf("IC_%d", i), "body": fmt.Sprintf("```audit-claims round=%d\n1. x\n```", i),
					"html_url": "https://github.com/x", "created_at": "2000-01-05T15:00:00Z", "author_association": "NONE", "user": map[string]any{"login": "a"}})
			}
			page(w, r, cs, path)
		case pr + "/comments":
			page(w, r, []any{map[string]any{"id": 1, "body": "review line", "path": "a.yaml"}}, path)
		case "repos/" + ghRepo:
			_, _ = fmt.Fprint(w, `{"full_name":"`+ghRepo+`","default_branch":"trunk"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"message":"Not Found"}`)
		}
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *fakeProxy) requests() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.seen...)
}

func (p *fakeProxy) shim(origin string) (*ghShim, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	env := map[string]string{"MUSTER_CONNECT_URL": p.URL, "MUSTER_HOOK_TOKEN": ghTok}
	return &ghShim{stdout: &out, stderr: &errb, getenv: func(k string) string { return env[k] }, http: p.Client(),
		origin: func() (string, error) { return origin, nil }}, &out, &errb
}

func (p *fakeProxy) run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	g, out, errb := p.shim("")
	return g.run(args), out.String(), errb.String()
}

// TestTheShimAnswersTheAuditBriefsReads: the exact argv a brief generator
// emits (its `pr view` field lists, `pr diff`, and the two comment reads),
// each answered through the proxy as GETs carrying the pod's token in
// X-Muster-Token and no Authorization header. Expected values are literal.
func TestTheShimAnswersTheAuditBriefsReads(t *testing.T) {
	p := newFakeProxy(t)
	rc, out, errs := p.run(t, "pr", "view", "900", "--json",
		"title,url,baseRefName,headRefOid,isCrossRepository,headRepository,headRepositoryOwner,comments,commits", "--repo", ghRepo)
	if rc != 0 {
		t.Fatalf("pr view: rc %d, stderr %s", rc, errs)
	}
	var v struct {
		Title, URL, BaseRefName, HeadRefOid string
		IsCrossRepository                   bool
		HeadRepository                      map[string]string
		HeadRepositoryOwner                 map[string]string
		Comments                            []struct {
			Body            string
			ViewerDidAuthor bool
			Author          map[string]string
		}
		Commits []struct{ Oid, MessageHeadline, MessageBody string }
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if v.Title != "Raise the limit" || v.URL != "https://github.com/"+ghRepo+"/pull/900" || v.BaseRefName != "trunk" || v.HeadRefOid != ghHead ||
		!v.IsCrossRepository || v.HeadRepository["nameWithOwner"] != ghFork || v.HeadRepository["name"] != "example-infra" ||
		v.HeadRepositoryOwner["login"] != "contrib" {
		t.Fatalf("pr view = %+v", v)
	}
	if len(v.Comments) != 3 || !strings.Contains(v.Comments[2].Body, "round=3") || v.Comments[0].ViewerDidAuthor || v.Comments[0].Author["login"] != "a" {
		t.Fatalf("comments (3, over two pages) = %+v", v.Comments)
	}
	if len(v.Commits) != 1 || v.Commits[0].Oid != ghHead || v.Commits[0].MessageHeadline != "Raise the limit" || v.Commits[0].MessageBody != "Claude-Session-Id: s-1" {
		t.Fatalf("commits = %+v", v.Commits)
	}
	rc, out, errs = p.run(t, "pr", "view", "900", "--repo", ghRepo, "--json", "number,url,state,changedFiles,files")
	var f struct {
		Number       int
		State        string
		ChangedFiles int
		Files        []struct{ Path string }
	}
	if rc != 0 || json.Unmarshal([]byte(out), &f) != nil {
		t.Fatalf("pr view files: rc %d %s %s", rc, out, errs)
	}
	if f.Number != 900 || f.State != "OPEN" || f.ChangedFiles != 3 || len(f.Files) != 3 || f.Files[2].Path != "c.yaml" {
		t.Fatalf("pr view files = %+v", f)
	}
	if rc, out, _ = p.run(t, "pr", "diff", "900", "--repo", ghRepo); rc != 0 || !strings.HasPrefix(out, "diff --git a/x.yaml") {
		t.Fatalf("pr diff: %d %q", rc, out)
	}
	if rc, out, _ = p.run(t, "pr", "diff", "900", "-R", ghRepo, "--name-only"); rc != 0 || out != "a.yaml\nb.yaml\nc.yaml\n" {
		t.Fatalf("pr diff --name-only: %d %q", rc, out)
	}
	rc, out, _ = p.run(t, "api", "repos/"+ghRepo+"/issues/900/comments", "--paginate")
	var cs []map[string]any
	if rc != 0 || json.Unmarshal([]byte(out), &cs) != nil || len(cs) != 3 {
		t.Fatalf("api --paginate: rc %d, %d merged items: %s", rc, len(cs), out)
	}
	if rc, out, _ = p.run(t, "api", "/repos/"+ghRepo+"/pulls/900/comments"); rc != 0 || !strings.Contains(out, "review line") {
		t.Fatalf("api pulls comments: %d %s", rc, out)
	}
	for _, s := range p.requests() {
		if !strings.HasPrefix(s, "GET ") {
			t.Fatalf("a non-GET reached the proxy: %s", s)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, tk := range p.toks {
		if tk != ghTok+"|" {
			t.Fatalf("a request carried %q, want the pod's token in X-Muster-Token and no Authorization", tk)
		}
	}
}

// TestTheShimResolvesTheRepository: a pull request URL names its own
// repository; with neither --repo nor a URL, GH_REPO then the checkout's
// origin (https or ssh, with or without .git) decide; {owner}/{repo} in an
// api path are filled the same way.
func TestTheShimResolvesTheRepository(t *testing.T) {
	p := newFakeProxy(t)
	if rc, out, errs := p.run(t, "pr", "view", "https://github.com/"+ghRepo+"/pull/900", "--json", "number"); rc != 0 || !strings.Contains(out, "900") {
		t.Fatalf("a URL: %d %s %s", rc, out, errs)
	}
	for _, origin := range []string{"https://github.com/" + ghRepo + ".git", "git@github.com:" + ghRepo + ".git", "https://github.com/" + ghRepo} {
		g, out, errb := p.shim(origin)
		if rc := g.run([]string{"pr", "view", "900", "--json", "headRefOid"}); rc != 0 || !strings.Contains(out.String(), ghHead) {
			t.Fatalf("origin %s: %d %s %s", origin, rc, out, errb)
		}
		g, out, _ = p.shim(origin)
		if rc := g.run([]string{"api", "repos/{owner}/{repo}"}); rc != 0 || !strings.Contains(out.String(), `"default_branch":"trunk"`) {
			t.Fatalf("placeholders with origin %s: %d %s", origin, rc, out)
		}
	}
	g, _, errb := p.shim("")
	if rc := g.run([]string{"pr", "view", "900", "--json", "number"}); rc != 1 || !strings.Contains(errb.String(), "no repository") {
		t.Fatalf("no repository anywhere: %d %s", rc, errb)
	}
}

// TestTheShimRefusesEveryWrite: every write shape gh has, refused with exit
// 1 and a message naming the shim, and NOTHING sent to the proxy.
func TestTheShimRefusesEveryWrite(t *testing.T) {
	p := newFakeProxy(t)
	for _, args := range [][]string{
		{"pr", "comment", "900", "--repo", ghRepo, "--body", "x"},
		{"pr", "merge", "900", "--repo", ghRepo, "--squash"},
		{"pr", "close", "900", "--repo", ghRepo},
		{"pr", "reopen", "900", "--repo", ghRepo},
		{"pr", "edit", "900", "--repo", ghRepo, "--title", "x"},
		{"pr", "ready", "900", "--repo", ghRepo},
		{"pr", "create", "--repo", ghRepo, "--title", "x"},
		{"pr", "review", "900", "--approve"},
		{"pr", "checkout", "900"},
		{"issue", "comment", "1", "--body", "x"},
		{"repo", "delete", ghRepo},
		{"release", "create", "v1.0.0"},
		{"api", "-X", "POST", "repos/" + ghRepo + "/issues/900/comments", "-f", "body=x"},
		{"api", "--method", "PATCH", "repos/" + ghRepo + "/pulls/900"},
		{"api", "--method=DELETE", "repos/" + ghRepo + "/git/refs/heads/x"},
		{"api", "-X", "PUT", "repos/" + ghRepo + "/pulls/900/merge"},
		{"api", "repos/" + ghRepo + "/issues/900/comments", "-f", "body=x"},
		{"api", "repos/" + ghRepo + "/issues/900/comments", "--raw-field", "body=x"},
		{"api", "repos/" + ghRepo + "/issues/900/comments", "-F", "body=@file"},
		{"api", "repos/" + ghRepo + "/issues/900/comments", "--input", "body.json"},
		{"api", "repos/" + ghRepo + "/issues/900/comments", "--field=body=x"},
		{"api", "graphql", "-f", "query=mutation { x }"},
		{"api", "graphql"},
		{"auth", "login"},
	} {
		rc, _, errs := p.run(t, args...)
		if rc != 1 || !strings.Contains(errs, "read-only shim") {
			t.Errorf("gh %s: rc %d, stderr %q, want a refusal", strings.Join(args, " "), rc, errs)
		}
	}
	if got := p.requests(); len(got) != 0 {
		t.Fatalf("refusals reached the proxy: %v", got)
	}
	// Control: the same proxy answers a read.
	if rc, _, errs := p.run(t, "pr", "view", "900", "--repo", ghRepo, "--json", "number"); rc != 0 {
		t.Fatalf("control read: %d %s", rc, errs)
	}
	if got := p.requests(); len(got) != 1 {
		t.Fatalf("the control read made %d requests", len(got))
	}
}

// TestTheShimSaysWhatItCannotDo: an unknown --json field lists the fields
// gh would; --jq and a missing --json are errors; an HTTP error is printed
// as gh prints one; without the proxy's environment nothing is attempted.
func TestTheShimSaysWhatItCannotDo(t *testing.T) {
	p := newFakeProxy(t)
	for name, c := range map[string]struct {
		args []string
		says string
	}{
		"an unknown field": {[]string{"pr", "view", "900", "--repo", ghRepo, "--json", "number,reviewDecision"}, `Unknown JSON field: "reviewDecision"`},
		"--jq":             {[]string{"pr", "view", "900", "--repo", ghRepo, "--json", "number", "--jq", ".number"}, "unknown flag --jq"},
		"no --json":        {[]string{"pr", "view", "900", "--repo", ghRepo}, "pass --json"},
		"no number":        {[]string{"pr", "view", "--repo", ghRepo, "--json", "number"}, "exactly one pull request"},
		"a 404":            {[]string{"pr", "view", "901", "--repo", ghRepo, "--json", "number"}, "HTTP 404: Not Found (https://api.github.com/repos/" + ghRepo + "/pulls/901)"},
	} {
		if rc, _, errs := p.run(t, c.args...); rc != 1 || !strings.Contains(errs, c.says) {
			t.Errorf("%s: rc %d, stderr %q, want one saying %q", name, rc, errs, c.says)
		}
	}
	var out, errb bytes.Buffer
	g := &ghShim{stdout: &out, stderr: &errb, getenv: func(string) string { return "" }, http: p.Client()}
	before := len(p.requests())
	if rc := g.run([]string{"pr", "view", "900", "--repo", ghRepo, "--json", "number"}); rc != 1 || !strings.Contains(errb.String(), "MUSTER_CONNECT_URL") {
		t.Fatalf("no proxy environment: %d %s", rc, errb.String())
	}
	if len(p.requests()) != before {
		t.Fatal("a request was sent without the proxy's environment")
	}
	if rc, out, _ := p.run(t, "--version"); rc != 0 || !strings.Contains(out, "read-only") {
		t.Fatalf("--version: %d %q", rc, out)
	}
}

// TestNextPageFollowsGitHubsLinkHeader: the rel="next" URL becomes a path
// under the proxy, with its query; no next is "".
func TestNextPageFollowsGitHubsLinkHeader(t *testing.T) {
	for link, want := range map[string]string{
		`<https://api.github.com/repositories/1/issues/2/comments?page=2>; rel="next", <https://api.github.com/repositories/1/issues/2/comments?page=5>; rel="last"`: "repositories/1/issues/2/comments?page=2",
		`<https://api.github.com/x?page=1>; rel="prev"`: "",
		"": "",
	} {
		if got := nextPage(link); got != want {
			t.Errorf("nextPage(%q) = %q, want %q", link, got, want)
		}
	}
	if !reflect.DeepEqual(prFieldNames()[:3], []string{"additions", "author", "baseRefName"}) {
		t.Fatalf("field names are not sorted: %v", prFieldNames()[:3])
	}
}
