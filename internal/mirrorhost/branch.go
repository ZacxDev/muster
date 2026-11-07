package mirrorhost

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// THE BRANCH OF A WINDOW (mirror/branches.go): the branch the window's working
// directory has checked out, read the way git itself reads it — the HEAD file
// of the repository the cwd is in, "ref: refs/heads/<branch>". A detached HEAD
// (a commit id), no repository, or a name outside mirror.ValidBranch is no
// branch.
//
// 🔴 NO git PROCESS. The repository is found as repo.go finds it — the nearest
// `.git` from the cwd up, one stat per level — and a worktree's (or a
// submodule's) `.git` FILE is followed to its "gitdir: <dir>", whose HEAD is
// that worktree's own. That location is CACHED PER CWD for repoCacheTTL, like
// the repo name; the HEAD itself is re-read at most every branchCacheTTL PER
// HEAD FILE — so windows in one checkout share one small read, and a
// `git checkout` shows within branchCacheTTL plus one Refresh.
//
// 🔴 A HUNG FILESYSTEM MUST NOT FREEZE THE WINDOW LIST (repo.go's #151 rule):
// each lookup gets repoWalkTimeout; past it the window has no branch (cached
// like any answer), and the stuck read — which cannot be cancelled — is left
// to finish in the background, ONE per key at a time, its answer replacing the
// empty one if it ever returns.

// branchCacheTTL is how long one HEAD file's branch is kept.
const branchCacheTTL = 10 * time.Second

// maxHeadBytes bounds a HEAD (or a `.git` file) read. A longer file is read as
// no answer at all — never cut, since a cut name is a different branch's.
const maxHeadBytes = 1024

// headPrefix begins a HEAD that names a branch.
const headPrefix = "ref: refs/heads/"

// branchReader answers each window's branch, cached.
type branchReader struct {
	// stat is os.Stat and read a bounded file read; a test counts their calls.
	stat func(string) (os.FileInfo, error)
	read func(string) ([]byte, error)
	// now is time.Now; a test moves it.
	now func() time.Time
	// timeout is repoWalkTimeout when 0; a test shortens it.
	timeout time.Duration

	mu sync.Mutex
	// heads is each cwd's HEAD file ("" for none), for repoCacheTTL.
	heads map[string]repoEntry
	// branches is each HEAD file's branch ("" for none), for branchCacheTTL.
	branches map[string]repoEntry
	// stuck is the keys ("cwd:" or "head:" + path) whose lookup outlived its
	// timeout and still runs.
	stuck map[string]bool
}

// windowBranches is EVERY window's branch, by window id: what a v2 per-window
// host sends as its branches message (before NormalizeBranches).
func (r *branchReader) windowBranches(windows []WindowInfo) map[string]string {
	var out map[string]string
	for _, w := range windows {
		if w.Cwd == "" {
			continue
		}
		if b := r.branch(w.Cwd); b != "" {
			if out == nil {
				out = map[string]string{}
			}
			out[w.ID] = b
		}
	}
	return out
}

// branch is cwd's branch, from the caches while they are fresh.
func (r *branchReader) branch(cwd string) string {
	head := r.cached("cwd:"+cwd, &r.heads, repoCacheTTL, func() string { return findHead(cwd, r.statFn(), r.readFn()) })
	if head == "" {
		return ""
	}
	return r.cached("head:"+head, &r.branches, branchCacheTTL, func() string { return headBranch(head, r.readFn()) })
}

func (r *branchReader) statFn() func(string) (os.FileInfo, error) {
	if r.stat != nil {
		return r.stat
	}
	return os.Stat
}

func (r *branchReader) readFn() func(string) ([]byte, error) {
	if r.read != nil {
		return r.read
	}
	return readHead
}

// cached is (*cache)[key] while it is younger than ttl, else fn's answer —
// bounded by the timeout, "" past it (and fn left to finish in the
// background, its late answer stored) — stored in *cache.
func (r *branchReader) cached(key string, cache *map[string]repoEntry, ttl time.Duration, fn func() string) string {
	now := time.Now
	if r.now != nil {
		now = r.now
	}
	t := now()
	r.mu.Lock()
	if e, ok := (*cache)[key]; ok && t.Sub(e.at) < ttl {
		r.mu.Unlock()
		return e.name
	}
	if r.stuck[key] {
		r.mu.Unlock()
		return ""
	}
	r.mu.Unlock()
	timeout := r.timeout
	if timeout <= 0 {
		timeout = repoWalkTimeout
	}
	done := make(chan string, 1)
	go func() { done <- fn() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case v := <-done:
		r.store(cache, key, v, t)
		return v
	case <-timer.C:
	}
	r.mu.Lock()
	if r.stuck == nil {
		r.stuck = map[string]bool{}
	}
	r.stuck[key] = true
	r.mu.Unlock()
	r.store(cache, key, "", t)
	go func() {
		late := <-done
		r.mu.Lock()
		delete(r.stuck, key)
		r.mu.Unlock()
		r.store(cache, key, late, now())
	}()
	return ""
}

func (r *branchReader) store(cache *map[string]repoEntry, key, v string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if *cache == nil || len(*cache) >= repoCacheMax {
		*cache = map[string]repoEntry{}
	}
	(*cache)[key] = repoEntry{name: v, at: at}
}

// findHead is the HEAD file of the repository cwd is in: the nearest `.git`
// from cwd up (repoName's walk) — a directory's own HEAD, or, for a `.git`
// FILE (a worktree, a submodule), the HEAD in the directory its
// "gitdir: <dir>" line names (relative to the `.git` file's directory when
// not absolute). "" for no cwd, a relative one, no repository, or a `.git`
// file that names no gitdir.
func findHead(cwd string, stat func(string) (os.FileInfo, error), read func(string) ([]byte, error)) string {
	cwd = strings.TrimSuffix(cwd, deletedSuffix)
	if cwd == "" || !filepath.IsAbs(cwd) {
		return ""
	}
	d := filepath.Clean(cwd)
	for i := 0; i < repoWalkMax; i++ {
		git := filepath.Join(d, ".git")
		if fi, err := stat(git); err == nil {
			if d == filepath.Dir(d) {
				return "" // a .git at the root names nothing (repoName's rule)
			}
			if fi.IsDir() {
				return filepath.Join(git, "HEAD")
			}
			b, err := read(git)
			if err != nil {
				return ""
			}
			line, _, _ := strings.Cut(string(b), "\n")
			dir, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
			if dir = strings.TrimSpace(dir); !ok || dir == "" {
				return ""
			}
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(d, dir)
			}
			return filepath.Join(filepath.Clean(dir), "HEAD")
		}
		if d == filepath.Dir(d) {
			break
		}
		d = filepath.Dir(d)
	}
	return ""
}

// headBranch is the branch HEAD file head names: "ref: refs/heads/<branch>"
// on its first line, the branch a mirror.ValidBranch; "" for anything else —
// a detached HEAD (a commit id), another ref, an unreadable file.
func headBranch(head string, read func(string) ([]byte, error)) string {
	b, err := read(head)
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(b), "\n")
	name, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), headPrefix)
	if !ok || !mirror.ValidBranch(name) {
		return ""
	}
	return name
}

// readHead is a HEAD (or `.git` file) read: catalog.go's readBounded at
// maxHeadBytes — a regular file only (a FIFO would block), a longer one an
// error, never its first maxHeadBytes.
func readHead(path string) ([]byte, error) { return readBounded(path, maxHeadBytes) }
