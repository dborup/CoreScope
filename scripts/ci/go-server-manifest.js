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

const REBALANCE_HINT = 'regenerate with scripts/rebalance-go-server-shards.js (see AGENTS.md, "Go server test shards")';
const RUNNABLE_NAME = /^(?:Test|Fuzz|Example)[A-Za-z0-9_]*$/;
// go test hands -run/-skip to the test binary as a single argv string
// (-test.run=<pattern>). Linux limits one argv string to MAX_ARG_STRLEN =
// 32 pages = 128 KiB and fails exec with E2BIG beyond it. Stay well below.
const MAX_PATTERN_BYTES = 96 * 1024;

// Manifest v2: shards 1..N-1 run only the entries pinned to them (-run);
// shard N is the catch-all and runs every other entry (-skip of all pins).
// A new Test needs no manifest edit: it runs in the catch-all shard.
function validateManifest(manifest, root, inventory = discoverTests(root)) {
  assert.ok(manifest && typeof manifest === 'object' && !Array.isArray(manifest), 'manifest must be an object');
  assert.equal(manifest.version, 2, 'unsupported Go manifest version');
  assert.equal(manifest.package, PACKAGE, 'unexpected Go manifest package');
  assert.ok([3, 4].includes(manifest.shards), 'Go manifest must have three or four shards');
  assert.ok(Array.isArray(manifest.pinned) && manifest.pinned.length > 0, 'missing pinned Go tests');
  const names = new Set();
  for (const source of inventory) {
    assert.ok(!names.has(source.name), `duplicate source test: ${source.name}`);
    names.add(source.name);
  }
  const pinned = new Set();
  for (const pin of manifest.pinned) {
    assert.ok(pin && typeof pin === 'object', 'invalid Go test pin');
    assert.ok(typeof pin.name === 'string' && RUNNABLE_NAME.test(pin.name) && pin.name !== 'TestMain', 'invalid runnable test name');
    assert.ok(!pinned.has(pin.name), `duplicate pin: ${pin.name}`);
    assert.ok(Number.isInteger(pin.shard) && pin.shard >= 1 && pin.shard < manifest.shards, `pinned shard must be 1..${manifest.shards - 1}: ${pin.name}`);
    assert.ok(Array.isArray(pin.samplesSeconds) && pin.samplesSeconds.length > 0 && pin.samplesSeconds.every(value => Number.isFinite(value) && value >= 0), `timing samples required: ${pin.name}`);
    const measuredMean = pin.samplesSeconds.reduce((sum, value) => sum + value, 0) / pin.samplesSeconds.length;
    assert.ok(Number.isFinite(pin.meanSeconds) && Math.abs(pin.meanSeconds - measuredMean) <= 0.000001, `mean differs from samples: ${pin.name}`);
    pinned.add(pin.name);
  }
  for (let shard = 1; shard <= manifest.shards; shard++) {
    assert.ok(assignedTests(manifest, inventory, shard).length > 0, `empty active Go shard ${shard}: ${REBALANCE_HINT}`);
  }
  assertPartition(manifest, inventory);
  return manifest;
}

// Pins whose Test no longer exists select nothing; they are harmless and are
// dropped by the next rebalance.
function stalePins(manifest, inventory) {
  const names = new Set(inventory.map(source => source.name));
  return manifest.pinned.filter(pin => !names.has(pin.name)).map(pin => pin.name);
}

function readManifest(file, root, inventory) {
  const raw = fs.readFileSync(file, 'utf8');
  const discovered = inventory || discoverTests(root);
  return { manifest: validateManifest(JSON.parse(raw), root, discovered), inventory: discovered, manifestSha256: sha256(raw) };
}

function assignedTests(manifest, inventory, shard) {
  assert.ok(Number.isInteger(shard) && shard >= 1 && shard <= manifest.shards, `invalid Go shard ${shard}`);
  const pins = new Map(manifest.pinned.map(pin => [pin.name, pin.shard]));
  // Match go test's source registration order, not the manifest's order;
  // all subtests of each parent remain selected.
  if (shard === manifest.shards) return inventory.filter(source => source.active && !pins.has(source.name));
  return inventory.filter(source => source.active && pins.get(source.name) === shard);
}

// Every active entry is selected by exactly one shard, and nothing else is.
function assertPartition(manifest, inventory) {
  const seen = new Set();
  for (let shard = 1; shard <= manifest.shards; shard++) {
    for (const test of assignedTests(manifest, inventory, shard)) {
      assert.ok(test.active, `inactive Go test selected: ${test.name}`);
      assert.ok(!seen.has(test.name), `Go test selected by two shards: ${test.name}`);
      seen.add(test.name);
    }
  }
  for (const source of inventory) if (source.active) assert.ok(seen.has(source.name), `Go test selected by no shard: ${source.name}`);
  return seen.size;
}

// Pinned shards select their own entries with -run. The catch-all skips the
// active entries of every pinned shard, so whatever else exists runs there.
function shardSelection(manifest, inventory, shard) {
  const tests = assignedTests(manifest, inventory, shard);
  if (shard < manifest.shards) return { tests, flag: '-run', pattern: testPattern(tests) };
  const others = [];
  for (let pinned = 1; pinned < manifest.shards; pinned++) others.push(...assignedTests(manifest, inventory, pinned));
  return { tests, flag: '-skip', pattern: testPattern(others) };
}

function testPattern(tests) {
  assert.ok(tests.length > 0, 'refusing an empty Go test pattern');
  const pattern = `^(${tests.map(test => test.name.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('|')})$`;
  assert.ok(Buffer.byteLength(pattern) <= MAX_PATTERN_BYTES, `Go test pattern is ${Buffer.byteLength(pattern)} bytes, over ${MAX_PATTERN_BYTES} (Linux MAX_ARG_STRLEN is 128 KiB): pin fewer entries, ${REBALANCE_HINT}`);
  return pattern;
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

module.exports = { PACKAGE, MAX_PATTERN_BYTES, REBALANCE_HINT, sha256, discoverTests, validateManifest, stalePins, readManifest, assignedTests, assertPartition, shardSelection, testPattern, runContext, requireContext };
