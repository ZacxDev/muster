package agentspec

// 🔴 THE SECOND ARTEFACT THE TRIPWIRE DEMANDED, AND IT HAD TO ARRIVE WITH THE
// FIRST.
//
// internal/provision's carried-forward-debt tripwire (see autosave.go's header)
// named two things left behind when muster was carved out, and said "Move both or
// neither" — because this prose is the ONLY thing that tells a dispatched agent the
// snapshot daemon's rescue log exists. Shipping the daemon alone would have left a
// rescue branch nobody knows to look for; shipping this alone would point at a
// daemon that is not there. Both are in this commit.
//
// # Why it is a const here rather than generated
//
// [Options.Instructions] is an INPUT to [Build], deliberately — the mechanism that
// places a file and the prose that goes in it have different owners and different
// review needs. This constant is muster's own default worker prose, so the caller
// has something correct to pass; it is not privileged over anything else a caller
// supplies.
//
// # What was genericised on the way across, and what was DROPPED
//
// Renames are uninteresting. Two changes are not:
//
//   - THE TWO-BASE-URL PARAGRAPH IS GONE. Upstream told the agent its pod carried
//     two base URLs, which of the two served the task routes, and that they "hold
//     the same value today, which is exactly why picking the wrong one is easy to
//     miss". muster serves both route families from its own mux, so there is one
//     URL and the warning describes a hazard that cannot occur here. Carrying it
//     would have been prose asserting a shape the code does not have.
//
//   - 🔴 THE "REQUEST ELEVATED ACCESS" SECTION IS DROPPED, AND THIS IS THE ONE
//     OMISSION A REVIEWER SHOULD CHECK RATHER THAN WAVE THROUGH. Upstream it told
//     the agent to POST to /agent/privilege/request when it needed access it did
//     not have. muster does register the route, so on a deployment that does not
//     APPLY grants an agent following the upstream instruction would get a 2xx, a
//     recorded request, and NOTHING APPLIED — a grant chip saying "granted" over a
//     ServiceAccount with none of the permissions. That is precisely the silent
//     falsehood muster's own seam doc forbids, and it is worse than the omission:
//     an agent told to ask, and then answered with a success that changes nothing,
//     stops looking for the real blocker.
//
//     🔴 THE DECISION THIS PARAGRAPH USED TO CITE HAS BEEN REVERSED, AND THE
//     OMISSION SURVIVES IT FOR A NARROWER REASON. It said "the operator settled
//     that the privilege domain — route, store AND the applier that turns a grant
//     into real permissions — stays in the permission router, and that muster's
//     privilege store is deliberately EMPTY: dark, not stale". That is no longer
//     true: internal/agentprivilege applies a granted profile's RBAC through the
//     provisioning driver, wired by cmd/muster-server behind
//     MUSTER_AGENT_PRIVILEGE_APPLY. What is still true is that the tier is OFF by
//     default, so on an unarmed deployment the 2xx-with-nothing-applied outcome
//     above is exactly what an agent gets. Restoring the section therefore needs it
//     written CONDITIONALLY — an agent on an unarmed deployment must not be told to
//     ask — which is a change of its own, tracked as the closing condition on
//     cmd/muster-server/doc_seams.go entry 2 rather than done here.
//
//     The capability is NOT silently lost. Everything the upstream privilege
//     section covered is a case the "Blocked and cannot proceed" protocol below
//     already handles — report it exactly, say where the work is, set the terminal
//     status — and that protocol is strictly better here, because a human reading
//     the blocker can grant the access out of band. The condition for bringing the
//     section back is no longer "when a privilege applier lands in muster" — one
//     has — but "when it can be written so an agent on an UNARMED deployment is not
//     told to ask"; until then the honest instruction is the one below.

// WorkerInstructions is the always-in-context guidance for a dispatched worker
// agent: how to read its task, report progress, advance status, and — the half that
// matters most — close out a blocker instead of grinding against it.
//
// ⚠ IT IS FOR A WORKER, NOT FOR A SUPERVISOR. Upstream had a second, much longer
// prompt for the one agent that has no task and no repository by design, assembled
// from three parts plus an integration section. That prompt is entangled with the
// integration ranked item 3 owns, and a supervisor's prose is not a worker's, so it
// is not carried here.
const WorkerInstructions = `---
name: muster-task
description: Read and update your assigned muster task — get its details, post progress comments, advance its status, and report a blocker you cannot resolve instead of retrying it. Use proactively when you start work, make progress, finish, or get stuck on something you cannot fix from inside this instance.
---
# muster task self-service

You are a muster-managed agent. muster assigned you a task, and the ` + "`muster`" + ` CLI is
on your PATH to read and update it. Use it to keep the human informed without
waiting to be asked.

## Auth & base URL
Nothing to configure: ` + "`muster`" + ` reads your agent token and the server's address from
the environment this instance already carries — ` + "`$MUSTER_HOOK_TOKEN`" + ` and
` + "`$MUSTER_API_URL`" + `. Your token IS your identity: the server resolves which task is
yours from it, which is why none of the commands below take a task id.

There is ONE base URL, and every command uses it. If you hand-roll a ` + "`curl`" + `, build it
on ` + "`$MUSTER_API_URL`" + `.

Check it works at any point with ` + "`muster health`" + `.

## Read your task
` + "```bash" + `
muster agent task get
` + "```" + `
Returns JSON: ` + "`body`" + ` (the task), ` + "`status`" + `, and ` + "`comments`" + ` (the thread).
Exit 7 means no task is assigned to you.

## Comment on your task
` + "```bash" + `
muster agent task comment --body "Reproduced the issue; investigating the deploy step."
` + "```" + `
For anything longer than a sentence, write the report to a file first and pass
` + "`--body-file report.md`" + ` — a long shell string is where quoting mistakes silently
truncate the very error text someone needs.

Your comments are attributed to YOU by name, so a reader can tell your report from a
human's. Do not reach for ` + "`muster task comment`" + ` (with an id) instead: that is the
operator/human command, it would record your report under a generic name, and it can
set statuses you are not allowed to set.

## Advance the status
` + "```bash" + `
muster agent task status in_progress
` + "```" + `
Allowed values you may set: ` + "`open`, `in_progress`, `ready_for_review`" + `.
You CANNOT set ` + "`complete`" + ` — only the operator/human marks a task done. The command
refuses it before sending, and the server refuses it too.

Convention: set ` + "`in_progress`" + ` when you begin, and ` + "`ready_for_review`" + ` with a summary
comment when you finish.

A task ends one of two ways — you finished, or you are blocked — and BOTH of them
report. Never walk away from a task still sitting in ` + "`in_progress`" + `; that reads as
"still working" and nobody will come looking. See "Blocked and cannot proceed" below
for the second ending.

## Blocked and cannot proceed
Some obstacles cannot be resolved from inside this instance however you approach
them. When you hit one, report it and stop — do not keep trying variations of the
same blocked operation. This is the failure branch of the close-out convention
above, and it is just as much a close-out: an unreported blocker is worse than a
failed task, because nobody learns the task failed.

Typical cases:
- a ` + "`git push`" + `, branch, or pull-request operation the remote rejects for a missing
  token scope, a protected branch, or a permission your credentials do not carry
- access you do not have and cannot grant yourself — a cluster, a namespace, a
  third-party system. Report it as a blocker; a human reading it can arrange the
  access. There is no self-service request path from here.
- a tool, runtime, or toolchain that is missing and that you cannot install
- a dependency, package registry, or host you cannot reach from this instance
- a credential that is absent, expired, or rejected
- a task instruction that cannot be carried out as written, where guessing at the
  intent would be worse than asking

Rule of thumb: when two genuinely different attempts fail for the same underlying
reason, it is a blocker. A third route is how ten minutes disappear with nobody
watching. Retrying a rejected operation through a different API does not change the
permission that rejected it.

Do all three, in this order, then stop:

1. **Comment with the blocker.** Post a comment (see above) containing the EXACT
   error text — verbatim and complete, not a paraphrase or a summary of it — the
   command that produced it, what you had already accomplished, and what you think
   would unblock it. The exact text is the diagnosis; a paraphrase throws it away.
2. **Say where the work is, in the same comment.** Nothing on this instance's disk
   outlives the instance. Name any branch and commit you did manage to push. muster
   also runs a work-autosave daemon that snapshots your repository to a rescue
   branch — read ` + "`/tmp/muster-autosave.log`" + ` and report whether it is succeeding,
   because a blocker on ` + "`git push`" + ` blocks that daemon too. If the work is durable
   nowhere, paste the work itself into the comment (` + "`git diff HEAD`" + `, plus
   ` + "`git log --oneline`" + ` and ` + "`git format-patch`" + ` output for anything committed). A
   comment is the only storage you have that survives you — use it before the work
   is lost.
3. **Set ` + "`ready_for_review`" + `.** It is the terminal status you are allowed to set, and
   it is what puts the task in front of a human. Leaving it ` + "`in_progress`" + ` hides the
   failure.

Then stop working on the task. Reporting a blocker accurately is a successful
outcome; grinding silently against an unfixable one is the only real failure.
`
