# Security policy

## Supported versions

Security fixes go into the **latest release** only. A fix ships as a new
release; older releases are not patched, so the remedy for a vulnerable install
is to upgrade.

| version | supported |
|---|---|
| the [latest release](https://github.com/ZacxDev/muster/releases/latest) | yes |
| any older release | no: upgrade to the latest |
| an unreleased `main` | best effort |

Before you report, check that the problem still happens on the latest release.

## Reporting a vulnerability

**Please do not report a security problem in a public issue, discussion or pull
request.**

Report it through GitHub's private vulnerability reporting, the one channel
for security reports: open the repository's **Security** tab and choose
**Report a vulnerability**, or go straight to
[the private report form](https://github.com/ZacxDev/muster/security/advisories/new).
Only you and the maintainers can read the report, and the fix is prepared in
that same private advisory.

A useful report says:

- the version (`muster --version`, `muster-mirror -version`, or the version the
  server's `/health` reports);
- the part affected: the server, `muster-mirror`, the `muster` CLI, a container
  image, the Compose stack, `install.sh` or the Helm chart;
- what an attacker needs (network position, a token, a signed-in account) and
  what they gain;
- the steps to reproduce it, and a proof of concept if you have one.

## What happens next

Reports are handled on a best-effort basis. You get an acknowledgement, then
the maintainers confirm the problem, prepare a fix in a private advisory and
publish it with a new release. The advisory credits you unless you ask not to
be named. Please give the fix a reasonable chance to ship before you disclose
the problem publicly.

## Scope

In scope: everything this repository builds and ships, that is the server, the
`muster-mirror` daemon, the `muster` CLI, the container images, the Compose
stack, `install.sh` and the Helm chart.

Some behaviour is by design and documented, so it is not a vulnerability on its
own. For example, an interactive mirror is a shell on that host: anyone who can
type into its Terminal view can run commands as the user `muster-mirror` runs
as. What each component trusts is in [docs/architecture.md](docs/architecture.md),
and the known limits are in [docs/limits.md](docs/limits.md). A way around one of
those documented boundaries is in scope.
