#!/bin/bash
# deploy-network is the only part of deploy that runs as root. deploy installs it
# at /usr/local/libexec/deploy-network with `deploy network bootstrap`, and a
# sudoers rule then lets the deploy user run exactly its verbs.
#
# It reads one desired network on stdin, validates every field, and renders the
# WireGuard config and firewall rules itself, so nothing deploy sends is ever
# run or written verbatim. Everything it touches is named after the runtime id
# and proven by an owner manifest, never by the name alone. Private keys are
# generated here and never printed.
#
# verbs:
#   version   print the helper version and its own checksum
#   check     validate stdin and print what apply would render; needs no root
#   observe   print the owned network's non-secret state
#   enroll    claim the interface and create its key; print the public key
#   apply     converge the owned network on stdin; no change when it matches
#   remove    take down and delete everything the runtime id owns
#   restore   reload the owned chains at boot (run by systemd, not deploy)
#
# checks are written as `test && test || refuse`, which is the intent here: any
# failing test refuses.
# shellcheck disable=SC2015
set -euo pipefail
umask 077
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export LC_ALL=C

readonly helper_version=1
readonly helper_path=/usr/local/libexec/deploy-network
readonly state_root=/var/lib/deploy-network
readonly config_root=/etc/wireguard
readonly unit_root=/etc/systemd/system

refuse() {
  printf 'refused: %s\n' "$*" >&2
  exit 2
}

# --- input --------------------------------------------------------------------

runtime='' environment='' cidr='' address='' listen=''
peers=() access=()

set_once() {
  local -n field=$1
  [ -z "$field" ] || refuse "$1 given twice"
  field=$2
}

input=''

# read_desired takes at most 64 KiB, keeping the text for the applied digest.
read_desired() {
  local line key value count=0
  input=$(head -c 65537)
  [ "${#input}" -le 65536 ] || refuse 'input is too large'
  while IFS= read -r line; do
    count=$((count + 1))
    [ "$count" -le 512 ] || refuse 'input has too many lines'
    [[ $line =~ ^[a-z]+( [!-~]+)+$ ]] || refuse "line $count is malformed"
    key=${line%% *}
    value=${line#* }
    case $key in
    runtime | environment | cidr | address | listen) set_once "$key" "$value" ;;
    peer) peers+=("$value") ;;
    access) access+=("$value") ;;
    *) refuse "unknown field $key on line $count" ;;
    esac
  done <<<"$input"
}

octet='(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])'
ipv4_pattern="^$octet(\\.$octet){3}\$"

ip_number() {
  local IFS=.
  # shellcheck disable=SC2086
  set -- $1
  echo $((($1 << 24) | ($2 << 16) | ($3 << 8) | $4))
}

valid_port() { [[ $1 =~ ^[1-9][0-9]{0,4}$ ]] && [ "$1" -le 65535 ]; }

network_number=0 network_mask=0 network_bits=0

# usable_address is a host address inside the cidr that is neither its network
# nor its broadcast address.
usable_address() {
  [[ $1 =~ $ipv4_pattern ]] || return 1
  local number
  number=$(ip_number "$1")
  [ $((number & network_mask)) -eq "$network_number" ] &&
    [ "$number" -ne "$network_number" ] &&
    [ "$number" -ne $((network_number | (~network_mask & 0xFFFFFFFF))) ]
}

wireguard_key_pattern='^[A-Za-z0-9+/]{42}[AEIMQUYcgkosw048]=$'
endpoint_pattern='^[a-zA-Z0-9][a-zA-Z0-9._-]{0,252}:[1-9][0-9]{0,4}$'

# validate checks the fields a verb needs. scope is identity (runtime and
# environment), interface (plus cidr, address and listen port) or full (plus
# peers and access).
validate() {
  local scope=$1
  [ -n "$runtime" ] || refuse 'runtime is required'
  [ -n "$environment" ] || refuse 'environment is required'
  [[ $runtime =~ ^[0-9a-f]{8}(-[a-z][a-z0-9]*(-[a-z0-9]+)*)?$ ]] || refuse 'invalid runtime id'
  [[ $environment =~ ^[a-z][a-z0-9]*(-[a-z0-9]+)*$ ]] && [ "${#environment}" -le 32 ] || refuse 'invalid environment'
  if [ "$environment" = production ]; then
    [ "${#runtime}" -eq 8 ] || refuse 'production uses the bare project id as its runtime id'
  else
    [ "$runtime" = "${runtime:0:8}-$environment" ] || refuse 'runtime id does not match the environment'
  fi
  if [ "$scope" = identity ]; then
    [ -z "$cidr$address$listen" ] && [ "${#peers[@]}" -eq 0 ] && [ "${#access[@]}" -eq 0 ] ||
      refuse 'this verb takes only runtime and environment'
    return
  fi

  [[ $cidr =~ ^(.+)/([1-9][0-9]?)$ ]] || refuse 'invalid cidr'
  local base=${BASH_REMATCH[1]}
  network_bits=${BASH_REMATCH[2]}
  [[ $base =~ $ipv4_pattern ]] && [ "$network_bits" -ge 8 ] && [ "$network_bits" -le 30 ] || refuse 'invalid cidr'
  network_number=$(ip_number "$base")
  network_mask=$(((0xFFFFFFFF << (32 - network_bits)) & 0xFFFFFFFF))
  [ $((network_number & network_mask)) -eq "$network_number" ] || refuse 'cidr is not canonical'
  if ! { [ $((network_number >> 24)) -eq 10 ] ||
    { [ $((network_number & 0xFFF00000)) -eq $((0xAC100000)) ] && [ "$network_bits" -ge 12 ]; } ||
    { [ $((network_number & 0xFFFF0000)) -eq $((0xC0A80000)) ] && [ "$network_bits" -ge 16 ]; }; }; then
    refuse 'cidr must be a private IPv4 subnet'
  fi
  usable_address "$address" || refuse 'address must be a usable address in the cidr'
  valid_port "$listen" || refuse 'invalid listen port'
  if [ "$scope" = interface ]; then
    [ "${#peers[@]}" -eq 0 ] && [ "${#access[@]}" -eq 0 ] || refuse 'this verb takes no peers or access'
    return
  fi

  local peer key peer_address endpoint keepalive extra seen_keys=' ' seen_addresses=" $address "
  for peer in "${peers[@]}"; do
    read -r key peer_address endpoint keepalive extra <<<"$peer"
    [ -n "$keepalive" ] && [ -z "$extra" ] || refuse 'peer needs key, address, endpoint and keepalive'
    [[ $key =~ $wireguard_key_pattern ]] || refuse 'invalid peer public key'
    usable_address "$peer_address" || refuse "peer $peer_address is not a usable address in the cidr"
    [[ $seen_keys != *" $key "* ]] || refuse 'peer public key given twice'
    [[ $seen_addresses != *" $peer_address "* ]] || refuse "peer address $peer_address is used twice"
    seen_keys+="$key " seen_addresses+="$peer_address "
    if [ "$endpoint" != - ]; then
      [[ $endpoint =~ $endpoint_pattern ]] && valid_port "${endpoint##*:}" || refuse 'endpoint must be hostname:port or IPv4:port'
    fi
    [[ $keepalive =~ ^(0|[1-9][0-9]{0,4})$ ]] && [ "$keepalive" -le 65535 ] || refuse 'invalid keepalive'
  done

  local rule source port seen_rules=' '
  for rule in "${access[@]}"; do
    read -r source port extra <<<"$rule"
    [ -n "$port" ] && [ -z "$extra" ] || refuse 'access needs source and port'
    [[ $seen_addresses == *" $source "* ]] && [ "$source" != "$address" ] || refuse "access source $source is not a peer"
    valid_port "$port" || refuse 'invalid access port'
    [[ $seen_rules != *" $source:$port "* ]] || refuse 'access rule given twice'
    seen_rules+="$source:$port "
  done
}

# --- rendering ----------------------------------------------------------------
# these match HostNetwork.WireGuardConfig and HostNetwork.FirewallRules, which a
# go test checks through the check verb.

interface_name() {
  local digest
  digest=$(printf '%s' "$runtime" | sha256sum)
  printf 'dw%s' "${digest:0:12}"
}

render_config() {
  local peer key peer_address endpoint keepalive
  printf '[Interface]\nAddress = %s/%d\nListenPort = %d\nSaveConfig = false\nTable = off\n' "$address" "$network_bits" "$listen"
  for peer in "${peers[@]}"; do
    read -r key peer_address endpoint keepalive <<<"$peer"
    printf '\n[Peer]\nPublicKey = %s\nAllowedIPs = %s/32\n' "$key" "$peer_address"
    [ "$endpoint" = - ] || printf 'Endpoint = %s\n' "$endpoint"
    [ "$keepalive" -eq 0 ] || printf 'PersistentKeepalive = %d\n' "$keepalive"
  done
}

render_rules() {
  local name=$1 chain in forward out rule source port ports=' ' peer key peer_address
  chain=DEPLOY-$(tr '[:lower:]' '[:upper:]' <<<"${name#dw}")
  in=$chain-IN forward=$chain-FWD out=$chain-OUT
  printf '*filter\n:%s - [0:0]\n:%s - [0:0]\n:%s - [0:0]\n' "$in" "$forward" "$out"
  printf -- '-A %s -p udp --dport %d -j ACCEPT\n' "$in" "$listen"
  printf -- '-A %s -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN\n' "$forward"
  for rule in "${access[@]}"; do
    read -r source port <<<"$rule"
    printf -- '-A %s -i %s -s %s/32 -p tcp -m conntrack --ctorigdst %s --ctorigdstport %d -j RETURN\n' "$forward" "$name" "$source" "$address" "$port"
  done
  for rule in "${access[@]}"; do
    read -r source port <<<"$rule"
    [[ $ports != *" $port "* ]] || continue
    ports+="$port "
    printf -- '-A %s -p tcp -m conntrack --ctorigdst %s --ctorigdstport %d -j DROP\n' "$forward" "$address" "$port"
  done
  for peer in "${peers[@]}"; do
    read -r key peer_address _ <<<"$peer"
    printf -- '-A %s -d %s/32 ! -o %s -j REJECT\n' "$forward" "$peer_address" "$name"
    printf -- '-A %s -d %s/32 ! -o %s -j REJECT\n' "$out" "$peer_address" "$name"
  done
  printf 'COMMIT\n'
}

chains_of() {
  local chain
  chain=DEPLOY-$(tr '[:lower:]' '[:upper:]' <<<"${1#dw}")
  printf '%s\n' "$chain-IN" "$chain-FWD" "$chain-OUT"
}

# jumps pairs each built in chain with the owned chain it sends traffic to.
jumps_of() {
  local chain
  chain=DEPLOY-$(tr '[:lower:]' '[:upper:]' <<<"${1#dw}")
  printf '%s\n' "INPUT $chain-IN" "DOCKER-USER $chain-FWD" "OUTPUT $chain-OUT"
}

render_unit() {
  cat <<EOF
[Unit]
Description=deploy managed network firewall for $1
Requires=docker.service
After=docker.service wg-quick@$1.service
PartOf=docker.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=$helper_path restore $1

[Install]
WantedBy=multi-user.target docker.service
EOF
}

render_owner() {
  printf '{\n  "runtimeId": "%s",\n  "environment": "%s",\n  "cidr": "%s",\n  "address": "%s",\n  "listenPort": %d\n}\n' \
    "$runtime" "$environment" "$cidr" "$address" "$listen"
}

# --- host ---------------------------------------------------------------------

require_root() { [ "$(id -u)" -eq 0 ] || refuse 'needs root; run through sudo -n'; }

require_platform() {
  local command os
  [ -d /run/systemd/system ] || refuse 'systemd is required'
  os=$(grep -E '^(ID|ID_LIKE)=' /etc/os-release 2>/dev/null || true)
  [[ $os =~ (debian|ubuntu) ]] || refuse 'only Debian and Ubuntu hosts are supported'
  for command in wg wg-quick ip iptables iptables-save iptables-restore ss flock sha256sum systemctl cmp; do
    command -v "$command" >/dev/null || refuse "$command is not installed"
  done
  iptables -w -n -L DOCKER-USER >/dev/null 2>&1 || refuse 'rootful Docker on iptables is required; DOCKER-USER is missing'
}

lock() {
  exec 9>/run/deploy-network.lock
  flock -w 30 9 || refuse 'another network change is running on this host'
}

# write_file replaces a file atomically, refusing symlinks on the way.
write_file() {
  local target=$1 mode=$2 directory temporary
  directory=$(dirname "$target")
  [ -d "$directory" ] && [ ! -L "$directory" ] || refuse "$directory is not a plain directory"
  [ ! -L "$target" ] || refuse "$target is a symlink"
  temporary=$(mktemp "$directory/.deploy-network.XXXXXXXX")
  cat >"$temporary"
  chmod "$mode" "$temporary"
  mv -fT "$temporary" "$target"
}

# same reports whether a file already holds exactly what stdin holds.
same() { [ -f "$1" ] && [ ! -L "$1" ] && cmp -s "$1" -; }

owned_directory() {
  local directory
  for directory in "$state_root" "$config_root"; do
    if [ -e "$directory" ]; then
      [ -d "$directory" ] && [ ! -L "$directory" ] || refuse "$directory is not a plain directory"
    else
      install -d -o root -g root -m 0700 "$directory"
    fi
  done
}

owner_matches() {
  local owner=$state_root/$1/owner.json
  [ -f "$owner" ] && [ ! -L "$owner" ] &&
    grep -qxF "  \"runtimeId\": \"$runtime\"," "$owner" &&
    grep -qxF "  \"environment\": \"$environment\"," "$owner"
}

require_owner() {
  local name=$1
  [ -e "$state_root/$name/owner.json" ] || refuse "$name is not enrolled; run enroll first"
  owner_matches "$name" || refuse "$name belongs to another runtime or environment"
  [ -f "$config_root/$name.key" ] && [ ! -L "$config_root/$name.key" ] ||
    refuse "the private key for $name is missing; remove it and enroll again to create a new identity"
}

listening_as() {
  wg show "$1" listen-port 2>/dev/null || true
}

# require_free refuses a listen port or subnet that something else already uses.
require_free() {
  local name=$1 line destination base bits number mask
  if [ -n "$(ss -Hlun "sport = :$listen")" ] && [ "$(listening_as "$name")" != "$listen" ]; then
    refuse "UDP port $listen is already in use on this host"
  fi
  while read -r line; do
    destination=${line%% *}
    [[ $line != *" dev $name "* && $line != *" dev $name" ]] || continue
    [ "$destination" != default ] || continue
    base=${destination%/*} bits=32
    [[ $destination != */* ]] || bits=${destination#*/}
    [[ $base =~ $ipv4_pattern ]] || continue
    number=$(ip_number "$base")
    mask=$(((0xFFFFFFFF << (32 - (bits < network_bits ? bits : network_bits))) & 0xFFFFFFFF))
    if [ $((number & mask)) -eq $((network_number & mask)) ]; then
      refuse "$cidr overlaps the existing route $destination"
    fi
  done < <(ip -4 -o route show table main)
}

changes=()
changed() { changes+=("$1"); }

ensure_jumps() {
  local builtin target
  while read -r builtin target; do
    iptables -w -C "$builtin" -j "$target" 2>/dev/null && continue
    iptables -w -I "$builtin" 1 -j "$target"
    changed "jump $builtin"
  done < <(jumps_of "$1")
}

live_rules() {
  local chain
  while read -r chain; do
    iptables -w -S "$chain" 2>/dev/null || printf 'absent %s\n' "$chain"
  done < <(chains_of "$1")
}

# --- verbs --------------------------------------------------------------------

verb_version() {
  local digest
  digest=$(sha256sum <"$0")
  printf 'deploy-network %s %s\n' "$helper_version" "${digest%% *}"
}

verb_check() {
  read_desired
  validate full
  render_config
  printf '%%%%\n'
  render_rules "$(interface_name)"
}

verb_enroll() {
  require_root
  require_platform
  read_desired
  validate interface
  lock
  owned_directory
  local name key
  name=$(interface_name)
  if [ -e "$state_root/$name" ]; then
    require_owner "$name"
  else
    if ip link show "$name" >/dev/null 2>&1 || [ -e "$config_root/$name.conf" ] || [ -e "$config_root/$name.key" ] ||
      [ -e "$unit_root/deploy-network-$name.service" ] || iptables -w -n -L "$(chains_of "$name" | head -n 1)" >/dev/null 2>&1; then
      refuse "$name already exists without an owner manifest; it is not deploy's to take"
    fi
    require_free "$name"
    install -d -o root -g root -m 0700 "$state_root/$name"
    render_owner | write_file "$state_root/$name/owner.json" 0600
    wg genkey | write_file "$config_root/$name.key" 0600
  fi
  printf 'interface %s\npublicKey %s\n' "$name" "$(wg pubkey <"$config_root/$name.key")"
}

verb_apply() {
  require_root
  require_platform
  local digest
  read_desired
  validate full
  digest=$(printf '%s\n' "$input" | sha256sum)
  lock
  owned_directory
  local name state config unit header_before='' header_after
  name=$(interface_name)
  state=$state_root/$name config=$config_root/$name.conf unit=deploy-network-$name.service
  require_owner "$name"
  require_free "$name"

  if ! render_owner | same "$state/owner.json"; then
    render_owner | write_file "$state/owner.json" 0600
    changed owner
  fi

  local rendered
  rendered=$(render_config)
  rendered=$(printf '%s\nPrivateKey = %s\n%s' "[Interface]" "$(cat "$config_root/$name.key")" "${rendered#\[Interface\]$'\n'}")
  [ ! -f "$config" ] || header_before=$(sed '/^$/q' "$config")
  header_after=$(sed '/^$/q' <<<"$rendered")
  local config_changed=false
  if ! printf '%s\n' "$rendered" | same "$config"; then
    printf '%s\n' "$rendered" | write_file "$config" 0600
    config_changed=true
    changed config
  fi
  unset rendered

  if ! systemctl is-enabled --quiet "wg-quick@$name"; then
    systemctl enable --quiet "wg-quick@$name"
    changed 'wireguard enabled'
  fi
  if ! systemctl is-active --quiet "wg-quick@$name"; then
    systemctl start "wg-quick@$name"
    changed 'wireguard started'
  elif [ "$config_changed" = true ] && [ "$header_before" != "$header_after" ]; then
    systemctl restart "wg-quick@$name"
    changed 'wireguard restarted'
  elif [ "$config_changed" = true ]; then
    wg syncconf "$name" <(wg-quick strip "$name")
    changed peers
  fi

  local peer key peer_address wanted=' ' route
  for peer in "${peers[@]}"; do
    read -r key peer_address _ <<<"$peer"
    wanted+="$peer_address/32 "
    if [ -z "$(ip -4 route show "$peer_address/32" dev "$name")" ]; then
      ip -4 route replace "$peer_address/32" dev "$name"
      changed "route $peer_address"
    fi
  done
  while read -r route _; do
    [[ $route == */* ]] || route=$route/32
    [[ $wanted == *" $route "* ]] && continue
    ip -4 route del "$route" dev "$name"
    changed "route $route removed"
  done < <(ip -4 -o route show dev "$name" | grep -v ' proto kernel ' || true)

  if ! render_rules "$name" | same "$state/firewall.rules" || ! live_rules "$name" | same "$state/firewall.live"; then
    render_rules "$name" | write_file "$state/firewall.rules" 0600
    iptables-restore -w --noflush <"$state/firewall.rules"
    live_rules "$name" | write_file "$state/firewall.live" 0600
    changed firewall
  fi
  ensure_jumps "$name"

  if ! render_unit "$name" | same "$unit_root/$unit"; then
    render_unit "$name" | write_file "$unit_root/$unit" 0644
    systemctl daemon-reload
    changed 'boot unit'
  fi
  if ! systemctl is-enabled --quiet "$unit"; then
    systemctl enable --quiet "$unit"
    changed 'boot unit enabled'
  fi

  if ! printf '%s\n' "${digest%% *}" | same "$state/applied"; then
    printf '%s\n' "${digest%% *}" | write_file "$state/applied" 0600
  fi
  # the unit runs restore, which takes the lock itself
  if ! systemctl is-active --quiet "$unit"; then
    exec 9>&-
    systemctl start "$unit"
    changed 'boot unit started'
  fi
  if [ "${#changes[@]}" -eq 0 ]; then
    echo unchanged
  else
    printf 'changed %s\n' "${changes[@]}"
  fi
}

verb_observe() {
  require_root
  require_platform
  read_desired
  validate identity
  local name state builtin target chain digest
  name=$(interface_name)
  state=$state_root/$name
  verb_version
  printf 'interface %s\n' "$name"
  if [ ! -e "$state" ]; then
    echo 'owner absent'
    return
  fi
  owner_matches "$name" || refuse "$name belongs to another runtime or environment"
  echo 'owner present'
  if [ -f "$config_root/$name.key" ]; then
    printf 'publicKey %s\n' "$(wg pubkey <"$config_root/$name.key")"
  else
    echo 'publicKey absent'
  fi
  printf 'wireguard %s %s\n' "$(systemctl is-enabled "wg-quick@$name" 2>/dev/null || true)" "$(systemctl is-active "wg-quick@$name" 2>/dev/null || true)"
  printf 'boot %s %s\n' "$(systemctl is-enabled "deploy-network-$name.service" 2>/dev/null || true)" "$(systemctl is-active "deploy-network-$name.service" 2>/dev/null || true)"
  if ip link show "$name" >/dev/null 2>&1; then
    printf 'listen %s\n' "$(listening_as "$name")"
    wg show "$name" allowed-ips | while read -r key allowed; do printf 'peer %s %s\n' "$key" "$allowed"; done
    ip -4 -o route show dev "$name" | { grep -v ' proto kernel ' || true; } | while read -r route _; do printf 'route %s\n' "$route"; done
  else
    echo 'link absent'
  fi
  digest=$(live_rules "$name" | sha256sum)
  printf 'firewall %s\n' "${digest%% *}"
  if live_rules "$name" | same "$state/firewall.live"; then echo 'firewall matches'; else echo 'firewall drifted'; fi
  while read -r builtin target; do
    if iptables -w -C "$builtin" -j "$target" 2>/dev/null; then chain=present; else chain=absent; fi
    printf 'jump %s %s\n' "$builtin" "$chain"
  done < <(jumps_of "$name")
  if [ -f "$state/applied" ]; then printf 'applied %s\n' "$(cat "$state/applied")"; else echo 'applied none'; fi
}

verb_remove() {
  require_root
  require_platform
  read_desired
  validate identity
  lock
  local name builtin target chain unit
  name=$(interface_name)
  unit=deploy-network-$name.service
  [ -e "$state_root/$name" ] || { echo "absent $name"; return; }
  owner_matches "$name" || refuse "$name belongs to another runtime or environment"
  systemctl disable --now --quiet "$unit" 2>/dev/null || true
  rm -f "$unit_root/$unit"
  systemctl disable --now --quiet "wg-quick@$name" 2>/dev/null || true
  systemctl daemon-reload
  ! ip link show "$name" >/dev/null 2>&1 || ip link delete "$name"
  while read -r builtin target; do
    while iptables -w -C "$builtin" -j "$target" 2>/dev/null; do iptables -w -D "$builtin" -j "$target"; done
  done < <(jumps_of "$name")
  while read -r chain; do
    if iptables -w -n -L "$chain" >/dev/null 2>&1; then
      iptables -w -F "$chain"
      iptables -w -X "$chain"
    fi
  done < <(chains_of "$name")
  rm -f "$config_root/$name.conf" "$config_root/$name.key"
  rm -rf "${state_root:?}/$name"
  echo "removed $name"
}

verb_restore() {
  require_root
  local name=${1:-}
  [[ $name =~ ^dw[0-9a-f]{12}$ ]] || refuse 'restore takes an owned interface name'
  [ -f "$state_root/$name/owner.json" ] && [ -f "$state_root/$name/firewall.rules" ] || refuse "$name has no recorded firewall"
  lock
  iptables-restore -w --noflush <"$state_root/$name/firewall.rules"
  ensure_jumps "$name"
}

usage='usage: deploy-network version|check|observe|enroll|apply|remove'
case ${1:-}:$# in
version:1) verb_version ;;
check:1) verb_check ;;
observe:1) verb_observe ;;
enroll:1) verb_enroll ;;
apply:1) verb_apply ;;
remove:1) verb_remove ;;
restore:2) verb_restore "$2" ;;
*) refuse "$usage" ;;
esac
