package agentprovision

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// THE JOB CHECKOUT'S HOUSEKEEPING: what keeps an agent's volume from filling
// with git data that sha jobs leave behind. Two operations, each one exec in
// the agent's live instance, like the rest of this file's siblings.
//
//  1. SweepJobWorktrees removes every job worktree under .muster-jobs and every
//     refs/muster-jobs/* ref. A job's own removal (RemoveJobWorktree) runs in a
//     deferred call of the server process running the job, so a job whose
//     server stopped or crashed mid-turn (docs/jobs.md "No survival of a running
//     turn across a restart") leaves its worktree and its refs forever; 19 of
//     them (4.8 GB) were found on one review worker. The job runner calls it
//     while the agent's CURRENT job still reads `running` — the job store then
//     starts no other job of the agent on any server — so every entry it finds
//     is an orphan.
//
//  2. MaintainJobRepository is BOUNDED git maintenance of the checkout jobs
//     fetch into, replacing git's own auto-gc there.
//
// 🔴 WHY GIT'S OWN AUTO-GC IS TURNED OFF IN THE CHECKOUT. A worker's checkout
// is often SHALLOW, and a job's anonymous `git fetch +refs/pull/<n>/head` into a
// shallow clone, without --depth, downloads the pull request's whole history
// again on every job (the job's ref is deleted afterwards, so the next fetch
// cannot offer it as common ground): one 300–400 MB pack per job, 1,389 of
// them (161 GiB) on one worker. Auto-gc should have repacked them but
// (a) a stale .git/gc.log ("too many unreachable loose objects") blocks every
// auto-gc while it is younger than gc.logExpiry, and is rewritten by each
// attempt, so it never ran again; and (b) when it does run it is a FULL repack
// (`git repack -d -l -A --unpack-unreachable=…`), whose pack-objects keeps an
// entry for every object of every pack: in a 3 GiB container it took anon
// memory from ~0.9 GB to over 2.7 GB within seconds and was OOM-killed, even
// with pack.threads=1 and small window/cache limits — and it would have taken
// the agent's live session with it. So gc.auto=0, gc.autoPackLimit=0 and
// maintenance.auto=false are written into the checkout's own config on every
// pass (a session can change that config), and only the bounded steps below
// run.
//
// 🔴 WHY MULTI-PACK-INDEX STEPS, NOT gc/repack: what they hold in memory.
//   - `multi-pack-index write` reads every pack's .idx (mmapped: page cache,
//     reclaimable, not anon) and sorts ONE fanout slice (1/256th of all
//     in-pack entries) at a time, keeping one entry per DISTINCT object. Its
//     anon peak follows the distinct objects, not how many duplicate packs
//     carry them.
//   - `multi-pack-index expire` deletes every pack none of whose objects the
//     index takes from it. The index takes a duplicated object from the NEWEST
//     pack holding it, so a pack whose every object a later fetch brought again
//     goes at once: the bulk of the shallow-fetch duplicates, with no repack.
//   - `multi-pack-index repack --batch-size=B` rewrites, oldest first, only
//     packs whose share of the index is small, until about B bytes of them are
//     chosen, into one pack; a later expire removes the packs it replaced. Its
//     pack-objects sees only that batch's objects, reusing their deltas
//     (pack.threads=1, pack.windowMemory bounds any delta search), so its
//     memory follows B, not the repository.
//   - the loose-objects task packs loose objects (at most 50,000 a pass) and
//     deletes loose copies of packed ones.
//
// None of them deletes an object no other pack holds — no prune of
// unreachable objects, no `repack -d` of reachable sets — which is what makes
// them safe beside a concurrent fetch (git's own background maintenance runs
// the same steps beside a working user). Unreachable objects therefore stay,
// packed; with the duplicates gone that is the pull requests' own commits.
//
// ⚠ ONE PASS MAY NOT FINISH. A pass gates each step on a deadline (it never
// kills git mid-step, which would leave a lock) and the next job's pass
// continues. A pass runs when it is due: the checkout holds more than
// maxPacks packs, or the last pass started more than interval ago.

// MaintenanceStampFile is the file in the checkout's git directory whose mtime
// is when the last maintenance pass started.
const MaintenanceStampFile = "muster-maintenance"

// Housekeeping defaults (MaintainJobRepository).
const (
	// MaintenanceInterval: a pass runs at least this often while jobs run.
	MaintenanceInterval = 24 * time.Hour
	// MaintenanceMaxPacks: more packs than this makes a pass due at once. One
	// sha job on a shallow checkout adds a pack, so this is a handful of jobs.
	MaintenanceMaxPacks = 8
	// MaintenanceBatchBytes is `multi-pack-index repack --batch-size`: about how
	// many bytes of packs one pass rewrites.
	MaintenanceBatchBytes = 256 << 20
	// MaintenanceBudget bounds one pass: no step STARTS after it has elapsed.
	MaintenanceBudget = 10 * time.Minute
	// StaleGCLogAge: a .git/gc.log older than this is printed and removed.
	StaleGCLogAge = 24 * time.Hour
)

// MaintenanceGitConfig is the `-c` every maintenance git runs with: one
// thread, small delta window and caches, no bitmaps, the multi-pack-index on.
// They are command-line config, so the checkout's own (the session's) values
// for them do not count.
var MaintenanceGitConfig = []string{
	"core.multiPackIndex=true",
	"pack.threads=1",
	"pack.windowMemory=64m",
	"pack.deltaCacheSize=16m",
	"core.deltaBaseCacheLimit=32m",
	"core.packedGitLimit=256m",
	"repack.writeBitmaps=false",
}

// AutoGCOffConfig is what MaintainJobRepository writes into the checkout's
// own config on every pass: git's own auto-gc and auto-maintenance off.
var AutoGCOffConfig = [][2]string{
	{"gc.auto", "0"},
	{"gc.autoPackLimit", "0"},
	{"maintenance.auto", "false"},
}

// jobSweepScript removes every job worktree under $2 (entries named by a job
// id only; anything else there is left) of the checkout $1, prunes git's
// worktree list, and deletes every refs/muster-jobs/* ref. It prints one line
// per worktree removed ("removed <dir>"), and fails only when one is still
// there.
var jobSweepScript = gitRunsNoRepoCommands + `export LC_ALL=C
main=$1 jobs=$2
cd "$main" 2>/dev/null || exit 0
left=
for d in "$jobs"/*; do
	case ${d##*/} in
	'*'|*[!0-9]*|0*) continue ;;
	esac
	[ -e "$d" ] || [ -L "$d" ] || continue
	git worktree remove --force --force "$d" >/dev/null 2>&1
	rm -rf "$d"
	if [ -e "$d" ] || [ -L "$d" ]; then
		left="$left $d"
	else
		echo "removed $d"
	fi
done
git worktree prune >/dev/null 2>&1
git for-each-ref --format='%(refname)' refs/muster-jobs/ | while read -r r; do
	git update-ref -d "$r" >/dev/null 2>&1 && echo "deleted $r"
done
if [ -n "$left" ]; then
	echo "could not remove the job worktree(s)$left" >&2
	exit 1
fi`

// jobMaintainScript is one maintenance pass over the checkout $1: $2 the pass
// interval and $3 the gc.log age limit (seconds), $4 the pack count over which
// a pass is due at once, $5 the repack batch size (bytes), $6 the pass budget
// (seconds); the rest are MaintenanceGitConfig. See the comment above.
var jobMaintainScript = gitRunsNoRepoCommands + `export LC_ALL=C
main=$1 interval=$2 logage=$3 maxpacks=$4 batch=$5 budget=$6
shift 6
cd "$main" 2>/dev/null || { echo "no checkout at $main"; exit 0; }
gd=$(git rev-parse --path-format=absolute --git-common-dir) || exit 1
G=git
for c in "$@"; do G="$G -c $c"; done
` + autoGCOffLines() + `
now=$(date +%s)
if [ -f "$gd/gc.log" ]; then
	age=$((now - $(stat -c %Y "$gd/gc.log")))
	if [ "$age" -ge "$logage" ]; then
		echo "gc.log (${age}s old), removed; it said:"
		head -c 2048 "$gd/gc.log" | sed 's/^/  | /'
		rm -f "$gd/gc.log"
	fi
fi
packs() { ls "$gd/objects/pack/" 2>/dev/null | grep -c '\.pack$'; }
size() { du -sk "$gd/objects" 2>/dev/null | cut -f1; }
before=$(packs)
stamp=$gd/` + MaintenanceStampFile + `
if [ -f "$stamp" ] && [ $((now - $(stat -c %Y "$stamp"))) -lt "$interval" ] && [ "$before" -le "$maxpacks" ]; then
	echo "not due: $before pack(s), last pass $((now - $(stat -c %Y "$stamp")))s ago"
	exit 0
fi
touch "$stamp" || exit 1
kb=$(size)
end=$((now + budget))
rc=0
step() {
	if [ "$(date +%s)" -ge "$end" ]; then
		echo "skipped (budget spent): $*"
		return 0
	fi
	$G "$@" >/dev/null 2>"$gd/muster-maintenance.err" || {
		echo "failed: git $*: $(tail -c 500 "$gd/muster-maintenance.err")"
		rc=1
	}
}
step maintenance run --task=loose-objects --quiet
step multi-pack-index write --no-progress
step multi-pack-index expire --no-progress
step multi-pack-index repack --no-progress --batch-size="$batch"
step multi-pack-index expire --no-progress
rm -f "$gd/muster-maintenance.err"
echo "packs $before -> $(packs), objects ${kb}K -> $(size)K"
exit $rc`

// autoGCOffLines is AutoGCOffConfig as `git config --local` lines.
func autoGCOffLines() string {
	var b strings.Builder
	for i, kv := range AutoGCOffConfig {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("git config --local " + kv[0] + " " + kv[1] + " || exit 1")
	}
	return b.String()
}

// checkoutDir is ag's checkout, refusing an agent with no repository as
// jobDirs does.
func checkoutDir(ag agents.Agent) (string, error) {
	main, _, err := jobDirs(ag, 1)
	return main, err
}

// SweepJobWorktrees removes every job worktree of ag's checkout and every
// refs/muster-jobs/* ref (jobSweepScript), and returns what it removed, one
// line each. Call it only while no job of ag can be running but the caller's
// own, whose worktree is already removed: it does not ask which jobs run.
func (a *Adapter) SweepJobWorktrees(ctx context.Context, ag agents.Agent) ([]string, error) {
	main, err := checkoutDir(ag)
	if err != nil {
		return nil, err
	}
	var out, errb bytes.Buffer
	cmd := []string{"sh", "-c", jobSweepScript, "muster-job-sweep", main, agents.JobWorktreesDir(main)}
	err = provision.Exec(ctx, a.driver, agents.RefOf(ag), cmd, nil, &out, &errb)
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	if err != nil {
		return lines, fmt.Errorf("sweep the job worktrees of %s: %w: %s", main, err, strings.TrimSpace(capString(errb.String(), 1024)))
	}
	return lines, nil
}

// MaintenanceOptions bound one MaintainJobRepository pass; a zero field is the
// package default.
type MaintenanceOptions struct {
	Interval, StaleGCLog, Budget time.Duration
	MaxPacks                     int
	BatchBytes                   int64
}

func (o MaintenanceOptions) withDefaults() MaintenanceOptions {
	if o.Interval <= 0 {
		o.Interval = MaintenanceInterval
	}
	if o.StaleGCLog <= 0 {
		o.StaleGCLog = StaleGCLogAge
	}
	if o.Budget <= 0 {
		o.Budget = MaintenanceBudget
	}
	if o.MaxPacks <= 0 {
		o.MaxPacks = MaintenanceMaxPacks
	}
	if o.BatchBytes <= 0 {
		o.BatchBytes = MaintenanceBatchBytes
	}
	return o
}

func secs(d time.Duration) string { return strconv.FormatInt(int64(d/time.Second), 10) }

// MaintainJobRepository runs one bounded maintenance pass (jobMaintainScript)
// on ag's checkout with the package defaults, and returns its report.
func (a *Adapter) MaintainJobRepository(ctx context.Context, ag agents.Agent) (string, error) {
	return a.MaintainJobRepositoryWith(ctx, ag, MaintenanceOptions{})
}

// MaintainJobRepositoryWith is MaintainJobRepository with opts.
func (a *Adapter) MaintainJobRepositoryWith(ctx context.Context, ag agents.Agent, opts MaintenanceOptions) (string, error) {
	main, err := checkoutDir(ag)
	if err != nil {
		return "", err
	}
	o := opts.withDefaults()
	var out bytes.Buffer
	cmd := append([]string{"sh", "-c", jobMaintainScript, "muster-job-maintain", main,
		secs(o.Interval), secs(o.StaleGCLog), strconv.Itoa(o.MaxPacks), strconv.FormatInt(o.BatchBytes, 10), secs(o.Budget)},
		MaintenanceGitConfig...)
	err = provision.Exec(ctx, a.driver, agents.RefOf(ag), cmd, nil, &out, &out)
	report := strings.TrimSpace(capString(out.String(), 4096))
	if err != nil {
		return report, fmt.Errorf("maintain the repository %s: %w", main, err)
	}
	return report, nil
}
