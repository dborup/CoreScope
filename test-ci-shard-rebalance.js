'use strict';
// #869: scripts/rebalance-e2e-shards.js regenerates scripts/e2e-shards.json
// from the receipts (e2e-shard-N/result.json) of green CI runs.

const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { test } = require('node:test');
const { readTimingInputs, planShards, rebalanceManifest } = require('./scripts/rebalance-e2e-shards');
const { validateManifest } = require('./scripts/ci/e2e-manifest');

const SUITES = [
  { file: 'test-node-liveness-e2e.js', output: 'node-liveness' },
  { file: 'test-e2e-playwright.js', output: 'e2e' },
  { file: 'test-big-e2e.js', output: 'e2e' },
  { file: 'test-mid-e2e.js', output: 'e2e' },
  { file: 'test-small-e2e.js', output: 'e2e' },
  { file: 'test-new-e2e.js', output: 'e2e' },
  { file: 'test-slideover-1056-e2e.js', output: 'e2e', repeatGate: 3 },
];

function baseManifest(shards) {
  return {
    version: 1, exclusions: [], metadata: { keep: true },
    suites: SUITES.map((suite, index) => ({ ...suite, shard: shards[index], env: suite.output === 'e2e' ? { BASE_URL: '$BASE_URL' } : {}, meanSeconds: 0 })),
  };
}

// One CI run: seconds per suite, written as three shard receipts.
function writeRun(dir, shardOf, seconds, runId) {
  for (const shard of [1, 2, 3]) {
    const runs = [];
    for (const suite of SUITES.filter(s => shardOf[s.file] === shard && seconds[s.file] !== undefined)) {
      runs.push({ file: suite.file, kind: 'suite', iteration: 1, code: 0, signal: null, durationMs: seconds[suite.file] * 1000 });
      for (let i = 1; i <= (suite.repeatGate || 0); i++) runs.push({ file: suite.file, kind: 'repeat-gate', iteration: i, code: 0, signal: null, durationMs: 10000 });
    }
    const folder = path.join(dir, `e2e-shard-${shard}`);
    fs.mkdirSync(folder, { recursive: true });
    fs.writeFileSync(path.join(folder, 'result.json'), JSON.stringify({ version: 1, shard, status: 'success', runContext: { runId, attempt: '1', headSha: 'abc' }, runs }));
  }
}

function fixture(t) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'ci869-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  fs.writeFileSync(path.join(root, 'test-all.sh'), '#!/bin/sh\n');
  for (const suite of SUITES) fs.writeFileSync(path.join(root, suite.file), '\n');
  const shardOf = { 'test-node-liveness-e2e.js': 1, 'test-e2e-playwright.js': 2, 'test-big-e2e.js': 2, 'test-mid-e2e.js': 2, 'test-small-e2e.js': 3, 'test-new-e2e.js': 3, 'test-slideover-1056-e2e.js': 3 };
  const runA = path.join(root, 'run-a');
  const runB = path.join(root, 'run-b');
  writeRun(runA, shardOf, { 'test-node-liveness-e2e.js': 3, 'test-e2e-playwright.js': 50, 'test-big-e2e.js': 100, 'test-mid-e2e.js': 100, 'test-small-e2e.js': 10, 'test-slideover-1056-e2e.js': 5 }, '1');
  writeRun(runB, shardOf, { 'test-node-liveness-e2e.js': 5, 'test-e2e-playwright.js': 70, 'test-big-e2e.js': 120, 'test-mid-e2e.js': 120, 'test-small-e2e.js': 10, 'test-slideover-1056-e2e.js': 5 }, '2');
  return { root, runA, runB };
}

test('readTimingInputs averages suite and repeat-gate seconds per file over runs', t => {
  const f = fixture(t);
  const { samples } = readTimingInputs([f.runA, f.runB]);
  assert.deepEqual(samples.get('test-big-e2e.js').suite, [100, 120]);
  assert.deepEqual(samples.get('test-slideover-1056-e2e.js').repeat, [30, 30]);
  assert.equal(samples.has('test-new-e2e.js'), false);
});

test('a missing shard receipt is an error, not a silent zero', t => {
  const f = fixture(t);
  fs.rmSync(path.join(f.runB, 'e2e-shard-3'), { recursive: true });
  assert.throws(() => readTimingInputs([f.runA, f.runB]), /e2e-shard-3/);
});

test('rebalance spreads the measured time and keeps every suite on exactly one shard', t => {
  const f = fixture(t);
  const manifest = baseManifest([1, 2, 2, 2, 3, 3, 3]);
  const out = rebalanceManifest({ manifest, root: f.root, inputs: [f.runA, f.runB] });
  validateManifest(out, f.root);
  assert.equal(out.suites.length, SUITES.length);
  const load = [0, 0, 0];
  for (const suite of out.suites) load[suite.shard - 1] += suite.meanSeconds + (suite.repeatSeconds || 0);
  const projected = out.metadata.projectedSeconds;
  assert.ok(Math.max(...projected) / Math.min(...projected) <= 1.25, `projected ${projected}`);
  // Before: shard 2 held 460 s, shard 1 only 4 s.
  assert.ok(Math.max(...load) < 200, `load ${load}`);
  const big = out.suites.find(s => s.file === 'test-big-e2e.js');
  assert.equal(big.meanSeconds, 110);
  assert.deepEqual(big.secondsByExecution, [100, 120]);
});

test('suites without a measurement keep their old mean and the manifest order within a shard is stable', t => {
  const f = fixture(t);
  const manifest = baseManifest([1, 2, 2, 2, 3, 3, 3]);
  manifest.suites.find(s => s.file === 'test-new-e2e.js').meanSeconds = 40;
  const out = rebalanceManifest({ manifest, root: f.root, inputs: [f.runA, f.runB] });
  assert.equal(out.suites.find(s => s.file === 'test-new-e2e.js').meanSeconds, 40);
  const order = SUITES.map(s => s.file);
  for (const shard of [1, 2, 3]) {
    const idx = out.suites.filter(s => s.shard === shard).map(s => order.indexOf(s.file));
    assert.deepEqual(idx, [...idx].sort((a, b) => a - b));
  }
});

test('planShards is longest-first onto the lightest shard (mutant: first-fit would overload shard 1)', () => {
  const plan = planShards({ shards: 3, weights: [['a', 70], ['b', 60], ['c', 50], ['d', 40], ['e', 30], ['f', 30], ['g', 20]] });
  assert.equal(plan.projectedSeconds.reduce((x, y) => x + y, 0), 300);
  assert.ok(Math.max(...plan.projectedSeconds) - Math.min(...plan.projectedSeconds) <= 20, String(plan.projectedSeconds));
  assert.deepEqual([plan.shardOf.get('a'), plan.shardOf.get('b'), plan.shardOf.get('c')], [1, 2, 3]);
});

test('receipts of a failed shard are refused as timing input', t => {
  const f = fixture(t);
  const file = path.join(f.runA, 'e2e-shard-1', 'result.json');
  const receipt = JSON.parse(fs.readFileSync(file, 'utf8'));
  fs.writeFileSync(file, JSON.stringify({ ...receipt, status: 'failure' }));
  assert.throws(() => readTimingInputs([f.runA]), /not from a green shard/);
});
