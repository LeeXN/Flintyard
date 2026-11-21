#!/bin/bash
# Publication SIGKILL and public GC. No VMM, network changes or guest commands.
set -euo pipefail
flintyard_test_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$flintyard_test_root"
source scripts/dev-env.sh
if [[ $(id -u) != 0 ]]; then echo 'BLOCKED: native bootstrap recovery requires root' >&2; exit 77; fi
for tool in go gcc unshare mount; do
  if ! command -v "$tool" >/dev/null; then echo "BLOCKED: missing $tool" >&2; exit 77; fi
done
CGO_ENABLED=0 go build -o .local/bootstrap-cli ./cmd/flintyard
go test -race -c -o .local/native-bootstrap.test ./internal/state
set +e
unshare --mount --propagation private /bin/bash -c '
  set -euo pipefail
  mount -t tmpfs -o mode=0755 tmpfs /run
  export FY_BOOTSTRAP_INTEGRATION=1 FY_BOOTSTRAP_CLI="$1/.local/bootstrap-cli"
  exec "$1/.local/native-bootstrap.test" -test.v -test.run "^TestNativeBootstrap"
' flintyard-bootstrap-tests "$flintyard_test_root" 2>&1 | tee "$flintyard_test_root/.local/native-bootstrap.log"
flintyard_test_status=("${PIPESTATUS[@]}")
set -e
if [[ ${flintyard_test_status[0]} != 0 ]]; then exit "${flintyard_test_status[0]}"; fi
if [[ ${flintyard_test_status[1]} != 0 ]]; then exit "${flintyard_test_status[1]}"; fi
if [[ $(< "$flintyard_test_root/.local/native-bootstrap.log") == *'--- SKIP:'* ]]; then echo 'BLOCKED: skipped bootstrap prerequisites' >&2; exit 77; fi
