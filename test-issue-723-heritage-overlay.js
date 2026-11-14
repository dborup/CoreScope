/* test-issue-723-heritage-overlay.js — protected ancient monuments overlay
 * (#723 M1) and the overlay plumbing it shares with the #680 DHM terrain
 * hillshade in public/map-tile-providers.js.
 *
 * vm sandbox + recording Leaflet stub (tests/map-overlay-sandbox.js):
 *   - offered only for an enabled heritage block with an https URL, layers
 *     and integer zooms;
 *   - the WMS options: the agency's URL, both layers in one GetMap, WMS
 *     1.3.0 / EPSG:3857 / transparent PNG, minZoom 12, bounds, credit, and
 *     no option Leaflet would turn into an extra WMS parameter;
 *   - its own pane just above the hillshade pane, below overlays/markers;
 *   - the shared per-viewer preference, for BOTH overlays: off by default,
 *     hash wins over storage and is persisted, toggles write storage + hash,
 *     an empty hash never leaks the parameter into the query string (#682
 *     review F4);
 *   - the two overlays are independent, and Live's attribution control
 *     (one, for the map's lifetime) shows each overlay's credit exactly
 *     while it is on, also on a second toggle;
 *   - roles.js hands cfg.heritage to the registry.
 *
 * The exact request Leaflet sends is pinned by the E2E suite
 * (test-issue-723-heritage-overlay-e2e.js) with the real Leaflet and a
 * stubbed service. Nothing here contacts the real WMS.
 *
 * Runs via: node test-issue-723-heritage-overlay.js
 */
'use strict';
const fs = require('fs');
const path = require('path');
const assert = require('assert');
const { makeMap, makeSandbox, buildControl, liveAttributions, liveCredits } = require('./tests/map-overlay-sandbox');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

// What cmd/server/heritage_overlay.go sends while map.heritage is enabled.
const HERITAGE = {
  enabled: true,
  url: 'https://www.kulturarv.dk/ffgeoserver/public/wms',
  layers: 'fundogfortidsminder_areal_beskyttelse,fundogfortidsminder_punkt_fredet',
  minZoom: 12, maxZoom: 19,
  bounds: [[54.4646, 8.0899], [57.7289, 15.5689]],
  attribution: '© Slots- og Kulturstyrelsen',
};
const DHM = {
  enabled: true, url: '/api/map/dhm/wms', layer: 'dhm_terraen_skyggekort',
  minZoom: 7, maxZoom: 19, bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
  attribution: 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0',
};
const HERITAGE_LABEL = 'Protected ancient monuments (zoom 12+)';
const TERRAIN_LABEL = 'Terrain (DHM hillshade)';

const heritageLayer = (ctl) => (ctl && ctl.overlays ? ctl.overlays[HERITAGE_LABEL] : undefined);
const terrainLayer = (ctl) => (ctl && ctl.overlays ? ctl.overlays[TERRAIN_LABEL] : undefined);

console.log('\n=== #723 protected ancient monuments overlay ===');

// ── Offered only when enabled ───────────────────────────────────────────────

test('no heritage block → not offered', () => {
  const ctx = makeSandbox({});
  assert.strictEqual(ctx.window.MC_getHeritageConfig(), null);
  assert.strictEqual(buildControl(ctx).ctl.overlays, null);
});

for (const [name, cfg] of [
  ['enabled:false', Object.assign({}, HERITAGE, { enabled: false })],
  ['enabled:"true" (string)', Object.assign({}, HERITAGE, { enabled: 'true' })],
  ['plain http url', Object.assign({}, HERITAGE, { url: 'http://www.kulturarv.dk/ffgeoserver/public/wms' })],
  ['relative url', Object.assign({}, HERITAGE, { url: '/wms' })],
  ['missing layers', Object.assign({}, HERITAGE, { layers: '' })],
  ['minZoom as a string', Object.assign({}, HERITAGE, { minZoom: '12' })],
  ['fractional maxZoom', Object.assign({}, HERITAGE, { maxZoom: 18.5 })],
  ['missing bounds', Object.assign({}, HERITAGE, { bounds: undefined })],
  ['null', null],
]) {
  test('not offered for heritage block ' + name, () => {
    const ctx = makeSandbox({ heritage: cfg });
    assert.strictEqual(ctx.window.MC_getHeritageConfig(), null);
    assert.strictEqual(buildControl(ctx).ctl.overlays, null);
  });
}

test('enabled → one overlay, labelled with the zoom it starts at', () => {
  const ctx = makeSandbox({ heritage: HERITAGE });
  const { ctl } = buildControl(ctx);
  assert.deepStrictEqual(Object.keys(ctl.overlays), [HERITAGE_LABEL]);
  assert.strictEqual(ctl.overlayLabels[0].attrs['data-overlay-id'], 'heritage-monuments');
});

test('both enabled → terrain then monuments, each label tagged with its own id', () => {
  const ctx = makeSandbox({ dhm: DHM, heritage: HERITAGE });
  const { ctl } = buildControl(ctx);
  assert.deepStrictEqual(Object.keys(ctl.overlays), [TERRAIN_LABEL, HERITAGE_LABEL]);
  assert.deepStrictEqual(ctl.overlayLabels.map(l => l.attrs['data-overlay-id']), ['dhm-terrain', 'heritage-monuments']);
  assert.notStrictEqual(terrainLayer(ctl), heritageLayer(ctl));
});

test('appears when config arrives async (fromConfig rebuild)', () => {
  const ctx = makeSandbox({});
  const { map } = buildControl(ctx);
  ctx.window.MC_HERITAGE_CFG = HERITAGE;
  ctx.window.MC_initTileRegistry(true);
  assert.ok(heritageLayer(map._controls[map._controls.length - 1]), 'offered after config load');
});

test('dropped (and switched off) when the server stops offering it', () => {
  const ctx = makeSandbox({ heritage: HERITAGE, storage: { 'mc-heritage': '1' } });
  const { map, ctl } = buildControl(ctx, false);
  const l = heritageLayer(ctl);
  assert.strictEqual(map.hasLayer(l), true);
  assert.deepStrictEqual(liveCredits(map), [HERITAGE.attribution]);
  ctx.window.MC_HERITAGE_CFG = null;
  ctx.window.MC_initTileRegistry(true);
  assert.strictEqual(map.hasLayer(l), false, 'layer left on the map');
  assert.strictEqual(map._mcHeritageLayer, null);
  assert.strictEqual(map._controls[map._controls.length - 1].overlays, null);
  assert.deepStrictEqual(liveCredits(map), [], 'credit left without an overlay');
  assert.strictEqual(liveAttributions(map).length, 1);
});

// ── Request shape, opacity, z-order ────────────────────────────────────────

test('WMS options: the agency URL, both layers in one GetMap, 1.3.0 / EPSG:3857 / transparent PNG', () => {
  const ctx = makeSandbox({ heritage: HERITAGE });
  const l = heritageLayer(buildControl(ctx).ctl);
  assert.strictEqual(l.kind, 'wms');
  assert.strictEqual(l._url, 'https://www.kulturarv.dk/ffgeoserver/public/wms');
  const o = l.options;
  assert.strictEqual(o.layers, 'fundogfortidsminder_areal_beskyttelse,fundogfortidsminder_punkt_fredet');
  assert.strictEqual(o.styles, '');
  assert.strictEqual(o.format, 'image/png');
  assert.strictEqual(o.transparent, true);
  assert.strictEqual(o.version, '1.3.0');
  assert.strictEqual(o.crs.code, 'EPSG:3857');
  assert.strictEqual(o.tileSize, 256);
  assert.strictEqual(o.minZoom, 12);
  assert.strictEqual(o.maxZoom, 19);
  assert.deepStrictEqual(o.bounds._bounds, HERITAGE.bounds);
  assert.strictEqual(o.attribution, '© Slots- og Kulturstyrelsen');
  assert.strictEqual(o.className, 'mc-heritage-tiles');
  // Leaflet sends every option it does not know as a WMS parameter. These
  // are its own Layer/GridLayer/TileLayer/WMS options plus the five WMS
  // parameters above, and nothing else may be passed.
  const known = ['layers', 'styles', 'format', 'transparent', 'version', 'crs', 'tileSize', 'opacity',
    'pane', 'minZoom', 'maxZoom', 'bounds', 'attribution', 'className'];
  assert.deepStrictEqual(Object.keys(o).sort(), known.slice().sort());
});

test('opacity leaves the base map readable under the zones (0.7–0.95)', () => {
  const o = heritageLayer(buildControl(makeSandbox({ heritage: HERITAGE })).ctl).options;
  assert.ok(o.opacity >= 0.7 && o.opacity <= 0.95, 'opacity ' + o.opacity);
});

test('own pane just above the hillshade pane, below overlays (400) / markers (600)', () => {
  const ctx = makeSandbox({ dhm: DHM, heritage: HERITAGE });
  const { map, ctl } = buildControl(ctx);
  const hp = map.getPane(heritageLayer(ctl).options.pane);
  const tp = map.getPane(terrainLayer(ctl).options.pane);
  assert.ok(hp && tp, 'panes created');
  assert.notStrictEqual(hp, tp, 'shares the hillshade pane');
  assert.notStrictEqual(heritageLayer(ctl).options.pane, 'tilePane', 'in tilePane (would get the dark invert filter)');
  const hz = Number(hp.style.zIndex), tz = Number(tp.style.zIndex);
  assert.ok(hz > tz && hz < 400, `z-order terrain=${tz} heritage=${hz}`);
  assert.strictEqual(hp.style.pointerEvents, 'none');
  assert.ok(hp.classes.includes('mc-heritage-pane'));
  assert.ok(!hp.classes.includes('mc-terrain-pane'), 'would get the hillshade blend/invert');
});

// ── Shared preference plumbing, for both overlays ─────────────────────────

const OVERLAYS = [
  { name: 'terrain', param: 'terrain', key: 'mc-terrain-dhm', cfg: { dhm: DHM }, layer: terrainLayer },
  { name: 'heritage', param: 'heritage', key: 'mc-heritage', cfg: { heritage: HERITAGE }, layer: heritageLayer },
];

for (const ov of OVERLAYS) {
  const sb = (extra) => makeSandbox(Object.assign({}, ov.cfg, extra || {}));

  test(`${ov.name}: off by default, hash untouched`, () => {
    const ctx = sb();
    const { map, ctl } = buildControl(ctx);
    assert.strictEqual(map.hasLayer(ov.layer(ctl)), false);
    assert.strictEqual(ctx.replaced.length, 0);
  });

  test(`${ov.name}: stored "1" restores it on without rewriting the hash`, () => {
    const ctx = sb({ storage: { [ov.key]: '1' } });
    const { map, ctl } = buildControl(ctx);
    assert.strictEqual(map.hasLayer(ov.layer(ctl)), true);
    assert.strictEqual(ctx.replaced.length, 0);
  });

  test(`${ov.name}: ?${ov.param}=1 wins over stored "0" and is persisted`, () => {
    const ctx = sb({ hash: `#/live?${ov.param}=1`, storage: { [ov.key]: '0' } });
    const { map, ctl } = buildControl(ctx);
    assert.strictEqual(map.hasLayer(ov.layer(ctl)), true);
    assert.strictEqual(ctx.localStorage._raw[ov.key], '1');
  });

  test(`${ov.name}: ?${ov.param}=0 wins over stored "1" and is persisted`, () => {
    const ctx = sb({ hash: `#/map?${ov.param}=0`, storage: { [ov.key]: '1' } });
    const { map, ctl } = buildControl(ctx);
    assert.strictEqual(map.hasLayer(ov.layer(ctl)), false);
    assert.strictEqual(ctx.localStorage._raw[ov.key], '0');
  });

  test(`${ov.name}: a garbage hash value falls back to storage and is not persisted`, () => {
    const ctx = sb({ hash: `#/map?${ov.param}=yes`, storage: { [ov.key]: '1' } });
    const { map, ctl } = buildControl(ctx);
    assert.strictEqual(map.hasLayer(ov.layer(ctl)), true);
    assert.strictEqual(ctx.localStorage._raw[ov.key], '1');
  });

  test(`${ov.name}: toggle on → stored "1" + ?${ov.param}=1, other params kept`, () => {
    const ctx = sb({ hash: '#/map?lat=56&lon=10&zoom=13' });
    const { map, ctl } = buildControl(ctx);
    map.addLayer(ov.layer(ctl));
    assert.strictEqual(ctx.localStorage._raw[ov.key], '1');
    assert.strictEqual(ctx.location.hash, `#/map?lat=56&lon=10&zoom=13&${ov.param}=1`);
  });

  test(`${ov.name}: toggle off → stored "0" + parameter removed`, () => {
    const ctx = sb({ hash: `#/live?${ov.param}=1&heard=1h` });
    const { map, ctl } = buildControl(ctx);
    map.removeLayer(ov.layer(ctl));
    assert.strictEqual(ctx.localStorage._raw[ov.key], '0');
    assert.strictEqual(ctx.location.hash, '#/live?heard=1h');
  });

  // #682 review F4: with an empty location.hash the parameter goes into a
  // new hash, never into the query string (`/?debug=1?heritage=1`).
  test(`${ov.name}: toggle on with an empty hash writes #?${ov.param}=1, query string untouched`, () => {
    const ctx = sb({ hash: '', search: '?debug=1' });
    const { map, ctl } = buildControl(ctx);
    map.addLayer(ov.layer(ctl));
    assert.strictEqual(ctx.replaced[ctx.replaced.length - 1], `/?debug=1#?${ov.param}=1`);
    assert.strictEqual(ctx.location.search, '?debug=1');
    assert.strictEqual(ctx.location.hash, `#?${ov.param}=1`);
  });

  test(`${ov.name}: toggle off with an empty hash leaves the URL without a hash`, () => {
    const ctx = sb({ hash: '', search: '?debug=1', storage: { [ov.key]: '1' } });
    const { map, ctl } = buildControl(ctx);
    map.removeLayer(ov.layer(ctl));
    assert.strictEqual(ctx.replaced[ctx.replaced.length - 1], '/?debug=1');
  });

  test(`${ov.name}: initial restore happens before the layer control exists`, () => {
    const ctx = sb({ storage: { [ov.key]: '1' } });
    const map = makeMap();
    let controlsAtAdd = null;
    const origAdd = map.addLayer;
    map.addLayer = (l) => { if (l && l.kind === 'wms') controlsAtAdd = map._controls.length; return origAdd(l); };
    ctx.window.MC_createLayerControl(map, {}, 'topleft');
    assert.strictEqual(controlsAtAdd, 0);
  });
}

// ── Two overlays together ──────────────────────────────────────────────────

test('independent: ?heritage=1 turns on monuments only and stores nothing for terrain', () => {
  const ctx = makeSandbox({ dhm: DHM, heritage: HERITAGE, hash: '#/map?heritage=1' });
  const { map, ctl } = buildControl(ctx);
  assert.strictEqual(map.hasLayer(heritageLayer(ctl)), true);
  assert.strictEqual(map.hasLayer(terrainLayer(ctl)), false);
  assert.strictEqual(ctx.localStorage._raw['mc-heritage'], '1');
  assert.ok(!('mc-terrain-dhm' in ctx.localStorage._raw), 'terrain preference written');
});

test('independent: toggling one keeps the other\'s hash parameter and storage', () => {
  const ctx = makeSandbox({ dhm: DHM, heritage: HERITAGE, hash: '#/live?terrain=1&heritage=1' });
  const { map, ctl } = buildControl(ctx);
  assert.strictEqual(map.hasLayer(terrainLayer(ctl)), true);
  assert.strictEqual(map.hasLayer(heritageLayer(ctl)), true);
  map.removeLayer(heritageLayer(ctl));
  assert.strictEqual(ctx.location.hash, '#/live?terrain=1');
  assert.strictEqual(ctx.localStorage._raw['mc-terrain-dhm'], '1');
  assert.strictEqual(ctx.localStorage._raw['mc-heritage'], '0');
  assert.strictEqual(map.hasLayer(terrainLayer(ctl)), true);
});

// #728 review round 3: the control stays when all overlays are off, so
// the base map's credit (not on this bare map) is always shown.
test('Live: exactly one credit control, with any or no overlay on', () => {
  const ctx = makeSandbox({ dhm: DHM, heritage: HERITAGE });
  const { map, ctl } = buildControl(ctx, false);
  assert.strictEqual(liveAttributions(map).length, 1, 'none on');
  map.addLayer(terrainLayer(ctl));
  map.addLayer(heritageLayer(ctl));
  assert.strictEqual(liveAttributions(map).length, 1, 'both on');
  map.removeLayer(terrainLayer(ctl));
  assert.strictEqual(liveAttributions(map).length, 1, 'monuments still on');
  map.removeLayer(heritageLayer(ctl));
  assert.strictEqual(liveAttributions(map).length, 1, 'all off');
  assert.strictEqual(map.attributionControl, liveAttributions(map)[0]);
});

// Leaflet's control counts the credits already on the map when it is added
// but never drops those again, so a control kept across toggles would go on
// showing the agency credit after its layer is gone.
test('Live: the credit text follows the overlays that are on (no stale credit)', () => {
  const ctx = makeSandbox({ dhm: DHM, heritage: HERITAGE });
  const { map, ctl } = buildControl(ctx, false);
  map.addLayer(heritageLayer(ctl));
  assert.deepStrictEqual(liveCredits(map), [HERITAGE.attribution], 'monuments on');
  map.addLayer(terrainLayer(ctl));
  assert.deepStrictEqual(liveCredits(map), [HERITAGE.attribution, DHM.attribution].sort(), 'both on');
  map.removeLayer(heritageLayer(ctl));
  assert.deepStrictEqual(liveCredits(map), [DHM.attribution], 'monuments off, terrain on');
  map.removeLayer(terrainLayer(ctl));
  assert.deepStrictEqual(liveCredits(map), [], 'all off');
  map.addLayer(terrainLayer(ctl));
  map.addLayer(heritageLayer(ctl));
  map.removeLayer(terrainLayer(ctl));
  assert.deepStrictEqual(liveCredits(map), [HERITAGE.attribution], 'terrain off, monuments on');
});

// Leaflet's attribution control claims map.attributionControl on add and
// never releases it; a control rebuilt per toggle once found that dead
// control on the second "on" and added no credit at all.
for (const ov of OVERLAYS) {
  test(`Live: ${ov.name} on → off → on shows the credit again`, () => {
    const ctx = makeSandbox(ov.cfg);
    const { map, ctl } = buildControl(ctx, false);
    map.addLayer(ov.layer(ctl));
    map.removeLayer(ov.layer(ctl));
    assert.deepStrictEqual(liveCredits(map), [], 'credit left after off');
    map.addLayer(ov.layer(ctl));
    assert.deepStrictEqual(liveCredits(map), [ov.layer(ctl).options.attribution], 'no credit on the second toggle');
    assert.strictEqual(liveAttributions(map).length, 1);
    assert.strictEqual(map.attributionControl, liveAttributions(map)[0]);
  });
}

test('Map page (own attribution control) never gets a second one', () => {
  const ctx = makeSandbox({ dhm: DHM, heritage: HERITAGE, storage: { 'mc-heritage': '1', 'mc-terrain-dhm': '1' } });
  const { map, ctl } = buildControl(ctx, true);
  map.removeLayer(heritageLayer(ctl));
  map.addLayer(heritageLayer(ctl));
  assert.strictEqual(map._attributionControls.length, 0);
  assert.strictEqual(map.attributionControl.builtIn, true);
});

test('control rebuild keeps both layers and their on state', () => {
  const ctx = makeSandbox({ dhm: DHM, heritage: HERITAGE, storage: { 'mc-heritage': '1' } });
  const { map, ctl } = buildControl(ctx);
  const h = heritageLayer(ctl), t = terrainLayer(ctl);
  ctx.window.MC_initTileRegistry(true);
  const ctl2 = map._controls[map._controls.length - 1];
  assert.notStrictEqual(ctl2, ctl);
  assert.strictEqual(heritageLayer(ctl2), h);
  assert.strictEqual(terrainLayer(ctl2), t);
  assert.strictEqual(map.hasLayer(h), true);
  assert.strictEqual(map.hasLayer(t), false);
});

test('a rebuild does not wire the toggle twice (one save per toggle)', () => {
  const ctx = makeSandbox({ heritage: HERITAGE });
  const { map } = buildControl(ctx);
  ctx.window.MC_initTileRegistry(true);
  ctx.window.MC_initTileRegistry(true);
  const n = ctx.replaced.length;
  map.addLayer(map._mcHeritageLayer);
  assert.strictEqual(ctx.replaced.length - n, 1);
});

// ── roles.js wiring ─────────────────────────────────────────────────────────

test('roles.js stores cfg.heritage as MC_HERITAGE_CFG before the registry re-init', () => {
  const roles = fs.readFileSync(path.join(__dirname, 'public', 'roles.js'), 'utf8');
  const a = roles.indexOf('window.MC_HERITAGE_CFG = cfg.heritage || null;');
  const b = roles.indexOf('window.MC_initTileRegistry(true)');
  assert.ok(a > 0, 'MC_HERITAGE_CFG assignment present');
  assert.ok(b > a, 'assigned before MC_initTileRegistry(true) rebuilds the controls');
});

console.log(`\n${passed} passed, ${failed} failed`);
process.exit(failed ? 1 : 0);
