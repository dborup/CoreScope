'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { createHash } = require('node:crypto');

const SHARDS = [1, 2, 3];
const CORE_SUITE = 'test-e2e-playwright.js';
const REPEAT_SUITE = 'test-slideover-1056-e2e.js';
const NODE_LIVENESS = 'test-node-liveness-e2e.js';
const LEGACY_EXCLUSIONS = new Set([
  'test-rx-coverage-mobile-nav-e2e.js',
]);
const ENV_KEYS = new Set([
  'BASE_URL', 'CHROMIUM_REQUIRE', 'AXE_SCREENSHOT_DIR',
  'CORESCOPE_SERVER_BIN', 'CORESCOPE_INGESTOR_BIN', 'FIXTURE_DB',
  'CORESCOPE_MIGRATE_BIN', 'CORESCOPE_LEAFLET_DIR',
]);

function suiteFile(file) {
  return typeof file === 'string' && /^test-[A-Za-z0-9_-]+\.js$/.test(file);
}

function discoverSuites(root) {
  const units = new Set([...fs.readFileSync(path.join(root, 'test-all.sh'), 'utf8')
    .matchAll(/^run (test-[A-Za-z0-9_-]+\.js)\s*$/gm)].map(match => match[1]));
  return fs.readdirSync(root).filter(file => /^test-.*\.js$/.test(file)).filter(file => {
    const full = path.join(root, file);
    assert.ok(fs.lstatSync(full).isFile(), `suite must be a regular file: ${file}`);
    // #1697 has an E2E filename but is a VM unit test in the unit registry.
    if (file === 'test-issue-1697-mqtt-mobile-e2e.js' && units.has(file)) return false;
    if (/e2e|playwright/.test(file)) return true;
    return /(?:\b(?:require|import)\s*\(\s*|\bfrom\s*)['"](?:playwright(?:\/[^'"]+)?|@playwright\/test(?:\/[^'"]+)?)['"]/.test(fs.readFileSync(full, 'utf8'));
  }).sort();
}

function validateManifest(manifest, root) {
  assert.ok(manifest && typeof manifest === 'object' && !Array.isArray(manifest), 'manifest must be an object');
  assert.equal(manifest.version, 1, 'unsupported manifest version');
  assert.ok(Array.isArray(manifest.suites), 'manifest suites must be an array');
  assert.ok(Array.isArray(manifest.exclusions), 'manifest exclusions must be an array');
  const assigned = new Map();
  for (const suite of manifest.suites) {
    assert.ok(suite && typeof suite === 'object', 'invalid suite entry');
    assert.ok(suiteFile(suite.file), `invalid suite filename: ${suite.file}`);
    assert.notEqual(suite.file, 'test-issue-1697-mqtt-mobile-e2e.js', '#1697 must remain in the unit registry');
    assert.ok(SHARDS.includes(suite.shard), `invalid shard for ${suite.file}`);
    assert.ok(!assigned.has(suite.file), `duplicate suite assignment: ${suite.file}`);
    assert.ok(fs.lstatSync(path.join(root, suite.file)).isFile(), `missing suite file: ${suite.file}`);
    assert.ok(suite.env && typeof suite.env === 'object' && !Array.isArray(suite.env), `env must be an object: ${suite.file}`);
    for (const [key, value] of Object.entries(suite.env)) {
      assert.ok(ENV_KEYS.has(key), `unsupported environment key: ${key}`);
      assert.equal(typeof value, 'string', `non-string environment value: ${key}`);
      assert.ok(!/[\x00\r\n]/.test(value), `invalid environment value: ${key}`);
      assert.ok(!value.includes('$') || value === '$BASE_URL' || value === '$LEAFLET_DIR', `unsupported environment expansion: ${key}`);
      if (key === 'BASE_URL') assert.equal(value, '$BASE_URL', 'BASE_URL must use the pinned local shard target');
    }
    assert.ok(['e2e', 'node-liveness'].includes(suite.output), `invalid output for ${suite.file}`);
    assert.equal(suite.output === 'node-liveness', suite.file === NODE_LIVENESS, 'only node liveness uses its separate output');
    if (suite.setup !== undefined) {
      assert.equal(suite.file, 'test-issue-315-estimated-positions-e2e.js', 'setup is only allowed for #315');
      assert.equal(suite.setup, 'leaflet-315', 'unknown suite setup');
    }
    if (suite.repeatGate !== undefined) {
      assert.equal(suite.file, REPEAT_SUITE, 'only slide-over has the repeat gate');
      assert.equal(suite.repeatGate, 3, 'the permanent slide-over gate must run three times');
    }
    assigned.set(suite.file, suite);
  }
  for (const shard of SHARDS) {
    assert.ok(manifest.suites.some(suite => suite.shard === shard), `empty shard ${shard}`);
  }
  assert.ok(assigned.has(CORE_SUITE), 'missing core Playwright suite');
  assert.ok(assigned.has(NODE_LIVENESS), 'missing node-liveness suite');
  assert.equal(assigned.get(REPEAT_SUITE)?.repeatGate, 3, 'missing permanent slide-over repeat gate');
  const excluded = new Set();
  for (const entry of manifest.exclusions) {
    assert.ok(entry && LEGACY_EXCLUSIONS.has(entry.file), `unapproved suite exclusion: ${entry?.file}`);
    assert.ok(typeof entry.reason === 'string' && entry.reason.trim().length > 20, `missing exclusion rationale: ${entry.file}`);
    assert.ok(!excluded.has(entry.file), `duplicate exclusion: ${entry.file}`);
    assert.ok(!assigned.has(entry.file), `assigned suite also excluded: ${entry.file}`);
    assert.ok(fs.lstatSync(path.join(root, entry.file)).isFile(), `missing excluded suite: ${entry.file}`);
    excluded.add(entry.file);
  }
  for (const file of discoverSuites(root)) {
    assert.ok(assigned.has(file) || excluded.has(file), `unassigned E2E suite: ${file}`);
  }
  return manifest;
}

function readManifest(file, root) {
  const raw = fs.readFileSync(file, 'utf8');
  return {
    manifest: validateManifest(JSON.parse(raw), root),
    manifestSha256: createHash('sha256').update(raw).digest('hex'),
  };
}

function expectedRuns(manifest, shard) {
  const suites = manifest.suites.filter(suite => suite.shard === shard);
  const ordinary = suites.map(suite => ({ file: suite.file, kind: 'suite', iteration: 1 }));
  const repeated = suites.flatMap(suite => Array.from({ length: suite.repeatGate || 0 }, (_, index) => ({
    file: suite.file, kind: 'repeat-gate', iteration: index + 1,
  })));
  return [...ordinary, ...repeated];
}

function runContext(env) {
  return { runId: env.GITHUB_RUN_ID || null, attempt: env.GITHUB_RUN_ATTEMPT || null, headSha: env.GITHUB_SHA || null };
}

module.exports = { SHARDS, CORE_SUITE, REPEAT_SUITE, NODE_LIVENESS, discoverSuites, validateManifest, readManifest, expectedRuns, runContext };
