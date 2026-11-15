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
 * Every suite in scripts/e2e-shards.json that reads the packet table's rows
 * (ROW_PATTERNS: a `#pktBody tr`/`td` selector, a `#pktTable tbody` selector,
 * or the rows of getElementById('pktBody')) must therefore set the window
 * itself: through the shared helper tests/e2e-packets-window.js, a
 * `meshcore-time-window` localStorage seed, or a `timeWindow=` link (#787). A
 * suite whose /api/packets is stubbed in the browser does not depend on the
 * fixture and is listed in STUBBED with the reason.
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
// A packets-page row read. The empty table still holds one <tr> ("No packets
// found"), so a suite that waits for `#pktBody tr` or counts rows > 0 passes
// on an empty window and then tests nothing, or fails further on.
const ROW_PATTERNS = [
  /#pktBody\s*(?:>\s*)?(?:tr|td)\b/,         // #pktBody tr[data-hash], > tr, td.col-path, row counts
  /#pktBody\s+[.[]/,                          // #pktBody .path-hops, a cell's contents
  /#pktTable\s+tbody\b/,                      // #pktTable tbody tr
  /getElementById\(\s*['"]pktBody['"]\s*\)/,  // .rows, .children, querySelectorAll on the body
];
const PINS = [/\bpinPacketsTimeWindow\(/, /meshcore-time-window/, /timeWindow=/];

// Suites that answer /api/packets themselves (page.route), so the fixture's
// age cannot empty their table.
const STUBBED = {
  'test-issue-406-packets-live-channel-e2e.js': '/api/packets is fulfilled by page.route from an in-test packet list',
};

const checks = [];
function check(name, fn) { checks.push({ name, fn }); }

function readsRows(src) {
  return ROW_PATTERNS.some(re => re.test(src));
}

function suitesNeedingRows(manifest, read) {
  return manifest.suites
    .map(s => s.file)
    .filter(f => !STUBBED[f])
    .map(f => ({ file: f, src: read(f) }))
    .filter(s => s.src !== null && readsRows(s.src));
}

// A suite that uses the helper pins every context it opens: each
// `const X = await browser.newContext(` needs a `pinPacketsTimeWindow(X)`
// before the next context is opened. Suites reuse names (`ctx` per block), so
// one pin must not cover every context of that name (#787).
function unpinnedContexts(src) {
  if (!/\bpinPacketsTimeWindow\(/.test(src)) return [];
  const decls = [...src.matchAll(/\bconst\s+(\w+)\s*=\s*await\s+\w+\.newContext\(/g)];
  return decls
    .filter((m, i) => {
      const own = src.slice(m.index, i + 1 < decls.length ? decls[i + 1].index : src.length);
      return !new RegExp('\\bpinPacketsTimeWindow\\(\\s*' + m[1] + '\\s*[,)]').test(own);
    })
    .map(m => m[1]);
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

check('the scan finds the suites that read packet rows', () => {
  const files = suites.map(s => s.file);
  for (const f of [
    'test-touch-gestures-coverage-e2e.js', 'test-gestures-1062-e2e.js',
    // #787: `#pktBody tr` waits and row counts, not only `tr[data-hash]`.
    'test-issue-1122-packets-filter-ux-e2e.js', 'test-issue-1128-multi-viewport-e2e.js',
    'test-issue-1128-packets-layout-e2e.js', 'test-issue-620-region-dropdown-viewport-e2e.js',
  ]) {
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

// The detector itself: each row read is found, a non-row mention is not, an
// unpinned row-reading suite is flagged, and each pin form clears it.
check('detector: every member of the row family is a row read', () => {
  for (const s of [
    "page.waitForSelector('#pktBody tr[data-hash]')",
    "page.locator('#pktBody tr').first().waitFor()",
    "document.querySelectorAll('#pktBody tr').length > 0",
    "page.locator('#pktBody tr').count()",
    "page.$$eval('#pktBody tr:not([id^=vscroll]) .col-type .badge', f)",
    "document.querySelector('#pktBody > tr:not(.group-child)')",
    "page.waitForSelector('#pktBody tr.group-header')",
    "document.querySelectorAll('#pktBody td.col-path')",
    "document.querySelectorAll('#pktBody td[data-filter-field=\"type\"]')",
    "document.querySelectorAll('#pktBody .path-hops')",
    "document.querySelectorAll('#pktTable tbody tr').length",
    "document.getElementById('pktBody').rows.length",
    "document.getElementById(\"pktBody\").children.length",
  ]) assert.ok(readsRows(s), 'not seen as a row read: ' + s);
  for (const s of [
    "page.waitForSelector('#pktBody')",
    "page.waitForSelector('#pktTable', { state: 'attached' })",
    "page.waitForSelector('#nodesBody tr[data-hash]')",
    "page.goto(BASE + '/#/packets')",
    "#pktBody. Serial behavior pushes",
  ]) assert.ok(!readsRows(s), 'seen as a row read: ' + s);
});

check('detector: an unpinned suite is flagged and each pin form clears it', () => {
  const fake = { suites: [{ file: 'a.js' }, { file: 'b.js' }, { file: 'c.js' }, { file: 'd.js' }, { file: 'e.js' }, { file: 'f.js' }] };
  const src = {
    'a.js': "await page.waitForFunction(() => document.querySelectorAll('#pktBody tr').length > 0)",
    'b.js': "await pinPacketsTimeWindow(ctx); await page.waitForSelector('#pktBody tr[data-hash]')",
    'c.js': "localStorage.setItem('meshcore-time-window', '60'); '#pktBody tr'",
    'd.js': "page.goto(BASE + '/#/packets?timeWindow=60'); '#pktTable tbody tr'",
    'e.js': "page.goto(BASE + '/#/nodes')",
    'f.js': "await page.locator('#pktBody tr').first().waitFor({ state: 'attached' })",
  };
  const found = suitesNeedingRows(fake, f => src[f]);
  assert.deepStrictEqual(found.map(s => s.file), ['a.js', 'b.js', 'c.js', 'd.js', 'f.js']);
  assert.deepStrictEqual(unpinned(found), ['a.js', 'f.js']);
});

check('detector: a helper user with one unpinned context is flagged', () => {
  const src = "const ctx = await browser.newContext({});\nawait pinPacketsTimeWindow(ctx);\n" +
    "const ctxD = await browser.newContext({});\n'#pktBody tr'";
  assert.deepStrictEqual(unpinnedContexts(src), ['ctxD']);
  assert.deepStrictEqual(unpinned([{ file: 'f.js', src }]), ['f.js']);
  assert.deepStrictEqual(unpinnedContexts(src + '\nawait pinPacketsTimeWindow(ctxD);'), []);
});

check('detector: a pin covers only the context opened just before it', () => {
  const open = "{ const ctx = await browser.newContext({});\n";
  const pin = "  await pinPacketsTimeWindow(ctx);\n";
  assert.deepStrictEqual(unpinnedContexts(open + pin + "}\n" + open + "}\n'#pktBody tr'"), ['ctx']);
  assert.deepStrictEqual(unpinnedContexts(open + "}\n" + open + pin + "}\n'#pktBody tr'"), ['ctx']);
  assert.deepStrictEqual(unpinnedContexts(open + pin + "}\n" + open + pin + "}\n'#pktBody tr'"), []);
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
