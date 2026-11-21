#!/usr/bin/env python3
"""Prepare locked Ubuntu rootfs and original kernel/modules without package execution."""
import argparse
import copy
import ctypes
import gzip
import hashlib
import json
import os
from pathlib import Path
import posixpath
import stat
import subprocess
import tarfile
import tempfile

import prepare

BASE = Path(__file__).resolve().parent


def member_name(value):
    if not value or value.startswith("/") or "\x00" in value or len(value.encode("utf-8")) > 4096:
        raise ValueError("invalid archive path")
    while value.startswith("./"):
        value = value[2:]
    value = value.rstrip("/")
    if value not in ("", ".") and any(part in ("", ".", "..") for part in value.split("/")):
        raise ValueError("unsafe archive path")
    return value


def package_target(name, release):
    selected = {"boot/vmlinuz-" + release, "boot/config-" + release, "boot/System.map-" + release,
                "lib/modprobe.d/blacklist_linux_" + release + ".conf"}
    prefixes = ("lib/modules/" + release, "lib/firmware/" + release)
    if name not in selected and not any(name == prefix or name.startswith(prefix + "/") for prefix in prefixes):
        return None
    return "usr/" + name if name.startswith("lib/") else name


def locked_input(path, sha, size=None, gzip_tar_sha=None):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    stream = os.fdopen(fd, "rb")
    try:
        st = os.fstat(stream.fileno())
        if not stat.S_ISREG(st.st_mode) or st.st_size < 1 or st.st_size > prepare.LIMIT or (size is not None and st.st_size != size):
            raise ValueError("locked input type or size mismatch")
        digest = hashlib.sha256()
        for data in iter(lambda: stream.read(1 << 20), b""):
            digest.update(data)
        if digest.hexdigest() != sha:
            if gzip_tar_sha is None:
                raise ValueError("locked input digest mismatch")
            stream.seek(0)
            expanded, count = hashlib.sha256(), 0
            with gzip.GzipFile(fileobj=stream, mode="rb") as gz:
                for data in iter(lambda: gz.read(1 << 20), b""):
                    count += len(data)
                    if count > prepare.LIMIT:
                        raise ValueError("locked rootfs expansion exceeds limit")
                    expanded.update(data)
            if expanded.hexdigest() != gzip_tar_sha:
                raise ValueError("locked rootfs content digest mismatch")
        stream.seek(0)
        return stream
    except BaseException:
        stream.close()
        raise


def publish_directory(source, destination):
    libc = ctypes.CDLL(None, use_errno=True)
    rename = getattr(libc, "renameat2", None)
    if rename is None:
        raise RuntimeError("atomic no-replace directory publication is unavailable")
    rename.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint]
    rename.restype = ctypes.c_int
    if rename(-100, os.fsencode(source), -100, os.fsencode(destination), 1) != 0:
        error = ctypes.get_errno()
        raise OSError(error, os.strerror(error), str(destination))
    fd = os.open(Path(destination).parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


class Capture:
    def __init__(self, stream, output):
        self.stream, self.output = stream, output
        self.hash = hashlib.sha256()
        self.size = 0

    def read(self, size=-1):
        data = self.stream.read(size)
        self.output.write(data)
        self.hash.update(data)
        self.size += len(data)
        return data


def copy_package(out, deb, kind, architecture, release, seen, output, entry, budget):
    suffix = {"x86_64": "amd64", "aarch64": "arm64"}[architecture]
    package = "linux-{}-{}".format("image" if kind == "image" else "modules", release)
    deb.seek(0)
    args = ["dpkg-deb", "-f", "/proc/self/fd/{}".format(deb.fileno()), "Package", "Version", "Architecture"]
    data = subprocess.check_output(args, pass_fds=(deb.fileno(),), timeout=120, text=True).splitlines()
    if data != ["Package: " + package, "Version: 6.8.0-147.147", "Architecture: " + suffix]:
        raise ValueError("Ubuntu package metadata differs from lock")
    deb.seek(0)
    found, count, package_seen = set(), 0, set()
    with tempfile.TemporaryFile() as errors:
        proc = subprocess.Popen(["dpkg-deb", "--fsys-tarfile", "/proc/self/fd/{}".format(deb.fileno())],
                                pass_fds=(deb.fileno(),), stdout=subprocess.PIPE, stderr=errors)
        try:
            with tarfile.open(fileobj=proc.stdout, mode="r|") as archive:
                for member in archive:
                    name = member_name(member.name)
                    if name in package_seen or len(package_seen) >= 1000000:
                        raise ValueError("duplicate or excessive package members")
                    package_seen.add(name)
                    target = package_target(name, release)
                    if target is None:
                        continue
                    if not (member.isfile() or member.isdir() or member.issym()) or member.sparse is not None or member.size < 0 or member.size > prepare.LIMIT:
                        raise ValueError("unsupported package member")
                    budget[0] += member.size
                    if budget[0] > prepare.LIMIT:
                        raise ValueError("prepared rootfs expansion exceeds limit")
                    if target in seen:
                        raise ValueError("package member collides with rootfs")
                    if member.issym():
                        resolved = posixpath.normpath(posixpath.join(posixpath.dirname(target), member.linkname))
                        if not resolved.startswith("usr/lib/modules/" + release + "/"):
                            raise ValueError("package symlink escapes the locked modules tree")
                    mapped = copy.copy(member)
                    mapped.name = target
                    mapped.pax_headers = dict(member.pax_headers)
                    if "path" in mapped.pax_headers:
                        mapped.pax_headers["path"] = target
                    source = archive.extractfile(member) if member.isfile() else None
                    capture_name = "kernel.packaged" if target == "boot/vmlinuz-" + release else "config" if target == "boot/config-" + release else None
                    if capture_name:
                        expected_sha = entry["packaged_kernel_sha256"] if capture_name == "kernel.packaged" else entry["config_sha256"]
                        expected_size = entry["packaged_kernel_size"] if capture_name == "kernel.packaged" else entry["config_size"]
                        with (output / capture_name).open("xb") as target_file:
                            capture = Capture(source, target_file)
                            out.addfile(mapped, capture)
                            target_file.flush()
                            os.fsync(target_file.fileno())
                        if capture.size != expected_size or capture.hash.hexdigest() != expected_sha:
                            raise ValueError("distribution boot member differs from lock")
                        found.add(capture_name)
                    else:
                        out.addfile(mapped, source)
                    seen[target] = (mapped.type, mapped.linkname)
                    count += 1
            if proc.wait(timeout=180) != 0:
                raise RuntimeError("dpkg-deb could not decode locked package")
        finally:
            if proc.poll() is None:
                proc.kill()
                proc.wait()
            proc.stdout.close()
    return found, count


def build(base_root, image_deb, modules_deb, architecture, entry, output):
    release = "6.8.0-147-generic"
    seen, count, budget = {}, 0, [0]
    rootfs = output / "rootfs.tar.gz"
    with tarfile.open(fileobj=base_root, mode="r|gz") as archive, rootfs.open("xb") as raw:
        with gzip.GzipFile(filename="", mode="wb", compresslevel=6, mtime=0, fileobj=raw) as zipped:
            with tarfile.open(fileobj=zipped, mode="w|", format=tarfile.PAX_FORMAT) as out:
                for member in archive:
                    name = member_name(member.name)
                    if name in seen or len(seen) >= 1000000 or not (member.isfile() or member.isdir() or member.issym() or member.islnk()) or member.sparse is not None:
                        raise ValueError("unexpected base rootfs member")
                    if member.size < 0 or member.size > prepare.LIMIT - budget[0]:
                        raise ValueError("base rootfs expansion exceeds limit")
                    budget[0] += member.size
                    seen[name] = (member.type, member.linkname)
                    out.addfile(member, archive.extractfile(member) if member.isfile() else None)
                for name in ("boot", "usr", "usr/lib", "usr/lib/modules", "usr/lib/modprobe.d"):
                    if name not in seen or seen[name][0] != tarfile.DIRTYPE:
                        raise ValueError("expected merged-usr directory is missing")
                if seen.get("lib") != (tarfile.SYMTYPE, "usr/lib"):
                    raise ValueError("base rootfs has unexpected /lib layout")
                if "usr/lib/firmware" not in seen:
                    directory = tarfile.TarInfo("usr/lib/firmware")
                    directory.type, directory.mode, directory.uid, directory.gid, directory.mtime = tarfile.DIRTYPE, 0o755, 0, 0, 0
                    out.addfile(directory)
                    seen[directory.name] = (directory.type, "")
                found_image, n = copy_package(out, image_deb, "image", architecture, release, seen, output, entry, budget)
                count += n
                found_modules, n = copy_package(out, modules_deb, "modules", architecture, release, seen, output, entry, budget)
                count += n
                if found_image != {"kernel.packaged"} or found_modules != {"config"}:
                    raise ValueError("locked kernel or config member missing")
        raw.flush()
        os.fsync(raw.fileno())
    if prepare.digest(rootfs) != entry["rootfs_sha256"] and prepare.gzip_content_digest(rootfs) != entry["rootfs_tar_sha256"]:
        raise ValueError("prepared Ubuntu rootfs differs from lock")
    packaged = output / "kernel.packaged"
    kernel = output / "kernel"
    if architecture == "aarch64":
        h, total = hashlib.sha256(), 0
        with gzip.open(packaged, "rb") as source, kernel.open("xb") as target:
            for data in iter(lambda: source.read(1 << 20), b""):
                total += len(data)
                if total > 512 << 20:
                    raise ValueError("decompressed kernel exceeds limit")
                h.update(data)
                target.write(data)
            target.flush()
            os.fsync(target.fileno())
        if h.hexdigest() != entry["runtime_kernel_sha256"]:
            raise ValueError("decompressed distribution kernel differs from lock")
    else:
        os.link(packaged, kernel)
    with kernel.open("rb") as stream:
        header = stream.read(0x238)
    if architecture == "x86_64" and (len(header) < 0x238 or header[0x202:0x206] != b"HdrS" or header[0x1fe:0x200] != b"\x55\xaa" or not int.from_bytes(header[0x236:0x238], "little") & 1):
        raise ValueError("distribution kernel is not an x86_64 boot image")
    if architecture == "aarch64" and (len(header) < 64 or header[56:60] != b"ARM\x64"):
        raise ValueError("distribution kernel is not an aarch64 Image")
    packaged.unlink()
    return count


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("architecture", choices=("x86_64", "aarch64"))
    for name in ("base-rootfs", "image-deb", "modules-deb", "output-dir"):
        parser.add_argument("--" + name, required=True, type=Path)
    args = parser.parse_args()
    fixture_lock = prepare.strict_json((BASE / "lock.json").read_bytes())
    data = (BASE / "ubuntu-original.lock.json").read_bytes()
    if hashlib.sha256(data).hexdigest() != fixture_lock["families"]["ubuntu"]["original_kernel_lock_sha256"]:
        raise ValueError("Ubuntu original kernel lock digest mismatch")
    lock = prepare.strict_json(data)
    if lock["kernel_release"] != "6.8.0-147-generic" or lock["package_version"] != "6.8.0-147.147":
        raise ValueError("unsupported Ubuntu package lock")
    entry = lock["architectures"][args.architecture]
    if args.output_dir.exists() or args.output_dir.is_symlink():
        raise FileExistsError("Ubuntu fixture output directory already exists")
    args.output_dir.parent.mkdir(parents=True, exist_ok=True)
    with locked_input(args.base_rootfs, entry["base_rootfs_sha256"], gzip_tar_sha=entry["base_rootfs_tar_sha256"]) as base, \
         locked_input(args.image_deb, entry["image_package"]["sha256"], entry["image_package"]["size"]) as image, \
         locked_input(args.modules_deb, entry["modules_package"]["sha256"], entry["modules_package"]["size"]) as modules, \
         tempfile.TemporaryDirectory(prefix=".ubuntu-original-", dir=args.output_dir.parent) as directory:
        work = Path(directory)
        with (work / "staging.json").open("x") as marker:
            json.dump({"kind": "UbuntuOriginalFixtureStaging", "publisher_pid": os.getpid(), "architecture": args.architecture}, marker)
            marker.write("\n")
            marker.flush()
            os.fsync(marker.fileno())
        fd = os.open(work, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)
        output = work / "output"
        output.mkdir(mode=0o700)
        count = build(base, image, modules, args.architecture, entry, output)
        metadata = {"api_version": "flintyard.fixture-ubuntu-original/v1", "architecture": args.architecture,
                    "kernel_release": lock["kernel_release"], "kernel_origin": "distribution-binary",
                    "rootfs_sha256": prepare.digest(output / "rootfs.tar.gz"), "kernel_sha256": entry["runtime_kernel_sha256"],
                    "config_sha256": entry["config_sha256"], "selected_package_members": count, "boot_validation": "not_run"}
        with (output / "provenance.json").open("x") as stream:
            json.dump(metadata, stream, indent=2)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        for path in output.iterdir():
            path.chmod(0o400)
            fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
            try:
                os.fsync(fd)
            finally:
                os.close(fd)
        fd = os.open(output, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)
        publish_directory(output, args.output_dir)
    print(json.dumps(metadata))


if __name__ == "__main__":
    main()
