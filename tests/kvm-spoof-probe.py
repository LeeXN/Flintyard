#!/usr/bin/env python3
"""Inject one spoofed loopback UDP packet through an owned test veth only."""
import argparse
import ipaddress
import json
import os
from pathlib import Path
import re
import socket
import struct
import subprocess
import sys
import time


def checksum(data):
    if len(data) % 2:
        data += b"\x00"
    total = sum(struct.unpack("!{}H".format(len(data) // 2), data))
    while total >> 16:
        total = (total & 0xffff) + (total >> 16)
    return (~total) & 0xffff


def packet(source_mac, destination_mac, target, port):
    src = socket.inet_aton("127.0.0.9")
    dst = socket.inet_aton(target)
    payload = b"flintyard-udp-spoof"
    udp = struct.pack("!HHHH", 47000, port, 8 + len(payload), 0)
    pseudo = src + dst + struct.pack("!BBH", 0, socket.IPPROTO_UDP, len(udp) + len(payload))
    udp_checksum = checksum(pseudo + udp + payload) or 0xffff
    udp = struct.pack("!HHHH", 47000, port, 8 + len(payload), udp_checksum)
    header = struct.pack("!BBHHHBBH4s4s", 0x45, 0, 20 + len(udp) + len(payload), 0x1731, 0,
                         64, socket.IPPROTO_UDP, 0, src, dst)
    header = header[:10] + struct.pack("!H", checksum(header)) + header[12:]
    return bytes.fromhex(destination_mac.replace(":", "")) + bytes.fromhex(source_mac.replace(":", "")) + b"\x08\x00" + header + udp + payload


def info(argv):
    data = subprocess.check_output(argv, universal_newlines=True)
    return json.loads(data)[0]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--pid", required=True, type=int)
    parser.add_argument("--host-if", required=True)
    parser.add_argument("--peer-if", required=True)
    parser.add_argument("--target", required=True)
    parser.add_argument("--port", required=True, type=int)
    parser.add_argument("--send", action="store_true")
    parser.add_argument("--source-mac")
    parser.add_argument("--destination-mac")
    args = parser.parse_args()
    suffix = args.host_if[3:]
    if os.geteuid() != 0 or not re.fullmatch(r"kve[0-9a-f]{10}", args.host_if) or args.peer_if != "kvp" + suffix:
        raise ValueError("invalid owned fixture interface")
    target = str(ipaddress.IPv4Address(args.target))
    if not 1 <= args.port <= 65535:
        raise ValueError("invalid mapped port")
    if args.send:
        frame = packet(args.source_mac, args.destination_mac, target, args.port)
        with socket.socket(socket.AF_PACKET, socket.SOCK_RAW) as sender:
            sender.bind((args.peer_if, 0))
            sender.send(frame)
        return
    netns = "/proc/{}/ns/net".format(args.pid)
    if os.stat(netns).st_ino == os.stat("/proc/self/ns/net").st_ino:
        raise ValueError("fixture endpoint is not in a separate namespace")
    owner = "flintyard-kvm-fixture:" + suffix
    host = info(["ip", "-j", "-d", "link", "show", "dev", args.host_if])
    peer = info(["nsenter", "--net=" + netns, "--", "ip", "-j", "-d", "link", "show", "dev", args.peer_if])
    if host.get("ifalias") != owner or peer.get("ifalias") != owner:
        raise ValueError("fixture interface ownership mismatch")
    frame = packet(peer["address"], host["address"], target, args.port)
    with socket.socket(socket.AF_PACKET, socket.SOCK_RAW, socket.htons(3)) as observer:
        observer.bind((args.host_if, 0))
        observer.settimeout(5)
        subprocess.check_call(["nsenter", "--net=" + netns, "--", sys.executable, str(Path(__file__).resolve()),
                               "--pid", str(args.pid), "--host-if", args.host_if, "--peer-if", args.peer_if,
                               "--target", target, "--port", str(args.port), "--send",
                               "--source-mac", peer["address"], "--destination-mac", host["address"]])
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            received = observer.recv(4096)
            if received[:len(frame)] == frame:
                print(json.dumps({"spoofed_source": "127.0.0.9", "target_port": args.port, "host_ingress_observed": True}))
                return
    raise RuntimeError("spoof packet was not observed on the owned host interface")


if __name__ == "__main__":
    main()
