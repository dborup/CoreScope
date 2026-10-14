'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { spawnSync } = require('node:child_process');
const { createHash } = require('node:crypto');

const PACKAGE = 'github.com/corescope/server';
const sha256 = raw => createHash('sha256').update(raw).digest('hex');

function discoverTests(root) {
  root = path.resolve(root);
  const source = path.join(__dirname, 'go-test-inventory.go');
  const result = spawnSync('go', ['run', source, '--root', root], {
    cwd: root, encoding: 'utf8', maxBuffer: 16 * 1024 * 1024,
  });
  assert.equal(result.signal, null, `Go inventory terminated: ${result.signal}`);
  assert.equal(result.status, 0, `Go inventory failed: ${result.stderr || result.error?.message}`);
  const inventory = JSON.parse(result.stdout);
  assert.equal(inventory.version, 1, 'unsupported Go inventory version');
  assert.equal(inventory.package, PACKAGE, 'unexpected inventory package');
  assert.ok(Array.isArray(inventory.entries) && inventory.entries.length > 0, 'empty Go test inventory');
  return inventory.entries;
}

function validateManifest(manifest, root, inventory = discoverTests(root)) {
  assert.ok(manifest && typeof manifest === 'object' && !Array.isArray(manifest), 'manifest must be an object');
  assert.equal(manifest.version, 1, 'unsupported Go manifest version');
  assert.equal(manifest.package, PACKAGE, 'unexpected Go manifest package');
  assert.ok([3, 4].includes(manifest.shards), 'Go manifest must have three or four shards');
  assert.ok(Array.isArray(manifest.tests) && manifest.tests.length > 0, 'missing Go test assignments');
  const sourceByName = new Map();
  for (const source of inventory) {
    assert.ok(!sourceByName.has(source.name), `duplicate source test: ${source.name}`);
    sourceByName.set(source.name, source);
  }
  const assigned = new Map();
  for (const test of manifest.tests) {
    assert.ok(test && typeof test === 'object', 'invalid Go test assignment');
    assert.ok(typeof test.name === 'string' && /^(?:Test|Fuzz|Example)[A-Za-z0-9_]*$/.test(test.name) && test.name !== 'TestMain', 'invalid runnable test name');
    assert.ok(!assigned.has(test.name), `duplicate test assignment: ${test.name}`);
    const source = sourceByName.get(test.name);
    assert.ok(source, `stale test assignment: ${test.name}`);
    assert.equal(test.file, source.file, `test source mismatch: ${test.name}`);
    assert.ok(Number.isInteger(test.shard) && test.shard >= 1 && test.shard <= manifest.shards, `invalid shard: ${test.name}`);
    assert.ok(Array.isArray(test.samplesSeconds) && test.samplesSeconds.length === 3 && test.samplesSeconds.every(value => Number.isFinite(value) && value >= 0), `three finite timing samples required: ${test.name}`);
    assert.ok(Number.isFinite(test.meanSeconds) && test.meanSeconds >= 0, `invalid mean timing: ${test.name}`);
    const measuredMean = test.samplesSeconds.reduce((sum, value) => sum + value, 0) / 3;
    assert.ok(Math.abs(test.meanSeconds - measuredMean) <= 0.000001, `mean differs from samples: ${test.name}`);
    assigned.set(test.name, test);
  }
  for (const source of inventory) assert.ok(assigned.has(source.name), `unassigned Go test: ${source.name}`);
  for (let shard = 1; shard <= manifest.shards; shard++) {
    assert.ok(inventory.some(source => source.active && assigned.get(source.name).shard === shard), `empty active Go shard ${shard}`);
  }
  return manifest;
}

function readManifest(file, root, inventory) {
  const raw = fs.readFileSync(file, 'utf8');
  const discovered = inventory || discoverTests(root);
  return { manifest: validateManifest(JSON.parse(raw), root, discovered), inventory: discovered, manifestSha256: sha256(raw) };
}

function assignedTests(manifest, inventory, shard) {
  assert.ok(Number.isInteger(shard) && shard >= 1 && shard <= manifest.shards, `invalid Go shard ${shard}`);
  const assignment = new Map(manifest.tests.map(test => [test.name, test]));
  // Match go test's source registration order, not longest-first insertion
  // order in the timing manifest; all subtests of each parent remain selected.
  return inventory.filter(source => source.active && assignment.get(source.name).shard === shard);
}

function testPattern(tests) {
  assert.ok(tests.length > 0, 'refusing an empty Go test pattern');
  return `^(${tests.map(test => test.name.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('|')})$`;
}

function runContext(env) {
  return { runId: env.GITHUB_RUN_ID || null, attempt: env.GITHUB_RUN_ATTEMPT || null, headSha: env.GITHUB_SHA || null };
}

function requireContext(receipt, current) {
  assert.deepEqual({ runId: receipt?.runId, headSha: receipt?.headSha }, { runId: current.runId, headSha: current.headSha }, 'receipt from another run or head');
  if (current.attempt === null) assert.equal(receipt.attempt, null, 'local receipt has a CI attempt');
  else {
    assert.ok(/^[1-9][0-9]*$/.test(current.attempt) && /^[1-9][0-9]*$/.test(receipt?.attempt), 'invalid receipt attempt');
    assert.ok(Number.isSafeInteger(Number(current.attempt)) && Number.isSafeInteger(Number(receipt.attempt)), 'unsafe receipt attempt');
    assert.ok(Number(receipt.attempt) <= Number(current.attempt), 'receipt from a future attempt');
  }
}

module.exports = { PACKAGE, sha256, discoverTests, validateManifest, readManifest, assignedTests, testPattern, runContext, requireContext };
