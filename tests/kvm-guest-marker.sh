#!/bin/bash
# External guest disk-persistence check. The marker belongs to this test VM.
set -euo pipefail
if [[ $# -ne 5 ]]; then
  echo 'usage: kvm-guest-marker.sh IPV4 PRIVATE_KEY KNOWN_HOSTS write|check|absent TOKEN' >&2
  exit 2
fi
guest=$1
key=$2
known_hosts=$3
action=$4
token=$5
if [[ ! $guest =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ || ! -f $key || -L $key || ! -f $known_hosts || -L $known_hosts || ! $token =~ ^[a-z0-9]{16,64}$ ]]; then
  echo 'invalid test target, key, known-hosts file or token' >&2
  exit 2
fi
if [[ $action != write && $action != check && $action != absent ]]; then echo 'invalid marker action' >&2; exit 2; fi
ssh -F /dev/null -o BatchMode=yes -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes \
  -o UserKnownHostsFile="$known_hosts" -o GlobalKnownHostsFile=/dev/null -o ConnectTimeout=8 \
  -i "$key" "devops@$guest" /bin/sh -s -- "$action" "$token" <<'GUEST'
set -eu
action=$1
token=$2
file="$HOME/.flintyard-test-$token"
case "$action" in
  write) umask 077; ( set -C; printf '%s\n' "$token" > "$file" ); sync ;;
  check) test "$(cat "$file")" = "$token" ;;
  absent) test ! -e "$file" ;;
  *) exit 2 ;;
esac
printf 'disk_marker_%s=passed\n' "$action"
GUEST
