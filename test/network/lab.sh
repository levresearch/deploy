#!/usr/bin/env bash
set -euo pipefail
lab=${DEPLOY_NETWORK_LAB:?set the disposable lab directory}
container=${DEPLOY_NETWORK_CONTAINER:-deploy-network-experiment}
image=${DEPLOY_NETWORK_IMAGE:?set the disposable tools image}
here=$(cd -- "$(dirname -- "$0")" && pwd)

case ${1:-} in
up)
  mkdir -p "$lab"
  chmod 700 "$lab"
  cp "$here/docker.service" "$lab/"
  docker run -d --name "$container" --label deploy.network-experiment=true \
    --user "$(id -u):$(id -g)" --group-add "$(stat -c %g /dev/kvm)" \
    --device /dev/kvm --cap-drop ALL --security-opt no-new-privileges \
    --memory 2300m --memory-swap 3g \
    -v "$lab:/lab" -v "$here:/fixture:ro" "$image" >/dev/null
  docker exec "$container" bash /fixture/boot.sh
  ;;
ssh)
  guest=${2:?guest a or b}
  shift 2
  case $guest in a) port=22101 ;; b) port=22102 ;; *) exit 2 ;; esac
  docker exec -i "$container" ssh -p "$port" -i /lab/id_ed25519 \
    -o BatchMode=yes -o ConnectTimeout=3 -o StrictHostKeyChecking=accept-new \
    -o UserKnownHostsFile=/lab/known_hosts root@127.0.0.1 "$@"
  ;;
down)
  test "$(docker inspect -f '{{index .Config.Labels "deploy.network-experiment"}}' "$container")" = true
  docker rm -f "$container"
  ;;
*) echo 'usage: lab.sh up|ssh a|b command...|down' >&2; exit 2 ;;
esac
