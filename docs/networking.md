# Networking

[简体中文](networking.zh-CN.md)

Each environment has its own private IPv4 network. The Flintyard host can connect directly to the assigned guest IPs. Nodes in the same environment can communicate over their private bridge. Set `network.nat` to `false` for a private network without outbound NAT.

## Address assignment

Set `network.cidr` to `"auto"` to let Flintyard reserve a non-conflicting subnet. Use an explicit private CIDR if other systems need to know the address range ahead of time. You can set `ipv4` on a node to choose a fixed address inside that subnet. Reserve the gateway, network, and broadcast addresses.

## Outbound access

Set `network.nat` to `true` when guests must access package repositories or container registries. Ensure IPv4 forwarding is enabled on the host under its network policy.

```sh
sysctl net.ipv4.ip_forward
ip -4 route show default
```

When needed, set `network.egress_interface` to a specific host interface. Use `network.dns` to pass IPv4 DNS servers that guests can reach. Choose actual resolvers for your network. A host-local loopback resolver belongs to the host's own namespace.

## Expose a guest port

Declare the exact host address and port, transport protocol, node name, and guest port. The guest service must be listening on the specified port. The following fragment maps a service on node `node-1` to the host's loopback.

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

Merge the fragment into the environment JSON. Use a specific non-loopback IPv4 address assigned to the host for access from other machines. Non-loopback forwarding requires IPv4 forwarding and compatible routes, host firewall rules, and upstream ACLs. Verify it from the intended client.

Mappings are enforced by owned nftables rules. Flintyard checks for existing listeners and conflicting reservations before setup. Keep the host port unallocated by other services during the environment's lifetime. `status -o json` reports assigned endpoints and known firewall diagnostics.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| `E_FORWARDING_DISABLED` | Check the host's IPv4 forwarding setting for NAT and non-loopback access. |
| `E_NETWORK_CONFLICT` | Pick a different explicit subnet or use `cidr: "auto"`. |
| `E_PORT_IN_USE` | Inspect host listeners with `ss` and use a free address and port. |
| Guest can access IPs but cannot resolve names | Confirm the configured DNS server is reachable from the guest. |
| Host can connect but an external client cannot | Check the exact bind address, routes, and firewall policy. |
| SSH banner times out | Check the guest serial log, network initialization, and SSH service. |

[NAT and port forwarding example](../examples/environment-nat.json) · [CLI diagnostics](cli.md) · [Container workload tutorial](container-workload.md)
