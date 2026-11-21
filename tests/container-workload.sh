#!/bin/bash
# Build a Debian image and exercise two container platforms in disposable microVMs.
set -euo pipefail
stage=all
if [[ ${1:-} == --stage && $# == 8 ]]; then
  stage=$2
  shift 2
  case $stage in build|start|deploy|verify|cleanup) ;; *) exit 2 ;; esac
fi
if [[ $# != 6 || $EUID != 0 ]]; then
  echo "usage: sudo bash $0 [--stage STAGE] FLINTYARD FIRECRACKER JAILER DEBIAN_INPUTS REPORT_DIR DNS_IPV4" >&2
  exit 2
fi
script_dir=$(cd -- "$(dirname -- "$0")" && pwd)
fy=$(realpath -- "$1")
firecracker=$(realpath -- "$2")
jailer=$(realpath -- "$3")
inputs=$(realpath -- "$4")
parent=$(realpath -- "$5")
dns=$6
[[ $dns =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ && -d $parent ]]
docker_port=${FY_DEMO_DOCKER_PORT:-13030}
k3s_port=${FY_DEMO_K3S_PORT:-13031}
hold_seconds=${FY_DEMO_HOLD_SECONDS:-0}
[[ $docker_port =~ ^[1-9][0-9]{3,4}$ && $k3s_port =~ ^[1-9][0-9]{3,4}$ ]]
((docker_port <= 65535 && k3s_port <= 65535 && docker_port != k3s_port))
[[ $hold_seconds =~ ^(0|[1-9][0-9]{0,2})$ ]] && ((hold_seconds <= 300))

architecture=$(uname -m)
k3s_version=v1.37.2+k3s1
case $architecture in
  x86_64) k3s_sha=265b3ee6a3198ef14033c63ef46cbb32370d83795199fb6d1b01f21575eddbda ;;
  aarch64) k3s_sha=a779ccf1e5e2e33a62f5a29c2f1c0bbf99431e100579d4c10f0aa0742a6aa84b ;;
  *) echo "Unsupported host architecture: $architecture" >&2; exit 2 ;;
esac
image=docker.io/gitea/gitea@sha256:cd1d2614b403fc9b085fa52ceb4424dde9c4dcf5da8e3263abb27955562070c4
umask 077
if [[ $stage == all ]]; then
  report=$(mktemp -d -- "$parent/workload.XXXXXXXX")
else
  report=$parent
  [[ $(stat -c '%u:%a' -- "$report") == 0:700 && ${report##*/} == workload.* ]]
fi
suffix=${report##*.}
[[ $suffix =~ ^[a-zA-Z0-9]{1,20}$ ]]
exec 9>"$report/stage.lock"
flock -n 9
environment="container-demo-${suffix,,}"
state="$report/state"
key="$report/guest-key"
config="$report/environment.json"
run_fy() { "$fy" "$1" --state-dir "$state" -o json "${@:2}" 9>&-; }

destroy() {
  local port failed=0
  if [[ -f $report/started ]]; then
    printf 'Destroy %s\n' "$environment"
    if run_fy down --timeout 3m "$environment" > "$report/down.json" &&
       jq -er 'select(.ok and .data.state == "deleted" and (.data.cleanup_errors | length == 0)) |
         "Environment \(.data.environment_id // "already absent"): \(.data.state)"' "$report/down.json"; then
      rm -- "$report/started"
      if [[ -f $report/verified ]]; then
        for port in "$docker_port" "$k3s_port"; do
          if curl --noproxy '*' -s --connect-timeout 2 --max-time 4 -o /dev/null "http://127.0.0.1:$port/"; then
            printf 'Unexpected HTTP endpoint after down at port %s\n' "$port" >&2
            failed=1
          else
            printf 'HTTP endpoint closed after down: 127.0.0.1:%s\n' "$port"
          fi
        done
      fi
    else
      cat "$report/down.json" >&2
      failed=1
    fi
  else
    printf 'No VM startup to clean up.\n'
  fi
  rm -f -- "$key" "$key.pub"
  return "$failed"
}

finish() {
  local rc=$? node
  trap - EXIT
  if ((rc != 0)) && [[ -f $report/started && $stage != cleanup ]]; then
    for node in docker k3s; do
      run_fy logs --timeout 20s --node "$node" --source serial --tail 120 "$environment" > "$report/$node-serial.json" || true
    done
  fi
  if [[ $stage == all || ( $stage != cleanup && $rc != 0 ) ]]; then
    destroy || rc=1
  fi
  printf 'Evidence: %s\n' "$report"
  exit "$rc"
}
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

build_image() {
  printf 'Build a Debian image offline (%s)\n' "$architecture"
  python3 "$script_dir/../fixtures/render-debian-cloud.py" "$architecture" --input-dir "$inputs" \
    --output "$report/debian-containers.fyimg" --recipe "$report/debian-base.recipe.json"
  # Authorize the ephemeral test user to install platforms inside its disposable VM.
  jq --arg step 'printf "%s\n" "devops ALL=(root) NOPASSWD: ALL" > /etc/sudoers.d/flintyard-container-demo; chmod 0440 /etc/sudoers.d/flintyard-container-demo' \
    '.steps += [{argv:["/bin/sh","-ec",$step],working_dir:"/",timeout_seconds:60}] | .output.name="debian-container-demo"' \
    "$report/debian-base.recipe.json" > "$report/debian.recipe.json"
  "$fy" image build --offline --timeout "${FY_DEMO_BUILD_TIMEOUT:-40m}" -o json --state-dir "$state" \
    --recipe "$report/debian.recipe.json" > "$report/build.json" 9>&- || { cat "$report/build.json" >&2; exit 1; }
  jq -er 'select(.ok) | "Image ID: \(.data.image_id)\nPackage SHA-256: \(.data.package_sha256)\nKernel: \(.data.manifest.kernel.release)\nCache hit: \(.data.cache_hit)"' "$report/build.json"
}

start_vms() {
  local image_id
  printf 'Start two Firecracker microVMs and check access\n'
  [[ ! -e $report/started && ! -e $report/verified ]]
  image_id=$(jq -er '.data.image_id' "$report/build.json")

  ssh-keygen -qt ed25519 -N '' -f "$key"
  jq -n --arg arch "$architecture" --arg name "$environment" --arg key "$key.pub" --arg image "$image_id" \
    --arg dns "$dns" --argjson docker_port "$docker_port" --argjson k3s_port "$k3s_port" '
    {api_version:"flintyard/v1alpha1",kind:"Environment",name:$name,architecture:$arch,
     network:{cidr:"auto",nat:true,dns:[$dns]},
     nodes:[{name:"docker",image:$image,vcpu_count:2,memory_mib:1024},
            {name:"k3s",image:$image,vcpu_count:2,memory_mib:2304}],
     access:{public_key:$key,probe:{type:"ssh_banner",port:22}},
     port_forwards:[{node:"docker",protocol:"tcp",bind_address:"127.0.0.1",host_port:$docker_port,guest_port:3000},
                    {node:"k3s",protocol:"tcp",bind_address:"127.0.0.1",host_port:$k3s_port,guest_port:30300}]}' > "$config"
  touch "$report/started"
  run_fy up --timeout "${FY_DEMO_UP_TIMEOUT:-15m}" --file "$config" --firecracker "$firecracker" --jailer "$jailer" \
    --events "$report/events.jsonl" > "$report/up.json" || { cat "$report/up.json" >&2; exit 1; }
  run_fy status --probe "$environment" > "$report/status.json" || { cat "$report/status.json" >&2; exit 1; }
  jq -er 'select(.ok and .data.state == "ready" and (.data.nodes|length) == 2 and all(.data.nodes[]; .access.probe_passed)) |
    "Environment \(.data.environment_id): ready",
    (.data.nodes[] | "  \(.name)  \(.ipv4)  Firecracker \(.firecracker_version)")' "$report/status.json"
}

deploy() {
  local platform=$1 port=$2 address
  address=$(jq -er --arg platform "$platform" '.data.nodes[] | select(.name==$platform) | .access.host' "$report/status.json")
  [[ $address =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]]
  ssh -o BatchMode=yes -o ConnectTimeout=10 -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new \
    -o "UserKnownHostsFile=$report/$platform.known_hosts" -i "$key" "devops@$address" \
    sudo -n /bin/bash -s -- "$platform" "$image" "$address" "$port" "$k3s_version" "$k3s_sha" \
    < "$script_dir/container-workload-guest.sh" 2>&1 | tee "$report/$platform.log" | sed -u "s/^/[$platform] /"
}
deploy_workload() {
  local docker_pid k3s_pid result=0
  [[ -f $report/started ]]
  printf 'Deploy Gitea on Docker and K3s\n'
  deploy docker "$docker_port" &
  docker_pid=$!
  deploy k3s "$k3s_port" &
  k3s_pid=$!
  wait "$docker_pid" || result=1
  wait "$k3s_pid" || result=1
  ((result == 0))
}

verify_workload() {
  local port endpoint elapsed
  [[ -f $report/started ]]
  for port in "$docker_port" "$k3s_port"; do
    endpoint="http://127.0.0.1:$port"
    printf 'Verify Gitea at %s\n' "$endpoint"
    curl --noproxy '*' -fsS --max-time 15 "$endpoint/api/v1/repos/flintyard/container-demo" |
      jq -ce 'select(.full_name == "flintyard/container-demo") | {full_name,default_branch,clone_url}' | tee "$report/http-$port.json"
    timeout 25s git -c http.proxy= ls-remote --exit-code "$endpoint/flintyard/container-demo.git" HEAD | tee "$report/git-$port.log"
  done
  touch "$report/verified"
  printf 'Both repositories passed HTTP and Git checks.\n'
  if ((hold_seconds > 0)); then
    printf 'Waiting up to %s seconds for screenshots. Evidence: %s\n' "$hold_seconds" "$report"
    for ((elapsed = 0; elapsed < hold_seconds; elapsed++)); do
      [[ ! -f $report/captured ]] || break
      sleep 1
    done
  fi
}

case $stage in
  all)
    build_image
    start_vms
    deploy_workload
    verify_workload
    ;;
  build) build_image ;;
  start) start_vms ;;
  deploy) deploy_workload ;;
  verify) verify_workload ;;
  cleanup) destroy ;;
esac
