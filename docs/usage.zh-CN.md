# 启动和管理环境

[English](usage.md)

环境是同一台宿主上的一组 Firecracker 微虚机，具有独立的环境名称。环境名称和镜像 ID 在各自的状态目录内有效。对同一环境执行操作时，请使用相同的 `--state-dir`。

## 单节点快速开始

完成 [安装](install.zh-CN.md)，使用安装指南中的 `FY`、`FIRECRACKER` 和 `JAILER` 绝对路径。准备采用 `seed-v1` 初始化的镜像，将 `IMAGE` 设为镜像包路径，将 `EXPECTED_SHA256` 设为通过可信渠道获得的摘要。可根据 [锁定的 Debian 输入](../fixtures/README.md#debian-原版内核离线镜像) 构建示例镜像。

如果 `/var/lib` 空间不足，将 `STATE_PARENT` 设为已有且由 root 管理的目录，所在文件系统应能容纳镜像缓存和客户机可写盘。

```sh
: "${FY:?set FY}" "${FIRECRACKER:?set FIRECRACKER}" "${JAILER:?set JAILER}"
: "${IMAGE:?set IMAGE}" "${EXPECTED_SHA256:?set EXPECTED_SHA256}"
ARCH="$(uname -m)"
WORK="$(mktemp -d)"
STATE_PARENT="${STATE_PARENT:-/var/lib}"
STATE="$(sudo mktemp -d "$STATE_PARENT/flintyard-demo.XXXXXXXX")"
ssh-keygen -q -t ed25519 -N '' -f "$WORK/id_ed25519"

sudo "$FY" image import --state-dir "$STATE" --sha256 "$EXPECTED_SHA256" \
  -o json "$IMAGE" > "$WORK/image.json"

jq -n --arg arch "$ARCH" --arg image "$(jq -er '.data.image_id' "$WORK/image.json")" \
  --arg key "$WORK/id_ed25519.pub" '{
    api_version: "flintyard/v1alpha1", kind: "Environment", name: "demo",
    architecture: $arch, network: {cidr: "auto", nat: false, dns: []},
    nodes: [{name: "node-1", image: $image, vcpu_count: 2, memory_mib: 1024}],
    access: {public_key: $key, probe: {type: "ssh_banner", port: 22}},
    port_forwards: []
  }' > "$WORK/environment.json"

sudo "$FY" up --state-dir "$STATE" --timeout 5m --file "$WORK/environment.json" \
  --firecracker "$FIRECRACKER" --jailer "$JAILER"
sudo "$FY" status --state-dir "$STATE" --probe -o json demo > "$WORK/status.json"
ACCESS=$(jq -ce 'select(.ok) | .data.nodes[0].access | select(.probe_passed)' "$WORK/status.json")
SSH_TARGET=$(printf '%s\n' "$ACCESS" | jq -er '.username + "@" + .host')
SSH_PORT=$(printf '%s\n' "$ACCESS" | jq -er .port)
ssh_demo() {
  ssh -n -i "$WORK/id_ed25519" -o IdentitiesOnly=yes \
    -o UserKnownHostsFile="$WORK/known_hosts" -o StrictHostKeyChecking=accept-new \
    -p "$SSH_PORT" "$SSH_TARGET" "$@"
}
ssh_demo uname -m
```

SSH banner 探针检查 SSH 服务是否可达，SSH 命令使用调用者保管的私钥进行认证。只把公钥交给 Flintyard。`status` 显示当前进程和 API 观察结果，可加 `--probe` 重新检查访问。

使用以下命令查看环境和串口日志。

```sh
sudo "$FY" list --state-dir "$STATE"
sudo "$FY" logs --state-dir "$STATE" --node node-1 --source serial --tail 100 demo
```

## 停机、恢复和销毁

`stop` 终止 VMM，保留每个节点的可写盘、seed 和身份。停机前先关闭应用并将客户机写入刷新到磁盘。用相同的配置运行 `up` 即可恢复。

```sh
ssh_demo sync
sudo "$FY" stop --state-dir "$STATE" demo
sudo "$FY" up --state-dir "$STATE" --timeout 5m --file "$WORK/environment.json" \
  --firecracker "$FIRECRACKER" --jailer "$JAILER"
```

`down` 删除环境的可写盘、seed 盘和所属运行资源，默认保留镜像文件、缓存和启动日志。使用 `down --purge` 可同时删除诊断记录。

```sh
sudo "$FY" down --state-dir "$STATE" demo
```

对运行中的环境再次执行 `up` 时，配置未变化就会返回观察到的状态。需要修改配置时，先备份应用数据，再用 `down` 移除旧环境。

## 配置多个节点

在环境的 `nodes` 数组中添加节点。每个节点指定唯一名称、镜像 ID 或本地包、vCPU 数和内存（MiB）。可由 Flintyard 分配地址，也可在环境子网内指定 IPv4 地址。镜像和公钥的相对路径从环境配置文件目录解析。所有节点应使用同一架构。

[环境 Schema](../schemas/environment-v1.schema.json) · [多节点配置](../examples/environment.json) · [网络指南](networking.zh-CN.md)

## 回收残留资源

如果进程或宿主意外停止，先运行 `status`，再尝试对该环境执行 `down`。要回收已确认的残留资源，在目标环境目录外保存试运行计划，检查后使用同一状态目录应用仍有效的计划。

```sh
PLAN="$(mktemp -d "$WORK/gc.XXXXXXXX")/plan.json"
sudo "$FY" gc --state-dir "$STATE" --dry-run --plan "$PLAN"
sudo cat "$PLAN"
```

确认计划中的目标和资源后再执行清理。

```sh
sudo "$FY" gc --state-dir "$STATE" --apply --plan "$PLAN"
```

计划有效期为 10 分钟。计划过期，或宿主、目标状态、资源身份发生变化时，请重新生成。错误信息与退出码见 [CLI 参考](cli.zh-CN.md)。
