#!/bin/bash
# External KVM acceptance probe. The product itself does not execute guest commands.
set -euo pipefail
if [[ $# -lt 3 || $# -gt 4 ]]; then
  echo 'usage: kvm-guest-check.sh IPV4 PRIVATE_KEY KNOWN_HOSTS [systemd-networkd|openrc-ifupdown|networkmanager]' >&2
  exit 2
fi
guest=$1
key=$2
known_hosts=$3
profile=${4:-systemd-networkd}
if [[ ! $guest =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ || ! -f $key || -L $key ]]; then
  echo 'invalid guest address or test key' >&2
  exit 2
fi
if [[ $profile != systemd-networkd && $profile != openrc-ifupdown && $profile != networkmanager ]]; then
  echo 'invalid guest profile' >&2
  exit 2
fi
umask 077
if [[ ! -e $known_hosts && ! -L $known_hosts ]]; then
  parent=$(dirname -- "$known_hosts")
  temporary=$(mktemp "$parent/.flintyard-hostkey.XXXXXXXX")
  trap 'rm -f -- "$temporary"' EXIT
  ssh-keyscan -T 5 -t ed25519 "$guest" > "$temporary" 2>/dev/null
  [[ -s $temporary ]] || { echo 'no guest host key received' >&2; exit 1; }
  ln -- "$temporary" "$known_hosts"
fi
[[ -f $known_hosts && ! -L $known_hosts ]] || { echo 'invalid known-hosts file' >&2; exit 2; }
printf 'observed_host_key='
ssh-keygen -lf "$known_hosts"
ssh -F /dev/null -o BatchMode=yes -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes \
  -o UserKnownHostsFile="$known_hosts" -o GlobalKnownHostsFile=/dev/null -o ConnectTimeout=8 \
  -i "$key" "devops@$guest" /bin/sh -s -- "$profile" <<'GUEST'
set -eu
profile=$1
test "$(id -un)" = devops
test -b /dev/vda
test -r /etc/resolv.conf
case "$profile" in
  systemd-networkd) test -r /etc/systemd/network/00-flintyard.network ;;
  openrc-ifupdown) test -r /etc/network/interfaces ;;
  networkmanager) : ;; # Its connection file is intentionally root-only.
  *) exit 2 ;;
esac
test "$(stat -c %a "$HOME/.ssh")" = 700
test "$(stat -c %a "$HOME/.ssh/authorized_keys")" = 600
grep -Eq '^[0-9a-f]{32}$' /etc/machine-id
printf 'user='; id -un
printf 'hostname='; hostname
printf 'kernel='; uname -r
printf 'machine_id='; cat /etc/machine-id
printf 'guest_host_key='; ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub
printf 'root_device='; findmnt -n -o SOURCE /
printf 'addresses='; ip -o -4 addr show scope global
printf 'profile=%s\nresolver_readable=yes\nssh_permissions=700/600\n' "$profile"
GUEST
