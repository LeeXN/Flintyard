package guestinit

// This installed guest program reads data only. It has no listener, RPC or
// command payload, and never sources/evals seed contents.
const seedAdapter = `#!/bin/sh
set -eu
umask 077
seed=/run/flintyard/seed
install -d -m 0755 /run/flintyard
install -d -m 0700 "$seed" /var/lib/flintyard
device=
for attempt in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22 23 24 25 26 27 28 29 30; do
  device=$(blkid -L FYSEED 2>/dev/null || true)
  [ -n "$device" ] && break
  sleep 1
done
[ -n "$device" ] || { echo 'FYSEED unavailable' >&2; exit 1; }
mount -o ro "$device" "$seed"
trap 'umount "$seed"' EXIT
IFS= read -r version < "$seed/version"
[ "$version" = 1 ] || exit 1
IFS= read -r instance < "$seed/instance-id"
IFS= read -r hostname_value < "$seed/hostname"
IFS= read -r mac < "$seed/mac"
IFS= read -r address < "$seed/address"
IFS= read -r prefix < "$seed/prefix"
IFS= read -r gateway < "$seed/gateway"
IFS= read -r username < "$seed/username"
IFS= read -r expected < /etc/flintyard/username
IFS= read -r profile < /etc/flintyard/profile
IFS= read -r port < /etc/flintyard/ssh-port
[ "$username" = "$expected" ] || exit 1
case "$instance" in *[!0-9a-f/-]*|'') exit 1;; esac
case "$instance" in ????????-????-????-????-????????????/????????-????-????-????-????????????) ;; *) exit 1;; esac
[ ${#instance} -eq 73 ] || exit 1
case "$hostname_value" in *[!a-z0-9-]*|'') exit 1;; esac
[ ${#hostname_value} -le 63 ] || exit 1
case "$mac" in *[!0-9a-f:]*|'') exit 1;; esac
[ ${#mac} -eq 17 ] || exit 1
case "$address:$gateway" in *[!0-9.:]*|'') exit 1;; esac
case "$prefix:$port" in *[!0-9:]*|'') exit 1;; esac
[ "$prefix" -ge 8 ] && [ "$prefix" -le 29 ] && [ "$port" -ge 1 ] && [ "$port" -le 65535 ] || exit 1
if [ -f /var/lib/flintyard/instance-id ]; then
  IFS= read -r previous < /var/lib/flintyard/instance-id
  [ "$previous" = "$instance" ] || { echo 'root disk belongs to another instance' >&2; exit 1; }
fi
iface=
for attempt in 1 2 3 4 5 6 7 8 9 10; do
  for nic in /sys/class/net/*; do
    IFS= read -r candidate < "$nic/address"
    if [ "$candidate" = "$mac" ]; then iface=${nic##*/}; break; fi
  done
  [ -n "$iface" ] && break
  sleep 1
done
[ -n "$iface" ] || exit 1
case "$iface" in *[!A-Za-z0-9_.:-]*|'') exit 1;; esac
printf '%s\n' "$hostname_value" > /etc/hostname
chmod 644 /etc/hostname
hostname "$hostname_value"
if [ ! -s /etc/machine-id ]; then tr -d '-' < /proc/sys/kernel/random/uuid > /etc/machine-id; chmod 444 /etc/machine-id; fi
: > /run/flintyard/resolv.conf
dns_list=
while IFS= read -r dns; do
  case "$dns" in *[!0-9.]*|'') exit 1;; esac
  printf 'nameserver %s\n' "$dns" >> /run/flintyard/resolv.conf
  dns_list="$dns_list$dns;"
done < "$seed/dns"
chmod 644 /run/flintyard/resolv.conf
resolv_link=/etc/resolv.conf
resolv_tmp="${resolv_link}.flintyard.$$"
ln -s /run/flintyard/resolv.conf "$resolv_tmp"
trap 'rm -f "$resolv_tmp"; umount "$seed"' EXIT
[ ! -d "$resolv_link" ] || { echo 'resolver path is a directory' >&2; exit 1; }
mv -f "$resolv_tmp" "$resolv_link"
trap 'umount "$seed"' EXIT
case "$profile" in
 systemd-networkd)
  install -d -m 0755 /etc/systemd/network
  { printf '[Match]\nMACAddress=%s\n[Network]\nDHCP=no\nIPv6AcceptRA=no\nLinkLocalAddressing=no\nAddress=%s/%s\nGateway=%s\n' "$mac" "$address" "$prefix" "$gateway"; while IFS= read -r dns; do printf 'DNS=%s\n' "$dns"; done < "$seed/dns"; } > /etc/systemd/network/00-flintyard.network
  chmod 644 /etc/systemd/network/00-flintyard.network
  ;;
 networkmanager)
  mkdir -p /etc/NetworkManager/system-connections
  node_uuid=${instance#*/}
  printf '[connection]\nid=flintyard\nuuid=%s\ntype=ethernet\n[ethernet]\nmac-address=%s\n[ipv4]\nmethod=manual\naddress1=%s/%s,%s\ndns=%s\nignore-auto-dns=true\n[ipv6]\nmethod=disabled\n' "$node_uuid" "$mac" "$address" "$prefix" "$gateway" "$dns_list" > /etc/NetworkManager/system-connections/flintyard.nmconnection
  chmod 600 /etc/NetworkManager/system-connections/flintyard.nmconnection
  ;;
 openrc-ifupdown)
  install -d -m 0755 /etc/network
  printf 'auto lo\niface lo inet loopback\nauto %s\niface %s inet static\n  address %s/%s\n  gateway %s\n' "$iface" "$iface" "$address" "$prefix" "$gateway" > /etc/network/interfaces
  chmod 644 /etc/network/interfaces
  ;;
 *) exit 1;;
esac
home=$(awk -F: -v user="$username" '$1==user { print $6 }' /etc/passwd)
uid=$(awk -F: -v user="$username" '$1==user { print $3 }' /etc/passwd)
gid=$(awk -F: -v user="$username" '$1==user { print $4 }' /etc/passwd)
case "$home" in /*) ;; *) exit 1;; esac
case "$uid:$gid" in *[!0-9:]*|'') exit 1;; esac
install -d -m 700 -o "$uid" -g "$gid" "$home/.ssh"
install -m 600 -o "$uid" -g "$gid" "$seed/authorized_keys" "$home/.ssh/authorized_keys"
mkdir -p /run/sshd
ssh-keygen -A
printf '%s\n' "$instance" > /var/lib/flintyard/instance-id.tmp
mv /var/lib/flintyard/instance-id.tmp /var/lib/flintyard/instance-id
`
