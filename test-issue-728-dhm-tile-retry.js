/* test-issue-728-dhm-tile-retry.js — #728 (#769 review N3).
 *
 * With the orthophoto and the hillshade both cold, a view is about 120
 * tiles against the proxy's 64 admission slots (#744), so some tiles come
 * back `busy`: a 1×1 transparent PNG marked no-store. Leaflet keeps a
 * loaded tile, so it stayed blank until it left the view. Each DHM layer
 * (the hillshade overlay and the ortho base layer) now reloads such a
 * placeholder once, 3–5 s later, if it is still on the map and in view.
 *
 *   R1  A 1×1 tile is reloaded once (same URL) after 3000–4999 ms; a real
 *       256×256 tile never is.
 *   R2  At most once per tile: a placeholder again after the reload is left.
 *   R3  Not in view any more (panned away, other zoom), unloaded/aborted,
 *       or the layer off the map: no reload; leaving the map clears every
 *       pending timer.
 *   R4  The delay is jittered across the window, so a view's retries do
 *       not arrive together.
 *   R5  Both DHM layers have it (hillshade overlay, ortho base layer, also
 *       inside the Esri group); the protected-monument overlay, which is
 *       not DHM, does not.
 *
 * Runs via: node test-issue-728-dhm-tile-retry.js  (pure vm sandbox)
 */
'use strict';
const assert = require('assert');
const { makeSandbox, buildControl } = require('./tests/map-overlay-sandbox');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const BOUNDS = [[54.4265, 7.99125], [57.778, 15.5995]];
const ORTHO = {
  url: '/api/map/dhm/ortho/{z}/{x}/{y}', minZoom: 7, maxZoom: 20, bounds: BOUNDS,
  attribution: 'Indeholder data fra GeoDanmark, Ortofoto forår, CC BY 4.0',
};
const DHM = {
  enabled: true, url: '/api/map/dhm/wms', layer: 'dhm_terraen_skyggekort', minZoom: 7, maxZoom: 19, bounds: BOUNDS,
  attribution: 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0', ortho: ORTHO,
};
const HERITAGE = {
  enabled: true, url: 'https://www.kulturarv.dk/ffgeoserver/public/wms', layers: 'a,b',
  minZoom: 12, maxZoom: 19, bounds: BOUNDS, attribution: '© Slots- og Kulturstyrelsen',
};

// A sandbox with fake timers and a fixed Math.random.
function setup(opts) {
  opts = opts || {};
  const ctx = makeSandbox(Object.assign({ dhm: DHM, heritage: HERITAGE }, opts));
  const timers = [];
  let now = 0;
  ctx.setTimeout = (fn, ms) => { const t = { fn, at: now + ms, ms, live: true }; timers.push(t); return t; };
  ctx.clearTimeout = (t) => { if (t) t.live = false; };
  ctx.Math = Object.create(Math);
  ctx.Math.random = () => (opts.random !== undefined ? opts.random : 0.5);
  const { map, ctl } = buildControl(ctx, true);
  const clock = {
    timers,
    advance(ms) {
      now += ms;
      for (const t of timers.slice()) if (t.live && t.at <= now) { t.live = false; t.fn(); }
    },
    pending: () => timers.filter(t => t.live).length,
  };
  return { ctx, map, ctl, clock };
}
const terrainOf = (ctl) => ctl.overlays['Terrain (DHM hillshade)'];
const heritageOf = (ctl) => ctl.overlays['Protected ancient monuments (zoom 12+)'];
const orthoOf = (map) => {
  const e = map._controls[map._controls.length - 1].base['dhm-ortho'];
  return e.kind === 'layerGroup' ? Array.from(e.members).find(l => l._url === ORTHO.url) : e;
};

// A loaded tile image; `sets` counts src assignments after the first load.
function tile(w, url) {
  const img = { naturalWidth: w, naturalHeight: w, sets: 0 };
  let src = url || 'http://localhost/api/map/dhm/ortho/12/1/1';
  Object.defineProperty(img, 'src', { get: () => src, set: (v) => { src = v; img.sets++; } });
  return img;
}
// Tile (z, x, y) inside the sandbox view (zoom 12, 1024×768 px): x 0..3, y 0..2.
const IN_VIEW = { z: 12, x: 1, y: 1 };

function load(layer, img, coords) { layer.fire('tileload', { tile: img, coords: coords || IN_VIEW }); }

console.log('── #728 N3 R1: a placeholder is reloaded once, after the delay ──');

for (const [name, layerOf] of [['ortho', (s) => orthoOf(s.map)], ['hillshade', (s) => terrainOf(s.ctl)]]) {
  test('R1 ' + name + ': a 1×1 tile is reloaded with the same URL after 3000–4999 ms', () => {
    const s = setup();
    const layer = layerOf(s);
    s.map.addLayer(layer);
    const img = tile(1);
    const url = img.src;
    load(layer, img);
    s.clock.advance(2999);
    assert.strictEqual(img.sets, 0, 'reloaded before 3 s');
    s.clock.advance(2000);
    assert.strictEqual(img.sets, 1, 'not reloaded within 5 s');
    assert.strictEqual(img.src, url);
  });
  test('R1 ' + name + ': a real 256×256 tile is never reloaded', () => {
    const s = setup();
    const layer = layerOf(s);
    s.map.addLayer(layer);
    const img = tile(256);
    load(layer, img);
    s.clock.advance(60000);
    assert.strictEqual(img.sets, 0);
    assert.strictEqual(s.clock.timers.length, 0, 'a timer was set for a real tile');
  });
}

console.log('\n── #728 N3 R2: once per tile ──');

test('R2 a tile that is still a placeholder after its reload is left alone', () => {
  const s = setup();
  const layer = orthoOf(s.map);
  s.map.addLayer(layer);
  const img = tile(1);
  load(layer, img);
  s.clock.advance(5000);
  load(layer, img); // the reload came back busy again
  s.clock.advance(60000);
  assert.strictEqual(img.sets, 1);
});

test('R2 a whole view of placeholders: one reload each, no more', () => {
  const s = setup();
  const layer = orthoOf(s.map);
  s.map.addLayer(layer);
  const imgs = [];
  for (let x = 0; x < 4; x++) for (let y = 0; y < 3; y++) { const img = tile(1); imgs.push(img); load(layer, img, { z: 12, x, y }); }
  s.clock.advance(5000);
  imgs.forEach(img => load(layer, img, IN_VIEW));
  s.clock.advance(60000);
  assert.deepStrictEqual(imgs.map(i => i.sets), imgs.map(() => 1));
});

console.log('\n── #728 N3 R3: only while on the map and in view ──');

test('R3 panned out of view before the delay: no reload', () => {
  const s = setup();
  const layer = orthoOf(s.map);
  s.map.addLayer(layer);
  const img = tile(1);
  load(layer, img);
  s.map.view = { zoom: 12, min: { x: 4096, y: 4096 }, max: { x: 5120, y: 4864 } };
  s.clock.advance(5000);
  assert.strictEqual(img.sets, 0);
});

test('R3 a tile at the edge of the view still counts as in view; one just past it does not', () => {
  const s = setup();
  const layer = orthoOf(s.map);
  s.map.addLayer(layer);
  const edge = tile(1), past = tile(1);
  load(layer, edge, { z: 12, x: 3, y: 2 }); // 768..1024 × 512..768: inside 0..1024 × 0..768
  load(layer, past, { z: 12, x: 4, y: 0 }); // starts at x 1024: outside
  s.clock.advance(5000);
  assert.strictEqual(edge.sets, 1);
  assert.strictEqual(past.sets, 0);
});

test('R3 zoomed to another level before the delay: no reload', () => {
  const s = setup();
  const layer = orthoOf(s.map);
  s.map.addLayer(layer);
  const img = tile(1);
  load(layer, img);
  s.map.view = { zoom: 13, min: { x: 0, y: 0 }, max: { x: 2048, y: 1536 } };
  s.clock.advance(5000);
  assert.strictEqual(img.sets, 0);
});

for (const ev of ['tileunload', 'tileabort']) {
  test('R3 ' + ev + ' before the delay: no reload', () => {
    const s = setup();
    const layer = orthoOf(s.map);
    s.map.addLayer(layer);
    const img = tile(1);
    load(layer, img);
    layer.fire(ev, { tile: img, coords: IN_VIEW });
    s.clock.advance(5000);
    assert.strictEqual(img.sets, 0);
  });
}

test('R3 the layer leaves the map: every pending timer is cleared, nothing reloads', () => {
  const s = setup();
  const layer = terrainOf(s.ctl);
  s.map.addLayer(layer);
  const imgs = [tile(1), tile(1), tile(1)];
  imgs.forEach((img, i) => load(layer, img, { z: 12, x: i, y: 0 }));
  assert.strictEqual(s.clock.pending(), 3);
  s.map.removeLayer(layer);
  assert.strictEqual(s.clock.pending(), 0, 'timers left after the layer left');
  s.clock.advance(60000);
  assert.deepStrictEqual(imgs.map(i => i.sets), [0, 0, 0]);
});

test('R3 the ortho group leaves the map (another base picked): nothing reloads', () => {
  const s = setup({ mapCfg: { tiles: { providers: { esri: { enabled: true } } } } });
  const group = s.map._controls[s.map._controls.length - 1].base['dhm-ortho'];
  assert.strictEqual(group.kind, 'layerGroup');
  s.map.addLayer(group);
  const img = tile(1);
  load(orthoOf(s.map), img);
  s.map.removeLayer(group);
  s.clock.advance(60000);
  assert.strictEqual(img.sets, 0);
});

console.log('\n── #728 N3 R4: jitter ──');

test('R4 the delay spans 3000–4999 ms with Math.random', () => {
  for (const [r, want] of [[0, 3000], [0.5, 4000], [0.9999, 4999]]) {
    const s = setup({ random: r });
    const layer = orthoOf(s.map);
    s.map.addLayer(layer);
    load(layer, tile(1));
    assert.strictEqual(s.clock.timers[0].ms, want, 'random ' + r);
  }
});

console.log('\n── #728 N3 R5: which layers ──');

test('R5 the protected-monument overlay (not DHM) does not retry', () => {
  const s = setup();
  const layer = heritageOf(s.ctl);
  s.map.addLayer(layer);
  const img = tile(1, 'https://www.kulturarv.dk/x');
  load(layer, img);
  s.clock.advance(60000);
  assert.strictEqual(img.sets, 0);
  assert.strictEqual(s.clock.timers.length, 0);
});

console.log(`\n#728 N3 DHM tile retry: ${passed} passed, ${failed} failed`);
if (failed) process.exit(1);
