# 参与 Flintyard 开发

[English](CONTRIBUTING.md)

反馈问题时，请在 GitHub Issue 中提供 CLI 版本、宿主架构与内核、Firecracker 和 jailer 版本以及实际命令。附上最小配置、相关 JSON 错误或简短日志，并移除凭据及私有基础设施信息。

## 本地检查

使用 Go 1.27.3、Python 3 和支持竞态测试的 C 工具链。在仓库根目录运行以下命令。

```sh
python3 -m unittest discover -s scripts -p 'test_*.py'
python3 scripts/smoke.py
```

冒烟检查涵盖代码格式、Schema、文档链接、Go 测试和双架构构建。修改虚机创建或清理逻辑时，请在 KVM 宿主上运行容器示例。 [宿主安装](docs/install.zh-CN.md) · [容器示例](docs/container-workload.zh-CN.md)

## 仓库布局

- `cmd/flintyard/` 包含 CLI 和输出逻辑。
- `pkg/flintyard/` 提供 CLI 调用的本地操作。
- `internal/` 包含 Firecracker API 客户端、镜像和构建工具、Linux 网络、分配与生命周期状态。
- `schemas/`、`examples/` 和 `fixtures/` 包含协议、样例和可复现输入准备。
- `tests/` 包含外部验证，例如客户机 SSH 检查。

保持 CLI、核心接口、Schema 和样例一致。修改运行行为时，记录实际宿主架构、运行版本、命令和结果，并分别记录模拟与实体硬件验证。

提交聚焦的 Pull Request，附简明说明和验证结果。为现有测试未覆盖的回归补充验证，尤其是生命周期、镜像完整性与清理相关修改。
