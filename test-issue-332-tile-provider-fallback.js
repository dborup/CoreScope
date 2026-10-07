/* test-issue-332-tile-provider-fallback.js — #332.
 *
 * Bug: several maps still built their tile URL (and attribution) from a
 * hard-coded CARTO template instead of the operator's configured
 * dark/light provider. Since August 2026 keyless CARTO raster tiles come
 * back HTTP 200 with "API KEY REQUIRED" stamped into the PNG, so those
 * maps render watermarked tiles even on an instance configured for
 * Esri/OpenTopoMap/OSM:
 *
 *   - public/roles.js        window.TILE_DARK / window.TILE_LIGHT were
 *                            CARTO URLs, and getTileUrl() ignored the
 *                            configured LIGHT provider entirely (it
 *                            returned TILE_LIGHT unconditionally).
 *   - public/customize-v2.js both geo-filter preview maps hard-coded
 *                            CARTO light_all.
 *   - public/live.js         the url + attribution fallbacks assumed CARTO.
 *   - public/map.js          same two fallbacks as live.js.
 *
 * Contract under test:
 *   M1  The registry's fallback defaults (no darkDefault/lightDefault in
 *       config) are NON-CARTO, and are always resolvable in REGISTRY so
 *       no caller has to invent a URL of its own.
 *   M2  MC_getTileSpec(type) is the one resolver: it returns a string url
 *       (function-valued `url` entries invoked) plus the provider's own
 *       attribution.
 *   M3  roles.js getTileUrl() honours the configured provider for EACH
 *       provider and for BOTH themes, and its last-resort fallback is
 *       non-CARTO. getActiveTileProvider() resolves in light mode too.
 *   M4  The two customizer preview maps take url + attribution from the
 *       configured light provider (asserted by running the production
 *       L.tileLayer() call sites, not by grepping).
 *   M5  live.js / map.js tile resolvers prefer MC_getTileSpec and their
 *       fallback attribution does not name CartoDB.
 *   M6  The only runtime `basemaps.cartocdn.com` literal left under
 *       public/ is inside map-tile-providers.js _getCartoBase().
 *
 * Runs via: node test-issue-332-tile-provider-fallback.js
 * Pure vm sandbox — no jsdom, no Playwright.
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

const CARTO_HOST = 'cartocdn.com';

function makeStorage() {
  const store = {};
  return {
    getItem(k)    { return Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null; },
    setItem(k, v) { store[k] = String(v); },
    removeItem(k) { delete store[k]; },
    clear()       { for (const k of Object.keys(store)) delete store[k]; },
  };
}

// ─── Sandbox ────────────────────────────────────────────────────────────
// opts.theme      'dark' | 'light'  (document.documentElement data-theme)
// opts.mapCfg     value for window.MC_MAP_CFG, applied BEFORE the registry
//                 self-initialises at parse time.
function makeSandbox(opts) {
  opts = opts || {};
  const tilePane = {
    style: { filter: '' },
    setAttribute: () => {},
    getAttribute: () => null,
    removeAttribute: () => {},
  };
  const ctx = {
    console,
    setTimeout, clearTimeout,
    JSON, Date, Math, Object, Array, String, Number, Boolean, Set, Map,
    fetch: () => Promise.resolve({ ok: false, json: () => Promise.resolve({}) }),
    localStorage: makeStorage(),
    document: {
      documentElement: {
        getAttribute: () => opts.theme || 'dark',
        setAttribute: () => {},
        style: { getPropertyValue: () => '', setProperty: () => {} },
      },
      querySelector: (sel) => (sel === '.leaflet-tile-pane' ? tilePane : null),
      querySelectorAll: () => [],
      getElementById: () => null,
      createElement: () => ({ style: {}, appendChild: () => {}, setAttribute: () => {}, addEventListener: () => {} }),
      addEventListener: () => {},
      body: { appendChild: () => {}, style: {} },
      head: { appendChild: () => {} },
      readyState: 'complete',
    },
    window: {
      addEventListener: () => {},
      dispatchEvent: () => true,
      matchMedia: () => ({ matches: (opts.theme || 'dark') === 'dark', addEventListener: () => {} }),
    },
    CustomEvent: function (type, init) { this.type = type; this.detail = (init && init.detail) || null; },
  };
  ctx.window.localStorage = ctx.localStorage;
  ctx.window.document = ctx.document;
  if (opts.mapCfg !== undefined) ctx.window.MC_MAP_CFG = opts.mapCfg;
  ctx.globalThis = ctx;
  ctx.tilePane = tilePane;
  vm.createContext(ctx);
  return ctx;
}

function loadInto(ctx, relPath) {
  const src = fs.readFileSync(path.join(__dirname, relPath), 'utf8');
  vm.runInContext(src, ctx, { filename: relPath });
  // Mirror window.* back onto sandbox globals so bare-name refs inside the
  // loaded files (TILE_DARK, getTileUrl, …) resolve like they do in a browser.
  for (const k of Object.keys(ctx.window)) if (!(k in ctx)) ctx[k] = ctx.window[k];
}

// Full stack: registry + roles.js, with config already in place.
function loadStack(opts) {
  const ctx = makeSandbox(opts);
  loadInto(ctx, path.join('public', 'map-tile-providers.js'));
  loadInto(ctx, path.join('public', 'roles.js'));
  return ctx;
}

// Brace-matched extraction of `function <name>(...) { … }` from a source file.
function extractFn(relPath, name) {
  const src = fs.readFileSync(path.join(__dirname, relPath), 'utf8');
  const re = new RegExp('function\\s+' + name + '\\s*\\([^)]*\\)\\s*\\{');
  const m = src.match(re);
  if (!m) throw new Error(name + ' definition not found in ' + relPath);
  const start = m.index;
  let depth = 0, i = start;
  for (; i < src.length; i++) {
    if (src[i] === '{') depth++;
    else if (src[i] === '}') { depth--; if (depth === 0) { i++; break; } }
  }
  return src.slice(start, i);
}

// Pull out the statement(s) that attach a tile layer to a named Leaflet map
// in customize-v2.js, so the PRODUCTION call site is what gets executed.
// Matches from the preceding `var _cv2Tile`-style line (if any) through
// `.addTo(<mapVar>);`.
function extractTileAttach(relPath, mapVar) {
  const src = fs.readFileSync(path.join(__dirname, relPath), 'utf8');
  const anchor = '.addTo(' + mapVar + ');';
  // `.addTo(_gfMap);` also appears on markers/polygons, so find the
  // L.tileLayer( call whose own .addTo() is the one we want.
  let from = 0, callStart = -1, end = -1;
  for (;;) {
    const i = src.indexOf('L.tileLayer(', from);
    if (i === -1) break;
    const j = src.indexOf(anchor, i);
    if (j !== -1 && j - i < 400) { callStart = i; end = j + anchor.length; break; }
    from = i + 1;
  }
  if (callStart === -1) throw new Error('no L.tileLayer(...)' + anchor + ' in ' + relPath);
  // Include the statements between the enclosing `L.map(` call and the tile
  // layer — that is where the call site resolves its provider spec.
  const blockStart = src.lastIndexOf('L.map(', callStart);
  const lineStart = src.indexOf('\n', src.indexOf(';', blockStart)) + 1;
  return src.slice(lineStart > 0 && lineStart < callStart ? lineStart : callStart, end);
}

function makeTileLayerSpy() {
  const calls = [];
  const L = {
    tileLayer(url, opts) {
      return { addTo(map) { calls.push({ url, opts, map }); return this; } };
    },
  };
  return { L, calls };
}

const CFG_EMPTY  = { tiles: { providers: {} } };
function cfgDefaults(dark, light) {
  return {
    tiles: {
      darkDefault: dark,
      lightDefault: light,
      providers: {
        carto: { enabled: true, domain: '' },
        opentopomap: { enabled: true },
        usgs: { enabled: true },
      },
    },
  };
}

// ═══ M1 — fallback defaults are non-CARTO and always resolvable ═════════
console.log('\n── #332 M1: registry fallback defaults are non-CARTO ──');

test('M1 dark fallback (no darkDefault configured) is not a CARTO style', () => {
  const ctx = loadStack({ theme: 'dark', mapCfg: CFG_EMPTY });
  const id = ctx.window.MC_getDarkTileProvider();
  const p  = ctx.window.MC_TILE_PROVIDERS[id];
  assert.ok(p, 'fallback dark id ' + JSON.stringify(id) + ' must exist in REGISTRY');
  assert.notStrictEqual(p.provider, 'carto', 'fallback dark provider must not be carto, got id=' + id);
  const url = typeof p.url === 'function' ? p.url() : p.url;
  assert.ok(url.indexOf(CARTO_HOST) === -1, 'fallback dark URL must not hit CARTO: ' + url);
});

test('M1 light fallback (no lightDefault configured) is not a CARTO style', () => {
  const ctx = loadStack({ theme: 'light', mapCfg: CFG_EMPTY });
  const id = ctx.window.MC_getLightTileProvider();
  const p  = ctx.window.MC_TILE_PROVIDERS[id];
  assert.ok(p, 'fallback light id ' + JSON.stringify(id) + ' must exist in REGISTRY');
  assert.notStrictEqual(p.provider, 'carto', 'fallback light provider must not be carto, got id=' + id);
  const url = typeof p.url === 'function' ? p.url() : p.url;
  assert.ok(url.indexOf(CARTO_HOST) === -1, 'fallback light URL must not hit CARTO: ' + url);
});

test('M1 fallback ids stay in REGISTRY even when every optional provider is disabled', () => {
  const ctx = loadStack({
    theme: 'dark',
    mapCfg: { tiles: { providers: {
      carto: { enabled: false }, osm: { enabled: false },
      opentopomap: { enabled: false }, usgs: { enabled: false }, stamen: { enabled: false },
    } } },
  });
  const reg = ctx.window.MC_TILE_PROVIDERS;
  const d = ctx.window.MC_getDarkTileProvider();
  const l = ctx.window.MC_getLightTileProvider();
  assert.ok(reg[d], 'dark fallback ' + d + ' missing from REGISTRY when providers are disabled');
  assert.ok(reg[l], 'light fallback ' + l + ' missing from REGISTRY when providers are disabled');
  assert.strictEqual(reg[d].type, 'dark', d + ' must be a dark style');
  assert.strictEqual(reg[l].type, 'light', l + ' must be a light style');
});

test('M1 a configured darkDefault/lightDefault still wins over the fallback', () => {
  const ctx = loadStack({ theme: 'dark', mapCfg: cfgDefaults('carto-dark', 'carto-light') });
  assert.strictEqual(ctx.window.MC_getDarkTileProvider(), 'carto-dark');
  assert.strictEqual(ctx.window.MC_getLightTileProvider(), 'carto-light');
});

// ═══ M2 — MC_getTileSpec is the single resolver ═════════════════════════
console.log('\n── #332 M2: MC_getTileSpec(type) ──');

test('M2 MC_getTileSpec is exposed', () => {
  const ctx = loadStack({ theme: 'dark', mapCfg: CFG_EMPTY });
  assert.strictEqual(typeof ctx.window.MC_getTileSpec, 'function', 'window.MC_getTileSpec must exist');
});

test('M2 MC_getTileSpec("dark") returns the active dark provider url + attribution', () => {
  const ctx = loadStack({ theme: 'dark', mapCfg: cfgDefaults('esri-darkgray-labels', 'carto-light') });
  const spec = ctx.window.MC_getTileSpec('dark');
  assert.strictEqual(spec.id, 'esri-darkgray-labels');
  assert.strictEqual(typeof spec.url, 'string', 'url must be a string template, got ' + typeof spec.url);
  assert.ok(/World_Dark_Gray_Base/.test(spec.url), 'expected the Esri dark canvas URL, got ' + spec.url);
  assert.strictEqual(spec.attribution, 'Tiles © Esri');
});

test('M2 MC_getTileSpec("light") returns the active LIGHT provider, not the dark one', () => {
  const ctx = loadStack({ theme: 'dark', mapCfg: cfgDefaults('esri-darkgray-labels', 'opentopomap') });
  const spec = ctx.window.MC_getTileSpec('light');
  assert.strictEqual(spec.id, 'opentopomap');
  assert.ok(/tile\.opentopomap\.org/.test(spec.url), 'expected the OpenTopoMap URL, got ' + spec.url);
  assert.ok(/OpenTopoMap/.test(spec.attribution), 'expected the OpenTopoMap attribution, got ' + spec.attribution);
});

test('M2 MC_getTileSpec invokes function-valued url (never leaks the function)', () => {
  const ctx = loadStack({ theme: 'dark', mapCfg: cfgDefaults('carto-dark', 'carto-light') });
  const spec = ctx.window.MC_getTileSpec('dark');
  assert.strictEqual(typeof spec.url, 'string');
  assert.ok(/\{z\}/.test(spec.url) && /\{x\}/.test(spec.url) && /\{y\}/.test(spec.url),
    'url must keep the leaflet placeholders, got ' + spec.url);
});

test('M2 MC_getTileSpec carries invertFilter + maxZoom from the provider', () => {
  const ctx = loadStack({ theme: 'dark', mapCfg: cfgDefaults('positron-dark', 'opentopomap') });
  const dark = ctx.window.MC_getTileSpec('dark');
  assert.ok(dark.invertFilter && /invert\(1\)/.test(dark.invertFilter), 'positron-dark inverts');
  const light = ctx.window.MC_getTileSpec('light');
  assert.strictEqual(light.maxZoom, 17, 'OpenTopoMap caps at zoom 17');
});

test('M2 MC_getTileSpec never returns a CARTO attribution for a non-CARTO provider', () => {
  const ctx = loadStack({ theme: 'light', mapCfg: cfgDefaults('esri-darkgray-labels', 'usgs-topo') });
  const spec = ctx.window.MC_getTileSpec('light');
  assert.ok(!/Carto/i.test(spec.attribution), 'USGS attribution must not name Carto: ' + spec.attribution);
});

// ═══ M3 — roles.js getTileUrl() per provider + both themes + fallback ══
console.log('\n── #332 M3: roles.js getTileUrl() ──');

// One case per provider family, dark and light, driven through real config.
const PROVIDER_CASES = [
  { theme: 'dark',  dark: 'carto-dark',            light: 'carto-light', match: /basemaps\.cartocdn\.com\/dark_all/ },
  { theme: 'dark',  dark: 'carto-voyager-dark',    light: 'carto-light', match: /rastertiles\/voyager/ },
  { theme: 'dark',  dark: 'positron-dark',         light: 'carto-light', match: /light_all/ },
  { theme: 'dark',  dark: 'esri-darkgray-labels',  light: 'carto-light', match: /World_Dark_Gray_Base/ },
  { theme: 'light', dark: 'carto-dark',            light: 'carto-light', match: /basemaps\.cartocdn\.com\/light_all/ },
  { theme: 'light', dark: 'carto-dark',            light: 'carto-voyager', match: /rastertiles\/voyager/ },
  { theme: 'light', dark: 'carto-dark',            light: 'opentopomap', match: /tile\.opentopomap\.org/ },
  { theme: 'light', dark: 'carto-dark',            light: 'usgs-topo',   match: /basemap\.nationalmap\.gov.*USGSTopo/ },
  { theme: 'light', dark: 'carto-dark',            light: 'usgs-imagery', match: /USGSImageryTopo/ },
];

for (const c of PROVIDER_CASES) {
  test('M3 ' + c.theme + ' + ' + (c.theme === 'dark' ? c.dark : c.light) + ' → getTileUrl() returns that provider’s URL', () => {
    const ctx = loadStack({ theme: c.theme, mapCfg: cfgDefaults(c.dark, c.light) });
    const url = ctx.window.getTileUrl();
    assert.strictEqual(typeof url, 'string', 'getTileUrl() must return a string, got ' + typeof url);
    assert.ok(c.match.test(url), 'expected ' + c.match + ', got ' + url);
  });
}

test('M3 OSM provider (token-upgraded) reaches getTileUrl() in light mode', () => {
  const ctx = loadStack({
    theme: 'light',
    mapCfg: { tiles: { darkDefault: 'osm-dark', lightDefault: 'osm-standard', providers: {
      osm: { enabled: true, provider: 'maptiler', token: 'T0K' },
    } } },
  });
  const url = ctx.window.getTileUrl();
  assert.ok(/api\.maptiler\.com/.test(url), 'expected the MapTiler URL, got ' + url);
  assert.ok(/key=T0K/.test(url), 'token must be carried through, got ' + url);
});

test('M3 stamen provider reaches getTileUrl() in light mode', () => {
  const ctx = loadStack({
    theme: 'light',
    mapCfg: { tiles: { lightDefault: 'stamen-toner-lite', providers: {
      stamen: { enabled: true, token: 'S1' },
    } } },
  });
  const url = ctx.window.getTileUrl();
  assert.ok(/stadiamaps\.com/.test(url), 'expected the Stadia/Stamen URL, got ' + url);
});

test('M3 FALLBACK: no registry at all, dark theme → non-CARTO URL', () => {
  const ctx = makeSandbox({ theme: 'dark' });
  loadInto(ctx, path.join('public', 'roles.js'));   // roles.js alone — no registry
  const url = ctx.window.getTileUrl();
  assert.strictEqual(typeof url, 'string');
  assert.ok(url.indexOf(CARTO_HOST) === -1, 'dark fallback must not hit CARTO: ' + url);
  assert.ok(/\{z\}/.test(url), 'fallback must still be a tile template: ' + url);
});

test('M3 FALLBACK: no registry at all, light theme → non-CARTO URL', () => {
  const ctx = makeSandbox({ theme: 'light' });
  loadInto(ctx, path.join('public', 'roles.js'));
  const url = ctx.window.getTileUrl();
  assert.ok(url.indexOf(CARTO_HOST) === -1, 'light fallback must not hit CARTO: ' + url);
  assert.ok(/\{z\}/.test(url), 'fallback must still be a tile template: ' + url);
});

test('M3 FALLBACK: window.TILE_DARK / TILE_LIGHT are not CARTO templates', () => {
  const ctx = makeSandbox({ theme: 'dark' });
  loadInto(ctx, path.join('public', 'roles.js'));
  assert.ok(ctx.window.TILE_DARK.indexOf(CARTO_HOST) === -1, 'TILE_DARK: ' + ctx.window.TILE_DARK);
  assert.ok(ctx.window.TILE_LIGHT.indexOf(CARTO_HOST) === -1, 'TILE_LIGHT: ' + ctx.window.TILE_LIGHT);
});

test('M3 FALLBACK: a registry whose active style has no url still degrades non-CARTO', () => {
  const ctx = loadStack({ theme: 'dark', mapCfg: CFG_EMPTY });
  ctx.window.MC_getTileSpec = function () { return { id: 'broken', url: null, attribution: '' }; };
  ctx.window.MC_TILE_PROVIDERS = { broken: { provider: 'x', type: 'dark' } };
  ctx.window.MC_getDarkTileProvider = function () { return 'broken'; };
  const url = ctx.window.getTileUrl();
  assert.ok(url.indexOf(CARTO_HOST) === -1, 'degraded path must not hit CARTO: ' + url);
});

test('M3 legacy registry shape (no MC_getTileSpec) still honours the LIGHT provider', () => {
  const ctx = makeSandbox({ theme: 'light' });
  loadInto(ctx, path.join('public', 'roles.js'));
  ctx.window.MC_TILE_PROVIDERS = {
    'legacy-light': { provider: 'esri', type: 'light', url: function () { return 'https://legacy.example/{z}/{x}/{y}.png'; } },
  };
  ctx.window.MC_getLightTileProvider = function () { return 'legacy-light'; };
  const url = ctx.window.getTileUrl();
  assert.strictEqual(url, 'https://legacy.example/{z}/{x}/{y}.png',
    'light mode must consult MC_getLightTileProvider, got ' + url);
});

test('M3 getActiveTileProvider() resolves the configured LIGHT provider in light mode', () => {
  const ctx = loadStack({ theme: 'light', mapCfg: cfgDefaults('carto-dark', 'opentopomap') });
  const p = ctx.window.getActiveTileProvider();
  assert.ok(p, 'expected a provider object in light mode, got ' + JSON.stringify(p));
  assert.strictEqual(p.type, 'light');
  assert.ok(/OpenTopoMap/.test(p.attribution), 'attribution must come from the provider, got ' + p.attribution);
});

test('M3 getActiveTileProvider() still resolves the dark provider in dark mode', () => {
  const ctx = loadStack({ theme: 'dark', mapCfg: cfgDefaults('esri-darkgray-labels', 'carto-light') });
  const p = ctx.window.getActiveTileProvider();
  assert.ok(p && p.type === 'dark', 'expected the dark provider, got ' + JSON.stringify(p));
  assert.strictEqual(p.attribution, 'Tiles © Esri');
});

// ═══ M4 — customizer preview maps ══════════════════════════════════════
console.log('\n── #332 M4: customizer geo-filter preview maps ──');

const CV2 = path.join('public', 'customize-v2.js');

function runPreviewHelper(spec) {
  const ctx = makeSandbox({ theme: 'light' });
  ctx.window.TILE_LIGHT = 'https://fallback.example/{z}/{x}/{y}.png';
  if (spec) ctx.window.MC_getTileSpec = function () { return spec; };
  vm.runInContext(extractFn(CV2, '_cv2PreviewTile') + '\nvar __out = _cv2PreviewTile();', ctx, { filename: CV2 });
  return ctx.__out;
}

test('M4 _cv2PreviewTile() takes url + attribution + maxZoom from the light provider', () => {
  const out = runPreviewHelper({ id: 'opentopomap', url: 'https://a.tile.opentopomap.org/{z}/{x}/{y}.png', attribution: '© OpenTopoMap', maxZoom: 17 });
  assert.strictEqual(out.url, 'https://a.tile.opentopomap.org/{z}/{x}/{y}.png');
  assert.strictEqual(out.attribution, '© OpenTopoMap');
  assert.strictEqual(out.maxZoom, 17);
});

test('M4 _cv2PreviewTile() falls back to the non-CARTO TILE_LIGHT when no registry is present', () => {
  const out = runPreviewHelper(null);
  assert.strictEqual(out.url, 'https://fallback.example/{z}/{x}/{y}.png');
  assert.ok(!/Carto/i.test(out.attribution), 'fallback attribution must not name Carto: ' + out.attribution);
});

// Execute the PRODUCTION call sites so re-hardcoding a URL there fails here.
for (const [label, mapVar] of [['geo-filter tab', '_gfMap'], ['geo-filter modal', '_gfModalMap']]) {
  test('M4 ' + label + ' preview map attaches the configured light provider', () => {
    const ctx = makeSandbox({ theme: 'light' });
    const spy = makeTileLayerSpy();
    ctx.L = spy.L;
    ctx.window.TILE_LIGHT = 'https://fallback.example/{z}/{x}/{y}.png';
    ctx.window.MC_getTileSpec = function (t) {
      assert.strictEqual(t, 'light', 'preview maps must ask for the LIGHT provider, got ' + t);
      return { id: 'opentopomap', url: 'https://a.tile.opentopomap.org/{z}/{x}/{y}.png', attribution: '© OpenTopoMap', maxZoom: 17 };
    };
    ctx[mapVar] = { __name: mapVar };
    const code = extractFn(CV2, '_cv2PreviewTile') + '\n' + extractTileAttach(CV2, mapVar);
    vm.runInContext(code, ctx, { filename: CV2 });
    assert.strictEqual(spy.calls.length, 1, 'expected exactly one tile layer, got ' + spy.calls.length);
    const call = spy.calls[0];
    assert.ok(call.url.indexOf(CARTO_HOST) === -1, label + ' must not request CARTO tiles: ' + call.url);
    assert.strictEqual(call.url, 'https://a.tile.opentopomap.org/{z}/{x}/{y}.png');
    assert.strictEqual(call.opts.attribution, '© OpenTopoMap');
    assert.strictEqual(call.opts.maxZoom, 17);
  });
}

// ═══ M5 — live.js / map.js resolvers ═══════════════════════════════════
console.log('\n── #332 M5: live.js / map.js tile resolvers ──');

const RESOLVERS = [
  { file: path.join('public', 'live.js'), fn: '_liveResolveTile' },
  { file: path.join('public', 'map.js'),  fn: '_resolveTileUrl'  },
];

function runResolver(rel, fnName, setup) {
  const ctx = makeSandbox({ theme: 'dark' });
  ctx.window.TILE_DARK  = 'https://fallback.example/dark/{z}/{x}/{y}.png';
  ctx.window.TILE_LIGHT = 'https://fallback.example/light/{z}/{x}/{y}.png';
  if (setup) setup(ctx);
  for (const k of Object.keys(ctx.window)) if (!(k in ctx)) ctx[k] = ctx.window[k];
  vm.runInContext(extractFn(rel, fnName) + '\nvar __fn = ' + fnName + ';', ctx, { filename: rel });
  return ctx.__fn;
}

for (const r of RESOLVERS) {
  test('M5 ' + r.file + ' ' + r.fn + '(): no registry, light → attribution does not name CartoDB', () => {
    const out = runResolver(r.file, r.fn)(false);
    assert.ok(!/Carto/i.test(out.attribution), 'got ' + out.attribution);
    assert.ok(out.url.indexOf(CARTO_HOST) === -1, 'got ' + out.url);
  });

  test('M5 ' + r.file + ' ' + r.fn + '(): no registry, dark → attribution does not name CartoDB', () => {
    const out = runResolver(r.file, r.fn)(true);
    assert.ok(!/Carto/i.test(out.attribution), 'got ' + out.attribution);
    assert.ok(out.url.indexOf(CARTO_HOST) === -1, 'got ' + out.url);
  });

  test('M5 ' + r.file + ' ' + r.fn + '(): prefers MC_getTileSpec for both themes', () => {
    const seen = [];
    const fn = runResolver(r.file, r.fn, (ctx) => {
      ctx.window.MC_getTileSpec = function (t) {
        seen.push(t);
        return { id: 'x-' + t, url: 'https://spec.example/' + t + '/{z}/{x}/{y}.png', attribution: 'ATTR-' + t, refUrl: null, maxZoom: 19 };
      };
    });
    const d = fn(true), l = fn(false);
    assert.deepStrictEqual(seen, ['dark', 'light'], 'resolver must pass the theme through, saw ' + JSON.stringify(seen));
    assert.strictEqual(d.url, 'https://spec.example/dark/{z}/{x}/{y}.png');
    assert.strictEqual(d.attribution, 'ATTR-dark');
    assert.strictEqual(l.url, 'https://spec.example/light/{z}/{x}/{y}.png');
    assert.strictEqual(l.attribution, 'ATTR-light');
  });

  test('M5 ' + r.file + ' ' + r.fn + '(): legacy registry — picks the accessor matching the theme', () => {
    const fn = runResolver(r.file, r.fn, (ctx) => {
      // No MC_getTileSpec: a partial/older registry exposing only accessors.
      ctx.window.MC_TILE_PROVIDERS = {
        'd1': { type: 'dark',  url: 'https://dark.example/{z}/{x}/{y}.png',  attribution: 'ATTR-D' },
        'l1': { type: 'light', url: 'https://light.example/{z}/{x}/{y}.png', attribution: 'ATTR-L' },
      };
      ctx.window.MC_getDarkTileProvider  = function () { return 'd1'; };
      ctx.window.MC_getLightTileProvider = function () { return 'l1'; };
    });
    const d = fn(true), l = fn(false);
    assert.strictEqual(d.url, 'https://dark.example/{z}/{x}/{y}.png', 'dark must use MC_getDarkTileProvider');
    assert.strictEqual(d.attribution, 'ATTR-D');
    assert.strictEqual(l.url, 'https://light.example/{z}/{x}/{y}.png', 'light must use MC_getLightTileProvider');
    assert.strictEqual(l.attribution, 'ATTR-L');
  });

  test('M5 ' + r.file + ' ' + r.fn + '(): carries the provider refUrl through', () => {
    const fn = runResolver(r.file, r.fn, (ctx) => {
      ctx.window.MC_getTileSpec = function () {
        return { id: 'esri', url: 'https://e/{z}/{x}/{y}', attribution: 'Tiles © Esri', refUrl: 'https://e/ref/{z}/{x}/{y}', maxZoom: 19 };
      };
    });
    assert.strictEqual(fn(true).refUrl, 'https://e/ref/{z}/{x}/{y}');
  });
}

// ═══ M6 — no stray CARTO literal under public/ ═════════════════════════
console.log('\n── #332 M6: no hard-coded CARTO host outside _getCartoBase() ──');

test('M6 the only basemaps.cartocdn.com literal under public/ is in _getCartoBase()', () => {
  const dir = path.join(__dirname, 'public');
  const offenders = [];
  (function walk(d) {
    for (const name of fs.readdirSync(d)) {
      const p = path.join(d, name);
      const st = fs.statSync(p);
      if (st.isDirectory()) { walk(p); continue; }
      if (!/\.(js|html)$/.test(name)) continue;
      const rel = path.relative(__dirname, p);
      const src = fs.readFileSync(p, 'utf8');
      src.split('\n').forEach((line, i) => {
        if (line.indexOf('basemaps.cartocdn.com') === -1) return;
        if (rel === path.join('public', 'map-tile-providers.js') && /_getCartoBase/.test(line)) return;
        offenders.push(rel + ':' + (i + 1) + ': ' + line.trim().slice(0, 100));
      });
    }
  })(dir);
  assert.deepStrictEqual(offenders, [],
    'hard-coded CARTO host(s) found:\n    ' + offenders.join('\n    '));
});

// ─── Summary ────────────────────────────────────────────────────────────
console.log('\n' + (failed === 0 ? '✅' : '❌') + ' #332: ' + passed + ' passed, ' + failed + ' failed');
process.exit(failed === 0 ? 0 : 1);
