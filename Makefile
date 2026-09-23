# muster developer tasks.
#
# Every target here is something CI also runs, spelled the same way. A gate that
# exists only in CI is a gate contributors discover by having it go red.

SHELL := /usr/bin/env bash

COMPOSE ?= docker compose
COMPOSE_FILE := docker-compose.test.yml

# The DSN docker-compose.test.yml serves. It is stated ONCE, here, and
# `test-db` prints it — two copies is how a recipe drifts into naming the wrong
# database, which is the specific failure that file's header is about.
TEST_DSN := postgres://muster:muster@127.0.0.1:55432/muster_test?sslmode=disable

.PHONY: build vet test test-db test-db-down leakscan css css-check check help

# The stylesheet the UI serves, and the classes that prove each Tailwind content
# entry is still matching something. See css-check.
CSS_OUT := web/static/app.css
CSS_IN := web/css/input.css
TAILWIND ?= npx --yes tailwindcss@3

# 🔴 EACH ENTRY IS A POSITIVE CONTROL FOR ONE CONTENT GLOB, NOT DECORATION.
# Every class below is written in exactly one place in the tree and is reachable
# ONLY through `./internal/ui/**/*.go`. Measured with that glob deliberately
# pointed at a non-existent directory: Tailwind exits 0, emits a VALID 5,594-byte
# stylesheet instead of the real 38,274-byte one, and every class here drops to
# zero occurrences. That is the whole hazard — there is no error, no warning in
# the exit code, and nothing on a developer's machine looks wrong.
#
# ⚠ PLAIN ALPHANUMERIC CLASSES ONLY, AND THAT IS A HARNESS FIX RATHER THAN A
# TASTE. A responsive or opacity-modified class (`lg:pl-72`, `bg-rose-500/95`)
# is escaped in the CSS as `.lg\:pl-72`, and carrying that backslash through
# Make into a shell into grep's pattern language made grep warn about a "stray \
# before :" and match nothing — a MISSING verdict for a class that was present.
# That is the instrument failing, reported as a finding about the code.
# Each of these is written in a different moved view, so the set spans the glob
# rather than sampling one file.
#
# 🔴 THE SIXTH CLASS COVERS A DIFFERENT CONTENT ENTRY, NOT A SIXTH VIEW.
# `bg-emerald-600` is written ONLY in internal/api/login.go — the sign-in page,
# the one document this app emits from outside internal/ui — so it is the
# positive control for that entry. Without it, deleting `./internal/api/login.go`
# from tailwind.config.js would ship a login page with an unstyled submit button
# and every check here would stay green.
CSS_REQUIRED_CLASSES := bg-emerald-500 text-indigo-300 bg-rose-500 bg-sky-500 text-amber-200 bg-emerald-600

help:
	@echo "muster:"
	@echo "  make build        go build ./..."
	@echo "  make vet          go vet ./..."
	@echo "  make test-db      start the throwaway Postgres, migrate it, print the exports"
	@echo "  make test         the Go suite, with the database REQUIRED (not skipped)"
	@echo "  make leakscan     the leak gate, self-test first"
	@echo "  make css          rebuild $(CSS_OUT) from the Go views"
	@echo "  make css-check    prove the committed stylesheet matches the views"
	@echo "  make check        vet + test + leakscan + css-check"
	@echo "  make test-db-down stop and remove the throwaway Postgres"

build:
	go build ./...

vet:
	go vet ./...

# 🔴 `MUSTER_TEST_REQUIRE_DB=1` IS THE POINT OF THIS TARGET, NOT DECORATION.
# Bare `go test ./...` with no database SKIPS every Postgres-backed test and
# reports GREEN — a verdict about a suite that executed none of the store logic,
# indistinguishable in the output from one that executed all of it. This target
# refuses to produce that verdict: without a database it FAILS, naming the
# variable and the way to start one.
#
# ⚠ IT DOES NOT START THE DATABASE FOR YOU, and that is deliberate. A test
# target that silently provisions infrastructure hides how long it takes and
# what it costs, and it makes `make test` unusable against a Postgres you
# already have. Run `make test-db` once; it prints what to export.
test:
	MUSTER_TEST_DATABASE_URL="$${MUSTER_TEST_DATABASE_URL:-$(TEST_DSN)}" \
	MUSTER_TEST_REQUIRE_DB=1 \
	go test -race -cover ./...

# Start the throwaway Postgres, wait for it, and migrate the TEMPLATE once.
#
# 🔴 THE MIGRATE STEP IS NOT OPTIONAL BOOKKEEPING. internal/dbtest gives each
# test package its own database by `CREATE DATABASE … TEMPLATE`, copying an
# already-migrated schema. Skip this and nothing breaks — but every package
# bootstraps the schema itself, which is the slow path this design exists to
# avoid.
test-db:
	$(COMPOSE) -f $(COMPOSE_FILE) up -d --wait
	DATABASE_URL="$(TEST_DSN)" go run ./cmd/muster-migrate
	@echo
	@echo "Postgres is up and the template is migrated. Export these:"
	@echo
	@echo "    export MUSTER_TEST_DATABASE_URL='$(TEST_DSN)'"
	@echo "    export MUSTER_TEST_REQUIRE_DB=1"
	@echo
	@echo "The second one turns a missing database from a SKIP into a FAILURE."
	@echo "Without it a suite that ran nothing still reports green."

test-db-down:
	$(COMPOSE) -f $(COMPOSE_FILE) down -v

# 🔴 THE SELF-TEST RUNS FIRST, AND THE `&&` IS LOAD-BEARING. A scanner that
# cannot go red reports zero findings exactly the way a clean tree does. The
# self-test proves it can refuse a realistic sensitive string, produce a
# non-zero count, and NOT refuse legitimate content — and only then is the scan
# below a measurement rather than a claim.
leakscan:
	python3 tests/leakscan.py --self-test && python3 tests/leakscan.py

# Rebuild the stylesheet from the Go views.
css:
	mkdir -p $(dir $(CSS_OUT))
	$(TAILWIND) -i $(CSS_IN) -o $(CSS_OUT) --minify

# 🔴 TAILWIND CANNOT FAIL THE WAY YOU NEED IT TO, SO THIS TARGET FAILS FOR IT.
#
# Tailwind emits rules only for classes it FINDS. A content entry that matches
# nothing is not an error and not a non-zero exit — it silently produces a
# smaller, perfectly valid stylesheet. The page then renders unstyled wherever
# that stylesheet is actually served, which is typically a container and not the
# machine the build ran on, so the first report is "it looks broken in prod".
#
# Two separate claims are checked here, and they fail for different reasons:
#
#   1. THE BUILD STILL REACHES THE VIEWS. Rebuild into a scratch file and assert
#      each class in CSS_REQUIRED_CLASSES is present. A zero here means a content
#      glob stopped matching — the silent case above.
#   2. THE COMMITTED STYLESHEET IS THE ONE THOSE VIEWS PRODUCE. Compare the
#      scratch build against the committed file byte for byte. $(CSS_OUT) is
#      COMMITTED, not gitignored, precisely because a later `go:embed` will make
#      it a build input — and this repository's .gitignore says in its first line
#      that nothing `go build` needs may be ignored, because the project muster
#      came from ignored exactly this file and broke `go build` on a fresh clone.
#      Committing it without this comparison would just move the failure from
#      "missing" to "stale", so both halves ship together.
#
# ⚠ `cmp`, NOT a parsed diff summary. Parsing another tool's output makes its
# FORMAT a dependency; cmp answers the only question here with an exit code.
css-check:
	@set -e; \
	tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	$(TAILWIND) -i $(CSS_IN) -o "$$tmp/app.css" --minify >/dev/null 2>&1; \
	test -s "$$tmp/app.css" || { echo "css-check: the build produced an EMPTY stylesheet"; exit 1; }; \
	for c in $(CSS_REQUIRED_CLASSES); do \
	  grep -qF -- "$$c" "$$tmp/app.css" || { \
	    echo "css-check: '$$c' is MISSING from the built stylesheet."; \
	    echo "  A Tailwind content entry has stopped matching. Tailwind does not"; \
	    echo "  error on this — it emits a smaller valid file and exits 0."; \
	    echo "  Check the content globs in tailwind.config.js."; \
	    exit 1; }; \
	done; \
	test -f $(CSS_OUT) || { echo "css-check: $(CSS_OUT) is not committed — run 'make css'"; exit 1; }; \
	cmp -s "$$tmp/app.css" $(CSS_OUT) || { \
	  echo "css-check: $(CSS_OUT) is STALE — the views have changed since it was built."; \
	  echo "  Run 'make css' and commit the result."; \
	  exit 1; }; \
	echo "css-check: $$(wc -c < $(CSS_OUT)) bytes, all $(words $(CSS_REQUIRED_CLASSES)) control classes present, committed copy matches"

check: vet test leakscan css-check
