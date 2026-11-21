#!/bin/bash
# Native privileged network checks only. No KVM, guest execution or host cleanup.
set -euo pipefail
flintyard_test_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$flintyard_test_root"
source scripts/dev-env.sh
if [[ $# -gt 1 ]]; then echo 'usage: tests/native-network.sh [GO_TEST_PATTERN]' >&2; exit 2; fi
flintyard_test_pattern="${1:-^TestNative}"
if [[ $(id -u) != 0 ]]; then
  echo 'BLOCKED: root and mount/net namespace capabilities are required' >&2
  exit 77
fi
for tool in go gcc unshare mount mknod ip nft ping; do
  if ! command -v "$tool" >/dev/null; then
    echo "BLOCKED: missing $tool" >&2
    exit 77
  fi
done
go test -race -c -o .local/native-network.test ./internal/network
# All mutations, including /run/netns, /dev/net/tun and forwarding, are confined
# to this private mount + network namespace. Failure destroys that test scope.
set +e
unshare --mount --net --propagation private /bin/bash -c '
  set -euo pipefail
  mount -t tmpfs -o mode=0755 tmpfs /run
  mount -t tmpfs -o mode=0755 tmpfs /dev
  mknod -m 666 /dev/null c 1 3
  mknod -m 666 /dev/zero c 1 5
  mknod -m 666 /dev/random c 1 8
  mknod -m 666 /dev/urandom c 1 9
  ln -s /proc/self/fd /dev/fd
  ln -s /proc/self/fd/0 /dev/stdin
  ln -s /proc/self/fd/1 /dev/stdout
  ln -s /proc/self/fd/2 /dev/stderr
  mkdir /dev/net
  mknod -m 666 /dev/net/tun c 10 200
  export FY_NETWORK_INTEGRATION=1
  exec "$@"
' flintyard-network-tests "$flintyard_test_root/.local/native-network.test" -test.v -test.run "$flintyard_test_pattern" 2>&1 | tee "$flintyard_test_root/.local/native-network.log"
flintyard_test_status=("${PIPESTATUS[@]}")
set -e
if [[ ${flintyard_test_status[0]} != 0 ]]; then exit "${flintyard_test_status[0]}"; fi
if [[ ${flintyard_test_status[1]} != 0 ]]; then exit "${flintyard_test_status[1]}"; fi
if [[ $(< "$flintyard_test_root/.local/native-network.log") == *'--- SKIP:'* ]]; then
  echo 'BLOCKED: native suite contains skipped prerequisites; completed checks remain in .local/native-network.log' >&2
  exit 77
fi
