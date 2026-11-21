#!/bin/bash
# Real build SIGKILL/file/mount/loop recovery. No KVM or guest execution.
set -euo pipefail
flintyard_test_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$flintyard_test_root"
source scripts/dev-env.sh
if [[ $(id -u) != 0 ]]; then echo 'BLOCKED: native build GC requires root' >&2; exit 77; fi
for tool in go gcc unshare mount mknod mkfs.ext4; do
  if ! command -v "$tool" >/dev/null; then echo "BLOCKED: missing $tool" >&2; exit 77; fi
done
CGO_ENABLED=0 go build -o .local/build-worker-cli ./cmd/flintyard
CGO_ENABLED=0 go build -o .local/build-step-fixture ./tests/fixtures/build-step
go test -race -c -o .local/native-build-gc.test ./internal/build
set +e
unshare --mount --propagation private /bin/bash -c '
  set -euo pipefail
  mount -t tmpfs -o mode=0755 tmpfs /run
  mount -t tmpfs -o mode=0755 tmpfs /dev
  mknod -m 666 /dev/null c 1 3
  mknod -m 666 /dev/zero c 1 5
  mknod -m 666 /dev/random c 1 8
  mknod -m 666 /dev/urandom c 1 9
  mknod -m 600 /dev/loop-control c 10 237
  export FY_BUILD_GC_INTEGRATION=1 FY_BUILD_INTEGRATION=1
  export FY_BUILD_WORKER="$1/.local/build-worker-cli" FY_BUILD_STEP_FIXTURE="$1/.local/build-step-fixture"
  exec "$1/.local/native-build-gc.test" -test.v -test.run "^TestNativeGCBuild"
' flintyard-build-gc-tests "$flintyard_test_root" 2>&1 | tee "$flintyard_test_root/.local/native-build-gc.log"
flintyard_test_status=("${PIPESTATUS[@]}")
set -e
if [[ ${flintyard_test_status[0]} != 0 ]]; then exit "${flintyard_test_status[0]}"; fi
if [[ ${flintyard_test_status[1]} != 0 ]]; then exit "${flintyard_test_status[1]}"; fi
if [[ $(< "$flintyard_test_root/.local/native-build-gc.log") == *'--- SKIP:'* ]]; then
  echo 'BLOCKED: native build GC suite contains skipped prerequisites' >&2
  exit 77
fi
