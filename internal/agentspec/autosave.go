package agentspec

import (
	_ "embed"
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/provision"
)

// autosaveScript is the in-pod work-autosave daemon.
//
// See autosave.sh for the incident that motivates it and the design rationale. It
// is embedded rather than inlined as a Go string so the bytes the tests execute
// under /bin/sh are byte-for-byte the bytes shipped into the instance.
//
//go:embed autosave.sh
var autosaveScript string

// 🔴 WHY THIS EXISTS HERE AT ALL, SINCE THE PACKAGE DOC SAYS Init IS EMPTY.
//
// internal/provision carried a TRIPWIRE — a test asserting that provision.Spec
// still had no non-test producer, in internal/provision/carried_forward_debt_test.go,
// removed in the commit that added this file. Two artefacts were deliberately left
// behind when muster was carved out — this daemon, and the agent instruction set —
// BECAUSE there was nowhere to deliver them, and the tripwire's stated closing
// condition was that the first non-test producer of provision.Spec carries them
// across. [Build] is that producer. So they arrive here, and the tripwire was
// DELETED rather than widened, exactly as its own failure message instructed: it
// forbade adding the new producer to its exemption list, on the grounds that
// widening it turns a closing condition into a comment nobody will act on.
//
// ⚠ ITS NAME IS DELIBERATELY NOT SPELLED HERE. A separate guard
// (internal/modulegate's citation check) fails when a comment names a test that
// does not exist, and it caught an earlier draft of this paragraph doing exactly
// that — a citation to a deleted test reads to the next person as coverage that is
// still in place. The file path above is the durable reference; `git log` on it is
// how you read the tripwire's full argument.
//
// 🔴 THE INSTALL HALF IS NOT PORTED, AND THAT IS THE POINT RATHER THAN AN
// OMISSION. Upstream, installing this script meant base64-encoding it onto one
// line, decoding it to a path, chmod'ing it, and wrapping the whole thing in
// if/then/else — because the chart re-indented every line of its init hook (so a
// heredoc was corrupted) and ran it under `set -e` (so a bare non-zero aborted
// container startup and a failed autosave install cost the agent its existence).
// Both of those are workarounds for one chart. [provision.File] places bytes at a
// path natively, so the encoding, the decode, the chmod and the guard all
// disappear: 24 lines of shell become one File and one Init entry. Porting the
// install half would have imported the workaround without the problem — which is
// the specific mistake the tripwire's comment warned about.
const (
	// autosavePath is where the daemon is placed inside the instance.
	autosavePath = "/usr/local/bin/muster-autosave"
	// autosaveMode is the file mode. It must be executable: the Init entry below
	// runs it through /bin/sh explicitly, but a non-executable daemon at a path
	// named like a binary is a trap for anyone who later invokes it directly.
	autosaveMode fs.FileMode = 0o755
	// autosaveLogPath is where the daemon writes its diagnostics.
	//
	// ⚠ IT CANNOT USE STDOUT, and that is measured upstream rather than assumed:
	// a process backgrounded from a container's init script inherits an orphaned
	// pipe that is NOT the container log stream, so anything it echoes is
	// invisible to `kubectl logs`.
	autosaveLogPath = "/tmp/muster-autosave.log"
	// autosaveRefPrefix namespaces every snapshot ref so a rescue branch is
	// unmistakable and can never collide with a branch a human or the agent
	// created. One ref per agent, force-updated in place.
	autosaveRefPrefix = "refs/heads/muster-wip/"
	// autosaveIntervalSeconds is the snapshot cadence. Two minutes bounds the
	// worst-case loss to ~2 minutes of work while staying far below the rate at
	// which an agent produces meaningfully different trees.
	autosaveIntervalSeconds = 120
	// autosaveMinIntervalSeconds is the floor the cadence is held above. At or
	// near zero the daemon becomes a hot loop re-walking the whole worktree
	// continuously on an emptyDir.
	autosaveMinIntervalSeconds = 30
)

// 🔴 THERE IS NO DEFAULT EXCLUDE LIST, AND THE REASON IS NOT STYLE.
//
// Upstream this was seven hardcoded filenames belonging to ONE agent runtime. The
// port moved them out of the shell into Go — and the repository's own secret gate
// then REFUSED the Go copy, because those names identify a specific private
// product and this repository is public. That refusal was correct in a way the
// port had missed: a default list is not just a leak, it is WRONG for every
// runtime but one, in two directions at once. The unknown runtime's bookkeeping
// files get force-pushed into a stranger's repository, AND paths it never creates
// are silently dropped from the rescue snapshot — so a repository legitimately
// holding an untracked root TOOLS.md loses it from the one copy meant to save it.
// Neither failure is visible from inside the instance.
//
// So the list belongs to whoever knows which runtime they are running:
// [Config.AutosaveExcludePatterns]. Empty is a valid and silent answer — a runtime
// that writes nothing into the workspace genuinely has nothing to exclude.
//
// ⚠ EACH ENTRY SHOULD BE ANCHORED WITH A LEADING "/". Unanchored, `TOOLS.md` also
// excludes `docs/TOOLS.md`, which the agent may legitimately be editing. Neither
// this package nor the daemon adds the anchor for you, because silently rewriting a
// caller's pattern would make an intentionally-unanchored one impossible to express.

// autosaveExcludeEnvValue renders exclude patterns as the daemon's colon-separated
// MUSTER_WIP_EXCLUDE value.
//
// Colon-separated rather than newline-separated because the daemon is started from
// a single Init command line, which a newline would terminate. A pattern containing
// a colon is refused rather than silently split — it would become two wrong
// patterns, and a gitignore pattern with a colon in it is legal.
func autosaveExcludeEnvValue(patterns []string) (string, error) {
	for _, p := range patterns {
		if strings.Contains(p, ":") {
			return "", fmt.Errorf("agentspec: exclude pattern %q contains a colon, which is the list separator; it would be split into two wrong patterns", p)
		}
	}
	return strings.Join(patterns, ":"), nil
}

// autosaveRef returns the remote ref an agent's snapshots are pushed to.
//
// 🔴 KEYED ON THE AGENT ID, NOT THE NAME, AND THE ID IS WHAT MAKES IT SAFE FOR
// ALL TIME. Keyed on name alone, a new agent drawing a recycled name on the same
// repository would force-push its snapshot over a dead agent's rescue branch —
// destroying the only copy of that work at precisely the moment someone went
// looking for it. The id is monotonic and never reused, so the ref is unique
// forever; the name is kept in front of it purely so the branch is legible to a
// human scanning a branch list.
//
// ⚠ DO NOT "SIMPLIFY" THIS TO THE NAME BECAUSE NAMES ARE TOMBSTONED NOW. Name
// reuse being prevented going forward is not the same claim as no two agents ever
// having shared a name: history that predates a tombstone table cannot be
// backfilled, because nothing recorded the names. Each such name is reissuable
// once more, and this key is what makes that harmless.
func autosaveRef(agentName string, agentID int64) string {
	return autosaveRefPrefix + agentName + "-" + strconv.FormatInt(agentID, 10)
}

// autosaveFile is the daemon as a [provision.File].
func autosaveFile() provision.File {
	return provision.File{
		Path:    autosavePath,
		Content: []byte(autosaveScript),
		Mode:    autosaveMode,
	}
}

// autosaveInvocation is the single [provision.Spec.Init] entry that starts the
// daemon.
//
// It is ONE line carrying every required variable, deliberately: a guard can then
// match an actual invocation rather than an install line, and there is no ordering
// dependency between separate Init entries to get wrong.
//
// ⚠ IT IS BACKGROUNDED WITH `&` AND THAT IS LOAD-BEARING. The daemon is an
// infinite loop; run in the foreground it would block the instance's own process
// from ever starting, turning a durability feature into a total outage. The
// upstream version needed an if/then/else around this to survive `set -e`; here a
// driver owns how Init entries are run, so the guard belongs to the driver's
// contract rather than to this string.
func autosaveInvocation(repoPath string, agentName string, agentID int64, excludes []string) (string, error) {
	excl, err := autosaveExcludeEnvValue(excludes)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"MUSTER_WIP_REPO=%s MUSTER_WIP_REF=%s MUSTER_WIP_INTERVAL=%d MUSTER_WIP_EXCLUDE=%s /bin/sh %s &",
		shellQuote(repoPath),
		shellQuote(autosaveRef(agentName, agentID)),
		autosaveIntervalSeconds,
		shellQuote(excl),
		autosavePath,
	), nil
}

// shellQuote single-quotes a value for POSIX sh, escaping embedded quotes.
//
// ⚠ IT IS STILL NEEDED EVEN THOUGH FILES ARE DATA NOW. The Init entry above is a
// shell contract by [provision.Spec.Init]'s own admission — the least portable
// field in the spec — so a repository path containing a quote would otherwise
// break out of the assignment.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// autosaveInitCommands is retained as a named seam for the ported tests, which
// assert on the invocation as a unit.
func autosaveInitCommands(agentName string, agentID int64, repoPath string, excludes []string) string {
	cmd, err := autosaveInvocation(repoPath, agentName, agentID, excludes)
	if err != nil {
		panic(err) // test-only seam; a bad pattern is a programming error here
	}
	return cmd
}
