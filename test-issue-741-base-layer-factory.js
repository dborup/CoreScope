/* test-issue-741-base-layer-factory.js — #741 M1a.
 *
 * #741 wants Danish base maps from Dataforsyningen, and the research (M0)
 * found that neither Skærmkortet nor Natur- og Friluftskort has a Web
 * Mercator WMTS: WMS in EPSG:3857 (`L.tileLayer.wms`) is the only
 * Leaflet-compatible route. The provider registry cannot express that
 * today — `getTileSpec()` hands out a `url` string and ten call sites do
 * `L.tileLayer(spec.url, …)` themselves, two of them with their own
 * hard-coded `maxZoom`. M1a is the DRY refactor that fixes only the
 * shape: one shared factory that owns the `L.tileLayer` vs
 * `L.tileLayer.wms` decision, with no new provider, no token and no
 * behaviour change.
 *
 * Contract under test:
 *   F1  MC_createBaseLayer(spec, opts) exists on window and is the one
 *       place a provider spec becomes a Leaflet base layer.
 *   F2  Tile branch: for every style the registry offers, the factory
 *       builds an L.tileLayer at the style's resolved URL (a lazy
 *       function-valued `url` invoked) with the style's own attribution
 *       and zoom ceiling — never a CARTO credit, never an invented one.
 *   F3  WMS branch: a WMS-shaped spec (a stub provider, never
 *       Dataforsyningen, and no request at all) builds an L.tileLayer.wms
 *       at the descriptor's base URL with the allow-listed WMS options,
 *       the spec's attribution and NOTHING else — Leaflet turns every
 *       option it does not recognise into a WMS request parameter.
 *   F4  The call site still wins: `opts` override anything derived from
 *       the spec, and a field the spec does not carry adds no option key,
 *       so a site that passed no maxZoom keeps Leaflet's own default.
 *   F5  Wiring: every converted call site calls the factory, and none of
 *       them calls L.tileLayer / L.tileLayer.wms directly any more. The
 *       only direct calls left under public/ are the factory's own (plus
 *       the overlay builder, which is not a base map).
 *   F6  No behaviour change, proven by running the PRODUCTION call sites
 *       (map.js initial + theme sync, live.js initial + theme sync,
 *       nodes.js inset helper, regions map, both customizer previews, the
 *       home minimap and the layer control) against a recording Leaflet
 *       and asserting the URL and the exact option object each one built
 *       before the refactor — including nodes.js's maxZoom 18 against
 *       map.js/live.js's 19, deliberately left inconsistent here.
 *   F7  The registry's own invariants are untouched: the invert filter,
 *       the Esri gate, the attribution registry and the legacy-URL branch
 *       resolve exactly as before.
 *
 * Runs via: node test-issue-741-base-layer-factory.js (pure vm sandbox,
 * no jsdom, no Playwright, no network).
 */
'use strict';
const vm     = require('vm');
const fs     = require('fs');
const path   = require('path');
const assert = require('assert');
const { makeLeaflet, makeMap, makeSandbox, buildControl } = require('./tests/map-overlay-sandbox');

const PUBLIC_DIR = process.env.PUBLIC_DIR || path.join(__dirname, 'public');
const read = (f) => fs.readFileSync(path.join(PUBLIC_DIR, f), 'utf8');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const OSM_CREDIT = '© OpenStreetMap contributors';

// The layers are built inside a vm realm, so their option objects do not
// share the host's Object.prototype; deepStrictEqual compares prototypes.
// Copy both sides into host-realm plain values before comparing.
function plain(v) {
  if (Array.isArray(v)) return v.map(plain);
  if (v && typeof v === 'object') {
    const o = {};
    for (const k of Object.keys(v)) o[k] = plain(v[k]);
    return o;
  }
  return v;
}
function sameOptions(actual, expected, msg) {
  assert.deepStrictEqual(plain(actual), plain(expected), msg);
}
// Every optional provider on, so the style table below is the full one.
const ALL_ON = { tiles: { providers: {
  carto: { enabled: true, domain: '' }, osm: { enabled: true },
  opentopomap: { enabled: true }, usgs: { enabled: true },
  stamen: { enabled: true, token: 'S1' }, esri: { enabled: true },
} } };

// ── F1/F2/F3/F4: the factory itself ───────────────────────────────────────
console.log('\n── #741 F1: the shared base-layer factory exists ──');

test('F1 window.MC_createBaseLayer is a function', () => {
  const ctx = makeSandbox({ mapCfg: ALL_ON });
  assert.strictEqual(typeof ctx.window.MC_createBaseLayer, 'function',
    'the registry must publish the one factory every map builds its base layer with');
});

function factory(ctx) {
  return (spec, opts) => {
    ctx.__spec = spec; ctx.__opts = opts;
    return vm.runInContext('window.MC_createBaseLayer(__spec, __opts)', ctx);
  };
}

console.log('\n── #741 F2: tile branch — every registry style, unchanged ──');

test('F2 every offered style builds an L.tileLayer at its own URL and credit', () => {
  const ctx = makeSandbox({ mapCfg: ALL_ON });
  const make = factory(ctx);
  const reg = ctx.window.MC_TILE_PROVIDERS;
  const ids = Object.keys(reg);
  assert.ok(ids.length >= 14, 'expected the full style table, got ' + ids.length);
  for (const id of ids) {
    const p = reg[id];
    const url = typeof p.url === 'function' ? p.url() : p.url;
    const layer = make(p, { attribution: p.attribution || '', maxZoom: p.maxZoom || 19 });
    assert.strictEqual(layer.kind, 'tile', id + ' must be a plain tile layer');
    assert.strictEqual(layer._url, url, id + ': the lazy url must be resolved to a string');
    assert.strictEqual(typeof layer._url, 'string', id + ': never hand Leaflet a function');
    sameOptions(layer.options, { attribution: p.attribution, maxZoom: p.maxZoom },
      id + ': the layer control built exactly these options before the refactor');
  }
});

test('F2 a getTileSpec() result goes through unchanged, for both themes', () => {
  for (const theme of ['light', 'dark']) {
    const ctx = makeSandbox({ theme, mapCfg: ALL_ON });
    const spec = ctx.window.MC_getTileSpec(theme);
    const layer = factory(ctx)(spec, { maxZoom: 19 });
    assert.strictEqual(layer.kind, 'tile', theme);
    assert.strictEqual(layer._url, spec.url, theme);
    sameOptions(layer.options, { attribution: spec.attribution, maxZoom: 19 }, theme);
  }
});

console.log('\n── #741 F3: WMS branch — a stub provider, no real service ──');

// A deliberately fake service: #741 M1a adds no provider, and no test may
// touch api.dataforsyningen.dk.
const STUB_WMS = {
  url: 'https://wms.example.invalid/stub',
  layers: 'stub_layer',
  tileSize: 512,
  minZoom: 6,
  maxZoom: 19,
  bounds: [[54.5, 7.9], [57.9, 15.3]],
};
const STUB_CREDIT = 'Stub Agency, Stub Dataset, CC BY 4.0';

test('F3 a WMS spec builds L.tileLayer.wms at the descriptor URL', () => {
  const ctx = makeSandbox({ mapCfg: ALL_ON });
  const layer = factory(ctx)({ id: 'stub-wms', wms: STUB_WMS, attribution: STUB_CREDIT });
  assert.strictEqual(layer.kind, 'wms', 'a WMS spec must not become a plain tile layer');
  assert.strictEqual(layer._url, STUB_WMS.url);
});

test('F3 the WMS options are the allow-listed set, with the GetMap defaults', () => {
  const ctx = makeSandbox({ mapCfg: ALL_ON });
  const layer = factory(ctx)({ id: 'stub-wms', wms: STUB_WMS, attribution: STUB_CREDIT });
  sameOptions(layer.options, {
    attribution: STUB_CREDIT,
    layers: 'stub_layer',
    styles: '',
    format: 'image/png',
    transparent: false,
    version: '1.3.0',
    crs: ctx.L.CRS.EPSG3857,
    tileSize: 512,
    minZoom: 6,
    maxZoom: 19,
    bounds: ctx.L.latLngBounds(STUB_WMS.bounds),
  });
});

test('F3 a key the descriptor does not carry is not passed on', () => {
  const ctx = makeSandbox({ mapCfg: ALL_ON });
  const layer = factory(ctx)({ wms: { url: STUB_WMS.url, layers: 'a' }, attribution: STUB_CREDIT });
  assert.deepStrictEqual(Object.keys(layer.options).sort(),
    ['attribution', 'crs', 'format', 'layers', 'styles', 'tileSize', 'transparent', 'version'].sort(),
    'no bounds/minZoom/maxZoom option may be invented');
  assert.strictEqual(layer.options.tileSize, 256, 'Leaflet\'s own tile size is the default');
});

test('F3 an unknown field on the spec never reaches Leaflet (it would become a WMS parameter)', () => {
  const ctx = makeSandbox({ mapCfg: ALL_ON });
  const layer = factory(ctx)({
    id: 'stub-wms', provider: 'stub', label: 'Stub', type: 'light', invertFilter: 'invert(1)',
    refUrl: 'https://wms.example.invalid/ref', wms: Object.assign({ sausage: 'yes' }, STUB_WMS),
    attribution: STUB_CREDIT,
  });
  for (const junk of ['id', 'provider', 'label', 'type', 'invertFilter', 'refUrl', 'sausage', 'url', 'wms']) {
    assert.ok(!(junk in layer.options), junk + ' must not become a request parameter');
  }
});

test('F3 the descriptor can override every default', () => {
  const ctx = makeSandbox({ mapCfg: ALL_ON });
  const crs = { code: 'EPSG:25832' };
  const layer = factory(ctx)({ wms: {
    url: STUB_WMS.url, layers: 'a', styles: 'fancy', format: 'image/jpeg',
    transparent: true, version: '1.1.1', crs, tileSize: 1024,
  } });
  sameOptions(layer.options, {
    layers: 'a', styles: 'fancy', format: 'image/jpeg', transparent: true,
    version: '1.1.1', crs, tileSize: 1024,
  });
});

console.log('\n── #741 F4: the call site still wins ──');

test('F4 opts override the spec', () => {
  const ctx = makeSandbox({ mapCfg: ALL_ON });
  const layer = factory(ctx)(
    { url: 'https://a.example/{z}/{x}/{y}.png', attribution: 'spec', maxZoom: 17 },
    { attribution: 'site', maxZoom: 18, opacity: 0.5 });
  sameOptions(layer.options, { attribution: 'site', maxZoom: 18, opacity: 0.5 });
});

test('F4 a spec without maxZoom adds no maxZoom option', () => {
  const ctx = makeSandbox({ mapCfg: ALL_ON });
  const layer = factory(ctx)({ url: 'https://a.example/{z}/{x}/{y}.png', attribution: OSM_CREDIT });
  sameOptions(layer.options, { attribution: OSM_CREDIT },
    'the home minimap passes no zoom ceiling and must keep Leaflet\'s own');
});

test('F4 a spec without attribution adds no attribution option', () => {
  const ctx = makeSandbox({ mapCfg: ALL_ON });
  const layer = factory(ctx)({ url: 'https://a.example/ref/{z}/{y}/{x}' }, { maxZoom: 18 });
  sameOptions(layer.options, { maxZoom: 18 },
    'the node-detail reference overlay credited nothing of its own');
});

test('F4 an empty-string attribution is kept, not dropped', () => {
  const ctx = makeSandbox({ mapCfg: ALL_ON });
  const layer = factory(ctx)({ url: 'https://a.example/{z}/{x}/{y}.png' }, { attribution: '', maxZoom: 19 });
  sameOptions(layer.options, { attribution: '', maxZoom: 19 });
});

test('F4 a lazy function-valued url is invoked', () => {
  const ctx = makeSandbox({ mapCfg: ALL_ON });
  const layer = factory(ctx)({ url: () => 'https://lazy.example/{z}/{x}/{y}.png' });
  assert.strictEqual(layer._url, 'https://lazy.example/{z}/{x}/{y}.png');
});

// ── F5: wiring ────────────────────────────────────────────────────────────
console.log('\n── #741 F5: every base-map call site goes through the factory ──');

// The ten call sites §6.3 of the research lists, by the file that owns them.
const CALL_SITE_FILES = {
  'map.js': 3, 'live.js': 3, 'nodes.js': 2, 'regions-map.js': 1,
  'customize-v2.js': 2, 'home.js': 1, 'map-tile-providers.js': 2,
};

for (const file of Object.keys(CALL_SITE_FILES)) {
  test('F5 ' + file + ' builds its base layer through MC_createBaseLayer', () => {
    const src = read(file);
    const n = (src.match(/createBaseLayer\s*\(/g) || []).length;
    assert.ok(n >= CALL_SITE_FILES[file],
      'expected at least ' + CALL_SITE_FILES[file] + ' factory call(s), found ' + n);
  });
}

// Code lines only: the factory's own doc comment names L.tileLayer.
const isComment = (line) => /^\s*(\/\/|\*|\/\*)/.test(line);
function directCalls(file) {
  const out = [];
  read(file).split('\n').forEach((line, i) => {
    if (!isComment(line) && /\bL\.tileLayer(\.wms)?\s*\(/.test(line)) out.push((i + 1) + ' ' + line.trim());
  });
  return out;
}

test('F5 no page builds a base layer with L.tileLayer directly any more', () => {
  const offenders = [];
  for (const file of Object.keys(CALL_SITE_FILES)) {
    if (file === 'map-tile-providers.js') continue; // the factory and the overlays live there
    for (const hit of directCalls(file)) offenders.push(file + ':' + hit);
  }
  assert.deepStrictEqual(offenders, [], 'these call sites bypass the factory:\n' + offenders.join('\n'));
});

test('F5 inside the registry, L.tileLayer is called only by the factory and the overlay builder', () => {
  const hits = directCalls('map-tile-providers.js');
  assert.strictEqual(hits.length, 3,
    'expected the tile branch, the WMS branch and _createOverlayLayer, got:\n' + hits.join('\n'));
});

// ── F6: the production call sites, before and after ───────────────────────
console.log('\n── #741 F6: the production call sites build the same layers ──');

// Extracts the statements between `from` (from the start of its line) and
// `to` (inclusive) so the PRODUCTION code is what runs, not a copy of it.
function extract(file, from, to, nth) {
  const src = read(file);
  let i = -1;
  for (let k = 0; k <= (nth || 0); k++) {
    i = src.indexOf(from, i + 1);
    if (i < 0) throw new Error('anchor ' + JSON.stringify(from) + ' #' + (nth || 0) + ' not in ' + file);
  }
  const j = src.indexOf(to, i);
  if (j < 0) throw new Error('end anchor ' + JSON.stringify(to) + ' not after ' + JSON.stringify(from) + ' in ' + file);
  const start = src.lastIndexOf('\n', i) + 1;
  return src.slice(start, j + to.length);
}

// Brace-matched `function <name>(…) { … }`, as #332/#390/#752 extract it.
function extractFn(file, name) {
  const src = read(file);
  const m = src.match(new RegExp('function\\s+' + name + '\\s*\\([^)]*\\)\\s*\\{'));
  if (!m) throw new Error(name + ' not found in ' + file);
  let depth = 0, i = m.index;
  for (; i < src.length; i++) {
    if (src[i] === '{') depth++;
    else if (src[i] === '}' && --depth === 0) { i++; break; }
  }
  return src.slice(m.index, i);
}

// A sandbox with the real registry (so the factory under test is the
// shipped one) and a recording Leaflet, plus whatever locals a call site
// needs. Every layer the snippet builds lands in ctx.__layers.
function siteCtx(locals, opts) {
  const ctx = makeSandbox(Object.assign({ mapCfg: ALL_ON }, opts || {}));
  const layers = [];
  const L = ctx.L;
  const tile = L.tileLayer, wms = L.tileLayer.wms;
  L.tileLayer = (url, o) => { const l = tile(url, o); layers.push(l); return l; };
  L.tileLayer.wms = (url, o) => { const l = wms(url, o); layers.push(l); return l; };
  L.map = () => { const m = makeMap(); m.setView = () => m; m.invalidateSize = () => {}; return m; };
  L.marker = () => ({ addTo: () => ({}) });
  ctx.__layers = layers;
  Object.assign(ctx, locals || {});
  for (const k of Object.keys(ctx.window)) if (!(k in ctx)) ctx[k] = ctx.window[k];
  return ctx;
}

function group() {
  const g = { _layers: new Set() };
  g.addLayer = (l) => { g._layers.add(l); return g; };
  g.removeLayer = (l) => { g._layers.delete(l); return g; };
  g.hasLayer = (l) => g._layers.has(l);
  return g;
}

const SPEC = { url: 'https://stub.example/{z}/{x}/{y}.png', attribution: 'Stub credit', refUrl: null, maxZoom: 17 };
const SPEC_REF = Object.assign({}, SPEC, { refUrl: 'https://stub.example/ref/{z}/{y}/{x}' });

// Each entry: the production snippet, the locals it needs, and the layers
// it built before the refactor — url plus the exact option object.
const SITES = [
  {
    name: 'map.js initial base layer (light)',
    file: 'map.js', from: 'const _initTile = _resolveTileUrl(isDark);', to: "window.MC_applyTileFilter();",
    locals: () => ({ isDark: false, _darkRefLayer: null, autoLayerGroup: group(), _resolveTileUrl: () => SPEC }),
    expect: [{ url: SPEC.url, options: { attribution: SPEC.attribution, maxZoom: 19 } }],
  },
  {
    name: 'map.js initial base layer + dark reference overlay',
    file: 'map.js', from: 'const _initTile = _resolveTileUrl(isDark);', to: "window.MC_applyTileFilter();",
    locals: () => ({ isDark: true, _darkRefLayer: null, autoLayerGroup: group(), _resolveTileUrl: () => SPEC_REF }),
    expect: [
      { url: SPEC_REF.url, options: { attribution: SPEC_REF.attribution, maxZoom: 19 } },
      { url: SPEC_REF.refUrl, options: { maxZoom: 19, attribution: SPEC_REF.attribution } },
    ],
  },
  {
    name: 'map.js _syncDarkTiles reference overlay',
    file: 'map.js', fn: '_syncDarkTiles', call: '_syncDarkTiles(true);',
    locals: () => ({
      _resolveTileUrl: () => SPEC_REF, _darkRefLayer: null, autoLayerGroup: group(),
      tileLayer: { setUrl() {}, options: {} }, map: makeMap(),
    }),
    expect: [{ url: SPEC_REF.refUrl, options: { maxZoom: 19, attribution: SPEC_REF.attribution } }],
  },
  {
    name: 'live.js initial base layer (light)',
    file: 'live.js', from: 'const _liveInitTile = _liveResolveTile(isDark);', to: "window.MC_applyTileFilter();",
    locals: () => ({ isDark: false, _liveDarkRefLayer: null, liveAutoLayerGroup: group(), _liveResolveTile: () => SPEC }),
    expect: [{ url: SPEC.url, options: { maxZoom: 19, attribution: SPEC.attribution } }],
  },
  {
    name: 'live.js initial base layer + dark reference overlay',
    file: 'live.js', from: 'const _liveInitTile = _liveResolveTile(isDark);', to: "window.MC_applyTileFilter();",
    locals: () => ({ isDark: true, _liveDarkRefLayer: null, liveAutoLayerGroup: group(), _liveResolveTile: () => SPEC_REF }),
    expect: [
      { url: SPEC_REF.url, options: { maxZoom: 19, attribution: SPEC_REF.attribution } },
      { url: SPEC_REF.refUrl, options: { maxZoom: 19, attribution: SPEC_REF.attribution } },
    ],
  },
  {
    name: 'live.js _liveSyncDarkTiles reference overlay',
    file: 'live.js', fn: '_liveSyncDarkTiles', call: '_liveSyncDarkTiles(true);',
    locals: () => ({
      _liveResolveTile: () => SPEC_REF, _liveDarkRefLayer: null, liveAutoLayerGroup: group(),
      tileLayer: { setUrl() {}, options: {} }, map: makeMap(),
    }),
    expect: [{ url: SPEC_REF.refUrl, options: { maxZoom: 19, attribution: SPEC_REF.attribution } }],
  },
  {
    name: 'nodes.js inset helper (node detail, node-reach, packet-path, area, rx-coverage, subpath)',
    file: 'nodes.js', fn: '_applyTilesToNodeMap', call: '_applyTilesToNodeMap(__insetMap);',
    locals: () => ({ __insetMap: makeMap() }),
    winLocals: () => ({
      getTileUrl: () => SPEC.url,
      getActiveTileProvider: () => ({ attribution: SPEC.attribution, refUrl: SPEC_REF.refUrl, invertFilter: null }),
    }),
    expect: [
      { url: SPEC.url, options: { maxZoom: 18, attribution: SPEC.attribution } },
      { url: SPEC_REF.refUrl, options: { maxZoom: 18 } },
    ],
  },
  {
    name: 'regions map',
    file: 'regions-map.js',
    from: "const theme = document.documentElement.getAttribute('data-theme')",
    to: "map.getPane('tilePane').style.filter = tile && tile.invertFilter ? tile.invertFilter : '';",
    locals: (ctx) => ({ root: ctx.window, map: makeMap(), tileLayer: null }),
    expect: [{ url: SPEC.url, options: { maxZoom: 17, attribution: SPEC.attribution } }],
    stubSpec: true,
  },
  {
    name: 'customizer geo-filter modal preview',
    file: 'customize-v2.js', fn: '_cv2PreviewTile',
    from: 'var _cv2Tile = _cv2PreviewTile();', to: '.addTo(_gfModalMap);', nth: 0,
    locals: () => ({ _gfModalMap: makeMap() }),
    expect: [{ url: SPEC.url, options: { attribution: SPEC.attribution, maxZoom: 17 } }],
    stubSpec: true,
  },
  {
    name: 'customizer geo-filter tab preview',
    file: 'customize-v2.js', fn: '_cv2PreviewTile',
    from: 'var _cv2Tile = _cv2PreviewTile();', to: '.addTo(_gfMap);', nth: 1,
    locals: () => ({ _gfMap: makeMap() }),
    expect: [{ url: SPEC.url, options: { attribution: SPEC.attribution, maxZoom: 17 } }],
    stubSpec: true,
  },
  {
    name: 'home My Mesh health minimap',
    file: 'home.js', from: 'miniMap = L.map(mapEl,', to: '.addTo(miniMap);',
    locals: () => ({ mapEl: { id: 'healthMap' }, miniMap: null, node: { lat: 56, lon: 10 } }),
    expect: [{
      url: 'https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png',
      options: { attribution: OSM_CREDIT },
    }],
  },
];

for (const site of SITES) {
  test('F6 ' + site.name, () => {
    const ctx = siteCtx(null);
    if (site.stubSpec) ctx.window.MC_getTileSpec = ctx.MC_getTileSpec = () => SPEC;
    if (site.winLocals) Object.assign(ctx.window, site.winLocals());
    Object.assign(ctx, site.locals(ctx));
    let code = '';
    if (site.fn) code += extractFn(site.file, site.fn) + '\n';
    if (site.from) code += extract(site.file, site.from, site.to, site.nth) + '\n';
    if (site.call) code += site.call + '\n';
    vm.runInContext(code, ctx, { filename: site.file });
    const got = ctx.__layers.map(l => ({ url: l._url, options: l.options }));
    sameOptions(got, site.expect);
    for (const l of ctx.__layers) assert.strictEqual(l.kind, 'tile', 'today every base map is a tile layer');
  });
}

test('F6 the layer control offers every style with its own credit and ceiling', () => {
  const ctx = makeSandbox({ mapCfg: ALL_ON });
  const { ctl } = buildControl(ctx, true);
  const reg = ctx.window.MC_TILE_PROVIDERS;
  const ids = Object.keys(ctl.base).filter(k => k !== '__auto__');
  assert.deepStrictEqual(ids.sort(), Object.keys(reg).sort(), 'every offered style is selectable');
  for (const id of ids) {
    const p = reg[id], layer = ctl.base[id];
    assert.strictEqual(layer.kind, 'tile', id);
    assert.strictEqual(layer._url, typeof p.url === 'function' ? p.url() : p.url, id);
    sameOptions(layer.options, { attribution: p.attribution, maxZoom: p.maxZoom }, id);
  }
});

test('F6 an explicit pick in the layer control still locks the pane filter', () => {
  const ctx = makeSandbox({ theme: 'dark', mapCfg: ALL_ON });
  const { map, ctl } = buildControl(ctx, true);
  const pane = map.getPane('tilePane');
  let explicit = null, filter = null;
  pane.setAttribute = (k, v) => { if (k === 'data-explicit-layer') explicit = v; };
  map.addLayer(ctl.base['positron-dark']);
  ctl.base['positron-dark'].fire('add');
  filter = pane.style.filter;
  assert.strictEqual(explicit, 'true', 'the pane is marked explicit');
  assert.strictEqual(filter, ctx.window.MC_TILE_PROVIDERS['positron-dark'].invertFilter,
    'the style\'s invert filter is applied by the layer\'s own add hook');
  map.addLayer(ctl.base['carto-light']);
  ctl.base['carto-light'].fire('add');
  assert.strictEqual(pane.style.filter, '', 'a light style clears it');
});

// ── F7: the registry's own invariants ─────────────────────────────────────
console.log('\n── #741 F7: no registry behaviour changed ──');

test('F7 the invert filter still follows theme + selected provider', () => {
  const ctx = makeSandbox({ theme: 'dark', mapCfg: ALL_ON, storage: { 'mc-dark-tile-provider': 'positron-dark' } });
  assert.strictEqual(ctx.window.MC_getTileSpec('dark').invertFilter,
    'invert(1) hue-rotate(180deg) brightness(0.9) contrast(1.05)');
  assert.strictEqual(ctx.window.MC_getTileSpec('light').invertFilter, null);
});

test('F7 the Esri gate still needs the JSON boolean true', () => {
  for (const [label, cfg] of [
    ['missing', { tiles: { providers: {} } }],
    ['string "true"', { tiles: { providers: { esri: { enabled: 'true' } } } }],
    ['wrong case', { tiles: { providers: { Esri: { enabled: true } } } }],
  ]) {
    const ctx = makeSandbox({ mapCfg: cfg });
    const ids = Object.keys(ctx.window.MC_TILE_PROVIDERS).filter(id => id.indexOf('esri-') === 0);
    assert.deepStrictEqual(ids, [], 'esri must stay gated with enabled ' + label);
  }
  const on = makeSandbox({ mapCfg: { tiles: { providers: { esri: { enabled: true } } } } });
  assert.ok(Object.keys(on.window.MC_TILE_PROVIDERS).some(id => id.indexOf('esri-') === 0));
});

test('F7 the legacy raw-URL branch still bypasses the registry', () => {
  const ctx = makeSandbox({ mapCfg: ALL_ON });
  ctx.window.MC_DARK_TILE_URL_OVERRIDE = 'https://operator.example/dark/{z}/{x}/{y}.png';
  const spec = ctx.window.MC_getTileSpec('dark');
  assert.strictEqual(spec.id, null);
  assert.strictEqual(spec.url, 'https://operator.example/dark/{z}/{x}/{y}.png');
  assert.strictEqual(spec.attribution, OSM_CREDIT);
  assert.strictEqual(spec.invertFilter, null);
  const layer = factory(ctx)(spec, { maxZoom: 19 });
  assert.strictEqual(layer._url, spec.url);
  sameOptions(layer.options, { attribution: OSM_CREDIT, maxZoom: 19 });
});

test('F7 the credit builder and the compact class are untouched', () => {
  const ctx = makeSandbox({ mapCfg: ALL_ON });
  assert.strictEqual(ctx.window.MC_CREDIT_COMPACT_CLASS, 'mc-credit-compact');
  assert.strictEqual(typeof ctx.window.MC_buildCredit, 'function');
  assert.strictEqual(typeof ctx.window.MC_ensureAttribution, 'function');
  assert.strictEqual(
    ctx.window.MC_buildCredit('Stub Agency, Stub Dataset, CC BY 4.0', [
      { mark: 'Stub Dataset', href: 'https://example.com/d' },
      { mark: 'CC BY 4.0', href: 'https://example.com/l', rel: 'license' },
    ]),
    'Stub Agency, <a href="https://example.com/d" target="_blank" rel="noopener noreferrer">Stub Dataset</a>, ' +
    '<a href="https://example.com/l" target="_blank" rel="license">CC BY 4.0</a>');
});

console.log('\n═══════════════════════════════════════');
console.log('  #741 M1a base-layer factory: ' + passed + ' passed, ' + failed + ' failed');
console.log('═══════════════════════════════════════');
process.exit(failed ? 1 : 0);
