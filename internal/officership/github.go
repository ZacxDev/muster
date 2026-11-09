// Package officership is what lets an officer SHIP: push branches, merge its
// own pull requests and tag releases in one repository, with every write
// decided in muster-server (docs/officers.md "Shipping officers").
//
//   - pushgate.go: the connector-proxy write filter (a connectors.Gate) an
//     instance's every GitHub request passes through;
//   - gates.go: officer_merge_pr's and officer_tag_release's gates, all read
//     fresh from GitHub and HQ at call time;
//   - lint.go: the destructive-DDL lint on migration files;
//   - rollout.go: the post-release watch and the automatic rollback.
//
// 🔴 THE REPOSITORY ENFORCES NOTHING, SO THIS PACKAGE IS THE BRANCH
// PROTECTION. A private repository on a plan without branch protection or
// rulesets lets any `contents: write` token push to its base branch, push any
// tag and merge with red checks. The officer's write token therefore never
// leaves muster: the instance's requests reach GitHub only through the push
// gate, and merges and tags are made here, after the gates, with muster's own
// token.
package officership

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// PR is the part of a pull request the gates read.
type PR struct {
	Number         int
	NodeID         string // the GraphQL id (officer_mark_ready's mutation names it)
	State          string
	Draft          bool
	Merged         bool
	Mergeable      *bool
	Author         string
	AuthorType     string
	HeadRef        string
	HeadSHA        string
	HeadRepo       string
	BaseRef        string
	BaseRepo       string
	Title          string
	MergeCommitSHA string
	CreatedAt      time.Time
	// Body is the pull request's description (OfficerPRs fills it: the draft
	// gate reads its item from the first line).
	Body string
}

// Compare is GET compare/{base}...{head}.
type Compare struct {
	Status   string // ahead | behind | diverged | identical
	AheadBy  int
	BehindBy int
	Files    []File
	Commits  []string // shas, oldest first
	// MergeBase is the comparison's merge base commit: what a removed file
	// is read at when GitHub shows no diff for it (the draft gate).
	MergeBase string
}

// File is one changed file of a pull request or a comparison.
type File struct {
	Path     string
	Previous string // a rename's old path
	Status   string // added | modified | removed | renamed | copied | changed | unchanged
	// Patch is the file's unified diff hunks as GitHub reports them, and
	// Changes its added+removed line count. GitHub OMITS the patch of a binary
	// file (reported with changes 0, MEASURED on a read-only comparison: every
	// added, modified and removed PNG came back changes 0, additions 0,
	// deletions 0, no patch), of a very large one (changes > 0), and of a pure
	// rename or mode change. An empty Patch is therefore a diff nobody can
	// read whatever Changes says (the draft gate's ForbiddenFiles).
	Patch   string
	Changes int
}

// Branch is one branch: its name (without refs/heads/) and head commit.
type Branch struct {
	Name string
	SHA  string
}

// CheckRun is one check run on a commit.
type CheckRun struct {
	ID         int64
	Name       string
	Status     string
	Conclusion string
	AppSlug    string
}

// Comment is any human-or-bot text on a pull request: a conversation comment,
// a review body or a review comment.
type Comment struct {
	Author     string
	AuthorType string // User | Bot | Organization
	Body       string
}

// TagRef is one refs/tags/* ref: its name and the object it points at.
type TagRef struct {
	Name       string
	ObjectSHA  string
	ObjectType string // commit (lightweight) | tag (annotated)
}

// TagObject is an annotated tag.
type TagObject struct {
	SHA        string
	Tagger     string
	TaggedAt   time.Time
	TargetSHA  string
	TargetType string
	Message    string
}

// Tagger is who an annotated tag is made by.
type Tagger struct {
	Name, Email string
	When        time.Time
}

// ErrRefExists is a create of a ref that already exists (GitHub's 422): the
// compare-and-swap against a concurrent tag or branch.
var ErrRefExists = errors.New("officership: the ref already exists")

// ErrRefMissing is a ref GitHub does not have (DeleteRef).
var ErrRefMissing = errors.New("officership: no such ref")

// ErrMergeConflict is a merge GitHub could not make cleanly (409).
var ErrMergeConflict = errors.New("officership: merge conflict")

// GitHub is every GitHub read and write the gates and the watcher make, for
// ONE repository. The REST implementation is [REST]; the tests use a fake.
type GitHub interface {
	AppSlug(ctx context.Context) (string, error)
	PR(ctx context.Context, n int) (PR, error)
	PRFiles(ctx context.Context, n int) ([]File, error)
	Compare(ctx context.Context, base, head string) (Compare, error)
	// Statuses is the latest commit status per context on sha.
	Statuses(ctx context.Context, sha string) (map[string]string, error)
	CheckRuns(ctx context.Context, sha, name string) ([]CheckRun, error)
	PRComments(ctx context.Context, n int) ([]Comment, error)
	FileAt(ctx context.Context, path, ref string) ([]byte, error)
	BranchSHA(ctx context.Context, branch string) (string, error)
	Tags(ctx context.Context) ([]TagRef, error)
	TagObject(ctx context.Context, sha string) (TagObject, error)
	// Commit is a commit's tree and parents.
	Commit(ctx context.Context, sha string) (tree string, parents []string, err error)
	PRsForCommit(ctx context.Context, sha string) ([]PR, error)
	Merge(ctx context.Context, n int, sha string) (string, error)
	CreateTag(ctx context.Context, name, message, sha string, t Tagger) (string, error)
	CreateRef(ctx context.Context, ref, sha string) error
	CreateCommit(ctx context.Context, message, tree string, parents []string) (string, error)
	MergeInto(ctx context.Context, base, head, message string) (string, error)
	OpenPR(ctx context.Context, title, head, base, body string) (int, error)
	// PRsForBranch is every pull request whose head is branch in this
	// repository (GitHub's head=<owner>:<branch> filter, which finds a closed
	// pull request whose branch was deleted too), in state "open", "closed"
	// or "all". Every page is read; more than maxPages pages is an error,
	// never a truncated answer.
	PRsForBranch(ctx context.Context, branch, state string) ([]PR, error)
	// OfficerPRs is every pull request login opened in this repository
	// (GitHub's pulls list, every page, newest created first, kept when its
	// author is login), in state "open", "closed" or "all", with its Body and
	// head. For the App's own login the read stops at the App's creation;
	// otherwise the list is read whole. More than maxPages pages read is an
	// error, never a truncated answer.
	OfficerPRs(ctx context.Context, login, state string) ([]PR, error)
	// DeleteBranch deletes refs/heads/<branch> (the draft gate, after it
	// closed a pull request whose branch holds a refused change).
	DeleteBranch(ctx context.Context, branch string) error
	// Branches is every branch whose name starts with prefix.
	Branches(ctx context.Context, prefix string) ([]Branch, error)
	// CommitTime is sha's committer date.
	CommitTime(ctx context.Context, sha string) (time.Time, error)
	// MarkReady converts draft pull request n (GraphQL node nodeID) to ready
	// for review. 🔴 REST cannot: PATCH pulls takes no `draft` field (only
	// POST pulls does), so this is the GraphQL markPullRequestReadyForReview
	// mutation, and it errs unless GitHub answers pull request n, not a draft.
	MarkReady(ctx context.Context, n int, nodeID string) error
	// ClosePR closes pull request n (PATCH pulls/<n> state "closed"): the
	// draft gate's answer to a push that put a refused change into an open
	// pull request.
	ClosePR(ctx context.Context, n int) error
	// UpdateBranch moves refs/heads/<branch> from `before` to `after` by ONE
	// compare-and-swap that is also fast-forward-only: GraphQL updateRefs with
	// beforeOid `before` and force false. GitHub refuses it — and nothing
	// moves — when the branch is not at `before` or `after` is not a
	// descendant of it. 🔴 THE ONLY BRANCH MOVE muster makes for a shepherd
	// (shepherd.go), and never forced.
	UpdateBranch(ctx context.Context, branch, before, after string) error
	// DeleteRef deletes ref (a full name under refs/, outside refs/heads and
	// refs/tags: the shepherd's scratch refs). A missing ref is ErrRefMissing.
	DeleteRef(ctx context.Context, ref string) error
}

// REST is [GitHub] over GitHub's REST API for Repo, authenticated by Token
// (the officer App's installation token, minted per call by muster).
type REST struct {
	Base  string // API base, no trailing slash
	Repo  string // owner/name
	Token func(ctx context.Context) (string, error)
	// Slug reads the App's slug (GET /app, as the App).
	Slug func(ctx context.Context) (string, error)
	// Created reads when the App was created (GET /app's created_at, as the
	// App). OfficerPRs stops reading the App's own pull requests there; nil
	// reads the whole list.
	Created func(ctx context.Context) (time.Time, error)
	HTTP    *http.Client
}

// maxGitHubBody bounds one response read.
const maxGitHubBody = 8 << 20

// maxPages bounds every paginated read (100 per page).
const maxPages = 30

type httpError struct {
	status int
	body   string
	op     string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("GitHub answered %d while %s: %s", e.status, e.op, truncate(e.body, 300))
}

func statusOf(err error) int {
	var he *httpError
	if errors.As(err, &he) {
		return he.status
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func (g *REST) repoPath() string {
	owner, name, _ := strings.Cut(g.Repo, "/")
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
}

func (g *REST) do(ctx context.Context, method, path string, accept string, in, out any, op string) error {
	return g.doURL(ctx, method, g.Base+path, accept, in, out, op)
}

// doURL is do on a full URL (the GraphQL endpoint is not under Base).
func (g *REST) doURL(ctx context.Context, method, u string, accept string, in, out any, op string) error {
	tok, err := g.Token(ctx)
	if err != nil {
		return fmt.Errorf("no GitHub token while %s: %w", op, err)
	}
	var rd io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if accept == "" {
		accept = "application/vnd.github+json"
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := g.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach GitHub while %s: %v", op, scrub(err))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxGitHubBody))
	if resp.StatusCode/100 != 2 {
		return &httpError{status: resp.StatusCode, body: string(raw), op: op}
	}
	if out == nil {
		return nil
	}
	if b, ok := out.(*[]byte); ok {
		*b = raw
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("GitHub's answer while %s is not the expected JSON: %v", op, err)
	}
	return nil
}

func scrub(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}

type restPR struct {
	Number    int    `json:"number"`
	NodeID    string `json:"node_id"`
	State     string `json:"state"`
	Draft     bool   `json:"draft"`
	Merged    bool   `json:"merged"`
	Mergeable *bool  `json:"mergeable"`
	Title     string `json:"title"`
	User      struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
	Head struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref  string `json:"ref"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
	MergeCommitSHA string    `json:"merge_commit_sha"`
	CreatedAt      time.Time `json:"created_at"`
	Body           string    `json:"body"`
}

func (r restPR) pr() PR {
	p := PR{Number: r.Number, NodeID: r.NodeID, State: r.State, Draft: r.Draft, Merged: r.Merged, Mergeable: r.Mergeable, Title: r.Title,
		Author: r.User.Login, AuthorType: r.User.Type, HeadRef: r.Head.Ref, HeadSHA: r.Head.SHA, BaseRef: r.Base.Ref,
		MergeCommitSHA: r.MergeCommitSHA, CreatedAt: r.CreatedAt, Body: r.Body}
	if r.Head.Repo != nil {
		p.HeadRepo = r.Head.Repo.FullName
	}
	if r.Base.Repo != nil {
		p.BaseRepo = r.Base.Repo.FullName
	}
	return p
}

// AppSlug implements GitHub.
func (g *REST) AppSlug(ctx context.Context) (string, error) { return g.Slug(ctx) }

// PR implements GitHub.
func (g *REST) PR(ctx context.Context, n int) (PR, error) {
	var r restPR
	err := g.do(ctx, http.MethodGet, g.repoPath()+"/pulls/"+strconv.Itoa(n), "", nil, &r, "reading pull request #"+strconv.Itoa(n))
	return r.pr(), err
}

type restFile struct {
	Filename         string `json:"filename"`
	PreviousFilename string `json:"previous_filename"`
	Status           string `json:"status"`
	Patch            string `json:"patch"`
	Changes          int    `json:"changes"`
}

// PRFiles implements GitHub.
func (g *REST) PRFiles(ctx context.Context, n int) ([]File, error) {
	var out []File
	for page := 1; page <= maxPages; page++ {
		var batch []restFile
		if err := g.do(ctx, http.MethodGet, fmt.Sprintf("%s/pulls/%d/files?per_page=100&page=%d", g.repoPath(), n, page), "", nil,
			&batch, "listing pull request files"); err != nil {
			return nil, err
		}
		for _, f := range batch {
			out = append(out, File{Path: f.Filename, Previous: f.PreviousFilename, Status: f.Status})
		}
		if len(batch) < 100 {
			return out, nil
		}
	}
	return nil, fmt.Errorf("pull request #%d changes more than %d files: too many to vouch for", n, maxPages*100)
}

// Compare implements GitHub. 🔴 GitHub lists at most 300 files and 250
// commits in one comparison; a comparison at either cap is refused rather
// than read as complete.
func (g *REST) Compare(ctx context.Context, base, head string) (Compare, error) {
	var r struct {
		Status   string     `json:"status"`
		AheadBy  int        `json:"ahead_by"`
		BehindBy int        `json:"behind_by"`
		Files    []restFile `json:"files"`
		Commits  []struct {
			SHA string `json:"sha"`
		} `json:"commits"`
		TotalCommits int `json:"total_commits"`
		MergeBase    struct {
			SHA string `json:"sha"`
		} `json:"merge_base_commit"`
	}
	if err := g.do(ctx, http.MethodGet, g.repoPath()+"/compare/"+url.PathEscape(base)+"..."+url.PathEscape(head), "",
		nil, &r, "comparing "+base+"..."+head); err != nil {
		return Compare{}, err
	}
	if len(r.Files) >= 300 || r.TotalCommits > len(r.Commits) {
		return Compare{}, fmt.Errorf("the comparison %s...%s is larger than GitHub lists in one answer (%d commits, %d files)",
			base, head, r.TotalCommits, len(r.Files))
	}
	c := Compare{Status: r.Status, AheadBy: r.AheadBy, BehindBy: r.BehindBy, MergeBase: r.MergeBase.SHA}
	for _, f := range r.Files {
		c.Files = append(c.Files, File{Path: f.Filename, Previous: f.PreviousFilename, Status: f.Status, Patch: f.Patch, Changes: f.Changes})
	}
	for _, x := range r.Commits {
		c.Commits = append(c.Commits, x.SHA)
	}
	return c, nil
}

// Statuses implements GitHub (the combined status: the latest per context).
func (g *REST) Statuses(ctx context.Context, sha string) (map[string]string, error) {
	out := map[string]string{}
	for page := 1; page <= 5; page++ {
		var r struct {
			Statuses []struct {
				Context string `json:"context"`
				State   string `json:"state"`
			} `json:"statuses"`
		}
		if err := g.do(ctx, http.MethodGet, fmt.Sprintf("%s/commits/%s/status?per_page=100&page=%d", g.repoPath(), url.PathEscape(sha), page),
			"", nil, &r, "reading the commit statuses of "+sha); err != nil {
			return nil, err
		}
		for _, s := range r.Statuses {
			if _, seen := out[s.Context]; !seen {
				out[s.Context] = s.State
			}
		}
		if len(r.Statuses) < 100 {
			break
		}
	}
	return out, nil
}

// CheckRuns implements GitHub.
func (g *REST) CheckRuns(ctx context.Context, sha, name string) ([]CheckRun, error) {
	var r struct {
		CheckRuns []struct {
			ID         int64  `json:"id"`
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			App        *struct {
				Slug string `json:"slug"`
			} `json:"app"`
		} `json:"check_runs"`
	}
	if err := g.do(ctx, http.MethodGet, fmt.Sprintf("%s/commits/%s/check-runs?check_name=%s&filter=all&per_page=100", g.repoPath(),
		url.PathEscape(sha), url.QueryEscape(name)), "", nil, &r, "reading check runs on "+sha); err != nil {
		return nil, err
	}
	var out []CheckRun
	for _, c := range r.CheckRuns {
		cr := CheckRun{ID: c.ID, Name: c.Name, Status: c.Status, Conclusion: c.Conclusion}
		if c.App != nil {
			cr.AppSlug = c.App.Slug
		}
		out = append(out, cr)
	}
	return out, nil
}

// PRComments implements GitHub: conversation comments, review bodies and
// review comments, every page.
func (g *REST) PRComments(ctx context.Context, n int) ([]Comment, error) {
	var out []Comment
	for _, src := range []string{fmt.Sprintf("/issues/%d/comments", n), fmt.Sprintf("/pulls/%d/reviews", n), fmt.Sprintf("/pulls/%d/comments", n)} {
		for page := 1; ; page++ {
			if page > maxPages {
				return nil, fmt.Errorf("pull request #%d has more comments than muster reads (%s)", n, src)
			}
			var batch []struct {
				Body string `json:"body"`
				User *struct {
					Login string `json:"login"`
					Type  string `json:"type"`
				} `json:"user"`
			}
			if err := g.do(ctx, http.MethodGet, fmt.Sprintf("%s%s?per_page=100&page=%d", g.repoPath(), src, page), "", nil, &batch,
				"reading pull request comments"); err != nil {
				return nil, err
			}
			for _, c := range batch {
				cm := Comment{Body: c.Body}
				if c.User != nil {
					cm.Author, cm.AuthorType = c.User.Login, c.User.Type
				}
				out = append(out, cm)
			}
			if len(batch) < 100 {
				break
			}
		}
	}
	return out, nil
}

// FileAt implements GitHub (the raw bytes of path at ref).
func (g *REST) FileAt(ctx context.Context, path, ref string) ([]byte, error) {
	var raw []byte
	segs := strings.Split(path, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	err := g.do(ctx, http.MethodGet, g.repoPath()+"/contents/"+strings.Join(segs, "/")+"?ref="+url.QueryEscape(ref),
		"application/vnd.github.raw+json", nil, &raw, "reading "+path+" at "+ref)
	return raw, err
}

// BranchSHA implements GitHub.
func (g *REST) BranchSHA(ctx context.Context, branch string) (string, error) {
	var r struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	err := g.do(ctx, http.MethodGet, g.repoPath()+"/git/ref/heads/"+escapeRef(branch), "", nil, &r, "reading branch "+branch)
	if err == nil && r.Object.SHA == "" {
		err = fmt.Errorf("branch %s has no sha", branch)
	}
	return r.Object.SHA, err
}

func escapeRef(ref string) string {
	segs := strings.Split(ref, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// Tags implements GitHub: every refs/tags/v* ref, in ONE request. GitHub's
// git/matching-refs (like git/refs) does not paginate: it ignores page and
// per_page and answers every matching ref on every call, with no Link header
// (measured: a repo with 282 v* tags answered the same 282 for page 1, 2, 3
// and 50). Looping pages therefore only re-reads the same list.
func (g *REST) Tags(ctx context.Context) ([]TagRef, error) {
	var refs []struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		} `json:"object"`
	}
	if err := g.do(ctx, http.MethodGet, g.repoPath()+"/git/matching-refs/tags/v", "", nil, &refs, "listing tags"); err != nil {
		return nil, err
	}
	out := make([]TagRef, 0, len(refs))
	for _, r := range refs {
		out = append(out, TagRef{Name: strings.TrimPrefix(r.Ref, "refs/tags/"), ObjectSHA: r.Object.SHA, ObjectType: r.Object.Type})
	}
	return out, nil
}

// TagObject implements GitHub.
func (g *REST) TagObject(ctx context.Context, sha string) (TagObject, error) {
	var r struct {
		SHA     string `json:"sha"`
		Message string `json:"message"`
		Tagger  *struct {
			Name string    `json:"name"`
			Date time.Time `json:"date"`
		} `json:"tagger"`
		Object struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		} `json:"object"`
	}
	if err := g.do(ctx, http.MethodGet, g.repoPath()+"/git/tags/"+url.PathEscape(sha), "", nil, &r, "reading tag object "+sha); err != nil {
		return TagObject{}, err
	}
	t := TagObject{SHA: r.SHA, TargetSHA: r.Object.SHA, TargetType: r.Object.Type, Message: r.Message}
	if r.Tagger != nil {
		t.Tagger, t.TaggedAt = r.Tagger.Name, r.Tagger.Date
	}
	return t, nil
}

// Commit implements GitHub.
func (g *REST) Commit(ctx context.Context, sha string) (string, []string, error) {
	var r struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
	}
	if err := g.do(ctx, http.MethodGet, g.repoPath()+"/git/commits/"+url.PathEscape(sha), "", nil, &r, "reading commit "+sha); err != nil {
		return "", nil, err
	}
	var ps []string
	for _, p := range r.Parents {
		ps = append(ps, p.SHA)
	}
	return r.Tree.SHA, ps, nil
}

// PRsForCommit implements GitHub.
func (g *REST) PRsForCommit(ctx context.Context, sha string) ([]PR, error) {
	var rs []restPR
	if err := g.do(ctx, http.MethodGet, g.repoPath()+"/commits/"+url.PathEscape(sha)+"/pulls?per_page=100", "", nil, &rs,
		"reading the pull requests of "+sha); err != nil {
		return nil, err
	}
	var out []PR
	for _, r := range rs {
		out = append(out, r.pr())
	}
	return out, nil
}

// Merge implements GitHub: a squash merge that GitHub refuses (409) unless
// the head is still sha — the match-head-commit guard.
func (g *REST) Merge(ctx context.Context, n int, sha string) (string, error) {
	var r struct {
		SHA    string `json:"sha"`
		Merged bool   `json:"merged"`
	}
	err := g.do(ctx, http.MethodPut, g.repoPath()+"/pulls/"+strconv.Itoa(n)+"/merge", "",
		map[string]string{"merge_method": "squash", "sha": sha}, &r, "merging pull request #"+strconv.Itoa(n))
	if err != nil {
		return "", err
	}
	if !r.Merged || r.SHA == "" {
		return "", fmt.Errorf("GitHub did not report pull request #%d merged", n)
	}
	return r.SHA, nil
}

// CreateTag implements GitHub: an annotated tag OBJECT (no ref yet).
func (g *REST) CreateTag(ctx context.Context, name, message, sha string, t Tagger) (string, error) {
	var r struct {
		SHA string `json:"sha"`
	}
	err := g.do(ctx, http.MethodPost, g.repoPath()+"/git/tags", "", map[string]any{
		"tag": name, "message": message, "object": sha, "type": "commit",
		"tagger": map[string]string{"name": t.Name, "email": t.Email, "date": t.When.UTC().Format(time.RFC3339)},
	}, &r, "creating tag object "+name)
	if err == nil && r.SHA == "" {
		err = errors.New("GitHub created a tag object with no sha")
	}
	return r.SHA, err
}

// CreateRef implements GitHub. An existing ref is ErrRefExists.
func (g *REST) CreateRef(ctx context.Context, ref, sha string) error {
	err := g.do(ctx, http.MethodPost, g.repoPath()+"/git/refs", "", map[string]string{"ref": ref, "sha": sha}, nil, "creating "+ref)
	if statusOf(err) == http.StatusUnprocessableEntity && strings.Contains(strings.ToLower(err.Error()), "already exists") {
		return fmt.Errorf("%w: %s", ErrRefExists, ref)
	}
	return err
}

// CreateCommit implements GitHub.
func (g *REST) CreateCommit(ctx context.Context, message, tree string, parents []string) (string, error) {
	var r struct {
		SHA string `json:"sha"`
	}
	err := g.do(ctx, http.MethodPost, g.repoPath()+"/git/commits", "", map[string]any{"message": message, "tree": tree, "parents": parents},
		&r, "creating a commit")
	return r.SHA, err
}

// MergeInto implements GitHub (POST merges: head merged into branch base). A
// conflict is ErrMergeConflict.
func (g *REST) MergeInto(ctx context.Context, base, head, message string) (string, error) {
	var r struct {
		SHA string `json:"sha"`
	}
	err := g.do(ctx, http.MethodPost, g.repoPath()+"/merges", "", map[string]string{"base": base, "head": head, "commit_message": message},
		&r, "merging "+head+" into "+base)
	if statusOf(err) == http.StatusConflict {
		return "", fmt.Errorf("%w: %s into %s", ErrMergeConflict, head, base)
	}
	return r.SHA, err
}

// OpenPR implements GitHub.
func (g *REST) OpenPR(ctx context.Context, title, head, base, body string) (int, error) {
	var r struct {
		Number int `json:"number"`
	}
	err := g.do(ctx, http.MethodPost, g.repoPath()+"/pulls", "", map[string]string{"title": title, "head": head, "base": base, "body": body},
		&r, "opening a pull request")
	return r.Number, err
}

// ClosePR implements GitHub.
func (g *REST) ClosePR(ctx context.Context, n int) error {
	return g.do(ctx, http.MethodPatch, g.repoPath()+"/pulls/"+strconv.Itoa(n), "", map[string]string{"state": "closed"}, nil,
		"closing pull request #"+strconv.Itoa(n))
}

// DeleteRef implements GitHub. Only a scratch ref is deleted here.
func (g *REST) DeleteRef(ctx context.Context, ref string) error {
	name, ok := strings.CutPrefix(ref, "refs/")
	if !ok || strings.HasPrefix(name, "heads/") || strings.HasPrefix(name, "tags/") {
		return fmt.Errorf("DeleteRef deletes a scratch ref, never %s", ref)
	}
	err := g.do(ctx, http.MethodDelete, g.repoPath()+"/git/refs/"+escapeRef(name), "", nil, nil, "deleting "+ref)
	if s := statusOf(err); s == http.StatusNotFound || s == http.StatusUnprocessableEntity {
		return fmt.Errorf("%w: %s", ErrRefMissing, ref)
	}
	return err
}

const updateRefsMutation = `mutation($repo: ID!, $updates: [RefUpdate!]!) { updateRefs(input: {repositoryId: $repo, refUpdates: $updates}) { clientMutationId } }`

// UpdateBranch implements GitHub. MEASURED (a user token): with
// beforeOid not the branch's sha, GitHub answers an error and the branch is
// unchanged; with force false, a non-fast-forward is "rejecting non-fast
// forward update" and unchanged; with both right, the branch moves. REST
// PATCH git/refs with force false refuses a non-fast-forward of a branch but
// pins no before sha, so it is not used.
func (g *REST) UpdateBranch(ctx context.Context, branch, before, after string) error {
	op := "moving branch " + branch + " from " + short(before) + " to " + short(after)
	var repo struct {
		NodeID string `json:"node_id"`
	}
	if err := g.do(ctx, http.MethodGet, g.repoPath(), "", nil, &repo, "reading the repository's node id"); err != nil {
		return err
	}
	if repo.NodeID == "" {
		return errors.New("GitHub gave no node id for " + g.Repo)
	}
	var r struct {
		Data struct {
			UpdateRefs *struct {
				ClientMutationID *string `json:"clientMutationId"`
			} `json:"updateRefs"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := g.doURL(ctx, http.MethodPost, g.graphqlURL(), "", map[string]any{"query": updateRefsMutation,
		"variables": map[string]any{"repo": repo.NodeID, "updates": []map[string]any{{
			"name": "refs/heads/" + branch, "beforeOid": before, "afterOid": after, "force": false}}}}, &r, op); err != nil {
		return err
	}
	// GraphQL answers 200 with an errors array.
	if len(r.Errors) > 0 {
		msgs := make([]string, 0, len(r.Errors))
		for _, e := range r.Errors {
			msgs = append(msgs, e.Message)
		}
		return fmt.Errorf("GitHub refused %s: %s", op, truncate(strings.Join(msgs, "; "), 300))
	}
	if r.Data.UpdateRefs == nil {
		return fmt.Errorf("GitHub did not confirm %s", op)
	}
	return nil
}

// graphqlURL is the GraphQL endpoint beside Base (api.github.com/graphql; a
// test's or the e2eterm fixture's loopback base gets <base>/graphql).
func (g *REST) graphqlURL() string { return g.Base + "/graphql" }

const markReadyMutation = `mutation($id: ID!) { markPullRequestReadyForReview(input: {pullRequestId: $id}) { pullRequest { number isDraft } } }`

// MarkReady implements GitHub.
func (g *REST) MarkReady(ctx context.Context, n int, nodeID string) error {
	op := "marking pull request #" + strconv.Itoa(n) + " ready for review"
	var r struct {
		Data struct {
			Mark *struct {
				PullRequest *struct {
					Number  int  `json:"number"`
					IsDraft bool `json:"isDraft"`
				} `json:"pullRequest"`
			} `json:"markPullRequestReadyForReview"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := g.doURL(ctx, http.MethodPost, g.graphqlURL(), "", map[string]any{"query": markReadyMutation,
		"variables": map[string]string{"id": nodeID}}, &r, op); err != nil {
		return err
	}
	// GraphQL answers 200 with an errors array.
	if len(r.Errors) > 0 {
		msgs := make([]string, 0, len(r.Errors))
		for _, e := range r.Errors {
			msgs = append(msgs, e.Message)
		}
		return fmt.Errorf("GitHub refused %s: %s", op, truncate(strings.Join(msgs, "; "), 300))
	}
	if r.Data.Mark == nil || r.Data.Mark.PullRequest == nil {
		return fmt.Errorf("GitHub answered no pull request while %s", op)
	}
	if pr := r.Data.Mark.PullRequest; pr.Number != n || pr.IsDraft {
		return fmt.Errorf("GitHub answered pull request #%d (draft %v) while %s", pr.Number, pr.IsDraft, op)
	}
	return nil
}

// Branches implements GitHub: every refs/heads/<prefix>* ref, in ONE
// request. git/matching-refs does not paginate (see Tags): it answers every
// match on every call and ignores page and per_page.
func (g *REST) Branches(ctx context.Context, prefix string) ([]Branch, error) {
	var refs []struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := g.do(ctx, http.MethodGet, g.repoPath()+"/git/matching-refs/heads/"+escapeRef(prefix),
		"", nil, &refs, "listing branches under "+prefix); err != nil {
		return nil, err
	}
	out := make([]Branch, 0, len(refs))
	for _, r := range refs {
		out = append(out, Branch{Name: strings.TrimPrefix(r.Ref, "refs/heads/"), SHA: r.Object.SHA})
	}
	return out, nil
}

// CommitTime implements GitHub.
func (g *REST) CommitTime(ctx context.Context, sha string) (time.Time, error) {
	var r struct {
		Commit struct {
			Committer struct {
				Date time.Time `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	err := g.do(ctx, http.MethodGet, g.repoPath()+"/commits/"+url.PathEscape(sha), "", nil, &r, "reading commit "+sha)
	return r.Commit.Committer.Date, err
}

// PRsForBranch implements GitHub.
func (g *REST) PRsForBranch(ctx context.Context, branch, state string) ([]PR, error) {
	owner, _, _ := strings.Cut(g.Repo, "/")
	q := url.Values{"head": {owner + ":" + branch}, "state": {state}, "per_page": {"100"}}
	var out []PR
	for page := 1; page <= maxPages; page++ {
		q.Set("page", strconv.Itoa(page))
		var batch []restPR
		if err := g.do(ctx, http.MethodGet, g.repoPath()+"/pulls?"+q.Encode(), "", nil, &batch, "listing the pull requests from "+branch); err != nil {
			return nil, err
		}
		for _, r := range batch {
			out = append(out, r.pr())
		}
		if len(batch) < 100 {
			return out, nil
		}
	}
	return nil, fmt.Errorf("more than %d pull requests from %s: too many to vouch for", maxPages*100, branch)
}

// OfficerPRs implements GitHub. It reads the pulls list, never the issues
// list: an officer App's token carries Pull requests but no Issues
// permission, so GitHub answers the issues list 403 for it. The pulls list
// has no author filter, so it is read newest created first and only login's
// pull requests are kept, compared case-insensitively.
//
// 🔴 When login is this App's own bot (Slug) and Created is set, the read
// stops after the first page that reaches back past the App's creation: the
// App's bot cannot have opened a pull request before the App existed, so
// every one of its pull requests is on that page or an earlier one. The cost
// is then the repository's pull requests since the App was created, not its
// whole history. Any other login reads the whole list. A list that is not
// newest-created first is an error (the stop would be a guess), and so is an
// unreadable slug or creation time, or (when stopping) a listed pull request
// without a created_at. A pull request created at exactly the App's creation
// time does not stop the read; the next page is read too.
func (g *REST) OfficerPRs(ctx context.Context, login, state string) ([]PR, error) {
	var since time.Time
	if g.Created != nil && g.Slug != nil {
		slug, err := g.Slug(ctx)
		if err != nil {
			return nil, err
		}
		if strings.EqualFold(login, BotLogin(slug)) {
			if since, err = g.Created(ctx); err != nil {
				return nil, err
			}
		}
	}
	q := url.Values{"state": {state}, "sort": {"created"}, "direction": {"desc"}, "per_page": {"100"}}
	var out []PR
	var prev time.Time
	for page := 1; page <= maxPages; page++ {
		q.Set("page", strconv.Itoa(page))
		var batch []restPR
		if err := g.do(ctx, http.MethodGet, g.repoPath()+"/pulls?"+q.Encode(), "", nil, &batch, "listing "+login+"'s pull requests"); err != nil {
			return nil, err
		}
		for _, r := range batch {
			if !since.IsZero() && r.CreatedAt.IsZero() {
				// A missing created_at would read as older than the App and
				// stop the read early, and would skip the order check below.
				return nil, fmt.Errorf("GitHub's pulls list for %s carried #%d without a created_at: cannot vouch for %s's pull requests",
					g.Repo, r.Number, login)
			}
			if !prev.IsZero() && r.CreatedAt.After(prev) {
				return nil, fmt.Errorf("GitHub's pulls list for %s is not newest created first (#%d is newer than the one before it): cannot vouch for %s's pull requests",
					g.Repo, r.Number, login)
			}
			prev = r.CreatedAt
			if strings.EqualFold(r.User.Login, login) {
				out = append(out, r.pr())
			}
		}
		if len(batch) < 100 || (!since.IsZero() && batch[len(batch)-1].CreatedAt.Before(since)) {
			return out, nil
		}
	}
	return nil, fmt.Errorf("more than %d pull requests in %s (anyone's) to find %s's among: too many to vouch for", maxPages*100, g.Repo, login)
}

// DeleteBranch implements GitHub.
func (g *REST) DeleteBranch(ctx context.Context, branch string) error {
	return g.do(ctx, http.MethodDelete, g.repoPath()+"/git/refs/heads/"+escapeRef(branch), "", nil, nil, "deleting branch "+branch)
}
