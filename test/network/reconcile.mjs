import { spawn } from 'node:child_process';
import { createHash } from 'node:crypto';
import { readFileSync, writeFileSync } from 'node:fs';
import { ssh, assert, until, pass, inContainer, asDeployer, succeed } from './common.mjs';

// deploy network status, plan and apply against the guests, after helper.mjs
// has bootstrapped the helper and written the project and ssh config.
const lab = process.env.DEPLOY_NETWORK_LAB;
const container = process.env.DEPLOY_NETWORK_CONTAINER;
const image = 'redis@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499';
const runtime = 'fb00000a';
const iface = 'dw' + createHash('sha256').update(runtime).digest('hex').slice(0, 12);
const chain = 'DEPLOY-' + iface.slice(2).toUpperCase();
const root = `/home/deployer/Projects/lab/${runtime}`;
const deploy = (...args) => inContainer(['/lab/bin/deploy', 'network', ...args, '-C', '/lab/project']);
const record = host => JSON.parse(ssh(host, `cat ${root}/network.json`));
const recordTime = host => ssh(host, `stat -c %Y ${root}/network.json`);
const config = `${lab}/project/.deploy.json`;

let planned = succeed(deploy('plan'));
assert.match(planned, /^  a \(vm-a\): enroll$/m);
assert.match(planned, /^  b \(vm-b\): enroll$/m);
assert.match(planned, /would change 2 of 2 hosts$/);
assert.match(succeed(deploy('status')), /owner +none; apply enrolls it/);
assert.equal(ssh('a', `test -e ${root} && echo present || echo absent`), 'absent');
pass('plan and status read both hosts and change nothing');

const applied = succeed(deploy('apply'));
assert.match(applied, /^a \(vm-a\): enrolled dw/m);
assert.match(applied, /^b \(vm-b\): enrolled dw/m);
assert.match(applied, /^a \(vm-a\): .*changed config/m);
assert.match(applied, /^b \(vm-b\): .*changed config/m);
for (const host of ['a', 'b']) {
  const written = record(host);
  assert.equal(written.generation, 1);
  assert.equal(written.destination, host);
  assert.equal(written.interface, iface);
  assert.equal(ssh(host, `ls /home/deployer/Projects/lab/.deploy-locks | grep -c operation || true`), '0');
}
ssh('a', `docker run -d --name reconcile-provider --restart=no -p 10.78.0.1:6390:6379 ${image} >/dev/null`);
const reach = () => ssh('b', 'if nc -z -w 3 10.78.0.1 6390; then echo open; else echo denied; fi');
await until('provider reachable after deploy network apply', () => reach() === 'open', 60000);
pass('apply enrolls both hosts, exchanges keys, brings up the link and records generation 1');

planned = succeed(deploy('plan'));
assert.match(planned, /^  a \(vm-a\): unchanged$/m);
assert.match(planned, /^  b \(vm-b\): unchanged$/m);
assert.match(planned, /nothing to change$/);
const times = { a: recordTime('a'), b: recordTime('b') };
const again = succeed(deploy('apply'));
assert.match(again, /^a \(vm-a\): unchanged$/m);
assert.match(again, /nothing changed$/);
assert.deepEqual({ a: recordTime('a'), b: recordTime('b') }, times);
assert.equal(record('a').generation, 1);
pass('an unchanged network plans nothing and a repeated apply writes nothing');

const status = succeed(deploy('status'));
for (const host of ['a', 'b']) {
  assert.ok(!status.includes(ssh(host, `cat /etc/wireguard/${iface}.key`)), 'status printed a private key');
}
assert.match(status, /state +matches the desired network/);
pass('status shows both hosts without secrets');

ssh('a', `iptables -F ${chain}-FWD; iptables -D DOCKER-USER -j ${chain}-FWD`);
planned = succeed(deploy('plan'));
assert.match(planned, /^  a \(vm-a\): firewall chains drifted; jump from DOCKER-USER is missing$/m);
assert.match(planned, /^  b \(vm-b\): unchanged$/m);
const repaired = succeed(deploy('apply'));
assert.match(repaired, /^a \(vm-a\): .*changed firewall/m);
assert.match(repaired, /^b \(vm-b\): unchanged$/m);
assert.equal(record('a').generation, 2);
assert.equal(record('b').generation, 1);
assert.equal(reach(), 'open');
pass('drift on one host is planned, repaired and recorded there only');

const project = JSON.parse(readFileSync(config, 'utf8'));
project.network.peers.b.keepalive = 15;
writeFileSync(config, JSON.stringify(project));
planned = succeed(deploy('plan'));
assert.match(planned, /^  b \(vm-b\): desired network differs from the applied one$/m);
assert.match(planned, /^  a \(vm-a\): unchanged$/m);
const started = ssh('b', `systemctl show wg-quick@${iface} -p ActiveEnterTimestampMonotonic`);
const reconfigured = succeed(deploy('apply'));
assert.equal(ssh('b', `systemctl show wg-quick@${iface} -p ActiveEnterTimestampMonotonic`), started);
assert.match(reconfigured, /^b \(vm-b\): .*changed peers/m);
assert.match(ssh('b', `wg show ${iface} persistent-keepalive`), /\t15$/);
assert.equal(record('b').generation, 2);
pass('a config change reaches only the host it affects, without restarting WireGuard');

const holder = spawn('docker', ['exec', '-i', container, '/lab/bin/ssh', 'vm-a',
  `exec 9>>/home/deployer/Projects/lab/.deploy-locks/${runtime}; flock -n 9; echo held; read -r ignored`], { stdio: ['pipe', 'pipe', 'pipe'] });
await new Promise((resolve, reject) => {
  holder.stdout.once('data', chunk => String(chunk).includes('held') ? resolve() : reject(new Error(String(chunk))));
  holder.once('exit', code => reject(new Error(`lock holder exited ${code}`)));
});
ssh('a', `iptables -F ${chain}-FWD`);
const blocked = deploy('apply');
assert.notEqual(blocked.status, 0);
assert.match(blocked.stderr, /lock|another deploy/i);
assert.match(succeed(deploy('plan')), /^  a \(vm-a\): firewall chains drifted$/m);
holder.stdin.end();
await new Promise(resolve => holder.once('close', resolve));
assert.match(succeed(deploy('apply')), /^a \(vm-a\): .*changed firewall/m);
pass('a held destination lock refuses apply before any host changes');

ssh('a', 'docker rm -f reconcile-provider >/dev/null');
for (const host of ['a', 'b']) {
  assert.equal(succeed(asDeployer(host, 'sudo -n /usr/local/libexec/deploy-network remove', `runtime ${runtime}\nenvironment production\n`)), `removed ${iface}`);
  ssh(host, 'rm -rf /home/deployer/Projects');
}
pass('reconcile fixture removed');
