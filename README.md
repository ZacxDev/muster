# muster

To muster is to assemble a force for duty; a muster roll is the register of who
is present and what they are assigned.

muster is a work queue for coding agents: **tasks** that people and agents both
read and write, **dispatch** that provisions an agent against a task, and a
**chief** that reports on the fleet. It is a service with an HTTP API and its own
Postgres database.

🔴 **muster is a foundation, not a working system yet.** This repository
currently holds its schema, its test harness, its build and its gates —
deliberately, and in that order, because those are the things that are painful to
retrofit. There is **no server, no HTTP API, no CLI and no UI in this tree
today.** Everything below describes what you can run right now; nothing below
describes a feature you can use.

## Quickstart

You need Go (the version in `go.mod`) and Docker. Nix users get both from the
flake.

```sh
git clone https://github.com/ZacxDev/muster && cd muster
go build ./...          # works on a fresh clone — no asset pipeline, no codegen
make test-db            # starts a throwaway Postgres, migrates it, prints 2 exports
make test               # the Go suite, with the database REQUIRED
make leakscan           # the leak gate; its self-test runs first
```

`make test-db` prints two exports. **Set both.** The second one is the point:

```sh
export MUSTER_TEST_DATABASE_URL='postgres://muster:muster@127.0.0.1:55432/muster_test?sslmode=disable'
export MUSTER_TEST_REQUIRE_DB=1
```

🔴 **Without `MUSTER_TEST_REQUIRE_DB=1`, a bare `go test ./...` on a machine with
no Postgres is GREEN having executed none of the store logic.** Every
database-backed test calls `t.Skipf`, `go test` exits 0, and the output looks
exactly like a run that tested everything. That variable turns the absence of a
database into a hard failure that names itself. CI sets it. Set it locally too.

## What you can run

| you want | run |
|---|---|
| build everything | `go build ./...` |
| the static checks | `go vet ./...` |
| a throwaway Postgres, migrated | `make test-db` |
| the suite, database required | `make test` |
| the suite without a database (skips, and says so) | `go test ./...` |
| apply the schema to any database | `DATABASE_URL=… go run ./cmd/muster-migrate` |
| the leak gate | `make leakscan` |
| everything CI runs | `make check` |
| stop the throwaway Postgres | `make test-db-down` |
| a pinned dev shell (nix) | `nix develop` |
| build the migrator (nix) | `nix build .#muster-migrate` |
| build the migrator image (nix, Linux) | `nix build .#migrate-image` |

## The schema

One migration, `internal/db/migrations/0001_init.sql`, creating thirteen tables
in four groups:

- **tasks** — `notes`, `note_comments`, `note_attachments`, `task_sessions`
- **agents** — `agents`, `chat_sessions`, `chat_messages`
- **runbooks** — `runbooks`, `runbook_runs`
- **privilege** — `privilege_profiles`, `agent_privileges`, `privilege_requests`
- plus `github_connection`, a pinned singleton

`notes` is the task table. The name is historical; it is kept because every id in
it is a task number people cite, and renaming it would be a data migration that
buys a word.

Two properties worth knowing before you change it:

- 🔴 **Every `id` is `GENERATED ALWAYS AS IDENTITY`,** and
  `internal/db/migrate_test.go` asserts it for every identity column in the
  schema, not a list of named ones. Weakening a column to `BY DEFAULT` is
  invisible for months: it changes nothing about how muster writes, and only
  shows up when somebody concludes a hand-written `INSERT` importer is fine.
  Existing rows are meant to arrive by `pg_dump --data-only`, whose `COPY` path
  is exempt from the restriction.
- ⚠ **Several columns are deliberately FK-free** — `task_sessions.session_id`,
  `notes.source_session_id`, `runbook_runs.agent_id`. They name things in other
  systems, or they are audit rows that must outlive their subjects. Adding a
  foreign key to one of them is not a cleanup.

`0001` is a single statement of the schema's **end state**, not a replay of the
history it was extracted from. Later changes get their own numbered file.

## Testing

`internal/dbtest` gives every Postgres-backed test **package** its own database,
by `CREATE DATABASE … TEMPLATE` from the one your DSN names — so a package
inherits an already-migrated schema instead of rebuilding it, and nothing another
package writes is visible to it. The creates are serialised on one advisory lock,
and that serialisation **degrades rather than fails**: an optimisation that can
redden the gate is worse than no optimisation.

Read `internal/dbtest/dbtest.go`'s package comment before touching it. The
mechanism looks over-built and each part of it is there for a measured failure.

## Limits, stated plainly

- **There is no server.** No HTTP handlers, no routes, no UI, no CLI. `go build
  ./...` produces exactly one binary, `muster-migrate`, and it is a CI and
  development fixture rather than a deploy tool.
- **The `e2e` and `e2e-unit` CI jobs are stubs.** They run nothing and say so in
  their own output. They fail the moment a spec file appears, so the first real
  e2e test cannot land against a green tick that never collected it.
- **`nix build` runs the pure-Go tests only.** The sandbox has no database, so
  every Postgres-backed test skips there. A green `nix flake check` is not a
  green suite; the CI `test` job is.
- **The leak gate does not scan itself.** `tests/leakscan.py` has to contain
  realistic sensitive strings — they are its negative controls — so it is exempt
  by name and printed as a named skip. A real secret pasted into that file is
  invisible to the gate. Review changes to it by hand.
- **No provisioner, no agent runtime.** The interesting design questions — a
  pluggable provisioner with a capability declaration, and whether an agent that
  is not a specific vendor's container can be driven at all — are not answered
  here.

## Licence

MIT. See `LICENSE`.
