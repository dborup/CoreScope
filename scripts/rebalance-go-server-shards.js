#!/usr/bin/env node
// Regenerates scripts/go-server-shards.json from GitHub runner timings.
//
// Adding a Test needs no manifest edit: everything that is not pinned runs in
// the last (catch-all) shard. Rebalance only when the shard times drift apart
// (the go-test job summary prints them) or after adding/removing a Test that
// takes more than ~10 s on CI:
//
//   gh run download <run-id> -R dborup/CoreScope -p 'go-server-*' -D /tmp/go-run-<run-id>
//   node scripts/rebalance-go-server-shards.js /tmp/go-run-<run-id> [/tmp/go-run-<other-id> ...]
//
// Each argument is one CI run: a directory holding the go-server-*/test.jsonl
// artifacts of all shards. Several runs are averaged per Test. The cheapest
// entries fill the catch-all shard up to 1/N of the total; the rest are
// pinned longest-first to the lightest of shards 1..N-1. Unmeasured Tests
// count as zero and stay in the catch-all.
'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { PACKAGE, sha256, discoverTests, validateManifest } = require('./ci/go-server-manifest');

const round = value => Math.round(value * 1000) / 1000;
// Six decimals keep float noise out of the manifest; the guard allows 1e-6.
const mean = values => Math.round(values.reduce((sum, value) => sum + value, 0) / values.length * 1e6) / 1e6;

function findTestJSON(directory) {
  const found = [];
  for (const entry of fs.readdirSync(directory, { withFileTypes: true }).sort((a, b) => a.name.localeCompare(b.name))) {
    const file = path.join(directory, entry.name);
    if (entry.isDirectory()) found.push(...findTestJSON(file));
    else if (entry.isFile() && entry.name === 'test.jsonl') found.push(file);
  }
  return found;
}

// Top-level pass/skip Elapsed per Test; subtests are part of their parent.
function readTimingInputs(inputs) {
  const samples = new Map();
  const sources = [];
  for (const input of inputs) {
    const files = fs.existsSync(input) ? findTestJSON(input) : [];
    assert.ok(files.length > 0, `no test.jsonl under ${input}`);
    const run = new Map();
    const source = { input: path.basename(path.resolve(input)), runContexts: [], files: [] };
    for (const file of files) {
      const raw = fs.readFileSync(file, 'utf8');
      source.files.push({ file: path.relative(input, file), sha256: sha256(raw) });
      const receiptFile = path.join(path.dirname(file), 'receipt.json');
      if (fs.existsSync(receiptFile)) {
        const context = JSON.stringify(JSON.parse(fs.readFileSync(receiptFile, 'utf8')).runContext);
        if (!source.runContexts.includes(context)) source.runContexts.push(context);
      }
      for (const [index, line] of raw.split('\n').entries()) {
        if (!line.trim()) continue;
        let event;
        try { event = JSON.parse(line); } catch { assert.fail(`${file}:${index + 1} is not go test -json output`); }
        if (!event.Test || event.Test.includes('/') || !['pass', 'skip'].includes(event.Action)) continue;
        assert.ok(Number.isFinite(event.Elapsed) && event.Elapsed >= 0, `invalid Elapsed for ${event.Test} in ${file}`);
        assert.ok(!run.has(event.Test), `${event.Test} ran twice in one CI run (${input})`);
        run.set(event.Test, event.Elapsed);
      }
    }
    source.runContexts = source.runContexts.map(context => JSON.parse(context));
    for (const [name, seconds] of run) {
      if (!samples.has(name)) samples.set(name, []);
      samples.get(name).push(seconds);
    }
    sources.push(source);
  }
  return { samples, sources };
}

function planShards({ inventory, samples, shards }) {
  assert.ok(Number.isInteger(shards) && shards >= 2, 'need at least two shards');
  const entries = inventory.filter(source => source.active).map(source => {
    const values = samples.get(source.name) || [];
    return { name: source.name, samplesSeconds: values, meanSeconds: values.length ? mean(values) : 0 };
  });
  const total = entries.reduce((sum, entry) => sum + entry.meanSeconds, 0);
  assert.ok(total > 0, 'no measured Go test time');
  const byName = (a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0);
  const ascending = [...entries].sort((a, b) => a.meanSeconds - b.meanSeconds || byName(a, b));
  let catchAllSeconds = 0;
  let split = 0;
  while (split < ascending.length && catchAllSeconds + ascending[split].meanSeconds <= total / shards) {
    catchAllSeconds += ascending[split++].meanSeconds;
  }
  const heavy = ascending.slice(split).sort((a, b) => b.meanSeconds - a.meanSeconds || byName(a, b));
  assert.ok(heavy.length >= shards - 1, `only ${heavy.length} entries left for ${shards - 1} pinned shards`);
  const bins = Array(shards - 1).fill(0);
  const pinned = heavy.map(entry => {
    const index = bins.indexOf(Math.min(...bins));
    bins[index] += entry.meanSeconds;
    return { name: entry.name, shard: index + 1, samplesSeconds: entry.samplesSeconds, meanSeconds: entry.meanSeconds };
  });
  return {
    pinned: pinned.sort((a, b) => a.shard - b.shard || b.meanSeconds - a.meanSeconds || byName(a, b)),
    projectedSeconds: [...bins, catchAllSeconds].map(round),
    unmeasured: entries.filter(entry => entry.samplesSeconds.length === 0).map(entry => entry.name).sort(),
  };
}

function buildManifest({ plan, sources, shards }) {
  return {
    version: 2, shards, package: PACKAGE,
    metadata: {
      generatedBy: 'scripts/rebalance-go-server-shards.js',
      method: `Top-level go test -json Elapsed from GitHub runners, mean per Test over the source runs. The cheapest entries fill catch-all shard ${shards} up to 1/${shards} of the total; the rest are pinned longest-first to the lightest of shards 1..${shards - 1}. Unpinned and new Tests run in shard ${shards}.`,
      projectedTestSeconds: plan.projectedSeconds,
      unmeasuredEntries: plan.unmeasured.length,
      sources,
    },
    pinned: plan.pinned.map(({ name, shard, samplesSeconds, meanSeconds }) => ({ name, shard, samplesSeconds, meanSeconds })),
  };
}

// One pin per line keeps rebalance diffs reviewable.
function formatManifest(manifest) {
  const { pinned, ...head } = manifest;
  const top = JSON.stringify(head, null, 2);
  return `${top.slice(0, -2)},\n  "pinned": [\n${pinned.map(pin => `    ${JSON.stringify(pin)}`).join(',\n')}\n  ]\n}\n`;
}

function main(argv) {
  const root = process.cwd();
  let output = path.join(root, 'scripts/go-server-shards.json');
  let shards = fs.existsSync(output) ? JSON.parse(fs.readFileSync(output, 'utf8')).shards : 3;
  const inputs = [];
  for (let index = 0; index < argv.length; index++) {
    if (argv[index] === '--shards') shards = Number(argv[++index]);
    else if (argv[index] === '--output') output = path.resolve(argv[++index]);
    else inputs.push(argv[index]);
  }
  assert.ok(inputs.length > 0, 'usage: rebalance-go-server-shards.js [--shards N] [--output file] <ci-run-dir>...');
  const inventory = discoverTests(root);
  const { samples, sources } = readTimingInputs(inputs);
  const plan = planShards({ inventory, samples, shards });
  const manifest = validateManifest(buildManifest({ plan, sources, shards }), root, inventory);
  fs.writeFileSync(output, formatManifest(manifest));
  const counts = Array.from({ length: shards - 1 }, (_, index) => plan.pinned.filter(pin => pin.shard === index + 1).length);
  console.log(`Wrote ${path.relative(root, output)}: pinned ${counts.join(' / ')} entries to shards 1..${shards - 1}, the rest run in catch-all shard ${shards}.`);
  console.log(`Projected test seconds per shard: ${plan.projectedSeconds.join(' / ')} (${plan.unmeasured.length} active entries unmeasured, counted as 0).`);
}

if (require.main === module) {
  try { main(process.argv.slice(2)); } catch (error) { console.error(error.message); process.exitCode = 1; }
}
module.exports = { readTimingInputs, planShards, buildManifest, formatManifest };
