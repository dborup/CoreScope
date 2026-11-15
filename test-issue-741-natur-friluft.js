/* test-issue-741-natur-friluft.js — #741 M2: Natur- og Friluftskort as a
 * Dataforsyningen base map, through WMS in EPSG:3857.
 *
 * The owner's token checks (#741, "Owner token checks §8: results") found
 * that wms/natur_friluftskort lists EPSG:3857 and answers a 3857 GetMap of
 * Natur_Friluft_basis with a PNG, about 1.0 s cold. The WMTS is 25832
 * only. M2 adds one style next to the M1b Skærmkort styles, on the same
 * gate and the same client-visible base-map token; the server side
 * (cmd/server, #805) is keyed on the provider and needs nothing new.
 *
 * Contract under test:
 *   G   The gate: the style exists exactly when the Skærmkort styles do,
 *       map.tiles.providers.dataforsyningen.enabled === true and a
 *       non-blank string token. Off by default and for every near miss.
 *   S   The style: light only (no dark entry, no invert filter), one layer,
 *       Natur_Friluft_basis, and never Vejdirektoratet (its terms are
 *       unread) or the name layers.
 *   W   The factory output: an L.tileLayer.wms at wms/natur_friluftskort
 *       with the token on the base URL and exactly the Skærmkort options
 *       (512 px, zoomOffset -1, zoom 6-19, Denmark bounds) except the
 *       layer, from the layer control and from getTileSpec().
 *   K   No extra tile churn: nothing beyond the Skærmkort options reaches
 *       Leaflet, so keepBuffer and the update* settings stay Leaflet's.
 *   T   The token: the Skærmkort token, read from the same key, and no
 *       other key; rotating it moves both.
 *   C   The credit: "Indeholder data fra Klimadatastyrelsen, Natur- og
 *       Friluftskort, CC BY 4.0" with the dataset and licence links, built
 *       by the shared walker; the Skærmkort credit is unchanged.
 *   D   A default naming the style while it is gated off is skipped and
 *       warned about; as a dark default it is never shown (light only).
 *   P   A layer-control pick of it survives a theme switch (no filter in
 *       dark), a control rebuild and SPA navigation.
 *
 * No test touches api.dataforsyningen.dk and no real token is used: the
 * vm sandbox records the layers it is asked to build and fetches nothing.
 *
 * Runs via: node test-issue-741-natur-friluft.js
 */
'use strict';
const vm = require('vm');
const assert = require('assert');
const { makeSandbox, buildControl } = require('./tests/map-overlay-sandbox');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

// Host-realm copies, so deepStrictEqual does not trip over vm prototypes.
function plain(v) {
  if (Array.isArray(v)) return Array.from(v, plain);
  if (v && typeof v === 'object') {
    const o = {};
    for (const k of Object.keys(v)) o[k] = plain(v[k]);
    return o;
  }
  return v;
}

const TOKEN = 'e2e-741-dummy-token';
const ID = 'dataforsyningen-natur-friluft';
const SK = 'dataforsyningen-skaermkort';
const SERVICE = 'https://api.dataforsyningen.dk/wms/natur_friluftskort';
const SK_SERVICE = 'https://api.dataforsyningen.dk/topo_skaermkort_DAF';
const LAYER = 'Natur_Friluft_basis';
const ON = (extra) => ({ tiles: Object.assign({ providers: { dataforsyningen: { enabled: true, token: TOKEN } } }, extra || {}) });
const BOUNDS = [[53.015, 2.47842], [58.6403, 17.5578]];
const CREDIT = 'Indeholder data fra Klimadatastyrelsen, Natur- og Friluftskort, CC BY 4.0';
const DATASET_URL = 'https://dataforsyningen.dk/data/2683';
const LICENSE_URL = 'https://creativecommons.org/licenses/by/4.0/';
const SK_CREDIT = 'Indeholder data fra Klimadatastyrelsen og GeoDanmark, Skærmkort, CC BY 4.0';

const registry = (mapCfg) => makeSandbox({ mapCfg }).window.MC_TILE_PROVIDERS;

// ── G: the gate ───────────────────────────────────────────────────────────
console.log('\n── #741 M2 G: the Skærmkort gate, unchanged ──');

test('G off by default: no config, no style', () => {
  for (const cfg of [undefined, { tiles: {} }, { tiles: { providers: {} } }]) {
    assert.ok(!(ID in registry(cfg)), JSON.stringify(cfg));
  }
});

test('G enabled with a token offers it', () => {
  const reg = registry(ON());
  assert.ok(reg[ID], 'not offered: ' + Object.keys(reg).join(','));
});

/* The same gate, not a copy of it: across every config shape the M1b suite
 * pins, Natur is offered exactly when Skærmkort is. */
const SHAPES = {
  'on': { enabled: true, token: TOKEN },
  'enabled false': { enabled: false, token: TOKEN },
  'no enabled': { token: TOKEN },
  'enabled "true" (a string)': { enabled: 'true', token: TOKEN },
  'enabled 1': { enabled: 1, token: TOKEN },
  'no token': { enabled: true },
  'empty token': { enabled: true, token: '' },
  'blank token': { enabled: true, token: '   ' },
  'numeric token': { enabled: true, token: 42 },
  'token object': { enabled: true, token: { value: TOKEN } },
  'Enabled (wrong case)': { Enabled: true, token: TOKEN },
  'Token (wrong case)': { enabled: true, Token: TOKEN },
  'not an object': 'x',
  'null': null,
  'an array': [TOKEN],
};
for (const name of Object.keys(SHAPES)) {
  test('G ' + name + ': offered exactly when Skærmkort is', () => {
    const reg = registry({ tiles: { providers: { dataforsyningen: SHAPES[name] } } });
    assert.strictEqual(ID in reg, SK in reg, 'Natur ' + (ID in reg) + ', Skærmkort ' + (SK in reg));
    assert.strictEqual(ID in reg, name === 'on');
  });
}

test('G a wrong-case or separate provider key does not switch it on', () => {
  for (const k of ['Dataforsyningen', 'DATAFORSYNINGEN', 'natur_friluftskort', 'naturFriluft']) {
    const providers = {}; providers[k] = { enabled: true, token: TOKEN };
    assert.ok(!(ID in registry({ tiles: { providers } })), k);
  }
});

test('G the gate follows config that lands after load', () => {
  const ctx = makeSandbox({ mapCfg: { tiles: {} } });
  assert.ok(!ctx.window.MC_TILE_PROVIDERS[ID]);
  ctx.window.MC_MAP_CFG = ON();
  ctx.window.MC_initTileRegistry(true);
  assert.ok(ctx.window.MC_TILE_PROVIDERS[ID], 'on once the config arrives');
  ctx.window.MC_MAP_CFG = { tiles: { providers: { dataforsyningen: { enabled: false, token: TOKEN } } } };
  ctx.window.MC_initTileRegistry(true);
  assert.ok(!ctx.window.MC_TILE_PROVIDERS[ID], 'and off again');
});

// ── S: the style ──────────────────────────────────────────────────────────
console.log('\n── #741 M2 S: light only, one layer ──');

test('S light, never inverted, of the dataforsyningen provider', () => {
  const s = registry(ON())[ID];
  assert.strictEqual(s.type, 'light');
  assert.strictEqual(s.invertFilter, null);
  assert.strictEqual(s.provider, 'dataforsyningen');
  assert.strictEqual(s.label, 'Natur- og Friluftskort (Denmark)');
});

test('S light only: no dark style requests the Natur service', () => {
  const reg = registry(ON());
  const natur = Object.keys(reg).filter(id => reg[id].wms && String(reg[id].wms.url()).indexOf(SERVICE) === 0);
  assert.deepStrictEqual(natur, [ID]);
  for (const id of Object.keys(reg)) {
    if (reg[id].type === 'dark') assert.ok(!reg[id].wms || reg[id].wms.layers !== LAYER, id);
  }
});

test('S exactly Natur_Friluft_basis: no Vejdirektoratet, no name layers', () => {
  const w = registry(ON())[ID].wms;
  assert.strictEqual(w.layers, LAYER);
  assert.ok(!/,/.test(w.layers), 'one layer');
  assert.ok(!/Vejdirektoratet|Stednavne|Vejnavne/i.test(w.layers), w.layers);
});

test('S no tile template, so nothing can build it as XYZ', () => {
  const s = registry(ON())[ID];
  assert.ok(!s.url && !s.baseUrl);
});

// ── W: what the factory builds ────────────────────────────────────────────
console.log('\n── #741 M2 W: the factory builds the Natur WMS layer ──');

function expectedOptions(ctx, id, layers) {
  return {
    attribution: ctx.window.MC_TILE_PROVIDERS[id].attribution,
    maxZoom: 19,
    layers,
    styles: '',
    format: 'image/png',
    transparent: false,
    version: '1.3.0',
    crs: ctx.L.CRS.EPSG3857,
    tileSize: 512,
    zoomOffset: -1,
    minZoom: 6,
    bounds: ctx.L.latLngBounds(BOUNDS),
  };
}

test('W the layer control builds it as L.tileLayer.wms with exactly these options', () => {
  const ctx = makeSandbox({ mapCfg: ON() });
  const { ctl } = buildControl(ctx, true);
  const layer = ctl.base[ID];
  assert.ok(layer, 'offered in the layer control');
  assert.strictEqual(layer.kind, 'wms');
  assert.strictEqual(layer._url, SERVICE + '?token=' + TOKEN);
  assert.deepStrictEqual(plain(layer.options), plain(expectedOptions(ctx, ID, LAYER)));
});

test('W getTileSpec() of the selected style builds the same layer', () => {
  const ctx = makeSandbox({ mapCfg: ON({ lightDefault: ID }) });
  ctx.__spec = ctx.window.MC_getTileSpec('light');
  assert.strictEqual(ctx.__spec.id, ID);
  assert.strictEqual(ctx.__spec.url, null, 'no tile template');
  assert.deepStrictEqual(plain(ctx.__spec.wms), {
    url: SERVICE + '?token=' + TOKEN, layers: LAYER,
    tileSize: 512, zoomOffset: -1, minZoom: 6, maxZoom: 19, bounds: BOUNDS,
  });
  const layer = vm.runInContext('window.MC_createBaseLayer(__spec, { maxZoom: 19 })', ctx);
  assert.strictEqual(layer.kind, 'wms');
  assert.strictEqual(layer._url, SERVICE + '?token=' + TOKEN);
  assert.deepStrictEqual(plain(layer.options), plain(expectedOptions(ctx, ID, LAYER)));
});

// ── K: no extra tile churn ────────────────────────────────────────────────
console.log('\n── #741 M2 K: no option beyond Skærmkort\'s, so no extra churn ──');

test('K the Natur layer carries the Skærmkort options, except the layer and the credit', () => {
  const ctx = makeSandbox({ mapCfg: ON() });
  const { ctl } = buildControl(ctx, true);
  const n = plain(ctl.base[ID].options), s = plain(ctl.base[SK].options);
  for (const o of [n, s]) { delete o.layers; delete o.attribution; }
  assert.deepStrictEqual(n, s);
});

test('K keepBuffer, the update* settings and retina doubling stay Leaflet\'s defaults', () => {
  const ctx = makeSandbox({ mapCfg: ON() });
  const { ctl } = buildControl(ctx, true);
  const o = ctl.base[ID].options;
  for (const k of ['keepBuffer', 'updateWhenIdle', 'updateWhenZooming', 'updateInterval', 'detectRetina', 'maxNativeZoom', 'uppercase']) {
    assert.ok(!(k in o), k + ' is set: ' + o[k]);
  }
});

test('K the descriptor is a copy: changing a spec never changes the registry', () => {
  const ctx = makeSandbox({ mapCfg: ON({ lightDefault: ID }) });
  const spec = ctx.window.MC_getTileSpec('light');
  spec.wms.layers = 'Vejdirektoratet';
  spec.wms.bounds[0][0] = 0;
  const again = ctx.window.MC_getTileSpec('light');
  assert.strictEqual(again.wms.layers, LAYER);
  assert.strictEqual(again.wms.bounds[0][0], BOUNDS[0][0]);
});

// ── T: one token ──────────────────────────────────────────────────────────
console.log('\n── #741 M2 T: the Skærmkort token, and no other ──');

const tokenOf = (url) => new URL(url).searchParams.getAll('token');

test('T the Natur URL carries the Skærmkort token, URL-encoded, exactly once', () => {
  const raw = ' a b&c=d/é?token=x ';
  const ctx = makeSandbox({ mapCfg: { tiles: { providers: { dataforsyningen: { enabled: true, token: raw } } } } });
  const { ctl } = buildControl(ctx, true);
  const url = ctl.base[ID]._url;
  assert.strictEqual(url, SERVICE + '?token=' + encodeURIComponent(raw.trim()));
  assert.deepStrictEqual(tokenOf(url), [raw.trim()]);
  assert.deepStrictEqual(tokenOf(url), tokenOf(ctl.base[SK]._url), 'the same token as Skærmkort');
  assert.strictEqual(url.split('?').length, 2, 'one query string');
});

test('T no other config key is read for it', () => {
  const ctx = makeSandbox({ mapCfg: { tiles: { providers: {
    dataforsyningen: { enabled: true, token: TOKEN, naturToken: 'other-1', natur_friluftskort: { token: 'other-2' } },
    natur_friluftskort: { enabled: true, token: 'other-3' },
  } } } });
  const { ctl } = buildControl(ctx, true);
  assert.deepStrictEqual(tokenOf(ctl.base[ID]._url), [TOKEN]);
});

test('T a rotated token moves both maps, re-read on every call', () => {
  const ctx = makeSandbox({ mapCfg: ON({ lightDefault: ID }) });
  ctx.window.MC_MAP_CFG = ON({ lightDefault: ID });
  ctx.window.MC_MAP_CFG.tiles.providers.dataforsyningen.token = 'rotated';
  ctx.window.MC_initTileRegistry(true);
  assert.strictEqual(ctx.window.MC_getTileSpec('light').wms.url, SERVICE + '?token=rotated');
  const { ctl } = buildControl(ctx, true);
  assert.strictEqual(ctl.base[SK]._url, SK_SERVICE + '?token=rotated');
});

test('T the token is not a Leaflet option (it would become a second WMS parameter)', () => {
  const ctx = makeSandbox({ mapCfg: ON() });
  const { ctl } = buildControl(ctx, true);
  for (const k of Object.keys(ctl.base[ID].options)) assert.ok(!/token/i.test(k), k);
});

// ── C: the credit ─────────────────────────────────────────────────────────
console.log('\n── #741 M2 C: the Natur- og Friluftskort CC BY 4.0 credit ──');

const LINKS = [
  { mark: 'Natur- og Friluftskort', href: DATASET_URL, rel: 'noopener noreferrer' },
  { mark: 'CC BY 4.0', href: LICENSE_URL, rel: 'noopener noreferrer license' },
];

test('C the style carries the credit built by the shared walker', () => {
  const ctx = makeSandbox({ mapCfg: ON() });
  assert.strictEqual(ctx.window.MC_TILE_PROVIDERS[ID].attribution, ctx.window.MC_buildCredit(CREDIT, LINKS));
});

test('C the dataset and the licence are anchors, everything else is text', () => {
  assert.strictEqual(registry(ON())[ID].attribution,
    'Indeholder data fra Klimadatastyrelsen, ' +
    '<a href="' + DATASET_URL + '" target="_blank" rel="noopener noreferrer">Natur- og Friluftskort</a>, ' +
    '<a href="' + LICENSE_URL + '" target="_blank" rel="noopener noreferrer license">CC BY 4.0</a>');
});

test('C the text of the credit is the plain credit, byte for byte', () => {
  assert.strictEqual(registry(ON())[ID].attribution.replace(/<[^>]+>/g, ''), CREDIT);
});

test('C the Skærmkort styles keep their own credit', () => {
  const reg = registry(ON());
  for (const id of Object.keys(reg).filter(i => i.indexOf('dataforsyningen-skaermkort') === 0)) {
    assert.strictEqual(reg[id].attribution.replace(/<[^>]+>/g, ''), SK_CREDIT, id);
    assert.ok(reg[id].attribution.indexOf(DATASET_URL) < 0, id + ' links the Natur dataset');
  }
  assert.ok(reg[ID].attribution.indexOf('GeoDanmark') < 0, 'the Natur credit names GeoDanmark');
});

test('C the credit is there while the gate is off too (the registry builds it at load)', () => {
  const ctx = makeSandbox({ mapCfg: { tiles: {} } });
  ctx.window.MC_MAP_CFG = ON();
  ctx.window.MC_initTileRegistry(true);
  assert.strictEqual(ctx.window.MC_TILE_PROVIDERS[ID].attribution.replace(/<[^>]+>/g, ''), CREDIT);
});

// ── D: defaults ───────────────────────────────────────────────────────────
console.log('\n── #741 M2 D: a gated-off default is skipped and warned; dark never shows it ──');

function warningsFor(mapCfg, theme) {
  const warned = [];
  const orig = console.warn;
  console.warn = (m) => warned.push(String(m));
  let ctx;
  try { ctx = makeSandbox({ mapCfg, theme }); } finally { console.warn = orig; }
  return { ctx, warned };
}

test('D a Natur light default without the token falls back and names both switches', () => {
  const { ctx, warned } = warningsFor({ tiles: { lightDefault: ID, providers: { dataforsyningen: { enabled: true } } } });
  assert.strictEqual(ctx.window.MC_getTileSpec('light').id, 'osm-standard');
  assert.strictEqual(warned.length, 1, warned.join('\n'));
  assert.ok(warned[0].indexOf(ID) >= 0, warned[0]);
  assert.ok(warned[0].indexOf('map.tiles.providers.dataforsyningen.enabled') >= 0, warned[0]);
  assert.ok(warned[0].indexOf('map.tiles.providers.dataforsyningen.token') >= 0, warned[0]);
});

test('D an enabled Natur light default is used, with no warning', () => {
  const { ctx, warned } = warningsFor(ON({ lightDefault: ID }));
  assert.strictEqual(ctx.window.MC_getTileSpec('light').id, ID);
  assert.deepStrictEqual(warned, []);
});

test('D light only: never inverted, and not selectable or storable as a dark style', () => {
  const ctx = makeSandbox({ theme: 'dark', mapCfg: ON(), storage: { 'mc-dark-tile-provider': ID } });
  assert.notStrictEqual(ctx.window.MC_getTileSpec('dark').id, ID, 'a stored dark choice of it is ignored');
  assert.strictEqual(ctx.window.MC_setDarkTileProvider(ID), false, 'not selectable as a dark style');
});

/* An operator's darkDefault naming a light style is honoured as is, not
 * inverted — existing behaviour for every light style, Skærmkort standard
 * included; M2 changes nothing there. */
test('D as an operator\'s darkDefault it shows as published, exactly like Skærmkort standard', () => {
  for (const id of [ID, SK]) {
    const ctx = makeSandbox({ theme: 'dark', mapCfg: ON({ darkDefault: id }) });
    const spec = ctx.window.MC_getTileSpec('dark');
    assert.strictEqual(spec.id, id);
    assert.strictEqual(spec.invertFilter, null, id + ' is not inverted');
  }
});

// ── P: a pick in the layer control lasts the page session ─────────────────
console.log('\n── #741 M2 P: a pick survives a theme switch, a rebuild and SPA navigation ──');

const lastCtl = (map) => map._controls[map._controls.length - 1];
function pick(map, name) {
  const base = lastCtl(map).base;
  for (const k of Object.keys(base)) if (k !== name && base[k] && map.hasLayer(base[k])) map.removeLayer(base[k]);
  if (base[name] && base[name].kind) map.addLayer(base[name]);
  map.fire('baselayerchange', { name, layer: base[name] });
}
const shown = (map) => {
  const base = lastCtl(map).base;
  return Object.keys(base).filter(id => map.hasLayer(base[id]));
};
function setTheme(ctx, theme) { ctx.document.documentElement.getAttribute = () => theme; }

test('P a pick shows Natur, unfiltered, and the Auto group leaves the map', () => {
  const ctx = makeSandbox({ mapCfg: ON() });
  const { map } = buildControl(ctx, true);
  pick(map, ID);
  assert.deepStrictEqual(shown(map), [ID]);
  assert.strictEqual(map.panes.tilePane.style.filter, '');
  assert.strictEqual(map.panes.tilePane.attrs['data-explicit-layer'], 'true');
});

test('P in the dark theme a pick stays unfiltered (light only, not inverted)', () => {
  const ctx = makeSandbox({ theme: 'dark', mapCfg: ON() });
  const { map } = buildControl(ctx, true);
  pick(map, ID);
  assert.strictEqual(map.panes.tilePane.style.filter, '');
  ctx.window.MC_applyTileFilter();
  assert.strictEqual(map.panes.tilePane.style.filter, '', 'the theme filter leaves an explicit pick alone');
});

test('P the pick survives a theme switch, a rebuild and SPA navigation', () => {
  const ctx = makeSandbox({ mapCfg: ON() });
  const { map } = buildControl(ctx, true);
  pick(map, ID);
  setTheme(ctx, 'dark');
  ctx.window.MC_initTileRegistry(true);
  assert.deepStrictEqual(shown(map), [ID], 'rebuild in the dark theme');
  const again = buildControl(ctx, true).map;
  assert.deepStrictEqual(shown(again), [ID], 'a new map and control (SPA navigation)');
  assert.strictEqual(lastCtl(again).base[ID].kind, 'wms');
  assert.strictEqual(lastCtl(again).base[ID]._url, SERVICE + '?token=' + TOKEN);
});

test('P switching between Skærmkort and Natur swaps the layer, not the token', () => {
  const ctx = makeSandbox({ mapCfg: ON() });
  const { map } = buildControl(ctx, true);
  pick(map, SK);
  pick(map, ID);
  assert.deepStrictEqual(shown(map), [ID]);
  pick(map, SK);
  assert.deepStrictEqual(shown(map), [SK]);
});

console.log('\n═══════════════════════════════════════');
console.log('  #741 M2 Natur- og Friluftskort: ' + passed + ' passed, ' + failed + ' failed');
console.log('═══════════════════════════════════════');
if (failed) process.exit(1);
