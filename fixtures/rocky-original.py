#!/usr/bin/env python3
"""Verify a locked Rocky RPM set and render a native, offline image recipe."""
import argparse
import gzip
import hashlib
import io
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import tarfile
import tempfile
import zlib

import prepare
from render import firecracker_boot_args, publish_recipe

BASE = Path(__file__).resolve().parent
PACKAGE_PATH = "var/lib/flintyard-fixture-rpm"
RPM_FILENAME = re.compile(r"[A-Za-z0-9_.+~-]+\.rpm")


def run(*args, **kwargs):
    return subprocess.run(args, check=True, **kwargs)


def extract_member(rpm, name):
    with subprocess.Popen(["rpm2cpio", str(rpm)], stdout=subprocess.PIPE) as cpio:
        try:
            result = run("cpio", "--extract", "--to-stdout", "--quiet", "./" + name,
                         stdin=cpio.stdout, capture_output=True).stdout
        finally:
            cpio.stdout.close()
        if cpio.wait():
            raise ValueError("could not read kernel RPM payload")
    return result


def verify_packages(package_dir, entries, kernel_entry, key, database):
    if not (1 <= len(entries) <= 248):
        raise ValueError("invalid locked RPM count")
    run("rpmkeys", "--dbpath", str(database), "--import", str(key))
    paths = []
    for name, expected in sorted(entries.items()):
        if not RPM_FILENAME.fullmatch(name):
            raise ValueError("invalid locked RPM filename")
        path = package_dir / name
        st = path.lstat()
        if not stat.S_ISREG(st.st_mode) or st.st_size != expected["size"] or prepare.digest(path) != expected["sha256"]:
            raise ValueError(f"RPM differs from its lock: {name}")
        check = run("rpmkeys", "--dbpath", str(database), "--checksig", str(path),
                    capture_output=True, text=True, env={**os.environ, "LC_ALL": "C"})
        if "signature" not in check.stdout.lower() or not check.stdout.rstrip().endswith("OK"):
            raise ValueError(f"missing verified RPM signature: {name}")
        paths.append(path)
    core_name = "kernel-core-" + kernel_entry["kernel_release"] + ".rpm"
    if core_name not in entries or entries[core_name]["sha256"] != kernel_entry["package_sha256"]:
        raise ValueError("RPM set does not contain the locked original kernel")
    return paths, package_dir / core_name


def append_packages(rootfs, output, paths, entries):
    with tarfile.open(rootfs, "r|gz") as source, output.open("xb") as raw, \
            gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as compressed, \
            tarfile.open(fileobj=compressed, mode="w|", format=tarfile.PAX_FORMAT) as archive:
        parents = set()
        for member in source:
            name = member.name.rstrip("/")
            if name == PACKAGE_PATH or name.startswith(PACKAGE_PATH + "/"):
                raise ValueError("base root contains reserved RPM directory")
            if name in ("var", "var/lib"):
                if not member.isdir():
                    raise ValueError("RPM directory has a non-directory parent")
                parents.add(name)
            archive.addfile(member, source.extractfile(member) if member.isfile() else None)
        if parents != {"var", "var/lib"}:
            raise ValueError("RPM directory parents missing")
        folder = tarfile.TarInfo(PACKAGE_PATH)
        folder.type, folder.mode = tarfile.DIRTYPE, 0o755
        archive.addfile(folder)
        for path in paths:
            expected = entries[path.name]
            fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
            with os.fdopen(fd, "rb") as stream:
                data = stream.read(expected["size"] + 1)
            if len(data) != expected["size"] or hashlib.sha256(data).hexdigest() != expected["sha256"]:
                raise ValueError(f"RPM changed during bundling: {path.name}")
            info = tarfile.TarInfo(PACKAGE_PATH + "/" + path.name)
            info.mode, info.size = 0o644, len(data)
            archive.addfile(info, io.BytesIO(data))


def prepare_recipe(arch, rootfs, package_dir, key, output_dir, image, recipe_path):
    family = prepare.strict_json((BASE / "lock.json").read_bytes())["families"]["rocky"]
    kernel_data = (BASE / "rocky-original-kernel.lock.json").read_bytes()
    package_data = (BASE / "rocky-packages.lock.json").read_bytes()
    if (hashlib.sha256(kernel_data).hexdigest() != family["original_kernel_lock_sha256"] or
            hashlib.sha256(package_data).hexdigest() != family["offline_package_lock_sha256"]):
        raise ValueError("Rocky fixture locks differ from their recorded digests")
    kernel_lock = prepare.strict_json(kernel_data)
    package_lock = prepare.strict_json(package_data)
    base = family["arches"][arch]
    kernel = kernel_lock["architectures"][arch]
    if rootfs.is_symlink() or prepare.digest(rootfs) != base["rootfs_sha256"]:
        raise ValueError("Rocky base root differs from its lock")
    if key.is_symlink() or prepare.digest(key) != kernel_lock["signing_key_sha256"]:
        raise ValueError("Rocky signing key differs from its lock")
    if package_lock["kernel_package_version"] != kernel_lock["package_version"]:
        raise ValueError("Rocky package and kernel locks disagree")
    release = kernel["kernel_release"]
    if not re.fullmatch(r"[A-Za-z0-9_.+-]+", release):
        raise ValueError("invalid locked release")

    output_dir.mkdir(mode=0o700, parents=True, exist_ok=False)
    try:
        with tempfile.TemporaryDirectory(prefix=".rpmdb-", dir=output_dir) as directory:
            entries = package_lock["packages"][arch]
            packages, core = verify_packages(package_dir, entries, kernel, key, Path(directory))
            boot = extract_member(core, f"lib/modules/{release}/vmlinuz")
            config = extract_member(core, f"lib/modules/{release}/config")
            if hashlib.sha256(boot).hexdigest() != kernel["packaged_kernel_sha256"]:
                raise ValueError("packaged Rocky kernel digest mismatch")
            if hashlib.sha256(config).hexdigest() != kernel["config_sha256"]:
                raise ValueError("packaged Rocky config digest mismatch")
            if arch == "aarch64":
                boot = zlib.decompress(boot[kernel["embedded_gzip_offset"]:], wbits=31)
            if hashlib.sha256(boot).hexdigest() != kernel["runtime_kernel_sha256"]:
                raise ValueError("runtime Rocky kernel digest mismatch")
            (output_dir / "kernel").write_bytes(boot)
            (output_dir / "kernel.config").write_bytes(config)
            bundled = output_dir / "rootfs.tar.gz"
            append_packages(rootfs, bundled, packages, entries)
        rpm_args = ["/" + PACKAGE_PATH + "/" + p.name for p in packages]
        initrd = f"/boot/initramfs-{release}.img"
        steps = [
            ["rpmkeys", "--import", "/etc/pki/rpm-gpg/RPM-GPG-KEY-Rocky-9"],
            ["dnf", "-y", "--disablerepo=*", "--setopt=install_weak_deps=False",
             "--setopt=localpkg_gpgcheck=True", "--setopt=keepcache=False", "install", *rpm_args],
            ["depmod", release],
            ["dracut", "--force", "--no-hostonly", "--no-hostonly-cmdline", "--add-drivers",
             "virtio_pci virtio_blk virtio_net ext4", initrd, release],
            ["/bin/sh", "-ec", "id devops >/dev/null 2>&1 || useradd -m -s /bin/bash devops; "
             "mkdir -p /etc/cloud; : > /etc/cloud/cloud-init.disabled; "
             "printf '%s\\n' 'UsePAM yes' > /etc/ssh/sshd_config; "
             "printf '%s\\n' '/dev/vda / ext4 defaults 0 1' > /etc/fstab; "
             "systemctl enable sshd.service; dnf --disablerepo='*' clean all"],
            ["rm", "-r", "--", "/" + PACKAGE_PATH],
        ]
        recipe = {
            "api_version": "flintyard/v1alpha1", "kind": "BuildRecipe", "architecture": arch,
            "os": {"family": "rocky", "release": kernel_lock["release"]},
            "rootfs": {"kind": "archive", "size_mib": 4096,
                       "source": {"path": str(bundled), "sha256": prepare.digest(bundled)}},
            "kernel": {"mode": "prebuilt", "source": {"path": str(output_dir / "kernel"), "sha256": kernel["runtime_kernel_sha256"]},
                       "release": release, "origin": "distribution-binary", "modules": "rootfs"},
            "boot": {"root_device": "/dev/vda",
                     "kernel_args": firecracker_boot_args(arch),
                     "virtio_transport": "pci"},
            "generated_initrd_path": initrd, "network": False, "reproducibility": "inputs_locked",
            "guest_prepare": {"mode": "seed-v1", "profile": "networkmanager", "username": "devops",
                              "probe": {"type": "ssh_banner", "port": 22}},
            "steps": [{"argv": argv, "working_dir": "/", "timeout_seconds": 1800} for argv in steps],
            "requires": ["mount", "umount"],
            "output": {"name": "rocky-original-pci", "version": kernel_lock["release"], "path": str(image)},
        }
        publish_recipe(recipe_path, recipe)
    except BaseException:
        shutil.rmtree(output_dir)
        raise


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("architecture", choices=("x86_64", "aarch64"))
    for name in ("rootfs", "package-dir", "key", "output-dir", "image", "recipe"):
        parser.add_argument("--" + name, required=True, type=Path)
    args = parser.parse_args()
    prepare_recipe(args.architecture, args.rootfs.absolute(), args.package_dir.resolve(strict=True), args.key.absolute(),
                   args.output_dir.resolve(), args.image.resolve(), args.recipe.absolute())


if __name__ == "__main__":
    main()
