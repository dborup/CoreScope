/* test-issue-734-customizer-overlays.js — #734 "Later (customizer)", AGENTS
 * rule 8: the DHM overlay viewer settings that M1 (#774) and M2 (#798)
 * hard-coded, in the customizer (public/customize-v2.js, section
 * `dhmOverlays`), and how the overlays use them (public/map-tile-providers.js,
 * MC_DHMOverlays).
 *
 * The real files run in vm sandboxes; nothing is copied. The overlays use
 * the recording Leaflet stub (tests/map-overlay-sandbox.js); the customizer
 * the shared customizer sandbox (tests/customizer-sandbox.js), with the real
 * map-tile-providers.js beside it (index.html loads it first).
 *
 * Covered:
 *   1. the keys and validSetting: hillshade off|terrain|surface, contours a
 *      boolean, contourMinZoom a whole zoom; nothing else;
 *   2. settings(): the viewer's section over the built-ins (off, off, the
 *      server's contour minZoom); the contour min-zoom stays inside the
 *      server's range; junk is ignored;
 *   3. precedence for whether an overlay starts on: URL > the remembered
 *      choice > the customizer default > built-in (off), for the hillshade
 *      pair and for contours; the customizer default never writes the hash;
 *      an off while the default is on rides the link as =0, as M2 writes it
 *      for a default-on overlay;
 *   4. contourMinZoom: the contour layer's minZoom and label follow it, live
 *      on dhm-defaults-changed;
 *   5. the customizer: the section, stored values, import validation,
 *      export round trip, the event, the rendered inputs, one shared rule.
 *
 * Runs via: node test-issue-734-customizer-overlays.js
 */
'use strict';
const fs = require('fs');
const path = require('path');
const assert = require('assert');
const { makeSandbox, buildControl } = require('./tests/map-overlay-sandbox');
const { loadCustomizer: loadCustomizerSandbox } = require('./tests/customizer-sandbox');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const ATTRIBUTION = 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0';
// The block as cmd/server/dhm_proxy.go sends it (#733 parts, #734 overlays).
const DHM = {
  enabled: true, url: '/api/map/dhm/wms', layer: 'dhm_terraen_skyggekort',
  minZoom: 7, maxZoom: 19, bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
  attribution: ATTRIBUTION,
  datasetName: 'Danmarks Højdemodel', datasetUrl: 'https://dataforsyningen.dk/data/930',
  licenseName: 'CC BY 4.0', licenseUrl: 'https://creativecommons.org/licenses/by/4.0/',
  overlays: [
    { id: 'terrain', minZoom: 7, maxZoom: 19, datasetName: 'Danmarks Højdemodel', datasetUrl: 'https://dataforsyningen.dk/data/930' },
    { id: 'surface', minZoom: 7, maxZoom: 19, datasetName: 'Danmarks Højdemodel', datasetUrl: 'https://dataforsyningen.dk/data/928' },
    { id: 'contours', minZoom: 14, maxZoom: 19, datasetName: 'Danmarks Højdemodel', datasetUrl: 'https://dataforsyningen.dk/data/926' },
  ],
};
const LABEL_ID = { terrain: 'dhm-terrain', surface: 'dhm-surface', contours: 'dhm-contours' };
const plain = (o) => JSON.parse(JSON.stringify(o));

// The overlay half: the registry with the stub map, the viewer's section on
// window.SITE_CONFIG (where the customizer pipeline puts it).
function overlaySetup(opts) {
  opts = opts || {};
  const sb = { dhm: opts.dhm || DHM, storage: opts.storage };
  if (opts.hash !== undefined) sb.hash = opts.hash;
  const ctx = makeSandbox(sb);
  if (opts.section !== undefined) ctx.window.SITE_CONFIG = { dhmOverlays: opts.section };
  const { map, ctl } = buildControl(ctx);
  return { ctx, map, ctl, store: ctx.localStorage._raw, D: ctx.window.MC_DHMOverlays };
}
function layerFor(map, id) {
  return map[{ terrain: '_mcTerrainLayer', surface: '_mcSurfaceLayer', contours: '_mcContourLayer' }[id]];
}
function onIds(map) { return ['terrain', 'surface', 'contours'].filter(id => layerFor(map, id) && map.hasLayer(layerFor(map, id))); }
function lastControl(map) { return map._controls[map._controls.length - 1]; }

console.log('\n=== #734 customizer: DHM overlay defaults ===');

// ── 1. Keys and validSetting ────────────────────────────────────────────────

test('the keys are hillshade, contours and contourMinZoom (the M3 opacity is not one yet)', () => {
  const { D } = overlaySetup();
  assert(D, 'MC_DHMOverlays missing');
  assert.deepStrictEqual(Array.from(D.SETTING_KEYS), ['hillshade', 'contours', 'contourMinZoom']);
  assert.deepStrictEqual(Array.from(D.HILLSHADE_VALUES), ['off', 'terrain', 'surface']);
});

test('validSetting: the hillshade enum, a boolean, a whole zoom; nothing else', () => {
  const { D } = overlaySetup();
  const ok = { hillshade: ['off', 'terrain', 'surface'], contours: [true, false], contourMinZoom: [0, 14, 19] };
  const bad = {
    hillshade: ['Surface', 'contours', '1', '0', '', null, true, 1, {}],
    contours: [1, 0, 'true', null, undefined, {}],
    contourMinZoom: [20, -1, 14.5, '15', null, NaN, Infinity, true],
  };
  for (const k of Object.keys(ok)) ok[k].forEach(v => assert.strictEqual(D.validSetting(k, v), true, k + ' rejected ' + JSON.stringify(v)));
  for (const k of Object.keys(bad)) bad[k].forEach(v => assert.strictEqual(D.validSetting(k, v), false, k + ' accepted ' + String(v)));
  ['hasOwnProperty', '__proto__', 'token', 'layer', 'url', 'elevationOpacity'].forEach(k => assert.strictEqual(D.validSetting(k, 'off'), false, k));
});

// ── 2. settings() ───────────────────────────────────────────────────────────

test('settings: the built-ins are off, off and the server\'s contour minZoom', () => {
  const { D } = overlaySetup();
  assert.deepStrictEqual(plain(D.settings({})), { hillshade: 'off', contours: false, contourMinZoom: 14 });
  assert.deepStrictEqual(plain(D.settings(null)), { hillshade: 'off', contours: false, contourMinZoom: 14 });
  assert.deepStrictEqual(Array.from(D.contourZoomRange()), [14, 19]);
});

test('settings: the viewer section over the built-ins; junk ignored', () => {
  const { D } = overlaySetup();
  assert.deepStrictEqual(plain(D.settings({ hillshade: 'surface', contours: true, contourMinZoom: 16 })),
    { hillshade: 'surface', contours: true, contourMinZoom: 16 });
  assert.deepStrictEqual(plain(D.settings({ hillshade: 'SURFACE', contours: 'yes', contourMinZoom: '16', token: 'x' })),
    { hillshade: 'off', contours: false, contourMinZoom: 14 });
});

test('settings: the contour min-zoom stays inside the server\'s range', () => {
  const { D } = overlaySetup();
  assert.strictEqual(D.settings({ contourMinZoom: 10 }).contourMinZoom, 14, 'below the server minimum');
  assert.strictEqual(D.settings({ contourMinZoom: 19 }).contourMinZoom, 19);
  const narrow = JSON.parse(JSON.stringify(DHM));
  narrow.overlays[2] = Object.assign({}, narrow.overlays[2], { minZoom: 15, maxZoom: 17 });
  const n = overlaySetup({ dhm: narrow }).D;
  assert.strictEqual(n.settings({ contourMinZoom: 14 }).contourMinZoom, 15);
  assert.strictEqual(n.settings({ contourMinZoom: 19 }).contourMinZoom, 17, 'above the server maximum');
});

test('settings: no contourMinZoom without contours offered; window.SITE_CONFIG by default', () => {
  const noContours = Object.assign({}, DHM, { overlays: DHM.overlays.slice(0, 2) });
  const { D } = overlaySetup({ dhm: noContours });
  assert.strictEqual(D.settings({ contourMinZoom: 16 }).contourMinZoom, null);
  assert.strictEqual(D.contourZoomRange(), null);
  const s = overlaySetup({ section: { hillshade: 'terrain' } });
  assert.strictEqual(s.D.settings().hillshade, 'terrain', 'the live section was not read');
});

// ── 3. Precedence ───────────────────────────────────────────────────────────

test('built-in: nothing starts on', () => {
  const { map } = overlaySetup();
  assert.deepStrictEqual(onIds(map), []);
});

for (const v of ['terrain', 'surface']) {
  test('customizer hillshade=' + v + ': that hillshade starts on, and the hash is not written', () => {
    const { ctx, map } = overlaySetup({ section: { hillshade: v } });
    assert.deepStrictEqual(onIds(map), [v]);
    assert.deepStrictEqual(ctx.replaced, [], 'the customizer default was written to the link');
    assert.strictEqual(ctx.location.hash, '#/map');
  });
}

test('customizer contours=true: contours start on (with the hillshade default)', () => {
  const { map, ctx } = overlaySetup({ section: { hillshade: 'surface', contours: true } });
  assert.deepStrictEqual(onIds(map), ['surface', 'contours']);
  assert.deepStrictEqual(ctx.replaced, []);
});

test('URL wins over the customizer: ?terrain=0, ?terrain=1, ?contours=0', () => {
  let s = overlaySetup({ hash: '#/map?terrain=0&contours=0', section: { hillshade: 'surface', contours: true } });
  assert.deepStrictEqual(onIds(s.map), []);
  s = overlaySetup({ hash: '#/map?terrain=1', section: { hillshade: 'surface' } });
  assert.deepStrictEqual(onIds(s.map), ['terrain']);
  s = overlaySetup({ hash: '#/map?terrain=surface&contours=1', section: { hillshade: 'terrain' } });
  assert.deepStrictEqual(onIds(s.map), ['surface', 'contours']);
});

test('a garbage URL value falls through to the customizer default', () => {
  const { map } = overlaySetup({ hash: '#/map?terrain=xyz&contours=2', section: { hillshade: 'surface', contours: true } });
  assert.deepStrictEqual(onIds(map), ['surface', 'contours']);
});

test('the remembered layer-control choice wins over the customizer default (as #724 M5)', () => {
  const { map } = overlaySetup({ storage: { 'mc-terrain-dhm': '0', 'mc-contours-dhm': '0' }, section: { hillshade: 'surface', contours: true } });
  assert.deepStrictEqual(onIds(map), []);
  const t = overlaySetup({ storage: { 'mc-terrain-dhm': '1' }, section: { hillshade: 'surface' } });
  assert.deepStrictEqual(onIds(t.map), ['terrain']);
});

test('off while the customizer default is on rides the link as =0; with the built-in it is dropped', () => {
  const s = overlaySetup({ section: { hillshade: 'surface', contours: true } });
  s.map.removeLayer(layerFor(s.map, 'surface'));
  s.map.removeLayer(layerFor(s.map, 'contours'));
  assert(/[?&]terrain=0\b/.test(s.ctx.location.hash), s.ctx.location.hash);
  assert(/[?&]contours=0\b/.test(s.ctx.location.hash), s.ctx.location.hash);
  assert.strictEqual(s.store['mc-terrain-dhm'], '0');
  // Built-in default: as M2, the parameter is dropped.
  const b = overlaySetup({ hash: '#/map?terrain=surface&contours=1' });
  b.map.removeLayer(layerFor(b.map, 'surface'));
  b.map.removeLayer(layerFor(b.map, 'contours'));
  assert.strictEqual(b.ctx.location.hash, '#/map');
});

test('switching the hillshade with a customizer default still leaves one on', () => {
  const s = overlaySetup({ section: { hillshade: 'terrain' } });
  // The control: terrain off, then surface on (and the other order).
  s.map.removeLayer(layerFor(s.map, 'terrain'));
  s.map.addLayer(layerFor(s.map, 'surface'));
  assert(/[?&]terrain=surface\b/.test(s.ctx.location.hash), s.ctx.location.hash);
  assert.strictEqual(s.store['mc-terrain-dhm'], 'surface');
});

test('MC_forgetOverlayPref drops the remembered choice, so the customizer default applies again', () => {
  const s = overlaySetup({ storage: { 'mc-terrain-dhm': '0', 'mc-contours-dhm': '0' } });
  s.ctx.window.MC_forgetOverlayPref('dhm-terrain');
  s.ctx.window.MC_forgetOverlayPref('dhm-contours');
  assert.strictEqual(s.store['mc-terrain-dhm'], undefined);
  assert.strictEqual(s.store['mc-contours-dhm'], undefined);
});

// ── 4. contourMinZoom on the layer ──────────────────────────────────────────

test('the contour layer starts at the customizer min-zoom and says so', () => {
  const { map, ctl } = overlaySetup({ section: { contourMinZoom: 16 } });
  assert.strictEqual(layerFor(map, 'contours').options.minZoom, 16);
  assert(Object.keys(ctl.overlays).indexOf('Contour lines (DHM, zoom 16+)') >= 0, Object.keys(ctl.overlays).join(' | '));
  // The hillshade keeps the server's range.
  assert.strictEqual(layerFor(map, 'terrain').options.minZoom, 7);
});

test('below the server minimum the server\'s gate holds', () => {
  const { map } = overlaySetup({ section: { contourMinZoom: 9 } });
  assert.strictEqual(layerFor(map, 'contours').options.minZoom, 14);
});

test('dhm-defaults-changed: the contour layer and label follow without a reload; the choice stays', () => {
  const s = overlaySetup({ hash: '#/map?contours=1' });
  const layer = layerFor(s.map, 'contours');
  let resets = 0;
  layer._resetView = () => { resets++; };
  layer._map = s.map;
  s.ctx.window.SITE_CONFIG = { dhmOverlays: { contourMinZoom: 17 } };
  s.ctx.window.dispatchEvent(new s.ctx.CustomEvent('dhm-defaults-changed'));
  assert.strictEqual(layerFor(s.map, 'contours'), layer, 'the layer was replaced');
  assert.strictEqual(layer.options.minZoom, 17);
  assert(resets >= 1, 'the layer did not re-check its zoom range');
  const ctl = lastControl(s.map);
  assert(Object.keys(ctl.overlays).indexOf('Contour lines (DHM, zoom 17+)') >= 0, Object.keys(ctl.overlays).join(' | '));
  assert(s.map.hasLayer(layer), 'the viewer\'s choice was dropped');
});

test('a removed map stops listening for dhm-defaults-changed', () => {
  const s = overlaySetup();
  const n = s.map._controls.length;
  s.map.fire('unload');
  s.ctx.window.SITE_CONFIG = { dhmOverlays: { contourMinZoom: 17 } };
  s.ctx.window.dispatchEvent(new s.ctx.CustomEvent('dhm-defaults-changed'));
  assert.strictEqual(s.map._controls.length, n, 'a removed map rebuilt its control');
});

// ── 5. The customizer ───────────────────────────────────────────────────────

function loadCustomizer(opts) {
  opts = opts || {};
  return loadCustomizerSandbox({
    server: opts.server,
    dhm: 'dhm' in opts ? opts.dhm : DHM,
    scripts: opts.noRegistry ? [] : ['map-tile-providers.js'],
  });
}
const stored = (c) => JSON.parse(c.localStorage.getItem('cs-theme-overrides') || '{}');

test('dhmOverlays is a known object section', () => {
  const { api } = loadCustomizer();
  assert(api.VALID_SECTIONS.indexOf('dhmOverlays') >= 0);
  assert.strictEqual(api.validateShape({ dhmOverlays: { hillshade: 'surface' } }).valid, true);
  const bad = api.validateShape({ dhmOverlays: 'surface' });
  assert.strictEqual(bad.valid, false);
  assert(/must be an object/.test(bad.errors.join(' ')), bad.errors.join(' '));
});

test('only valid settings are stored; each rejected one warns', () => {
  const c = loadCustomizer();
  c.api.writeOverrides({ dhmOverlays: { hillshade: 'contours', contours: 'on', contourMinZoom: 25, token: 'secret' } });
  assert.strictEqual(stored(c).dhmOverlays, undefined, JSON.stringify(stored(c)));
  assert.strictEqual(c.warnings.filter(w => /Invalid DHM overlay setting/.test(w)).length, 3, c.warnings.join('\n'));
  const d = loadCustomizer();
  d.api.writeOverrides({ dhmOverlays: { hillshade: 'surface', contours: true, contourMinZoom: 16 } });
  assert.deepStrictEqual(stored(d).dhmOverlays, { hillshade: 'surface', contours: true, contourMinZoom: 16 });
});

test('without the registry on the page no setting is accepted', () => {
  const c = loadCustomizer({ noRegistry: true });
  c.api.writeOverrides({ dhmOverlays: { hillshade: 'surface' } });
  assert.strictEqual(stored(c).dhmOverlays, undefined);
  assert.strictEqual(c.api.dhmOverlaySettings({}), null);
  assert.strictEqual(c.api.renderDHMOverlaySettings(), '');
});

test('validateShape names every bad value', () => {
  const { api } = loadCustomizer();
  const res = api.validateShape({ dhmOverlays: { hillshade: 'x', contours: 1, contourMinZoom: 30, extra: 1 } });
  assert.strictEqual(res.valid, false);
  const msg = res.errors.join(' | ');
  assert(/dhmOverlays\.hillshade = "x" .*off, terrain or surface/.test(msg), msg);
  assert(/dhmOverlays\.contours = "1" .*true or false/.test(msg), msg);
  assert(/dhmOverlays\.contourMinZoom = "30" .*14-19/.test(msg), msg);
  assert.strictEqual(res.errors.length, 3, msg);
  // A whole zoom outside the server's contour range is refused on import too.
  const low = api.validateShape({ dhmOverlays: { contourMinZoom: 10 } });
  assert.strictEqual(low.valid, false, 'z10 imported below the server minimum');
  assert(/dhmOverlays\.contourMinZoom = "10" .*14-19/.test(low.errors.join(' | ')), low.errors.join(' | '));
  assert.strictEqual(api.validateShape({ dhmOverlays: { contourMinZoom: 17 } }).valid, true);
});

test('the dhmOverlays section survives an export round trip', () => {
  const first = loadCustomizer();
  const delta = { dhmOverlays: { hillshade: 'terrain', contours: true, contourMinZoom: 15 } };
  first.api.writeOverrides(delta);
  const exported = first.api.readOverrides();
  assert.deepStrictEqual(plain(exported), delta);
  const second = loadCustomizer();
  const shape = second.api.validateShape(exported);
  assert.strictEqual(shape.valid, true, shape.errors.join('; '));
  second.api.writeOverrides(exported);
  assert.deepStrictEqual(plain(second.api.readOverrides()), delta);
  assert.deepStrictEqual(plain(second.api.dhmOverlaySettings(second.api.computeEffective({}, exported))),
    { hillshade: 'terrain', contours: true, contourMinZoom: 15 });
});

test('a changed setting announces dhm-defaults-changed once the pipeline has run; an unchanged one is silent', () => {
  const c = loadCustomizer({ server: {} });
  c.events.length = 0;
  c.api.writeOverrides({ dhmOverlays: { contourMinZoom: 16 } });
  c.api.init({});
  assert(c.events.indexOf('dhm-defaults-changed') >= 0, 'no signal: ' + c.events.join(', '));
  c.events.length = 0;
  c.api.applyCSS(c.api.computeEffective({}, c.api.readOverrides()), c.api.readOverrides());
  assert.strictEqual(c.events.indexOf('dhm-defaults-changed'), -1, 'a no-op run announced a change');
});

test('the DHM Overlays section: shown only with DHM overlays offered, inputs bounded by the server', () => {
  const c = loadCustomizer();
  c.ctx.window.SITE_CONFIG = { dhmOverlays: { hillshade: 'surface', contours: true, contourMinZoom: 16 } };
  const html = c.api.renderDHMOverlaySettings();
  assert(/DHM Map Overlays/.test(html), html);
  const sel = html.match(/<select[^>]*data-cv2-overlay="hillshade"[^>]*>([\s\S]*?)<\/select>/);
  assert(sel, 'no hillshade select');
  assert.deepStrictEqual((sel[1].match(/value="([a-z]+)"/g) || []).map(s => s.slice(7, -1)), ['off', 'terrain', 'surface']);
  assert(/<option value="surface" selected>/.test(sel[1]), sel[1]);
  const box = html.match(/<input[^>]*data-cv2-overlay="contours"[^>]*>/);
  assert(box && /type="checkbox"/.test(box[0]) && /checked/.test(box[0]), box && box[0]);
  const num = html.match(/<input[^>]*data-cv2-overlay="contourMinZoom"[^>]*>/);
  assert(num && /min="14" max="19"/.test(num[0]) && /value="16"/.test(num[0]), num && num[0]);
  assert(/for="cv2-overlay-hillshade"/.test(html) && /id="cv2-overlay-hillshade"/.test(html), 'label not bound');
  assert(/A link that says otherwise still wins/.test(html));
  // Only what the server offers.
  const terrainOnly = Object.assign({}, DHM, { overlays: [DHM.overlays[0]] });
  const t = loadCustomizer({ dhm: terrainOnly }).api.renderDHMOverlaySettings();
  assert(!/value="surface"/.test(t) && !/data-cv2-overlay="contours"/.test(t) && !/contourMinZoom/.test(t), t);
  assert.strictEqual(loadCustomizer({ dhm: null }).api.renderDHMOverlaySettings(), '', 'shown without DHM');
});

test('the customizer and the overlays share one rule (no second copy of the values or the bounds)', () => {
  const src = fs.readFileSync(path.join(__dirname, 'public', 'customize-v2.js'), 'utf8');
  const code = src.replace(/\/\*[\s\S]*?\*\//g, '').replace(/^\s*\/\/.*$/gm, '');
  const block = code.slice(code.indexOf('function _dhmOverlays'), code.indexOf('function _dhmOverlays') + 4000);
  assert(code.indexOf('function _dhmOverlays') >= 0, 'no _dhmOverlays accessor');
  assert(!/\b14\b|\b19\b/.test(block), 'customize-v2.js hard-codes a DHM zoom bound');
  assert(/D\.validSetting\(/.test(code) && /D\.settings\(/.test(code));
});

test('server map.dhm config stays out: the section never carries the token or layer names', () => {
  const c = loadCustomizer();
  c.api.writeOverrides({ dhmOverlays: { token: 'x', layer: 'dhm_terraen_skyggekort', url: '/evil' }, dhm: { token: 'x' } });
  const s = stored(c);
  assert.strictEqual(s.dhmOverlays, undefined);
  assert.strictEqual(s.dhm, undefined);
});

console.log('\n' + passed + ' passed, ' + failed + ' failed');
if (failed) process.exit(1);
