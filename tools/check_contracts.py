#!/usr/bin/env python3
"""Validate the repository's JSON examples and documentation links.

The checker implements the JSON Schema vocabulary used by the distributed schemas.
"""

from pathlib import Path
import copy
import datetime
import ipaddress
import json
import os
import re
import sys

ROOT = Path(__file__).resolve().parents[1]


def markdown_files(root):
    # Build workspaces can contain whole, untrusted guest filesystems under
    # .local; generated outputs and git internals are not project documents.
    excluded = {".git", ".local", "bin", "dist"}
    for current, dirs, files in os.walk(root, topdown=True, followlinks=False):
        if Path(current) == root:
            dirs[:] = [name for name in dirs if name not in excluded]
        dirs.sort()
        for name in sorted(files):
            if name.endswith(".md"):
                yield Path(current) / name


def load(path):
    def pairs(items):
        result = {}
        for key, value in items:
            if key in result:
                raise ValueError(f"duplicate JSON key: {key}")
            result[key] = value
        return result

    return json.loads(path.read_text(encoding="utf-8"), object_pairs_hook=pairs)


def validate(value, schema, where="$"):
    """Check the JSON Schema keywords used in this repository."""
    if "const" in schema and (type(value) is not type(schema["const"])
                              or value != schema["const"]):
        raise ValueError(f"{where}: const")
    if "enum" in schema and value not in schema["enum"]:
        raise ValueError(f"{where}: enum")
    if "not" in schema:
        try:
            validate(value, schema["not"], where)
        except ValueError:
            pass
        else:
            raise ValueError(f"{where}: forbidden shape")
    if "oneOf" in schema:
        matches = 0
        for alternative in schema["oneOf"]:
            try:
                validate(value, alternative, where)
            except ValueError:
                continue
            matches += 1
        if matches != 1:
            raise ValueError(f"{where}: oneOf matched {matches}")
    if "type" in schema:
        choices = schema["type"]
        if isinstance(choices, str):
            choices = [choices]
        valid = {
            "object": type(value) is dict,
            "array": type(value) is list,
            "string": type(value) is str,
            "integer": type(value) is int,
            "boolean": type(value) is bool,
            "null": value is None,
            "number": type(value) in (int, float),
        }
        if not any(valid[item] for item in choices):
            raise ValueError(f"{where}: type")
    if type(value) is dict:
        for key in schema.get("required", []):
            if key not in value:
                raise ValueError(f"{where}: missing {key}")
        properties = schema.get("properties", {})
        extra = schema.get("additionalProperties", True)
        for key, item in value.items():
            if key in properties:
                validate(item, properties[key], f"{where}.{key}")
            elif extra is False:
                raise ValueError(f"{where}: unknown {key}")
            elif type(extra) is dict:
                validate(item, extra, f"{where}.{key}")
    if type(value) is list:
        if len(value) < schema.get("minItems", 0):
            raise ValueError(f"{where}: minItems")
        if len(value) > schema.get("maxItems", len(value)):
            raise ValueError(f"{where}: maxItems")
        for i, item in enumerate(value):
            validate(item, schema.get("items", {}), f"{where}[{i}]")
    if type(value) is str:
        if len(value) < schema.get("minLength", 0):
            raise ValueError(f"{where}: minLength")
        if len(value) > schema.get("maxLength", len(value)):
            raise ValueError(f"{where}: maxLength")
        if "pattern" in schema and not re.search(schema["pattern"], value):
            raise ValueError(f"{where}: pattern")
        if schema.get("format") == "ipv4":
            try:
                ipaddress.IPv4Address(value)
            except ipaddress.AddressValueError as error:
                raise ValueError(f"{where}: ipv4") from error
        if schema.get("format") == "date-time":
            parsed = datetime.datetime.fromisoformat(value.replace("Z", "+00:00"))
            if parsed.tzinfo is None:
                raise ValueError(f"{where}: timezone")
    if type(value) in (int, float):
        if value < schema.get("minimum", value):
            raise ValueError(f"{where}: minimum")
        if value > schema.get("maximum", value):
            raise ValueError(f"{where}: maximum")


def main():
    pairs = {
        "image-manifest": "image",
        "import-spec": "import",
        "build-prebuilt": "recipe",
        "build-kernel-source": "recipe",
        "environment": "environment",
        "environment-aarch64": "environment",
        "environment-nat": "environment",
        "result-up": "result",
        "result-failed": "result",
        "event": "event",
    }
    schemas = {
        name: load(ROOT / "schemas" / f"{name}-v1.schema.json")
        for name in set(pairs.values())
    }
    examples = {}
    for name, schema in pairs.items():
        examples[name] = load(ROOT / "examples" / f"{name}.json")
        validate(examples[name], schemas[schema])

    negatives = []
    for key, bad in [("architecture", "riscv64"), ("unknown", True),
                     ("name", "../escape")]:
        changed = copy.deepcopy(examples["environment"])
        changed[key] = bad
        negatives.append((changed, "environment"))
    changed = copy.deepcopy(examples["environment"])
    changed["nodes"][0]["memory_mib"] = True
    negatives.append((changed, "environment"))
    changed = copy.deepcopy(examples["environment-nat"])
    changed["port_forwards"][0]["host_port"] = 65536
    negatives.append((changed, "environment"))
    changed = copy.deepcopy(examples["build-prebuilt"])
    changed["rootfs"]["source"]["url"] = "https://example.invalid/source"
    negatives.append((changed, "recipe"))
    changed = copy.deepcopy(examples["build-kernel-source"])
    changed["initrd"] = {"path": "extra", "sha256": "0" * 64}
    negatives.append((changed, "recipe"))
    changed = copy.deepcopy(examples["result-up"])
    changed["ok"] = False
    negatives.append((changed, "result"))
    changed = copy.deepcopy(examples["image-manifest"])
    changed["initialization"]["profile"] = "guess-from-os"
    negatives.append((changed, "image"))
    changed = copy.deepcopy(examples["import-spec"])
    changed["inputs"][0]["sha256"] = "wrong"
    negatives.append((changed, "import"))
    for value, schema in negatives:
        try:
            validate(value, schemas[schema])
        except ValueError:
            continue
        raise ValueError(f"negative unexpectedly passed: {schema}")

    link_count = 0
    fenced_json = 0
    fence_pattern = re.compile(r"^(" + chr(96) * 3 + r"|~~~)([a-z]*)\s*$")
    for path in markdown_files(ROOT):
        content = path.read_text(encoding="utf-8")
        active = None
        code = []
        for line in content.splitlines():
            match = fence_pattern.match(line)
            if match:
                if active is None:
                    active = match.group(2)
                    code = []
                else:
                    if active == "json":
                        json.loads("\n".join(code))
                        fenced_json += 1
                    active = None
                continue
            if active is not None:
                code.append(line)
        if active is not None:
            raise ValueError(f"unclosed fence: {path}")
        for target in re.findall(r"\[[^\]]+\]\(([^)]+)\)", content):
            if "://" in target or target.startswith("#"):
                continue
            target = target.split("#", 1)[0]
            resolved = (path.parent / target).resolve()
            if not resolved.is_relative_to(ROOT):
                raise ValueError(f"link escapes package: {path}: {target}")
            if not resolved.exists():
                raise ValueError(f"missing link: {path}: {target}")
            link_count += 1

    print(json.dumps({
        "schema_files": len(schemas),
        "examples_validated": len(pairs),
        "negative_cases_rejected": len(negatives),
        "local_links_checked": link_count,
        "fenced_json_checked": fenced_json,
        "status": "passed",
        "scope": "JSON examples and documentation links",
    }, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, json.JSONDecodeError) as error:
        print(f"FAILED: {error}", file=sys.stderr)
        sys.exit(1)
