#!/bin/sh
# muster work-autosave — make a dispatched agent's work survive the agent dying.
#
# WHY THIS EXISTS (incident, 2026-08-12): agent `bold-moth`, dispatched at task
# #172 on civitai/civitai, produced 14 files of correct work and then went silent.
# Four hours later it was on branch `main` with ZERO commits and everything
# uncommitted, in /data/repos/<name> on an emptyDir. A second pod restart, an
# eviction, or the idle reaper would have destroyed all of it; it survived only
# because a human went looking and pulled a `git diff` out by hand.
#
# The close-out protocol in the system prompt (commit -> push -> PR) is a FINAL
# act, so work is durable only at the very end of a run and anything that kills
# the agent before that step loses everything silently. Prose cannot fix that:
# the agent was already instructed to push, it just never got there. This daemon
# removes the agent from the durability path entirely — it snapshots on a timer
# whether or not the agent cooperates, is still alive, or ever reaches its
# close-out.
#
# HOW IT STAYS OUT OF THE AGENT'S WAY: the snapshot is built in a SEPARATE git
# index (GIT_INDEX_FILE) and committed with `commit-tree`, so it never touches
# the agent's HEAD, current branch, index, working tree or stash. The agent's
# own `git checkout -b` / `commit` / `push` close-out keeps working untouched,
# and there is no lock contention on .git/index. Snapshots land on ONE remote
# ref per agent (force-updated in place), so the repo gets a single rescue
# branch, not a stream of noise commits.
#
# 🔴 AND IT NEVER RUNS THE TARGET REPOSITORY'S HOOKS — see $nohooks below. A
# machine-generated snapshot has no business executing a third party's
# verification, and doing so cost this daemon its entire purpose once already
# (incident, 2026-08-12, second occurrence).
#
# Config (environment):
#   MUSTER_WIP_REPO       absolute path of the git worktree to snapshot (required)
#   MUSTER_WIP_REF        full remote ref to force-update, e.g.
#                           refs/heads/muster-wip/bold-moth-42 (required)
#   MUSTER_WIP_REMOTE     remote name or URL to push to (default: origin)
#   MUSTER_WIP_INTERVAL   seconds between snapshots (default: 120)
#   MUSTER_WIP_LOG        log destination; "-" keeps stdout (default: a file)
#   MUSTER_WIP_MAX_CYCLES stop after N cycles; 0 = run forever (test seam)
#   MUSTER_WIP_EXCLUDE    colon-separated gitignore patterns for the agent
#                           runtime's own workspace files, e.g.
#                           "/SOUL.md:/IDENTITY.md:/.someruntime/". Empty is valid
#                           and silent. See install_excludes: upstream this was a
#                           hardcoded list of ONE runtime's filenames, and baking
#                           it in is wrong in both directions for any other.
#
# POSIX sh only: an agent image's /bin/sh may be dash rather than bash, so no
# bashisms — and the daemon is started from a single init command line, which is
# why the exclude list is colon-separated rather than newline-separated.

# The chart's init script runs under `set -e` (see the chart's own note at
# templates/deployment.yaml:118-131). This is a separately-invoked script so it
# does not inherit that, but be explicit: a non-zero from any probe below must
# never kill the daemon.
set +e

repo="${MUSTER_WIP_REPO:-}"
ref="${MUSTER_WIP_REF:-}"
remote="${MUSTER_WIP_REMOTE:-origin}"
interval="${MUSTER_WIP_INTERVAL:-120}"
max_cycles="${MUSTER_WIP_MAX_CYCLES:-0}"
log_dest="${MUSTER_WIP_LOG:-/tmp/muster-autosave.log}"
# Colon-separated gitignore patterns for the agent runtime's own workspace files.
# Empty is valid and silent — see install_excludes for why the list is a parameter.
exclude="${MUSTER_WIP_EXCLUDE:-}"

# Diagnostics go to a FILE. Per the chart's own comment (deployment.yaml:118-131),
# processes backgrounded from the container init script inherit an orphaned pipe
# for stdout/stderr that is NOT the container log stream, so `echo` from here is
# invisible to `kubectl logs`. The chart's git-sync and workspace-sync loops both
# mitigate this the same way. Tail it with:
#   kubectl exec <pod> -- tail -f /tmp/muster-autosave.log
if [ "$log_dest" != "-" ]; then
	touch "$log_dest" 2>/dev/null || log_dest=/dev/null
	exec >>"$log_dest" 2>&1
fi

log() { echo "[$(date -Iseconds)] muster-autosave: $*"; }
err() { echo "[$(date -Iseconds)] muster-autosave: $*" >&2; }

if [ -z "$repo" ] || [ -z "$ref" ]; then
	err "MUSTER_WIP_REPO and MUSTER_WIP_REF are required; not starting"
	exit 2
fi

# Snapshots are machine-authored. Set the identity explicitly rather than relying
# on a pod gitconfig that may not exist — commit-tree fails outright without one.
GIT_AUTHOR_NAME="muster-autosave"
GIT_AUTHOR_EMAIL="autosave@muster.invalid"
GIT_COMMITTER_NAME="$GIT_AUTHOR_NAME"
GIT_COMMITTER_EMAIL="$GIT_AUTHOR_EMAIL"
export GIT_AUTHOR_NAME GIT_AUTHOR_EMAIL GIT_COMMITTER_NAME GIT_COMMITTER_EMAIL

# 🔴 $nohooks NEUTRALISES THE TARGET REPOSITORY'S HOOKS FOR EVERY GIT COMMAND
# THIS DAEMON RUNS. Applied to ALL of them, not just the ones measured to fire a
# hook today, so a call site added later cannot silently reintroduce the class.
#
# WHY (incident, 2026-08-12, the SECOND one): agent `bold-otter` (#49) was
# dispatched at civitai/civitai, which ships `.husky/pre-push` and sets
# `core.hooksPath = .husky/_`. The snapshot push ran that hook — a FULL
# TYPECHECK — every cycle. It failed every cycle, so `git ls-remote origin
# 'refs/heads/muster-wip/*'` returned zero refs: durability was completely off
# on the exact repository whose incident motivated this daemon, while the pod
# burned a repeated typecheck on the agent's own CPU and memory budget. The
# operator-facing log line showed "Running typecheck for all files" where a git
# error belongs.
#
# MEASURED, not assumed (git 2.55, both hook shapes — a classic
# `.git/hooks/pre-push` and a `core.hooksPath`-redirected `.husky/_/pre-push`;
# they are different code paths and civitai uses the latter):
#   - read-tree / add / write-tree / commit-tree are plumbing and fire NOTHING.
#   - `git push` fires `pre-push`.
#   - `git update-ref` fires `reference-transaction` — 3x on git 2.55
#     (preparing, prepared, committed) and 2x on git 2.39.5, which is what the
#     Debian-based agent image ships. The COUNT is version-dependent; that it
#     fires at all is not.
#   - `git push` to a NAMED remote ALSO fires `reference-transaction` locally,
#     because it updates refs/remotes/<remote>/… afterwards.
# So `--no-verify` alone is NOT sufficient: it covers `pre-push` and nothing
# else, and two of the paths above are `reference-transaction`. Overriding
# core.hooksPath is the only measure that covers all of them, which is why this
# is defence in depth rather than belt-and-braces for its own sake.
#
# /dev/null is deliberate: git resolves "<hooksPath>/<name>", so the lookup can
# never succeed, and unlike a real empty directory under /tmp nobody can plant
# an executable in it. Passed as `-c` on each command line rather than via
# GIT_CONFIG_KEY_n/GIT_CONFIG_COUNT because the agent container already uses
# those env vars to install its git credential helper — setting GIT_CONFIG_COUNT
# here would CLOBBER that and break the push authentication outright. Verified
# that `-c` and the env-based config coexist: both keys survive.
#
# Intentionally UNQUOTED at every call site so it word-splits into "-c" and
# "core.hooksPath=/dev/null"; the value contains no whitespace or glob
# characters, and this script only ever runs under sh/dash where that splitting
# is well-defined (same idiom, and same reasoning, as $timeout_prefix below).
nohooks="-c core.hooksPath=/dev/null"

index="/tmp/.muster-autosave.index"
# localRef keeps the newest snapshot commit reachable inside the pod even when
# the push fails, so it is not a dangling object one `git gc` away from deletion.
local_ref="refs/muster-wip/last"
# A push into a TCP blackhole would otherwise stall this single-process loop
# forever with no signal at all — no snapshots, no log line, nothing.
push_timeout="${MUSTER_WIP_PUSH_TIMEOUT:-60}"

# pushed_tree is the tree OID of the last snapshot CONFIRMED pushed (or, at
# startup, the pristine clone's tree — see seed_pushed_tree). It advances ONLY
# after a successful push, so a transient push failure is retried on the next
# cycle instead of being silently skipped as "unchanged". Assigned from inside
# functions that are deliberately called directly (never in a subshell or
# pipeline) so the assignments survive the cycle.
pushed_tree=""
index_head=""
prepared=""
fail_streak=0
notified=""
push_out=""
push_rc=0

# An agent runtime writes its own bookkeeping files into the agent's workspace,
# and for a repo-backed agent the workspace IS the clone — so those files sit
# untracked in someone else's repository and would be force-pushed to a branch
# there. Exclude them locally.
#
# 🔴 THE LIST IS A PARAMETER, NOT A CONSTANT, AND THAT IS THE ONE STRUCTURAL
# CHANGE THIS PORT MAKES. Upstream it was seven hardcoded entries naming ONE agent
# runtime's bookkeeping files. Baked in here they would be wrong for every other
# runtime in two directions at once: silently pushing the new runtime's droppings
# into a stranger's repository, and silently excluding paths that runtime does not
# create (so a repo legitimately holding a root TOOLS.md it does not track would
# have it dropped from its own rescue snapshot). Neither failure is visible from
# inside the pod. The caller names the runtime, so the caller names the list.
#
# ⚠ AN EMPTY LIST IS A VALID ANSWER AND MUST STAY SILENT, not warn: a runtime
# that writes nothing into the workspace genuinely has nothing to exclude, and a
# warning on the common case is how a log stops being read.
#
# .git/info/exclude is the right instrument: it is never committed (so the
# agent's own close-out PR is unaffected) and it only applies to UNTRACKED
# paths, so a repository that genuinely tracks a file of one of these names is
# unaffected — read-tree puts tracked files in the index before `add` runs.
# Entries should be anchored with a leading "/" so only the repo ROOT is
# excluded, never a legitimate docs/TOOLS.md deeper in the tree; this function
# does NOT add the anchor for you, because silently rewriting a caller's pattern
# would make an intentionally-unanchored one impossible to express.
install_excludes() {
	[ -n "$exclude" ] || return 0
	# shellcheck disable=SC2086
	gitdir=$(git $nohooks rev-parse --git-dir 2>/dev/null) || return 1
	[ -n "$gitdir" ] || return 1
	mkdir -p "$gitdir/info" 2>/dev/null
	excl="$gitdir/info/exclude"
	grep -q 'muster-autosave' "$excl" 2>/dev/null && return 0
	{
		echo "# added by muster-autosave: keep agent-runtime files out of snapshots"
		# Colon-separated so one env var can carry the list: a newline-separated
		# value cannot survive being passed through the single init command line
		# this daemon is started from.
		printf '%s\n' "$exclude" | tr ':' '\n' | while IFS= read -r pattern; do
			[ -n "$pattern" ] && echo "$pattern"
		done
	} >>"$excl" 2>/dev/null
}

# seed_pushed_tree suppresses the pointless FIRST snapshot of a pristine clone.
# Without it every agent force-pushes an empty snapshot at pod start before doing
# any work.
#
# ⚠ CONDITIONAL ON HEAD ALREADY BEING ON THE REMOTE. If the clone carries LOCAL
# COMMITS (a container restart after the agent committed, say), HEAD^{tree} can
# equal the worktree tree while those commits exist nowhere else — seeding then
# would mark the tree "already pushed" and skip forever, losing exactly the
# committed-but-unpushed work this daemon exists to rescue. So seed only when
# HEAD is not ahead of its upstream; otherwise leave pushed_tree empty and let
# cycle 1 push.
seed_pushed_tree() {
	# shellcheck disable=SC2086
	ahead=$(git $nohooks rev-list --count '@{upstream}..HEAD' 2>/dev/null)
	if [ "$ahead" = "0" ]; then
		# shellcheck disable=SC2086
		pushed_tree=$(git $nohooks rev-parse --verify 'HEAD^{tree}' 2>/dev/null)
		log "clone is pristine at $pushed_tree; first snapshot suppressed until work appears"
	else
		log "clone has local commits not on the remote; snapshotting immediately"
	fi
}

# refresh_index seeds the private index from HEAD when HEAD moves.
#
# ⚠ read-tree IS REQUIRED FOR CORRECTNESS, not just speed. Building the index
# empty makes `git add -A` treat a TRACKED file that also matches .gitignore
# (force-added with `add -f`, which real repos do for e.g. a committed
# config.env) as merely ignored: it is left out of the snapshot AND recorded as
# DELETED. The agent's edits to it are lost, and recovering the snapshot by
# merge/cherry-pick would delete the file. Measured.
#
# The index is REUSED between cycles because git's stat cache then makes each
# pass a stat-only walk instead of re-hashing the entire worktree every 120s on
# an emptyDir. read-tree drops that stat cache, so it must run only when HEAD
# actually moved — doing it every cycle would forfeit the whole benefit.
refresh_index() {
	# shellcheck disable=SC2086
	head_now=$(git $nohooks rev-parse --verify HEAD 2>/dev/null)
	if [ -f "$index" ] && [ "$head_now" = "$index_head" ]; then
		return 0
	fi
	rm -f "$index"
	if [ -n "$head_now" ]; then
		# shellcheck disable=SC2086
		GIT_INDEX_FILE="$index" git $nohooks read-tree "$head_now" 2>/dev/null || rm -f "$index"
	fi
	index_head="$head_now"
}

# ONE push invocation, with the timeout applied as an optional command PREFIX.
#
# ⚠ Deliberately not an if/else over two `git push` lines. That shape left the
# no-timeout branch unreachable on any host where coreutils `timeout` exists —
# which is every host the suite runs on and the agent image itself — so it was
# dead code that no test could exercise and no mutant could be caught in.
# $timeout_prefix is intentionally UNQUOTED so it word-splits into "timeout N"
# or disappears entirely; this script only ever runs under sh/dash, where that
# splitting is well-defined.
#
# --no-verify is the escape the hook files themselves document, and it declares
# the intent in the one place a reader looks. It is NOT the load-bearing measure
# — $nohooks is, because --no-verify covers `pre-push` and nothing else while
# this very command also fires `reference-transaction`. Both are here on
# purpose; see the $nohooks block for the measurements.
push_snapshot() {
	# shellcheck disable=SC2086
	$timeout_prefix git $nohooks push --no-verify --force "$remote" "$commit:$ref" 2>&1
}

# push_failure_reason names WHICH failure happened, because the old line pasted
# $push_out after a bare colon and could not tell three very different things
# apart: an auth/network error from git, a push killed by $timeout (which exits
# 124 and produces NO output at all, so the line ended in a dangling colon), and
# — before $nohooks — a target-repo hook's own stdout, which is how an operator
# came to read "Running typecheck for all files" where a git error belongs.
#
# Reads the globals set by the caller. 124 is coreutils `timeout`'s
# killed-the-child status; it is only trustworthy as such when the prefix is
# actually in play, so an absent `timeout` falls through to the git branch
# rather than mislabelling a genuine git exit 124.
push_failure_reason() {
	if [ -n "$timeout_prefix" ] && [ "$push_rc" -eq 124 ]; then
		printf 'timed out after %ss' "$push_timeout"
	elif [ -z "$push_out" ]; then
		printf 'git failed with exit %s and produced no output' "$push_rc"
	else
		printf 'git failed with exit %s: %s' "$push_rc" \
			"$(printf '%s' "$push_out" | tr -s '\n' ' ')"
	fi
}

# notify_durability_lost tells the HUMAN, once per failure episode, that the
# work is no longer being saved off-pod. Everything else this daemon prints
# lands in a file inside an ephemeral pod — which dies with exactly the work it
# was reporting on — so a log line is not a usable channel for the one message
# that matters. This posts to the agent self-service API using the per-agent
# MUSTER_HOOK_TOKEN already present in the pod, so it appears on the agent's task
# thread where a human actually looks. Strictly best-effort.
#
# ⚠ THERE IS ONE BASE URL HERE, AND THE UPSTREAM ARGUMENT FOR TWO IS GONE RATHER
# THAN RENAMED. Upstream this line defaulted a TASK-side base URL to a ROUTER-side
# one and carried a long argument about which of the two to post to, because
# those were separate deployments whose URLs diverged at a cutover: built on the
# router URL, this POST would 404 exactly when the durability alarm needed to
# fire. muster serves both route families from its own mux — see
# cmd/muster/config.go, which makes the same argument and names the test that
# checks it — so there is no second URL and no fallback to get wrong.
#
# ⚠ THE EMPTY CHECK IS STILL LOAD-BEARING. This script runs without `set -u`, so
# an unset variable expands to EMPTY rather than erroring, and the POST would go
# to a bare "/agent/task/comment" — a relative URL curl rejects, silently. The
# guard below is what turns that into a logged refusal.
notify_durability_lost() {
	[ -n "$notified" ] && return 0
	task_api_url="${MUSTER_API_URL:-}"
	[ -n "$task_api_url" ] && [ -n "$MUSTER_HOOK_TOKEN" ] || return 0
	command -v curl >/dev/null 2>&1 || return 0
	notified=1
	# The only interpolated value is the remote; strip the two characters that
	# could break out of a JSON string rather than hand-rolling an escaper.
	safe_remote=$(printf '%s' "$remote" | tr -d '"\\')
	printf '{"body":"muster work-autosave cannot push to %s. This agent work is NOT being saved off-pod and will be lost if the pod restarts or is reaped. Snapshots are still committed locally at %s."}' \
		"$safe_remote" "$local_ref" >/tmp/.muster-autosave-notify.json 2>/dev/null || return 0
	curl -sf -m 15 -X POST \
		-H "Authorization: Bearer $MUSTER_HOOK_TOKEN" \
		-H "Content-Type: application/json" \
		--data @/tmp/.muster-autosave-notify.json \
		"$task_api_url/agent/task/comment" >/dev/null 2>&1 \
		&& log "notified the task thread that durability is off" \
		|| err "could not notify the task thread that durability is off"
}

snapshot() {
	cd "$repo" 2>/dev/null || {
		err "workspace $repo is not accessible (yet); will retry"
		return 1
	}
	# shellcheck disable=SC2086
	git $nohooks rev-parse --git-dir >/dev/null 2>&1 || {
		err "$repo is not a git repository (yet); will retry"
		return 1
	}
	# One-time setup, deferred until the clone actually exists.
	if [ -z "$prepared" ]; then
		install_excludes
		seed_pushed_tree
		prepared=1
	fi

	refresh_index
	# shellcheck disable=SC2086
	if ! GIT_INDEX_FILE="$index" git $nohooks add -A 2>/dev/null; then
		rm -f "$index"
		index_head=""
		err "could not stage the workspace; will retry"
		return 1
	fi
	# shellcheck disable=SC2086
	tree=$(GIT_INDEX_FILE="$index" git $nohooks write-tree 2>/dev/null)
	if [ -z "$tree" ]; then
		err "could not write a tree; will retry"
		return 1
	fi

	# Unchanged since the last CONFIRMED-pushed snapshot: no commit, no push, no
	# noise. This is what keeps an idle agent from producing a commit every cycle.
	if [ "$tree" = "$pushed_tree" ]; then
		return 0
	fi

	# shellcheck disable=SC2086
	head=$(git $nohooks rev-parse --verify HEAD 2>/dev/null)
	msg="muster autosave snapshot $(date -Iseconds)

Automatic snapshot of the agent workspace, pushed so the work survives the
agent's pod. Not authored by the agent and not intended to be merged as-is."
	if [ -n "$head" ]; then
		# shellcheck disable=SC2086
		commit=$(git $nohooks commit-tree "$tree" -p "$head" -m "$msg" 2>/dev/null)
	else
		# shellcheck disable=SC2086
		commit=$(git $nohooks commit-tree "$tree" -m "$msg" 2>/dev/null)
	fi
	if [ -z "$commit" ]; then
		err "could not create a snapshot commit; will retry"
		return 1
	fi
	# Anchor it locally FIRST: if the push fails, the snapshot is still a
	# reachable ref instead of a dangling object that `git gc` may collect.
	# shellcheck disable=SC2086
	git $nohooks update-ref "$local_ref" "$commit" 2>/dev/null

	# Split from the `if` so the exit STATUS survives: a timeout kill (124) and a
	# git error are indistinguishable from the output alone, and a timeout's
	# output is empty. POSIX: the status of an assignment whose value comes from
	# a command substitution IS that command's status.
	push_out=$(push_snapshot)
	push_rc=$?
	if [ "$push_rc" -eq 0 ]; then
		pushed_tree="$tree"
		fail_streak=0
		notified=""
		log "pushed snapshot $commit -> $ref"
		return 0
	fi
	# The commit object exists locally even though the push failed, so the work
	# still survives a container restart (the emptyDir outlives the container, not
	# the pod). Keep looping: the next cycle retries the SAME tree because
	# pushed_tree was not advanced.
	fail_streak=$((fail_streak + 1))
	err "push to '$remote' $ref failed ($(push_failure_reason)); work is committed locally as $commit ($local_ref) but is NOT durable off-pod"
	if [ "$fail_streak" -ge 3 ]; then
		notify_durability_lost
	fi
	return 1
}

timeout_prefix=""
if command -v timeout >/dev/null 2>&1; then
	timeout_prefix="timeout $push_timeout"
else
	err "coreutils 'timeout' not found; a hung push cannot be bounded"
fi
log "started (repo=$repo ref=$ref remote=$remote interval=${interval}s log=$log_dest)"
cycles=0
while :; do
	snapshot
	cycles=$((cycles + 1))
	if [ "$max_cycles" -gt 0 ] && [ "$cycles" -ge "$max_cycles" ]; then
		break
	fi
	sleep "$interval"
done
