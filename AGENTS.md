# AGENTS.md

Guidance for coding agents working in this repository, following the
[AGENTS.md](https://agents.md) convention. It is a contributor document like
[CONTRIBUTING.md](CONTRIBUTING.md), and the same rules apply to an agent's
change as to anyone's. This file holds links, not facts: each one goes to the
place that owns its subject, so it cannot go stale behind the code.

## Installing or running muster

- **Install it, with a person**: [INSTALL.md](INSTALL.md), written for an
  agent to execute. Download a release's own copy as a file (its top has the
  one prompt) and follow it step by step.
- **Try it on one machine**: [docs/quickstart-local.md](docs/quickstart-local.md)
  (Docker Compose, the one-line `install.sh`).
- **Kubernetes**: [docs/kubernetes.md](docs/kubernetes.md) (the Helm chart).
- **Optional features** and what each needs: [docs/features.md](docs/features.md).
  Every setting: [docs/configuration.md](docs/configuration.md).
- **Is it working?** `muster doctor` ([docs/cli.md](docs/cli.md#muster-doctor)).
- **Any command's flags and defaults**: its own help (`muster --help`,
  `muster <verb> --help`, `muster-mirror -h`), which is generated from the
  code and wins over any document that disagrees. The server is configured by
  environment variables only: [docs/configuration.md](docs/configuration.md).

## Working on muster

- [CONTRIBUTING.md](CONTRIBUTING.md) is the authority on building and testing
  (including why a green `go test ./...` without a database means nothing);
  [docs/development.md](docs/development.md) runs muster from source.
- **CI is where a change is validated.** `.github/workflows/ci.yml` runs every
  job on each pull request. Everything it runs can also run locally
  (CONTRIBUTING.md lists it); while iterating, prefer the tests your change
  touches (a package, a single test, a single script), and treat CI's run on
  the pull request as the full check: read its result there.
- **This repository is public.** Run `make leakscan` before you push: no
  credentials, private hostnames or addresses, personal names or dates in code,
  commits or pull request text ([CONTRIBUTING.md](CONTRIBUTING.md#the-leak-gate)).
- **Security problems** are reported privately: [SECURITY.md](SECURITY.md).
