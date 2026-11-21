# Build, import, and validate Linux images

[简体中文](images.zh-CN.md)

Flintyard's portable `.fyimg` format contains a manifest, a Linux kernel, a raw root disk, and optionally an initrd. The image ID identifies the manifest. The package SHA-256 covers the complete compressed archive. Keep the two values when publishing an image to a trusted artifact store.

## Import an existing image

Use `FY` from the installation guide. Set `IMAGE` to the package path and obtain `EXPECTED_SHA256` from a trusted source. [Installation](install.md)

```sh
STATE="${STATE:-/var/lib/flintyard}"
sudo "$FY" image import --state-dir "$STATE" --sha256 "$EXPECTED_SHA256" "$IMAGE"
sudo "$FY" image inspect "$IMAGE"
```

Import verifies manifest and payload integrity and publishes an immutable cache entry. Use the displayed image ID as a node's `image` value. For scripts, add `-o json` and read `data.image_id`. The manifest schema defines the supported architecture, boot arguments, first-boot modes, and payload fields. [Image schema](../schemas/image-v1.schema.json)

## Package your own files

Create a local import specification with matching kernel, optional initrd, raw root disk, and payload digests. Choose the root device path the guest kernel expects. Resolve relative input paths from the directory containing the spec. Choose a new path for the output package.

```sh
sudo "$FY" image import --state-dir "${STATE:-/var/lib/flintyard}" --spec ./import.json --output ./custom.fyimg
```

Use the [import specification](../examples/import-spec.json) as a starting point. Calculate actual digests with `sha256sum`. An image with `external` initialization must handle networking and access itself. Use `seed-v1` when the image contains the matching first-boot adapter.

## Build a reusable image

Set `RECIPE` to the JSON recipe path. Build on a Linux host matching the image architecture. For a ready-to-use recipe, prepare the Debian cloud image first. [Debian cloud image](../fixtures/README.en.md#debian-stock-kernel-offline-image)

```sh
sudo "$FY" doctor --mode build
sudo "$FY" image build --state-dir "${STATE:-/var/lib/flintyard}" --offline --recipe "$RECIPE" --timeout 20m
```

The build uses a local raw root disk or a root filesystem archive. An archive recipe specifies the ext4 disk size. A raw root disk retains its source size, so prepare a filesystem with enough space for the intended guest workload. Steps declared by a recipe run with root privileges inside the build root. Review the recipe and its source artifacts before running a build.

- **Prebuilt kernel**. Pin a kernel binary and provide the matching modules in the root filesystem. The [prebuilt recipe](../examples/build-prebuilt.json) shows the fields.
- **Kernel source**. Pin the source archive, configuration, and ordered patches by digest. The pinned Linux recipe uses `make`, `gcc`, `ld`, `flex`, `bison`, `bc`, and `xz`. Flintyard checks that the supplied configuration stays intact and records compiler details and the resulting kernel release. [Source recipe](../examples/build-kernel-source.json)
- **Initrd and transport**. Supply a matching initrd or generate it explicitly in the build steps using `generated_initrd_path`. Virtio MMIO is the default. Set `boot.virtio_transport` to `pci` for a kernel and initrd with compatible PCI drivers.

Use `--offline` with prepared local inputs or a complete input cache. Ordinary builds can fetch pinned HTTPS inputs. Use a trusted publisher for the inputs and check their integrity against the lock. [Recipe schema](../schemas/recipe-v1.schema.json) · [Distribution image recipes](../fixtures/README.en.md)

## Test boot and access

`image inspect` checks the package statically. `image validate` creates a temporary microVM, runs the declared TCP or SSH banner probe, saves a host-specific validation record, and cleans up.

```sh
VALIDATE_WORK="$(mktemp -d)"
ssh-keygen -q -t ed25519 -N '' -f "$VALIDATE_WORK/id_ed25519"
sudo "$FY" image validate --state-dir "${STATE:-/var/lib/flintyard}" --timeout 5m \
  --firecracker "$FIRECRACKER" --jailer "$JAILER" \
  --public-key "$VALIDATE_WORK/id_ed25519.pub" "$IMAGE"
rm -- "$VALIDATE_WORK/id_ed25519" "$VALIDATE_WORK/id_ed25519.pub"
rmdir -- "$VALIDATE_WORK"
```

The example uses `FIRECRACKER` and `JAILER` from installation and generates a temporary SSH key for a `seed-v1` image. For `external` initialization, omit `--public-key` and configure access through the image. A successful SSH banner confirms that the SSH protocol is reachable. Use an SSH client for an authenticated login. Pass `--keep-failed` and inspect the resulting environment when debugging a failed boot. [Runtime and SSH examples](usage.md)
