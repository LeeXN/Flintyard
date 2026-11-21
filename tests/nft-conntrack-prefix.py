#!/usr/bin/env python3
"""Probe nft conntrack CIDR parsing and packet matches in a private netns."""
import json
import os
import socket
import subprocess


def run(argv, data=None):
    return subprocess.run(argv, input=data, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                          universal_newlines=True)


def main():
    if os.geteuid() != 0 or os.readlink("/proc/self/ns/net") == os.readlink("/proc/1/ns/net"):
        raise RuntimeError("refusing nft probe outside a private root network namespace")
    link = run(["ip", "link", "set", "lo", "up"])
    if link.returncode:
        raise RuntimeError(link.stderr)
    for address in ("10.240.1.10/32", "10.240.1.200/32", "10.240.2.10/32"):
        added = run(["ip", "address", "add", address, "dev", "lo"])
        if added.returncode:
            raise RuntimeError(added.stderr)
    base = {"nftables": [
        {"add": {"table": {"family": "inet", "name": "fyprobe"}}},
        {"add": {"chain": {"family": "inet", "table": "fyprobe", "name": "output",
                            "type": "filter", "hook": "output", "prio": 0, "policy": "accept"}}},
    ]}
    result = run(["nft", "-j", "-f", "-"], json.dumps(base))
    if result.returncode:
        raise RuntimeError(result.stderr)
    ct = {"ct": {"key": "saddr", "dir": "original", "family": "ip"}}
    checks = {
        "prefix": {"match": {"op": "==", "left": ct, "right": {"prefix": {"addr": "10.240.1.0", "len": 24}}}},
        "mask_string": {"match": {"op": "==", "left": {"&": [ct, "255.255.255.0"]}, "right": "10.240.1.0"}},
        "mask_integer": {"match": {"op": "==", "left": {"&": [ct, 0xFFFFFF00]}, "right": 0x0AF00100}},
        "range": {"match": {"op": "==", "left": ct, "right": {"range": ["10.240.1.0", "10.240.1.255"]}}},
    }
    for name, expr in checks.items():
        ipv4 = {"match": {"op": "==", "left": {"payload": {"protocol": "ip", "field": "daddr"}}, "right": "127.0.0.2"}}
        rule = {"nftables": [{"add": {"rule": {"family": "inet", "table": "fyprobe", "chain": "output",
                                              "comment": name, "expr": [ipv4, expr, {"counter": None}]}}}]}
        result = run(["nft", "-j", "-f", "-"], json.dumps(rule))
        if result.returncode:
            print(json.dumps({"variant": name, "status": "unsupported", "detail": result.stderr.strip()}))
    text = run(["nft", "add", "rule", "inet", "fyprobe", "output", "ct", "original", "ip", "saddr", "10.240.1.0/24", "counter", "comment", "text-prefix"])
    if text.returncode:
        print(json.dumps({"variant": "text-prefix", "status": "unsupported", "detail": text.stderr.strip()}))
    receiver = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    receiver.bind(("127.0.0.2", 50051))
    receiver.settimeout(2)
    try:
        for address in ("10.240.1.10", "10.240.1.200", "10.240.2.10"):
            with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as sender:
                sender.bind((address, 0))
                sender.sendto(b"probe", ("127.0.0.2", 50051))
                receiver.recvfrom(64)
    finally:
        receiver.close()
    listing = run(["nft", "-j", "list", "table", "inet", "fyprobe"])
    if listing.returncode:
        raise RuntimeError(listing.stderr)
    packet_counts = {}
    for item in json.loads(listing.stdout)["nftables"]:
        rule = item.get("rule")
        if rule:
            counters = [expr["counter"]["packets"] for expr in rule["expr"] if "counter" in expr]
            packet_counts[rule["comment"]] = counters[0]
            print(json.dumps({"variant": rule["comment"], "packets": counters[0], "expressions": rule["expr"]}))
    if packet_counts.get("prefix") != 2:
        raise RuntimeError("JSON conntrack prefix failed to match exactly the two in-subnet packets")


if __name__ == "__main__":
    main()
