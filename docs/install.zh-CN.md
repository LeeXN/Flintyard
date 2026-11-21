# 安装

[English](install.md)

Flintyard 的运行宿主需要可用的 KVM。宿主、Firecracker、jailer 和客户机镜像的架构必须一致。 [宿主与镜像兼容性](compatibility.zh-CN.md)

## 宿主要求

- 硬件虚拟化通过 `/dev/kvm` 提供。如果宿主本身是虚机，在外层 Hypervisor 上开启嵌套虚拟化。
- 内核页大小为 4 KiB，内存 cgroup 控制器可用。用 `getconf PAGESIZE` 和 `flintyard doctor` 检查。
- 使用 root 配置网络、磁盘和 jailer。按宿主的防火墙及转发策略配置客户机流量。 [网络指南](networking.zh-CN.md)
- 运行虚机需要 `ip`、`nft` 和 `mkfs.ext4`。在 Debian 或 Ubuntu 上分别由 `iproute2`、`nftables` 和 `e2fsprogs` 提供。
- 构建镜像需要 `mount`、`umount`、`chroot` 和 `unshare`。源码内核配方还需要编译器、链接器、`make` 和锁定内核配置所需的依赖。
- 编译 Flintyard 需要 Go 1.27.3 和 `make`。准备 Debian 镜像和快速开始使用 Python 3、`jq`、`curl`、`sfdisk`、`debugfs` 和 OpenSSH 客户端。 [Go 下载](https://go.dev/dl/)

为构建临时目录、只读镜像缓存和每个节点的独立可写盘预留磁盘空间。状态目录应位于可信父目录下，例如 `/var/lib`，默认路径为 `/var/lib/flintyard`。

## 从源码构建

在仓库根目录执行。后续安装与快速开始请使用同一个 shell。

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

## 安装锁定版本的 Firecracker 与 jailer

从同一个官方归档取得两个二进制，并使用锁文件中的摘要校验归档。解包位置须允许 root 访问和执行。 [兼容性锁文件](../compatibility.lock.json)

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

`doctor --mode build` 检查镜像构建依赖，`doctor --mode validate` 检查临时虚机验证依赖。默认报告显示每项检查及结果，CI 可用 `-o json`。如需调整 jailed VMM 的数字 UID/GID 分配范围，使用 `--uid-range`。后续运行 `up` 时请保持相同的范围。

下一步可运行 [单节点快速开始](usage.zh-CN.md#单节点快速开始)。
