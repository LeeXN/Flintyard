# 在临时微虚机中运行容器业务

[English](container-workload.md)

从一份 Debian 13 镜像启动两台 Firecracker 微虚机，在 Docker 和 K3s 中分别部署 Gitea。示例可在本机运行，也可交给自托管 GitLab Runner。

## 在本机运行

先完成主教程的二进制安装和 Debian 下载与提取。此处使用 `/opt/flintyard/bin/` 的三个二进制和 `.local/quickstart/debian-inputs/`，无需先构建 `.fyimg`。从仓库根目录执行。宿主需安装 `git`，并按网络策略启用 IPv4 转发。运行时输入客户机可达的 IPv4 DNS 地址，允许客户机访问 Debian 软件包仓库、GitHub 下载及 Docker Hub。为两台虚机预留至少 3,328 MiB 可用内存，并为镜像和磁盘预留空间。 [二进制安装](usage.zh-CN.md#1-准备宿主并构建二进制) · [准备 Debian 输入](../fixtures/README.md#debian-原版内核离线镜像) · [网络](networking.zh-CN.md)

```sh
(
  set -eu
  printf 'Guest-reachable IPv4 DNS: '
  IFS= read -r guest_dns
  test -n "$guest_dns"
  test -f .local/quickstart/debian-inputs/provenance.json
  report_dir=$(sudo mktemp -d /var/lib/flintyard-workload.XXXXXXXX)
  sudo bash tests/container-workload.sh /opt/flintyard/bin/flintyard \
    /opt/flintyard/bin/firecracker /opt/flintyard/bin/jailer \
    .local/quickstart/debian-inputs "$report_dir" "$guest_dns"
)
```

脚本会输出报告目录。若 `/var/lib` 空间不足，把 `mktemp` 命令的目录改成容量足够、由 root 管理的已有路径。Docker 和 K3s 的宿主端口分别为 13030 和 13031。可在 `sudo` 后显式传入 `FY_DEMO_DOCKER_PORT` 和 `FY_DEMO_K3S_PORT`，例如 `sudo FY_DEMO_DOCKER_PORT=13130 FY_DEMO_K3S_PORT=13131 bash …`。通过 `sudo FY_DEMO_HOLD_SECONDS=120 bash …` 可在验证成功后保留一段访问时间，此时可从两个映射端口访问 `/flintyard/container-demo`。

CI 通过 SSH 连接客户机，并使用镜像中配置的临时 `devops` 用户安装容器平台。 [Gitea 容器配置](https://docs.gitea.com/installation/install-with-docker/)

## 在自托管 GitLab Runner 中运行

从 GitLab 界面或 API 在受保护分支启动 [父流水线](../ci/gitlab-demo.yml)。父流水线使用资源组，在整个子流水线运行期间串行调度本项目的演示任务，各作业分别归档 `workload-stage.log`。 [子流水线定义](../ci/gitlab-demo-jobs.yml) · [GitLab 资源组](https://docs.gitlab.com/ci/resource_groups/)

| CI 作业 | 执行内容 |
| --- | --- |
| `build-image` | 通过 Flintyard 构建并导入锁定的 Debian 镜像。 |
| `start-vms` | 使用缓存镜像启动两台客户机，检查 SSH banner。 |
| `deploy-gitea` | 在对应客户机中安装 Docker 和 K3s，并部署 Gitea。 |
| `verify-gitea` | 通过宿主映射检查两个 Gitea API 和 Git HEAD。 |
| `destroy-vms` | 执行 Flintyard 清理。如果已完成业务验证，同时检查两个端口已关闭。前序作业失败时仍执行此作业。 |

![五个 GitLab 作业均已成功完成](images/flintyard-workload-stages.png)

使用支持 `strategy: mirror` 的 GitLab 18.2 或更新版本。将项目的 CI 配置文件设为 `ci/gitlab-demo.yml`，在 KVM 宿主上注册一台专用于项目可信代码的受保护 Shell Runner，标签设为 `flintyard-workload`。五个作业使用同一台机器。 [GitLab 流水线设置](https://docs.gitlab.com/ci/pipelines/settings/) · [下游流水线](https://docs.gitlab.com/ci/pipelines/downstream_pipelines/)

在 Runner 宿主上准备主教程的二进制和 Debian 输入，在仓库根目录运行以下命令。输入这个 Runner 上的客户机可达的 IPv4 DNS 服务器。

```sh
(
  set -eu
  printf 'Guest-reachable IPv4 DNS: '
  IFS= read -r guest_dns
  test -n "$guest_dns"
  test -f .local/quickstart/debian-inputs/provenance.json
  install_dir=/opt/flintyard-demo
  data_dir=/var/lib/flintyard-demo
  sudo install -d -m 0755 "$install_dir/bin" "$install_dir/tests" "$install_dir/fixtures"
  sudo install -d -m 0700 "$data_dir" "$data_dir/inputs"
  sudo install -m 0755 /opt/flintyard/bin/flintyard /opt/flintyard/bin/firecracker \
    /opt/flintyard/bin/jailer "$install_dir/bin/"
  sudo install -m 0755 ci/gitlab-demo-run-stage.sh "$install_dir/run-stage"
  sudo install -m 0644 tests/container-workload.sh tests/container-workload-guest.sh "$install_dir/tests/"
  sudo install -m 0644 fixtures/render-debian-cloud.py fixtures/prepare.py fixtures/render.py \
    fixtures/debian-cloud.lock.json fixtures/lock.json "$install_dir/fixtures/"
  sudo cp -R -- .local/quickstart/debian-inputs/. "$data_dir/inputs/"
  printf '%s\n' "$guest_dns" | sudo tee "$install_dir/dns-ipv4" >/dev/null
)
```

用 `visudo` 授权 Runner 账号免密执行已安装的 `/opt/flintyard-demo/run-stage`。以 `gitlab-runner` 账号为例。

```sudoers
gitlab-runner ALL=(root) NOPASSWD: /opt/flintyard-demo/run-stage
```

各作业的 `workload-stage.log` 可从 GitLab 下载。镜像、状态和诊断文件位于 Runner 上由 root 管理的 `/var/lib/flintyard-demo/ci-runs/workload.<child-pipeline-id>`。脚本在清理时删除临时 SSH 密钥。构建或部署失败后，可启动一条新的流水线。

如果流水线在两个作业之间被取消，或 Runner 离线，可在流水线停止后在 Runner 宿主上进行清理。命令提示时输入子流水线 URL（`/-/pipelines/<id>`）中的数字 ID。如果 Flintyard 报告清理不完整，检查报告目录，按 [资源回收](usage.zh-CN.md#回收残留资源) 处理。

```sh
(
  set -eu
  printf 'Child pipeline ID: '
  IFS= read -r pipeline_id
  sudo /opt/flintyard-demo/run-stage cleanup "$pipeline_id"
)
```

## 核对运行结果

### 镜像与虚机状态

在 `build.json` 中核对镜像 ID、包摘要和内核版本。`status.json` 应显示两台客户机均为 `ready`，各有独立的 IPv4 地址和映射到宿主回环地址的 Web 端口。

![GitLab 作业中的离线 Debian 镜像构建与两台微虚机就绪](images/flintyard-workload-build-up.png)

### Gitea 部署

Docker 在容器中运行 Gitea，K3s 通过 NodePort Service 暴露 Gitea Deployment。对每个映射端口，确认 `http-<port>.json` 指向 `flintyard/container-demo` 仓库，且 `git-<port>.log` 包含 `git ls-remote … HEAD` 返回的提交。

| Docker 上的 Gitea | K3s 上的 Gitea |
| --- | --- |
| ![Docker 上的 Gitea 仓库](images/flintyard-workload-docker.png) | ![K3s 上的 Gitea 仓库](images/flintyard-workload-k3s.png) |

### 清理

确认 `down.json` 的状态为 `deleted`，且 `cleanup_errors` 为空。两个映射的 Web 入口应停止响应。构建好的镜像和诊断文件保留在报告目录中。

![两个仓库验证通过、环境销毁且映射关闭](images/flintyard-workload-cleanup.png)
