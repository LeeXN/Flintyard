# 制作、导入和验证 Linux 镜像

[English](images.md) · [从构建到启动的完整教程](usage.zh-CN.md)

从仓库根目录运行以下示例。使用安装到 `/opt/flintyard/bin/flintyard` 的 CLI，示例缓存位于 `/var/lib/flintyard-tutorial`。状态目录中的镜像 ID 可用于同一状态目录下的环境。 [安装步骤](usage.zh-CN.md#1-准备宿主并构建二进制)

| 已有材料 | 使用方式 | 产物 |
| --- | --- | --- |
| 已发布的 `.fyimg` 和可信摘要 | `image inspect`、`image import PACKAGE` | 校验结果及本地缓存 |
| 已能配套启动的内核、raw 根盘及可选 initrd | `image import --spec FILE --output PACKAGE` | 新 `.fyimg` 和本地缓存 |
| 根文件系统材料，还需要制作根盘、准备客户机或编译内核 | `image build --recipe FILE` | 新 `.fyimg`、本地缓存及构建记录 |
| 想验证镜像在目标宿主上能启动 | `image validate IMAGE` | 目标宿主上的临时启动及探针记录 |

镜像 ID 对应 manifest，格式为 `sha256:…`。包 SHA-256 覆盖整个压缩包。分发时保留两者，并通过独立可信渠道提供包摘要。

## 导入现有镜像

将 `.fyimg` 放到 `.local/quickstart/image.fyimg`，将发布方提供的整包摘要保存为 `.local/quickstart/image.sha256`，只保留 64 位十六进制值。以下命令从固定文件读入值，可以在新 shell 执行。

```sh
sudo /opt/flintyard/bin/flintyard image inspect .local/quickstart/image.fyimg
sudo /opt/flintyard/bin/flintyard image import --state-dir /var/lib/flintyard-tutorial \
  --sha256 "$(cat .local/quickstart/image.sha256)" -o json .local/quickstart/image.fyimg \
  > .local/quickstart/import.json
jq -er 'select(.ok) | .data.image_id' .local/quickstart/import.json
```

`image inspect` 也接受已缓存的镜像 ID。查看缓存时，传入导入镜像时相同的 `--state-dir`。`up` 的节点字段 `image` 接受该 ID 或本地包文件，使用 ID 可避免在配置中写入镜像文件的存放位置。 [配置和启动](usage.zh-CN.md#5-设置虚机并启动)

## 组装自有镜像

准备与目标宿主匹配的内核、包含客户机系统的 raw 根盘和内核需要的 initrd。根盘必须已可用，组包不会调整客户机或安装启动适配程序。

1. 创建 `.local/custom-image/inputs/`，放入 `kernel`、`rootfs.raw`，需要时放入 `initrd`。将 [导入规格样例](../examples/import-spec.json) 复制为 `.local/custom-image/import.json`。
2. 按实际镜像修改 `name`、`version`、`architecture`、`os`、`kernel.release`、`kernel.origin` 和 `kernel.modules`。使用 `sha256sum` 计算 `inputs` 中每个文件的摘要。将每个 `inputs[].path` 改为相对规格文件的路径，例如 `inputs/kernel`。若没有配置文件摘要，移除样例中的 `kernel.config_sha256`。
3. 核对 `boot.root_device` 对应客户机内的根设备。`boot.kernel_path`、`rootfs_path` 以及可选的 `initrd_path` 使用包内的 `payload/…` 路径。添加 initrd 时，同时在 `inputs` 添加 `role: "initrd"` 和实际摘要。
4. 已准备 Flintyard 首次启动适配程序的根盘用 `seed-v1`，填入实际的 `profile`、`username` 和探针。由镜像自带的程序处理网络和访问时用 `external`，删除 `initialization.profile`，并在环境配置中省略 `access.public_key`。

编辑完成后执行组装和导入，输出路径必须尚不存在。

```sh
sudo /opt/flintyard/bin/flintyard image import --state-dir /var/lib/flintyard-tutorial \
  --spec .local/custom-image/import.json --output .local/custom-image/custom.fyimg -o json
```

[导入规格 Schema](../schemas/import-v1.schema.json) · [镜像 manifest Schema](../schemas/image-v1.schema.json)

## 构建可复用镜像

初次制作选 [Debian 离线构建](usage.zh-CN.md#3-制作-debian-镜像或准备已有镜像)。要定制，从 [预构建内核配方](../examples/build-prebuilt.json) 或 [内核源码配方](../examples/build-kernel-source.json) 开始。把改好的配方保存为 `.local/custom-image/build.recipe.json`。

| 配方字段 | 怎么填 |
| --- | --- |
| `rootfs` | `kind: "raw"` 保留输入盘大小。`kind: "archive"` 需要 `size_mib`，生成 ext4 根盘。 |
| `kernel` | `mode: "prebuilt"` 使用现成内核，根盘需要相应模块。`mode: "source"` 锁定源码归档、配置和有序补丁，使用同架构编译工具。Flintyard 记录实际内核版本和构建信息。 |
| `boot`、`initrd` | 设置根设备和内核参数。可提供配套 initrd 或通过 `generated_initrd_path` 声明构建步骤生成的 initrd。默认 virtio MMIO，只有内核和 initrd 支持时才选 `boot.virtio_transport: "pci"`。 |
| `steps`、`requires`、`guest_prepare` | `steps` 在构建根目录内以 root 执行，运行前审查配方和输入。将额外宿主工具写入 `requires`，选择与客户机 init 系统匹配的准备模式和访问探针。 |
| 输入与输出 | 输入使用配方目录下的相对路径、绝对路径或带固定摘要的 HTTPS URL。`output.path` 使用尚未存在的输出文件。配方的相对输出路径也以配方目录为基准。 |

以下用于输入已准备好、构建步骤不需要联网的配方。`--offline` 禁用构建网络。需要下载已锁定输入或运行联网的配方时，按配方启用 `network` 并省略 `--offline`。预构建内核样例含联网的 `apt-get` 步骤，直接照搬该样例不能离线构建。

```sh
sudo /opt/flintyard/bin/flintyard doctor --mode build
sudo /opt/flintyard/bin/flintyard image build --state-dir /var/lib/flintyard-tutorial \
  --recipe .local/custom-image/build.recipe.json --offline --timeout 20m -o json \
  > .local/custom-image/build.json
```

查看 `build.json` 的 `data.image_id`、`data.package_sha256` 和 `data.log_path`。命中构建缓存时，结果的 `data.cache_hit` 为真。源码内核还需要 `make`、`gcc`、`ld`、`flex`、`bison`、`bc`、`xz` 等工具。 [配方 Schema](../schemas/recipe-v1.schema.json) · [发行版镜像配方](../fixtures/README.md)

## 验证启动与访问

`image validate` 使用镜像声明的 TCP 或 SSH banner 探针运行临时虚机，保存验证记录后清理。`probe.type: "none"` 的镜像可静态检查，但无法执行此访问验证。下面以教程镜像为例，从同一个仓库根目录生成临时密钥。更换要验证的镜像时，可以直接传其他包路径，或者传同一状态目录内的镜像 ID。

```sh
(
  set -eu
  tmp=$(mktemp -d)
  trap 'rm -f -- "$tmp/id_ed25519" "$tmp/id_ed25519.pub"; rmdir -- "$tmp"' EXIT
  ssh-keygen -q -t ed25519 -N '' -f "$tmp/id_ed25519"
  sudo /opt/flintyard/bin/flintyard image validate --state-dir /var/lib/flintyard-tutorial \
    --timeout 5m --firecracker /opt/flintyard/bin/firecracker --jailer /opt/flintyard/bin/jailer \
    --public-key "$tmp/id_ed25519.pub" -o json .local/quickstart/image.fyimg
)
```

`seed-v1` 验证需要公钥。`external` 不支持注入，省略 `--public-key`。关注 `data.static_passed`、`boot_passed`、`access_passed`、`cleanup_incomplete` 和 `record_path`。SSH banner 通过表示服务可达，实际认证请用 SSH 登录。`--keep-failed` 可保留失败的临时环境，排查后用结果中的 `environment_name` 执行 `down`。 [生命周期教程](usage.zh-CN.md)
