#!/usr/bin/env python3
"""Interrupt only our own up process at a selected journal boundary."""
import argparse
import json
import os
from pathlib import Path
import signal
import subprocess
import time


def starttime(pid):
    data = Path("/proc/{}/stat".format(pid)).read_text()
    return int(data[data.rfind(")") + 2:].split()[19])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("cli", "state-dir", "spec", "firecracker", "jailer", "output"):
        parser.add_argument("--" + name, required=True, type=Path)
    parser.add_argument("--boundary", choices=("netns", "live-vmm"), default="netns")
    args = parser.parse_args()
    spec = json.loads(args.spec.read_text())
    name = spec["name"]
    if args.state_dir.exists() or args.state_dir.is_symlink():
        raise ValueError("crash test requires a new, isolated state directory")
    args.output.mkdir(mode=0o700, parents=False, exist_ok=False)
    argv = [str(args.cli), "up", "--state-dir", str(args.state_dir), "--file", str(args.spec),
            "--firecracker", str(args.firecracker), "--jailer", str(args.jailer), "--uid-range", "60000:60099",
            "--wait", "ssh_banner", "--timeout", "10m", "--events", str(args.output / "events.jsonl"), "--json"]
    with (args.output / "up.json").open("x") as stdout, (args.output / "up.stderr").open("x") as stderr:
        proc = subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=stdout, stderr=stderr, start_new_session=True)
        try:
            expected_binary = args.cli.stat()
            deadline = time.monotonic() + 600
            identity = None
            while time.monotonic() < deadline:
                if proc.poll() is not None:
                    raise RuntimeError("up exited before the requested crash boundary")
                if identity is None:
                    try:
                        executable = os.stat("/proc/{}/exe".format(proc.pid))
                        if (executable.st_dev, executable.st_ino) == (expected_binary.st_dev, expected_binary.st_ino):
                            identity = starttime(proc.pid)
                    except (FileNotFoundError, ProcessLookupError):
                        pass
                envs = args.state_dir / "envs"
                for state_file in envs.glob("*/state.json") if envs.exists() else []:
                    try:
                        state = json.loads(state_file.read_text())
                        if state["name"] != name or state_file.parent.name != state["id"]:
                            continue
                        resources = []
                        for journal_file in (state_file.parent / "operations").glob("*.json"):
                            resources.extend(json.loads(journal_file.read_text())["resources"])
                        owned = [r for r in resources if r["kind"] == "netns" and r["state"] == "done"]
                        vmms = [r for r in resources if r["kind"] == "vmm-process" and r["state"] == "done"]
                        if args.boundary == "netns" and any(r["kind"] == "vmm-process" for r in resources):
                            raise RuntimeError("VMM launch started before the intended network boundary")
                        if not owned or identity is None:
                            continue
                        if args.boundary == "live-vmm" and (not vmms or state["nodes"][0]["state"] != "running"):
                            continue
                        current = os.stat("/proc/{}/exe".format(proc.pid))
                        if starttime(proc.pid) != identity or (current.st_dev, current.st_ino) != (expected_binary.st_dev, expected_binary.st_ino):
                            raise RuntimeError("up process identity changed")
                        report = {"environment_id": state["id"], "name": name, "up_pid": proc.pid,
                                  "up_starttime": identity, "boundary": args.boundary,
                                  "namespace": owned[0]["identity"]["name"], "result": "controller_sigkill"}
                        if args.boundary == "live-vmm":
                            vmm = json.loads(vmms[0]["identity"]["identity"])
                            if starttime(vmm["pid"]) != vmm["starttime"]:
                                raise RuntimeError("VMM identity changed before controller crash")
                            report["vmm_pid"] = vmm["pid"]
                            report["vmm_starttime"] = vmm["starttime"]
                        proc.kill()
                        if proc.wait(timeout=10) != -signal.SIGKILL:
                            raise RuntimeError("up did not terminate by SIGKILL")
                        with (args.output / "crash.json").open("x") as output:
                            json.dump(report, output, indent=2)
                            output.write("\n")
                        print(json.dumps(report))
                        return
                    except FileNotFoundError:
                        continue
                time.sleep(0.02)
            raise TimeoutError("crash boundary not reached")
        finally:
            if proc.poll() is None:
                proc.terminate()
                try:
                    proc.wait(timeout=30)
                except subprocess.TimeoutExpired:
                    proc.kill()
                    proc.wait()


if __name__ == "__main__":
    main()
