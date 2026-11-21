#!/bin/bash
# Run inside a disposable Debian microVM through the external acceptance test.
set -euo pipefail
[[ $# == 6 && $EUID == 0 ]]
platform=$1
image=$2
address=$3
public_port=$4
k3s_version=$5
k3s_sha=$6
[[ $image =~ ^docker\.io/gitea/gitea@sha256:[0-9a-f]{64}$ ]]
[[ $address =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ && $public_port =~ ^[0-9]{4,5}$ ]]
[[ $k3s_version =~ ^v[0-9.]+\+k3s[0-9]+$ && $k3s_sha =~ ^[0-9a-f]{64}$ ]]
export DEBIAN_FRONTEND=noninteractive

apt-get -qq -o Acquire::Retries=2 update
root_url="http://127.0.0.1:$public_port/"
case $platform in
  docker)
    apt-get -qy --no-install-recommends install docker.io docker-cli curl jq
    systemctl enable --now docker
    printf 'Docker Engine '; docker version --format '{{.Server.Version}}'
    docker run -d --name gitea --publish 3000:3000 \
      -e GITEA__database__DB_TYPE=sqlite3 \
      -e GITEA__database__PATH=/var/lib/gitea/data/gitea.db \
      -e GITEA__security__INSTALL_LOCK=true \
      -e GITEA__service__DISABLE_REGISTRATION=true \
      -e GITEA__server__DISABLE_SSH=true \
      -e "GITEA__server__ROOT_URL=$root_url" "$image"
    docker ps --filter name=gitea --format 'table {{.Names}}\t{{.Status}}\t{{.Ports}}'
    container=(docker exec gitea)
    port=3000
    ;;
  k3s)
    apt-get -qy --no-install-recommends install ca-certificates curl jq iptables
    case $(uname -m) in
      x86_64) asset=k3s ;;
      aarch64) asset=k3s-arm64 ;;
      *) exit 2 ;;
    esac
    download=$(mktemp)
    trap 'rm -- "$download"' EXIT
    curl --fail --show-error --silent --location --retry 2 --max-time 360 \
      --output "$download" "https://github.com/k3s-io/k3s/releases/download/${k3s_version/+/%2B}/$asset"
    printf '%s  %s\n' "$k3s_sha" "$download" | sha256sum -c
    install -m 0755 "$download" /usr/local/bin/k3s
    k3s --version | sed -n '1p'
    cat > /etc/systemd/system/flintyard-demo-k3s.service <<'UNIT'
[Unit]
Description=K3s for Flintyard container acceptance
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
ExecStart=/usr/local/bin/k3s server --flannel-backend=host-gw --disable=traefik --disable=servicelb --disable=local-storage --disable=metrics-server --disable=coredns
KillMode=process
Delegate=yes
TimeoutStartSec=300

[Install]
WantedBy=multi-user.target
UNIT
    systemctl daemon-reload
    if ! systemctl enable --now flintyard-demo-k3s; then
      journalctl -u flintyard-demo-k3s -n 60 --no-pager >&2
      exit 1
    fi
    registered=false
    for ((attempt = 0; attempt < 120; attempt++)); do
      if k3s kubectl get nodes -o name 2>/dev/null | grep -q '^node/'; then
        registered=true
        break
      fi
      sleep 2
    done
    if ! "$registered" || ! k3s kubectl wait --for=condition=Ready node --all --timeout=4m; then
      journalctl -u flintyard-demo-k3s -n 60 --no-pager >&2
      exit 1
    fi
    k3s kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: gitea
spec:
  replicas: 1
  selector:
    matchLabels: {app: gitea}
  template:
    metadata:
      labels: {app: gitea}
    spec:
      containers:
        - name: gitea
          image: $image
          ports: [{containerPort: 3000}]
          env:
            - {name: GITEA__database__DB_TYPE, value: sqlite3}
            - {name: GITEA__database__PATH, value: /var/lib/gitea/data/gitea.db}
            - {name: GITEA__security__INSTALL_LOCK, value: "true"}
            - {name: GITEA__service__DISABLE_REGISTRATION, value: "true"}
            - {name: GITEA__server__DISABLE_SSH, value: "true"}
            - {name: GITEA__server__ROOT_URL, value: "$root_url"}
          readinessProbe:
            httpGet: {path: /api/v1/version, port: 3000}
---
apiVersion: v1
kind: Service
metadata:
  name: gitea
spec:
  type: NodePort
  selector: {app: gitea}
  ports: [{port: 3000, targetPort: 3000, nodePort: 30300}]
EOF
    if ! k3s kubectl rollout status deployment/gitea --timeout=6m; then
      k3s kubectl describe pods -l app=gitea >&2
      exit 1
    fi
    k3s kubectl get nodes,pods,services
    container=(k3s kubectl exec deployment/gitea --)
    port=30300
    ;;
  *) exit 2 ;;
esac

url="http://$address:$port"
for ((attempt = 0; attempt < 60; attempt++)); do
  if version=$(curl -fsS --max-time 3 "$url/api/v1/version" 2>/dev/null); then break; fi
  sleep 2
done
printf '%s\n' "${version:-}" | jq -ce 'select(.version == "1.26.4")'

password=$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')
"${container[@]}" gitea --config /etc/gitea/app.ini admin user create \
  --username flintyard --password "$password" --email demo@example.invalid --admin --must-change-password=false
curl -fsS --max-time 30 --user "flintyard:$password" -H 'Content-Type: application/json' \
  --data '{"name":"container-demo","auto_init":true,"description":"Gitea repository created in a Flintyard microVM"}' \
  "$url/api/v1/user/repos" | jq -ce 'select(.full_name == "flintyard/container-demo") | {full_name, default_branch}'
