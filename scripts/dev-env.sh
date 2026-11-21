# Source from Bash: source scripts/dev-env.sh
flintyard_dev_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ -x /workspace/toolchains/go/bin/go ]]; then
  export PATH="/workspace/toolchains/go/bin:$PATH"
fi
# Keep writable scratch inside this checkout. In particular root's state-path
# tests intentionally reject /tmp as an ancestor controlled by other users.
mkdir -p "$flintyard_dev_root/.local/tmp" "$flintyard_dev_root/.local/go-build"
export GOCACHE="${GOCACHE:-$flintyard_dev_root/.local/go-build}"
export TMPDIR="${TMPDIR:-$flintyard_dev_root/.local/tmp}"
export GOTOOLCHAIN=local
unset flintyard_dev_root
