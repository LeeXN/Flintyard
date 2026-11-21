# Distribution image recipes

[简体中文](README.md)

Use these recipes to build Flintyard images from pinned Ubuntu, Debian, Rocky Linux, and Alpine inputs. The preparation tools require Linux and Python 3.9 or newer. Set `FY` using the installation guide. Use `TMPDIR` for downloads and intermediates on a filesystem with more space. Set `STATE` to a root-managed path to keep the image cache on a different filesystem. [Installation](../docs/install.md) · [Input lock](lock.json) · [Compatibility](../docs/compatibility.md)

## Prepare a root filesystem

Run from the repository root. Choose `ubuntu`, `debian`, `rocky`, or `alpine`. The lock supplies the URL and digest for the selected distribution and architecture. The Ubuntu preparer normalizes the device entries listed in the lock. Use a kernel with devtmpfs for the resulting image.

```sh
ARCH="$(uname -m)"
FAMILY=ubuntu
ROOT_WORK="$(mktemp -d)"
curl --fail --location --output "$ROOT_WORK/source.archive" \
  "$(jq -er --arg family "$FAMILY" --arg arch "$ARCH" '.families[$family].arches[$arch].url' fixtures/lock.json)"
python3 fixtures/prepare.py "$FAMILY" "$ARCH" --input "$ROOT_WORK/source.archive" \
  --output "$ROOT_WORK/$FAMILY-$ARCH.tar.gz"
```

## Build with a kernel from source

This example uses the prepared Ubuntu root filesystem and pinned upstream Linux source. Install the recipe's compiler dependencies and generate the configuration on the target architecture. Use `--modules disabled` with the minimal Debian, Rocky Linux, or Alpine roots. Recipes without prepared package bundles require access to the respective package repository. [Build requirements and recipe options](../docs/images.md#build-a-reusable-image)

```sh
KERNEL_WORK="$(mktemp -d)"
KERNEL_RELEASE="$(jq -er .kernel.release fixtures/lock.json)"
KERNEL_ARCHIVE="$KERNEL_WORK/linux-$KERNEL_RELEASE.tar.xz"
curl --fail --location --output "$KERNEL_ARCHIVE" "$(jq -er .kernel.url fixtures/lock.json)" &&
printf '%s  %s\n' "$(jq -er .kernel.sha256 fixtures/lock.json)" "$KERNEL_ARCHIVE" | sha256sum -c &&
tar -xJf "$KERNEL_ARCHIVE" -C "$KERNEL_WORK"
```

Generate the configuration and build the image using the prepared Ubuntu root filesystem.

```sh
python3 fixtures/configure-kernel.py --source "$KERNEL_WORK/linux-$KERNEL_RELEASE" \
  --architecture "$ARCH" --output "$KERNEL_WORK/ubuntu.config" --modules enabled
python3 fixtures/render.py ubuntu "$ARCH" --rootfs "$ROOT_WORK/ubuntu-$ARCH.tar.gz" \
  --kernel "$KERNEL_ARCHIVE" --config "$KERNEL_WORK/ubuntu.config" \
  --output "$ROOT_WORK/ubuntu.fyimg" --recipe "$ROOT_WORK/ubuntu.recipe.json"
sudo "$FY" image build --state-dir "${STATE:-/var/lib/flintyard}" --offline --timeout 150m \
  --recipe "$ROOT_WORK/ubuntu.recipe.json"
```

## Debian stock-kernel offline image

Run from the repository root. The preparation tools use `sfdisk` and `debugfs` to verify the pinned cloud archive and extract the matching root disk, stock kernel, and initrd. Use `DEBIAN_INPUTS` from this step in the container example. [Debian input lock](debian-cloud.lock.json) · [Container example](../docs/container-workload.md)

```sh
ARCH="$(uname -m)"
DEMO="$(mktemp -d)"
curl --fail --location --output "$DEMO/debian-cloud.tar.xz" \
  "$(jq -er --arg arch "$ARCH" '.architectures[$arch].url' fixtures/debian-cloud.lock.json)"
python3 fixtures/debian-cloud.py "$ARCH" --archive "$DEMO/debian-cloud.tar.xz" \
  --output-dir "$DEMO/inputs"
DEBIAN_INPUTS="$DEMO/inputs"
```

To produce a `.fyimg`, render and build the offline recipe from those inputs.

```sh
RECIPE="$DEMO/debian.recipe.json"
python3 fixtures/render-debian-cloud.py "$ARCH" --input-dir "$DEBIAN_INPUTS" \
  --output "$DEMO/debian.fyimg" --recipe "$RECIPE"
sudo "$FY" image build --state-dir "${STATE:-/var/lib/flintyard}" --offline --timeout 20m \
  -o json --recipe "$RECIPE" \
  > "$DEMO/build.json"
IMAGE="$DEMO/debian.fyimg"
EXPECTED_SHA256="$(jq -er '.data.package_sha256' "$DEMO/build.json")"
```

Use `IMAGE` and `EXPECTED_SHA256` to start a guest. [Single-node quickstart](../docs/usage.md#single-node-quickstart)

## Ubuntu stock kernel

Prepare the Ubuntu root filesystem, then download the kernel and modules for the same architecture. Install `dpkg-deb` on the preparation host. The script verifies the locked package hashes and extracts the files. The recipe generates the initrd in the build root. [Ubuntu kernel lock](ubuntu-original.lock.json)

```sh
curl --fail --location --output "$ROOT_WORK/linux-image.deb" \
  "$(jq -er --arg arch "$ARCH" '.architectures[$arch].image_package.url' fixtures/ubuntu-original.lock.json)"
curl --fail --location --output "$ROOT_WORK/linux-modules.deb" \
  "$(jq -er --arg arch "$ARCH" '.architectures[$arch].modules_package.url' fixtures/ubuntu-original.lock.json)"
python3 fixtures/ubuntu-original.py "$ARCH" \
  --base-rootfs "$ROOT_WORK/ubuntu-$ARCH.tar.gz" \
  --image-deb "$ROOT_WORK/linux-image.deb" --modules-deb "$ROOT_WORK/linux-modules.deb" \
  --output-dir "$ROOT_WORK/ubuntu-original-inputs"
python3 fixtures/render-ubuntu-original.py "$ARCH" --input-dir "$ROOT_WORK/ubuntu-original-inputs" \
  --output "$ROOT_WORK/ubuntu-original.fyimg" --recipe "$ROOT_WORK/ubuntu-original.recipe.json"
sudo "$FY" image build --state-dir "${STATE:-/var/lib/flintyard}" --offline --timeout 20m \
  --recipe "$ROOT_WORK/ubuntu-original.recipe.json"
```

## Rocky Linux stock kernel

Prepare the Rocky Linux root filesystem, then download the RPMs and signing key for the same architecture from the lock files. Set `ROCKY_RPMS` to the package directory and `ROCKY_KEY` to the key file. The preparation script uses `rpmkeys`, `rpm2cpio`, and `cpio` to verify and unpack the packages. The recipe installs local RPMs, generates an initrd, and selects virtio PCI. [Kernel lock](rocky-original-kernel.lock.json) · [RPM lock](rocky-packages.lock.json)

```sh
python3 fixtures/rocky-original.py "$ARCH" \
  --rootfs "$ROOT_WORK/rocky-$ARCH.tar.gz" --package-dir "$ROCKY_RPMS" --key "$ROCKY_KEY" \
  --output-dir "$ROOT_WORK/rocky-inputs" --image "$ROOT_WORK/rocky.fyimg" \
  --recipe "$ROOT_WORK/rocky.recipe.json"
sudo "$FY" image build --state-dir "${STATE:-/var/lib/flintyard}" --offline --timeout 20m \
  --recipe "$ROOT_WORK/rocky.recipe.json"
```

## Alpine offline packages

Prepare the Alpine root filesystem and run the kernel download and extraction commands above. [Kernel source](#build-with-a-kernel-from-source)

Bundle the pinned packages and indexes with the root filesystem, then render an offline recipe with a kernel configuration that disables modules. [Alpine package lock](alpine-packages.lock.json)

```sh
python3 fixtures/fetch-alpine.py "$ARCH" --output-dir "$ROOT_WORK/alpine-packages"
python3 fixtures/alpine-bundle.py "$ARCH" --rootfs "$ROOT_WORK/alpine-$ARCH.tar.gz" \
  --package-dir "$ROOT_WORK/alpine-packages" --output "$ROOT_WORK/alpine-$ARCH-offline.tar.gz"
python3 fixtures/configure-kernel.py --source "$KERNEL_WORK/linux-$KERNEL_RELEASE" \
  --architecture "$ARCH" --output "$KERNEL_WORK/alpine.config" --modules disabled
python3 fixtures/render.py alpine "$ARCH" --rootfs "$ROOT_WORK/alpine-$ARCH-offline.tar.gz" \
  --kernel "$KERNEL_ARCHIVE" --config "$KERNEL_WORK/alpine.config" \
  --output "$ROOT_WORK/alpine.fyimg" --recipe "$ROOT_WORK/alpine.recipe.json"
sudo "$FY" image build --state-dir "${STATE:-/var/lib/flintyard}" --offline --timeout 150m \
  --recipe "$ROOT_WORK/alpine.recipe.json"
```
