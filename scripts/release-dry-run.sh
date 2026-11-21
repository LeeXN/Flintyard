#!/bin/bash
# Build a local release candidate from a single committed source tree.
set -euo pipefail
if [[ $# != 4 ]]; then
  echo 'usage: release-dry-run.sh REPOSITORY COMMIT COMPATIBILITY_JSON NEW_OUTPUT_DIR' >&2
  exit 2
fi
repo=$(realpath -- "$1")
commit=$(git -C "$repo" rev-parse --verify "$2^{commit}")
compatibility=$(realpath -- "$3")
output=$(realpath -m -- "$4")
go=${GO:-go}
export GOTOOLCHAIN=local
toolchain=$("$go" version | awk '{print $3}')
[[ $toolchain == go1.27.3 ]] || { echo "expected Go 1.27.3, found $toolchain" >&2; exit 2; }
[[ ! -e $output && ! -L $output ]]
stage=$(mktemp -d -- "$(dirname -- "$output")/.flintyard-release-XXXXXXXX")
trap 'rm -rf -- "$stage"' EXIT
mkdir -- "$stage/source" "$stage/packages"
git -C "$repo" archive --format=tar --prefix=flintyard/ "$commit" | gzip -n > "$stage/source.tar.gz"
tar -xzf "$stage/source.tar.gz" -C "$stage/source"
cd "$stage/source/flintyard"
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" "$go" build -trimpath -buildvcs=false -o "$stage/flintyard-linux-$arch" ./cmd/flintyard
done
case $(uname -m) in
  x86_64) native=amd64 ;;
  aarch64) native=arm64 ;;
  *) echo 'release verification requires a supported Linux host' >&2; exit 3 ;;
esac
version=$("$stage/flintyard-linux-$native" version --json | jq -er '.data.version')
[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.]+)?$ ]]
for arch in amd64 arm64; do
  tar -czf "$stage/packages/flintyard-v$version-linux-$arch.tar.gz" \
    -C "$stage" "flintyard-linux-$arch" \
    -C "$stage/source/flintyard" LICENSE THIRD-PARTY-NOTICES.md licenses
done
cp -- "$stage/source.tar.gz" "$stage/packages/flintyard-v$version-source.tar.gz"
jq -e --arg commit "$commit" 'select(type == "object" and .commit == $commit)' "$compatibility" > "$stage/packages/compatibility.json"
printf 'version=%s\ncommit=%s\ntoolchain=%s\nlinux_architectures=amd64,arm64\n' "$version" "$commit" "$toolchain" > "$stage/packages/BUILD-INFO"
cd "$stage/packages"
sha256sum ./*.tar.gz BUILD-INFO compatibility.json > SHA256SUMS
sha256sum -c SHA256SUMS
cd /
mv -T -n -- "$stage/packages" "$output"
[[ ! -e $stage/packages ]]
printf 'release_dry_run=%s\n' "$output"
