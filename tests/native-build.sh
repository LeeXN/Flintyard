#!/bin/bash
# Real isolated ext4 build checks with explicitly nonbootable kernel fixtures.
set -euo pipefail
flintyard_test_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$flintyard_test_root"
source scripts/dev-env.sh
if [[ $(id -u) != 0 ]]; then echo 'BLOCKED: native image build tests require root' >&2; exit 77; fi
for tool in go gcc mkfs.ext4 debugfs; do
  if ! command -v "$tool" >/dev/null; then echo "BLOCKED: missing $tool" >&2; exit 77; fi
done
CGO_ENABLED=0 go build -o .local/build-worker-cli ./cmd/flintyard
CGO_ENABLED=0 go build -o .local/build-step-fixture ./tests/fixtures/build-step
go test -race -c -o .local/native-build.test ./internal/build
export FY_BUILD_INTEGRATION=1
export FY_BUILD_WORKER="$flintyard_test_root/.local/build-worker-cli"
export FY_BUILD_STEP_FIXTURE="$flintyard_test_root/.local/build-step-fixture"
set +e
"$flintyard_test_root/.local/native-build.test" -test.v -test.run '^TestNativeBuild' 2>&1 | tee "$flintyard_test_root/.local/native-build.log"
flintyard_test_status=("${PIPESTATUS[@]}")
set -e
if [[ ${flintyard_test_status[0]} != 0 ]]; then exit "${flintyard_test_status[0]}"; fi
if [[ ${flintyard_test_status[1]} != 0 ]]; then exit "${flintyard_test_status[1]}"; fi
if [[ $(< "$flintyard_test_root/.local/native-build.log") == *'--- SKIP:'* ]]; then
  echo 'BLOCKED: native build suite contains skipped prerequisites' >&2
  exit 77
fi
