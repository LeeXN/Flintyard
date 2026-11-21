#!/bin/bash
# Extract real locked fixture archives; this does not boot or run guest code.
set -euo pipefail
fixture_repo="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$fixture_repo"
source scripts/dev-env.sh
if [[ $(id -u) != 0 ]]; then echo 'BLOCKED: rootfs metadata preservation requires root' >&2; exit 77; fi
if [[ $# != 1 || ! -d $1 ]]; then echo 'usage: native-fixture-rootfs.sh PREPARED_ROOTFS_DIRECTORY' >&2; exit 2; fi
export FLINTYARD_FIXTURE_ROOTFS_DIR="$(cd -- "$1" && pwd)"
go test -timeout 90m -count=1 -v -run '^TestNativeFixtureRootfsArchives$' ./internal/image
