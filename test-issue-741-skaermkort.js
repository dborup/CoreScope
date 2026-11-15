/* test-issue-741-skaermkort.js — #741 M1b: the Dataforsyningen Skærmkort
 * base maps through WMS in EPSG:3857.
 *
 * The research (M0) found no Web Mercator WMTS for Skærmkortet, but one
 * WMS, topo_skaermkort_DAF, that serves all three variants in EPSG:3857.
 * M1a (#778) gave the registry one base-layer factory with a WMS branch;
 * M1b adds the provider. The browser fetches the tiles itself, with a
 * token of its own that is client-visible by design and is NOT the DHM
 * token (the server tests in cmd/server pin that side).
 *
 * Contract under test:
 *   G   The gate: the styles exist only with
 *       map.tiles.providers.dataforsyningen.enabled === true (the JSON
 *       boolean, exact-case keys) AND a non-blank string token. Off by
 *       default, and off for every near miss.
 *   S   The styles: standard, grå and dæmpet for light; grå under the
 *       existing invert filter for dark; Denmark-only labels.
 *   W   The factory output: an L.tileLayer.wms at the service URL with the
 *       token on the base URL (URL-encoded, exactly once) and exactly the
 *       allow-listed options — tileSize 512 with zoomOffset -1, zoom 6-19,
 *       Denmark bounds — from the layer control and from getTileSpec().
 *   T   getTileSpec() carries a resolved copy of the descriptor, re-read
 *       on every call, and `url` stays null for a WMS style.
 *   C   The credit: "Indeholder data fra Klimadatastyrelsen og GeoDanmark,
 *       Skærmkort, CC BY 4.0", with the dataset and the licence linked by
 *       the shared walker and the text byte-identical to the plain credit.
 *   D   A darkDefault/lightDefault naming a gated-off style is skipped and
 *       warned about, naming both switches.
 *
 * No test touches api.dataforsyningen.dk and no real token is used: the
 * vm sandbox records the layers it is asked to build and fetches nothing.
 *
 * Runs via: node test-issue-741-skaermkort.js
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
const SERVICE = 'https://api.dataforsyningen.dk/topo_skaermkort_DAF';
const ON = (extra) => ({ tiles: Object.assign({ providers: { dataforsyningen: { enabled: true, token: TOKEN } } }, extra || {}) });
const LIGHT_IDS = ['dataforsyningen-skaermkort', 'dataforsyningen-skaermkort-graa', 'dataforsyningen-skaermkort-daempet'];
const DARK_IDS = ['dataforsyningen-skaermkort-graa-dark'];
const ALL_IDS = LIGHT_IDS.concat(DARK_IDS);
const LAYERS = {
  'dataforsyningen-skaermkort': 'dtk_skaermkort',
  'dataforsyningen-skaermkort-graa': 'dtk_skaermkort_graa',
  'dataforsyningen-skaermkort-daempet': 'dtk_skaermkort_daempet',
  'dataforsyningen-skaermkort-graa-dark': 'dtk_skaermkort_graa',
};
const INVERT_CSS = 'invert(1) hue-rotate(180deg) brightness(0.9) contrast(1.05)';
// EX_GeographicBoundingBox of the three layers (research M0 §2).
const BOUNDS = [[53.015, 2.47842], [58.6403, 17.5578]];
const CREDIT = 'Indeholder data fra Klimadatastyrelsen og GeoDanmark, Skærmkort, CC BY 4.0';
const DATASET_URL = 'https://dataforsyningen.dk/data/962';
const LICENSE_URL = 'https://creativecommons.org/licenses/by/4.0/';

// The Skærmkort styles; Natur- og Friluftskort (M2) shares the gate and is
// pinned by test-issue-741-natur-friluft.js.
const offered = (mapCfg) => Object.keys(makeSandbox({ mapCfg }).window.MC_TILE_PROVIDERS)
  .filter(id => id.indexOf('dataforsyningen-skaermkort') === 0).sort();

// ── G: the gate ───────────────────────────────────────────────────────────
console.log('\n── #741 G: off unless enabled === true and a token ──');

test('G off by default: no config, no styles', () => {
  assert.deepStrictEqual(offered(undefined), []);
  assert.deepStrictEqual(offered({ tiles: {} }), []);
  assert.deepStrictEqual(offered({ tiles: { providers: {} } }), []);
});

test('G enabled with a token offers exactly the four styles', () => {
  assert.deepStrictEqual(offered(ON()), ALL_IDS.slice().sort());
});

const NEAR_MISSES = {
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
  'ENABLED/TOKEN (wrong case)': { ENABLED: true, TOKEN: TOKEN },
};
for (const name of Object.keys(NEAR_MISSES)) {
  test('G ' + name + ' → off', () => {
    assert.deepStrictEqual(offered({ tiles: { providers: { dataforsyningen: NEAR_MISSES[name] } } }), []);
  });
}

test('G a wrong-case provider key → off', () => {
  for (const k of ['Dataforsyningen', 'DATAFORSYNINGEN', 'dataForsyningen']) {
    const providers = {}; providers[k] = { enabled: true, token: TOKEN };
    assert.deepStrictEqual(offered({ tiles: { providers } }), [], k);
  }
});

test('G a malformed block → off, and the registry still loads', () => {
  for (const providers of ['x', 5, true, null, []]) {
    assert.deepStrictEqual(offered({ tiles: { providers } }), [], JSON.stringify(providers));
  }
  for (const d of ['x', 5, true, null, [TOKEN]]) {
    const ctx = makeSandbox({ mapCfg: { tiles: { providers: { dataforsyningen: d } } } });
    assert.ok(ctx.window.MC_TILE_PROVIDERS['osm-standard'], 'the fallback is still offered for ' + JSON.stringify(d));
  }
});

test('G the gate follows config that lands after load (MC_initTileRegistry)', () => {
  const ctx = makeSandbox({ mapCfg: { tiles: {} } });
  assert.ok(!ctx.window.MC_TILE_PROVIDERS['dataforsyningen-skaermkort']);
  ctx.window.MC_MAP_CFG = ON();
  ctx.window.MC_initTileRegistry(true);
  assert.ok(ctx.window.MC_TILE_PROVIDERS['dataforsyningen-skaermkort'], 'enabled once the config arrives');
  ctx.window.MC_MAP_CFG = { tiles: { providers: { dataforsyningen: { enabled: true, token: '' } } } };
  ctx.window.MC_initTileRegistry(true);
  assert.ok(!ctx.window.MC_TILE_PROVIDERS['dataforsyningen-skaermkort'], 'and off again without a token');
});

test('G the other providers are unaffected', () => {
  const on = Object.keys(makeSandbox({ mapCfg: ON() }).window.MC_TILE_PROVIDERS).filter(id => id.indexOf('dataforsyningen-') !== 0).sort();
  const off = Object.keys(makeSandbox({ mapCfg: { tiles: {} } }).window.MC_TILE_PROVIDERS).sort();
  assert.deepStrictEqual(on, off);
});

// ── S: the styles ─────────────────────────────────────────────────────────
console.log('\n── #741 S: standard, grå, dæmpet; grå inverted for dark ──');

test('S the light styles are not inverted, the dark one is grå under INVERT_CSS', () => {
  const reg = makeSandbox({ mapCfg: ON() }).window.MC_TILE_PROVIDERS;
  for (const id of LIGHT_IDS) {
    assert.strictEqual(reg[id].type, 'light', id);
    assert.strictEqual(reg[id].invertFilter, null, id);
    assert.strictEqual(reg[id].provider, 'dataforsyningen', id);
  }
  const dark = reg['dataforsyningen-skaermkort-graa-dark'];
  assert.strictEqual(dark.type, 'dark');
  assert.strictEqual(dark.invertFilter, INVERT_CSS, 'the existing dark-mode filter, not a new one');
  assert.strictEqual(dark.wms.layers, 'dtk_skaermkort_graa', 'the achromatic variant inverts to a neutral dark map');
});

test('S each style requests its own layer', () => {
  const reg = makeSandbox({ mapCfg: ON() }).window.MC_TILE_PROVIDERS;
  for (const id of ALL_IDS) assert.strictEqual(reg[id].wms.layers, LAYERS[id], id);
});

test('S the labels say Skærmkort and Denmark', () => {
  const reg = makeSandbox({ mapCfg: ON() }).window.MC_TILE_PROVIDERS;
  assert.strictEqual(reg['dataforsyningen-skaermkort'].label, 'Skærmkort (Denmark)');
  assert.strictEqual(reg['dataforsyningen-skaermkort-graa'].label, 'Skærmkort grå (Denmark)');
  assert.strictEqual(reg['dataforsyningen-skaermkort-daempet'].label, 'Skærmkort dæmpet (Denmark)');
  assert.strictEqual(reg['dataforsyningen-skaermkort-graa-dark'].label, 'Skærmkort grå (Denmark)');
});

test('S no style carries a tile template, so nothing can build it as XYZ', () => {
  const reg = makeSandbox({ mapCfg: ON() }).window.MC_TILE_PROVIDERS;
  for (const id of ALL_IDS) assert.ok(!reg[id].url && !reg[id].baseUrl, id);
});

// ── W: what the factory builds ────────────────────────────────────────────
console.log('\n── #741 W: the factory builds the WMS layer, and only that ──');

function expectedOptions(ctx, id) {
  return {
    attribution: ctx.window.MC_TILE_PROVIDERS[id].attribution,
    maxZoom: 19,
    layers: LAYERS[id],
    styles: '',
    format: 'image/png',
    transparent: 'FALSE',
    version: '1.3.0',
    crs: ctx.L.CRS.EPSG3857,
    tileSize: 512,
    zoomOffset: -1,
    minZoom: 6,
    bounds: ctx.L.latLngBounds(BOUNDS),
  };
}

test('W the layer control builds every style as L.tileLayer.wms with exactly these options', () => {
  const ctx = makeSandbox({ mapCfg: ON() });
  const { ctl } = buildControl(ctx, true);
  for (const id of ALL_IDS) {
    const layer = ctl.base[id];
    assert.ok(layer, id + ' is offered in the layer control');
    assert.strictEqual(layer.kind, 'wms', id);
    assert.strictEqual(layer._url, SERVICE + '?token=' + TOKEN, id);
    assert.deepStrictEqual(plain(layer.options), plain(expectedOptions(ctx, id)), id);
  }
});

test('W the token is URL-encoded, sits on the base URL exactly once, and is trimmed', () => {
  const raw = ' a b&c=d/é?token=x ';
  const ctx = makeSandbox({ mapCfg: { tiles: { providers: { dataforsyningen: { enabled: true, token: raw } } } } });
  const { ctl } = buildControl(ctx, true);
  const url = ctl.base['dataforsyningen-skaermkort']._url;
  assert.strictEqual(url, SERVICE + '?token=' + encodeURIComponent(raw.trim()));
  assert.strictEqual(url.split('?').length, 2, 'one query string');
  assert.strictEqual((url.match(/[?&]token=/g) || []).length, 1, 'one token parameter');
  assert.ok(url.indexOf(SERVICE + '?') === 0, 'https, the agency\'s own host, the WMS service');
});

test('W the token is not a Leaflet option (Leaflet would rename or duplicate a wmsParam)', () => {
  const ctx = makeSandbox({ mapCfg: ON() });
  const { ctl } = buildControl(ctx, true);
  for (const id of ALL_IDS) {
    const o = ctl.base[id].options;
    for (const k of Object.keys(o)) assert.ok(!/token/i.test(k), id + ': option ' + k);
    for (const k of ['uppercase', 'detectRetina', 'id', 'provider', 'label', 'type', 'invertFilter', 'url', 'wms']) {
      assert.ok(!(k in o), id + ': ' + k + ' must not become a request parameter');
    }
  }
});

test('W getTileSpec() of a selected style builds the same layer', () => {
  for (const [theme, id] of [['light', 'dataforsyningen-skaermkort-daempet'], ['dark', 'dataforsyningen-skaermkort-graa-dark']]) {
    const ctx = makeSandbox({ theme, mapCfg: ON(theme === 'light' ? { lightDefault: id } : { darkDefault: id }) });
    ctx.__spec = ctx.window.MC_getTileSpec(theme);
    const layer = vm.runInContext('window.MC_createBaseLayer(__spec, { maxZoom: 19 })', ctx);
    assert.strictEqual(layer.kind, 'wms', theme);
    assert.strictEqual(layer._url, SERVICE + '?token=' + TOKEN, theme);
    assert.deepStrictEqual(plain(layer.options), plain(expectedOptions(ctx, id)), theme);
  }
});

// ── T: getTileSpec carries the descriptor ─────────────────────────────────
console.log('\n── #741 T: getTileSpec() carries a resolved WMS descriptor ──');

test('T a WMS style resolves to url null and a wms copy with a string URL', () => {
  const ctx = makeSandbox({ mapCfg: ON({ lightDefault: 'dataforsyningen-skaermkort' }) });
  const spec = ctx.window.MC_getTileSpec('light');
  assert.strictEqual(spec.id, 'dataforsyningen-skaermkort');
  assert.strictEqual(spec.url, null, 'no tile template');
  assert.ok(spec.wms, 'the descriptor is carried');
  assert.strictEqual(spec.wms.url, SERVICE + '?token=' + TOKEN, 'resolved to a string');
  assert.deepStrictEqual(plain(spec.wms), {
    url: SERVICE + '?token=' + TOKEN, layers: 'dtk_skaermkort',
    tileSize: 512, zoomOffset: -1, minZoom: 6, maxZoom: 19, bounds: BOUNDS,
  });
  assert.strictEqual(spec.attribution, ctx.window.MC_TILE_PROVIDERS['dataforsyningen-skaermkort'].attribution);
  assert.strictEqual(spec.maxZoom, 19);
  assert.strictEqual(spec.invertFilter, null);
});

test('T the descriptor is a copy: changing it never changes the registry', () => {
  const ctx = makeSandbox({ mapCfg: ON({ lightDefault: 'dataforsyningen-skaermkort' }) });
  const spec = ctx.window.MC_getTileSpec('light');
  spec.wms.layers = 'mutated';
  spec.wms.bounds[0][0] = 0;
  const again = ctx.window.MC_getTileSpec('light');
  assert.strictEqual(again.wms.layers, 'dtk_skaermkort');
  assert.strictEqual(again.wms.bounds[0][0], BOUNDS[0][0]);
});

test('T the token is re-read on every call, so a config that lands late applies', () => {
  const ctx = makeSandbox({ mapCfg: ON({ lightDefault: 'dataforsyningen-skaermkort' }) });
  ctx.window.MC_MAP_CFG = ON({ lightDefault: 'dataforsyningen-skaermkort' });
  ctx.window.MC_MAP_CFG.tiles.providers.dataforsyningen.token = 'rotated';
  ctx.window.MC_initTileRegistry(true);
  assert.strictEqual(ctx.window.MC_getTileSpec('light').wms.url, SERVICE + '?token=rotated');
});

test('T a tile style still has no wms field (the contract stays additive)', () => {
  const ctx = makeSandbox({ mapCfg: ON() });
  for (const theme of ['light', 'dark']) {
    const spec = ctx.window.MC_getTileSpec(theme);
    assert.ok(!('wms' in spec), theme + ': ' + JSON.stringify(Object.keys(spec)));
    assert.strictEqual(typeof spec.url, 'string', theme);
  }
});

test('T the dark style resolves the invert filter for the theme', () => {
  const ctx = makeSandbox({ theme: 'dark', mapCfg: ON({ darkDefault: 'dataforsyningen-skaermkort-graa-dark' }) });
  const spec = ctx.window.MC_getTileSpec('dark');
  assert.strictEqual(spec.invertFilter, INVERT_CSS);
  assert.strictEqual(spec.wms.layers, 'dtk_skaermkort_graa');
});

// ── C: the credit ─────────────────────────────────────────────────────────
console.log('\n── #741 C: the CC BY 4.0 credit with its two links ──');

const LINKS = [
  { mark: 'Skærmkort', href: DATASET_URL, rel: 'noopener noreferrer' },
  { mark: 'CC BY 4.0', href: LICENSE_URL, rel: 'noopener noreferrer license' },
];

test('C every style carries the credit built by the shared walker', () => {
  const ctx = makeSandbox({ mapCfg: ON() });
  const want = ctx.window.MC_buildCredit(CREDIT, LINKS);
  for (const id of ALL_IDS) assert.strictEqual(ctx.window.MC_TILE_PROVIDERS[id].attribution, want, id);
});

test('C the dataset and the licence are anchors, everything else is text', () => {
  const ctx = makeSandbox({ mapCfg: ON() });
  assert.strictEqual(ctx.window.MC_TILE_PROVIDERS['dataforsyningen-skaermkort'].attribution,
    'Indeholder data fra Klimadatastyrelsen og GeoDanmark, ' +
    '<a href="' + DATASET_URL + '" target="_blank" rel="noopener noreferrer">Skærmkort</a>, ' +
    '<a href="' + LICENSE_URL + '" target="_blank" rel="noopener noreferrer license">CC BY 4.0</a>');
});

test('C the text of the credit is the plain credit, byte for byte', () => {
  const ctx = makeSandbox({ mapCfg: ON() });
  const html = ctx.window.MC_TILE_PROVIDERS['dataforsyningen-skaermkort'].attribution;
  assert.strictEqual(html.replace(/<[^>]+>/g, ''), CREDIT);
});

test('C the dataset link is the Skærmkort product page and the licence CC BY 4.0', () => {
  const ctx = makeSandbox({ mapCfg: ON() });
  const html = ctx.window.MC_TILE_PROVIDERS['dataforsyningen-skaermkort'].attribution;
  const hrefs = (html.match(/href="[^"]+"/g) || []).map(h => h.slice(6, -1));
  assert.deepStrictEqual(hrefs, [DATASET_URL, LICENSE_URL]);
});

// ── D: a gated-off default ────────────────────────────────────────────────
console.log('\n── #741 D: a default naming a gated-off style is skipped and warned ──');

function warningsFor(mapCfg) {
  const warned = [];
  const orig = console.warn;
  console.warn = (m) => warned.push(String(m));
  let ctx;
  try { ctx = makeSandbox({ mapCfg }); } finally { console.warn = orig; }
  return { ctx, warned };
}

test('D a dataforsyningen default without the token falls back and names both switches', () => {
  const { ctx, warned } = warningsFor({ tiles: {
    darkDefault: 'dataforsyningen-skaermkort-graa-dark', lightDefault: 'dataforsyningen-skaermkort',
    providers: { dataforsyningen: { enabled: true } },
  } });
  assert.strictEqual(ctx.window.MC_getTileSpec('light').id, 'osm-standard');
  assert.strictEqual(ctx.window.MC_getTileSpec('dark').id, 'osm-dark');
  assert.strictEqual(warned.length, 2, warned.join('\n'));
  for (const w of warned) {
    assert.ok(w.indexOf('map.tiles.providers.dataforsyningen.enabled') >= 0, w);
    assert.ok(w.indexOf('map.tiles.providers.dataforsyningen.token') >= 0, w);
  }
});

test('D the Esri warning keeps naming only its own switch', () => {
  const { warned } = warningsFor({ tiles: { darkDefault: 'esri-darkgray-labels' } });
  assert.strictEqual(warned.length, 1);
  assert.ok(warned[0].indexOf('map.tiles.providers.esri.enabled: true') >= 0, warned[0]);
  assert.ok(warned[0].indexOf('token') < 0, warned[0]);
});

test('D an enabled default is used, with no warning', () => {
  const { ctx, warned } = warningsFor(ON({ lightDefault: 'dataforsyningen-skaermkort-graa' }));
  assert.strictEqual(ctx.window.MC_getTileSpec('light').id, 'dataforsyningen-skaermkort-graa');
  assert.deepStrictEqual(warned, []);
});

// ── P: a pick in the layer control lasts the session ──────────────────────
console.log('\n── #741 P: a layer-control pick survives a theme switch, a rebuild and SPA navigation ──');

/* Coordinator decision: no new persistence mechanism. A pick in the layer
 * control lives in memory for the page session: it survives a theme switch,
 * a rebuild of the control (config landing late) and leaving the Map or
 * Live page and coming back (a new map and a new control). A reload starts
 * from Auto again; across reloads the customizer's choice applies, as for
 * every other non-ortho style. */
const SK = 'dataforsyningen-skaermkort-daempet';
const lastCtl = (map) => map._controls[map._controls.length - 1];
// What L.Control.Layers does when a base radio is clicked.
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

test('P a pick shows that style, and the Auto group leaves the map', () => {
  const ctx = makeSandbox({ mapCfg: ON() });
  const { map } = buildControl(ctx, true);
  assert.deepStrictEqual(shown(map), ['__auto__'], 'Auto first');
  pick(map, SK);
  assert.deepStrictEqual(shown(map), [SK]);
});

test('P the pick survives a rebuild of the control', () => {
  const ctx = makeSandbox({ mapCfg: ON() });
  const { map } = buildControl(ctx, true);
  pick(map, SK);
  const before = lastCtl(map);
  ctx.window.MC_initTileRegistry(true); // config landing late rebuilds every control
  assert.notStrictEqual(lastCtl(map), before, 'precondition: the control was rebuilt');
  assert.deepStrictEqual(shown(map), [SK]);
  assert.strictEqual(lastCtl(map).base[SK].kind, 'wms');
});

test('P the pick survives a theme switch, and a rebuild after it', () => {
  const ctx = makeSandbox({ mapCfg: ON() });
  const { map } = buildControl(ctx, true);
  pick(map, SK);
  setTheme(ctx, 'dark');
  assert.deepStrictEqual(shown(map), [SK], 'a theme switch leaves an explicit pick alone');
  ctx.window.MC_initTileRegistry(true);
  assert.deepStrictEqual(shown(map), [SK], 'and a rebuild in the other theme restores the pick, not that theme\'s default');
});

test('P the pick survives SPA navigation: a new map and control start from it', () => {
  const ctx = makeSandbox({ mapCfg: ON() });
  const first = buildControl(ctx, true).map;
  pick(first, SK);
  const again = buildControl(ctx, true).map; // back on Map (or on to Live)
  assert.deepStrictEqual(shown(again), [SK]);
  pick(again, 'carto-voyager-dark');
  assert.deepStrictEqual(shown(buildControl(ctx, true).map), ['carto-voyager-dark'], 'any registry style, either theme');
});

test('P a restored pick is on the map before the control exists (Leaflet would re-render the labels)', () => {
  const ctx = makeSandbox({ mapCfg: ON() });
  pick(buildControl(ctx, true).map, SK);
  const seen = [];
  const layers = ctx.L.control.layers;
  ctx.L.control.layers = (base, overlays, opts) => {
    const c = layers(base, overlays, opts);
    const addTo = c.addTo;
    c.addTo = (map) => { seen.push(map.hasLayer(base[SK])); return addTo(map); };
    return c;
  };
  const { map } = buildControl(ctx, true); // SPA navigation
  ctx.window.MC_initTileRegistry(true);    // and a rebuild (of both maps' controls)
  assert.ok(seen.length >= 2 && seen.every(Boolean), 'on the map at every control creation: ' + JSON.stringify(seen));
  assert.deepStrictEqual(shown(map), [SK]);
});

test('P picking Auto ends it', () => {
  const ctx = makeSandbox({ mapCfg: ON() });
  const { map } = buildControl(ctx, true);
  pick(map, SK);
  pick(map, '__auto__');
  assert.deepStrictEqual(shown(buildControl(ctx, true).map), ['__auto__']);
});

// The #728 Aerial entry (server-proxied ortho tiles), as dhm.ortho sends it.
const ORTHO_DHM = {
  enabled: true, url: '/api/map/dhm/wms', layer: 'dhm_terraen_skyggekort', minZoom: 7, maxZoom: 19,
  bounds: [[54.4265, 7.99125], [57.778, 15.5995]], attribution: 'DHM credit',
  ortho: { url: '/api/map/dhm/ortho/{z}/{x}/{y}', minZoom: 7, maxZoom: 20,
    bounds: [[54.4265, 7.99125], [57.778, 15.5995]], attribution: 'Ortho credit' },
};

test('P picking the Aerial entry ends it too (the Aerial pick has its own pref)', () => {
  const ctx = makeSandbox({ mapCfg: ON(), dhm: ORTHO_DHM });
  const { map } = buildControl(ctx, true);
  pick(map, SK);
  pick(map, 'dhm-ortho');
  assert.deepStrictEqual(shown(buildControl(ctx, true).map), ['dhm-ortho'], 'the Aerial pref restores it');
  ctx.location.hash = '#/map?ortho=0';
  assert.deepStrictEqual(shown(buildControl(ctx, true).map), ['__auto__'], 'not the Skærmkort pick made before it');
});

test('P a reload starts from Auto: nothing new is stored', () => {
  const storage = {};
  const ctx = makeSandbox({ mapCfg: ON(), storage });
  pick(buildControl(ctx, true).map, SK);
  assert.deepStrictEqual(Object.keys(ctx.localStorage._raw).filter(k => k !== 'mc-ortho-base'), [],
    'no new storage key: ' + JSON.stringify(ctx.localStorage._raw));
  const reloaded = makeSandbox({ mapCfg: ON(), storage: ctx.localStorage._raw });
  assert.deepStrictEqual(shown(buildControl(reloaded, true).map), ['__auto__']);
});

test('P a pick that is no longer offered falls back to Auto', () => {
  const ctx = makeSandbox({ mapCfg: ON() });
  const { map } = buildControl(ctx, true);
  pick(map, SK);
  ctx.window.MC_MAP_CFG = { tiles: {} };
  ctx.window.MC_initTileRegistry(true);
  assert.deepStrictEqual(shown(map), ['__auto__']);
  assert.deepStrictEqual(shown(buildControl(ctx, true).map), ['__auto__']);
});

console.log('\n═══════════════════════════════════════');
console.log('  #741 M1b Skærmkort: ' + passed + ' passed, ' + failed + ' failed');
console.log('═══════════════════════════════════════');
if (failed) process.exit(1);
