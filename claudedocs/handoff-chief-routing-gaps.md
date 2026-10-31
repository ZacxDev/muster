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
- **Shipped and live:**
  - muster 0.118.0 (tag v0.118.0, commit 98ff27db): `muster_help` plus `current_definition` on every refused MCP call, on both doors (#376).
  - The hint extractor reads `owner/repo#N` as `#N` (#367); it shipped in an earlier release by another session.
  - dotfiles #2431 (ec263e60): the PR-authorship hook reports `gh -R/--repo` creates and a first-line PR URL. Shipped to both hosts via `ship.sh`; the deployed hook's hash matches main.
  - dotfiles #2441 (fc320674): the host muster CLI and muster-mirror are at 98ff27d. Both hosts' mirrors are confirmed in their own unit.
- **Verified live:**
  - The MCP door, called with the chief's token: `tools/list` offers `muster_help`; `muster_help` for `route_to_session` lists `task_id`; a refused call carries `current_definition`.
  - After a chief pod restart, the chief's own log shows it loaded `muster_help` and received the current definition.
- **Closed unmerged:** #375. Turning tool search off fixed staleness but cost about 15k prompt tokens per request; the operator chose the server-side fix instead.
- **Routing outcome:** the unrouted items were already handled by their sessions (the HQ statuses showed it). On the operator's instruction they were NOT routed, to avoid confusing sessions. The chief's own open asks about them could not be withdrawn by the chief (a classifier stopped its reply); the operator dismisses them in Attention.

## Open investigations — live diagnosis state
### A routed message never reaches a busy session
- as-of: 2026-10-31
- **Symptom + exact repro:** route a plugin message to a session that stays busy. The paste waits for 30 s of idle, never gets it, and the route times out after `RouteTimeout` (30 min) as `no answer before the deadline`. Nobody is told that it was never delivered.
- **Observed (with values):** two manual routes ended with send_queue rows `state=cancelled detail=cancelled claimed_at=NULL` at the deadline, and session_routes `state=timed_out`. One other route ended `failed` with "May have been sent: muster handed it to the host, which did not confirm it".
- **Ruled out:** the route tool refusing the target. via: command — `plugin_message_route` returned `state=routed` for each.
- **Leading hypothesis:** by design, the send queue never interrupts a working session. The gap is the silent hand-back: the hand-back does not say "never delivered (session busy the whole window)" as opposed to "delivered, not answered".
- **Next probe:** `select r.route_id, r.state, q.state, q.claimed_at from session_routes r join send_queue q on q.id=r.send_queue_id where r.state='timed_out' order by r.id desc limit 20;` — count how many timed-out routes were never claimed.

## Next steps (ranked)
1. Distinguish a never-delivered route from an unanswered one in the hand-back text and in Attention (muster: internal/api/session_routes.go handBackRoute, the send queue's claim state). forcing: none
2. Rotate the chief's MCP token (`X-Muster-Token` in the chief pod's Claude config) and the ccd hook secret: both were printed into this session's local transcript. forcing: security — exposed in a local transcript, nowhere else
3. Quiet pushes (`git -C $W push -q`) are still not reported by the PR-authorship hook. The operator dropped that path; only revisit if review mails keep missing because of it. forcing: none

## Defects (batched)
- The chief's model was stopped by a safety classifier on several routing and withdrawal replies (no text, "stopped before it ran"). Not investigated.

## Gotchas / decisions / dead-ends
- **The cause of the stale tool schema, measured:** Claude Code saves a deferred MCP tool's schema in the transcript (`deferred_tools_record`) and replays it on every `--resume`. A pod restart does NOT refresh it, a ToolSearch re-fetch returns the saved copy, and `alwaysLoad` does not survive a resume either. The client does not enforce the stale schema: a call with an argument absent from it still succeeds.
- **What fixes staleness:** `current_definition` on a refused call. In a real CLI repro the model recovered on its first refusal. A tool-set version stamp in descriptions was ignored by the model, so it was dropped.
- **Two separate routing paths, two separate misses.** The PR-author route needs a `session_prs` row, written by the dotfiles hook; the chief's hint needs a shared distinctive term between the message and a session's focus. Review mails missed both.
- **Releasing muster ships everything merged since the last tag.** Check `git log <last-tag>..<ref>` and its migrations before applying; another session's unreleased PR rides along.
- **A replay of 14 days of transcripts is the gate for hook changes.** The first hook rewrite passed its own tests yet would have dropped 200 PR creates the old hook recorded (heredoc bodies). The replay script lives only in a scratchpad; rebuild it from the PR description of dotfiles #2431 if needed.

## How to verify
- MCP door, with the chief's token (never print it): `tools/list` contains `"name":"muster_help"`; `tools/call muster_help {"tool":"route_to_session"}` contains `task_id`; a refused `route_to_session {"message_id":<a no-reply message>,"subject":"mirror:none"}` has `"isError":true` and `current_definition`.
- Hook: load `~/.claude/hooks/pr-session-bind-hook.py` (`readlink -f`), call `reports_of` on a `gh -R owner/repo pr create …` payload whose stdout is the PR URL, and expect one `gh-pr-create` report. Check on both hosts that the deployed file's hash matches `git show origin/main:scripts/claude-hooks/pr-session-bind-hook.py`.
