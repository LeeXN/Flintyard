#!/usr/bin/env python3
"""Render an explicit JSON recipe from verified fixture inputs, not runtime inventory."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import tempfile

import prepare

BASE = Path(__file__).resolve().parent


def digest(path):
    h = hashlib.sha256()
    with path.open("rb") as stream:
        for data in iter(lambda: stream.read(1 << 20), b""):
            h.update(data)
    return h.hexdigest()


def firecracker_boot_args(architecture):
    args = ["console=ttyS0", "reboot=k", "panic=1", "rw"]
    if architecture == "aarch64":
        args.insert(0, "keep_bootcon")
    return args


def publish_recipe(recipe_path, recipe):
    recipe_path = Path(recipe_path)
    recipe_path.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix=".fixture-recipe-", dir=recipe_path.parent) as directory:
        staged = Path(directory) / "recipe.json"
        with staged.open("x") as stream:
            json.dump(recipe, stream, indent=2)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.link(staged, recipe_path)
        parent = os.open(recipe_path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(parent)
        finally:
            os.close(parent)


def render(family, arch, rootfs, kernel, config, output, recipe_path):
    lock = json.loads((BASE / "lock.json").read_text())
    selected = lock["families"][family]
    rootfs, kernel, config = [Path(p).resolve(strict=True) for p in (rootfs, kernel, config)]
    root_entry = selected["arches"][arch]
    expected_rootfs = root_entry["normalization"]["sha256"] if "normalization" in root_entry else root_entry["rootfs_sha256"]
    root_sha = digest(rootfs)
    offline_alpine = family == "alpine" and (root_sha == root_entry.get("offline_rootfs_sha256") or
        ("offline_rootfs_tar_sha256" in root_entry and prepare.gzip_content_digest(rootfs) == root_entry["offline_rootfs_tar_sha256"]))
    normalized = root_sha == expected_rootfs or ("normalization" in root_entry and
        prepare.gzip_content_digest(rootfs) == root_entry["normalization"]["tar_sha256"])
    if (not normalized and not offline_alpine) or digest(kernel) != lock["kernel"]["sha256"]:
        raise ValueError("rootfs/kernel differs from fixture input lock")
    metadata = json.loads(Path(str(config) + ".json").read_text())
    config_sha = digest(config)
    if metadata["architecture"] != arch or metadata["kernel_release"] != lock["kernel"]["release"] or metadata["config_sha256"] != config_sha:
        raise ValueError("native generated config identity mismatch")
    modules = metadata["modules"]
    if family != "ubuntu" and modules != "disabled":
        raise ValueError("minimal roots lack depmod before recipe steps; explicitly use --modules disabled or prepare a root containing kmod")
    release = lock["kernel"]["release"]
    package_steps = {
        "ubuntu": [],
        "debian": ["apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y openssh-server iproute2 util-linux systemd-sysv systemd-resolved kmod"],
        "rocky": ["dnf install -y systemd NetworkManager openssh-server iproute util-linux kmod && dnf clean all"],
        "alpine": ["apk add --no-cache openrc openssh-server iproute2 util-linux coreutils shadow busybox-openrc busybox-mdev-openrc"],
    }[family]
    offline_step = None
    if offline_alpine:
        package_data = (BASE / "alpine-packages.lock.json").read_bytes()
        if hashlib.sha256(package_data).hexdigest() != selected["offline_package_lock_sha256"]:
            raise ValueError("Alpine package lock digest mismatch")
        package_lock = json.loads(package_data)
        selected_packages = []
        for name in ("openrc", "openssh-server", "iproute2", "util-linux", "coreutils", "shadow", "busybox-openrc", "busybox-mdev-openrc"):
            versions = [p["version"] for p in package_lock["packages"][arch] if p["name"] == name]
            if len(versions) != 1 or not re.fullmatch(r"[A-Za-z0-9_.+\-]+", versions[0]):
                raise ValueError("missing or invalid locked Alpine package version")
            selected_packages.append(name + "=" + versions[0])
        offline_step = {"argv": ["apk", "add", "--no-network", "--no-cache", "--repositories-file",
                                 "/var/lib/flintyard-fixture-apk/repositories"] + selected_packages,
                        "working_dir": "/", "timeout_seconds": 1800}
        package_steps = []
    # These are trusted, explicit build commands. Minimal-root package resolution
    # is marked best_effort until a separately verified package bundle is locked.
    if family == "alpine":
        customization = "set -e; id devops >/dev/null 2>&1 || useradd -m -s /bin/sh devops; usermod -p NP devops; rc-update add devfs sysinit; rc-update add dmesg sysinit; rc-update add mdev sysinit; rc-update add hwdrivers sysinit; rc-update add bootmisc boot; rc-update add hostname boot; rc-update add syslog boot"
    elif family == "rocky":
        # Rocky's default UsePAM directive is in an included file. Explicitly
        # declare it for guest_prepare, which replaces the SSH configuration.
        customization = ("set -e; id devops >/dev/null 2>&1 || useradd -m -s /bin/bash devops; "
                         "mkdir -p /etc/cloud; : > /etc/cloud/cloud-init.disabled; "
                         "printf '%s\\n' 'UsePAM yes' > /etc/ssh/sshd_config; "
                         "systemctl enable sshd.service")
    else:
        customization = "id devops >/dev/null 2>&1 || useradd -m -s /bin/bash devops; mkdir -p /etc/cloud; : > /etc/cloud/cloud-init.disabled; systemctl disable ssh.socket 2>/dev/null || true; systemctl enable ssh.service 2>/dev/null || systemctl enable sshd.service"
    # The archive may describe a cloud disk label that is not used by the
    # generated ext4 volume. Bind its root mount to this recipe's declared
    # virtio root device instead of inheriting the source image's fstab.
    fstab = "printf '%s\\n' '/dev/vda / ext4 defaults 0 1' > /etc/fstab"
    steps = [{"argv": ["/bin/sh", "-c", command], "working_dir": "/", "timeout_seconds": 1800}
             for command in package_steps + [fstab, customization]]
    if offline_step:
        steps.insert(0, offline_step)
        steps.append({"argv": ["/bin/sh", "-c", "rm -rf -- /var/lib/flintyard-fixture-apk"],
                      "working_dir": "/", "timeout_seconds": 120})
    generated = None
    if family == "ubuntu" and modules == "enabled":
        # initramfs-tools otherwise probes /proc/swaps on the build host and
        # may bake its unrelated swap device into the guest initrd.
        steps.append({"argv": ["/bin/sh", "-c", "printf '%s\\n' 'RESUME=none' 'FSTYPE=ext4' > /etc/initramfs-tools/conf.d/flintyard-fixture"],
                      "working_dir": "/", "timeout_seconds": 60})
        steps.append({"argv": ["update-initramfs", "-c", "-k", release], "working_dir": "/", "timeout_seconds": 900})
        generated = "/boot/initrd.img-" + release
    recipe = {
        "api_version": "flintyard/v1alpha1", "kind": "BuildRecipe", "architecture": arch,
        "os": {"family": family, "release": selected["release"]},
        "rootfs": {"kind": "archive", "source": {"path": str(rootfs), "sha256": root_sha}, "size_mib": 1024 if family == "alpine" else 4096},
        "kernel": {"mode": "source", "source": {"path": str(kernel), "sha256": digest(kernel)}, "strip_components": 1,
                   "config_file": str(config), "config_sha256": config_sha, "patches": [],
                   "image_target": "vmlinux" if arch == "x86_64" else "Image", "jobs": 8, "timeout_seconds": 7200,
                   "origin": "upstream-source", "expected_release": release},
        "boot": {"root_device": "/dev/vda", "kernel_args": firecracker_boot_args(arch)},
        "steps": steps, "guest_prepare": {"mode": "seed-v1", "profile": selected["profile"], "username": "devops",
                                         "probe": {"type": "ssh_banner", "port": 22}},
        "requires": ["make", "gcc", "ld", "flex", "bison", "bc", "xz"], "network": bool(package_steps),
        "reproducibility": "best_effort" if package_steps else "inputs_locked",
        "output": {"name": family + "-fixture", "version": selected["release"] + "-linux-" + release, "path": str(Path(output).resolve())},
    }
    if generated:
        recipe["generated_initrd_path"] = generated
    recipe_path = Path(recipe_path)
    publish_recipe(recipe_path, recipe)
    return {"recipe": str(recipe_path.resolve()), "family": family, "architecture": arch,
            "kernel_classification": "explicit adapted upstream source", "reproducibility": recipe["reproducibility"], "boot_validation": "not_run"}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("family", choices=("ubuntu", "debian", "rocky", "alpine"))
    parser.add_argument("architecture", choices=("x86_64", "aarch64"))
    for arg in ("rootfs", "kernel", "config", "output", "recipe"):
        parser.add_argument("--" + arg, required=True, type=Path)
    args = parser.parse_args()
    print(json.dumps(render(args.family, args.architecture, args.rootfs, args.kernel, args.config, args.output, args.recipe)))


if __name__ == "__main__":
    main()
