# Host setup and installation

[简体中文](install.zh-CN.md) · [Complete build-to-SSH walkthrough](usage.md#single-node-quickstart)

## Host prerequisites

| Task | Requirements |
| --- | --- |
| Compile Flintyard | Go 1.27.3 or a compatible newer toolchain. Use `go build` for one CLI binary. `make build` uses `make` and produces Linux amd64 and arm64 binaries. |
| Prepare Debian inputs | Python 3.9+, `curl`, `jq`, `sfdisk`, `debugfs`, and xz archive support. See the [quickstart](usage.md#1-prepare-the-host-and-build-flintyard) for package installation commands. |
| Build images | Matching-architecture Linux, root, `mkfs.ext4`, `mount`, `umount`, `chroot`, `unshare`, and any tools declared by the recipe. Image builds do not require KVM. |
| Boot or validate guests | `x86_64` or `aarch64` Linux, root, `/dev/kvm`, TAP support through `/dev/net/tun`, 4 KiB host pages, a memory cgroup controller, `ip`, `nft`, and `mkfs.ext4`. Firecracker, jailer, and the image must match the host architecture. |
| Log in over SSH | An OpenSSH client, the matching private key, and a running guest SSH service reachable from the client. |

A virtualized host needs nested KVM exposed by its hypervisor. Plan for downloads, build scratch space, the expanded image cache, and a separate writable disk for each node. [Compatibility and validation coverage](compatibility.md)

## Build from source

Build for the current host from the repository root and install to `/opt/flintyard/bin/flintyard`. For distribution, `make build` produces `bin/flintyard-linux-amd64` and `bin/flintyard-linux-arm64`. `make package` produces tarballs and checksums in `dist/`. [Build and install commands](usage.md#1-prepare-the-host-and-build-flintyard)

## Install the pinned Firecracker and jailer

Use the official archive for the architecture and version in the [compatibility lock](../compatibility.lock.json). Verify the archive checksum, then install both binaries to `/opt/flintyard/bin/`. When running as root, the binaries and their ancestors must be controlled by root and protected from replacement by ordinary users. [Download, verify, and install](usage.md#2-install-matching-firecracker-and-jailer-binaries)

## Choose the state directory and check the host

The default state directory is `/var/lib/flintyard`. The walkthrough uses `/var/lib/flintyard-tutorial`. When using a larger disk, choose its `--state-dir` from the beginning and use the same path for every operation on the environment. Use a real path under trusted ancestors, with no symlinks or parent directories writable by ordinary users. Keep the state directory at mode `0700`.

```sh
sudo /opt/flintyard/bin/flintyard doctor --mode run \
  --firecracker /opt/flintyard/bin/firecracker --jailer /opt/flintyard/bin/jailer
sudo /opt/flintyard/bin/flintyard doctor --mode build
```

Use `--mode validate` in place of `--mode run` when checking for temporary image validation. Inspect any failed checks before proceeding. Use `-o json` in scripts. Pass the same nondefault `--uid-range` to `doctor`, `up`, and `image validate`.

Next, [build or prepare an image](usage.md#3-build-the-debian-image-or-prepare-an-existing-image).
