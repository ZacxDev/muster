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

.PHONY: build vet test test-db test-db-down leakscan check help

help:
	@echo "muster:"
	@echo "  make build        go build ./..."
	@echo "  make vet          go vet ./..."
	@echo "  make test-db      start the throwaway Postgres, migrate it, print the exports"
	@echo "  make test         the Go suite, with the database REQUIRED (not skipped)"
	@echo "  make leakscan     the leak gate, self-test first"
	@echo "  make check        vet + test + leakscan"
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

check: vet test leakscan
