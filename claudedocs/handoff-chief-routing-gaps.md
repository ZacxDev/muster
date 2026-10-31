# Handoff: chief-routing-gaps

## Run this first — the index, one command
```bash
<dotfiles>/scripts/cairn-ops/read.sh recall --repo "$(git rev-parse --show-toplevel)"
```
Terse pointers this doc does not carry, curated by past sessions and outliving it.
🔴 RECALL, NOT LIVE OBSERVATION — every line is a pointer to VERIFY, never a current
reading, and it may describe a gotcha already fixed. `scope-absent`/`scope-empty` means
nothing is recorded yet: ordinary, not an error, and not a clean bill of health.
Non-blocking: if it exits non-zero, print the stderr line and carry on.

## Goal
Messages and tasks reaching the chief were not routed to the live sessions already working them. Find out why, route what was still unhandled, and fix the causes.
- **closing-condition:** `check` — a review mail for a PR made with `gh -R <repo> pr create` routes to its authoring session (a `session_prs` row exists for it), and the chief's `muster_help {tool: route_to_session}` returns a definition that lists `task_id` (both observed live; see How to verify).

## State now
- **This arc is CLOSED.** Its closing condition was checked live:
  - Review mails for two PRs created with `gh pr create --repo <owner/repo>` (routes 117 and 118) were relayed to their authoring sessions, and each PR has a `session_prs` row.
  - The chief's `muster_help` for `route_to_session` lists `task_id`, checked last session on 0.118.0.
- **Follow-on arc, also closed: a never-delivered route says so.**
  - muster 0.119.0 is live (tag v0.119.0, commit a33492fb). It ships #378, the fix, and #379, which removed a denied identifier from this doc so the leak gate goes green on `main`.
  - **The change:** when the deadline sweep's own cancel of a still-queued paste succeeds after `TimeOutRoute`, `RouteStore.TimedOutUndelivered` does three things. It sets the route's error to `never delivered: the session stayed busy until the deadline, so its queued message was cancelled`. It rewords the still-owed hand-back to `it was never delivered: the session stayed busy until the deadline (<ts>), and its queued message was cancelled unsent`. And it writes a `session_route.undelivered` audit row.
  - **Audit:** round 0 dropped D1 (the window length); round 1 found nothing that blocks a merge.
  - **Verified live:** route `rt_45ec08314d4fee5031edbc55`, message 348 routed to a session kept busy until its deadline at 23:12:24Z.
    - The route ended `timed_out`, and its paste (row 147) ended `cancelled` and never claimed.
    - The route's error is the new text.
    - The chief's conversation holds the reworded hand-back as message 1990.
- **#381 (merged, test-only):** the nudge fixtures hide any older owner-1 chief that the shared per-package test DB already holds.
  - Before it, one leftover `chief` row, from an interrupted test run, made all 30 `TestChiefNudge*` tests fail locally, while CI stayed green on its fresh DB.
  - The leftover row was deleted. On `main` at 61eaa738, all 40 nudge tests pass locally.
- **Operator decisions:**
  - The never-delivered outcome stays in the chief's conversation only; no Attention or route-listing view.
  - Token rotation (rank 2) is skipped for now.
  - Quiet pushes (rank 3) stay dropped.

## Open investigations — live diagnosis state
### A routed message never reaches a busy session
- as-of: 2026-10-31
- **Symptom + exact repro:** route a plugin message to a session that stays busy. The paste waits for 30 s of idle, never gets it, and the route times out after `RouteTimeout` (30 min) as `no answer before the deadline`. Nobody is told that it was never delivered.
- **Observed (with values):** two manual routes ended with send_queue rows `state=cancelled detail=cancelled claimed_at=NULL` at the deadline, and session_routes `state=timed_out`. One other route ended `failed` with "May have been sent: muster handed it to the host, which did not confirm it".
- **Ruled out:** the route tool refusing the target. via: command — `plugin_message_route` returned `state=routed` for each.
- **Leading hypothesis:** by design, the send queue never interrupts a working session. The gap is the silent hand-back: the hand-back does not say "never delivered (session busy the whole window)" as opposed to "delivered, not answered".
- **Next probe:** `select r.route_id, r.state, q.state, q.claimed_at from session_routes r join send_queue q on q.id=r.send_queue_id where r.state='timed_out' order by r.id desc limit 20;` — count how many timed-out routes were never claimed.

### RESOLVED: a routed message never reaches a busy session (the block above)
- as-of: 2026-10-31
- **Probe result:** of the 20 most recent `timed_out` routes, 5 had their paste `cancelled` and never claimed (never delivered), 2 were `sent`, 2 failed after a claim, and 11 had no `send_queue` row (all from Oct 28–30).
- **Ruled out:** that the hand-back's silence was by design and harmless. Five of those 20 routes were never delivered, and their hand-backs said nothing about it. via: measurement
- **Fix:** #378, shipped in 0.119.0 and verified live on route `rt_45ec08314d4fee5031edbc55` (see State now). Leaving the busy session uninterrupted remains the design; only the hand-back's wording changed.
- **Not investigated:** why 11 older timed-out routes have no `send_queue` row. They predate Oct 30 and may come from before routes were linked to the queue.

## Next steps (ranked)
1. DONE: #378 tells a never-delivered route apart from an unanswered one (0.119.0, verified live). forcing: none
2. Rotate the chief's MCP token (`X-Muster-Token` in the chief pod's Claude config) and the ccd hook secret. Both were printed into an earlier session's local transcript. The operator chose to skip this for now. forcing: security — exposed in a local transcript, nowhere else
3. Quiet pushes (`git -C $W push -q`) are still not reported by the PR-authorship hook. The operator dropped this; revisit only if review mails keep missing because of it. forcing: none

## Defects (batched)
- The chief's model was stopped by a safety classifier on several routing and withdrawal replies (no text, "stopped before it ran"). Not investigated.
- 🟢 Open on #378: `TimeOutRoute` commits the hand-back in its old wording before the cancel and `TimedOutUndelivered` run. If a concurrent sweep, or a crash, delivers it in that gap, the chief gets "no answer came by". This is never a false "never delivered", and it matches pre-#378 behaviour.

## Gotchas / decisions / dead-ends
- **The cause of the stale tool schema, measured:** Claude Code saves a deferred MCP tool's schema in the transcript (`deferred_tools_record`) and replays it on every `--resume`. A pod restart does NOT refresh it, a ToolSearch re-fetch returns the saved copy, and `alwaysLoad` does not survive a resume either. The client does not enforce the stale schema: a call with an argument absent from it still succeeds.
- **What fixes staleness:** `current_definition` on a refused call. In a real CLI repro the model recovered on its first refusal. A tool-set version stamp in descriptions was ignored by the model, so it was dropped.
- **Two separate routing paths, two separate misses.** The PR-author route needs a `session_prs` row, written by the dotfiles hook; the chief's hint needs a shared distinctive term between the message and a session's focus. Review mails missed both.
- **Releasing muster ships everything merged since the last tag.** Check `git log <last-tag>..<ref>` and its migrations before applying; another session's unreleased PR rides along.
- **A replay of 14 days of transcripts is the gate for hook changes.** The first hook rewrite passed its own tests yet would have dropped 200 PR creates the old hook recorded (heredoc bodies). The replay script lives only in a scratchpad; rebuild it from the PR description of dotfiles #2431 if needed.

- **Locally the leak gate cannot see denied identifiers.** `tests/leakscan.py` needs `MUSTER_LEAKSCAN_KEY` (a CI secret) to check its hashed denylist; without it, it skips that check and reports `0 findings`. Only CI's `leak gate` job is a measurement. Before landing a doc in this public repo, grep it for private repo and host names yourself.
- **Each package's PG test DB is shared across worktrees.** Its name (e.g. `ms_api_<hash>`) comes from the package path, not the checkout, and an interrupted run leaves rows behind. Read it with `set muster.scope='all'`, because row-level security hides every row otherwise. A local-only red that CI does not show is usually a leftover row: compare it against a fresh DB, or find the row, before calling `main` red.
- **A push to `trunk` can be rejected when another session pushes first.** `muster-ops release --apply` stages the deploy commit on a branch, and `verify` will then poll forever for a rollout that never started. Cherry-pick the commit onto `origin/trunk` in a worktree and push that.
- **Forcing a never-delivered timeout:** call `plugin_message_route` with no subject (it routes to THIS session), then keep the session busy with back-to-back foreground commands until `timeout_at`. A paste waits for 30 s of idle, and the sweep fires within seconds of the deadline.

## How to verify
- Never-delivered route (live): `select r.state, r.error, q.state, q.claimed_at from session_routes r join send_queue q on q.id=r.send_queue_id where r.route_id='rt_45ec08314d4fee5031edbc55';` should return `timed_out`, the `never delivered: …` error, `cancelled`, and a NULL `claimed_at`. The chief's `chat_messages` should hold the hand-back containing `it was never delivered`.
- Tests: `go test -race -run 'TestATimedOutRoute|TestARouteClaimedBetween|TestTimedOutUndelivered' ./internal/api/ ./internal/plugins/` with `MUSTER_TEST_DATABASE_URL` exported.
- MCP door, with the chief's token (never print it): `tools/list` contains `"name":"muster_help"`, and `tools/call muster_help {"tool":"route_to_session"}` contains `task_id`.
- PR-author routing: a review mail for a PR created with `gh pr create --repo …` gets a `session_routes` row whose target is the author session in `session_prs`.
