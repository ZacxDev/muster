# Scheduled host jobs

A **scheduled host job** is a fixed command one of your mirrored machines runs
on a cron, whose output becomes a version of a muster page. A report that
needs that machine's own access (its SSH keys, a browser profile, a local
database) runs where that access is, and you read the result on a page, with
its run history and an Attention item when a run fails.

A job is not an officer: no agent runs, nothing reasons about the output, and
declaring jobs does not make the machine an officer host or change how its
mirror door's owner-authority tools behave ([below](#what-declaring-jobs-does-not-change)).

- [Declaring a job](#declaring-a-job)
- [How a run goes](#how-a-run-goes)
- [The page](#the-page)
- [When a run fails](#when-a-run-fails)
- [Reading and running jobs (`muster job`)](#reading-and-running-jobs-muster-job)
- [What muster can and cannot make a host do](#what-muster-can-and-cannot-make-a-host-do)
- [What declaring jobs does not change](#what-declaring-jobs-does-not-change)
- [Limits](#limits)

## Declaring a job

Jobs are declared **on the host**, in muster-mirror's jobs file, and nowhere
else: `-jobs-file`, or `MUSTER_MIRROR_JOBS_FILE`
([configuration.md](configuration.md)). muster has no verb that adds or
changes one.

```json
{
  "jobs": [
    {
      "name": "smoke",
      "command": ["/usr/bin/date", "-u"],
      "cron": "0 14 * * *",
      "timeoutSeconds": 60
    },
    {
      "name": "disk-report",
      "command": ["/bin/sh", "-c", "df -h / | sed 's/^/    /'"],
      "cron": "*/30 * * * *",
      "title": "Disk on this machine",
      "output": "markdown",
      "dir": "/tmp"
    }
  ]
}
```

| field | required | meaning |
|---|---|---|
| `name` | yes | lowercase letters, digits and `-` (at most 63); unique on the host |
| `command` | yes | the argv, run as given with **no shell**; its program is an **absolute path** (the service's `PATH` is not your shell's). A job that wants a shell declares `["/bin/sh", "-c", "…"]` itself |
| `cron` | yes | five fields, minute hour day-of-month month day-of-week, **UTC**: the grammar of officer schedules ([officers-builtin.md](officers-builtin.md)). No names, no `@daily` |
| `timeoutSeconds` | no | 1–21600, default 600. At the limit the host kills the command's whole process group |
| `output` | no | `markdown` (default): stdout is one markdown block. `blocks`: stdout is a JSON list of page blocks, as the page tools take ([pages.md](pages.md)) |
| `title` | no | the page's title (default: the job's name) |
| `dir` | no | the working directory, absolute (default: the home directory; when `HOME` names no directory, muster-mirror's own working directory) |

The file is read **once, at start**, and validated whole: one bad entry and no
job of that file is declared. The reason is logged, and sent to muster, which
shows it in `muster job list` and keeps the jobs the host declared before —
so their next fire is refused by the host and reaches your Attention, rather
than going quiet. Check a file before restarting with
`muster-mirror -list -jobs -jobs-file <path>`, which prints one line per job
and dials nothing.

Changing a job is an edit of the file and a restart of muster-mirror. A job
removed from the file stops being fired; its run history stays.

## How a run goes

**muster fires the cron**, not the host: one scheduler on the server, the
officer schedules' compare-and-set, so a due time is never fired twice, and a
fire while the host is **offline** is a run of its own with an Attention item
— a missed run is seen, not silent. (Like the rest of the mirror, this needs
muster to run as ONE replica: a host is connected to one process, and a
replica not holding that connection would see it offline.) Missed fires
(muster itself down) collapse to one, as officer schedules' do.

1. At a due time muster records a run and sends the host a `job-run` over the
   mirror tunnel naming the job and the run — nothing else
   ([below](#what-muster-can-and-cannot-make-a-host-do)).
2. The host looks the name up in its own file and runs that entry's argv,
   with no stdin, capturing stdout (at most 512 KiB) and the last 2 KiB of
   stderr. **Its environment** is muster-mirror's own — `HOME`, `PATH`,
   `SSH_AUTH_SOCK` and every other variable the service has — **minus every
   variable whose name starts `MUSTER_`** (the daemon's token file, URL and
   switches are not the job's). A job needing a tool the service's `PATH`
   does not reach names it by absolute path, or sets it up in a wrapper
   script.
3. When it ends — exit, timeout — the host sends a `job-result` on whichever
   connection is up then, so a run that outlives a reconnect still reports
   (a result that finds no connection is kept for the next one).
4. muster writes the page when the output changed, records the run, and
   raises an Attention item when it failed.

**One run at a time.** A fire while the job's previous run is still going is
**skipped** and recorded as such (status `skipped`), never stacked. The
database holds the rule (one running run per job), and the host refuses a
second start of a job still running as well.

A run whose result never arrives — the host died mid-run, the result was lost
— is called **lost** once its timeout plus five minutes has passed, with an
Attention item.

## The page

Every job writes **its own page**: one of your pages, which muster makes, as
the host, on the job's first successful output and reuses after. A jobs file
names no page. If the job's page is deleted, the next run makes another.

**The page never fills.** A page keeps at most 100 versions
([pages.md](pages.md)); a job's page is a *rolling* one: when a new version
would be the 101st, muster drops the job page's **oldest version that nothing
references** first, so the page keeps its newest 100 and its id and share
links never change. A version is never dropped while it is referenced: the
**current** version (the one your view and every live share link show) and
any version a **response** was sent at are kept. If every older version is one
of those, the run fails with the page's own "too many versions" reason, as any
other page's would. Every other page keeps the plain cap.

A new version is written **only when the output changed**: when the blocks and
the title are the same as the page's current version, the run is `ok` and no
version is written (`page_version` is empty in its run). Rolling the page back
to an older version makes the next run compare against that one.

With `output: "blocks"` the output is validated exactly as a page tool's
blocks are; output that is not a valid page **fails the run** (status
`invalid`) with an Attention item, and the page is not touched. The same goes
for markdown larger than one markdown block holds (64 KiB).

Shareable pages must be on (`MUSTER_PAGES=1`, [pages.md](pages.md)); without
them every run fails with that reason.

**Team hosts.** Pages are personal (a team member cannot read a page a team
owns), so a team host's jobs are **refused**: muster records none of them and
logs why. Declare the job on a personal host.

## When a run fails

Each failed run raises **one** Attention item, a notice from muster labelled
`job <name> on <host>`, recorded on the run so it is never raised twice. The
words, exactly:

| what happened | the item |
|---|---|
| non-zero exit | `Job <name> on <host> failed: it exited with status <n>.` (its duration is in `muster job runs`) |
| timeout | `Job <name> on <host> timed out: the host stopped it after its <timeout> limit.` |
| output is not a valid page | `Job <name> on <host> ran, but its output is not a valid page, so the page was not changed: <why>.` |
| stdout over 512 KiB, page not writable | `Job <name> on <host> failed: <why>.` |
| host offline at fire time | `Job <name> on <host> did not run at <YYYY-MM-DD HH:MM UTC>: <host> was not connected to muster.` — **one** item for consecutive offline fires of the job (each run records it), withdrawn when the host connects again or a run succeeds |
| the host refused the run | `Job <name> on <host> did not run: <host> refused it (<why>).` |
| no result came back | `Job <name> on <host> was sent at <time>, and muster never got its result back (it was due within <timeout>). Check muster-mirror's log on <host>.` |

Every item for a run that started ends with the tail of its stderr
(`The end of its stderr:` and up to 2 KiB), or `Its stderr was empty.`

A run that succeeds raises nothing; neither does a skipped one.

If muster cannot **record** a host's jobs message (the database refuses it),
the jobs as last recorded stand, `muster job list` shows the reason as the
host's `jobs_error`, and one item labelled `jobs on <host>` says so; it is
withdrawn when a later connection's jobs are recorded. A new page
version sends no notification either: the item for a failure is the push
(Attention's own, [attention.md](attention.md)).

## Reading and running jobs (`muster job`)

```
muster job list                  # your hosts' declared jobs, per host, as each reported them
muster job runs smoke            # a job's runs, newest first
muster job runs smoke --host x   # when the name is declared on several hosts
muster job run smoke             # run it now, as its cron would
muster job forget old-box smoke  # a host that is gone: delete the job, its runs and its schedule
```

`job forget <host> <name>` is for a machine you decommissioned: its job
would otherwise fire, and raise its "did not run" item, for ever. It deletes
the job, its run history and its schedule, and withdraws its open items. A
host that still declares the job records it again on its next connection —
remove it from that host's jobs file instead. Your own jobs only.

`job list` shows each host's jobs (name, cron, command, page, output,
timeout, next fire), whether the host is connected, and — for a connected
host whose jobs file could not be read — why. `job runs` shows each run's
trigger (`cron` or `manual`), status (`running`, `ok`, `failed`, `timeout`,
`invalid`, `offline`, `skipped`, `refused`, `lost`), when it started and
ended, its exit code, duration, the page version it wrote (none when the
output was unchanged), the stderr tail, and its Attention item. `job run`
answers the run as recorded: `running` (sent; `job runs` shows how it ends),
`skipped`, `offline` or `refused`. The API is `GET /api/host-jobs`,
`GET /api/host-jobs/{name}/runs`, `POST /api/host-jobs/{name}/run` and `DELETE /api/host-jobs/{name}?host=`, as the
CLI's credential: a personal API token, or the hook token acting as the admin
([cli.md](cli.md)).

## What muster can and cannot make a host do

A compromised muster must not be able to run new code on your machine. So:

- **The command is the host's.** muster stores a job's command only to show
  it; it never sends it back. A `job-run` names a job and a run, and the host
  runs **its own** copy of that job's argv.
- **The host checks every `job-run`, three ways, before anything starts**, each
  its own refusal: the message carries nothing but its type, its request and
  its trace context (`message`); the request carries nothing but the run and
  the job's name — a command, arguments or an environment are refused, never
  merged (`arguments`); and the name is one its file declares (`undeclared`).
  A refused `job-run` starts nothing and is answered with the refusal, which
  muster records and raises.
- **muster checks too**: it sends a job-run only for a job the host's last
  jobs message declared.

At worst, then, a compromised muster can run one of the commands you already
declared on that machine, at a time you did not choose. Declare commands that
are safe to run at any time.

## What declaring jobs does not change

- The host is **not** an officer host: nothing is declared on the officer
  roster, and no officer window is recorded.
- Its mirror token is **not** marked as one that starts agents
  (`mirror_tokens.spawned_at`).
- So its mirror door's owner-authority tools (`attention_answer`,
  `dispatch_approve`, `plugin_draft_decide`, …) behave exactly as before: no
  step-up card ([sending-and-step-up.md](sending-and-step-up.md),
  [mirroring.md](mirroring.md#a-machine-that-starts-agents-is-step-up-gated)).
- Jobs need neither `-interactive` nor `-agent-spawn`.

## Limits

- At most 32 jobs per host; argv at most 32 arguments and 4 KiB.
- stdout at most 512 KiB (a page version's own bound); more fails the run.
- A markdown output is one markdown block: at most 64 KiB.
- The stderr tail kept and shown: the last 2 KiB.
- A job's page holds its newest 100 versions ([above](#the-page)): older
  unreferenced ones are dropped as new ones are written.
- Each job keeps its last 1000 runs.
- An older muster-mirror sends no jobs message, so muster sends it no
  `job-run`; an older muster ignores the jobs message. Upgrade both.
