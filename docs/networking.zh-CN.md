# 网络

[English](networking.md)

每个环境使用独立的 IPv4 私网。Flintyard 宿主可以直接访问分配给客户机的 IP。同一环境的节点通过私有网桥互通。将 `network.nat` 设为 `false` 可使用无出站 NAT 的私网。

## 分配地址

将 `network.cidr` 设为 `"auto"`，由 Flintyard 预留无冲突的子网。如果其他系统需要提前获知网段，可显式填写私网 CIDR。节点的 `ipv4` 可指定为该子网内的固定地址，网关、网络地址和广播地址需留出。

## 出站访问

客户机需要访问软件包仓库或镜像仓库时，将 `network.nat` 设为 `true`，并按宿主网络策略开启 IPv4 转发。

```sh
sysctl net.ipv4.ip_forward
ip -4 route show default
```

必要时通过 `network.egress_interface` 指定宿主出口接口。在 `network.dns` 中填入客户机可达的 IPv4 DNS 服务器。宿主的 回环地址上的 DNS 解析器只存在于宿主网络命名空间内。

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

将片段合入环境 JSON。若要从其他机器访问，请选用分配给宿主的具体非回环 IPv4 地址，同时核对 IPv4 转发、路由、宿主防火墙和上游 ACL，并从实际客户端验证。

Flintyard 通过自身创建的 nftables 规则实现映射，设置前会检查现有监听和端口预留冲突。环境运行期间，请保留该宿主端口。`status` 显示端口映射与已知防火墙诊断，也可加 `-o json` 获取结构化结果。

## 排查问题

| 现象 | 检查项 |
| --- | --- |
| `E_FORWARDING_DISABLED` | 检查 NAT 或非回环 访问所需的宿主 IPv4 转发。 |
| `E_NETWORK_CONFLICT` | 选择其他子网，或使用 `cidr: "auto"`。 |
| `E_PORT_IN_USE` | 用 `ss` 查看宿主监听，选择空闲的地址和端口。 |
| 客户机可访问 IP，域名解析失败 | 核对配置的 DNS 服务器是否对客户机可达。 |
| 宿主可连，外部客户端连接失败 | 检查绑定地址、路由和防火墙策略。 |
| SSH banner 探针超时 | 检查客户机串口日志、网络初始化与 SSH 服务。 |

[NAT 与映射配置样例](../examples/environment-nat.json) · [CLI 诊断](cli.zh-CN.md) · [容器业务教程](container-workload.zh-CN.md)
