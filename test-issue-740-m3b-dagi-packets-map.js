/**
 * Unit tests for #740 M3b: the DAGI filter on Packets and the selected
 * area's outline on the Map.
 *
 *  - dagi-filter.js: decodeRings() (the polygon endpoint's delta encoding)
 *    and polygonQuery() (only a checked selection is ever asked for).
 *  - dagi-outline.js: one outline at a time, fit only when asked, a bounded
 *    cache, the latest request wins, cleared on clear().
 *  - packets.js / map.js / index.html wiring, by source scan where the
 *    behaviour needs a browser (the E2E suites cover it there).
 *
 * The real files run in a vm sandbox. The polygon response is in the server's
 * shape (GET /api/config/dagi/polygons, curled from a local server).
 */
'use strict';
const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

let passed = 0, failed = 0;
const queue = [];
function test(name, fn) { queue.push([name, fn]); }
const plain = (v) => (v === undefined ? v : JSON.parse(JSON.stringify(v)));
const same = (a, b, m) => assert.deepStrictEqual(plain(a), plain(b), m);
const flush = () => new Promise((r) => setImmediate(r));

/* ---------- dagi-filter.js in a sandbox ---------- */

function makeConfig() {
  return {
    enabled: true,
    credit: 'Indeholder data fra Klimadatastyrelsen, DAGI, november 2026, CC BY 4.0 — data bearbejdet',
    source: {},
    layers: [
      { layer: 'kommune', param: 'kommune', areas: [{ code: '0751', name: 'Aarhus' }, { code: '0860', name: 'Hjørring' }] },
      { layer: 'region', param: 'dkregion', areas: [{ code: '1083', name: 'Region Syddanmark' }] },
      { layer: 'postnummer', param: 'postnr', areas: [
        { code: '1050', name: 'København K', codes: ['1050', '1051', '1473'] }, { code: '8000', name: 'Aarhus C' }] },
    ],
  };
}

function loadDagi(opts) {
  opts = opts || {};
  const storage = Object.create(null);
  if (opts.stored) storage['meshcore-dagi-filter'] = JSON.stringify(opts.stored);
  const location = { hash: opts.hash || '#/map' };
  const win = {
    location,
    history: { state: null, replaceState(st, _t, url) { location.hash = url; } },
    matchMedia: () => ({ matches: false }),
  };
  const requests = [];
  const ctx = {
    window: win, document: {}, console, Promise, JSON, String, Number, Array, Object, Math, isFinite,
    encodeURIComponent, decodeURIComponent, URLSearchParams, setTimeout, clearTimeout, Intl,
    localStorage: { getItem: (k) => (k in storage ? storage[k] : null), setItem: (k, v) => { storage[k] = String(v); }, removeItem: (k) => { delete storage[k]; } },
    fetch: (url) => { requests.push(url); return Promise.resolve({ ok: true, json: () => Promise.resolve(makeConfig()) }); },
  };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync('public/url-state.js', 'utf8'), ctx, { filename: 'public/url-state.js' });
  win.URLState = ctx.URLState || win.URLState;
  vm.runInContext(fs.readFileSync('public/dagi-filter.js', 'utf8'), ctx, { filename: 'public/dagi-filter.js' });
  return { DF: win.DagiFilter, requests, location };
}

// A unit square (lon 10..11, lat 56..57) with a hole (10.25..10.75 by
// 56.25..56.75), and a separate island square, in quantum 1e-5 deltas.
function squareResponse() {
  const q = 1e-5;
  const ring = (pts) => {
    const out = [];
    let px = 0, py = 0;
    pts.forEach(([lon, lat], i) => {
      const x = Math.round(lon / q), y = Math.round(lat / q);
      if (i === 0) out.push(x, y); else out.push(x - px, y - py);
      px = x; py = y;
    });
    return out;
  };
  return {
    layer: 'kommune', param: 'kommune', credit: 'Indeholder data fra Klimadatastyrelsen, DAGI', quantum: q,
    encoding: 'rings: [lon0, lat0, dlon, dlat, ...] in quantum degrees, closing vertex implicit, even-odd',
    features: [
      { code: '0751', name: 'Aarhus', rings: [
        ring([[10, 56], [11, 56], [11, 57], [10, 57]]),
        ring([[10.25, 56.25], [10.75, 56.25], [10.75, 56.75], [10.25, 56.75]]),
      ] },
      { code: '0751', name: 'Aarhus', rings: [ring([[12, 55], [12.5, 55], [12.5, 55.5]])] },
    ],
  };
}
const close = (a, b) => Math.abs(a - b) < 1e-9;
function sameLatLngs(got, want, m) {
  assert.strictEqual(got.length, want.length, (m || '') + ': ' + got.length + ' vertices, want ' + want.length);
  got.forEach((p, i) => assert.ok(close(p[0], want[i][0]) && close(p[1], want[i][1]),
    (m || '') + ': vertex ' + i + ' is ' + JSON.stringify(p) + ', want ' + JSON.stringify(want[i])));
}

test('decodeRings: deltas summed, times the quantum, swapped to [lat, lon]; holes and islands kept as rings', () => {
  const { DF } = loadDagi();
  const d = DF.decodeRings(squareResponse());
  assert.strictEqual(d.rings.length, 3, 'outer, hole and island');
  sameLatLngs(d.rings[0], [[56, 10], [56, 11], [57, 11], [57, 10]], 'outer');
  sameLatLngs(d.rings[1], [[56.25, 10.25], [56.25, 10.75], [56.75, 10.75], [56.75, 10.25]], 'hole');
  sameLatLngs(d.rings[2], [[55, 12], [55, 12.5], [55.5, 12.5]], 'island');
  assert.strictEqual(d.vertices, 11);
  sameLatLngs(d.bounds, [[55, 10], [57, 12.5]], 'bounds');
});

test('decodeRings: the closing vertex stays implicit (no first point repeated)', () => {
  const { DF } = loadDagi();
  const r = DF.decodeRings(squareResponse()).rings[0];
  assert.ok(!(close(r[0][0], r[r.length - 1][0]) && close(r[0][1], r[r.length - 1][1])), 'the first vertex is repeated at the end');
});

test('decodeRings: a body that is not a polygon response gives no rings and no bounds', () => {
  const { DF } = loadDagi();
  for (const bad of [null, {}, { quantum: 1e-5 }, { quantum: 0, features: squareResponse().features },
    { quantum: 'x', features: [] }, { quantum: 1e-5, features: [{ rings: [[1, 2, 3, 4]] }, { rings: 'x' }, null] }]) {
    const d = DF.decodeRings(bad);
    same(d, { rings: [], bounds: null, vertices: 0 }, JSON.stringify(bad));
  }
});

test('decodeRings: Region Syddanmark size (5.3K vertices) decodes in well under a frame', () => {
  const { DF } = loadDagi();
  const n = 5302, ring = [1000000, 5500000];
  for (let i = 1; i < n; i++) ring.push((i % 7) - 3, (i % 5) - 2);
  const resp = { quantum: 1e-5, features: [{ code: '1083', rings: [ring] }] };
  let best = Infinity;
  for (let k = 0; k < 20; k++) {
    const t0 = process.hrtime.bigint();
    const d = DF.decodeRings(resp);
    best = Math.min(best, Number(process.hrtime.bigint() - t0) / 1e6);
    assert.strictEqual(d.vertices, n);
  }
  console.log(`    decodeRings: ${n} vertices in ${best.toFixed(2)} ms (best of 20)`);
  assert.ok(best < 16, 'decode took ' + best + ' ms');
});

test('polygonQuery: nothing before the config has checked the code', async () => {
  const s = loadDagi({ hash: '#/map?kommune=0751' });
  s.DF.syncFromUrl(s.location.hash);
  assert.strictEqual(s.DF.polygonQuery(), '', 'asked for an unchecked code');
  await s.DF.ready();
  assert.strictEqual(s.DF.polygonQuery(), 'layer=kommune&code=0751');
});

test('polygonQuery: the layer name, not the filter param; a street-postcode member asks for its group', async () => {
  const s = loadDagi({ stored: { param: 'dkregion', code: '1083' } });
  await s.DF.ready();
  assert.strictEqual(s.DF.polygonQuery(), 'layer=region&code=1083');
  const p = loadDagi({ stored: { param: 'postnr', code: '1051' } });
  await p.DF.ready();
  assert.strictEqual(p.DF.polygonQuery(), 'layer=postnummer&code=1050');
});

test('polygonQuery: an unknown code is never asked for, and no selection asks for nothing', async () => {
  const s = loadDagi({ stored: { param: 'kommune', code: '9999' } });
  await s.DF.ready();
  assert.strictEqual(s.DF.polygonQuery(), '');
  const none = loadDagi();
  await none.DF.ready();
  assert.strictEqual(none.DF.polygonQuery(), '');
});

/* ---------- dagi-outline.js with a fake Leaflet ---------- */

function loadOutline(opts) {
  opts = opts || {};
  const log = [];
  const map = {
    layers: new Set(),
    addLayer(l) { this.layers.add(l); log.push('add'); },
    removeLayer(l) { this.layers.delete(l); log.push('remove'); },
    fitBounds(b, o) { log.push(['fit', plain(b), plain(o)]); },
  };
  const L = {
    polygon(rings, options) {
      const layer = { rings, options, addTo(m) { m.addLayer(this); return this; } };
      log.push(['polygon', rings.length, plain(options)]);
      return layer;
    },
  };
  const fetches = [];
  const holds = [];
  const fetchJson = (q) => {
    fetches.push(q);
    if (opts.fail) return Promise.reject(new Error('HTTP 500'));
    if (opts.hold) return new Promise((r) => holds.push(() => r(squareResponse())));
    return Promise.resolve(squareResponse());
  };
  const ctx = { window: {}, console, Promise, Map, Object, String, Math };
  vm.createContext(ctx);
  Object.assign(ctx, { localStorage: { getItem: () => null }, document: {}, encodeURIComponent, decodeURIComponent, isFinite });
  vm.runInContext(fs.readFileSync('public/dagi-filter.js', 'utf8'), ctx, { filename: 'public/dagi-filter.js' });
  vm.runInContext(fs.readFileSync('public/dagi-outline.js', 'utf8'), ctx, { filename: 'public/dagi-outline.js' });
  const color = opts.color === undefined ? 'var-colour' : opts.color;
  const outline = ctx.window.DagiOutline.create({
    L, map, fetchJson, decode: ctx.window.DagiFilter.decodeRings,
    color: () => color, escape: (s) => '[esc]' + s,
  });
  return { outline, map, log, fetches, holds };
}
const polygons = (log) => log.filter((e) => Array.isArray(e) && e[0] === 'polygon');
const fits = (log) => log.filter((e) => Array.isArray(e) && e[0] === 'fit');

test('outline: show() draws one polygon of every ring, styled from the CSS colour, not interactive', async () => {
  const s = loadOutline();
  await s.outline.show('layer=kommune&code=0751', { fit: false });
  const p = polygons(s.log);
  assert.strictEqual(p.length, 1);
  assert.strictEqual(p[0][1], 3, 'rings');
  assert.strictEqual(p[0][2].color, 'var-colour');
  assert.strictEqual(p[0][2].fillColor, 'var-colour');
  assert.strictEqual(p[0][2].interactive, false);
  assert.strictEqual(p[0][2].className, 'dagi-outline');
  assert.strictEqual(s.map.layers.size, 1);
  assert.strictEqual(s.outline.query(), 'layer=kommune&code=0751');
});

test('outline: no colour variable means no colour of our own (Leaflet default), never a hardcoded one', async () => {
  const s = loadOutline({ color: '' });
  await s.outline.show('layer=kommune&code=0751', { fit: false });
  const o = polygons(s.log)[0][2];
  assert.ok(!('color' in o) && !('fillColor' in o), JSON.stringify(o));
  const code = fs.readFileSync('public/dagi-outline.js', 'utf8').replace(/\/\*[\s\S]*?\*\/|\/\/.*$/gm, '');
  assert.ok(!/#[0-9a-fA-F]{3,8}\b|rgba?\(|hsla?\(/.test(code), 'a hardcoded colour in dagi-outline.js');
});

test('outline: the credit is the attribution, escaped', async () => {
  const s = loadOutline();
  await s.outline.show('layer=kommune&code=0751', { fit: false });
  assert.strictEqual(polygons(s.log)[0][2].attribution, '[esc]Indeholder data fra Klimadatastyrelsen, DAGI');
});

test('outline: fit only when asked, to the decoded bounds', async () => {
  const s = loadOutline();
  await s.outline.show('layer=kommune&code=0751', { fit: false });
  assert.strictEqual(fits(s.log).length, 0, 'fitted without a pick');
  await s.outline.show('layer=region&code=1083', { fit: true });
  const f = fits(s.log);
  assert.strictEqual(f.length, 1);
  sameLatLngs(f[0][1], [[55, 10], [57, 12.5]], 'fitted bounds');
});

test('outline: one outline at a time: a new area replaces the old one at once, before its answer', async () => {
  const s = loadOutline({ hold: true });
  const first = s.outline.show('layer=kommune&code=0751', { fit: false });
  s.holds.shift()();
  await first;
  assert.strictEqual(s.map.layers.size, 1);
  const second = s.outline.show('layer=kommune&code=0860', { fit: false });
  assert.strictEqual(s.map.layers.size, 0, 'the old outline stays while the new one loads');
  s.holds.shift()();
  await second;
  assert.strictEqual(s.map.layers.size, 1);
  assert.strictEqual(polygons(s.log).length, 2);
});

test('outline: only the latest request draws; clear() wins over an answer in flight', async () => {
  const s = loadOutline({ hold: true });
  const a = s.outline.show('layer=kommune&code=0751', { fit: true });
  const b = s.outline.show('layer=kommune&code=0860', { fit: false });
  s.holds[1]();
  await b;
  s.holds[0]();
  await a;
  assert.strictEqual(polygons(s.log).length, 1, 'a stale answer drew');
  assert.strictEqual(fits(s.log).length, 0, 'a stale answer fitted');
  assert.strictEqual(s.outline.query(), 'layer=kommune&code=0860');
  const c = s.outline.show('layer=region&code=1083', { fit: true });
  s.outline.clear();
  s.holds[2]();
  await c;
  assert.strictEqual(s.map.layers.size, 0, 'drawn after clear()');
  assert.strictEqual(fits(s.log).length, 0);
});

test('outline: clear() removes the outline; a second clear() is harmless', async () => {
  const s = loadOutline();
  await s.outline.show('layer=kommune&code=0751', { fit: false });
  s.outline.clear();
  assert.strictEqual(s.map.layers.size, 0);
  assert.strictEqual(s.outline.query(), '');
  s.outline.clear();
  assert.strictEqual(s.map.layers.size, 0);
});

test('outline: each area is fetched once (cached); the same area again is not redrawn', async () => {
  const s = loadOutline();
  await s.outline.show('layer=kommune&code=0751', { fit: false });
  await s.outline.show('layer=kommune&code=0751', { fit: false });
  assert.strictEqual(polygons(s.log).length, 1, 'redrawn for the same area');
  await s.outline.show('layer=kommune&code=0860', { fit: false });
  await s.outline.show('layer=kommune&code=0751', { fit: false });
  same(s.fetches, ['layer=kommune&code=0751', 'layer=kommune&code=0860']);
});

test('outline: the cache is bounded', async () => {
  const s = loadOutline();
  for (let i = 0; i < 12; i++) await s.outline.show('layer=kommune&code=' + i, { fit: false });
  assert.ok(s.outline.cacheSize() <= 4, 'cache holds ' + s.outline.cacheSize());
  await s.outline.show('layer=kommune&code=0', { fit: false });
  assert.strictEqual(s.fetches.length, 13, 'an evicted area is fetched again');
});

test('outline: the same area picked again still fits', async () => {
  const s = loadOutline();
  await s.outline.show('layer=kommune&code=0751', { fit: false });
  await s.outline.show('layer=kommune&code=0751', { fit: true });
  assert.strictEqual(fits(s.log).length, 1);
});

test('outline: a failed request draws nothing and is retried next time', async () => {
  const s = loadOutline({ fail: true });
  await s.outline.show('layer=kommune&code=0751', { fit: true });
  assert.strictEqual(s.map.layers.size, 0);
  assert.strictEqual(fits(s.log).length, 0);
  assert.strictEqual(s.outline.query(), '', 'a failed area still counts as drawn');
  await s.outline.show('layer=kommune&code=0751', { fit: true });
  assert.strictEqual(s.fetches.length, 2);
});

/* ---------- wiring (source scans; behaviour in the E2E suites) ---------- */

function slice(src, from, to) {
  const a = src.indexOf(from);
  assert.ok(a >= 0, 'no ' + from);
  const b = to ? src.indexOf(to, a + from.length) : src.length;
  assert.ok(b > a, 'no ' + to + ' after ' + from);
  return src.slice(a, b);
}

test('index.html: dagi-outline.js is loaded, after dagi-filter.js and before map.js', () => {
  const html = fs.readFileSync('public/index.html', 'utf8');
  const f = html.indexOf('src="dagi-filter.js?v=__BUST__"');
  const o = html.indexOf('src="dagi-outline.js?v=__BUST__"');
  const m = html.indexOf('src="map.js?v=__BUST__"');
  assert.ok(f > 0 && o > f && m > o, `dagi-filter ${f}, dagi-outline ${o}, map ${m}`);
});

test('packets.js: the DAGI control beside Area, the ADVERT note, and destroy() releases both', () => {
  const src = fs.readFileSync('public/packets.js', 'utf8');
  assert.ok(/id="packetsAreaFilter"[^\n]*\n\s*<div id="packetsDagiFilter"/.test(src), 'the control is not beside Area');
  assert.ok(/id="packetsDagiNotice"[^>]*role="status"/.test(src), 'no status line');
  assert.ok(/DagiFilter\.init\(document\.getElementById\('packetsDagiFilter'\)\)/.test(src));
  const destroy = slice(src, '  function destroy() {', 'async function loadObservers');
  assert.ok(/DagiFilter\.offChange\(/.test(destroy) && /DagiFilter\.release\(/.test(destroy), 'destroy() keeps the control');
  assert.ok(/dagiMembers = null/.test(destroy), 'destroy() keeps the member set');
});

test('packets.js: loadPackets waits for ready() before it reads queryString() and sends', () => {
  const src = fs.readFileSync('public/packets.js', 'utf8');
  const load = slice(src, 'async function loadPackets()', 'function renderLeft()');
  const ready = load.indexOf('await DagiFilter.ready()');
  const sync = load.indexOf('syncDagiMembers()');
  const qs = load.indexOf('dagiQuery:');
  const send = load.indexOf("await api('/packets?'");
  assert.ok(ready > 0 && sync > ready && qs > ready && send > qs, `ready ${ready}, sync ${sync}, dagiQuery ${qs}, send ${send}`);
});

test('packets.js: the live member set is keyed on the sent query, empty means nothing, and the sender is the server rule', () => {
  const src = fs.readFileSync('public/packets.js', 'utf8');
  const sync = slice(src, 'function syncDagiMembers()', '\n  }\n');
  assert.ok(/DagiFilter\.queryString\(\)/.test(sync), 'not keyed on the sent query');
  assert.ok(/emptyMatchesAll: false/.test(sync), 'an empty DAGI set must match nothing');
  assert.ok(/geoFilter=0/.test(sync), 'the server rule ignores geo_filter');
  assert.ok(/ScopeMemberFilter\.advertSender\(getParsedDecoded\(p\)\)/.test(sync));
});

test('map.js: the control in the map controls, the node fetch after ready(), the outline fitted only on a pick, and destroy() clears it', () => {
  const src = fs.readFileSync('public/map.js', 'utf8');
  assert.ok(/<div id="mapAreaFilter"><\/div>\s*<div id="mapDagiFilter"/.test(src), 'the control is not beside Area');
  assert.ok(/DagiFilter\.init\(document\.getElementById\('mapDagiFilter'\), \{ beside: document\.getElementById\('mapControls'\) \}\)/.test(src),
    'the picker opens inside the scrolling controls panel, which clips it');
  const req = slice(src, 'async function loadNodesRequest(', 'Load observers for jump buttons');
  const ready = req.indexOf('await DagiFilter.ready()');
  const qs = req.indexOf('DagiFilter.queryString()');
  const fetch = req.indexOf('await fetchAllNodes(');
  assert.ok(ready > 0 && qs > ready && fetch > qs, `ready ${ready}, queryString ${qs}, fetch ${fetch}`);
  assert.ok(/DagiFilter\.onChange\(function \(\) \{[^}]*refreshDagiOutline\(true\)/.test(src), 'a pick does not fit');
  assert.ok(/refreshDagiOutline\(false\)/.test(src), 'a cold link must draw without a fit');
  const destroy = slice(src, '  function destroy() {', 'registerPage(\'map\'');
  assert.ok(/dagiOutline\.clear\(\)/.test(destroy) && /DagiFilter\.release\(/.test(destroy) && /DagiFilter\.offChange\(/.test(destroy), 'destroy() keeps the outline or the control');
  assert.ok(!/moveend[^\n]*refreshDagiOutline|zoomend[^\n]*refreshDagiOutline/.test(src), 'a request per pan');
});

/* ---------- run ---------- */

(async () => {
  console.log('\n=== #740 M3b: DAGI on Packets and the Map outline ===');
  for (const [name, fn] of queue) {
    try { await fn(); await flush(); passed++; console.log(`  ✓ ${name}`); }
    catch (e) { failed++; console.error(`  ✗ ${name}: ${e.message}`); }
  }
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
