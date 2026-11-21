#!/usr/bin/env python3
"""External validation of one or two three-node KVM environments already started."""
import argparse
import concurrent.futures
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import subprocess


def sha256(path):
    h = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--result-a", required=True, type=Path)
    parser.add_argument("--result-b", type=Path)
    parser.add_argument("--state-dir", required=True, type=Path)
    parser.add_argument("--key", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--rootfs-sha256", required=True)
    parser.add_argument("--profile", choices=("systemd-networkd", "openrc-ifupdown", "networkmanager"), default="systemd-networkd")
    parser.add_argument("--connectivity", choices=("icmp", "tcp"), default="icmp")
    args = parser.parse_args()
    if not re.fullmatch(r"[0-9a-f]{64}", args.rootfs_sha256):
        raise ValueError("invalid original rootfs checksum")
    args.output.mkdir(mode=0o700, parents=False, exist_ok=False)
    scripts = Path(__file__).resolve().parent
    nodes = []
    networks = []
    cache = None
    disk_ids = set()
    inputs = [("a", args.result_a)]
    if args.result_b is not None:
        inputs.append(("b", args.result_b))
    for label, path in inputs:
        envelope = json.loads(path.read_text())
        data = envelope["data"]
        if not envelope["ok"] or data["state"] != "ready" or len(data["nodes"]) != 3:
            raise ValueError("environment did not report three ready nodes: " + label)
        env_id = data["environment_id"]
        if not re.fullmatch(r"[0-9a-f-]{36}", env_id):
            raise ValueError("invalid environment ID")
        state = json.loads((args.state_dir / "envs" / env_id / "state.json").read_text())
        plan = state["reservation"]["plan"]
        networks.append(ipaddress.ip_network(plan["cidr"]))
        if plan["nat"]:
            raise ValueError("isolation check requires nat=false")
        for index, node in enumerate(data["nodes"], 1):
            if not node["access"]["probe_passed"]:
                raise ValueError("node did not pass SSH banner probe")
            state_node = next(n for n in state["nodes"] if n["id"] == node["node_id"])
            ip = str(ipaddress.IPv4Address(node["ipv4"]))
            if ipaddress.IPv4Address(ip) not in networks[-1]:
                raise ValueError("node IP outside reserved network")
            disk = Path(state_node["disk"]["path"])
            stat = disk.stat()
            identity = (stat.st_dev, stat.st_ino)
            if identity in disk_ids:
                raise ValueError("writable nodes share an inode")
            disk_ids.add(identity)
            image_id = state_node["image_id"]
            if not re.fullmatch(r"sha256:[0-9a-f]{64}", image_id):
                raise ValueError("invalid cached image identity")
            image_root = args.state_dir / "images" / image_id.replace(":", "-") / "payload/rootfs.raw"
            if cache is not None and cache != image_root:
                raise ValueError("test nodes should share one read-only cache input")
            cache = image_root
            nodes.append({"label": label, "index": index, "node": node, "ip": ip,
                          "known_hosts": args.output / (label + str(index) + ".known_hosts")})
    if (len(networks) == 2 and networks[0].overlaps(networks[1])) or len({n["ip"] for n in nodes}) != len(nodes):
        raise ValueError("test environments overlap")
    cached_stat = cache.stat()
    if cached_stat.st_mode & 0o222 or (cached_stat.st_dev, cached_stat.st_ino) in disk_ids or sha256(cache) != args.rootfs_sha256:
        raise ValueError("cached rootfs identity or checksum differs")

    def invoke(logname, script, *values):
        result = subprocess.run(["bash", str(scripts / script)] + [str(v) for v in values],
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, universal_newlines=True, timeout=60)
        with (args.output / logname).open("x") as log:
            log.write(result.stdout)
            log.write(result.stderr)
        if result.returncode:
            raise RuntimeError("guest check failed: " + logname)
        return result.stdout

    def check_node(n):
        output = invoke(n["label"] + str(n["index"]) + ".log", "kvm-guest-check.sh", n["ip"], args.key, n["known_hosts"], args.profile)
        fields = dict(line.split("=", 1) for line in output.splitlines() if "=" in line)
        if fields.get("hostname") != n["node"]["hostname"] or not re.fullmatch(r"[0-9a-f]{32}", fields.get("machine_id", "")):
            raise ValueError("guest identity differs from allocated node")
        observed = fields["observed_host_key"].split()[1]
        guest = fields["guest_host_key"].split()[1]
        if observed != guest:
            raise ValueError("SSH host key observed from host differs from guest")
        return fields["machine_id"], observed

    with concurrent.futures.ThreadPoolExecutor(max_workers=6) as pool:
        identities = list(pool.map(check_node, nodes))
    if len({i[0] for i in identities}) != len(nodes) or len({i[1] for i in identities}) != len(nodes):
        raise ValueError("cloned machine IDs or SSH host keys are not unique")
    for source in nodes:
        for target in nodes:
            if source is target or source["label"] != target["label"]:
                continue
            invoke("ping-{}{}-{}{}.log".format(source["label"], source["index"], target["label"], target["index"]),
                   "kvm-guest-ping.sh", source["ip"], args.key, source["known_hosts"], target["ip"], "reachable", args.connectivity)
    if len(networks) == 2:
        for source, target in ((nodes[0], nodes[3]), (nodes[3], nodes[0])):
            invoke("isolation-{}-{}.log".format(source["label"], target["label"]), "kvm-guest-ping.sh",
                   source["ip"], args.key, source["known_hosts"], target["ip"], "blocked", args.connectivity)
    token = "fy20261109multidisk01"
    markers = [(nodes[0], "write"), (nodes[1], "absent")]
    if len(networks) == 2:
        markers.append((nodes[3], "absent"))
    for node, action in markers:
        invoke("disk-{}{}-{}.log".format(node["label"], node["index"], action), "kvm-guest-marker.sh",
               node["ip"], args.key, node["known_hosts"], action, token)
    if sha256(cache) != args.rootfs_sha256:
        raise ValueError("guest write altered the cached rootfs")
    architectures = sorted({n["node"]["architecture"] for n in nodes})
    report = {"architectures": architectures, "profile": args.profile, "environments": len(networks), "nodes": len(nodes),
              "ssh_authentication": "passed", "unique_machine_ids": "passed", "unique_host_keys": "passed",
              "independent_disks_and_cache": "passed", "same_environment_" + args.connectivity: "passed",
              "cross_environment_" + args.connectivity: "blocked_as_expected" if len(networks) == 2 else "not_run",
              "source_of_trust": "ephemeral lab TOFU, then verified against guest key"}
    with (args.output / "report.json").open("x") as stream:
        json.dump(report, stream, indent=2)
        stream.write("\n")
    print(json.dumps(report))


if __name__ == "__main__":
    main()
