package podagent

import (
	"fmt"
	"regexp"
	"strings"
)

// A PRIVATE repository's read-only DEPLOY KEY, for a worker pool configured with
// one (MUSTER_AGENT_CC_POOL_DEPLOY_KEYS). Every use of the key is made by the
// pod's KEY HOLDER — a container of its own, under a uid that is not the
// session's, the only container the key is mounted into (internal/agentspec,
// provision.KeyHolder) — running MirrorScript: at the pod's start (its
// command, MirrorHolderCommand) it clones the repository into a bare MIRROR in
// a directory it shares READ-ONLY with the agent's container; for each machine
// job muster execs the same script into it to probe the key and fetch the
// job's commits into that mirror. ccd clones the agent's checkout FROM the
// mirror (CheckoutConfig.Mirror), and a job's worktree script fetches from it
// (internal/agentprovision jobworkspace.go): neither holds, nor can name, the
// key. Every other pod keeps the anonymous HTTPS clone.
//
// 🔴 ssh.github.com ON PORT 443, NOT github.com ON 22. A claude-code pod may reach
// public addresses on TCP 443 only (agentspec.ClaudeCodeNetwork), and GitHub
// serves the same SSH endpoint on ssh.github.com:443 — same host keys, same
// deploy-key authorisation — so the pod's network policy is not widened for it.
//
// 🔴 THE HOST KEYS ARE PINNED, NOT LEARNT. GitHubKnownHosts is GitHub's published
// set (https://api.github.com/meta, "ssh_keys"), and the ssh command refuses any
// other (StrictHostKeyChecking=yes, no global or user file). A changed GitHub key
// fails every clone and fetch, loudly, until this constant is updated.

const (
	// DeployKeyHost and DeployKeyPort are the SSH endpoint the key is used with.
	DeployKeyHost = "ssh.github.com"
	DeployKeyPort = 443
)

// GitHubKnownHosts is the known_hosts content for DeployKeyHost:DeployKeyPort.
// MEASURED at the commit that introduced it: identical, key for key, to
// `ssh-keyscan -p 443 ssh.github.com` and to api.github.com/meta's ssh_keys.
const GitHubKnownHosts = `[ssh.github.com]:443 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl
[ssh.github.com]:443 ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBEmKSENjQEezOmxkZMy7opKgwFB9nkt5YRrYMjNuG5N87uRgg6CLrbo5wAdT/y6v0mKV0U2w0WZ2YB/++Tpockg=
[ssh.github.com]:443 ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQCj7ndNxQowgcQnjshcLrqPEiiphnt+VTTvDP6mHBL9j1aNUkY4Ue1gvwnGLVlOhGeYrnZaMgRK6+PKCUXaDbC7qtbW8gIkhL7aGCsOr/C56SJMy/BCZfxd1nWzAOxSDPgVsmerOBYfNqltV9/hWCqBywINIR+5dIg6JTJ72pcEpEjcYgXkE2YEFXV1JHnsKgbLWNlhScqb2UmyRkQyytRLtL+38TGxkxCflmO+5Z8CSSNY7GidjMIZ7Q4zMjA2n1nGrlTDkzwDCsw+wqFPGQA179cnfGWOWRVruj16z6XyvxvjJwbz0wQZ75XK5tKSb7FNyeIEs4TT4jk+S4dhPeAUC5y+bDYirYgM4GC7uEnztnZyaVWQ7B381AK4Qdrwt51ZqExKbQpTUNn+EjqoTwvqNj4kqx5QUCI0ThS/YkOxJCXmPUWZbhjpCg56i+2aB6CmK2JGhn57K5mj0MNdBXA4/WnwH6XoPWJzK5Nyu2zB3nAZp+S5hpQs+p1vN1/wsjk=
`

// DeployKeyURL is the SSH URL repository owner/name is cloned and fetched from
// with a deploy key.
func DeployKeyURL(repo string) string {
	return fmt.Sprintf("ssh://git@%s:%d/%s.git", DeployKeyHost, DeployKeyPort, repo)
}

// sshSafePath is a path that needs no quoting in GIT_SSH_COMMAND, which git
// hands to a shell.
var sshSafePath = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)

// DeployKeySSHCommand is GIT_SSH_COMMAND for a fetch with the key at keyPath,
// checking the server against the known_hosts file at knownHostsPath.
//
// Every option a session could otherwise steer is fixed here: -F /dev/null (no
// ~/.ssh/config, which the session can write — a ProxyCommand there runs a
// command), no agent and only this identity (IdentityAgent=none,
// IdentitiesOnly=yes), the pinned hosts only (UserKnownHostsFile,
// GlobalKnownHostsFile=/dev/null, StrictHostKeyChecking=yes), no prompt
// (BatchMode=yes), and no proxy, multiplexing or local command.
//
// ⚠ GIT_SSH_COMMAND outranks core.sshCommand, which the checkout's own config
// (the session's) could set; it is set only on the git that fetches, never in an
// environment the session inherits.
func DeployKeySSHCommand(keyPath, knownHostsPath string) (string, error) {
	for _, p := range []string{keyPath, knownHostsPath} {
		if !sshSafePath.MatchString(p) || strings.Contains(p, "..") {
			return "", fmt.Errorf("podagent: deploy-key path %q is not an absolute path of [A-Za-z0-9._/-]", p)
		}
	}
	return "ssh -F /dev/null -o IdentityAgent=none -o IdentitiesOnly=yes -o IdentityFile=" + keyPath +
		" -o UserKnownHostsFile=" + knownHostsPath + " -o GlobalKnownHostsFile=/dev/null" +
		" -o StrictHostKeyChecking=yes -o BatchMode=yes -o ProxyCommand=none -o ProxyJump=none" +
		" -o ControlMaster=no -o ControlPath=none -o PermitLocalCommand=no", nil
}

// The messages MirrorScript fails with (each opens the error a job records).
const (
	// MirrorCloneMessage: the holder could not clone the repository with the key.
	MirrorCloneMessage = "the worker pool's deploy-key holder could not clone the repository with the key (a key GitHub refuses, a repository it does not open, or no network); nothing was run"
	// MirrorNoDefaultBranchMessage: the mirror has no default branch to probe with.
	MirrorNoDefaultBranchMessage = "the deploy-key holder's mirror of the repository has no default branch to probe the key's write access with; nothing was run"
	// DeployKeyWritableMessage: the key could open GitHub's receive-pack.
	DeployKeyWritableMessage = "the worker pool's deploy key has WRITE access (a dry-run push to the repository was accepted), so it was not used: add it to the repository as a deploy key WITHOUT write access; nothing was run"
	// PullFetchMessage: the pull request's head could not be fetched.
	PullFetchMessage = "the pull request's head could not be fetched — anonymously, or with the worker pool's deploy key where it has one — (a private repository without a deploy key, a deploy key GitHub refuses, no such pull request, or no network); nothing was run"
	// BaseFetchMessage: the job's base_sha could not be fetched; the base follows.
	BaseFetchMessage = "the job's base commit could not be fetched — anonymously, or with the worker pool's deploy key where it has one — (no such commit in the repository, a key GitHub refuses, or no network); nothing was run"
)

// ReadOnlyProbeRef is the ref the write probe dry-run-pushes to. Nothing is
// ever written there: a dry run sends no objects and updates no ref.
const ReadOnlyProbeRef = "refs/heads/muster-read-only-probe"

// MirrorScript runs in the KEY HOLDER, as its uid, with the key: $1 the
// repository's SSH URL (DeployKeyURL), $2 the mirror's path (a bare repository
// in the directory shared read-only with the agent's container), $3
// GIT_SSH_COMMAND (DeployKeySSHCommand), $4 a pull request number ("" = only
// make sure the mirror exists), $5 a base commit to fetch ("" = none).
//
//  1. Without a mirror, clone one (`--bare`) into a temporary directory beside
//     it and rename it into place, so a reader sees no mirror or a whole one;
//     `uploadpack.allowAnySHA1InWant`, so a job can fetch its base by id.
//  2. With $4: PROBE the key for write access — a `git push --dry-run` of the
//     mirror's default branch to ReadOnlyProbeRef. A dry run sends nothing; it
//     only opens GitHub's receive-pack, which GitHub refuses for a read-only
//     deploy key. Accepted: the key can push, exit 11 (DeployKeyWritableMessage).
//     🔴 THE REFSPEC IS FORCED ("+"): unforced, a branch of that name already on
//     GitHub at a commit the default branch does not descend from is refused as
//     a non-fast-forward BEFORE write access is asked, and a writable key read as
//     read-only. A probe that fails for any other reason (the network) is not a
//     refusal: the fetch then fails on its own.
//  3. Fetch refs/pull/$4/head into the mirror at the same name (exit 8), and
//     $5 by id (exit 12), for the job's worktree script to fetch from the
//     mirror with no credential.
//
// 🔴 IT READS NO CONFIGURATION THE SESSION CAN WRITE. Its git directory is the
// mirror, which only this uid writes (the agent's container mounts it
// read-only); GIT_CONFIG_NOSYSTEM and GIT_CONFIG_GLOBAL=/dev/null drop the
// image's and $HOME's; the ssh command fixes every option (-F /dev/null, …).
// So a push URL, receive-pack or upload-pack program planted in the AGENT's
// checkout — which once made the probe exit 128 and a writable key read as
// read-only — is not seen here at all.
//
// ⚠ The key reaches no argv and no environment: ssh is given its PATH
// (IdentityFile=), and reads it itself, in this container.
//
// Serialised by flock(1) on <mirror>.lock where the image has it (Debian's
// util-linux does): two jobs fetching one pull request's ref at once would
// otherwise fail one of them on git's ref lock.
const MirrorScript = `export LC_ALL=C GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_TERMINAL_PROMPT=0 GIT_SSH_COMMAND="$3"
unset GIT_DIR GIT_WORK_TREE GIT_CONFIG_PARAMETERS GIT_CONFIG_COUNT GIT_PROXY_COMMAND GIT_ASKPASS SSH_ASKPASS
url=$1 m=$2 pr=$4 base=$5
umask 022
mkdir -p "${m%/*}" || exit 1
if command -v flock >/dev/null 2>&1; then
	exec 9>"$m.lock" && flock 9 || exit 1
fi
if [ ! -f "$m/HEAD" ]; then
	t=$(mktemp -d "$m.XXXXXX") || exit 1
	if ! git clone --bare --quiet -- "$url" "$t/r"; then
		rm -rf "$t"
		echo "` + MirrorCloneMessage + `" >&2
		exit 13
	fi
	git -C "$t/r" config uploadpack.allowAnySHA1InWant true || { rm -rf "$t"; exit 1; }
	mv -T "$t/r" "$m" 2>/dev/null
	rm -rf "$t"
	[ -f "$m/HEAD" ] || exit 1
fi
[ -n "$pr" ] || exit 0
cd "$m" || exit 1
head=$(git rev-parse --verify --quiet 'HEAD^{commit}') ||
	{ echo "` + MirrorNoDefaultBranchMessage + `" >&2; exit 4; }
if git push --dry-run --quiet --no-verify "$url" "+$head:` + ReadOnlyProbeRef + `" >/dev/null 2>&1; then
	echo "` + DeployKeyWritableMessage + `" >&2
	exit 11
fi
git fetch --quiet --no-tags "$url" "+refs/pull/$pr/head:refs/pull/$pr/head" ||
	{ echo "` + PullFetchMessage + `" >&2; exit 8; }
if [ -n "$base" ]; then
	git fetch --quiet --no-tags --no-write-fetch-head "$url" "$base" && git cat-file -e "$base^{commit}" ||
		{ echo "` + BaseFetchMessage + ` (base_sha $base)" >&2; exit 12; }
fi`

// MirrorUploadPack is the --upload-pack command for every fetch, clone and
// ls-remote the agent's container makes from the key holder's mirror at
// mirror (ccd's clone, the job script's fetches).
//
// 🔴 WHY UPLOAD-PACK'S OWN COMMAND LINE. The mirror is owned by the holder's
// uid, and git refuses another user's repository ("detected dubious
// ownership") unless safe.directory names it in protected configuration. For a
// local path git CLEARS the config environment before it spawns upload-pack,
// so `-c safe.directory=…` or GIT_CONFIG_* on the calling git never reach it,
// and git 2.39's upload-pack checks ownership itself (measured on the image's
// git 2.39.5: clone and fetch die with rc 128). Global and system config are
// off for muster's gits (GIT_CONFIG_GLOBAL=/dev/null, GIT_CONFIG_NOSYSTEM=1),
// and the global file is the session's to write anyway. So the `-c` goes on
// upload-pack's own command line, which git runs through a shell with the
// path appended. git 2.55 needs none of it, so only the image's smoke
// (images/claude-code-agent/deploykey-smoke.sh, which pins this string) can
// show the failure.
//
// The path is interpolated into that shell command, so it is held to
// DeployKeySSHCommand's rule (absolute, [A-Za-z0-9._/-] only).
func MirrorUploadPack(mirror string) (string, error) {
	if !sshSafePath.MatchString(mirror) || strings.Contains(mirror, "..") {
		return "", fmt.Errorf("podagent: mirror path %q is not an absolute path of [A-Za-z0-9._/-]", mirror)
	}
	return "git -c safe.directory=" + mirror + " upload-pack", nil
}

// mirrorHolderBoot is the key holder's command: run MirrorScript ($1) until
// the mirror exists — retrying every 30 s, so a key not yet added on GitHub
// only delays the agent's clone — then idle, ending at once on SIGTERM.
const mirrorHolderBoot = `s=$1
shift
trap 'exit 0' TERM INT
until sh -c "$s" muster-mirror "$1" "$2" "$3" "" ""; do
	echo "muster: the repository mirror could not be made; retrying in 30s" >&2
	sleep 30 &
	wait $!
done
echo "muster: the repository mirror is ready at $2" >&2
while :; do
	sleep 3600 &
	wait $!
done`

// MirrorHolderCommand is the key holder's container command for repository
// owner/name, its mirror at mirrorPath, and the key and known_hosts at keyPath
// and knownHostsPath (DeployKeySSHCommand's refusals apply).
func MirrorHolderCommand(repo, mirrorPath, keyPath, knownHostsPath string) ([]string, error) {
	ssh, err := DeployKeySSHCommand(keyPath, knownHostsPath)
	if err != nil {
		return nil, err
	}
	return []string{"sh", "-c", mirrorHolderBoot, "muster-deploy-key-holder", MirrorScript, DeployKeyURL(repo), mirrorPath, ssh}, nil
}
