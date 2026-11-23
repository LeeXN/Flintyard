# 宿主准备与安装

[English](install.md) · [从构建到登录的完整教程](usage.zh-CN.md#单节点快速开始)

## 宿主要求

| 任务 | 要求 |
| --- | --- |
| 编译 Flintyard | Go 1.27.3 或兼容的更新工具链。构建单个 CLI 使用 `go build`。`make build` 另需 `make`，会生成 Linux amd64 和 arm64 二进制。 |
| 准备 Debian 输入 | Python 3.9+、`curl`、`jq`、`sfdisk`、`debugfs` 和可读取 xz 归档的环境。系统包安装命令见 [快速开始](usage.zh-CN.md#1-准备宿主并构建二进制)。 |
| 构建镜像 | 同架构 Linux、root、`mkfs.ext4`、`mount`、`umount`、`chroot`、`unshare` 以及配方声明的工具。镜像制作本身无需 KVM。 |
| 启动或验证虚机 | `x86_64` 或 `aarch64` Linux、root、`/dev/kvm`、`/dev/net/tun` 的 TAP 支持、4 KiB 宿主页、内存 cgroup 控制器，以及 `ip`、`nft`、`mkfs.ext4`。Firecracker、jailer 和镜像必须匹配宿主架构。 |
| 通过 SSH 登录 | OpenSSH 客户端、对应私钥，以及客户机中已运行并可达的 SSH 服务。 |

在虚拟化宿主上运行时，需要由外层 Hypervisor 暴露嵌套 KVM。为下载、构建临时空间、展开后的缓存和各节点的独立可写盘预留空间。 [兼容性与已有验证范围](compatibility.zh-CN.md)

## 从源码构建

从仓库根目录编译本机版本并安装到 `/opt/flintyard/bin/flintyard`。需要分发时，运行 `make build` 生成 `bin/flintyard-linux-amd64` 和 `bin/flintyard-linux-arm64`，或运行 `make package` 生成 `dist/` 下的压缩包及摘要。 [构建和安装命令](usage.zh-CN.md#1-准备宿主并构建二进制)

## 安装锁定版本的 Firecracker 与 jailer

使用 [兼容性锁文件](../compatibility.lock.json) 中相同版本、相同架构的官方归档。先查验摘要，再将两个二进制安装到 `/opt/flintyard/bin/`。以 root 运行时，二进制和祖先目录必须由 root 管理，且不能被普通用户替换。 [下载、校验和安装命令](usage.zh-CN.md#2-安装匹配的-firecracker-和-jailer)

## 选择状态目录并预检

CLI 默认状态目录是 `/var/lib/flintyard`。主教程使用独立的 `/var/lib/flintyard-tutorial`。迁到容量更大的磁盘时，从开始就让所有命令使用同一个 `--state-dir`。路径使用真实目录，避免软链接或可由普通用户写入的祖先目录。状态目录权限应为 `0700`。

```sh
sudo /opt/flintyard/bin/flintyard doctor --mode run \
  --firecracker /opt/flintyard/bin/firecracker --jailer /opt/flintyard/bin/jailer
sudo /opt/flintyard/bin/flintyard doctor --mode build
```

排查临时虚机验证时，可将第一条命令的 `--mode run` 改为 `--mode validate`。诊断检查失败时查看对应项，脚本可加 `-o json`。使用非默认 `--uid-range` 时，在诊断、`up` 和 `image validate` 中传相同值。

下一步 [制作或准备镜像](usage.zh-CN.md#3-制作-debian-镜像或准备已有镜像)。
