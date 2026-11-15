/* test-issue-792-elevation-overlay.js — the colour-by-elevation overlay
 * (#792), the page half.
 *
 * Unit tests for public/map-tile-providers.js and public/customize-v2.js in
 * the vm sandboxes (tests/map-overlay-sandbox.js, tests/customizer-sandbox.js):
 *   1. the dhm block's `elevation` entry (cmd/server/dhm_proxy.go) is
 *      offered under the hillshade, with its own derived credit ("data
 *      bearbejdet") and the ramp metres; a malformed entry is not;
 *   2. its tile layer: id as `layers`, exactly the allow-listed WMS keys,
 *      its own pane under the hillshade, served z9-13 and upscaled above;
 *   3. URL and storage like every overlay: ?elevation=1|0, mc-elevation-dhm;
 *   4. the legend: a bar from the CSS variables and the metres, built from
 *      DOM nodes only;
 *   5. the customizer: elevationOpacity (10-100 %, built-in 60), stored,
 *      validated, rendered only while offered, applied to an open map.
 *
 * Runs via: node test-issue-792-elevation-overlay.js
 */
'use strict';
const fs = require('fs');
const path = require('path');
const assert = require('assert');
const { makeSandbox, buildControl, makeElement, makeTextNode } = require('./tests/map-overlay-sandbox');
const { loadCustomizer: loadCustomizerSandbox } = require('./tests/customizer-sandbox');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const ATTRIBUTION = 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0';
const DERIVED = ATTRIBUTION + ' — data bearbejdet';
const LEGEND_M = [0, 25, 50, 75, 100, 125, 150, 175];
const ELEVATION = { id: 'elevation', minZoom: 9, maxZoom: 13, datasetName: 'Danmarks Højdemodel',
  datasetUrl: 'https://dataforsyningen.dk/data/930', attribution: DERIVED, legendM: LEGEND_M };
// The block as cmd/server/dhm_proxy.go sends it since #792.
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
    ELEVATION,
  ],
};
const LABEL = 'Colour by elevation (DHM, zoom 9+)';
const plain = (o) => JSON.parse(JSON.stringify(o));

function withElevation(patch) {
  const e = Object.assign({}, ELEVATION, patch);
  return Object.assign({}, DHM, { overlays: DHM.overlays.slice(0, 3).concat([e]) });
}
function setup(opts) {
  opts = opts || {};
  const ctx = makeSandbox(Object.assign({ dhm: DHM }, opts));
  if (opts.section !== undefined) ctx.window.SITE_CONFIG = { dhmOverlays: opts.section };
  const { map, ctl } = buildControl(ctx, opts.withAttribution);
  return { ctx, map, ctl, store: ctx.localStorage._raw, D: ctx.window.MC_DHMOverlays };
}
const layer = (ctl) => ctl.overlays && ctl.overlays[LABEL];

console.log('\n=== #792 colour-by-elevation overlay ===');

// ── 1. Offered ──────────────────────────────────────────────────────────────

test('offered under the hillshade pair, above the contours', () => {
  const { ctl } = setup();
  assert.deepStrictEqual(ctl.overlayLabels.map(l => l.attrs['data-overlay-id']),
    ['dhm-terrain', 'dhm-surface', 'dhm-elevation', 'dhm-contours']);
  assert(layer(ctl), 'no "' + LABEL + '" in ' + Object.keys(ctl.overlays));
});

test('not offered by a server without the entry; the #734 overlays are unchanged', () => {
  const { ctl, D } = setup({ dhm: Object.assign({}, DHM, { overlays: DHM.overlays.slice(0, 3) }) });
  assert.strictEqual(layer(ctl), undefined);
  assert.deepStrictEqual(Array.from(D.offered()), ['terrain', 'surface', 'contours']);
  assert.deepStrictEqual(plain(D.settings({})), { hillshade: 'off', contours: false, contourMinZoom: 14 });
});

test('a malformed entry is not offered (credit, legend, zooms)', () => {
  const bad = [
    { attribution: undefined }, { attribution: '' }, { attribution: 7 },
    { legendM: undefined }, { legendM: [] }, { legendM: [0] }, { legendM: [5, 50] }, { legendM: [0, 50, 50] },
    { legendM: [0, 50, 25] }, { legendM: [0, '50'] }, { legendM: [0, 50.5] }, { legendM: 'x' },
    { minZoom: 14, maxZoom: 13 }, { maxZoom: 20 }, { minZoom: '9' },
  ];
  bad.forEach(p => {
    const { ctl } = setup({ dhm: withElevation(p) });
    assert.strictEqual(layer(ctl), undefined, 'offered with ' + JSON.stringify(p));
  });
});

// ── 2. The tile layer ──────────────────────────────────────────────────────

test('the layer asks for layers=elevation with exactly the allow-listed keys', () => {
  const { ctl } = setup();
  const l = layer(ctl);
  assert.strictEqual(l.kind, 'wms');
  assert.strictEqual(l._url, '/api/map/dhm/wms');
  const o = l.options;
  assert.strictEqual(o.layers, 'elevation');
  assert.deepStrictEqual(Object.keys(o).sort(), ['attribution', 'bounds', 'className', 'crs', 'format', 'layers',
    'maxNativeZoom', 'maxZoom', 'minZoom', 'opacity', 'pane', 'styles', 'tileSize', 'transparent', 'version'].sort());
  assert.strictEqual(o.styles, '');
  assert.strictEqual(o.format, 'image/png');
  assert.strictEqual(o.version, '1.3.0');
});

test('served z9-13, then upscaled to the block maxZoom with no further request', () => {
  const o = layer(setup().ctl).options;
  assert.strictEqual(o.minZoom, 9);
  assert.strictEqual(o.maxNativeZoom, 13);
  assert.strictEqual(o.maxZoom, 19);
  // A block maxZoom below the entry's keeps the entry's.
  const low = layer(setup({ dhm: Object.assign(withElevation({}), { maxZoom: 12 }) }).ctl).options;
  assert.strictEqual(low.maxZoom, 13);
  assert.strictEqual(low.maxNativeZoom, 13);
});

test('its own pane under the hillshade (245 < 250), outside the dark-theme filter', () => {
  const { map, ctl } = setup({ hash: '#/map?elevation=1' });
  const l = layer(ctl);
  assert.strictEqual(l.options.pane, 'mcElevationPane');
  const pane = map.panes.mcElevationPane;
  assert(pane, 'pane not created');
  assert.strictEqual(pane.style.zIndex, '245');
  assert.deepStrictEqual(pane.classes, ['mc-elevation-pane']);
  assert.strictEqual(l.options.className, 'mc-elevation-tiles');
  const css = fs.readFileSync(path.join(__dirname, 'public', 'style.css'), 'utf8');
  assert(!/\.mc-elevation-pane\s*\{[^}]*filter/.test(css), 'the elevation pane is filtered');
});

test('the credit is the derived one, with the dataset and licence linked', () => {
  const o = layer(setup().ctl).options;
  assert(/data bearbejdet/.test(o.attribution), o.attribution);
  assert(/href="https:\/\/dataforsyningen\.dk\/data\/930"/.test(o.attribution), o.attribution);
  assert(/href="https:\/\/creativecommons\.org\/licenses\/by\/4\.0\/"/.test(o.attribution), o.attribution);
  // The #734 overlays keep the plain credit.
  const { ctl } = setup();
  const terrain = ctl.overlays['Hillshade: terrain (DHM)'].options.attribution;
  assert(!/bearbejdet/.test(terrain), terrain);
});

// ── 3. URL and storage ─────────────────────────────────────────────────────

test('off by default; ?elevation=1 turns it on and is remembered', () => {
  let s = setup();
  assert.strictEqual(s.map.hasLayer(layer(s.ctl)), false);
  s = setup({ hash: '#/map?elevation=1' });
  assert.strictEqual(s.map.hasLayer(layer(s.ctl)), true);
  assert.strictEqual(s.store['mc-elevation-dhm'], '1');
});

test('the stored choice applies; the hash wins over it; garbage falls back', () => {
  let s = setup({ storage: { 'mc-elevation-dhm': '1' } });
  assert.strictEqual(s.map.hasLayer(layer(s.ctl)), true);
  s = setup({ storage: { 'mc-elevation-dhm': '1' }, hash: '#/map?elevation=0' });
  assert.strictEqual(s.map.hasLayer(layer(s.ctl)), false);
  assert.strictEqual(s.store['mc-elevation-dhm'], '0');
  s = setup({ storage: { 'mc-elevation-dhm': '1' }, hash: '#/map?elevation=surface' });
  assert.strictEqual(s.map.hasLayer(layer(s.ctl)), true);
});

test('a toggle writes ?elevation=1 and storage; off drops the parameter', () => {
  const s = setup({ hash: '#/map?node=x' });
  const l = layer(s.ctl);
  s.map.addLayer(l);
  assert.strictEqual(s.store['mc-elevation-dhm'], '1');
  assert(/[?&]elevation=1\b/.test(s.ctx.location.hash), s.ctx.location.hash);
  assert(/[?&]node=x\b/.test(s.ctx.location.hash), s.ctx.location.hash);
  s.map.removeLayer(l);
  assert.strictEqual(s.store['mc-elevation-dhm'], '0');
  assert(!/elevation/.test(s.ctx.location.hash), s.ctx.location.hash);
  // Independent of the hillshade's parameter.
  assert(!/terrain=/.test(s.ctx.location.hash), s.ctx.location.hash);
});

// ── 4. Legend ──────────────────────────────────────────────────────────────

test('the legend labels: first, every 50 m not crowding the last, last with its unit', () => {
  const { D } = setup();
  assert.deepStrictEqual(Array.from(D.elevationLegendLabels(LEGEND_M)), [0, 50, 100, 175]);
  assert.deepStrictEqual(Array.from(D.elevationLegendLabels([0, 100])), [0, 100]);
  assert.deepStrictEqual(Array.from(D.elevationLegendLabels([0, 50, 100, 150, 200])), [0, 50, 100, 150, 200]);
});

test('the legend: a bar from the CSS variables in order, the metres as text nodes', () => {
  const { D } = setup();
  const doc = { createElement: makeElement, createTextNode: makeTextNode };
  const box = D.buildElevationLegend(doc, LEGEND_M);
  assert.strictEqual(box.className, 'mc-elev-legend');
  assert.strictEqual(box.attrs.role, 'img');
  assert.strictEqual(box.attrs['aria-label'], 'Elevation colours from 0 to 175 m');
  const [bar, scale] = box.children;
  const vars = bar.style.background.match(/var\(--dhm-elevation-(\d+)\)/g).map(v => Number(v.match(/\d+/)[0]));
  assert.deepStrictEqual(vars, LEGEND_M);
  assert(!/#[0-9a-f]{3,8}\b|rgb/i.test(bar.style.background), 'a hardcoded colour: ' + bar.style.background);
  assert(/^linear-gradient\(to right, var\(--dhm-elevation-0\) 0%, /.test(bar.style.background), bar.style.background);
  assert.deepStrictEqual(scale.children.map(c => c.textContent), ['0', '50', '100', '175 m']);
  assert.deepStrictEqual(scale.children.map(c => c.style.left), ['0%', (50 / 175 * 100) + '%', (100 / 175 * 100) + '%', '100%']);
  // Nothing went through innerHTML.
  const walk = (n) => (n.children || []).every(c => c.nodeType === 3 || (c.nodeType === 1 && walk(c)));
  assert(walk(box));
});

test('the legend sits under the toggle (decorate), and the CSS defines every variable', () => {
  const { ctx, map } = setup();
  const OVERLAY_LABELS = '.leaflet-control-layers-overlays label';
  // A real label with a parent, as L.Control.Layers renders it.
  const parent = makeElement('div');
  const label = makeElement('label');
  label.ownerDocument = { createElement: makeElement, createTextNode: makeTextNode };
  parent.children.push(label);
  label.parentNode = parent;
  label.nextSibling = null;
  parent.insertBefore = (n) => { parent.children.push(n); };
  const L = ctx.L;
  const orig = L.control.layers;
  L.control.layers = (b, o, opts) => {
    const c = orig(b, o, opts);
    const labels = Object.keys(o || {}).map(n => n === LABEL ? label : { name: n, setAttribute() {}, attrs: {} });
    c.getContainer = () => ({ querySelectorAll: (sel) => sel === OVERLAY_LABELS ? labels : [] });
    return c;
  };
  ctx.window.MC_createLayerControl(map, { _auto: true }, 'topleft');
  const legend = parent.children[1];
  assert(legend && legend.className === 'mc-elev-legend', 'no legend after the label');
  const css = fs.readFileSync(path.join(__dirname, 'public', 'style.css'), 'utf8');
  const root = css.slice(css.indexOf(':root {'), css.length);
  LEGEND_M.forEach(m => assert(new RegExp('--dhm-elevation-' + m + ':\\s*#[0-9a-f]{6};').test(root), 'no --dhm-elevation-' + m));
  for (const cls of ['.mc-elev-legend-bar', '.mc-elev-legend-scale']) {
    const rule = css.slice(css.indexOf(cls + ' {'), css.indexOf('}', css.indexOf(cls + ' {')));
    assert(rule && !/#[0-9a-f]{3,8}\b/i.test(rule), cls + ' hardcodes a colour');
  }
});

// ── 5. Customizer: opacity ─────────────────────────────────────────────────

test('elevationOpacity: whole percent 10-100; built-in 60; only while offered', () => {
  const { D } = setup();
  assert(D.SETTING_KEYS.indexOf('elevationOpacity') >= 0);
  [10, 35, 60, 100].forEach(v => assert.strictEqual(D.validSetting('elevationOpacity', v), true, String(v)));
  [9, 101, 0, -5, 50.5, '50', null, NaN, Infinity, true, {}].forEach(v =>
    assert.strictEqual(D.validSetting('elevationOpacity', v), false, String(v)));
  assert.deepStrictEqual(plain(D.settings({})), { hillshade: 'off', contours: false, contourMinZoom: 14, elevationOpacity: 60 });
  assert.strictEqual(D.settings({ elevationOpacity: 35 }).elevationOpacity, 35);
  assert.strictEqual(D.settings({ elevationOpacity: 5 }).elevationOpacity, 60);
  assert.strictEqual(D.settings({ elevationOpacity: '35' }).elevationOpacity, 60);
});

test('the layer opacity is the setting; a changed default reaches an open map at once', () => {
  const { ctx, ctl } = setup({ section: { elevationOpacity: 35 } });
  const l = layer(ctl);
  assert.strictEqual(l.options.opacity, 0.35);
  let set = null;
  l.setOpacity = (v) => { set = v; l.options.opacity = v; return l; };
  ctx.window.SITE_CONFIG = { dhmOverlays: { elevationOpacity: 80 } };
  ctx.window.dispatchEvent(new ctx.CustomEvent('dhm-defaults-changed'));
  assert.strictEqual(set, 0.8);
  // The same layer: the viewer's on/off and the tiles stay.
  const after = ctx.window.MC_DHMOverlays && layer(setup().ctl);
  assert(after);
  // The other overlays' fixed opacities are unchanged.
  assert.strictEqual(ctl.overlays['Hillshade: terrain (DHM)'].options.opacity, 0.65);
  assert.strictEqual(ctl.overlays['Contour lines (DHM, zoom 14+)'].options.opacity, 1);
});

function loadCustomizer(opts) {
  opts = opts || {};
  return loadCustomizerSandbox({ server: opts.server, dhm: 'dhm' in opts ? opts.dhm : DHM, scripts: ['map-tile-providers.js'] });
}
const stored = (c) => JSON.parse(c.localStorage.getItem('cs-theme-overrides') || '{}');

test('customizer: stored when valid, refused and warned otherwise', () => {
  const c = loadCustomizer();
  c.api.writeOverrides({ dhmOverlays: { elevationOpacity: 5 } });
  assert.strictEqual(stored(c).dhmOverlays, undefined);
  assert.strictEqual(c.warnings.filter(w => /Invalid DHM overlay setting/.test(w)).length, 1, c.warnings.join('\n'));
  const d = loadCustomizer();
  d.api.writeOverrides({ dhmOverlays: { elevationOpacity: 40 } });
  assert.deepStrictEqual(stored(d).dhmOverlays, { elevationOpacity: 40 });
});

test('customizer: import names a bad opacity; a good one round-trips', () => {
  const { api } = loadCustomizer();
  const res = api.validateShape({ dhmOverlays: { elevationOpacity: 101 } });
  assert.strictEqual(res.valid, false);
  assert(/dhmOverlays\.elevationOpacity = "101" .*a whole percent 10-100/.test(res.errors.join(' | ')), res.errors.join(' | '));
  assert.strictEqual(api.validateShape({ dhmOverlays: { elevationOpacity: 45 } }).valid, true);
  const first = loadCustomizer();
  first.api.writeOverrides({ dhmOverlays: { elevationOpacity: 45 } });
  const second = loadCustomizer();
  second.api.writeOverrides(first.api.readOverrides());
  assert.strictEqual(second.api.dhmOverlaySettings(second.api.computeEffective({}, second.api.readOverrides())).elevationOpacity, 45);
});

test('customizer: the opacity input is rendered only while the overlay is offered', () => {
  const c = loadCustomizer();
  c.ctx.window.SITE_CONFIG = { dhmOverlays: { elevationOpacity: 45 } };
  const html = c.api.renderDHMOverlaySettings();
  const inp = html.match(/<input[^>]*data-cv2-overlay="elevationOpacity"[^>]*>/);
  assert(inp, 'no opacity input: ' + html);
  assert(/type="number"/.test(inp[0]) && /min="10" max="100"/.test(inp[0]) && /value="45"/.test(inp[0]), inp[0]);
  assert(/for="cv2-overlay-elevationOpacity"/.test(html) && /id="cv2-overlay-elevationOpacity"/.test(html), 'label not bound');
  const without = loadCustomizer({ dhm: Object.assign({}, DHM, { overlays: DHM.overlays.slice(0, 3) }) }).api.renderDHMOverlaySettings();
  assert(!/elevationOpacity/.test(without), without);
});

test('a changed opacity announces dhm-defaults-changed', () => {
  const c = loadCustomizer({ server: {} });
  c.events.length = 0;
  c.api.writeOverrides({ dhmOverlays: { elevationOpacity: 30 } });
  c.api.init({});
  assert(c.events.indexOf('dhm-defaults-changed') >= 0, c.events.join(', '));
});

console.log('\n' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
