# Networking

[简体中文](networking.zh-CN.md) · [Node configuration](usage.md#configure-node-resources-and-networking)

Each environment has its own private IPv4 network. Once guest networking is configured, the host can reach guest IPs directly and nodes can reach each other over their private bridge. Keep `network.nat: false` and `dns: []` when you only need host SSH or communication between nodes.

Set these fields in the environment JSON before the first `up`. To change an existing environment's networking, test under a new name or back up its data, destroy it, and create it again. [Stop and destroy](usage.md#stop-resume-and-destroy)

## Address assignment

Set `network.cidr` to `"auto"` to let Flintyard reserve a non-conflicting subnet. Use an explicit private CIDR if other systems need to know the address range ahead of time. You can set `ipv4` on a node to choose a fixed address inside that subnet. Reserve the gateway, network, and broadcast addresses.

## Outbound access

Set `network.nat` to `true` when guests must access package repositories or container registries. Ensure IPv4 forwarding is enabled on the host under its network policy.

```sh
sysctl net.ipv4.ip_forward
ip -4 route show default
```

`net.ipv4.ip_forward` should be `1`. This is a host-wide setting. If authorized, enable it for the current boot with `sudo sysctl -w net.ipv4.ip_forward=1`. Manage persistence through the host's network policy. Flintyard leaves the setting to the host operator.

Use the route output to identify the host egress. Set `network.egress_interface` when a single default egress cannot be inferred. Use `network.dns` to pass IPv4 resolvers that guests can reach. A host-local loopback resolver belongs to the host's own namespace.

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

Merge the fragment into the environment JSON and start the environment. Start a service on port `3000` inside `node-1`. For an HTTP service, test it from the host with `curl --fail http://127.0.0.1:13030/`. A loopback-only mapping can use `nat: false`.

Use a specific non-loopback IPv4 address assigned to the host for access from other machines. Non-loopback forwarding requires IPv4 forwarding and compatible routes, host firewall rules, and upstream ACLs. Verify it from the intended client.

Mappings are enforced by owned nftables rules. Flintyard checks for existing listeners and conflicting reservations before setup. Keep the port unallocated by other services during the environment's lifetime. Read the tutorial environment's mappings and diagnostics as follows. Replace `debian-demo` if you used another name.

```sh
sudo /opt/flintyard/bin/flintyard status --state-dir /var/lib/flintyard-tutorial \
  -o json debian-demo | jq '.data.network'
```

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
