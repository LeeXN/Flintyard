# Third-party materials

The Flintyard binaries include code from the Go standard library and `golang.org/x/sys` v0.42.0. Both distributions include the same license and patent grant. [Go license](licenses/go/LICENSE) · [Go patent grant](licenses/go/PATENTS)

The kernel configuration templates in [fixtures/kernels](fixtures/kernels) come from Firecracker 1.17.0. The upstream source revision and template checksums are recorded in the [fixture lock](fixtures/lock.json). The Firecracker distribution's license and notices are included with the templates. [Firecracker license](licenses/firecracker/LICENSE) · [Firecracker notice](licenses/firecracker/NOTICE)

The Alpine repository indexes in [fixtures/alpine-indexes](fixtures/alpine-indexes) are upstream package metadata. The [Alpine package lock](fixtures/alpine-packages.lock.json) records the source indexes and package versions. Individual packages retain their upstream notices and licensing terms.

Downloaded distro images, kernel source, Firecracker binaries, Go toolchains, and container images retain their respective upstream licensing terms. See the [fixture documentation](fixtures/README.en.md) for their sources.
