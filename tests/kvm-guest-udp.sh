#!/bin/bash
# A finite, unprivileged UDP echo endpoint inside one disposable test guest.
set -euo pipefail
if [[ $# -lt 4 || $# -gt 5 ]]; then echo 'usage: kvm-guest-udp.sh IPV4 PRIVATE_KEY KNOWN_HOSTS PORT [COUNT]' >&2; exit 2; fi
guest=$1; key=$2; known_hosts=$3; port=$4; count=${5:-3}
if [[ ! $guest =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ || ! $port =~ ^[0-9]{1,5}$ || ! $count =~ ^[1-9]$ || ! -f $key || -L $key || ! -f $known_hosts || -L $known_hosts ]]; then
  echo 'invalid UDP test inputs' >&2; exit 2
fi
ssh -F /dev/null -o BatchMode=yes -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes \
  -o UserKnownHostsFile="$known_hosts" -o GlobalKnownHostsFile=/dev/null -o ConnectTimeout=8 \
  -i "$key" "devops@$guest" python3 -u - "$port" "$count" <<'GUEST'
import socket, sys
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.bind(('0.0.0.0', int(sys.argv[1])))
s.settimeout(600)
print('udp_ready', flush=True)
for i in range(int(sys.argv[2])):
    data, peer = s.recvfrom(128)
    if not data.startswith(b'flintyard-udp-'):
        raise ValueError('unexpected test datagram')
    s.sendto(b'echo:' + data, peer)
    print('udp_echo', i + 1, flush=True)
s.close()
GUEST
