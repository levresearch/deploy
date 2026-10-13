#!/usr/bin/env bash
set -euo pipefail
here=$(cd -- "$(dirname -- "$0")" && pwd)
output=${DEPLOY_NETWORK_OUTPUT:?set an absolute directory for non-secret results}
[[ $output = /* ]] || { echo 'results directory must be absolute' >&2; exit 2; }
test "$(uname -m)" = x86_64
test -c /dev/kvm
command -v node >/dev/null
command -v go >/dev/null
docker info >/dev/null
mkdir -p "$output"
export DEPLOY_NETWORK_LAB
DEPLOY_NETWORK_LAB=$(mktemp -d "$output/.lab.XXXXXXXX")
run_id=${DEPLOY_NETWORK_LAB##*.}
export DEPLOY_NETWORK_CONTAINER="deploy-network-${run_id,,}"
export DEPLOY_NETWORK_IMAGE="$DEPLOY_NETWORK_CONTAINER:tools"

cleanup() {
  result=$?
  trap - EXIT INT TERM
  if ! docker info >/dev/null 2>&1; then
    echo "cleanup blocked: Docker unavailable; retain $DEPLOY_NETWORK_LAB and remove $DEPLOY_NETWORK_CONTAINER when available" >&2
    exit 1
  fi
  if docker container inspect "$DEPLOY_NETWORK_CONTAINER" >/dev/null 2>&1; then
    if ! bash "$here/lab.sh" down; then
      echo "cleanup blocked: container remains; retain $DEPLOY_NETWORK_LAB" >&2
      exit 1
    fi
  fi
  if docker image inspect "$DEPLOY_NETWORK_IMAGE" >/dev/null 2>&1; then
    docker image rm "$DEPLOY_NETWORK_IMAGE" >/dev/null || result=1
  fi
  rm -rf -- "$DEPLOY_NETWORK_LAB"
  echo 'CLEANUP removed disposable container, image tag, VM disks and keys'
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

CGO_ENABLED=0 go -C "$here/../.." build -o "$DEPLOY_NETWORK_LAB/deploy" . > "$output/build.log" 2>&1
docker build -t "$DEPLOY_NETWORK_IMAGE" "$here" >> "$output/build.log" 2>&1
bash "$here/lab.sh" up > "$output/boot.log" 2>&1
for phase in ${DEPLOY_NETWORK_PHASES:-setup network lifecycle locks failures helper reconcile acceptance}; do
  echo "RUN $phase"
  node "$here/$phase.mjs" 2>&1 | tee "$output/$phase.log"
done
echo 'PASS all network experiments'
