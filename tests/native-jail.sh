#!/bin/bash
# Filesystem/cgroup checks only; does not boot a VM or change host controllers.
set -euo pipefail
flintyard_test_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$flintyard_test_root"
source scripts/dev-env.sh
if [[ $(id -u) != 0 ]]; then echo 'BLOCKED: native jail tests require root' >&2; exit 77; fi
for tool in go gcc unshare mount; do
  if ! command -v "$tool" >/dev/null; then echo "BLOCKED: missing $tool" >&2; exit 77; fi
done
go test -race -c -o .local/native-jail.test ./internal/firecracker
set +e
unshare --mount --propagation private /bin/bash -c '
  set -euo pipefail
  mount -t tmpfs -o mode=0755 tmpfs /run
  export FY_JAIL_INTEGRATION=1
  exec "$@"
' flintyard-jail-tests "$flintyard_test_root/.local/native-jail.test" -test.v -test.run '^TestNativeJail' 2>&1 | tee "$flintyard_test_root/.local/native-jail.log"
flintyard_test_status=("${PIPESTATUS[@]}")
set -e
if [[ ${flintyard_test_status[0]} != 0 ]]; then exit "${flintyard_test_status[0]}"; fi
if [[ ${flintyard_test_status[1]} != 0 ]]; then exit "${flintyard_test_status[1]}"; fi
if [[ $(< "$flintyard_test_root/.local/native-jail.log") == *'--- SKIP:'* ]]; then
  echo 'BLOCKED: native jail suite has skipped prerequisites; inspect .local/native-jail.log' >&2
  exit 77
fi
