/* test-hash-stats-sort-226.js
 *
 * #226: the Hash Stats multi-byte adopters table sorted by the wrong column
 * (its column map missed Role, so each header read the cell to its left),
 * compared Last Seen as "5m ago" text, could only sort ascending, had no
 * state or indicator, and lost the order at the next filter click.
 *
 * Contract pinned here, on the REAL analytics.js in a vm:
 * - each header sorts by its own column, in both directions, and the
 *   column's own cells come out in that order (ties keep the server order);
 * - Last Seen follows the timestamps; a row without one is last either way;
 * - clicking the sorted header flips the direction, another header starts
 *   ascending; aria-sort and the arrow mark the sorted column only;
 * - a filter click keeps the sort; the filter itself is unchanged;
 * - the sort is written to the URL as mbsort=/mbdir= (URL only), defaults
 *   left out; an unknown value is the default and never reaches markup.
 */
'use strict';

const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + (e && e.message || e)); }
}

const NOW = Date.now();
const iso = (msAgo) => new Date(NOW - msAgo).toISOString();
const MIN = 60 * 1000, HOUR = 60 * MIN, DAY = 24 * HOUR;

// Every column orders these differently from the input order and from each
// other. Last Seen's text order ("10m ago" < "2h ago" < "3d ago" < "45s ago")
// is not its time order.
const NODES = [
  { name: 'delta', pubkey: 'aa', role: 'repeater', hashSize: 2, packets: 7, lastSeen: iso(2 * HOUR) },
  { name: 'Alpha', pubkey: 'bb', role: 'companion', hashSize: 3, packets: 30, lastSeen: iso(10 * MIN) },
  { name: 'charlie', pubkey: 'cc', role: 'room', hashSize: 2, packets: 4, lastSeen: iso(3 * DAY) },
  { name: 'bravo', pubkey: 'dd', role: 'sensor', hashSize: 3, packets: 100, lastSeen: '' },
  { name: 'echo', pubkey: 'ee', role: '', hashSize: 2, packets: 9, lastSeen: iso(45 * 1000) },
];
const CAPS = [
  { pubkey: 'aa', status: 'confirmed' },
  { pubkey: 'cc', status: 'suspected' },
  { pubkey: 'dd', status: 'confirmed' },
  { pubkey: 'ee', status: 'suspected' },
  // bb has no capability record: unknown
];
const TS = Object.fromEntries(NODES.map((n) => [n.pubkey, n.lastSeen ? Date.parse(n.lastSeen) : null]));

// Expected row order (by pubkey) per column and direction.
const EXPECTED = {
  name: { asc: ['bb', 'dd', 'cc', 'aa', 'ee'], desc: ['ee', 'aa', 'cc', 'dd', 'bb'] },
  role: { asc: ['bb', 'aa', 'cc', 'dd', 'ee'], desc: ['ee', 'dd', 'cc', 'aa', 'bb'] },
  status: { asc: ['aa', 'dd', 'cc', 'ee', 'bb'], desc: ['bb', 'cc', 'ee', 'aa', 'dd'] },
  hashSize: { asc: ['aa', 'cc', 'ee', 'bb', 'dd'], desc: ['bb', 'dd', 'aa', 'cc', 'ee'] },
  packets: { asc: ['cc', 'aa', 'ee', 'bb', 'dd'], desc: ['dd', 'bb', 'ee', 'aa', 'cc'] },
  lastSeen: { asc: ['cc', 'aa', 'bb', 'ee', 'dd'], desc: ['ee', 'bb', 'aa', 'cc', 'dd'] },
};
const INPUT_ORDER = NODES.map((n) => n.pubkey);

function env() {
  const timers = [];
  let handler = null;
  const wrap = { innerHTML: '' };
  const buttons = ['all', 'confirmed', 'suspected', 'unknown'].map((f) => {
    const cls = new Set();
    return { dataset: { mbFilter: f }, classList: { toggle: (c, on) => (on ? cls.add(c) : cls.delete(c)), contains: (c) => cls.has(c) } };
  });
  const section = {
    addEventListener: (type, fn) => { if (type === 'click') handler = fn; },
    querySelector: (sel) => (sel === '#mbAdoptersTableWrap' ? wrap : null),
    querySelectorAll: (sel) => (sel === '[data-mb-filter]' ? buttons : []),
  };
  const ctx = {
    window: { addEventListener() {}, removeEventListener() {}, dispatchEvent() {} },
    document: {
      readyState: 'complete', body: { appendChild() {} }, head: { appendChild() {} },
      createElement: () => ({ style: {}, setAttribute() {}, appendChild() {} }),
      getElementById: (id) => (id === 'mbAdoptersSection' ? section : null),
      addEventListener() {}, querySelectorAll: () => [], querySelector: () => null,
      documentElement: { style: {}, getAttribute: () => null, setAttribute() {} },
    },
    console: { log() {}, warn() {}, error() {} }, Date, Math, Array, Object, String, Number, JSON, RegExp,
    Error, TypeError, parseInt, parseFloat, isNaN, isFinite, encodeURIComponent, decodeURIComponent,
    setTimeout: (fn) => { timers.push(fn); return timers.length; }, clearTimeout() {},
    setInterval: () => 0, clearInterval() {}, requestAnimationFrame: () => 0, cancelAnimationFrame() {},
    performance: { now: () => 0 }, Map, Set, Promise, URLSearchParams, Infinity, NaN,
    localStorage: { getItem: () => null, setItem() {}, removeItem() {} },
    sessionStorage: { getItem: () => null, setItem() {}, removeItem() {} },
    location: { hash: '#/analytics?tab=hashsizes' },
    history: { state: null, replaceState(st, _t, url) { ctx.history.state = st; ctx.location.hash = url; } },
    CustomEvent: class CustomEvent {}, getComputedStyle: () => ({ getPropertyValue: () => '' }),
    registerPage() {}, fetch: async () => ({ ok: true, json: async () => ({}) }),
  };
  vm.createContext(ctx);
  const load = (f) => { vm.runInContext(fs.readFileSync(f, 'utf8'), ctx); for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k]; };
  load('public/roles.js');
  load('public/url-state.js');
  try { load('public/app.js'); } catch (e) { /* DOM-only tail */ }
  ctx.registerPage = () => {};
  load('public/analytics.js');
  const render = ctx._analyticsRenderMultiByteAdopters;
  assert.strictEqual(typeof render, 'function', '_analyticsRenderMultiByteAdopters not exposed');
  return {
    ctx, wrap, buttons,
    timeAgo: ctx.timeAgo,
    // Renders the card and wires its click handler, as the page does.
    mount(filter, sort) {
      const html = render(NODES, CAPS, filter, sort);
      const m = /<div id="mbAdoptersTableWrap">([\s\S]*)<\/div><\/div><\/div>$/.exec(html);
      wrap.innerHTML = m ? m[1] : '';
      while (timers.length) timers.shift()();
      return html;
    },
    clickHeader(col) {
      assert.ok(handler, 'no click handler on #mbAdoptersSection');
      const th = { dataset: { sort: col } };
      handler({ target: { closest: (sel) => (sel === '[data-sort]' || sel === 'th[data-sort]' ? th : null) } });
    },
    clickFilter(f) {
      assert.ok(handler, 'no click handler on #mbAdoptersSection');
      const btn = buttons.find((b) => b.dataset.mbFilter === f);
      handler({ target: { closest: (sel) => (sel === '[data-mb-filter]' ? btn : null) } });
    },
    params: () => Object.fromEntries(new URLSearchParams(ctx.location.hash.split('?')[1] || '')),
  };
}

const strip = (s) => s.replace(/<[^>]*>/g, '').replace(/&amp;/g, '&').replace(/\s+/g, ' ').trim();

// The table as { headers: [{ col, attrs, text }], rows: [{ pubkey, cells: [text] }] }.
function parseTable(html) {
  const headers = [];
  const thRe = /<th(\s[^>]*)?>([\s\S]*?)<\/th>/g;
  let m;
  while ((m = thRe.exec(html))) {
    const col = /data-sort="([^"]*)"/.exec(m[1] || '');
    headers.push({ col: col ? col[1] : null, attrs: m[1] || '', text: strip(m[2]) });
  }
  const rows = [];
  const trRe = /<tr class="clickable-row"[^>]*data-value="#\/nodes\/([^"]*)"[^>]*>([\s\S]*?)<\/tr>/g;
  while ((m = trRe.exec(html))) {
    const cells = [];
    const tdRe = /<td[^>]*>([\s\S]*?)<\/td>/g;
    let c;
    while ((c = tdRe.exec(m[2]))) cells.push(strip(c[1]));
    rows.push({ pubkey: decodeURIComponent(m[1]), cells });
  }
  return { headers, rows };
}

// The cells of one column, read through the column's own header.
function columnCells(t, col) {
  const i = t.headers.findIndex((h) => h.col === col);
  assert.ok(i >= 0, 'no header for ' + col);
  return t.rows.map((r) => r.cells[i]);
}

// What each column's cells show for a row, to compare the column's own cells.
function cellOf(e, pubkey, col) {
  const n = NODES.find((x) => x.pubkey === pubkey);
  const cap = CAPS.find((c) => c.pubkey === pubkey);
  switch (col) {
    case 'name': return n.name;
    case 'role': return n.role || 'unknown';
    case 'status': return { confirmed: 'Confirmed', suspected: 'Suspected' }[cap && cap.status] || 'Unknown';
    case 'hashSize': return n.hashSize + '-byte';
    case 'packets': return String(n.packets);
    case 'lastSeen': return n.lastSeen ? e.timeAgo(n.lastSeen) : '—';
  }
  throw new Error(col);
}

console.log('\n=== #226: each header sorts by its own column, both directions ===');
for (const col of Object.keys(EXPECTED)) {
  for (const dir of ['asc', 'desc']) {
    test(col + ' ' + dir + ': the column\'s own cells come out in order', () => {
      const e = env();
      const t = parseTable(e.mount('all', { col, dir }));
      assert.deepStrictEqual(columnCells(t, col), EXPECTED[col][dir].map((pk) => cellOf(e, pk, col)), 'cells of ' + col);
      assert.deepStrictEqual(t.rows.map((r) => r.pubkey), EXPECTED[col][dir], 'row order');
    });
  }
}

test('Last Seen follows the timestamps, not the "x ago" text; no timestamp is last both ways', () => {
  for (const dir of ['asc', 'desc']) {
    const t = parseTable(env().mount('all', { col: 'lastSeen', dir }));
    const ts = t.rows.map((r) => TS[r.pubkey]);
    assert.strictEqual(ts[ts.length - 1], null, dir + ': the row without Last Seen is not last');
    const known = ts.slice(0, -1);
    for (let i = 1; i < known.length; i++) {
      assert.ok(dir === 'asc' ? known[i - 1] <= known[i] : known[i - 1] >= known[i], dir + ': ' + JSON.stringify(known));
    }
  }
});

test('no sort, "none" and unknown columns keep the server order', () => {
  for (const sort of [undefined, null, { col: 'none', dir: 'asc' }, { col: 'bogus', dir: 'desc' }, { col: '__proto__', dir: 'asc' }]) {
    const t = parseTable(env().mount('all', sort));
    assert.deepStrictEqual(t.rows.map((r) => r.pubkey), INPUT_ORDER, JSON.stringify(sort));
  }
});

test('aria-sort and the arrow mark the sorted column only', () => {
  const t = parseTable(env().mount('all', { col: 'packets', dir: 'desc' }));
  const sorted = t.headers.filter((h) => /aria-sort="(ascending|descending)"/.test(h.attrs));
  assert.deepStrictEqual(sorted.map((h) => h.col), ['packets']);
  assert.ok(/aria-sort="descending"/.test(sorted[0].attrs), sorted[0].attrs);
  assert.ok(/sort-active/.test(sorted[0].attrs), 'no sort-active class');
  assert.ok(sorted[0].text.indexOf('↓') >= 0, 'no ↓ arrow: ' + sorted[0].text);
  for (const h of t.headers.filter((x) => x.col !== 'packets')) {
    assert.ok(!/aria-sort="(ascending|descending)"/.test(h.attrs) && !/sort-active/.test(h.attrs), h.col + ' marked');
  }
});

console.log('\n=== #226: clicks ===');

test('clicking a header sorts ascending, clicking it again descending', () => {
  const e = env();
  e.mount('all');
  e.clickHeader('packets');
  assert.deepStrictEqual(parseTable(e.wrap.innerHTML).rows.map((r) => r.pubkey), EXPECTED.packets.asc, 'first click');
  e.clickHeader('packets');
  assert.deepStrictEqual(parseTable(e.wrap.innerHTML).rows.map((r) => r.pubkey), EXPECTED.packets.desc, 'second click');
  e.clickHeader('role');
  assert.deepStrictEqual(parseTable(e.wrap.innerHTML).rows.map((r) => r.pubkey), EXPECTED.role.asc, 'another column starts ascending');
});

test('every header click sorts its own column (Role included)', () => {
  for (const col of Object.keys(EXPECTED)) {
    const e = env();
    e.mount('all');
    e.clickHeader(col);
    const t = parseTable(e.wrap.innerHTML);
    assert.deepStrictEqual(columnCells(t, col), EXPECTED[col].asc.map((pk) => cellOf(e, pk, col)), col);
  }
});

test('a filter click keeps the sort and still filters', () => {
  const e = env();
  e.mount('all');
  e.clickHeader('packets');
  e.clickHeader('packets');
  e.clickFilter('confirmed');
  const t = parseTable(e.wrap.innerHTML);
  assert.deepStrictEqual(t.rows.map((r) => r.pubkey), ['dd', 'aa'], 'confirmed rows, packets descending');
  assert.ok(t.headers.some((h) => h.col === 'packets' && /aria-sort="descending"/.test(h.attrs)), 'indicator lost');
  assert.strictEqual(e.params().mbf, 'confirmed', 'mbf= not written');
  assert.ok(e.buttons.find((b) => b.dataset.mbFilter === 'confirmed').classList.contains('active'), 'filter button not active');
});

test('a header click keeps the filter', () => {
  const e = env();
  e.mount('suspected');
  e.clickHeader('packets');
  assert.deepStrictEqual(parseTable(e.wrap.innerHTML).rows.map((r) => r.pubkey), ['cc', 'ee']);
});

console.log('\n=== #226: URL ===');

test('clicks write mbsort=/mbdir= next to mbf=; the default direction is left out', () => {
  const e = env();
  e.mount('all');
  e.clickHeader('lastSeen');
  assert.deepStrictEqual(e.params(), { tab: 'hashsizes', mbsort: 'lastSeen' });
  e.clickHeader('lastSeen');
  assert.deepStrictEqual(e.params(), { tab: 'hashsizes', mbsort: 'lastSeen', mbdir: 'desc' });
  e.clickFilter('suspected');
  assert.deepStrictEqual(e.params(), { tab: 'hashsizes', mbf: 'suspected', mbsort: 'lastSeen', mbdir: 'desc' });
});

test('hostile sort values never reach the markup', () => {
  const evil = 'x"><img src=x onerror=alert(1)>';
  const html = env().mount('all', { col: evil, dir: evil });
  assert.ok(html.indexOf('onerror') < 0, 'value echoed into the markup');
});

console.log('\n' + passed + ' passed, ' + failed + ' failed');
if (failed) {
  console.log('test-hash-stats-sort-226.js: FAIL');
  process.exit(1);
}
console.log('test-hash-stats-sort-226.js: PASS');
