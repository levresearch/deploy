import { createHash } from 'node:crypto';
import { mkdirSync, writeFileSync } from 'node:fs';
import { ssh, assert, until, pass, inContainer, asDeployer, succeed } from './common.mjs';

// a real version 2 deploy across both guests, after helper.mjs bootstrapped
// the helper: deploy provisions the managed network itself, the consumer on b
// reaches the provider on a with authentication, and the services come back
// after a reboot. the project builds nothing, so the tools container needs no
// Docker daemon of its own.
const lab = process.env.DEPLOY_NETWORK_LAB;
const image = 'redis@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499';
const helperPath = '/usr/local/libexec/deploy-network';
const runtime = 'fb00000d';
const iface = 'dw' + createHash('sha256').update(runtime).digest('hex').slice(0, 12);
const root = `/home/deployer/Projects/app/${runtime}`;
const git = (...args) => succeed(inContainer(['git', '-C', '/lab/app', '-c', 'user.name=lab', '-c', 'user.email=lab@example.invalid', ...args]));
const deploy = (...args) => inContainer(['/lab/bin/deploy', ...args, '-C', '/lab/app']);
const exec = (service, ...command) => inContainer(['/lab/bin/deploy', 'exec', service, '-C', '/lab/app', '--', ...command]);

const config = {
  version: 2, id: runtime, name: 'app',
  destinations: { a: 'vm-a:Projects/app', b: 'vm-b:Projects/app' },
  network: {
    cidr: '10.76.0.0/24', peers: {
      a: { address: '10.76.0.1', listenPort: 51824, endpoint: '10.231.0.1:51824' },
      b: { address: '10.76.0.2', listenPort: 51824, keepalive: 25 },
    },
  },
  services: {
    cache: {
      destination: 'a', image, stateful: true,
      command: ['redis-server', '--requirepass', 'lab-only'],
      environment: { REDISCLI_AUTH: 'lab-only' },
      healthcheck: { command: ['CMD', 'redis-cli', 'ping'], interval: '2s', retries: 15 },
      endpoints: { resp: { port: 6379, hostPort: 6391, protocol: 'tcp' } },
    },
    api: {
      destination: 'b', image, dependsOn: { cache: 'healthy' }, env: ['api.env'],
      environment: { REDISCLI_AUTH: 'lab-only', REVISION: 'one' },
      healthcheck: { command: ['CMD-SHELL', 'REDISCLI_AUTH= redis-cli ping'], interval: '2s', retries: 15 },
      connections: { cache: { service: 'cache', endpoint: 'resp', hostEnv: 'CACHE_HOST', portEnv: 'CACHE_PORT' } },
    },
  },
};
const commit = message => {
  writeFileSync(`${lab}/app/.deploy.json`, JSON.stringify(config, null, 1));
  git('add', '-A');
  git('commit', '-q', '-m', message);
  return git('rev-parse', '--short=7', 'HEAD');
};
const state = host => JSON.parse(ssh(host, `cat ${root}/services.json`));
const container = (host, service) => ssh(host, `docker ps --filter label=com.docker.compose.service=${service} --filter label=com.docker.compose.project --format '{{.Names}}' | grep '^deploy-${runtime}'`);
const viaApi = (command) => ssh('b', `docker exec ${container('b', 'api')} sh -c '${command}' 2>&1 || true`);

mkdirSync(`${lab}/app`, { recursive: true });
git('init', '-q', '-b', 'main');
const first = commit('first');
writeFileSync(`${lab}/api.env`, 'GREETING=from the env file\n', { mode: 0o600 });
const pushed = deploy('env', 'push', '/lab/api.env');
assert.equal(pushed.status, 0, pushed.stderr);
assert.match(pushed.stdout, /pushed api\.env to .* on vm-b/);
assert.doesNotMatch(pushed.stdout, /vm-a/);
assert.equal(ssh('a', `test -e ${root}/env/api.env && echo present || echo absent`), 'absent');
assert.equal(ssh('b', `cat ${root}/env/api.env`), 'GREETING=from the env file');
writeFileSync(`${lab}/unused.env`, 'NOTHING=here\n', { mode: 0o600 });
const unused = deploy('env', 'push', '/lab/unused.env');
assert.notEqual(unused.status, 0);
assert.match(unused.stderr, /lists unused\.env, so it has nowhere to go/);
pass('env push sends a file only to the destination whose service lists it');

let result = deploy();
assert.equal(result.status, 0, `${result.stdout}\n${result.stderr}`);
assert.match(result.stdout, /published images, nothing to build/);
assert.match(result.stdout, /managed network\n {4}[ab] \(vm-[ab]\): enrolled /);
assert.match(result.stdout, /api reaches cache at 10\.76\.0\.1:6391/);
assert.equal(viaApi('redis-cli -h $CACHE_HOST -p $CACHE_PORT ping'), 'PONG');
assert.match(viaApi('REDISCLI_AUTH= redis-cli -h $CACHE_HOST -p $CACHE_PORT ping'), /NOAUTH/);
assert.equal(viaApi('echo $CACHE_HOST:$CACHE_PORT'), '10.76.0.1:6391');
assert.match(ssh('b', 'nc -z -w 3 10.231.0.1 6391 && echo open || echo closed'), /closed/);
assert.equal(state('a').destination, 'a');
assert.equal(state('b').services.api.current.commit, first);
assert.equal(ssh('a', `cat /var/lib/deploy-network/${iface}/workloads`), `deploy-${runtime}-shared-cache-1`);
assert.equal(ssh('b', `cat /var/lib/deploy-network/${iface}/workloads`), `deploy-${runtime}_api_${first}-api-1`);
pass('a fresh deploy across two hosts provisions the network and the consumer reaches its provider with authentication only');

const cacheBefore = ssh('a', `docker inspect -f '{{.Id}}' deploy-${runtime}-shared-cache-1`);
const networkGeneration = JSON.parse(ssh('a', `cat ${root}/network.json`)).generation;
result = deploy();
assert.equal(result.status, 0, `${result.stdout}\n${result.stderr}`);
assert.match(result.stdout, /managed network[\s\S]*nothing changed/);
assert.equal(JSON.parse(ssh('a', `cat ${root}/network.json`)).generation, networkGeneration);
assert.equal(ssh('a', `docker inspect -f '{{.Id}}' deploy-${runtime}-shared-cache-1`), cacheBefore);
pass('deploying the same commit again changes no network and leaves the provider alone');

config.services.api.environment.REVISION = 'two';
const second = commit('second');
result = deploy();
assert.equal(result.status, 0, `${result.stdout}\n${result.stderr}`);
assert.equal(state('b').services.api.current.commit, second);
assert.equal(state('b').services.api.previous.commit, first);
assert.equal(ssh('a', `docker inspect -f '{{.Id}}' deploy-${runtime}-shared-cache-1`), cacheBefore);
assert.equal(viaApi('echo $REVISION'), 'two');
assert.equal(ssh('b', `cat /var/lib/deploy-network/${iface}/workloads`), `deploy-${runtime}_api_${second}-api-1`);
pass('a change to the consumer moves only the consumer, and its boot record follows it');

config.services.api.environment.REVISION = 'broken';
config.services.api.healthcheck = { command: ['CMD', 'false'], interval: '1s', retries: 3 };
commit('broken');
result = deploy();
assert.notEqual(result.status, 0);
assert.match(result.stderr, /destination b: .*never became healthy/);
assert.equal(state('b').services.api.current.commit, second);
assert.equal(viaApi('echo $REVISION'), 'two');
assert.equal(viaApi('redis-cli -h $CACHE_HOST -p $CACHE_PORT ping'), 'PONG');
for (const host of ['a', 'b']) {
  assert.equal(ssh(host, 'ls /home/deployer/Projects/app/.deploy-locks 2>/dev/null | grep -c operation || true'), '0');
}
pass('a candidate that fails on the second destination leaves both destinations on what was live and recorded as such');

const status = succeed(deploy('status'));
assert.match(status, /app on vm-a:\S+[\s\S]*cache +shared[\s\S]*\n\napp on vm-b:\S+[\s\S]*api +\w{7}/);
assert.match(succeed(deploy('releases')), new RegExp(`api\\n\\* ${second}\\n- ${first}`));
assert.equal(succeed(exec('api', 'printenv', 'CACHE_HOST')).split('\n').pop(), '10.76.0.1');
assert.equal(succeed(exec('api', 'printenv', 'GREETING')).split('\n').pop(), 'from the env file');
const unknown = exec('nope', 'true');
assert.notEqual(unknown.status, 0);
assert.match(unknown.stderr, /no service called "nope" on any destination/);
pass('status and releases cover every destination, and exec finds the destination a service runs on');

const notSelected = deploy('--service', 'nope');
assert.notEqual(notSelected.status, 0);
assert.match(notSelected.stderr, /--service names "nope"/);
config.services.api.environment.REVISION = 'three';
config.services.api.healthcheck = { command: ['CMD-SHELL', 'REDISCLI_AUTH= redis-cli ping'], interval: '2s', retries: 15 };
const third = commit('third');
result = deploy('--service', 'api');
assert.equal(result.status, 0, `${result.stdout}\n${result.stderr}`);
assert.equal(state('b').services.api.current.commit, third);
assert.equal(ssh('a', `docker inspect -f '{{.Id}}' deploy-${runtime}-shared-cache-1`), cacheBefore);
pass('deploy --service api moves only api');

result = deploy('rollback', '--service', 'api');
assert.equal(result.status, 0, `${result.stdout}\n${result.stderr}`);
assert.equal(state('b').services.api.current.commit, second);
assert.equal(viaApi('echo $REVISION'), 'two');
assert.deepEqual(state('b').services.api.paths[second], ['connect 10.76.0.1:6391']);
assert.equal(ssh('b', `cat /var/lib/deploy-network/${iface}/workloads`), `deploy-${runtime}_api_${second}-api-1`);
pass('rollback across destinations moves api back on b and its boot record follows it');

const boot = ssh('b', 'cat /proc/sys/kernel/random/boot_id');
try { ssh('b', 'systemctl reboot'); } catch {}
await until('guest b rebooted', () => ssh('b', 'cat /proc/sys/kernel/random/boot_id') !== boot, 120000);
await until('api after reboot', () => { try { return viaApi('redis-cli -h $CACHE_HOST -p $CACHE_PORT ping') === 'PONG'; } catch { return false; } }, 120000);
pass('after the consumer host reboots, the network unit starts the live release and it reaches its provider again');

const cacheMove = structuredClone(config);
cacheMove.services.cache.destination = 'b';
delete cacheMove.services.api.connections;
writeFileSync(`${lab}/app/.deploy.json`, JSON.stringify(cacheMove, null, 1));
git('commit', '-q', '-am', 'move the cache');
result = deploy();
assert.notEqual(result.status, 0);
assert.match(result.stderr, /stateful cache moves from a to b, but its data stays on a/);
assert.equal(ssh('a', `docker inspect -f '{{.Id}}' deploy-${runtime}-shared-cache-1`), cacheBefore);
git('revert', '--no-edit', 'HEAD');
pass('moving a stateful service to another host is refused before anything changes');

config.services.api.destination = 'a';
const moved = commit('move the api next to the cache');
assert.match(succeed(deploy('env', 'push', '/lab/api.env')), /on vm-a/);
result = deploy();
assert.equal(result.status, 0, `${result.stdout}\n${result.stderr}`);
assert.match(result.stdout, /retired api from b; it now runs on a/);
assert.equal(state('a').services.api.current.commit, moved);
assert.equal(state('b').services.api, undefined);
assert.equal(ssh('b', `docker ps -q --filter label=com.docker.compose.service=api | wc -l`), '0');
assert.equal(ssh('a', `docker exec $(docker ps -q --filter label=com.docker.compose.service=api) sh -c 'redis-cli -h $CACHE_HOST -p $CACHE_PORT ping'`), 'PONG');
assert.equal(ssh('a', `docker exec $(docker ps -q --filter label=com.docker.compose.service=api) printenv CACHE_HOST`), 'cache');
pass('a stateless service moved to the provider\'s host runs there over Docker networking and is retired from the host it left');

result = inContainer(['/lab/bin/deploy', 'destroy', '-D', 'b', '-C', '/lab/app'], 'app\n');
assert.equal(result.status, 0, `${result.stdout}\n${result.stderr}`);
assert.match(result.stdout, new RegExp(`removing   managed network ${iface} on vm-b`));
assert.deepEqual(state('b').services, {});
assert.equal(ssh('b', `cat ${root}/env/api.env`), 'GREETING=from the env file');
assert.equal(ssh('b', `ip link show ${iface} >/dev/null 2>&1 && echo up || echo gone`), 'gone');
assert.equal(ssh('a', `docker exec $(docker ps -q --filter label=com.docker.compose.service=api) sh -c 'redis-cli -h $CACHE_HOST -p $CACHE_PORT ping'`), 'PONG');
pass('destroy -D b removes that destination and its network, which nothing links any more, and leaves a serving');

result = inContainer(['/lab/bin/deploy', 'destroy', '--volumes', '-C', '/lab/app'], 'app\n');
assert.equal(result.status, 0, `${result.stdout}\n${result.stderr}`);
assert.match(result.stdout, new RegExp(`removed managed network ${iface} on vm-a`));
for (const host of ['a', 'b']) {
  assert.equal(ssh(host, `docker ps -aq --filter label=com.docker.compose.project | xargs -r docker inspect -f '{{.Name}}' | grep -c '^/deploy-${runtime}' || true`), '0');
  assert.equal(ssh(host, `ip link show ${iface} >/dev/null 2>&1 && echo up || echo gone; test -e /var/lib/deploy-network/${iface} && echo owned || echo free; test -e ${root} && echo kept || echo removed`), 'gone\nfree\nremoved');
}
pass('destroy --volumes removes every destination and the managed network, leaving nothing of the project');
ssh('a', 'rm -rf /home/deployer/Projects/app');
ssh('b', 'rm -rf /home/deployer/Projects/app');
