# 发行版镜像配方

[English](README.en.md) · [镜像构建参考](../docs/images.zh-CN.md)

在仓库根目录运行本页命令。准备输入需要 Linux 和 Python 3.9+。执行构建前，先将 Flintyard 安装到 `/opt/flintyard/bin/flintyard`，构建宿主需和镜像同架构。每个命令块重新定义自己的变量，步骤之间只通过下文列出的文件衔接。配方、准备目录及镜像使用新的输出路径。 [构建 CLI](../docs/usage.zh-CN.md#1-准备宿主并构建二进制) · [输入锁文件](lock.json) · [宿主兼容性](../docs/compatibility.zh-CN.md)

| 目标 | 顺序 |
| --- | --- |
| Debian 原版内核 | [教程的 Debian 离线构建](#debian-原版内核离线镜像)。 |
| Ubuntu 原版内核 | [准备 Ubuntu 根文件系统](#准备根文件系统) → [Ubuntu 原版内核](#ubuntu-原版内核)。 |
| Rocky Linux 原版内核 | [准备 Rocky 根文件系统](#准备根文件系统) → [Rocky Linux 原版内核](#rocky-linux-原版内核)。 |
| Ubuntu 源码内核 | [准备 Ubuntu 根文件系统](#准备根文件系统) → [从内核源码构建](#从内核源码构建)。 |
| Alpine 离线软件包与源码内核 | [准备 Alpine 根文件系统](#准备根文件系统) → [下载锁定的内核源码](#下载锁定的内核源码) → [Alpine 离线软件包](#alpine-离线软件包)。 |

## Debian 原版内核离线镜像

主教程包含完整下载、校验、提取、配方生成和构建指令。只需要预先准备容器示例的输入时，完成第一段下载与提取即可，产物位于 `.local/quickstart/debian-inputs/`。要制作可运行的 `.fyimg`，继续执行该节构建命令。 [教程的 Debian 构建步骤](../docs/usage.zh-CN.md#3-制作-debian-镜像或准备已有镜像) · [Debian 输入锁文件](debian-cloud.lock.json) · [容器示例](../docs/container-workload.zh-CN.md)

## 准备根文件系统

将 `family=ubuntu` 改为所需的 `ubuntu`、`rocky`、`alpine` 或 `debian`。这个步骤输出 `.local/fixtures/<family>/rootfs.tar.gz`。脚本校验下载的归档并按锁文件规范化。Ubuntu 根盘启动时需要内核提供 devtmpfs。

```sh
(
  set -eu
  family=ubuntu
  arch=$(uname -m)
  work=".local/fixtures/$family"
  mkdir -p "$work"
  url=$(jq -er --arg family "$family" --arg arch "$arch" \
    '.families[$family].arches[$arch].url' fixtures/lock.json)
  curl --fail --location --output "$work/source.archive" "$url"
  python3 fixtures/prepare.py "$family" "$arch" \
    --input "$work/source.archive" --output "$work/rootfs.tar.gz"
)
```

## Ubuntu 原版内核

准备好 `.local/fixtures/ubuntu/rootfs.tar.gz`。宿主需有 `dpkg-deb`。准备脚本校验并提取内核和模块包，构建时生成 initrd。 [Ubuntu 内核锁文件](ubuntu-original.lock.json)

```sh
(
  set -eu
  arch=$(uname -m)
  work=.local/fixtures/ubuntu-original
  mkdir -p "$work"
  image_url=$(jq -er --arg arch "$arch" '.architectures[$arch].image_package.url' \
    fixtures/ubuntu-original.lock.json)
  modules_url=$(jq -er --arg arch "$arch" '.architectures[$arch].modules_package.url' \
    fixtures/ubuntu-original.lock.json)
  curl --fail --location --output "$work/linux-image.deb" "$image_url"
  curl --fail --location --output "$work/linux-modules.deb" "$modules_url"
  python3 fixtures/ubuntu-original.py "$arch" --base-rootfs .local/fixtures/ubuntu/rootfs.tar.gz \
    --image-deb "$work/linux-image.deb" --modules-deb "$work/linux-modules.deb" \
    --output-dir "$work/inputs"
  python3 fixtures/render-ubuntu-original.py "$arch" --input-dir "$work/inputs" \
    --output "$work/ubuntu.fyimg" --recipe "$work/build.recipe.json"
  sudo /opt/flintyard/bin/flintyard image build --state-dir /var/lib/flintyard-tutorial \
    --offline --timeout 20m --recipe "$work/build.recipe.json" -o json > "$work/build.json"
)
```

## Rocky Linux 原版内核

准备好 `.local/fixtures/rocky/rootfs.tar.gz`。安装 `rpmkeys`、`rpm2cpio` 和 `cpio`。从锁文件下载 RPM 与签名密钥，准备脚本会校验包摘要及签名。配方使用本地 RPM、生成 initrd 并启用 virtio PCI。 [内核锁文件](rocky-original-kernel.lock.json) · [RPM 锁文件](rocky-packages.lock.json)

```sh
(
  set -eu
  arch=$(uname -m)
  work=.local/fixtures/rocky-original
  mkdir -p "$work/rpms"
  key_url=$(jq -er '.signing_key_url' fixtures/rocky-original-kernel.lock.json)
  curl --fail --location --output "$work/RPM-GPG-KEY-Rocky-9" "$key_url"
  jq -er --arg arch "$arch" '.packages[$arch] | to_entries[] | [.key, .value.url] | @tsv' \
    fixtures/rocky-packages.lock.json > "$work/rpms.tsv"
  while IFS="$(printf '\t')" read -r filename url; do
    curl --fail --location --output "$work/rpms/$filename" "$url"
  done < "$work/rpms.tsv"
  python3 fixtures/rocky-original.py "$arch" --rootfs .local/fixtures/rocky/rootfs.tar.gz \
    --package-dir "$work/rpms" --key "$work/RPM-GPG-KEY-Rocky-9" \
    --output-dir "$work/inputs" --image "$work/rocky.fyimg" --recipe "$work/build.recipe.json"
  sudo /opt/flintyard/bin/flintyard image build --state-dir /var/lib/flintyard-tutorial \
    --offline --timeout 20m --recipe "$work/build.recipe.json" -o json > "$work/build.json"
)
```

## 从内核源码构建

Ubuntu 示例使用已准备的 `.local/fixtures/ubuntu/rootfs.tar.gz`。先安装源码配方所需的本机编译工具，再下载锁定的上游源码。 [源码编译工具与配方选项](../docs/images.zh-CN.md#构建可复用镜像)

### 下载锁定的内核源码

供 Ubuntu 和 Alpine 的源码内核构建复用。以下命令保留源码归档 `.local/fixtures/kernel/linux.tar.xz`，并解包到同一目录的 `linux-<发行版本>/`。

```sh
(
  set -eu
  work=.local/fixtures/kernel
  mkdir -p "$work"
  url=$(jq -er '.kernel.url' fixtures/lock.json)
  digest=$(jq -er '.kernel.sha256' fixtures/lock.json)
  curl --fail --location --output "$work/linux.tar.xz" "$url"
  printf '%s  %s\n' "$digest" "$work/linux.tar.xz" | sha256sum -c
  tar -xJf "$work/linux.tar.xz" -C "$work"
)
```

### 生成 Ubuntu 源码内核配方并构建

```sh
(
  set -eu
  arch=$(uname -m)
  release=$(jq -er '.kernel.release' fixtures/lock.json)
  work=.local/fixtures/ubuntu-source
  mkdir -p "$work"
  python3 fixtures/configure-kernel.py --source ".local/fixtures/kernel/linux-$release" \
    --architecture "$arch" --output "$work/kernel.config" --modules enabled
  python3 fixtures/render.py ubuntu "$arch" --rootfs .local/fixtures/ubuntu/rootfs.tar.gz \
    --kernel .local/fixtures/kernel/linux.tar.xz --config "$work/kernel.config" \
    --output "$work/ubuntu.fyimg" --recipe "$work/build.recipe.json"
  sudo /opt/flintyard/bin/flintyard image build --state-dir /var/lib/flintyard-tutorial \
    --offline --timeout 150m --recipe "$work/build.recipe.json" -o json > "$work/build.json"
)
```

最小 Debian、Rocky Linux 和 Alpine 根盘直接走通用的 `render.py` 源码流程时，选择 `--modules disabled`，除非已为构建前的根盘配好 `depmod`。需要在构建中访问软件包仓库的配方应允许网络。Alpine 下面的方案已经把包准备到本地。

## Alpine 离线软件包

先准备 `.local/fixtures/alpine/rootfs.tar.gz` 和 [内核源码](#下载锁定的内核源码)。下载已锁定的包和索引并打入根文件系统，使用禁用模块的内核配置。下载阶段需要网络，后续构建可离线。 [Alpine 软件包锁文件](alpine-packages.lock.json)

```sh
(
  set -eu
  arch=$(uname -m)
  release=$(jq -er '.kernel.release' fixtures/lock.json)
  work=.local/fixtures/alpine-offline
  mkdir -p "$work"
  python3 fixtures/fetch-alpine.py "$arch" --output-dir "$work/packages"
  python3 fixtures/alpine-bundle.py "$arch" --rootfs .local/fixtures/alpine/rootfs.tar.gz \
    --package-dir "$work/packages" --output "$work/rootfs-offline.tar.gz"
  python3 fixtures/configure-kernel.py --source ".local/fixtures/kernel/linux-$release" \
    --architecture "$arch" --output "$work/kernel.config" --modules disabled
  python3 fixtures/render.py alpine "$arch" --rootfs "$work/rootfs-offline.tar.gz" \
    --kernel .local/fixtures/kernel/linux.tar.xz --config "$work/kernel.config" \
    --output "$work/alpine.fyimg" --recipe "$work/build.recipe.json"
  sudo /opt/flintyard/bin/flintyard image build --state-dir /var/lib/flintyard-tutorial \
    --offline --timeout 150m --recipe "$work/build.recipe.json" -o json > "$work/build.json"
)
```

构建成功后，用相应 `build.json` 中的 `data.image_id` 配置同一状态目录里的虚机。要转移到其他宿主，带上 `.fyimg` 和 `data.package_sha256`，再按 [导入步骤](../docs/images.zh-CN.md#导入现有镜像) 导入。
