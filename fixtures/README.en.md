# Distribution image recipes

[简体中文](README.md) · [Image build reference](../docs/images.md)

Run the commands from the repository root. Preparing inputs requires Linux and Python 3.9+. Install Flintyard at `/opt/flintyard/bin/flintyard` and build on a host matching the image architecture. Each command block defines its own variables and reads the files named in the preceding steps. Choose new paths for prepared input directories, recipes, and image packages. [Build the CLI](../docs/usage.md#1-prepare-the-host-and-build-flintyard) · [Input lock](lock.json) · [Compatibility](../docs/compatibility.md)

| Goal | Steps |
| --- | --- |
| Debian stock kernel | [Offline Debian walkthrough](#debian-stock-kernel-offline-image). |
| Ubuntu stock kernel | [Prepare the Ubuntu root filesystem](#prepare-a-root-filesystem) → [Ubuntu stock kernel](#ubuntu-stock-kernel). |
| Rocky Linux stock kernel | [Prepare the Rocky root filesystem](#prepare-a-root-filesystem) → [Rocky Linux stock kernel](#rocky-linux-stock-kernel). |
| Ubuntu kernel from source | [Prepare the Ubuntu root filesystem](#prepare-a-root-filesystem) → [Build with a kernel from source](#build-with-a-kernel-from-source). |
| Alpine offline packages and kernel from source | [Prepare the Alpine root filesystem](#prepare-a-root-filesystem) → [Download pinned kernel source](#download-the-pinned-kernel-source) → [Alpine offline packages](#alpine-offline-packages). |

## Debian stock-kernel offline image

The main walkthrough includes the download, verification, extraction, recipe rendering, and build commands. To prepare inputs for the container workload, stop after the download and extraction block. It writes `.local/quickstart/debian-inputs/`. To produce a bootable `.fyimg`, continue with the build block. [Debian build steps](../docs/usage.md#3-build-the-debian-image-or-prepare-an-existing-image) · [Debian input lock](debian-cloud.lock.json) · [Container workload](../docs/container-workload.md)

## Prepare a root filesystem

Set `family=ubuntu` to the distribution you need, using `ubuntu`, `rocky`, `alpine`, or `debian`. This writes `.local/fixtures/<family>/rootfs.tar.gz`. The preparation script checks the source archive and normalizes it against the lock. Ubuntu's normalized root filesystem needs devtmpfs in the boot kernel.

```sh
(
  set -eu
  family=ubuntu
  arch=$(uname -m)
  work=".local/fixtures/$family"
  mkdir -p "$work"
  url=$(jq -er --arg family "$family" --arg arch "$arch" \
    '.families[$family].arches[$arch].url' fixtures/lock.json)
  curl --fail --location --output "$work/source.archive" "$url"
  python3 fixtures/prepare.py "$family" "$arch" \
    --input "$work/source.archive" --output "$work/rootfs.tar.gz"
)
```

## Ubuntu stock kernel

Prepare `.local/fixtures/ubuntu/rootfs.tar.gz` and install `dpkg-deb` on the host. The preparation script checks and extracts the kernel and modules packages. The build generates the initrd. [Ubuntu kernel lock](ubuntu-original.lock.json)

```sh
(
  set -eu
  arch=$(uname -m)
  work=.local/fixtures/ubuntu-original
  mkdir -p "$work"
  image_url=$(jq -er --arg arch "$arch" '.architectures[$arch].image_package.url' \
    fixtures/ubuntu-original.lock.json)
  modules_url=$(jq -er --arg arch "$arch" '.architectures[$arch].modules_package.url' \
    fixtures/ubuntu-original.lock.json)
  curl --fail --location --output "$work/linux-image.deb" "$image_url"
  curl --fail --location --output "$work/linux-modules.deb" "$modules_url"
  python3 fixtures/ubuntu-original.py "$arch" --base-rootfs .local/fixtures/ubuntu/rootfs.tar.gz \
    --image-deb "$work/linux-image.deb" --modules-deb "$work/linux-modules.deb" \
    --output-dir "$work/inputs"
  python3 fixtures/render-ubuntu-original.py "$arch" --input-dir "$work/inputs" \
    --output "$work/ubuntu.fyimg" --recipe "$work/build.recipe.json"
  sudo /opt/flintyard/bin/flintyard image build --state-dir /var/lib/flintyard-tutorial \
    --offline --timeout 20m --recipe "$work/build.recipe.json" -o json > "$work/build.json"
)
```

## Rocky Linux stock kernel

Prepare `.local/fixtures/rocky/rootfs.tar.gz` and install `rpmkeys`, `rpm2cpio`, and `cpio`. Download the RPMs and signing key from the lock. The preparation script checks package digests and signatures. The recipe uses local RPMs, generates an initrd, and enables virtio PCI. [Kernel lock](rocky-original-kernel.lock.json) · [RPM lock](rocky-packages.lock.json)

```sh
(
  set -eu
  arch=$(uname -m)
  work=.local/fixtures/rocky-original
  mkdir -p "$work/rpms"
  key_url=$(jq -er '.signing_key_url' fixtures/rocky-original-kernel.lock.json)
  curl --fail --location --output "$work/RPM-GPG-KEY-Rocky-9" "$key_url"
  jq -er --arg arch "$arch" '.packages[$arch] | to_entries[] | [.key, .value.url] | @tsv' \
    fixtures/rocky-packages.lock.json > "$work/rpms.tsv"
  while IFS="$(printf '\t')" read -r filename url; do
    curl --fail --location --output "$work/rpms/$filename" "$url"
  done < "$work/rpms.tsv"
  python3 fixtures/rocky-original.py "$arch" --rootfs .local/fixtures/rocky/rootfs.tar.gz \
    --package-dir "$work/rpms" --key "$work/RPM-GPG-KEY-Rocky-9" \
    --output-dir "$work/inputs" --image "$work/rocky.fyimg" --recipe "$work/build.recipe.json"
  sudo /opt/flintyard/bin/flintyard image build --state-dir /var/lib/flintyard-tutorial \
    --offline --timeout 20m --recipe "$work/build.recipe.json" -o json > "$work/build.json"
)
```

## Build with a kernel from source

The Ubuntu example uses the prepared `.local/fixtures/ubuntu/rootfs.tar.gz`. Install the native kernel build tools, then download the pinned upstream source. [Build tools and recipe options](../docs/images.md#build-a-reusable-image)

### Download the pinned kernel source

Use this archive for Ubuntu and Alpine source builds. The block retains `.local/fixtures/kernel/linux.tar.xz` and extracts `linux-<release>/` alongside it.

```sh
(
  set -eu
  work=.local/fixtures/kernel
  mkdir -p "$work"
  url=$(jq -er '.kernel.url' fixtures/lock.json)
  digest=$(jq -er '.kernel.sha256' fixtures/lock.json)
  curl --fail --location --output "$work/linux.tar.xz" "$url"
  printf '%s  %s\n' "$digest" "$work/linux.tar.xz" | sha256sum -c
  tar -xJf "$work/linux.tar.xz" -C "$work"
)
```

### Render and build the Ubuntu source-kernel recipe

```sh
(
  set -eu
  arch=$(uname -m)
  release=$(jq -er '.kernel.release' fixtures/lock.json)
  work=.local/fixtures/ubuntu-source
  mkdir -p "$work"
  python3 fixtures/configure-kernel.py --source ".local/fixtures/kernel/linux-$release" \
    --architecture "$arch" --output "$work/kernel.config" --modules enabled
  python3 fixtures/render.py ubuntu "$arch" --rootfs .local/fixtures/ubuntu/rootfs.tar.gz \
    --kernel .local/fixtures/kernel/linux.tar.xz --config "$work/kernel.config" \
    --output "$work/ubuntu.fyimg" --recipe "$work/build.recipe.json"
  sudo /opt/flintyard/bin/flintyard image build --state-dir /var/lib/flintyard-tutorial \
    --offline --timeout 150m --recipe "$work/build.recipe.json" -o json > "$work/build.json"
)
```

For minimal Debian, Rocky Linux, and Alpine roots using the generic `render.py` source workflow, set `--modules disabled` unless the root already has `depmod` before build steps run. Recipes that fetch packages during the build need networking. The Alpine workflow below prepares its packages locally.

## Alpine offline packages

Prepare `.local/fixtures/alpine/rootfs.tar.gz` and the [pinned kernel source](#download-the-pinned-kernel-source) first. Download the locked packages and indexes, bundle them into the root filesystem, and configure the kernel with modules disabled. The download needs network access. The build runs offline. [Alpine package lock](alpine-packages.lock.json)

```sh
(
  set -eu
  arch=$(uname -m)
  release=$(jq -er '.kernel.release' fixtures/lock.json)
  work=.local/fixtures/alpine-offline
  mkdir -p "$work"
  python3 fixtures/fetch-alpine.py "$arch" --output-dir "$work/packages"
  python3 fixtures/alpine-bundle.py "$arch" --rootfs .local/fixtures/alpine/rootfs.tar.gz \
    --package-dir "$work/packages" --output "$work/rootfs-offline.tar.gz"
  python3 fixtures/configure-kernel.py --source ".local/fixtures/kernel/linux-$release" \
    --architecture "$arch" --output "$work/kernel.config" --modules disabled
  python3 fixtures/render.py alpine "$arch" --rootfs "$work/rootfs-offline.tar.gz" \
    --kernel .local/fixtures/kernel/linux.tar.xz --config "$work/kernel.config" \
    --output "$work/alpine.fyimg" --recipe "$work/build.recipe.json"
  sudo /opt/flintyard/bin/flintyard image build --state-dir /var/lib/flintyard-tutorial \
    --offline --timeout 150m --recipe "$work/build.recipe.json" -o json > "$work/build.json"
)
```

Use `data.image_id` in the corresponding `build.json` to configure a guest in the same state directory. To move the image to another host, copy both the `.fyimg` and `data.package_sha256` and follow the [import steps](../docs/images.md#import-an-existing-image).
