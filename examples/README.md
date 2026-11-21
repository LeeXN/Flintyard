# JSON 配置与结果样例

[English](README.en.md)

这些 JSON 文件可作为镜像配方、环境配置和 CLI 结果的样例。启动虚机前，替换示例路径、镜像 ID、摘要和公钥。 [使用指南](../docs/usage.zh-CN.md)

| 文件 | 对应协议 |
| --- | --- |
| [image-manifest.json](image-manifest.json) | 镜像包中的 manifest |
| [import-spec.json](import-spec.json) | 由本地文件组装镜像包 |
| [build-prebuilt.json](build-prebuilt.json) | 使用已有内核构建根盘 |
| [build-kernel-source.json](build-kernel-source.json) | aarch64 源码内核、模块与 initrd 构建 |
| [environment.json](environment.json) | x86_64 三节点独立私网 |
| [environment-aarch64.json](environment-aarch64.json) | aarch64 三节点独立私网 |
| [environment-nat.json](environment-nat.json) | NAT 与本机 loopback 映射 |
| [result-up.json](result-up.json) | 可访问环境的结构化结果 |
| [result-failed.json](result-failed.json) | 探针超时及清理结果 |
| [event.json](event.json) | 单条进度事件 |

填写实际输入的内核版本和 SHA-256。NAT 样例中的 `192.0.2.53` 和 `eth0` 应替换为客户机可达的解析器和宿主实际的出口网卡。私网样例保留空 DNS 列表。

源码配方中的配置、源码归档和 initrd 路径应指向配套文件。使用 `.tar.xz` 根文件系统归档时，请声明并安装 `xz`。 [镜像指南](../docs/images.zh-CN.md)
