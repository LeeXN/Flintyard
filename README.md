# Flintyard

[English](README.en.md) · [安装](docs/install.zh-CN.md) · [使用指南](docs/usage.zh-CN.md) · [CLI 参考](docs/cli.zh-CN.md)

Flintyard 是面向开发机和自托管 CI 的本地 Firecracker 命令行工具。它从 Linux 镜像启动一个或多个微虚机，为每个节点分配独立的可写盘和 IPv4 地址，并在任务结束后清理环境。

## 功能

| 功能 | 用法 |
| --- | --- |
| 宿主诊断 | `doctor` 检查 KVM、运行二进制、权限、网络工具和 cgroup |
| 镜像构建 | `image build` 根据 JSON 配方制作可搬运的 `.fyimg`，使用已有内核或从固定源码和配置编译 |
| 镜像导入与验证 | `image import` 导入包或从本地文件组包。`image inspect` 查看清单和摘要。`image validate` 临时启动并执行探针 |
| 多节点环境 | `up` 根据同一份 JSON 启动多个节点，为每个节点创建独立可写盘和 IPv4 地址 |
| 访问和网络 | `status --probe` 复查 TCP 或 SSH banner。可为环境开启 NAT 和显式 TCP/UDP 端口映射 |
| 状态与日志 | `status`、`list` 和 `logs` 查看当前状态、串口和 Firecracker 启动日志。`-o json` 与 `--events FILE` 便于脚本读取 |
| 停机与清理 | `stop` 保留可写盘供再次 `up`。`down` 销毁可写盘。`gc` 预览并回收已确认归属的残留资源 |

## 快速开始

1. 在 Linux KVM 宿主上编译 CLI，安装 Firecracker 和 jailer，并运行 `doctor`。 [安装指南](docs/install.zh-CN.md)
2. 使用与宿主同架构的 `.fyimg`，或构建一份 Debian 镜像。 [构建 Debian 镜像](fixtures/README.md#debian-原版内核离线镜像)
3. 导入镜像，启动一个节点，通过 SSH 登录，结束后销毁环境。 [单节点快速开始](docs/usage.zh-CN.md#单节点快速开始)

## 在 CI 中运行容器应用

GitLab 示例使用 Flintyard 创建临时微虚机，由 CI 在客户机内通过 Docker 和 K3s 分别部署 Gitea。 [在本机和 GitLab Runner 上运行](docs/container-workload.zh-CN.md)

![GitLab 中的容器应用流水线](docs/images/flintyard-workload-stages.png)

## 文档

- [宿主与镜像兼容性](docs/compatibility.zh-CN.md)
- [启动、登录、停机恢复和销毁](docs/usage.zh-CN.md)
- [镜像导入、配方、内核和验证](docs/images.zh-CN.md)
- [私网、NAT、端口映射和排错](docs/networking.zh-CN.md)
- [CLI 参数、JSON 和退出码](docs/cli.zh-CN.md)
- [配置样例](examples/README.md) 和 [JSON Schema](schemas/)
- [开发、测试和贡献](CONTRIBUTING.zh-CN.md)

## 许可证

Flintyard 采用 [MIT 许可证](LICENSE)。第三方材料的来源和许可见 [第三方声明](THIRD-PARTY-NOTICES.md)。
