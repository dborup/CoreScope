/* test-issue-859-layer-control.js — #859, the #856 review follow-ups that
 * are logic rather than layout:
 *
 *   W  The console warning for an operator default that is not used names
 *      the id as configured, not the one a CARTO id is migrated to (#845),
 *      and says why: CARTO is off by default, and how to turn it on.
 *   D  config.example.json says so next to darkDefault/lightDefault.
 *   N  Owner addition (2026-11-16), a regression from #856: no base map is
 *      hidden by the theme filter. A style with no variant for the current
 *      theme is listed as its other variant (a light-only map is offered,
 *      and shown, as the light map in dark mode); a style with both variants
 *      is listed once, as the current theme's, and a pick still moves to its
 *      counterpart on a theme switch.
 *
 * The real public/map-tile-providers.js runs in the vm sandbox
 * (tests/map-overlay-sandbox.js). The tap height, the per-render layout and
 * the antenna sites zoom hint are browser layout, so they are pinned by
 * test-issue-845-layer-groups-e2e.js (cases marked #859).
 *
 * Runs via: node test-issue-859-layer-control.js
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

// The [map tiles] warnings printed while the registry loads `tiles`.
function warnings(tiles) {
  const seen = [];
  const orig = console.warn;
  console.warn = (...a) => { const m = a.join(' '); if (/^\[map tiles\]/.test(m)) seen.push(m); else orig.apply(console, a); };
  try { makeSandbox({ mapCfg: { tiles } }); } finally { console.warn = orig; }
  return seen;
}

console.log('\n=== #859 layer control follow-ups: the CARTO default warning ===');

test('W1 darkDefault carto-dark, CARTO off, Esri on: one warning naming carto-dark, the reason and the map used', () => {
  const w = warnings({ darkDefault: 'carto-dark', providers: { esri: { enabled: true } } });
  assert.strictEqual(w.length, 1, JSON.stringify(w));
  assert.ok(w[0].indexOf('map.tiles.darkDefault "carto-dark"') >= 0, 'not the configured id: ' + w[0]);
  assert.ok(/CARTO is off by default since #845/.test(w[0]), 'no reason: ' + w[0]);
  assert.ok(w[0].indexOf('map.tiles.providers.carto.enabled: true') >= 0, 'no switch: ' + w[0]);
  assert.ok(w[0].indexOf('Using "esri-darkgray-labels" instead.') >= 0, 'not the map used: ' + w[0]);
  assert.ok(w[0].indexOf('"esri-darkgray-labels" is') < 0, 'blames the migrated id: ' + w[0]);
});

test('W2 darkDefault carto-dark with Esri off, lightDefault carto-voyager: each named, with its keyless map', () => {
  const w = warnings({ darkDefault: 'carto-dark', lightDefault: 'carto-voyager', providers: {} });
  assert.strictEqual(w.length, 2, JSON.stringify(w));
  assert.ok(/darkDefault "carto-dark" is not used: CARTO is off by default.*Using "osm-dark" instead\.$/.test(w[0]), w[0]);
  assert.ok(/lightDefault "carto-voyager" is not used: CARTO is off by default.*Using "osm-standard" instead\.$/.test(w[1]), w[1]);
});

test('W3 CARTO on: a CARTO default is used, no warning', () => {
  assert.deepStrictEqual(warnings({ darkDefault: 'carto-dark', lightDefault: 'carto-light', providers: { carto: { enabled: true } } }), []);
});

test('W4 the other gated and unknown defaults keep their wording', () => {
  const w = warnings({ darkDefault: 'esri-darkgray-labels', lightDefault: 'no-such-map', providers: {} });
  assert.deepStrictEqual(w, [
    '[map tiles] map.tiles.darkDefault "esri-darkgray-labels" is ignored: its provider is not enabled (set map.tiles.providers.esri.enabled: true). Using the fallback base map instead.',
    '[map tiles] map.tiles.lightDefault "no-such-map" is ignored: it is not a known tile style. Using the fallback base map instead.',
  ]);
});

test('W5 a usable default warns nothing', () => {
  assert.deepStrictEqual(warnings({ darkDefault: 'osm-dark', lightDefault: 'osm-standard', providers: { osm: { enabled: true } } }), []);
});

test('D1 config.example.json: the defaults comment says CARTO is opt-in, migrated, and warned about', () => {
  const ex = JSON.parse(fs.readFileSync(path.join(__dirname, 'config.example.json'), 'utf8'));
  const c = ex.map.tiles._comment_defaults;
  assert.ok(/Carto is opt-in too \(#845\)/.test(c), c);
  assert.ok(/migrated to its nearest keyless map/.test(c), c);
  assert.ok(/console names the configured id/.test(c), c);
  assert.ok(/warns once, naming the configured Carto id/.test(ex.map.tiles.providers._comment_carto), ex.map.tiles.providers._comment_carto);
});

// ── N: nothing hidden by the theme filter ───────────────────────────────────
const DF = { enabled: true, token: 'e2e-859-dummy' };
const PROVIDERS = { osm: { enabled: true }, esri: { enabled: true }, opentopomap: { enabled: true }, dataforsyningen: DF };

// The base maps the real control lists on a stub map, in `theme`.
function listed(theme, pick) {
  const ctx = makeSandbox({ mapCfg: { tiles: { providers: PROVIDERS } }, theme });
  ctx.document.documentElement = { getAttribute: () => theme };
  ctx.MutationObserver = function () { this.observe = () => {}; this.disconnect = () => {}; };
  ctx.window.MC_DHM_CFG = { enabled: true, url: '/api/map/dhm', ortho: { url: '/api/map/dhm/ortho/{z}/{x}/{y}', minZoom: 10, maxZoom: 19, bounds: [[54, 8], [58, 15.5]], attribution: 'SDFI' } };
  const map = makeMap();
  ctx.window.MC_createLayerControl(map, { _auto: true }, 'topleft');
  const ctl = map._controls.filter(c => !c._removed).slice(-1)[0];
  if (pick) map.fire('baselayerchange', { name: pick, layer: ctl.base[pick] });
  const last = map._controls.filter(c => !c._removed).slice(-1)[0];
  return { ids: Object.keys(last.base), P: ctx.window.MC_TILE_PROVIDERS, LG: ctx.window.MC_LayerGroups };
}

test('N1 dark: all four Danish base maps (light-only ones as their light variant) and Aerial are listed', () => {
  const { ids } = listed('dark');
  for (const id of ['dataforsyningen-skaermkort', 'dataforsyningen-skaermkort-daempet', 'dataforsyningen-natur-friluft',
    'dataforsyningen-skaermkort-graa-dark', 'dhm-ortho']) assert.ok(ids.includes(id), id + ' missing from ' + ids);
  assert.ok(!ids.includes('dataforsyningen-skaermkort-graa'), 'Skærmkort grå listed twice (light and dark)');
});

test('N2 light: every Danish base map and Aerial are listed, each once', () => {
  const { ids } = listed('light');
  for (const id of ['dataforsyningen-skaermkort', 'dataforsyningen-skaermkort-graa', 'dataforsyningen-skaermkort-daempet',
    'dataforsyningen-natur-friluft', 'dhm-ortho']) assert.ok(ids.includes(id), id + ' missing from ' + ids);
  assert.ok(!ids.includes('dataforsyningen-skaermkort-graa-dark'), 'Skærmkort grå listed twice');
});

test('N3 no style is hidden in either theme: each is listed itself or through its counterpart', () => {
  for (const theme of ['light', 'dark']) {
    const { ids, P, LG } = listed(theme);
    Object.keys(P).forEach((id) => {
      assert.ok(ids.includes(id) || ids.includes(LG.counterpartId(id, P)), theme + ': ' + id + ' is hidden');
    });
  }
});

test('N4 a style with both variants is listed as the current theme\'s only', () => {
  const dark = listed('dark').ids, light = listed('light').ids;
  assert.ok(dark.includes('osm-dark') && !dark.includes('osm-standard'), 'dark: ' + dark);
  assert.ok(light.includes('osm-standard') && !light.includes('osm-dark'), 'light: ' + light);
});

test('N5 the model: themeBaseIds lists a no-counterpart style in the other theme without a pick', () => {
  const { P, LG } = listed('light');
  const dark = LG.themeBaseIds(P, true);
  assert.ok(dark.denmark.includes('dataforsyningen-natur-friluft') && dark.world.includes('opentopomap'), JSON.stringify(dark));
  const light = LG.themeBaseIds(P, false);
  assert.ok(light.world.includes('esri-darkgray-labels'), 'a dark-only map is hidden in light: ' + JSON.stringify(light));
});

console.log(`\n${passed} passed, ${failed} failed`);
process.exit(failed ? 1 : 0);
