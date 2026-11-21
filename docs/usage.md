# Start and manage an environment

[简体中文](usage.zh-CN.md)

An environment is a named group of Firecracker microVMs on one host. Names and imported image IDs are scoped to a state root. Keep using the same `--state-dir` for all commands that address an environment.

## Single-node quickstart

Complete the [installation](install.md). This example uses the absolute `FY`, `FIRECRACKER`, and `JAILER` paths from that guide, and an image prepared for `seed-v1`. Set `IMAGE` to the package path and `EXPECTED_SHA256` to the digest supplied through a trusted channel. To generate a sample package, follow the [pinned Debian build](../fixtures/README.en.md#debian-stock-kernel-offline-image).

If `/var/lib` has limited space, set `STATE_PARENT` to an existing root-managed directory on a filesystem with room for the image cache and the guest disk.

```sh
: "${FY:?set FY}" "${FIRECRACKER:?set FIRECRACKER}" "${JAILER:?set JAILER}"
: "${IMAGE:?set IMAGE}" "${EXPECTED_SHA256:?set EXPECTED_SHA256}"
ARCH="$(uname -m)"
WORK="$(mktemp -d)"
STATE_PARENT="${STATE_PARENT:-/var/lib}"
STATE="$(sudo mktemp -d "$STATE_PARENT/flintyard-demo.XXXXXXXX")"
ssh-keygen -q -t ed25519 -N '' -f "$WORK/id_ed25519"

sudo "$FY" image import --state-dir "$STATE" --sha256 "$EXPECTED_SHA256" \
  -o json "$IMAGE" > "$WORK/image.json"

jq -n --arg arch "$ARCH" --arg image "$(jq -er '.data.image_id' "$WORK/image.json")" \
  --arg key "$WORK/id_ed25519.pub" '{
    api_version: "flintyard/v1alpha1", kind: "Environment", name: "demo",
    architecture: $arch, network: {cidr: "auto", nat: false, dns: []},
    nodes: [{name: "node-1", image: $image, vcpu_count: 2, memory_mib: 1024}],
    access: {public_key: $key, probe: {type: "ssh_banner", port: 22}},
    port_forwards: []
  }' > "$WORK/environment.json"

sudo "$FY" up --state-dir "$STATE" --timeout 5m --file "$WORK/environment.json" \
  --firecracker "$FIRECRACKER" --jailer "$JAILER"
sudo "$FY" status --state-dir "$STATE" --probe -o json demo > "$WORK/status.json"
ACCESS=$(jq -ce 'select(.ok) | .data.nodes[0].access | select(.probe_passed)' "$WORK/status.json")
SSH_TARGET=$(printf '%s\n' "$ACCESS" | jq -er '.username + "@" + .host')
SSH_PORT=$(printf '%s\n' "$ACCESS" | jq -er .port)
ssh_demo() {
  ssh -n -i "$WORK/id_ed25519" -o IdentitiesOnly=yes \
    -o UserKnownHostsFile="$WORK/known_hosts" -o StrictHostKeyChecking=accept-new \
    -p "$SSH_PORT" "$SSH_TARGET" "$@"
}
ssh_demo uname -m
```

The SSH banner probe checks connectivity to the SSH service. The SSH command authenticates with the private key held by the caller. Supply only the public key to Flintyard. Plain `status` reports process and API observations. Add `--probe` for a fresh access check.

Use the following commands to observe the environment.

```sh
sudo "$FY" list --state-dir "$STATE"
sudo "$FY" logs --state-dir "$STATE" --node node-1 --source serial --tail 100 demo
```

## Stop, resume, and destroy

`stop` terminates the VMM and retains each node's writable disk, seed, and identity. Shut down the application and flush guest writes before stopping. Run `up` with the same resolved configuration to resume.

```sh
ssh_demo sync
sudo "$FY" stop --state-dir "$STATE" demo
sudo "$FY" up --state-dir "$STATE" --timeout 5m --file "$WORK/environment.json" \
  --firecracker "$FIRECRACKER" --jailer "$JAILER"
```

Use `down` to discard the environment's writable disks, seed disks, and owned runtime resources. Image files, cache, and startup logs remain available by default. Use `down --purge` to discard diagnostics as well.

```sh
sudo "$FY" down --state-dir "$STATE" demo
```

A repeated `up` with unchanged configuration observes the running environment. Change the configuration after explicitly removing the old environment with `down`. Back up any application data before that step.

## Configure multiple nodes

Add entries to the environment's `nodes` array. Each entry specifies a unique name, image ID or local package, vCPU count, and memory in MiB. The IP can be assigned by Flintyard or specified in the environment's IPv4 subnet. Images and public key paths are resolved relative to the environment file. All nodes use the same architecture.

[Environment schema](../schemas/environment-v1.schema.json) · [Multi-node example](../examples/environment.json) · [Network guide](networking.md)

## Recover stale resources

If a process or host stops unexpectedly, first check `status` and try `down` for the affected environment. To recover confirmed leftovers, save a dry-run plan outside the target environment, inspect it, and apply the still-valid plan with the same state root.

```sh
PLAN="$(mktemp -d "$WORK/gc.XXXXXXXX")/plan.json"
sudo "$FY" gc --state-dir "$STATE" --dry-run --plan "$PLAN"
sudo cat "$PLAN"
```

Apply the plan after reviewing its targets and resources.

```sh
sudo "$FY" gc --state-dir "$STATE" --apply --plan "$PLAN"
```

Generate a new plan if the plan expires after 10 minutes or the host, target state, or resource identities change. See the [CLI reference](cli.md) for diagnostics and exit codes.
