#!/usr/bin/env python3
"""External TCP SSH-banner and UDP echo checks for disposable KVM guests."""
import argparse
import json
import socket


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host", required=True)
    parser.add_argument("--tcp-port", required=True, type=int)
    parser.add_argument("--udp-port", type=int)
    parser.add_argument("--token", default="probe")
    parser.add_argument("--expect-tcp", choices=("open", "blocked"), default="open")
    parser.add_argument("--expect-udp", choices=("open", "blocked"), default="open")
    args = parser.parse_args()
    try:
        with socket.create_connection((args.host, args.tcp_port), timeout=4) as stream:
            stream.settimeout(4)
            banner = stream.recv(256)
            if args.expect_tcp == "blocked":
                raise RuntimeError("unexpected reachable TCP mapping")
            if not banner.startswith(b"SSH-2.0-"):
                raise RuntimeError("mapped TCP did not return an SSH banner")
            tcp_source = stream.getsockname()[0]
    except OSError:
        if args.expect_tcp != "blocked":
            raise
        print(json.dumps({"tcp": "blocked_as_expected", "host": args.host, "port": args.tcp_port}))
        return
    result = {"tcp": "passed", "source": tcp_source, "host": args.host, "port": args.tcp_port}
    print(json.dumps(result), flush=True)
    if args.udp_port is not None:
        payload = ("flintyard-udp-" + args.token).encode("ascii")
        try:
            with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as datagram:
                datagram.settimeout(4)
                datagram.sendto(payload, (args.host, args.udp_port))
                reply, _ = datagram.recvfrom(256)
                if args.expect_udp == "blocked":
                    raise RuntimeError("unexpected reachable UDP mapping")
                if reply != b"echo:" + payload:
                    raise RuntimeError("mapped UDP response differs from test payload")
                result["udp"] = "passed"
        except OSError:
            if args.expect_udp != "blocked":
                raise
            result["udp"] = "blocked_as_expected"
        print(json.dumps(result))


if __name__ == "__main__":
    main()
