import { execFileSync, spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import assert from 'node:assert/strict';

export { assert };
const lab = fileURLToPath(new URL('./lab.sh', import.meta.url));
export const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
export const quote = text => "'" + text.replaceAll("'", "'\\''") + "'";
export function ssh(host, command, input) {
  return execFileSync('bash', [lab, 'ssh', host, `set -euo pipefail; ${command}`], {
    input, encoding: 'utf8', timeout: 90000,
    stdio: ['pipe', 'pipe', 'pipe'], maxBuffer: 1024 * 1024,
  }).trim();
}
export function put(host, path, content, mode = '600') {
  ssh(host, `umask 077; cat > ${quote(path)}; chmod ${mode} ${quote(path)}`, content);
}
export async function until(label, predicate, timeout = 60000) {
  const end = Date.now() + timeout;
  let error;
  while (Date.now() < end) {
    try { if (await predicate()) return; } catch (caught) { error = caught; }
    await sleep(1000);
  }
  throw new Error(`timed out: ${label}`, { cause: error });
}
export function pass(message) { console.log(`PASS ${message}`); }

// inContainer runs a command in the tools container, where the deploy binary
// and the deployer ssh config live; helper.mjs writes both.
export function inContainer(args, input = '') {
  const container = process.env.DEPLOY_NETWORK_CONTAINER;
  const result = spawnSync('docker', ['exec', '-i', container, ...args], { input, encoding: 'utf8', timeout: 240000 });
  return { status: result.status, stdout: result.stdout.trim(), stderr: result.stderr.trim() };
}
export const asDeployer = (host, command, input = '') => inContainer(['/lab/bin/ssh', `vm-${host}`, command], input);
export function succeed(result) {
  assert.equal(result.status, 0, `${result.stdout}\n${result.stderr}`);
  return result.stdout;
}
