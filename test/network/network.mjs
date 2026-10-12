import { ssh, put, assert, until, pass } from './common.mjs';
const image = 'redis@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499';
const ping = () => ssh('b', 'timeout 4 docker exec -e REDISCLI_AUTH=disposable-network-test consumer redis-cli -h 10.77.0.1 ping');
await until('authenticated container traffic', () => ping() === 'PONG');
assert.match(ssh('a', 'wg show wg0 endpoints'), /10\.231\.0\.22:/);
pass('authenticated container-to-container traffic through WireGuard and NAT');
assert.match(ssh('b', 'docker exec consumer redis-cli -h 10.77.0.1 ping'), /NOAUTH/);
assert.equal(ssh('b', 'if nc -z -w 2 10.231.0.1 6379; then echo exposed; else echo denied; fi'), 'denied');
ssh('b', 'ip route add 10.77.0.1/32 dev underlay table 100; ip rule add priority 50 from 10.231.0.2 lookup 100');
assert.equal(ssh('b', 'if nc -s 10.231.0.2 -z -w 2 10.77.0.1 6379; then echo exposed; else echo denied; fi'), 'denied');
ssh('b', 'ip rule del priority 50; ip route flush table 100');
pass('authentication required; public and directly routed plaintext access denied');
ssh('a', 'ufw allow in on wan to any port 22 proto tcp >/dev/null; ufw allow in on underlay to any port 51820 proto udp >/dev/null; ufw --force enable >/dev/null');
assert.equal(ping(), 'PONG');
ssh('a', 'ufw reload >/dev/null');
assert.equal(ping(), 'PONG');
assert.equal(ssh('b', 'if nc -z -w 2 10.231.0.1 6379; then echo exposed; else echo denied; fi'), 'denied');
pass('UFW enable and reload preserve allowed traffic and private port isolation');
put('a', '/root/reconcile.sh', `#!/bin/bash
set -euo pipefail
exec 9>/run/deploy-lab-network.lock
flock -n 9
desired=$(sha256sum /etc/wireguard/wg0.conf | cut -d' ' -f1)
if test -f /root/applied && test "$(cat /root/applied)" = "$desired"; then
  exit 0
fi
wg syncconf wg0 <(wg-quick strip wg0)
printf '%s\\n' "$desired" > /root/applied
echo changed
`, '700');
ssh('a', '/root/reconcile.sh');
const snapshot = () => ssh('a', 'sha256sum /etc/wireguard/wg0.conf; stat -c %Y /etc/wireguard/wg0.conf; wg show wg0 public-key; wg show wg0 allowed-ips; ip -4 route show; iptables-save; systemctl show wg-quick@wg0 -p ActiveEnterTimestampMonotonic')
  .split('\n').filter(line => !line.startsWith('#')).map(line => line.replace(/\[\d+:\d+\]/g, '[0:0]')).join('\n');
const before = snapshot();
assert.equal(ssh('a', '/root/reconcile.sh'), '');
assert.equal(snapshot(), before);
pass('unchanged prototype reconciliation preserves config, key, rules, routes and service start time');
const extra = ssh('b', 'umask 077; wg genkey > /root/extra.key; wg pubkey < /root/extra.key');
ssh('a', `printf '\n[Peer]\nPublicKey = ${extra}\nAllowedIPs = 10.77.0.3/32\n' >> /etc/wireguard/wg0.conf`);
ssh('b', 'docker exec -e REDISCLI_AUTH=disposable-network-test consumer sh -c \'for i in $(seq 1 40); do redis-cli -h 10.77.0.1 ping; sleep .1; done\' > /root/continuous.log 2>&1 &');
assert.equal(ssh('a', '/root/reconcile.sh'), 'changed');
assert.equal(ssh('a', 'ip route show 10.77.0.3/32'), '');
ssh('a', 'ip route add 10.77.0.3/32 dev wg0');
await until('continuous requests complete', () => ssh('b', 'wc -l < /root/continuous.log') === '40');
assert.equal(ssh('b', "sort -u /root/continuous.log"), 'PONG');
pass('adding a peer preserves existing traffic; peer sync alone does not install its route');
ssh('a', `iptables -I DEPLOY-LAB 2 -p tcp -m conntrack --ctorigdst 10.77.0.1 --ctorigdstport 6380 -j DROP;
docker run -d --name second-provider --restart=no -p 10.77.0.1:6380:6379 -v /root/redis.conf:/etc/redis.conf:ro ${image} redis-server /etc/redis.conf >/dev/null`);
assert.equal(ssh('b', 'if nc -z -w 2 10.77.0.1 6380; then echo exposed; else echo denied; fi'), 'denied');
const originalInterface = ssh('a', 'systemctl show wg-quick@wg0 -p ActiveEnterTimestampMonotonic');
ssh('a', 'iptables -I DEPLOY-LAB 2 -i wg0 -s 10.77.0.2/32 -p tcp -m conntrack --ctorigdst 10.77.0.1 --ctorigdstport 6380 -j RETURN');
assert.equal(ssh('b', 'timeout 4 docker exec -e REDISCLI_AUTH=disposable-network-test consumer redis-cli -h 10.77.0.1 -p 6380 ping'), 'PONG');
assert.equal(ssh('a', 'systemctl show wg-quick@wg0 -p ActiveEnterTimestampMonotonic'), originalInterface);
assert.equal(ping(), 'PONG');
ssh('a', 'docker rm -f second-provider >/dev/null');
pass('adding a service port admits its consumer without restarting WireGuard or disrupting existing traffic');
