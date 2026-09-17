/**
 * Analytics table ids must stay unique across repeated id-assignment passes.
 *
 * renderTab() numbers `.analytics-table` elements by position once the tab
 * has rendered, and the pass runs again (async sections, theme refresh).
 * The Scopes tab first renders one static table, then inserts its async
 * sections ahead of it; the second pass used to hand the first inserted
 * table the static table's positional id, so axe reported
 * `duplicate-id analytics-tbl-scopes-0`.
 *
 * Drives the real assignAnalyticsTableIds from public/analytics.js against a
 * minimal fake DOM that mirrors the observed Scopes insertion order.
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

// A container whose tables are listed in document order; the fake document
// resolves ids across every table currently attached.
function makeDom(ctx) {
  const dom = { tables: [], resizeCalls: [] };
  dom.el = { querySelectorAll: (sel) => (sel === '.analytics-table' ? dom.tables.slice() : []) };
  ctx.document.getElementById = (id) => dom.tables.find(t => t.id === id) || null;
  ctx.makeColumnsResizable = (selector, key) => { dom.resizeCalls.push({ selector, key }); };
  return dom;
}

const table = (name) => ({ name, id: '' });
const ids = (tables) => tables.map(t => t.id);
const dupes = (tables) => {
  const seen = new Map();
  for (const t of tables) seen.set(t.id, (seen.get(t.id) || 0) + 1);
  return [...seen].filter(([, n]) => n > 1).map(([id]) => id);
};

console.log('\n=== analytics.js: assignAnalyticsTableIds ===');
const ctx = makeCtx();
const assign = ctx._analyticsAssignTableIds;

test('assignAnalyticsTableIds is exposed for tests', () => {
  assert.strictEqual(typeof assign, 'function', 'window._analyticsAssignTableIds must be a function');
});

if (typeof assign === 'function') {
  test('single pass keeps the positional numbering', () => {
    const dom = makeDom(ctx);
    dom.tables = [table('a'), table('b'), table('c')];
    assign(dom.el, 'rf');
    assert.deepStrictEqual(ids(dom.tables), ['analytics-tbl-rf-0', 'analytics-tbl-rf-1', 'analytics-tbl-rf-2']);
    assert.deepStrictEqual(dom.resizeCalls.map(c => c.key),
      ['meshcore-analytics-rf-0-col-widths', 'meshcore-analytics-rf-1-col-widths', 'meshcore-analytics-rf-2-col-widths']);
  });

  test('Scopes: async tables inserted ahead of a numbered table get unique ids', () => {
    const dom = makeDom(ctx);
    const staticTbl = table('overview-static');
    dom.tables = [staticTbl];
    assign(dom.el, 'scopes');
    assert.strictEqual(staticTbl.id, 'analytics-tbl-scopes-0');

    // Observed second-pass document order on the Scopes tab.
    dom.tables = [table('scope-adoption'), staticTbl, table('no-scope'), table('never-relay')];
    assign(dom.el, 'scopes');
    assert.deepStrictEqual(dupes(dom.tables), [], `duplicate ids: ${ids(dom.tables).join(', ')}`);
    assert.strictEqual(staticTbl.id, 'analytics-tbl-scopes-0', 'an already-assigned id must not change');
    // Same sequence the browser shows on the Scopes tab after the fix.
    assert.deepStrictEqual(ids(dom.tables),
      ['analytics-tbl-scopes-1', 'analytics-tbl-scopes-0', 'analytics-tbl-scopes-2', 'analytics-tbl-scopes-3']);
    for (const t of dom.tables) {
      assert.match(t.id, /^analytics-tbl-scopes-\d+$/);
      assert.ok(dom.resizeCalls.some(c => c.selector === '#' + t.id), `makeColumnsResizable not called for #${t.id}`);
    }
  });

  test('re-running with no new tables leaves ids unchanged', () => {
    const dom = makeDom(ctx);
    dom.tables = [table('a'), table('b')];
    assign(dom.el, 'nodes');
    const first = ids(dom.tables);
    assign(dom.el, 'nodes');
    assert.deepStrictEqual(ids(dom.tables), first);
  });
}

console.log(`\n${passed} passed, ${failed} failed`);
if (failed) process.exit(1);
