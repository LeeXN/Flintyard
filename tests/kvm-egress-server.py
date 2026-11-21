#!/usr/bin/env python3
"""Short-lived external TCP and authoritative DNS endpoints for KVM tests."""
import argparse
import json
import selectors
import socket
import struct
import time


def dns_response(data):
    if len(data) < 17 or data[4:6] != b"\x00\x01":
        return None, False
    position, labels = 12, []
    while position < len(data):
        size = data[position]
        position += 1
        if size == 0:
            break
        if size > 63 or position + size > len(data):
            return None, False
        labels.append(data[position:position + size].lower())
        position += size
    if position + 4 > len(data) or b".".join(labels) != b"flintyard-test.invalid":
        return None, False
    kind, qclass = struct.unpack("!HH", data[position:position + 4])
    if qclass != 1:
        return None, False
    question = data[12:position + 4]
    answer = kind == 1
    header = data[:2] + struct.pack("!HHHHH", 0x8180, 1, int(answer), 0, 0)
    record = b"\xc0\x0c" + struct.pack("!HHIH", 1, 1, 30, 4) + socket.inet_aton("198.51.100.42") if answer else b""
    return header + question + record, answer


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--bind", required=True)
    parser.add_argument("--expected-source", required=True)
    parser.add_argument("--tcp-port", type=int, default=59000)
    parser.add_argument("--dns-port", type=int, default=53)
    parser.add_argument("--timeout", type=int, default=300)
    args = parser.parse_args()
    tcp = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    udp = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    tcp.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    tcp.bind((args.bind, args.tcp_port))
    udp.bind((args.bind, args.dns_port))
    tcp.listen(4)
    selector = selectors.DefaultSelector()
    selector.register(tcp, selectors.EVENT_READ, "tcp")
    selector.register(udp, selectors.EVENT_READ, "dns")
    print(json.dumps({"status": "ready", "bind": args.bind, "tcp_port": args.tcp_port, "dns_port": args.dns_port}), flush=True)
    seen = set()
    deadline = time.monotonic() + args.timeout
    try:
        while len(seen) < 2:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError("controlled egress endpoints did not receive both probes")
            for key, _ in selector.select(remaining):
                if key.data == "tcp":
                    conn, peer = tcp.accept()
                    with conn:
                        conn.settimeout(5)
                        data = conn.recv(128)
                        if data != b"flintyard-nat-test\n" or peer[0] != args.expected_source:
                            raise ValueError("unexpected TCP request or source")
                        conn.sendall(b"flintyard-nat-ok\n")
                    seen.add("tcp")
                    print(json.dumps({"probe": "tcp", "source": peer[0], "result": "passed"}), flush=True)
                else:
                    data, peer = udp.recvfrom(4096)
                    response, answered = dns_response(data)
                    if response is None:
                        continue
                    if peer[0] != args.expected_source:
                        raise ValueError("unexpected DNS source")
                    udp.sendto(response, peer)
                    if answered:
                        seen.add("dns")
                        print(json.dumps({"probe": "dns", "source": peer[0], "result": "passed"}), flush=True)
    finally:
        selector.close()
        tcp.close()
        udp.close()


if __name__ == "__main__":
    main()
