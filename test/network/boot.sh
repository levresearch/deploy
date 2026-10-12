#!/usr/bin/env bash
set -euo pipefail
cd /lab
curl -fsSL --retry 3 -o base.qcow2 https://cloud.debian.org/images/cloud/bookworm/20261006-2623/debian-12-generic-amd64-20261006-2623.qcow2
echo 'f631afb1d8123a29779f720f7f08a97e9f2ce5f5c705b8d9f753df8ed0c3afddeb34e4f254dfa41a6330e9a637387c80a8fe453d789446f9d8be7d7714d0ce7e  base.qcow2' | sha512sum -c -
curl -fsSL --retry 3 -o docker.tgz https://download.docker.com/linux/static/stable/x86_64/docker-29.1.3.tgz
sha256sum docker.tgz > docker.sha256
echo 'c019c608ba2bb009dd673f3230e4d743f36a78d36166c6c2444c05d0aa9ff0d9  docker.tgz' | sha256sum -c -
ssh-keygen -q -t ed25519 -N '' -f id_ed25519
for index in 1 2; do
  name=a
  link=listen=127.0.0.1:23000
  if [ "$index" = 2 ]; then name=b; link=connect=127.0.0.1:23000; fi
  qemu-img create -q -f qcow2 -F qcow2 -b /lab/base.qcow2 "$name.qcow2" 6G
  cat > "$name.user" <<EOF
#cloud-config
hostname: network-$name
disable_root: false
ssh_pwauth: false
users:
  - name: root
    ssh_authorized_keys:
      - $(cat id_ed25519.pub)
package_update: true
packages: [wireguard-tools, iptables, ufw, conntrack, curl]
runcmd:
  - mkdir -p /mnt/lab
  - mount -t 9p -o trans=virtio,version=9p2000.L lab /mnt/lab
  - tar -xzf /mnt/lab/docker.tgz -C /usr/local/bin --strip-components=1
  - cp /mnt/lab/docker.service /etc/systemd/system/docker.service
  - systemctl daemon-reload
  - systemctl enable --now docker
  - touch /root/ready
EOF
  cat > "$name.network" <<EOF
version: 2
ethernets:
  wan:
    match: {macaddress: '52:54:00:12:00:0$index'}
    set-name: wan
    dhcp4: true
  underlay:
    match: {macaddress: '52:54:00:13:00:0$index'}
    set-name: underlay
    addresses: [10.231.0.$index/24]
EOF
  echo "instance-id: network-$name" > "$name.meta"
  cloud-localds --network-config="$name.network" "$name.iso" "$name.user" "$name.meta"
  qemu-system-x86_64 -enable-kvm -cpu host -m 768 -smp 2 -display none \
    -daemonize -pidfile "$name.pid" -serial "file:$name.console" \
    -drive "file=$name.qcow2,if=virtio,format=qcow2" \
    -drive "file=$name.iso,media=cdrom,format=raw" \
    -netdev "user,id=wan,hostfwd=tcp:127.0.0.1:2210$index-:22" \
    -device "virtio-net-pci,netdev=wan,mac=52:54:00:12:00:0$index" \
    -netdev "socket,id=test,$link" \
    -device "virtio-net-pci,netdev=test,mac=52:54:00:13:00:0$index" \
    -virtfs local,path=/lab,mount_tag=lab,security_model=none,readonly=on
done
echo 'two disposable guests started'
