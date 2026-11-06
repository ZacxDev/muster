# AGENTS.md

For coding agents working on muster.

## 🔴 CI runs on CI, not on this workstation

`.github/workflows/ci.yml` (build, test, leakscan, nix, css, e2e, release-dry-run,
images) is where a change is validated. Do **not** run the whole local mirror to
"reproduce CI" — the box is shared by every agent and the operator's desktop, and a
whole-suite run spins the disks and slows both.

Whole-suite invocations are refused by the local agent tooling.

**A change-scoped subset is the local loop** — `make vet`, `go test ./internal/<pkg>/`,
`make css-check`, `go test ./... -run <Test>`. The full suite is CI's:
`make check` / `make test` / `make e2e` are the CI mirrors, not the iteration loop.
`go test ./...` is deliberately allowed — that is a reason it CAN run, never a licence to
run the whole suite casually.

