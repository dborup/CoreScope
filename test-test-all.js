/* test-test-all.js (#174)
 *
 * Self-test for test-all.sh, the unit suite CI runs.
 *
 * 1. Behaviour: a failing file does not stop the run, the summary lists every
 *    failed file, and the exit code is non-zero. The real test-all.sh is run
 *    under /bin/sh (and dash when present) in a temp dir, with its file list
 *    swapped for small fixture files.
 * 2. File list: every entry exists, appears once and runs without a server
 *    or a browser (E2E files belong in the Playwright step of deploy.yml).
 * 3. Registry: every root test-*.js runs in test-all.sh or deploy.yml, or is
 *    listed in KNOWN_UNREGISTERED with a reason. New files cannot go unrun.
 */
'use strict';

const assert = require('assert');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawnSync } = require('child_process');

const ROOT = __dirname;
const SRC = fs.readFileSync(path.join(ROOT, 'test-all.sh'), 'utf8');
const RUN_LINE = /^run (\S+)$/;

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

function registeredFiles() {
  return SRC.split('\n').map((l) => RUN_LINE.exec(l)).filter(Boolean).map((m) => m[1]);
}

// test-all.sh with its file list replaced by `files`.
function scriptWith(files) {
  const out = [];
  let inserted = false;
  for (const line of SRC.split('\n')) {
    if (!RUN_LINE.test(line)) { out.push(line); continue; }
    if (!inserted) { files.forEach((f) => out.push('run ' + f)); inserted = true; }
  }
  assert(inserted, 'test-all.sh has no `run <file>` lines');
  return out.join('\n');
}

const FIXTURES = {
  'pass-a.js': "console.log('pass-a ran');\n",
  'fail-b.js': "console.log('fail-b ran'); process.exit(3);\n",
  'throw-c.js': "throw new Error('boom');\n",
  'pass-d.js': "console.log('pass-d ran');\n",
};

function runSuite(shell, files) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'test-all-'));
  try {
    for (const [name, body] of Object.entries(FIXTURES)) fs.writeFileSync(path.join(dir, name), body);
    fs.writeFileSync(path.join(dir, 'test-all.sh'), scriptWith(files));
    const r = spawnSync(shell, ['test-all.sh'], { cwd: dir, encoding: 'utf8' });
    return { status: r.status, out: r.stdout + r.stderr };
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
}

// The indented lines under "Failed:" in the summary.
function failedList(out) {
  const lines = out.split('\n');
  const i = lines.findIndex((l) => l.trim() === 'Failed:');
  if (i < 0) return [];
  const list = [];
  for (let j = i + 1; j < lines.length && /^ {4}\S/.test(lines[j]); j++) list.push(lines[j].trim());
  return list;
}

const shells = [];
for (const s of ['/bin/sh', '/bin/dash', '/usr/bin/dash']) {
  if (!fs.existsSync(s)) continue;
  const real = fs.realpathSync(s);
  if (!shells.some((x) => x.real === real)) shells.push({ path: s, real });
}

console.log('test-all.sh behaviour');
for (const { path: sh } of shells) {
  test(`${sh}: failures do not stop the run, are listed, and exit non-zero`, () => {
    const r = runSuite(sh, ['pass-a.js', 'fail-b.js', 'throw-c.js', 'missing-e.js', 'pass-d.js']);
    assert.notStrictEqual(r.status, 0, 'exit status must be non-zero, got ' + r.status);
    assert(r.out.includes('pass-d ran'), 'pass-d.js (after the failures) must still run');
    assert(r.out.includes('2 passed, 3 failed (5 files)'), 'summary counts missing:\n' + r.out);
    assert.deepStrictEqual(failedList(r.out), ['fail-b.js', 'throw-c.js', 'missing-e.js']);
    assert(!r.out.includes('All tests passed'), 'must not claim success');
  });

  test(`${sh}: all green exits 0 with no failure list`, () => {
    const r = runSuite(sh, ['pass-a.js', 'pass-d.js']);
    assert.strictEqual(r.status, 0, 'exit status ' + r.status + '\n' + r.out);
    assert(r.out.includes('2 passed, 0 failed (2 files)'), 'summary counts missing:\n' + r.out);
    assert(r.out.includes('All tests passed'));
    assert.deepStrictEqual(failedList(r.out), []);
  });
}

console.log('test-all.sh file list');
const files = registeredFiles();

test('every registered file exists', () => {
  const missing = files.filter((f) => !fs.existsSync(path.join(ROOT, f)));
  assert.deepStrictEqual(missing, []);
});

test('no file is registered twice', () => {
  const seen = new Set();
  const dup = files.filter((f) => (seen.has(f) ? true : (seen.add(f), false)));
  assert.deepStrictEqual(dup, []);
});

test('no registered file needs a browser or a server (E2E belongs in deploy.yml)', () => {
  const e2e = /require\(['"](@playwright\/test|playwright)['"]\)|chromium\.launch|process\.env\.BASE_URL/;
  const bad = files.filter((f) => e2e.test(fs.readFileSync(path.join(ROOT, f), 'utf8')));
  assert.deepStrictEqual(bad, []);
});

test('test-all.sh registers itself through this test', () => {
  assert(files.includes('test-test-all.js'));
});

// Root test files that no runner runs yet, with the reason. Fix one, register
// it, and delete its entry here. Do not add a file to this list to get a new
// test past the check below: register it instead.
const KNOWN_UNREGISTERED = {
  // red unit tests (stale after intended UI changes)
  'test-channel-colors.js': 'red: expects 4px border + tint, #675 changed it to 3px',
  'test-channel-ux-followup.js': 'red: copy text changed by the Phosphor migration',
  'test-channel-ux-round2.js': 'red: expects the 📤 glyph, now #ph-share-network',
  'test-customizer-v2.js': 'red: computeEffective adds home defaults since #525',
  'test-drag-manager.js': 'red: removeAttribute mock does not update dataset (#1567)',
  'test-fluid-scaffolding.js': 'red: reads only the first :root block',
  'test-hop-resolver-affinity.js': 'red: fixture geometry wrong since #874',
  'test-issue-1470-card-bg-contrast.js': 'red: indexOf matches a style.css comment',
  'test-issue-1646-compare-polish.js': 'red: font-size parser reads a comment',
  'test-packets.js': 'red: 13 emoji assertions after the Phosphor migration',
  'test-perf-disk-io-1120.js': 'red: ⚠️ is #ph-warning; anomaly detector reworked (#1593)',
  // need something CI's unit job does not have
  'test-marker-outline-weight.js': 'needs @playwright/test (not a dependency) and a server',
  'test-table-sort.js': 'needs jsdom (not a dependency)',
  'test-touch-targets.js': 'red in Chromium: expects 48px targets, CSS has 44px',
  // E2E
  'test-channel-modal-e2e.js': 'red: Add button text and sidebar sections changed',
  'test-issue-1522-trace-url-sync-e2e.js': 'needs @playwright/test (not a dependency)',
  'test-node-reach-e2e.js': 'red: #nqMap .leaflet-container never visible',
  'test-path-inspector-e2e.js': 'needs @playwright/test (not a dependency)',
  'test-rx-coverage-mobile-nav-e2e.js': 'skips while clientRxCoverage is off (the default)',
};

console.log('root test registry');
const deploy = fs.readFileSync(path.join(ROOT, '.github/workflows/deploy.yml'), 'utf8');
const inDeploy = new Set([...deploy.matchAll(/\bnode (test-[\w.-]+\.js)\b/g)].map((m) => m[1]));
const runSomewhere = (f) => files.includes(f) || inDeploy.has(f);
const rootTests = fs.readdirSync(ROOT).filter((f) => /^test-.*\.js$/.test(f));

test('every root test-*.js runs in test-all.sh or deploy.yml', () => {
  const orphans = rootTests.filter((f) => !runSomewhere(f) && !(f in KNOWN_UNREGISTERED));
  assert.deepStrictEqual(orphans, [], orphans.join(', ') + ' run nowhere: add `run <file>` to ' +
    'test-all.sh (unit) or a line to the Playwright step in deploy.yml (E2E)');
});

test('KNOWN_UNREGISTERED has no stale entries', () => {
  const stale = Object.keys(KNOWN_UNREGISTERED).filter((f) => !rootTests.includes(f) || runSomewhere(f));
  assert.deepStrictEqual(stale, [], stale.join(', ') + ': deleted or now registered, remove from KNOWN_UNREGISTERED');
});

console.log(`\n${passed} passed, ${failed} failed`);
process.exit(failed ? 1 : 0);
