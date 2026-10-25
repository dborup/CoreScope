/**
 * Unit + integration tests for #560 (Relates to #560): Analytics -> Nodes'
 * Role Breakdown must be computed from the already-loaded, FILTERED node
 * rows (region + area), not from /api/nodes' global `counts` metadata
 * (cmd/server/db.go GetAllRoleCounts, unconditional over the whole `nodes`
 * table). That global count still backs other consumers unchanged.
 *
 * Section A: the pure counting function nodeRoleBreakdownCounts
 *   (window._analyticsNodeRoleBreakdownCounts) -- known roles, an unknown
 *   role, and an empty row set.
 * Section B: renderNodesTab (window._analyticsRenderNodesTab) wired end to
 *   end against stub fetchAllNodes/api -- synthetic global counts that
 *   disagree with the filtered rows, exactly one network round trip per
 *   render, and a late (superseded) response that must not clobber a
 *   newer render's output.
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
async function testAsync(name, fn) {
  try {
    await fn();
    passed++;
    console.log(`  ✅ ${name}`);
  } catch (e) {
    failed++;
    console.log(`  ❌ ${name}: ${e.message}`);
  }
}

// Same sandbox shape as test-analytics-nodes-without-scope.js.
function makeSandbox() {
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
    setTimeout, clearTimeout, setInterval: () => {}, clearInterval: () => {},
    fetch: () => Promise.resolve({ json: () => Promise.resolve({}) }),
    performance: { now: () => Date.now() },
    localStorage: (() => { const s = {}; return { getItem: k => s[k] || null, setItem: (k, v) => { s[k] = String(v); }, removeItem: k => { delete s[k]; } }; })(),
    location: { hash: '' },
    CustomEvent: class CustomEvent {},
    Map, Promise, URLSearchParams,
    addEventListener: () => {},
    dispatchEvent: () => {},
    requestAnimationFrame: (cb) => setTimeout(cb, 0),
  };
  vm.createContext(ctx);
  return ctx;
}

function loadInCtx(ctx, file) {
  if (!ctx.__payloadLabelsLoaded && file !== 'public/payload-labels.js') {
    ctx.__payloadLabelsLoaded = true;
    vm.runInContext(fs.readFileSync('public/payload-labels.js', 'utf8'), ctx);
  }
  vm.runInContext(fs.readFileSync(file, 'utf8'), ctx);
  for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k];
}

// --- fetchAllNodes/api stubs recorded per test so each case can assert on
// exactly how many times the network was touched and with what the render
// saw as its synthetic "server" response.
function makeAnalyticsSandbox() {
  const ctx = makeSandbox();
  ctx.getComputedStyle = () => ({ getPropertyValue: () => '' });
  ctx.registerPage = () => {};
  ctx.RegionFilter = { init: () => {}, onChange: () => {}, regionQueryString: () => '' };
  ctx.AreaFilter = { init: () => {}, onChange: () => {}, areaQueryString: () => '' };
  ctx.onWS = () => {};
  ctx.offWS = () => {};
  ctx.connectWS = () => {};
  ctx.invalidateApiCache = () => {};
  ctx.makeColumnsResizable = () => {};
  ctx.initTabBar = () => {};
  ctx.IATA_COORDS_GEO = {};
  loadInCtx(ctx, 'public/roles.js');
  loadInCtx(ctx, 'public/app.js');
  loadInCtx(ctx, 'public/heard-by.js');
  try { loadInCtx(ctx, 'public/analytics.js'); } catch (e) {
    for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k];
  }
  return ctx;
}

const ctx = makeAnalyticsSandbox();
const nodeRoleBreakdownCounts = ctx.window._analyticsNodeRoleBreakdownCounts;
const renderNodesTab = ctx.window._analyticsRenderNodesTab;

console.log('\n=== analytics.js: nodeRoleBreakdownCounts (unit) ===');

test('is exported for testing', () => {
  assert.strictEqual(typeof nodeRoleBreakdownCounts, 'function');
});

test('counts each known role, pluralized -- same shape as GetAllRoleCounts', () => {
  const nodes = [
    { public_key: 'p1', role: 'repeater' },
    { public_key: 'p2', role: 'repeater' },
    { public_key: 'p3', role: 'room' },
    { public_key: 'p4', role: 'companion' },
    { public_key: 'p5', role: 'sensor' },
  ];
  assert.strictEqual(JSON.stringify(nodeRoleBreakdownCounts(nodes)), JSON.stringify({ repeaters: 2, rooms: 1, companions: 1, sensors: 1 }));
});

test('an unknown/unrecognized role is not tallied into any bucket and does not throw', () => {
  const nodes = [
    { public_key: 'p1', role: 'repeater' },
    { public_key: 'p2', role: 'observer' },    // a real role, but not one GetAllRoleCounts tracks today
    { public_key: 'p3', role: 'gateway' },      // never a known role at all
    { public_key: 'p4', role: null },
    { public_key: 'p5' },                        // role field missing entirely
  ];
  const result = nodeRoleBreakdownCounts(nodes);
  assert.strictEqual(JSON.stringify(result), JSON.stringify({ repeaters: 1, rooms: 0, companions: 0, sensors: 0 }));
  const sum = Object.keys(result).reduce((a, k) => a + result[k], 0);
  assert.ok(sum < nodes.length, 'unknown-role rows must not inflate any known bucket');
});

test('an empty row set renders zero for every known role, not an absent/undefined count', () => {
  assert.strictEqual(JSON.stringify(nodeRoleBreakdownCounts([])), JSON.stringify({ repeaters: 0, rooms: 0, companions: 0, sensors: 0 }));
});

test('a non-array input is treated as empty rather than throwing', () => {
  const zero = JSON.stringify({ repeaters: 0, rooms: 0, companions: 0, sensors: 0 });
  assert.strictEqual(JSON.stringify(nodeRoleBreakdownCounts(undefined)), zero);
  assert.strictEqual(JSON.stringify(nodeRoleBreakdownCounts(null)), zero);
});

// =====================================================================
// Section B -- renderNodesTab wired end to end.
// =====================================================================

function makeEl() {
  return { _h: '', set innerHTML(v) { this._h = v; }, get innerHTML() { return this._h; } };
}

function extractBadge(html, key) {
  const m = new RegExp(key + ':\\s*(\\d+)').exec(html);
  return m ? Number(m[1]) : null;
}

console.log('\n=== analytics.js: renderNodesTab (integration) ===');

(async () => {
  await testAsync('renders the Role Breakdown from the filtered rows, not from a disagreeing global `counts`', async () => {
    let fetchCalls = 0, apiCalls = 0;
    const filteredNodes = [
      { public_key: 'p1', role: 'repeater', last_seen: new Date().toISOString() },
      { public_key: 'p2', role: 'repeater', last_seen: new Date().toISOString() },
      { public_key: 'p3', role: 'room', last_seen: new Date().toISOString() },
    ];
    ctx.fetchAllNodes = async () => {
      fetchCalls++;
      // Deliberately disagreeing global counts (as if the region filter
      // returned 3 rows while the unfiltered mesh has far more of each role).
      return { nodes: filteredNodes, counts: { repeaters: 999, rooms: 999, companions: 999, sensors: 999 }, total: filteredNodes.length };
    };
    ctx.api = async () => { apiCalls++; return []; };

    const el = makeEl();
    await renderNodesTab(el);

    assert.strictEqual(fetchCalls, 1, 'exactly one fetchAllNodes call -- no extra request added by the fix');
    assert.strictEqual(apiCalls, 1, 'exactly one bulk-health call -- no extra request added by the fix');
    assert.strictEqual(extractBadge(el._h, 'repeaters'), 2, 'repeaters badge must come from the 2 filtered rows, not the global 999');
    assert.strictEqual(extractBadge(el._h, 'rooms'), 1, 'rooms badge must come from the 1 filtered row, not the global 999');
    assert.strictEqual(extractBadge(el._h, 'companions'), 0);
    assert.strictEqual(extractBadge(el._h, 'sensors'), 0);
    assert.ok(!/999/.test(el._h), 'the disagreeing global count must never reach the page');

    // Network Status total and the role-breakdown sum describe the same
    // (filtered) population, since every row above has a known role.
    const totalMatch = /font-weight:700">(\d+)<\/div>\s*<div[^>]*>Total Nodes/.exec(el._h);
    assert.ok(totalMatch, 'Total Nodes stat must render');
    const total = Number(totalMatch[1]);
    const sum = ['repeaters', 'rooms', 'companions', 'sensors'].reduce((a, k) => a + extractBadge(el._h, k), 0);
    assert.strictEqual(sum, total, 'role-breakdown sum must equal the Network Status total for an all-known-role population');
  });

  await testAsync('an empty filtered result renders zero for every role, not a fallback to any global count', async () => {
    ctx.fetchAllNodes = async () => ({ nodes: [], counts: { repeaters: 42, rooms: 7, companions: 3, sensors: 1 }, total: 0 });
    ctx.api = async () => [];

    const el = makeEl();
    await renderNodesTab(el);

    assert.strictEqual(extractBadge(el._h, 'repeaters'), 0);
    assert.strictEqual(extractBadge(el._h, 'rooms'), 0);
    assert.strictEqual(extractBadge(el._h, 'companions'), 0);
    assert.strictEqual(extractBadge(el._h, 'sensors'), 0);
    assert.ok(!/42|>7<|>3<|>1</.test(el._h.replace(/Total Nodes/g, '')), 'none of the stale global counts must leak into an empty render');
  });

  await testAsync('a late response from a superseded render cannot overwrite the current breakdown', async () => {
    // First call's fetch is held open; second call (a simulated filter
    // change while the first is still pending) resolves immediately. The
    // first call's response must be discarded once it finally arrives.
    let resolveFirst;
    let call = 0;
    ctx.fetchAllNodes = async () => {
      call++;
      if (call === 1) {
        return new Promise((resolve) => { resolveFirst = resolve; });
      }
      return { nodes: [{ public_key: 'new1', role: 'companion', last_seen: new Date().toISOString() }], counts: {}, total: 1 };
    };
    ctx.api = async () => [];

    const el = makeEl();
    const firstRender = renderNodesTab(el); // not awaited -- still pending
    await renderNodesTab(el); // second, newer render resolves first

    assert.strictEqual(extractBadge(el._h, 'companions'), 1, 'the newer render\'s single companion must be showing');
    assert.strictEqual(extractBadge(el._h, 'repeaters'), 0);

    // Now let the stale first request resolve with different data.
    resolveFirst({ nodes: [{ public_key: 'old1', role: 'repeater' }, { public_key: 'old2', role: 'repeater' }], counts: {}, total: 2 });
    await firstRender;

    assert.strictEqual(extractBadge(el._h, 'companions'), 1, 'the stale response must not have overwritten the newer render');
    assert.strictEqual(extractBadge(el._h, 'repeaters'), 0, 'the stale response\'s repeater rows must never have reached the page');
  });

  // =====================================================================
  // Section C -- CS-Minimax PR#580 round-2 review, finding 2: fetchAllNodes
  // silently slices to its safetyCap (public/app.js). A truncated node list
  // must not be presented to the user as a complete population.
  // =====================================================================
  console.log('\n=== analytics.js: renderNodesTab surfaces a truncated node list ===');

  await testAsync('shows a truncation notice when fetchAllNodes reports truncated: true', async () => {
    ctx.fetchAllNodes = async () => ({
      nodes: [{ public_key: 'p1', role: 'repeater', last_seen: new Date().toISOString() }],
      counts: {}, total: 1, truncated: true,
    });
    ctx.api = async () => [];
    const el = makeEl();
    await renderNodesTab(el);
    assert.ok(/truncat/i.test(el._h), 'a truncated node list must render a visible notice, got: ' + el._h.slice(0, 200));
  });

  await testAsync('shows no truncation notice when fetchAllNodes reports truncated: false', async () => {
    ctx.fetchAllNodes = async () => ({
      nodes: [{ public_key: 'p1', role: 'repeater', last_seen: new Date().toISOString() }],
      counts: {}, total: 1, truncated: false,
    });
    ctx.api = async () => [];
    const el = makeEl();
    await renderNodesTab(el);
    assert.ok(!/truncat/i.test(el._h), 'a complete node list must not render a truncation notice, got: ' + el._h.slice(0, 200));
  });

  // =====================================================================
  // Section D -- CS-Minimax PR#580 round-2 review, finding 1: an area-only
  // filter and a combined region+area filter must both reach fetchAllNodes
  // -- the unit sandbox previously stubbed AreaFilter.areaQueryString() to
  // '' and never varied it, so a regression dropping AreaFilter's
  // contribution to the request (mutant M7) passed the whole suite.
  // =====================================================================
  console.log('\n=== analytics.js: renderNodesTab passes area/region+area query to fetchAllNodes ===');

  await testAsync('an area-only filter is included in the fetchAllNodes query', async () => {
    let recordedQuery = null;
    ctx.RegionFilter.regionQueryString = () => '';
    ctx.AreaFilter.areaQueryString = () => '&area=SFBAY';
    ctx.fetchAllNodes = async (q) => { recordedQuery = q; return { nodes: [], counts: {}, total: 0 }; };
    ctx.api = async () => [];
    const el = makeEl();
    await renderNodesTab(el);
    assert.ok(recordedQuery && recordedQuery.indexOf('&area=SFBAY') !== -1,
      'an area-only filter must reach fetchAllNodes\' query, got: ' + recordedQuery);
    ctx.AreaFilter.areaQueryString = () => '';
  });

  await testAsync('a combined region+area filter includes BOTH pieces in the fetchAllNodes query', async () => {
    let recordedQuery = null;
    ctx.RegionFilter.regionQueryString = () => '&region=MRY';
    ctx.AreaFilter.areaQueryString = () => '&area=SFBAY';
    ctx.fetchAllNodes = async (q) => { recordedQuery = q; return { nodes: [], counts: {}, total: 0 }; };
    ctx.api = async () => [];
    const el = makeEl();
    await renderNodesTab(el);
    assert.ok(recordedQuery && recordedQuery.indexOf('&region=MRY') !== -1 && recordedQuery.indexOf('&area=SFBAY') !== -1,
      'a combined region+area filter must pass BOTH pieces to fetchAllNodes\' query, got: ' + recordedQuery);
    ctx.RegionFilter.regionQueryString = () => '';
    ctx.AreaFilter.areaQueryString = () => '';
  });

  console.log('\n════════════════════════════════════════');
  console.log(`  Role Breakdown (#560): ${passed} passed, ${failed} failed`);
  console.log('════════════════════════════════════════');
  process.exit(failed === 0 ? 0 : 1);
})();
