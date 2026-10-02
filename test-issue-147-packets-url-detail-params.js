/* test-issue-147-packets-url-detail-params.js — #147
 *
 * updatePacketsUrl() rebuilds #/packets/<hash>?… from buildPacketsQuery(),
 * which only knows filter params. The packet-detail params it does not own,
 * ?obs= (selected observation) and ?viewPath=1 (View Path modal), must
 * survive: on cold load (init() calls it to show the Clear button), on every
 * filter change and on Clear Filters.
 *
 * Decision under test: ?obs= travels with the detail subpath; ?viewPath=1
 * describes the open View Path modal, so it is kept only while that modal
 * (#packetPathModal) is open. (Closing the modal drops it from the address
 * bar: see test-packet-path-map.js.)
 *
 * Runs the REAL buildPacketsQuery / updateClearFiltersVisibility /
 * updatePacketsUrl and the real Clear Filters handler body from
 * public/packets.js (plus the real public/url-state.js for sort) inside a
 * vm.createContext sandbox, so the closure state they share (filters,
 * savedTimeWindowMin, sort) is a sandbox global the tests can set.
 */
'use strict';

const vm = require('vm');
const fs = require('fs');
const path = require('path');
const assert = require('assert');

console.log('--- test-issue-147-packets-url-detail-params.js ---');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log(`  ✅ ${name}`); }
  catch (e) { failed++; console.log(`  ❌ ${name}: ${e.message}`); }
}

const PACKETS_SRC = fs.readFileSync(path.join(__dirname, 'public/packets.js'), 'utf8');
const URLSTATE_SRC = fs.readFileSync(path.join(__dirname, 'public/url-state.js'), 'utf8');

// Source of the block that opens at the first '{' after `marker`, through its
// matching '}'. With keepHead the text from marker to that '{' is included.
function extractBlock(src, marker, keepHead) {
  const idx = src.indexOf(marker);
  assert(idx !== -1, marker + ' not found in public/packets.js');
  const open = src.indexOf('{', idx + (keepHead ? 0 : marker.length));
  let depth = 0;
  for (let i = open; i < src.length; i++) {
    if (src[i] === '{') depth++;
    else if (src[i] === '}' && --depth === 0) return keepHead ? src.substring(idx, i + 1) : src.substring(open + 1, i);
  }
  throw new Error('unbalanced braces after ' + marker);
}

const URL_FUNCS = [
  'function buildPacketsQuery(',
  'function updateClearFiltersVisibility()',
  'function updatePacketsUrl(',
].map((m) => extractBlock(PACKETS_SRC, m, true)).join('\n');
const CLEAR_BODY = extractBlock(PACKETS_SRC, "if (clearBtn) clearBtn.addEventListener('click', function()", false);

// opts.modalOpen: a #packetPathModal overlay is in the DOM.
function makeSandbox(startHash, opts) {
  opts = opts || {};
  const urls = [];
  const els = {};
  for (const id of ['clearFiltersBtn', 'fHash', 'fNode', 'fChannel', 'fTimeWindow', 'fMyNodes',
    'packetFilterInput', 'packetFilterError', 'packetFilterCount']) {
    els[id] = { id, value: '', style: { display: 'none' }, classList: { add() {}, remove() {} } };
  }
  if (opts.modalOpen) els.packetPathModal = { id: 'packetPathModal' };
  const store = {};
  const ctx = {
    console, URLSearchParams, encodeURIComponent, decodeURIComponent,
    String, Number, Array, Object, RegExp, JSON, Math, Set, Map, Error,
    location: { hash: startHash },
    history: { replaceState(_s, _t, url) { urls.push(url); ctx.location.hash = url; } },
    document: { getElementById: (id) => els[id] || null },
    localStorage: {
      getItem: (k) => (k in store ? store[k] : null),
      setItem: (k, v) => { store[k] = String(v); },
      removeItem: (k) => { delete store[k]; },
    },
    __region: [],
    window: {},
  };
  ctx.RegionFilter = {
    getRegionParam: () => ctx.__region.join(','),
    setSelected: (a) => { ctx.__region = a.slice(); },
  };
  ctx.globalThis = ctx;
  vm.createContext(ctx);
  vm.runInContext(URLSTATE_SRC, ctx, { filename: 'public/url-state.js' });
  ctx.URLState = ctx.window.URLState;
  vm.runInContext(
    'var filters = {}; var savedTimeWindowMin = 15; var DEFAULT_TIME_WINDOW = 15;\n' +
    "var _packetSortColumn = null; var _packetSortDirection = 'desc';\n" +
    'var _observerFilterSet = null; var selectedObservers = new Set(); var selectedTypes = new Set();\n' +
    'function buildObserverMenu() {} function updateObsTrigger() {} function buildTypeMenu() {}\n' +
    'function updateTypeTrigger() {} function loadPackets() {}\n' +
    URL_FUNCS + '\nfunction __clearFilters() {' + CLEAR_BODY + '}',
    ctx, { filename: 'packets.js (#147 extract)' });
  return {
    ctx, urls, els,
    set(code) { vm.runInContext(code, ctx); },
    update(detail) {
      ctx.__detail = detail;
      vm.runInContext(detail === undefined ? 'updatePacketsUrl()' : 'updatePacketsUrl(__detail)', ctx);
      return ctx.location.hash;
    },
    clear() { vm.runInContext('__clearFilters()', ctx); return ctx.location.hash; },
    query(tw, region, skipHash) { return ctx.buildPacketsQuery(tw, region, skipHash); },
  };
}

function params(hash) { return new URLSearchParams(String(hash).split('?')[1] || ''); }
function countParam(hash, name) { return params(hash).getAll(name).length; }

const DETAIL = '#/packets/abc123';

// ---- Cold load: init() calls updatePacketsUrl() once (packets.js ~1984) ----

test('cold load #/packets/<hash>?obs=123 keeps ?obs=123', () => {
  const s = makeSandbox(DETAIL + '?obs=123');
  s.set("filters.hash = 'abc123'"); // init() sets filters.hash from the route
  assert.strictEqual(s.update(), DETAIL + '?obs=123');
});

test('cold load #/packets/<hash>?obs=123&viewPath=1 (modal open) keeps both', () => {
  const s = makeSandbox(DETAIL + '?obs=123&viewPath=1', { modalOpen: true });
  s.set("filters.hash = 'abc123'");
  assert.strictEqual(s.update(), DETAIL + '?obs=123&viewPath=1');
});

test('cold load with URL filters keeps filters and ?obs=', () => {
  const s = makeSandbox(DETAIL + '?timeWindow=60&observer=OBS1&obs=123');
  s.set("filters.hash = 'abc123'; filters.observer = 'OBS1'; savedTimeWindowMin = 60");
  assert.strictEqual(s.update(), DETAIL + '?timeWindow=60&observer=OBS1&obs=123');
});

// ---- viewPath decision: kept only while its modal is open ----

test('viewPath: kept on a filter change while the View Path modal is open', () => {
  const s = makeSandbox(DETAIL + '?obs=123&viewPath=1', { modalOpen: true });
  s.set("filters.hash = 'abc123'; filters.observer = 'OBS1'");
  const h = s.update();
  assert.strictEqual(params(h).get('observer'), 'OBS1');
  assert.strictEqual(params(h).get('obs'), '123');
  assert.strictEqual(params(h).get('viewPath'), '1', 'viewPath dropped although the modal is open: ' + h);
});

test('viewPath: dropped once the modal is closed (URL describes what is shown), obs kept', () => {
  const s = makeSandbox(DETAIL + '?obs=123&viewPath=1', { modalOpen: false });
  s.set("filters.hash = 'abc123'; filters.observer = 'OBS1'");
  assert.strictEqual(s.update(), DETAIL + '?observer=OBS1&obs=123');
});

test('viewPath: never invented when the URL did not have it', () => {
  const s = makeSandbox(DETAIL + '?obs=123', { modalOpen: true });
  s.set("filters.hash = 'abc123'");
  assert.strictEqual(countParam(s.update(), 'viewPath'), 0);
});

// ---- Every filter type keeps ?obs= (and updates its own param) ----

const FILTER_CASES = [
  ['observer', "filters.observer = 'OBS1'", 'observer', 'OBS1'],
  ['region', "__region = ['EU']", 'region', 'EU'],
  ['hash (text filter, not the subpath)', "filters.hash = 'ffee01'", 'hash', 'ffee01'],
  ['node', "filters.node = 'nodepubkey0123456789'", 'node', 'nodepubkey0123456789'],
  ['channel', "filters.channel = 'public'", 'channel', 'public'],
  ['filter expression', "filters._filterExpr = 'type == ADVERT && snr > 5'", 'filter', 'type == ADVERT && snr > 5'],
  ['time window', 'savedTimeWindowMin = 60', 'timeWindow', '60'],
  ['sort', "_packetSortColumn = 'snr'; _packetSortDirection = 'asc'", 'sort', 'snr:asc'],
];
for (const [label, code, name, value] of FILTER_CASES) {
  test(`filter change keeps ?obs=: ${label}`, () => {
    const s = makeSandbox(DETAIL + '?obs=123');
    s.set(code);
    const h = s.update();
    assert(h.startsWith(DETAIL + '?'), 'detail subpath lost: ' + h);
    assert.strictEqual(params(h).get(name), value, `${name} not written: ${h}`);
    assert.strictEqual(params(h).get('obs'), '123', '?obs= dropped: ' + h);
    assert.strictEqual(countParam(h, 'obs'), 1, '?obs= duplicated: ' + h);
  });
}

test('filter change keeps ?obs=: area (not a URL param; URL otherwise unchanged)', () => {
  // AreaFilter.onChange -> updatePacketsUrl(); the area itself is not in the hash.
  const s = makeSandbox(DETAIL + '?obs=123');
  assert.strictEqual(s.update(), DETAIL + '?obs=123');
});

test('removing a filter keeps ?obs=', () => {
  const s = makeSandbox(DETAIL + '?observer=OBS1&obs=123');
  s.set("filters.observer = 'OBS1'");
  s.update();
  s.set('filters.observer = undefined');
  assert.strictEqual(s.update(), DETAIL + '?obs=123');
});

test('repeated updates are idempotent (obs stays once, last)', () => {
  const s = makeSandbox(DETAIL + '?obs=123');
  s.set("filters.observer = 'OBS1'; savedTimeWindowMin = 60");
  const a = s.update(), b = s.update(), c = s.update();
  assert.strictEqual(a, DETAIL + '?timeWindow=60&observer=OBS1&obs=123');
  assert.strictEqual(b, a);
  assert.strictEqual(c, a);
});

test('list view (#/packets, no detail) gets no ?obs=', () => {
  const s = makeSandbox('#/packets?obs=123');
  s.set("filters.observer = 'OBS1'");
  assert.strictEqual(s.update(), '#/packets?observer=OBS1');
});

// ---- Clear Filters (#121/#132): filter params go, detail subpath + obs stay ----

test('Clear Filters on a detail URL removes filter params, keeps subpath and ?obs=', () => {
  const s = makeSandbox(DETAIL + '?timeWindow=60&region=EU&observer=OBS1&obs=123');
  s.set("filters.hash = 'abc123'; filters.observer = 'OBS1'; savedTimeWindowMin = 60; __region = ['EU']");
  assert.strictEqual(s.clear(), DETAIL + '?obs=123');
  assert.strictEqual(s.els.clearFiltersBtn.style.display, 'none', 'Clear button still visible after clearing');
});

test('Clear Filters on a detail URL drops viewPath=1 when the modal is closed', () => {
  const s = makeSandbox(DETAIL + '?observer=OBS1&obs=123&viewPath=1', { modalOpen: false });
  s.set("filters.observer = 'OBS1'");
  assert.strictEqual(s.clear(), DETAIL + '?obs=123');
});

test('Clear Filters on the list view still yields bare #/packets', () => {
  const s = makeSandbox('#/packets?timeWindow=60&observer=OBS1');
  s.set("filters.observer = 'OBS1'; savedTimeWindowMin = 60");
  assert.strictEqual(s.clear(), '#/packets');
});

// ---- buildPacketsQuery() stays filter-only, output unchanged ----

test('buildPacketsQuery() output is unchanged and never contains detail params', () => {
  // Golden values = master 727efca0 output. The hash carries ?obs=&viewPath=
  // and the modal is open, so a buildPacketsQuery() that reads them would differ.
  const s = makeSandbox(DETAIL + '?obs=123&viewPath=1', { modalOpen: true });
  const q = (setup, tw, region, skipHash) => {
    s.set('filters = {}; _packetSortColumn = null; _packetSortDirection = "desc";' + setup);
    return s.query(tw, region, skipHash);
  };
  assert.strictEqual(q('', 15, ''), '');
  assert.strictEqual(q('', 60, ''), '?timeWindow=60');
  assert.strictEqual(q('', 15, 'EU,US-W'), '?region=EU%2CUS-W');
  assert.strictEqual(q("filters.hash = 'abc123'", 15, ''), '?hash=abc123');
  assert.strictEqual(q("filters.hash = 'abc123'", 15, '', true), '');
  assert.strictEqual(
    q("filters.hash = 'h'; filters.node = 'n'; filters.observer = 'o'; filters.channel = 'c'; filters._filterExpr = 'a b'; _packetSortColumn = 'snr'", 30, 'EU'),
    '?timeWindow=30&region=EU&hash=h&node=n&observer=o&channel=c&filter=a%20b&sort=snr');
  assert.strictEqual(q("_packetSortColumn = 'time'", 15, ''), '');
  assert.strictEqual(q("_packetSortColumn = 'time'; _packetSortDirection = 'asc'", 15, ''), '?sort=time%3Aasc');
});

// ---- Detail writers: selecting a packet / observation, closing the detail ----
// selectPacket() (also the cold-load auto-select), the observation-row click
// and the detail pane's onClose wrote #/packets/... themselves, dropping the
// filter params (and viewPath on cold load). They pass the new detail state
// to updatePacketsUrl({ subpath, obs }) instead.

test('selecting a packet keeps the filter params and writes its ?obs=', () => {
  const s = makeSandbox('#/packets?timeWindow=60&observer=OBS1');
  s.set("filters.observer = 'OBS1'; savedTimeWindowMin = 60");
  assert.strictEqual(s.update({ subpath: '/abc123', obs: '7' }), DETAIL + '?timeWindow=60&observer=OBS1&obs=7');
});

test('cold-load auto-select of the same packet keeps ?viewPath=1 while the modal is open', () => {
  const s = makeSandbox(DETAIL + '?obs=123&viewPath=1', { modalOpen: true });
  s.set("filters.hash = 'abc123'");
  assert.strictEqual(s.update({ subpath: '/abc123', obs: '123' }), DETAIL + '?obs=123&viewPath=1');
});

test('selecting another packet carries neither the old ?obs= nor ?viewPath=1', () => {
  const s = makeSandbox(DETAIL + '?obs=123&viewPath=1', { modalOpen: true });
  assert.strictEqual(s.update({ subpath: '/ffee01', obs: null }), '#/packets/ffee01');
});

test('clicking another observation row switches ?obs= and keeps the filters', () => {
  const s = makeSandbox(DETAIL + '?timeWindow=60&obs=1');
  s.set('savedTimeWindowMin = 60');
  assert.strictEqual(s.update({ subpath: '/abc123', obs: '2' }), DETAIL + '?timeWindow=60&obs=2');
});

test('closing the detail drops subpath and detail params, keeps the filters', () => {
  const s = makeSandbox(DETAIL + '?timeWindow=60&observer=OBS1&obs=1');
  s.set("filters.observer = 'OBS1'; savedTimeWindowMin = 60");
  assert.strictEqual(s.update({ subpath: '', obs: null }), '#/packets?timeWindow=60&observer=OBS1');
});

// ---- One place: updatePacketsUrl() is the only #/packets hash writer ----

test('updatePacketsUrl() is the only history.replaceState() writer in packets.js', () => {
  const writes = PACKETS_SRC.match(/history\.replaceState\(/g) || [];
  assert.strictEqual(writes.length, 1, 'expected 1 replaceState call (in updatePacketsUrl), got ' + writes.length);
  const fn = extractBlock(PACKETS_SRC, 'function updatePacketsUrl(', true);
  assert(/history\.replaceState\(/.test(fn), 'the writer is not inside updatePacketsUrl()');
});

test('buildPacketsQuery() is called only from updatePacketsUrl() (every caller covered)', () => {
  const calls = PACKETS_SRC.match(/buildPacketsQuery\(/g) || [];
  assert.strictEqual(calls.length, 2, 'expected the definition + one call, got ' + calls.length);
  const fn = extractBlock(PACKETS_SRC, 'function updatePacketsUrl(', true);
  assert(/buildPacketsQuery\(/.test(fn), 'updatePacketsUrl() no longer builds the query');
  assert(/updatePacketsUrl\(\);\s*\n\s*\n\s*\/\/ Filter event listeners/.test(PACKETS_SRC),
    'init() cold-load updatePacketsUrl() call moved; re-check #147 cold-load coverage');
});

console.log(`\n${passed} passed, ${failed} failed`);
if (failed > 0) process.exit(1);
console.log('All tests passed ✅');
