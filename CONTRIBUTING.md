# Contributing to Flintyard

[简体中文](CONTRIBUTING.zh-CN.md)

For questions or bugs, open a GitHub issue with the CLI version, host architecture and kernel, Firecracker and jailer versions, and the exact command you ran. Include a minimal configuration, the relevant JSON error, or a short log excerpt. Remove credentials and private infrastructure details from reports.

## Local checks

Use Go 1.27.3, Python 3, and a C toolchain for race tests. Run the following commands in the repository root.

```sh
python3 -m unittest discover -s scripts -p 'test_*.py'
python3 scripts/smoke.py
```

The smoke command checks formatting, schemas, documentation links, Go tests, and builds for both architectures. For changes to VM provisioning or cleanup, use the container example on a KVM host. [Host setup](docs/install.md) · [Container example](docs/container-workload.md)

## Repository layout

- `cmd/flintyard/` contains the CLI and presentation logic.
- `pkg/flintyard/` exposes the local operations used by the CLI.
- `internal/` contains the Firecracker API client, image and build tooling, Linux networking, allocation, and lifecycle state.
- `schemas/`, `examples/`, and `fixtures/` describe the wire formats and reproducible input preparation.
- `tests/` contains external checks, including guest SSH checks.

Keep the CLI and core contracts in sync with the schemas and their examples. For a runtime change, describe the real host architecture, runtime versions, commands, and observed behavior. Record emulation and native hardware results separately.

Submit focused pull requests with a short description and verification results. Add regression coverage for failures that existing tests do not catch, especially when changing lifecycle, image integrity, or cleanup.
