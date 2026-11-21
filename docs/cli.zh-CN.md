# CLI 参考

[English](cli.md)

选项写在位置参数之前。运行 `flintyard COMMAND --help` 可查看该命令的完整选项。通用参数包括 `--state-dir DIR`（默认 `/var/lib/flintyard`）、`--timeout DURATION` 和 `-o text|json`。超时使用 Go 时长格式，例如 `180s` 或 `5m`。

| 命令 | 用途 |
| --- | --- |
| `version` | 查看 Flintyard、协议和构建信息。 |
| `doctor --mode run\|build\|validate` | 检查宿主和所选二进制。 |
| `image build --recipe FILE [--offline]` | 执行构建配方、导出镜像包并写入缓存。 |
| `image import --sha256 DIGEST PACKAGE` | 校验并导入本地镜像包。 |
| `image import --spec FILE --output PACKAGE` | 从本地文件组装并导入新镜像包。 |
| `image inspect IMAGE` | 查看包文件或已导入的镜像 ID。 |
| `image validate --public-key FILE IMAGE` | 启动临时虚机并执行配置的访问探针。 |
| `up --file FILE` | 创建或恢复一个环境。 |
| `status [--probe] ENV` | 查看环境状态，可再次执行访问探针。 |
| `list [--probe]` | 列出当前状态目录中的环境。 |
| `logs --node NAME [--source serial\|firecracker] ENV` | 查看节点的启动日志。 |
| `stop ENV` | 停止 VMM，保留可写盘和节点身份。 |
| `down [--purge] ENV` | 删除环境的可写盘和所属资源，可一并删除诊断记录。 |
| `gc --dry-run --plan FILE` | 为已确认的残留资源生成清理计划。 |
| `gc --apply --plan FILE` | 重新核对并执行仍有效的清理计划。 |

## 常用运行选项

- `--firecracker PATH`、`--jailer PATH` 和 `--uid-range START:END` 指定运行二进制和 jail 用户范围，适用于 `doctor`、`up` 和 `image validate`。
- `up --wait configured` 使用环境中配置的探针。可通过 `--wait tcp` 或 `--wait ssh_banner` 指定探针。使用 `--wait none` 时，虚机启动后直接返回。
- `up` 和 `image validate` 的 `--keep-failed` 可保留已确认的资源供排查。检查完成后运行 `down`。
- `logs --tail N` 读取最后 N 行，默认 200 行。`logs --follow` 以文本模式持续向标准输出写入日志，可显式设置 `--timeout`。

## 输出与事件

CLI 默认输出可读文本。`list` 显示环境表格，`status` 显示环境、节点和端口映射。文本模式下日志按原样输出，错误信息写到标准错误。

以下示例沿用快速开始中的 `FY`、`STATE` 和 `demo` 环境。 [单节点快速开始](usage.zh-CN.md#单节点快速开始)

```sh
sudo "$FY" list --state-dir "$STATE"
sudo "$FY" status --state-dir "$STATE" --probe demo
sudo "$FY" status --state-dir "$STATE" --probe -o json demo | jq -er '.data.nodes[] | select(.access.probe_passed) | .access.host'
```

使用 `-o json` 时，标准输出包含一个 JSON 结果，字段为 `api_version`、`operation_id`、`ok`、`data` 和 `error`。脚本应分别捕获标准输出和标准错误。旧的 `--json` 仍可使用。脚本可根据 `error.code` 判断错误类型，诊断时查看 `error.message`。失败结果的 `data` 仍可能包含资源状态和清理详情。

使用 `--events FILE` 记录 JSON Lines 事件，其中包含操作 ID、序号、阶段以及相关节点或资源身份。 [结果 Schema](../schemas/result-v1.schema.json) · [事件 Schema](../schemas/event-v1.schema.json) · [JSON 样例](../examples/README.md)

| 退出码 | 含义 |
| --- | --- |
| 0 | 成功 |
| 2 | 命令用法或配置错误 |
| 3 | 依赖、权限、版本或架构不满足要求 |
| 4 | 镜像、构建、完整性、存储或本机 I/O 出错 |
| 5 | 启动、API 就绪或访问探针失败 |
| 6 | 状态、锁、端口、防火墙或资源身份冲突 |
| 7 | 清理不完整 |
| 130 | 操作被中断 |

清理失败时，可能优先返回清理错误。查看资源状态和诊断信息后，参考 [回收残留资源](usage.zh-CN.md#回收残留资源)。

## 访问状态

探针可配置为 TCP、SSH banner 或禁用。`status` 可显示已有探针记录，标注为 `last passed` 或 `last failed`。用 `status --probe` 重新检查。SSH banner 通过表示 SSH 服务可达，实际登录使用外部 SSH 客户端和私钥。JSON 中的 `observed_at`、`last_probe_at` 和 `stale` 表明状态及探针结果何时取得。
