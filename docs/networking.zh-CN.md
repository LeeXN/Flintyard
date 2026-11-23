# 网络

[English](networking.md) · [虚机配置步骤](usage.zh-CN.md#设置虚机资源和网络)

每个环境使用独立的 IPv4 私网。完成客户机网络初始化后，宿主可以直接访问分配给客户机的 IP，同一环境的节点也能互通。只需宿主 SSH 或节点间通信时，保持 `network.nat: false`、`dns: []` 即可。

以下字段在首次 `up` 前写入环境 JSON。要更改已有环境的网络配置，请用新的环境名验证，或在备份后销毁并重建原环境。 [停机与销毁](usage.zh-CN.md#停机恢复和销毁)

## 分配地址

将 `network.cidr` 设为 `"auto"`，由 Flintyard 预留无冲突的子网。如果其他系统需要提前获知网段，可显式填写私网 CIDR。节点的 `ipv4` 可指定为该子网内的固定地址，网关、网络地址和广播地址需留出。

## 出站访问

客户机需要访问软件包仓库或镜像仓库时，将 `network.nat` 设为 `true`，并按宿主网络策略开启 IPv4 转发。

```sh
sysctl net.ipv4.ip_forward
ip -4 route show default
```

`net.ipv4.ip_forward` 应为 `1`。启用转发会影响宿主全局网络行为。获准后可用 `sudo sysctl -w net.ipv4.ip_forward=1` 临时开启，长期设置由宿主网络策略管理。Flintyard 不修改此开关。

从路由输出确定宿主出口。默认路由无法确定唯一出口时，在 `network.egress_interface` 填真实网卡名。在 `network.dns` 填入客户机能访问的 IPv4 DNS 服务器。宿主回环地址上的 DNS 解析器只在宿主网络命名空间内可用。

## 暴露客户机端口

填写宿主绑定地址、端口、传输协议、节点名和客户机端口。服务须在客户机上监听对应端口。以下片段将 `node-1` 的一个服务映射到宿主回环地址。

```json
{
  "port_forwards": [
    {
      "bind_address": "127.0.0.1",
      "host_port": 13030,
      "node": "node-1",
      "guest_port": 3000,
      "protocol": "tcp"
    }
  ]
}
```

将片段合入环境 JSON 并启动。在 `node-1` 上启动监听客户机端口 `3000` 的服务。若服务使用 HTTP，可从宿主执行 `curl --fail http://127.0.0.1:13030/` 检查。如只需要本机映射，可继续使用 `nat: false`。

若要从其他机器访问，请选用分配给宿主的具体非回环 IPv4 地址，同时核对 IPv4 转发、路由、宿主防火墙和上游 ACL，并从实际客户端验证。

Flintyard 通过自身创建的 nftables 规则实现映射，设置前会检查现有监听和端口预留冲突。环境运行期间，请保留该宿主端口。以下命令读取主教程环境的映射和网络诊断。若你用了不同环境名称，请替换末尾的 `debian-demo`。

```sh
sudo /opt/flintyard/bin/flintyard status --state-dir /var/lib/flintyard-tutorial \
  -o json debian-demo | jq '.data.network'
```

## 排查问题

| 现象 | 检查项 |
| --- | --- |
| `E_FORWARDING_DISABLED` | 检查 NAT 或非回环访问所需的宿主 IPv4 转发。 |
| `E_NETWORK_CONFLICT` | 选择其他子网，或使用 `cidr: "auto"`。 |
| `E_PORT_IN_USE` | 用 `ss` 查看宿主监听，选择空闲的地址和端口。 |
| 客户机可访问 IP，域名解析失败 | 核对配置的 DNS 服务器是否对客户机可达。 |
| 宿主可连，外部客户端连接失败 | 检查绑定地址、路由和防火墙策略。 |
| SSH banner 探针超时 | 检查客户机串口日志、网络初始化与 SSH 服务。 |

[NAT 与映射配置样例](../examples/environment-nat.json) · [CLI 诊断](cli.zh-CN.md) · [容器业务教程](container-workload.zh-CN.md)
