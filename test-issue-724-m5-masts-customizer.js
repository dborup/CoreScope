/* test-issue-724-m5-masts-customizer.js — #724 M5: the antenna sites
 * settings in the customizer (public/customize-v2.js, section `masts`) and
 * how the layer and the profile hint use them (public/masts-layer.js
 * settings / validSetting, public/masts-hint.js, public/map-tile-providers.js).
 *
 * The real files run in vm sandboxes; nothing is copied. The layer and the
 * hint use the recording Leaflet stub (tests/map-overlay-sandbox.js); the
 * customizer uses the browser-shaped sandbox of
 * test-issue-681-m6-customizer-terrain.js.
 *
 * Covered:
 *   1. the bounds mirror cmd/server/masts_proxy.go; validSetting is integers
 *      (and one boolean) only;
 *   2. settings(): the viewer's section over the operator's block over the
 *      built-ins; the zoom gate is a floor; the strong tier never passes the
 *      distance; an old server's block without the hint fields;
 *   3. the layer: the zoom gate follows the settings, live on
 *      masts-defaults-changed; the popup wording follows the distances;
 *   4. the sites overlay default follows `enabled`; a remembered choice and
 *      the hash still win; MC_forgetOverlayPref;
 *   5. the hint: the distance and the strong tier follow the settings,
 *      cellsNear at the longest distance (990 m) is still at most four cells
 *      and complete, and masts-defaults-changed re-pins an open path;
 *   6. the customizer: the section, stored values, import validation,
 *      export round trip, the event, the rendered inputs.
 *
 * Runs via: node test-issue-724-m5-masts-customizer.js
 */
'use strict';
const fs = require('fs');
const path = require('path');
const vm = require('vm');
const assert = require('assert');
const { makeSandbox, buildControl } = require('./tests/map-overlay-sandbox');

let passed = 0, failed = 0;
const pending = [];
function test(name, fn) {
  try {
    const r = fn();
    if (r && typeof r.then === 'function') {
      pending.push(r.then(() => { passed++; console.log('  ✅ ' + name); },
        (e) => { failed++; console.log('  ❌ ' + name + ': ' + e.message); }));
      return;
    }
    passed++; console.log('  ✅ ' + name);
  } catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const MASTS = {
  enabled: true, url: '/api/masts', minZoom: 13, cellSizeM: 2000,
  attribution: 'Antenna positions: Mastedatabasen, Digitaliseringsstyrelsen (CC0)',
  sourceUrl: 'https://mastedatabasen.dk/', cellExtent: [219, 3017, 462, 3209],
  hintDistanceM: 500, hintStrongM: 100,
};
const HINT_SRC = fs.readFileSync(path.join(__dirname, 'public', 'masts-hint.js'), 'utf8');
const tick = () => new Promise((r) => setImmediate(r));
const R = 6371000;
const north = (lat, lon, m) => [lat + m / R * 180 / Math.PI, lon];
const A = [56.1629, 10.2039];
const B = [56.2000, 10.4000];
const LTE800 = [{ technology: 'LTE', bandMHz: 800, interference: true }];
const site = (ll, entries) => ({ lat: ll[0], lon: ll[1], stations: ['S1'], entries: entries || LTE800, interference: true });

function layerSandbox(opts) {
  opts = opts || {};
  const ctx = makeSandbox(Object.assign({ masts: opts.cfg || MASTS }, opts.sandbox || {}));
  if (opts.section !== undefined) ctx.window.SITE_CONFIG = { masts: opts.section };
  vm.runInContext(HINT_SRC, ctx);
  return ctx;
}

console.log('\n=== #724 M5 antenna sites settings ===');

// ── 1. Bounds ───────────────────────────────────────────────────────────────

test('the bounds and defaults mirror cmd/server/masts_proxy.go', () => {
  const go = fs.readFileSync(path.join(__dirname, 'cmd', 'server', 'masts_proxy.go'), 'utf8');
  const c = (name) => {
    const m = go.match(new RegExp('\\b' + name + '\\s*=\\s*(\\d+)\\b'));
    assert(m, name + ' not found in masts_proxy.go');
    return Number(m[1]);
  };
  const ML = layerSandbox().window.MastsLayer;
  assert.deepStrictEqual(Array.from(ML.LIMITS.minZoom), [c('mastsMinZoom'), c('mastsMinZoomMax')]);
  assert.deepStrictEqual(Array.from(ML.LIMITS.hintDistanceM), [c('mastsHintDistanceMin'), c('mastsHintDistanceMax')]);
  assert.deepStrictEqual(Array.from(ML.LIMITS.hintStrongM), [0, c('mastsHintDistanceMax')]);
  assert.strictEqual(ML.MIN_ZOOM, c('mastsMinZoom'));
  assert.strictEqual(ML.HINT_DISTANCE_M, c('mastsHintDistanceDefault'));
  assert.strictEqual(ML.HINT_STRONG_M, c('mastsHintStrongDefault'));
});

test('validSetting: whole numbers in bounds, a boolean for enabled, nothing else', () => {
  const ML = layerSandbox().window.MastsLayer;
  const ok = { minZoom: [13, 15, 18], hintDistanceM: [100, 500, 990], hintStrongM: [0, 100, 990], enabled: [true, false] };
  const bad = {
    minZoom: [12, 19, 13.5, '14', null, undefined, NaN, Infinity, true, [14], {}],
    hintDistanceM: [99, 991, -500, 500.5, '500', '0x1F4', 5e2 + 0.1, NaN, null, false],
    hintStrongM: [-1, 991, 50.5, '100', null, NaN],
    enabled: [1, 0, 'true', 'false', null, undefined, {}],
  };
  for (const k of Object.keys(ok)) ok[k].forEach((v) => assert.strictEqual(ML.validSetting(k, v), true, k + ' rejected ' + JSON.stringify(v)));
  for (const k of Object.keys(bad)) bad[k].forEach((v) => assert.strictEqual(ML.validSetting(k, v), false, k + ' accepted ' + String(v)));
  ['hasOwnProperty', 'constructor', '__proto__', 'cellSizeM', 'url'].forEach((k) => assert.strictEqual(ML.validSetting(k, 1), false, k));
});

// ── 2. settings() ───────────────────────────────────────────────────────────

test('settings: the operator block, then the viewer section, then the built-ins', () => {
  const ML = layerSandbox().window.MastsLayer;
  const plain = (o) => JSON.parse(JSON.stringify(o));
  assert.deepStrictEqual(plain(ML.settings(MASTS, {})), { enabled: true, minZoom: 13, hintDistanceM: 500, hintStrongM: 100 });
  const op = Object.assign({}, MASTS, { minZoom: 14, hintDistanceM: 800, hintStrongM: 50 });
  assert.deepStrictEqual(plain(ML.settings(op, {})), { enabled: true, minZoom: 14, hintDistanceM: 800, hintStrongM: 50 });
  assert.deepStrictEqual(plain(ML.settings(op, { enabled: false, minZoom: 16, hintDistanceM: 300, hintStrongM: 20 })),
    { enabled: false, minZoom: 16, hintDistanceM: 300, hintStrongM: 20 });
  // A pre-M5 server sends no hint fields: the built-ins.
  const old = Object.assign({}, MASTS); delete old.hintDistanceM; delete old.hintStrongM;
  assert.deepStrictEqual(plain(ML.settings(old, {})), { enabled: true, minZoom: 13, hintDistanceM: 500, hintStrongM: 100 });
  // Junk from the server or the section is not a setting.
  assert.deepStrictEqual(plain(ML.settings(Object.assign({}, MASTS, { hintDistanceM: '800', minZoom: 3 }), { hintStrongM: '5', enabled: 'no' })),
    { enabled: true, minZoom: 13, hintDistanceM: 500, hintStrongM: 100 });
  assert.deepStrictEqual(plain(ML.settings(null, null)), { enabled: true, minZoom: 13, hintDistanceM: 500, hintStrongM: 100 });
});

test('settings: the viewer may raise the zoom gate, never lower it below the operator', () => {
  const ML = layerSandbox().window.MastsLayer;
  const op = Object.assign({}, MASTS, { minZoom: 15 });
  assert.strictEqual(ML.settings(op, { minZoom: 13 }).minZoom, 15);
  assert.strictEqual(ML.settings(op, { minZoom: 17 }).minZoom, 17);
});

test('settings: the strong tier never passes the distance', () => {
  const ML = layerSandbox().window.MastsLayer;
  assert.strictEqual(ML.settings(MASTS, { hintDistanceM: 200, hintStrongM: 300 }).hintStrongM, 200);
  assert.strictEqual(ML.settings(MASTS, { hintDistanceM: 150 }).hintStrongM, 100);
  assert.strictEqual(ML.settings(Object.assign({}, MASTS, { hintDistanceM: 200, hintStrongM: 400 }), {}).hintStrongM, 100);
});

test('settings() reads window.SITE_CONFIG.masts when no section is given', () => {
  const ctx = layerSandbox({ section: { hintDistanceM: 700 } });
  assert.strictEqual(ctx.window.MastsLayer.settings(MASTS).hintDistanceM, 700);
  ctx.window.SITE_CONFIG = {};
  assert.strictEqual(ctx.window.MastsLayer.settings(MASTS).hintDistanceM, 500);
});

// ── 3. The layer ────────────────────────────────────────────────────────────

function liveHarness(view, opts) {
  const ctx = layerSandbox(opts);
  const timers = [];
  ctx.setTimeout = (fn) => { timers.push(fn); return fn; };
  const requests = [];
  const sitesFor = {};
  ctx.fetch = (url) => {
    requests.push(url);
    const key = decodeURIComponent(url.split('cell=')[1]);
    return Promise.resolve({ ok: true, status: 200, headers: { get: () => null },
      json: () => Promise.resolve({ truncated: false, sites: sitesFor[key] || [] }) });
  };
  const { map } = buildControl(ctx);
  map.getZoom = () => view.z;
  map.getBounds = () => ({ getSouth: () => view.b.south, getWest: () => view.b.west, getNorth: () => view.b.north, getEast: () => view.b.east });
  const flush = async () => { for (let i = 0; i < 6; i++) { while (timers.length) timers.shift()(); await tick(); } };
  return { ctx, map, requests, sitesFor, flush };
}
const Z14 = { z: 14, b: { south: 56.15, west: 10.19, north: 56.17, east: 10.22 } };

test('the status line names the effective zoom gate', () => {
  const ctx = layerSandbox({ section: { minZoom: 15 } });
  const ML = ctx.window.MastsLayer;
  const s0 = { pending: 0, failed: 0, truncated: 0 };
  assert.strictEqual(ML.statusText(14, MASTS, true, s0, false), 'Zoom in to 15+ to see antenna positions');
  assert.strictEqual(ML.statusText(15, MASTS, true, s0, false), '');
  ctx.window.SITE_CONFIG = {};
  assert.strictEqual(ML.statusText(14, MASTS, true, s0, false), '');
  assert.strictEqual(ML.statusText(12, Object.assign({}, MASTS, { minZoom: 14 }), true, s0, false), 'Zoom in to 14+ to see antenna positions');
});

test('a raised gate fetches nothing below it; lowering it again applies at once', async () => {
  const h = liveHarness(Z14, { section: { minZoom: 15 } });   // sites overlay on by default
  h.map.fire('moveend');
  await h.flush();
  assert.strictEqual(h.requests.length, 0, 'fetched below the viewer gate: ' + h.requests);
  h.ctx.window.SITE_CONFIG = { masts: {} };
  h.ctx.window.dispatchEvent(new h.ctx.CustomEvent('masts-defaults-changed'));
  await h.flush();
  assert(h.requests.length > 0, 'the lowered gate did not apply');
});

test('the popup hint quotes the effective distances', () => {
  const ctx = layerSandbox({ section: { hintDistanceM: 800, hintStrongM: 50 } });
  const html = ctx.window.MastsLayer.popupHTML(site(A), MASTS);
  assert(/Within about 800 m, especially within 50 m/.test(html), html);
});

// ── 4. The sites overlay default ────────────────────────────────────────────

function overlayOn(ctx) {
  const { map } = buildControl(ctx);
  return { map, on: map.hasLayer(map._mcMastsSitesLayer) };
}

test('enabled: false makes the sites overlay start off; the hash and a stored choice still win', () => {
  assert.strictEqual(overlayOn(layerSandbox()).on, true, 'default on');
  assert.strictEqual(overlayOn(layerSandbox({ section: { enabled: false } })).on, false, 'customizer off ignored');
  assert.strictEqual(overlayOn(layerSandbox({ section: { enabled: false }, sandbox: { storage: { 'mc-masts': '1' } } })).on, true, 'a stored on lost');
  assert.strictEqual(overlayOn(layerSandbox({ section: { enabled: false }, sandbox: { hash: '#/map?masts=1' } })).on, true, 'a link lost');
  assert.strictEqual(overlayOn(layerSandbox({ section: { enabled: true }, sandbox: { storage: { 'mc-masts': '0' } } })).on, false, 'a stored off lost');
  // The interference overlay stays off by default either way.
  const { map } = buildControl(layerSandbox({ section: { enabled: true } }));
  assert.strictEqual(map.hasLayer(map._mcMastsIntLayer), false);
});

test('turning the sites overlay off writes masts=0, whatever the default', () => {
  for (const enabled of [true, false]) {
    const ctx = layerSandbox({ section: { enabled }, sandbox: { hash: '#/map?masts=1' } });
    const { map } = buildControl(ctx);
    map.removeLayer(map._mcMastsSitesLayer);
    assert(/[?&]masts=0\b/.test(ctx.location.hash), enabled + ': ' + ctx.location.hash);
  }
});

test('MC_forgetOverlayPref drops the remembered choice of that overlay only', () => {
  const ctx = layerSandbox({ sandbox: { storage: { 'mc-masts': '0', 'mc-masts-int': '1', 'mc-terrain-dhm': '1' } } });
  ctx.window.MC_forgetOverlayPref('masts-sites');
  assert.strictEqual(ctx.localStorage.getItem('mc-masts'), null);
  assert.strictEqual(ctx.localStorage.getItem('mc-masts-int'), '1');
  assert.strictEqual(ctx.localStorage.getItem('mc-terrain-dhm'), '1');
  ctx.window.MC_forgetOverlayPref('nope');
});

// ── 5. The hint ─────────────────────────────────────────────────────────────

test('the hint distance and strong tier follow the settings', () => {
  const ctx = layerSandbox({ section: { hintDistanceM: 800, hintStrongM: 50 } });
  const H = ctx.window.MastsHint;
  assert.strictEqual(H.HINT_DISTANCE_M, 800);
  assert.strictEqual(H.HINT_STRONG_M, 50);
  const want = { 49: 'strong', 50: 'strong', 51: 'near', 80: 'near', 700: 'near', 800: 'near', 801: null };
  for (const d of Object.keys(want).map(Number)) {
    const got = H.nearestFor(A[0], A[1], [site(north(A[0], A[1], d))]);
    assert.strictEqual(got ? got.tier : null, want[d], d + ' m → ' + JSON.stringify(got));
  }
  const text = H.hintFor({ a: H.nearestFor(A[0], A[1], [site(north(A[0], A[1], 700))]) }).detail;
  assert(/Within about 800 m, especially within 50 m/.test(text), text);
  // Strong tier off.
  ctx.window.SITE_CONFIG = { masts: { hintStrongM: 0 } };
  assert.strictEqual(H.nearestFor(A[0], A[1], [site(north(A[0], A[1], 1))]).tier, 'near');
  assert.strictEqual(H.nearestFor(A[0], A[1], [site(A)]).tier, 'strong');
});

test('the operator values drive the hint when the viewer set none', () => {
  const ctx = layerSandbox({ cfg: Object.assign({}, MASTS, { hintDistanceM: 300, hintStrongM: 30 }) });
  const H = ctx.window.MastsHint;
  assert.strictEqual(H.nearestFor(A[0], A[1], [site(north(A[0], A[1], 400))]), null);
  assert.strictEqual(H.nearestFor(A[0], A[1], [site(north(A[0], A[1], 40))]).tier, 'near');
});

test('cellsNear at the longest distance (990 m): complete and at most four cells', () => {
  const ctx = layerSandbox({ section: { hintDistanceM: 990 } });
  const H = ctx.window.MastsHint, ML = ctx.window.MastsLayer;
  const D = Math.PI / 180, M = 990.49;
  const dest = (ll, brg) => {
    const d = M / R, p = ll[0] * D, l = ll[1] * D;
    const p2 = Math.asin(Math.sin(p) * Math.cos(d) + Math.cos(p) * Math.sin(d) * Math.cos(brg));
    return [p2 / D, (l + Math.atan2(Math.sin(brg) * Math.sin(d) * Math.cos(p), Math.cos(d) - Math.sin(p) * Math.sin(p2))) / D];
  };
  for (let i = 0; i < 300; i++) {
    const lat = 54.6 + (i * 0.0071) % 3.0, lon = 8.1 + (i * 0.0193) % 7.4;
    const keys = H.cellsNear(lat, lon, MASTS);
    assert(keys.length <= 4, lat + ',' + lon + ' → ' + keys.length + ' cells');
    for (let k = 0; k < 72; k++) {
      const p = dest([lat, lon], k * 5 * D);
      const q = ML.toUTM32(p[0], p[1]);
      const ce = Math.floor(q.e / 2000), cn = Math.floor(q.n / 2000);
      if (ce < MASTS.cellExtent[0] || ce > MASTS.cellExtent[2] || cn < MASTS.cellExtent[1] || cn > MASTS.cellExtent[3]) continue;
      assert(keys.includes(ce + ',' + cn), 'cell ' + ce + ',' + cn + ' at 990 m from ' + lat + ',' + lon + ' not listed: ' + keys);
    }
  }
});

test('masts-defaults-changed re-pins an open path for the new distance', async () => {
  const h = liveHarness({ z: 9, b: { south: 55.5, west: 9.5, north: 56.8, east: 11.0 } }, { sandbox: { storage: { 'mc-masts': '0' } } });
  const H = h.ctx.window.MastsHint;
  // A site 900 m north of A: outside 500 m, inside 990 m.
  const far = north(A[0], A[1], 900);
  const p = h.ctx.window.MastsLayer.toUTM32(far[0], far[1]);
  const farKey = Math.floor(p.e / 2000) + ',' + Math.floor(p.n / 2000);
  h.sitesFor[farKey] = [site(far)];
  const hint = H.attach({ map: h.map, cfg: MASTS, panel: () => null });
  hint.update(A, B);
  await h.flush();
  const before = new Set(h.requests);
  assert.strictEqual(H.nearestFor(A[0], A[1], [site(far)]), null, '900 m hinted at 500 m');
  h.ctx.window.SITE_CONFIG = { masts: { hintDistanceM: 990 } };
  h.ctx.window.dispatchEvent(new h.ctx.CustomEvent('masts-defaults-changed'));
  await h.flush();
  const asked = h.requests.map((u) => decodeURIComponent(u.split('cell=')[1]));
  assert(asked.includes(farKey), 'the 990 m cells were not pinned: ' + asked);
  assert(h.requests.length > before.size, 'nothing new asked');
  assert.strictEqual(H.nearestFor(A[0], A[1], h.sitesFor[farKey]).tier, 'near');
  assert(h.map._mcMasts.stats().wanted <= h.ctx.window.MastsLayer.PIN_CELL_CAP, 'over the pin cap');
  // After clear() the event does nothing.
  hint.clear();
  const n = h.requests.length;
  h.ctx.window.SITE_CONFIG = { masts: { hintDistanceM: 100 } };
  h.ctx.window.dispatchEvent(new h.ctx.CustomEvent('masts-defaults-changed'));
  await h.flush();
  assert.strictEqual(h.map._mcMasts.stats().wanted, 0, 'a cleared hint re-pinned');
  assert.strictEqual(h.requests.length, n);
});

// ── 6. The customizer ───────────────────────────────────────────────────────

function makeStyle() {
  const props = {};
  return {
    setProperty(name, value) { props[name] = String(value); },
    removeProperty(name) { delete props[name]; },
    getPropertyValue(name) { return props[name] || ''; },
  };
}

/* customize-v2.js in the sandbox of test-issue-681-m6-customizer-terrain.js,
 * with the real masts-layer.js beside it. MC_getMastsConfig is the
 * registry's validator in the page; here it hands out the block as is (the
 * validator itself is covered in test-issue-724-masts-layer.js). */
function loadCustomizer(opts) {
  opts = opts || {};
  const store = {};
  const localStorage = {
    getItem(k) { return Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null; },
    setItem(k, v) { store[k] = String(v); },
    removeItem(k) { delete store[k]; },
  };
  const warnings = [];
  const events = [];
  const el = () => ({ id: '', textContent: '', innerHTML: '', className: '', setAttribute() {}, appendChild() {}, style: makeStyle(),
    addEventListener() {}, querySelectorAll: () => [], querySelector: () => null });
  const ctx = {
    window: {
      addEventListener() {}, dispatchEvent: (ev) => { events.push(ev && ev.type); return true; },
      SITE_CONFIG: {}, setInstanceTitle() {},
    },
    document: {
      readyState: 'complete', createElement: el, head: { appendChild() {} },
      body: { style: makeStyle(), setAttribute() {}, getAttribute: () => null, removeAttribute() {}, hasAttribute: () => false },
      documentElement: { style: makeStyle(), getAttribute: () => 'light', setAttribute() {}, removeAttribute() {} },
      getElementById: () => null, addEventListener() {}, querySelector: () => null, querySelectorAll: () => [],
    },
    console: Object.assign({}, console, { warn: (...a) => { warnings.push(a.join(' ')); } }),
    localStorage,
    setTimeout: (fn) => fn(), clearTimeout() {},
    Date, Math, Array, Object, JSON, String, Number, Boolean, Set, Map,
    parseInt, parseFloat, isNaN, isFinite, Infinity, NaN, undefined,
    MutationObserver: class { observe() {} },
    HashChangeEvent: class {},
    CustomEvent: class { constructor(t, o) { this.type = t; this.detail = o && o.detail; } },
    getComputedStyle: () => ({ getPropertyValue: () => '' }),
  };
  ctx.window.localStorage = localStorage;
  ctx.window.CustomEvent = ctx.CustomEvent;
  ctx.self = ctx.window;
  const cfg = 'cfg' in opts ? opts.cfg : MASTS;
  ctx.window.MC_getMastsConfig = () => cfg;
  vm.createContext(ctx);
  if (!opts.noLayer) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, 'public', 'masts-layer.js'), 'utf8'), ctx, { filename: 'masts-layer.js' });
  }
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'public', 'antenna-height.js'), 'utf8'), ctx, { filename: 'antenna-height.js' });
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'public', 'customize-v2.js'), 'utf8'), ctx, { filename: 'customize-v2.js' });
  const api = ctx.window._customizerV2;
  if (opts.server) api.init(opts.server);
  return { api, ctx, localStorage, warnings, events };
}

const stored = (c) => JSON.parse(c.localStorage.getItem('cs-theme-overrides') || '{}');

test('masts is a known object section', () => {
  const { api } = loadCustomizer();
  assert(api.VALID_SECTIONS.indexOf('masts') >= 0);
  assert.deepStrictEqual(Array.from(api.MASTS_SETTING_KEYS), ['enabled', 'minZoom', 'hintDistanceM', 'hintStrongM']);
  assert.strictEqual(api.validateShape({ masts: { hintDistanceM: 800 } }).valid, true);
  const bad = api.validateShape({ masts: 'on' });
  assert.strictEqual(bad.valid, false);
  assert(/must be an object/.test(bad.errors.join(' ')), bad.errors.join(' '));
});

test('only valid settings are stored; each rejected one warns', () => {
  const c = loadCustomizer();
  c.api.writeOverrides({ masts: { enabled: 'yes', minZoom: 12, hintDistanceM: 991, hintStrongM: 50, cellSizeM: 1 } });
  assert.deepStrictEqual(stored(c).masts, { hintStrongM: 50 });
  assert.strictEqual(c.warnings.filter((w) => /Invalid antenna sites setting/.test(w)).length, 3, c.warnings.join('\n'));
  const d = loadCustomizer();
  d.api.writeOverrides({ masts: { enabled: false, minZoom: 18, hintDistanceM: 100, hintStrongM: 0 } });
  assert.deepStrictEqual(stored(d).masts, { enabled: false, minZoom: 18, hintDistanceM: 100, hintStrongM: 0 });
  const e = loadCustomizer();
  e.api.writeOverrides({ masts: { hintDistanceM: '800', minZoom: 14.5 } });
  assert.strictEqual(stored(e).masts, undefined, 'a string or a fraction was stored');
});

test('without the layer on the page no setting is accepted', () => {
  const c = loadCustomizer({ noLayer: true });
  c.api.writeOverrides({ masts: { hintDistanceM: 800 } });
  assert.strictEqual(stored(c).masts, undefined);
  assert.strictEqual(c.api.mastsSettings({}), null);
  assert.strictEqual(c.api.renderMastsSettings(), '');
  assert.strictEqual(c.api.validateShape({ masts: { hintDistanceM: 800 } }).valid, false);
});

test('validateShape names every bad value and its bounds', () => {
  const { api } = loadCustomizer();
  const res = api.validateShape({ masts: { hintDistanceM: 5000, minZoom: '15', enabled: 1, hintStrongM: 10, extra: 1 } });
  assert.strictEqual(res.valid, false);
  const msg = res.errors.join(' | ');
  assert(/masts\.hintDistanceM = "5000" .*100-990/.test(msg), msg);
  assert(/masts\.minZoom = "15" .*13-18/.test(msg), msg);
  assert(/masts\.enabled = "1" .*true or false/.test(msg), msg);
  assert.strictEqual(res.errors.length, 3, msg);
});

test('the masts section survives an export round trip', () => {
  const first = loadCustomizer();
  const delta = { masts: { enabled: false, minZoom: 15, hintDistanceM: 750, hintStrongM: 60 } };
  first.api.writeOverrides(delta);
  const exported = first.api.readOverrides();
  assert.deepStrictEqual(JSON.parse(JSON.stringify(exported)), delta);
  const second = loadCustomizer();
  const shape = second.api.validateShape(exported);
  assert.strictEqual(shape.valid, true, shape.errors.join('; '));
  second.api.writeOverrides(exported);
  assert.deepStrictEqual(JSON.parse(JSON.stringify(second.api.readOverrides())), delta);
  assert.deepStrictEqual(JSON.parse(JSON.stringify(second.api.mastsSettings(second.api.computeEffective({}, exported)))),
    { enabled: false, minZoom: 15, hintDistanceM: 750, hintStrongM: 60 });
});

test('mastsSettings: this section over the operator block', () => {
  const op = Object.assign({}, MASTS, { minZoom: 14, hintDistanceM: 600 });
  const { api } = loadCustomizer({ cfg: op });
  const plain = (o) => JSON.parse(JSON.stringify(o));
  assert.deepStrictEqual(plain(api.mastsSettings({})), { enabled: true, minZoom: 14, hintDistanceM: 600, hintStrongM: 100 });
  assert.deepStrictEqual(plain(api.mastsSettings({ masts: { minZoom: 13, hintStrongM: 700 } })), { enabled: true, minZoom: 14, hintDistanceM: 600, hintStrongM: 600 });
});

test('a changed setting announces masts-defaults-changed once the pipeline has run; an unchanged one is silent', () => {
  const c = loadCustomizer({ server: {} });
  c.events.length = 0;
  c.api.writeOverrides({ masts: { hintDistanceM: 800 } });
  c.api.init({});
  assert(c.events.indexOf('masts-defaults-changed') >= 0, 'no signal: ' + c.events.join(', '));
  c.events.length = 0;
  c.api.applyCSS(c.api.computeEffective({}, c.api.readOverrides()), c.api.readOverrides());
  assert.strictEqual(c.events.indexOf('masts-defaults-changed'), -1, 'a no-op run announced a change');
  const recoloured = Object.assign({ theme: { mastSite: '#123456' } }, c.api.readOverrides());
  c.api.applyCSS(c.api.computeEffective({}, recoloured), recoloured);
  assert.strictEqual(c.events.indexOf('masts-defaults-changed'), -1, 'a colour announced a settings change');
});

test('the Antenna Sites section: shown only with the layer offered, inputs bounded by the rule', () => {
  const op = Object.assign({}, MASTS, { minZoom: 14 });
  const c = loadCustomizer({ cfg: op });
  c.ctx.window.SITE_CONFIG = { masts: { hintDistanceM: 700, hintStrongM: 70, enabled: false } };
  const html = c.api.renderMastsSettings();
  assert(/Antenna Sites/.test(html), html);
  const input = (key) => {
    const m = html.match(new RegExp('<input[^>]*data-cv2-masts="' + key + '"[^>]*>'));
    assert(m, 'no input for ' + key);
    return m[0];
  };
  assert(/type="checkbox"/.test(input('enabled')) && !/checked/.test(input('enabled')), input('enabled'));
  assert(/min="14" max="18"/.test(input('minZoom')) && /value="14"/.test(input('minZoom')), input('minZoom'));
  assert(/min="100" max="990"/.test(input('hintDistanceM')) && /value="700"/.test(input('hintDistanceM')), input('hintDistanceM'));
  assert(/min="0" max="700"/.test(input('hintStrongM')) && /value="70"/.test(input('hintStrongM')), input('hintStrongM'));
  assert(/id="cv2-masts-hintDistanceM"/.test(html) && /for="cv2-masts-hintDistanceM"/.test(html), 'label not bound');
  assert.strictEqual(loadCustomizer({ cfg: null }).api.renderMastsSettings(), '', 'shown without the layer offered');
});

test('the customizer and the layer share one rule (no second copy of the bounds)', () => {
  const src = fs.readFileSync(path.join(__dirname, 'public', 'customize-v2.js'), 'utf8');
  const code = src.replace(/\/\*[\s\S]*?\*\//g, '').replace(/^\s*\/\/.*$/gm, '');
  assert(!/\b990\b/.test(code) && !/hintDistanceM\s*[<>]=?\s*\d/.test(code), 'customize-v2.js hard-codes a masts bound');
  assert(/ML\.validSetting\(/.test(code) && /ML\.settings\(/.test(code));
});

Promise.all(pending).then(() => {
  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  if (failed) process.exit(1);
});
