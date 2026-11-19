package agentprovision

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// THE SWEEP OF LEFT-BEHIND JOB WORKTREES. A job's own removal
// (RemoveJobWorktree) runs in a deferred call of the server process running
// the job, so a job whose server stopped or crashed mid-turn (docs/jobs.md
// "No survival of a running turn across a restart") leaves its worktree under
// .muster-jobs and its refs/muster-jobs/* refs for ever; 19 of them (4.8 GB)
// were found on one review worker. SweepJobWorktrees removes them all. The job
// runner calls it while the agent's CURRENT job still reads `running` — the
// job store then starts no other job of the agent on any server — and after
// that job's own worktree is removed, so every entry it finds is an orphan.

// jobSweepScript removes every job worktree under $2 (entries named by a job
// id only; anything else there is left) of the checkout $1, prunes git's
// worktree list, and deletes every refs/muster-jobs/* ref. It prints one line
// per worktree removed ("removed <dir>") and per ref deleted ("deleted
// <ref>"), and fails only when a worktree is still there.
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

// SweepJobWorktrees removes every job worktree of ag's checkout and every
// refs/muster-jobs/* ref (jobSweepScript), and returns what it removed, one
// line each. Call it only while no job of ag can be running but the caller's
// own, whose worktree is already removed: it does not ask which jobs run.
func (a *Adapter) SweepJobWorktrees(ctx context.Context, ag agents.Agent) ([]string, error) {
	main, _, err := jobDirs(ag, 1)
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
