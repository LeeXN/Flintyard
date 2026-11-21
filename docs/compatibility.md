# Host and image compatibility

[简体中文](compatibility.zh-CN.md)

Flintyard targets Linux on `x86_64` (`amd64`) and `aarch64` (`arm64`). Use the official Firecracker and jailer versions in the compatibility lock, install binaries for the host architecture, and run `doctor` before provisioning. [Compatibility lock](../compatibility.lock.json) · [Installation](install.md)

| Architecture | Runtime coverage |
| --- | --- |
| `x86_64` | Native KVM with Debian, Ubuntu, Rocky Linux, and Alpine images. The GitLab container example ran on an x86_64 host with nested KVM. |
| `aarch64` | Debian image build, boot, SSH, stop, resume, and cleanup on a QEMU-emulated aarch64 host. |

A bootable image needs a kernel, root filesystem, and initialization profile that work together, plus matching modules and initrd when used. Run `image validate` on the intended deployment host. [Image guide](images.md#test-boot-and-access) · [Distribution image recipes](../fixtures/README.en.md)

Use the [JSON schemas](../schemas/) from the same revision as your CLI for `flintyard/v1alpha1` configuration and results.
