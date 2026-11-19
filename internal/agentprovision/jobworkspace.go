package agentprovision

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/jobs"
	"github.com/ZacxDev/muster/internal/podagent"
	"github.com/ZacxDev/muster/internal/provision"
)

// THE MACHINE JOB API'S WORKSPACE OPERATIONS (internal/api jobs.go): give a
// commit job a git worktree of its own, isolated from that commit's agent
// configuration; read a file back out of the job's directory after its turn;
// delete that file once read; remove the worktree after the job. Each runs a
// command inside the agent's live instance through the driver's exec, exactly
// as the terminal does, so a driver that cannot exec refuses them with
// provision.ErrUnsupported.
//
// 🔴 NO COMMAND INTERPOLATES A VALUE INTO SHELL TEXT. The script is a
// constant and every caller-supplied value travels as a positional argument
// ($1, $2, …), so a sha or a path cannot become shell syntax. Every value is
// also validated here (jobs.ValidSHA, jobs.ValidResultPath — the same
// predicates the HTTP handler applies — and a positive job id), before any
// exec.

// JobOverlayRootFiles are the files at a job worktree's root that are replaced
// by the repository's default branch's (or removed, when the default branch
// has none). Together with every `.claude` directory (jobOverlayScript) they
// are what the overlay covers, and nothing else is.
//
//   - CLAUDE.md, CLAUDE.local.md, AGENTS.md: the instructions the CLI loads at
//     launch from the project root (CLAUDE.local.md is its personal memory
//     file, loaded beside CLAUDE.md).
//   - .mcp.json: the project's MCP servers. The CLI STARTS each server's
//     command, so it is code the commit would otherwise run.
var JobOverlayRootFiles = []string{"CLAUDE.md", "CLAUDE.local.md", "AGENTS.md", ".mcp.json"}

// gitRunsNoRepoCommands opens every script here that runs git in the agent's
// workspace. That workspace is the SESSION's: a tool call can write its .git
// (and its $HOME/.gitconfig), and git runs commands that config names. The
// overrides are exported as GIT_CONFIG_* — git's command-line-config channel,
// which outranks the repository's own config and reaches every git the script's
// git starts — rather than as `-c` per call, so a git added to a script later
// cannot miss them.
//
// WHAT IS NEUTRALISED BY NAME (gitNeutralised; each row measured by
// TestTheJobScriptsRunNoPlantedCommandAndShowNoneATurnCredential):
//   - core.fsmonitor=false — status / checkout / ls-files run it.
//   - core.hooksPath=/dev/null — every hook in a hooks directory.
//   - hook.<event>.enabled=false, for every githooks(5) event — config-based
//     hooks (hook.<name>.command / .event, newer git), which a hooks path does
//     not govern. The image's git (2.39) predates them and ignores the keys.
//   - core.alternateRefsCommand= — a fetch from a repository with alternates
//     runs it.
//   - protocol.ext.allow=never — an ext:: remote URL runs a command on fetch.
//   - core.sshCommand=ssh — a repository-set ssh command runs on an ssh fetch.
//   - remote.origin.uploadpack=git-upload-pack — for a local-path origin the
//     upload-pack command runs LOCALLY (set-head --auto contacts origin).
//   - credential.helper= — an empty value resets the helper list; a "!cmd"
//     helper would run when origin asks for credentials.
//   - core.askPass= — the askpass program the same request could run.
//   - submodule.recurse=false, fetch.recurseSubmodules=false.
//
// And one by ENVIRONMENT, because config cannot do it: GIT_PROXY_COMMAND= (set,
// empty). core.gitProxy is multi-valued and "the first match wins", so a value
// planted in the repository's config would beat any override appended after it;
// the environment variable replaces the config lookup entirely, and an empty one
// means no proxy. (A git:// origin is reached by `git remote set-head --auto`.)
//
// 🔴 WHAT STILL RUNS, AND THE ONE THING STANDING BETWEEN IT AND THE CREDENTIALS:
//   - filter drivers (filter.<any name>.smudge / .process, selected by
//     .gitattributes or .git/info/attributes) — `git worktree add` and the
//     overlay's checkouts run them; their names are the session's to choose, so
//     no override can name them;
//   - anything else git runs from config that this list does not name.
//
// They run as the agent's own user (which the session already is), and they see
// no turn credential ONLY because every exec into the container is stripped of
// them (the adapter's scrubexec driver). That prefix is THE credential guard for
// these; weakening it re-opens the route this list cannot close.
//
// ⚠ uploadpack.packObjectsHook (honoured only from protected config: global,
// system, command line) is NOT in the list: the pull-request fetch runs with
// GIT_CONFIG_GLOBAL=/dev/null, which its upload-pack inherits, so a global hook
// is not read there (measured: it does not fire), and an empty override would
// make upload-pack run an empty command instead of pack-objects.
var gitNeutralised = append([][2]string{
	{"core.fsmonitor", "false"},
	{"core.hooksPath", "/dev/null"},
	{"core.alternateRefsCommand", ""},
	{"protocol.ext.allow", "never"},
	{"core.sshCommand", "ssh"},
	{"remote.origin.uploadpack", "git-upload-pack"},
	{"credential.helper", ""},
	{"core.askPass", ""},
	{"submodule.recurse", "false"},
	{"fetch.recurseSubmodules", "false"},
}, hookEventsDisabled()...)

// gitHookEvents is githooks(5)'s list of hook events (git 2.55).
var gitHookEvents = []string{
	"applypatch-msg", "pre-applypatch", "post-applypatch", "pre-commit", "pre-merge-commit",
	"prepare-commit-msg", "commit-msg", "post-commit", "pre-rebase", "post-checkout", "post-merge",
	"pre-push", "pre-receive", "update", "proc-receive", "post-receive", "post-update",
	"reference-transaction", "push-to-checkout", "pre-auto-gc", "post-rewrite", "sendemail-validate",
	"fsmonitor-watchman", "post-index-change",
}

func hookEventsDisabled() [][2]string {
	out := make([][2]string, 0, len(gitHookEvents))
	for _, e := range gitHookEvents {
		out = append(out, [2]string{"hook." + e + ".enabled", "false"})
	}
	return out
}

// gitRunsNoRepoCommands is gitNeutralised as a shell prelude (values are
// literals without spaces or quotes, so they need no quoting).
var gitRunsNoRepoCommands = func() string {
	var b strings.Builder
	b.WriteString("export GIT_PROXY_COMMAND= GIT_CONFIG_COUNT=" + strconv.Itoa(len(gitNeutralised)))
	for i, kv := range gitNeutralised {
		n := strconv.Itoa(i)
		b.WriteString(" GIT_CONFIG_KEY_" + n + "=" + kv[0] + " GIT_CONFIG_VALUE_" + n + "=" + kv[1])
	}
	b.WriteString("\n")
	return b.String()
}()

// jobOverlayScript prepares job worktree $2 of the checkout $1 at commit $3,
// of pull request $5 of the repository at the URL $4, taking the agent
// configuration from commit $6 ("" = the default branch's tip). $7 is "" for
// an ANONYMOUS fetch of an https URL, or — for a worker pool with a deploy key,
// whose $4 is then the path of the KEY HOLDER's mirror — the --upload-pack
// command every fetch from it runs (podagent.MirrorUploadPack; see "THE DEPLOY
// KEY" below). $8 is 1 to overlay the repository's agent configuration, 0 to
// strip it and restore none (a deploy-key pool's job:
// agentspec.StripsRepoConfig). $9 is a BASE commit to fetch ("" = none; see
// "THE BASE" below). ${10} is how many seconds the one-time unshallow may take
// ("0" = do not unshallow; see "THE UNSHALLOW" below). The remaining arguments
// are JobOverlayRootFiles:
//
//  1. with $8 = 1 (an overlay to take from it), resolve the default branch,
//     refs/remotes/origin/HEAD (asking origin with `git remote set-head --auto`
//     only when the ref is missing), and fail with NoDefaultBranchMessage when
//     it cannot be resolved: there is then
//     no trusted configuration to give the job;
//  2. fetch refs/pull/$5/head ANONYMOUSLY from $4 into refs/muster-jobs/<job
//     id> (see below), and refuse (NotOnPullRequestMessage) unless $3 is that
//     head or an ancestor of it;
//  3. with a config ref $6, refuse (ConfigRefNotOnDefaultBranchMessage)
//     unless it is reachable from the default branch: from the checkout's
//     origin/HEAD, or else from the default branch's tip as fetched — the same
//     anonymous fetch as the pull request's, of $4's HEAD, into
//     refs/muster-jobs/<job id>-config. The overlay's SOURCE is then $6, not
//     the default branch's tip;
//  4. refuse (SymlinkOverlayMessage) when the source carries a symlink among
//     the overlaid paths: a symlink restored from it could point at a file the
//     commit controls;
//  5. `git worktree add --detach` the commit at $2 — the agent's own checkout,
//     its HEAD, branch and working tree, is not touched;
//  6. THE OVERLAY: remove every `.claude` directory (or file) anywhere in the
//     worktree, then restore every `.claude` the source has, at the same
//     paths; remove each root file in JobOverlayRootFiles, then restore the
//     source's copy where it has one.
//
// 🔴 WITH $8 = 0 THE REMOVAL STILL RUNS AND NOTHING IS RESTORED: none of the
// repository's agent configuration — the commit's or the default branch's —
// is in the worktree, so the turn has the job's prompt and muster's own
// configuration (the managed-policy CLAUDE.md, the pod's user settings) only.
// Skipping the overlay never means "leave the commit's configuration in place".
// Steps 3 and 4 (the source) are skipped: there is no source.
//
// 🔴 THE BASE ($9 set). The commit is fetched from $4 — the same URL, the same
// anonymity or deploy key, the same refusals as the pull request's head — into
// refs/muster-jobs/<job id>-base, BEFORE the worktree is made, so the turn can
// `git diff <base>...HEAD` with no credential of its own (the worktree shares
// the checkout's objects and refs). A base that cannot be fetched fails the job
// (BaseFetchMessage, naming it) and nothing is made. The ref is deleted with
// the worktree (jobRemoveScript).
//
// 🔴 THE UNSHALLOW (${10} > 0). A SHALLOW checkout (`git rev-parse
// --is-shallow-repository`; muster's own clone, internal/podagent checkout.go,
// never makes one — a session or an operator did) is made complete ONCE,
// before the pull request's fetch: `git fetch --unshallow` of $4's HEAD, with
// the same anonymity (or the same --upload-pack from the mirror) as the fetches
// below. WHY: a fetch of a pull request whose merge-base with what the
// checkout has lies BELOW the shallow boundary downloads that history again —
// the job's ref is deleted afterwards, so the next such fetch cannot offer it
// as common ground either. Measured: a depth-1 clone gained ~18,500 objects
// per such pull-request fetch, a complete one 1; one worker held 1,389 packs
// (161 GiB) this way. The unshallow runs under `timeout` (${10} seconds, a
// share of the job's own budget: unshallowBudget) with pack.threads=1 on BOTH
// sides: on the fetch's own command line, and — from a mirror, whose
// upload-pack and pack-objects run in this container too, and to which git
// does not pass the fetch's -c — inside the --upload-pack command itself
// ("git -c pack.threads=1 -c safe.directory=… upload-pack"). Measured on a
// ~700,000-object repository: the serving pack-objects peaked at 25 threads
// and 710 MiB without it, 1 thread and 312 MiB with it.
//
// ITS FAILING — a timeout, the network, no `timeout` binary — never fails the
// job: UnshallowFailedNote goes to stderr and the job proceeds on the shallow
// checkout. What a failed attempt costs is then contained:
//   - the tmp_pack_* / tmp_idx_* / tmp_rev_* files a cut-off transfer leaves in
//     objects/pack are removed — only those newer than the attempt's start, so
//     a pack another process had in flight BEFORE it is left (one that another
//     git started during the attempt is not told apart);
//   - the failure is counted in UnshallowFailedFile, and the next attempt waits
//     1 h after one failure, doubling up to 16 h, instead of costing every
//     later job a third of its deadline; a success removes the count.
//
// A complete checkout skips all of it (one rev-parse).
//
// 🔴 THE DEPLOY KEY ($7 set). This script never sees the key, and nothing it
// runs can use it: it runs in the AGENT's container, as the session's uid,
// where the key is not mounted. Before it runs, PrepareJobWorktree has had the
// pod's KEY HOLDER (another container, another uid) probe the key for write
// access and fetch the pull request's head and the base INTO ITS MIRROR
// (podagent.MirrorScript); $4 is that mirror's path, mounted read-only here,
// and the fetches below are local ones from it, each with --upload-pack=$7. The
// mirror is owned by the holder's uid, and git refuses another user's
// repository ("detected dubious ownership"). 🔴 safe.directory as `-c` or
// GIT_CONFIG_* on THIS git does not help: for a local path git clears the
// config environment before it spawns upload-pack, and git 2.39's upload-pack
// checks ownership itself (measured on the image's git, 2.39.5). $7 puts the
// `-c safe.directory=<mirror>` on upload-pack's own command line. A newer git
// (2.55) fetches without it, so this side's tests cannot show the failure; the
// image's cc-deploykey-smoke does. No default branch is resolved here ($8 is 0
// on such a pool), so origin — the mirror — is never asked.
// (Whatever this side runs, it runs as the session's uid with the turn
// credentials scrubbed — the class "WHAT STILL RUNS" above names — and the
// key is not in this container to be reached.)
//
// 🔴 WHY A CONFIG REF MUST BE ON THE DEFAULT BRANCH. Its purpose is to pin
// WHICH trusted configuration a job runs with (the caller recorded a version of
// it before submitting, and the default branch may have moved since). Any
// other commit would let a caller name the pull request's own commit — fetched
// into this very checkout a step earlier — and so give the commit under review
// its own configuration back: exactly what the overlay exists to prevent.
//
// 🔴 WHY `.claude` AT EVERY DEPTH, NOT ONLY AT THE ROOT. The root `.claude/`
// holds the project settings (hooks: commands the CLI runs), agents and
// skills; and the CLI also discovers `.claude/skills` in SUBDIRECTORIES as a
// session works in them (measured in the CLI bundle: its own guidance names
// "a subproject directory (e.g. `ios/.claude/skills/…`)"), and a skill's or
// an agent's frontmatter can declare hooks. Replacing only the root would let
// a commit add `sub/.claude/…` and have it loaded.
//
// 🔴 EVERY PATH LIST IS READ NUL-SEPARATED (ls-tree -z, grep -z, sed -z, sort
// -z, xargs -0). Without -z, git C-quotes a path with a byte outside ASCII
// under the default core.quotePath ("…\303\251…", with a leading quote), so a
// symlink at .claude/skills/é/… slipped past the symlink check and was
// restored, and a non-ASCII file in a nested .claude/ made the restore check
// out a quoted path that does not exist — failing every sha job on the
// repository.
//
// 🔴 AND IN THE C LOCALE, WITH grep -a. Under a UTF-8 locale GNU grep treats a
// record whose name is not valid UTF-8 as binary and DROPS it from -z output
// (exit 0, "binary file matches"), so a symlink at .claude/x<0xE9> passed the
// check and a nested sub<0xE9>/.claude/ was not restored. LC_ALL=C makes every
// tool here work on bytes, whatever locale the instance's exec carries.
//
// 🔴 THE FETCH IS ANONYMOUS, AND THE SCRIPT ENFORCES IT RATHER THAN ASSUMING IT.
// An agent holding an approved grant on a connector that reaches github.com has
// git's GLOBAL config routing every https://github.com/ URL through muster's
// credentialed proxy (internal/podagent gitroute.go: url.<proxy>.insteadOf and
// http.<proxy>.extraHeader, written with `git config --global`). The fetch runs
// with GIT_CONFIG_GLOBAL=/dev/null and GIT_CONFIG_NOSYSTEM=1, so that routing is
// not read, with `credential.helper=` (no helper) and `http.extraHeader=` (no
// header), and it refuses (AnonymousFetchMessage) when what git would still use
// differs: the URL `git ls-remote --get-url` resolves is not $4 (an insteadOf
// in the checkout's own config), or that config names an extra header or a
// credential setting. So the bytes come from $4 itself, with no credential.
// Pool workers hold no GitHub grant at all (internal/api createPoolJob) — this is
// what makes the fetch anonymous for any agent.
//
// ⚠ NOT OVERLAID, AND SO THE COMMIT'S: every other file — including
// CLAUDE.md / AGENTS.md files below the root (instructions the CLI reads as it
// works in those directories; text, not commands) and anything the turn
// chooses to run from the tree (a build script, a test). The overlay keeps the
// COMMIT's configuration from configuring the reviewer; it does not make the
// commit's code safe to execute.
var jobOverlayScript = gitRunsNoRepoCommands + `export LC_ALL=C
main=$1 wt=$2 sha=$3 url=$4 pr=$5 cfg=$6 up=$7 overlay=$8 base=$9 unshallow=${10}
shift 10
unset GIT_SSH_COMMAND
cd "$main" || exit 1
def=
if [ "$overlay" = 1 ]; then
	def=$(git rev-parse --verify --quiet 'refs/remotes/origin/HEAD^{commit}') ||
		{ git remote set-head origin --auto >/dev/null 2>&1 &&
			def=$(git rev-parse --verify --quiet 'refs/remotes/origin/HEAD^{commit}'); } ||
		{ echo "` + NoDefaultBranchMessage + `" >&2; exit 4; }
fi
ref=refs/muster-jobs/${wt##*/}
got=$(GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 git ls-remote --get-url "$url") || exit 1
creds=$(GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 git config --show-scope --get-regexp '^(http\..*extraheader|credential\..*)$' |
	grep -v '^command')
if [ "$got" != "$url" ] || [ -n "$creds" ]; then
	echo "` + AnonymousFetchMessage + ` (git would fetch $got)" >&2
	exit 7
fi
gd=
if [ "$unshallow" -gt 0 ] 2>/dev/null && [ "$(git rev-parse --is-shallow-repository 2>/dev/null)" = true ]; then
	gd=$(git rev-parse --path-format=absolute --git-common-dir 2>/dev/null) || gd=
fi
if [ -n "$gd" ]; then
	mark=$gd/` + UnshallowFailedFile + ` n=0 age=0 hold=0
	if [ -f "$mark" ]; then
		n=$(head -c 16 "$mark" | tr -cd 0-9)
		n=${n:-1}
		k=$n
		[ "$k" -gt ` + unshallowBackoffDoublings + ` ] && k=` + unshallowBackoffDoublings + `
		hold=$(( ` + unshallowBackoffBaseSeconds + ` << (k - 1) ))
		age=$(( $(date +%s) - $(stat -c %Y "$mark") ))
	fi
	if [ "$n" -gt 0 ] && [ "$age" -lt "$hold" ]; then
		echo "` + UnshallowBackoffNote + ` ($n failed attempt(s), the last ${age}s ago; next try after ${hold}s)" >&2
	else
		upu=$up
		case $up in
		git\ *) upu="git -c pack.threads=1 ${up#git }" ;;
		esac
		start=$gd/muster-unshallow-start
		: > "$start"
		if GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 GIT_TERMINAL_PROMPT=0 \
			timeout -s TERM "$unshallow" git -c credential.helper= -c http.extraHeader= -c pack.threads=1 \
			fetch --quiet --no-tags --unshallow ${upu:+"--upload-pack=$upu"} "$url"; then
			rm -f "$mark"
		else
			echo "` + UnshallowFailedNote + `" >&2
			find "$gd/objects/pack" -maxdepth 1 -type f \( -name 'tmp_pack_*' -o -name 'tmp_idx_*' -o -name 'tmp_rev_*' \) \
				-newer "$start" -exec rm -f {} + 2>/dev/null
			echo $((n + 1)) > "$mark"
		fi
		rm -f "$start"
	fi
fi
GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 GIT_TERMINAL_PROMPT=0 \
	git -c credential.helper= -c http.extraHeader= fetch --quiet --no-tags ${up:+"--upload-pack=$up"} "$url" "+refs/pull/$pr/head:$ref" ||
	{ echo "` + PullFetchMessage + `" >&2; exit 8; }
head=$(git rev-parse --verify --quiet "$ref^{commit}") || exit 1
if [ "$sha" != "$head" ] && ! git merge-base --is-ancestor "$sha" "$head" 2>/dev/null; then
	echo "` + NotOnPullRequestMessage + ` (pull request $pr's head is $head)" >&2
	exit 6
fi
if [ -n "$base" ]; then
	GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 GIT_TERMINAL_PROMPT=0 \
		git -c credential.helper= -c http.extraHeader= fetch --quiet --no-tags ${up:+"--upload-pack=$up"} "$url" "+$base:$ref-base" ||
		{ echo "` + BaseFetchMessage + ` (base_sha $base)" >&2; exit 12; }
	got=$(git rev-parse --verify --quiet "$ref-base^{commit}") && [ "$got" = "$base" ] ||
		{ echo "` + BaseFetchMessage + ` (base_sha $base: the fetched ref names ${got:-nothing})" >&2; exit 12; }
fi
src=$def
if [ "$overlay" != 1 ]; then
	src=
elif [ -n "$cfg" ]; then
	if ! git merge-base --is-ancestor "$cfg" "$def" 2>/dev/null; then
		GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 GIT_TERMINAL_PROMPT=0 \
			git -c credential.helper= -c http.extraHeader= fetch --quiet --no-tags ${up:+"--upload-pack=$up"} "$url" "+HEAD:$ref-config" ||
			{ echo "` + DefaultBranchFetchMessage + `" >&2; exit 10; }
		tip=$(git rev-parse --verify --quiet "$ref-config^{commit}") || exit 1
		if ! git merge-base --is-ancestor "$cfg" "$tip" 2>/dev/null; then
			echo "` + ConfigRefNotOnDefaultBranchMessage + ` (the default branch is at $tip)" >&2
			exit 9
		fi
	fi
	src=$cfg
fi
if [ -n "$src" ]; then
	tab=$(printf '\t')
	links=$(git ls-tree -r -z "$src" | grep -azE "^120000 [^$tab]*$tab(.*/)?\.claude(/|\$)" | tr '\0' '\n')
	for p in "$@"; do
		case $(git ls-tree "$src" -- "$p") in
		120000\ *) links="$links
$p" ;;
		esac
	done
	if [ -n "$links" ]; then
		echo "` + SymlinkOverlayMessage + `" >&2
		printf '%s\n' "$links" | head -n 20 >&2
		exit 5
	fi
fi
mkdir -p "${wt%/*}" || exit 1
git worktree add --quiet --detach "$wt" "$sha" || exit 1
cd "$wt" || exit 1
find . -name .git -prune -o -name .claude -prune -exec rm -rf {} + || exit 1
for p in "$@"; do
	rm -rf ./"$p" || exit 1
done
[ -n "$src" ] || exit 0
git ls-tree -r -z --name-only "$src" | grep -azE '(^|/)\.claude(/|$)' |
	sed -zE 's#^((.*/)?\.claude)(/.*)?$#\1#' | sort -zu |
	xargs -0r -n1 sh -c 'git --literal-pathspecs checkout --quiet "$1" -- "$2" || exit 255' sh "$src" || exit 1
for p in "$@"; do
	if git cat-file -e "$src:$p" 2>/dev/null; then
		git --literal-pathspecs checkout --quiet "$src" -- "$p" || exit 1
	fi
done`

// UnshallowFailedNote is what the worktree script prints (to stderr) when the
// one-time unshallow of a shallow checkout failed; the job proceeds.
const UnshallowFailedNote = "muster: could not unshallow the checkout (git fetch --unshallow failed or ran out of time); the job proceeds on the shallow checkout"

// UnshallowFailedFile is the file in the checkout's git directory counting the
// unshallow's consecutive failures (its mtime is the last one's time). While it
// exists the unshallow is skipped for unshallowBackoffBaseSeconds × 2^(n−1)
// seconds (n capped at unshallowBackoffDoublings + 1: 1 h, 2 h, … 16 h); a
// success removes it.
const UnshallowFailedFile = "muster-unshallow-failed"

// UnshallowBackoffNote is what the worktree script prints (to stderr) when it
// skips the unshallow because the last attempts failed.
const UnshallowBackoffNote = "muster: not unshallowing the checkout yet: it failed recently"

const (
	unshallowBackoffBaseSeconds = "3600"
	unshallowBackoffDoublings   = "5"
)

// UnshallowMaxBudget bounds the one-time unshallow of a shallow checkout
// (jobOverlayScript, "THE UNSHALLOW"); unshallowBudget also caps it at a third
// of what is left of the job's own deadline.
const UnshallowMaxBudget = 10 * time.Minute

// unshallowMinBudget: with less than this the unshallow is not tried at all.
const unshallowMinBudget = 30 * time.Second

// unshallowBudget is how many seconds the unshallow may take within ctx: the
// lesser of UnshallowMaxBudget and a third of the time left before ctx's
// deadline, 0 (not tried) below unshallowMinBudget. A third leaves the rest of
// the job's deadline for its fetch, its turn and its read-back: an unshallow
// that runs out of time is killed and the job proceeds, never times out.
func unshallowBudget(ctx context.Context) int {
	d := UnshallowMaxBudget
	if dl, ok := ctx.Deadline(); ok {
		if r := time.Until(dl) / 3; r < d {
			d = r
		}
	}
	if d < unshallowMinBudget {
		return 0
	}
	return int(d / time.Second)
}

// NoDefaultBranchMessage opens the error of a job whose repository's default
// branch could not be resolved.
const NoDefaultBranchMessage = "the repository's default branch (origin/HEAD) could not be resolved, so there is no trusted agent configuration to give the job's worktree; nothing was run"

// AnonymousFetchMessage opens the error of a job whose checkout's own git
// configuration would rewrite the anonymous fetch URL or add a header or a
// credential to it.
const AnonymousFetchMessage = "the checkout's git configuration rewrites the anonymous fetch URL or adds a header or credential to it, so the pull request was not fetched; nothing was run"

// PullFetchMessage opens the error of a job whose pull request could not be
// fetched (podagent's, which the key holder's script fails with too).
const PullFetchMessage = podagent.PullFetchMessage

// BaseFetchMessage opens the error of a job whose base_sha could not be
// fetched; the base follows it.
const BaseFetchMessage = podagent.BaseFetchMessage

// BaseRefSuffix is what a job's base ref adds to its pull-request ref:
// refs/muster-jobs/<job id>-base.
const BaseRefSuffix = "-base"

// DeployKeyWritableMessage opens the error of a job whose worker pool's deploy
// key could open GitHub's receive-pack: it can push, so it was not used.
const DeployKeyWritableMessage = podagent.DeployKeyWritableMessage

// ConfigRefStrippedMessage opens the error of a job with a config_ref on an
// agent whose jobs get none of the repository's configuration
// (agentspec.StripsRepoConfig). The job API refuses such a job when it is
// submitted; this holds when the configuration moved since.
const ConfigRefStrippedMessage = "the job names a config_ref, but its worker pool's jobs get none of the repository's agent configuration (a pool with a deploy key), so there is nothing to overlay; nothing was run"

// DeployKeyRepoMismatchMessage opens the error of a job whose repository is
// not the one its worker pool's deploy key is configured for.
const DeployKeyRepoMismatchMessage = "the job's repository is not the one its worker pool's deploy key is configured for (MUSTER_AGENT_CC_POOL_DEPLOY_KEYS), so the key was not used; nothing was run"

// NotOnPullRequestMessage opens the error of a job whose sha is not on its pull
// request; the pull request's head follows it.
const NotOnPullRequestMessage = "the job's sha is neither the pull request's head nor an ancestor of it, so it was not checked out; nothing was run"

// ConfigRefNotOnDefaultBranchMessage opens the error of a job whose
// config_ref is not reachable from the repository's default branch; the
// default branch's tip follows it.
const ConfigRefNotOnDefaultBranchMessage = "the job's config_ref is not reachable from the repository's default branch, so its agent configuration is not trusted; nothing was run"

// DefaultBranchFetchMessage opens the error of a job whose config_ref is not
// on the checkout's origin/HEAD and whose default branch could then not be
// fetched anonymously to look for it there.
const DefaultBranchFetchMessage = "the job's config_ref is not on the checkout's origin/HEAD, and the repository's default branch could not be fetched anonymously to look for it; nothing was run"

// DefaultAnonymousGitBase is the anonymous fetch base: the public GitHub.
const DefaultAnonymousGitBase = "https://github.com/"

// SymlinkOverlayMessage opens the error of a job refused because the default
// branch carries a symlink among the overlaid paths; the paths follow it.
const SymlinkOverlayMessage = "the repository's default branch has a symlink among the agent-configuration paths a job's worktree takes from it, which could point at a file the job's commit controls; nothing was run"

// jobRemoveScript removes job worktree $2 of the checkout $1: `git worktree
// remove --force --force` (twice: a locked worktree too), then the directory
// itself if git left it (it was not a registered worktree, or git refused),
// then `git worktree prune`, and the job's fetched pull-request and
// default-branch refs. It fails only when the directory is still there.
var jobRemoveScript = gitRunsNoRepoCommands + `cd "$1" 2>/dev/null && git worktree remove --force --force "$2" >/dev/null 2>&1
rm -rf "$2"
cd "$1" 2>/dev/null && git worktree prune >/dev/null 2>&1
cd "$1" 2>/dev/null && git update-ref -d "refs/muster-jobs/${2##*/}" >/dev/null 2>&1
cd "$1" 2>/dev/null && git update-ref -d "refs/muster-jobs/${2##*/}-config" >/dev/null 2>&1
cd "$1" 2>/dev/null && git update-ref -d "refs/muster-jobs/${2##*/}-base" >/dev/null 2>&1
if [ -e "$2" ] || [ -L "$2" ]; then
	echo "the job's worktree $2 could not be removed" >&2
	exit 1
fi`

// readScript prints at most $2 bytes of the file $1. It follows symlinks: the
// path is checked lexically (jobs.ValidResultPath), not resolved, so a symlink
// in the workspace can point the read elsewhere in the instance.
const readScript = `head -c "$2" -- "$1"`

// jobDirs is ag's working directory and, for a positive jobID, that job's
// worktree, refusing an agent with no repository when a worktree is asked for.
func jobDirs(ag agents.Agent, jobID int64) (main, wt string, err error) {
	if strings.TrimSpace(ag.Repo) == "" {
		return "", "", fmt.Errorf("agentprovision: agent %q has no repository, so a job cannot have a worktree of it", ag.Name)
	}
	if jobID <= 0 {
		return "", "", fmt.Errorf("agentprovision: job id %d is not positive", jobID)
	}
	main, err = agentspec.ClaudeCodeWorkingDir(ag)
	if err != nil {
		return "", "", err
	}
	return main, agents.JobWorktreeDir(main, jobID), nil
}

// AnonymousFetchURL is the URL a commit job's pull request is fetched from for
// repository owner/name: base + owner/name (base empty: DefaultAnonymousGitBase).
func AnonymousFetchURL(base, repo string) string {
	if base == "" {
		base = DefaultAnonymousGitBase
	}
	return base + repo
}

// PrepareJobWorktree creates job jobID's worktree of ag's repository at sha,
// which must be pull request pr's head of repo (owner/name) or an ancestor of
// it — fetched anonymously, or, for an agent of a pool with a deploy key,
// through the pod's key holder (jobFetchWithKey) — with the overlay taken from
// configRef ("" = the default branch's tip; else a full commit id the default
// branch must reach), and returns its absolute path in the instance. On a pool
// with a deploy key the repository's agent configuration is removed and none
// restored (agentspec.StripsRepoConfig), and a configRef is refused. With
// baseSHA that commit is fetched the same way into
// refs/muster-jobs/<jobID>-base. ag's own checkout is not moved. On an error a
// partly made worktree may be left; RemoveJobWorktree removes it either way.
func (a *Adapter) PrepareJobWorktree(ctx context.Context, ag agents.Agent, jobID int64, repo string, pr int, sha, configRef, baseSHA string) (string, error) {
	if !jobs.ValidSHA(sha) {
		return "", fmt.Errorf("agentprovision: %q is not a full commit id (40 lowercase hex)", sha)
	}
	if configRef != "" && !jobs.ValidSHA(configRef) {
		return "", fmt.Errorf("agentprovision: config ref %q is not a full commit id (40 lowercase hex)", configRef)
	}
	if baseSHA != "" && !jobs.ValidSHA(baseSHA) {
		return "", fmt.Errorf("agentprovision: base %q is not a full commit id (40 lowercase hex)", baseSHA)
	}
	if pr <= 0 {
		return "", fmt.Errorf("agentprovision: pull request %d is not positive", pr)
	}
	if !jobs.ValidRepo(repo) {
		return "", fmt.Errorf("agentprovision: %q is not a repository owner/name", repo)
	}
	strip := a.StripsRepoConfig(ag)
	if strip && configRef != "" {
		return "", fmt.Errorf("%s (pool %q, config_ref %s)", ConfigRefStrippedMessage, ag.WorkerPool, configRef)
	}
	main, wt, err := jobDirs(ag, jobID)
	if err != nil {
		return "", err
	}
	url, uploadPack := AnonymousFetchURL(a.anonGitBase, repo), ""
	if k, ok := agentspec.PoolDeployKeyFor(ag, a.spec); ok {
		if err := a.jobFetchWithKey(ctx, ag, k, repo, pr, baseSHA); err != nil {
			return "", err
		}
		if uploadPack, err = podagent.MirrorUploadPack(agentspec.MirrorPath); err != nil {
			return "", err
		}
		url = agentspec.MirrorPath
	}
	overlay := "1"
	if strip {
		overlay = "0"
	}
	var out bytes.Buffer
	cmd := append([]string{"sh", "-c", jobOverlayScript, "muster-job-worktree", main, wt, sha,
		url, strconv.Itoa(pr), configRef, uploadPack, overlay, baseSHA, strconv.Itoa(unshallowBudget(ctx))}, JobOverlayRootFiles...)
	if err := provision.Exec(ctx, a.driver, agents.RefOf(ag), cmd, nil, &out, &out); err != nil {
		return "", fmt.Errorf("create the worktree %s at %s: %w: %s", wt, sha, err, strings.TrimSpace(capString(out.String(), 1024)))
	}
	return wt, nil
}

// StripsRepoConfig reports whether ag's jobs get none of the repository's
// agent configuration (agentspec.StripsRepoConfig, with this adapter's
// configuration). The job API asks it before recording a job with a
// config_ref.
func (a *Adapter) StripsRepoConfig(ag agents.Agent) bool {
	return agentspec.StripsRepoConfig(ag, a.spec)
}

// PoolRepoRefusal is agentspec.PoolRepoRefusal with this adapter's
// configuration: why ag's spec would be refused for working on a repository
// other than its pool's deploy key's, or nil.
func (a *Adapter) PoolRepoRefusal(ag agents.Agent) error {
	return agentspec.PoolRepoRefusal(ag, a.spec)
}

// jobFetchWithKey has ag's KEY HOLDER — the container of its pod that alone
// holds the pool's deploy key k, under its own uid — probe the key for write
// access and fetch pull request pr's head (and baseSHA) of repo into its
// mirror (podagent.MirrorScript), for the worktree script to fetch from with no
// credential.
//
// 🔴 THE KEY IS ONE REPOSITORY'S. A job naming any other repository is refused
// here, before any exec (DeployKeyRepoMismatchMessage) — never fetched
// anonymously instead, which would hide a misconfigured pool behind a
// public-repository fallback. (agentspec refuses a worker on another repository
// too; this holds when the configuration moved after the worker was built.)
func (a *Adapter) jobFetchWithKey(ctx context.Context, ag agents.Agent, k agentspec.PoolDeployKey, repo string, pr int, baseSHA string) error {
	if !agentspec.SameRepo(repo, k.Repo) {
		return fmt.Errorf("%s (job repository %q, pool %q's deploy key: %q)", DeployKeyRepoMismatchMessage, repo, ag.WorkerPool, k.Repo)
	}
	ssh, err := podagent.DeployKeySSHCommand(agentspec.DeployKeyPath, agentspec.KnownHostsPath)
	if err != nil {
		return err
	}
	var out bytes.Buffer
	cmd := []string{"sh", "-c", podagent.MirrorScript, "muster-deploy-key-fetch",
		podagent.DeployKeyURL(repo), agentspec.MirrorPath, ssh, strconv.Itoa(pr), baseSHA}
	if err := provision.ExecIn(ctx, a.driver, agents.RefOf(ag), agentspec.DeployKeyHolderName, cmd, nil, &out, &out); err != nil {
		return fmt.Errorf("fetch pull request %d with the deploy key (container %s): %w: %s", pr, agentspec.DeployKeyHolderName, err,
			strings.TrimSpace(capString(out.String(), 1024)))
	}
	return nil
}

// RemoveJobWorktree removes job jobID's worktree of ag's repository
// (jobRemoveScript). A worktree that is not there is not an error.
func (a *Adapter) RemoveJobWorktree(ctx context.Context, ag agents.Agent, jobID int64) error {
	main, wt, err := jobDirs(ag, jobID)
	if err != nil {
		return err
	}
	var out bytes.Buffer
	cmd := []string{"sh", "-c", jobRemoveScript, "muster-job-worktree-remove", main, wt}
	if err := provision.Exec(ctx, a.driver, agents.RefOf(ag), cmd, nil, &out, &out); err != nil {
		return fmt.Errorf("remove the worktree %s: %w: %s", wt, err, strings.TrimSpace(capString(out.String(), 1024)))
	}
	return nil
}

// ErrFileTooLarge is ReadWorkspaceFile's answer for a file over its limit.
var ErrFileTooLarge = errors.New("the file is larger than the limit")

// ReadWorkspaceFile returns the contents of rel byte for byte, refusing a file
// over max bytes with ErrFileTooLarge rather than truncating it. rel is
// relative to job jobID's worktree for a positive jobID (PrepareJobWorktree),
// and to the directory ag's CLI works in (agentspec.ClaudeCodeWorkingDir) for
// zero.
func (a *Adapter) ReadWorkspaceFile(ctx context.Context, ag agents.Agent, jobID int64, rel string, max int) ([]byte, error) {
	if !jobs.ValidResultPath(rel) {
		return nil, fmt.Errorf("agentprovision: %q is not a relative path inside the workspace", rel)
	}
	if max <= 0 {
		return nil, fmt.Errorf("agentprovision: a read limit of %d bytes", max)
	}
	dir, err := agentspec.ClaudeCodeWorkingDir(ag)
	if err != nil {
		return nil, err
	}
	if jobID != 0 {
		if _, dir, err = jobDirs(ag, jobID); err != nil {
			return nil, err
		}
	}
	full := path.Join(dir, rel)
	var out, errb bytes.Buffer
	// One byte past the limit: present means the file is over it.
	cmd := []string{"sh", "-c", readScript, "muster-job-read", full, strconv.Itoa(max + 1)}
	if err := provision.Exec(ctx, a.driver, agents.RefOf(ag), cmd, nil, &out, &errb); err != nil {
		return nil, fmt.Errorf("read %s: %w: %s", full, err, strings.TrimSpace(capString(errb.String(), 1024)))
	}
	if out.Len() > max {
		return nil, fmt.Errorf("read %s: %w (%d bytes)", full, ErrFileTooLarge, max)
	}
	return out.Bytes(), nil
}

// removeScript deletes the file $1 — the same file as $3, relative to the
// checkout in $2 — only when it is certain the repository does not track it.
// A missing file is not an error; rm removes a symlink itself, never its target.
//
// 🔴 FAIL CLOSED: ONLY `ls-files --error-unmatch` EXITING EXACTLY 1 ("did not
// match") MEANS UNTRACKED. Exit 0 means tracked: it prints RemoveTrackedMarker
// and deletes nothing. ANY other status — git missing (127), a refusal such as
// "dubious ownership" or another fatal error (128) — leaves the file in place
// and fails with a message, which the runner records as a note: treating "git
// could not answer" as "untracked" would delete a tracked file. The one case
// that skips git is a working directory with no .git at all (an agent with no
// repository), where nothing can be tracked.
//
// 🔴 --literal-pathspecs: a result_path is a file name, and without it a name
// such as ":(top)…" or "*.md" would be read as pathspec magic or a glob and
// could match a DIFFERENT, tracked file — or none.
var removeScript = gitRunsNoRepoCommands + `if [ -e "$2/.git" ]; then
	git --literal-pathspecs -C "$2" ls-files --error-unmatch -- "$3" >/dev/null 2>&1
	rc=$?
	if [ "$rc" -eq 0 ]; then
		echo ` + RemoveTrackedMarker + `
		exit 0
	fi
	if [ "$rc" -ne 1 ]; then
		echo "could not tell whether $3 is tracked (git ls-files exited $rc), so it was left in place" >&2
		exit 3
	fi
fi
rm -f -- "$1"`

// RemoveTrackedMarker is what removeScript prints when it leaves a tracked file.
const RemoveTrackedMarker = "muster-job-remove:tracked"

// RemoveUntrackedWorkspaceFile deletes rel — a path relative to the directory
// ag's CLI works in, validated as ReadWorkspaceFile validates it — unless the
// repository checked out there tracks it. removed reports whether it deleted
// (or found nothing to delete); false with a nil error means the file is
// tracked and was left in place. It is for a job WITHOUT a commit, whose turn
// ran in the agent's own working directory; a commit job's whole worktree is
// removed instead (RemoveJobWorktree).
func (a *Adapter) RemoveUntrackedWorkspaceFile(ctx context.Context, ag agents.Agent, rel string) (removed bool, err error) {
	if !jobs.ValidResultPath(rel) {
		return false, fmt.Errorf("agentprovision: %q is not a relative path inside the workspace", rel)
	}
	dir, err := agentspec.ClaudeCodeWorkingDir(ag)
	if err != nil {
		return false, err
	}
	full := path.Join(dir, rel)
	var out bytes.Buffer
	cmd := []string{"sh", "-c", removeScript, "muster-job-remove", full, dir, rel}
	if err := provision.Exec(ctx, a.driver, agents.RefOf(ag), cmd, nil, &out, &out); err != nil {
		return false, fmt.Errorf("remove %s: %w: %s", full, err, strings.TrimSpace(capString(out.String(), 1024)))
	}
	return strings.TrimSpace(out.String()) != RemoveTrackedMarker, nil
}

func capString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
