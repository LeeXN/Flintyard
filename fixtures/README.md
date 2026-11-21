# 发行版镜像配方

[English](README.en.md)

这些配方从 Ubuntu、Debian、Rocky Linux 和 Alpine 的固定输入构建 Flintyard 镜像。准备工具需要 Linux 和 Python 3.9 或更新版本。构建时使用安装指南中的 `FY`。将 `TMPDIR` 指向空间足够的可写目录，以容纳下载和输入文件。需要将缓存放到其他磁盘时，设置由 root 管理的 `STATE` 路径。 [安装](../docs/install.zh-CN.md) · [镜像输入锁文件](lock.json) · [兼容性](../docs/compatibility.zh-CN.md)

## 准备根文件系统

在仓库根目录执行。选择 `ubuntu`、`debian`、`rocky` 或 `alpine`，脚本按当前架构从锁文件取得并校验输入。Ubuntu 原始归档中的设备节点按锁文件规范化，启动内核需要提供 devtmpfs。

```sh
ARCH="$(uname -m)"
FAMILY=ubuntu
ROOT_WORK="$(mktemp -d)"
curl --fail --location --output "$ROOT_WORK/source.archive" \
  "$(jq -er --arg family "$FAMILY" --arg arch "$ARCH" '.families[$family].arches[$arch].url' fixtures/lock.json)"
python3 fixtures/prepare.py "$FAMILY" "$ARCH" --input "$ROOT_WORK/source.archive" \
  --output "$ROOT_WORK/$FAMILY-$ARCH.tar.gz"
```

## 从内核源码构建

以下用准备好的 Ubuntu 根文件系统和锁定的上游 Linux 源码构建镜像。先安装配方所需的编译工具，再在同架构宿主上生成配置。最小 Debian、Rocky Linux 和 Alpine 根文件系统使用 `--modules disabled`。未预先准备本地软件包的配方需要访问其软件包仓库。 [构建要求与配方选项](../docs/images.zh-CN.md#构建可复用镜像)

```sh
KERNEL_WORK="$(mktemp -d)"
KERNEL_RELEASE="$(jq -er .kernel.release fixtures/lock.json)"
KERNEL_ARCHIVE="$KERNEL_WORK/linux-$KERNEL_RELEASE.tar.xz"
curl --fail --location --output "$KERNEL_ARCHIVE" "$(jq -er .kernel.url fixtures/lock.json)" &&
printf '%s  %s\n' "$(jq -er .kernel.sha256 fixtures/lock.json)" "$KERNEL_ARCHIVE" | sha256sum -c &&
tar -xJf "$KERNEL_ARCHIVE" -C "$KERNEL_WORK"
```

以下用生成的配置和准备好的 Ubuntu 根文件系统构建镜像。

```sh
python3 fixtures/configure-kernel.py --source "$KERNEL_WORK/linux-$KERNEL_RELEASE" \
  --architecture "$ARCH" --output "$KERNEL_WORK/ubuntu.config" --modules enabled
python3 fixtures/render.py ubuntu "$ARCH" --rootfs "$ROOT_WORK/ubuntu-$ARCH.tar.gz" \
  --kernel "$KERNEL_ARCHIVE" --config "$KERNEL_WORK/ubuntu.config" \
  --output "$ROOT_WORK/ubuntu.fyimg" --recipe "$ROOT_WORK/ubuntu.recipe.json"
sudo "$FY" image build --state-dir "${STATE:-/var/lib/flintyard}" --offline --timeout 150m \
  --recipe "$ROOT_WORK/ubuntu.recipe.json"
```

## Debian 原版内核离线镜像

在仓库根目录执行。准备工具使用 `sfdisk` 和 `debugfs`，对照锁文件校验 Debian 云归档并提取配套的根盘、原版内核和 initrd。准备完成后，可将 `DEBIAN_INPUTS` 交给容器示例。 [Debian 输入锁文件](debian-cloud.lock.json) · [容器示例](../docs/container-workload.zh-CN.md)

```sh
ARCH="$(uname -m)"
DEMO="$(mktemp -d)"
curl --fail --location --output "$DEMO/debian-cloud.tar.xz" \
  "$(jq -er --arg arch "$ARCH" '.architectures[$arch].url' fixtures/debian-cloud.lock.json)"
python3 fixtures/debian-cloud.py "$ARCH" --archive "$DEMO/debian-cloud.tar.xz" \
  --output-dir "$DEMO/inputs"
DEBIAN_INPUTS="$DEMO/inputs"
```

需要得到 `.fyimg` 时，用准备好的输入生成并执行离线配方。

```sh
RECIPE="$DEMO/debian.recipe.json"
python3 fixtures/render-debian-cloud.py "$ARCH" --input-dir "$DEBIAN_INPUTS" \
  --output "$DEMO/debian.fyimg" --recipe "$RECIPE"
sudo "$FY" image build --state-dir "${STATE:-/var/lib/flintyard}" --offline --timeout 20m \
  -o json --recipe "$RECIPE" \
  > "$DEMO/build.json"
IMAGE="$DEMO/debian.fyimg"
EXPECTED_SHA256="$(jq -er '.data.package_sha256' "$DEMO/build.json")"
```

使用 `IMAGE` 和 `EXPECTED_SHA256` 启动虚机。 [单节点快速开始](../docs/usage.zh-CN.md#单节点快速开始)

## Ubuntu 原版内核

先准备 Ubuntu 根文件系统，再从锁文件下载与该架构匹配的内核和模块包。准备宿主需要 `dpkg-deb`。脚本校验包摘要并提取文件，配方在构建根中生成 initrd。 [Ubuntu 内核锁文件](ubuntu-original.lock.json)

```sh
curl --fail --location --output "$ROOT_WORK/linux-image.deb" \
  "$(jq -er --arg arch "$ARCH" '.architectures[$arch].image_package.url' fixtures/ubuntu-original.lock.json)"
curl --fail --location --output "$ROOT_WORK/linux-modules.deb" \
  "$(jq -er --arg arch "$ARCH" '.architectures[$arch].modules_package.url' fixtures/ubuntu-original.lock.json)"
python3 fixtures/ubuntu-original.py "$ARCH" \
  --base-rootfs "$ROOT_WORK/ubuntu-$ARCH.tar.gz" \
  --image-deb "$ROOT_WORK/linux-image.deb" --modules-deb "$ROOT_WORK/linux-modules.deb" \
  --output-dir "$ROOT_WORK/ubuntu-original-inputs"
python3 fixtures/render-ubuntu-original.py "$ARCH" --input-dir "$ROOT_WORK/ubuntu-original-inputs" \
  --output "$ROOT_WORK/ubuntu-original.fyimg" --recipe "$ROOT_WORK/ubuntu-original.recipe.json"
sudo "$FY" image build --state-dir "${STATE:-/var/lib/flintyard}" --offline --timeout 20m \
  --recipe "$ROOT_WORK/ubuntu-original.recipe.json"
```

## Rocky Linux 原版内核

先准备 Rocky Linux 根文件系统，再按锁文件下载对应架构的 RPM 和签名密钥。设置 `ROCKY_RPMS` 为 RPM 目录，`ROCKY_KEY` 为密钥文件路径。准备脚本使用 `rpmkeys`、`rpm2cpio` 和 `cpio` 校验并展开软件包。配方从本地 RPM 安装软件、生成 initrd 并启用 virtio PCI。 [内核锁文件](rocky-original-kernel.lock.json) · [RPM 锁文件](rocky-packages.lock.json)

```sh
python3 fixtures/rocky-original.py "$ARCH" \
  --rootfs "$ROOT_WORK/rocky-$ARCH.tar.gz" --package-dir "$ROCKY_RPMS" --key "$ROCKY_KEY" \
  --output-dir "$ROOT_WORK/rocky-inputs" --image "$ROOT_WORK/rocky.fyimg" \
  --recipe "$ROOT_WORK/rocky.recipe.json"
sudo "$FY" image build --state-dir "${STATE:-/var/lib/flintyard}" --offline --timeout 20m \
  --recipe "$ROOT_WORK/rocky.recipe.json"
```

## Alpine 离线软件包

准备 Alpine 根文件系统，再执行内核源码下载和解包命令。 [内核源码](#从内核源码构建)

将锁定的软件包与索引打包进根文件系统，并用禁用模块的内核配置生成离线配方。 [Alpine 软件包锁文件](alpine-packages.lock.json)

```sh
python3 fixtures/fetch-alpine.py "$ARCH" --output-dir "$ROOT_WORK/alpine-packages"
python3 fixtures/alpine-bundle.py "$ARCH" --rootfs "$ROOT_WORK/alpine-$ARCH.tar.gz" \
  --package-dir "$ROOT_WORK/alpine-packages" --output "$ROOT_WORK/alpine-$ARCH-offline.tar.gz"
python3 fixtures/configure-kernel.py --source "$KERNEL_WORK/linux-$KERNEL_RELEASE" \
  --architecture "$ARCH" --output "$KERNEL_WORK/alpine.config" --modules disabled
python3 fixtures/render.py alpine "$ARCH" --rootfs "$ROOT_WORK/alpine-$ARCH-offline.tar.gz" \
  --kernel "$KERNEL_ARCHIVE" --config "$KERNEL_WORK/alpine.config" \
  --output "$ROOT_WORK/alpine.fyimg" --recipe "$ROOT_WORK/alpine.recipe.json"
sudo "$FY" image build --state-dir "${STATE:-/var/lib/flintyard}" --offline --timeout 150m \
  --recipe "$ROOT_WORK/alpine.recipe.json"
```
