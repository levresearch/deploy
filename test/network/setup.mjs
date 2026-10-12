import { ssh, put, until, pass } from './common.mjs';

export const redis = 'redis@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499';
for (const host of ['a', 'b']) {
  await until(`guest ${host} ready`, () => ssh(host, 'test -f /root/ready && docker info >/dev/null && echo ready') === 'ready', 600000);
  ssh(host, `docker pull ${redis} >/dev/null; apt-get install -y netcat-openbsd >/dev/null`);
  ssh(host, 'umask 077; wg genkey > /root/wg.key; wg pubkey < /root/wg.key > /root/wg.pub');
  pass(`guest ${host} has independent Docker and WireGuard`);
}
const pubs = Object.fromEntries(['a', 'b'].map(host => [host, ssh(host, 'cat /root/wg.pub')]));
ssh('b', 'ip address add 10.231.0.22/24 dev underlay; iptables -t nat -A POSTROUTING -o underlay -p udp --dport 51820 -j SNAT --to-source 10.231.0.22');
for (const [host, index, peer] of [['a', 1, 'b'], ['b', 2, 'a']]) {
  put(host, '/root/peer.conf', `[Interface]\nAddress = 10.77.0.${index}/32\nListenPort = 51820\nSaveConfig = false\n\n[Peer]\nPublicKey = ${pubs[peer]}\nAllowedIPs = 10.77.0.${3-index}/32\n${host === 'b' ? 'Endpoint = 10.231.0.1:51820\nPersistentKeepalive = 25\n' : ''}`);
  ssh(host, 'install -d -m 700 /etc/wireguard; { head -n 1 /root/peer.conf; printf "PrivateKey = "; cat /root/wg.key; tail -n +2 /root/peer.conf; } > /etc/wireguard/wg0.conf; chmod 600 /etc/wireguard/wg0.conf; systemctl enable --now wg-quick@wg0');
}
ssh('a', `iptables -N DEPLOY-LAB
iptables -I DOCKER-USER 1 -j DEPLOY-LAB
iptables -A DEPLOY-LAB -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
iptables -A DEPLOY-LAB -i wg0 -s 10.77.0.2/32 -p tcp -m conntrack --ctorigdst 10.77.0.1 --ctorigdstport 6379 -j RETURN
iptables -A DEPLOY-LAB -p tcp -m conntrack --ctorigdst 10.77.0.1 --ctorigdstport 6379 -j DROP
iptables -A DEPLOY-LAB -j RETURN`);
put('a', '/root/redis.conf', 'bind 0.0.0.0\nprotected-mode yes\nrequirepass disposable-network-test\nappendonly no\nsave ""\n');
ssh('a', `chown $(docker run --rm ${redis} id -u redis) /root/redis.conf`);
ssh('a', `docker create --name provider --restart=no -p 10.77.0.1:6379:6379 -v /root/redis.conf:/etc/redis.conf:ro ${redis} redis-server /etc/redis.conf >/dev/null; docker start provider`);
ssh('b', `docker network create consumer >/dev/null; docker run -d --name consumer --network consumer ${redis} sleep infinity >/dev/null`);
pass('private Redis provider and consumer created');
