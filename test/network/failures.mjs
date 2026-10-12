import { ssh, put, assert, until, pass, sleep } from './common.mjs';
const image = 'redis@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499';
const ping = () => ssh('b', 'timeout 4 docker exec -e REDISCLI_AUTH=disposable-network-test consumer redis-cli -h 10.77.0.1 ping');
ssh('a', `docker run -d --name unrelated --restart unless-stopped ${image} sleep infinity >/dev/null`);
put('a', '/etc/systemd/system/wg-quick@wg0.service.d/fail.conf', '[Service]\nExecStartPre=/bin/false\n');
ssh('a', 'systemctl daemon-reload; systemctl restart wg-quick@wg0 || true');
assert.equal(ssh('a', 'docker inspect -f "{{.State.Running}}" provider'), 'false');
assert.equal(ssh('a', 'docker inspect -f "{{.State.Running}}" unrelated'), 'true');
ssh('a', 'rm /etc/systemd/system/wg-quick@wg0.service.d/fail.conf; systemctl daemon-reload; systemctl reset-failed; systemctl start wg-quick@wg0');
await until('recovered provider', () => ping() === 'PONG');
pass('failed network setup blocks its provider without stopping unrelated containers');

ssh('b', 'iptables -I DOCKER-USER 1 -d 10.77.0.0/24 ! -o wg0 -j REJECT; ip route del 10.77.0.1/32 dev wg0');
assert.equal(ssh('b', 'if timeout 4 docker exec -e REDISCLI_AUTH=disposable-network-test consumer redis-cli -h 10.77.0.1 ping; then echo leaked; else echo blocked; fi').split('\n').at(-1), 'blocked');
assert.match(ssh('b', 'iptables -nvL DOCKER-USER'), /[1-9]\d*\s+\d+\s+REJECT/);
ssh('b', 'ip route add 10.77.0.1/32 dev wg0');
assert.equal(ping(), 'PONG');
pass('missing private route hits the egress guard instead of leaking through the default route');

const peer = ssh('b', 'wg show wg0 public-key');
ssh('a', `wg set wg0 peer ${peer} allowed-ips 10.77.0.2/32,10.77.0.4/32; ip route add 10.77.0.4/32 dev wg0`);
ssh('b', 'ip address add 10.77.0.4/32 dev wg0');
assert.equal(ssh('b', 'if nc -s 10.77.0.4 -z -w 2 10.77.0.1 6379; then echo allowed; else echo denied; fi'), 'denied');
assert.equal(ping(), 'PONG');
ssh('a', `wg set wg0 peer ${peer} allowed-ips 10.77.0.2/32; ip route del 10.77.0.4/32 dev wg0`);
ssh('b', 'ip address del 10.77.0.4/32 dev wg0');
pass('authenticated tunnel traffic from an unauthorized private source is denied');

ssh('a', 'systemctl disable --now deploy-lab-provider');
const boot = ssh('a', 'cat /proc/sys/kernel/random/boot_id');
try { ssh('a', 'systemctl reboot'); } catch {}
await until('stopped-service reboot', () => ssh('a', 'cat /proc/sys/kernel/random/boot_id') !== boot, 90000);
await until('Docker after stopped-service reboot', () => ssh('a', 'systemctl is-active docker') === 'active');
await sleep(10000);
assert.equal(ssh('a', 'docker inspect -f "{{.State.Running}}" provider'), 'false');
assert.equal(ssh('a', 'docker inspect -f "{{.State.Running}}" unrelated'), 'true');
ssh('a', 'systemctl enable --now deploy-lab-provider');
await until('explicit restart', () => ping() === 'PONG');
pass('explicit disable/stop survives VM reboot; unrelated Docker restart policy still works');

ssh('a', `docker create --name provider-previous --restart=no -p 10.77.0.1:6379:6379 -v /root/redis.conf:/etc/redis.conf:ro ${image} redis-server /etc/redis.conf >/dev/null; systemctl stop deploy-lab-provider; mkdir -p /etc/systemd/system/deploy-lab-provider.service.d`);
put('a', '/etc/systemd/system/deploy-lab-provider.service.d/previous.conf', '[Service]\nExecStart=\nExecStart=/usr/local/bin/docker start -a provider-previous\nExecStop=\nExecStop=-/usr/local/bin/docker stop -t 3 provider-previous\n');
ssh('a', 'systemctl daemon-reload; systemctl start deploy-lab-provider');
await until('previous container serving', () => ping() === 'PONG');
assert.equal(ssh('a', 'docker inspect -f "{{.State.Running}}" provider'), 'false');
ssh('a', 'systemctl restart docker');
await until('previous after Docker restart', () => ping() === 'PONG');
assert.equal(ssh('a', 'docker inspect -f "{{.State.Running}}" provider'), 'false');
assert.equal(ssh('a', 'docker inspect -f "{{.State.Running}}" provider-previous'), 'true');
pass('switching the recorded container restores only the chosen previous release after daemon restart');
