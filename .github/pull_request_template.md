## What this changes

<!-- What the change does and why. Link the issue it addresses, if there is one. -->

## How it was tested

<!--
What you ran, and what you could not run (see CONTRIBUTING.md, "Pull requests").
For a regression test, say which commit or mutation you saw it fail against.
If the Postgres-backed tests did not run, say so.
-->

## Checklist

- [ ] Tests cover the change, and a new test was seen to fail before the fix.
- [ ] `make leakscan` passes (no credentials, private hostnames, addresses or dates).
- [ ] Docs are updated in this pull request: a new environment variable has a row in `docs/configuration.md`.
- [ ] The branch is up to date with `main`.
