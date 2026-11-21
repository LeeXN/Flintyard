#!/bin/bash
set -euo pipefail
version=1.27.3
sha=65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40
if [ "$(uname -s)/$(uname -m)" != Linux/x86_64 ]; then
  echo 'This cloud bootstrap is pinned for the current Linux x86_64 executor; CLI cross-builds also target arm64.' >&2
  exit 1
fi
mkdir -p /workspace/toolchains
if [ ! -x /workspace/toolchains/go/bin/go ]; then
  staging=$(mktemp -d /workspace/toolchains/.go-install-XXXXXX)
  trap 'rm -rf "$staging"' EXIT
  curl -fsSL "https://go.dev/dl/go${version}.linux-amd64.tar.gz" -o "$staging/go.tar.gz"
  echo "$sha  $staging/go.tar.gz" | sha256sum -c -
  tar -xzf "$staging/go.tar.gz" -C "$staging"
  mv "$staging/go" /workspace/toolchains/go
fi
[ "$(/workspace/toolchains/go/bin/go version)" = "go version go${version} linux/amd64" ]
cd /workspace/Flintyard
source scripts/dev-env.sh
go mod download
make build
