/* test-issue-734-dhm-overlays.js — DHM overlay toggles and credit (#734 M2).
 *
 * Unit tests for the overlay half of public/map-tile-providers.js in the
 * vm sandbox with the recording Leaflet stub (tests/map-overlay-sandbox.js):
 *   - a dhm block with `overlays` (cmd/server/dhm_proxy.go, #734) offers
 *     terrain, surface and contours, each sending its id as `layers` and
 *     exactly the WMS keys the proxy allow-lists;
 *   - terrain and surface are one either-or hillshade (one stored key, one
 *     hash parameter); contours is its own toggle above it, from the
 *     server's minZoom;
 *   - the hash/storage table: ?terrain=1|terrain|surface|0, ?contours=1|0,
 *     the legacy stored '1', garbage values;
 *   - each overlay's credit links its own dataset (#734 Q5) and the
 *     licence, with the plain #733 text and no "data bearbejdet";
 *   - a block without `overlays` (a server before #734) is terrain only,
 *     exactly as #680 (test-issue-680-dhm-terrain.js keeps pinning that).
 *
 * Runs via: node test-issue-734-dhm-overlays.js
 */
'use strict';
const assert = require('assert');
const { makeSandbox, buildControl, liveCredits } = require('./tests/map-overlay-sandbox');

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
const LABELS = {
  terrain: 'Hillshade: terrain (DHM)',
  surface: 'Hillshade: surface, buildings and trees (DHM)',
  contours: 'Contour lines (DHM, zoom 14+)',
};
const DATASET = { terrain: '/data/930', surface: '/data/928', contours: '/data/926' };

function withOverlays(list) { return Object.assign({}, DHM, { overlays: list }); }
function layerOf(ctl, id) { return ctl.overlays && ctl.overlays[LABELS[id]]; }
function on(map, ctl) {
  return Object.keys(LABELS).filter(id => layerOf(ctl, id) && map.hasLayer(layerOf(ctl, id)));
}
function setup(opts) {
  const ctx = makeSandbox(Object.assign({ dhm: DHM }, opts || {}));
  const { map, ctl } = buildControl(ctx, (opts || {}).withAttribution);
  return { ctx, map, ctl, store: ctx.localStorage._raw };
}

console.log('\n=== #734 M2 DHM overlay toggles and credit ===');

// ── What is offered ────────────────────────────────────────────────────────

test('a block with overlays offers terrain, surface and contours, in that order', () => {
  const { ctl } = setup();
  assert.deepStrictEqual(Object.keys(ctl.overlays), [LABELS.terrain, LABELS.surface, LABELS.contours]);
  assert.deepStrictEqual(ctl.overlayLabels.map(l => l.attrs['data-overlay-id']), ['dhm-terrain', 'dhm-surface', 'dhm-contours']);
});

test('only the ids the server offers are offered (terrain only, surface only)', () => {
  let { ctl } = setup({ dhm: withOverlays([DHM.overlays[0]]) });
  assert.deepStrictEqual(Object.keys(ctl.overlays), [LABELS.terrain]);
  ({ ctl } = setup({ dhm: withOverlays([DHM.overlays[1]]) }));
  assert.deepStrictEqual(Object.keys(ctl.overlays), [LABELS.surface]);
  ({ ctl } = setup({ dhm: withOverlays([]) }));
  assert.strictEqual(ctl.overlays, null, 'an empty list offers nothing');
});

test('an unknown id or a malformed entry is never offered', () => {
  const { ctl } = setup({ dhm: withOverlays([
    { id: 'elevation', minZoom: 10, maxZoom: 19, datasetUrl: 'https://dataforsyningen.dk/data/930' },
    { id: 'contours', minZoom: '14', maxZoom: 19, datasetUrl: 'https://dataforsyningen.dk/data/926' },
    { id: 'surface', minZoom: 7, maxZoom: 25, datasetUrl: 'https://dataforsyningen.dk/data/928' },
    null,
  ]) });
  assert.strictEqual(ctl.overlays, null);
});

test('an overlay above z19 is not offered, even within Leaflet\'s zoom range (#798 review F2)', () => {
  // 20 passes _isZoom (0..22); only the z19 clamp refuses it, so this row
  // pins that clamp: the proxy serves nothing above z19.
  const { ctl } = setup({ dhm: withOverlays([
    Object.assign({}, DHM.overlays[0], { maxZoom: 20 }),
  ]) });
  assert.strictEqual(ctl.overlays, null);
});

test('a block without overlays (pre-#734 server) is terrain only, sending the #680 layer', () => {
  const old = Object.assign({}, DHM); delete old.overlays;
  const { ctl } = setup({ dhm: old });
  const keys = Object.keys(ctl.overlays);
  assert.strictEqual(keys.length, 1);
  assert.strictEqual(ctl.overlays[keys[0]].options.layers, 'dhm_terraen_skyggekort');
});

// ── Request shape ──────────────────────────────────────────────────────────

const KNOWN = ['layers', 'styles', 'format', 'transparent', 'version', 'crs', 'tileSize', 'opacity',
  'pane', 'minZoom', 'maxZoom', 'bounds', 'attribution', 'className'];

for (const id of ['terrain', 'surface', 'contours']) {
  test(id + ': WMS options are exactly the allow-listed keys, layers = the id, styles empty', () => {
    const { ctl } = setup();
    const l = layerOf(ctl, id);
    assert.strictEqual(l.kind, 'wms');
    assert.strictEqual(l._url, '/api/map/dhm/wms');
    const o = l.options;
    assert.deepStrictEqual(Object.keys(o).sort(), KNOWN.slice().sort());
    assert.strictEqual(o.layers, id);
    // The proxy maps the id to the upstream style; the browser never names one.
    assert.strictEqual(o.styles, '');
    assert.strictEqual(o.format, 'image/png');
    assert.strictEqual(o.transparent, true);
    assert.strictEqual(o.version, '1.3.0');
    assert.strictEqual(o.crs.code, 'EPSG:3857');
    assert.strictEqual(o.tileSize, 256);
    assert.deepStrictEqual(o.bounds._bounds, DHM.bounds);
    assert.ok(!JSON.stringify(o).toLowerCase().includes('token'));
  });
}

test('each overlay takes its zoom range from its own server entry', () => {
  const { ctl } = setup({ dhm: withOverlays([
    DHM.overlays[0], DHM.overlays[1], Object.assign({}, DHM.overlays[2], { minZoom: 15, maxZoom: 18 }),
  ]) });
  assert.strictEqual(layerOf(ctl, 'terrain').options.minZoom, 7);
  assert.strictEqual(layerOf(ctl, 'surface').options.maxZoom, 19);
  const c = ctl.overlays['Contour lines (DHM, zoom 15+)'];
  assert.ok(c, 'label follows the server minZoom');
  assert.strictEqual(c.options.minZoom, 15);
  assert.strictEqual(c.options.maxZoom, 18);
});

test('contours: minZoom 14 (no tile below it), opacity 1, own pane above the hillshade, below the monuments', () => {
  const { map, ctl } = setup();
  const c = layerOf(ctl, 'contours').options, t = layerOf(ctl, 'terrain').options;
  assert.strictEqual(c.minZoom, 14);
  assert.strictEqual(c.opacity, 1);
  assert.notStrictEqual(c.pane, t.pane);
  const zc = Number(map.getPane(c.pane).style.zIndex), zt = Number(map.getPane(t.pane).style.zIndex);
  assert.ok(zc > zt && zc < 260, 'contour pane z ' + zc + ' (hillshade ' + zt + ', monuments 260)');
  assert.strictEqual(map.getPane(c.pane).style.pointerEvents, 'none');
});

test('surface: the hillshade pane and opacity, its own tile class', () => {
  const { ctl } = setup();
  const s = layerOf(ctl, 'surface').options, t = layerOf(ctl, 'terrain').options;
  assert.strictEqual(s.pane, t.pane);
  assert.strictEqual(s.opacity, t.opacity);
  assert.notStrictEqual(s.className, t.className);
});

// ── Hash and storage ───────────────────────────────────────────────────────

for (const [name, opts, wantOn, wantStore] of [
  ['nothing → all off', {}, [], {}],
  ['legacy stored "1" → terrain', { storage: { 'mc-terrain-dhm': '1' } }, ['terrain'], { 'mc-terrain-dhm': '1' }],
  ['stored "surface" → surface', { storage: { 'mc-terrain-dhm': 'surface' } }, ['surface'], { 'mc-terrain-dhm': 'surface' }],
  ['stored "0" → off', { storage: { 'mc-terrain-dhm': '0' } }, [], { 'mc-terrain-dhm': '0' }],
  ['?terrain=1 → terrain, saved', { hash: '#/map?terrain=1', storage: { 'mc-terrain-dhm': 'surface' } }, ['terrain'], { 'mc-terrain-dhm': '1' }],
  ['?terrain=terrain → terrain, saved as "1"', { hash: '#/map?terrain=terrain' }, ['terrain'], { 'mc-terrain-dhm': '1' }],
  ['?terrain=surface → surface, saved', { hash: '#/map?terrain=surface', storage: { 'mc-terrain-dhm': '1' } }, ['surface'], { 'mc-terrain-dhm': 'surface' }],
  ['?terrain=0 → off, saved', { hash: '#/map?terrain=0', storage: { 'mc-terrain-dhm': 'surface' } }, [], { 'mc-terrain-dhm': '0' }],
  ['?terrain=xyz, nothing stored → off, nothing saved', { hash: '#/map?terrain=xyz' }, [], {}],
  ['?terrain=xyz falls back to the stored choice', { hash: '#/map?terrain=xyz', storage: { 'mc-terrain-dhm': 'surface' } }, ['surface'], { 'mc-terrain-dhm': 'surface' }],
  ['?terrain=Surface (case) is garbage', { hash: '#/map?terrain=Surface' }, [], {}],
  ['?contours=1 → contours, saved', { hash: '#/map?contours=1' }, ['contours'], { 'mc-contours-dhm': '1' }],
  ['stored contours "1" → contours', { storage: { 'mc-contours-dhm': '1' } }, ['contours'], { 'mc-contours-dhm': '1' }],
  ['?contours=0 wins over stored "1"', { hash: '#/map?contours=0', storage: { 'mc-contours-dhm': '1' } }, [], { 'mc-contours-dhm': '0' }],
  ['?terrain=surface&contours=1 → both', { hash: '#/map?terrain=surface&contours=1' }, ['surface', 'contours'], { 'mc-terrain-dhm': 'surface', 'mc-contours-dhm': '1' }],
]) {
  test('restore: ' + name, () => {
    const { map, ctl, store, ctx } = setup(opts);
    assert.deepStrictEqual(on(map, ctl), wantOn);
    assert.deepStrictEqual(Object.assign({}, store), wantStore);
    assert.strictEqual(ctx.replaced.length, 0, 'the restore never rewrites the hash');
  });
}

test('MC_resolveOverlayOn with values: the hillshade table', () => {
  const r = setup().ctx.window.MC_resolveOverlayOn;
  const known = ['0', '1', 'terrain', 'surface'];
  const terrain = ['1', 'terrain'], surface = ['surface'];
  assert.strictEqual(r('terrain', null, false, terrain, known), true);
  assert.strictEqual(r('surface', '1', false, terrain, known), false);
  assert.strictEqual(r('surface', null, false, surface, known), true);
  assert.strictEqual(r('xyz', 'surface', false, surface, known), true);
  assert.strictEqual(r(null, '1', false, surface, known), false);
  // The 3-argument form is #680's, unchanged.
  assert.strictEqual(r('1', '0'), true);
  assert.strictEqual(r('surface', '1'), true, 'a value the 3-argument form does not know falls back to storage');
});

test('toggle surface on → stored "surface", ?terrain=surface, other params kept', () => {
  const { map, ctl, store, ctx } = setup({ hash: '#/map?lat=56&lon=10&zoom=15' });
  map.addLayer(layerOf(ctl, 'surface'));
  assert.strictEqual(store['mc-terrain-dhm'], 'surface');
  assert.strictEqual(ctx.location.hash, '#/map?lat=56&lon=10&zoom=15&terrain=surface');
});

test('toggle terrain on → "1" and ?terrain=1 (the #680 value)', () => {
  const { map, ctl, store, ctx } = setup({ hash: '#/live' });
  map.addLayer(layerOf(ctl, 'terrain'));
  assert.strictEqual(store['mc-terrain-dhm'], '1');
  assert.strictEqual(ctx.location.hash, '#/live?terrain=1');
});

test('toggle contours on and off → its own key and parameter', () => {
  const { map, ctl, store, ctx } = setup({ hash: '#/map?terrain=surface', storage: { 'mc-terrain-dhm': 'surface' } });
  map.addLayer(layerOf(ctl, 'contours'));
  assert.strictEqual(store['mc-contours-dhm'], '1');
  assert.strictEqual(ctx.location.hash, '#/map?terrain=surface&contours=1');
  map.removeLayer(layerOf(ctl, 'contours'));
  assert.strictEqual(store['mc-contours-dhm'], '0');
  assert.strictEqual(ctx.location.hash, '#/map?terrain=surface');
  assert.strictEqual(store['mc-terrain-dhm'], 'surface', 'the hillshade choice is untouched');
});

test('switching terrain → surface in either order leaves surface chosen', () => {
  for (const order of ['remove-first', 'add-first']) {
    const { map, ctl, store, ctx } = setup({ hash: '#/map?terrain=1' });
    const t = layerOf(ctl, 'terrain'), s = layerOf(ctl, 'surface');
    if (order === 'remove-first') { map.removeLayer(t); map.addLayer(s); }
    else { map.addLayer(s); map.removeLayer(t); }
    assert.strictEqual(store['mc-terrain-dhm'], 'surface', order);
    assert.strictEqual(ctx.location.hash, '#/map?terrain=surface', order);
  }
});

test('turning the chosen hillshade off clears the choice', () => {
  const { map, ctl, store, ctx } = setup({ hash: '#/map?terrain=surface&zoom=9' });
  map.removeLayer(layerOf(ctl, 'surface'));
  assert.strictEqual(store['mc-terrain-dhm'], '0');
  assert.strictEqual(ctx.location.hash, '#/map?zoom=9');
});

// ── Either-or in the layer control ─────────────────────────────────────────

// The inputs of the control as Leaflet builds them: one checkbox per
// overlay, input.layerId = L.stamp(layer).
function inputsFor(ctx, ctl, checked) {
  return Object.keys(LABELS).filter(id => layerOf(ctl, id)).map(id => ({
    id, type: 'checkbox', checked: checked.includes(id), layerId: ctx.L.stamp(layerOf(ctl, id)),
  }));
}

test('checking surface unchecks terrain (and the reverse); contours is independent', () => {
  const { ctx, map, ctl } = setup();
  const pick = ctx.window.MC_uncheckGroupSiblings;
  let inputs = inputsFor(ctx, ctl, ['terrain', 'surface', 'contours']);
  assert.strictEqual(pick(map, inputs, inputs[1]), 1);
  assert.deepStrictEqual(inputs.filter(i => i.checked).map(i => i.id), ['surface', 'contours']);
  inputs = inputsFor(ctx, ctl, ['terrain', 'surface', 'contours']);
  assert.strictEqual(pick(map, inputs, inputs[0]), 1);
  assert.deepStrictEqual(inputs.filter(i => i.checked).map(i => i.id), ['terrain', 'contours']);
  inputs = inputsFor(ctx, ctl, ['terrain', 'contours']);
  assert.strictEqual(pick(map, inputs, inputs[2]), 0, 'contours unchecks nothing');
  assert.deepStrictEqual(inputs.filter(i => i.checked).map(i => i.id), ['terrain', 'contours']);
});

test('unchecking a hillshade checks nothing else (Off is both unchecked)', () => {
  const { ctx, map, ctl } = setup();
  const inputs = inputsFor(ctx, ctl, ['contours']);
  assert.strictEqual(ctx.window.MC_uncheckGroupSiblings(map, inputs, inputs[0]), 0);
  assert.deepStrictEqual(inputs.filter(i => i.checked).map(i => i.id), ['contours']);
});

test('an input of another overlay or a non-checkbox is ignored', () => {
  const { ctx, map, ctl } = setup();
  const inputs = inputsFor(ctx, ctl, ['terrain', 'surface']);
  assert.strictEqual(ctx.window.MC_uncheckGroupSiblings(map, inputs, { type: 'checkbox', checked: true, layerId: -1 }), 0);
  assert.strictEqual(ctx.window.MC_uncheckGroupSiblings(map, inputs, { type: 'radio', checked: true, layerId: inputs[1].layerId }), 0);
  assert.strictEqual(ctx.window.MC_uncheckGroupSiblings(map, inputs, null), 0);
  assert.deepStrictEqual(inputs.filter(i => i.checked).map(i => i.id), ['terrain', 'surface']);
});

// ── Credit ─────────────────────────────────────────────────────────────────

function hrefs(credit) { return (credit.match(/href="[^"]*"/g) || []).map(h => h.slice(6, -1)); }

for (const id of ['terrain', 'surface', 'contours']) {
  test(id + ': credit links its own dataset and the licence, plain #733 text, nothing derived', () => {
    const { ctl } = setup();
    const credit = layerOf(ctl, id).options.attribution;
    assert.deepStrictEqual(hrefs(credit), ['https://dataforsyningen.dk' + DATASET[id], 'https://creativecommons.org/licenses/by/4.0/']);
    assert.strictEqual(credit.replace(/<[^>]*>/g, ''), ATTRIBUTION);
    assert.ok(!/bearbejdet/.test(credit));
    assert.ok(/rel="noopener noreferrer license"/.test(credit));
  });
}

test('an entry with a non-https dataset link keeps the plain dataset mark', () => {
  const { ctl } = setup({ dhm: withOverlays([Object.assign({}, DHM.overlays[2], { datasetUrl: 'javascript:alert(1)' })]) });
  const credit = layerOf(ctl, 'contours').options.attribution;
  assert.deepStrictEqual(hrefs(credit), ['https://creativecommons.org/licenses/by/4.0/']);
  assert.ok(!/javascript/.test(credit));
});

test('Live: with surface and contours on, both credits show; each leaves with its overlay', () => {
  const { map, ctl } = setup({ hash: '#/live?terrain=surface&contours=1' });
  const s = layerOf(ctl, 'surface').options.attribution, c = layerOf(ctl, 'contours').options.attribution;
  assert.notStrictEqual(s, c);
  assert.deepStrictEqual(liveCredits(map), [s, c].sort());
  map.removeLayer(layerOf(ctl, 'contours'));
  assert.deepStrictEqual(liveCredits(map), [s]);
  map.removeLayer(layerOf(ctl, 'surface'));
  assert.deepStrictEqual(liveCredits(map), []);
});

console.log(`\n${passed} passed, ${failed} failed`);
process.exit(failed ? 1 : 0);
