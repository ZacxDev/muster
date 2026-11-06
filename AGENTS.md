# AGENTS.md

For coding agents working on muster.

## 🔴 CI runs on CI, not on this workstation

`.github/workflows/ci.yml` (build, test, leakscan, nix, css, e2e, release-dry-run,
images) is where a change is validated. Do **not** run the whole local mirror to
"reproduce CI" — the box is shared by every agent and the operator's desktop, and a
whole-suite run spins the disks and slows both.

The heavy-command gate (devrc, both runtimes) HARD-DENIES the enumerated whole-suite
forms (`make -j`/bare `make`/build-ci targets, `nix build .#checks.*`, `turbo`/`pnpm -r`,
an unscoped `scripts/tests/run-ci-suite.sh`) and names the `remote-run` form.

**A change-scoped subset is the local loop** — `make vet`, `go test ./internal/<pkg>/`,
`make css-check`, `go test ./... -run <Test>`. The full suite is CI's:
`make check` / `make test` / `make e2e` are the CI mirrors, not the iteration loop.
`go test ./...` is deliberately allowed (the per-pane cgroup caps bound it) — that is a
reason it CAN run, never a licence to run the whole suite casually.

CI-wait time is recorded: `ci-wait-report` reads `source=ci` in ClickHouse
`activity.events` (`kind=wait` agent-blocked, `kind=run` pipeline duration,
`kind=local-blocked` refused local attempts).
