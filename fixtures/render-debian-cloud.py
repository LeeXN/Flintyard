#!/usr/bin/env python3
"""Render a locked, offline recipe preserving Debian's original cloud kernel."""
import argparse
import hashlib
import json
from pathlib import Path

import prepare
import render

BASE = Path(__file__).resolve().parent


def recipe(architecture, input_dir, output, recipe_path):
    fixture_lock = prepare.strict_json((BASE / "lock.json").read_bytes())
    data = (BASE / "debian-cloud.lock.json").read_bytes()
    if hashlib.sha256(data).hexdigest() != fixture_lock["families"]["debian"]["cloud_lock_sha256"]:
        raise ValueError("Debian cloud lock digest mismatch")
    entry = prepare.strict_json(data)["architectures"][architecture]
    input_dir = Path(input_dir)
    files = {}
    for role, filename, sha, size in (
        ("rootfs", "rootfs.raw", entry["rootfs_sha256"], entry["partition_size"]),
        ("kernel", "kernel", entry["boot_files"]["kernel"]["sha256"], entry["boot_files"]["kernel"]["size"]),
        ("initrd", "initrd", entry["boot_files"]["initrd"]["sha256"], entry["boot_files"]["initrd"]["size"]),
        ("config", "config", entry["boot_files"]["config"]["sha256"], entry["boot_files"]["config"]["size"]),
    ):
        path = input_dir / filename
        if path.is_symlink() or not path.is_file() or path.stat().st_size != size or prepare.digest(path) != sha:
            raise ValueError("Debian cloud input differs from lock: " + role)
        files[role] = {"path": str(path.resolve(strict=True)), "sha256": sha}
    metadata = prepare.strict_json((input_dir / "provenance.json").read_bytes())
    if metadata["architecture"] != architecture or metadata["rootfs_sha256"] != entry["rootfs_sha256"] or metadata["kernel_release"] != entry["kernel_release"]:
        raise ValueError("Debian cloud normalization provenance differs from lock")
    command = ("set -e; id devops >/dev/null 2>&1 || useradd -m -s /bin/bash devops; "
               "mkdir -p /etc/cloud; : > /etc/cloud/cloud-init.disabled; "
               "printf '%s\\n' '/dev/vda / ext4 defaults 0 1' > /etc/fstab; "
               "systemctl disable ssh.socket 2>/dev/null || true; systemctl enable ssh.service")
    result = {
        "api_version": "flintyard/v1alpha1", "kind": "BuildRecipe", "architecture": architecture,
        "os": {"family": "debian", "release": "13"},
        "rootfs": {"kind": "raw", "source": files["rootfs"]},
        "kernel": {"mode": "prebuilt", "source": files["kernel"], "release": entry["kernel_release"],
                   "origin": "distribution-binary", "modules": "rootfs"},
        "initrd": files["initrd"],
        "boot": {"root_device": "/dev/vda", "kernel_args": render.firecracker_boot_args(architecture)},
        "steps": [{"argv": ["/bin/sh", "-c", command], "working_dir": "/", "timeout_seconds": 600}],
        "guest_prepare": {"mode": "seed-v1", "profile": "systemd-networkd", "username": "devops",
                          "probe": {"type": "ssh_banner", "port": 22}},
        "requires": ["mount", "umount"], "network": False, "reproducibility": "inputs_locked",
        "output": {"name": "debian-cloud-original", "version": "13-cloud-6.12.115", "path": str(Path(output).resolve())},
    }
    render.publish_recipe(recipe_path, result)
    return {"recipe": str(Path(recipe_path).resolve()), "architecture": architecture,
            "kernel_classification": "distribution-binary", "kernel_release": entry["kernel_release"],
            "boot_validation": "not_run"}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("architecture", choices=("x86_64", "aarch64"))
    parser.add_argument("--input-dir", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--recipe", required=True, type=Path)
    args = parser.parse_args()
    print(json.dumps(recipe(args.architecture, args.input_dir, args.output, args.recipe)))


if __name__ == "__main__":
    main()
