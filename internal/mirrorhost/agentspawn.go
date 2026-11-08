package mirrorhost

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/opencodehost"
)

// THE CHIEF STARTS AN AGENT ON THIS HOST (agent-spawn): the host half of
// docs/design/chief-dispatch.md section 3. The wire and the server half are
// internal/mirror's agentspawn.go.
//
// 🔴 THIS HOST'S OWN RULES HOLD WHATEVER muster SENDS (5.5, a compromised
// server). The request names a runtime, a repository, a base branch, a prompt
// and a display name. Everything else is decided here:
//
//   - OFF unless this host opted in: -interactive, -agent-spawn, the DEFAULT
//     tmux server, and a prompt directory (agentSpawnUnavailable);
//   - the AUDIT LOG's first line is written before anything else, and a spawn
//     whose line cannot be written is refused (fail closed);
//   - the repository must be on this host's OWN allow-list (-agent-spawn-repos,
//     empty by default: nothing spawns until the owner lists one), and must
//     resolve to exactly one clone under this host's roots;
//   - the RATE LIMIT is counted from the audit log, so a restart does not reset it;
//   - the prompt must begin with muster's fixed preface (mirror.SpawnPreface):
//     Claude Code reads a first word equal to a subcommand name as that
//     subcommand even after `--` (measured);
//   - every argv word is a constant or a value built here and checked against
//     hostWordRE; the prompt never reaches tmux or a shell's parser: it travels
//     in a 0600 file a CONSTANT wrapper reads into a quoted variable;
//   - the worktree is fresh, cut from origin/<base> after a fetch, under this
//     host's own agents directory; the owner's checkout is never touched;
//   - Claude Code's folder-trust entry is written for that worktree and
//     nothing else (trustSpawnedWorktree);
//   - opencode (agentspawn_opencode.go) only beside -opencode-send and a token,
//     with the TUI's version the service's, and a model the host's own opencode
//     lists under OpenRouter — all checked before any git call; its prompt is
//     POSTED through opencode's API once the window is bound, never an argv
//     word.

// DefaultAgentSpawnRate is -agent-spawn-rate's default: 3 per 10 minutes and
// 10 per 24 hours (its daily part equals the server's daily cap).
const DefaultAgentSpawnRate = "3/10m,10/24h"

// AgentSpawnDeadline is the host's spawn deadline, from RECEIPT of the request
// (queue wait included): below the server's 150 s wait by a 30 s margin.
const AgentSpawnDeadline = 120 * time.Second

// agentSpawnReserve is what the submodule step leaves for the steps after it.
const agentSpawnReserve = 15 * time.Second

// AgentSpawnConfig is -agent-spawn and its sibling flags (docs/design/
// chief-dispatch.md 3.2).
type AgentSpawnConfig struct {
	// Enabled is -agent-spawn. OFF by default.
	Enabled bool
	// Roots are the directories whose DIRECT children are the clones a
	// worktree may be cut from.
	Roots []string
	// Dir is where worktrees go (<Dir>/<owner>__<repo>/<dispatch>).
	Dir string
	// Repos is the allow-list, <owner>/<repo>, EMPTY BY DEFAULT: an empty list
	// spawns nothing.
	Repos []string
	// Rate is -agent-spawn-rate ("" = DefaultAgentSpawnRate).
	Rate string
	// Log is the append-only audit log (JSON lines, 0600).
	Log string
	// Server is the muster URL this mirror connects to, recorded on each
	// request's first line.
	Server string
	// RuntimeDir is $XDG_RUNTIME_DIR: prompt files go under it ("" refuses).
	RuntimeDir string

	// --- test seams (no flag sets these) ---

	// ClaudeBin, when set, is the claude binary (absolute); otherwise it is
	// resolved against the tmux server's global PATH.
	ClaudeBin string
	// OpenCodeBin, when set, is the opencode binary (absolute) whose version
	// is compared with the service's; otherwise `opencode` on the tmux
	// server's global PATH.
	OpenCodeBin string
	// MirrorBin, when set, is the launcher binary an opencode window runs
	// (`<MirrorBin> opencode -s <id>`); otherwise this process's own
	// executable, so the launcher and the daemon are one build.
	MirrorBin string
	// BindWait replaces openCodeBindWait (tests).
	BindWait time.Duration
	// ClaudeState, when set, is Claude Code's state file; otherwise
	// $CLAUDE_CONFIG_DIR/.claude.json, else ~/.claude.json.
	ClaudeState string
	// Git is the git binary, absolute: New resolves "git" on PATH into it
	// when it is unset ("" after that: no git, nothing is offered).
	Git string
	// Now is the clock (nil = time.Now).
	Now func() time.Time
	// Deadline replaces AgentSpawnDeadline (tests).
	Deadline time.Duration
	// TrustLockWait replaces claudeLockWait (tests).
	TrustLockWait time.Duration
	// beforeTrustRead, when set (tests), runs between the trust entry's rename
	// and its read-back: where a concurrent Claude Code write would land.
	beforeTrustRead func(path string)
	// beforeNewWindow, when set (tests), runs just before the deadline check
	// that precedes `new-window`.
	beforeNewWindow func()
}

func (c AgentSpawnConfig) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// git is the git binary: Git, which New resolves to an absolute path at
// startup ("" when there is none: agentSpawnUnavailable says "git not found").
func (c AgentSpawnConfig) git() string { return c.Git }

func (c AgentSpawnConfig) deadline() time.Duration {
	if c.Deadline > 0 {
		return c.Deadline
	}
	return AgentSpawnDeadline
}

// hostWordRE is the characters a HOST-BUILT word of the spawn's tmux command may
// have: paths, names and ids. None of them can form a trailing ';', which tmux
// would read as its command separator.
var hostWordRE = regexp.MustCompile(`^[A-Za-z0-9/._=@:-]+$`)

// claudeWrapper is the ONLY shell text the spawned window's login shell
// interprets for claude: read the prompt file named by $1 (the `printf x`
// sentinel keeps trailing newlines), delete it, and exec the rest of the
// arguments with the prompt as ONE final argument. The prompt is only ever the
// value of a double-quoted variable, so the shell never parses it. A failed
// read exits 97.
const claudeWrapper = `p=$(cat -- "$1" && printf x) || exit 97; p=${p%x}; rm -f -- "$1"; shift; exec "$@" "$p"`

// spawnConstantWords is every CONSTANT word the spawn hands tmux (the
// argv's constants and tmux's own options). TestNoSpawnConstantWordEndsInASemicolon
// asserts none ends in ';'.
var spawnConstantWords = []string{claudeWrapper, loginScript, "-l", "-c", "--session-id", "--", "opencode", "-s",
	"new-window", "new-session", "-d", "-P", "-F", "#{window_id} #{pane_id}", "-n", "-t", "-s", mirror.AgentSpawnSession,
	"set-option", "-w", "automatic-rename", "off", mirror.DispatchOption}

// errSpawn is a refusal with its code (mirror.Spawn*) and fixed detail.
type errSpawn struct{ code, detail string }

func (e *errSpawn) Error() string { return e.code + ": " + e.detail }

func refuseSpawn(code, format string, a ...any) *errSpawn {
	return &errSpawn{code: code, detail: fmt.Sprintf(format, a...)}
}

// agentSpawner is this host's spawn state: it outlives each connection.
type agentSpawner struct {
	// mu serialises spawns: one at a time per host.
	mu sync.Mutex
	// fwd is the forward limit's record (12 per window per hour).
	fwdMu sync.Mutex
	fwd   map[string][]time.Time
}

// spawnRate is one window of the rate limit: at most n in d.
type spawnRate struct {
	n int
	d time.Duration
}

// ParseAgentSpawnRate reads -agent-spawn-rate: comma-separated <n>/<duration>,
// a duration with units m or h ("3/10m,10/24h").
func ParseAgentSpawnRate(s string) ([]spawnRate, error) {
	if strings.TrimSpace(s) == "" {
		s = DefaultAgentSpawnRate
	}
	var out []spawnRate
	for _, part := range strings.Split(s, ",") {
		n, d, ok := strings.Cut(strings.TrimSpace(part), "/")
		k, err := strconv.Atoi(n)
		if !ok || err != nil || k <= 0 {
			return nil, fmt.Errorf("-agent-spawn-rate %q: each part is <count>/<duration>, e.g. 3/10m", s)
		}
		dur, err := time.ParseDuration(d)
		if err != nil || dur <= 0 {
			return nil, fmt.Errorf("-agent-spawn-rate %q: %q is not a duration", s, d)
		}
		out = append(out, spawnRate{n: k, d: dur})
	}
	return out, nil
}

// repoNameRE is the SHAPE of an <owner>/<repo> GitHub name. A repository
// name may start with '.' (".github", ".dotfiles"); validRepoName is the
// whole rule, refusing "." and ".." and any "..".
var repoNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)

// validRepoName reports whether s is an <owner>/<repo> name this host takes:
// repoNameRE's shape, no ".." anywhere, and a repository part that is not ".".
// THE one rule, for the allow-list, a request and an origin URL.
func validRepoName(s string) bool {
	_, repo, _ := strings.Cut(s, "/")
	return repoNameRE.MatchString(s) && !strings.Contains(s, "..") && repo != "."
}

// repoOwnerRE is an <owner> GitHub name, for an "<owner>/*" entry.
var repoOwnerRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)

// ParseAgentSpawnRepos reads -agent-spawn-repos: comma-separated <owner>/<repo>
// or <owner>/* (every repository of that owner: the repository part is exactly
// the literal "*"), lower-cased.
func ParseAgentSpawnRepos(s string) ([]string, error) {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if owner, ok := strings.CutSuffix(part, "/*"); ok && repoOwnerRE.MatchString(owner) {
			out = append(out, strings.ToLower(owner)+"/*")
			continue
		}
		if !validRepoName(part) {
			return nil, fmt.Errorf("-agent-spawn-repos: %q is not <owner>/<repo> or <owner>/*", part)
		}
		out = append(out, strings.ToLower(part))
	}
	return out, nil
}

// repoAllowed reports whether repo (already a valid <owner>/<repo>) is on the
// allow-list: its exact lower-cased name, or the entry "<its owner>/*" — built
// from repo's own owner and compared for EQUALITY, never as a prefix, so
// "acme-evil/x" does not match "acme/*".
func repoAllowed(list []string, repo string) bool {
	r := strings.ToLower(repo)
	owner, _, _ := strings.Cut(r, "/")
	return slices.Contains(list, r) || slices.Contains(list, owner+"/*")
}

// --- the startup and per-spawn preconditions ---------------------------------

// agentSpawnUnavailable is why this host cannot spawn at all now, "" when it
// can: the flags, the default tmux server, the directories. Run at startup (the
// mirror logs it) and again at each spawn.
func agentSpawnUnavailable(cfg Config) string {
	sc := cfg.AgentSpawn
	switch {
	case !cfg.Interactive:
		return "not interactive"
	case !sc.Enabled:
		return "agent spawn is off"
	}
	if why := notDefaultTmuxServer(cfg.Backend); why != "" {
		return why
	}
	if sc.RuntimeDir == "" {
		return "no XDG_RUNTIME_DIR for prompt files"
	}
	if sc.Git == "" || !filepath.IsAbs(sc.Git) {
		return "git not found"
	}
	if sc.Dir == "" || !filepath.IsAbs(sc.Dir) || sc.Log == "" {
		return "the agents directory and the audit log must be absolute paths"
	}
	if !hostWordRE.MatchString(sc.Dir) || !hostWordRE.MatchString(promptDir(sc.RuntimeDir)) {
		return "a spawn path has characters tmux would parse"
	}
	dir := realOrClean(sc.Dir)
	for _, r := range sc.Roots {
		root := realOrClean(r)
		if pathWithin(dir, root) || pathWithin(root, dir) {
			return "the agents directory and a root must not contain each other"
		}
	}
	if _, err := ParseAgentSpawnRate(sc.Rate); err != nil {
		return "the spawn rate does not parse"
	}
	return ""
}

// notDefaultTmuxServer is "not the default tmux server" unless backend b is a
// Tmux on the DEFAULT server: no -L/-S, or one that resolves to
// realpath(${TMUX_TMPDIR:-/tmp})/tmux-<uid>/default (design 3.8: a non-default
// server is a tenant's by multi-tenant.md's own classification).
func notDefaultTmuxServer(b Backend) string {
	t, ok := tmuxOf(b)
	if !ok {
		return "not the default tmux server"
	}
	if t.Socket == "" && t.SocketPath == "" {
		return ""
	}
	tmpdir := os.Getenv("TMUX_TMPDIR")
	if tmpdir == "" {
		tmpdir = "/tmp"
	}
	def := filepath.Join(realOrClean(tmpdir), "tmux-"+strconv.Itoa(os.Getuid()), "default")
	switch {
	// The socket may not exist yet: its DIRECTORY is resolved, then its name.
	case t.SocketPath != "" && filepath.Join(realOrClean(filepath.Dir(t.SocketPath)), filepath.Base(t.SocketPath)) == def:
		return ""
	case t.SocketPath == "" && t.Socket == "default":
		return ""
	}
	return "not the default tmux server"
}

// tmuxOf is b's Tmux, looking through wrappers.
func tmuxOf(b Backend) (Tmux, bool) {
	for b != nil {
		switch t := b.(type) {
		case Tmux:
			return t, true
		case *Tmux:
			return *t, true
		}
		u, ok := b.(interface{ Unwrap() Backend })
		if !ok {
			return Tmux{}, false
		}
		b = u.Unwrap()
	}
	return Tmux{}, false
}

func realOrClean(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// within reports whether p is root or below it.
func pathWithin(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// promptDir is where prompt files go.
func promptDir(runtimeDir string) string { return filepath.Join(runtimeDir, "muster-mirror", "prompts") }

// agentSpawnRuntimes is what this host advertises in its hello (Msg.AgentSpawn):
// claude, when spawning is available and its binary resolves; opencode, when
// this host also SENDS to opencode (Msg.OpenCodeSend: -opencode-send and a
// token — the spawn posts the first prompt through that API) and both opencode
// and the launcher resolve. Whether the service is up, its version and its
// models are checked at each spawn, never advertised (design 3.1).
func (h *hostSide) agentSpawnRuntimes() []string {
	if h.version < 2 || agentSpawnUnavailable(h.c.cfg) != "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(h.ctx, 5*time.Second)
	defer cancel()
	var out []string
	if _, err := h.c.claudeBinary(ctx); err != nil {
		h.c.log.Printf("muster-mirror: -agent-spawn: claude is not offered: %v", err)
	} else {
		out = append(out, mirror.SpawnRuntimeClaude)
	}
	if why := h.c.openCodeSpawnOff(ctx, h.openCodeSend()); why != "" {
		h.c.log.Printf("muster-mirror: -agent-spawn: opencode is not offered: %s", why)
	} else {
		out = append(out, mirror.SpawnRuntimeOpenCode)
	}
	return out
}

// claudeBinary is the absolute claude binary a spawn runs: the test seam, or
// `claude` on the tmux server's global PATH (the window's own PATH), so a
// profile that changes PATH cannot swap it.
func (c *Client) claudeBinary(ctx context.Context) (string, error) {
	return c.serverPathBinary(ctx, "claude", c.cfg.AgentSpawn.ClaudeBin)
}

// serverPathBinary is the absolute binary name a spawn runs (or checks): seam
// when set, else name on the tmux server's global PATH (the window's own PATH),
// so a profile that changes PATH cannot swap it.
func (c *Client) serverPathBinary(ctx context.Context, name, seam string) (string, error) {
	if b := seam; b != "" {
		if !filepath.IsAbs(b) || !hostWordRE.MatchString(b) {
			return "", fmt.Errorf("the %s binary is not an absolute path tmux would leave alone", name)
		}
		return b, nil
	}
	t, ok := tmuxOf(c.cfg.Backend)
	if !ok {
		return "", errors.New("no tmux backend")
	}
	out, err := t.run(ctx, "show-environment", "-g", "PATH")
	if err != nil {
		return "", fmt.Errorf("the tmux server's PATH cannot be read: %v", err)
	}
	path := strings.TrimPrefix(strings.TrimSpace(out), "PATH=")
	for _, d := range filepath.SplitList(path) {
		if !filepath.IsAbs(d) {
			continue
		}
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 && hostWordRE.MatchString(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s is not on the tmux server's PATH", name)
}

// --- the audit log -----------------------------------------------------------

// spawnAuditRequest is a request's FIRST line, written before any other work.
type spawnAuditRequest struct {
	At           time.Time `json:"at"`
	Kind         string    `json:"kind"` // "request"
	Dispatch     int64     `json:"dispatch"`
	Server       string    `json:"server"`
	Runtime      string    `json:"runtime"`
	Repo         string    `json:"repo"`
	Base         string    `json:"base"`
	Model        string    `json:"model"`
	Name         string    `json:"name"`
	PromptSHA256 string    `json:"prompt_sha256"`
	PromptBytes  int       `json:"prompt_bytes"`
	Prompt       string    `json:"prompt"`
}

// spawnAuditAdmit is the line the rate limit counts: written UNDER the
// spawner's lock the moment a request passes the rate check, before the next
// request can count (a result line is written after the lock is released).
type spawnAuditAdmit struct {
	At       time.Time `json:"at"`
	Kind     string    `json:"kind"` // "admit"
	Dispatch int64     `json:"dispatch"`
}

// The audit log's bounds. Every string field is cut to auditFieldMax bytes
// (the prompt to mirror.MaxSpawnPrompt) before the line is marshalled, so no
// line is longer than auditLineMax; a line read back longer than that (written
// by hand, or by an older build) is skipped, never an error. A log past
// auditLogMax refuses every spawn until it is rotated.
const (
	auditFieldMax = 512
	auditLineMax  = 64 << 10
	auditLogMax   = 64 << 20
)

// capField is s cut to max bytes on a rune boundary, marked when cut.
func capField(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max - len("…[cut]")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…[cut]"
}

// spawnAuditResult is a request's SECOND line, its outcome. Admitted marks a
// request that passed the rate check: what the rate limit counts.
type spawnAuditResult struct {
	At       time.Time `json:"at"`
	Kind     string    `json:"kind"` // "result"
	Dispatch int64     `json:"dispatch"`
	Result   string    `json:"result"`
	Detail   string    `json:"detail,omitempty"`
	Admitted bool      `json:"admitted"`
	Window   string    `json:"window,omitempty"`
	Session  string    `json:"session,omitempty"`
	Worktree string    `json:"worktree,omitempty"`
	Branch   string    `json:"branch,omitempty"`
}

// appendAudit appends one JSON line to the audit log, creating it 0600 and
// O_APPEND, and fsyncs it. The line's error is the spawn's refusal when it is
// the first line.
func appendAudit(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// admittedSince counts the audit log's admissions ("admit" lines) at or after
// since. A log that cannot be read is an error (the caller refuses: the limit
// is never skipped because its record failed); a line that is corrupt or
// longer than auditLineMax is skipped, so one bad line cannot disable every
// later check.
func admittedSince(path string, since time.Time) (int, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n := 0
	r := bufio.NewReaderSize(f, auditLineMax)
	for {
		line, err := r.ReadSlice('\n')
		long := errors.Is(err, bufio.ErrBufferFull)
		for errors.Is(err, bufio.ErrBufferFull) { // skip the rest of an oversize line
			_, err = r.ReadSlice('\n')
		}
		if !long && len(line) > 0 {
			var a spawnAuditAdmit
			if json.Unmarshal(line, &a) == nil && a.Kind == "admit" && !a.At.Before(since) {
				n++
			}
		}
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
	}
}

// rateRefusal is the rate limit's refusal, "" when spawn admitted now passes
// every window of rates.
func rateRefusal(path string, rates []spawnRate, now time.Time) (string, error) {
	for _, r := range rates {
		n, err := admittedSince(path, now.Add(-r.d))
		if err != nil {
			return "", err
		}
		if n >= r.n {
			return fmt.Sprintf("rate limited: %d spawns in the last %s", n, shortDuration(r.d)), nil
		}
	}
	return "", nil
}

func shortDuration(d time.Duration) string {
	switch {
	case d%time.Hour == 0 && d >= time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + " hours"
	case d%time.Minute == 0:
		return strconv.Itoa(int(d/time.Minute)) + " minutes"
	}
	return d.String()
}

// AgentSpawnLogTail prints the last n lines of the audit log (muster-mirror
// agent-spawn-log).
func AgentSpawnLogTail(w io.Writer, path string, n int) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for _, l := range lines {
		if _, err := fmt.Fprintln(w, l); err != nil {
			return err
		}
	}
	return nil
}

// --- the request's own checks --------------------------------------------------

// baseRE is the characters a base branch may have before git judges it.
var baseRE = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)

// checkBase validates base BEFORE it reaches git: no leading '-', no "..", the
// character set, then `git check-ref-format --branch`.
func checkBase(ctx context.Context, git, base string) error {
	if base == "" {
		return nil
	}
	if strings.HasPrefix(base, "-") || strings.Contains(base, "..") || !baseRE.MatchString(base) {
		return refuseSpawn(mirror.SpawnBadBase, "branch %q is not a branch name this host accepts", base)
	}
	if err := exec.CommandContext(ctx, git, "check-ref-format", "--branch", base).Run(); err != nil {
		return refuseSpawn(mirror.SpawnBadBase, "branch %q is not a valid branch name", base)
	}
	return nil
}

// githubRepoOf is the <owner>/<repo> a GitHub remote URL names (lowercased),
// "" for any other URL.
func githubRepoOf(raw string) string {
	raw = strings.TrimSpace(raw)
	var path string
	switch {
	case strings.HasPrefix(raw, "git@github.com:"):
		path = strings.TrimPrefix(raw, "git@github.com:")
	default:
		u, err := url.Parse(raw)
		if err != nil || !strings.EqualFold(u.Hostname(), "github.com") || (u.Scheme != "https" && u.Scheme != "ssh" && u.Scheme != "git") {
			return ""
		}
		path = strings.TrimPrefix(u.Path, "/")
	}
	path = strings.TrimSuffix(strings.TrimSuffix(path, "/"), ".git")
	if !validRepoName(path) {
		return ""
	}
	return strings.ToLower(path)
}

// resolveClone is the ONE repository of repo directly under a root: its origin
// URL names repo. Entries are counted by the repository they belong to —
// realpath of `git rev-parse --git-common-dir` — so a worktree of a clone is
// that clone, not a second one; an entry whose rev-parse fails (a prunable
// worktree) is skipped. The repository's MAIN working tree (the common dir's
// parent) must itself be under a root, and is what the spawn's worktree is cut
// from. None, or two genuinely separate clones, is no_repo, naming the
// repository.
func resolveClone(ctx context.Context, git string, roots []string, repo string) (string, error) {
	want := strings.ToLower(repo)
	underARoot := func(p string) bool {
		for _, r := range roots {
			if pathWithin(p, realOrClean(r)) {
				return true
			}
		}
		return false
	}
	var found []string // main working trees, one per common dir
	for _, r := range roots {
		root := realOrClean(r)
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			dir := filepath.Join(root, e.Name())
			if fi, err := os.Stat(filepath.Join(dir, ".git")); err != nil || (!fi.IsDir() && !fi.Mode().IsRegular()) {
				continue
			}
			// The CONFIGURED url (`config`, not `remote get-url`, which applies
			// insteadOf rewrites): the repository the owner cloned.
			out, err := exec.CommandContext(ctx, git, "-C", dir, "config", "--get", "remote.origin.url").Output()
			if err != nil || githubRepoOf(string(out)) != want {
				continue
			}
			common, err := exec.CommandContext(ctx, git, "-C", dir, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
			if err != nil {
				continue // a prunable worktree: its repository is gone
			}
			cd, err := filepath.EvalSymlinks(strings.TrimSpace(string(common)))
			if err != nil || filepath.Base(cd) != ".git" {
				continue // a bare repository has no working tree to cut from
			}
			main := filepath.Dir(cd)
			if underARoot(main) && !slices.Contains(found, main) {
				found = append(found, main)
			}
		}
	}
	switch len(found) {
	case 0:
		return "", refuseSpawn(mirror.SpawnNoRepo, "repo %s has no clone under this host's roots", repo)
	case 1:
		return found[0], nil
	}
	return "", refuseSpawn(mirror.SpawnNoRepo, "repo %s has %d clones under this host's roots", repo, len(found))
}

// newSessionID is a fresh UUIDv4, minted here: claude's --session-id.
func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// --- the spawn -------------------------------------------------------------------

// spawnPlan is what a spawn created, for its cleanup.
type spawnPlan struct {
	clone, dir, branch, baseSHA, prompt string
	worktreeAdded                       bool
	// oc and session: the opencode session this spawn created ("" = none).
	oc      *openCodeSpawn
	session string
}

// undo removes what a spawn that ran no agent created: the prompt file, the
// opencode session (nothing was posted to it), the worktree (never forced),
// and the branch only while it still points at the commit it was cut from
// (update-ref with the old value: never -D).
func (p *spawnPlan) undo(git string) {
	if p.prompt != "" {
		_ = os.Remove(p.prompt)
	}
	if p.session != "" && p.oc != nil {
		p.oc.removeSession(p.session)
	}
	if !p.worktreeAdded {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, git, "-C", p.clone, "worktree", "remove", p.dir).Run()
	if p.baseSHA != "" {
		_ = exec.CommandContext(ctx, git, "-C", p.clone, "update-ref", "-d", "refs/heads/"+p.branch, p.baseSHA).Run()
	}
}

// agentSpawn serves an agent-spawn and answers agent-spawned.
func (h *hostSide) agentSpawn(m mirror.Msg) {
	w, err := h.c.spawnAgent(h.ctx, h, m, h.c.cfg.AgentSpawn.now())
	ans := mirror.Msg{Type: mirror.TypeAgentSpawned, ID: m.ID, Dispatch: m.Dispatch}
	var e *errSpawn
	switch {
	case err == nil:
		ans.Spawned = &w
	case errors.As(err, &e):
		ans.Refusal, ans.Error = e.code, e.detail
	default:
		ans.Refusal, ans.Error = mirror.SpawnFailed, err.Error()
	}
	_ = h.send(ans)
	if err == nil {
		h.nudge()
	}
}

// spawnAgent runs one request — its checks and its spawn, in the order the
// design states — and writes its audit log's SECOND line (the result) for
// every request whose first line was written.
func (c *Client) spawnAgent(ctx context.Context, h *hostSide, m mirror.Msg, received time.Time) (mirror.AgentSpawned, error) {
	w, admitted, logged, err := c.spawnAgentSteps(ctx, h, m, received)
	result := spawnAuditResult{Kind: "result", Dispatch: m.Dispatch, Admitted: admitted}
	var e *errSpawn
	switch {
	case err == nil:
		result.Result, result.Window, result.Session, result.Worktree, result.Branch = "spawned", w.Window, w.Session, w.Worktree, w.Branch
		c.log.Printf("muster-mirror: SPAWNED %s for dispatch %d in %s:%s (worktree %s), as muster asked", spawnRuntimeOf(m), m.Dispatch, w.Session, w.Window, w.Worktree)
	case errors.As(err, &e):
		result.Result, result.Detail = e.code, e.detail
		if e.code == mirror.SpawnNotListed || w.Window != "" {
			result.Window, result.Session, result.Worktree, result.Branch = w.Window, w.Session, w.Worktree, w.Branch
		}
		c.log.Printf("muster-mirror: spawn for dispatch %d refused (%s): %s", m.Dispatch, e.code, e.detail)
	default:
		result.Result, result.Detail = mirror.SpawnFailed, err.Error()
		c.log.Printf("muster-mirror: spawn for dispatch %d failed: %v", m.Dispatch, err)
	}
	if logged {
		result.At = c.cfg.AgentSpawn.now()
		result.Detail = capField(result.Detail, auditFieldMax)
		result.Window, result.Session = capField(result.Window, auditFieldMax), capField(result.Session, auditFieldMax)
		result.Worktree, result.Branch = capField(result.Worktree, auditFieldMax), capField(result.Branch, auditFieldMax)
		if aerr := appendAudit(c.cfg.AgentSpawn.Log, result); aerr != nil {
			c.log.Printf("muster-mirror: spawn audit log: the result line for dispatch %d could not be written: %v", m.Dispatch, aerr)
		}
	}
	return w, err
}

// spawnAgentSteps is the request's checks and spawn. admitted says it passed
// the rate check (what the limit counts); logged that its first audit line was
// written.
func (c *Client) spawnAgentSteps(ctx context.Context, h *hostSide, m mirror.Msg, received time.Time) (w mirror.AgentSpawned, admitted, logged bool, err error) {
	sc := c.cfg.AgentSpawn
	// 0. Off is off: no file is touched for a host that never opted in.
	if why := agentSpawnUnavailable(c.cfg); why == "agent spawn is off" || why == "not interactive" {
		return w, false, false, refuseSpawn(mirror.SpawnUnavailable, "%s", why)
	}
	req := mirror.AgentSpawnReq{}
	if m.Spawn != nil {
		req = *m.Spawn
	}
	// 1. THE AUDIT LOG'S FIRST LINE, before the rate check and any git or tmux
	// call. Fail closed.
	sum := sha256.Sum256([]byte(req.Prompt))
	f := func(v string) string { return capField(v, auditFieldMax) }
	line := spawnAuditRequest{At: received, Kind: "request", Dispatch: m.Dispatch, Server: f(sc.Server), Runtime: f(req.Runtime),
		Repo: f(req.Repo), Base: f(req.Base), Model: f(req.Model), Name: f(req.Name), PromptSHA256: hex.EncodeToString(sum[:]),
		PromptBytes: len(req.Prompt), Prompt: capField(req.Prompt, mirror.MaxSpawnPrompt)}
	if sc.Log == "" {
		return w, false, false, refuseSpawn(mirror.SpawnUnavailable, "audit log unwritable")
	}
	if fi, err := os.Stat(sc.Log); err == nil && fi.Size() > auditLogMax {
		c.log.Printf("muster-mirror: spawn for dispatch %d refused: the audit log %s is over %d MB; rotate it", m.Dispatch, sc.Log, auditLogMax>>20)
		return w, false, false, refuseSpawn(mirror.SpawnUnavailable, "audit log full (over %d MB): rotate it on the host", auditLogMax>>20)
	}
	if appendAudit(sc.Log, line) != nil {
		c.log.Printf("muster-mirror: spawn for dispatch %d refused: the audit log cannot be written", m.Dispatch)
		return w, false, false, refuseSpawn(mirror.SpawnUnavailable, "audit log unwritable")
	}
	logged = true
	c.log.Printf("muster-mirror: spawn request: dispatch %d, %s in %s (base %q), prompt %d bytes sha256 %s, from %s",
		m.Dispatch, req.Runtime, req.Repo, req.Base, len(req.Prompt), line.PromptSHA256[:12], sc.Server)
	// 2. This host's own preconditions.
	if why := agentSpawnUnavailable(c.cfg); why != "" {
		return w, false, true, refuseSpawn(mirror.SpawnUnavailable, "%s", why)
	}
	// 3. The request, field by field.
	switch req.Runtime {
	case mirror.SpawnRuntimeClaude:
		if req.Model != "" {
			return w, false, true, refuseSpawn(mirror.SpawnFailed, "muster sent a model for claude (a muster bug)")
		}
	case mirror.SpawnRuntimeOpenCode:
		// The model's SHAPE, before anything else about opencode: an
		// OpenRouter id, which this host then finds in its own opencode's
		// list (openCodeSpawnCheck).
		if _, _, ok := mirror.ParseSpawnModel(req.Model); !ok {
			return w, false, true, refuseSpawn(mirror.SpawnBadModel, "%q is not an OpenRouter model (openrouter/<vendor>/<model>)", capField(req.Model, 140))
		}
		if why := c.openCodeSendOff(); why != "" {
			return w, false, true, refuseSpawn(mirror.SpawnUnavailable, "%s", why)
		}
	default:
		return w, false, true, refuseSpawn(mirror.SpawnFailed, "muster sent an unknown runtime (a muster bug)")
	}
	if m.Dispatch <= 0 {
		return w, false, true, refuseSpawn(mirror.SpawnFailed, "muster sent no dispatch id (a muster bug)")
	}
	if len(req.Prompt) == 0 || len(req.Prompt) > mirror.MaxSpawnPrompt {
		return w, false, true, refuseSpawn(mirror.SpawnFailed, "the prompt is outside the limits (a muster bug)")
	}
	// 🔴 THE PREFACE: a prompt that does not begin with it could be a claude
	// subcommand name, which `--` does not stop (measured).
	if !strings.HasPrefix(req.Prompt, mirror.SpawnPreface) {
		return w, false, true, refuseSpawn(mirror.SpawnFailed, "the prompt does not begin with muster's preface")
	}
	if !validRepoName(req.Repo) {
		return w, false, true, refuseSpawn(mirror.SpawnNoRepo, "%q is not <owner>/<repo>", req.Repo)
	}
	// 4. THE ALLOW-LIST, before any git call.
	if !repoAllowed(sc.Repos, req.Repo) {
		return w, false, true, refuseSpawn(mirror.SpawnNoRepo, "repo %s is not on this host's allow-list", req.Repo)
	}
	if err := checkBase(ctx, sc.git(), req.Base); err != nil {
		return w, false, true, err
	}
	deadline := received.Add(sc.deadline())
	dctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	// 5. One spawn at a time. The deadline started at RECEIPT, so a spawn that
	// waited here behind a slow one has less time, not a fresh 120 s.
	c.spawner.mu.Lock()
	defer c.spawner.mu.Unlock()
	if !sc.now().Before(deadline) {
		return w, false, true, refuseSpawn(mirror.SpawnFailed, "deadline passed")
	}
	// 6. THE RATE LIMIT, counted from the audit log.
	rates, _ := ParseAgentSpawnRate(sc.Rate)
	why, rerr := rateRefusal(sc.Log, rates, sc.now())
	if rerr != nil {
		return w, false, true, refuseSpawn(mirror.SpawnUnavailable, "audit log unwritable")
	}
	if why != "" {
		return w, false, true, refuseSpawn(mirror.SpawnUnavailable, "%s", why)
	}
	// 🔴 THE ADMISSION IS RECORDED HERE, UNDER spawner.mu: the next request
	// waiting on the lock counts it. (The result line is written after the
	// lock is released, so counting it let concurrent requests past the limit.)
	if err := appendAudit(sc.Log, spawnAuditAdmit{At: sc.now(), Kind: "admit", Dispatch: m.Dispatch}); err != nil {
		return w, false, true, refuseSpawn(mirror.SpawnUnavailable, "audit log unwritable")
	}
	admitted = true
	w, err = c.spawnRuntime(dctx, h, m.Dispatch, req, deadline)
	return w, admitted, true, err
}

// spawnRuntimeOf is the runtime m asks for, for a log line.
func spawnRuntimeOf(m mirror.Msg) string {
	if m.Spawn == nil {
		return "?"
	}
	return m.Spawn.Runtime
}

// spawnRuntime is steps 7 onwards, ONE spawner for both runtimes with a branch
// where they differ: the clone, the worktree, then claude's trust entry and
// prompt file or opencode's session, the window, and for opencode the bind and
// the first prompt through its API.
func (c *Client) spawnRuntime(ctx context.Context, h *hostSide, dispatch int64, req mirror.AgentSpawnReq, deadline time.Time) (mirror.AgentSpawned, error) {
	sc := c.cfg.AgentSpawn
	git := sc.git()
	t, ok := tmuxOf(c.cfg.Backend)
	if !ok {
		return mirror.AgentSpawned{}, refuseSpawn(mirror.SpawnUnavailable, "no tmux backend")
	}
	// The tmux server must already run, and no global or `agents` session
	// @muster-dispatch may be set (it would tag every window, by inheritance).
	pidBefore, err := t.serverPID(ctx)
	if err != nil {
		return mirror.AgentSpawned{}, refuseSpawn(mirror.SpawnUnavailable, "no tmux server")
	}
	if v, _ := t.run(ctx, "show-options", "-gqv", mirror.DispatchOption); strings.TrimSpace(v) != "" {
		return mirror.AgentSpawned{}, refuseSpawn(mirror.SpawnUnavailable, "a global %s is set on the tmux server", mirror.DispatchOption)
	}
	if _, herr := t.run(ctx, "has-session", "-t", "="+mirror.AgentSpawnSession); herr == nil {
		if v, _ := t.run(ctx, "show-options", "-qv", "-t", "="+mirror.AgentSpawnSession, mirror.DispatchOption); strings.TrimSpace(v) != "" {
			return mirror.AgentSpawned{}, refuseSpawn(mirror.SpawnUnavailable, "the %s session has a %s of its own", mirror.AgentSpawnSession, mirror.DispatchOption)
		}
	}
	opencode := req.Runtime == mirror.SpawnRuntimeOpenCode
	var bin string
	var oc *openCodeSpawn
	if opencode {
		// opencode's own preconditions and the model, against the host's
		// opencode service — BEFORE any git call: a refusal here leaves
		// nothing to undo.
		if oc, err = c.openCodeSpawnCheck(ctx, req.Model); err != nil {
			return mirror.AgentSpawned{}, err
		}
		bin = oc.mirrorBin
	} else if bin, err = c.claudeBinary(ctx); err != nil {
		return mirror.AgentSpawned{}, refuseSpawn(mirror.SpawnUnavailable, "claude not found")
	}
	clone, err := resolveClone(ctx, git, sc.Roots, req.Repo)
	if err != nil {
		return mirror.AgentSpawned{}, err
	}
	base := req.Base
	if base == "" {
		out, err := exec.CommandContext(ctx, git, "-C", clone, "symbolic-ref", "--short", "refs/remotes/origin/HEAD").Output()
		if err != nil {
			return mirror.AgentSpawned{}, refuseSpawn(mirror.SpawnBadBase, "repo %s's remote has no default branch here", req.Repo)
		}
		base = strings.TrimPrefix(strings.TrimSpace(string(out)), "origin/")
		if err := checkBase(ctx, git, base); err != nil || base == "" {
			return mirror.AgentSpawned{}, refuseSpawn(mirror.SpawnBadBase, "repo %s's default branch is not usable", req.Repo)
		}
	}
	owner, name, _ := strings.Cut(strings.ToLower(req.Repo), "/")
	dir := filepath.Join(realOrClean(sc.Dir), owner+"__"+name, strconv.FormatInt(dispatch, 10))
	slug := mirror.SpawnSlug(req.Name, 40)
	branch := "muster/" + strconv.FormatInt(dispatch, 10)
	if slug != "" {
		branch += "-" + slug
	}
	winSlug := mirror.SpawnSlug(req.Name, 24)
	winName := "cc-" + strconv.FormatInt(dispatch, 10)
	sid := newSessionID()
	if opencode {
		winName = "oc-" + strconv.FormatInt(dispatch, 10)
		sid = opencodehost.NewSessionID(sc.now())
	}
	if winSlug != "" {
		winName += "-" + winSlug
	}
	promptFile := filepath.Join(promptDir(sc.RuntimeDir), strconv.FormatInt(dispatch, 10))
	for _, word := range []string{dir, branch, winName, sid, promptFile, bin} {
		if !hostWordRE.MatchString(word) {
			return mirror.AgentSpawned{}, refuseSpawn(mirror.SpawnFailed, "a spawn word has characters tmux would parse")
		}
	}
	plan := &spawnPlan{clone: clone, dir: dir, branch: branch, oc: oc}
	fail := func(e error) (mirror.AgentSpawned, error) {
		plan.undo(git)
		return mirror.AgentSpawned{}, e
	}
	// 7. Fetch, then the worktree, from origin/<base>.
	if out, err := exec.CommandContext(ctx, git, "-C", clone, "fetch", "--no-tags", "origin", base).CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			return fail(refuseSpawn(mirror.SpawnFailed, "deadline passed"))
		}
		if strings.Contains(string(out), "couldn't find remote ref") {
			return fail(refuseSpawn(mirror.SpawnBadBase, "branch %q does not exist on %s's remote", base, req.Repo))
		}
		return fail(refuseSpawn(mirror.SpawnFailed, "the fetch failed"))
	}
	shaOut, err := exec.CommandContext(ctx, git, "-C", clone, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+base+"^{commit}").Output()
	if err != nil {
		return fail(refuseSpawn(mirror.SpawnBadBase, "branch %q does not exist on %s's remote", base, req.Repo))
	}
	baseSHA := strings.TrimSpace(string(shaOut))
	if exec.CommandContext(ctx, git, "-C", clone, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch).Run() == nil {
		return fail(refuseSpawn(mirror.SpawnFailed, "a leftover branch %s exists", branch))
	}
	if _, err := os.Lstat(dir); err == nil {
		return fail(refuseSpawn(mirror.SpawnFailed, "a leftover worktree directory exists"))
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return fail(refuseSpawn(mirror.SpawnFailed, "the agents directory cannot be made"))
	}
	if err := exec.CommandContext(ctx, git, "-C", clone, "worktree", "add", "--no-track", "-b", branch, dir, "refs/remotes/origin/"+base).Run(); err != nil {
		if ctx.Err() != nil {
			return fail(refuseSpawn(mirror.SpawnFailed, "deadline passed"))
		}
		return fail(refuseSpawn(mirror.SpawnFailed, "the worktree could not be made"))
	}
	plan.worktreeAdded, plan.baseSHA = true, baseSHA
	// 8. Submodules, under their own budget: a warning, never a refusal.
	warning := ""
	if _, err := os.Stat(filepath.Join(dir, ".gitmodules")); err == nil {
		budget := time.Until(deadline) - agentSpawnReserve
		if budget <= 0 {
			warning = "submodules incomplete"
		} else {
			sctx, scancel := context.WithTimeout(ctx, budget)
			if exec.CommandContext(sctx, git, "-C", dir, "submodule", "update", "--init", "--recursive").Run() != nil {
				warning = "submodules incomplete"
			}
			scancel()
		}
	}
	// 9. claude: the trust entry, then the prompt file. opencode: its session,
	// in the worktree, on the model — which runs NOTHING until the prompt is
	// posted after the bind (step 12), so a spawn that fails before then has
	// started no agent and deletes the session (spawnPlan.undo).
	if opencode {
		if err := oc.createSession(ctx, sid, dir); err != nil {
			return fail(err)
		}
		plan.session = sid
	} else {
		if err := c.trustSpawnedWorktree(ctx, clone, dir, dispatch, true); err != nil {
			return fail(err)
		}
		if err := writePromptFile(sc.RuntimeDir, promptFile, req.Prompt); err != nil {
			return fail(refuseSpawn(mirror.SpawnFailed, "the prompt file could not be written"))
		}
		plan.prompt = promptFile
	}
	// 10. THE DEADLINE CHECK, immediately before new-window: the point of no
	// return.
	if sc.beforeNewWindow != nil {
		sc.beforeNewWindow()
	}
	if !sc.now().Before(deadline) {
		return fail(refuseSpawn(mirror.SpawnFailed, "deadline passed"))
	}
	shell, err := t.loginShell(ctx)
	if err != nil || !hostWordRE.MatchString(shell) {
		return fail(refuseSpawn(mirror.SpawnUnavailable, "no login shell"))
	}
	// 🔴 NO PROMPT IN EITHER ARGV: claude's is read from its 0600 file by the
	// constant wrapper; opencode's is posted through its API (step 12).
	argv := []string{shell, "-l", "-c", claudeWrapper, shell, promptFile, bin, "--session-id", sid, "--"}
	if opencode {
		argv = []string{shell, "-l", "-c", loginScript, shell, bin, "opencode", "-s", sid}
	}
	winID, pane, err := t.newAgentWindow(ctx, winName, dir, argv)
	if err != nil {
		return fail(refuseSpawn(mirror.SpawnFailed, "the window could not be started"))
	}
	// From here a window exists: a failure kills it, then undoes the rest.
	killFail := func(e error) (mirror.AgentSpawned, error) {
		kctx, kcancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = t.run(kctx, "kill-window", "-t", winID)
		kcancel()
		return fail(e)
	}
	if pidAfter, err := t.serverPID(ctx); err != nil || pidAfter != pidBefore {
		return killFail(refuseSpawn(mirror.SpawnFailed, "the tmux server changed during the spawn"))
	}
	if _, err := t.run(ctx, "set-option", "-w", "-t", winID, "automatic-rename", "off"); err != nil {
		return killFail(refuseSpawn(mirror.SpawnFailed, "tag not set"))
	}
	if _, err := t.run(ctx, "set-option", "-w", "-t", winID, mirror.DispatchOption, strconv.FormatInt(dispatch, 10)); err != nil {
		return killFail(refuseSpawn(mirror.SpawnFailed, "tag not set"))
	}
	if tag, err := t.dispatchTagOf(ctx, winID); err != nil || tag != dispatch {
		return killFail(refuseSpawn(mirror.SpawnFailed, "tag not set"))
	}
	// The landing check: tmux returns 0 for a missing -c path and opens in the
	// home directory instead.
	if cwd, err := t.run(ctx, "display-message", "-p", "-t", pane, "#{pane_current_path}"); err != nil || realOrClean(strings.TrimSpace(cwd)) != dir {
		return killFail(refuseSpawn(mirror.SpawnFailed, "landed outside the worktree"))
	}
	w := mirror.AgentSpawned{Session: mirror.AgentSpawnSession, Window: winID, Pane: pane, Worktree: dir, Branch: branch, AgentSession: sid, Warning: warning}
	if opencode {
		// 11. THE BIND: the launcher bound the pane to the session this host
		// created (the listing reads it: SourceLauncher, Send, that session).
		// Not bound in time: nothing has run — the window is killed and the
		// session and worktree removed.
		if !c.waitOpenCodeBind(ctx, winID, sid) {
			return killFail(refuseSpawn(mirror.SpawnFailed, "the window did not bind to its opencode session"))
		}
		// 12. THE FIRST PROMPT, AFTER THE BIND, NEVER BEFORE: posted through
		// opencode's API under an id derived from this dispatch and session.
		// 🔴 From here the window is KEPT whatever happens: the owner may be
		// typing in it. A prompt not accepted is `failed`, and the server's
		// orphan notice tells the owner about the window.
		if err := oc.postFirstPrompt(dispatch, sid, req.Prompt); err != nil {
			c.log.Printf("muster-mirror: spawn for dispatch %d: the first prompt was not accepted: %v", dispatch, err)
			return w, refuseSpawn(mirror.SpawnFailed, "prompt not accepted")
		}
	}
	// 13. LISTING BEFORE REPLY: agent-spawned only once this connection has
	// SENT a listing with the window; otherwise not_listed (the row stays
	// pending, and the next listing settles it).
	if h != nil && !h.listWindow(ctx, mirror.AgentSpawnSession, winID) {
		return w, refuseSpawn(mirror.SpawnNotListed, "the window is not in a listing yet")
	}
	return w, nil
}

// writePromptFile writes prompt to path under runtimeDir's prompt directory:
// the directory 0700, the file O_CREAT|O_EXCL 0600.
func writePromptFile(runtimeDir, path, prompt string) error {
	d := promptDir(runtimeDir)
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(d, 0o700); err != nil {
		return err
	}
	if filepath.Dir(path) != d {
		return errors.New("the prompt file is outside the prompt directory")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(prompt); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	return f.Close()
}

// listWindow has this connection read and SEND a listing, until it has sent one
// holding (session, window) — at most a few tries.
func (h *hostSide) listWindow(ctx context.Context, session, window string) bool {
	for i := 0; i < 5; i++ {
		h.mu.Lock()
		ok := h.listedWindows[windowKey{session, window}]
		h.mu.Unlock()
		if ok {
			return true
		}
		done := make(chan struct{})
		select {
		case h.kick <- done:
		case <-ctx.Done():
			return false
		case <-h.ctx.Done():
			return false
		}
		select {
		case <-done:
		case <-ctx.Done():
			return false
		case <-h.ctx.Done():
			return false
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.listedWindows[windowKey{session, window}]
}

// serverPID is the tmux server's pid; an error when no server runs.
func (t Tmux) serverPID(ctx context.Context) (int, error) {
	out, err := t.run(ctx, "display-message", "-p", "#{pid}")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(out))
}

// dispatchTagOf is window's OWN @muster-dispatch: `show-options -w` with no -A,
// so a session or global value is never inherited. 0 for none.
func (t Tmux) dispatchTagOf(ctx context.Context, window string) (int64, error) {
	if mirror.ValidWindowID(window) != nil {
		return 0, fmt.Errorf("%q is not a window id", window)
	}
	out, err := t.run(ctx, "show-options", "-w", "-t", window, "-qv", mirror.DispatchOption)
	if err != nil {
		return 0, err
	}
	v := strings.TrimSpace(out)
	if v == "" {
		return 0, nil
	}
	return strconv.ParseInt(v, 10, 64)
}

// newAgentWindow opens a DETACHED window named name in the `agents` session
// (created when absent) with working directory dir, running argv as tmux's own
// command arguments, with resumeEnv (no PATH: the window takes the server's).
// It answers the window and pane ids. Every word is already checked.
func (t Tmux) newAgentWindow(ctx context.Context, name, dir string, argv []string) (string, string, error) {
	session := mirror.AgentSpawnSession
	opts := []string{"-d", "-P", "-F", "#{window_id} #{pane_id}", "-n", name, "-c", dir}
	var out string
	var err error
	if _, herr := t.run(ctx, "has-session", "-t", "="+session); herr == nil {
		out, err = t.runEnv(ctx, resumeEnv(), append(append([]string{"new-window", "-t", "=" + session + ":"}, opts...), argv...)...)
	} else {
		out, err = t.runEnv(ctx, resumeEnv(), append(append([]string{"new-session", "-s", session}, opts...), argv...)...)
		if err != nil && strings.Contains(err.Error(), "duplicate session") {
			out, err = t.runEnv(ctx, resumeEnv(), append(append([]string{"new-window", "-t", "=" + session + ":"}, opts...), argv...)...)
		}
	}
	if err != nil {
		return "", "", err
	}
	win, pane, _ := strings.Cut(strings.TrimSpace(out), " ")
	if mirror.ValidWindowID(win) != nil || !strings.HasPrefix(pane, "%") {
		return "", "", fmt.Errorf("tmux answered %q for the new window", out)
	}
	return win, pane, nil
}

// --- Claude Code's folder-trust entry (design 3.6.1) -----------------------------

// claudeStateFile is Claude Code's user state file: the seam, else
// $CLAUDE_CONFIG_DIR/.claude.json when the mirror's CLAUDE_CONFIG_DIR is set
// (measured: that file, nothing under $HOME), else ~/.claude.json.
func (c *Client) claudeStateFile() string {
	if p := c.cfg.AgentSpawn.ClaudeState; p != "" {
		return p
	}
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, ".claude.json")
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".claude.json")
}

// trustSpawnedWorktree writes projects[dir].hasTrustDialogAccepted = true into
// Claude Code's state file: THE ONLY FUNCTION IN muster THAT WRITES CLAUDE
// CODE'S CONFIGURATION. Every check is made at the moment of the write:
//
//  1. dir is absolute, clean and its own real path (measured: the key must be
//     the real path, no trailing slash);
//  2. dir is EXACTLY <agent-spawn-dir>/<owner>__<repo>/<dispatch>, the dispatch
//     being this spawn's;
//  3. the agents directory and the roots do not contain each other (startup);
//  4. this spawn CREATED dir (created) and the clone's `worktree list` lists it;
//  5. the runtime is claude (the caller's: only spawnClaude calls this).
//
// The write changes ONE value: decoded as raw JSON at two levels, re-encoded
// with HTML escaping off, written to a temporary file and renamed; then read
// back and compared semantically. A mismatch is REFUSED and NEVER RESTORED
// (restoring could revert a write Claude Code made in between). A missing or
// unparseable file is refused, never created or repaired.
func (c *Client) trustSpawnedWorktree(ctx context.Context, clone, dir string, dispatch int64, created bool) error {
	sc := c.cfg.AgentSpawn
	bad := func(why string) error { return refuseSpawn(mirror.SpawnFailed, "trust entry not written: %s", why) }
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return bad("not an absolute, clean path")
	}
	if real, err := filepath.EvalSymlinks(dir); err != nil || real != dir {
		return bad("not its own real path")
	}
	spawnDir := realOrClean(sc.Dir)
	repoDir := filepath.Dir(dir)
	if filepath.Dir(repoDir) != spawnDir || filepath.Base(dir) != strconv.FormatInt(dispatch, 10) || !strings.Contains(filepath.Base(repoDir), "__") {
		return bad("not a worktree of this spawn under the agents directory")
	}
	for _, r := range sc.Roots {
		root := realOrClean(r)
		if pathWithin(dir, root) || pathWithin(spawnDir, root) || pathWithin(root, spawnDir) {
			return bad("under a root")
		}
	}
	if !created {
		return bad("this spawn did not create it")
	}
	out, err := exec.CommandContext(ctx, sc.git(), "-C", clone, "worktree", "list", "--porcelain").Output()
	if err != nil || !slices.Contains(strings.Split(string(out), "\n"), "worktree "+dir) {
		return bad("the clone does not list it as a worktree")
	}
	path := c.claudeStateFile()
	// 🔴 NEVER THROUGH A SYMLINK. That is muster's own choice and fails closed;
	// Claude Code 2.1.289 itself does write through one (allowSymlink: true).
	fi, err := os.Lstat(path)
	if err != nil {
		return bad("Claude Code's state file cannot be read")
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return bad("Claude Code's state file is a symlink or not a regular file")
	}
	// 🔴 CLAUDE CODE'S OWN LOCK around the read-modify-rename (lockClaudeState):
	// without it a write of Claude Code's landing between this read and the
	// rename is lost. Not had within the bound: no entry, and the window
	// simply shows the trust dialog (fail closed).
	unlock, err := lockClaudeState(path, sc.trustLockWait())
	if err != nil {
		return bad("Claude Code's state file is locked by another writer")
	}
	defer unlock()
	orig, err := os.ReadFile(path)
	if err != nil {
		return bad("Claude Code's state file cannot be read")
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(orig, &top) != nil || top == nil {
		return bad("Claude Code's state file does not parse")
	}
	projects := map[string]json.RawMessage{}
	if raw, ok := top["projects"]; ok {
		if json.Unmarshal(raw, &projects) != nil || projects == nil {
			return bad("Claude Code's projects do not parse")
		}
	}
	if _, ok := projects[dir]; ok {
		return bad("an entry for this new directory already exists")
	}
	projects[dir] = json.RawMessage(`{"hasTrustDialogAccepted":true}`)
	pj, err := encodeJSON(projects)
	if err != nil {
		return bad("encode")
	}
	top["projects"] = json.RawMessage(pj)
	want, err := encodeJSON(top)
	if err != nil {
		return bad("encode")
	}
	tmp := path + ".tmp." + strconv.Itoa(os.Getpid()) + "." + hex.EncodeToString(sumShort(want))
	// The file's own mode, kept, as Claude Code keeps it (Claude Code creates
	// the file 0600, but an existing file keeps whatever mode it has).
	if err := os.WriteFile(tmp, want, fi.Mode().Perm()); err != nil {
		return bad("the temporary file cannot be written")
	}
	if err := os.Chmod(tmp, fi.Mode().Perm()); err != nil {
		_ = os.Remove(tmp)
		return bad("the temporary file cannot be written")
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return bad("the rename failed")
	}
	if sc.beforeTrustRead != nil {
		sc.beforeTrustRead(path)
	}
	got, err := os.ReadFile(path)
	if err != nil || !sameJSON(got, want) {
		// 🔴 NEVER RESTORE: a mismatch most likely means Claude Code wrote the
		// file in between, and restoring would revert that write.
		return bad("the file read back differs (another writer?)")
	}
	return nil
}

// Claude Code's state-file lock (proper-lockfile, as Claude Code 2.1.289 uses
// it): a DIRECTORY at <realpath of the file>.lock, made with mkdir (atomic),
// removed to release; one whose mtime is older than claudeLockStale is stale
// and taken over, as proper-lockfile does.
const (
	claudeLockStale = 10 * time.Second
	claudeLockWait  = 5 * time.Second
)

var errClaudeStateLocked = errors.New("Claude Code's state file is locked")

// lockClaudeState takes path's lock, waiting at most wait.
func lockClaudeState(path string, wait time.Duration) (func(), error) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	dir := real + ".lock"
	deadline := time.Now().Add(wait)
	for {
		err := os.Mkdir(dir, 0o755)
		if err == nil {
			return func() { _ = os.Remove(dir) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		// The deadline is checked on EVERY pass, a stale lock's included.
		if !time.Now().Before(deadline) {
			return nil, errClaudeStateLocked
		}
		if fi, serr := os.Stat(dir); serr == nil && time.Since(fi.ModTime()) > claudeLockStale {
			// A stale lock that cannot be removed (not empty, a read-only
			// file system, denied) is not taken: fail closed, no entry.
			if rerr := os.Remove(dir); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
				return nil, errClaudeStateLocked
			}
			continue
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (c AgentSpawnConfig) trustLockWait() time.Duration {
	if c.TrustLockWait > 0 {
		return c.TrustLockWait
	}
	return claudeLockWait
}

// encodeJSON is v as Claude Code-style JSON: two-space indent, NO HTML
// escaping (a '<', '>' or '&' in any string stays as it is).
func encodeJSON(v any) ([]byte, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

// sameJSON reports whether a and b decode to the same value.
func sameJSON(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return string(xa) == string(ya)
}

func sumShort(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:4]
}

// --- forwarding into a spawned window (design 3.10) -------------------------------

// forwardsPerHour bounds the forwards one window takes in an hour (a guard
// against paste loops).
const forwardsPerHour = 12

// errNotMusterWindow refuses a paste naming a dispatch into a window whose own
// tag differs or is absent.
var errNotMusterWindow = errors.New("that window is not the one muster started for this dispatch")

// errForwardLimited refuses the 13th forward into one window in an hour.
var errForwardLimited = fmt.Errorf("this window already took %d forwards in the last hour", forwardsPerHour)

// checkDispatchPaste is THE host's bound on forwarding: a paste naming a
// dispatch lands only in a window whose OWN @muster-dispatch equals it, and at
// most forwardsPerHour times an hour per window.
func (h *hostSide) checkDispatchPaste(ctx context.Context, m mirror.Msg) error {
	t, ok := tmuxOf(h.c.cfg.Backend)
	if !ok || m.Window == "" {
		return errNotMusterWindow
	}
	tag, err := t.dispatchTagOf(ctx, m.Window)
	if err != nil || tag != m.Dispatch {
		return errNotMusterWindow
	}
	sp := &h.c.spawner
	sp.fwdMu.Lock()
	defer sp.fwdMu.Unlock()
	if sp.fwd == nil {
		sp.fwd = map[string][]time.Time{}
	}
	now := time.Now()
	key := m.Window
	kept := sp.fwd[key][:0]
	for _, at := range sp.fwd[key] {
		if now.Sub(at) < time.Hour {
			kept = append(kept, at)
		}
	}
	if len(kept) >= forwardsPerHour {
		sp.fwd[key] = kept
		return errForwardLimited
	}
	sp.fwd[key] = append(kept, now)
	return nil
}

// --- the cleanup sweep (design 3.3) -----------------------------------------------

// SweepAgentWorktrees removes every spawned worktree under the agents directory
// whose dispatch names no live tagged window, when it holds nothing: a clean
// working tree and no commit the remote lacks. Anything else is KEPT and logged
// with the reason. Never forced: `worktree remove` without --force, and the
// branch deleted only by `update-ref -d` against the tip it was verified at.
// It also deletes prompt files more than an hour old.
func (c *Client) SweepAgentWorktrees(ctx context.Context, live map[int64]bool) {
	sc := c.cfg.AgentSpawn
	if sc.Dir == "" {
		return
	}
	git := sc.git()
	repos, _ := os.ReadDir(realOrClean(sc.Dir))
	for _, r := range repos {
		if !r.IsDir() || !strings.Contains(r.Name(), "__") {
			continue
		}
		rd := filepath.Join(realOrClean(sc.Dir), r.Name())
		ds, _ := os.ReadDir(rd)
		for _, d := range ds {
			id, err := strconv.ParseInt(d.Name(), 10, 64)
			if !d.IsDir() || err != nil || id <= 0 || live[id] {
				continue
			}
			if keep := c.removeWorktree(ctx, git, filepath.Join(rd, d.Name())); keep != "" {
				c.log.Printf("muster-mirror: spawned worktree %s kept: %s", filepath.Join(rd, d.Name()), keep)
			}
		}
	}
	if sc.RuntimeDir != "" {
		ps, _ := os.ReadDir(promptDir(sc.RuntimeDir))
		for _, p := range ps {
			if fi, err := p.Info(); err == nil && fi.Mode().IsRegular() && time.Since(fi.ModTime()) > time.Hour {
				_ = os.Remove(filepath.Join(promptDir(sc.RuntimeDir), p.Name()))
			}
		}
	}
}

// removeWorktree removes one spawned worktree, or answers why it is kept.
func (c *Client) removeWorktree(ctx context.Context, git, dir string) string {
	run := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, git, append([]string{"-C", dir}, args...)...).Output()
		return strings.TrimSpace(string(out)), err
	}
	status, err := run("status", "--porcelain")
	if err != nil {
		return "its status cannot be read"
	}
	if status != "" {
		return "it has uncommitted changes"
	}
	unpushed, err := run("rev-list", "--count", "HEAD", "--not", "--remotes")
	if err != nil {
		return "its commits cannot be read"
	}
	if unpushed != "0" {
		return "its branch has commits the remote lacks"
	}
	branch, _ := run("symbolic-ref", "--short", "HEAD")
	tip, err := run("rev-parse", "HEAD")
	if err != nil {
		return "its HEAD cannot be read"
	}
	common, err := run("rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "its clone cannot be found"
	}
	clone := filepath.Dir(common)
	if err := exec.CommandContext(ctx, git, "-C", clone, "worktree", "remove", dir).Run(); err != nil {
		return "git refused to remove it"
	}
	if strings.HasPrefix(branch, "muster/") {
		_ = exec.CommandContext(ctx, git, "-C", clone, "update-ref", "-d", "refs/heads/"+branch, tip).Run()
	}
	return ""
}

// sweepLoop runs the worktree sweep at start and every hour after that, for a
// host with -agent-spawn: a crash leaves no worktree unaccounted for.
func (c *Client) sweepLoop(ctx context.Context) {
	for {
		c.sweepOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Hour):
		}
	}
}

// sweepOnce reads which dispatches have a live tagged window, then sweeps. A
// listing that cannot be read sweeps NOTHING: an unknown live set is not an
// empty one.
func (c *Client) sweepOnce(ctx context.Context) {
	wl, ok := WindowListerOf(c.cfg.Backend)
	if !ok {
		return
	}
	// Never beside a spawn: a spawn between its worktree and its window has a
	// directory no window names yet.
	c.spawner.mu.Lock()
	defer c.spawner.mu.Unlock()
	lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ws, err := wl.Windows(lctx)
	if err != nil {
		c.log.Printf("muster-mirror: worktree sweep skipped: the windows cannot be read: %v", err)
		return
	}
	live := map[int64]bool{}
	for _, w := range ws {
		if w.Dispatch > 0 {
			live[w.Dispatch] = true
		}
	}
	c.SweepAgentWorktrees(lctx, live)
}

// AgentSpawnUnavailable is why this host cannot spawn agents now ("" when it
// can): what the daemon logs at startup.
func (c *Client) AgentSpawnUnavailable() string { return agentSpawnUnavailable(c.cfg) }
