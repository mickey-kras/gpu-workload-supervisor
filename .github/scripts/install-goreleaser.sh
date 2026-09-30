#!/usr/bin/env bash
set -euo pipefail

version="$1"
[[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]
[[ "${RUNNER_OS}" == Linux && "${RUNNER_ARCH}" == X64 ]]
command -v cosign >/dev/null

work_dir=$(mktemp -d "${RUNNER_TEMP}/goreleaser.XXXXXX")
trap 'rm -rf "$work_dir"' EXIT
cd "$work_dir"
archive=goreleaser_Linux_x86_64.tar.gz
base_url="https://github.com/goreleaser/goreleaser/releases/download/${version}"
for file in checksums.txt checksums.txt.sigstore.json "$archive"; do
  curl --fail --silent --show-error --location --retry 3 \
    --output "$file" "${base_url}/${file}"
done

cosign verify-blob \
  --certificate-identity "https://github.com/goreleaser/goreleaser/.github/workflows/release.yml@refs/tags/${version}" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --bundle checksums.txt.sigstore.json checksums.txt
awk -v archive="$archive" '$2 == archive { print; found++ } END { if (found != 1) exit 1 }' \
  checksums.txt > archive.checksum
sha256sum --check --strict archive.checksum

# Extract only the executable, after authenticating this exact archive.
tar -xzf "$archive" goreleaser
install_dir=$(mktemp -d "${RUNNER_TEMP}/goreleaser-bin.XXXXXX")
install -m 0755 goreleaser "${install_dir}/goreleaser"
printf '%s\n' "$install_dir" >> "$GITHUB_PATH"
