#!/usr/bin/env bash
# The install-and-verify block under a release's notes (markdown on stdout).
# The `release` job runs it and hands it to scripts/release-notes.py --install;
# tests/release_notes_test.py runs it too, so the block the notes carry is the
# one that is tested.
#
#   scripts/release-install-block.sh <tag> <owner/repo>       e.g. v1.2.3 ZacxDev/muster
#
# Exit 0 ok; 2 usage.
set -euo pipefail

[ $# -eq 2 ] || { echo "usage: $0 <vX.Y.Z> <owner/repo>" >&2; exit 2; }
tag="$1"; repo="$2"
[[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "release-install-block: $tag is not vX.Y.Z" >&2; exit 2; }
[[ "$repo" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || { echo "release-install-block: $repo is not owner/repo" >&2; exit 2; }
v="${tag#v}"; owner="${repo%%/*}"; owner="${owner,,}"
dl="https://github.com/$repo/releases/download/$tag"

cat <<EOF
## Install

- **This machine, in one line** (Linux, macOS): \`curl -fsSL $dl/install.sh | sh\` — the server in Docker, and \`muster-mirror\` and the \`muster\` CLI into \`~/.local/bin\` (\`MUSTER_PACKAGE=deb\` or \`rpm\`: the system packages instead). [docs/quickstart-local.md](https://github.com/$repo/blob/$tag/docs/quickstart-local.md)
- **Server by hand:** download \`docker-compose.yml\` and run \`docker compose up -d --wait\` (no \`.env\` needed: the secrets are generated; \`env.example\`, saved as \`.env\`, sets your own). Images: \`ghcr.io/$owner/muster:$v\` (linux/amd64, linux/arm64), \`ghcr.io/$owner/muster-stt-cohere:$v\` (dictation, GPU, linux/amd64).
- **Each machine:** \`muster-mirror_<os>_<arch>.tar.gz\` and \`muster_<os>_<arch>.tar.gz\` (linux, darwin; amd64, arm64), or the \`.deb\` / \`.rpm\` packages (linux).
- **Windows:** the CLI only (\`muster-mirror\` needs tmux): \`scoop install $dl/muster.json\`, or \`muster_windows_amd64.zip\` / \`muster_windows_arm64.zip\`.
- **Homebrew:** \`brew install zacxdev/muster/muster zacxdev/muster/muster-mirror\` (the tap gets this release's \`muster.rb\` / \`muster-mirror.rb\`; [docs/releases.md](https://github.com/$repo/blob/$tag/docs/releases.md#homebrew-and-scoop)).
- **Kubernetes:** \`helm install muster $dl/muster-$v.tgz\` (its default image is this release). [docs/kubernetes.md](https://github.com/$repo/blob/$tag/docs/kubernetes.md)

## Verify

Every asset is in \`checksums.txt\`, and every asset, every program inside them and every image has a signed build-provenance attestation:

\`\`\`sh
sha256sum --check --ignore-missing checksums.txt
gh attestation verify muster_linux_amd64.tar.gz --repo $repo
gh attestation verify oci://ghcr.io/$owner/muster:$v --repo $repo
\`\`\`
EOF
