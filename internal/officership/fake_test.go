package officership

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
)

// fakeGH is an in-memory GitHub for one repository. Nothing here talks to a
// network; every value is synthetic.
type fakeGH struct {
	mu         sync.Mutex
	slug       string
	prs        map[int]PR
	files      map[int][]File
	contents   map[string]string // "path@ref" -> body
	compares   map[string]Compare
	statuses   map[string]map[string]string
	checkRuns  map[string][]CheckRun // sha -> runs
	comments   map[int][]Comment
	branches   map[string]string
	tags       []TagRef
	tagObjects map[string]TagObject
	commits    map[string]commitInfo
	prsFor     map[string][]PR
	mergeErr   error
	refErr     error

	merged      []int
	mergedSHA   []string
	createdTags []string
	createdRefs []string
}

type commitInfo struct {
	tree    string
	parents []string
}

func newFakeGH() *fakeGH {
	return &fakeGH{slug: "ship-officer", prs: map[int]PR{}, files: map[int][]File{}, contents: map[string]string{},
		compares: map[string]Compare{}, statuses: map[string]map[string]string{}, checkRuns: map[string][]CheckRun{},
		comments: map[int][]Comment{}, branches: map[string]string{}, tagObjects: map[string]TagObject{},
		commits: map[string]commitInfo{}, prsFor: map[string][]PR{}}
}

var errNotFound = errors.New("fake: not found")

func (f *fakeGH) AppSlug(context.Context) (string, error) { return f.slug, nil }
func (f *fakeGH) PR(_ context.Context, n int) (PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.prs[n]
	if !ok {
		return PR{}, errNotFound
	}
	return p, nil
}
func (f *fakeGH) PRFiles(_ context.Context, n int) ([]File, error) { return f.files[n], nil }
func (f *fakeGH) Compare(_ context.Context, base, head string) (Compare, error) {
	c, ok := f.compares[base+"..."+head]
	if !ok {
		return Compare{}, fmt.Errorf("fake: no comparison %s...%s", base, head)
	}
	return c, nil
}
func (f *fakeGH) Statuses(_ context.Context, sha string) (map[string]string, error) {
	return f.statuses[sha], nil
}
func (f *fakeGH) CheckRuns(_ context.Context, sha, name string) ([]CheckRun, error) {
	var out []CheckRun
	for _, r := range f.checkRuns[sha] {
		if r.Name == name {
			out = append(out, r)
		}
	}
	return out, nil
}
func (f *fakeGH) PRComments(_ context.Context, n int) ([]Comment, error) { return f.comments[n], nil }
func (f *fakeGH) FileAt(_ context.Context, path, ref string) ([]byte, error) {
	b, ok := f.contents[path+"@"+ref]
	if !ok {
		return nil, errNotFound
	}
	return []byte(b), nil
}
func (f *fakeGH) BranchSHA(_ context.Context, b string) (string, error) {
	s, ok := f.branches[b]
	if !ok {
		return "", errNotFound
	}
	return s, nil
}
func (f *fakeGH) Tags(context.Context) ([]TagRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]TagRef(nil), f.tags...), nil
}
func (f *fakeGH) TagObject(_ context.Context, sha string) (TagObject, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tagObjects[sha]
	if !ok {
		return TagObject{}, errNotFound
	}
	return t, nil
}
func (f *fakeGH) Commit(_ context.Context, sha string) (string, []string, error) {
	c, ok := f.commits[sha]
	if !ok {
		return "", nil, errNotFound
	}
	return c.tree, c.parents, nil
}
func (f *fakeGH) PRsForCommit(_ context.Context, sha string) ([]PR, error) { return f.prsFor[sha], nil }
func (f *fakeGH) Merge(_ context.Context, n int, sha string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mergeErr != nil {
		return "", f.mergeErr
	}
	f.merged = append(f.merged, n)
	f.mergedSHA = append(f.mergedSHA, sha)
	return "squash" + sha, nil
}
func (f *fakeGH) CreateTag(_ context.Context, name, _, sha string, t Tagger) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	obj := "tagobj-" + name
	f.tagObjects[obj] = TagObject{SHA: obj, Tagger: t.Name, TaggedAt: t.When, TargetSHA: sha, TargetType: "commit"}
	f.createdTags = append(f.createdTags, name)
	return obj, nil
}
func (f *fakeGH) CreateRef(_ context.Context, ref, sha string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refErr != nil {
		return f.refErr
	}
	f.createdRefs = append(f.createdRefs, ref+"="+sha)
	return nil
}
func (f *fakeGH) CreateCommit(_ context.Context, _, tree string, parents []string) (string, error) {
	return "revert-of-" + parents[0], nil
}
func (f *fakeGH) MergeInto(_ context.Context, base, head, _ string) (string, error) {
	return "merged-" + head, nil
}
func (f *fakeGH) OpenPR(_ context.Context, _, head, base, _ string) (int, error) { return 900, nil }

// fakeHQ is the HQ side of the gates.
type fakeHQ struct {
	mu         sync.Mutex
	enabled    bool
	enabledErr error
	noticeErr  error
	notices    []officers.Notice
	noticePath string
	preflight  officers.Preflight
	preErr     error
}

func (h *fakeHQ) Enabled(context.Context) (bool, error) { return h.enabled, h.enabledErr }
func (h *fakeHQ) PostNotice(_ context.Context, path string, n officers.Notice) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.noticeErr != nil {
		return h.noticeErr
	}
	h.noticePath = path
	h.notices = append(h.notices, n)
	return nil
}
func (h *fakeHQ) Preflight(context.Context) (officers.Preflight, error) { return h.preflight, h.preErr }

// fixture constants. 🔴 Every sha and name is pairwise distinct, and none
// equals a constant the gates spell.
const (
	fxRepo    = "example-org/example-web"
	fxHead    = "1111111111111111111111111111111111111111"
	fxMain    = "2222222222222222222222222222222222222222"
	fxPrevTag = "3333333333333333333333333333333333333333"
	fxPRHead  = "4444444444444444444444444444444444444444"
	fxPR      = 42
)

var fxChecks = []string{"ci / build", "ci / e2e", "ci / lint"}

func fxShip() *officers.Ship {
	return &officers.Ship{Stage: officers.StageRelease, Base: "main", BranchPrefix: "officer/", RequiredChecks: fxChecks,
		ReviewCheck: "pr-review", Migrations: "db/migrations/", SensitivePaths: []string{"internal/auth/", "*token*"},
		NoticePath: officers.DefaultNoticePath, NoticeChannel: "ch/example-alerts", ReleasesPerDay: 3, MinReleaseGap: 30 * time.Minute,
		Rollout: &officers.Rollout{HealthURL: "https://svc.example.test/healthz", VersionKey: "version", LiveReads: 2,
			LiveWithin: 20 * time.Minute, HealthyFor: 2 * time.Minute, Interval: 30 * time.Second, Rollback: officers.RollbackOff,
			RollbackWithin: 10 * time.Minute}}
}

func ptrBool(b bool) *bool { return &b }

// greenMergeFixture: a pull request that passes every merge gate.
func greenMergeFixture() (*Gates, *fakeGH, *fakeHQ) {
	gh := newFakeGH()
	hq := &fakeHQ{enabled: true}
	gh.prs[fxPR] = PR{Number: fxPR, State: "open", Mergeable: ptrBool(true), Author: BotLogin(gh.slug), AuthorType: "Bot",
		HeadRef: "officer/17-fix-thing", HeadSHA: fxHead, HeadRepo: fxRepo, BaseRef: "main", BaseRepo: fxRepo, Title: "fix the thing"}
	gh.compares["main..."+fxHead] = Compare{Status: "ahead", AheadBy: 2}
	gh.statuses[fxHead] = map[string]string{"ci / build": "success", "ci / e2e": "success", "ci / lint": "success"}
	gh.checkRuns[fxHead] = []CheckRun{{ID: 5, Name: "pr-review", Status: "completed", Conclusion: "success", AppSlug: "reviewer"}}
	gh.files[fxPR] = []File{{Path: "internal/web/handler.go", Status: "modified"}, {Path: "db/migrations/0090_add.sql", Status: "added"}}
	gh.contents["db/migrations/0090_add.sql@"+fxHead] = "CREATE TABLE widgets (id bigint);\nALTER TABLE things ADD COLUMN note text;"
	g := &Gates{OfficerID: "engineering:example-web", Repo: fxRepo, Ship: fxShip(), GH: gh, HQ: hq,
		Now:      func() time.Time { return time.Date(2000, 1, 5, 15, 0, 0, 0, time.UTC) },
		InFlight: func() (string, bool) { return "", false }}
	return g, gh, hq
}
