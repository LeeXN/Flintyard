# Installation

[简体中文](install.zh-CN.md)

Flintyard needs a Linux host with working KVM. Use the same architecture for the host, Firecracker, jailer, and guest image. [Host and image compatibility](compatibility.md)

## Host prerequisites

- Hardware virtualization exposed through `/dev/kvm`. For a virtualized Linux host, enable nested virtualization in the outer hypervisor.
- 4 KiB kernel pages and a usable memory cgroup controller. Run `getconf PAGESIZE` and `flintyard doctor` to check the selected host.
- Root access to provision networking, disks, and jailer. A host whose firewall or forwarding policy blocks VM traffic may require explicit configuration. [Networking](networking.md)
- `ip`, `nft`, and `mkfs.ext4` for VM operations. On Debian or Ubuntu these are available in `iproute2`, `nftables`, and `e2fsprogs`.
- `mount`, `umount`, `chroot`, and `unshare` for building images. Source-kernel recipes also need a compiler, linker, `make`, and the dependencies of the pinned kernel configuration.
- Go 1.27.3 and `make` to compile Flintyard. The Debian image preparation and quickstart use Python 3, `jq`, `curl`, `sfdisk`, `debugfs`, and the OpenSSH client. [Go downloads](https://go.dev/dl/)

Allow enough space for the image build scratch directory, immutable cache, and an independent writable root disk for each node. On the KVM host, choose a private state directory under a trusted ancestor such as `/var/lib`. The default state root is `/var/lib/flintyard`.

## Build from source

Run the commands from the repository root. Keep the same shell for the installation and quickstart.

```sh
go version
make build

ARCH=$(uname -m)
case "$ARCH" in
  x86_64) GO_ARCH=amd64 ;;
  aarch64) GO_ARCH=arm64 ;;
  *) printf 'Unsupported architecture: %s\n' "$ARCH" >&2; exit 1 ;;
esac
FY="$(pwd -P)/bin/flintyard-linux-$GO_ARCH"
"$FY" version
```

## Install the pinned Firecracker and jailer

Use both binaries from the same official archive and verify its checksum. Extract them to a location where root can access and execute them. [Compatibility lock](../compatibility.lock.json)

```sh
FC_VERSION="$(jq -er .firecracker.version compatibility.lock.json)"
TOOLS="$(pwd -P)/bin/firecracker-$FC_VERSION-$ARCH"
ARCHIVE="$TOOLS/firecracker.tgz"
mkdir -p "$TOOLS"
curl --fail --location --output "$ARCHIVE" \
  "$(jq -er --arg arch "$ARCH" '.firecracker.assets[$arch].url' compatibility.lock.json)" &&
printf '%s  %s\n' \
  "$(jq -er --arg arch "$ARCH" '.firecracker.assets[$arch].sha256' compatibility.lock.json)" \
  "$ARCHIVE" | sha256sum -c &&
tar -xzf "$ARCHIVE" --no-same-owner -C "$TOOLS"

FIRECRACKER="$TOOLS/release-v$FC_VERSION-$ARCH/firecracker-v$FC_VERSION-$ARCH"
JAILER="$TOOLS/release-v$FC_VERSION-$ARCH/jailer-v$FC_VERSION-$ARCH"
sudo "$FY" doctor --mode run --firecracker "$FIRECRACKER" --jailer "$JAILER"
```

Use `doctor --mode build` for an image-building host and `doctor --mode validate` to check the dependencies for temporary image validation. The default report lists each check and its outcome. Use `-o json` for CI. `--uid-range` reserves the numeric UID/GID allocation range for jailed VMMs. Use the same range on subsequent `up` operations if you choose a nondefault range.

Continue with the [single-node quickstart](usage.md#single-node-quickstart).
