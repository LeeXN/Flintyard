#!/usr/bin/env python3
"""Verify locked fixture bytes and materialize a flat rootfs archive, offline.

OCI support here is limited to the locked, single-layer Rocky input. It copies
data only, never uses a registry/runtime, and is not a product image importer.
"""
import argparse
import copy
import gzip
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import tarfile
import tempfile

BASE = Path(__file__).resolve().parent
LIMIT = 64 << 30
DIGEST = re.compile(r"^[0-9a-f]{64}$")


def normalize_rootfs(source, destination, devices):
    """Copy archive data, omitting only explicitly locked devtmpfs nodes.

    No member is extracted. All other metadata and guest links stay in the
    archive for the product's normal, independent extraction checks.
    """
    expected = {item["path"]: (item["major"], item["minor"]) for item in devices}
    if len(expected) != len(devices) or any(not name.startswith("dev/") for name in expected):
        raise ValueError("invalid device normalization lock")
    removed, seen, expanded = set(), set(), 0
    with tarfile.open(source, "r|*") as archive, destination.open("xb") as raw:
        with gzip.GzipFile(filename="", mode="wb", compresslevel=6, mtime=0, fileobj=raw) as zipped:
            with tarfile.open(fileobj=zipped, mode="w|", format=tarfile.PAX_FORMAT) as out:
                for index, original in enumerate(archive):
                    if index >= 1000000:
                        raise ValueError("rootfs member count exceeds limit")
                    name = original.name
                    while name.startswith("./"):
                        name = name[2:]
                    name = name.rstrip("/")
                    if name not in ("", ".") and (name.startswith("/") or any(p in ("", ".", "..") for p in name.split("/"))):
                        raise ValueError("unsafe rootfs member path")
                    if name in seen:
                        raise ValueError("duplicate rootfs member")
                    seen.add(name)
                    expanded += original.size
                    if original.size < 0 or expanded > LIMIT:
                        raise ValueError("rootfs expansion exceeds limit")
                    if name in expected:
                        if not original.ischr() or (original.devmajor, original.devminor) != expected[name] or original.size:
                            raise ValueError("locked devtmpfs node differs from archive")
                        removed.add(name)
                        continue
                    if not (original.isfile() or original.isdir() or original.issym() or original.islnk()) or original.sparse is not None:
                        raise ValueError("unexpected special or sparse rootfs member")
                    member = copy.copy(original)
                    out.addfile(member, archive.extractfile(original) if original.isfile() else None)
        raw.flush()
        os.fsync(raw.fileno())
    if removed != set(expected):
        raise ValueError("locked devtmpfs nodes missing from archive")
    return sorted(removed)


def strict_json(data):
    def pairs(items):
        result = {}
        for key, value in items:
            if key in result:
                raise ValueError("duplicate JSON key: " + key)
            result[key] = value
        return result
    return json.loads(data.decode("utf-8", errors="strict"), object_pairs_hook=pairs)


def digest(path):
    h = hashlib.sha256()
    with Path(path).open("rb") as stream:
        for block in iter(lambda: stream.read(1 << 20), b""):
            h.update(block)
    return h.hexdigest()


def gzip_content_digest(path):
    """Hash the bounded uncompressed tar so gzip encoder versions can differ."""
    h, size = hashlib.sha256(), 0
    with gzip.open(path, "rb") as stream:
        for block in iter(lambda: stream.read(1 << 20), b""):
            size += len(block)
            if size > LIMIT:
                raise ValueError("normalized tar expansion exceeds limit")
            h.update(block)
    return h.hexdigest()


def blob_name(value):
    if not value.startswith("sha256:") or not DIGEST.fullmatch(value[7:]):
        raise ValueError("invalid OCI digest")
    return "blobs/sha256/" + value[7:]


def oci_single_layer(source, destination, architecture, expected_layer, expected_diff):
    # Never extract outer member names. Only validated ordinary blobs are read.
    with tarfile.open(source, "r:xz") as archive:
        members = {}
        for index, member in enumerate(archive):
            if index >= 4096:
                raise ValueError("OCI member count exceeds limit")
            name = member.name.rstrip("/")
            if member.name.startswith("/") or "\\" in name or any(p in ("", ".", "..") for p in name.split("/")):
                raise ValueError("unsafe OCI member path")
            if name in members or not (member.isfile() or member.isdir()) or member.size < 0 or member.size > LIMIT:
                raise ValueError("duplicate, linked, special or oversized OCI member")
            if name not in ("blobs", "blobs/sha256", "index.json", "oci-layout") and not re.fullmatch(r"blobs/sha256/[0-9a-f]{64}", name):
                raise ValueError("unexpected OCI member")
            members[name] = member

        def read_json(name, expected=None, size=None):
            member = members[name]
            if not member.isfile() or member.size > 1 << 20:
                raise ValueError("invalid OCI JSON member")
            data = archive.extractfile(member).read((1 << 20) + 1)
            if expected and hashlib.sha256(data).hexdigest() != expected.removeprefix("sha256:"):
                raise ValueError("OCI metadata digest mismatch")
            if size is not None and len(data) != size:
                raise ValueError("OCI metadata size mismatch")
            return strict_json(data)

        if read_json("oci-layout").get("imageLayoutVersion") != "1.0.0":
            raise ValueError("unsupported OCI layout")
        index = read_json("index.json")
        if index.get("schemaVersion") != 2 or len(index.get("manifests", [])) != 1:
            raise ValueError("fixture requires exactly one OCI manifest")
        descriptor = index["manifests"][0]
        manifest = read_json(blob_name(descriptor["digest"]), descriptor["digest"], descriptor["size"])
        if manifest.get("schemaVersion") != 2 or len(manifest.get("layers", [])) != 1:
            raise ValueError("fixture requires exactly one rootfs layer")
        config_desc = manifest["config"]
        config = read_json(blob_name(config_desc["digest"]), config_desc["digest"], config_desc["size"])
        if config.get("os") != "linux" or config.get("architecture") != {"x86_64": "amd64", "aarch64": "arm64"}[architecture]:
            raise ValueError("OCI rootfs architecture/OS mismatch")
        if config.get("rootfs", {}).get("diff_ids") != ["sha256:" + expected_diff]:
            raise ValueError("OCI uncompressed identity mismatch")
        layer = manifest["layers"][0]
        if layer.get("mediaType") != "application/vnd.oci.image.layer.v1.tar+gzip" or layer["digest"] != "sha256:" + expected_layer:
            raise ValueError("OCI layer format/digest differs from input lock")
        member = members[blob_name(layer["digest"])]
        if not member.isfile() or member.size != layer["size"]:
            raise ValueError("OCI layer size mismatch")
        h, count = hashlib.sha256(), 0
        with archive.extractfile(member) as stream, destination.open("xb") as out:
            for block in iter(lambda: stream.read(1 << 20), b""):
                count += len(block)
                if count > LIMIT:
                    raise ValueError("OCI layer exceeds limit")
                h.update(block)
                out.write(block)
            out.flush()
            os.fsync(out.fileno())
        if h.hexdigest() != expected_layer or count != layer["size"]:
            raise ValueError("OCI layer payload digest/size mismatch")
    h, count = hashlib.sha256(), 0
    with gzip.open(destination, "rb") as stream:
        for block in iter(lambda: stream.read(1 << 20), b""):
            count += len(block)
            if count > LIMIT:
                raise ValueError("OCI layer expansion exceeds limit")
            h.update(block)
    if h.hexdigest() != expected_diff:
        raise ValueError("OCI layer uncompressed digest mismatch")
    with tarfile.open(destination, "r:gz") as archive:
        for member in archive:
            if PurePosixPath(member.name).name.startswith(".wh."):
                raise ValueError("whiteouts require layering; not supported by this fixture normalizer")


def prepare(family, architecture, source, output):
    lock = strict_json((BASE / "lock.json").read_bytes())
    entry = lock["families"][family]["arches"][architecture]
    source, output = Path(source), Path(output)
    if not source.is_file() or source.is_symlink() or source.stat().st_size != entry["size_bytes"] or digest(source) != entry["sha256"]:
        raise ValueError("fixture source type, size or SHA-256 mismatch")
    output.parent.mkdir(parents=True, exist_ok=True)
    if output.exists() or output.is_symlink():
        raise FileExistsError("fixture output already exists")
    with tempfile.TemporaryDirectory(prefix=".fixture-", dir=output.parent) as temporary:
        staging = Path(temporary) / "rootfs"
        if entry["format"] == "oci-single-layer-tar-xz":
            oci_single_layer(source, staging, architecture, entry["rootfs_sha256"], entry["rootfs_diff_id"])
        elif entry["format"] in ("rootfs-tar-gzip", "rootfs-tar-xz"):
            with source.open("rb") as stream, staging.open("xb") as out:
                for block in iter(lambda: stream.read(1 << 20), b""):
                    out.write(block)
                out.flush()
                os.fsync(out.fileno())
        else:
            raise ValueError("unsupported locked fixture input format")
        if digest(staging) != entry["rootfs_sha256"]:
            raise ValueError("materialized rootfs digest mismatch")
        normalization = entry.get("normalization")
        removed = []
        if normalization:
            if normalization["format"] != "tar-gzip-v1":
                raise ValueError("unsupported rootfs normalization")
            normalized = Path(temporary) / "normalized"
            removed = normalize_rootfs(staging, normalized, normalization["drop_devices"])
            if digest(normalized) != normalization["sha256"] and gzip_content_digest(normalized) != normalization["tar_sha256"]:
                raise ValueError("normalized rootfs digest mismatch")
            staging = normalized
        staging.chmod(0o400)
        os.link(staging, output)  # Publish without overwriting an existing output.
        directory = os.open(output.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    return {"family": family, "architecture": architecture, "source_sha256": entry["sha256"],
            "rootfs_sha256": digest(output), "removed_devtmpfs_nodes": removed,
            "path": str(output.resolve()), "boot_validation": "not_run"}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("family", choices=("ubuntu", "debian", "rocky", "alpine"))
    parser.add_argument("architecture", choices=("x86_64", "aarch64"))
    parser.add_argument("--input", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    print(json.dumps(prepare(args.family, args.architecture, args.input, args.output)))


if __name__ == "__main__":
    main()
