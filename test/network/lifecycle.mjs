import { ssh, put, assert, until, pass, sleep } from './common.mjs';
const ping = () => ssh('b', 'timeout 4 docker exec -e REDISCLI_AUTH=disposable-network-test consumer redis-cli -h 10.77.0.1 ping');
const running = () => ssh('a', 'docker inspect -f "{{.State.Running}}" provider') === 'true';
put('a', '/root/policy.sh', `#!/bin/bash
set -euo pipefail
iptables -N DEPLOY-LAB 2>/dev/null || true
iptables -C DOCKER-USER -j DEPLOY-LAB 2>/dev/null || iptables -I DOCKER-USER 1 -j DEPLOY-LAB
if ! iptables -C DEPLOY-LAB -j RETURN 2>/dev/null; then
  iptables -A DEPLOY-LAB -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
  iptables -A DEPLOY-LAB -i wg0 -s 10.77.0.2/32 -p tcp -m conntrack --ctorigdst 10.77.0.1 --ctorigdstport 6379 -j RETURN
  iptables -A DEPLOY-LAB -p tcp -m conntrack --ctorigdst 10.77.0.1 --ctorigdstport 6379 -j DROP
  iptables -A DEPLOY-LAB -j RETURN
fi
`, '700');
put('a', '/etc/systemd/system/deploy-lab-policy.service', `[Unit]
Requires=docker.service wg-quick@wg0.service
After=docker.service wg-quick@wg0.service
PartOf=docker.service
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/root/policy.sh
[Install]
WantedBy=docker.service
`);
put('a', '/etc/systemd/system/deploy-lab-provider.service', `[Unit]
Requires=docker.service wg-quick@wg0.service deploy-lab-policy.service
After=docker.service wg-quick@wg0.service deploy-lab-policy.service
PartOf=docker.service
[Service]
ExecStart=/usr/local/bin/docker start -a provider
ExecStop=-/usr/local/bin/docker stop -t 3 provider
Restart=always
RestartSec=1
TimeoutStopSec=10
[Install]
WantedBy=multi-user.target docker.service wg-quick@wg0.service
`);
ssh('a', 'docker stop provider >/dev/null; systemctl daemon-reload; systemctl enable --now deploy-lab-policy deploy-lab-provider');
await until('supervised provider ready', () => ping() === 'PONG');
assert.equal(ssh('a', 'docker inspect -f "{{.HostConfig.RestartPolicy.Name}}" provider'), 'no');
const start = ssh('a', 'docker inspect -f "{{.State.StartedAt}}" provider');
ssh('a', 'docker kill provider >/dev/null');
await until('container crash restarted', () => ping() === 'PONG');
assert.notEqual(ssh('a', 'docker inspect -f "{{.State.StartedAt}}" provider'), start);
pass('systemd restarts a crashed container with Docker restart disabled');
ssh('a', 'systemctl stop deploy-lab-provider');
await sleep(2500);
assert.equal(running(), false);
ssh('a', 'systemctl start deploy-lab-provider');
await until('provider manual start', () => ping() === 'PONG');
ssh('a', 'systemctl restart docker');
await until('provider after Docker restart', () => ping() === 'PONG');
pass('operator stop is respected; Docker restart restores the supervised service');
const oldBoot = ssh('a', 'cat /proc/sys/kernel/random/boot_id');
ssh('a', 'mkdir -p /etc/systemd/system/wg-quick@wg0.service.d');
put('a', '/etc/systemd/system/wg-quick@wg0.service.d/delay.conf', '[Service]\nExecStartPre=/bin/sleep 8\n');
try { ssh('a', 'systemctl reboot'); } catch {}
await until('new boot', () => ssh('a', 'cat /proc/sys/kernel/random/boot_id') !== oldBoot, 90000);
await until('provider after delayed WireGuard boot', () => ping() === 'PONG', 90000);
const times = ssh('a', 'systemctl show wg-quick@wg0 deploy-lab-policy deploy-lab-provider -p Id -p ActiveEnterTimestampMonotonic');
console.log(times);
const entered = [...times.matchAll(/ActiveEnterTimestampMonotonic=(\d+)/g)].map(match => Number(match[1]));
assert.equal(entered.length, 3);
assert.ok(entered[0] > 0 && entered[0] <= entered[1] && entered[1] <= entered[2]);
assert.equal(ssh('a', 'iptables -C DOCKER-USER -j DEPLOY-LAB && echo policy'), 'policy');
pass('actual VM reboot restores WireGuard, firewall and authenticated service access');
ssh('a', 'systemctl stop wg-quick@wg0');
await until('provider stopped with private interface', () => !running());
assert.equal(ssh('a', 'systemctl is-active docker'), 'active');
ssh('a', 'systemctl start wg-quick@wg0');
await until('provider restored with network', () => ping() === 'PONG');
pass('private network loss stops only its dependent service; restoration starts it again');
