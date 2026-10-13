import { createHash } from 'node:crypto';
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { ssh, assert, until, pass, inContainer, asDeployer, succeed } from './common.mjs';

// acceptance for the managed network, after helper.mjs and reconcile.mjs:
// production and staging side by side, a conflicting project, reboot, UFW and
// Docker restarts, route loss, an unsupported host, a missing DOCKER-USER chain,
// a rebuilt peer, an apply that fails halfway and logs that never carry a
// private key.
const lab = process.env.DEPLOY_NETWORK_LAB;
const image = 'redis@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499';
const helperPath = '/usr/local/libexec/deploy-network';
const interfaceOf = runtime => 'dw' + createHash('sha256').update(runtime).digest('hex').slice(0, 12);
const production = interfaceOf('fb00000a');
const staging = interfaceOf('fb00000a-staging');
const outputs = [];
const deploy = (directory, ...args) => {
  const result = inContainer(['/lab/bin/deploy', 'network', ...args, '-C', directory]);
  outputs.push(result.stdout, result.stderr);
  return result;
};
const network = (...args) => deploy('/lab/project', ...args);
const plan = environment => succeed(network('plan', '-e', environment));
let lastPlan = '';
const unchanged = environment => {
  lastPlan = plan(environment);
  return /nothing to change$/.test(lastPlan) || lastPlan;
};
const untilUnchanged = async (label, environments, timeout = 60000) => {
  try {
    await until(label, () => environments.every(environment => unchanged(environment) === true), timeout);
  } catch (error) {
    throw new Error(`${error.message}; last plan:\n${lastPlan}`);
  }
};
const reach = (address, from = '') => ssh('b', `if nc ${from} -z -w 3 ${address} 6390; then echo open; else echo denied; fi`);

const config = `${lab}/project/.deploy.json`;
const project = JSON.parse(readFileSync(config, 'utf8'));
project.environments = {
  production: {},
  staging: {
    network: {
      cidr: '10.79.0.0/24', peers: {
        a: { address: '10.79.0.1', listenPort: 51822, endpoint: '10.231.0.1:51822' },
        b: { address: '10.79.0.2', listenPort: 51822, keepalive: 25 },
      },
    },
  },
};
writeFileSync(config, JSON.stringify(project));

for (const environment of ['production', 'staging']) {
  succeed(network('apply', '-e', environment));
}
ssh('a', `docker run -d --name production-provider --restart=unless-stopped -p 10.78.0.1:6390:6379 ${image} >/dev/null`);
ssh('a', `docker run -d --name staging-provider --restart=unless-stopped -p 10.79.0.1:6390:6379 ${image} >/dev/null`);
await until('production provider', () => reach('10.78.0.1') === 'open', 60000);
await until('staging provider', () => reach('10.79.0.1') === 'open', 60000);
assert.notEqual(production, staging);
assert.equal(reach('10.78.0.1', '-s 10.79.0.2'), 'denied');
assert.equal(unchanged('production'), true);
assert.equal(unchanged('staging'), true);
pass('production and staging run side by side on separate interfaces, and staging cannot reach production');

mkdirSync(`${lab}/other`, { recursive: true });
const other = structuredClone(project);
delete other.environments;
other.id = 'fb00000c';
other.destinations = { a: 'vm-a:Projects/other', b: 'vm-b:Projects/other' };
writeFileSync(`${lab}/other/.deploy.json`, JSON.stringify(other));
let conflict = deploy('/lab/other', 'apply');
assert.notEqual(conflict.status, 0);
assert.match(conflict.stderr, /UDP port 51821 is already in use/);
other.network.peers.a.listenPort = other.network.peers.b.listenPort = 51823;
other.network.peers.a.endpoint = '10.231.0.1:51823';
writeFileSync(`${lab}/other/.deploy.json`, JSON.stringify(other));
conflict = deploy('/lab/other', 'apply');
assert.notEqual(conflict.status, 0);
assert.match(conflict.stderr, /overlaps the existing route/);
assert.equal(ssh('a', `test -e /var/lib/deploy-network/${interfaceOf('fb00000c')} && echo owned || echo absent`), 'absent');
assert.equal(ssh('a', 'ls /home/deployer/Projects/other/.deploy-locks 2>/dev/null | grep -c operation || true'), '0');
assert.equal(unchanged('production'), true);
assert.equal(unchanged('staging'), true);
pass('another project cannot take a used port or an overlapping subnet, and leaves no operation behind');

const boot = ssh('a', 'cat /proc/sys/kernel/random/boot_id');
try { ssh('a', 'systemctl reboot'); } catch {}
await until('guest a rebooted', () => ssh('a', 'cat /proc/sys/kernel/random/boot_id') !== boot, 120000);
await until('production after reboot', () => reach('10.78.0.1') === 'open', 120000);
await until('staging after reboot', () => reach('10.79.0.1') === 'open', 60000);
await untilUnchanged('plans after reboot', ['production', 'staging']);
pass('a reboot restores both networks and their firewall from the boot units, with nothing to apply');

ssh('a', 'ufw reload >/dev/null');
assert.equal(reach('10.78.0.1'), 'open');
ssh('a', 'systemctl restart docker');
await until('production after docker restart', () => reach('10.78.0.1') === 'open', 90000);
await untilUnchanged('plans after docker restart', ['production', 'staging']);
pass('UFW reload and a Docker restart keep the owned chains and jumps in place');

ssh('b', `ip route del 10.78.0.1/32 dev ${production}; ip route del 10.78.0.0/24 dev ${production}`);
assert.equal(reach('10.78.0.1'), 'denied');
assert.match(ssh('b', `iptables -nvL DEPLOY-${production.slice(2).toUpperCase()}-OUT`), /^\s*[1-9]\d*\s+\d+\s+REJECT/m);
assert.match(plan('production'), /^  b \(vm-b\): route to 10\.78\.0\.1 is missing$/m);
succeed(network('apply', '-e', 'production'));
assert.equal(reach('10.78.0.1'), 'open');
pass('a lost route fails closed at the egress guard, then plan names it and apply restores it');

ssh('b', 'mv /etc/os-release /etc/os-release.lab');
const unsupported = network('plan', '-e', 'production');
ssh('b', 'mv /etc/os-release.lab /etc/os-release');
assert.notEqual(unsupported.status, 0);
assert.match(unsupported.stderr, /only Debian and Ubuntu hosts are supported/);
assert.equal(unchanged('production'), true);
pass('an unsupported host is refused with the reason before anything changes');

// as on a host where Docker manages nftables directly and never makes the chain
ssh('b', 'iptables -F DOCKER-USER; while iptables -D FORWARD -j DOCKER-USER 2>/dev/null; do :; done; iptables -X DOCKER-USER');
const noDockerChain = network('plan', '-e', 'production');
ssh('b', 'systemctl restart docker');
assert.notEqual(noDockerChain.status, 0);
assert.match(noDockerChain.stderr, /DOCKER-USER is missing/);
await untilUnchanged('plans after docker comes back', ['production', 'staging'], 90000);
assert.equal(reach('10.78.0.1'), 'open');
pass('a host without the DOCKER-USER chain is refused, and the boot unit restores the jumps once Docker makes it again');

const oldKey = succeed(network('status', '-e', 'production')).match(/^b \(vm-b\)[\s\S]*?publicKey +(\S+)$/m)[1];
succeed(asDeployer('b', `sudo -n ${helperPath} remove`, 'runtime fb00000a\nenvironment production\n'));
const rebuilt = plan('production');
assert.match(rebuilt, /^  b \(vm-b\): enroll$/m);
assert.match(rebuilt, /^  a \(vm-a\): waiting for a peer to enroll$/m);
const reenrolled = succeed(network('apply', '-e', 'production'));
assert.match(reenrolled, /^b \(vm-b\): enrolled /m);
const peers = ssh('a', `wg show ${production} peers`);
assert.ok(!peers.includes(oldKey), 'the old key is still a peer');
assert.equal(peers.split('\n').length, 1);
await until('production after re-enrollment', () => reach('10.78.0.1') === 'open', 60000);
assert.equal(unchanged('production'), true);
pass('a rebuilt peer enrolls with a new key, and its old key is revoked on the other host');

const productionRoot = '/home/deployer/Projects/lab/fb00000a';
const productionState = `/var/lib/deploy-network/${production}`;
const generation = host => JSON.parse(ssh(host, `cat ${productionRoot}/network.json`)).generation;
const hostState = host => ssh(host, `sha256sum < /etc/wireguard/${production}.conf; cat ${productionState}/owner.json ${productionState}/applied; iptables -S DEPLOY-${production.slice(2).toUpperCase()}-OUT; ip -4 -o addr show dev ${production} | awk '{print $4}'`);
// hosts apply in the order of their machine identities, which each lab boot draws anew
const [first, second] = [...plan('production').matchAll(/^  ([ab]) \(vm-[ab]\)/gm)].map(match => match[1]);
const before = { first: generation(first), second: generation(second), state: hostState(second) };
project.network.peers.b.address = '10.78.0.3';
writeFileSync(config, JSON.stringify(project));
ssh(second, `chattr +i ${productionState}/applied`);
let halfway;
try {
  halfway = network('apply', '-e', 'production');
} finally {
  ssh(second, `chattr -i ${productionState}/applied`);
}
assert.notEqual(halfway.status, 0);
assert.match(halfway.stderr, new RegExp(`failed on vm-${second}: .*is back to its previous state; hosts before it were applied, apply again`));
assert.equal(generation(first), before.first + 1);
assert.equal(generation(second), before.second);
assert.equal(hostState(second), before.state);
assert.equal(ssh(second, `{ ls -A ${productionState} | grep '^\\.deploy-network' || true; ls -A /run/deploy-network-prior; } | wc -l`), '0');
assert.match(plan('production'), new RegExp(`^  ${second} \\(vm-${second}\\): desired network differs from the applied one`, 'm'));
succeed(network('apply', '-e', 'production'));
assert.equal(generation(second), before.second + 1);
assert.match(ssh(second, `grep -c '10.78.0.2/' ${productionState}/previous/config`), /^[1-9]$/);
await until('production from the moved peer', () => reach('10.78.0.1', '-s 10.78.0.3') === 'open', 60000);
assert.equal(unchanged('production'), true);
pass('an apply that fails on the second host puts that host back as it was, and the next apply converges');

const secrets = ['a', 'b'].flatMap(host => [production, staging].map(name => ssh(host, `cat /etc/wireguard/${name}.key`)));
for (const secret of secrets) {
  assert.ok(!outputs.some(output => output.includes(secret)), 'a private key reached deploy output');
}
pass(`no private key appeared in ${outputs.length} captured deploy outputs`);

ssh('a', 'docker rm -f production-provider staging-provider >/dev/null');
for (const host of ['a', 'b']) {
  for (const environment of ['production', 'staging']) {
    const runtime = environment === 'production' ? 'fb00000a' : 'fb00000a-staging';
    succeed(asDeployer(host, `sudo -n ${helperPath} remove`, `runtime ${runtime}\nenvironment ${environment}\n`));
  }
  ssh(host, 'rm -rf /home/deployer/Projects');
}
pass('acceptance fixture removed');
