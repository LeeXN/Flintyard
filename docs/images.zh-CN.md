# 构建、导入和验证 Linux 镜像

[English](images.md)

Flintyard 可搬运的 `.fyimg` 归档包括 manifest、Linux 内核、raw 根盘和可选的 initrd。镜像 ID 标识 manifest，包的 SHA-256 则覆盖整个压缩归档。将镜像发布到可信制品库时，请同时保存这两个值。

## 导入现有镜像

使用安装指南中的 `FY`。将 `IMAGE` 设为镜像包路径，将 `EXPECTED_SHA256` 设为从可信渠道获得的包摘要。 [安装指南](install.zh-CN.md)

```sh
STATE="${STATE:-/var/lib/flintyard}"
sudo "$FY" image import --state-dir "$STATE" --sha256 "$EXPECTED_SHA256" "$IMAGE"
sudo "$FY" image inspect "$IMAGE"
```

导入会检查 manifest 和各文件的完整性，生成只读缓存条目。将显示的镜像 ID 填入节点的 `image`。脚本可使用 `-o json` 读取 `data.image_id`。 [镜像 Schema](../schemas/image-v1.schema.json)

## 组装自有镜像

准备本地导入规格，写入匹配的内核、可选 initrd、raw 根盘及文件摘要。根设备路径应匹配客户机内核，输入的相对路径从规格文件所在目录解析。为输出镜像包指定尚不存在的路径。

```sh
sudo "$FY" image import --state-dir "${STATE:-/var/lib/flintyard}" --spec ./import.json --output ./custom.fyimg
```

可以从 [导入规格样例](../examples/import-spec.json) 开始，用 `sha256sum` 计算实际摘要。采用 `external` 初始化的镜像需要自行配置网络与访问。镜像已包含匹配的首次启动适配程序时，使用 `seed-v1`。

## 构建可复用镜像

将 `RECIPE` 设为 JSON 配方路径。要从 Debian 云镜像开始，按发行版指南生成配方。在与镜像同架构的 Linux 宿主上构建。 [Debian 云镜像](../fixtures/README.md#debian-原版内核离线镜像)

```sh
sudo "$FY" doctor --mode build
sudo "$FY" image build --state-dir "${STATE:-/var/lib/flintyard}" --offline --recipe "$RECIPE" --timeout 20m
```

构建可使用 raw 根盘或根文件系统归档。归档配方应指定 ext4 磁盘大小。raw 根盘保留输入大小，制作时应为客户机负载预留空间。配方中的步骤以 root 身份在构建根目录内执行。执行前请审查配方及其输入。

- **预构建内核**。锁定内核二进制，在根文件系统中提供配套模块。 [样例](../examples/build-prebuilt.json)
- **内核源码**。按摘要锁定源码归档、配置及有序补丁，并使用适合宿主架构的工具链。锁定的 Linux 配方需要 `make`、`gcc`、`ld`、`flex`、`bison`、`bc` 和 `xz`。Flintyard 检查配置保持不变，并记录编译器信息和内核版本。 [源码配方样例](../examples/build-kernel-source.json)
- **Initrd 与传输方式**。使用配套 initrd，或在构建步骤中通过 `generated_initrd_path` 显式生成。默认采用 virtio MMIO。如果内核和 initrd 支持 PCI 驱动，可将 `boot.virtio_transport` 设为 `pci`。

`--offline` 使用预先准备的本地输入或完整缓存。普通构建可下载已锁定的 HTTPS 输入。锁文件和摘要用于核对字节完整性，输入来源的可信度取决于发布者。 [配方 Schema](../schemas/recipe-v1.schema.json) · [发行版镜像配方](../fixtures/README.md)

## 验证启动与访问

`image inspect` 静态检查镜像包。`image validate` 创建临时微虚机，运行配置的 TCP 或 SSH banner 探针，保存与宿主相关的验证记录，然后清理。

```sh
VALIDATE_WORK="$(mktemp -d)"
ssh-keygen -q -t ed25519 -N '' -f "$VALIDATE_WORK/id_ed25519"
sudo "$FY" image validate --state-dir "${STATE:-/var/lib/flintyard}" --timeout 5m \
  --firecracker "$FIRECRACKER" --jailer "$JAILER" \
  --public-key "$VALIDATE_WORK/id_ed25519.pub" "$IMAGE"
rm -- "$VALIDATE_WORK/id_ed25519" "$VALIDATE_WORK/id_ed25519.pub"
rmdir -- "$VALIDATE_WORK"
```

该示例沿用安装指南中的 `FIRECRACKER` 和 `JAILER` 路径，并为 `seed-v1` 镜像生成临时 SSH 公钥。使用 `external` 初始化时，可省略 `--public-key`，并按镜像自身的方式配置访问。SSH banner 通过表示 SSH 服务可达，身份认证仍需使用 SSH 客户端登录。排查启动问题时可用 `--keep-failed` 保留环境。 [运行和 SSH 样例](usage.zh-CN.md)
