#!/bin/bash
# External guest probe against the controlled TCP and DNS test endpoints.
set -euo pipefail
if [[ $# -ne 6 ]]; then
  echo 'usage: kvm-guest-egress.sh GUEST_IPV4 PRIVATE_KEY KNOWN_HOSTS ENDPOINT_IPV4 TCP_PORT enabled|disabled' >&2
  exit 2
fi
guest=$1; key=$2; known_hosts=$3; endpoint=$4; port=$5; expected=$6
ip_pattern='^([0-9]{1,3}\.){3}[0-9]{1,3}$'
if [[ ! $guest =~ $ip_pattern || ! $endpoint =~ $ip_pattern || ! $port =~ ^[0-9]{1,5}$ || ! -f $key || -L $key || ! -f $known_hosts || -L $known_hosts ]]; then
  echo 'invalid egress test inputs' >&2; exit 2
fi
if [[ $expected != enabled && $expected != disabled ]]; then echo 'invalid expected result' >&2; exit 2; fi
ssh -F /dev/null -o BatchMode=yes -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes \
  -o UserKnownHostsFile="$known_hosts" -o GlobalKnownHostsFile=/dev/null -o ConnectTimeout=8 \
  -i "$key" "devops@$guest" /bin/sh -s -- "$endpoint" "$port" "$expected" <<'GUEST'
set -eu
endpoint=$1; port=$2; expected=$3
ip -4 route get "$endpoint"
if answer=$(printf 'flintyard-nat-test\n' | nc -w 3 "$endpoint" "$port" 2>/dev/null) && [ "$answer" = flintyard-nat-ok ]; then tcp=enabled; else tcp=disabled; fi
if RES_OPTIONS='attempts:1 timeout:2' getent ahostsv4 flintyard-test.invalid 2>/dev/null | grep -q '^198\.51\.100\.42 '; then dns=enabled; else dns=disabled; fi
printf 'tcp=%s dns=%s expected=%s\n' "$tcp" "$dns" "$expected"
test "$tcp" = "$expected" && test "$dns" = "$expected"
GUEST
