package mirrorhost

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// THE REPO OF A WINDOW (muster's repo chip, mirror/repos.go): the git top-level
// directory's name for the window's Claude Code working directory, else that
// directory's own name — and, beside it, the GitHub repository that checkout's
// origin names (originGitHubRepo).
//
// 🔴 NO git PROCESS, AND NOT EVEN A stat PER WINDOW PER TICK. The top level is
// found the way git itself finds it — the nearest directory, from the cwd up,
// holding a `.git` (a directory, or a worktree's `.git` file) — with one stat
// per level, and the answer is CACHED PER CWD (repoNamer) for repoCacheTTL: a
// host whose windows sit in the same directories costs nothing between
// expiries, however many windows and ticks. TestTheRepoLookupIsCachedPerCwd
// counts the stats.
//
// 🔴 A HUNG FILESYSTEM MUST NOT FREEZE THE WINDOW LIST (#151). A stat on a dead
// NFS or sshfs mount blocks in the kernel, and this lookup runs in the listing
// path. So a walk gets repoWalkTimeout; past it the window is named by its cwd
// alone (cached like any answer), and the walk — which cannot be cancelled —
// is left to finish in the background, ONE per cwd at a time, its answer
// replacing the fallback if it ever returns.

// repoCacheTTL is how long a cwd's answer is kept: a `git init` (or a deleted
// .git) shows within this long.
const repoCacheTTL = 5 * time.Minute

// repoCacheMax bounds the cache; past it, it is emptied and refilled.
const repoCacheMax = 1024

// repoWalkMax bounds the walk up from a cwd.
const repoWalkMax = 64

// repoWalkTimeout bounds one cwd's walk in the listing path.
const repoWalkTimeout = 300 * time.Millisecond

// deletedSuffix is what Linux appends to a /proc/<pid>/cwd link whose directory
// was removed.
const deletedSuffix = " (deleted)"

// repoNamer answers repoName per cwd, cached.
type repoNamer struct {
	// stat is os.Stat; a test counts its calls.
	stat func(string) (os.FileInfo, error)
	// readFile is os.ReadFile (the git config read, originGitHubRepo).
	readFile func(string) ([]byte, error)
	// now is time.Now; a test moves it.
	now func() time.Time
	// timeout is repoWalkTimeout when 0; a test shortens it.
	timeout time.Duration

	mu    sync.Mutex
	cache map[string]repoEntry
	// walking is the cwds whose walk outlived its timeout and still runs.
	walking map[string]bool
}

type repoEntry struct {
	repoFound
	at time.Time
}

// repoFound is one cwd's answer: its repo name (repoName) and the GitHub
// <owner>/<name> its checkout's origin names (originGitHubRepo, "" for none).
type repoFound struct{ name, github string }

// name is cwd's repo name (repoName), from the cache while it is fresh.
func (r *repoNamer) name(cwd string) string { return r.lookup(cwd).name }

// lookup is cwd's repo name and GitHub repository, from the cache while it is
// fresh.
func (r *repoNamer) lookup(cwd string) repoFound {
	if cwd == "" {
		return repoFound{}
	}
	now := time.Now
	if r.now != nil {
		now = r.now
	}
	t := now()
	r.mu.Lock()
	if e, ok := r.cache[cwd]; ok && t.Sub(e.at) < repoCacheTTL {
		r.mu.Unlock()
		return e.repoFound
	}
	if r.walking[cwd] {
		// A walk of this cwd is still stuck: start no second one.
		r.mu.Unlock()
		return repoFound{name: cwdName(cwd)}
	}
	r.mu.Unlock()
	stat := os.Stat
	if r.stat != nil {
		stat = r.stat
	}
	readFile := os.ReadFile
	if r.readFile != nil {
		readFile = r.readFile
	}
	timeout := r.timeout
	if timeout <= 0 {
		timeout = repoWalkTimeout
	}
	done := make(chan repoFound, 1)
	go func() {
		// The config read is INSIDE the bounded walk: a hung filesystem costs
		// the window its GitHub repository exactly as it costs it its name.
		name, top := repoTop(cwd, stat)
		f := repoFound{name: name}
		if top != "" {
			f.github = originGitHubRepo(top, readFile)
		}
		done <- f
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case f := <-done:
		r.store(cwd, f, t)
		return f
	case <-timer.C:
	}
	f := repoFound{name: cwdName(cwd)}
	r.mu.Lock()
	if r.walking == nil {
		r.walking = map[string]bool{}
	}
	r.walking[cwd] = true
	r.mu.Unlock()
	r.store(cwd, f, t)
	go func() {
		late := <-done
		r.mu.Lock()
		delete(r.walking, cwd)
		r.mu.Unlock()
		r.store(cwd, late, now())
	}()
	return f
}

func (r *repoNamer) store(cwd string, f repoFound, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cache == nil || len(r.cache) >= repoCacheMax {
		r.cache = map[string]repoEntry{}
	}
	r.cache[cwd] = repoEntry{repoFound: f, at: at}
}

// cwdName is the name for cwd itself, with no filesystem access: what
// repoName answers when no .git is found.
func cwdName(cwd string) string {
	cwd = strings.TrimSuffix(cwd, deletedSuffix)
	if cwd == "" || !filepath.IsAbs(cwd) {
		return ""
	}
	start := filepath.Clean(cwd)
	if start == filepath.Dir(start) {
		return ""
	}
	return mirror.CleanRepoName(filepath.Base(start))
}

// repoName is the name muster shows for a window working in cwd: the basename
// of the nearest directory from cwd up holding a `.git` (git's own top level,
// a worktree's included), else cwd's own basename; "" for no cwd, a relative
// one, or the filesystem root. A cwd whose directory was deleted (Linux's
// " (deleted)" link) is looked up by its path without the suffix: a deleted
// subdirectory of a repository still names the repository, and a deleted
// directory outside one is named by itself. The name is CleanRepoName's.
func repoName(cwd string, stat func(string) (os.FileInfo, error)) string {
	n, _ := repoTop(cwd, stat)
	return n
}

// repoTop is repoName and the top-level directory it named the window by: ""
// when no .git was found (the name is then cwd's own).
func repoTop(cwd string, stat func(string) (os.FileInfo, error)) (string, string) {
	cwd = strings.TrimSuffix(cwd, deletedSuffix)
	if cwd == "" || !filepath.IsAbs(cwd) {
		return "", ""
	}
	start := filepath.Clean(cwd)
	d := start
	for i := 0; i < repoWalkMax; i++ {
		if _, err := stat(filepath.Join(d, ".git")); err == nil {
			if d == filepath.Dir(d) {
				break // a .git at the root names nothing
			}
			return mirror.CleanRepoName(filepath.Base(d)), d
		}
		if d == filepath.Dir(d) {
			break
		}
		d = filepath.Dir(d)
	}
	return cwdName(start), ""
}

// THE WINDOW'S GITHUB REPOSITORY: the <owner>/<name> its checkout's
// remote.origin.url names, read by githubRepoOf — THE one rule for a GitHub
// remote URL, the one resolveClone matches a spawn's clone by — "" for a
// remote that is not GitHub's, or for none.
//
// 🔴 WHY IT EXISTS. The repo chip is the checkout FOLDER's name, and a folder
// can be named differently from its repository (a clone of example-org/
// real-name into "checkout-x"). The chief read the folder name, guessed
// "<owner>/checkout-x", and every guess was refused as not connected while the real
// repository was.
//
// 🔴 STILL NO git PROCESS: the config FILE is read, once per cwd per
// repoCacheTTL, inside the same bounded walk. Only the repository's own config
// is read — `[include]` files and insteadOf rewrites are not followed — and the
// answer is a hint to muster, never a spawn's authority: resolveClone re-reads
// the clone's origin with `git config --get` when a spawn names it.

// gitConfigMax bounds the config file read.
const gitConfigMax = 1 << 20

// originGitHubRepo is the GitHub <owner>/<name> the remote.origin.url of the
// repository at top names (githubRepoOf, lowercased), "" for none. top is a
// top level repoTop found: its .git is a directory, or a worktree's (or a
// submodule's) `gitdir:` file, whose git dir may name a common dir
// (`commondir`) — where the repository's config lives.
func originGitHubRepo(top string, readFile func(string) ([]byte, error)) string {
	gitDir := filepath.Join(top, ".git")
	if b, err := readFile(gitDir); err == nil {
		// A file (reading a directory errs): "gitdir: <path>".
		line, _, _ := strings.Cut(string(b), "\n")
		p, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
		if !ok {
			return ""
		}
		if p = strings.TrimSpace(p); !filepath.IsAbs(p) {
			p = filepath.Join(top, p)
		}
		gitDir = filepath.Clean(p)
		if c, err := readFile(filepath.Join(gitDir, "commondir")); err == nil {
			cd := strings.TrimSpace(string(c))
			if !filepath.IsAbs(cd) {
				cd = filepath.Join(gitDir, cd)
			}
			gitDir = filepath.Clean(cd)
		}
	}
	b, err := readFile(filepath.Join(gitDir, "config"))
	if err != nil || len(b) > gitConfigMax {
		return ""
	}
	return githubRepoOf(gitConfigOriginURL(string(b)))
}

// gitConfigOriginURL is the LAST `url` of the `[remote "origin"]` section(s)
// in git config text (what `git config --get` answers for a key set twice), ""
// for none. The section and key names compare without case and the
// subsection exactly, as git compares them; a quoted value is unquoted.
func gitConfigOriginURL(text string) string {
	in, url := false, ""
	for _, raw := range strings.Split(text, "\n") {
		l := strings.TrimSpace(raw)
		if l == "" || l[0] == '#' || l[0] == ';' {
			continue
		}
		if l[0] == '[' {
			end := strings.IndexByte(l, ']')
			if end < 0 {
				in = false
				continue
			}
			sec, sub, hasSub := strings.Cut(strings.TrimSpace(l[1:end]), " ")
			// `[remote "origin"]`, or git's deprecated `[remote.origin]` (whose
			// subsection is compared without case, as git lowercases it).
			in = (hasSub && strings.EqualFold(sec, "remote") && strings.TrimSpace(sub) == `"origin"`) ||
				(!hasSub && strings.EqualFold(sec, "remote.origin"))
			if rest := strings.TrimSpace(l[end+1:]); in && rest != "" {
				// "[remote "origin"] url = …" on one line.
				l = rest
			} else {
				continue
			}
		}
		if !in {
			continue
		}
		k, v, ok := strings.Cut(l, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "url") {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
			v = v[1 : len(v)-1]
		}
		url = v
	}
	return url
}

// windowRepos is EVERY window's repo name, by window id — a shell's as much as
// a Claude Code session's: what a v2 per-window host sends as its repos
// message.
func (r *repoNamer) windowRepos(windows []WindowInfo) map[string]string {
	return r.windowField(windows, func(f repoFound) string { return f.name })
}

// windowGitHubRepos is every window's GitHub repository (originGitHubRepo), by
// window id, from the same cache as windowRepos: what a v2 per-window host
// sends beside the names (Msg.GitHubRepos, on the same repos message).
func (r *repoNamer) windowGitHubRepos(windows []WindowInfo) map[string]string {
	return r.windowField(windows, func(f repoFound) string { return f.github })
}

// windowField is field of every window's lookup, by window id, "" left out.
func (r *repoNamer) windowField(windows []WindowInfo, field func(repoFound) string) map[string]string {
	var out map[string]string
	for _, w := range windows {
		if w.Cwd == "" {
			continue
		}
		if n := field(r.lookup(w.Cwd)); n != "" {
			if out == nil {
				out = map[string]string{}
			}
			out[w.ID] = n
		}
	}
	return out
}
