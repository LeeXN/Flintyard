# 从源码到运行第一台微虚机

[English](usage.md) · [CLI 速查](cli.zh-CN.md) · [镜像指南](images.zh-CN.md) · [网络指南](networking.zh-CN.md)

从仓库根目录开始，先构建 Flintyard，再制作或导入镜像，最后配置、启动、登录并销毁环境。每个步骤都会说明需要哪些文件以及会产出什么。可以换终端继续，只需回到同一个仓库根目录。

[构建与安装](#1-准备宿主并构建二进制) · [制作镜像](#3-制作-debian-镜像或准备已有镜像) · [导入镜像](#4-校验导入并取得镜像-id) · [配置和启动](#5-设置虚机并启动) · [登录](#6-查看状态登录和检查客户机) · [停机与销毁](#停机恢复和销毁)

| 路径 | 用途 |
| --- | --- |
| `/opt/flintyard/bin/` | 安装 Flintyard、Firecracker 和 jailer，均由 root 管理。 |
| `.local/quickstart/` | 存放本教程的镜像包、摘要、环境配置、SSH 密钥和命令结果。 |
| `/var/lib/flintyard-tutorial` | 本教程专用状态目录，存放导入缓存、环境可写盘和诊断。 |

以下命令使用专用状态目录。正式环境可以使用 CLI 默认的 `/var/lib/flintyard`，也可以选另一个由 root 管理、容量足够的真实路径。同一个环境的所有命令必须使用同一个状态目录。

## 单节点快速开始

### 1. 准备宿主并构建二进制

使用 `x86_64` 或 `aarch64` Linux 宿主，准备可用的 `/dev/kvm`、`/dev/net/tun`、4 KiB 宿主页和内存 cgroup 控制器。若宿主本身是虚机，外层需启用嵌套虚拟化。仓库、状态目录和构建临时空间所在磁盘应留有足够空间，尤其是扩展后的根盘和每台节点的可写盘。 [宿主要求](install.zh-CN.md)

先安装 Go 1.27.3 或兼容的更新工具链。在 Debian 或 Ubuntu 宿主上，可用以下命令安装本教程的系统工具。 [Go 下载](https://go.dev/dl/)

```sh
sudo apt-get update
sudo apt-get install build-essential make git ca-certificates curl jq python3 \
  iproute2 nftables e2fsprogs fdisk mount util-linux xz-utils openssh-client
```

在已检出的仓库根目录构建当前宿主的二进制。`make build` 可以同时生成 Linux amd64 和 arm64 的分发文件，本教程只需当前宿主版本。

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

### 2. 安装匹配的 Firecracker 和 jailer

从仓库锁文件指定的同一个官方归档取得两个二进制，核对 SHA-256 后再安装。Flintyard 以 root 运行时会检查二进制及其上级目录的所有权和写权限，因此这里将工具安装到 `/opt/flintyard/bin`。下面的临时变量只在这一段有效。 [兼容性锁文件](../compatibility.lock.json)

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

修复 `doctor` 报出的失败项后再继续。镜像构建使用 `doctor --mode build`。使用非默认 jail UID/GID 范围时，`doctor`、`up` 和 `image validate` 应传入相同的 `--uid-range`。

### 3. 制作 Debian 镜像，或准备已有镜像

以下两种方式任选一种。下一步都使用 `.local/quickstart/image.fyimg` 和 `.local/quickstart/image.sha256`，摘要文件只包含 64 位十六进制 SHA-256。

#### 方式 A：从锁定的 Debian 云镜像制作

先下载固定版本的 Debian 云归档。准备脚本校验归档，再提取 raw 根盘、原版内核和 initrd 到 `.local/quickstart/debian-inputs/`。目的目录须尚不存在。若已有同一架构、同一锁文件生成的输入，可以从下一段开始。 [Debian 输入锁文件](../fixtures/debian-cloud.lock.json)

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

用输入生成 `debian.recipe.json`，再执行离线构建。构建成功会输出 `image.fyimg`、写入状态目录的镜像缓存，并在 `build.json` 中报告镜像 ID、包摘要和诊断信息。配方和输出文件使用未占用的路径，重建时请另选输出文件名和配方文件名。

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

下载输入需要联网。`--offline` 作用于 Flintyard 的构建阶段，该阶段只使用准备好的本地输入或完整缓存。 [其他发行版与源码内核](../fixtures/README.md)

#### 方式 B：使用已发布的镜像包

准备同架构、支持 `seed-v1` 且 SSH 监听 22 端口的 `.fyimg`，以及由可信发布方提供的包摘要。首次试用时，把包复制为 `.local/quickstart/image.fyimg`，把对应包摘要保存为 `.local/quickstart/image.sha256`。外部镜像的摘要应由可信渠道提供。其他初始化方式见 [镜像指南](images.zh-CN.md)。

```sh
mkdir -p .local/quickstart
# 把下面两个示例路径换成你实际收到的文件路径。
cp -- /path/to/published.fyimg .local/quickstart/image.fyimg
awk 'NR == 1 {print $1}' /path/to/published.fyimg.sha256 > .local/quickstart/image.sha256
```

### 4. 校验、导入并取得镜像 ID

`image inspect` 查看包的静态信息。`image import --sha256` 核对整个包的摘要并写入本地只读缓存。方式 A 在同一个状态目录构建时已写入缓存，这里仍可执行导入命令核对包并取得统一的输出。之后配置使用 `data.image_id`，格式为 `sha256:…`。它标识 manifest，和压缩包的 `package_sha256` 各有用途。

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

发布镜像前可额外执行 `image validate`，在目标宿主临时启动客户机并验证访问探针。 [镜像启动验证](images.zh-CN.md#验证启动与访问)

### 5. 设置虚机并启动

首次执行会生成专用 SSH 密钥和 `.local/quickstart/environment.json`。配置创建 `debian-demo` 环境，包含一台 2 vCPU、1,024 MiB 内存的 `node-1`。`id_ed25519.pub` 相对于 JSON 文件解析。已有密钥时复用同一对密钥，只将公钥路径写进配置。

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

在第一次启动前，可以按下文的 [虚机配置](#设置虚机资源和网络) 修改 CPU、内存或网络。首次使用建议保持私网配置，宿主可直接 SSH 登录。

```sh
sudo /opt/flintyard/bin/flintyard up --state-dir /var/lib/flintyard-tutorial \
  --file .local/quickstart/environment.json --timeout 5m --wait configured \
  --firecracker /opt/flintyard/bin/firecracker --jailer /opt/flintyard/bin/jailer
```

`--wait configured` 等待配置中的 SSH banner 探针。`ready` 表示探针通过。`--wait none` 跳过等待，此时可再运行 `status --probe`。

### 6. 查看状态、登录和检查客户机

用 `status --probe` 获取当前端点，再由 SSH 客户端使用私钥登录。以下命令从 JSON 读取实际地址、用户和端口。首次登录时为此环境单独记录 SSH 主机密钥。SSH banner 表示 SSH 服务可达，身份认证由 SSH 客户端完成。

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

下面的命令在 **SSH 登录后的客户机内** 执行。写入标记文件后退出 SSH，稍后可用它检查磁盘是否保留。

```sh
uname -m
ip -4 addr
df -h /
printf '%s\n' 'Flintyard disk persists across stop and resume' > ~/flintyard-demo.txt
sync
exit
```

回到 **宿主**，使用 `list` 查看环境，用 `logs` 排查启动。`--source firecracker` 查看启动日志，`--source serial` 查看串口。想持续查看时，在最后的环境名之前添加 `--follow`，用 Ctrl+C 结束。

```sh
sudo /opt/flintyard/bin/flintyard list --state-dir /var/lib/flintyard-tutorial
sudo /opt/flintyard/bin/flintyard logs --state-dir /var/lib/flintyard-tutorial \
  --node node-1 --source serial --tail 100 debian-demo
```

## 停机、恢复和销毁

**保留可写盘时用 `stop`。** 先在客户机关闭应用并刷新写入。`stop` 结束 VMM，保留可写盘、seed 和节点身份。用同一份配置再次执行 `up`，重新运行 SSH 步骤后用 `cat ~/flintyard-demo.txt` 确认写入仍在。

```sh
sudo /opt/flintyard/bin/flintyard stop --state-dir /var/lib/flintyard-tutorial debian-demo
sudo /opt/flintyard/bin/flintyard up --state-dir /var/lib/flintyard-tutorial \
  --file .local/quickstart/environment.json --timeout 5m \
  --firecracker /opt/flintyard/bin/firecracker --jailer /opt/flintyard/bin/jailer
```

**拆除环境时用 `down`。** 先导出要保留的客户机数据。`down` 删除可写盘、seed 和归属该环境的运行资源，保留镜像包、镜像缓存和诊断。若也要删除诊断，在环境名前加 `--purge`。

```sh
sudo /opt/flintyard/bin/flintyard down --state-dir /var/lib/flintyard-tutorial debian-demo
```

## 设置虚机资源和网络

编辑 `environment.json` 后再启动。运行中或已停机的同名环境使用原有配置标识。要修改镜像、CPU、内存、节点列表、网络或注入公钥，建议用新环境名称先验证。要沿用原名称，先备份客户机数据，再用 `down` 删除旧环境后重新创建。安装应用和修改操作系统配置在客户机内通过 SSH 完成。

| 字段 | 如何填写 |
| --- | --- |
| `name`、`architecture` | 环境名在状态目录内唯一。架构须与宿主和所有镜像一致，使用 `x86_64` 或 `aarch64`。 |
| `nodes[]` | 每台节点指定唯一 `name`、`image`、`vcpu_count` 和 `memory_mib`。内存单位是 MiB，最少 128 MiB。当前 Firecracker 版本支持 1 至 32 vCPU。`image` 可用缓存 ID 或本地 `.fyimg` 路径，相对路径以配置文件目录为基准。 |
| `network.cidr`、`nodes[].ipv4` | `"auto"` 自动分配无冲突子网。需要固定节点 IP 时，建议同时设定明确的 RFC 1918 私网 CIDR（`/8` 至 `/29`），并避开网关、网络地址和广播地址。 |
| `network.nat`、`dns`、`egress_interface` | 私网用 `nat: false`、`dns: []`。需要出站时启用 NAT，配置客户机可达的 IPv4 DNS，且宿主需开启 IPv4 转发。自动检测不到唯一出口时才需指定实际网卡。 |
| `access.public_key`、`access.probe` | `seed-v1` 支持公钥注入。`external` 由镜像自己初始化，省略公钥字段。探针可选 `ssh_banner`、`tcp` 或 `none`，`port` 必填。`seed-v1` 使用 SSH banner 时端口须与镜像匹配。 |
| `port_forwards[]` | 每条填写 `protocol`（`tcp`/`udp`）、`bind_address`、`host_port`、目标 `node` 和 `guest_port`。宿主本机使用 `127.0.0.1`。对外开放使用宿主持有的具体非回环 IPv4，并核对转发与防火墙策略。 |

从单节点配置派生一个双节点环境。示例为新环境使用独立的自动私网，并清空原来的固定 IP 或端口映射，以免与原环境冲突。运行后可用 `status` 和 `down` 管理 `debian-pair`。

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

[环境 Schema](../schemas/environment-v1.schema.json) · [多节点样例](../examples/environment.json) · [NAT、端口映射和故障排查](networking.zh-CN.md)

## 回收残留资源

宿主或进程意外停止时，先检查 `status` 和日志，再用 `down` 清理已知环境。若资源仍有残留，用 `gc` 生成计划并检查归属、路径和排除项。计划应放在目标环境目录之外，首次使用下例时 `gc-plan.json` 应尚不存在。

```sh
sudo /opt/flintyard/bin/flintyard gc --state-dir /var/lib/flintyard-tutorial \
  --dry-run --plan .local/quickstart/gc-plan.json
sudo cat .local/quickstart/gc-plan.json
```

确认目标和资源后，对同一个状态目录应用该计划。

```sh
sudo /opt/flintyard/bin/flintyard gc --state-dir /var/lib/flintyard-tutorial \
  --apply --plan .local/quickstart/gc-plan.json
```

计划有效期为 10 分钟。计划过期，或宿主、目标状态、资源身份发生变化时，用新的文件名重新生成计划。记录和脚本集成见 [CLI 退出码与事件](cli.zh-CN.md#输出与事件)，实战示例见 [在虚机内运行容器业务](container-workload.zh-CN.md)。
