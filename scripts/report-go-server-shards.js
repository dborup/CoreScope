#!/usr/bin/env node
'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { PACKAGE, sha256, readManifest, assignedTests, runContext, requireContext } = require('./ci/go-server-manifest');
const { verifyTestJSON } = require('./run-go-server-shard');

function requireFile(file) {
  assert.ok(fs.lstatSync(file).isFile(), `missing or non-regular Go artifact: ${file}`);
  return fs.readFileSync(file, 'utf8');
}

function parseCoverprofile(raw) {
  const lines = raw.trimEnd().split('\n');
  assert.equal(lines.shift(), 'mode: atomic', 'coverage must preserve the race-enabled atomic mode');
  assert.ok(lines.length > 0, 'empty coverage blocks');
  const blocks = new Map();
  for (const line of lines) {
    const match = /^([^\s]+):(\d+)\.(\d+),(\d+)\.(\d+) (\d+) (\d+)$/.exec(line);
    assert.ok(match, `invalid coverage block: ${line}`);
    const [file, startLine, startCol, endLine, endCol, statements, count] = match.slice(1);
    assert.ok(file.startsWith(PACKAGE + '/') && !file.includes('..'), `unexpected coverage package: ${file}`);
    for (const value of [startLine, startCol, endLine, endCol, statements]) assert.ok(Number.isSafeInteger(Number(value)), 'unsafe coverage location or statement count');
    assert.ok(Number(startLine) > 0 && Number(startCol) > 0 && Number(endLine) > 0 && Number(endCol) > 0, 'coverage locations must be positive');
    assert.ok(Number(endLine) > Number(startLine) || (Number(endLine) === Number(startLine) && Number(endCol) >= Number(startCol)), 'coverage block end precedes start');
    const location = `${file}:${startLine}.${startCol},${endLine}.${endCol}`;
    assert.ok(!blocks.has(location), `duplicate coverage block: ${location}`);
    assert.ok(BigInt(count) <= 9223372036854775807n, 'coverage count exceeds Go coverprofile range');
    blocks.set(location, { location, statements: Number(statements), count: BigInt(count) });
  }
  return blocks;
}

function coverageStats(blocks) {
  let totalStatements = 0;
  let coveredStatements = 0;
  for (const block of blocks.values()) {
    totalStatements += block.statements;
    if (block.count > 0n) coveredStatements += block.statements;
  }
  assert.ok(Number.isSafeInteger(totalStatements) && totalStatements > 0, 'invalid coverage statement denominator');
  return { totalStatements, coveredStatements, coveragePercent: coveredStatements / totalStatements * 100 };
}

function mergeCoverprofiles(profiles) {
  assert.ok(Array.isArray(profiles) && profiles.length > 0, 'no coverage profiles');
  const merged = parseCoverprofile(profiles[0]);
  for (const raw of profiles.slice(1)) {
    const next = parseCoverprofile(raw);
    assert.equal(next.size, merged.size, 'coverage block set differs between shards');
    for (const [location, block] of merged) {
      const other = next.get(location);
      assert.ok(other, `missing coverage block: ${location}`);
      assert.equal(other.statements, block.statements, `coverage statement count differs: ${location}`);
      block.count += other.count;
      assert.ok(block.count <= 9223372036854775807n, 'merged coverage count exceeds Go coverprofile range');
    }
  }
  const profile = 'mode: atomic\n' + [...merged.values()].map(block => `${block.location} ${block.statements} ${block.count}`).join('\n') + '\n';
  return { profile, ...coverageStats(merged) };
}

// Ingestor keeps its original non-race coverage profile and independent
// badge. Its artifact must come from this same workflow checkout too.
function writeIngestorReceipt(root, env = process.env) {
  const raw = requireFile(path.join(root, 'cmd/ingestor/ingestor-coverage.out'));
  assert.ok(raw.startsWith('mode: set\n') && raw.trim().split('\n').length > 1, 'missing ingestor coverage profile');
  const folder = path.join(root, '.ci-go/ingestor');
  fs.mkdirSync(folder, { recursive: true });
  fs.writeFileSync(path.join(folder, 'profile.out'), raw);
  fs.writeFileSync(path.join(folder, 'receipt.json'), JSON.stringify({
    version: 1, status: 'success', runContext: runContext(env), profileSha256: sha256(raw),
  }, null, 2) + '\n');
}

function verifyIngestorArtifact(root, folder, env = process.env) {
  assert.equal(env.GO_INGESTOR_RESULT, 'success', 'current Go ingestor job must succeed');
  assert.deepEqual(fs.readdirSync(folder).sort(), ['profile.out', 'receipt.json'], 'missing or unexpected ingestor artifact');
  const receipt = JSON.parse(requireFile(path.join(folder, 'receipt.json')));
  assert.equal(receipt.version, 1, 'unsupported ingestor receipt version');
  assert.equal(receipt.status, 'success', 'ingestor receipt must succeed');
  requireContext(receipt.runContext, runContext(env));
  const raw = requireFile(path.join(folder, 'profile.out'));
  assert.ok(raw.startsWith('mode: set\n') && raw.trim().split('\n').length > 1, 'missing ingestor coverage profile');
  assert.equal(sha256(raw), receipt.profileSha256, 'ingestor coverage artifact differs from receipt');
  fs.mkdirSync(path.join(root, 'cmd/ingestor'), { recursive: true });
  fs.writeFileSync(path.join(root, 'cmd/ingestor/ingestor-coverage.out'), raw);
}

function aggregateShards({ root, manifestFile, artifactsDir, outputDir = root, env = process.env, inventory }) {
  const discovered = readManifest(manifestFile, root, inventory);
  const { manifest, manifestSha256 } = discovered;
  const current = runContext(env);
  assert.equal(env.GO_GUARD_RESULT, 'success', 'current Go assignment guard must succeed');
  assert.equal(env.GO_INGESTOR_RESULT, 'success', 'current Go ingestor job must succeed');
  const expectedFolders = Array.from({ length: manifest.shards }, (_, index) => `go-server-${index + 1}`).sort();
  assert.deepEqual(fs.readdirSync(artifactsDir).sort(), expectedFolders, 'missing or unexpected Go shard artifact');
  const profiles = [];
  const receipts = [];
  for (let shard = 1; shard <= manifest.shards; shard++) {
    assert.equal(env[`GO_SERVER_${shard}_RESULT`], 'success', `current Go shard ${shard} result must succeed`);
    const folder = path.join(artifactsDir, `go-server-${shard}`);
    const receipt = JSON.parse(requireFile(path.join(folder, 'receipt.json')));
    assert.equal(receipt.version, 1, 'unsupported Go receipt version');
    assert.equal(receipt.shard, shard, 'Go receipt belongs to another shard');
    assert.equal(receipt.package, PACKAGE, 'Go receipt package differs');
    assert.equal(receipt.manifestSha256, manifestSha256, 'stale Go manifest receipt');
    requireContext(receipt.runContext, current);
    assert.equal(receipt.status, 'success', `Go shard ${shard} failed`);
    const selected = assignedTests(manifest, discovered.inventory, shard);
    assert.deepEqual(receipt.selected, selected.map(test => ({ name: test.name, file: test.file, kind: test.kind })), 'Go receipt selection differs from manifest');
    assert.ok(Number.isFinite(Date.parse(receipt.startedAt)) && Number.isFinite(Date.parse(receipt.completedAt)), 'invalid Go receipt timestamps');
    assert.ok(Date.parse(receipt.completedAt) >= Date.parse(receipt.startedAt), 'Go receipt end precedes start');
    assert.ok(Date.parse(receipt.completedAt) <= Date.now() + 60_000, 'Go receipt is from the future');
    assert.ok(Array.isArray(receipt.runs) && receipt.runs.length === 2, 'missing Go build/test execution');
    for (const [index, run] of receipt.runs.entries()) {
      assert.equal(run.kind, index === 0 ? 'build' : 'test', 'unexpected Go process execution');
      assert.equal(run.code, 0, 'Go process failed');
      assert.equal(run.signal, null, 'Go process terminated by a signal');
      assert.ok(Number.isFinite(run.durationMs) && run.durationMs >= 0, 'invalid Go process duration');
    }
    const rawJSON = requireFile(path.join(folder, 'test.jsonl'));
    assert.equal(sha256(rawJSON), receipt.testJSONSha256, 'Go JSON artifact differs from receipt');
    assert.deepEqual(verifyTestJSON(rawJSON, selected).tests, receipt.tests, 'Go test execution receipt differs from actual JSON');
    const rawProfile = requireFile(path.join(folder, 'profile.out'));
    assert.equal(sha256(rawProfile), receipt.profileSha256, 'Go coverage artifact differs from receipt');
    profiles.push(rawProfile);
    receipts.push(receipt);
  }
  const merged = mergeCoverprofiles(profiles);
  const coverageFile = path.join(outputDir, 'cmd/server/server-coverage.out');
  fs.mkdirSync(path.dirname(coverageFile), { recursive: true });
  fs.writeFileSync(coverageFile, merged.profile);
  const report = {
    version: 1, status: 'success', manifestSha256, runContext: current,
    testCount: receipts.reduce((total, receipt) => total + receipt.tests.length, 0),
    shards: manifest.shards, totalStatements: merged.totalStatements,
    coveredStatements: merged.coveredStatements, coveragePercent: merged.coveragePercent,
    coverageSha256: sha256(merged.profile),
    shardDurationsMs: receipts.map(receipt => receipt.runs.reduce((total, run) => total + run.durationMs, 0)),
    receipts,
  };
  fs.mkdirSync(path.join(outputDir, '.ci-go'), { recursive: true });
  fs.writeFileSync(path.join(outputDir, '.ci-go/server-shards-report.json'), JSON.stringify(report, null, 2) + '\n');
  console.log(`Go server coverage: ${merged.coveredStatements}/${merged.totalStatements} statements (${merged.coveragePercent.toFixed(1)}%)`);
  if (env.GITHUB_STEP_SUMMARY) fs.appendFileSync(env.GITHUB_STEP_SUMMARY, `## Go server shards\n\n${report.testCount} runnable entries verified across ${report.shards} shards.\n\nCoverage: ${merged.coveredStatements}/${merged.totalStatements} statements (${merged.coveragePercent.toFixed(1)}%).\n`);
  return report;
}

if (require.main === module) {
  try {
    const root = process.cwd();
    assert.ok(process.argv.length <= 3, 'unexpected Go report arguments');
    if (process.argv[2] === '--ingestor-receipt') writeIngestorReceipt(root);
    else {
      assert.equal(process.argv[2], undefined, 'unexpected Go report argument');
      aggregateShards({ root,
        manifestFile: process.env.GO_SERVER_MANIFEST || path.join(root, 'scripts/go-server-shards.json'),
        artifactsDir: process.env.GO_SERVER_ARTIFACTS || path.join(root, '.ci-go/downloaded'),
      });
      verifyIngestorArtifact(root, path.join(root, '.ci-go/ingestor-downloaded'));
    }
  } catch (error) { console.error(error.message); process.exitCode = 1; }
}
module.exports = { parseCoverprofile, coverageStats, mergeCoverprofiles, aggregateShards, writeIngestorReceipt, verifyIngestorArtifact };
