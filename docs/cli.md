# CLI reference

[简体中文](cli.zh-CN.md)

Flags go before positional arguments. Run `flintyard COMMAND --help` for the complete list of flags. Operations take `--state-dir DIR` (default `/var/lib/flintyard`), `--timeout DURATION`, and `-o text|json`. Durations use Go notation such as `180s` or `5m`.

| Command | Purpose |
| --- | --- |
| `version` | Print the Flintyard version, protocol, and build information. |
| `doctor --mode run\|build\|validate` | Check the selected host and binary paths. |
| `image build --recipe FILE [--offline]` | Execute a build recipe, export a package, and populate the cache. |
| `image import --sha256 DIGEST PACKAGE` | Check and import a local package. |
| `image import --spec FILE --output PACKAGE` | Assemble and import a new package from local files. |
| `image inspect IMAGE` | Inspect a package path or an imported image ID. |
| `image validate --public-key FILE IMAGE` | Boot a temporary guest and run the configured access probe. |
| `up --file FILE` | Create or resume an environment. |
| `status [--probe] ENV` | Inspect an environment and optionally repeat its access checks. |
| `list [--probe]` | List environments in the selected state root. |
| `logs --node NAME [--source serial\|firecracker] ENV` | Read a node's startup log. |
| `stop ENV` | Stop the VMMs and retain writable disks and node identities. |
| `down [--purge] ENV` | Delete an environment and its writable disks. Use `--purge` to remove diagnostics. |
| `gc --dry-run --plan FILE` | Write a plan for confirmed leftover resources. |
| `gc --apply --plan FILE` | Recheck and apply a still-valid plan. |

## Common runtime flags

- `--firecracker PATH`, `--jailer PATH`, and `--uid-range START:END` select binaries and the jailed user range. Use these flags on `doctor`, `up`, and `image validate`.
- `up --wait configured` uses the environment's configured probe. `--wait tcp` and `--wait ssh_banner` select a probe explicitly. `--wait none` returns after launch without waiting for application access.
- `--keep-failed` on `up` or `image validate` retains confirmed resources for debugging. Inspect the environment and run `down` when finished.
- Use `logs --tail N` for a bounded snapshot (default 200 lines) or `logs --follow` for a continuous byte stream. Set an explicit timeout to bound follow mode. Follow mode streams to stdout in text mode.

## Output and events

Flintyard displays readable text by default. `list` shows an environment table and `status` shows the environment, its nodes, and exposed ports. Logs are written verbatim in text mode. Text-mode errors go to stderr.

Reuse `FY`, `STATE`, and the `demo` environment from the quickstart. [Single-node quickstart](usage.md#single-node-quickstart)

```sh
sudo "$FY" list --state-dir "$STATE"
sudo "$FY" status --state-dir "$STATE" --probe demo
sudo "$FY" status --state-dir "$STATE" --probe -o json demo | jq -er '.data.nodes[] | select(.access.probe_passed) | .access.host'
```

With `-o json`, stdout carries the result envelope. It contains `api_version`, `operation_id`, `ok`, `data`, and `error`. Capture stdout and stderr separately. The older `--json` flag remains an alias. On error, inspect `error.code` for automation and `error.message` for diagnostics. A failed operation can still return resource and cleanup details in `data`.

Supply `--events FILE` for JSON Lines with operation ID, sequence, stage, and node or resource identity where applicable. Use the [result schema](../schemas/result-v1.schema.json) and [event schema](../schemas/event-v1.schema.json) when consuming records. [Example results](../examples/README.en.md)

| Exit status | Meaning |
| --- | --- |
| 0 | Success |
| 2 | Usage or configuration error |
| 3 | Unavailable capability, permissions, unsupported version, or architecture mismatch |
| 4 | Image, build, integrity, storage, or local I/O failure |
| 5 | Launch, API readiness, or access probe failure |
| 6 | State, lock, port, firewall, or resource identity conflict |
| 7 | Incomplete cleanup |
| 130 | Interrupted operation |

A cleanup error can take precedence over interruption. Inspect the reported state and diagnostics, then follow [recovery](usage.md#recover-stale-resources).

## Access status

A configured probe can be TCP, SSH banner, or none. A plain `status` can show the last recorded probe result, labeled `last passed` or `last failed`. Use `status --probe` for a fresh check. An SSH banner reports protocol reachability. Authenticate with your own SSH client and private key. In JSON, read `observed_at`, `last_probe_at`, and `stale` to gauge how recent the observations are.
