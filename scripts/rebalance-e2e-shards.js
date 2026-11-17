#!/usr/bin/env node
// Regenerates the shard assignment in scripts/e2e-shards.json from the
// receipts of green CI runs (#869). Rebalance when the Playwright E2E shard
// durations in the CI job summary drift apart (slowest > ~1.25x fastest), or
// after adding several slow suites:
//
//   gh run download <run-id> -R dborup/CoreScope -p 'e2e-shard-*' -D /tmp/e2e-run-<run-id>
//   node scripts/rebalance-e2e-shards.js /tmp/e2e-run-<run-id> [/tmp/e2e-run-<other-id> ...]
//
// Each argument is one CI run: a directory holding e2e-shard-{1,2,3}/result.json.
// Several runs are averaged per suite. Suites are assigned longest-first to the
// lightest shard. A suite that no run measured keeps its old meanSeconds.
// Suite order within a shard follows the existing manifest order.
'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { SHARDS, validateManifest } = require('./ci/e2e-manifest');

const round3 = value => Math.round(value * 1000) / 1000;
const mean = values => values.reduce((sum, value) => sum + value, 0) / values.length;

// Returns Map file -> { suite: [seconds per run], repeat: [seconds per run] }.
function readTimingInputs(inputs) {
  const samples = new Map();
  for (const input of inputs) {
    const run = new Map();
    for (const shard of SHARDS) {
      const file = path.join(input, `e2e-shard-${shard}`, 'result.json');
      assert.ok(fs.existsSync(file), `missing receipt ${file}`);
      const receipt = JSON.parse(fs.readFileSync(file, 'utf8'));
      assert.equal(receipt.status, 'success', `${file} is not from a green shard`);
      for (const entry of receipt.runs) {
        assert.ok(Number.isFinite(entry.durationMs) && entry.durationMs >= 0, `invalid duration for ${entry.file} in ${file}`);
        if (!run.has(entry.file)) run.set(entry.file, { suite: 0, repeat: 0, seenSuite: false, seenRepeat: false });
        const slot = run.get(entry.file);
        if (entry.kind === 'suite') { slot.suite += entry.durationMs / 1000; slot.seenSuite = true; }
        else { slot.repeat += entry.durationMs / 1000; slot.seenRepeat = true; }
      }
    }
    for (const [name, slot] of run) {
      if (!samples.has(name)) samples.set(name, { suite: [], repeat: [] });
      if (slot.seenSuite) samples.get(name).suite.push(round3(slot.suite));
      if (slot.seenRepeat) samples.get(name).repeat.push(round3(slot.repeat));
    }
  }
  return { samples };
}

// weights: [[name, seconds], ...]. Longest first onto the lightest shard.
function planShards({ shards = SHARDS.length, weights }) {
  const sorted = [...weights].sort((a, b) => b[1] - a[1] || (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0));
  const bins = Array(shards).fill(0);
  const shardOf = new Map();
  for (const [name, seconds] of sorted) {
    const index = bins.indexOf(Math.min(...bins));
    bins[index] += seconds;
    shardOf.set(name, index + 1);
  }
  return { shardOf, projectedSeconds: bins.map(round3) };
}

function rebalanceManifest({ manifest, root, inputs }) {
  const { samples } = readTimingInputs(inputs);
  const old = manifest.suites;
  const measured = old.map(suite => {
    const sample = samples.get(suite.file);
    const suiteSeconds = sample && sample.suite.length ? sample.suite : null;
    return {
      suite,
      meanSeconds: suiteSeconds ? round3(mean(suiteSeconds)) : suite.meanSeconds || 0,
      secondsByExecution: suiteSeconds || suite.secondsByExecution,
      repeatSeconds: sample && sample.repeat.length ? round3(mean(sample.repeat)) : 0,
    };
  });
  const plan = planShards({ weights: measured.map(m => [m.suite.file, m.meanSeconds + m.repeatSeconds]) });
  const suites = measured.map(m => {
    const next = { ...m.suite, shard: plan.shardOf.get(m.suite.file), meanSeconds: m.meanSeconds };
    if (m.secondsByExecution) next.secondsByExecution = m.secondsByExecution;
    else delete next.secondsByExecution;
    if (m.repeatSeconds) next.repeatSeconds = m.repeatSeconds;
    return next;
  });
  // Stable order: shard by shard, original relative order inside a shard.
  const ordered = [...suites].map((suite, index) => ({ suite, index }))
    .sort((a, b) => a.suite.shard - b.suite.shard || a.index - b.index).map(item => item.suite);
  // shardPredictions of the old baseline would be stale after a rebalance.
  const { shardPredictions, algorithm, ...keptMetadata } = manifest.metadata || {};
  const out = {
    ...manifest,
    metadata: {
      ...keptMetadata,
      generatedBy: 'scripts/rebalance-e2e-shards.js',
      method: 'Mean suite seconds (plus repeat-gate seconds) from e2e-shard-N/result.json receipts of green CI runs; longest first onto the lightest shard; unmeasured suites keep their old meanSeconds. Order inside a shard follows the previous manifest.',
      projectedSeconds: plan.projectedSeconds,
      sourceRuns: inputs.map(input => path.basename(path.resolve(input))),
    },
    suites: ordered,
  };
  return validateManifest(out, root);
}

// One suite per line keeps rebalance diffs reviewable.
function formatManifest(manifest) {
  const { suites, ...head } = manifest;
  const { exclusions, ...rest } = head;
  const top = JSON.stringify({ ...rest, exclusions }, null, 2);
  return `${top.slice(0, -2)},\n  "suites": [\n${suites.map(suite => `    ${JSON.stringify(suite)}`).join(',\n')}\n  ]\n}\n`;
}

function main(argv) {
  const root = process.cwd();
  let output = path.join(root, 'scripts/e2e-shards.json');
  const inputs = [];
  for (let index = 0; index < argv.length; index++) {
    if (argv[index] === '--output') output = path.resolve(argv[++index]);
    else inputs.push(argv[index]);
  }
  assert.ok(inputs.length > 0, 'usage: rebalance-e2e-shards.js [--output file] <ci-run-dir>...');
  const manifest = JSON.parse(fs.readFileSync(path.join(root, 'scripts/e2e-shards.json'), 'utf8'));
  const out = rebalanceManifest({ manifest, root, inputs });
  fs.writeFileSync(output, formatManifest(out));
  console.log(`Wrote ${path.relative(root, output)}. Projected seconds per shard: ${out.metadata.projectedSeconds.join(' / ')}.`);
}

if (require.main === module) {
  try { main(process.argv.slice(2)); } catch (error) { console.error(error.message); process.exitCode = 1; }
}
module.exports = { readTimingInputs, planShards, rebalanceManifest, formatManifest };
