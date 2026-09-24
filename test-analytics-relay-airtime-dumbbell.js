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
    { payload_type: 'ADVERT (flood)', type: 4, route_class: 'flood', count: 30, count_pct: 30, score: 6e9, airtime_pct: 60 },
    { payload_type: 'ACK', type: 3, route_class: null, count: 50, count_pct: 50, score: 2e9, airtime_pct: 20 },
    { payload_type: 'ADVERT (zero-hop)', type: 4, route_class: 'zero_hop', count: 15, count_pct: 15, score: 1.5e9, airtime_pct: 15 },
    { payload_type: 'ADVERT', type: 4, route_class: 'legacy', count: 5, count_pct: 5, score: 5e8, airtime_pct: 5 },
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
const airtimeColourOf = (row) => (row.match(/dumbbell-dot-airtime" style="[^"]*background:([^;"]+)/) || [])[1];
const attrOf = (row, name) => { const m = row.match(new RegExp('^[^>]*\\s' + name + '="([^"]*)"')); return m ? m[1] : null; };
const identityOf = (row) => attrOf(row, 'data-payload-type') + '/' + attrOf(row, 'data-route-class');
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

  test('ADVERT rows sharing numeric type get distinct airtime colours', () => {
    const rows = rowsOf(render(splitResponse));
    const colours = rows.filter(r => /ADVERT/.test(labelOf(r))).map(airtimeColourOf);
    assert.strictEqual(colours.length, 3);
    assert.strictEqual(new Set(colours).size, 3, `colours: ${colours.join(', ')}`);
  });

  test('each row exposes its stable (type, route_class) identity', () => {
    const rows = rowsOf(render(splitResponse));
    assert.deepStrictEqual(rows.map(identityOf), ['4/flood', '3/null', '4/zero_hop', '4/legacy']);
    assert.strictEqual(new Set(rows.map(identityOf)).size, rows.length, 'row identities must be unique');
  });

  test('identity comes from route_class, not from the display label', () => {
    const relabelled = Object.assign({}, splitResponse, {
      rows: splitResponse.rows.map(r => Object.assign({}, r, { payload_type: 'Advert #' + r.count })),
    });
    const rows = rowsOf(render(relabelled));
    assert.deepStrictEqual(rows.map(identityOf), ['4/flood', '3/null', '4/zero_hop', '4/legacy']);
  });

  test('responses without route_class (older servers) still render every row', () => {
    const legacy = Object.assign({}, splitResponse, {
      rows: splitResponse.rows.map(r => { const c = Object.assign({}, r); delete c.route_class; return c; }),
    });
    const rows = rowsOf(render(legacy));
    assert.deepStrictEqual(rows.map(labelOf), ['ADVERT (flood)', 'ACK', 'ADVERT (zero-hop)', 'ADVERT']);
    assert.deepStrictEqual(rows.map(r => attrOf(r, 'data-route-class')), [null, null, null, null]);
  });

  // #89: a payload seen on both flood and zero-hop routes is one mixed row.
  const mixedResponse = {
    rows: [
      { payload_type: 'ADVERT (flood)', type: 4, route_class: 'flood', count: 30, count_pct: 60, score: 6e9, airtime_pct: 75 },
      { payload_type: 'ADVERT (mixed)', type: 4, route_class: 'mixed', count: 5, count_pct: 10, score: 2e9, airtime_pct: 25 },
      { payload_type: 'ADVERT (zero-hop)', type: 4, route_class: 'zero_hop', count: 15, count_pct: 30, score: 0, airtime_pct: 0 },
    ],
    total_count: 50,
    total_score: 8e9,
  };

  test('mixed ADVERT row renders with its own stable identity', () => {
    const rows = rowsOf(render(mixedResponse));
    assert.deepStrictEqual(rows.map(labelOf), ['ADVERT (flood)', 'ADVERT (mixed)', 'ADVERT (zero-hop)']);
    assert.deepStrictEqual(rows.map(identityOf), ['4/flood', '4/mixed', '4/zero_hop']);
  });

  test('mixed row uses the theme colour and explains the route mix', () => {
    const rows = rowsOf(render(mixedResponse));
    const mixed = rows[1];
    assert.strictEqual(airtimeColourOf(mixed), 'var(--status-purple)');
    assert.ok(!['flood', 'zero_hop'].some((_, i) => airtimeColourOf(rows[i * 2]) === 'var(--status-purple)'),
      'only the mixed row uses the mixed colour');
    const tip = titleOf(mixed);
    assert.ok(/both flood and zero-hop routes/.test(tip), 'tooltip explains both routes: ' + tip);
    assert.ok(/counted once/.test(tip), 'tooltip says it is counted once: ' + tip);
    assert.ok(!/both flood and zero-hop routes/.test(titleOf(rows[0])), 'flood row has no mixed explanation');
  });

  test('mixed colour does not depend on the row position', () => {
    const onlyMixed = { rows: [mixedResponse.rows[1]], total_count: 5, total_score: 2e9 };
    const rows = rowsOf(render(onlyMixed));
    assert.deepStrictEqual(rows.map(identityOf), ['4/mixed']);
    assert.strictEqual(airtimeColourOf(rows[0]), 'var(--status-purple)');
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
