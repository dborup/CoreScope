/**
 * Relay Airtime Share dumbbell: one rendered row per API row.
 *
 * /api/analytics/relay-airtime-share splits ADVERT into "ADVERT (flood)",
 * "ADVERT (zero-hop)" and the legacy "ADVERT" bucket, and all three rows
 * carry the same numeric `type` (4). The renderer must therefore treat each
 * row independently: keying rows by `type` would collapse the split.
 *
 * Drives the real renderRelayAirtimeDumbbell from public/analytics.js.
 */
'use strict';

const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

let passed = 0, failed = 0;
function test(name, fn) {
  try {
    fn();
    passed++;
    console.log(`  ✅ ${name}`);
  } catch (e) {
    failed++;
    console.log(`  ❌ ${name}: ${e.message}`);
  }
}

function makeCtx() {
  const ctx = {
    window: { addEventListener: () => {}, dispatchEvent: () => {} },
    document: {
      readyState: 'complete',
      createElement: () => ({ id: '', textContent: '', innerHTML: '' }),
      head: { appendChild: () => {} },
      getElementById: () => null,
      addEventListener: () => {},
      querySelectorAll: () => [],
      querySelector: () => null,
    },
    console, Date, Infinity, Math, Array, Object, String, Number, JSON, RegExp,
    Error, TypeError, parseInt, parseFloat, isNaN, isFinite,
    encodeURIComponent, decodeURIComponent,
    setTimeout: () => {}, clearTimeout: () => {}, setInterval: () => 0, clearInterval: () => {},
    fetch: () => Promise.resolve({ json: () => Promise.resolve({}) }),
    performance: { now: () => Date.now() },
    localStorage: (() => { const s = {}; return { getItem: k => s[k] || null, setItem: (k, v) => { s[k] = String(v); }, removeItem: k => { delete s[k]; } }; })(),
    location: { hash: '' },
    CustomEvent: class CustomEvent {},
    Map, Set, Promise, URLSearchParams,
    addEventListener: () => {},
    dispatchEvent: () => {},
    requestAnimationFrame: () => 0,
    getComputedStyle: () => ({ getPropertyValue: () => '' }),
    registerPage: () => {},
    RegionFilter: { init: () => {}, onChange: () => {}, regionQueryString: () => '' },
    onWS: () => {}, offWS: () => {}, connectWS: () => {},
    invalidateApiCache: () => {}, initTabBar: () => {},
    IATA_COORDS_GEO: {},
  };
  vm.createContext(ctx);
  const load = (file) => {
    vm.runInContext(fs.readFileSync(file, 'utf8'), ctx);
    for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k];
  };
  load('public/payload-labels.js');
  load('public/roles.js');
  load('public/app.js');
  try { load('public/analytics.js'); } catch (_) {
    for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k];
  }
  return ctx;
}

// Shape returned by the API for a window containing all three ADVERT buckets.
const splitResponse = {
  rows: [
    { payload_type: 'ADVERT (flood)', type: 4, count: 30, count_pct: 30, score: 6e9, airtime_pct: 60 },
    { payload_type: 'ACK', type: 3, count: 50, count_pct: 50, score: 2e9, airtime_pct: 20 },
    { payload_type: 'ADVERT (zero-hop)', type: 4, count: 15, count_pct: 15, score: 1.5e9, airtime_pct: 15 },
    { payload_type: 'ADVERT', type: 4, count: 5, count_pct: 5, score: 5e8, airtime_pct: 5 },
  ],
  total_count: 100,
  total_score: 1e10,
  preset: { freq_hz: 869.6e6, bw_khz: 62.5, sf: 8, cr: 5, preamble: 16 },
};

// Split rendered HTML into per-row chunks (each starts at a dumbbell-row div).
function rowsOf(html) {
  return html.split('<div class="dumbbell-row"').slice(1);
}
const labelOf = (row) => (row.match(/class="dumbbell-label"[^>]*>([^<]*)</) || [])[1];
const titleOf = (row) => (row.match(/^ title="([^"]*)"/) || [])[1] || '';
const airtimeLeftOf = (row) => (row.match(/dumbbell-dot-airtime" style="[^"]*left:([0-9.]+)%/) || [])[1];

console.log('\n=== analytics.js: renderRelayAirtimeDumbbell ===');
const ctx = makeCtx();
const render = ctx._analyticsRenderRelayAirtimeDumbbell;

test('renderRelayAirtimeDumbbell is exposed for tests', () => {
  assert.strictEqual(typeof render, 'function', 'window._analyticsRenderRelayAirtimeDumbbell must be a function');
});

if (typeof render === 'function') {
  test('ADVERT rows sharing numeric type 4 each render as their own row, in API order', () => {
    const rows = rowsOf(render(splitResponse));
    assert.deepStrictEqual(rows.map(labelOf), ['ADVERT (flood)', 'ACK', 'ADVERT (zero-hop)', 'ADVERT']);
  });

  test('each row carries its own tooltip, count and airtime position', () => {
    const rows = rowsOf(render(splitResponse));
    splitResponse.rows.forEach((r, i) => {
      const tip = titleOf(rows[i]);
      assert.ok(tip.startsWith(r.payload_type + '\n'), `row ${i} tooltip starts with ${JSON.stringify(tip.slice(0, 30))}`);
      assert.ok(tip.includes('Count: ' + r.count.toLocaleString() + ' (' + r.count_pct.toFixed(2) + '%)'), `row ${i} tooltip count`);
      assert.ok(tip.includes('Airtime: ' + r.airtime_pct.toFixed(2) + '%'), `row ${i} tooltip airtime`);
      assert.strictEqual(airtimeLeftOf(rows[i]), r.airtime_pct.toFixed(3), `row ${i} airtime dot position`);
    });
  });

  test('rendered chart introduces no element ids', () => {
    const html = render(splitResponse);
    assert.ok(!/\sid="/.test(html), 'dumbbell markup must not contain id attributes');
  });

  test('a single ADVERT class renders one row', () => {
    const only = { rows: [{ payload_type: 'ADVERT (zero-hop)', type: 4, count: 3, count_pct: 100, score: 1e9, airtime_pct: 100 }], total_count: 3, total_score: 1e9 };
    assert.deepStrictEqual(rowsOf(render(only)).map(labelOf), ['ADVERT (zero-hop)']);
  });

  test('empty rows render the empty-state message', () => {
    const html = render({ rows: [], total_count: 0, total_score: 0 });
    assert.ok(html.includes('No relay-airtime data in this window.'));
    assert.strictEqual(rowsOf(html).length, 0);
  });
}

console.log(`\n${passed} passed, ${failed} failed`);
if (failed > 0) process.exit(1);
