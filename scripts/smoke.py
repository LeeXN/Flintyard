#!/usr/bin/env python3
"""Run local Go, packaging and offline CLI checks; no root or KVM required."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parents[1]
STAGES = ("format", "contracts", "vet", "test", "race", "build", "package", "cli")


def arguments(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("binary", nargs="?", type=Path,
                        help="existing CLI path (compatible with the previous script)")
    parser.add_argument("--go", default=os.environ.get("GO", "go"),
                        help="Go executable or absolute path (default: GO or go)")
    parser.add_argument("--timeout", type=int, default=600,
                        help="timeout in seconds per command (default: 600)")
    parser.add_argument("--cli-only", action="store_true",
                        help="run only the six offline checks using an existing binary")
    for stage in STAGES:
        flags = [f"--skip-{stage}"]
        if stage == "contracts":
            flags.append("--skip-design")
        parser.add_argument(*flags, action="store_true",
                            help=f"skip {stage}; report it as SKIP, never PASS")
    args = parser.parse_args(argv)
    if args.timeout <= 0:
        parser.error("--timeout must be positive")
    if args.cli_only:
        for stage in STAGES:
            if stage != "cli":
                setattr(args, f"skip_{stage}", True)
    if all(getattr(args, f"skip_{stage}") for stage in STAGES):
        parser.error("all checks are skipped; select at least one stage")
    return args


def command(argv, timeout, env=None, capture=False):
    print("RUN", " ".join(str(a) for a in argv), flush=True)
    return subprocess.run(argv, cwd=ROOT, env=env, check=True,
                          text=True, capture_output=capture, timeout=timeout)


def check_format(go, timeout, env):
    # Resolve the sibling gofmt when --go selects a toolchain by absolute path.
    executable = shutil.which(go)
    if executable is None:
        raise RuntimeError(f"Go executable not found: {go}")
    gofmt = Path(executable).resolve().with_name("gofmt")
    files = sorted(str(p.relative_to(ROOT)) for folder in ("cmd", "pkg", "internal")
                   for p in (ROOT / folder).rglob("*.go"))
    result = command([str(gofmt), "-l", *files], timeout, env, capture=True)
    if result.stdout.strip():
        raise RuntimeError("unformatted Go files (not modified):\n" + result.stdout)


def build(go, timeout, env):
    (ROOT / "bin").mkdir(exist_ok=True)
    for arch in ("amd64", "arm64"):
        build_env = dict(env, CGO_ENABLED="0", GOOS="linux", GOARCH=arch)
        # Include core packages not yet linked into the CLI: producing the
        # command alone does not prove that both architectures compile them.
        command([go, "build", "-trimpath", "./..."], timeout, build_env)
        command([go, "build", "-trimpath", "-o", f"bin/flintyard-linux-{arch}",
                 "./cmd/flintyard"], timeout, build_env)


def expected_version():
    source = (ROOT / "internal/version/version.go").read_text(encoding="utf-8")
    declaration = re.search(r'^const Version = "([0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.]+)?)"$', source, re.M)
    if declaration is None:
        raise RuntimeError("Flintyard version declaration not found")
    return declaration[1]


def package():
    # Check both inputs before creating outputs. No toolchain or make is required
    # when --skip-build uses existing binaries.
    binaries = [ROOT / "bin" / f"flintyard-linux-{a}" for a in ("amd64", "arm64")]
    for binary in binaries:
        if not binary.is_file():
            raise RuntimeError(f"missing {binary}; enable build or use --skip-package")
    dist = ROOT / "dist"
    dist.mkdir(exist_ok=True)
    sums = []
    version = expected_version()
    license_files = [ROOT / "LICENSE", ROOT / "THIRD-PARTY-NOTICES.md"]
    license_files.extend(sorted(path for path in (ROOT / "licenses").rglob("*") if path.is_file()))
    for arch, binary in zip(("amd64", "arm64"), binaries):
        name = f"flintyard-v{version}-linux-{arch}.tar.gz"
        output = dist / name
        files = {binary.name: binary}
        files.update((path.relative_to(ROOT).as_posix(), path) for path in license_files)
        with tarfile.open(output, "w:gz") as archive:
            for member, path in files.items():
                archive.add(path, arcname=member, recursive=False)
        expected = hashlib.sha256(output.read_bytes()).hexdigest()
        with tarfile.open(output, "r:gz") as archive:
            entries = archive.getmembers()
            if [entry.name for entry in entries] != list(files) or not all(entry.isfile() for entry in entries):
                raise RuntimeError(f"invalid package contents: {name}")
            for entry in entries:
                with archive.extractfile(entry) as payload, files[entry.name].open("rb") as source:
                    if hashlib.sha256(payload.read()).digest() != hashlib.sha256(source.read()).digest():
                        raise RuntimeError(f"packaged file mismatch: {name}: {entry.name}")
        sums.append(f"{expected}  {name}\n")
    (dist / "SHA256SUMS").write_text("".join(sums))
    for line in (dist / "SHA256SUMS").read_text().splitlines():
        expected, name = line.split("  ", 1)
        if hashlib.sha256((dist / name).read_bytes()).hexdigest() != expected:
            raise RuntimeError(f"package checksum mismatch: {name}")


def cli_smoke(exe, timeout):
    def run(args, expected=0):
        p = subprocess.run([str(exe), *args], capture_output=True, text=True, cwd=ROOT, timeout=timeout)
        if p.returncode != expected:
            raise RuntimeError(f"{args}: exit {p.returncode}, expected {expected}: {p.stdout} {p.stderr}")
        result = json.loads(p.stdout)
        if result["ok"] != (expected == 0):
            raise RuntimeError(f"inconsistent result: {result}")
        print("PASS", " ".join(args[:2]), f"(exit {expected})")
        return result


    version = run(["version", "-o", "json"])["data"]["version"]
    if version != expected_version():
        raise RuntimeError("CLI version differs from the packaged snapshot version")
    with tempfile.TemporaryDirectory(prefix="flintyard-smoke-") as tmp:
        d = Path(tmp)
        spec = json.loads((ROOT / "examples/import-spec.json").read_text())
        spec["kernel"].pop("config_sha256", None)
        spec["initialization"].pop("profile", None)
        spec["initialization"]["mode"] = "external"
        for entry in spec["inputs"]:
            data = ("NONBOOTABLE contract fixture " + entry["role"]).encode()
            (d / entry["role"]).write_bytes(data)
            entry["path"] = entry["role"]
            entry["sha256"] = hashlib.sha256(data).hexdigest()
        (d / "spec.json").write_text(json.dumps(spec))
        common = ["-o", "json", "--state-dir", str(d / "state")]
        package = str(d / "test.fyimg")
        result = run(["image", "import", *common, "--spec", str(d / "spec.json"), "--output", package])
        data = result["data"]
        if data["validation"] != "not_run":
            raise RuntimeError("synthetic payload must not be marked boot validated")
        if data["manifest"]["provenance"]["builder"] != "flintyard/v" + version:
            raise RuntimeError("image packer and CLI report different versions")
        run(["image", "inspect", "-o", "json", package])
        run(["image", "inspect", *common, data["image_id"]])
        again = run(["image", "import", *common, "--sha256", data["package_sha256"], package])
        if again["data"]["image_id"] != data["image_id"]:
            raise RuntimeError("image identity changed during repeat import")
        bad = run(["image", "import", *common, "--sha256", "1" * 64, package], 4)
        if bad["error"]["code"] != "E_DIGEST_MISMATCH":
            raise RuntimeError("wrong failure code")
    print("6 offline CLI checks passed. Synthetic payloads only; no VM was booted.", flush=True)


def main(argv=None):
    args = arguments(argv)
    if platform.system() != "Linux":
        print("FAIL: Linux is required", file=sys.stderr)
        return 1
    arch = {"x86_64": "amd64", "aarch64": "arm64"}.get(platform.machine())
    if arch is None:
        print("FAIL: Linux amd64 or arm64 is required", file=sys.stderr)
        return 1
    exe = args.binary.resolve() if args.binary else ROOT / "bin" / f"flintyard-linux-{arch}"
    env = dict(os.environ, GOTOOLCHAIN="local")
    actions = {
        "format": lambda: check_format(args.go, args.timeout, env),
        "contracts": lambda: command([sys.executable, "tools/check_contracts.py"], args.timeout),
        "vet": lambda: command([args.go, "vet", "./..."], args.timeout, env),
        "test": lambda: command([args.go, "test", "-count=1", "./..."], args.timeout, env),
        "race": lambda: command([args.go, "test", "-race", "-count=1", "./..."], args.timeout, env),
        "build": lambda: build(args.go, args.timeout, env),
        "package": package,
        "cli": lambda: cli_smoke(exe, args.timeout),
    }
    results = dict.fromkeys(STAGES, "NOT_RUN")
    for stage in STAGES:
        if getattr(args, f"skip_{stage}"):
            results[stage] = "SKIP"
    failed = False
    for stage in STAGES:
        if results[stage] == "SKIP":
            print("SKIP", stage, flush=True)
            continue
        print("START", stage, flush=True)
        try:
            actions[stage]()
        except (OSError, ValueError, RuntimeError, subprocess.SubprocessError,
                tarfile.TarError) as error:
            results[stage] = "FAIL"
            print(f"FAIL {stage}: {error}", file=sys.stderr, flush=True)
            failed = True
            break
        results[stage] = "PASS"
        print("PASS", stage, flush=True)
    print("\nSummary:", flush=True)
    for stage, status in results.items():
        print(f"  {stage}: {status}", flush=True)
    print("Native ARM execution and KVM/VM acceptance were not run.", flush=True)
    print("Privileged network checks are separate: bash tests/native-network.sh (SKIP is not PASS).", flush=True)
    return 1 if failed else 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        print("Interrupted; remaining checks were not completed.", file=sys.stderr)
        sys.exit(130)
