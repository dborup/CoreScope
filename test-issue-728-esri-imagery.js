/* test-issue-728-esri-imagery.js — #728 M1.
 *
 * Esri World Imagery as a base style, and the opt-in gate for every Esri
 * style (owner decision (b) on #728): map.tiles.providers.esri.enabled.
 *
 *   E1  esri-imagery / esri-imagery-dark: exact URL ({z}/{y}/{x}), exact
 *       credit, maxZoom 19, no invert filter, one per theme.
 *   E2  Gate: no config, enabled:false, a missing flag, the string "true",
 *       and the shipped config.example.json offer NO esri style (the Dark
 *       Gray canvas included); enabled:true offers all three; enabling Esri
 *       does not pull in another opt-in provider.
 *   E3  A darkDefault/lightDefault naming an Esri style while Esri is off
 *       resolves to the existing fallback (legacy URL, else osm-dark /
 *       osm-standard), and warns once, naming the switch.
 *   E4  Per-viewer choice: MC_set*TileProvider persists an Esri style only
 *       while Esri is on; a stored Esri choice is ignored while it is off.
 *   E5  Dark theme with the imagery selected: the tile pane gets no filter.
 *
 * Runs via: node test-issue-728-esri-imagery.js  (pure vm sandbox)
 */
'use strict';
const vm     = require('vm');
const fs     = require('fs');
const path   = require('path');
const assert = require('assert');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const URL_IMAGERY = 'https://server.arcgisonline.com/ArcGIS/rest/services/World_Imagery/MapServer/tile/{z}/{y}/{x}';
const CREDIT = 'Tiles © Esri — Source: Esri, Vantor, Earthstar Geographics, and the GIS User Community';
const ESRI_IDS = ['esri-darkgray-labels', 'esri-imagery', 'esri-imagery-dark'];
const SRC = fs.readFileSync(path.resolve(__dirname, process.env.PROVIDERS_FILE || 'public/map-tile-providers.js'), 'utf8');

function makeStorage(init) {
  const store = Object.assign({}, init || {});
  return {
    getItem(k)    { return Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null; },
    setItem(k, v) { store[k] = String(v); },
    removeItem(k) { delete store[k]; },
  };
}

// opts: theme, mapCfg (MC_MAP_CFG at parse time), storage, legacyDark/legacyLight
function load(opts) {
  opts = opts || {};
  const warnings = [];
  const attrs = {};
  const tilePane = {
    style: { filter: '' },
    setAttribute: (k, v) => { attrs[k] = String(v); },
    getAttribute: (k) => (k in attrs ? attrs[k] : null),
    removeAttribute: (k) => { delete attrs[k]; },
  };
  const ctx = {
    console: { log() {}, error() {}, warn: (m) => warnings.push(String(m)) },
    JSON, Object, Array, String, Number, Boolean, Math,
    localStorage: makeStorage(opts.storage),
    document: {
      documentElement: { getAttribute: () => opts.theme || 'light' },
      querySelector: (sel) => (sel === '.leaflet-tile-pane' ? tilePane : null),
    },
    window: {
      addEventListener() {},
      dispatchEvent() { return true; },
      matchMedia: () => ({ matches: false }),
    },
    CustomEvent: function (type, init) { this.type = type; this.detail = init && init.detail; },
  };
  ctx.window.localStorage = ctx.localStorage;
  ctx.window.document = ctx.document;
  if (opts.mapCfg !== undefined) ctx.window.MC_MAP_CFG = opts.mapCfg;
  if (opts.legacyDark) ctx.window.MC_DARK_TILE_URL_OVERRIDE = opts.legacyDark;
  if (opts.legacyLight) ctx.window.MC_LIGHT_TILE_URL_OVERRIDE = opts.legacyLight;
  vm.createContext(ctx);
  vm.runInContext(SRC, ctx);
  ctx.warnings = warnings;
  ctx.tilePane = tilePane;
  return ctx;
}

const tiles = (providers, extra) => ({ tiles: Object.assign({ providers: providers || {} }, extra || {}) });
const ESRI_ON = { esri: { enabled: true } };
const urlOf = (p) => (typeof p.url === 'function' ? p.url() : p.url);

console.log('── #728 E1: Esri World Imagery entries ──');

test('E1 esri-imagery is a light style and esri-imagery-dark a dark one', () => {
  const reg = load({ mapCfg: tiles(ESRI_ON) }).window.MC_TILE_PROVIDERS;
  assert.strictEqual(reg['esri-imagery'].type, 'light');
  assert.strictEqual(reg['esri-imagery-dark'].type, 'dark');
  for (const id of ['esri-imagery', 'esri-imagery-dark']) {
    assert.strictEqual(reg[id].provider, 'esri', id + ' provider');
    assert.strictEqual(reg[id].label, 'Esri World Imagery', id + ' label');
  }
});

test('E1 tile URL is the World_Imagery MapServer in {z}/{y}/{x} order, over TLS, keyless', () => {
  const reg = load({ mapCfg: tiles(ESRI_ON) }).window.MC_TILE_PROVIDERS;
  for (const id of ['esri-imagery', 'esri-imagery-dark']) assert.strictEqual(urlOf(reg[id]), URL_IMAGERY, id);
});

test('E1 credit is the service copyrightText with the Esri tile credit, exactly', () => {
  const reg = load({ mapCfg: tiles(ESRI_ON) }).window.MC_TILE_PROVIDERS;
  for (const id of ['esri-imagery', 'esri-imagery-dark']) assert.strictEqual(reg[id].attribution, CREDIT, id);
});

test('E1 maxZoom 19 and no invert filter on either theme entry (it is a photo)', () => {
  const reg = load({ mapCfg: tiles(ESRI_ON) }).window.MC_TILE_PROVIDERS;
  for (const id of ['esri-imagery', 'esri-imagery-dark']) {
    assert.strictEqual(reg[id].maxZoom, 19, id + ' maxZoom');
    assert.strictEqual(reg[id].invertFilter, null, id + ' invertFilter');
  }
});

test('E1 MC_getTileSpec("dark") for the imagery: same URL, credit, z19, no invert', () => {
  const ctx = load({ theme: 'dark', mapCfg: tiles(ESRI_ON, { darkDefault: 'esri-imagery-dark' }) });
  const spec = ctx.window.MC_getTileSpec('dark');
  assert.deepStrictEqual(
    { id: spec.id, url: spec.url, attribution: spec.attribution, maxZoom: spec.maxZoom, invertFilter: spec.invertFilter },
    { id: 'esri-imagery-dark', url: URL_IMAGERY, attribution: CREDIT, maxZoom: 19, invertFilter: null });
});

console.log('\n── #728 E2: the Esri opt-in gate ──');

const OFF_CASES = {
  'no MC_MAP_CFG': undefined,
  'no esri block': tiles({}),
  'esri block without enabled': tiles({ esri: {} }),
  'enabled:false': tiles({ esri: { enabled: false } }),
  'enabled:"true" (string)': tiles({ esri: { enabled: 'true' } }),
  'enabled:1': tiles({ esri: { enabled: 1 } }),
};
for (const [label, cfg] of Object.entries(OFF_CASES)) {
  test('E2 ' + label + ' → no Esri style is offered, Dark Gray included', () => {
    const reg = load({ mapCfg: cfg }).window.MC_TILE_PROVIDERS;
    for (const id of ESRI_IDS) assert.ok(!reg[id], id + ' must not be offered');
  });
}

test('E2 the shipped config.example.json keeps Esri off', () => {
  const t = JSON.parse(fs.readFileSync(path.join(__dirname, 'config.example.json'), 'utf8')).map.tiles;
  assert.strictEqual(t.providers.esri && t.providers.esri.enabled, false, 'config.example.json must document esri.enabled: false');
  const reg = load({ mapCfg: { tiles: t } }).window.MC_TILE_PROVIDERS;
  for (const id of ESRI_IDS) assert.ok(!reg[id], id + ' must ship disabled');
});

test('E2 enabled:true offers all three Esri styles', () => {
  const reg = load({ mapCfg: tiles(ESRI_ON) }).window.MC_TILE_PROVIDERS;
  for (const id of ESRI_IDS) assert.ok(reg[id], id + ' must be offered');
});

test('E2 enabling Esri does not pull in the other opt-in providers', () => {
  const reg = load({ mapCfg: tiles(ESRI_ON) }).window.MC_TILE_PROVIDERS;
  for (const id of ['opentopomap', 'usgs-topo', 'usgs-imagery', 'stamen-toner-lite']) assert.ok(!reg[id], id + ' must stay absent');
});

test('E2 the gate follows config that lands after parse time (async MC_initTileRegistry)', () => {
  const ctx = load({ mapCfg: undefined });
  assert.ok(!ctx.window.MC_TILE_PROVIDERS['esri-imagery']);
  ctx.window.MC_MAP_CFG = tiles(ESRI_ON);
  ctx.window.MC_initTileRegistry(true);
  for (const id of ESRI_IDS) assert.ok(ctx.window.MC_TILE_PROVIDERS[id], id + ' after async enable');
  ctx.window.MC_MAP_CFG = tiles({ esri: { enabled: false } });
  ctx.window.MC_initTileRegistry(true);
  for (const id of ESRI_IDS) assert.ok(!ctx.window.MC_TILE_PROVIDERS[id], id + ' after async disable');
});

console.log('\n── #728 E3: an Esri default while Esri is off falls back ──');

test('E3 darkDefault esri-darkgray-labels, Esri off → osm-dark (the built-in fallback)', () => {
  const ctx = load({ theme: 'dark', mapCfg: tiles({}, { darkDefault: 'esri-darkgray-labels' }) });
  assert.strictEqual(ctx.window.MC_getDarkTileProvider(), 'osm-dark');
  const spec = ctx.window.MC_getTileSpec('dark');
  assert.strictEqual(spec.id, 'osm-dark');
  assert.ok(/tile\.openstreetmap\.org/.test(spec.url), 'fallback url ' + spec.url);
  assert.ok(!/arcgisonline/.test(spec.url));
});

test('E3 lightDefault esri-imagery, Esri off → osm-standard', () => {
  const ctx = load({ mapCfg: tiles({ esri: { enabled: false } }, { lightDefault: 'esri-imagery', darkDefault: 'esri-imagery-dark' }) });
  assert.strictEqual(ctx.window.MC_getLightTileProvider(), 'osm-standard');
  assert.strictEqual(ctx.window.MC_getDarkTileProvider(), 'osm-dark');
  assert.strictEqual(ctx.window.MC_getTileSpec('light').id, 'osm-standard');
});

test('E3 a legacy darkUrl still applies when the Esri default is skipped (#364/#390 precedence)', () => {
  const legacy = 'https://tiles.example/dark/{z}/{x}/{y}.png';
  const ctx = load({ theme: 'dark', legacyDark: legacy, mapCfg: tiles({}, { darkDefault: 'esri-darkgray-labels' }) });
  const spec = ctx.window.MC_getTileSpec('dark');
  assert.strictEqual(spec.id, null);
  assert.strictEqual(spec.url, legacy);
});

test('E3 the same default resolves to Esri once Esri is on', () => {
  const ctx = load({ mapCfg: tiles(ESRI_ON, { darkDefault: 'esri-darkgray-labels', lightDefault: 'esri-imagery' }) });
  assert.strictEqual(ctx.window.MC_getDarkTileProvider(), 'esri-darkgray-labels');
  assert.strictEqual(ctx.window.MC_getLightTileProvider(), 'esri-imagery');
  assert.strictEqual(ctx.window.MC_getTileSpec('light').url, URL_IMAGERY);
});

test('E3 a skipped Esri default warns once per key, naming the id and the switch', () => {
  const ctx = load({ mapCfg: tiles({}, { darkDefault: 'esri-darkgray-labels', lightDefault: 'esri-imagery' }) });
  ctx.window.MC_initTileRegistry(true); // a second init (config re-applied) must not repeat it
  assert.strictEqual(ctx.warnings.length, 2, 'warnings: ' + JSON.stringify(ctx.warnings));
  assert.ok(/darkDefault "esri-darkgray-labels"/.test(ctx.warnings[0]), ctx.warnings[0]);
  assert.ok(/lightDefault "esri-imagery"/.test(ctx.warnings[1]), ctx.warnings[1]);
  for (const w of ctx.warnings) assert.ok(w.includes('map.tiles.providers.esri.enabled: true'), w);
});

test('E3 no warning when the default is available, or none is set', () => {
  assert.deepStrictEqual(load({ mapCfg: tiles(ESRI_ON, { darkDefault: 'esri-darkgray-labels' }) }).warnings, []);
  assert.deepStrictEqual(load({ mapCfg: tiles({}) }).warnings, []);
  assert.deepStrictEqual(load({ mapCfg: undefined }).warnings, []);
});

test('E3 the warning also covers the other opt-in providers (generic)', () => {
  const w = load({ mapCfg: tiles({}, { lightDefault: 'opentopomap' }) }).warnings;
  assert.strictEqual(w.length, 1);
  assert.ok(w[0].includes('map.tiles.providers.opentopomap.enabled: true'), w[0]);
});

console.log('\n── #728 E4: per-viewer choice ──');

test('E4 MC_setLightTileProvider("esri-imagery") persists while Esri is on', () => {
  const ctx = load({ mapCfg: tiles(ESRI_ON) });
  assert.strictEqual(ctx.window.MC_setLightTileProvider('esri-imagery'), true);
  assert.strictEqual(ctx.localStorage.getItem('mc-light-tile-provider'), 'esri-imagery');
  assert.strictEqual(ctx.window.MC_getLightTileProvider(), 'esri-imagery');
  assert.strictEqual(ctx.window.MC_setDarkTileProvider('esri-imagery-dark'), true);
  assert.strictEqual(ctx.localStorage.getItem('mc-dark-tile-provider'), 'esri-imagery-dark');
});

test('E4 the light/dark ids are not interchangeable', () => {
  const ctx = load({ mapCfg: tiles(ESRI_ON) });
  assert.strictEqual(ctx.window.MC_setLightTileProvider('esri-imagery-dark'), false);
  assert.strictEqual(ctx.window.MC_setDarkTileProvider('esri-imagery'), false);
});

test('E4 a stored choice survives a reload (new page, same storage)', () => {
  const ctx = load({ mapCfg: tiles(ESRI_ON), storage: { 'mc-light-tile-provider': 'esri-imagery', 'mc-dark-tile-provider': 'esri-imagery-dark' } });
  assert.strictEqual(ctx.window.MC_getLightTileProvider(), 'esri-imagery');
  assert.strictEqual(ctx.window.MC_getDarkTileProvider(), 'esri-imagery-dark');
});

test('E4 with Esri off, setting it is refused and a stored choice is ignored', () => {
  const ctx = load({ mapCfg: tiles({}), storage: { 'mc-light-tile-provider': 'esri-imagery', 'mc-dark-tile-provider': 'esri-darkgray-labels' } });
  assert.strictEqual(ctx.window.MC_getLightTileProvider(), 'osm-standard');
  assert.strictEqual(ctx.window.MC_getDarkTileProvider(), 'osm-dark');
  assert.strictEqual(ctx.window.MC_setLightTileProvider('esri-imagery'), false);
});

console.log('\n── #728 E5: dark theme filter ──');

test('E5 dark theme + esri-imagery-dark → the tile pane has no filter', () => {
  const ctx = load({ theme: 'dark', mapCfg: tiles(ESRI_ON) });
  ctx.tilePane.style.filter = 'invert(1)'; // left over from an inverted style
  ctx.window.MC_setDarkTileProvider('esri-imagery-dark');
  ctx.window.MC_applyTileFilter();
  assert.strictEqual(ctx.tilePane.style.filter, '');
});

console.log(`\n#728 esri imagery: ${passed} passed, ${failed} failed`);
if (failed) process.exit(1);
