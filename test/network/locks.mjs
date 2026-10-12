import { spawn, execFileSync } from 'node:child_process';
import { ssh, put, assert, until, pass, sleep } from './common.mjs';

const container = process.env.DEPLOY_NETWORK_CONTAINER || 'deploy-network-experiment';
const holder = spawn('docker', ['exec', '-i', container, 'bash', '-c', `echo $$ > /lab/holder.pid; exec ssh -p 22101 -i /lab/id_ed25519 -o BatchMode=yes -o UserKnownHostsFile=/lab/known_hosts root@127.0.0.1 'exec 9>/run/separate.lock; flock -n 9; touch /root/holder-ready; read -r ignored'`], { stdio: ['pipe', 'pipe', 'pipe'] });
await until('separate lock held', () => ssh('a', 'test -f /root/holder-ready && echo ready') === 'ready');
ssh('a', 'systemd-run --unit=lab-orphan --collect /bin/sh -c "touch /root/orphan-running; sleep 8; touch /root/orphan-effect"');
await until('separate work running', () => ssh('a', 'test -f /root/orphan-running && echo ready') === 'ready');
execFileSync('docker', ['exec', container, 'sh', '-c', 'kill -KILL "$(cat /lab/holder.pid)"']);
await until('lost SSH holder releases lock', () => ssh('a', 'flock -n /run/separate.lock true && echo acquired') === 'acquired');
assert.equal(ssh('a', 'systemctl is-active lab-orphan'), 'active');
console.log('OBSERVED separate SSH lock is released while independent remote work remains active');
holder.stdin.destroy();
await until('orphan completed', () => ssh('a', 'test -f /root/orphan-effect && echo done') === 'done');

put('a', '/root/owned-operation.sh', `#!/bin/bash
set -euo pipefail
exec 9>/run/owned-operation.lock
flock -n 9
test ! -e /root/operation-pending
echo pending > /root/operation-pending
sync /root/operation-pending
touch /root/owned-running
sleep 6
echo effect >> /root/effects
sync /root/effects
if test -f /root/simulate-result-loss; then exit 7; fi
mv /root/operation-pending /root/operation-complete
sync /root/operation-complete
`, '700');
ssh('a', 'systemd-run --unit=lab-owned --collect /root/owned-operation.sh');
await until('owned work running', () => ssh('a', 'test -f /root/owned-running && echo ready') === 'ready');
assert.equal(ssh('a', 'if flock -n /run/owned-operation.lock true; then echo acquired; else echo blocked; fi'), 'blocked');
await until('owned result committed', () => ssh('a', 'test -f /root/operation-complete && echo done') === 'done');
assert.equal(ssh('a', 'wc -l < /root/effects'), '1');
pass('remote operation retains its own lock after its initiating SSH command exits');
ssh('a', 'rm /root/owned-running /root/operation-complete; touch /root/simulate-result-loss; systemd-run --unit=lab-unknown --collect /root/owned-operation.sh');
await until('unknown operation finished effect', () => ssh('a', 'wc -l < /root/effects') === '2');
await sleep(1000);
assert.equal(ssh('a', 'if /root/owned-operation.sh; then echo replayed; else echo refused; fi'), 'refused');
assert.equal(ssh('a', 'wc -l < /root/effects'), '2');
pass('lost result remains indeterminate and blocks automatic replay after lock release');
