#!/bin/bash
# Install as /opt/flintyard-demo/run-stage, owned and writable only by root.
set -euo pipefail
[[ $EUID == 0 && $# == 2 ]]
stage=$1
pipeline=$2
case $stage in build|start|deploy|verify|cleanup) ;; *) exit 2 ;; esac
[[ $pipeline =~ ^[1-9][0-9]{0,18}$ ]]
export PATH=/usr/sbin:/usr/bin:/sbin:/bin
umask 077
install=/opt/flintyard-demo
data=/var/lib/flintyard-demo
runs="$data/ci-runs"
run="$runs/workload.$pipeline"
if [[ $stage == build ]]; then
  install -d -m 0700 -- "$runs" "$run"
elif [[ $stage == cleanup && ! -e $run ]]; then
  echo "No report directory for pipeline $pipeline."
  exit 0
fi
IFS= read -r dns < "$install/dns-ipv4"
exec /bin/bash "$install/tests/container-workload.sh" --stage "$stage" \
  "$install/bin/flintyard" "$install/bin/firecracker" "$install/bin/jailer" "$data/inputs" "$run" "$dns"
