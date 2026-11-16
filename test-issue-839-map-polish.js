/* test-issue-839-map-polish.js — #839 map findings from the rc19 test
 * (CS-Macmini), the minor ones:
 *
 *   F3  A WMS base-map tile that fails (Dataforsyningen answers 200 with an
 *       XML ServiceException, Chrome blocks it with ERR_BLOCKED_BY_ORB) is
 *       reloaded once, 3–5 s later, while it is still loaded and in view;
 *       never twice, never for a tile template, and every timer goes when
 *       the layer leaves the map.
 *   F4  The antenna-site popup shows a commissioning date as the Danish
 *       date (YYYY-MM-DD), not the UTC timestamp of the local midnight.
 *   F5  A dark style that shares its label with a light style is listed as
 *       "<label> (dark)" in the layer control, so no two base maps share a
 *       name; the customizer's own label stays plain.
 *   F6  The Map Controls panel stops above the map credit (style.css uses
 *       the credit height map.js publishes). The repeated DHM credit is
 *       kept on purpose: each one links its own dataset, which the CC BY
 *       terms require (#733, #734 Q5; pinned by the #734 suites).
 *   F10 The attribution bar takes its colours from theme tokens, not
 *       Leaflet's white.
 *
 * vm sandbox + recording Leaflet stub (tests/map-overlay-sandbox.js); the
 * real files run. The real browser is covered by
 * test-issue-839-map-polish-e2e.js.
 *
 * Runs via: node test-issue-839-map-polish.js
 */
'use strict';
const fs = require('fs');
const path = require('path');
const assert = require('assert');
const { makeSandbox, makeMap } = require('./tests/map-overlay-sandbox');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const ALL_ON = {
  tiles: {
    providers: {
      osm: { enabled: true }, esri: { enabled: true }, stamen: { enabled: true, token: 't' },
      dataforsyningen: { enabled: true, token: 'e2e-839-dummy' },
    },
  },
};

// A sandbox with fake timers and a fixed Math.random (as #728 N3's suite).
function setup(opts) {
  opts = opts || {};
  const ctx = makeSandbox({ mapCfg: ALL_ON });
  const timers = [];
  let now = 0;
  ctx.setTimeout = (fn, ms) => { const t = { fn, at: now + ms, live: true }; timers.push(t); return t; };
  ctx.clearTimeout = (t) => { if (t) t.live = false; };
  ctx.Math = Object.create(Math);
  ctx.Math.random = () => 0.5;
  const clock = {
    advance(ms) {
      now += ms;
      for (const t of timers.slice()) if (t.live && t.at <= now) { t.live = false; t.fn(); }
    },
    pending: () => timers.filter(t => t.live).length,
    timers,
  };
  return { ctx, clock };
}

function tile(url) {
  const img = { naturalWidth: 0, naturalHeight: 0, sets: 0 };
  let src = url;
  Object.defineProperty(img, 'src', { get: () => src, set: (v) => { src = v; img.sets++; } });
  return img;
}
const IN_VIEW = { z: 12, x: 1, y: 1 };
const natur = (ctx) => ctx.window.MC_createBaseLayer(ctx.window.MC_TILE_PROVIDERS['dataforsyningen-natur-friluft']);

console.log('── #839 F3: a failed WMS base-map tile is fetched once more ──');

test('F3 a tileerror on a WMS base layer reloads that tile once, same URL, after 3000–4999 ms', () => {
  const { ctx, clock } = setup();
  const map = makeMap();
  const layer = natur(ctx);
  assert.strictEqual(layer.kind, 'wms');
  map.addLayer(layer);
  const img = tile('https://api.dataforsyningen.dk/wms/natur_friluftskort?token=x&request=GetMap');
  const url = img.src;
  layer.fire('tileerror', { tile: img, coords: IN_VIEW });
  clock.advance(2999);
  assert.strictEqual(img.sets, 0, 'reloaded before 3 s');
  clock.advance(2000);
  assert.strictEqual(img.sets, 1, 'not reloaded within 5 s');
  assert.strictEqual(img.src, url);
});

test('F3 at most once per tile: a second error after the reload is left alone', () => {
  const { ctx, clock } = setup();
  const map = makeMap();
  const layer = natur(ctx);
  map.addLayer(layer);
  const img = tile('https://api.dataforsyningen.dk/x');
  layer.fire('tileerror', { tile: img, coords: IN_VIEW });
  clock.advance(5000);
  layer.fire('tileerror', { tile: img, coords: IN_VIEW });
  clock.advance(60000);
  assert.strictEqual(img.sets, 1);
  assert.strictEqual(clock.pending(), 0);
});

test('F3 no reload for a tile that left the view, was unloaded, or a layer off the map', () => {
  const { ctx, clock } = setup();
  const map = makeMap();
  const layer = natur(ctx);
  map.addLayer(layer);
  const away = tile('a'), gone = tile('b'), zoomed = tile('c');
  layer.fire('tileerror', { tile: away, coords: { z: 12, x: 40, y: 1 } });
  layer.fire('tileerror', { tile: gone, coords: IN_VIEW });
  layer.fire('tileunload', { tile: gone });
  layer.fire('tileerror', { tile: zoomed, coords: IN_VIEW });
  map.view.zoom = 13;
  clock.advance(10000);
  assert.deepStrictEqual([away.sets, gone.sets, zoomed.sets], [0, 0, 0]);

  const s2 = setup();
  const map2 = makeMap();
  const l2 = natur(s2.ctx);
  map2.addLayer(l2);
  l2.fire('tileerror', { tile: tile('d'), coords: IN_VIEW });
  assert.strictEqual(s2.clock.pending(), 1);
  map2.removeLayer(l2);
  assert.strictEqual(s2.clock.pending(), 0, 'leaving the map must clear the timer');
});

test('F3 every WMS base style retries (Skærmkort too); a tile template does not', () => {
  const { ctx, clock } = setup();
  const reg = ctx.window.MC_TILE_PROVIDERS;
  const wmsIds = Object.keys(reg).filter(id => reg[id].wms);
  assert(wmsIds.length >= 5, 'expected the Dataforsyningen styles, got ' + wmsIds.join(','));
  for (const id of wmsIds) {
    const map = makeMap();
    const layer = ctx.window.MC_createBaseLayer(reg[id]);
    map.addLayer(layer);
    const img = tile(id);
    layer.fire('tileerror', { tile: img, coords: IN_VIEW });
    clock.advance(5000);
    assert.strictEqual(img.sets, 1, id + ' was not retried');
  }
  const map = makeMap();
  const osm = ctx.window.MC_createBaseLayer(reg['osm-standard']);
  map.addLayer(osm);
  const img = tile('osm');
  osm.fire('tileerror', { tile: img, coords: IN_VIEW });
  clock.advance(60000);
  assert.strictEqual(img.sets, 0, 'a tile template must not be retried');
  assert.strictEqual(clock.pending(), 0);
});

test('F3 the WMS request options are unchanged (#741/#820: only the allow-listed keys)', () => {
  const { ctx } = setup();
  const layer = natur(ctx);
  assert.deepStrictEqual(Object.keys(layer.options).sort(),
    ['attribution', 'bounds', 'crs', 'format', 'layers', 'maxZoom', 'minZoom', 'styles', 'tileSize', 'transparent', 'version', 'zoomOffset'].sort());
});

console.log('\n── #839 F4: commissioning dates in Danish time ──');

const MASTS = {
  enabled: true, url: '/api/masts', minZoom: 13, cellSizeM: 2000,
  attribution: 'Antenna positions: Mastedatabasen, Digitaliseringsstyrelsen (CC0)',
  sourceUrl: 'https://mastedatabasen.dk/', cellExtent: [219, 3017, 462, 3209],
};
const ML = makeSandbox({ masts: MASTS }).window.MastsLayer;

test('F4 the popup shows 2022-02-03 for 2022-02-02T23:00:00Z (winter) and 2024-07-13 for 2024-07-12T22:00:00Z (summer)', () => {
  const html = ML.popupHTML({
    lat: 55.75, lon: 9.42, street: 'Bredager', houseNumber: '31', stations: ['S1'],
    entries: [
      { technology: 'LTE', bandMHz: 800, service: 'Mobiltelefoni', commissioned: '2022-02-02T23:00:00Z', interference: true },
      { technology: '5G-NR', bandMHz: 3500, service: 'Mobiltelefoni', commissioned: '2024-07-12T22:00:00Z', interference: false },
    ],
    interference: true,
  }, MASTS);
  assert(html.includes('>2022-02-03<'), 'winter date: ' + html);
  assert(html.includes('>2024-07-13<'), 'summer date: ' + html);
  assert(!/T2[23]:00:00Z/.test(html), 'a raw UTC timestamp is shown: ' + html);
});

test('F4 commissionedDate: Danish date for ISO values, the published text otherwise', () => {
  const f = ML.commissionedDate;
  assert.strictEqual(f('2014-03-29T23:00:00Z'), '2014-03-30');
  assert.strictEqual(f('2014-03-30T22:00:00Z'), '2014-03-31', 'DST starts 30 March 2014');
  assert.strictEqual(f('2022-02-03T12:00:00+01:00'), '2022-02-03');
  assert.strictEqual(f('2001-01-01'), '2001-01-01', 'a plain date stays that date');
  assert.strictEqual(f('2015-03-01<b>'), '2015-03-01<b>', 'non-ISO is shown as published (and escaped by the popup)');
  assert.strictEqual(f('2022-02-02T23:00:00'), '2022-02-02T23:00:00', 'a timestamp without a zone is not guessed at');
  assert.strictEqual(f(''), '');
  assert.strictEqual(f(undefined), undefined);
});

console.log('\n── #839 F5: no two base maps share a name in the layer control ──');

test('F5 every registry style has a distinct layer-control label', () => {
  const ctx = makeSandbox({ mapCfg: ALL_ON });
  const reg = ctx.window.MC_TILE_PROVIDERS;
  const labels = Object.keys(reg).map(id => ctx.window.MC_baseLayerLabel(id));
  const dup = labels.filter((l, i) => labels.indexOf(l) !== i);
  assert.deepStrictEqual(dup, [], 'duplicate labels: ' + dup.join(', '));
});

test('F5 the dark twins are "(dark)"; dark-only styles and every light style keep their label', () => {
  const ctx = makeSandbox({ mapCfg: ALL_ON });
  const label = ctx.window.MC_baseLayerLabel;
  const want = {
    'carto-voyager-dark': 'Carto Voyager (dark)',
    'positron-dark': 'Carto Positron (dark)',
    'esri-imagery-dark': 'Esri World Imagery (dark)',
    'dataforsyningen-skaermkort-graa-dark': 'Skærmkort grå (Denmark) (dark)',
    'osm-dark': 'OSM Standard (dark)',
    'stamen-toner-dark': 'Stamen Toner Lite (dark)',
    'carto-dark': 'Carto Dark',
    'esri-darkgray-labels': 'Esri Dark Gray Canvas',
    'dataforsyningen-skaermkort-graa': 'Skærmkort grå (Denmark)',
    'osm-standard': 'OSM Standard',
  };
  for (const id of Object.keys(want)) assert.strictEqual(label(id), want[id], id);
  // The customizer lists the themes apart and keeps the plain label.
  assert.strictEqual(ctx.window.MC_TILE_PROVIDERS['osm-dark'].label, 'OSM Standard');
});

test('F5 with Carto off the built-in OSM pair is still told apart; an unknown id is its own label', () => {
  const ctx = makeSandbox({ mapCfg: { tiles: { providers: { carto: { enabled: false } } } } });
  assert.strictEqual(ctx.window.MC_baseLayerLabel('osm-dark'), 'OSM Standard (dark)');
  assert.strictEqual(ctx.window.MC_baseLayerLabel('osm-standard'), 'OSM Standard');
  assert.strictEqual(ctx.window.MC_baseLayerLabel('no-such-style'), 'no-such-style');
});

test('F5 the layer control writes the label as text, not markup', () => {
  const src = fs.readFileSync(path.join(__dirname, 'public', 'map-tile-providers.js'), 'utf8');
  assert(/span\.textContent = ' ' \+ baseLayerLabel\(id\);/.test(src), 'the control must label each style through baseLayerLabel');
  assert(!/span\.innerHTML = ' ' \+ \(REGISTRY\[id\]\.label/.test(src));
});

console.log('\n── #839 F6: the credit is not covered by the Map Controls panel ──');

test('F6 the Map Controls panel stops above the credit; map.js publishes the credit height and lets it go on destroy', () => {
  const css = fs.readFileSync(path.join(__dirname, 'public', 'style.css'), 'utf8');
  const rule = /\n\.map-controls \{[^}]*\}/.exec(css);
  assert(rule, 'no .map-controls rule');
  assert(/max-height: calc\(100% - 24px - var\(--map-credit-height, 0px\)\)/.test(rule[0]), rule[0]);
  const js = fs.readFileSync(path.join(__dirname, 'public', 'map.js'), 'utf8');
  assert(/setProperty\('--map-credit-height'/.test(js), 'map.js does not publish --map-credit-height');
  assert(/creditObs\.observe\(credit\)/.test(js) && /if \(creditObs\) creditObs\.disconnect\(\);/.test(js), 'observer not tied to the mount');
});

console.log('\n── #839 F10: the attribution bar follows the theme ──');

test('F10 the attribution background, text and links come from theme tokens, scoped above leaflet.css', () => {
  const css = fs.readFileSync(path.join(__dirname, 'public', 'style.css'), 'utf8');
  const sel = '.leaflet-container .leaflet-control.leaflet-control-attribution';
  const body = (s) => { const i = css.indexOf(s + ' {'); assert(i >= 0, 'no rule ' + s); return css.slice(i, css.indexOf('}', i)); };
  const bar = body(sel), link = body(sel + ' a');
  assert(/background: color-mix\(in srgb, var\(--surface-1\) \d+%, transparent\);/.test(bar), bar);
  assert(/color: var\(--text-muted\);/.test(bar), bar);
  assert(/color: var\(--link-color\);/.test(link), link);
  assert(!/#[0-9a-f]{3,8}\b|rgba?\(/i.test(bar + link), 'hard-coded colour in ' + bar + link);
});

console.log('\n#839 map polish: ' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
