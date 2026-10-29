package podagent

import (
	"fmt"
	"regexp"
	"strings"
)

// A PRIVATE repository's read-only DEPLOY KEY, for a worker pool configured with
// one (MUSTER_AGENT_CC_POOL_DEPLOY_KEYS; internal/agentspec mounts the key and
// hands ccd its path). The supervisor's clone of the agent's repository and every
// machine job's fetch (internal/agentprovision jobworkspace.go) go over SSH with
// this key and nothing else; every other pod keeps the anonymous HTTPS clone.
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
