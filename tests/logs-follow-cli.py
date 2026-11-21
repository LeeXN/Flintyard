#!/usr/bin/env python3
"""Verify CLI streaming against owned files; never starts a VM or guest command."""
import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import platform
import select
import signal
import subprocess
import tempfile
import time
import uuid

REPO = Path(__file__).resolve().parents[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path)
    args = parser.parse_args()
    arch = {"x86_64": "amd64", "aarch64": "arm64"}[platform.machine()]
    binary = (args.binary or REPO / "bin" / f"flintyard-linux-{arch}").resolve(strict=True)
    (REPO / ".local").mkdir(exist_ok=True)
    base = Path(tempfile.mkdtemp(prefix="follow-cli-", dir=REPO / ".local"))
    root = base / "state"
    root.mkdir(mode=0o700)
    rid, eid, nid = [str(uuid.uuid4()) for _ in range(3)]

    def write(path, value):
        path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        path.write_text(json.dumps(value))
        path.chmod(0o600)

    now = datetime.datetime.now(datetime.timezone.utc).isoformat()
    write(root / "host.json", {"api_version": "flintyard/v1alpha1", "kind": "StateRoot", "id": rid, "created_at": now})
    (root / "locks").mkdir(mode=0o700)
    env = root / "envs" / eid
    node = env / "nodes" / nid
    write(env / "owner.json", {"root_id": rid, "environment_id": eid})
    write(node / "owner.json", {"root_id": rid, "environment_id": eid, "node_id": nid})
    architecture = {"amd64": "x86_64", "arm64": "aarch64"}[arch]
    write(env / "state.json", {
        "api_version": "flintyard/v1alpha1", "kind": "EnvironmentState", "root_id": rid,
        "id": eid, "name": "log-fixture", "state": "stopped", "architecture": architecture,
        "created_at": now, "updated_at": now,
        "spec": {"api_version": "flintyard/v1alpha1", "kind": "Environment", "name": "log-fixture",
                 "architecture": architecture, "nodes": [{"name": "node-1", "image": ""}],
                 "access": {"probe": {"type": "none", "port": 22}}},
        "nodes": [{"id": nid, "name": "node-1", "state": "stopped", "image_id": ""}], "operations": [],
    })
    logs = node / "logs"
    logs.mkdir(mode=0o700)
    serial = logs / "serial.log"
    serial.write_bytes(b"before\n")
    serial.chmod(0o600)
    firecracker = logs / "firecracker.log"
    firecracker.write_bytes(b"fc\n")
    firecracker.chmod(0o600)
    argv = [str(binary), "logs", "--follow", "--tail", "1", "--state-dir", str(root), "--node", "node-1"]
    checks, children = [], []

    def start():
        child = subprocess.Popen(argv + ["log-fixture"], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        children.append(child)
        return child

    def read(child, count):
        data, until = b"", time.monotonic() + 3
        while len(data) < count:
            assert time.monotonic() < until, "stream readiness timed out"
            if select.select([child.stdout], [], [], .1)[0]:
                chunk = os.read(child.stdout.fileno(), count - len(data))
                assert chunk, "stream exited before expected data"
                data += chunk
        return data

    try:
        child = start()
        initial = read(child, 7)
        with serial.open("ab") as stream:
            stream.write(b"after\n")
            stream.flush()
            os.fsync(stream.fileno())
        appended = read(child, 6)
        child.send_signal(signal.SIGINT)
        extra, stderr = child.communicate(timeout=3)
        assert child.returncode == 130 and initial + appended + extra == b"before\nafter\n"
        checks.append({"argv": argv + ["log-fixture"], "kind": "append and SIGINT", "exit_code": child.returncode,
                       "stdout": (initial + appended + extra).decode(), "stderr": stderr.decode()})
        for extra, expected in [(["--timeout", "150ms"], b"after\n"),
                                (["--source", "firecracker", "--timeout", "150ms"], b"fc\n")]:
            run = subprocess.run(argv + extra + ["log-fixture"], capture_output=True, timeout=3)
            assert run.returncode == 4 and run.stdout == expected and b"deadline exceeded" in run.stderr
            checks.append({"argv": argv + extra + ["log-fixture"], "kind": "explicit deadline", "exit_code": run.returncode,
                           "stdout": run.stdout.decode(), "stderr": run.stderr.decode()})
        run = subprocess.run(argv + ["--json", "log-fixture"], capture_output=True, timeout=3)
        assert run.returncode == 2 and json.loads(run.stdout)["error"]["code"] == "E_CONFIG_INVALID"
        checks.append({"argv": argv + ["--json", "log-fixture"], "kind": "JSON exclusion", "exit_code": run.returncode,
                       "stdout": json.loads(run.stdout), "stderr": run.stderr.decode()})
        serial.write_bytes(b"x" * (9 << 20))
        child = start()
        assert select.select([child.stdout], [], [], 3)[0], "blocked-pipe stream never started"
        child.send_signal(signal.SIGINT)
        child.wait(timeout=3)  # Intentionally do not drain stdout before exit.
        data, stderr = child.communicate(timeout=3)
        assert child.returncode == 130 and data and data == b"x" * len(data)
        checks.append({"argv": argv + ["log-fixture"], "kind": "cancel with unread stdout pipe", "exit_code": child.returncode,
                       "stdout_bytes": len(data), "stdout_sha256": hashlib.sha256(data).hexdigest(), "stderr": stderr.decode()})
    finally:
        for child in children:
            if child.poll() is None:
                child.terminate()
                try:
                    child.communicate(timeout=3)
                except subprocess.TimeoutExpired:
                    child.kill()
                    child.communicate(timeout=3)
    (base / "checks.json").write_text(json.dumps({"scope": "owned file CLI fixture; no VM", "checks": checks}, indent=2) + "\n")
    print(json.dumps({"checks": len(checks), "evidence": str(base), "scope": "owned log files; no VM"}))


if __name__ == "__main__":
    main()
