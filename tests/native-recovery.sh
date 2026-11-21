#!/bin/bash
# Owned filesystem/cgroup gc checks. No VM boot and no host controller changes.
set -euo pipefail
flintyard_test_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$flintyard_test_root"
source scripts/dev-env.sh
if [[ $(id -u) != 0 ]]; then echo 'BLOCKED: native recovery tests require root' >&2; exit 77; fi
for tool in go gcc unshare mount; do
  if ! command -v "$tool" >/dev/null; then echo "BLOCKED: missing $tool" >&2; exit 77; fi
done
go test -race -c -o .local/native-recovery.test ./pkg/flintyard
set +e
unshare --mount --propagation private /bin/bash -c '
  set -euo pipefail
  mount -t tmpfs -o mode=0755 tmpfs /run
  export FY_RECOVERY_INTEGRATION=1
  exec "$@"
' flintyard-recovery-tests "$flintyard_test_root/.local/native-recovery.test" -test.v -test.run '^TestNativeRecovery' 2>&1 | tee "$flintyard_test_root/.local/native-recovery.log"
flintyard_test_status=("${PIPESTATUS[@]}")
set -e
if [[ ${flintyard_test_status[0]} != 0 ]]; then exit "${flintyard_test_status[0]}"; fi
if [[ ${flintyard_test_status[1]} != 0 ]]; then exit "${flintyard_test_status[1]}"; fi
if [[ $(< "$flintyard_test_root/.local/native-recovery.log") == *'--- SKIP:'* ]]; then
  echo 'BLOCKED: native recovery suite has skipped prerequisites' >&2
  exit 77
fi
