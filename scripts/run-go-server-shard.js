#!/usr/bin/env node
'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { spawn, spawnSync } = require('node:child_process');
const { performance } = require('node:perf_hooks');
const { PACKAGE, REBALANCE_HINT, sha256, readManifest, shardSelection, stalePins, runContext } = require('./ci/go-server-manifest');

function spawnLogged(command, args, options) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, { cwd: options.cwd, env: options.env, stdio: ['ignore', 'pipe', 'pipe'], detached: process.platform !== 'win32' });
    const write = (chunk, stdout) => {
      if (stdout && options.jsonFile) fs.appendFileSync(options.jsonFile, chunk);
      fs.appendFileSync(options.log, chunk);
      (stdout ? process.stdout : process.stderr).write(chunk);
    };
    child.stdout.on('data', chunk => write(chunk, true));
    child.stderr.on('data', chunk => write(chunk, false));
    let interrupted = null;
    const forward = signal => {
      interrupted = signal;
      // go test has its own test-binary child. Forward to the whole process
      // group so cancellation cannot leave that child running unattended.
      if (process.platform === 'win32') child.kill(signal);
      else {
        try { process.kill(-child.pid, signal); }
        catch (error) { if (error.code !== 'ESRCH') throw error; }
      }
    };
    const onInt = () => forward('SIGINT');
    const onTerm = () => forward('SIGTERM');
    process.once('SIGINT', onInt);
    process.once('SIGTERM', onTerm);
    const cleanup = () => {
      process.removeListener('SIGINT', onInt);
      process.removeListener('SIGTERM', onTerm);
    };
    child.once('error', error => { cleanup(); reject(error); });
    child.once('close', (code, signal) => {
      cleanup();
      // A cancelled child can handle TERM and return zero. Cancellation
      // remains failure even when its own exit code says otherwise.
      resolve({ code: interrupted ? 1 : code, signal: interrupted || signal });
    });
  });
}

function verifyTestJSON(raw, expected, pkg = PACKAGE) {
  const wanted = new Map(expected.map(test => [test.name, test]));
  assert.equal(wanted.size, expected.length, 'duplicate expected Go test');
  const seen = new Map();
  let packageStarted = false;
  let packagePassed = false;
  const lines = raw.split('\n').filter(line => line.trim());
  assert.ok(lines.length > 0, 'empty Go test JSON');
  for (const line of lines) {
    const event = JSON.parse(line);
    assert.ok(event && typeof event === 'object', 'invalid Go test event');
    assert.ok(['start', 'run', 'pass', 'skip', 'pause', 'cont', 'output', 'fail', 'build-output', 'build-fail'].includes(event.Action), `unknown Go action: ${event.Action}`);
    assert.ok(!['fail', 'build-fail'].includes(event.Action), `failed Go event: ${event.Test || event.Package || event.ImportPath}`);
    if (event.Package !== undefined) assert.equal(event.Package, pkg, 'unexpected Go package in shard');
    if (event.Test) {
      const parent = event.Test.split('/')[0];
      assert.ok(wanted.has(parent), `unassigned Go test executed: ${parent}`);
      if (event.Test !== parent) continue;
      if (event.Action === 'run') {
        assert.ok(!seen.has(parent), `Go test executed twice: ${parent}`);
        seen.set(parent, { name: parent, kind: wanted.get(parent).kind, status: 'running', elapsedSeconds: null });
      } else if (event.Action === 'pass' || event.Action === 'skip') {
        const item = seen.get(parent);
        assert.ok(item && item.status === 'running', `Go test ended without one run: ${parent}`);
        assert.ok(Number.isFinite(event.Elapsed) && event.Elapsed >= 0, `invalid test duration: ${parent}`);
        item.status = event.Action;
        item.elapsedSeconds = event.Elapsed;
      }
    } else if (event.Package === pkg && event.Action === 'start') {
      assert.ok(!packageStarted, 'Go package started twice');
      packageStarted = true;
    } else if (event.Package === pkg && event.Action === 'pass') {
      assert.ok(packageStarted && !packagePassed, 'Go package pass missing start or duplicated');
      packagePassed = true;
    }
  }
  assert.ok(packagePassed, 'missing successful Go package completion');
  for (const test of expected) assert.ok(['pass', 'skip'].includes(seen.get(test.name)?.status), `missing or incomplete Go test: ${test.name}`);
  return { packagePassed, tests: expected.map(test => seen.get(test.name)) };
}

function verifyRuntimeList(root, inventory, run = spawnSync) {
  const cwd = path.join(root, 'cmd/server');
  const packages = run('go', ['list', '-f', '{{.ImportPath}}', './...'], { cwd, encoding: 'utf8' });
  assert.equal(packages.signal, null, 'Go package listing signalled');
  assert.equal(packages.status, 0, `Go package listing failed: ${packages.stderr}`);
  assert.deepEqual(packages.stdout.trim().split(/\r?\n/), [PACKAGE], 'cmd/server contains an unexpected package');
  const result = run('go', ['test', '-race', '-list', '.', './...'], { cwd, encoding: 'utf8', maxBuffer: 16 * 1024 * 1024 });
  assert.equal(result.signal, null, 'Go runtime test listing signalled');
  assert.equal(result.status, 0, `Go runtime test listing failed: ${result.stderr}`);
  const runnable = result.stdout.split(/\r?\n/).filter(line => /^(?:Test|Fuzz|Example)[A-Za-z0-9_]*$/.test(line));
  assert.equal(new Set(runnable).size, runnable.length, 'duplicate runtime test entry');
  assert.deepEqual([...runnable].sort(), inventory.filter(source => source.active).map(source => source.name).sort(), 'Go runtime list differs from AST inventory');
  return runnable;
}

async function runShard(options) {
  const { root, manifestFile, shard } = options;
  const { manifest, inventory, manifestSha256 } = readManifest(manifestFile, root, options.inventory);
  const selection = shardSelection(manifest, inventory, shard);
  const selected = selection.tests;
  const env = { ...process.env, ...options.env };
  const folder = options.outputDir || path.join(root, '.ci-go', `server-${shard}`);
  fs.mkdirSync(folder, { recursive: true });
  const profile = path.join(folder, 'profile.out');
  const jsonFile = path.join(folder, 'test.jsonl');
  const log = path.join(folder, 'test.log');
  // Never reuse a previous local/profile artifact after a failed rerun.
  for (const file of [profile, jsonFile, log]) fs.writeFileSync(file, '');
  const receipt = {
    version: 1, shard, package: PACKAGE, manifestSha256, status: 'failure', selectionFlag: selection.flag,
    runContext: runContext(env), startedAt: new Date().toISOString(), completedAt: null,
    selected: selected.map(test => ({ name: test.name, file: test.file, kind: test.kind })),
    runs: [], tests: [], profileSha256: null, testJSONSha256: null,
  };
  const execute = options.runProcess || spawnLogged;
  const cwd = path.join(root, 'cmd/server');
  try {
    for (const [kind, args] of [
      ['build', ['build', '.']],
      ['test', ['test', '-timeout', '20m', '-race', `-coverprofile=${profile}`, '-count=1', '-json', selection.flag, selection.pattern, './...']],
    ]) {
      const begin = performance.now();
      const result = await execute('go', args, { cwd, env, log, jsonFile: kind === 'test' ? jsonFile : null });
      receipt.runs.push({ kind, code: result.code, signal: result.signal, durationMs: Math.round((performance.now() - begin) * 1000) / 1000 });
      assert.equal(result.signal, null, `Go ${kind} terminated by a signal`);
      assert.equal(result.code, 0, `Go ${kind} failed: exit ${result.code}`);
    }
    const rawJSON = fs.readFileSync(jsonFile, 'utf8');
    receipt.tests = verifyTestJSON(rawJSON, selected).tests;
    const rawProfile = fs.readFileSync(profile, 'utf8');
    assert.ok(rawProfile.startsWith('mode: atomic\n') && rawProfile.trim().split('\n').length > 1, 'missing atomic coverage profile');
    receipt.profileSha256 = sha256(rawProfile);
    receipt.testJSONSha256 = sha256(rawJSON);
    receipt.status = 'success';
    return receipt;
  } catch (error) {
    receipt.error = error.message;
    throw error;
  } finally {
    receipt.completedAt = new Date().toISOString();
    fs.writeFileSync(path.join(folder, 'receipt.json'), JSON.stringify(receipt, null, 2) + '\n');
  }
}

async function main() {
  const root = process.cwd();
  const manifestFile = process.env.GO_SERVER_MANIFEST || path.join(root, 'scripts/go-server-shards.json');
  const command = process.argv[2];
  assert.ok(process.argv.length <= 3, 'unexpected Go shard arguments');
  if (command === '--validate' || command === '--runtime-list' || command === '--list') {
    const { manifest, inventory } = readManifest(manifestFile, root);
    if (command === '--runtime-list') console.log(`Runtime inventory: ${verifyRuntimeList(root, inventory).length} runnable Go entries`);
    else if (command === '--list') {
      const selection = shardSelection(manifest, inventory, Number(process.env.GO_SERVER_SHARD));
      console.log(`${selection.flag} ${selection.pattern}`);
    } else {
      const counts = Array.from({ length: manifest.shards }, (_, index) => shardSelection(manifest, inventory, index + 1).tests.length);
      console.log(`Go assignments: ${counts.reduce((a, b) => a + b, 0)} runnable entries, each in exactly one shard (${counts.join(' / ')}); shard ${manifest.shards} is the catch-all for everything not pinned`);
      const stale = stalePins(manifest, inventory);
      if (stale.length) console.log(`Note: ${stale.length} pinned entries no longer exist and select nothing (${stale.join(', ')}); ${REBALANCE_HINT}`);
    }
    return;
  }
  assert.equal(command, undefined, 'unexpected Go shard argument');
  await runShard({ root, manifestFile, shard: Number(process.env.GO_SERVER_SHARD) });
}

if (require.main === module) main().catch(error => { console.error(error.message); process.exitCode = 1; });
module.exports = { spawnLogged, verifyTestJSON, verifyRuntimeList, runShard };
