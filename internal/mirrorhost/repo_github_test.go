package mirrorhost

import (
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// THE GITHUB REPOSITORY OF A WINDOW (repo.go, originGitHubRepo): read from the
// checkout's remote.origin.url, not from its folder's name. The fixtures are
// REAL git repositories (`git init`, `git remote add`, `git worktree add`), so
// the config layout read is git's own, not one written for the test. Every name
// is pairwise distinct: the folder ("clone-fixture"), the repository it is a clone
// of ("fixture-org/real-name"), the worktree's folder ("wt-elsewhere"), the
// non-GitHub clone ("gl-clone" of "fixture-org/other-thing") and the remote-less
// one ("bare-local").

// gitFixture runs git in dir, failing the test on an error.
func gitFixture(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// githubTree builds the four checkouts under a temp root and returns their
// directories.
func githubTree(t *testing.T) (clone, worktree, gitlab, noRemote string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on PATH")
	}
	root := t.TempDir()
	clone = filepath.Join(root, "src", "clone-fixture")
	gitlab = filepath.Join(root, "src", "gl-clone")
	noRemote = filepath.Join(root, "src", "bare-local")
	worktree = filepath.Join(root, "wt", "wt-elsewhere")
	for _, d := range []string{clone, gitlab, noRemote, filepath.Dir(worktree)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gitFixture(t, clone, "init", "-q")
	gitFixture(t, clone, "remote", "add", "origin", "https://github.com/fixture-org/real-name.git")
	gitFixture(t, clone, "commit", "-q", "--allow-empty", "-m", "first")
	gitFixture(t, clone, "worktree", "add", "-q", "-b", "side", worktree)
	gitFixture(t, gitlab, "init", "-q")
	gitFixture(t, gitlab, "remote", "add", "origin", "https://gitlab.example.com/fixture-org/other-thing.git")
	gitFixture(t, noRemote, "init", "-q")
	return clone, worktree, gitlab, noRemote
}

// TestTheGitHubRepoIsTheOriginsNotTheFolders: a clone whose folder is named
// unlike its repository reports the ORIGIN's owner/name — from the top level
// and from a subdirectory, and from a git worktree of it (whose .git is a
// file) — while the chip keeps the folder's name; a non-GitHub remote and a
// repository with no remote report none. Red before originGitHubRepo existed:
// nothing reported a GitHub repository at all.
func TestTheGitHubRepoIsTheOriginsNotTheFolders(t *testing.T) {
	clone, worktree, gitlab, noRemote := githubTree(t)
	sub := filepath.Join(clone, "cmd", "tool")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	r := &repoNamer{}
	for _, c := range []struct{ cwd, name, github string }{
		{clone, "clone-fixture", "fixture-org/real-name"},
		{sub, "clone-fixture", "fixture-org/real-name"},
		{worktree, "wt-elsewhere", "fixture-org/real-name"},
		{gitlab, "gl-clone", ""},
		{noRemote, "bare-local", ""},
	} {
		if got := r.lookup(c.cwd); got.name != c.name || got.github != c.github {
			t.Errorf("lookup(%s) = %+v, want name %q github %q", c.cwd, got, c.name, c.github)
		}
	}
	windows := []WindowInfo{{ID: "@1", Cwd: clone}, {ID: "@2", Cwd: worktree}, {ID: "@3", Cwd: gitlab}, {ID: "@4", Cwd: noRemote}}
	if got := r.windowGitHubRepos(windows); !maps.Equal(got, map[string]string{"@1": "fixture-org/real-name", "@2": "fixture-org/real-name"}) {
		t.Fatalf("windowGitHubRepos = %v", got)
	}
	if got := r.windowRepos(windows); !maps.Equal(got, map[string]string{"@1": "clone-fixture", "@2": "wt-elsewhere", "@3": "gl-clone", "@4": "bare-local"}) {
		t.Fatalf("windowRepos = %v: the chip must stay the folder's name", got)
	}
}

// TestGitConfigOriginURLReadsGitsOwnRules: the last url of [remote "origin"]
// wins (git config --get), another remote's url and a commented line are not
// read, the section name is case-insensitive and the subsection exact, and a
// quoted value is unquoted. Each URL is distinct so a mutant reading the wrong
// one is seen.
func TestGitConfigOriginURLReadsGitsOwnRules(t *testing.T) {
	cases := []struct{ name, text, want string }{
		{"last wins", "[remote \"origin\"]\n\turl = https://github.com/a-org/first.git\n\turl = https://github.com/b-org/second.git\n", "https://github.com/b-org/second.git"},
		{"other remote", "[remote \"upstream\"]\n\turl = https://github.com/c-org/up.git\n[remote \"origin\"]\n\turl = git@github.com:d-org/mine.git\n", "git@github.com:d-org/mine.git"},
		{"origin then other", "[remote \"origin\"]\n\turl = https://github.com/e-org/o.git\n[remote \"fork\"]\n\turl = https://github.com/f-org/f.git\n", "https://github.com/e-org/o.git"},
		{"comment", "[remote \"origin\"]\n#\turl = https://github.com/g-org/no.git\n", ""},
		{"section case", "[Remote \"origin\"]\n\tURL = \"https://github.com/h-org/yes.git\"\n", "https://github.com/h-org/yes.git"},
		{"subsection case", "[remote \"Origin\"]\n\turl = https://github.com/i-org/no.git\n", ""},
		{"none", "[core]\n\tbare = false\n", ""},
		{"legacy section", "[remote.origin]\n\turl = https://github.com/j-org/legacy.git\n", "https://github.com/j-org/legacy.git"},
		{"legacy section case", "[Remote.ORIGIN]\n\turl = https://github.com/k-org/upper.git\n", "https://github.com/k-org/upper.git"},
		{"legacy other remote", "[remote.fork]\n\turl = https://github.com/l-org/fork.git\n", ""},
	}
	for _, c := range cases {
		if got := gitConfigOriginURL(c.text); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	// The same texts through git itself: the parser agrees with git on every
	// case above (the expectations are git's answers, not the parser's).
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on PATH")
	}
	dir := t.TempDir()
	for i, c := range cases {
		p := filepath.Join(dir, "config-"+strconv.Itoa(i))
		if err := os.WriteFile(p, []byte(c.text), 0o644); err != nil {
			t.Fatal(err)
		}
		out, _ := exec.Command("git", "config", "-f", p, "--get", "remote.origin.url").Output()
		if got := strings.TrimSpace(string(out)); got != c.want {
			t.Errorf("%s: git answers %q, the case wants %q", c.name, got, c.want)
		}
	}
}

// TestAPerWindowHostSendsItsGitHubReposOnTheReposMessage: the repos message
// carries each window's GitHub repository beside its folder name, and the
// hub's normaliser keeps only owner/name values for listed windows.
func TestAPerWindowHostSendsItsGitHubReposOnTheReposMessage(t *testing.T) {
	clone, _, gitlab, _ := githubTree(t)
	d := newDrivenMuster(t)
	b := &windowsBackend{windows: []WindowInfo{
		{Session: "scratch3", ID: "@7", Index: 3, Active: true, Pane: "%21", PanePID: 1, Command: "claude",
			Claude: &ClaudePane{Pane: "%21", PID: 9, Gen: "sess-b"}, Cwd: clone},
		{Session: "scratch3", ID: "@4", Index: 1, Pane: "%14", PanePID: 2, Command: "zsh", Cwd: gitlab},
	}}
	d.run(t, b, nil, false)
	waitUntil(t, "the first repos message", func() bool { return len(d.of(mirror.TypeRepos)) == 1 })
	m := d.of(mirror.TypeRepos)[0]
	if !maps.Equal(m.Repos, map[string]string{"@7": "clone-fixture", "@4": "gl-clone"}) ||
		!maps.Equal(m.GitHubRepos, map[string]string{"@7": "fixture-org/real-name"}) {
		t.Fatalf("repos message: repos %v github_repos %v", m.Repos, m.GitHubRepos)
	}
	ws := []mirror.Window{{Session: "s", ID: "@7"}, {Session: "s", ID: "@4"}}
	if got := mirror.NormalizeGitHubRepos(map[string]string{"@7": "Fixture-Org/Real-Name", "@4": "not a repo", "@9": "fixture-org/unlisted"}, ws); !maps.Equal(got, map[string]string{"@7": "fixture-org/real-name"}) {
		t.Fatalf("NormalizeGitHubRepos = %v", got)
	}
}
