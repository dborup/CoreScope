'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const os = require('node:os');
const { spawn, spawnSync } = require('node:child_process');
const { test } = require('node:test');
const { validateManifest, expectedRuns } = require('./scripts/ci/e2e-manifest');
const { aggregateShards } = require('./scripts/report-e2e-shards');

function successfulResults(extra = {}) {
  return { ...process.env, E2E_1_RESULT: 'success', E2E_2_RESULT: 'success', E2E_3_RESULT: 'success', ...extra };
}

function fixture(t) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'ci434-shard-test-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  fs.writeFileSync(path.join(root, 'test-all.sh'), '#!/bin/sh\n');
  const scripts = {
    'test-node-liveness-e2e.js': "console.log('999 passed, 0 failed');\n",
    'test-e2e-playwright.js': "console.log('3 passed, 0 failed');\n",
    'test-slideover-1056-e2e.js': "console.log('2 passed, 0 failed');\n",
    'test-probe-e2e.js': 'console.log(process.env.' + "BASE_URL + '|' + process.env.CHROMIUM_REQUIRE);console.log('1 passed, 0 failed');\n",
  };
  for (const [file, source] of Object.entries(scripts)) fs.writeFileSync(path.join(root, file), source);
  const manifest = {
    version: 1, exclusions: [], suites: [
      { file: 'test-node-liveness-e2e.js', shard: 1, env: {}, output: 'node-liveness' },
      { file: 'test-e2e-playwright.js', shard: 2, env: { BASE_URL: '$BASE_URL' }, output: 'e2e' },
      { file: 'test-probe-e2e.js', shard: 2, env: { BASE_URL: '$BASE_URL', CHROMIUM_REQUIRE: '1' }, output: 'e2e' },
      { file: 'test-slideover-1056-e2e.js', shard: 3, env: { BASE_URL: '$BASE_URL' }, output: 'e2e', repeatGate: 3 },
    ],
  };
  const manifestFile = path.join(root, 'e2e-shards.json');
  function save() { fs.writeFileSync(manifestFile, JSON.stringify(manifest, null, 2) + '\n'); }
  save();
  function run(shard) {
    return spawnSync(process.execPath, [path.join(__dirname, 'scripts/run-e2e-shard.js')], {
      cwd: root, encoding: 'utf8', timeout: 15000,
      env: { ...process.env, E2E_MANIFEST: manifestFile, E2E_SHARD: String(shard), E2E_PORT: String(13580 + shard), BASE_URL: `http://localhost:${13580 + shard}` },
    });
  }
  function receipt(shard) { return JSON.parse(fs.readFileSync(path.join(root, '.ci-e2e', `shard-${shard}`, 'result.json'), 'utf8')); }
  function artifacts() {
    const folder = path.join(root, 'artifacts');
    fs.mkdirSync(folder, { recursive: true });
    for (const shard of [1, 2, 3]) fs.cpSync(path.join(root, '.ci-e2e', `shard-${shard}`), path.join(folder, `e2e-shard-${shard}`), { recursive: true });
    return folder;
  }
  return { root, manifest, manifestFile, run, receipt, save, artifacts };
}

test('guard validates current assignments and rejects newly added browser files', t => {
  const f = fixture(t);
  assert.equal(validateManifest(f.manifest, f.root), f.manifest);
  // Mutant: a new conventionally named E2E has no shard assignment.
  fs.writeFileSync(path.join(f.root, 'test-new_unassigned-e2e.js'), 'console.log("not run");\n');
  assert.throws(() => validateManifest(f.manifest, f.root), /unassigned E2E suite: test-new_unassigned-e2e\.js/);
  fs.unlinkSync(path.join(f.root, 'test-new_unassigned-e2e.js'));
  // Non-e2e names which import Playwright must not fall outside discovery.
  fs.writeFileSync(path.join(f.root, 'test-new-browser.js'), 'const { chromium } = requ' + "ire('playwright');\n");
  assert.throws(() => validateManifest(f.manifest, f.root), /unassigned E2E suite: test-new-browser\.js/);
  fs.unlinkSync(path.join(f.root, 'test-new-browser.js'));
  fs.writeFileSync(path.join(f.root, 'test-new-import.js'), 'const browser = await imp' + "ort('playwright');\n");
  assert.throws(() => validateManifest(f.manifest, f.root), /unassigned E2E suite: test-new-import\.js/);
});

test('guard rejects duplicate assignments, missing assignments, invalid shards and traversal', t => {
  const f = fixture(t);
  for (const change of [
    manifest => manifest.suites.push({ ...manifest.suites[2], shard: 1 }),
    manifest => { manifest.suites = manifest.suites.filter(suite => suite.file !== 'test-probe-e2e.js'); },
    manifest => { manifest.suites[2].shard = 4; },
    manifest => { manifest.suites[2].file = '../test-probe-e2e.js'; },
    manifest => { manifest.suites[2].file = '/tmp/test-probe-e2e.js'; },
    manifest => { manifest.suites[3].repeatGate = 2; },
    manifest => { manifest.suites[2].output = 'node-liveness'; },
    manifest => { manifest.suites[2].env.BASE_URL = '$(touch forbidden)'; },
    manifest => manifest.exclusions.push({ file: 'test-probe-e2e.js', reason: 'a new file must not receive an ad-hoc exclusion' }),
  ]) {
    const mutated = structuredClone(f.manifest);
    change(mutated);
    assert.throws(() => validateManifest(mutated, f.root));
  }
});

test('unit registry cannot hide newly added browser imports or E2E filenames', t => {
  const f = fixture(t);
  fs.writeFileSync(path.join(f.root, 'test-new-browser.js'), 'const { chromium } = requ' + "ire('playwright');\n");
  fs.writeFileSync(path.join(f.root, 'test-all.sh'), '#!/bin/sh\nrun test-new-browser.js\n');
  assert.throws(() => validateManifest(f.manifest, f.root), /unassigned E2E suite: test-new-browser.js/);
  fs.unlinkSync(path.join(f.root, 'test-new-browser.js'));
  fs.writeFileSync(path.join(f.root, 'test-added-e2e.js'), 'console.log("browser suite");\n');
  fs.appendFileSync(path.join(f.root, 'test-all.sh'), 'run test-added-e2e.js\n');
  assert.throws(() => validateManifest(f.manifest, f.root), /unassigned E2E suite: test-added-e2e.js/);
});

test('manifest cannot override the pinned local BASE_URL', t => {
  const f = fixture(t);
  for (const target of ['https://staging.invalid', 'https://production.invalid', 'http://localhost:13581']) {
    const manifest = structuredClone(f.manifest);
    manifest.suites[2].env.BASE_URL = target;
    assert.throws(() => validateManifest(manifest, f.root), /BASE_URL/);
  }
});

test('parent interruption remains failure when the child handles the signal and exits zero', async t => {
  for (const signal of ['SIGTERM', 'SIGINT']) {
    const f = fixture(t);
    fs.writeFileSync(path.join(f.root, 'test-node-liveness-e2e.js'),
      `process.on('${signal}', () => process.exit(0));console.log('CHILD_READY');setTimeout(() => process.exit(2), 5000);\n`);
    const child = spawn(process.execPath, [path.join(__dirname, 'scripts/run-e2e-shard.js')], {
      cwd: f.root, stdio: ['ignore', 'pipe', 'pipe'],
      env: { ...process.env, E2E_MANIFEST: f.manifestFile, E2E_SHARD: '1', E2E_PORT: '13581', BASE_URL: 'http://localhost:13581' },
    });
    t.after(() => { if (child.exitCode === null && child.signalCode === null) child.kill('SIGKILL'); });
    const exit = new Promise((resolve, reject) => { child.once('error', reject); child.once('close', (code, signal) => resolve({ code, signal })); });
    await new Promise((resolve, reject) => {
      const timeout = setTimeout(() => reject(new Error('child readiness timed out')), 4000);
      let output = '';
      child.stdout.on('data', chunk => {
        output += chunk;
        if (output.includes('CHILD_READY')) { clearTimeout(timeout); resolve(); }
      });
      child.once('error', reject);
    });
    assert.ok(child.kill(signal), 'signal only the spawned runner after its child is ready');
    const result = await exit;
    assert.notEqual(result.code, 0, 'cancelled runner must not report success after a graceful child exit');
    const receipt = f.receipt(1);
    assert.equal(receipt.status, 'failure');
    assert.equal(receipt.runs[0].signal, signal);
  }
});

test('runner executes real child processes, preserves env, repeats only on owning shard, and records every run', t => {
  const f = fixture(t);
  for (const shard of [1, 2, 3]) {
    const result = f.run(shard);
    assert.equal(result.status, 0, result.stderr);
    const receipt = f.receipt(shard);
    assert.equal(receipt.status, 'success');
    assert.deepEqual(receipt.runs.map(({ file, kind, iteration }) => ({ file, kind, iteration })), expectedRuns(f.manifest, shard));
    assert.ok(receipt.runs.every(run => run.code === 0 && run.signal === null && Number.isFinite(run.durationMs)));
  }
  assert.match(fs.readFileSync(path.join(f.root, '.ci-e2e/shard-2/e2e-output.txt'), 'utf8'), /http:\/\/localhost:13582\|1/);
  assert.equal(f.receipt(3).runs.length, 4, 'one ordinary and three repeat executions');
  const report = aggregateShards({ root: f.root, manifestFile: f.manifestFile, artifactsDir: f.artifacts(), outputDir: path.join(f.root, 'report'), env: successfulResults() });
  assert.equal(report.status, 'success');
  const combined = fs.readFileSync(path.join(f.root, 'report/e2e-output.txt'), 'utf8');
  assert.doesNotMatch(combined, /999 passed/, 'node-liveness remains excluded from original badge summary');
  assert.equal((combined.match(/2 passed/g) || []).length, 1, 'repeat gate remains excluded from original badge summary');
});

test('real child non-zero exit and signal fail the shard without running later suites', t => {
  for (const source of ['process.exit(23);\n', "process.kill(process.pid, 'SIGTERM');\n"]) {
    const f = fixture(t);
    fs.writeFileSync(path.join(f.root, 'test-e2e-playwright.js'), source);
    fs.writeFileSync(path.join(f.root, 'test-probe-e2e.js'), "require('node:fs').writeFileSync('incorrectly-ran', 'yes');\n");
    const result = f.run(2);
    assert.notEqual(result.status, 0);
    assert.ok(!fs.existsSync(path.join(f.root, 'incorrectly-ran')), 'fail-fast must prevent next suite');
    const receipt = f.receipt(2);
    assert.equal(receipt.status, 'failure');
    assert.equal(receipt.runs.length, 1);
    assert.ok(receipt.runs[0].code !== 0 || receipt.runs[0].signal !== null);
  }
});

test('report is fail-closed for every shard status, missing artifacts, missing execution, stale manifest and forged success', t => {
  const f = fixture(t);
  for (const shard of [1, 2, 3]) assert.equal(f.run(shard).status, 0);
  const artifactsDir = f.artifacts();
  const options = { root: f.root, manifestFile: f.manifestFile, artifactsDir, outputDir: path.join(f.root, 'report'), env: successfulResults() };
  assert.equal(aggregateShards(options).status, 'success');
  for (const shard of [1, 2, 3]) {
    for (const result of ['failure', 'skipped', 'cancelled', '']) {
      assert.throws(() => aggregateShards({ ...options, env: { ...options.env, [`E2E_${shard}_RESULT`]: result } }), /current shard.*result must be success/);
    }
    const file = path.join(artifactsDir, `e2e-shard-${shard}`, 'result.json');
    const original = fs.readFileSync(file, 'utf8');
    for (const status of ['failure', 'skipped', 'cancelled', '']) {
      const receipt = JSON.parse(original);
      receipt.status = status;
      fs.writeFileSync(file, JSON.stringify(receipt));
      assert.throws(() => aggregateShards(options), /did not succeed/);
    }
    for (const mutate of [
      receipt => receipt.runs.pop(),
      receipt => { receipt.runs[0].code = 1; },
      receipt => { receipt.runs[0].signal = 'SIGTERM'; },
      receipt => { receipt.manifestSha256 = '0'.repeat(64); },
      receipt => { receipt.runContext.runId = 'another-run'; },
      receipt => { receipt.shard = 4; },
      receipt => { receipt.runs[0].file = 'test-other-e2e.js'; },
      receipt => { receipt.runs[0].durationMs = -1; },
    ]) {
      const receipt = JSON.parse(original);
      mutate(receipt);
      fs.writeFileSync(file, JSON.stringify(receipt));
      assert.throws(() => aggregateShards(options));
    }
    fs.unlinkSync(file);
    assert.throws(() => aggregateShards(options));
    fs.writeFileSync(file, original);
  }
  const missingLog = path.join(artifactsDir, 'e2e-shard-3/e2e-output.txt');
  fs.unlinkSync(missingLog);
  assert.throws(() => aggregateShards(options));
});

test('valid earlier-attempt successes carry over, but wrong run/head or future attempts cannot', t => {
  const f = fixture(t);
  for (const shard of [1, 2, 3]) assert.equal(f.run(shard).status, 0);
  const artifactsDir = f.artifacts();
  const options = { root: f.root, manifestFile: f.manifestFile, artifactsDir, outputDir: path.join(f.root, 'report'), env: successfulResults({ GITHUB_RUN_ID: '123', GITHUB_RUN_ATTEMPT: '2', GITHUB_SHA: 'abc' }) };
  for (const shard of [1, 2, 3]) {
    const file = path.join(artifactsDir, `e2e-shard-${shard}/result.json`);
    const receipt = JSON.parse(fs.readFileSync(file, 'utf8'));
    receipt.runContext = { runId: '123', attempt: shard === 2 ? '2' : '1', headSha: 'abc' };
    fs.writeFileSync(file, JSON.stringify(receipt));
  }
  assert.equal(aggregateShards(options).status, 'success');
  const file = path.join(artifactsDir, 'e2e-shard-1/result.json');
  const original = fs.readFileSync(file, 'utf8');
  for (const mutation of [
    receipt => { receipt.runContext.attempt = '3'; },
    receipt => { receipt.runContext.attempt = '0'; },
    receipt => { receipt.runContext.attempt = 'not-a-number'; },
    receipt => { receipt.runContext.headSha = 'different'; },
    receipt => { receipt.runContext.runId = 'different'; },
  ]) {
    const receipt = JSON.parse(original);
    mutation(receipt);
    fs.writeFileSync(file, JSON.stringify(receipt));
    assert.throws(() => aggregateShards(options));
  }
});
