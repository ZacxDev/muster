# muster

To muster is to assemble a force for duty; a muster roll is the register of who
is present and what they are assigned.

muster is a work queue for coding agents: **tasks** that people and agents both
read and write, **dispatch** that provisions an agent against a task, and a
**chief** that reports on the fleet. It is a service with an HTTP API and its own
Postgres database.

🔴 **muster serves, and it does not yet provision.** This paragraph used to read
"there is no server, no HTTP API, no CLI and no UI in this tree today", and that
was true when it was written and false for the whole of the change that carved
the domain layer in — during which `internal/api` registered 99 routes,
`internal/ui` rendered every view, and **neither linked into any binary**. `go
list -deps ./cmd/...` resolved four packages of this module and `internal/api`
was in none of them: sixteen packages compiled, were tested, and no process
could execute a line of them. Two gates now make that impossible to repeat —
`TestTheHTTPLayerLinksIntoABinary` pins the link graph and
`TestTheServerListensAndServesHealth` binds a real port and drives a real
request. Neither is sufficient alone; see `internal/modulegate`.

What you get today: the task board, the agent surfaces, the runbooks, the
privileges, the machine API, a session-authenticated web UI, and a CLI. What you
do **not** get: agent provisioning. Nothing in this module implements
`api.Provisioner`, so every agent-control route is registered and refuses, and
the server says so on every boot. The four seams that state is made of — what,
why, what closes each, and who checks — are in `cmd/muster-server/doc_seams.go`.

## Quickstart

You need Go (the version in `go.mod`) and Docker. Nix users get both from the
flake.

```sh
git clone https://github.com/ZacxDev/muster && cd muster
go build ./...          # works on a fresh clone: three binaries, no codegen
make test-db            # starts a throwaway Postgres, migrates it, prints 2 exports
make test               # the Go suite, with the database REQUIRED
make leakscan           # the leak gate; its self-test runs first
make css-check          # the stylesheet matches the views, and is not stale
```

There **is** an asset pipeline — a Tailwind build — but its output
(`web/static/app.css`) is **committed**, because a later `go:embed` makes it a
build input and `.gitignore`'s first line says nothing `go build` needs may be
ignored. `make css-check` is what stops a committed stylesheet going stale, and
it is the check the container image runs too. It matters more than it looks:
Tailwind does **not** error on a content glob that matches nothing — it emits a
smaller, perfectly valid file and exits 0.

To run the server against that Postgres:

```sh
export DATABASE_URL="$MUSTER_TEST_DATABASE_URL"
export MUSTER_UI_PASSWORD='something at least as long as the refusal demands'
export MUSTER_STANDALONE=1     # "this deployment has no permission router"
go run ./cmd/muster-server     # then http://127.0.0.1:8105
```

🔴 `MUSTER_STANDALONE=1` is not a convenience flag. Without a permission router
the server has no way to ask whether a session transcript exists, so every
transcript link would render "no transcript recorded" — which is TRUE with no
router and a confident falsehood if you merely *forgot* to configure one. Those
two situations are the same observable, so the server refuses to report ready
until you say which you are in. `/readyz` answers 503 and names the field; every
`MUSTER_*` name is spelled once, in `cmd/muster-server/config.go`.

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

- **There is no agent provisioning.** `go build ./...` produces **three**
  binaries — `muster-server` (the service), `muster` (the machine/agent CLI) and
  `muster-migrate` (the schema step) — and the `Dockerfile` ships all three.
  What the server cannot do is create an agent: nothing here implements
  `api.Provisioner`, so dispatch, start, stop, destroy, logs and chat are
  registered routes that refuse. This line previously read "there is no server …
  produces exactly one binary"; both halves were falsified by the carve, and a
  reader who saw a non-empty `RegisterRoutes` and concluded otherwise was reading
  correctly and concluding wrongly.
- **The singleton background loops are ungated, so deploy one replica.**
  `internal/db` carries no lease, so the daily prune and the idle-task reap run
  unguarded. Both are idempotent, so a second replica costs duplicated work
  rather than duplicated effect — but it is not free. See
  `cmd/muster-server/doc_seams.go` entry 4.
- **The `e2e` and `e2e-unit` CI jobs are stubs.** They run nothing and say so in
  their own output. They fail the moment a spec file appears, so the first real
  e2e test cannot land against a green tick that never collected it.
- **`nix build` runs the pure-Go tests only.** The sandbox has no database, so
  every Postgres-backed test skips there. A green `nix flake check` is not a
  green suite; the CI `test` job is.
- **The leak gate does not scan itself — it AUDITS itself, which is narrower.**
  `tests/leakscan.py` has to contain realistic sensitive strings (they are its
  negative controls), so it is exempt from the scan and printed as a named skip.
  That exemption was unaudited and something hid in it: a `private-ip` control
  carried the LAN address of a real single-node cluster from this file's first
  commit out into public history, past a hand review, because it looked exactly
  like its synthetic neighbours. Every file in `SKIP_FILES` is now audited by
  VALUE — every address, hostname, registry reference, credential, denied name
  and dated stamp it spells must be objectively unroutable, objectively
  synthetic, or declared in `EXEMPT_FIXTURE_VALUES` with the reason it is safe,
  and anything new fails closed. What that still accepts is written out in the
  module beside the table: a real value somebody declares anyway, prose (a real
  incident narrated in a fixture spells no value an audit can extract — one did),
  IPv6 in any form, and the fixtures of every file the gate actually scans.
- **No agent runtime.** The provisioner can create an instance; nothing here
  talks to it. Sending a message, streaming its tokens, servicing its tool calls
  — and therefore the question of whether an agent that is not one specific
  vendor's container can be driven at all — are not answered here. That absence
  is deliberate: putting the conversation on the provisioner interface is what
  made the original un-implementable by anything else.
- **The provisioner has two drivers and neither has run against a real
  cluster from this tree.** The Kubernetes driver's tests use a fake clientset,
  which structurally cannot see scheduling, image pulls, volume attachment,
  admission, or the exec stream. A green suite there means the manifests are
  what the code says they are, not that a pod came up.
- **The Kubernetes driver restricts no egress.** No NetworkPolicy is rendered,
  and there is no capability flag to read that off — it is stated here and in
  the driver's own doc comment. Restricting an agent's egress by DNS name is the
  control that addresses exfiltration by a prompt-injected model, and muster
  does not implement it. `Capabilities.Policy` being true does not cover it.

## The provisioner

`internal/provision` is the seam between "an agent should exist, configured like
this" and whatever runs it. It imports nothing outside the standard library, so
wiring the no-op driver costs none of a cluster client's dependency tree.

Four rules define it. Each rule's main clause is pinned by a test in
`provisiontest.RunContract` rather than by prose — with **one named exception**:
rule 2's `ErrNotManaged` clause, which needs a shared backend to be expressible
at all and is therefore pinned in the Kubernetes driver's own ownership tests
instead. `RunContract` has 21 cases and none of them mentions `ErrNotManaged`.
The rules are stated in full in the package doc; in short:

1. **A driver that cannot see its backend returns an error, never an empty
   set.** A caller reads a `List` error as "keep the stored status" and an empty
   `List` as "nothing is running".
2. **`Destroy` returns `nil` only if the instance was removed or was already
   absent.** A bare `return nil` satisfies the compiler and means nothing. A
   FOREIGN object holding the instance's name is neither case: it is
   `ErrNotManaged`, which deliberately does **not** satisfy
   `errors.Is(err, ErrNotFound)` — otherwise `if err != nil && !errors.Is(err,
   ErrNotFound)`, the idiom this rule invites, discards the refusal in silence.
   That refusal also means **nothing was removed**: a driver decides ownership
   before it deletes, so "removed the instance, then reported a permanent
   failure" — a state no retry loop can leave — cannot happen.
3. **`Create` is idempotent; a divergent spec is an error**, not a silent
   overwrite. Note the one asymmetry: `Create`'s refusal over a foreign
   co-named object keeps `ErrDivergentSpec` and does **not** report
   `ErrNotManaged`.
4. **`Capabilities` is useless unless callers branch on it.** Each capability's
   refusal is spelled in exactly one place, and **`capabilities.go`'s doc
   comment is the authority on which one** — deliberately not copied here.
   There used to be a copy, it said "`CheckSpec` and `Grant` … are the only
   places", and it was wrong about `CheckScale` and `Exec`; a partial
   enumeration of the guards reads exactly like a complete one. A policy a
   driver cannot interpret is REFUSED — an interface reporting "granted" for a
   policy nobody applied is worse than no policy feature, because it reads as
   coverage.

`internal/provision/provisiontest` is the contract suite every driver must pass.
It requires a working driver, a **blind** one and a **restricted** one, and it
does not skip: without the blind driver the first rule is unfalsifiable, and
without the restricted one the capability refusals are never watched to fire.

| driver | what it is for |
|---|---|
| `provision.Noop` | records instead of provisioning; develop everything above the seam without a backend |
| `provision/k8s` | renders its own minimal manifests — Namespace, ServiceAccount, **two** Secrets (confidential env, and separately confidential files), ConfigMap, PVC, Deployment, Service — and installs no chart |

## Licence

MIT. See `LICENSE`.
