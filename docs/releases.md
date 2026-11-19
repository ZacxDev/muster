# Releases: what each one contains, and how to verify it

Every `vX.Y.Z` tag publishes a GitHub Release built by
`scripts/release-build.sh` (the same script every ready pull request runs in
CI's `release dry run` job), with notes generated from the pull requests merged
since the previous tag. Every asset has a SHA-256 in `checksums.txt` and a
signed build-provenance attestation.

## What a release contains

The programs, by platform. A cell lists the formats published for it; each is
the asset `<program>_<os>_<arch>.<format>` (`muster_linux_arm64.deb`,
`muster-mirror_darwin_amd64.tar.gz`, ...).

<!-- tests/release_platforms_test.py reads this table and the next one: their
assets must be exactly the set scripts/release-build.sh builds. -->

| program | linux amd64 | linux arm64 | darwin amd64 | darwin arm64 | windows amd64 | windows arm64 |
|---|---|---|---|---|---|---|
| `muster` | tar.gz, deb, rpm | tar.gz, deb, rpm | tar.gz | tar.gz | zip | zip |
| `muster-mirror` | tar.gz, deb, rpm | tar.gz, deb, rpm | tar.gz | tar.gz | — | — |

🔴 **There is no Windows `muster-mirror`.** It shows a machine's tmux windows
and drives them, and Windows has no tmux. On Windows the `muster` CLI talks to
any muster server (`--api-url`); to show a Windows machine's sessions, run them
in WSL, which is Linux: install there as on Linux.

Every other asset, once per release:

| asset | what it is |
|---|---|
| `docker-compose.yml` | the server, its default image this release ([quickstart-local.md](quickstart-local.md)) |
| `env.example` | its settings, `MUSTER_VERSION` set to this release (save it as `.env`) |
| `install.sh` | the one-line installer, this release stamped in |
| `INSTALL.md` | the agent install guide, this release stamped in |
| `muster-<version>.tgz` | the Helm chart ([kubernetes.md](kubernetes.md)) |
| `muster.rb` | Homebrew formula: the CLI |
| `muster-mirror.rb` | Homebrew formula: muster-mirror (depends on tmux) |
| `muster.json` | Scoop manifest: the CLI on Windows |
| `checksums.txt` | the SHA-256 of every asset above |

The server itself ships as images, not as a binary:
`ghcr.io/zacxdev/muster:<version>` (linux/amd64, linux/arm64),
`ghcr.io/zacxdev/claude-code-agent:<version>` and
`ghcr.io/zacxdev/muster-stt-cohere:<version>` (dictation, linux/amd64).

## Installing by platform

- **Linux or macOS, the whole thing:** `curl -fsSL
  https://github.com/ZacxDev/muster/releases/latest/download/install.sh | sh`
  ([quickstart-local.md](quickstart-local.md)). It installs the programs from
  the archives into `~/.local/bin` (or `MUSTER_BIN`), for you alone, and never
  touches system packages unless asked.
- **The installer, with packages** (Linux, opt-in): `MUSTER_PACKAGE=deb` (or
  `rpm`) installs the release's packages into `/usr/bin` instead, for every
  user of the machine. Unless you are root it asks before it uses sudo
  (`MUSTER_PACKAGE_SUDO=yes` answers for a script; with no terminal and no
  answer it refuses). It refuses a package older than the one installed,
  since every user's `muster-mirror` runs that one copy, unless
  `MUSTER_PACKAGE_DOWNGRADE=1`. Programs left in `~/.local/bin` by an earlier
  install come first on most `PATH`s: it names them; remove them.
- **A package by hand, Debian/Ubuntu (apt):**

  ```sh
  v=X.Y.Z; a=amd64    # or arm64
  base=https://github.com/ZacxDev/muster/releases/download/v$v
  curl -fsSLO "$base/checksums.txt" -O "$base/muster_linux_$a.deb" -O "$base/muster-mirror_linux_$a.deb"
  sha256sum --check --ignore-missing checksums.txt
  sudo apt install ./muster_linux_$a.deb ./muster-mirror_linux_$a.deb   # also installs tmux, which muster-mirror recommends
  # uninstall:
  sudo apt remove muster muster-mirror
  ```

- **A package by hand, Fedora/RHEL (dnf):**

  ```sh
  v=X.Y.Z; a=amd64    # or arm64
  base=https://github.com/ZacxDev/muster/releases/download/v$v
  curl -fsSLO "$base/checksums.txt" -O "$base/muster_linux_$a.rpm" -O "$base/muster-mirror_linux_$a.rpm"
  sha256sum --check --ignore-missing checksums.txt
  sudo dnf install ./muster_linux_$a.rpm ./muster-mirror_linux_$a.rpm
  # uninstall:
  sudo dnf remove muster muster-mirror
  ```

  The packages install `/usr/bin/muster` and `/usr/bin/muster-mirror` and
  nothing else: no service, no settings (the installer, or
  [mirroring.md](mirroring.md), sets those up). Removing them leaves each
  user's `~/.config/muster-mirror` and service unit; see INSTALL.md,
  "Remove a machine".
- **Homebrew** (macOS, Linux): see [Homebrew and Scoop](#homebrew-and-scoop).
- **Windows:** the CLI only. With [Scoop](https://scoop.sh):
  `scoop install https://github.com/ZacxDev/muster/releases/latest/download/muster.json`.
  Or download `muster_windows_amd64.zip` (or `_arm64`), check it against
  `checksums.txt` (`Get-FileHash muster_windows_amd64.zip`), and put
  `muster.exe` on your `PATH`. `install.sh` run from Git Bash, MSYS2 or Cygwin
  stops and says the same.

## Verifying a download

**Checksums** prove the file is the one the release lists:

```sh
sha256sum --check --ignore-missing checksums.txt      # macOS: shasum -a 256 --check --ignore-missing checksums.txt
```

**Attestations** prove the release workflow of this repository built it, from
which commit. Every asset of a release, every program inside its archives
and packages, and its `ghcr.io/zacxdev/muster:<version>` image have a signed
[build provenance](https://docs.github.com/en/actions/security-for-github-actions/using-artifact-attestations)
attestation (Sigstore, GitHub's attestation store). The
`muster-stt-cohere` and `claude-code-agent` images are attested by the run
that first pushed them: a version that reuses an earlier build (stt-cohere is
retagged when its directory did not change) carries that build's attestation,
or none if it predates attestations. Verify with the GitHub CLI:

```sh
gh attestation verify muster_linux_amd64.tar.gz --repo ZacxDev/muster
gh attestation verify ./muster --repo ZacxDev/muster                       # a program, unpacked
gh attestation verify oci://ghcr.io/zacxdev/muster:X.Y.Z --repo ZacxDev/muster
```

A pass prints the workflow (`.github/workflows/ci.yml`) and the commit that
built it. Add `--signer-workflow ZacxDev/muster/.github/workflows/ci.yml` to
require that workflow, not only this repository.

## Release notes and CHANGELOG.md

The notes and [CHANGELOG.md](../CHANGELOG.md) come from one script,
`scripts/release-notes.py` (tested by `tests/release_notes_test.py`): the pull
requests merged between a tag and the previous `vX.Y.Z` tag, grouped
**Security, Features, Fixes, Docs, Internal, Other changes**, each line linking
its pull request, under a one-line summary and any highlights; the install and
verify block is below them.

A pull request's group comes from its label, else from its title's
conventional-commit type, else it is "Other changes":

| label | or title type | group |
|---|---|---|
| `release:security` (or `security`) | `security:`, `sec:`, `harden:` | Security |
| `release:feature` (or `enhancement`) | `feat:`, `feature:` | Features |
| `release:fix` (or `bug`) | `fix:`, `bugfix:`, `hotfix:` | Fixes |
| `release:docs` (or `documentation`) | `docs:`, `doc:` | Docs |
| `release:internal` | `ci:`, `chore:`, `build:`, `test:`, `tests:`, `e2e:`, `refactor:`, `deps:`, `style:`, `deploy:` | Internal |
| `release:highlight` | — | also listed under the summary |
| `release:skip` (or `skip-changelog`) | — | left out |

A scope is kept and a type is dropped: `fix(mirror): reconnect` reads
**mirror:** reconnect, under Fixes. A title such as `officers: …` names a part
of muster, not a type, so it is "Other changes" until the pull request is
labelled. CHANGELOG.md is rewritten by the release flow's post-release pull
request, the one that moves the compose default to the new version
("compose: default to vX.Y.Z"): it runs `scripts/changelog-bump.sh` beside
`scripts/check-compose-default.sh --bump`. `tests/release_notes_test.py`
fails while CHANGELOG.md lacks the release the compose file defaults to.

## Homebrew and Scoop

Each release attaches a Homebrew formula per program and a Scoop manifest for
the CLI, generated from its own `checksums.txt`
(`scripts/release-manifests.py`), and the `release` job then commits them to
the tap and the bucket (`.github/scripts/publish-packages.sh`): one commit
each, "muster X.Y.Z", after the release is published.

- **Homebrew** (macOS, Linux), once the tap is published (the release job
  needs `MUSTER_PACKAGES_TOKEN`, below; each release's notes say which applies):

  ```sh
  brew install zacxdev/muster/muster zacxdev/muster/muster-mirror     # the tap ZacxDev/homebrew-muster
  brew upgrade muster muster-mirror
  ```

  Until then, from the release's own formulas in a local tap (Homebrew installs
  formulas only from a tap):

  ```sh
  brew tap-new --no-git "$USER/muster"
  t="$(brew --repository "$USER/muster")/Formula"
  curl -fsSL -o "$t/muster.rb" https://github.com/ZacxDev/muster/releases/latest/download/muster.rb
  curl -fsSL -o "$t/muster-mirror.rb" https://github.com/ZacxDev/muster/releases/latest/download/muster-mirror.rb
  brew install "$USER/muster/muster" "$USER/muster/muster-mirror"
  ```

- **Scoop** (Windows, the CLI only):

  ```sh
  scoop bucket add muster https://github.com/ZacxDev/scoop-muster     # once published, as above
  scoop install muster
  scoop update muster
  ```

  `scoop install <a release's muster.json URL>` (above) works without the bucket.

**The token (maintainers).** The job's own `GITHUB_TOKEN` cannot write to
another repository, so the step needs the repository secret
`MUSTER_PACKAGES_TOKEN`: a **fine-grained personal access token** with
*Repository access: only* `ZacxDev/homebrew-muster` and `ZacxDev/scoop-muster`,
and *Repository permissions: Contents: Read and write* (nothing else; Metadata
read-only comes with it). Set it with `gh secret set MUSTER_PACKAGES_TOKEN -R
ZacxDev/muster` (it reads the value from stdin), and note its expiry: when it
lapses the step fails as a warning and the release itself is unaffected.
Without the secret the step says it skipped. It never moves either
repository back to an older version, and a re-run commits nothing.
