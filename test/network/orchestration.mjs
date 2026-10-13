import { spawn } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdirSync, writeFileSync, readFileSync } from 'node:fs';
import { ssh, assert, until, pass, inContainer, succeed } from './common.mjs';

// the rest of the multi-host exit gate, after helper.mjs: a readiness graph
// that returns to a host, a fixed-port provider replaced in place, a network
// partition caught before anything switches, a release task cut off mid-run
// and an interrupted cutover, both recovered by a second operator from a
// separate clone with its own key.
const lab = process.env.DEPLOY_NETWORK_LAB;
const container = process.env.DEPLOY_NETWORK_CONTAINER;
const image = 'redis@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499';
const sleeper = 'busybox@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662';
const runtime = 'fb00000e';
const iface = 'dw' + createHash('sha256').update(runtime).digest('hex').slice(0, 12);
const root = `/home/deployer/Projects/orch/${runtime}`;
const UDP = 51825;

// the second operator: its own clone, key, ssh config and known hosts
const id2 = inContainer(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', '/lab/id2']);
assert.equal(id2.status, 0, id2.stderr);
const publicKey = readFileSync(`${lab}/id2.pub`, 'utf8').trim();
for (const host of ['a', 'b']) {
  ssh(host, `echo '${publicKey}' >> /home/deployer/.ssh/authorized_keys`);
}
writeFileSync(`${lab}/ssh_config2`, readFileSync(`${lab}/ssh_config`, 'utf8')
  .replace('/lab/id_ed25519', '/lab/id2').replace('/lab/known_hosts', '/lab/known_hosts2'), { mode: 0o600 });
mkdirSync(`${lab}/bin2`, { recursive: true });
writeFileSync(`${lab}/bin2/ssh`, '#!/bin/sh\nexec /usr/bin/ssh -F /lab/ssh_config2 "$@"\n', { mode: 0o755 });
writeFileSync(`${lab}/bin2/deploy`, '#!/bin/sh\nPATH=/lab/bin2:$PATH exec /lab/deploy "$@"\n', { mode: 0o755 });

const git = (directory, ...args) => succeed(inContainer(['git', '-C', directory, '-c', 'user.name=lab', '-c', 'user.email=lab@example.invalid', ...args]));
const first = (...args) => inContainer(['/lab/bin/deploy', ...args, '-C', '/lab/orch']);
const second = (...args) => inContainer(['/lab/bin2/deploy', ...args, '-C', '/lab/orch2']);
const state = host => JSON.parse(ssh(host, `cat ${root}/services.json`));
const named = (host, service) => ssh(host, `docker ps -q --filter label=com.docker.compose.service=${service} --filter label=com.docker.compose.project | head -n 1`);
const inside = (host, service, command) => ssh(host, `docker exec ${named(host, service)} sh -c '${command}' 2>&1 || true`);
let lastReply = '';
const webReachesApi = () => {
  try {
    lastReply = inside('a', 'web', 'redis-cli -h $API_HOST -p $API_PORT ping');
  } catch (error) {
    lastReply = String(error);
  }
  return lastReply === 'PONG';
};

const config = {
  version: 2, id: runtime, name: 'orch',
  destinations: { a: 'vm-a:Projects/orch', b: 'vm-b:Projects/orch' },
  network: {
    cidr: '10.75.0.0/24', peers: {
      a: { address: '10.75.0.1', listenPort: UDP, endpoint: `10.231.0.1:${UDP}` },
      b: { address: '10.75.0.2', listenPort: UDP, keepalive: 25 },
    },
  },
  services: {
    cache: {
      destination: 'a', image, stateful: true,
      command: ['redis-server', '--requirepass', 'lab-only'],
      environment: { REDISCLI_AUTH: 'lab-only' },
      healthcheck: { command: ['CMD', 'redis-cli', 'ping'], interval: '2s', retries: 15 },
      endpoints: { resp: { port: 6379, hostPort: 6392, protocol: 'tcp' } },
    },
    api: {
      destination: 'b', image, dependsOn: { cache: 'healthy' },
      environment: { REVISION: 'one' },
      healthcheck: { command: ['CMD', 'redis-cli', 'ping'], interval: '2s', retries: 15 },
      endpoints: { resp: { port: 6379, hostPort: 6393, protocol: 'tcp' } },
      connections: { cache: { service: 'cache', endpoint: 'resp', hostEnv: 'CACHE_HOST', portEnv: 'CACHE_PORT' } },
    },
    web: {
      destination: 'a', image, dependsOn: { api: 'healthy' },
      environment: { REVISION: 'one' },
      healthcheck: { command: ['CMD', 'redis-cli', 'ping'], interval: '2s', retries: 15 },
      connections: { api: { service: 'api', endpoint: 'resp', hostEnv: 'API_HOST', portEnv: 'API_PORT' } },
    },
    // after web in readiness, so web switches first and worker's slow stop
    // gives the interrupted cutover below its window
    worker: { destination: 'a', image: sleeper, command: ['sleep', '3600'], dependsOn: { web: 'started' }, environment: { REVISION: 'one' } },
  },
};
const commit = (directory, message) => {
  writeFileSync(`${lab}/${directory}/.deploy.json`, JSON.stringify(config, null, 1));
  git(`/lab/${directory}`, 'add', '-A');
  git(`/lab/${directory}`, 'commit', '-q', '-m', message);
  return git(`/lab/${directory}`, 'rev-parse', '--short=7', 'HEAD');
};
// runs deploy in the background and kills it once its output matches the
// pattern, or once the check passes, as an operator's laptop dying
// mid-deploy would
const killedAt = (moment, ...args) => new Promise((resolve, reject) => {
  const child = spawn('docker', ['exec', '-i', container, '/lab/bin/deploy', ...args, '-C', '/lab/orch'], { stdio: ['ignore', 'pipe', 'pipe'] });
  let output = '';
  let killed = false;
  const kill = () => {
    if (killed) return;
    killed = true;
    clearTimeout(timer);
    clearInterval(poll);
    inContainer(['sh', '-c', 'for p in /proc/[0-9]*; do [ "$(cat $p/comm 2>/dev/null)" = deploy ] && kill -9 ${p#/proc/}; done; true']);
  };
  const timer = setTimeout(() => { clearInterval(poll); reject(new Error(`deploy never reached the moment within 240s:\n${output}`)); }, 240000);
  const poll = setInterval(() => {
    if (typeof moment === 'function' && !killed) {
      try { if (moment()) kill(); } catch {}
    }
  }, 1000);
  const watch = chunk => {
    output += chunk;
    if (moment instanceof RegExp && moment.test(output)) kill();
  };
  child.stdout.on('data', watch);
  child.stderr.on('data', watch);
  child.on('close', () => { clearTimeout(timer); clearInterval(poll); resolve(output); });
});
const migrated = () => ssh('a', `docker exec deploy-${runtime}-shared-cache-1 redis-cli -a lab-only --no-auth-warning get migrated`);

mkdirSync(`${lab}/orch`, { recursive: true });
git('/lab/orch', 'init', '-q', '-b', 'main');
const one = commit('orch', 'one');
let result = first();
assert.equal(result.status, 0, `${result.stdout}\n${result.stderr}`);
assert.match(result.stdout, /api up[\s\S]*web up[\s\S]*worker up/);
assert.match(result.stdout, /web reaches api at 10\.75\.0\.2:6393/);
assert.ok(webReachesApi(), `web cannot reach api: ${lastReply}`);
assert.equal(inside('b', 'api', 'redis-cli -h $CACHE_HOST -p $CACHE_PORT -a lab-only --no-auth-warning ping'), 'PONG');
assert.equal(state('a').services.web.current.commit, one);
assert.equal(state('b').services.api.current.commit, one);
pass('a graph that goes from a to b and back to a starts in readiness order across both hosts');

config.services.api.environment.REVISION = 'two';
const two = commit('orch', 'two');
result = first('--service', 'api');
assert.equal(result.status, 0, `${result.stdout}\n${result.stderr}`);
assert.match(result.stdout, /api publishes a fixed port, so it is replaced in place/);
assert.equal(inside('b', 'api', 'echo $REVISION'), 'two');
assert.equal(ssh('b', `docker ps --filter label=com.docker.compose.service=api -q | wc -l`), '1');
await until('web reaches the replaced api', webReachesApi, 60000);
assert.equal(state('b').services.api.current.commit, two);
pass('a provider publishing a fixed port is replaced in place, and its consumer on the other host reaches the new one');

for (const host of ['a', 'b']) {
  ssh(host, `iptables -I INPUT -p udp --dport ${UDP} -j DROP`);
}
await until('the partition bites', () => !webReachesApi(), 120000);
config.services.web.environment.REVISION = 'two';
commit('orch', 'web two');
result = first('--service', 'web');
assert.notEqual(result.status, 0);
assert.match(result.stderr, /web on a cannot reach api at 10\.75\.0\.2:6393 over the managed network/);
assert.equal(state('a').services.web.current.commit, one);
assert.equal(inside('a', 'web', 'echo $REVISION'), 'one');
for (const host of ['a', 'b']) {
  ssh(host, `iptables -D INPUT -p udp --dport ${UDP} -j DROP`);
}
await until('the partition heals', webReachesApi, 120000);
result = first('--service', 'web');
assert.equal(result.status, 0, `${result.stdout}\n${result.stderr}`);
assert.equal(inside('a', 'web', 'echo $REVISION'), 'two');
pass('a network partition stops the deploy before anything switches, and the next deploy goes through once it heals');

config.release = {
  migrate: {
    destination: 'b', image, dependsOn: { cache: 'healthy' },
    // $$ keeps compose from interpolating these when it reads the file
    command: ['sh', '-c', 'redis-cli -h $$CACHE_HOST -p $$CACHE_PORT set migrated $$REVISION && sleep $$PAUSE'],
    environment: { REDISCLI_AUTH: 'lab-only', REVISION: 'three', PAUSE: '300' },
    connections: { cache: { service: 'cache', endpoint: 'resp', hostEnv: 'CACHE_HOST', portEnv: 'CACHE_PORT' } },
  },
};
const three = commit('orch', 'migrate');
// cut off after the task changed data and before it finished
const cutOffTask = await killedAt(() => migrated() === 'three');
assert.equal(migrated(), 'three', cutOffTask);
const runs = state('b').tasks.migrate;
assert.equal(runs.at(-1).outcome, 'indeterminate');
assert.equal(runs.at(-1).commit, three);

succeed(inContainer(['git', 'clone', '-q', '/lab/orch', '/lab/orch2']));
// a new operator trusts each host once, since deploy never adds host keys
for (const host of ['vm-a', 'vm-b']) {
  succeed(inContainer(['/lab/bin2/ssh', host, 'true']));
}
result = second();
assert.notEqual(result.status, 0);
assert.match(result.stderr, /did not finish .* deploy resolve [0-9a-f]{16}/, result.stderr);
const operation = result.stderr.match(/deploy resolve ([0-9a-f]{16})/)[1];
// what an operator does before resolving: make sure the task it cut off is gone
ssh('b', `docker ps -q --filter name=tasks --filter label=com.docker.compose.service=migrate | xargs -r docker rm -f >/dev/null`);
result = second('resolve', operation);
assert.equal(result.status, 0, `${result.stdout}\n${result.stderr}`);
assert.match(result.stdout, new RegExp(`resolved operation ${operation} on vm-a`));
assert.match(result.stdout, new RegExp(`resolved operation ${operation} on vm-b`));
assert.match(result.stdout, /release task migrate from \w+ marked resolved/);
config.release.migrate.environment.PAUSE = '0';
const four = commit('orch2', 'finish the migration quickly');
result = second();
assert.equal(result.status, 0, `${result.stdout}\n${result.stderr}`);
assert.deepEqual(state('b').tasks.migrate.slice(-2).map(run => run.outcome), ['resolved', 'completed']);
assert.equal(state('a').services.web.current.commit, four);
pass('a release task cut off mid-run blocks every destination until a second operator, from its own clone and key, resolves it; the task is never rerun silently');

config.services.web.environment.REVISION = 'five';
config.services.worker.environment.REVISION = 'five';
const five = commit('orch2', 'five');
git('/lab/orch', 'pull', '-q', '/lab/orch2', 'main');
// worker ignores SIGTERM, so stopping it takes compose's full timeout and the
// kill lands with web switched and worker not
const interrupted = await killedAt(/stopped web in /, '--service', 'web,worker');
assert.match(interrupted, /stopped web in deploy-fb00000e_web_\w{7}/);
assert.equal(state('a').services.web.current.commit, four);
result = second();
assert.notEqual(result.status, 0);
assert.match(result.stderr, /did not finish .* deploy resolve [0-9a-f]{16}/, result.stderr);
const cutOff = result.stderr.match(/deploy resolve ([0-9a-f]{16})/)[1];
succeed(second('resolve', cutOff));
result = second('--service', 'web,worker');
assert.equal(result.status, 0, `${result.stdout}\n${result.stderr}`);
for (const service of ['web', 'worker']) {
  assert.equal(state('a').services[service].current.commit, five);
  assert.equal(ssh('a', `docker ps --filter label=com.docker.compose.service=${service} -q | wc -l`), '1');
}
assert.equal(inside('a', 'web', 'echo $REVISION'), 'five');
assert.ok(webReachesApi(), `web cannot reach api: ${lastReply}`);
pass('an interrupted cutover is resolved and redeployed into one running release per service, recorded as such');

result = inContainer(['/lab/bin2/deploy', 'destroy', '--volumes', '-C', '/lab/orch2'], 'orch\n');
assert.equal(result.status, 0, `${result.stdout}\n${result.stderr}`);
for (const host of ['a', 'b']) {
  assert.equal(ssh(host, `ip link show ${iface} >/dev/null 2>&1 && echo up || echo gone; test -e ${root} && echo kept || echo removed`), 'gone\nremoved');
  ssh(host, 'rm -rf /home/deployer/Projects/orch');
}
pass('orchestration fixture destroyed');
