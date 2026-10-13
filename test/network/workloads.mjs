import { createHash } from 'node:crypto';
import { ssh, assert, until, pass, inContainer, asDeployer, succeed } from './common.mjs';

// the containers deploy records with the helper's workloads verb are left
// stopped by Docker at boot and on a daemon restart, and restore starts them
// once the private address, routes and firewall exist. runs after helper.mjs.
const image = 'redis@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499';
const helperPath = '/usr/local/libexec/deploy-network';
const runtime = 'fb00000a';
const iface = 'dw' + createHash('sha256').update(runtime).digest('hex').slice(0, 12);
const provider = `deploy-${runtime}-shared-cache-1`;
const consumer = `deploy-${runtime}_api_abcdef1-api-1`;
const missing = `deploy-${runtime}_gone_abcdef1-gone-1`;
const deploy = (...args) => inContainer(['/lab/bin/deploy', 'network', ...args, '-C', '/lab/project']);
const workloads = (host, ...names) => asDeployer(host, `sudo -n ${helperPath} workloads`,
  `runtime ${runtime}\nenvironment production\n` + names.map(name => `workload ${name}\n`).join(''));
const refused = (result, pattern) => {
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, pattern);
};
const running = (host, name) => ssh(host, `docker inspect -f '{{.State.Running}}' ${name} 2>/dev/null || echo missing`);
const reach = () => ssh('b', `docker exec ${consumer} redis-cli -h 10.78.0.1 -p 6390 ping 2>/dev/null || echo unreachable`);
const reboot = async host => {
  const boot = ssh(host, 'cat /proc/sys/kernel/random/boot_id');
  try { ssh(host, 'systemctl reboot'); } catch {}
  await until(`guest ${host} rebooted`, () => ssh(host, 'cat /proc/sys/kernel/random/boot_id') !== boot, 120000);
};
const monotonic = (host, unit, property) => Number(ssh(host, `systemctl show ${unit} -p ${property} --value`));

succeed(deploy('apply'));
ssh('a', `docker run -d --name ${provider} --restart=on-failure -p 10.78.0.1:6390:6379 ${image} >/dev/null`);
ssh('b', `docker run -d --name ${consumer} --restart=on-failure ${image} >/dev/null`);
await until('consumer reaches the provider', () => reach() === 'PONG', 60000);

refused(workloads('a', 'deploy-fb00000b-shared-cache-1'), /not a container of this runtime/);
refused(workloads('a', 'evil; reboot'), /malformed|not a container/);
refused(workloads('a', provider, provider), /given twice/);
refused(asDeployer('a', `sudo -n ${helperPath} workloads`, `runtime ${runtime}\nenvironment production\ncidr 10.78.0.0/24\n`), /takes only runtime, environment and workloads/);
refused(asDeployer('a', `sudo -n ${helperPath} observe`, `runtime ${runtime}\nenvironment production\nworkload ${provider}\n`), /takes no workloads/);
assert.equal(succeed(workloads('a', missing, provider)), 'changed workloads');
assert.equal(succeed(workloads('a', missing, provider)), 'unchanged');
assert.equal(succeed(workloads('b', consumer)), 'changed workloads');
assert.equal(ssh('a', `stat -c '%a %U' /var/lib/deploy-network/${iface}/workloads`), '600 root');
pass('workloads records only this runtime\'s containers, once each, and an unchanged list writes nothing');

ssh('a', 'systemctl restart docker');
await until('provider after docker restart', () => running('a', provider) === 'true', 90000);
await until('consumer after provider docker restart', () => reach() === 'PONG', 60000);
pass('a Docker restart brings the workload back through the network unit, not Docker');

await reboot('a');
await until('provider after reboot', () => running('a', provider) === 'true', 120000);
assert.match(ssh('a', `journalctl -b -u deploy-network-${iface} --no-pager`), new RegExp(`workload ${missing} is missing`));
const providerStart = ssh('a', `date -d "$(docker inspect -f '{{.State.StartedAt}}' ${provider})" +%s`);
const wireguardUp = ssh('a', `date -d "$(systemctl show wg-quick@${iface} -p ActiveEnterTimestamp --value)" +%s`);
assert.ok(Number(providerStart) >= Number(wireguardUp), 'the provider started before WireGuard');
await until('consumer after provider reboot', () => reach() === 'PONG', 60000);
pass('after a reboot the provider starts only once WireGuard is up, and a missing workload does not stop the rest');

await reboot('b');
await until('consumer after reboot', () => running('b', consumer) === 'true', 120000);
const consumerStart = monotonic('b', `deploy-network-${iface}`, 'InactiveExitTimestampMonotonic');
const routesReady = ssh('b', `ip -4 route show 10.78.0.1/32 dev ${iface} | wc -l`);
assert.equal(routesReady, '1');
assert.ok(consumerStart > 0);
const startedAt = ssh('b', `date -d "$(docker inspect -f '{{.State.StartedAt}}' ${consumer})" +%s`);
const unitStarted = ssh('b', `date -d "$(systemctl show deploy-network-${iface} -p InactiveExitTimestamp --value)" +%s`);
assert.ok(Number(startedAt) >= Number(unitStarted), 'the consumer started before the network unit restored routes and firewall');
await until('consumer reaches the provider after its own reboot', () => reach() === 'PONG', 60000);
pass('after a reboot the consumer starts from the network unit, after its routes and egress guard');

ssh('a', `docker rm -f ${provider} >/dev/null`);
ssh('b', `docker rm -f ${consumer} >/dev/null`);
for (const host of ['a', 'b']) {
  succeed(asDeployer(host, `sudo -n ${helperPath} remove`, `runtime ${runtime}\nenvironment production\n`));
  ssh(host, 'rm -rf /home/deployer/Projects');
}
pass('workloads fixture removed');
