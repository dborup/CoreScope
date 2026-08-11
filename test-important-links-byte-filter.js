/**
 * test-important-links-byte-filter.js
 *
 * Bug: the Important Links overlay (renderTopRoutes / computeTopRouteEdges,
 * public/map.js) ignored the Byte Size filter (All / 1-byte / 2-byte /
 * 3-byte). The Byte Size click handler only called renderMarkers(), and
 * computeTopRouteEdges was always joined against the full, unfiltered
 * `nodes` array — so Important Links could keep drawing to repeaters the
 * marker layer had just hidden.
 *
 * Fix: a shared pure predicate, nodePassesByteSizeFilter(node, byteSize),
 * used by both renderMarkers (marker layer) and renderTopRoutes (Important
 * Links, via the nodeList it passes into computeTopRouteEdges — the ranking
 * core itself is untouched). The Byte Size click handler now also calls
 * renderTopRoutes() directly (no new API call) when the overlay is on and
 * edges are already cached.
 *
 * All tests execute the REAL public/map.js in a vm sandbox (same pattern as
 * test-top-routes-overlay.js / test-map-scope-filter.js), not a
 * reimplementation.
 */
'use strict';

const vm = require('vm');
const fs = require('fs');
const path = require('path');
const assert = require('assert');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}
async function testAsync(name, fn) {
  try { await fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const src = fs.readFileSync(path.join(__dirname, 'public', 'map.js'), 'utf8');

console.log('\n=== wiring (structural — needs Leaflet/DOM) ===');
test('Byte Size click handler redraws Important Links locally (no loadTopRoutes call)', () => {
  const anchor = src.indexOf('filters.byteSize = btn.dataset.byte;');
  assert.ok(anchor !== -1, 'could not find the #mcByteFilter click handler body');
  const block = src.slice(anchor, anchor + 600);
  assert.ok(/renderTopRoutes\(\)/.test(block), 'handler must call renderTopRoutes()');
  assert.ok(!/loadTopRoutes\(\)/.test(block), 'handler must NOT call loadTopRoutes() — no new API call');
  assert.ok(/topRoutesEdges\s*&&\s*document\.getElementById\('mcTopRoutes'\)\?\.checked/.test(block),
    'redraw must be guarded on the overlay being on AND edges already cached');
});
test('renderMarkers and renderTopRoutes share nodePassesByteSizeFilter (no parallel implementation)', () => {
  assert.ok(/function nodePassesByteSizeFilter\(node, byteSize\)/.test(src));
  const renderMarkersSrc = src.slice(src.indexOf('function _renderMarkersInner'), src.indexOf('function _renderMarkersInner') + 2000);
  assert.ok(/nodePassesByteSizeFilter\(n, filters\.byteSize\)/.test(renderMarkersSrc));
});

// --- sandbox: load the REAL map.js (and roles.js, its usual companion) in a vm ---
function makeLeafletShim() {
  const L = {};
  L.point = (x, y) => ({ x, y });
  L.latLng = (a, b) => ({ lat: a, lng: b });
  L.divIcon = (opts) => ({ _isDivIcon: true, options: opts });
  L.layerGroup = () => {
    const layers = [];
    return {
      _isLayerGroup: true,
      _layers: layers,
      addLayer(l) { layers.push(l); return this; },
      removeLayer() { return this; },
      clearLayers() { layers.length = 0; return this; },
      eachLayer(fn) { layers.forEach(fn); },
      addTo() { return this; },
      hasLayer() { return false; },
    };
  };
  L.polyline = (points, opts) => ({ _isPolyline: true, points, options: opts || {}, bindPopup() { return this; }, addTo() { return this; } });
  L.marker = (latlng, opts) => ({ _isMarker: true, _latlng: latlng, options: opts || {}, getLatLng() { return this._latlng; }, bindPopup() { return this; }, bindTooltip() { return this; } });
  function MarkerClusterGroup(opts) { this.options = opts || {}; }
  MarkerClusterGroup.prototype.addLayer = function () { return this; };
  MarkerClusterGroup.prototype.addLayers = function () { return this; };
  MarkerClusterGroup.prototype.clearLayers = function () { return this; };
  L.MarkerClusterGroup = MarkerClusterGroup;
  L.markerClusterGroup = (opts) => new MarkerClusterGroup(opts);
  return L;
}

function makeFakeElements() {
  const el = (over) => Object.assign({ style: {}, textContent: '', checked: false, value: '' }, over);
  return {
    mcTopRoutes: el({ checked: true }),
    mcTopRoutesRankBy: el({ value: 'usefulness' }),
    mcTopRoutesN: el({ value: '50' }),
    mcTopRoutesHint: el({}),
  };
}

function makeSandbox(apiImpl, elements) {
  const doc = {
    documentElement: { style: {} },
    getElementById(id) { return elements[id] || null; },
    createElement() { return { id: '', textContent: '', innerHTML: '', style: {}, appendChild() {}, addEventListener() {}, setAttribute() {}, classList: { add() {}, remove() {}, toggle() {} } }; },
    querySelector() { return null; },
    querySelectorAll() { return []; },
    addEventListener() {},
    head: { appendChild() {} },
    body: { appendChild() {} },
  };
  let apiCalls = 0;
  const ctx = {
    window: {}, document: doc, console, Math, String, Number, JSON, Promise, Error, Date,
    setTimeout, clearTimeout, parseInt, parseFloat, isFinite, isNaN, Array, Object,
    getComputedStyle: () => ({ getPropertyValue: () => '' }),
    escapeHtml: (s) => (s == null ? '' : String(s)),
    api: (...a) => { apiCalls++; return apiImpl(...a); },
    CLIENT_TTL: { analyticsRF: 300000 },
    registerPage: () => {}, onWS: () => {}, offWS: () => {},
    localStorage: (() => { const s = {}; return { getItem: k => (k in s ? s[k] : null), setItem: (k, v) => { s[k] = String(v); }, removeItem: k => { delete s[k]; } }; })(),
    fetch: () => Promise.resolve({ json: () => Promise.resolve({}) }),
    addEventListener() {}, dispatchEvent() {},
    navigator: { userAgent: '' },
    L: makeLeafletShim(),
  };
  ctx.window.L = ctx.L;
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'public', 'roles.js'), 'utf8'), ctx);
  for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k];
  vm.runInContext(src, ctx);
  for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k];
  ctx.getApiCallCount = () => apiCalls;
  return ctx;
}

const FAKE_MAP = { removeLayer() {}, hasLayer() { return false; }, remove() {} };

console.log('\n=== nodePassesByteSizeFilter (pure predicate, behavioral) ===');
{
  const elements = makeFakeElements();
  const ctx = makeSandbox(() => Promise.resolve({ edges: [] }), elements);
  const internals = ctx.window.__meshcoreMapInternals;

  test('exposes nodePassesByteSizeFilter test hook', () => {
    assert.ok(typeof internals.nodePassesByteSizeFilter === 'function');
  });
  test('All: every node passes regardless of role/hash_size', () => {
    assert.strictEqual(internals.nodePassesByteSizeFilter({ role: 'repeater', hash_size: 3 }, 'all'), true);
    assert.strictEqual(internals.nodePassesByteSizeFilter({ role: 'companion' }, 'all'), true);
  });
  test('filter applies only to the repeater role — other roles always pass', () => {
    assert.strictEqual(internals.nodePassesByteSizeFilter({ role: 'companion', hash_size: 3 }, '1'), true);
    assert.strictEqual(internals.nodePassesByteSizeFilter({ role: 'sensor', hash_size: 2 }, '1'), true);
    assert.strictEqual(internals.nodePassesByteSizeFilter({ role: 'observer', hash_size: 3 }, '1'), true);
  });
  test('missing hash_size falls back to 1 (repeater role)', () => {
    assert.strictEqual(internals.nodePassesByteSizeFilter({ role: 'repeater' }, '1'), true);
    assert.strictEqual(internals.nodePassesByteSizeFilter({ role: 'repeater' }, '2'), false);
  });
  test('1/2/3-byte: repeater must match hash_size exactly', () => {
    assert.strictEqual(internals.nodePassesByteSizeFilter({ role: 'repeater', hash_size: 1 }, '1'), true);
    assert.strictEqual(internals.nodePassesByteSizeFilter({ role: 'repeater', hash_size: 2 }, '1'), false);
    assert.strictEqual(internals.nodePassesByteSizeFilter({ role: 'repeater', hash_size: 2 }, '2'), true);
    assert.strictEqual(internals.nodePassesByteSizeFilter({ role: 'repeater', hash_size: 3 }, '2'), false);
    assert.strictEqual(internals.nodePassesByteSizeFilter({ role: 'repeater', hash_size: 3 }, '3'), true);
  });
}

(async () => {
  console.log('\n=== renderTopRoutes respects the Byte Size filter (end-to-end, real module) ===');

  const NODES = [
    { public_key: 'AA', lat: 50.0, lon: 7.0, role: 'repeater', hash_size: 1, usefulness_score: 0.9, name: 'Alpha' },
    { public_key: 'BB', lat: 50.1, lon: 7.1, role: 'repeater', hash_size: 2, usefulness_score: 0.8, name: 'Bravo' },
    { public_key: 'CC', lat: 50.2, lon: 7.2, role: 'repeater', hash_size: 3, usefulness_score: 0.7, name: 'Charlie' },
    { public_key: 'DD', lat: 50.3, lon: 7.3, role: 'repeater', usefulness_score: 0.6, name: 'Delta' }, // no hash_size -> fallback 1
    { public_key: 'EE', lat: 50.4, lon: 7.4, role: 'companion', hash_size: 3, usefulness_score: 0.5, name: 'Echo' },
  ];
  // AA(1b)-BB(2b), BB(2b)-CC(3b), AA(1b)-DD(1b), CC(3b)-EE(companion, unaffected by byte filter)
  const EDGES = [
    { source: 'AA', target: 'BB', score: 0.9 },
    { source: 'BB', target: 'CC', score: 0.8 },
    { source: 'AA', target: 'DD', score: 0.7 },
    { source: 'CC', target: 'EE', score: 0.6 },
  ];
  const key = t => t.a + '-' + t.b;

  await testAsync('1) All: baseline shows every geo-located, positive-importance edge', async () => {
    const elements = makeFakeElements();
    const ctx = makeSandbox(() => Promise.resolve({ edges: EDGES }), elements);
    const internals = ctx.window.__meshcoreMapInternals;
    internals.setMapForTesting(FAKE_MAP);
    internals.setNodesForTesting(NODES);
    internals.setFiltersForTesting({ byteSize: 'all' });
    await internals.loadTopRoutes();
    const layer = internals.getTopRoutesLayerForTesting();
    assert.strictEqual(layer._layers.length, 4, 'All must draw all 4 edges');
  });

  await testAsync('2) 1-byte: drops any link with a repeater endpoint whose hash_size is 2 or 3', async () => {
    const elements = makeFakeElements();
    const ctx = makeSandbox(() => Promise.resolve({ edges: EDGES }), elements);
    const internals = ctx.window.__meshcoreMapInternals;
    internals.setMapForTesting(FAKE_MAP);
    internals.setNodesForTesting(NODES);
    internals.setFiltersForTesting({ byteSize: '1' });
    await internals.loadTopRoutes();
    const layer = internals.getTopRoutesLayerForTesting();
    // AA-BB dropped (BB=2b), BB-CC dropped (BB=2b,CC=3b), AA-DD kept (both 1b/fallback),
    // CC-EE dropped (CC=3b repeater endpoint; EE companion is irrelevant, CC fails).
    assert.strictEqual(layer._layers.length, 1, '1-byte must keep only AA-DD');
  });

  await testAsync('3) 2-byte: only links whose repeater endpoints are eligible under hash_size=2 semantics', async () => {
    const elements = makeFakeElements();
    const ctx = makeSandbox(() => Promise.resolve({ edges: EDGES }), elements);
    const internals = ctx.window.__meshcoreMapInternals;
    internals.setMapForTesting(FAKE_MAP);
    internals.setNodesForTesting(NODES);
    internals.setFiltersForTesting({ byteSize: '2' });
    await internals.loadTopRoutes();
    const layer = internals.getTopRoutesLayerForTesting();
    // AA-BB dropped (AA=1b), BB-CC dropped (CC=3b), AA-DD dropped (both 1b), CC-EE dropped (CC=3b).
    assert.strictEqual(layer._layers.length, 0, '2-byte must keep no edges from this fixture');
  });

  await testAsync('4) 3-byte: only links whose repeater endpoints are eligible under hash_size=3 semantics', async () => {
    const elements = makeFakeElements();
    const ctx = makeSandbox(() => Promise.resolve({ edges: EDGES }), elements);
    const internals = ctx.window.__meshcoreMapInternals;
    internals.setMapForTesting(FAKE_MAP);
    internals.setNodesForTesting(NODES);
    internals.setFiltersForTesting({ byteSize: '3' });
    await internals.loadTopRoutes();
    const layer = internals.getTopRoutesLayerForTesting();
    // Only CC-EE has a 3-byte repeater endpoint (CC) with the other endpoint (EE)
    // not a repeater, so unaffected by the filter -> kept. All others dropped.
    assert.strictEqual(layer._layers.length, 1, '3-byte must keep only CC-EE');
  });

  await testAsync('5) missing hash_size is treated as 1 in the overlay too', async () => {
    const elements = makeFakeElements();
    const ctx = makeSandbox(() => Promise.resolve({ edges: [{ source: 'AA', target: 'DD', score: 0.9 }] }), elements);
    const internals = ctx.window.__meshcoreMapInternals;
    internals.setMapForTesting(FAKE_MAP);
    internals.setNodesForTesting(NODES); // DD has no hash_size
    internals.setFiltersForTesting({ byteSize: '1' });
    await internals.loadTopRoutes();
    assert.strictEqual(internals.getTopRoutesLayerForTesting()._layers.length, 1, 'DD (no hash_size) must be treated as 1-byte and pass the 1-byte filter');
  });

  await testAsync('6) a link is dropped if only ONE endpoint passes the filter', async () => {
    const elements = makeFakeElements();
    const ctx = makeSandbox(() => Promise.resolve({ edges: [{ source: 'AA', target: 'BB', score: 0.9 }] }), elements);
    const internals = ctx.window.__meshcoreMapInternals;
    internals.setMapForTesting(FAKE_MAP);
    internals.setNodesForTesting(NODES); // AA=1b, BB=2b
    internals.setFiltersForTesting({ byteSize: '1' });
    await internals.loadTopRoutes();
    assert.strictEqual(internals.getTopRoutesLayerForTesting()._layers.length, 0, 'AA passes 1-byte but BB does not -> edge dropped');
    internals.setFiltersForTesting({ byteSize: '2' });
    internals.renderTopRoutes();
    assert.strictEqual(internals.getTopRoutesLayerForTesting()._layers.length, 0, 'BB passes 2-byte but AA does not -> edge dropped');
  });

  await testAsync('7) switching the byte filter redraws the overlay WITHOUT a new neighbor-graph API call', async () => {
    const elements = makeFakeElements();
    const ctx = makeSandbox(() => Promise.resolve({ edges: EDGES }), elements);
    const internals = ctx.window.__meshcoreMapInternals;
    internals.setMapForTesting(FAKE_MAP);
    internals.setNodesForTesting(NODES);
    internals.setFiltersForTesting({ byteSize: 'all' });
    await internals.loadTopRoutes();
    assert.strictEqual(ctx.getApiCallCount(), 1, 'exactly one API call from the initial load');

    // Simulate exactly what the Byte Size click handler now does: mutate the
    // filter, then call renderTopRoutes() directly — no loadTopRoutes().
    internals.setFiltersForTesting({ byteSize: '1' });
    internals.renderTopRoutes();
    assert.strictEqual(ctx.getApiCallCount(), 1, 'no new API call after switching the byte filter');
    assert.strictEqual(internals.getTopRoutesLayerForTesting()._layers.length, 1, 'overlay redrew using the cached edges');
  });

  await testAsync('8) overlay OFF: switching the byte filter creates no layer and makes no API call', async () => {
    const elements = makeFakeElements();
    const ctx = makeSandbox(() => Promise.resolve({ edges: EDGES }), elements);
    const internals = ctx.window.__meshcoreMapInternals;
    internals.setMapForTesting(FAKE_MAP);
    internals.setNodesForTesting(NODES);
    elements.mcTopRoutes.checked = false; // overlay never turned on
    internals.setFiltersForTesting({ byteSize: 'all' });

    // Mirror the click handler's exact guard: only render if checked AND edges cached.
    internals.setFiltersForTesting({ byteSize: '1' });
    if (internals.getTopRoutesEdgesForTesting() && elements.mcTopRoutes.checked) internals.renderTopRoutes();

    assert.strictEqual(ctx.getApiCallCount(), 0, 'no API call when the overlay was never toggled on');
    assert.strictEqual(internals.getTopRoutesLayerForTesting(), null, 'no layer created while the overlay is off');
  });

  await testAsync('9) switching back to All restores the full set from the cached edge list', async () => {
    const elements = makeFakeElements();
    const ctx = makeSandbox(() => Promise.resolve({ edges: EDGES }), elements);
    const internals = ctx.window.__meshcoreMapInternals;
    internals.setMapForTesting(FAKE_MAP);
    internals.setNodesForTesting(NODES);
    internals.setFiltersForTesting({ byteSize: 'all' });
    await internals.loadTopRoutes();

    internals.setFiltersForTesting({ byteSize: '3' });
    internals.renderTopRoutes();
    assert.strictEqual(internals.getTopRoutesLayerForTesting()._layers.length, 1);

    internals.setFiltersForTesting({ byteSize: 'all' });
    internals.renderTopRoutes();
    assert.strictEqual(ctx.getApiCallCount(), 1, 'still just the one original API call');
    assert.strictEqual(internals.getTopRoutesLayerForTesting()._layers.length, 4, 'All restores every edge from the cached list');
  });

  await testAsync('10) ranking (importance) and Top-N are unchanged for the filtered population', async () => {
    const elements = makeFakeElements();
    elements.mcTopRoutesN.value = '1';
    const rankedEdges = [
      { source: 'AA', target: 'BB', score: 0.5 },
      { source: 'AA', target: 'DD', score: 0.95 }, // both 1-byte, higher affinity
    ];
    const ctx = makeSandbox(() => Promise.resolve({ edges: rankedEdges }), elements);
    const internals = ctx.window.__meshcoreMapInternals;
    internals.setMapForTesting(FAKE_MAP);
    internals.setNodesForTesting(NODES);
    internals.setFiltersForTesting({ byteSize: '1' });
    await internals.loadTopRoutes();
    const layer = internals.getTopRoutesLayerForTesting();
    assert.strictEqual(layer._layers.length, 1, 'top-N=1 still caps the filtered population to 1');

    // Verify the ranking core itself (computeTopRouteEdges) is untouched: its
    // importance formula against the byte-filtered nodeList still matches
    // edgeStrength * mean(endpoint usefulness_score).
    const eligible = NODES.filter(n => internals.nodePassesByteSizeFilter(n, '1'));
    const top = internals.computeTopRouteEdges(rankedEdges, eligible, 'usefulness', 50);
    assert.strictEqual(key(top[0]), 'aa-dd');
    assert.ok(Math.abs(top[0].importance - (0.95 * (0.9 + 0.6) / 2)) < 1e-9);
  });

  console.log('\n=== existing async toggle/destroy race tests remain green against the changed code ===');

  await testAsync('toggle off before resolve still yields no layer (unaffected by byte-filter sync)', async () => {
    const elements = makeFakeElements();
    let resolve;
    const p = new Promise(r => { resolve = r; });
    const ctx = makeSandbox(() => p, elements);
    const internals = ctx.window.__meshcoreMapInternals;
    internals.setMapForTesting(FAKE_MAP);
    internals.setNodesForTesting(NODES);
    elements.mcTopRoutes.checked = true;
    const load = internals.loadTopRoutes();
    elements.mcTopRoutes.checked = false;
    internals.invalidateTopRoutesRequests();
    internals.clearTopRoutes();
    resolve({ edges: EDGES });
    await load;
    assert.strictEqual(internals.getTopRoutesLayerForTesting(), null);
  });

  await testAsync('destroy() while pending still renders nothing and throws nothing', async () => {
    const elements = makeFakeElements();
    let resolve;
    const p = new Promise(r => { resolve = r; });
    const ctx = makeSandbox(() => p, elements);
    const internals = ctx.window.__meshcoreMapInternals;
    internals.setMapForTesting(FAKE_MAP);
    internals.setNodesForTesting(NODES);
    elements.mcTopRoutes.checked = true;
    const load = internals.loadTopRoutes();
    internals.destroyForTesting();
    resolve({ edges: EDGES });
    await assert.doesNotReject(load);
    assert.strictEqual(internals.getTopRoutesLayerForTesting(), null);
  });

  console.log('\n────────────────────────────────────────');
  console.log('  ' + passed + ' passed, ' + failed + ' failed');
  if (failed) process.exit(1);
})();
