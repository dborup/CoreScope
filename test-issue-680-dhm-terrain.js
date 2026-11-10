/* test-issue-680-dhm-terrain.js — DHM terrain hillshade overlay (#680).
 *
 * Unit tests for the overlay half of public/map-tile-providers.js in a vm
 * sandbox with a recording Leaflet stub:
 *   - offered only when /api/config/client carried an enabled dhm block
 *     with a same-origin URL;
 *   - the WMS options produce exactly the request shape the Go proxy
 *     allow-lists (cmd/server/dhm_proxy.go dhmFixedParams);
 *   - own pane between base tiles (200) and overlays/markers (400/600);
 *   - hash (?terrain=1|0) wins over the stored choice and is saved;
 *     toggling saves + rewrites the hash;
 *   - roles.js hands cfg.dhm to the registry.
 *
 * Runs via: node test-issue-680-dhm-terrain.js
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

const SRC = fs.readFileSync(path.join(__dirname, 'public', 'map-tile-providers.js'), 'utf8');
const ENABLED = {
  enabled: true, url: '/api/map/dhm/wms', layer: 'dhm_terraen_skyggekort',
  minZoom: 7, maxZoom: 19, bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
  attribution: 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel',
};

function makeStorage(init) {
  const store = Object.assign({}, init || {});
  return {
    getItem(k) { return Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null; },
    setItem(k, v) { store[k] = String(v); },
    removeItem(k) { delete store[k]; },
    _raw: store,
  };
}

// Minimal Leaflet: records layers, panes and the control's overlays, and
// fires overlayadd/overlayremove the way L.Control.Layers does for layers
// it lists.
function makeLeaflet() {
  function evented(o) {
    const h = {};
    o.on = (types, fn) => { types.split(' ').forEach(t => (h[t] = h[t] || []).push(fn)); return o; };
    o.off = () => o;
    o.fire = (t, e) => { (h[t] || []).slice().forEach(fn => fn(Object.assign({ type: t }, e || {}))); return o; };
    return o;
  }
  const L = {
    CRS: { EPSG3857: { code: 'EPSG:3857' } },
    latLngBounds: (b) => ({ _bounds: b }),
    tileLayer: (url, opts) => evented({ _url: url, options: opts || {}, kind: 'tile' }),
    control: {
      layers: (base, overlays, opts) => {
        const overlayLabels = Object.keys(overlays || {}).map(name => {
          const attrs = {};
          return { name, setAttribute: (k, v) => { attrs[k] = v; }, attrs };
        });
        const ctl = {
          base, overlays, opts, overlayLabels,
          addTo(map) { ctl._map = map; map._controls.push(ctl); return ctl; },
          remove() { ctl._removed = true; },
          getContainer() {
            return {
              querySelectorAll: () => [],
              querySelector: (sel) => sel === '.leaflet-control-layers-overlays label' ? (overlayLabels[0] || null) : null,
            };
          },
        };
        return ctl;
      },
    },
  };
  L.tileLayer.wms = (url, opts) => evented({ _url: url, options: opts || {}, kind: 'wms' });
  L.control.attribution = (opts) => {
    const c = { opts, addTo(map) { c._map = map; map._attributionControls.push(c); return c; }, remove() { c._removed = true; } };
    return c;
  };
  return L;
}

function makeMap() {
  const panes = { tilePane: { zIndex: 200, attrs: {}, setAttribute() {}, removeAttribute() {}, style: {} } };
  const layers = new Set();
  const map = {
    _controls: [], _attributionControls: [], panes, layers,
    getPane: (n) => panes[n],
    createPane: (n) => {
      const classes = [];
      panes[n] = { style: {}, classList: { add: (c) => classes.push(c) }, classes };
      return panes[n];
    },
    hasLayer: (l) => layers.has(l),
    // Like Leaflet: a layer listed in the control fires overlayadd/remove.
    addLayer(l) {
      if (layers.has(l)) return map;
      layers.add(l);
      if (map._controls.some(c => !c._removed && Object.values(c.overlays || {}).includes(l))) map.fire('overlayadd', { layer: l });
      return map;
    },
    removeLayer(l) {
      if (!layers.has(l)) return map;
      layers.delete(l);
      if (map._controls.some(c => !c._removed && Object.values(c.overlays || {}).includes(l))) map.fire('overlayremove', { layer: l });
      return map;
    },
  };
  const h = {};
  map.on = (types, fn) => { types.split(' ').forEach(t => (h[t] = h[t] || []).push(fn)); return map; };
  map.off = () => map;
  map.fire = (t, e) => { (h[t] || []).slice().forEach(fn => fn(Object.assign({ type: t }, e || {}))); return map; };
  return map;
}

function makeSandbox(opts) {
  opts = opts || {};
  const listeners = {};
  const replaced = [];
  const loc = { hash: opts.hash || '#/map', pathname: '/', search: '' };
  const ctx = {
    console, URLSearchParams, JSON, Math, Object, Array, String, Number, Boolean,
    location: loc,
    history: {
      state: null,
      replaceState(_s, _t, url) { replaced.push(url); const i = url.indexOf('#'); loc.hash = i >= 0 ? url.slice(i) : ''; },
    },
    localStorage: makeStorage(opts.storage),
    document: {
      documentElement: { getAttribute: () => opts.theme || 'light' },
      querySelector: () => null,
      createElement: () => ({}),
    },
    CustomEvent: function (type, init) { this.type = type; this.detail = (init && init.detail) || null; },
    L: makeLeaflet(),
  };
  ctx.window = {
    addEventListener: (type, fn) => { (listeners[type] = listeners[type] || []).push(fn); },
    dispatchEvent: (ev) => { (listeners[ev.type] || []).slice().forEach(fn => fn(ev)); return true; },
    matchMedia: () => ({ matches: false }),
    localStorage: ctx.localStorage,
  };
  if (opts.dhm !== undefined) ctx.window.MC_DHM_CFG = opts.dhm;
  ctx.globalThis = ctx;
  vm.createContext(ctx);
  vm.runInContext(SRC, ctx);
  ctx.replaced = replaced;
  return ctx;
}

function buildControl(ctx, withAttribution) {
  const map = makeMap();
  if (withAttribution) map.attributionControl = { builtIn: true };
  const auto = { _auto: true };
  ctx.window.MC_createLayerControl(map, auto, 'topleft');
  const ctl = map._controls[map._controls.length - 1];
  return { map, ctl };
}

function terrainLayer(ctl) {
  return ctl && ctl.overlays ? ctl.overlays['Terrain (DHM hillshade)'] : undefined;
}

console.log('\n=== #680 DHM terrain overlay ===');

// ── Offered only when enabled ───────────────────────────────────────────────

test('no dhm config → no overlay in the layer control', () => {
  const ctx = makeSandbox({});
  const { ctl } = buildControl(ctx);
  assert.strictEqual(ctl.overlays, null);
});

for (const [name, cfg] of [
  ['enabled:false', Object.assign({}, ENABLED, { enabled: false })],
  ['enabled:"true" (string)', Object.assign({}, ENABLED, { enabled: 'true' })],
  ['absolute third-party url', Object.assign({}, ENABLED, { url: 'https://api.dataforsyningen.dk/dhm_DAF' })],
  ['protocol-relative url', Object.assign({}, ENABLED, { url: '//evil.example/wms' })],
  ['missing url', Object.assign({}, ENABLED, { url: undefined })],
  ['null', null],
]) {
  test('not offered for dhm config ' + name, () => {
    const ctx = makeSandbox({ dhm: cfg });
    assert.strictEqual(ctx.window.MC_getTerrainConfig(), null);
    const { ctl } = buildControl(ctx);
    assert.strictEqual(ctl.overlays, null);
  });
}

test('enabled dhm config → one overlay labelled "Terrain (DHM hillshade)"', () => {
  const ctx = makeSandbox({ dhm: ENABLED });
  const { ctl } = buildControl(ctx);
  assert.deepStrictEqual(Object.keys(ctl.overlays), ['Terrain (DHM hillshade)']);
  assert.strictEqual(ctl.overlayLabels[0].attrs['data-overlay-id'], 'dhm-terrain');
});

test('overlay appears when config arrives async (fromConfig rebuild)', () => {
  const ctx = makeSandbox({});
  const { map } = buildControl(ctx);
  assert.strictEqual(map._controls[0].overlays, null);
  ctx.window.MC_DHM_CFG = ENABLED;
  ctx.window.MC_initTileRegistry(true);
  const ctl = map._controls[map._controls.length - 1];
  assert.ok(terrainLayer(ctl), 'overlay offered after config load');
  assert.strictEqual(map._controls[0]._removed, true, 'old control torn down');
});

// ── Request shape, opacity, z-order ────────────────────────────────────────

test('WMS options = the proxy allow-list shape, nothing else', () => {
  const ctx = makeSandbox({ dhm: ENABLED });
  const l = terrainLayer(buildControl(ctx).ctl);
  assert.strictEqual(l.kind, 'wms');
  assert.strictEqual(l._url, '/api/map/dhm/wms');
  const o = l.options;
  assert.strictEqual(o.layers, 'dhm_terraen_skyggekort');
  assert.strictEqual(o.styles, '');
  assert.strictEqual(o.format, 'image/png');
  assert.strictEqual(o.transparent, true);
  assert.strictEqual(o.version, '1.3.0');
  assert.strictEqual(o.crs.code, 'EPSG:3857');
  assert.strictEqual(o.tileSize, 256);
  // Leaflet turns every option it does not know into a WMS parameter; the
  // proxy rejects unknown parameters, so only these keys may be passed.
  const known = ['layers', 'styles', 'format', 'transparent', 'version', 'crs', 'tileSize', 'opacity',
    'pane', 'minZoom', 'maxZoom', 'bounds', 'attribution', 'className'];
  for (const k of Object.keys(o)) assert.ok(known.includes(k), 'unexpected option ' + k);
  assert.ok(!JSON.stringify(o).includes('token'), 'no token-ish option');
  assert.deepStrictEqual(o.bounds._bounds, ENABLED.bounds);
  assert.strictEqual(o.attribution, ENABLED.attribution);
});

test('opacity is in the 0.6–0.75 band', () => {
  const ctx = makeSandbox({ dhm: ENABLED });
  const o = terrainLayer(buildControl(ctx).ctl).options;
  assert.ok(o.opacity >= 0.6 && o.opacity <= 0.75, 'opacity ' + o.opacity);
});

test('own pane above base tiles (200) and below overlays (400) / markers (600)', () => {
  const ctx = makeSandbox({ dhm: ENABLED });
  const { map, ctl } = buildControl(ctx);
  const l = terrainLayer(ctl);
  const pane = map.getPane(l.options.pane);
  assert.ok(pane, 'pane created');
  assert.notStrictEqual(l.options.pane, 'tilePane', 'not in tilePane (would get the dark invert filter)');
  const z = Number(pane.style.zIndex);
  assert.ok(z > 200 && z < 400, 'pane z-index ' + z);
  assert.strictEqual(pane.style.pointerEvents, 'none');
  assert.ok(pane.classes.includes('mc-terrain-pane'));
});

// ── Default, hash and stored-preference round-trip ─────────────────────────

test('off by default', () => {
  const ctx = makeSandbox({ dhm: ENABLED });
  const { map, ctl } = buildControl(ctx);
  assert.strictEqual(map.hasLayer(terrainLayer(ctl)), false);
  assert.strictEqual(ctx.replaced.length, 0, 'hash untouched');
});

test('stored "1" restores it on, without rewriting the hash', () => {
  const ctx = makeSandbox({ dhm: ENABLED, storage: { 'mc-terrain-dhm': '1' } });
  const { map, ctl } = buildControl(ctx);
  assert.strictEqual(map.hasLayer(terrainLayer(ctl)), true);
  assert.strictEqual(ctx.replaced.length, 0);
});

test('?terrain=1 wins over stored "0" and is saved', () => {
  const ctx = makeSandbox({ dhm: ENABLED, hash: '#/live?terrain=1', storage: { 'mc-terrain-dhm': '0' } });
  const { map, ctl } = buildControl(ctx);
  assert.strictEqual(map.hasLayer(terrainLayer(ctl)), true);
  assert.strictEqual(ctx.localStorage._raw['mc-terrain-dhm'], '1');
});

test('?terrain=0 wins over stored "1" and is saved', () => {
  const ctx = makeSandbox({ dhm: ENABLED, hash: '#/map?terrain=0', storage: { 'mc-terrain-dhm': '1' } });
  const { map, ctl } = buildControl(ctx);
  assert.strictEqual(map.hasLayer(terrainLayer(ctl)), false);
  assert.strictEqual(ctx.localStorage._raw['mc-terrain-dhm'], '0');
});

test('garbage ?terrain= falls back to the stored choice', () => {
  const ctx = makeSandbox({ dhm: ENABLED, hash: '#/map?terrain=yes', storage: { 'mc-terrain-dhm': '1' } });
  const { map, ctl } = buildControl(ctx);
  assert.strictEqual(map.hasLayer(terrainLayer(ctl)), true);
});

test('toggle on → stored "1" + ?terrain=1, other hash params kept', () => {
  const ctx = makeSandbox({ dhm: ENABLED, hash: '#/map?lat=56&lon=10&zoom=8' });
  const { map, ctl } = buildControl(ctx);
  map.addLayer(terrainLayer(ctl));
  assert.strictEqual(ctx.localStorage._raw['mc-terrain-dhm'], '1');
  assert.strictEqual(ctx.location.hash, '#/map?lat=56&lon=10&zoom=8&terrain=1');
});

test('toggle off → stored "0" + terrain param removed', () => {
  const ctx = makeSandbox({ dhm: ENABLED, hash: '#/live?terrain=1&heard=1h' });
  const { map, ctl } = buildControl(ctx);
  map.removeLayer(terrainLayer(ctl));
  assert.strictEqual(ctx.localStorage._raw['mc-terrain-dhm'], '0');
  assert.strictEqual(ctx.location.hash, '#/live?heard=1h');
});

test('control rebuild keeps the same layer and its on state', () => {
  const ctx = makeSandbox({ dhm: ENABLED, storage: { 'mc-terrain-dhm': '1' } });
  const { map, ctl } = buildControl(ctx);
  const l = terrainLayer(ctl);
  ctx.window.MC_initTileRegistry(true);
  const ctl2 = map._controls[map._controls.length - 1];
  assert.notStrictEqual(ctl2, ctl);
  assert.strictEqual(terrainLayer(ctl2), l);
  assert.strictEqual(map.hasLayer(l), true);
});

test('map without an attribution control (Live) gets one only while terrain is on', () => {
  const ctx = makeSandbox({ dhm: ENABLED });
  const { map, ctl } = buildControl(ctx, false);
  assert.strictEqual(map._attributionControls.length, 0, 'none while off');
  map.addLayer(terrainLayer(ctl));
  const live = map._attributionControls.filter(c => !c._removed);
  assert.strictEqual(live.length, 1, 'one while on');
  assert.strictEqual(live[0].opts.prefix, false);
  map.removeLayer(terrainLayer(ctl));
  assert.strictEqual(map._attributionControls.filter(c => !c._removed).length, 0, 'removed when off');
});

test('restored-on terrain on a map without attribution control adds the credit', () => {
  const ctx = makeSandbox({ dhm: ENABLED, storage: { 'mc-terrain-dhm': '1' } });
  const { map } = buildControl(ctx, false);
  assert.strictEqual(map._attributionControls.filter(c => !c._removed).length, 1);
});

test('map with its own attribution control (Map page) gets no second one', () => {
  const ctx = makeSandbox({ dhm: ENABLED, storage: { 'mc-terrain-dhm': '1' } });
  const { map, ctl } = buildControl(ctx, true);
  map.removeLayer(terrainLayer(ctl));
  map.addLayer(terrainLayer(ctl));
  assert.strictEqual(map._attributionControls.length, 0);
});

test('initial restore happens before the layer control exists (no label re-render)', () => {
  const ctx = makeSandbox({ dhm: ENABLED, storage: { 'mc-terrain-dhm': '1' } });
  const map = makeMap();
  let controlsAtAdd = null;
  const origAdd = map.addLayer;
  map.addLayer = (l) => { if (l && l.kind === 'wms') controlsAtAdd = map._controls.length; return origAdd(l); };
  ctx.window.MC_createLayerControl(map, {}, 'topleft');
  assert.strictEqual(controlsAtAdd, 0, 'terrain added after the control was created');
});

test('resolveTerrainOn precedence table', () => {
  const ctx = makeSandbox({});
  const r = ctx.window.MC_resolveTerrainOn;
  assert.strictEqual(r(null, null), false);
  assert.strictEqual(r(null, '1'), true);
  assert.strictEqual(r(null, '0'), false);
  assert.strictEqual(r('1', '0'), true);
  assert.strictEqual(r('0', '1'), false);
  assert.strictEqual(r('true', null), false);
});

// ── roles.js wiring ─────────────────────────────────────────────────────────

test('roles.js stores cfg.dhm as MC_DHM_CFG before the registry re-init', () => {
  const roles = fs.readFileSync(path.join(__dirname, 'public', 'roles.js'), 'utf8');
  const a = roles.indexOf('window.MC_DHM_CFG = cfg.dhm || null;');
  const b = roles.indexOf("window.MC_initTileRegistry(true)");
  assert.ok(a > 0, 'MC_DHM_CFG assignment present');
  assert.ok(b > a, 'assigned before MC_initTileRegistry(true) rebuilds the controls');
});

console.log(`\n${passed} passed, ${failed} failed`);
process.exit(failed ? 1 : 0);
