# 宿主与镜像兼容性

[English](compatibility.md)

Flintyard 面向 Linux `x86_64`（`amd64`）和 `aarch64`（`arm64`）。使用锁文件指定的官方 Firecracker 和 jailer 版本，安装与宿主同架构的二进制，并在创建虚机前执行 `doctor`。 [兼容性锁文件](../compatibility.lock.json) · [安装指南](install.zh-CN.md)

| 架构 | 运行覆盖范围 |
| --- | --- |
| `x86_64` | 在原生 KVM 上运行 Debian、Ubuntu、Rocky Linux 和 Alpine 镜像。GitLab 容器示例在 x86_64 嵌套 KVM 宿主上运行。 |
| `aarch64` | 在 QEMU 模拟的 aarch64 宿主上完成 Debian 镜像构建、启动、SSH、停机恢复和清理。 |

可启动的镜像需要相互匹配的内核、根文件系统和初始化配置，并在使用模块或 initrd 时提供配套文件。请在部署宿主上运行 `image validate`。 [镜像指南](images.zh-CN.md#验证启动与访问) · [发行版镜像配方](../fixtures/README.md)

处理 `flintyard/v1alpha1` 配置和结果时，请使用与 CLI 同一修订版本的 [JSON Schema](../schemas/)。
