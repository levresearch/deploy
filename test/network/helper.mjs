import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdirSync, writeFileSync } from 'node:fs';
import { ssh, assert, until, pass } from './common.mjs';

// the real deploy binary bootstraps the helper as an unprivileged user with
// password sudo, then the helper is driven only through sudo -n, the way deploy
// will drive it. wg0 and DEPLOY-LAB from the earlier phases stand in for
// network state deploy does not own.
const lab = process.env.DEPLOY_NETWORK_LAB;
const container = process.env.DEPLOY_NETWORK_CONTAINER;
const image = 'redis@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499';
const helperPath = '/usr/local/libexec/deploy-network';
const runtime = 'fb00000a';
const iface = 'dw' + createHash('sha256').update(runtime).digest('hex').slice(0, 12);
const chain = 'DEPLOY-' + iface.slice(2).toUpperCase();

mkdirSync(`${lab}/bin`, { recursive: true });
mkdirSync(`${lab}/project`, { recursive: true });
writeFileSync(`${lab}/ssh_config`, `Host vm-a
  Port 22101
Host vm-b
  Port 22102
Host *
  HostName 127.0.0.1
  User deployer
  IdentityFile /lab/id_ed25519
  IdentitiesOnly yes
  UserKnownHostsFile /lab/known_hosts
  StrictHostKeyChecking accept-new
`, { mode: 0o600 });
writeFileSync(`${lab}/bin/ssh`, '#!/bin/sh\nexec /usr/bin/ssh -F /lab/ssh_config "$@"\n', { mode: 0o755 });
writeFileSync(`${lab}/bin/deploy`, '#!/bin/sh\nPATH=/lab/bin:$PATH exec /lab/deploy "$@"\n', { mode: 0o755 });
writeFileSync(`${lab}/project/.deploy.json`, JSON.stringify({
  version: 2, id: runtime, name: 'lab',
  destinations: { a: 'vm-a:Projects/lab', b: 'vm-b:Projects/lab' },
  network: {
    cidr: '10.78.0.0/24', peers: {
      a: { address: '10.78.0.1', listenPort: 51821, endpoint: '10.231.0.1:51821' },
      b: { address: '10.78.0.2', listenPort: 51821, keepalive: 25 },
    },
  },
  services: {
    api: {
      destination: 'b', image: 'example/api', dependsOn: { db: 'healthy' },
      connections: { database: { service: 'db', endpoint: 'sql', hostEnv: 'DB_HOST', portEnv: 'DB_PORT' } },
    },
    db: {
      destination: 'a', image: 'example/db', stateful: true,
      healthcheck: { command: ['CMD', 'check'] },
      endpoints: { sql: { port: 6379, hostPort: 6390, protocol: 'tcp' } },
    },
  },
}));

function inContainer(args, input = '') {
  const result = spawnSync('docker', ['exec', '-i', container, ...args], { input, encoding: 'utf8', timeout: 240000 });
  return { status: result.status, stdout: result.stdout.trim(), stderr: result.stderr.trim() };
}
const deploy = (...args) => inContainer(['/lab/bin/deploy', ...args]);
// asDeployer runs one command on a guest as the unprivileged deploy user.
const asDeployer = (host, command, input = '') => inContainer(['/lab/bin/ssh', `vm-${host}`, command], input);
const helper = (host, verb, input = '') => asDeployer(host, `sudo -n ${helperPath} ${verb}`, input);
const succeed = result => {
  assert.equal(result.status, 0, `${result.stdout}\n${result.stderr}`);
  return result.stdout;
};
const refused = (result, reason) => {
  assert.notEqual(result.status, 0, result.stdout);
  assert.match(result.stderr, reason);
};

for (const host of ['a', 'b']) {
  refused(helper(host, 'version'), /password is required|a terminal is required/);
}
const foreign = host => ssh(host, 'sha256sum /etc/wireguard/wg0.conf; wg show wg0 public-key; wg show wg0 allowed-ips; iptables -S DEPLOY-LAB 2>/dev/null || true; iptables -S DOCKER-USER; ufw status verbose');
const before = { a: foreign('a'), b: foreign('b') };

const bootstrapped = inContainer(['expect', '/fixture/bootstrap.exp']);
assert.equal(bootstrapped.status, 0, `${bootstrapped.stdout}\n${bootstrapped.stderr}`);
assert.match(bootstrapped.stdout, /vm-a: helper [0-9a-f]{12} installed/);
assert.match(bootstrapped.stdout, /vm-b: helper [0-9a-f]{12} installed/);
const again = deploy('network', 'bootstrap', '-C', '/lab/project');
assert.equal(again.status, 0, again.stderr);
assert.match(again.stdout, /vm-a: helper \w+ already installed\nvm-b: helper \w+ already installed$/);
pass('bootstrap installs the helper with operator sudo once and is a no-op afterwards');

for (const host of ['a', 'b']) {
  assert.match(succeed(helper(host, 'version')), /^deploy-network 1 [0-9a-f]{64}$/);
  assert.equal(ssh(host, `stat -c '%U %G %a' ${helperPath} /etc/sudoers.d/deploy-network`), 'root root 755\nroot root 440');
  refused(asDeployer(host, 'sudo -n true'), /password is required|a terminal is required/);
  refused(asDeployer(host, `sudo -n ${helperPath} check`), /password is required|a terminal is required|not allowed/);
  refused(asDeployer(host, `sudo -n ${helperPath} restore ${iface}`), /password is required|a terminal is required|not allowed/);
}
pass('the deploy user can run only the helper verbs, and nothing else as root');

const identity = `runtime ${runtime}\nenvironment production\n`;
const interfaceInput = (address, port = 51821) => `${identity}cidr 10.78.0.0/24\naddress ${address}\nlisten ${port}\n`;
refused(helper('a', 'enroll', interfaceInput('10.78.0.1', 51820)), /UDP port 51820 is already in use/);
refused(helper('a', 'enroll', `${identity}cidr 10.231.0.0/24\naddress 10.231.0.9\nlisten 51821\n`), /overlaps the existing route/);
const squatter = 'dw' + createHash('sha256').update('fb00000b').digest('hex').slice(0, 12);
ssh('a', `ip link add ${squatter} type dummy`);
refused(helper('a', 'enroll', `runtime fb00000b\nenvironment production\ncidr 10.79.0.0/24\naddress 10.79.0.1\nlisten 51822\n`), /exists without an owner manifest/);
ssh('a', `ip link delete ${squatter}`);
refused(helper('a', 'apply', interfaceInput('10.78.0.1')), /not enrolled/);
assert.equal(ssh('a', `test -e /var/lib/deploy-network/${iface} && echo present || echo absent`), 'absent');
pass('enroll refuses a used port, an overlapping subnet and an interface it does not own');

const keys = {};
for (const [host, address] of [['a', '10.78.0.1'], ['b', '10.78.0.2']]) {
  const enrolled = succeed(helper(host, 'enroll', interfaceInput(address)));
  keys[host] = enrolled.match(/^publicKey (\S+)$/m)[1];
  assert.equal(succeed(helper(host, 'enroll', interfaceInput(address))), enrolled);
  assert.equal(ssh(host, `stat -c '%a' /etc/wireguard/${iface}.key /var/lib/deploy-network/${iface}/owner.json`), '600\n600');
}
const desired = {
  a: `${interfaceInput('10.78.0.1')}peer ${keys.b} 10.78.0.2 - 0\naccess 10.78.0.2 6390\n`,
  b: `${interfaceInput('10.78.0.2')}peer ${keys.a} 10.78.0.1 10.231.0.1:51821 25\n`,
};
const applied = {};
for (const host of ['a', 'b']) {
  applied[host] = succeed(helper(host, 'apply', desired[host]));
  assert.match(applied[host], /^changed config$/m);
}
ssh('a', `docker run -d --name helper-provider --restart=no -p 10.78.0.1:6390:6379 ${image} >/dev/null`);
const reach = (from = '') => ssh('b', `if nc ${from} -z -w 3 10.78.0.1 6390; then echo open; else echo denied; fi`);
await until('provider reachable through the managed network', () => reach() === 'open', 60000);
assert.match(ssh('a', `wg show ${iface} endpoints`), /10\.231\.0\.2:51821/);
ssh('b', 'ip route add 10.78.0.1/32 dev underlay table 101; ip rule add priority 51 from 10.231.0.2 lookup 101');
assert.equal(reach('-s 10.231.0.2'), 'denied');
ssh('b', 'ip rule del priority 51; ip route flush table 101');
pass('enroll and apply bring up the private link; the provider port admits its consumer and nothing routed around it');

const snapshot = host => ssh(host, `sha256sum /etc/wireguard/${iface}.conf; stat -c %Y /etc/wireguard/${iface}.conf /var/lib/deploy-network/${iface}/*; ip -4 route show dev ${iface}; iptables-save -t filter | grep -v '^#'; systemctl show wg-quick@${iface} deploy-network-${iface} -p ActiveEnterTimestampMonotonic`)
  .replace(/\[\d+:\d+\]/g, '[0:0]');
for (const host of ['a', 'b']) {
  const unchanged = snapshot(host);
  assert.equal(succeed(helper(host, 'apply', desired[host])), 'unchanged');
  assert.equal(snapshot(host), unchanged);
}
pass('an unchanged apply changes nothing: files, routes, rules and units are untouched');

const observed = succeed(helper('a', 'observe', identity));
const secret = ssh('a', `cat /etc/wireguard/${iface}.key`);
for (const output of [observed, applied.a, ...Object.values(keys)]) {
  assert.ok(!output.includes(secret), 'the private key was printed');
}
assert.match(observed, new RegExp(`^publicKey ${keys.a.replace(/[+/]/g, '\\$&')}$`, 'm'));
assert.match(observed, /^owner present$/m);
assert.match(observed, /^wireguard enabled active$/m);
assert.match(observed, /^boot enabled active$/m);
assert.match(observed, /^listen 51821$/m);
assert.match(observed, /^route 10\.78\.0\.2$/m);
assert.match(observed, /^firewall matches$/m);
assert.match(observed, /^jump DOCKER-USER present$/m);
const digest = createHash('sha256').update(desired.a).digest('hex');
assert.match(observed, new RegExp(`^applied ${digest}$`, 'm'));
assert.match(succeed(helper('a', 'observe', 'runtime fb00000c\nenvironment production\n')), /^owner absent$/m);
pass('observe reports ownership, keys, units, routes and firewall state without secrets');

ssh('a', `iptables -F ${chain}-FWD; iptables -D DOCKER-USER -j ${chain}-FWD; ip route del 10.78.0.2/32 dev ${iface}`);
const drifted = succeed(helper('a', 'observe', identity));
assert.match(drifted, /^firewall drifted$/m);
assert.match(drifted, /^jump DOCKER-USER absent$/m);
const repaired = succeed(helper('a', 'apply', desired.a));
assert.match(repaired, /^changed firewall$/m);
assert.match(repaired, /^changed jump DOCKER-USER$/m);
assert.match(repaired, /^changed route 10\.78\.0\.2$/m);
assert.match(succeed(helper('a', 'observe', identity)), /^firewall matches$/m);
assert.equal(reach(), 'open');
pass('apply repairs flushed chains, a removed jump and a lost route');

const started = ssh('b', `systemctl show wg-quick@${iface} -p ActiveEnterTimestampMonotonic`);
const changedPeer = succeed(helper('b', 'apply', desired.b.replace(' 25\n', ' 15\n')));
assert.match(changedPeer, /^changed peers$/m);
assert.equal(ssh('b', `systemctl show wg-quick@${iface} -p ActiveEnterTimestampMonotonic`), started);
assert.match(ssh('b', `wg show ${iface} persistent-keepalive`), /\t15$/);
assert.equal(reach(), 'open');
pass('a peer change syncs in place without restarting WireGuard');

const rejected = helper('a', 'apply', desired.a.replace('access 10.78.0.2 6390', 'access 10.78.0.2 6390; reboot'));
refused(rejected, /access needs source and port/);
refused(helper('a', 'apply', desired.a.replace('runtime fb00000a', 'runtime fb00000a-staging')), /production uses the bare project id/);
assert.equal(succeed(helper('a', 'apply', desired.a)), 'unchanged');
pass('malformed or mismatched input is refused before anything changes');

ssh('a', 'docker rm -f helper-provider >/dev/null');
for (const host of ['a', 'b']) {
  assert.equal(succeed(helper(host, 'remove', identity)), `removed ${iface}`);
  assert.equal(succeed(helper(host, 'remove', identity)), `absent ${iface}`);
  assert.equal(ssh(host, `ip link show ${iface} >/dev/null 2>&1 && echo link; ls /etc/wireguard /var/lib/deploy-network /etc/systemd/system | grep -c ${iface} || true; iptables-save | grep -c ${chain} || true`), '0\n0');
  assert.equal(foreign(host), before[host]);
}
const ping = () => ssh('b', 'timeout 4 docker exec -e REDISCLI_AUTH=disposable-network-test consumer redis-cli -h 10.77.0.1 ping');
await until('unowned network still serving', () => ping() === 'PONG');
pass('remove deletes everything the runtime owned and leaves wg0, its rules and UFW exactly as they were');
