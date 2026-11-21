# Run a container workload in disposable microVMs

[简体中文](container-workload.zh-CN.md)

Start two Firecracker microVMs from the same Debian 13 image and deploy Gitea on Docker and K3s. Run the example locally or on a self-hosted GitLab Runner.

## Run the walkthrough

Complete installation and prepare `DEBIAN_INPUTS` using the Debian guide. Install `git` on the host and enable IPv4 forwarding under your host policy. Set `GUEST_DNS` to an IPv4 DNS server reachable by the guests. Allow guest access to Debian package repositories, GitHub downloads, and Docker Hub. Set aside at least 3,328 MiB of available memory for the two guests and space for the image and disks. [Installation](install.md) · [Prepare Debian inputs](../fixtures/README.en.md#debian-stock-kernel-offline-image) · [Networking](networking.md)

```sh
STATE_PARENT="${STATE_PARENT:-/var/lib}"
REPORT_DIRECTORY="$(sudo mktemp -d "$STATE_PARENT/flintyard-workload.XXXXXXXX")"
: "${GUEST_DNS:?set a DNS server reachable from the guests}" "${DEBIAN_INPUTS:?prepare Debian inputs}"
sudo bash tests/container-workload.sh \
  "$FY" "$FIRECRACKER" "$JAILER" "$DEBIAN_INPUTS" "$REPORT_DIRECTORY" "$GUEST_DNS"
```

The script prints the report directory. If `/var/lib` is too small, set `STATE_PARENT` to an existing root-managed directory on a larger filesystem. The host-local web ports are 13030 for Docker and 13031 for K3s. To use different ports, pass `FY_DEMO_DOCKER_PORT` and `FY_DEMO_K3S_PORT` explicitly through `sudo`. Pass `FY_DEMO_HOLD_SECONDS=120` through `sudo` to keep a short browsing window open after verification. Visit `/flintyard/container-demo` at each forwarded port during that window.

The example connects to each guest over SSH and uses the disposable `devops` account to install the container platform. [Gitea container configuration](https://docs.gitea.com/installation/install-with-docker/)

## Use a self-hosted CI runner

The [parent pipeline](../ci/gitlab-demo.yml) runs on a protected ref when started from the GitLab UI or API. Its resource group serializes demo runs within the project for the duration of the [child pipeline](../ci/gitlab-demo-jobs.yml). Each stage publishes its own `workload-stage.log`. [GitLab resource groups](https://docs.gitlab.com/ci/resource_groups/)

| CI job | Action |
| --- | --- |
| `build-image` | Build and import the locked Debian image with Flintyard. |
| `start-vms` | Boot the two guests from the cached image and verify SSH banner access. |
| `deploy-gitea` | Install Docker and K3s in the corresponding guests, then deploy Gitea. |
| `verify-gitea` | Check both Gitea API responses and Git HEAD refs through the host forwards. |
| `destroy-vms` | Run Flintyard cleanup and check both ports have closed after successful verification. Run even when an earlier job fails. |

![All five GitLab jobs completed successfully](images/flintyard-workload-stages.png)

Use GitLab 18.2 or newer for `strategy: mirror`. Set the project's CI configuration file to `ci/gitlab-demo.yml` and assign one project-locked, protected Shell Runner tagged `flintyard-workload` on the KVM host. All five jobs use that same machine. Use a Runner dedicated to trusted project code. [GitLab pipeline settings](https://docs.gitlab.com/ci/pipelines/settings/) · [Downstream pipelines](https://docs.gitlab.com/ci/pipelines/downstream_pipelines/)

On the Runner host, install the reviewed scripts, binaries, and prepared inputs from this checkout. The operator configures `GUEST_DNS` as in the walkthrough above.

```sh
DEMO_INSTALL=/opt/flintyard-demo
DEMO_DATA=/var/lib/flintyard-demo
sudo install -d -m 0755 "$DEMO_INSTALL/bin" "$DEMO_INSTALL/tests" "$DEMO_INSTALL/fixtures"
sudo install -d -m 0700 "$DEMO_DATA" "$DEMO_DATA/inputs"
sudo install -m 0755 "$FY" "$DEMO_INSTALL/bin/flintyard"
sudo install -m 0755 "$FIRECRACKER" "$DEMO_INSTALL/bin/firecracker"
sudo install -m 0755 "$JAILER" "$DEMO_INSTALL/bin/jailer"
sudo install -m 0755 ci/gitlab-demo-run-stage.sh "$DEMO_INSTALL/run-stage"
sudo install -m 0644 tests/container-workload.sh tests/container-workload-guest.sh "$DEMO_INSTALL/tests/"
sudo install -m 0644 fixtures/render-debian-cloud.py fixtures/prepare.py fixtures/render.py \
  fixtures/debian-cloud.lock.json fixtures/lock.json "$DEMO_INSTALL/fixtures/"
sudo cp -R -- "$DEBIAN_INPUTS"/. "$DEMO_DATA/inputs/"
printf '%s\n' "$GUEST_DNS" | sudo tee "$DEMO_INSTALL/dns-ipv4" >/dev/null
```

Use `visudo` to grant the Runner account passwordless access to the installed `/opt/flintyard-demo/run-stage` command. The following entry uses the `gitlab-runner` account.

```sudoers
gitlab-runner ALL=(root) NOPASSWD: /opt/flintyard-demo/run-stage
```

Download each job's `workload-stage.log` from GitLab. On the runner, inspect the image, state, and diagnostics in the root-owned `/var/lib/flintyard-demo/ci-runs/workload.<child-pipeline-id>` directory. The script removes the temporary SSH key during cleanup. Start a fresh pipeline after a failed build or deployment.

If the pipeline is canceled between jobs or the runner goes offline, cleanup may need an operator. After the pipeline stops, set `CHILD_PIPELINE_ID` to the numeric ID in the child pipeline URL (`/-/pipelines/<id>`) and run the command on the runner host. If Flintyard reports incomplete cleanup, inspect the report directory and follow [resource recovery](usage.md#recover-stale-resources).

```sh
sudo /opt/flintyard-demo/run-stage cleanup "$CHILD_PIPELINE_ID"
```

## Check the results

### Image and VM readiness

Check `build.json` for the image ID, package digest, and kernel version. In `status.json`, both guests should be `ready`, with distinct IPv4 addresses and a loopback port forward for each web endpoint.

![An offline Debian image build and two ready microVMs in a GitLab job](images/flintyard-workload-build-up.png)

### Gitea deployment

Docker runs Gitea in a container. K3s exposes a Gitea Deployment through a NodePort Service. For each forwarded port, check that `http-<port>.json` identifies the `flintyard/container-demo` repository and that `git-<port>.log` contains the commit returned by `git ls-remote … HEAD`.

| Gitea on Docker | Gitea on K3s |
| --- | --- |
| ![Gitea repository running on Docker](images/flintyard-workload-docker.png) | ![Gitea repository running on K3s](images/flintyard-workload-k3s.png) |

### Cleanup

Confirm that `down.json` reports `deleted` with an empty `cleanup_errors` list. Both forwarded web endpoints should stop responding. The built image and diagnostics remain in the report directory.

![Both repositories checked and the environment deleted with both endpoints closed](images/flintyard-workload-cleanup.png)
