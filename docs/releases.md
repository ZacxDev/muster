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
  ([quickstart-local.md](quickstart-local.md)). On Linux it installs the
  `.deb` or `.rpm` packages (`/usr/bin`) when the machine has dpkg or rpm and
  you are root or have sudo without a password; otherwise the archives into
  `~/.local/bin`. `MUSTER_PACKAGE=tarball` always takes the archives;
  `MUSTER_PACKAGE=deb` (or `rpm`) always takes the package, asking sudo for a
  password if it needs one. Setting `MUSTER_BIN` means "install into that
  directory", so it never installs a package.
- **A package by hand** (Debian/Ubuntu, Fedora/RHEL/openSUSE):

  ```sh
  v=X.Y.Z; a=amd64    # or arm64
  base=https://github.com/ZacxDev/muster/releases/download/v$v
  curl -fsSLO "$base/checksums.txt" -O "$base/muster_linux_$a.deb" -O "$base/muster-mirror_linux_$a.deb"
  sha256sum --check --ignore-missing checksums.txt
  sudo dpkg -i muster_linux_$a.deb muster-mirror_linux_$a.deb     # or: sudo rpm -U *.rpm
  ```

  The packages install `/usr/bin/muster` and `/usr/bin/muster-mirror` and
  nothing else: no service, no settings. `muster-mirror` recommends `tmux`
  (`dpkg -i` and `rpm -U` install without it; install tmux yourself).
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
which commit. Every asset, every program inside an archive or package, and
every image has a signed [build provenance](https://docs.github.com/en/actions/security-for-github-actions/using-artifact-attestations)
attestation (Sigstore, GitHub's attestation store). Verify with the GitHub CLI:

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
labelled. Refresh CHANGELOG.md after a release with `make changelog`
(needs `gh`).

## Homebrew and Scoop

Each release attaches a Homebrew formula per program and a Scoop manifest for
the CLI, generated from its own `checksums.txt`
(`scripts/release-manifests.py`). Nothing publishes them to a tap or a bucket
yet: that needs a repository of its own, which is a decision for the
maintainers. Until then:

- **Homebrew**, from the release's formula in a local tap (Homebrew installs
  formulas only from a tap):

  ```sh
  brew tap-new --no-git "$USER/muster"
  t="$(brew --repository "$USER/muster")/Formula"
  curl -fsSL -o "$t/muster.rb" https://github.com/ZacxDev/muster/releases/latest/download/muster.rb
  curl -fsSL -o "$t/muster-mirror.rb" https://github.com/ZacxDev/muster/releases/latest/download/muster-mirror.rb
  brew install "$USER/muster/muster" "$USER/muster/muster-mirror"
  ```

- **Scoop**: `scoop install <the release's muster.json URL>` (above) needs no bucket.

**To publish them (maintainers).** Create the two repositories once, then copy
each release's files into them; both tools then update with their usual
commands (`brew upgrade`, `scoop update`):

```sh
# once: an empty public tap and bucket
gh repo create ZacxDev/homebrew-muster --public --description "Homebrew tap for muster"
gh repo create ZacxDev/scoop-muster --public --description "Scoop bucket for muster"

# per release vX.Y.Z
v=X.Y.Z
gh release download "v$v" -R ZacxDev/muster -p muster.rb -p muster-mirror.rb -p muster.json -D "/tmp/muster-$v"
git clone git@github.com:ZacxDev/homebrew-muster.git /tmp/tap && mkdir -p /tmp/tap/Formula
cp "/tmp/muster-$v"/*.rb /tmp/tap/Formula/ && git -C /tmp/tap add Formula && git -C /tmp/tap commit -m "muster $v" && git -C /tmp/tap push
git clone git@github.com:ZacxDev/scoop-muster.git /tmp/bucket && mkdir -p /tmp/bucket/bucket
cp "/tmp/muster-$v/muster.json" /tmp/bucket/bucket/ && git -C /tmp/bucket add bucket && git -C /tmp/bucket commit -m "muster $v" && git -C /tmp/bucket push
```

People then run `brew install zacxdev/muster/muster` and `scoop bucket add muster
https://github.com/ZacxDev/scoop-muster && scoop install muster`. Automating the
per-release copy from the `release` job needs a token that can push to those
repositories (a fine-grained token or a deploy key as a repository secret):
the job's own `GITHUB_TOKEN` cannot write to another repository.
