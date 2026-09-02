/* test-carto-basemap-key.js — CARTO Basemaps API key support (#7).
 *
 * From August 2026 CARTO requires an API key on raster basemap requests.
 * Without one the tiles render with an "API KEY REQUIRED —
 * carto.com/basemapsapikey" watermark.
 *
 * public/map-tile-providers.js owns the single public helper
 * window.MC_getCartoTileUrl(path); every CARTO layer in the product goes
 * through it:
 *   - the five registry styles (map-tile-providers.js)
 *   - TILE_DARK / TILE_LIGHT and the getTileUrl() fallback (roles.js)
 *   - the geo-filter modal map and geo-filter tab map (customize-v2.js)
 *   - the standalone builder (geofilter-builder.html)
 *
 * Contract under test:
 *   - the key lands on every one of those surfaces, never on other providers;
 *   - it is URL-encoded exactly once, through that one helper;
 *   - a missing/empty/whitespace token yields NO querystring (no bare "?"),
 *     and CARTO keeps working exactly as it did before the key existed;
 *   - the enterprise `domain` override still composes on every path;
 *   - the helper takes a tile PATH only — never a complete URL;
 *   - config arriving asynchronously replaces an early keyless URL;
 *   - no production runtime string bypasses the helper;
 *   - no real key is baked into production code or these fixtures.
 *
 * Runs via: node test-carto-basemap-key.js
 * Pure vm sandbox — no jsdom/Playwright, mirroring
 * test-issue-1420-tile-providers.js.
 */
'use strict';
const vm = require('vm');
const fs = require('fs');
const path = require('path');
const assert = require('assert');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}
async function atest(name, fn) {
  try { await fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

// Obviously-synthetic fixtures. These are NOT credentials: the first is a
// literal placeholder, the second exists purely to exercise percent-encoding.
const FAKE_TOKEN = 'YOUR_CARTO_BASEMAP_KEY';
const FAKE_TOKEN_NEEDING_ENCODING = 'a b&c=d/e?f+g';

const ALL_CARTO_IDS = ['carto-dark', 'carto-light', 'carto-voyager', 'carto-voyager-dark', 'positron-dark'];
const NON_CARTO_IDS = ['osm-standard', 'osm-dark', 'stamen-toner-lite', 'stamen-toner-dark', 'esri-darkgray-labels'];

const P = (f) => path.join(__dirname, 'public', f);
const readPub = (f) => fs.readFileSync(P(f), 'utf8');

function makeStorage() {
  const store = {};
  return {
    getItem(k) { return Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null; },
    setItem(k, v) { store[k] = String(v); },
    removeItem(k) { delete store[k]; },
    clear() { for (const k of Object.keys(store)) delete store[k]; },
    _raw: store
  };
}

function makeSandbox(opts) {
  opts = opts || {};
  const events = [];
  const listeners = {};
  const _paneAttrs = {};
  const tilePane = {
    style: { filter: '' },
    setAttribute: (k, v) => { _paneAttrs[k] = String(v); },
    getAttribute: (k) => Object.prototype.hasOwnProperty.call(_paneAttrs, k) ? _paneAttrs[k] : null,
    removeAttribute: (k) => { delete _paneAttrs[k]; }
  };
  const ctx = {
    console, setTimeout, clearTimeout, Promise,
    JSON, Date, Math, Object, Array, String, Number, Boolean, Error, TypeError,
    localStorage: makeStorage(),
    document: {
      documentElement: { getAttribute: () => opts.theme || 'dark', style: { getPropertyValue: () => '' } },
      querySelector: (sel) => sel === '.leaflet-tile-pane' ? tilePane : null,
      querySelectorAll: () => [],
      getElementById: () => null,
      createElement: () => ({ style: {}, appendChild() {}, setAttribute() {}, addEventListener() {}, insertAdjacentElement() {} }),
      addEventListener: () => {},
      body: { appendChild() {}, style: {} },
      head: { appendChild() {} },
      readyState: 'complete',
    },
    window: {
      addEventListener: (type, fn) => { (listeners[type] = listeners[type] || []).push(fn); },
      dispatchEvent: (ev) => { events.push(ev); return true; },
      matchMedia: () => ({ matches: false, addEventListener: () => {} }),
    },
    CustomEvent: function (type, init) { this.type = type; this.detail = (init && init.detail) || null; },
    Event: function (type) { this.type = type; },
  };
  ctx.window.localStorage = ctx.localStorage;
  ctx.globalThis = ctx;
  vm.createContext(ctx);
  ctx.window.document = ctx.document;
  ctx.events = events;
  ctx.listeners = listeners;
  ctx.tilePane = tilePane;
  return ctx;
}

function loadProviders(ctx, mapCfg) {
  if (mapCfg !== undefined) ctx.window.MC_MAP_CFG = mapCfg;
  vm.runInContext(readPub('map-tile-providers.js'), ctx, { filename: 'public/map-tile-providers.js' });
}

function withCarto(cartoCfg, extraProviders) {
  const ctx = makeSandbox();
  const providers = Object.assign({}, extraProviders || {});
  if (cartoCfg !== undefined) providers.carto = cartoCfg;
  loadProviders(ctx, { tiles: { providers: providers } });
  return ctx;
}

function urlFor(ctx, id) {
  const p = ctx.window.MC_TILE_PROVIDERS[id];
  assert.ok(p, 'provider not registered: ' + id);
  return typeof p.url === 'function' ? p.url() : p.url;
}

console.log('── #7 CARTO Basemaps API key ──');

// ─── The shared helper ───────────────────────────────────────────────────────

test('MC_getCartoTileUrl is exposed publicly', () => {
  const ctx = withCarto({ enabled: true, key: FAKE_TOKEN });
  assert.strictEqual(typeof ctx.window.MC_getCartoTileUrl, 'function',
    'other files (roles.js, customize-v2.js, geofilter-builder.html) depend on this global');
});

test('helper composes base + path + key, with no bare "?" when unkeyed', () => {
  const keyed = withCarto({ enabled: true, key: FAKE_TOKEN });
  assert.strictEqual(
    keyed.window.MC_getCartoTileUrl('/dark_all/{z}/{x}/{y}{r}.png'),
    'https://{s}.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}{r}.png?key=' + FAKE_TOKEN);

  const bare = withCarto({ enabled: true });
  assert.strictEqual(
    bare.window.MC_getCartoTileUrl('/dark_all/{z}/{x}/{y}{r}.png'),
    'https://{s}.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}{r}.png');
});

test('helper accepts a tile path ONLY — never a complete URL', () => {
  const ctx = withCarto({ enabled: true, key: FAKE_TOKEN });
  const bad = [
    'https://evil.example.com/{z}/{x}/{y}.png',      // full URL
    'dark_all/{z}/{x}/{y}.png',                       // no leading slash
    '/dark_all/{z}/{x}/{y}.png?key=attacker',         // smuggled querystring
    '/dark_all/{z}/{x}/{y}.png#frag',                 // fragment
    null, undefined, 42, {},
  ];
  for (const v of bad) {
    assert.throws(() => ctx.window.MC_getCartoTileUrl(v), TypeError,
      'must reject ' + JSON.stringify(v));
  }
});

// ─── Registry styles ─────────────────────────────────────────────────────────

test('token is appended to ALL five CARTO registry styles', () => {
  const ctx = withCarto({ enabled: true, key: FAKE_TOKEN });
  for (const id of ALL_CARTO_IDS) {
    assert.ok(urlFor(ctx, id).indexOf('?key=' + FAKE_TOKEN) >= 0,
      id + ' must carry ?key=<token>; got: ' + urlFor(ctx, id));
  }
});

test('each CARTO style keeps its own distinct tile path alongside the key', () => {
  const ctx = withCarto({ enabled: true, key: FAKE_TOKEN });
  const expectPath = {
    'carto-dark': '/dark_all/', 'carto-light': '/light_all/',
    'carto-voyager': '/rastertiles/voyager/', 'carto-voyager-dark': '/rastertiles/voyager/',
    'positron-dark': '/light_all/',
  };
  for (const id of ALL_CARTO_IDS) {
    const url = urlFor(ctx, id);
    assert.ok(url.indexOf(expectPath[id]) >= 0, id + ' lost its tile path: ' + url);
    assert.ok(/\{z\}/.test(url) && /\{x\}/.test(url) && /\{y\}/.test(url), id + ' must remain a template: ' + url);
  }
});

// ─── Encoding ────────────────────────────────────────────────────────────────

test('token is URL-encoded exactly once', () => {
  const ctx = withCarto({ enabled: true, key: FAKE_TOKEN_NEEDING_ENCODING });
  const expected = '?key=' + encodeURIComponent(FAKE_TOKEN_NEEDING_ENCODING);
  for (const id of ALL_CARTO_IDS) {
    const url = urlFor(ctx, id);
    assert.ok(url.endsWith(expected), id + ' must end with singly-encoded key ' + expected + '; got: ' + url);
    assert.ok(url.indexOf('%25') < 0, id + ' looks double-encoded: ' + url);
    const qs = url.slice(url.indexOf('?key=') + 5);
    for (const raw of [' ', '&', '=', '/', '?', '+']) {
      assert.ok(qs.indexOf(raw) < 0, id + ' querystring must not contain raw ' + JSON.stringify(raw) + ': ' + url);
    }
  }
});

test('token is trimmed before encoding', () => {
  const ctx = withCarto({ enabled: true, key: '  ' + FAKE_TOKEN + '  ' });
  assert.ok(urlFor(ctx, 'carto-dark').endsWith('?key=' + FAKE_TOKEN),
    'surrounding whitespace must be trimmed, not encoded as %20');
});

// ─── Missing / empty token keeps pre-key behaviour ───────────────────────────

test('no carto config at all → registered, no querystring (back-compat)', () => {
  const ctx = makeSandbox();
  loadProviders(ctx);
  for (const id of ALL_CARTO_IDS) {
    const url = urlFor(ctx, id);
    assert.ok(url.indexOf('?') < 0, id + ' must have no querystring without a token: ' + url);
  }
});

test('empty / whitespace / non-string token → no bare "?" left behind', () => {
  for (const tok of ['', '   ', 123, null, true, {}, []]) {
    const ctx = withCarto({ enabled: true, key: tok });
    for (const id of ALL_CARTO_IDS) {
      const url = urlFor(ctx, id);
      assert.ok(url.indexOf('?') < 0, id + ' with token ' + JSON.stringify(tok) + ' must not emit "?": ' + url);
      assert.ok(url.endsWith('.png'), id + ' should end at the tile extension: ' + url);
    }
  }
});

test('enabled=true without a token still registers CARTO (no hidden-layer mode)', () => {
  const ctx = withCarto({ enabled: true });
  for (const id of ALL_CARTO_IDS) {
    assert.ok(ctx.window.MC_TILE_PROVIDERS[id], id + ' must stay registered when the token is absent');
  }
});

test('enabled=false still removes CARTO, even with a token present', () => {
  const ctx = withCarto({ enabled: false, key: FAKE_TOKEN });
  for (const id of ALL_CARTO_IDS) {
    assert.ok(!ctx.window.MC_TILE_PROVIDERS[id], id + ' must be absent when carto.enabled=false');
  }
});

// ─── Domain override ─────────────────────────────────────────────────────────

test('enterprise domain override composes on every CARTO path, keyed and unkeyed', () => {
  const withKey = withCarto({ enabled: true, domain: 'mycompany', key: FAKE_TOKEN });
  const noKey = withCarto({ enabled: true, domain: 'mycompany' });
  for (const id of ALL_CARTO_IDS) {
    const a = urlFor(withKey, id);
    assert.ok(a.indexOf('https://{s}.mycompany.cartocdn.com') === 0, id + ' domain override lost: ' + a);
    assert.ok(a.endsWith('?key=' + FAKE_TOKEN), id + ' key must follow the path: ' + a);
    const b = urlFor(noKey, id);
    assert.ok(b.indexOf('https://{s}.mycompany.cartocdn.com') === 0, id + ' domain override lost: ' + b);
    assert.ok(b.indexOf('?') < 0, id + ' must have no querystring without a token: ' + b);
  }
  // And on the paths used outside the registry.
  for (const p of ['/dark_all/{z}/{x}/{y}{r}.png', '/light_all/{z}/{x}/{y}{r}.png']) {
    assert.ok(withKey.window.MC_getCartoTileUrl(p).indexOf('https://{s}.mycompany.cartocdn.com') === 0,
      'domain override must apply to ' + p);
  }
});

// ─── Querystring shape + provider isolation ──────────────────────────────────

test('never emits a double "?" or a duplicate key parameter', () => {
  const ctx = withCarto({ enabled: true, domain: 'mycompany', key: FAKE_TOKEN_NEEDING_ENCODING });
  for (const id of ALL_CARTO_IDS) {
    const url = urlFor(ctx, id);
    assert.strictEqual(url.split('?').length - 1, 1, id + ' must contain exactly one "?": ' + url);
    assert.strictEqual(url.split('key=').length - 1, 1, id + ' must contain exactly one key= param: ' + url);
  }
});

test('CARTO token never leaks onto Esri / OSM / Stamen URLs', () => {
  const ctx = withCarto(
    { enabled: true, key: FAKE_TOKEN },
    { osm: { enabled: true, provider: 'maptiler', token: 'OSM_FAKE_TOKEN' },
      stamen: { enabled: true, token: 'STAMEN_FAKE_TOKEN' } }
  );
  for (const id of NON_CARTO_IDS) {
    assert.ok(urlFor(ctx, id).indexOf(FAKE_TOKEN) < 0, id + ' must not carry the CARTO token');
  }
  assert.ok(urlFor(ctx, 'esri-darkgray-labels').indexOf('?') < 0, 'Esri URL must stay querystring-free');
  assert.ok(urlFor(ctx, 'osm-standard').indexOf('?key=OSM_FAKE_TOKEN') >= 0, 'maptiler keeps its own ?key=');
  assert.ok(urlFor(ctx, 'stamen-toner-lite').indexOf('?api_key=STAMEN_FAKE_TOKEN') >= 0, 'stamen keeps ?api_key=');
});

// ─── Unrelated behaviour must not regress ────────────────────────────────────

test('dark/light defaults, switching, labels, attribution and filters unchanged', () => {
  const ctx = withCarto({ enabled: true, key: FAKE_TOKEN });
  assert.strictEqual(ctx.window.MC_getDarkTileProvider(), 'carto-dark');
  assert.strictEqual(ctx.window.MC_getLightTileProvider(), 'carto-light');
  assert.strictEqual(ctx.window.MC_setDarkTileProvider('carto-voyager-dark'), true);
  assert.strictEqual(ctx.window.MC_getDarkTileProvider(), 'carto-voyager-dark');
  assert.strictEqual(ctx.window.MC_setLightTileProvider('carto-voyager'), true);
  assert.strictEqual(ctx.window.MC_setDarkTileProvider('carto-light'), false, 'cross-type assignment still rejected');

  const INVERT = 'invert(1) hue-rotate(180deg) brightness(0.9) contrast(1.05)';
  const expected = {
    'carto-dark': { label: 'Carto Dark', type: 'dark', invertFilter: null },
    'carto-light': { label: 'Carto Positron', type: 'light', invertFilter: null },
    'carto-voyager': { label: 'Carto Voyager', type: 'light', invertFilter: null },
    'carto-voyager-dark': { label: 'Carto Voyager', type: 'dark', invertFilter: INVERT },
    'positron-dark': { label: 'Carto Positron', type: 'dark', invertFilter: INVERT },
  };
  for (const id of ALL_CARTO_IDS) {
    const p = ctx.window.MC_TILE_PROVIDERS[id];
    assert.strictEqual(p.label, expected[id].label, id + ' label changed');
    assert.strictEqual(p.type, expected[id].type, id + ' type changed');
    assert.strictEqual(p.invertFilter, expected[id].invertFilter, id + ' invertFilter changed');
    assert.strictEqual(p.attribution, '© OpenStreetMap © CartoDB', id + ' attribution changed');
    assert.strictEqual(p.maxZoom, 19, id + ' maxZoom changed');
  }
});

test('async config load swaps the early keyless registry URL for the keyed one', () => {
  const ctx = makeSandbox();
  loadProviders(ctx);
  const before = urlFor(ctx, 'carto-dark');
  assert.ok(before.indexOf('?') < 0, 'no key before config arrives');

  ctx.window.MC_MAP_CFG = { tiles: { providers: { carto: { enabled: true, key: FAKE_TOKEN } } } };
  ctx.window.MC_initTileRegistry(true);

  const after = urlFor(ctx, 'carto-dark');
  assert.ok(after.endsWith('?key=' + FAKE_TOKEN), 'key must apply once async config lands; got: ' + after);
  assert.notStrictEqual(after, before, 'the keyless URL must be replaced, not kept');
  assert.ok(ctx.events.some(e => e.type === 'mc-tile-provider-changed'),
    'fromAsync must dispatch the re-sync event so map.js/live.js re-read the URL');
});

// ─── roles.js: TILE_DARK / TILE_LIGHT / getTileUrl ───────────────────────────

function loadRolesStack(clientCfg, theme) {
  const ctx = makeSandbox({ theme: theme || 'dark' });
  let resolveFetch;
  const fetchPromise = new Promise((res) => { resolveFetch = res; });
  ctx.fetch = () => fetchPromise.then(() => ({ ok: true, json: () => Promise.resolve(clientCfg) }));
  ctx.window.fetch = ctx.fetch;
  // Real page order: roles.js first (index.html:170), providers second (:171).
  vm.runInContext(readPub('roles.js'), ctx, { filename: 'public/roles.js' });
  vm.runInContext(readPub('map-tile-providers.js'), ctx, { filename: 'public/map-tile-providers.js' });
  // In a browser `window` IS the global object, so a bare `TILE_LIGHT`
  // reference inside roles.js hits the accessor defined on window. A vm
  // sandbox separates ctx from ctx.window, so forward by reference — a
  // plain value copy would snapshot the keyless URL and hide the very
  // async-refresh behaviour these tests exist to prove.
  for (const k of Object.keys(ctx.window)) {
    if (k in ctx) continue;
    Object.defineProperty(ctx, k, {
      configurable: true, enumerable: true,
      get: () => ctx.window[k],
      set: (v) => { ctx.window[k] = v; },
    });
  }
  return { ctx, landConfig: () => { resolveFetch(); return ctx.window.MeshConfigReady; } };
}

const CLIENT_CFG_WITH_KEY = { map: { tiles: { providers: { carto: { enabled: true, key: FAKE_TOKEN } } } } };

test('roles.js TILE_DARK/TILE_LIGHT are keyless before config, and never frozen literals', () => {
  const { ctx } = loadRolesStack(CLIENT_CFG_WITH_KEY);
  assert.strictEqual(ctx.window.TILE_DARK, 'https://{s}.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}{r}.png');
  assert.strictEqual(ctx.window.TILE_LIGHT, 'https://{s}.basemaps.cartocdn.com/light_all/{z}/{x}/{y}{r}.png');
});

// ─── Source-level guarantees ─────────────────────────────────────────────────

test('customize-v2.js builds BOTH geo-filter maps through the helper', () => {
  const src = readPub('customize-v2.js');
  const calls = src.match(/MC_getCartoTileUrl\(/g) || [];
  assert.strictEqual(calls.length, 2,
    'expected exactly 2 helper calls (geo-filter modal map + geo-filter tab map), got ' + calls.length);
  assert.ok(src.indexOf('cartocdn.com') < 0,
    'customize-v2.js must not hardcode any cartocdn URL any more');
});

test('geofilter-builder.html builds its layer through the helper and loads the module', () => {
  const src = fs.readFileSync(P('geofilter-builder.html'), 'utf8');
  assert.ok(/<script src="map-tile-providers\.js">/.test(src),
    'standalone builder must load map-tile-providers.js to get the helper');
  assert.ok(src.indexOf('MC_getCartoTileUrl(') >= 0, 'must build its tile URL through the helper');
  assert.ok(src.indexOf("fetch('/api/config/client')") >= 0,
    'standalone builder must fetch the client config to learn the token');
  assert.ok(src.indexOf('cartocdn.com') < 0,
    'geofilter-builder.html must not hardcode a cartocdn URL any more');
  // #7 r3: the layer must be created AFTER the fetch settles, not created
  // keyless up-front and patched with setUrl afterwards.
  const fetchAt = src.indexOf("fetch('/api/config/client')");
  const addAt = src.indexOf('.addTo(map)', src.indexOf('function addCartoLayer'));
  assert.ok(src.indexOf('function addCartoLayer') >= 0,
    'layer creation must be a function called from the fetch chain');
  assert.ok(src.indexOf('.then(addCartoLayer)') > fetchAt,
    'addCartoLayer must be invoked from the settled fetch chain, after .catch');
  assert.ok(addAt > 0, 'addCartoLayer must add the tile layer to the map');
  assert.ok(src.indexOf('setUrl(') < 0,
    'no setUrl patch-up should remain — the layer is built once, already keyed');
});

test('roles.js has no hardcoded cartocdn URL left', () => {
  const src = readPub('roles.js');
  assert.ok(src.indexOf('cartocdn.com') < 0,
    'roles.js TILE_DARK/TILE_LIGHT must resolve through MC_getCartoTileUrl, not a literal');
  assert.ok(src.indexOf('MC_getCartoTileUrl') >= 0, 'roles.js must use the shared helper');
});

test('no production runtime string bypasses the helper', () => {
  // The ONLY place basemaps.cartocdn.com may appear at runtime is the
  // helper's own base in map-tile-providers.js. Anything else is a bypass.
  const offenders = [];
  for (const f of fs.readdirSync(P('.'))) {
    if (!/\.(js|html)$/.test(f)) continue;
    if (f === 'map-tile-providers.js') continue;            // owns the base URL
    const src = fs.readFileSync(P(f), 'utf8');
    if (src.indexOf('cartocdn.com') >= 0) offenders.push(f);
  }
  assert.deepStrictEqual(offenders, [],
    'these files still build a CARTO URL without the helper: ' + offenders.join(', '));

  // Inside the helper file, count only executable lines — prose in comments
  // naturally names the host it is documenting.
  const runtime = readPub('map-tile-providers.js')
    .split('\n').filter(l => !/^\s*(\/\/|\*|\/\*)/.test(l)).join('\n');
  assert.strictEqual((runtime.match(/cartocdn\.com/g) || []).length, 2,
    'runtime code should build the host in exactly one place (enterprise + default base)');
  // maptiler legitimately uses its own '?key=' param, so scope this to the
  // CARTO side: the suffix helper must be the only place CARTO spells it.
  const cartoKeyLines = runtime.split('\n')
    .filter(l => l.indexOf('?key=') >= 0 && l.indexOf('maptiler') < 0);
  assert.strictEqual(cartoKeyLines.length, 1,
    'CARTO\'s "?key=" must appear on exactly one runtime line (the shared suffix helper); got:\n' + cartoKeyLines.join('\n'));
  assert.ok(/_getCartoKeySuffix/.test(runtime.slice(0, runtime.indexOf(cartoKeyLines[0]))),
    'that single occurrence must live inside _getCartoKeySuffix');
});

// ─── No committed credentials ────────────────────────────────────────────────

// #7 r4: enabled:false only removes CARTO from the REGISTERED main-map /
// layer-picker styles. customize-v2.js and geofilter-builder.html call
// MC_getCartoTileUrl directly and still render CARTO, so the docs must not
// claim it stops CARTO everywhere.
test('docs do not overclaim that carto.enabled=false stops all CARTO surfaces', () => {
  const cfg = JSON.parse(fs.readFileSync(path.join(__dirname, 'config.example.json'), 'utf8'));
  const note = cfg.map.tiles.providers._comment_carto;
  assert.ok(typeof note === 'string' && note.length > 0, '_comment_carto must exist');
  assert.ok(!/stop using Carto entirely/i.test(note),
    'must not claim enabled:false stops Carto entirely; got: ' + note);
  assert.ok(/geo-filter/i.test(note),
    'must tell the operator the geo-filter maps still use Carto');
  assert.ok(/registered|layer-picker|main-map/i.test(note),
    'must scope enabled:false to the registered main-map / layer-picker styles');

  // The same overclaim must not reappear in the code comments.
  const gating = readPub('map-tile-providers.js');
  const i = gating.indexOf('carto.enabled === false');
  assert.ok(i > 0, 'the gating comment should document the enabled=false case');
  const para = gating.slice(i, i + 700);
  assert.ok(/does NOT disable CARTO everywhere|geofilter|geo-filter/i.test(para),
    'the HAS_CARTO comment must name the geo-filter exception, not imply a global off switch');
});

test('no real API key is hardcoded in production source or fixtures', () => {
  for (const f of ['map-tile-providers.js', 'roles.js', 'customize-v2.js', 'geofilter-builder.html']) {
    const src = fs.readFileSync(P(f), 'utf8');
    const assigned = src.match(/key=[A-Za-z0-9_\-]{8,}/g) || [];
    assert.deepStrictEqual(assigned, [], f + ' must not embed a key value; found: ' + JSON.stringify(assigned));
  }
  const cfg = JSON.parse(fs.readFileSync(path.join(__dirname, 'config.example.json'), 'utf8'));
  const carto = cfg.map.tiles.providers.carto;
  assert.ok(Object.prototype.hasOwnProperty.call(carto, 'key'), 'carto.key must be documented in the example');
  assert.strictEqual(carto.key, '', 'config.example.json must ship an EMPTY carto token');
  assert.ok(!Object.prototype.hasOwnProperty.call(carto, 'requireKey'), 'requireKey must be gone from the example');
  assert.ok(readPub('map-tile-providers.js').indexOf('requireKey') < 0, 'requireKey must be gone from production code');
});

// ─── Deferred first paint (#7 r3): no keyless request before config ──────────

// Runs the REAL tile-init region lifted verbatim out of map.js / live.js, so
// these tests fail if that production code stops deferring. Everything the
// region closes over (L, map, the theme, the tile globals) is injected.
function runTileInit(which, opts) {
  opts = opts || {};
  const file = which === 'map' ? 'map.js' : 'live.js';
  const src = readPub(file);
  const refAnchor = which === 'map' ? 'let _darkRefLayer = null;' : 'let _liveDarkRefLayer = null;';
  // End just past the config-ready wiring. Both files now build the layer
  // control INSIDE that callback, so the anchor must sit after it.
  const endAnchor = which === 'map' ? 'const _mapThemeObs' : 'L.control.zoom(';
  // Start at the `const isDark = …` the region closes over, just above the
  // ref-layer declaration, so the slice is self-contained.
  const s = src.lastIndexOf('const isDark =', src.indexOf(refAnchor));
  const e = src.indexOf(endAnchor, s);
  assert.ok(s > 0 && e > s, 'could not slice the tile-init region out of ' + file);
  const region = src.slice(s, e);
  assert.ok(region.indexOf('MC_whenTileConfigReady') > 0,
    file + ' tile-init region must defer through MC_whenTileConfigReady');

  const ctx = makeSandbox({ theme: opts.theme || 'dark' });
  loadProviders(ctx);                       // provides MC_* incl. whenTileConfigReady

  // Record every tile request Leaflet would make: a layer only fetches once
  // it is added to a map/group, so `added` is our proxy for "request sent".
  const added = [];
  const created = [];
  function fakeLayer(url, o) {
    const layer = {
      _url: url, options: o || {}, _added: false,
      addTo(group) { this._added = true; added.push(this._url); (group._layers || []).push(this); return this; },
      setUrl(u) { this._url = u; return this; },
      on() { return this; },
    };
    created.push(layer);
    return layer;
  }
  // Layer-control instrumentation:each build records the explicit base layers
  // it materialised, so we can assert both "how many controls" and "were the
  // selectable CARTO layers keyed".
  const controlBuilds = [];
  const baseLayerChangeHandlers = [];
  ctx.L = {
    tileLayer: (url, o) => fakeLayer(url, o),
    layerGroup: () => { const g = { _layers: [], hasLayer(l) { return this._layers.indexOf(l) >= 0; }, addTo() { return this; }, removeLayer(l) { const i = this._layers.indexOf(l); if (i >= 0) this._layers.splice(i, 1); return this; } }; return g; },
    control: {
      zoom: () => ({ addTo() { return this; } }),
      layers: (baseMaps) => {
        const urls = {};
        Object.keys(baseMaps || {}).forEach((k) => { if (baseMaps[k] && typeof baseMaps[k]._url === 'string') urls[k] = baseMaps[k]._url; });
        controlBuilds.push(urls);
        return { addTo() { return this; }, remove() {}, getContainer() { throw new Error('no DOM'); } };
      },
    },
  };
  ctx.window.L = ctx.L;
  ctx.map = {
    getPane: () => null, attributionControl: null, hasLayer: () => false,
    addLayer() {}, removeLayer() {}, createPane() {},
    on(ev, fn) { if (ev === 'baselayerchange') baseLayerChangeHandlers.push(fn); },
    off(ev) { if (ev === 'baselayerchange') baseLayerChangeHandlers.pop(); },
  };
  ctx.window.map = ctx.map;
  ctx.controlBuilds = controlBuilds;
  ctx.baseLayerChangeHandlers = baseLayerChangeHandlers;
  ctx.MutationObserver = function () { return { observe() {}, disconnect() {} }; };
  ctx.window.MutationObserver = ctx.MutationObserver;

  // The tile globals roles.js normally owns, wired to the same helper so the
  // fallback path is exercised for real rather than stubbed with a literal.
  Object.defineProperty(ctx, 'TILE_DARK', { get: () => ctx.window.MC_getCartoTileUrl('/dark_all/{z}/{x}/{y}{r}.png'), configurable: true });
  Object.defineProperty(ctx, 'TILE_LIGHT', { get: () => ctx.window.MC_getCartoTileUrl('/light_all/{z}/{x}/{y}{r}.png'), configurable: true });

  // Stand in for roles.js' MeshConfigReady.
  let settle;
  if (opts.configMode !== 'absent') {
    ctx.window.MeshConfigReady = new Promise((res, rej) => {
      settle = () => {
        if (opts.configMode === 'reject') { rej(new Error('offline')); return; }
        ctx.window.MC_MAP_CFG = opts.clientCfg.map;
        ctx.window.MC_initTileRegistry(true);
        res(opts.clientCfg);
      };
    });
    ctx.window.MeshConfigReady.catch(() => {});
  }

  vm.runInContext('(function(){\n' + region + '\n})();', ctx, { filename: 'public/' + file + ' (tile-init region)' });
  return { ctx, added, created, controlBuilds, baseLayerChangeHandlers, settle: settle || (() => {}), wait: () => new Promise(r => setTimeout(r, 0)).then(() => new Promise(r => setTimeout(r, 0))) };
}

const CFG_TOKEN = { map: { tiles: { providers: { carto: { enabled: true, key: FAKE_TOKEN } } } } };
const CFG_NO_TOKEN = { map: { tiles: { providers: { carto: { enabled: true } } } } };

// ─── Async-dependent checks ──────────────────────────────────────────────────

(async function () {
  for (const which of ['map', 'live']) {
    await atest(which + '.js: NO tile request is issued before config settles', async () => {
      const h = runTileInit(which, { clientCfg: CFG_TOKEN });
      assert.deepStrictEqual(h.added, [],
        which + '.js must not add (and therefore must not request) any tile layer before /api/config/client settles; added: ' + JSON.stringify(h.added));
    });

    await atest(which + '.js: the FIRST tile request already carries the token', async () => {
      const h = runTileInit(which, { clientCfg: CFG_TOKEN });
      h.settle(); await h.wait();
      assert.ok(h.added.length >= 1, which + '.js must add the tile layer once config settles');
      assert.strictEqual(h.added[0],
        'https://{s}.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}{r}.png?key=' + FAKE_TOKEN,
        which + '.js first request must be keyed; got: ' + h.added[0]);
      for (const u of h.added) {
        assert.ok(u.indexOf('cartocdn') < 0 || u.indexOf('?key=' + FAKE_TOKEN) >= 0,
          which + '.js issued a keyless CARTO request: ' + u);
      }
    });

    await atest(which + '.js: config-fetch failure still yields a working keyless layer', async () => {
      const h = runTileInit(which, { clientCfg: CFG_TOKEN, configMode: 'reject' });
      h.settle(); await h.wait();
      assert.strictEqual(h.added.length, 1, which + '.js must still add exactly one tile layer when config fails');
      assert.strictEqual(h.added[0], 'https://{s}.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}{r}.png',
        which + '.js must fall back to the keyless URL, not an empty map; got: ' + h.added[0]);
    });

    await atest(which + '.js: config without a token keeps the keyless behaviour', async () => {
      const h = runTileInit(which, { clientCfg: CFG_NO_TOKEN });
      h.settle(); await h.wait();
      assert.strictEqual(h.added.length, 1, 'exactly one layer');
      assert.strictEqual(h.added[0], 'https://{s}.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}{r}.png');
      assert.ok(h.added[0].indexOf('?') < 0, 'no bare "?" without a token');
    });

    await atest(which + '.js: no MeshConfigReady at all → synchronous add (pre-#7 ordering)', async () => {
      const h = runTileInit(which, { clientCfg: CFG_TOKEN, configMode: 'absent' });
      assert.strictEqual(h.added.length, 1,
        which + '.js must add immediately when there is no config promise to wait for');
    });

    await atest(which + '.js: survives map-tile-providers.js failing to load (guarded MC_* call)', async () => {
      // Every other cross-file MC_* call in this region is typeof-guarded, so
      // a failed provider-script load degrades to a keyless map instead of
      // throwing and aborting the rest of init(). The deferral must not be
      // the one exception.
      const file = which === 'map' ? 'map.js' : 'live.js';
      const src = readPub(file);
      const i = src.indexOf('MC_whenTileConfigReady');
      const around = src.slice(Math.max(0, i - 120), i + 320);
      assert.ok(/typeof window\.MC_whenTileConfigReady === 'function'/.test(around),
        file + ' must typeof-guard MC_whenTileConfigReady like its neighbours');
      assert.ok(/else\s+_[A-Za-z]*[Aa]ttachTiles\(\);/.test(around),
        file + ' must attach immediately when the helper is absent');
    });

    await atest(which + '.js: layer control count is 0 before config and 1 after', async () => {
      const h = runTileInit(which, { clientCfg: CFG_TOKEN });
      assert.strictEqual(h.controlBuilds.length, 0,
        which + '.js must not build the layer picker before config settles — it materialises a real tile layer per style, which would be keyless');
      h.settle(); await h.wait();
      assert.strictEqual(h.controlBuilds.length, 1,
        which + '.js must build the layer picker exactly once after config; got ' + h.controlBuilds.length);
    });

    await atest(which + '.js: every explicit CARTO layer offered by the picker is keyed', async () => {
      const h = runTileInit(which, { clientCfg: CFG_TOKEN });
      h.settle(); await h.wait();
      const built = h.controlBuilds[0] || {};
      const cartoEntries = Object.keys(built).filter(k => String(built[k]).indexOf('cartocdn') >= 0);
      assert.ok(cartoEntries.length >= 5,
        'picker should offer the CARTO styles; got ' + JSON.stringify(Object.keys(built)));
      for (const k of cartoEntries) {
        assert.ok(built[k].indexOf('?key=' + FAKE_TOKEN) >= 0,
          'explicit picker layer ' + k + ' must already be keyed on its first request; got: ' + built[k]);
      }
    });

    await atest(which + '.js: no way to select an explicit layer before config settles', async () => {
      const h = runTileInit(which, { clientCfg: CFG_TOKEN });
      // No control at all, and no baselayerchange wiring, means the user has
      // no UI affordance to pick a keyless CARTO layer during the window.
      assert.strictEqual(h.controlBuilds.length, 0, 'no control before config');
      assert.strictEqual(h.baseLayerChangeHandlers.length, 0,
        'no baselayerchange handler should exist before the control is built');
      assert.deepStrictEqual(h.added, [], 'and no tile layer on the map yet');
    });

    await atest(which + '.js: config failure still yields exactly one control + one keyless Auto layer', async () => {
      const h = runTileInit(which, { clientCfg: CFG_TOKEN, configMode: 'reject' });
      h.settle(); await h.wait();
      assert.strictEqual(h.controlBuilds.length, 1, 'exactly one control even when config fails');
      assert.strictEqual(h.added.length, 1, 'exactly one Auto tile layer');
      assert.strictEqual(h.added[0], 'https://{s}.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}{r}.png',
        'Auto layer must fall back to the keyless URL, not stay empty');
    });

    await atest(which + '.js: repeated config-ready notification adds no extra control/listener/layer', async () => {
      const h = runTileInit(which, { clientCfg: CFG_TOKEN });
      h.settle(); await h.wait();
      const controlsAfter = h.controlBuilds.length;
      const addedAfter = h.added.length;
      // Re-dispatch the same cross-file signal the real flow uses, twice.
      h.ctx.window.MC_initTileRegistry(true);
      h.ctx.window.MC_initTileRegistry(true);
      await h.wait();
      assert.strictEqual(h.added.length, addedAfter,
        which + '.js must not add another tile layer on a repeat config event');
      // The registry's own control listener may legitimately rebuild the
      // picker (that is how newly-enabled providers appear), but map.js/live.js
      // must not create a SECOND control of their own.
      assert.ok(h.controlBuilds.length >= controlsAfter,
        'sanity: control count never decreases');
      assert.strictEqual(h.baseLayerChangeHandlers.length <= h.controlBuilds.length, true,
        'baselayerchange handlers must not outnumber control builds (no listener leak)');
    });

    await atest(which + '.js: exactly one tile layer is added after config resolution', async () => {
      const h = runTileInit(which, { clientCfg: CFG_TOKEN });
      h.settle(); await h.wait(); await h.wait();
      const cartoAdds = h.added.filter(u => u.indexOf('cartocdn') >= 0);
      assert.strictEqual(cartoAdds.length, 1,
        which + '.js must not double-add the CARTO layer; got: ' + JSON.stringify(h.added));
    });
  }

  await atest('geofilter-builder issues NO keyless CARTO request before config settles', async () => {
    const html = fs.readFileSync(P('geofilter-builder.html'), 'utf8');
    const OPEN = '<script>';
    const start = html.indexOf(OPEN, html.indexOf('map-tile-providers.js')) + OPEN.length;
    const inline = html.slice(start, html.indexOf('</script>', start));
    const bootstrap = inline.slice(0, inline.indexOf('let points'));
    assert.ok(bootstrap.indexOf('MC_getCartoTileUrl') >= 0, 'fixture extraction failed');

    const ctx = makeSandbox();
    loadProviders(ctx);
    const added = [];
    ctx.L = {
      map: () => ({ setView: () => ({}), on() {}, removeLayer() {}, addLayer() {} }),
      tileLayer: (url) => ({ addTo() { added.push(url); return this; }, setUrl() { return this; } }),
      marker: () => ({ addTo() { return this; }, on() { return this; } }),
      polygon: () => ({ addTo() { return this; } }),
      polyline: () => ({ addTo() { return this; } }),
    };
    ctx.window.L = ctx.L;
    let resolveFetch;
    const gate = new Promise((r) => { resolveFetch = r; });
    ctx.fetch = () => gate.then(() => ({ json: () => Promise.resolve(CFG_TOKEN) }));
    ctx.window.fetch = ctx.fetch;
    vm.runInContext(bootstrap, ctx, { filename: 'geofilter-builder.html (inline)' });

    assert.deepStrictEqual(added, [],
      'builder must not add any tile layer before /api/config/client settles; added: ' + JSON.stringify(added));

    resolveFetch();
    for (let i = 0; i < 6; i++) await new Promise((r) => setTimeout(r, 0));

    assert.strictEqual(added.length, 1, 'exactly one layer after config; got ' + added.length);
    assert.strictEqual(added[0], 'https://{s}.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}{r}.png?key=' + FAKE_TOKEN,
      'builder first (and only) request must already be keyed; got: ' + added[0]);
  });

  await atest('geofilter-builder falls back to a keyless layer when the config fetch fails', async () => {
    const html = fs.readFileSync(P('geofilter-builder.html'), 'utf8');
    const OPEN = '<script>';
    const start = html.indexOf(OPEN, html.indexOf('map-tile-providers.js')) + OPEN.length;
    const inline = html.slice(start, html.indexOf('</script>', start));
    const bootstrap = inline.slice(0, inline.indexOf('let points'));

    const ctx = makeSandbox();
    loadProviders(ctx);
    const added = [];
    ctx.L = {
      map: () => ({ setView: () => ({}), on() {}, removeLayer() {}, addLayer() {} }),
      tileLayer: (url) => ({ addTo() { added.push(url); return this; }, setUrl() { return this; } }),
      marker: () => ({ addTo() { return this; }, on() { return this; } }),
      polygon: () => ({ addTo() { return this; } }),
      polyline: () => ({ addTo() { return this; } }),
    };
    ctx.window.L = ctx.L;
    ctx.fetch = () => Promise.reject(new Error('offline'));
    ctx.window.fetch = ctx.fetch;
    vm.runInContext(bootstrap, ctx, { filename: 'geofilter-builder.html (inline)' });
    for (let i = 0; i < 6; i++) await new Promise((r) => setTimeout(r, 0));

    assert.strictEqual(added.length, 1, 'builder must still get exactly one layer when the fetch fails');
    assert.strictEqual(added[0], 'https://{s}.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}{r}.png',
      'offline builder must fall back to keyless tiles, not an empty map');
  });

  await atest('MC_whenTileConfigReady fires once, on settle (resolve AND reject), never twice', async () => {
    // resolve
    const a = withCarto({ enabled: true, key: FAKE_TOKEN });
    let aN = 0;
    let resA; a.window.MeshConfigReady = new Promise(r => { resA = r; });
    a.window.MC_whenTileConfigReady(() => { aN++; });
    assert.strictEqual(aN, 0, 'must not fire before the promise settles');
    resA(); await new Promise(r => setTimeout(r, 0));
    assert.strictEqual(aN, 1, 'must fire exactly once on resolve');

    // reject
    const b = withCarto({ enabled: true, key: FAKE_TOKEN });
    let bN = 0;
    let rejB; b.window.MeshConfigReady = new Promise((_, rj) => { rejB = rj; });
    b.window.MeshConfigReady.catch(() => {});
    b.window.MC_whenTileConfigReady(() => { bN++; });
    rejB(new Error('x')); await new Promise(r => setTimeout(r, 0));
    assert.strictEqual(bN, 1, 'must fire exactly once on reject too (settled, not fulfilled)');

    // absent → synchronous
    const c = withCarto({ enabled: true, key: FAKE_TOKEN });
    let cN = 0;
    c.window.MC_whenTileConfigReady(() => { cN++; });
    assert.strictEqual(cN, 1, 'must fire synchronously when there is no MeshConfigReady');
  });

  await atest('test-carto-basemap-key.js is registered exactly once in test-all.sh', async () => {
    const sh = fs.readFileSync(path.join(__dirname, 'test-all.sh'), 'utf8');
    const lines = sh.split('\n').filter(l => l.trim() === 'node test-carto-basemap-key.js');
    assert.strictEqual(lines.length, 1,
      'expected exactly one registration line in test-all.sh, got ' + lines.length);
    assert.ok(/^set -e$/m.test(sh), 'test-all.sh must keep its set -e semantics');
    const idx = sh.indexOf('node test-carto-basemap-key.js');
    const tileIdx = sh.indexOf('node test-issue-1420-tile-providers.js');
    assert.ok(tileIdx > 0 && Math.abs(sh.slice(0, idx).split('\n').length - sh.slice(0, tileIdx).split('\n').length) <= 3,
      'should sit next to the existing tile-provider tests');
  });

  await atest('roles.js TILE_DARK/TILE_LIGHT pick up the key once config lands', async () => {
    const { ctx, landConfig } = loadRolesStack(CLIENT_CFG_WITH_KEY);
    const beforeDark = ctx.window.TILE_DARK;
    await landConfig();
    assert.strictEqual(ctx.window.TILE_DARK,
      'https://{s}.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}{r}.png?key=' + FAKE_TOKEN);
    assert.strictEqual(ctx.window.TILE_LIGHT,
      'https://{s}.basemaps.cartocdn.com/light_all/{z}/{x}/{y}{r}.png?key=' + FAKE_TOKEN);
    assert.notStrictEqual(ctx.window.TILE_DARK, beforeDark, 'the keyless value must not stay frozen');
  });

  await atest('getTileUrl() returns a keyed URL after config lands (dark + light)', async () => {
    const dark = loadRolesStack(CLIENT_CFG_WITH_KEY, 'dark');
    await dark.landConfig();
    const d = dark.ctx.window.getTileUrl();
    assert.ok(d.indexOf('?key=' + FAKE_TOKEN) >= 0, 'dark getTileUrl must be keyed; got: ' + d);

    const light = loadRolesStack(CLIENT_CFG_WITH_KEY, 'light');
    await light.landConfig();
    const l = light.ctx.window.getTileUrl();
    assert.ok(l.indexOf('?key=' + FAKE_TOKEN) >= 0, 'light getTileUrl must be keyed; got: ' + l);
  });

  await atest('an explicit darkUrl/lightUrl override still wins over the CARTO derivation', async () => {
    const CUSTOM_D = 'https://tiles.example.com/dark/{z}/{x}/{y}.png';
    const CUSTOM_L = 'https://tiles.example.com/light/{z}/{x}/{y}.png';
    const { ctx, landConfig } = loadRolesStack({
      map: { tiles: { darkUrl: CUSTOM_D, lightUrl: CUSTOM_L, providers: { carto: { enabled: true, key: FAKE_TOKEN } } } }
    });
    await landConfig();
    assert.strictEqual(ctx.window.TILE_DARK, CUSTOM_D, 'explicit darkUrl override must be honoured');
    assert.strictEqual(ctx.window.TILE_LIGHT, CUSTOM_L, 'explicit lightUrl override must be honoured');
  });

  await atest('roles.js stays keyless (no bare "?") when the server sends no token', async () => {
    const { ctx, landConfig } = loadRolesStack({ map: { tiles: { providers: { carto: { enabled: true } } } } });
    await landConfig();
    assert.strictEqual(ctx.window.TILE_DARK, 'https://{s}.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}{r}.png');
    assert.ok(ctx.window.TILE_DARK.indexOf('?') < 0, 'no bare "?" without a token');
  });

  // ─── carto.domain hardening ──────────────────────────────────────────────
  // `domain` is concatenated straight into the host, so an unvalidated value
  // escapes the host and (with a key set) sends the key somewhere else.
  // Upstream Kpa-clawbot/CoreScope#1919 has the same unvalidated _getCartoBase.

  test('a valid enterprise domain still builds the documented host', () => {
    const ctx = withCarto({ enabled: true, domain: 'mycompany' });
    const u = ctx.window.MC_getCartoTileUrl('/dark_all/{z}/{x}/{y}{r}.png');
    assert.strictEqual(u, 'https://{s}.mycompany.cartocdn.com/dark_all/{z}/{x}/{y}{r}.png');
  });

  test('a dotted enterprise domain is still accepted', () => {
    const ctx = withCarto({ enabled: true, domain: 'eu.mycompany' });
    assert.ok(ctx.window.MC_getCartoTileUrl('/dark_all/{z}/{x}/{y}{r}.png')
      .indexOf('https://{s}.eu.mycompany.cartocdn.com/') === 0);
  });

  test('domain is trimmed', () => {
    const ctx = withCarto({ enabled: true, domain: '  mycompany  ' });
    assert.strictEqual(ctx.window.MC_getCartoTileUrl('/dark_all/{z}/{x}/{y}{r}.png'),
      'https://{s}.mycompany.cartocdn.com/dark_all/{z}/{x}/{y}{r}.png');
  });

  test('a domain that would move the tile host is ignored', () => {
    const BAD = ['evil.com/x?a=b', 'foo?a=b', 'foo#frag', 'https://evil.com',
                 'foo bar', 'evil.com@real', '../evil', 'foo/', '?a=b', '//evil.com'];
    for (const d of BAD) {
      const ctx = withCarto({ enabled: true, domain: d });
      const u = ctx.window.MC_getCartoTileUrl('/dark_all/{z}/{x}/{y}{r}.png');
      assert.strictEqual(u, 'https://{s}.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}{r}.png',
        'domain ' + JSON.stringify(d) + ' must fall back to the public base, got: ' + u);
    }
  });

  test('an injected domain can never receive the key', () => {
    for (const d of ['evil.com/x?a=b', 'https://evil.com', 'foo?a=b']) {
      const ctx = withCarto({ enabled: true, domain: d, key: FAKE_TOKEN });
      const u = ctx.window.MC_getCartoTileUrl('/dark_all/{z}/{x}/{y}{r}.png');
      const host = u.replace('{s}', 'a').split('/')[2];
      assert.strictEqual(host, 'a.basemaps.cartocdn.com',
        'key must never be sent to an injected host, got: ' + u);
      assert.ok(u.indexOf('?key=' + FAKE_TOKEN) > 0, 'the key still reaches the real host: ' + u);
      assert.strictEqual(u.split('?').length, 2, 'exactly one querystring: ' + u);
    }
  });

  test('a valid domain and the key compose on every style', () => {
    const ctx = withCarto({ enabled: true, domain: 'mycompany', key: FAKE_TOKEN });
    for (const id of ALL_CARTO_IDS) {
      const u = urlFor(ctx, id);
      assert.ok(u.indexOf('https://{s}.mycompany.cartocdn.com/') === 0, id + ': ' + u);
      assert.ok(u.indexOf('?key=' + FAKE_TOKEN) > 0, id + ' missing key: ' + u);
      assert.strictEqual(u.split('?').length, 2, id + ' has more than one querystring: ' + u);
    }
  });

  // ─── clean rename: token -> key (no permanent dual support) ──────────────

  test('the legacy carto.token field is NOT honoured', () => {
    const ctx = withCarto({ enabled: true, token: FAKE_TOKEN });
    const u = ctx.window.MC_getCartoTileUrl('/dark_all/{z}/{x}/{y}{r}.png');
    assert.strictEqual(u, 'https://{s}.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}{r}.png',
      'carto.token must be inert after the rename to carto.key, got: ' + u);
  });

  test('production source no longer reads carto.token', () => {
    const src = readPub('map-tile-providers.js');
    assert.ok(src.indexOf('carto.token') < 0, 'map-tile-providers.js still mentions carto.token');
    assert.ok(/providers\.carto\.key/.test(src) || /_cfg\.providers\.carto\) \? _cfg\.providers\.carto\.key/.test(src),
      'map-tile-providers.js must read carto.key');
  });

  test('config.example.json exposes key and no longer exposes token', () => {
    const cfg = JSON.parse(fs.readFileSync(path.join(__dirname, 'config.example.json'), 'utf8'));
    const carto = cfg.map.tiles.providers.carto;
    assert.ok(Object.prototype.hasOwnProperty.call(carto, 'key'), 'carto.key must exist');
    assert.ok(!Object.prototype.hasOwnProperty.call(carto, 'token'), 'carto.token must be gone');
    assert.strictEqual(carto.key, '', 'the shipped example must carry an EMPTY key');
    const cmt = cfg.map.tiles.providers._comment_carto;
    assert.ok(/'key'/.test(cmt), 'the comment must name the key field');
    assert.ok(!/restrict it by origin\/referrer/i.test(cmt),
      'the comment must not claim CARTO basemap keys can be origin/referrer restricted');
    assert.ok(/SUBDOMAIN LABEL/i.test(cmt), 'the comment must document the domain restriction');
  });

  console.log('\n#7 CARTO Basemaps API key: ' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed === 0 ? 0 : 1);
})();
