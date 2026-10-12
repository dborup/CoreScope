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

// ═══ M7 — analytics subpath minimap (round-2 F1) ════════════════════════
// The Analytics -> Subpaths detail minimap used `L.tileLayer(getTileUrl())`
// and never applied the provider's invertFilter. With the #332 dark default
// (osm-dark = the LIGHT OSM template + an invert filter) that rendered an
// un-inverted light basemap inside a dark page. It must go through the same
// shared helper every other inset map uses, not a second resolver of its own.
console.log('\n── #332 M7: analytics subpath minimap uses the shared tile helper ──');

const ANALYTICS = path.join('public', 'analytics.js');
const NODES     = path.join('public', 'nodes.js');
const INVERT_RE = /invert\(/;

// A Leaflet mock whose maps each own their own tilePane, so an assertion on
// "this map's pane" cannot be satisfied by some other map's pane.
function makeMapMock() {
  const calls = [];
  const panes = [];
  function fakeMap() {
    const tilePane = { style: { filter: '' } };
    panes.push(tilePane);
    return {
      getPane: (n) => (n === 'tilePane' ? tilePane : null),
      fitBounds() { return this; },
      setView() { return this; },
      on() { return this; },
    };
  }
  const L = {
    map() { return fakeMap(); },
    tileLayer(url, opts) {
      return { addTo(map) { calls.push({ url, opts, map }); return this; } };
    },
    circleMarker() { return { bindTooltip() { return this; }, addTo() { return this; } }; },
    polyline()     { return { addTo() { return this; } }; },
    marker()       { return { addTo() { return { bindPopup() {} }; } }; },
    latLngBounds() { return { pad() { return this; } }; },
  };
  return { L, calls, panes };
}

const SUBPATH_DATA = {
  nodes: [
    { name: 'A', lat: 37.5, lon: -122.3 },
    { name: 'B', lat: 37.9, lon: -122.1 },
  ],
  hops: ['33', '67'],
  totalMatches: 26,
  hourDistribution: new Array(24).fill(1),
  signal: { avgSnr: 5, avgRssi: -95, samples: 3 },
  observers: [],
  parentPaths: [],
  firstSeen: '2026-10-01T00:00:00Z',
  lastSeen: '2026-10-02T00:00:00Z',
};

// Executes the PRODUCTION renderSubpathDetail() with the PRODUCTION shared
// helper (extracted from nodes.js) on top of the real registry + roles.js.
function runSubpathDetail(opts) {
  const ctx = loadStack(opts);
  const mock = makeMapMock();
  ctx.L = mock.L;
  // The real shared helper, exported the way nodes.js exports it.
  vm.runInContext(
    extractFn(NODES, '_applyTilesToNodeMap') +
    '\nwindow._applyTilesToNodeMap = _applyTilesToNodeMap;',
    ctx, { filename: NODES });
  for (const k of Object.keys(ctx.window)) if (!(k in ctx)) ctx[k] = ctx.window[k];
  // Stubs for the view helpers renderSubpathDetail() reaches for.
  ctx.esc = (s) => String(s == null ? '' : s);
  ctx.formatDistance = (km) => km.toFixed(1) + ' km';
  ctx.statusGreen = () => '#0f0';
  ctx.statusRed = () => '#f00';
  ctx.statusYellow = () => '#ff0';
  ctx.__panel = { innerHTML: '', classList: { remove: () => {}, add: () => {} } };
  ctx.__data = JSON.parse(JSON.stringify(SUBPATH_DATA));
  vm.runInContext(
    extractFn(ANALYTICS, 'renderSubpathDetail') +
    '\nrenderSubpathDetail(__panel, __data);',
    ctx, { filename: ANALYTICS });
  assert.strictEqual(mock.calls.length >= 1, true,
    'the minimap attached no tile layer at all (' + mock.calls.length + ' calls)');
  // The minimap is the last map created, so its pane is the last recorded one.
  return { call: mock.calls[mock.calls.length - 1], pane: mock.panes[mock.panes.length - 1], calls: mock.calls };
}

test('M7 subpath minimap: dark + built-in default → OSM url AND the invert filter on its own pane', () => {
  const r = runSubpathDetail({ theme: 'dark', mapCfg: CFG_EMPTY });
  assert.ok(r.call.url.indexOf(CARTO_HOST) === -1, 'must not request CARTO tiles: ' + r.call.url);
  assert.ok(/tile\.openstreetmap\.org/.test(r.call.url), 'expected the keyless OSM template, got ' + r.call.url);
  assert.ok(INVERT_RE.test(r.pane.style.filter),
    'osm-dark is the LIGHT OSM template plus an invert filter — without the filter the ' +
    'minimap renders a light basemap in a dark page. Pane filter was ' + JSON.stringify(r.pane.style.filter));
});

test('M7 subpath minimap: light mode → the light provider and NO invert filter', () => {
  const r = runSubpathDetail({ theme: 'light', mapCfg: CFG_EMPTY });
  assert.ok(r.call.url.indexOf(CARTO_HOST) === -1, 'must not request CARTO tiles: ' + r.call.url);
  assert.ok(/tile\.openstreetmap\.org/.test(r.call.url), 'got ' + r.call.url);
  assert.strictEqual(r.pane.style.filter, '', 'a light style must not be inverted');
});

test('M7 subpath minimap: follows a configured dark provider (Esri, natively dark → no filter)', () => {
  const r = runSubpathDetail({ theme: 'dark', mapCfg: cfgDefaults('esri-darkgray-labels', 'opentopomap') });
  assert.ok(/server\.arcgisonline\.com/.test(r.call.url), 'expected the configured Esri url, got ' + r.call.url);
  assert.strictEqual(r.pane.style.filter, '', 'esri-darkgray-labels is natively dark — no invert filter');
  assert.strictEqual(r.call.opts.attribution, 'Tiles © Esri');
});

test('M7 subpath minimap: follows a configured light provider', () => {
  const r = runSubpathDetail({ theme: 'light', mapCfg: cfgDefaults('esri-darkgray-labels', 'opentopomap') });
  assert.ok(/tile\.opentopomap\.org/.test(r.call.url), 'expected the configured OpenTopoMap url, got ' + r.call.url);
  assert.ok(/OpenTopoMap/.test(r.call.opts.attribution), 'got ' + r.call.opts.attribution);
});

test('M7 analytics.js has no tile resolver of its own left', () => {
  const src = fs.readFileSync(path.join(__dirname, ANALYTICS), 'utf8');
  const offenders = [];
  src.split('\n').forEach((line, i) => {
    if (/^\s*(\/\/|\*|\/\*)/.test(line)) return; // comments explain the old shape
    if (/L\.tileLayer\s*\(\s*(window\.)?getTileUrl\s*\(/.test(line) ||
        /L\.tileLayer\s*\(\s*(window\.)?MC_TILE_PROVIDERS\b/.test(line)) {
      offenders.push((i + 1) + ': ' + line.trim());
    }
  });
  assert.deepStrictEqual(offenders, [],
    'analytics.js must resolve tiles through the shared helper, not inline:\n    ' + offenders.join('\n    '));
});

// ═══ M8 — OSM attribution on the new default (round-2 F3) ══════════════
// osm-standard / osm-dark credited "Maps © Mapbox/Thunderforest/MapTiler"
// unconditionally. #332 promotes them to the default every unconfigured
// instance shows, and such an instance uses none of those three vendors.
console.log('\n── #332 M8: OSM attribution matches the vendor actually in use ──');

function osmCfg(provider, token) {
  const osm = { enabled: true };
  if (provider) osm.provider = provider;
  if (token) osm.token = token;
  return { tiles: { providers: { carto: { enabled: true, domain: '' }, osm: osm } } };
}

const VENDORS = ['Mapbox', 'Thunderforest', 'MapTiler'];

for (const [label, type, theme] of [['osm-standard', 'light', 'light'], ['osm-dark', 'dark', 'dark']]) {
  test('M8 ' + label + ': keyless install credits plain OpenStreetMap only', () => {
    const ctx = loadStack({ theme, mapCfg: CFG_EMPTY });
    const spec = ctx.window.MC_getTileSpec(type);
    assert.strictEqual(spec.id, label, 'expected the built-in default, got ' + spec.id);
    assert.ok(/OpenStreetMap/.test(spec.attribution), 'got ' + spec.attribution);
    for (const v of VENDORS) {
      assert.ok(spec.attribution.indexOf(v) === -1,
        'a keyless install uses no ' + v + ' service, so it must not credit one: ' + spec.attribution);
    }
  });
}

for (const [provider, credited] of [['maptiler', 'MapTiler'], ['thunderforest', 'Thunderforest'], ['mapbox', 'Mapbox']]) {
  test('M8 osm with a ' + provider + ' token credits ' + credited + ' and nobody else', () => {
    const ctx = loadStack({ theme: 'light', mapCfg: osmCfg(provider, 'tok') });
    const spec = ctx.window.MC_getTileSpec('light');
    assert.strictEqual(spec.id, 'osm-standard');
    assert.ok(spec.attribution.indexOf(credited) !== -1,
      'the configured vendor must be credited: ' + spec.attribution);
    for (const v of VENDORS) {
      if (v === credited) continue;
      assert.ok(spec.attribution.indexOf(v) === -1,
        v + ' is not in use, so it must not be credited: ' + spec.attribution);
    }
    assert.ok(/OpenStreetMap/.test(spec.attribution), 'OSM data credit is still required: ' + spec.attribution);
  });
}

test('M8 osm with a provider but NO token credits plain OpenStreetMap (no tiles come from the vendor)', () => {
  const ctx = loadStack({ theme: 'light', mapCfg: osmCfg('maptiler', '') });
  const spec = ctx.window.MC_getTileSpec('light');
  assert.ok(/openstreetmap\.org/.test(spec.url), 'tokenless config still uses the OSMF tiles: ' + spec.url);
  for (const v of VENDORS) {
    assert.ok(spec.attribution.indexOf(v) === -1, 'got ' + spec.attribution);
  }
});

// ═══ M9 — documented zoom matches the declared maxZoom (round-2 F4) ════
console.log('\n── #332 M9: the documented default zoom is the real one ──');

test('M9 map-tile-providers.js and config.example.json document the OSM default\'s real maxZoom', () => {
  const ctx = loadStack({ theme: 'light', mapCfg: CFG_EMPTY });
  const real = ctx.window.MC_TILE_PROVIDERS['osm-standard'].maxZoom;
  assert.strictEqual(ctx.window.MC_getTileSpec('light').maxZoom, real);

  const js = fs.readFileSync(path.join(__dirname, 'public', 'map-tile-providers.js'), 'utf8');
  const head = js.slice(0, js.indexOf('var DEFAULT_ID'));
  const mJs = head.match(/tiles to zoom (\d+)/);
  assert.ok(mJs, 'the #332 comment must still state the default\'s zoom ceiling');
  assert.strictEqual(Number(mJs[1]), real,
    'the comment says zoom ' + mJs[1] + ' but osm-standard declares maxZoom ' + real);

  const cfg = JSON.parse(fs.readFileSync(path.join(__dirname, 'config.example.json'), 'utf8'));
  const note = cfg.map.tiles._comment_defaults;
  const mCfg = note.match(/tiles to zoom (\d+)/);
  assert.ok(mCfg, 'config.example.json must still document the default\'s zoom ceiling');
  assert.strictEqual(Number(mCfg[1]), real,
    'config.example.json says zoom ' + mCfg[1] + ' but osm-standard declares maxZoom ' + real);
});

// ═══ M10 — no CARTO id left as a frontend default (round-2 F5) ═════════
console.log('\n── #332 M10: the tile picker has no CARTO id fallback ──');

test('M10 _renderTileProviderSelector() marks no CARTO style selected when the accessors are missing', () => {
  const ctx = makeSandbox({ theme: 'dark' });
  ctx.esc = (s) => String(s == null ? '' : s);
  ctx.escAttr = (s) => String(s == null ? '' : s);
  // A registry is present (so the function does not bail), but the
  // MC_get*TileProvider accessors are not — the branch that used to fall
  // back to the ids 'carto-dark' / 'carto-light'.
  ctx.window.MC_TILE_PROVIDERS = {
    'carto-dark':   { type: 'dark',  label: 'Carto Dark' },
    'osm-dark':     { type: 'dark',  label: 'OSM Standard' },
    'carto-light':  { type: 'light', label: 'Carto Positron' },
    'osm-standard': { type: 'light', label: 'OSM Standard' },
  };
  ctx.window.MC_DARK_TILE_DEFAULT = 'osm-dark';
  ctx.window.MC_LIGHT_TILE_DEFAULT = 'osm-standard';
  for (const k of Object.keys(ctx.window)) if (!(k in ctx)) ctx[k] = ctx.window[k];
  vm.runInContext(
    extractFn(CV2, '_renderTileProviderSelector') + '\nvar __html = _renderTileProviderSelector();',
    ctx, { filename: CV2 });
  const html = String(ctx.__html);
  const selected = (html.match(/<option value="([^"]+)" selected>/g) || [])
    .map((s) => s.match(/value="([^"]+)"/)[1]);
  assert.ok(selected.length > 0, 'expected the picker to preselect something, got: ' + html.slice(0, 200));
  for (const id of selected) {
    assert.ok(!/^carto/.test(id),
      'a CARTO style must not be the picker\'s fallback default any more, got ' + id);
  }
});

test('M10 customize-v2.js has no hard-coded carto-* id left', () => {
  const src = fs.readFileSync(path.join(__dirname, CV2), 'utf8');
  const offenders = [];
  src.split('\n').forEach((line, i) => {
    if (/['"]carto-(dark|light)['"]/.test(line)) offenders.push((i + 1) + ': ' + line.trim());
  });
  assert.deepStrictEqual(offenders, [],
    'hard-coded CARTO provider id(s) in the frontend:\n    ' + offenders.join('\n    '));
});

// #364: exercise the real async client-config loader, with browser-like
// window/global identity. A registry fallback is not an explicit choice.
async function configuredStack(config, theme, storedLight) {
  const ctx = makeSandbox({ theme: theme || 'light' });
  Object.assign(ctx, ctx.window);
  ctx.window = ctx;
  if (storedLight) ctx.localStorage.setItem('mc-light-tile-provider', storedLight);
  ctx.fetch = () => Promise.resolve({ json: () => Promise.resolve(config) });
  loadInto(ctx, path.join('public', 'roles.js'));
  loadInto(ctx, path.join('public', 'map-tile-providers.js'));
  await ctx.MeshConfigReady;
  return ctx;
}
async function asyncTest(name, fn) {
  try { await fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}
async function legacyLightTests() {
  const url = 'https://private.example/tiles/{z}/{x}/{y}.png';
  for (const [label, config] of [
    ['tiles.light', { tiles: { light: url } }],
    ['map.tiles.lightUrl', { map: { tiles: { lightUrl: url } } }],
  ]) {
    await asyncTest('#364 ' + label + ' overrides the implicit light fallback', async () => {
      const ctx = await configuredStack(config);
      assert.strictEqual(ctx.TILE_LIGHT, url, 'the config loader actually ran');
      assert.strictEqual(ctx.getTileUrl(), url);
      const spec = ctx.MC_getTileSpec('light');
      assert.strictEqual(spec.url, url, 'every resolver sees the same override');
      assert.strictEqual(spec.invertFilter, null);
      assert.strictEqual(spec.refUrl, null);
      assert.strictEqual(ctx.getActiveTileProvider(), null, 'an unknown vendor is not an OSM registry provider');
      // Synthetic unused metadata makes borrowing the fallback provider's
      // credit, reference overlay or inversion observable in the real helper.
      Object.assign(ctx.MC_TILE_PROVIDERS['osm-standard'], {
        attribution: 'UNUSED VENDOR', refUrl: 'https://unused.example/labels/{z}/{x}/{y}', invertFilter: 'invert(1)',
      });
      const spy = makeTileLayerSpy();
      ctx.L = spy.L;
      const pane = { style: { filter: 'stale filter' } };
      ctx.__map = { getPane: () => pane };
      vm.runInContext(extractFn('public/nodes.js', '_applyTilesToNodeMap') + '\n_applyTilesToNodeMap(__map);', ctx);
      assert.strictEqual(spy.calls.length, 1, 'an unknown vendor must not borrow a registry reference overlay');
      assert.strictEqual(spy.calls[0].url, url);
      assert.strictEqual(spy.calls[0].opts.attribution, '© OpenStreetMap contributors', 'preserve the historical generic fallback credit');
      assert.strictEqual(pane.style.filter, '');
    });
    for (const stored of ['carto-light', 'osm-standard']) {
      await asyncTest('#364 browser choice ' + stored + ' wins over ' + label, async () => {
        const ctx = await configuredStack(config, 'light', stored);
        assert.strictEqual(ctx.MC_getTileSpec('light').id, stored);
        assert.notStrictEqual(ctx.getTileUrl(), url);
        assert.ok(ctx.getActiveTileProvider());
      });
    }
    await asyncTest('#364 invalid browser choice falls back to ' + label, async () => {
      const ctx = await configuredStack(config, 'light', 'missing-provider');
      assert.strictEqual(ctx.getTileUrl(), url);
    });
    await asyncTest('#364 ' + label + ' never replaces dark-provider tiles or their filter', async () => {
      const ctx = await configuredStack(config, 'dark');
      const spec = ctx.MC_getTileSpec('dark');
      assert.strictEqual(spec.id, 'osm-dark');
      assert.notStrictEqual(ctx.getTileUrl(), url);
      assert.ok(spec.invertFilter);
      ctx.MC_applyTileFilter();
      assert.strictEqual(ctx.tilePane.style.filter, spec.invertFilter);
    });
  }
  await asyncTest('#364 configured provider default wins over legacy URL; browser choice wins over both', async () => {
    const config = { map: { tiles: { lightUrl: url, lightDefault: 'opentopomap', providers: { opentopomap: { enabled: true } } } } };
    const ctx = await configuredStack(config);
    assert.strictEqual(ctx.MC_getTileSpec('light').id, 'opentopomap');
    assert.ok(ctx.getTileUrl().includes('tile.opentopomap.org'));
    ctx.MC_setLightTileProvider('osm-standard');
    assert.strictEqual(ctx.MC_getTileSpec('light').id, 'osm-standard');
    assert.ok(ctx.getTileUrl().includes('tile.openstreetmap.org'));
  });
  await asyncTest('#364 unavailable configured provider and disabled cached provider recover to legacy URL', async () => {
    const ctx = await configuredStack({ map: { tiles: { lightUrl: url, lightDefault: 'missing-provider', providers: { opentopomap: { enabled: false } } } } }, 'light', 'opentopomap');
    assert.strictEqual(ctx.getTileUrl(), url);
  });
  await asyncTest('#364 explicit programmatic server default wins over legacy URL', async () => {
    const ctx = await configuredStack({ tiles: { light: url } });
    ctx.MC_setServerDefaultLightTileProvider('carto-light');
    assert.strictEqual(ctx.MC_getTileSpec('light').id, 'carto-light');
    assert.ok(ctx.getTileUrl().includes('cartocdn.com'));
  });
  await asyncTest('#364 custom URL keeps generic attribution even when an unused OSM vendor is configured', async () => {
    const ctx = await configuredStack({ map: { tiles: { lightUrl: url, providers: { osm: { enabled: true, provider: 'maptiler', token: 'synthetic-token' } } } } });
    assert.strictEqual(ctx.getTileUrl(), url);
    assert.strictEqual(ctx.MC_getTileSpec('light').attribution, '© OpenStreetMap contributors');
    assert.strictEqual(ctx.getActiveTileProvider(), null);
  });
}

legacyLightTests().then(() => {
  console.log('\n' + (failed === 0 ? '✅' : '❌') + ' #332: ' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed === 0 ? 0 : 1);
}).catch((e) => { console.error(e); process.exit(1); });
