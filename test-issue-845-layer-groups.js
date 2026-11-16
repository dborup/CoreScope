/* test-issue-845-layer-groups.js — #845: the Map layer control groups the
 * Danish maps and layers under a collapsible "Denmark" section and lists
 * only the current theme's variants; CARTO is off by default and a saved
 * CARTO pick resolves to its nearest keyless map.
 *
 *   G  Grouping: the Danish base maps (Dataforsyningen, Aerial) go to the
 *      Denmark group, everything else stays in the base list.
 *   T  Theme filtering: only one theme's variants are listed; an explicit
 *      pick with no counterpart stays listed; the control's base maps
 *      follow the theme and keep the pick (mapped to its counterpart).
 *   V  Variant mapping: counterpartId / pickForTheme.
 *   R  Row text: "(Denmark)" and "DHM" dropped, the zoom hint apart.
 *   O  Open default: a saved state wins, else the centre over Denmark.
 *   C  CARTO: off by default (code), on only with enabled: true; every old
 *      id migrated; the customizer lists no CARTO style while it is off;
 *      config.example.json ships it off.
 *
 * vm sandbox + Leaflet stub (tests/map-overlay-sandbox.js); the real files
 * run. The real control in a browser: test-issue-845-layer-groups-e2e.js.
 *
 * Runs via: node test-issue-845-layer-groups.js
 */
'use strict';
const fs = require('fs');
const path = require('path');
const assert = require('assert');
const { makeSandbox, makeMap } = require('./tests/map-overlay-sandbox');
const { loadCustomizer } = require('./tests/customizer-sandbox');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const TOKEN = 'e2e-845-dummy';
const DF = { enabled: true, token: TOKEN };
function cfg(providers) { return { tiles: { providers } }; }
const ALL = cfg({ osm: { enabled: true }, esri: { enabled: true }, opentopomap: { enabled: true }, dataforsyningen: DF, carto: { enabled: true } });
const NO_CARTO = cfg({ osm: { enabled: true }, esri: { enabled: true }, opentopomap: { enabled: true }, dataforsyningen: DF });
const CARTO_IDS = ['carto-dark', 'carto-light', 'carto-voyager', 'carto-voyager-dark', 'positron-dark'];

// The registry's control on a stub map, in `theme`; returns the base keys
// and a way to flip the theme as <html data-theme> does.
function control(opts) {
  let theme = opts.theme || 'light';
  let observer = null;
  const ctx = makeSandbox({ mapCfg: opts.mapCfg, storage: opts.storage, theme });
  ctx.document.documentElement = { getAttribute: () => theme };
  ctx.MutationObserver = function (fn) { observer = fn; this.observe = () => {}; this.disconnect = () => { observer = null; }; };
  if (opts.ortho) ctx.window.MC_DHM_CFG = { enabled: true, url: '/api/map/dhm', ortho: { url: '/api/map/dhm/ortho/{z}/{x}/{y}', minZoom: 10, maxZoom: 19, bounds: [[54, 8], [58, 15.5]], attribution: 'SDFI' } };
  const map = makeMap();
  ctx.window.MC_createLayerControl(map, { _auto: true }, 'topleft');
  const last = () => map._controls.filter(c => !c._removed).slice(-1)[0];
  return {
    ctx, map, last,
    base: () => Object.keys(last().base),
    setTheme(t) { theme = t; if (observer) observer([]); },
    pick(id) { map.fire('baselayerchange', { name: id, layer: last().base[id] }); },
  };
}

console.log('\n=== #845 layer control: Denmark group, theme filter, CARTO ===');

// ── G / T: the model ────────────────────────────────────────────────────────
const reg = makeSandbox({ mapCfg: ALL }).window;
const LG = reg.MC_LayerGroups;

test('G1 the model is exported', () => {
  assert.ok(LG && typeof LG.themeBaseIds === 'function');
});

test('G2 light: Danish base maps in the Denmark group, the rest in the base list', () => {
  const r = LG.themeBaseIds(reg.MC_TILE_PROVIDERS, false, null);
  assert.deepStrictEqual(Array.from(r.denmark), ['dataforsyningen-skaermkort', 'dataforsyningen-skaermkort-graa',
    'dataforsyningen-skaermkort-daempet', 'dataforsyningen-natur-friluft']);
  assert.ok(r.world.includes('osm-standard') && r.world.includes('esri-imagery') && r.world.includes('opentopomap'));
  assert.ok(!r.world.some(id => id.indexOf('dataforsyningen') === 0), 'Danish map in the base list: ' + r.world);
});

test('G3 Aerial (dhm-ortho) belongs to the Denmark group', () => {
  assert.strictEqual(LG.isDenmarkBase('dhm-ortho', reg.MC_TILE_PROVIDERS), true);
  assert.strictEqual(LG.isDenmarkBase('osm-standard', reg.MC_TILE_PROVIDERS), false);
});

// #859 — a style of the other theme is listed only when it has no
// counterpart (a light-only map in dark mode); it is never hidden.
const onlyOwnOrSingle = (P, ids, type) => ids.forEach(id => assert.ok(P[id].type === type || !LG.counterpartId(id, P), id));
test('T1 light lists no dark style that has a light variant, dark no light style that has a dark one (#859)', () => {
  const P = reg.MC_TILE_PROVIDERS;
  const light = LG.themeBaseIds(P, false, null), dark = LG.themeBaseIds(P, true, null);
  onlyOwnOrSingle(P, light.world.concat(light.denmark), 'light');
  onlyOwnOrSingle(P, dark.world.concat(dark.denmark), 'dark');
  assert.deepStrictEqual(Array.from(dark.denmark), ['dataforsyningen-skaermkort', 'dataforsyningen-skaermkort-daempet',
    'dataforsyningen-natur-friluft', 'dataforsyningen-skaermkort-graa-dark']);
  assert.ok(dark.world.includes('osm-dark') && dark.world.includes('esri-darkgray-labels') && dark.world.includes('esri-imagery-dark'));
});

test('T2 an explicit pick with no counterpart in the theme stays listed; one with a counterpart does not', () => {
  const P = reg.MC_TILE_PROVIDERS;
  assert.ok(LG.themeBaseIds(P, true, 'dataforsyningen-natur-friluft').denmark.includes('dataforsyningen-natur-friluft'));
  assert.ok(LG.themeBaseIds(P, true, 'opentopomap').world.includes('opentopomap'));
  assert.ok(!LG.themeBaseIds(P, true, 'osm-standard').world.includes('osm-standard'));
});

// ── V: variant mapping ──────────────────────────────────────────────────────
test('V1 counterparts pair by provider and label, both ways', () => {
  const P = reg.MC_TILE_PROVIDERS;
  const pairs = [['osm-standard', 'osm-dark'], ['esri-imagery', 'esri-imagery-dark'],
    ['dataforsyningen-skaermkort-graa', 'dataforsyningen-skaermkort-graa-dark'],
    ['carto-light', 'positron-dark'], ['carto-voyager', 'carto-voyager-dark']];
  for (const [l, d] of pairs) {
    assert.strictEqual(LG.counterpartId(l, P), d, l);
    assert.strictEqual(LG.counterpartId(d, P), l, d);
  }
  for (const id of ['opentopomap', 'dataforsyningen-natur-friluft', 'esri-darkgray-labels', 'carto-dark']) {
    assert.strictEqual(LG.counterpartId(id, P), null, id);
  }
});

test('V2 pickForTheme: to the counterpart, or kept when there is none', () => {
  const P = reg.MC_TILE_PROVIDERS;
  assert.strictEqual(LG.pickForTheme('osm-standard', true, P), 'osm-dark');
  assert.strictEqual(LG.pickForTheme('osm-dark', false, P), 'osm-standard');
  assert.strictEqual(LG.pickForTheme('osm-dark', true, P), 'osm-dark');
  assert.strictEqual(LG.pickForTheme('dataforsyningen-natur-friluft', true, P), 'dataforsyningen-natur-friluft');
});

// ── R: row text ─────────────────────────────────────────────────────────────
test('R1 "(Denmark)" and "DHM" are dropped; the zoom hint is apart', () => {
  const cases = [
    ['Skærmkort grå (Denmark)', 'Skærmkort grå', ''],
    ['Contour lines (DHM, zoom 14+)', 'Contour lines', 'zoom 14+'],
    ['Hillshade: terrain (DHM)', 'Hillshade: terrain', ''],
    ['Protected ancient monuments (zoom 12+)', 'Protected ancient monuments', 'zoom 12+'],
    ['Antenna sites (mast, tower or rooftop)', 'Antenna sites (mast, tower or rooftop)', ''],
    ['Aerial photo', 'Aerial photo', ''],
  ];
  for (const [text, name, hint] of cases) {
    const r = LG.rowParts(text);
    assert.deepStrictEqual({ name: r.name, hint: r.hint }, { name, hint }, text);
  }
});

// ── O: open default ─────────────────────────────────────────────────────────
test('O1 a saved state wins; else open over Denmark only', () => {
  const cph = { lat: 55.68, lng: 12.57 }, berlin = { lat: 52.52, lng: 13.4 }, bornholm = { lat: 55.1, lng: 14.9 };
  assert.strictEqual(LG.openDefault('1', berlin), true);
  assert.strictEqual(LG.openDefault('0', cph), false);
  assert.strictEqual(LG.openDefault(null, cph), true);
  assert.strictEqual(LG.openDefault(null, bornholm), true);
  assert.strictEqual(LG.openDefault(null, berlin), false);
  assert.strictEqual(LG.openDefault(null, null), false);
});

// ── T: the control ──────────────────────────────────────────────────────────
test('T3 the control lists the light variants in light (#859: and the dark-only maps), Danish ones included', () => {
  const c = control({ mapCfg: ALL, theme: 'light' });
  const P = c.ctx.window.MC_TILE_PROVIDERS;
  onlyOwnOrSingle(P, c.base().filter(id => id !== '__auto__'), 'light');
  assert.ok(c.base().includes('dataforsyningen-skaermkort'));
  assert.strictEqual(c.base()[0], '__auto__');
});

test('T4 the control lists the dark variants in dark (#859: and the light-only maps)', () => {
  const c = control({ mapCfg: ALL, theme: 'dark' });
  const P = c.ctx.window.MC_TILE_PROVIDERS;
  onlyOwnOrSingle(P, c.base().filter(id => id !== '__auto__'), 'dark');
  assert.ok(!c.base().includes('osm-standard') && c.base().includes('dataforsyningen-natur-friluft'));
});

test('T5 a theme switch rebuilds the list and moves the pick to its counterpart, on the map', () => {
  const c = control({ mapCfg: ALL, theme: 'light' });
  c.pick('osm-standard');
  const before = c.last();
  c.setTheme('dark');
  assert.notStrictEqual(c.last(), before, 'not rebuilt');
  assert.ok(c.base().includes('osm-dark') && !c.base().includes('osm-standard'));
  assert.ok(c.map.hasLayer(c.last().base['osm-dark']), 'the dark counterpart is not on the map');
  c.setTheme('light');
  assert.ok(c.map.hasLayer(c.last().base['osm-standard']), 'not back to the light variant');
});

test('T6 a pick with no counterpart survives a theme switch, still listed and on the map', () => {
  const c = control({ mapCfg: ALL, theme: 'light' });
  c.pick('dataforsyningen-natur-friluft');
  c.setTheme('dark');
  assert.ok(c.base().includes('dataforsyningen-natur-friluft'));
  assert.ok(c.map.hasLayer(c.last().base['dataforsyningen-natur-friluft']));
});

test('T7 no rebuild when the theme attribute changes but the theme does not', () => {
  const c = control({ mapCfg: ALL, theme: 'light' });
  const before = c.last();
  c.setTheme('light');
  assert.strictEqual(c.last(), before);
});

test('T8 Aerial stays offered in both themes', () => {
  for (const theme of ['light', 'dark']) {
    const c = control({ mapCfg: ALL, theme, ortho: true });
    assert.ok(c.base().includes('dhm-ortho'), theme);
  }
});

test('T9 no "(dark)" suffix helper is left (#839 F5 superseded)', () => {
  assert.strictEqual(reg.MC_baseLayerLabel, undefined);
  const src = fs.readFileSync(path.join(__dirname, 'public', 'map-tile-providers.js'), 'utf8');
  assert.ok(!/' \(dark\)'/.test(src), 'a " (dark)" label suffix is still built');
});

// ── C: CARTO ────────────────────────────────────────────────────────────────
test('C1 CARTO is off by default: no config, a providers block without carto, or enabled not true', () => {
  const off = [undefined, cfg({ osm: { enabled: true } }), cfg({ carto: {} }), cfg({ carto: { enabled: 'true' } }), cfg({ carto: { enabled: 1 } })];
  off.forEach((m, i) => {
    const P = makeSandbox({ mapCfg: m }).window.MC_TILE_PROVIDERS;
    CARTO_IDS.forEach(id => assert.ok(!(id in P), 'case ' + i + ': ' + id));
  });
});

test('C2 CARTO on with enabled: true', () => {
  const P = makeSandbox({ mapCfg: cfg({ carto: { enabled: true } }) }).window.MC_TILE_PROVIDERS;
  CARTO_IDS.forEach(id => assert.ok(id in P, id));
});

test('C3 config.example.json ships carto.enabled false, with the reason', () => {
  const ex = JSON.parse(fs.readFileSync(path.join(__dirname, 'config.example.json'), 'utf8'));
  assert.strictEqual(ex.map.tiles.providers.carto.enabled, false);
  assert.ok(/API KEY REQUIRED/.test(ex.map.tiles.providers._comment_carto));
});

const MIGRATE = [
  // [stored key, old id, esri on?, expected]
  ['mc-light-tile-provider', 'carto-light', true, 'osm-standard'],
  ['mc-light-tile-provider', 'carto-voyager', true, 'osm-standard'],
  ['mc-dark-tile-provider', 'positron-dark', true, 'osm-dark'],
  ['mc-dark-tile-provider', 'carto-voyager-dark', true, 'osm-dark'],
  ['mc-dark-tile-provider', 'carto-dark', true, 'esri-darkgray-labels'],
  ['mc-dark-tile-provider', 'carto-dark', false, 'osm-dark'],
];
for (const [key, id, esri, want] of MIGRATE) {
  test('C4 a saved ' + id + ' (esri ' + (esri ? 'on' : 'off') + ') resolves to ' + want + ', with a tile URL', () => {
    const m = cfg(esri ? { esri: { enabled: true } } : {});
    const w = makeSandbox({ mapCfg: m, storage: { [key]: id } }).window;
    const dark = key === 'mc-dark-tile-provider';
    assert.strictEqual(dark ? w.MC_getDarkTileProvider() : w.MC_getLightTileProvider(), want);
    const spec = w.MC_getTileSpec(dark ? 'dark' : 'light');
    assert.strictEqual(spec.id, want);
    assert.ok(spec.url && spec.url.indexOf('cartocdn') < 0, 'blank or CARTO url: ' + spec.url);
    assert.strictEqual(w.localStorage.getItem(key), id, 'the saved value was rewritten');
  });
}

test('C5 an operator darkDefault/lightDefault naming CARTO is migrated too', () => {
  const w = makeSandbox({ mapCfg: { tiles: { darkDefault: 'carto-dark', lightDefault: 'carto-voyager', providers: { esri: { enabled: true } } } } }).window;
  assert.strictEqual(w.MC_getDarkTileProvider(), 'esri-darkgray-labels');
  assert.strictEqual(w.MC_getLightTileProvider(), 'osm-standard');
});

test('C6 with CARTO on, a saved CARTO pick is kept as it is', () => {
  const w = makeSandbox({ mapCfg: cfg({ carto: { enabled: true } }), storage: { 'mc-dark-tile-provider': 'carto-dark' } }).window;
  assert.strictEqual(w.MC_getDarkTileProvider(), 'carto-dark');
  assert.strictEqual(w.MC_migrateTileId('carto-dark'), 'carto-dark');
});

// The customizer's base-map lists, wired to the registry as the page does.
function customizerLists(mapCfg, storage) {
  const r = makeSandbox({ mapCfg, storage });
  const cust = loadCustomizer({
    setup(ctx) {
      ['MC_TILE_PROVIDERS', 'MC_DARK_TILE_DEFAULT', 'MC_LIGHT_TILE_DEFAULT', 'MC_getDarkTileProvider', 'MC_getLightTileProvider']
        .forEach((k) => { ctx.window[k] = r.window[k]; });
      const make = ctx.document.createElement;
      ctx.document.createElement = (tag) => {
        const el = make(tag);
        let text = '';
        Object.defineProperty(el, 'textContent', { get: () => text, set: (v) => { text = String(v); } });
        Object.defineProperty(el, 'innerHTML', { get: () => text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;') });
        return el;
      };
    },
  });
  const html = cust.api.renderTileProviderSelector();
  const sel = (id) => {
    const m = html.match(new RegExp('<select id="' + id + '"[^>]*>([\\s\\S]*?)</select>'));
    const opts = []; let selected = null;
    m[1].replace(/<option value="([^"]*)"( selected)?>/g, (_, v, s) => { opts.push(v); if (s) selected = v; return ''; });
    return { opts, selected };
  };
  return { html, light: sel('cv2-light-tile-provider'), dark: sel('cv2-dark-tile-provider') };
}

test('C7 the customizer lists no CARTO style while CARTO is off', () => {
  const r = customizerLists(NO_CARTO);
  assert.ok(!r.light.opts.concat(r.dark.opts).some(id => CARTO_IDS.includes(id)), r.light.opts + ' / ' + r.dark.opts);
  assert.ok(!/Carto/.test(r.html));
});

test('C8 the customizer preselects the migrated map for a saved CARTO pick', () => {
  const r = customizerLists(NO_CARTO, { 'mc-dark-tile-provider': 'carto-dark', 'mc-light-tile-provider': 'carto-light' });
  assert.strictEqual(r.dark.selected, 'esri-darkgray-labels');
  assert.strictEqual(r.light.selected, 'osm-standard');
});

test('C9 the customizer lists CARTO when the operator turns it on', () => {
  const r = customizerLists(ALL);
  assert.ok(r.light.opts.includes('carto-light') && r.dark.opts.includes('carto-dark'));
});

console.log('\n' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
