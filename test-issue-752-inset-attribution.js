/* test-issue-752-inset-attribution.js — #752.
 *
 * Every inset map built on nodes.js `_applyTilesToNodeMap` renders the
 * viewer's active base-map provider but used to be created with
 * `attributionControl: false` and never credited it. Esri requires a
 * visible "Powered by Esri" and source credit; OSM and CARTO require their
 * credit too. The fix reuses the registry's own control from #735
 * (`MC_ensureAttribution`, compact), so there is one mechanism for Live and
 * for the insets.
 *
 *   I1  Per inset type (node detail side pane and full page, node-reach,
 *       packet-path, area-nodes, rx-coverage, Analytics subpath minimap):
 *       the helper leaves exactly one attribution control on the map,
 *       bottom-right, with no Leaflet prefix, carrying the compact class
 *       and the active provider's exact credit — Esri World Imagery's
 *       "Powered by Esri | …" with Esri on, the OSM credit with it off.
 *   I2  The credit follows the base layer: removing the layer the helper
 *       added empties the control, and a credit swapped in place with
 *       MC_setLayerAttribution goes with the layer too (#735 N1 in the
 *       inset context).
 *   I3  One control per map: a second helper call (a re-render) adds none,
 *       a reference overlay (a provider's refUrl) adds no second credit,
 *       and a map that brings its own control (the Map page, the
 *       customizer previews) is left alone.
 *   I4  Wiring: every inset source file that creates a Leaflet map with
 *       `attributionControl: false` routes it through the shared helper (or
 *       builds the layer control, as Live does), so a new inset cannot
 *       quietly ship uncredited.
 *   I5  The compact credit is styled: public/style.css sizes the class the
 *       registry adds, from CSS variables, and the bottom-right QR overlays
 *       of the node-detail insets clear it.
 *
 * Runs via: node test-issue-752-inset-attribution.js (pure vm sandbox).
 * PUBLIC_DIR=<dir> runs it against another copy of public/ (used to show
 * the tests red on the pre-fix frontend).
 */
'use strict';
const vm     = require('vm');
const fs     = require('fs');
const path   = require('path');
const assert = require('assert');
const { makeLeaflet, makeMap } = require('./tests/map-overlay-sandbox');

const PUBLIC_DIR = process.env.PUBLIC_DIR || path.join(__dirname, 'public');
const read = (f) => fs.readFileSync(path.join(PUBLIC_DIR, f), 'utf8');

const POWERED_BY   = 'Powered by <a href="https://www.esri.com">Esri</a> | ';
const ESRI_CREDIT  = POWERED_BY + 'Tiles © Esri — Source: Esri, Vantor, Earthstar Geographics, and the GIS User Community';
const OSM_CREDIT   = '© OpenStreetMap contributors';
const COMPACT_CLASS = 'mc-credit-compact';
const ESRI_ON      = { tiles: { providers: { esri: { enabled: true } } } };

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

// The six inset types named in #752, with the Leaflet options each one
// really passes and the container it mounts in. node detail has two maps
// (the full page and the side pane), so seven call sites.
const INSETS = [
  { name: 'node detail, full page (#nodeFullMap, nodes.js)',        opts: { zoomControl: true,  attributionControl: false } },
  { name: 'node detail, side pane (#nodeMap, nodes.js)',            opts: { zoomControl: false, attributionControl: false } },
  { name: 'node-reach link map (#nqMap, node-reach-map.js)',        opts: { zoomControl: true,  attributionControl: false } },
  { name: 'packet-path map (packet-path-map.js)',                   opts: { zoomControl: true,  attributionControl: false } },
  { name: 'area-nodes map (area-nodes-map.js)',                     opts: { zoomControl: true,  attributionControl: false } },
  { name: 'rx-coverage map (#rxMap, rx-coverage.js)',               opts: { zoomControl: true,  attributionControl: false } },
  { name: 'Analytics subpath minimap (#subpathMap, analytics.js)',  opts: { zoomControl: false, attributionControl: false } },
];

// Files that own one of those maps, and the container each passes to L.map.
const INSET_FILES = {
  'nodes.js':            ['nodeFullMap', 'nodeMap'],
  'node-reach-map.js':   ['containerId'],
  'packet-path-map.js':  ['packetPathMapContainer'],
  'area-nodes-map.js':   ['areaNodesMapContainer'],
  'rx-coverage.js':      ['rxMap'],
  'analytics.js':        ['subpathMap'],
};

// ── sandbox ────────────────────────────────────────────────────────────────
// The real registry plus roles.js (getTileUrl / getActiveTileProvider), and
// the real _applyTilesToNodeMap source lifted out of nodes.js: the inset
// behaviour under test is exactly what ships.
function extractHelper(src) {
  const m = src.match(/function\s+_applyTilesToNodeMap\s*\(map\)\s*\{/);
  if (!m) throw new Error('_applyTilesToNodeMap definition not found in nodes.js');
  let depth = 0, i = m.index;
  for (; i < src.length; i++) {
    if (src[i] === '{') depth++;
    else if (src[i] === '}' && --depth === 0) { i++; break; }
  }
  return src.slice(m.index, i);
}

function makeStorage(init) {
  const store = Object.assign({}, init || {});
  return {
    getItem(k) { return Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null; },
    setItem(k, v) { store[k] = String(v); },
    removeItem(k) { delete store[k]; },
  };
}

// opts: theme, mapCfg (MC_MAP_CFG at parse time), storage
function sandbox(opts) {
  opts = opts || {};
  const ctx = {
    console: { log() {}, warn() {}, error() {} },
    setTimeout, clearTimeout,
    JSON, Date, Math, Object, Array, String, Number, Boolean, Set, Map, URLSearchParams,
    fetch: () => Promise.resolve({ ok: false, json: () => Promise.resolve({}) }),
    localStorage: makeStorage(opts.storage),
    document: {
      documentElement: { getAttribute: () => opts.theme || 'light', style: { getPropertyValue: () => '' } },
      querySelector: () => null,
      querySelectorAll: () => [],
      getElementById: () => null,
      createElement: () => ({ style: {}, appendChild() {}, setAttribute() {}, addEventListener() {} }),
      addEventListener() {},
      body: { appendChild() {}, style: {} },
      head: { appendChild() {} },
      readyState: 'complete',
    },
    window: {
      addEventListener() {},
      dispatchEvent() { return true; },
      matchMedia: () => ({ matches: false, addEventListener() {} }),
    },
    CustomEvent: function (type, init) { this.type = type; this.detail = (init && init.detail) || null; },
    L: makeLeaflet(),
  };
  ctx.window.localStorage = ctx.localStorage;
  ctx.window.document = ctx.document;
  if (opts.mapCfg !== undefined) ctx.window.MC_MAP_CFG = opts.mapCfg;
  ctx.globalThis = ctx;
  vm.createContext(ctx);
  for (const file of ['map-tile-providers.js', 'roles.js']) {
    vm.runInContext(read(file), ctx, { filename: file });
    // Mirror window.* back to sandbox globals, so a later file referencing
    // a bare name (which is window.X in a browser) resolves it.
    for (const k of Object.keys(ctx.window)) if (!(k in ctx)) ctx[k] = ctx.window[k];
  }
  vm.runInContext(extractHelper(read('nodes.js')), ctx, { filename: 'nodes.js#_applyTilesToNodeMap' });
  return ctx;
}

// One inset: the map as that caller creates it, then the shared helper.
function inset(ctx, opts) {
  const map = makeMap();
  if (opts.attributionControl !== false) map.attributionControl = { builtIn: true };
  ctx._insetMap = map;
  const layer = vm.runInContext('_applyTilesToNodeMap(_insetMap)', ctx);
  return { map, layer };
}

const controls = (map) => map._attributionControls.filter((c) => !c._removed);
const credits = (map) => controls(map).flatMap((c) => c.credits()).sort();

// ── I1: every inset credits its base map ───────────────────────────────────
console.log('\n── #752 I1: each inset type credits the provider it renders ──');

for (const ins of INSETS) {
  test('I1 ' + ins.name + ': one compact control with the Esri World Imagery credit', () => {
    const ctx = sandbox({ mapCfg: ESRI_ON, storage: { 'mc-light-tile-provider': 'esri-imagery' } });
    const { map, layer } = inset(ctx, ins.opts);
    assert.strictEqual(layer.options.attribution, ESRI_CREDIT, 'the tile layer carries the credit');
    assert.strictEqual(controls(map).length, 1, 'exactly one attribution control');
    assert.deepStrictEqual(credits(map), [ESRI_CREDIT]);
    const ctl = controls(map)[0];
    assert.strictEqual(map.attributionControl, ctl, 'the map claims it');
    assert.strictEqual(ctl.opts.position, 'bottomright');
    assert.strictEqual(ctl.opts.prefix, false, 'no Leaflet prefix in an inset');
    assert.ok(ctl.getContainer().classList.contains(COMPACT_CLASS),
      'the control is marked compact — got ' + JSON.stringify(ctl.classes));
  });
}

test('I1 the compact class is the one the registry publishes', () => {
  const ctx = sandbox({ mapCfg: ESRI_ON });
  assert.strictEqual(ctx.window.MC_CREDIT_COMPACT_CLASS, COMPACT_CLASS);
});

test('I1 with Esri off the inset credits OSM instead, in the same control', () => {
  for (const ins of INSETS) {
    const ctx = sandbox({ storage: { 'mc-light-tile-provider': 'esri-imagery' } });
    const { map, layer } = inset(ctx, ins.opts);
    assert.strictEqual(layer.options.attribution, OSM_CREDIT, ins.name);
    assert.deepStrictEqual(credits(map), [OSM_CREDIT], ins.name);
    assert.strictEqual(controls(map).length, 1, ins.name);
  }
});

test('I1 dark theme: the inset credits the dark provider it renders', () => {
  const ctx = sandbox({ theme: 'dark', mapCfg: ESRI_ON, storage: { 'mc-dark-tile-provider': 'esri-imagery-dark' } });
  const { map } = inset(ctx, INSETS[0].opts);
  assert.deepStrictEqual(credits(map), [ESRI_CREDIT]);
});

// ── I2: the credit follows the base layer ──────────────────────────────────
console.log('\n── #752 I2: the credit follows the base layer ──');

test('I2 removing the inset\'s tile layer empties the control', () => {
  for (const ins of INSETS) {
    const ctx = sandbox({ mapCfg: ESRI_ON, storage: { 'mc-light-tile-provider': 'esri-imagery' } });
    const { map, layer } = inset(ctx, ins.opts);
    map.removeLayer(layer);
    assert.deepStrictEqual(credits(map), [], ins.name);
    assert.strictEqual(controls(map).length, 1, ins.name + ': the control stays for the map\'s lifetime');
  }
});

// #735 N1, in the inset context: the hook must read getAttribution() at
// removal, not the text it saw when the hook was added.
test('I2 a credit swapped in place with MC_setLayerAttribution goes with the layer', () => {
  const ctx = sandbox({ mapCfg: ESRI_ON, storage: { 'mc-light-tile-provider': 'esri-imagery' } });
  const { map, layer } = inset(ctx, INSETS[0].opts);
  ctx.window.MC_setLayerAttribution(map, layer, OSM_CREDIT);
  assert.deepStrictEqual(credits(map), [OSM_CREDIT]);
  map.removeLayer(layer);
  assert.deepStrictEqual(credits(map), []);
});

// ── I3: one control per map ────────────────────────────────────────────────
console.log('\n── #752 I3: one control per map ──');

test('I3 a second helper call on the same map (a re-render) adds no second control', () => {
  const ctx = sandbox({ mapCfg: ESRI_ON, storage: { 'mc-light-tile-provider': 'esri-imagery' } });
  const { map } = inset(ctx, INSETS[0].opts);
  const again = vm.runInContext('_applyTilesToNodeMap(_insetMap)', ctx);
  assert.strictEqual(controls(map).length, 1);
  assert.deepStrictEqual(credits(map), [ESRI_CREDIT], 'the same credit is counted once per layer');
  map.removeLayer(again);
  assert.deepStrictEqual(credits(map), [ESRI_CREDIT], 'the first layer still credits');
});

test('I3 a provider reference overlay adds no second credit', () => {
  const ctx = sandbox({ mapCfg: ESRI_ON, storage: { 'mc-light-tile-provider': 'esri-imagery' } });
  const spec = ctx.window.MC_getTileSpec('light');
  // No shipped style has a refUrl today; a synthetic one exercises the
  // second-layer branch of the helper.
  ctx.window.getActiveTileProvider = () => ({
    attribution: spec.attribution, refUrl: 'https://example.invalid/ref/{z}/{y}/{x}', invertFilter: null,
  });
  const { map } = inset(ctx, INSETS[0].opts);
  assert.strictEqual(map.layers.size, 2, 'base plus reference overlay');
  assert.deepStrictEqual(credits(map), [ESRI_CREDIT]);
  assert.strictEqual(controls(map).length, 1);
});

test('I3 a map that brings its own control (Map page, customizer previews) is left alone', () => {
  const ctx = sandbox({ mapCfg: ESRI_ON, storage: { 'mc-light-tile-provider': 'esri-imagery' } });
  const { map } = inset(ctx, { attributionControl: true });
  assert.strictEqual(controls(map).length, 0, 'the registry adds none');
  assert.strictEqual(map.attributionControl.builtIn, true);
});

// ── I4: every inset is wired to the shared helper ──────────────────────────
console.log('\n── #752 I4: no inset creates an uncredited map ──');

// A map built with attributionControl: false has no credit of its own, so
// its creating function must route it through the shared helper (the insets)
// or build the layer control (Live, #735). A new inset that does neither
// fails here.
function creditedMapSites(src) {
  const out = [];
  const re = /L\.map\(([^)]*?)\{([^}]*)\}\s*\)/g;
  let m;
  while ((m = re.exec(src))) {
    if (!/attributionControl\s*:\s*false/.test(m[2])) continue;
    const container = (m[1].match(/^\s*'?"?([A-Za-z0-9_#.-]+)'?"?\s*,/) || [, m[1].trim()])[1];
    // The call's surrounding statement plus the next 2000 characters: the
    // same function body, where the credit has to be attached.
    const after = src.slice(m.index, m.index + 2000);
    out.push({
      container,
      credited: /_applyTilesToNodeMap\s*\(/.test(after) ||
                /MC_ensureAttribution\s*\(/.test(after) ||
                /MC_createLayerControl\s*\(/.test(src.slice(m.index)),
    });
  }
  return out;
}

for (const [file, containers] of Object.entries(INSET_FILES)) {
  test('I4 ' + file + ': every attributionControl:false map is routed through the helper', () => {
    const sites = creditedMapSites(read(file));
    assert.deepStrictEqual(sites.map((s) => s.container).sort(), containers.slice().sort(),
      'the maps found in ' + file);
    for (const s of sites) assert.ok(s.credited, file + ': ' + s.container + ' attaches no credit');
  });
}

test('I4 the only uncredited map left in public/ is the one #752 names out of scope', () => {
  // home.js's My-Mesh health minimap hardcodes OSM tiles instead of the
  // shared helper, so it is not one of the inset maps #752 lists; its OSM
  // credit gap is recorded in the PR, not fixed here. Any OTHER uncredited
  // map is a regression.
  const known = { 'home.js': ['mapEl'] };
  const found = {};
  for (const file of fs.readdirSync(PUBLIC_DIR).filter((f) => f.endsWith('.js'))) {
    const bad = creditedMapSites(read(file)).filter((s) => !s.credited).map((s) => s.container);
    if (bad.length) found[file] = bad.sort();
  }
  assert.deepStrictEqual(found, known);
});

// ── I5: the compact credit is sized, and overlays clear it ─────────────────
console.log('\n── #752 I5: the compact credit is styled from CSS variables ──');

test('I5 style.css sizes .leaflet-control-attribution.mc-credit-compact from variables', () => {
  const css = read('style.css');
  const rule = css.match(/\.leaflet-control-attribution\.mc-credit-compact\s*\{([^}]*)\}/);
  assert.ok(rule, 'no rule for the compact credit class — the control would render full size');
  for (const prop of ['font-size', 'line-height', 'max-width', 'max-height', 'overflow-y']) {
    assert.ok(new RegExp('(^|;)\\s*' + prop + '\\s*:').test(rule[1]), 'missing ' + prop);
  }
  const vars = rule[1].match(/var\(--mc-credit-compact-[a-z-]+/g) || [];
  assert.ok(vars.length >= 4, 'the sizes must come from CSS variables — got ' + JSON.stringify(vars));
  for (const v of vars) {
    const name = v.slice(4);
    assert.ok(new RegExp('\\n\\s*' + name + '\\s*:').test(css), name + ' is not defined');
  }
  assert.ok(!/#[0-9a-fA-F]{3,8}\b|rgba?\(/.test(rule[1]), 'no hardcoded colour in the rule: ' + rule[1]);
});

test('I5 the node-detail QR overlays clear the credit in the same corner', () => {
  const css = read('style.css');
  // Every rule that pins one of the two QR overlays over the map (the side
  // pane's at any width, the full page's at <=640px) must lift it above the
  // credit, which paints in the same corner.
  for (const sel of ['.node-map-qr-overlay', '.node-top-row .node-qr-wrap']) {
    const re = new RegExp(sel.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + '\\s*\\{([^}]*)\\}', 'g');
    const pinned = [...css.matchAll(re)].map((m) => m[1]).filter((b) => /position:\s*absolute/.test(b));
    assert.strictEqual(pinned.length, 1, 'expected one rule pinning ' + sel + ' over the map');
    assert.ok(/bottom:\s*var\(--mc-credit-compact-clearance\)/.test(pinned[0]),
      sel + ' does not clear the credit: ' + pinned[0].trim());
  }
  assert.ok(/\n\s*--mc-credit-compact-clearance\s*:[^;]*var\(--mc-credit-compact-max-height\)/.test(css),
    'the clearance must be derived from the credit cap');
});

console.log(`\n#752 inset attribution: ${passed} passed, ${failed} failed`);
if (failed) process.exit(1);
