/**
 * test-issue-807-hash-size-unknown.js
 *
 * #807: a node whose hash size is unknown (no hash_size from /api/nodes) must
 * never be drawn as a confirmed 1-byte node. map.js used `node.hash_size || 1`
 * in the repeater label and popup, so an unknown size looked exactly like a
 * real 1-byte repeater.
 *
 * Runs the REAL public/roles.js + public/map.js in a vm sandbox (same shim as
 * test-important-links-byte-filter.js).
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

const src = fs.readFileSync(path.join(__dirname, 'public', 'map.js'), 'utf8');

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


const ctx = makeSandbox(() => Promise.resolve({}), {});
// buildPopup's app.js helpers (not loaded in this sandbox).
ctx.truncate = (s, n) => String(s).slice(0, n);
ctx.timeAgo = () => '1h ago';
const internals = ctx.window.__meshcoreMapInternals;
const PK = '961d00aa11bb22cc';

console.log('\n=== getKnownHashSize (roles.js shared helper) ===');
test('known sizes 1-3 are returned as numbers', () => {
  assert.strictEqual(ctx.getKnownHashSize({ hash_size: 1 }), 1);
  assert.strictEqual(ctx.getKnownHashSize({ hash_size: 2 }), 2);
  assert.strictEqual(ctx.getKnownHashSize({ hash_size: 3 }), 3);
  assert.strictEqual(ctx.getKnownHashSize({ hash_size: '2' }), 2, 'string from the boundary is cast');
});
test('missing / null / 0 / reserved 4 / garbage is unknown (null), never 1', () => {
  for (const n of [{}, { hash_size: null }, { hash_size: 0 }, { hash_size: 'x' }, { hash_size: 1.5 }, { hash_size: 4 }, { hash_size: 9 }, null, undefined]) {
    assert.strictEqual(ctx.getKnownHashSize(n), null, JSON.stringify(n));
  }
});

console.log('\n=== makeRepeaterLabelIcon ===');
test('unknown size: label is marked unknown, not a confirmed 1-byte label', () => {
  const html = internals.makeRepeaterLabelIcon({ public_key: PK }, false, false, null).options.html;
  assert.ok(/class="mc-mb-label[^"]*\bhs-unknown\b/.test(html), 'hs-unknown class: ' + html);
  assert.ok(/aria-label="[^"]*hash size unknown"/.test(html), 'aria says size unknown: ' + html);
  assert.ok(/title="[^"]*[Hh]ash size unknown/.test(html), 'tooltip says size unknown');
  // A node can have adverts that declare no size (zero-hop direct, pre-#3293
  // firmware), so the tooltip must not claim none was seen (v0.2.9 review
  // nit, #809).
  assert.ok(/title="[^"]*no size-declaring advert known/.test(html), 'tooltip names the missing size-declaring advert: ' + html);
  assert.ok(!/no advert seen yet/.test(html), 'tooltip must not claim no advert was seen: ' + html);
  // "?" is already the multi-byte "suspected" glyph, so the unknown-size
  // marker is an ellipsis: "the prefix may be longer".
  assert.ok(/>96\u2026</.test(html), 'visible text is the 1-byte prefix plus an ellipsis: ' + html);
  assert.ok(!/aria-label="repeater hash 96"/.test(html), 'must not read as a plain 1-byte repeater');
});
test('unknown size keeps the multi-byte status glyph and aria wording', () => {
  const html = internals.makeRepeaterLabelIcon({ public_key: PK }, false, false, 'suspected').options.html;
  assert.ok(/aria-label="multi-byte suspected, hash 96, hash size unknown"/.test(html), html);
  assert.ok(/status-suspected/.test(html));
});
test('known 2-byte size: full 2-byte prefix, no unknown marker', () => {
  const html = internals.makeRepeaterLabelIcon({ public_key: PK, hash_size: 2 }, false, false, null).options.html;
  assert.ok(/>961D</.test(html), html);
  assert.ok(!/hs-unknown|size unknown/.test(html), html);
  assert.ok(/aria-label="repeater hash 961D"/.test(html));
});
test('known 1-byte size: plain 1-byte label, no unknown marker', () => {
  const html = internals.makeRepeaterLabelIcon({ public_key: PK, hash_size: 1 }, false, false, null).options.html;
  assert.ok(/>96</.test(html) && !/hs-unknown|size unknown/.test(html), html);
});

console.log('\n=== buildPopup ===');
test('unknown size: popup says size unknown instead of (1B)', () => {
  const html = internals.buildPopup({ public_key: PK, role: 'repeater', name: 'R' });
  assert.ok(/size unknown/.test(html), html);
  assert.ok(!/\(1B\)/.test(html), 'must not claim 1B');
});
test('known size: popup shows prefix and (2B)', () => {
  const html = internals.buildPopup({ public_key: PK, role: 'repeater', name: 'R', hash_size: 2 });
  assert.ok(/961D/.test(html) && /\(2B\)/.test(html), html);
});

console.log('\n=== source guard ===');
test('map.js has no `hash_size || 1` guess left', () => {
  assert.ok(!/hash_size\s*\|\|\s*1\b/.test(src));
});
test('Byte Size filter still buckets unknown with 1-byte (unchanged, pinned by test-important-links-byte-filter.js)', () => {
  assert.strictEqual(internals.nodePassesByteSizeFilter({ role: 'repeater' }, '1'), true);
  assert.strictEqual(internals.nodePassesByteSizeFilter({ role: 'repeater' }, '2'), false);
});

console.log('\n' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
