#!/bin/bash
# External acceptance probe for a prepared Ubuntu distribution-kernel guest.
set -euo pipefail
if [[ $# != 5 ]]; then
  echo 'usage: kvm-ubuntu-original-check.sh IPV4 PRIVATE_KEY KNOWN_HOSTS KERNEL_RELEASE DNS_IPV4' >&2
  exit 2
fi
guest=$1
key=$2
known_hosts=$3
release=$4
dns=$5
if [[ ! $release =~ ^[A-Za-z0-9._+-]+$ || ! $dns =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]]; then
  echo 'invalid expected kernel release or DNS address' >&2
  exit 2
fi
script_dir=$(cd -- "$(dirname -- "$0")" && pwd -P)
bash "$script_dir/kvm-guest-check.sh" "$guest" "$key" "$known_hosts" systemd-networkd
ssh -F /dev/null -o BatchMode=yes -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes \
  -o UserKnownHostsFile="$known_hosts" -o GlobalKnownHostsFile=/dev/null -o ConnectTimeout=8 \
  -i "$key" "devops@$guest" /bin/sh -s -- "$release" "$dns" <<'GUEST'
set -eu
release=$1
dns=$2
test "$(uname -r)" = "$release"
test -s "/lib/modules/$release/modules.dep"
test -s "/boot/initrd.img-$release"
test "$(readlink /etc/resolv.conf)" = /run/flintyard/resolv.conf
grep -Fx "nameserver $dns" /etc/resolv.conf >/dev/null
systemctl is-active --quiet flintyard-seed.service
printf 'kernel_release=%s\nmodules_dep=present\ninitrd=present\nseed_service=active\ndns_config=%s\n' "$release" "$dns"
GUEST
