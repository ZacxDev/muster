#!/usr/bin/env bash
# The install-and-verify block under a release's notes (markdown on stdout).
# The `release` job runs it and hands it to scripts/release-notes.py --install;
# tests/release_notes_test.py runs it too, so the block the notes carry is the
# one that is tested.
#
#   scripts/release-install-block.sh <tag> <owner/repo> tap|no-tap
#                                    e.g. v1.2.3 ZacxDev/muster no-tap
#
# tap|no-tap: whether the release job will publish this release's formulas to
# the Homebrew tap (<owner>/homebrew-muster) — it does when the
# MUSTER_PACKAGES_TOKEN secret is set (.github/scripts/publish-packages.sh).
# 🔴 `brew install <owner>/muster/…` is printed ONLY with `tap`: before the
# secret exists the tap holds no formula and that command fails ("No available
# formula"); with `no-tap` the block points at the formula assets instead.
# Required, never defaulted, so the caller cannot forget to decide.
#
# Exit 0 ok; 2 usage.
set -euo pipefail

[ $# -eq 3 ] || { echo "usage: $0 <vX.Y.Z> <owner/repo> tap|no-tap" >&2; exit 2; }
tag="$1"; repo="$2"; tap="$3"
case "$tap" in tap|no-tap) ;; *) echo "release-install-block: the third argument must be tap or no-tap, not '$tap'" >&2; exit 2 ;; esac
[[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "release-install-block: $tag is not vX.Y.Z" >&2; exit 2; }
[[ "$repo" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || { echo "release-install-block: $repo is not owner/repo" >&2; exit 2; }
v="${tag#v}"; owner="${repo%%/*}"; owner="${owner,,}"
dl="https://github.com/$repo/releases/download/$tag"

if [ "$tap" = tap ]; then
  brew="\`brew install $owner/muster/muster $owner/muster/muster-mirror\` (the tap $owner/homebrew-muster carries this release's formulas)"
else
  brew="this release's \`muster.rb\` and \`muster-mirror.rb\`, installed from a local tap (no published tap carries them yet: [docs/releases.md](https://github.com/$repo/blob/$tag/docs/releases.md#homebrew-and-scoop))"
fi

# 🔴 TRANSITION NOTICE, carried from the notes header #813 added: the muster
# image stopped shipping /muster-clankmail. Printed for releases BELOW 0.250.0
# only (tests/release_notes_test.py pins both sides), so it drops out on its
# own; delete this block once v0.250.0 has shipped.
clankmail=""
if [ "$(printf '%s\n%s\n' "$v" 0.250.0 | sort -V | head -n 1)" = "$v" ] && [ "$v" != 0.250.0 ]; then
  clankmail="
ClankMail plugin: the muster image no longer ships \`/muster-clankmail\` (earlier releases did). Use its own image, \`ghcr.io/$owner/muster-clankmail\` (https://github.com/ZacxDev/muster-clankmail; docs/clankmail.md).
"
fi

cat <<EOF
## Install

- **This machine, in one line** (Linux, macOS): \`curl -fsSL $dl/install.sh | sh\` — the server in Docker, and \`muster-mirror\` and the \`muster\` CLI into \`~/.local/bin\` (\`MUSTER_PACKAGE=deb\` or \`rpm\`: the system packages instead). [docs/quickstart-local.md](https://github.com/$repo/blob/$tag/docs/quickstart-local.md)
- **Server by hand:** download \`docker-compose.yml\` and run \`docker compose up -d --wait\` (no \`.env\` needed: the secrets are generated; \`env.example\`, saved as \`.env\`, sets your own). Images: \`ghcr.io/$owner/muster:$v\` (linux/amd64, linux/arm64), \`ghcr.io/$owner/muster-stt-cohere:$v\` (dictation, GPU, linux/amd64).
- **Each machine:** \`muster-mirror_<os>_<arch>.tar.gz\` and \`muster_<os>_<arch>.tar.gz\` (linux, darwin; amd64, arm64), or the \`.deb\` / \`.rpm\` packages (linux).
- **Windows:** the CLI only (\`muster-mirror\` needs tmux): \`scoop install $dl/muster.json\`, or \`muster_windows_amd64.zip\` / \`muster_windows_arm64.zip\`.
- **Homebrew:** $brew.
- **Kubernetes:** \`helm install muster $dl/muster-$v.tgz\` (its default image is this release). [docs/kubernetes.md](https://github.com/$repo/blob/$tag/docs/kubernetes.md)
$clankmail
## Verify

Every asset is in \`checksums.txt\`. This release's assets, the programs inside its archives and packages, and its \`ghcr.io/$owner/muster:$v\` image have signed build-provenance attestations from the workflow that built them. (The \`muster-stt-cohere\` and \`claude-code-agent\` images are attested only by the run that first pushed them, so one carried over from an earlier release may have none.)

\`\`\`sh
sha256sum --check --ignore-missing checksums.txt
gh attestation verify muster_linux_amd64.tar.gz --repo $repo
gh attestation verify oci://ghcr.io/$owner/muster:$v --repo $repo
\`\`\`
EOF
