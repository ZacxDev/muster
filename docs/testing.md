# Testing

[CONTRIBUTING.md](../CONTRIBUTING.md) has the short version. This page covers
what each suite is, and what a green result does and does not mean.

## The Go suite

```sh
make test-db                     # throwaway Postgres on 127.0.0.1:55432, template migrated
export MUSTER_TEST_DATABASE_URL='postgres://muster:muster@127.0.0.1:55432/muster_test?sslmode=disable'
export MUSTER_TEST_REQUIRE_DB=1
go test -race ./...              # or: make test (sets both for you)
make test-db-down
```

🔴 **Without `MUSTER_TEST_REQUIRE_DB=1`, a bare `go test ./...` on a machine with
no Postgres passes without having run any of the store logic.** Every
database-backed test calls `t.Skipf`, `go test` exits 0, and the output looks
exactly like a run that tested everything. That variable turns a missing database
into a hard failure that names itself. CI sets it; set it locally too.

`MUSTER_TEST_PG_PORT=55433 make test-db` gives a second checkout its own database.

**No Docker?** `scripts/with-test-pg.sh` does the same job with the dev shell's
own Postgres: it `initdb`s a throwaway cluster in a temp directory, starts it on
a free loopback port, creates and migrates `muster_test`, exports
`MUSTER_TEST_DATABASE_URL`, `MUSTER_TEST_PG_PORT` and `MUSTER_TEST_REQUIRE_DB=1`,
runs your command, and stops Postgres and deletes the directory on exit —
failure and SIGTERM/SIGINT included (SIGKILL cannot be trapped). On a signal
it stops the command's whole process group, grandchildren included. It exits
with the command's own code. TCP only (no Unix socket), so a long `$TMPDIR` is
fine. It cannot run as root, because `initdb` refuses to.

```sh
nix develop -c scripts/with-test-pg.sh go test -race ./...
nix develop -c python3 tests/with_test_pg_test.py    # the helper's own tests
```

**File attachments' object store** (`internal/uploads`, minio-go) is faked in
process by `internal/uploads/uploadstest` for the service and route tests, and
its timeouts are tested against a small HTTP fake. Neither checks a signature or
a multipart upload, so `TestAgainstARealS3` runs the client against a real
S3-compatible server, behind the `s3live` build tag. Built with the tag it fails, rather than skips, when no server is named:

```sh
MUSTER_TEST_S3_ENDPOINT=http://127.0.0.1:9000 MUSTER_TEST_S3_BUCKET=muster-test \
MUSTER_TEST_S3_ACCESS_KEY=… MUSTER_TEST_S3_SECRET_KEY=… \
  go test -tags s3live -run TestAgainstARealS3 -v ./internal/uploads/
```

The bucket must exist. The test also checks that a wrong secret is refused by
the server. CI does not run it.

`internal/dbtest` gives every Postgres-backed test **package** its own database,
created with `CREATE DATABASE … TEMPLATE` from the one your DSN names. So a
package inherits an already-migrated schema instead of rebuilding it, and nothing
another package writes is visible to it.

The creates are serialised on one advisory lock. If that lock is unavailable, the
serialisation degrades rather than fails: an optimisation that can make the gate
fail is worse than none. Read the package comment in `internal/dbtest/dbtest.go`
before touching it. The mechanism looks over-built, but each part of it is there
for a failure that actually happened.

## The gates

| target | what it checks |
|---|---|
| `go vet ./...` | static checks |
| `make test` | the Go suite, with the database required |
| `make leakscan` | The public-repo leak gate. Its self-test runs first, so it can be seen to refuse. |
| `make css-check` | The committed `web/static/app.css` is exactly what the views produce, and each Tailwind content glob still matches something. |
| `make verb-ledger` | The **built** CLI exposes exactly its specified verbs. |
| `make check` | All of the above. |
| `make e2e` | The Playwright suite (see below). |

Two documentation gates run inside `go test ./internal/modulegate/`:

- `TestEveryEnvVarTheCodeReadsIsDocumentedAndViceVersa` checks that the
  variables the code reads (collected by parsing the source) match the table in
  [configuration.md](configuration.md), in both directions.
- `TestEveryRelativeDocLinkResolves` checks that every relative link and image in
  `README.md`, `CONTRIBUTING.md` and `docs/` points at a file that exists, and
  that every `#anchor` points at a heading.

## The browser suite (`e2e/`)

The browser suite is Playwright on Chromium. It runs against locally built
servers and a seeded Postgres database whose name must end in `_e2e`, because the
seed truncates it. Some specs also start a real GoTrue in Docker
(`e2e/gotrue/compose.yml`). Mirrored-session specs run a real `muster-mirror`
over a real tunnel. Only tmux is faked: a build with the `e2emirror` tag. The
default viewport is a phone, 390×844.

```sh
cd e2e && npm ci && npx playwright install chromium && cd ..
MUSTER_E2E_DATABASE_URL='postgres://muster:muster@127.0.0.1:55432/muster_e2e?sslmode=disable' make e2e
```

Create the `_e2e` database first, for example with
`docker compose -f docker-compose.test.yml exec postgres createdb -U muster muster_e2e`.
`MUSTER_E2E_PORT` moves the whole port block (default 18181), so two checkouts
can run side by side.

**Layout shift** is gated in the browser suite too, by `e2e/tests/cls.spec.ts`,
at 390×844 (touch) and 1280×800. What it measures, exactly:

- every page's **load**: the page is opened, its htmx loads are let land, and
  the summed `layout-shift` entries (input-caused ones excluded) must stay under
  0.01. Each page must also show the content it is for, so a page that drew
  nothing cannot pass as still. The page list comes from
  `internal/api/testdata/routes.golden`: a new `GET` route fails the spec until
  it is measured there or named as not a page;
- the chief panel opened, and restored open on a load;
- **after a load**, started by script so nothing counts as input: the
  sidebar's list refreshing, and a boosted navigation and the loads it starts,
  with a positive control (an unprovoked shift must read above 0). ⚠ Chromium's
  layout-shift entries leave out a node REPLACED or MOVED in the DOM, and a
  sidebar refresh replaces its rows: its own rows read 0 however they change,
  so the measurement is of what stays on the page around it;
- a long list of mirrored windows (the sidebar's), arriving, refreshing live,
  restored by Back and left by a boosted navigation;
- the onboarding page `/agents` draws when home has nothing to open, on an
  empty database;
- a tapped tool-group expand is excluded as input, and the same expand by
  script is counted (the spec's own check of its exclusion).

`e2e/verdict.mjs` fails the run on any skip, or on fewer passes than its floor.
In CI the suite runs as parallel legs, each with its own Postgres
(`e2e/legs.json`). The `e2e` check aggregates them, and refuses a spec that ran
in no leg or in two.

**Signing in.** Password sign-in is throttled to 10 attempts a minute per
server, so a spec that fills the sign-in form for every test gets a 429 on a
fast runner and passes on a slow one. Specs sign in with `signIn(page, origin)`
from `e2e/lib/signin.ts`, which drives the form once per server and worker and
reuses that session afterwards. It checks the session is still accepted before
using it, so a restarted server or worker signs in again. Pass `{ fresh: true }`
when a test needs a session of its own. Every server the suite starts counts the
password sign-ins it handled, from its own log. A test fails once any server has
handled more than half of `loginRateBurst` (read from `internal/api/login.go`),
however the form was filled. The shared server is checked in global teardown.

## Screenshots

The images in [`docs/img/`](img) are generated, not hand-made:

```sh
MUSTER_E2E_DATABASE_URL='…/muster_e2e?sslmode=disable' \
  nix-shell -p ffmpeg pngquant --run 'make screenshots'
```

This runs `e2e/docshots/docs.spec.ts`. It uses only the seeded fixture board and
two fake mirrored hosts (`workstation` and `laptop`), whose windows and Claude
Code transcripts the spec writes, so no real data appears in it. It is opt-in and
never part of `make e2e`. It drives:

- a gotrue-mode server against the real GoTrue image;
- Chromium's virtual passkey authenticator;
- Chromium's fake microphone and a fake speech-to-text service.

Phone shots are 390×844 at 2×; `desktop.png` is 1280×800 at 2×. The spec also
writes the frames of the README's two animations, each to its own directory with
an ffmpeg concat list (each frame's hold), and the make target assembles each GIF
from them:

- `phone-flow.gif` (`e2e/.state/phone-flow/`): switch to a session by search,
  dictate a reply, send it, and watch the session answer;
- `ask-flow.gif` (`e2e/.state/ask-flow/`): a session asks a question (Claude
  Code's `AskUserQuestion`, registered by the real `muster-mirror ask` hook), it
  shows in the sidebar and on the Asks page, the answer is tapped in the session
  and typed into the fake host's picker, and the session carries on.

Then it compresses each PNG with pngquant when it is installed, and fails if a
PNG is over 200 KB or a GIF over 2 MB. `npm run capture` in `e2e/` is a separate generator, for the
PWA manifest's install-sheet screenshots.

## What green does not prove

- **`nix build` tests only the packages each derivation installs.** That is
  `cmd/muster` and `internal/taskstatus` for the CLI, and `cmd/muster-migrate` and
  `internal/db` for the migrator. A `go test ./...` inside the derivation would
  fail, not just be weaker. Several packages assert properties of the
  repository: they shell out to `git`, or read `Dockerfile`,
  `tailwind.config.js` and similar files, and the derivation's `src` is an
  allowlisted subset that carries none of them. `flake.nix`'s `goCheckPhase`
  states the full list. **The CI `test` job is the only fully green suite.**
- 🔴 **A green `nix flake check` is not evidence that a check ran.** Nix skips
  anything already in the store, and can print "running 0 flake checks / all
  checks passed!". It answers "does every output evaluate". So CI's `nix` job runs
  `nix build` on the named outputs, and keeps `flake check` as a separate step.
- **The live-environment controls are not in CI.** `make test-liveenv` needs a
  real coding-agent session and a reachable agent runtime. It is the only thing
  that proves a real runtime accepts muster's chat protocol.
