# JSON configuration and result examples

[简体中文](README.md)

Use these JSON files as templates for image recipes, environment configuration, and CLI results. Before starting guests, replace the placeholder paths, image IDs, digests, and public keys. [User guide](../docs/usage.md)

| File | Purpose |
| --- | --- |
| [image-manifest.json](image-manifest.json) | Manifest carried inside an image package |
| [import-spec.json](import-spec.json) | Assemble an image package from local files |
| [build-prebuilt.json](build-prebuilt.json) | Build a root disk with a prebuilt kernel |
| [build-kernel-source.json](build-kernel-source.json) | Build a source kernel, modules, and initrd for aarch64 |
| [environment.json](environment.json) | Three x86_64 nodes on a private network |
| [environment-aarch64.json](environment-aarch64.json) | Three aarch64 nodes on a private network |
| [environment-nat.json](environment-nat.json) | NAT and loopback port forwarding |
| [result-up.json](result-up.json) | Result for an accessible environment |
| [result-failed.json](result-failed.json) | Probe timeout and cleanup result |
| [event.json](event.json) | One progress event |

Use the kernel release and SHA-256 values from your inputs. Replace `192.0.2.53` and `eth0` in the NAT example with a guest-reachable resolver and the actual host egress interface. The private network examples leave public DNS unset.

The source recipe's config, source archive, and initrd paths must point to matching files. Declare and provide `xz` for `.tar.xz` root filesystem archives. [Image guide](../docs/images.md)
