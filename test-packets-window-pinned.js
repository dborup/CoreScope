#!/usr/bin/env node
/* Guard: an E2E suite that needs real packet rows pins the time window.
 *
 * CI freshens the fixture once, at the start of each Playwright shard, so its
 * newest packets are "now" only then. The packets page shows the last 15
 * minutes by default (public/packets.js normalizeTimeWindow), so a suite that
 * opens #/packets with no saved or linked window finds no rows once it starts
 * more than 15 minutes into its shard. That is how #777 turned master red: the
 * new #728 ortho suite in shard 2 moved test-touch-gestures-coverage-e2e.js
 * from about 14 to 15.3 minutes after the freshen, and the suite saw an empty
 * table. The cause was the suite's dependence on shard timing, not #777's code.
 *
 * Every suite in scripts/e2e-shards.json that waits for `#pktBody
 * tr[data-hash]` must therefore set the window itself: through the shared
 * helper tests/e2e-packets-window.js, a `meshcore-time-window` localStorage
 * seed, or a `timeWindow=` link. A suite whose /api/packets is stubbed in the
 * browser does not depend on the fixture and is listed in STUBBED with the
 * reason.
 *
 * The helper is run here the way Playwright runs it, against the key and the
 * normalizeTimeWindow that packets.js really uses, so a helper that stores a
 * wrong key or a value packets.js turns back into 15 minutes fails (#787).
 *
 * Run: node test-packets-window-pinned.js
 */
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const ROOT = __dirname;
const ROW_SELECTOR = '#pktBody tr[data-hash]';
const PINS = [/\bpinPacketsTimeWindow\(/, /meshcore-time-window/, /timeWindow=/];

// Suites that answer /api/packets themselves (page.route), so the fixture's
// age cannot empty their table.
const STUBBED = {
  'test-issue-406-packets-live-channel-e2e.js': '/api/packets is fulfilled by page.route from an in-test packet list',
};

const checks = [];
function check(name, fn) { checks.push({ name, fn }); }

function suitesNeedingRows(manifest, read) {
  return manifest.suites
    .map(s => s.file)
    .filter(f => !STUBBED[f])
    .map(f => ({ file: f, src: read(f) }))
    .filter(s => s.src !== null && s.src.indexOf(ROW_SELECTOR) >= 0);
}

// A suite that uses the helper pins every context it opens: each
// `const X = await browser.newContext(` needs a `pinPacketsTimeWindow(X)`.
function unpinnedContexts(src) {
  if (!/\bpinPacketsTimeWindow\(/.test(src)) return [];
  const names = [...src.matchAll(/\bconst\s+(\w+)\s*=\s*await\s+\w+\.newContext\(/g)].map(m => m[1]);
  return names.filter(n => !new RegExp('\\bpinPacketsTimeWindow\\(\\s*' + n + '\\s*[,)]').test(src));
}

function unpinned(suites) {
  return suites
    .filter(s => !PINS.some(re => re.test(s.src)) || unpinnedContexts(s.src).length)
    .map(s => s.file);
}

function readSuite(f) {
  const p = path.join(ROOT, f);
  return fs.existsSync(p) ? fs.readFileSync(p, 'utf8') : null;
}

// What packets.js does with a saved window: the key it reads when the script
// is evaluated, and its own normalizeTimeWindow, taken from the source.
function packetsWindowReader() {
  const src = fs.readFileSync(path.join(ROOT, 'public/packets.js'), 'utf8');
  const key = /\bsavedTimeWindowMin\s*=\s*normalizeTimeWindow\(\s*localStorage\.getItem\(\s*'([^']+)'\s*\)/.exec(src);
  assert.ok(key, 'packets.js no longer reads its saved window as normalizeTimeWindow(localStorage.getItem(KEY), ...)');
  const fn = /^( *)function normalizeTimeWindow\(raw, mobile\) \{[\s\S]*?^\1\}/m.exec(src);
  assert.ok(fn, 'normalizeTimeWindow(raw, mobile) not found in packets.js');
  const normalize = vm.runInNewContext(fn[0] + '\nnormalizeTimeWindow', { Number });
  return { key: key[1], normalize, src };
}

// Runs pin(ctx, ...args) against a fake browser context and evaluates each
// init script as Playwright does, `(fn)(arg)` from the function's source, in a
// page whose localStorage is `storage`. Returns what the page stored.
async function storedByHelper(pin, args, storage) {
  const scripts = [];
  const ctx = { addInitScript: async (fn, arg) => { scripts.push({ fn, arg }); } };
  const ret = await pin(ctx, ...args);
  assert.strictEqual(ret, ctx, 'pinPacketsTimeWindow must return the context it pinned');
  assert.strictEqual(scripts.length, 1, 'expected one init script, got ' + scripts.length);
  assert.strictEqual(typeof scripts[0].fn, 'function', 'the init script must be a function');
  const store = {};
  const localStorage = storage || {
    setItem: (k, v) => { store[k] = String(v); },
    getItem: k => (k in store ? store[k] : null),
  };
  vm.runInNewContext('(' + scripts[0].fn.toString() + ')(' + JSON.stringify(scripts[0].arg) + ')', { localStorage, String });
  return store;
}

console.log('=== E2E suites that need packet rows pin the time window ===');

const manifest = JSON.parse(fs.readFileSync(path.join(ROOT, 'scripts/e2e-shards.json'), 'utf8'));
const suites = suitesNeedingRows(manifest, readSuite);

check('the scan finds the suites that wait for packet rows', () => {
  const files = suites.map(s => s.file);
  for (const f of ['test-touch-gestures-coverage-e2e.js', 'test-gestures-1062-e2e.js']) {
    assert.ok(files.includes(f), f + ' was not found by the scan (selector or manifest changed?)');
  }
});

check('every such suite pins the window (no dependence on time since the freshen)', () => {
  const bad = unpinned(suites);
  assert.deepStrictEqual(bad, [], 'suites that rely on the 15-minute default window: ' + bad.join(', '));
});

check('the STUBBED exemptions still stub /api/packets', () => {
  for (const f of Object.keys(STUBBED)) {
    const src = readSuite(f);
    assert.ok(src !== null, f + ' is exempt but missing');
    assert.ok(/page\.route\(/.test(src) && /\/api\/packets/.test(src), f + ' is exempt but no longer stubs /api/packets');
  }
});

// The detector itself: an unpinned row-waiting suite is flagged, and each pin
// form clears it.
check('detector: an unpinned suite is flagged and each pin form clears it', () => {
  const fake = { suites: [{ file: 'a.js' }, { file: 'b.js' }, { file: 'c.js' }, { file: 'd.js' }, { file: 'e.js' }] };
  const src = {
    'a.js': `await page.waitForSelector('${ROW_SELECTOR}')`,
    'b.js': `await pinPacketsTimeWindow(ctx); await page.waitForSelector('${ROW_SELECTOR}')`,
    'c.js': `localStorage.setItem('meshcore-time-window', '60'); '${ROW_SELECTOR}'`,
    'd.js': `page.goto(BASE + '/#/packets?timeWindow=60'); '${ROW_SELECTOR}'`,
    'e.js': `page.goto(BASE + '/#/nodes')`,
  };
  const found = suitesNeedingRows(fake, f => src[f]);
  assert.deepStrictEqual(found.map(s => s.file), ['a.js', 'b.js', 'c.js', 'd.js']);
  assert.deepStrictEqual(unpinned(found), ['a.js']);
});

check('detector: a helper user with one unpinned context is flagged', () => {
  const src = `const ctx = await browser.newContext({});\nawait pinPacketsTimeWindow(ctx);\n` +
    `const ctxD = await browser.newContext({});\n'${ROW_SELECTOR}'`;
  assert.deepStrictEqual(unpinnedContexts(src), ['ctxD']);
  assert.deepStrictEqual(unpinned([{ file: 'f.js', src }]), ['f.js']);
  assert.deepStrictEqual(unpinnedContexts(src + '\nawait pinPacketsTimeWindow(ctxD);'), []);
});

// The helper, run: packets.js must read back the pinned window, on a phone
// (mobile caps above 180 and turns 0 into 15) and on a desktop.
check('helper stores the window packets.js reads, on mobile and desktop', async () => {
  const { PINNED_WINDOW_MIN, pinPacketsTimeWindow } = require('./tests/e2e-packets-window');
  const { key, normalize, src } = packetsWindowReader();
  assert.ok(Number.isInteger(PINNED_WINDOW_MIN) && PINNED_WINDOW_MIN > 15,
    'PINNED_WINDOW_MIN must be wider than the 15-minute default, got ' + PINNED_WINDOW_MIN);
  assert.ok(src.indexOf('<option value="' + PINNED_WINDOW_MIN + '">') >= 0, 'PINNED_WINDOW_MIN is not a time-window option');
  const store = await storedByHelper(pinPacketsTimeWindow, []);
  assert.deepStrictEqual(Object.keys(store), [key], 'the helper must store packets.js\'s key ' + key);
  for (const mobile of [true, false]) {
    assert.strictEqual(normalize(store[key], mobile), PINNED_WINDOW_MIN,
      'packets.js (' + (mobile ? 'mobile' : 'desktop') + ') reads ' + JSON.stringify(store[key]) +
      ' as ' + normalize(store[key], mobile) + ' minutes, not the pinned ' + PINNED_WINDOW_MIN);
  }
});

check('helper stores an explicit window as given', async () => {
  const { pinPacketsTimeWindow } = require('./tests/e2e-packets-window');
  const { key, normalize } = packetsWindowReader();
  const store = await storedByHelper(pinPacketsTimeWindow, [60]);
  assert.strictEqual(normalize(store[key], true), 60);
});

// Init scripts also run in frames whose storage throws (about:blank, opaque
// origins); the seed must not turn that into a page error.
check('helper init script survives a localStorage that throws', async () => {
  const { pinPacketsTimeWindow } = require('./tests/e2e-packets-window');
  const throwing = { setItem: () => { throw new Error('SecurityError'); } };
  await storedByHelper(pinPacketsTimeWindow, [], throwing);
});

// Exact-hash assertions drop only the window the suite pinned itself.
check('withoutPinnedWindow removes only timeWindow=<pinned>', () => {
  const { withoutPinnedWindow: w, PINNED_WINDOW_MIN: m } = require('./tests/e2e-packets-window');
  assert.strictEqual(w('#/packets?timeWindow=' + m), '#/packets');
  assert.strictEqual(w('#/packets/ab12?timeWindow=' + m), '#/packets/ab12');
  assert.strictEqual(w('#/packets?timeWindow=' + m + '&hash=ab12'), '#/packets?hash=ab12');
  assert.strictEqual(w('#/packets?hash=ab12&timeWindow=' + m + '&obs=7'), '#/packets?hash=ab12&obs=7');
  assert.strictEqual(w('#/packets?timeWindow=60&hash=ab12'), '#/packets?timeWindow=60&hash=ab12');
  assert.strictEqual(w('#/packets?timeWindow=' + m + '0'), '#/packets?timeWindow=' + m + '0');
  assert.strictEqual(w('#/packets?hash=ab12'), '#/packets?hash=ab12');
  assert.strictEqual(w('#/live'), '#/live');
});

(async () => {
  let passed = 0, failed = 0;
  for (const { name, fn } of checks) {
    try { await fn(); passed++; console.log('  ✓ ' + name); }
    catch (e) { failed++; console.error('  ✗ ' + name + '\n    ' + e.message); }
  }
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
