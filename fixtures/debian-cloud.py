#!/usr/bin/env python3
"""Normalize a locked Debian cloud disk to a direct ext4 root and boot files."""
import argparse
import ctypes
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import tarfile
import tempfile

import prepare

BASE = Path(__file__).resolve().parent
RELEASE = re.compile(r"^[A-Za-z0-9.+_\-]+$")


def validate_sparse(member, expected_size):
    if member.name != "disk.raw" or not member.isfile() or member.size != expected_size:
        raise ValueError("cloud archive must contain the locked ordinary disk member")
    end = 0
    for offset, size in member.sparse or []:
        if offset < end or size <= 0 or offset + size > expected_size:
            raise ValueError("invalid cloud sparse extent")
        end = offset + size


def validate_partition(table, entry):
    if table.get("label") != "gpt" or table.get("sectorsize") != 512:
        raise ValueError("cloud disk is not the locked GPT sector format")
    parts = table.get("partitions", [])
    if not parts or len(parts) > 128:
        raise ValueError("invalid cloud partition count")
    ranges, matches = [], []
    for part in parts:
        start, count = part.get("start"), part.get("size")
        if type(start) is not int or type(count) is not int or start < 1 or count < 1:
            raise ValueError("invalid cloud partition bounds")
        first, last = start * 512, (start + count) * 512
        if last > entry["disk_size"] or any(first < b and a < last for a, b in ranges):
            raise ValueError("cloud partitions overlap or exceed the disk")
        ranges.append((first, last))
        if part.get("type", "").upper() == entry["partition_type"]:
            matches.append((part, first, last - first))
    if len(matches) != 1:
        raise ValueError("cloud disk has no unique root partition")
    part, offset, size = matches[0]
    if (part.get("uuid", "").upper(), offset, size) != (entry["partition_uuid"], entry["partition_offset"], entry["partition_size"]):
        raise ValueError("cloud root partition identity differs from lock")
    return offset, size


def sparse_copy(source, destination, expected_size, expected_sha):
    digest, count = hashlib.sha256(), 0
    with Path(destination).open("xb") as out:
        while count < expected_size:
            data = source.read(min(1 << 20, expected_size - count))
            if not data:
                raise ValueError("cloud disk or root partition is truncated")
            digest.update(data)
            count += len(data)
            if data.strip(b"\x00"):
                out.write(data)
            else:
                out.seek(len(data), os.SEEK_CUR)
        out.truncate(count)
        out.flush()
        os.fsync(out.fileno())
    if digest.hexdigest() != expected_sha:
        raise ValueError("cloud disk or root partition digest mismatch")


def extract_boot(rootfs, guest_name, output, expected):
    if not RELEASE.fullmatch(guest_name):
        raise ValueError("invalid locked boot filename")
    fd = os.open(output, os.O_CREAT | os.O_EXCL | os.O_RDWR | os.O_CLOEXEC, 0o600)
    try:
        # The destination is an already-open file, so debugfs never parses a
        # user-supplied destination path as part of its command language.
        result = subprocess.run(["debugfs", "-R", "dump /boot/{} /proc/self/fd/{}".format(guest_name, fd), str(rootfs)],
                                pass_fds=(fd,), stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=120)
        if result.returncode:
            raise RuntimeError("debugfs failed to copy a locked boot file")
        os.fsync(fd)
    finally:
        os.close(fd)
    if Path(output).stat().st_size != expected["size"] or prepare.digest(output) != expected["sha256"]:
        raise ValueError("distribution boot file differs from lock")


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


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("architecture", choices=("x86_64", "aarch64"))
    parser.add_argument("--archive", required=True, type=Path)
    parser.add_argument("--output-dir", required=True, type=Path)
    args = parser.parse_args()
    fixture_lock = prepare.strict_json((BASE / "lock.json").read_bytes())
    lock_data = (BASE / "debian-cloud.lock.json").read_bytes()
    if hashlib.sha256(lock_data).hexdigest() != fixture_lock["families"]["debian"]["cloud_lock_sha256"]:
        raise ValueError("Debian cloud lock digest mismatch")
    entry = prepare.strict_json(lock_data)["architectures"][args.architecture]
    source_fd = os.open(args.archive, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    source = os.fdopen(source_fd, "rb")
    source_stat = os.fstat(source.fileno())
    if not stat.S_ISREG(source_stat.st_mode) or source_stat.st_size != entry["archive_size"]:
        source.close()
        raise ValueError("Debian cloud archive type or size differs from input lock")
    h = hashlib.sha256()
    for data in iter(lambda: source.read(1 << 20), b""):
        h.update(data)
    if h.hexdigest() != entry["archive_sha256"]:
        source.close()
        raise ValueError("Debian cloud archive digest differs from input lock")
    source.seek(0)
    if args.output_dir.exists() or args.output_dir.is_symlink():
        source.close()
        raise FileExistsError("Debian cloud output directory already exists")
    args.output_dir.parent.mkdir(parents=True, exist_ok=True)
    with source, tempfile.TemporaryDirectory(prefix=".debian-cloud-", dir=args.output_dir.parent) as directory:
        work = Path(directory)
        with (work / "staging.json").open("x") as marker:
            json.dump({"kind": "DebianCloudFixtureStaging", "archive_sha256": entry["archive_sha256"], "publisher_pid": os.getpid()}, marker)
            marker.write("\n")
            marker.flush()
            os.fsync(marker.fileno())
        work_fd = os.open(work, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(work_fd)
        finally:
            os.close(work_fd)
        disk, output = work / "disk.raw", work / "output"
        output.mkdir(mode=0o700)
        with tarfile.open(fileobj=source, mode="r|xz") as archive:
            member = archive.next()
            if member is None:
                raise ValueError("cloud archive is empty")
            validate_sparse(member, entry["disk_size"])
            with archive.extractfile(member) as stream:
                sparse_copy(stream, disk, entry["disk_size"], entry["disk_sha256"])
            if archive.next() is not None:
                raise ValueError("cloud archive contains an unexpected extra member")
        table = json.loads(subprocess.check_output(["sfdisk", "--json", str(disk)], timeout=120))["partitiontable"]
        offset, size = validate_partition(table, entry)
        rootfs = output / "rootfs.raw"
        with disk.open("rb") as stream:
            stream.seek(offset)
            sparse_copy(stream, rootfs, size, entry["rootfs_sha256"])
        with rootfs.open("rb") as stream:
            stream.seek(1080)
            if stream.read(2) != b"\x53\xef":
                raise ValueError("locked root partition is not ext4")
        release = entry["kernel_release"]
        if not RELEASE.fullmatch(release):
            raise ValueError("invalid distribution kernel release")
        for role, prefix in (("kernel", "vmlinuz-"), ("initrd", "initrd.img-"), ("config", "config-")):
            extract_boot(rootfs, prefix + release, output / role, entry["boot_files"][role])
        metadata = {"api_version": "flintyard.fixture-cloud-output/v1", "architecture": args.architecture,
                    "archive_sha256": entry["archive_sha256"], "rootfs_sha256": entry["rootfs_sha256"],
                    "kernel_release": release, "boot_files": entry["boot_files"], "boot_validation": "not_run"}
        with (output / "provenance.json").open("x") as stream:
            json.dump(metadata, stream, indent=2)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        for file in output.iterdir():
            file.chmod(0o400)
            fd = os.open(file, os.O_RDONLY | os.O_NOFOLLOW)
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
