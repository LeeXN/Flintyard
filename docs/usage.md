# From source to your first microVM

[简体中文](usage.zh-CN.md) · [CLI reference](cli.md) · [Image guide](images.md) · [Networking](networking.md)

Build Flintyard, create or import an image, then configure, start, access, and remove an environment. Run the steps from the repository root. You can open a new shell between steps if you return to the same checkout.

[Build and install](#1-prepare-the-host-and-build-flintyard) · [Build an image](#3-build-the-debian-image-or-prepare-an-existing-image) · [Import](#4-inspect-import-and-read-the-image-id) · [Configure and boot](#5-configure-and-boot-the-microvm) · [Log in](#6-check-status-log-in-and-inspect-the-guest) · [Stop and destroy](#stop-resume-and-destroy)

| Path | Purpose |
| --- | --- |
| `/opt/flintyard/bin/` | Root-managed copies of Flintyard, Firecracker, and jailer. |
| `.local/quickstart/` | Tutorial image, checksum, environment configuration, SSH keys, and command results. |
| `/var/lib/flintyard-tutorial` | Dedicated state directory for the image cache, writable disks, and diagnostics. |

For other deployments, use the default `/var/lib/flintyard` or another root-managed path with enough space. Use the same state directory for every operation on a given environment.

## Single-node quickstart

### 1. Prepare the host and build Flintyard

Use a Linux `x86_64` or `aarch64` host with working `/dev/kvm` and `/dev/net/tun`, 4 KiB host pages, and a memory cgroup controller. A virtualized host needs nested virtualization enabled by its hypervisor. Allow room for build scratch space, the expanded image cache, and a writable disk per node. [Host requirements](install.md)

Install Go 1.27.3 or a compatible newer toolchain. On a Debian or Ubuntu host, install the system tools for the walkthrough as follows. [Go downloads](https://go.dev/dl/)

```sh
sudo apt-get update
sudo apt-get install build-essential make git ca-certificates curl jq python3 \
  iproute2 nftables e2fsprogs fdisk mount util-linux xz-utils openssh-client
```

Build for the current host from the checked-out repository root. `make build` produces both Linux amd64 and arm64 distribution binaries. The tutorial needs only the host build.

```sh
(
  set -eu
  go version
  mkdir -p bin
  go build -trimpath -o bin/flintyard ./cmd/flintyard
  sudo install -d -o root -g root -m 0755 /opt/flintyard/bin
  sudo install -o root -g root -m 0755 bin/flintyard /opt/flintyard/bin/flintyard
  /opt/flintyard/bin/flintyard version
)
```

### 2. Install matching Firecracker and jailer binaries

Use the same official archive for both executables and verify its SHA-256 before installation. When operating as root, Flintyard checks ownership and write permissions on these binaries and their ancestors. The shell variables below are scoped to this block. [Compatibility lock](../compatibility.lock.json)

```sh
(
  set -eu
  arch=$(uname -m)
  version=$(jq -er '.firecracker.version' compatibility.lock.json)
  url=$(jq -er --arg arch "$arch" '.firecracker.assets[$arch].url' compatibility.lock.json)
  digest=$(jq -er --arg arch "$arch" '.firecracker.assets[$arch].sha256' compatibility.lock.json)
  mkdir -p .local
  downloads=$(mktemp -d .local/firecracker.XXXXXXXX)
  curl --fail --location --output "$downloads/firecracker.tgz" "$url"
  printf '%s  %s\n' "$digest" "$downloads/firecracker.tgz" | sha256sum -c
  tar -xzf "$downloads/firecracker.tgz" --no-same-owner -C "$downloads"
  sudo install -o root -g root -m 0755 \
    "$downloads/release-v$version-$arch/firecracker-v$version-$arch" /opt/flintyard/bin/firecracker
  sudo install -o root -g root -m 0755 \
    "$downloads/release-v$version-$arch/jailer-v$version-$arch" /opt/flintyard/bin/jailer
)
```

```sh
sudo /opt/flintyard/bin/flintyard doctor --mode run \
  --firecracker /opt/flintyard/bin/firecracker --jailer /opt/flintyard/bin/jailer
```

Resolve any failed `doctor` checks before proceeding. Use `doctor --mode build` for image-building prerequisites. If you change the jailed UID/GID range, pass the same `--uid-range` to `doctor`, `up`, and `image validate`.

### 3. Build the Debian image or prepare an existing image

Choose one of the two routes. Both produce `.local/quickstart/image.fyimg` and `.local/quickstart/image.sha256`. The checksum file contains only the 64-digit hexadecimal SHA-256.

#### Option A: Build from the locked Debian cloud image

Download the pinned Debian cloud archive. The preparation script verifies it and writes the raw root disk, stock kernel, and initrd into `.local/quickstart/debian-inputs/`. The destination must be new. If you already have inputs prepared using the same architecture and lock file, start with the next block. [Debian input lock](../fixtures/debian-cloud.lock.json)

```sh
(
  set -eu
  umask 077
  mkdir -p .local/quickstart
  url=$(jq -er --arg arch "$(uname -m)" '.architectures[$arch].url' fixtures/debian-cloud.lock.json)
  curl --fail --location --output .local/quickstart/debian-cloud.tar.xz "$url"
  python3 fixtures/debian-cloud.py "$(uname -m)" \
    --archive .local/quickstart/debian-cloud.tar.xz \
    --output-dir .local/quickstart/debian-inputs
)
```

Render `debian.recipe.json` and run the offline build. A successful build writes `image.fyimg`, populates the state directory's image cache, and reports the image ID, package checksum, and diagnostics in `build.json`. Use new recipe and output file names if you need to rebuild.

```sh
(
  set -eu
  umask 077
  python3 fixtures/render-debian-cloud.py "$(uname -m)" \
    --input-dir .local/quickstart/debian-inputs \
    --output .local/quickstart/image.fyimg --recipe .local/quickstart/debian.recipe.json
  sudo /opt/flintyard/bin/flintyard doctor --mode build
  sudo /opt/flintyard/bin/flintyard image build --state-dir /var/lib/flintyard-tutorial \
    --offline --timeout 20m --recipe .local/quickstart/debian.recipe.json -o json \
    > .local/quickstart/build.json
  jq -er 'select(.ok) | .data.package_sha256' .local/quickstart/build.json \
    > .local/quickstart/image.sha256
)
```

Downloading the inputs requires network access. `--offline` applies to the Flintyard build, which uses prepared inputs or a complete cache. [Other distributions and source kernels](../fixtures/README.en.md)

#### Option B: Use a published image package

Obtain a matching-architecture `.fyimg` prepared for `seed-v1` with SSH on port 22, together with the package checksum from its trusted publisher. For a fresh walkthrough, copy the package to `.local/quickstart/image.fyimg` and save its checksum as `.local/quickstart/image.sha256`. See the [image guide](images.md) for other initialization modes.

```sh
mkdir -p .local/quickstart
# Replace the two example paths with the files from your publisher.
cp -- /path/to/published.fyimg .local/quickstart/image.fyimg
awk 'NR == 1 {print $1}' /path/to/published.fyimg.sha256 > .local/quickstart/image.sha256
```

### 4. Inspect, import, and read the image ID

`image inspect` checks the package statically. `image import --sha256` verifies the whole package checksum and registers a read-only cache entry. Option A has already populated the same cache, so this import verifies the package and supplies the same output in either path. The `sha256:…` value in `data.image_id` identifies the manifest. Use it in the environment configuration. `package_sha256` identifies the compressed package.

```sh
(
  set -eu
  sudo /opt/flintyard/bin/flintyard image inspect .local/quickstart/image.fyimg
  sudo /opt/flintyard/bin/flintyard image import --state-dir /var/lib/flintyard-tutorial \
    --sha256 "$(cat .local/quickstart/image.sha256)" -o json .local/quickstart/image.fyimg \
    > .local/quickstart/import.json
  jq -er 'select(.ok) | .data.image_id' .local/quickstart/import.json
)
```

Before distributing an image, you can run `image validate` on the target host to boot a temporary guest and run its access probe. [Boot validation](images.md#test-boot-and-access)

### 5. Configure and boot the microVM

Generate a dedicated SSH key pair and `.local/quickstart/environment.json`. The configuration creates environment `debian-demo` with one node named `node-1`, two vCPUs, and 1,024 MiB of memory. `id_ed25519.pub` is relative to the JSON file. The command reuses the key pair if it already exists. Only the public key path enters the configuration.

```sh
(
  set -eu
  umask 077
  image_id=$(jq -er 'select(.ok) | .data.image_id' .local/quickstart/import.json)
  if [ ! -f .local/quickstart/id_ed25519 ]; then
    ssh-keygen -q -t ed25519 -N '' -f .local/quickstart/id_ed25519
  fi
  test -f .local/quickstart/id_ed25519.pub
  jq -n --arg arch "$(uname -m)" --arg image "$image_id" '{
    api_version: "flintyard/v1alpha1",
    kind: "Environment",
    name: "debian-demo",
    architecture: $arch,
    network: {cidr: "auto", nat: false, dns: []},
    nodes: [{name: "node-1", image: $image, vcpu_count: 2, memory_mib: 1024}],
    access: {public_key: "id_ed25519.pub", probe: {type: "ssh_banner", port: 22}},
    port_forwards: []
  }' > .local/quickstart/environment.json
  cat .local/quickstart/environment.json
)
```

You can [adjust CPU, memory, and networking](#configure-node-resources-and-networking) before the first boot. The default private network lets the host reach the guest over SSH.

```sh
sudo /opt/flintyard/bin/flintyard up --state-dir /var/lib/flintyard-tutorial \
  --file .local/quickstart/environment.json --timeout 5m --wait configured \
  --firecracker /opt/flintyard/bin/firecracker --jailer /opt/flintyard/bin/jailer
```

`--wait configured` waits for the SSH banner probe declared in the configuration. `ready` means the probe passed. `--wait none` skips this wait. Run `status --probe` when you want to check access.

### 6. Check status, log in, and inspect the guest

Use `status --probe` to refresh the endpoint, then authenticate with the private key in your SSH client. This command reads the observed host, username, and port from JSON and uses a separate known-hosts file for the guest. The banner probe only checks reachability of the SSH service.

```sh
(
  set -eu
  sudo /opt/flintyard/bin/flintyard status --state-dir /var/lib/flintyard-tutorial \
    --probe -o json debian-demo > .local/quickstart/status.json
  access=$(jq -ce 'select(.ok) | .data.nodes[] | select(.name == "node-1") |
    .access | select(.probe_passed)' .local/quickstart/status.json)
  target=$(printf '%s\n' "$access" | jq -er '.username + "@" + .host')
  port=$(printf '%s\n' "$access" | jq -er '.port')
  ssh -i .local/quickstart/id_ed25519 -o IdentitiesOnly=yes \
    -o UserKnownHostsFile=.local/quickstart/known_hosts -o StrictHostKeyChecking=accept-new \
    -p "$port" "$target"
)
```

Run the following **inside the guest after logging in over SSH**. Write a marker before returning to the host so you can check whether the disk was retained.

```sh
uname -m
ip -4 addr
df -h /
printf '%s\n' 'Flintyard disk persists across stop and resume' > ~/flintyard-demo.txt
sync
exit
```

Back **on the host**, use `list` to view environments. Use `logs --source serial` for the guest console or `--source firecracker` for startup logs. Add `--follow` before the environment name to stream logs until Ctrl+C.

```sh
sudo /opt/flintyard/bin/flintyard list --state-dir /var/lib/flintyard-tutorial
sudo /opt/flintyard/bin/flintyard logs --state-dir /var/lib/flintyard-tutorial \
  --node node-1 --source serial --tail 100 debian-demo
```

## Stop, resume, and destroy

**Use `stop` to retain writable disks.** Shut down applications and flush writes inside the guest first. `stop` terminates the VMM and retains its writable disk, seed, and node identity. Rerun `up` with the same configuration. Repeat the SSH step and run `cat ~/flintyard-demo.txt` to confirm the guest write survived.

```sh
sudo /opt/flintyard/bin/flintyard stop --state-dir /var/lib/flintyard-tutorial debian-demo
sudo /opt/flintyard/bin/flintyard up --state-dir /var/lib/flintyard-tutorial \
  --file .local/quickstart/environment.json --timeout 5m \
  --firecracker /opt/flintyard/bin/firecracker --jailer /opt/flintyard/bin/jailer
```

**Use `down` to remove the environment.** Export guest data you want to keep first. `down` removes writable disks, seed disks, and owned runtime resources. It retains image packages, the cache, and diagnostics. Add `--purge` before the environment name to remove diagnostics as well.

```sh
sudo /opt/flintyard/bin/flintyard down --state-dir /var/lib/flintyard-tutorial debian-demo
```

## Configure node resources and networking

Edit `environment.json` before first launch. An existing running or stopped environment keeps its original configuration identity. To change an image, CPU, memory, nodes, networking, or the injected key, test the new specification under a different name. To reuse the name, back up guest data, bring the existing environment down, and create it again. Install applications and edit operating system configuration inside the guest over SSH.

| Field | How to use it |
| --- | --- |
| `name`, `architecture` | The environment name is unique within a state directory. The host and all images must use the same architecture, `x86_64` or `aarch64`. |
| `nodes[]` | Give each node a unique `name`, `image`, `vcpu_count`, and `memory_mib`. Memory is in MiB and starts at 128. The current Firecracker version supports 1–32 vCPUs. Use a cached image ID or a local `.fyimg` path relative to the configuration file. |
| `network.cidr`, `nodes[].ipv4` | `"auto"` allocates a free subnet. For fixed node addresses, choose an explicit RFC 1918 subnet (`/8` through `/29`) and leave the gateway, network, and broadcast addresses free. |
| `network.nat`, `dns`, `egress_interface` | Use `nat: false` and `dns: []` for the private example. For outbound access, enable NAT, configure a guest-reachable IPv4 resolver, and enable host IPv4 forwarding. Select a real host egress interface when it cannot be inferred unambiguously. |
| `access.public_key`, `access.probe` | `seed-v1` can inject a public key. With `external` initialization, omit the key and configure access in the image. Select `ssh_banner`, `tcp`, or `none`. `port` is required. The SSH banner port for a `seed-v1` image must match the image. |
| `port_forwards[]` | For each mapping, set `protocol` (`tcp`/`udp`), `bind_address`, `host_port`, target `node`, and `guest_port`. Use `127.0.0.1` for host-only access. For external access, bind a specific non-loopback IPv4 address on the host and check forwarding and firewall policy. |

Derive a two-node environment from the single-node configuration. This gives it a separate automatically allocated network and clears fixed addresses and port mappings to avoid conflicts with the original environment. Manage `debian-pair` by name with `status` and `down`.

```sh
jq '.name = "debian-pair" |
  .network = {cidr: "auto", nat: false, dns: []} | .port_forwards = [] |
  .nodes |= map(del(.ipv4)) |
  .nodes += [(.nodes[0] | .name = "node-2" | .memory_mib = 512)]' \
  .local/quickstart/environment.json > .local/quickstart/pair.json
sudo /opt/flintyard/bin/flintyard up --state-dir /var/lib/flintyard-tutorial \
  --file .local/quickstart/pair.json --timeout 5m \
  --firecracker /opt/flintyard/bin/firecracker --jailer /opt/flintyard/bin/jailer
```

[Environment schema](../schemas/environment-v1.schema.json) · [Multi-node example](../examples/environment.json) · [NAT, port forwards, and troubleshooting](networking.md)

## Recover stale resources

After a host or process crash, inspect `status` and the logs, then run `down` for the affected environment. To recover leftovers, generate a `gc` plan and review the owners, paths, and exclusions. Keep the plan outside the affected environment directories. The path in this example must be new.

```sh
sudo /opt/flintyard/bin/flintyard gc --state-dir /var/lib/flintyard-tutorial \
  --dry-run --plan .local/quickstart/gc-plan.json
sudo cat .local/quickstart/gc-plan.json
```

After checking the targets and resources, apply that plan to the same state directory.

```sh
sudo /opt/flintyard/bin/flintyard gc --state-dir /var/lib/flintyard-tutorial \
  --apply --plan .local/quickstart/gc-plan.json
```

A plan expires after 10 minutes. If it expires or the host, target state, or resource identities change, generate a new plan at a new path. See [CLI exit codes and events](cli.md#output-and-events) for scripting and [the container workload tutorial](container-workload.md) for a full example.
