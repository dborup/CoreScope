#!/usr/bin/env node
'use strict';

const fs = require('node:fs');
const path = require('node:path');
const assert = require('node:assert/strict');
const { spawn } = require('node:child_process');
const { performance } = require('node:perf_hooks');
const { SHARDS, CORE_SUITE, readManifest, expectedRuns, runContext } = require('./ci/e2e-manifest');

function spawnLogged(command, args, options) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, { cwd: options.root, env: options.env, stdio: ['ignore', 'pipe', 'pipe'] });
    const write = chunk => {
      fs.appendFileSync(options.log, chunk);
      process.stdout.write(chunk);
    };
    child.stdout.on('data', write);
    child.stderr.on('data', write);
    let interruption = null;
    const forward = signal => {
      interruption = signal;
      child.kill(signal);
    };
    const onInt = () => forward('SIGINT');
    const onTerm = () => forward('SIGTERM');
    process.once('SIGINT', onInt);
    process.once('SIGTERM', onTerm);
    child.once('error', reject);
    child.once('close', (code, signal) => {
      process.removeListener('SIGINT', onInt);
      process.removeListener('SIGTERM', onTerm);
      // A child may gracefully handle TERM/INT and exit zero. The parent
      // nevertheless was cancelled and must never record successful work.
      resolve({ code: interruption ? 1 : code, signal: interruption || signal });
    });
  });
}

function suiteEnv(suite, baseEnv, leafletDir) {
  const env = { ...baseEnv };
  for (const [key, value] of Object.entries(suite.env)) {
    env[key] = value === '$BASE_URL' ? baseEnv.BASE_URL : value === '$LEAFLET_DIR' ? leafletDir : value;
    assert.ok(env[key] !== undefined, `missing environment expansion for ${key}`);
  }
  return env;
}

async function runShard(options) {
  const { root, manifestFile, shard } = options;
  assert.ok(SHARDS.includes(shard), `invalid shard: ${shard}`);
  const { manifest, manifestSha256 } = readManifest(manifestFile, root);
  const baseEnv = { ...process.env, ...options.env };
  const expectedPort = String(13580 + shard);
  assert.equal(baseEnv.E2E_PORT, expectedPort, 'shard server port must be distinct and pinned');
  assert.equal(baseEnv.BASE_URL, `http://localhost:${expectedPort}`, 'shard BASE_URL must match its server');
  const outputDir = options.outputDir || path.join(root, '.ci-e2e', `shard-${shard}`);
  fs.mkdirSync(outputDir, { recursive: true });
  const output = path.join(outputDir, 'e2e-output.txt');
  const liveness = path.join(outputDir, 'node-liveness-e2e-output.txt');
  const repeat = path.join(outputDir, 'slideover-repeat-output.txt');
  for (const log of [output, liveness, repeat]) fs.writeFileSync(log, '');
  const receipt = {
    version: 1, shard, manifestSha256, status: 'failure',
    runContext: runContext(baseEnv),
    startedAt: new Date().toISOString(), completedAt: null, runs: [],
  };
  const runProcess = options.runProcess || spawnLogged;
  const suites = manifest.suites.filter(suite => suite.shard === shard);
  const byFile = new Map(suites.map(suite => [suite.file, suite]));
  try {
    for (const expected of expectedRuns(manifest, shard)) {
      const suite = byFile.get(expected.file);
      const label = expected.kind === 'repeat-gate' ? `${suite.file} (repeat-gate)` : suite.file;
      const firstRepeat = expected.kind === 'repeat-gate' && expected.iteration === 1;
      if (expected.kind !== 'repeat-gate' || firstRepeat) console.log(`=== E2E SUITE: ${label} ===`);
      if (expected.kind === 'repeat-gate') console.log(`--- slide-over E2E run ${expected.iteration}/3 ---`);
      const log = expected.kind === 'repeat-gate' ? repeat : suite.output === 'node-liveness' ? liveness : output;
      const begin = performance.now();
      let leafletDir;
      if (suite.setup && expected.kind === 'suite') {
        assert.ok(baseEnv.RUNNER_TEMP, 'RUNNER_TEMP is required for #315 assets');
        leafletDir = path.join(baseEnv.RUNNER_TEMP, 'corescope-315-leaflet');
        const setup = await runProcess('bash', ['scripts/ci/prepare-e2e-leaflet.sh'], { root, env: baseEnv, log });
        assert.equal(setup.signal, null, '#315 setup terminated by a signal');
        assert.equal(setup.code, 0, '#315 asset preparation failed');
      }
      const result = await runProcess(process.execPath, [suite.file], { root, env: suiteEnv(suite, baseEnv, leafletDir), log });
      const measured = { ...expected, code: result.code, signal: result.signal, durationMs: Math.round((performance.now() - begin) * 1000) / 1000 };
      receipt.runs.push(measured);
      assert.equal(result.signal, null, `${suite.file} terminated by ${result.signal}`);
      assert.equal(result.code, 0, `${suite.file} failed with exit ${result.code}`);
      if (expected.kind !== 'repeat-gate' || expected.iteration === suite.repeatGate) console.log(`=== E2E SUITE END: ${label} ===`);
    }
    receipt.status = 'success';
    return receipt;
  } catch (error) {
    receipt.error = error.message;
    throw error;
  } finally {
    receipt.completedAt = new Date().toISOString();
    fs.writeFileSync(path.join(outputDir, 'result.json'), JSON.stringify(receipt, null, 2) + '\n');
  }
}

async function main() {
  const root = process.cwd();
  const manifestFile = process.env.E2E_MANIFEST || path.join(root, 'scripts/e2e-shards.json');
  if (process.argv[2] === '--validate') {
    const { manifest } = readManifest(manifestFile, root);
    console.log(`E2E assignments: ${manifest.suites.length} suites across three non-empty shards`);
    return;
  }
  if (process.argv[2] === '--core-shard') {
    const { manifest } = readManifest(manifestFile, root);
    console.log(manifest.suites.find(suite => suite.file === CORE_SUITE).shard);
    return;
  }
  assert.equal(process.argv.length, 2, 'unexpected arguments');
  await runShard({ root, manifestFile, shard: Number(process.env.E2E_SHARD) });
}

if (require.main === module) main().catch(error => { console.error(error.message); process.exitCode = 1; });
module.exports = { spawnLogged, suiteEnv, runShard };
