#!/usr/bin/env python3
"""Explicitly generate a native-host, locked fixture config before image build.

This is test-input preparation, not permission for the product to silently
normalize a recipe's config. The template, argv, diff and final SHA are recorded.
"""
import argparse
import difflib
import hashlib
import json
import os
from pathlib import Path
import platform
import subprocess
import tempfile

BASE = Path(__file__).resolve().parent


def publish_config(config, report, output):
    """Publish complete provenance before the config, without replacement."""
    report_path = Path(str(output) + ".json")
    staged_report = config.parent / "provenance.json"
    with staged_report.open("x") as stream:
        json.dump(report, stream, indent=2)
        stream.write("\n")
        stream.flush()
        os.fsync(stream.fileno())
    config.chmod(0o444)
    staged_report.chmod(0o444)
    with config.open("rb") as stream:
        os.fsync(stream.fileno())
    # A crash between links leaves only a complete provenance file, never a
    # config that render.py can mistake for an audited pair.
    directory = os.open(output.parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.link(staged_report, report_path)
        os.fsync(directory)
        try:
            os.link(config, output)
        except OSError as exc:
            # Do not unlink a path a concurrent process may have replaced.
            raise RuntimeError("config publication failed; provenance retained at " + str(report_path)) from exc
        os.fsync(directory)
    finally:
        os.close(directory)


def configure(source, architecture, output, modules):
    native = {"x86_64": "x86_64", "aarch64": "aarch64"}.get(platform.machine())
    if architecture != native:
        raise ValueError("fixture configuration is generated on the matching native host")
    source, output = Path(source).resolve(strict=True), Path(os.path.abspath(output))
    lock = json.loads((BASE / "lock.json").read_text())
    template = lock["kernel"]["config_templates"][architecture]
    data = (BASE / template["path"]).read_bytes()
    if hashlib.sha256(data).hexdigest() != template["sha256"]:
        raise ValueError("kernel config template digest mismatch")
    report_path = Path(str(output) + ".json")
    if output.exists() or output.is_symlink() or report_path.exists() or report_path.is_symlink():
        raise FileExistsError("kernel configuration or provenance already exists")
    output.parent.mkdir(parents=True, exist_ok=True)
    commands, logs = [], []

    def run(argv):
        commands.append([str(a) for a in argv])
        result = subprocess.run(commands[-1], capture_output=True, text=True, timeout=180,
                                env={"PATH": "/usr/sbin:/usr/bin:/sbin:/bin", "LANG": "C", "LC_ALL": "C"})
        logs.append({"argv": commands[-1], "exit_code": result.returncode, "stdout": result.stdout, "stderr": result.stderr})
        if result.returncode:
            raise RuntimeError("fixture config command failed: " + str(logs[-1]))
        return result.stdout

    with tempfile.TemporaryDirectory(prefix=".kernel-config-", dir=output.parent) as directory:
        work = Path(directory)
        config = work / ".config"
        config.write_bytes(data)
        options = (["--enable", "MODULES", "--module", "DUMMY"] if modules == "enabled" else ["--disable", "MODULES"]) + ["--enable", "BLK_DEV_INITRD",
                   "--enable", "RD_GZIP", "--enable", "RD_ZSTD"]
        if architecture == "x86_64":
            options += ["--enable", "VIRTIO_MMIO_CMDLINE_DEVICES"]
        run([source / "scripts/config", "--file", config] + options)
        common = ["make", "-C", source, "O=" + str(work), "ARCH=" + {"x86_64": "x86_64", "aarch64": "arm64"}[architecture], "CC=gcc", "LD=ld"]
        run(common + ["olddefconfig"])
        release = run(common + ["-s", "--no-print-directory", "kernelrelease"]).strip()
        if release != lock["kernel"]["release"]:
            raise ValueError("config source release differs from locked kernel")
        final = config.read_bytes()
        final_sha = hashlib.sha256(final).hexdigest()
        report = {"architecture": architecture, "kernel_release": release, "modules": modules, "template_sha256": template["sha256"],
                  "config_sha256": final_sha, "explicit_options": options, "commands": commands, "logs": logs,
                  "normalization_explicitly_performed_before_recipe_lock": True,
                  "diff": "".join(difflib.unified_diff(data.decode().splitlines(True), final.decode().splitlines(True),
                                                    fromfile="fixed-firecracker-template", tofile="native-locked-config")),
                  "native_kernel_build": "not_run", "boot_validation": "not_run"}
        publish_config(config, report, output)
    return {"path": str(output), "sha256": final_sha, "architecture": architecture, "kernel_release": release}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", required=True, type=Path)
    parser.add_argument("--architecture", required=True, choices=("x86_64", "aarch64"))
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--modules", choices=("enabled", "disabled"), default="enabled")
    args = parser.parse_args()
    print(json.dumps(configure(args.source, args.architecture, args.output, args.modules)))


if __name__ == "__main__":
    main()
