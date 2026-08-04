/**
 * "Important Links" map overlay (issue #672 / D, ported from upstream
 * Kpa-clawbot/CoreScope PR #1771) — a public, B-weighted top-routes layer in
 * public/map.js. It joins the loaded nodes (coords + the #672 usefulness/
 * bridge/redundancy/traffic scores from /api/nodes) with the public
 * neighbor-graph edges, ranks edges by a chosen axis, and draws the top-N
 * weighted polylines so geographic chokepoints stand out.
 *
 * Three layers of coverage, all executing the REAL public/map.js in a vm
 * sandbox (same pattern as test-map-scope-filter.js / test-map-clustering.js)
 * rather than a hand-copied reimplementation:
 *
 *  - structural pins (file-grep) for the DOM wiring that needs Leaflet/DOM
 *    (toggle, rank-by select, slider, load/clear/render handlers);
 *  - BEHAVIORAL tests for the pure ranking core computeTopRouteEdges against
 *    fixtures, asserting on importance, ordering, top-N and skips;
 *  - RACE-CONDITION regression tests for loadTopRoutes's generation-guard
 *    fix (review round after the initial #1771 port), using controlled/
 *    deferred Promises to drive exact interleavings: toggle-off before
 *    resolve, two overlapping requests resolving out of order, an old
 *    request failing after a newer one already succeeded, and destroy()
 *    firing while a request is in flight.
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

console.log('\n=== overlay wiring (structural — needs Leaflet/DOM) ===');
test('controls template has the #mcTopRoutes toggle', () => {
  assert.ok(/<input type="checkbox" id="mcTopRoutes">/.test(src));
});
['usefulness', 'bridge', 'redundancy', 'traffic', 'affinity'].forEach(ax => {
  test('rank-by select offers "' + ax + '"', () => {
    assert.ok(new RegExp('value="' + ax + '"').test(src));
  });
});
test('top-N slider present', () => {
  assert.ok(/id="mcTopRoutesN"/.test(src) && /type="range"/.test(src));
});
test('loadTopRoutes fetches the public neighbor-graph endpoint', () => {
  assert.ok(/function loadTopRoutes[\s\S]{0,600}\/analytics\/neighbor-graph/.test(src));
});
test('toggle wires loadTopRoutes on check', () => {
  assert.ok(/if \(e\.target\.checked\) \{\s*loadTopRoutes\(\);\s*\} else \{/.test(src));
});
test('toggle-off invalidates in-flight requests before clearing (race fix)', () => {
  assert.ok(/invalidateTopRoutesRequests\(\);\s*clearTopRoutes\(\);/.test(src));
});
test('renderTopRoutes builds a dedicated layer group', () => {
  assert.ok(/renderTopRoutes\(\)/.test(src) && /topRoutesLayer = L\.layerGroup\(\)/.test(src));
});
test('a generation counter guards loadTopRoutes against stale responses', () => {
  assert.ok(/topRoutesGeneration\+\+/.test(src));
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

// Minimal fake DOM: only the specific elements the Top Routes code queries by
// id. Each is a plain mutable object (checked/value/style/textContent) —
// enough for loadTopRoutes/renderTopRoutes/clearTopRoutes without needing a
// full DOM tree (init()'s markup-building is never invoked by these tests).
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
    // roles.js's top-level syncBadgeColors() (and other one-off init code
    // that isn't part of what these tests exercise) needs a minimally
    // functional createElement/head/body — same shim shape as
    // test-map-scope-filter.js's sandbox.
    createElement() { return { id: '', textContent: '', innerHTML: '', style: {}, appendChild() {}, addEventListener() {}, setAttribute() {}, classList: { add() {}, remove() {}, toggle() {} } }; },
    querySelector() { return null; },
    querySelectorAll() { return []; },
    addEventListener() {},
    head: { appendChild() {} },
    body: { appendChild() {} },
  };
  const ctx = {
    window: {}, document: doc, console, Math, String, Number, JSON, Promise, Error, Date,
    setTimeout, clearTimeout, parseInt, parseFloat, isFinite, isNaN, Array, Object,
    getComputedStyle: () => ({ getPropertyValue: () => '' }),
    escapeHtml: (s) => (s == null ? '' : String(s)),
    api: apiImpl,
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
  return ctx;
}

// A deferred promise the test resolves/rejects on its own schedule, so
// interleavings (which request resolves first) are deterministic rather than
// timing-dependent.
function deferred() {
  let resolve, reject;
  const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

const FAKE_MAP = { removeLayer() {}, hasLayer() { return false; }, remove() {} };

console.log('\n=== ranking core (behavioral, executes the real computeTopRouteEdges) ===');
{
  const elements = makeFakeElements();
  const ctx = makeSandbox(() => Promise.resolve({ edges: [] }), elements);
  const internals = ctx.window.__meshcoreMapInternals;

  test('exposes computeTopRouteEdges test hook', () => {
    assert.ok(internals, 'window.__meshcoreMapInternals not exposed by map.js');
    assert.ok(typeof internals.computeTopRouteEdges === 'function', 'computeTopRouteEdges not exported');
  });

  const nodes = [
    { public_key: 'AA', lat: 50.0, lon: 7.0, usefulness_score: 0.9, bridge_score: 0.1 },
    { public_key: 'BB', lat: 50.1, lon: 7.1, usefulness_score: 0.8, bridge_score: 0.9 },
    { public_key: 'CC', lat: 50.2, lon: 7.2, usefulness_score: 0.1, bridge_score: 0.1 },
    { public_key: 'DD', lat: null, lon: null, usefulness_score: 0.9 },                  // no GPS
    { public_key: 'FF', lat: 50.4, lon: 7.4, usefulness_score: 0 },                     // zero score
    { public_key: 'GG', lat: 50.5, lon: 7.5, usefulness_score: 0 },                     // zero score
  ];
  const edges = [
    { source: 'AA', target: 'BB', score: 0.5 },
    { source: 'AA', target: 'CC', score: 0.8 },
    { source: 'BB', target: 'CC', score: 0.3 },
    { source: 'AA', target: 'DD', score: 0.9 }, // DD has no GPS → skipped
    { source: 'FF', target: 'GG', score: 0.6 }, // both zero usefulness → skipped on usefulness axis
  ];

  const u = internals.computeTopRouteEdges(edges, nodes, 'usefulness', 50);
  const key = t => t.a + '-' + t.b;
  test('usefulness: top link is AA↔BB, importance = edge.score × mean(endpoint scores)', () => {
    assert.strictEqual(key(u[0]), 'aa-bb');
    assert.ok(Math.abs(u[0].importance - 0.425) < 1e-9, 'importance=' + u[0].importance);
  });
  test('usefulness: GPS-less (AA-DD) and zero-score (FF-GG) edges dropped → 3 remain', () => {
    assert.strictEqual(u.length, 3);
  });
  test('edge with a GPS-less endpoint is skipped', () => {
    assert.ok(!u.some(t => t.a === 'aa' && t.b === 'dd'));
  });
  test('zero-importance edge skipped on a score axis', () => {
    assert.ok(!u.some(t => t.a === 'ff' || t.b === 'gg'));
  });
  test('edges sorted by importance desc', () => {
    assert.ok(u.every((t, i) => i === 0 || u[i - 1].importance >= t.importance));
  });

  const b = internals.computeTopRouteEdges(edges, nodes, 'bridge', 50);
  test('switching axis (usefulness→bridge) reorders the 2nd-ranked link', () => {
    assert.strictEqual(key(b[1]), 'bb-cc');
    assert.strictEqual(key(u[1]), 'aa-cc');
  });

  const aff = internals.computeTopRouteEdges(edges, nodes, 'affinity', 50);
  test('affinity axis: importance is the raw edge affinity (top = AA↔CC at 0.8)', () => {
    assert.strictEqual(key(aff[0]), 'aa-cc');
    assert.ok(Math.abs(aff[0].importance - 0.8) < 1e-9);
  });
  test('zero-score nodes still link on the affinity axis (no endpoint weighting)', () => {
    assert.ok(aff.some(t => t.a === 'ff' && t.b === 'gg'));
  });
  const capped = internals.computeTopRouteEdges(edges, nodes, 'usefulness', 2);
  test('top-N caps the result to N highest-importance links', () => {
    assert.strictEqual(capped.length, 2);
    assert.strictEqual(key(capped[0]), 'aa-bb');
  });
}

(async () => {
  console.log('\n=== renderTopRoutes: end-to-end draw against the real module (Leaflet-shimmed) ===');

  await testAsync('loadTopRoutes → renderTopRoutes populates the layer group and hides the empty-state hint', async () => {
    const elements = makeFakeElements();
    const ctx = makeSandbox(() => Promise.resolve({
      edges: [{ source: 'AA', target: 'BB', score: 0.9, avg_snr: 4.2 }],
    }), elements);
    const internals = ctx.window.__meshcoreMapInternals;
    internals.setMapForTesting(FAKE_MAP);
    internals.setNodesForTesting([
      { public_key: 'AA', lat: 50.0, lon: 7.0, usefulness_score: 0.9, name: 'Alpha' },
      { public_key: 'BB', lat: 50.1, lon: 7.1, usefulness_score: 0.8, name: 'Bravo' },
    ]);
    elements.mcTopRoutes.checked = true;

    await internals.loadTopRoutes();

    const layer = internals.getTopRoutesLayerForTesting();
    assert.ok(layer, 'renderTopRoutes should have built a layer group');
    assert.strictEqual(layer._layers.length, 1, 'one edge with both endpoints geo-located should draw one polyline');
    assert.strictEqual(elements.mcTopRoutesHint.style.display, 'none', 'the empty-state hint must be hidden when links are drawn');
  });

  await testAsync('loadTopRoutes with no drawable edges shows the empty-state hint instead of a populated layer', async () => {
    const elements = makeFakeElements();
    // Edge references a node with no GPS -> computeTopRouteEdges drops it -> nothing to draw.
    const ctx = makeSandbox(() => Promise.resolve({
      edges: [{ source: 'AA', target: 'BB', score: 0.9 }],
    }), elements);
    const internals = ctx.window.__meshcoreMapInternals;
    internals.setMapForTesting(FAKE_MAP);
    internals.setNodesForTesting([
      { public_key: 'AA', lat: null, lon: null, usefulness_score: 0.9 },
      { public_key: 'BB', lat: 50.1, lon: 7.1, usefulness_score: 0.8 },
    ]);
    elements.mcTopRoutes.checked = true;

    await internals.loadTopRoutes();

    const layer = internals.getTopRoutesLayerForTesting();
    assert.ok(layer, 'renderTopRoutes still builds an (empty) layer group');
    assert.strictEqual(layer._layers.length, 0);
    assert.strictEqual(elements.mcTopRoutesHint.style.display, '', 'the empty-state hint must be shown when nothing is drawable');
  });

  console.log('\n=== async race-condition regression (#1771 review fix) ===');

  await testAsync('scenario 1: toggle on → request starts → toggle off before resolve → NO layer is (re)added', async () => {
    const elements = makeFakeElements();
    const d = deferred();
    const ctx = makeSandbox(() => d.promise, elements);
    const internals = ctx.window.__meshcoreMapInternals;
    internals.setMapForTesting(FAKE_MAP);
    internals.setNodesForTesting([{ public_key: 'AA', lat: 1, lon: 1, usefulness_score: 1 }, { public_key: 'BB', lat: 2, lon: 2, usefulness_score: 1 }]);

    elements.mcTopRoutes.checked = true;
    const p = internals.loadTopRoutes(); // gen=1, awaiting d.promise

    // Toggle off before the response arrives — exactly what the real
    // change-handler does on uncheck.
    elements.mcTopRoutes.checked = false;
    internals.invalidateTopRoutesRequests();
    internals.clearTopRoutes();

    d.resolve({ edges: [{ source: 'AA', target: 'BB', score: 0.9 }] });
    await p;

    assert.strictEqual(internals.getTopRoutesLayerForTesting(), null,
      'a layer must not be added once the overlay has been toggled off');
  });

  await testAsync('scenario 2: two requests overlap and the OLDEST resolves LAST — it must not overwrite the newest', async () => {
    const elements = makeFakeElements();
    const deferreds = [deferred(), deferred()];
    let call = 0;
    const ctx = makeSandbox(() => deferreds[call++].promise, elements);
    const internals = ctx.window.__meshcoreMapInternals;
    internals.setMapForTesting(FAKE_MAP);
    internals.setNodesForTesting([
      { public_key: 'AA', lat: 1, lon: 1, usefulness_score: 1 },
      { public_key: 'BB', lat: 2, lon: 2, usefulness_score: 1 },
      { public_key: 'CC', lat: 3, lon: 3, usefulness_score: 1 },
    ]);
    elements.mcTopRoutes.checked = true;

    const p1 = internals.loadTopRoutes(); // gen=1
    const p2 = internals.loadTopRoutes(); // gen=2 (e.g. the node-reload sync hook firing again)

    // Newer request resolves FIRST.
    deferreds[1].resolve({ edges: [{ source: 'BB', target: 'CC', score: 0.9 }] });
    await p2;
    const afterNewer = internals.getTopRoutesEdgesForTesting();
    assert.strictEqual(afterNewer.length, 1);
    assert.strictEqual(afterNewer[0].source, 'BB');

    // Older request resolves LAST — must be a no-op against current state.
    deferreds[0].resolve({ edges: [{ source: 'AA', target: 'BB', score: 0.1 }] });
    await p1;
    const afterOlder = internals.getTopRoutesEdgesForTesting();
    assert.strictEqual(afterOlder, afterNewer, 'the older, later-resolving response must not replace the newer edges');
  });

  await testAsync('scenario 3: an old request fails AFTER a newer one succeeded — checkbox stays checked, layer preserved', async () => {
    const elements = makeFakeElements();
    const deferreds = [deferred(), deferred()];
    let call = 0;
    const ctx = makeSandbox(() => deferreds[call++].promise, elements);
    const internals = ctx.window.__meshcoreMapInternals;
    internals.setMapForTesting(FAKE_MAP);
    internals.setNodesForTesting([
      { public_key: 'AA', lat: 1, lon: 1, usefulness_score: 1 },
      { public_key: 'BB', lat: 2, lon: 2, usefulness_score: 1 },
    ]);
    elements.mcTopRoutes.checked = true;

    const p1 = internals.loadTopRoutes(); // gen=1 — will fail
    const p2 = internals.loadTopRoutes(); // gen=2 — will succeed

    deferreds[1].resolve({ edges: [{ source: 'AA', target: 'BB', score: 0.9 }] });
    await p2;
    const layerAfterSuccess = internals.getTopRoutesLayerForTesting();
    assert.ok(layerAfterSuccess, 'the newer, successful request should have rendered a layer');
    assert.strictEqual(elements.mcTopRoutes.checked, true);

    deferreds[0].reject(new Error('old request: network down'));
    await p1; // must not throw (loadTopRoutes catches internally)

    assert.strictEqual(elements.mcTopRoutes.checked, true,
      'an old failure arriving after a newer success must NOT uncheck the checkbox');
    assert.strictEqual(internals.getTopRoutesLayerForTesting(), layerAfterSuccess,
      'an old failure arriving after a newer success must NOT clear the current layer');
  });

  await testAsync('scenario 4: destroy() fires while a request is pending — the response renders nothing and throws nothing', async () => {
    const elements = makeFakeElements();
    const d = deferred();
    const ctx = makeSandbox(() => d.promise, elements);
    const internals = ctx.window.__meshcoreMapInternals;
    internals.setMapForTesting(FAKE_MAP);
    internals.setNodesForTesting([{ public_key: 'AA', lat: 1, lon: 1, usefulness_score: 1 }, { public_key: 'BB', lat: 2, lon: 2, usefulness_score: 1 }]);
    elements.mcTopRoutes.checked = true;

    const p = internals.loadTopRoutes(); // gen=1, pending
    internals.destroyForTesting(); // sets map=null, bumps generation, nulls layer/edges

    d.resolve({ edges: [{ source: 'AA', target: 'BB', score: 0.9 }] });
    await assert.doesNotReject(p, undefined, 'a response arriving after destroy() must not throw uncaught');

    assert.strictEqual(internals.getTopRoutesLayerForTesting(), null,
      'no layer must be rendered against a destroyed map page');
  });

  console.log('\n────────────────────────────────────────');
  console.log('  ' + passed + ' passed, ' + failed + ' failed');
  if (failed) process.exit(1);
})();
