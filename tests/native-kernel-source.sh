#!/bin/bash
# Compile-only Linux source acceptance. The rootfs contains data, not an OS.
# No downloads, KVM, guest login, or configuration normalization.
set -euo pipefail
flintyard_test_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$flintyard_test_root"
source scripts/dev-env.sh
if [[ $# != 4 && $# != 5 ]]; then
  echo 'usage: tests/native-kernel-source.sh SOURCE_ARCHIVE SOURCE_SHA256 CONFIG EXPECTED_RELEASE [STATIC_BUSYBOX_FOR_DEPMOD]' >&2
  exit 2
fi
if [[ $(id -u) != 0 ]]; then echo 'BLOCKED: root required for private build namespaces' >&2; exit 77; fi
for tool in go python3 make gcc ld mkfs.ext4 xz; do
  if ! command -v "$tool" >/dev/null; then echo "BLOCKED: missing $tool" >&2; exit 77; fi
done
flintyard_kernel_run="$(mktemp -d "$flintyard_test_root/.local/kernel-native-XXXXXXXX")"
python3 - "$flintyard_kernel_run" "$@" <<'PY'
import hashlib, io, json, platform, re, struct, sys, tarfile
from pathlib import Path
run, source, expected, config, release = sys.argv[1:6]
run, source, config = Path(run), Path(source).resolve(strict=True), Path(config).resolve(strict=True)
def digest(path):
    h = hashlib.sha256()
    with path.open('rb') as f:
        for b in iter(lambda: f.read(1024*1024), b''): h.update(b)
    return h.hexdigest()
if not re.fullmatch('[0-9a-f]{64}', expected) or digest(source) != expected:
    raise SystemExit('source SHA-256 mismatch')
modules = 'CONFIG_MODULES=y' in config.read_text().splitlines()
busybox = None
if modules:
    if len(sys.argv) != 7: raise SystemExit('CONFIG_MODULES=y requires explicit static BusyBox with depmod')
    busybox = Path(sys.argv[6]).resolve(strict=True)
    b = busybox.read_bytes()
    if b[:6] != b'\x7fELF\x02\x01': raise SystemExit('BusyBox must be a native 64-bit little-endian ELF')
    offset, = struct.unpack_from('<Q', b, 32)
    size, count = struct.unpack_from('<HH', b, 54)
    for i in range(count):
        kind, = struct.unpack_from('<I', b, offset + size*i)
        if kind == 3: raise SystemExit('BusyBox must be static; guest host libraries are not copied implicitly')
arch = {'x86_64': 'x86_64', 'aarch64': 'aarch64'}.get(platform.machine())
if arch is None: raise SystemExit('unsupported native architecture')
root = run / 'root.tar'
with tarfile.open(root, 'w') as archive:
    b = b'compile-only data root; no Linux userspace or init\n'
    info = tarfile.TarInfo('compile-only.txt'); info.size = len(b); info.mode = 0o644
    archive.addfile(info, io.BytesIO(b))
    if busybox:
        b = busybox.read_bytes()
        info = tarfile.TarInfo('bin/busybox'); info.size = len(b); info.mode = 0o755
        archive.addfile(info, io.BytesIO(b))
        for name in ('bin/sh', 'sbin/depmod'):
            info = tarfile.TarInfo(name); info.type = tarfile.LNKTYPE; info.linkname = 'bin/busybox'; info.mode = 0o755
            archive.addfile(info)
recipe = {
 'api_version':'flintyard/v1alpha1','kind':'BuildRecipe','architecture':arch,
 'os':{'family':'compile-only','release':'no-userspace'},
 'rootfs':{'kind':'archive','source':{'path':str(root),'sha256':digest(root)},'size_mib':64},
 'kernel':{'mode':'source','source':{'path':str(source),'sha256':expected},'strip_components':1,
 'config_file':str(config),'config_sha256':digest(config),'patches':[],
 'image_target':'vmlinux' if arch=='x86_64' else 'Image','jobs':8,'timeout_seconds':1800,
 'origin':'upstream-source','expected_release':release},
 'boot':{'root_device':'/dev/vda','kernel_args':['console=ttyS0'] + (['keep_bootcon'] if arch=='aarch64' else [])},
 'steps':[],'guest_prepare':{'mode':'external','username':'root','probe':{'type':'none','port':22}},
 'requires':['make','gcc','ld','flex','bison','bc','xz'],'network':False,'reproducibility':'inputs_locked',
 'output':{'name':'linux-compile-only','version':release,'path':str(run/'linux.fyimg')}
}
(run/'recipe.json').write_text(json.dumps(recipe,indent=2)+'\n')
if busybox:
    (run/'busybox-input.json').write_text(json.dumps({'path':str(busybox),'sha256':digest(busybox)})+'\n')
print('compile-only evidence directory:', run, file=sys.stderr)
PY
CGO_ENABLED=0 go build -o "$flintyard_kernel_run/flintyard" ./cmd/flintyard
set +e
"$flintyard_kernel_run/flintyard" image build --state-dir "$flintyard_kernel_run/state" --recipe "$flintyard_kernel_run/recipe.json" --offline --timeout 35m --json > "$flintyard_kernel_run/result.json"
flintyard_kernel_status=$?
set -e
cat "$flintyard_kernel_run/result.json"
echo "evidence: $flintyard_kernel_run; VM validation not run" >&2
exit "$flintyard_kernel_status"
