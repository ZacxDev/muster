#!/usr/bin/env bash
# The deploy-key holder's smoke test, on the REAL image:
#
#   docker run --rm --user 0 <image> cc-deploykey-smoke
#
# It runs as ROOT only to switch uids (setpriv); every check runs as the uid
# production uses. A worker pool with a deploy key runs this image twice in one
# pod: the agent's container as uid 1000 and the key holder as uid 1001
# (internal/agentspec DeployKeyHolderUID), which alone mounts the key, clones a
# bare mirror into /var/lib/muster-mirror/repo.git and fetches each job's
# commits into it; ccd and the job script then read that mirror as uid 1000.
#
#   (a) as uid 1001: ssh runs (`ssh -V`), and the holder's exact ssh command
#       reaches the connect step — it does not stop at "No user exists for uid";
#   (b) as uid 1001, a bare mirror with a commit at the production path; then as
#       uid 1000, with the system and global git config disabled (as every
#       muster git runs), the exact clone and fetch flags production uses
#       succeed against it — git refuses another user's repository ("detected
#       dubious ownership") unless told it is safe in a way upload-pack sees;
#   (c) as uid 1000, a 0440 file owned by root and a group uid 1000 is not in
#       cannot be read (the key's mode, read by any other uid).
#
# 🔴 THE FLAGS BELOW ARE PINNED, NOT COPIED BY HAND: the Go test
# TestTheDeployKeySmokePinsProductionsFlags (internal/agentspec) asserts each
# equals what the Go code emits, so this script tests the production strings.
set -euo pipefail

# PINNED: podagent.DeployKeySSHCommand(agentspec.DeployKeyPath, agentspec.KnownHostsPath)
SSH_CMD='ssh -F /dev/null -o IdentityAgent=none -o IdentitiesOnly=yes -o IdentityFile=/etc/muster/deploy-key/ssh-privatekey -o UserKnownHostsFile=/etc/muster/deploy-key/known_hosts -o GlobalKnownHostsFile=/dev/null -o StrictHostKeyChecking=yes -o BatchMode=yes -o ProxyCommand=none -o ProxyJump=none -o ControlMaster=no -o ControlPath=none -o PermitLocalCommand=no'
# PINNED: agentspec.MirrorPath
MIRROR='/var/lib/muster-mirror/repo.git'
# PINNED: the arguments a mirror checkout puts before git's subcommand.
GIT_PRE='-c safe.directory=/var/lib/muster-mirror/repo.git'
# PINNED: the --upload-pack command for a fetch from the mirror ('' = none).
UPLOAD_PACK=''
HOLDER_UID=1001
SESSION_UID=1000

fail() { echo "deploykey-smoke: FAIL: $*" >&2; exit 1; }
[ "$(id -u)" = 0 ] || fail "run as root (docker run --user 0): it switches uids with setpriv"
command -v setpriv >/dev/null || fail "setpriv is missing"

as() { # as <uid> <cmd...>: run with that uid/gid and no supplementary groups.
	local u=$1
	shift
	setpriv --reuid="$u" --regid="$u" --clear-groups env HOME=/tmp LC_ALL=C GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null "$@"
}

check_a() (
	# (a) ssh as the holder's uid.
	out=$(as "$HOLDER_UID" ssh -V 2>&1) || fail "(a) ssh -V as uid $HOLDER_UID: $out"
	echo "deploykey-smoke: (a) ssh -V as uid $HOLDER_UID: $out"
	# Port 1 on loopback: nothing listens, so a working ssh stops at the connect
	# step ("Connection refused"); one that cannot run stops before it.
	# shellcheck disable=SC2086
	out=$(as "$HOLDER_UID" $SSH_CMD -o ConnectTimeout=5 -p 1 git@127.0.0.1 git-upload-pack x 2>&1) && fail "(a) ssh to a closed port succeeded"
	echo "deploykey-smoke: (a) holder ssh as uid $HOLDER_UID: $out"
	case $out in
	*"No user exists"*) fail "(a) the holder's ssh cannot run as uid $HOLDER_UID: $out" ;;
	*"Connection refused"*) ;;
	*) fail "(a) the holder's ssh did not reach the connect step: $out" ;;
	esac

)
check_b() (
	# (b) the mirror, written by the holder's uid, read by the session's.
	install -d -o "$HOLDER_UID" -g "$HOLDER_UID" -m 0755 "$(dirname "$MIRROR")"
	work=$(mktemp -d)
	chown "$HOLDER_UID:$HOLDER_UID" "$work"
	# shellcheck disable=SC2016
	as "$HOLDER_UID" sh -euc '
		git init -q -b trunk "$1"
		printf "mirrored\n" > "$1/f"
		git -C "$1" add f
		git -C "$1" -c user.name=t -c user.email=t@t commit -q -m one
		git clone -q --bare "$1" "$2"
		git -C "$2" update-ref refs/pull/7/head trunk
	' sh "$work/src" "$MIRROR"
	[ "$(stat -c %u "$MIRROR")" = "$HOLDER_UID" ] || fail "(b) the mirror is not owned by uid $HOLDER_UID"
	dest=$(mktemp -d)
	chown "$SESSION_UID:$SESSION_UID" "$dest"
	up=()
	[ -z "$UPLOAD_PACK" ] || up=(--upload-pack="$UPLOAD_PACK")
	# shellcheck disable=SC2086
	out=$(as "$SESSION_UID" git $GIT_PRE clone --quiet --no-local "${up[@]}" -- "$MIRROR" "$dest/co" 2>&1) ||
		fail "(b) clone of the mirror as uid $SESSION_UID: $out"
	[ "$(cat "$dest/co/f")" = mirrored ] || fail "(b) the clone does not hold the mirror's file"
	echo "deploykey-smoke: (b) clone --no-local of a uid-$HOLDER_UID mirror as uid $SESSION_UID: ok"
	# shellcheck disable=SC2086
	out=$(as "$SESSION_UID" git $GIT_PRE -C "$dest/co" fetch --quiet --no-tags "${up[@]}" "$MIRROR" "+refs/pull/7/head:refs/muster-jobs/1" 2>&1) ||
		fail "(b) fetch from the mirror as uid $SESSION_UID: $out"
	as "$SESSION_UID" git -C "$dest/co" rev-parse --verify --quiet refs/muster-jobs/1 >/dev/null || fail "(b) the fetched ref is missing"
	echo "deploykey-smoke: (b) fetch <mirror> refs/pull/7/head as uid $SESSION_UID: ok"

)
check_c() (
	# (c) the key's mode, against another uid.
	key=$(mktemp)
	echo "smoke-marker" > "$key"
	chown "0:$HOLDER_UID" "$key"
	chmod 0440 "$key"
	if as "$SESSION_UID" cat "$key" >/dev/null 2>&1; then fail "(c) uid $SESSION_UID read a 0440 root:$HOLDER_UID file"; fi
	as "$HOLDER_UID" cat "$key" >/dev/null 2>&1 || fail "(c) control: uid $HOLDER_UID cannot read its own group's file"
	echo "deploykey-smoke: (c) uid $SESSION_UID cannot read a 0440 root:$HOLDER_UID file: ok"
)
# Each check runs on its own (a subshell: its fail exits only it), so one
# run reports every check that fails.
failed=0
check_a || failed=1
check_b || failed=1
check_c || failed=1
rm -rf "$(dirname "$MIRROR")"
[ "$failed" = 0 ] || { echo "deploykey-smoke: FAILED" >&2; exit 1; }
echo "deploykey-smoke: ok"
