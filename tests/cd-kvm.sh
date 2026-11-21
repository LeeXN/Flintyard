#!/bin/bash
# External CD acceptance. The caller supplies a trusted, pinned image digest.
set -euo pipefail
umask 077
if [[ $# != 6 || $EUID != 0 || ! $6 =~ ^[0-9a-f]{64}$ ]]; then
  echo 'usage (root): cd-kvm.sh REPORT_PARENT FLINTYARD FIRECRACKER JAILER IMAGE SHA256' >&2
  exit 2
fi
report=$(mktemp -d -- "$1/cd-kvm-XXXXXXXX")
bin=$2
firecracker=$3
jailer=$4
image=$5
image_sha=$6
state="$report/state"
config="$report/environment.json"
env=cd-validation
script_dir=$(cd -- "$(dirname -- "$0")" && pwd -P)
started=false

fy() {
  if [[ $1 == image ]]; then
    "$bin" "$1" "$2" --json --state-dir "$state" "${@:3}"
  else
    "$bin" "$1" --json --state-dir "$state" "${@:2}"
  fi
}

cleanup() {
  local rc=$?
  trap - EXIT
  if "$started"; then
    if fy down --timeout 120s "$env" > "$report/down.json" &&
       jq -e '.ok and .data.state == "deleted"' "$report/down.json" > /dev/null; then
      jq -c '{state:.data.state,environment_id:.data.environment_id}' "$report/down.json"
    else
      cat "$report/down.json" >&2
      rc=1
    fi
  fi
  rm -f -- "$report/key" "$report/key.pub"
  printf 'evidence=%s\n' "$report"
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

fy doctor --mode run --firecracker "$firecracker" --jailer "$jailer" > "$report/doctor.json"
fy image import --timeout 15m --sha256 "$image_sha" "$image" > "$report/import.json"
image_id=$(jq -er '.data.image_id // .data.id' "$report/import.json")
architecture=$(jq -er '.data.architecture' "$report/doctor.json")
ssh-keygen -q -t ed25519 -N '' -f "$report/key"
jq -n --arg name "$env" --arg arch "$architecture" --arg image "$image_id" --arg key "$report/key.pub" '
  {api_version:"flintyard/v1alpha1",kind:"Environment",name:$name,architecture:$arch,
   network:{cidr:"auto",nat:false,dns:[]},
   nodes:[range(1;4)|{name:("node-"+tostring),image:$image,vcpu_count:1,memory_mib:512}],
   access:{public_key:$key,probe:{type:"ssh_banner",port:22}},port_forwards:[]}' > "$config"

started=true
fy up --timeout 5m --file "$config" --firecracker "$firecracker" --jailer "$jailer" --events "$report/events.jsonl" > "$report/up.json"
fy status --probe "$env" > "$report/status.json"
jq -e '.ok and .data.state == "ready" and (.data.nodes|length == 3) and
       all(.data.nodes[]; .access.probe_passed and (.access.authenticated == false))' "$report/status.json" > /dev/null
profile=$(jq -er '.data.manifest.initialization.profile' "$report/import.json")
while IFS=$'\t' read -r name host; do
  bash "$script_dir/kvm-guest-check.sh" "$host" "$report/key" "$report/$name.known-hosts" "$profile" > "$report/$name.ssh.log"
done < <(jq -r '.data.nodes[] | [.name, .access.host] | @tsv' "$report/status.json")
test -s "$report/node-1.ssh.log" && test -s "$report/node-2.ssh.log" && test -s "$report/node-3.ssh.log"
for field in machine_id guest_host_key; do
  [[ $(sed -n "s/^$field=//p" "$report"/node-*.ssh.log | sort -u | wc -l) == 3 ]]
done
fy logs --node node-1 --source serial --tail 80 "$env" > "$report/serial.json"
jq -e '.ok and (.data.content|length > 0)' "$report/serial.json" > /dev/null
jq -c '{state:.data.state,environment_id:.data.environment_id,nodes:[.data.nodes[]|{name,host:.access.host,probe:.access.probe}]}' "$report/status.json"
