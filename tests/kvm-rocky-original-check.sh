#!/bin/bash
# External acceptance probe for a Rocky distribution-kernel guest.
set -euo pipefail
if [[ $# != 4 ]]; then
  echo 'usage: kvm-rocky-original-check.sh IPV4 PRIVATE_KEY KNOWN_HOSTS KERNEL_RELEASE' >&2
  exit 2
fi
guest=$1
key=$2
known_hosts=$3
release=$4
if [[ ! $release =~ ^[A-Za-z0-9._+-]+$ ]]; then
  echo 'invalid expected kernel release' >&2
  exit 2
fi
script_dir=$(cd -- "$(dirname -- "$0")" && pwd -P)
bash "$script_dir/kvm-guest-check.sh" "$guest" "$key" "$known_hosts" networkmanager
ssh -F /dev/null -o BatchMode=yes -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes \
  -o UserKnownHostsFile="$known_hosts" -o GlobalKnownHostsFile=/dev/null -o ConnectTimeout=8 \
  -i "$key" "devops@$guest" /bin/sh -s -- "$release" <<'GUEST'
set -eu
release=$1
test "$(uname -r)" = "$release"
test -s "/lib/modules/$release/modules.dep"
test -s "/boot/initramfs-$release.img"
systemctl is-active --quiet flintyard-seed.service
systemctl is-active --quiet NetworkManager.service
printf 'kernel_release=%s\nmodules_dep=present\ninitrd=present\nseed_service=active\nnetwork_manager=active\n' "$release"
GUEST
