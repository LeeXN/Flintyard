#!/bin/bash
# External guest-to-guest connectivity check for owned KVM test environments.
set -euo pipefail
if [[ $# -lt 5 || $# -gt 6 ]]; then
  echo 'usage: kvm-guest-ping.sh SOURCE_IPV4 PRIVATE_KEY KNOWN_HOSTS TARGET_IPV4 reachable|blocked [icmp|tcp]' >&2
  exit 2
fi
source_ip=$1
key=$2
known_hosts=$3
target=$4
expected=$5
protocol=${6:-icmp}
ip_pattern='^([0-9]{1,3}\.){3}[0-9]{1,3}$'
if [[ ! $source_ip =~ $ip_pattern || ! $target =~ $ip_pattern || ! -f $key || -L $key || ! -f $known_hosts || -L $known_hosts ]]; then
  echo 'invalid test target, key or known-hosts file' >&2
  exit 2
fi
if [[ $expected != reachable && $expected != blocked ]]; then echo 'invalid expected result' >&2; exit 2; fi
if [[ $protocol != icmp && $protocol != tcp ]]; then echo 'invalid connectivity protocol' >&2; exit 2; fi
ssh -F /dev/null -o BatchMode=yes -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes \
  -o UserKnownHostsFile="$known_hosts" -o GlobalKnownHostsFile=/dev/null -o ConnectTimeout=8 \
  -i "$key" "devops@$source_ip" /bin/sh -s -- "$target" "$expected" "$protocol" <<'GUEST'
set -eu
target=$1
expected=$2
protocol=$3
ip -4 route get "$target"
if [ "$protocol" = icmp ]; then
  if ! ping -c 1 -W 2 127.0.0.1 >/dev/null 2>&1; then echo 'ICMP probe unavailable' >&2; exit 3; fi
  if output=$(ping -c 2 -W 2 "$target" 2>&1); then observed=reachable; else observed=blocked; fi
else
  command -v nc >/dev/null 2>&1 || { echo 'TCP probe unavailable' >&2; exit 3; }
  output=$(printf '' | nc -w 3 "$target" 22 2>&1) || true
  case "$output" in SSH-2.0-*) observed=reachable ;; *) observed=blocked ;; esac
fi
printf '%s\n' "$output"
printf 'target=%s protocol=%s expected=%s observed=%s\n' "$target" "$protocol" "$expected" "$observed"
test "$observed" = "$expected"
GUEST
