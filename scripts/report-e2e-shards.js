#!/usr/bin/env node
'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { SHARDS, CORE_SUITE, readManifest, expectedRuns, runContext } = require('./ci/e2e-manifest');

function requireFile(file) {
  assert.ok(fs.lstatSync(file).isFile(), `missing or non-regular shard artifact: ${file}`);
  return fs.readFileSync(file, 'utf8');
}

function aggregateShards({ root, manifestFile, artifactsDir, outputDir = root, env = process.env }) {
  const { manifest, manifestSha256 } = readManifest(manifestFile, root);
  const current = runContext(env);
  const receipts = [];
  const logs = [];
  for (const shard of SHARDS) {
    assert.equal(env[`E2E_${shard}_RESULT`], 'success', `current shard ${shard} result must be success`);
    const folder = path.join(artifactsDir, `e2e-shard-${shard}`);
    const receipt = JSON.parse(requireFile(path.join(folder, 'result.json')));
    assert.equal(receipt.version, 1, `unsupported receipt version: shard ${shard}`);
    assert.equal(receipt.shard, shard, 'artifact assigned to wrong shard');
    assert.equal(receipt.manifestSha256, manifestSha256, `stale manifest receipt: shard ${shard}`);
    assert.deepEqual({ runId: receipt.runContext?.runId, headSha: receipt.runContext?.headSha },
      { runId: current.runId, headSha: current.headSha }, `receipt from a different CI run or head: shard ${shard}`);
    if (current.attempt === null) {
      assert.equal(receipt.runContext.attempt, null, 'local receipt unexpectedly has a CI attempt');
    } else {
      const previous = receipt.runContext.attempt;
      assert.ok(/^[1-9][0-9]*$/.test(current.attempt) && /^[1-9][0-9]*$/.test(previous), 'invalid receipt attempt');
      assert.ok(Number.isSafeInteger(Number(current.attempt)) && Number.isSafeInteger(Number(previous)), 'unsafe receipt attempt');
      // --failed reruns carry over already successful jobs. Their older
      // receipts remain valid for the same run/head/manifest, but never for
      // a newer attempt. The actual current needs.result was checked above.
      assert.ok(Number(previous) <= Number(current.attempt), `receipt from a future attempt: shard ${shard}`);
    }
    assert.equal(receipt.status, 'success', `shard ${shard} did not succeed`);
    assert.ok(Number.isFinite(Date.parse(receipt.startedAt)) && Number.isFinite(Date.parse(receipt.completedAt)), 'invalid receipt timestamps');
    assert.ok(Date.parse(receipt.completedAt) >= Date.parse(receipt.startedAt), 'receipt end precedes start');
    assert.ok(Array.isArray(receipt.runs), `missing run receipt: shard ${shard}`);
    const expected = expectedRuns(manifest, shard);
    assert.equal(receipt.runs.length, expected.length, `missing or extra suite execution: shard ${shard}`);
    for (const [index, run] of receipt.runs.entries()) {
      assert.deepEqual({ file: run.file, kind: run.kind, iteration: run.iteration }, expected[index], `incorrect suite execution: shard ${shard}, index ${index}`);
      assert.equal(run.code, 0, `failed child receipt: ${run.file}`);
      assert.equal(run.signal, null, `signalled child receipt: ${run.file}`);
      assert.ok(Number.isFinite(run.durationMs) && run.durationMs >= 0, `invalid duration: ${run.file}`);
    }
    logs.push(requireFile(path.join(folder, 'e2e-output.txt')));
    requireFile(path.join(folder, 'node-liveness-e2e-output.txt'));
    requireFile(path.join(folder, 'slideover-repeat-output.txt'));
    receipts.push(receipt);
  }
  const coreShard = manifest.suites.find(suite => suite.file === CORE_SUITE).shard;
  const coverageSource = path.join(artifactsDir, `e2e-shard-${coreShard}`, 'coverage');
  fs.mkdirSync(outputDir, { recursive: true });
  fs.writeFileSync(path.join(outputDir, 'e2e-output.txt'), logs.join('\n'));
  if (fs.existsSync(coverageSource)) {
    const coverageTarget = path.join(outputDir, '.nyc_output');
    fs.mkdirSync(coverageTarget, { recursive: true });
    for (const file of fs.readdirSync(coverageSource)) {
      assert.ok(/^[a-zA-Z0-9_-]+\.json$/.test(file), `unexpected coverage artifact: ${file}`);
      const raw = requireFile(path.join(coverageSource, file));
      JSON.parse(raw);
      fs.writeFileSync(path.join(coverageTarget, file), raw);
    }
  }
  const report = {
    version: 1, manifestSha256, status: 'success', coreShard,
    runContext: runContext(env),
    suiteCount: manifest.suites.length,
    shardDurationsMs: receipts.map(receipt => receipt.runs.reduce((total, run) => total + run.durationMs, 0)),
    receipts,
  };
  fs.writeFileSync(path.join(outputDir, 'e2e-shards-report.json'), JSON.stringify(report, null, 2) + '\n');
  return report;
}

if (require.main === module) {
  try {
    const root = process.cwd();
    const report = aggregateShards({
      root, manifestFile: process.env.E2E_MANIFEST || path.join(root, 'scripts/e2e-shards.json'),
      artifactsDir: process.env.E2E_ARTIFACTS || path.join(root, '.ci-e2e', 'downloaded'),
    });
    console.log(`Verified ${report.suiteCount} suites and every repeat across three successful shards`);
  } catch (error) { console.error(error.message); process.exitCode = 1; }
}
module.exports = { aggregateShards };
