#!/usr/bin/env python3
"""Attach a locked, signed offline APK repository to an Alpine root archive."""
import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import stat
import tarfile
import tempfile

import prepare

BASE = Path(__file__).resolve().parent
PREFIX = "var/lib/flintyard-fixture-apk"
NAME = re.compile(r"^[A-Za-z0-9_.+\-]+$")


def archive_name(name):
    if not name or len(name.encode("utf-8")) > 4096 or name.startswith("/") or "\x00" in name:
        raise ValueError("invalid Alpine archive path")
    while name.startswith("./"):
        name = name[2:]
    name = name.rstrip("/")
    if name not in ("", ".") and any(p in ("", ".", "..") for p in name.split("/")):
        raise ValueError("unsafe Alpine archive path")
    return name


class DigestReader:
    def __init__(self, stream):
        self.stream = stream
        self.hash = hashlib.sha256()
        self.count = 0

    def read(self, size=-1):
        data = self.stream.read(size)
        self.count += len(data)
        self.hash.update(data)
        return data


def bundle(source, destination, arch, lock, package_dir):
    root = Path(package_dir).resolve(strict=True)
    if arch not in ("x86_64", "aarch64"):
        raise ValueError("unsupported architecture")
    for part in (root / arch, root / arch / "main", root / arch / "community"):
        if part.is_symlink() or not part.is_dir():
            raise ValueError("package directory must contain ordinary architecture/repository directories")
    packages = lock["packages"][arch]
    if not packages or len(packages) > 4096:
        raise ValueError("invalid locked package set")
    seen, expanded, required_dirs = set(), 0, set()
    with tarfile.open(source, "r|gz") as archive, Path(destination).open("xb") as raw:
        with gzip.GzipFile(filename="", mode="wb", compresslevel=6, mtime=0, fileobj=raw) as zipped:
            with tarfile.open(fileobj=zipped, mode="w|", format=tarfile.PAX_FORMAT) as out:
                for index, member in enumerate(archive):
                    if index >= 1000000:
                        raise ValueError("rootfs member count exceeds limit")
                    name = archive_name(member.name)
                    if name in seen or name == PREFIX or name.startswith(PREFIX + "/"):
                        raise ValueError("duplicate or reserved rootfs path")
                    seen.add(name)
                    if name in ("var", "var/lib"):
                        if not member.isdir():
                            raise ValueError("offline repository ancestor is not a directory")
                        required_dirs.add(name)
                    if member.size < 0 or member.size > prepare.LIMIT - expanded:
                        raise ValueError("rootfs expansion exceeds limit")
                    expanded += member.size
                    if not (member.isfile() or member.isdir() or member.issym() or member.islnk()) or member.sparse is not None:
                        raise ValueError("unsupported special or sparse rootfs member")
                    out.addfile(member, archive.extractfile(member) if member.isfile() else None)
                if required_dirs != {"var", "var/lib"}:
                    raise ValueError("offline repository parent directories missing")

                def directory(name):
                    info = tarfile.TarInfo(name + "/")
                    info.type, info.mode, info.uid, info.gid, info.mtime = tarfile.DIRTYPE, 0o755, 0, 0, 0
                    out.addfile(info)

                def add_file(name, source_path, expected_sha, expected_size):
                    fd = os.open(source_path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
                    with os.fdopen(fd, "rb") as stream:
                        st = os.fstat(stream.fileno())
                        if not stat.S_ISREG(st.st_mode) or st.st_size != expected_size or expected_size < 1 or expected_size > prepare.LIMIT:
                            raise ValueError("offline package type or size mismatch")
                        info = tarfile.TarInfo(name)
                        info.mode, info.uid, info.gid, info.mtime, info.size = 0o644, 0, 0, 0, expected_size
                        reader = DigestReader(stream)
                        out.addfile(info, reader)
                        if reader.count != expected_size or reader.hash.hexdigest() != expected_sha:
                            raise ValueError("offline package digest mismatch")

                directory(PREFIX)
                for repo in ("main", "community"):
                    directory(PREFIX + "/" + repo)
                    directory(PREFIX + "/" + repo + "/" + arch)
                    index = lock["indexes"][arch][repo]
                    add_file(PREFIX + "/" + repo + "/" + arch + "/APKINDEX.tar.gz",
                             root / arch / repo / "APKINDEX.tar.gz", index["sha256"], index["size"])
                package_names = set()
                for package in sorted(packages, key=lambda p: (p["repository"], p["name"], p["version"])):
                    repo, name, version = package["repository"], package["name"], package["version"]
                    if repo not in ("main", "community") or not NAME.fullmatch(name) or not NAME.fullmatch(version):
                        raise ValueError("invalid package identity")
                    filename = name + "-" + version + ".apk"
                    if (repo, filename) in package_names:
                        raise ValueError("duplicate package identity")
                    package_names.add((repo, filename))
                    add_file(PREFIX + "/" + repo + "/" + arch + "/" + filename,
                             root / arch / repo / filename, package["sha256"], package["size"])
                repositories = ("file:///" + PREFIX + "/main\nfile:///" + PREFIX + "/community\n").encode()
                info = tarfile.TarInfo(PREFIX + "/repositories")
                info.mode, info.uid, info.gid, info.mtime, info.size = 0o644, 0, 0, 0, len(repositories)
                out.addfile(info, io.BytesIO(repositories))
        raw.flush()
        os.fsync(raw.fileno())


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("architecture", choices=("x86_64", "aarch64"))
    parser.add_argument("--rootfs", required=True, type=Path)
    parser.add_argument("--package-dir", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    lock = prepare.strict_json((BASE / "lock.json").read_bytes())
    entry = lock["families"]["alpine"]["arches"][args.architecture]
    if args.rootfs.is_symlink() or prepare.digest(args.rootfs) != entry["rootfs_sha256"]:
        raise ValueError("Alpine rootfs differs from the fixture input lock")
    package_data = (BASE / "alpine-packages.lock.json").read_bytes()
    if hashlib.sha256(package_data).hexdigest() != lock["families"]["alpine"]["offline_package_lock_sha256"]:
        raise ValueError("Alpine package lock digest mismatch")
    package_lock = prepare.strict_json(package_data)
    expected = entry["offline_rootfs_sha256"]
    args.output.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix=".alpine-bundle-", dir=args.output.parent) as directory:
        staged = Path(directory) / "rootfs.tar.gz"
        bundle(args.rootfs, staged, args.architecture, package_lock, args.package_dir)
        actual = prepare.digest(staged)
        if actual != expected and prepare.gzip_content_digest(staged) != entry["offline_rootfs_tar_sha256"]:
            raise ValueError("offline Alpine rootfs differs from the normalization lock")
        staged.chmod(0o400)
        os.link(staged, args.output)
        fd = os.open(args.output.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)
    print(json.dumps({"architecture": args.architecture, "rootfs_sha256": actual,
                      "packages": len(package_lock["packages"][args.architecture]), "boot_validation": "not_run"}))


if __name__ == "__main__":
    main()
