#!/usr/bin/env python3
"""Render an offline recipe retaining Ubuntu's original kernel and modules."""
import argparse
import hashlib
import json
from pathlib import Path

import prepare
import render

BASE = Path(__file__).resolve().parent


def recipe(architecture, input_dir, output, recipe_path):
    fixture_lock = prepare.strict_json((BASE / "lock.json").read_bytes())
    data = (BASE / "ubuntu-original.lock.json").read_bytes()
    if hashlib.sha256(data).hexdigest() != fixture_lock["families"]["ubuntu"]["original_kernel_lock_sha256"]:
        raise ValueError("Ubuntu original kernel lock digest mismatch")
    lock = prepare.strict_json(data)
    entry = lock["architectures"][architecture]
    input_dir = Path(input_dir)
    rootfs, kernel, config = (input_dir / name for name in ("rootfs.tar.gz", "kernel", "config"))
    for path in (rootfs, kernel, config):
        if path.is_symlink() or not path.is_file():
            raise ValueError("Ubuntu original input is not an ordinary file")
    root_sha = prepare.digest(rootfs)
    if root_sha != entry["rootfs_sha256"] and prepare.gzip_content_digest(rootfs) != entry["rootfs_tar_sha256"]:
        raise ValueError("Ubuntu original rootfs differs from lock")
    if prepare.digest(kernel) != entry["runtime_kernel_sha256"] or prepare.digest(config) != entry["config_sha256"]:
        raise ValueError("Ubuntu original kernel or config differs from lock")
    metadata = prepare.strict_json((input_dir / "provenance.json").read_bytes())
    if metadata["architecture"] != architecture or metadata["kernel_release"] != lock["kernel_release"] or metadata["rootfs_sha256"] != root_sha:
        raise ValueError("Ubuntu original normalization provenance differs")
    release = lock["kernel_release"]
    fstab = "printf '%s\\n' '/dev/vda / ext4 defaults 0 1' > /etc/fstab"
    customize = ("set -e; id devops >/dev/null 2>&1 || useradd -m -s /bin/bash devops; "
                 "mkdir -p /etc/cloud; : > /etc/cloud/cloud-init.disabled; "
                 "systemctl disable ssh.socket 2>/dev/null || true; systemctl enable ssh.service")
    initramfs = "printf '%s\\n' 'RESUME=none' 'FSTYPE=ext4' > /etc/initramfs-tools/conf.d/flintyard-fixture"
    result = {
        "api_version": "flintyard/v1alpha1", "kind": "BuildRecipe", "architecture": architecture,
        "os": {"family": "ubuntu", "release": "24.04"},
        "rootfs": {"kind": "archive", "source": {"path": str(rootfs.resolve(strict=True)), "sha256": root_sha}, "size_mib": 4096},
        "kernel": {"mode": "prebuilt", "source": {"path": str(kernel.resolve(strict=True)), "sha256": entry["runtime_kernel_sha256"]},
                   "release": release, "origin": "distribution-binary", "modules": "rootfs"},
        "boot": {"root_device": "/dev/vda", "kernel_args": render.firecracker_boot_args(architecture)},
        "steps": [{"argv": ["/bin/sh", "-c", fstab], "working_dir": "/", "timeout_seconds": 60},
                  {"argv": ["/bin/sh", "-c", customize], "working_dir": "/", "timeout_seconds": 600},
                  {"argv": ["depmod", "-a", release], "working_dir": "/", "timeout_seconds": 600},
                  {"argv": ["/bin/sh", "-c", initramfs], "working_dir": "/", "timeout_seconds": 60},
                  {"argv": ["update-initramfs", "-c", "-k", release], "working_dir": "/", "timeout_seconds": 900}],
        "generated_initrd_path": "/boot/initrd.img-" + release,
        "guest_prepare": {"mode": "seed-v1", "profile": "systemd-networkd", "username": "devops",
                          "probe": {"type": "ssh_banner", "port": 22}},
        "requires": [], "network": False, "reproducibility": "inputs_locked",
        "output": {"name": "ubuntu-original-kernel", "version": "24.04-original-6.8.0-147", "path": str(Path(output).resolve())},
    }
    render.publish_recipe(recipe_path, result)
    return {"recipe": str(Path(recipe_path).resolve()), "architecture": architecture,
            "kernel_classification": "distribution-binary", "kernel_release": release, "boot_validation": "not_run"}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("architecture", choices=("x86_64", "aarch64"))
    for name in ("input-dir", "output", "recipe"):
        parser.add_argument("--" + name, required=True, type=Path)
    args = parser.parse_args()
    print(json.dumps(recipe(args.architecture, args.input_dir, args.output, args.recipe)))


if __name__ == "__main__":
    main()
