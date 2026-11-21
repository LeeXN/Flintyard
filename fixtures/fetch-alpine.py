#!/usr/bin/env python3
"""Fetch exactly the Alpine packages in the fixture lock, with SHA verification."""
import argparse
import concurrent.futures
import hashlib
import http.client
import json
import os
from pathlib import Path
import re
import stat
import ssl
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request

import prepare

BASE = Path(__file__).resolve().parent
NAME = re.compile(r"^[A-Za-z0-9_.+\-]+$")


def checked_file(path, expected_sha, expected_size):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as stream:
        st = os.fstat(stream.fileno())
        if not stat.S_ISREG(st.st_mode) or st.st_size != expected_size:
            raise ValueError("existing package has unexpected type or size")
        h = hashlib.sha256()
        for data in iter(lambda: stream.read(1 << 20), b""):
            h.update(data)
        if h.hexdigest() != expected_sha:
            raise ValueError("existing package digest mismatch")


def publish(path, expected_sha, expected_size, source):
    if not re.fullmatch(r"[0-9a-f]{64}", expected_sha) or expected_size < 1 or expected_size > prepare.LIMIT:
        raise ValueError("invalid locked package digest or size")
    path = Path(path)
    if path.exists() or path.is_symlink():
        checked_file(path, expected_sha, expected_size)
        return "reused"
    with tempfile.TemporaryDirectory(prefix=".apk-fetch-", dir=path.parent) as directory:
        staged = Path(directory) / "data"
        h, total = hashlib.sha256(), 0
        with source() as incoming, staged.open("xb") as output:
            for chunk in iter(lambda: incoming.read(1 << 20), b""):
                total += len(chunk)
                if total > expected_size:
                    raise ValueError("package exceeds locked size")
                h.update(chunk)
                output.write(chunk)
            output.flush()
            os.fsync(output.fileno())
        if total != expected_size or h.hexdigest() != expected_sha:
            raise ValueError("package differs from locked size or digest")
        staged.chmod(0o400)
        try:
            os.link(staged, path)
        except FileExistsError:
            checked_file(path, expected_sha, expected_size)
            return "reused"
        fd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)
    return "downloaded"


class OfficialRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        parsed = urllib.parse.urlsplit(newurl)
        if parsed.scheme != "https" or parsed.hostname != "dl-cdn.alpinelinux.org" or parsed.username or parsed.password:
            raise ValueError("unsafe Alpine package redirect")
        return super().redirect_request(req, fp, code, msg, headers, newurl)


def retry_fetch(operation, attempts=3):
    for attempt in range(attempts):
        try:
            return operation()
        except urllib.error.HTTPError as err:
            if err.code != 429 and not 500 <= err.code < 600:
                raise
            if attempt + 1 == attempts:
                raise
        except (urllib.error.URLError, TimeoutError, ConnectionError, ssl.SSLError, http.client.IncompleteRead):
            if attempt + 1 == attempts:
                raise
        time.sleep(0.25 * (2 ** attempt))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("architecture", choices=("x86_64", "aarch64"))
    parser.add_argument("--output-dir", required=True, type=Path)
    args = parser.parse_args()
    fixture_lock = prepare.strict_json((BASE / "lock.json").read_bytes())
    data = (BASE / "alpine-packages.lock.json").read_bytes()
    if hashlib.sha256(data).hexdigest() != fixture_lock["families"]["alpine"]["offline_package_lock_sha256"]:
        raise ValueError("Alpine package lock digest mismatch")
    lock = prepare.strict_json(data)
    args.output_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
    if args.output_dir.is_symlink() or not args.output_dir.is_dir():
        raise ValueError("package output root is not a directory")
    for name in (args.architecture, args.architecture + "/main", args.architecture + "/community"):
        path = args.output_dir / name
        path.mkdir(mode=0o700, parents=True, exist_ok=True)
        if path.is_symlink() or not path.is_dir():
            raise ValueError("package output path is not an ordinary directory")
    for repo in ("main", "community"):
        index = lock["indexes"][args.architecture][repo]
        source_path = BASE / "alpine-indexes" / args.architecture / repo / "APKINDEX.tar.gz"
        publish(args.output_dir / args.architecture / repo / "APKINDEX.tar.gz", index["sha256"], index["size"],
                lambda p=source_path: p.open("rb"))
    opener = urllib.request.build_opener(OfficialRedirect())
    tasks, identities = [], set()
    for package in lock["packages"][args.architecture]:
        repo, name, version = package["repository"], package["name"], package["version"]
        if repo not in ("main", "community") or not NAME.fullmatch(name) or not NAME.fullmatch(version):
            raise ValueError("invalid locked package identity")
        filename = name + "-" + version + ".apk"
        url = "https://dl-cdn.alpinelinux.org/alpine/{}/{}/{}/{}".format(lock["repository_release"], repo, args.architecture, filename)
        if package["url"] != url or (repo, filename) in identities:
            raise ValueError("package URL or identity differs from lock")
        identities.add((repo, filename))
        path = args.output_dir / args.architecture / repo / filename
        tasks.append((path, package["sha256"], package["size"], url))

    def fetch(task):
        path, sha, size, url = task
        return retry_fetch(lambda: publish(path, sha, size, lambda: opener.open(urllib.request.Request(url, headers={"User-Agent": "Flintyard-fixture-input/0.1"}), timeout=90)))

    with concurrent.futures.ThreadPoolExecutor(max_workers=6) as pool:
        results = list(pool.map(fetch, tasks))
    print(json.dumps({"architecture": args.architecture, "packages": len(results),
                      "downloaded": results.count("downloaded"), "reused": results.count("reused")}))


if __name__ == "__main__":
    main()
