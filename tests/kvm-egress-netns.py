#!/usr/bin/env python3
"""Own a temporary external TCP/DNS endpoint in an unmounted network namespace.

The peer and host veth are tagged before the host end enters the host namespace.
The child dies with this supervisor; its namespace disappearance destroys both
ends of the veth. Send 'stop' on stdin to release it after the KVM test.
"""
import argparse
import ctypes
import importlib.util
import ipaddress
import json
import os
from pathlib import Path
import secrets
import selectors
import signal
import socket
import subprocess
import sys
import time


PEER = "198.18.250.2"
HOST = "198.18.250.1"
CIDR = ipaddress.ip_network("198.18.250.0/30")


def command(argv):
    result = subprocess.run(argv, stdout=subprocess.PIPE, stderr=subprocess.PIPE, universal_newlines=True)
    if result.returncode:
        raise RuntimeError("command failed: {}: {}".format(argv, result.stderr.strip()))
    return result.stdout


def child(args):
    libc = ctypes.CDLL(None, use_errno=True)
    if libc.prctl(1, signal.SIGTERM, 0, 0, 0) != 0 or os.getppid() != args.parent:
        raise RuntimeError("cannot establish parent-death cleanup")
    command(["ip", "link", "add", args.host_if, "type", "veth", "peer", "name", args.peer_if])
    command(["ip", "link", "set", "dev", args.host_if, "alias", args.owner])
    command(["ip", "link", "set", "dev", args.peer_if, "alias", args.owner])
    command(["ip", "link", "set", "dev", args.host_if, "netns", str(args.parent)])
    command(["ip", "link", "set", "dev", "lo", "up"])
    command(["ip", "address", "add", PEER + "/30", "dev", args.peer_if])
    command(["ip", "link", "set", "dev", args.peer_if, "up"])
    command(["ip", "route", "add", "default", "via", HOST, "dev", args.peer_if])
    spec = importlib.util.spec_from_file_location("egress_server", str(Path(__file__).resolve().parent / "kvm-egress-server.py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    tcp = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    udp = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    tcp.bind((PEER, 59000))
    udp.bind((PEER, 53))
    tcp.listen(4)
    selector = selectors.DefaultSelector()
    selector.register(tcp, selectors.EVENT_READ, "tcp")
    selector.register(udp, selectors.EVENT_READ, "dns")
    print(json.dumps({"status": "child-ready", "pid": os.getpid(), "peer": PEER}), flush=True)
    deadline = time.monotonic() + args.timeout
    try:
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                return
            for key, _ in selector.select(remaining):
                if key.data == "tcp":
                    conn, peer = tcp.accept()
                    with conn:
                        conn.settimeout(5)
                        data = conn.recv(128)
                        if data != b"flintyard-nat-test\n":
                            continue
                        conn.sendall(b"flintyard-nat-ok\n")
                    print(json.dumps({"probe": "tcp", "source": peer[0]}), flush=True)
                else:
                    data, peer = udp.recvfrom(4096)
                    response, answered = module.dns_response(data)
                    if response is not None:
                        udp.sendto(response, peer)
                        if answered:
                            print(json.dumps({"probe": "dns", "source": peer[0]}), flush=True)
    finally:
        selector.close()
        tcp.close()
        udp.close()


def supervisor(args):
    if os.geteuid() != 0:
        raise RuntimeError("network namespace fixture requires root")
    routes = json.loads(command(["ip", "-j", "-4", "route", "show", "table", "all"]))
    for route in routes:
        destination = route.get("dst", "default")
        if destination != "default" and ipaddress.ip_network(destination, strict=False).overlaps(CIDR):
            raise RuntimeError("fixture subnet overlaps an existing host route")
    addresses = json.loads(command(["ip", "-j", "-4", "address", "show"]))
    for link in addresses:
        for address in link.get("addr_info", []):
            network = ipaddress.ip_network("{}/{}".format(address["local"], address["prefixlen"]), strict=False)
            if network.overlaps(CIDR):
                raise RuntimeError("fixture subnet overlaps an existing host address")
    suffix = secrets.token_hex(5)
    host_if, peer_if = "kve" + suffix, "kvp" + suffix
    owner = "flintyard-kvm-fixture:" + suffix
    child_argv = ["unshare", "--net", sys.executable, str(Path(__file__).resolve()), "--child",
                  "--parent", str(os.getpid()), "--host-if", host_if, "--peer-if", peer_if,
                  "--owner", owner, "--timeout", str(args.timeout)]
    proc = subprocess.Popen(child_argv, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                            stderr=subprocess.STDOUT, universal_newlines=True, bufsize=1)
    host_index = None
    try:
        selector = selectors.DefaultSelector()
        selector.register(proc.stdout, selectors.EVENT_READ, "child")
        selector.register(sys.stdin, selectors.EVENT_READ, "stdin")
        deadline = time.monotonic() + args.timeout
        ready = False
        while time.monotonic() < deadline:
            for key, _ in selector.select(min(5, deadline - time.monotonic())):
                if key.data == "stdin":
                    if sys.stdin.readline().strip() in ("", "stop"):
                        return
                else:
                    line = proc.stdout.readline()
                    if not line:
                        raise RuntimeError("external endpoint exited before cleanup")
                    print(line, end="", flush=True)
                    event = json.loads(line)
                    if event.get("status") == "child-ready":
                        info = json.loads(command(["ip", "-j", "-d", "link", "show", "dev", host_if]))[0]
                        if info.get("ifalias") != owner:
                            raise RuntimeError("fixture host interface owner mismatch")
                        host_index = info["ifindex"]
                        command(["ip", "address", "add", HOST + "/30", "dev", host_if])
                        command(["ip", "link", "set", "dev", host_if, "up"])
                        ready = True
                        print(json.dumps({"status": "ready", "host_interface": host_if, "peer": PEER,
                                          "child_pid": proc.pid, "owner": owner}), flush=True)
            if proc.poll() is not None:
                raise RuntimeError("external endpoint process exited")
        if not ready:
            raise RuntimeError("external endpoint did not become ready")
    finally:
        if proc.poll() is None:
            proc.terminate()
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait()
        # Namespace teardown normally removes the peer. If it did not, refuse
        # to touch any replacement whose alias/index no longer match.
        result = subprocess.run(["ip", "-j", "-d", "link", "show", "dev", host_if],
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, universal_newlines=True)
        if result.returncode == 0:
            info = json.loads(result.stdout)[0]
            if host_index is None or info["ifindex"] != host_index or info.get("ifalias") != owner:
                raise RuntimeError("refusing to remove changed fixture interface")
            command(["ip", "link", "delete", "dev", host_if])
        print(json.dumps({"status": "cleaned", "host_interface": host_if}), flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--timeout", type=int, default=900)
    parser.add_argument("--child", action="store_true")
    parser.add_argument("--parent", type=int)
    parser.add_argument("--host-if")
    parser.add_argument("--peer-if")
    parser.add_argument("--owner")
    args = parser.parse_args()
    if args.child:
        child(args)
    else:
        supervisor(args)


if __name__ == "__main__":
    main()
