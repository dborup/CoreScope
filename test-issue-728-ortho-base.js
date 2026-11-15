/* test-issue-728-ortho-base.js — #728 M3.
 *
 * The "Aerial (Denmark)" base entry: the Danish spring orthophoto from the
 * server's same-origin proxy (/api/map/dhm/ortho/{z}/{x}/{y}), offered
 * only while /api/config/client carries dhm.ortho.
 *
 *   A1  Offered only with a valid dhm.ortho block: not without dhm, without
 *       ortho, or with a cross-origin, protocol-relative or template-less
 *       url, bad zooms, bounds or credit. One entry, on both themes.
 *   A2  Esri off: the entry is the ortho tile layer alone, at the
 *       server's url with its zooms and bounds; no Esri URL anywhere.
 *       Esri on (map.tiles.providers.esri.enabled): a layer group of Esri
 *       World Imagery under the ortho layer, Esri's URL and credit from
 *       the M1 style.
 *   A3  Credit: built by the shared walker from the block's own parts
 *       (dataset and licence linked); no credit string copied into JS.
 *   A4  Never inverted: selecting it clears the tile pane filter and locks
 *       it, on the dark theme too.
 *   A5  Foreign mask (dhm.ortho.foreignMask): a tile the mask marks is
 *       not requested, at the mask zoom, below it (only when every tile
 *       under it is marked) and above it; a malformed mask is ignored.
 *   A6  Per viewer: picking it stores mc-ortho-base and sets ?ortho=1;
 *       picking anything else clears both; a reload restores it; the hash
 *       wins over storage; config that arrives later still restores it.
 *   A7  Not on the map unless chosen (no ortho request otherwise).
 *   A8  Live (no attribution control of its own) shows the ortho credit,
 *       and Esri's with it when Esri is on, and drops them on Auto.
 *
 * Runs via: node test-issue-728-ortho-base.js  (pure vm sandbox)
 */
'use strict';
const assert = require('assert');
const { makeSandbox, buildControl, liveCredits } = require('./tests/map-overlay-sandbox');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const ORTHO_ID = 'dhm-ortho';
const ORTHO_URL = '/api/map/dhm/ortho/{z}/{x}/{y}';
const ESRI_URL = 'https://server.arcgisonline.com/ArcGIS/rest/services/World_Imagery/MapServer/tile/{z}/{y}/{x}';
const ESRI_CREDIT = 'Powered by <a href="https://www.esri.com">Esri</a> | Tiles © Esri — Source: Esri, Vantor, Earthstar Geographics, and the GIS User Community';

// What cmd/server/dhm_proxy.go sends while map.dhm.ortho is on.
function orthoBlock(extra) {
  return Object.assign({
    url: ORTHO_URL, minZoom: 7, maxZoom: 20,
    bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
    attribution: 'Indeholder data fra GeoDanmark, Ortofoto forår, CC BY 4.0',
    datasetName: 'Ortofoto forår', datasetUrl: 'https://dataforsyningen.dk/data/981',
    licenseName: 'CC BY 4.0', licenseUrl: 'https://creativecommons.org/licenses/by/4.0/',
  }, extra || {});
}
function dhmBlock(ortho) {
  const b = {
    enabled: true, url: '/api/map/dhm/wms', layer: 'dhm_terraen_skyggekort',
    minZoom: 7, maxZoom: 19, bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
    attribution: 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0',
    datasetName: 'Danmarks Højdemodel', datasetUrl: 'https://dataforsyningen.dk/data/930',
    licenseName: 'CC BY 4.0', licenseUrl: 'https://creativecommons.org/licenses/by/4.0/',
  };
  if (ortho !== undefined) b.ortho = ortho;
  return b;
}
const ESRI_ON = { tiles: { providers: { esri: { enabled: true } } } };

// A mask in the server's encoding: bits row-major, LSB first, base64.
function mask(z, x0, y0, cols, rows, marked) {
  const bytes = Buffer.alloc(Math.ceil(cols * rows / 8));
  for (const [c, r] of marked) { const i = r * cols + c; bytes[i >> 3] |= 1 << (i & 7); }
  return { z, x0, y0, cols, rows, bits: bytes.toString('base64') };
}

function setup(opts) {
  opts = opts || {};
  const ctx = makeSandbox(Object.assign({ dhm: dhmBlock(orthoBlock()) }, opts));
  const auto = ctx.L.tileLayer('https://tile.openstreetmap.org/{z}/{x}/{y}.png', { attribution: '© OpenStreetMap contributors' });
  const { map, ctl } = buildControl(ctx, opts.mapPage !== false && !opts.live, opts.live ? auto : null);
  return { ctx, map, ctl, auto };
}
const lastCtl = (map) => map._controls[map._controls.length - 1];
const entry = (map) => lastCtl(map).base[ORTHO_ID];
// The ortho tile layer itself: the entry, or its top member with Esri on.
function orthoTiles(layer) {
  if (layer.kind !== 'layerGroup') return layer;
  return Array.from(layer.members).find(l => l._url === ORTHO_URL);
}
// What L.Control.Layers does when the viewer picks a base layer.
function pick(map, name) {
  const base = lastCtl(map).base;
  for (const k of Object.keys(base)) if (k !== name && base[k] && map.hasLayer(base[k])) map.removeLayer(base[k]);
  if (base[name] && base[name].kind) map.addLayer(base[name]);
  map.fire('baselayerchange', { name, layer: base[name] });
}

console.log('── #728 A1: offered only with dhm.ortho ──');

test('A1 offered with a valid dhm.ortho block, as one entry', () => {
  const { map } = setup();
  const ids = Object.keys(lastCtl(map).base);
  assert.strictEqual(ids.filter(id => id === ORTHO_ID).length, 1, 'ids ' + ids);
  assert.ok(entry(map), 'no ortho entry');
});

test('A1 offered on the dark theme too', () => {
  const { map } = setup({ theme: 'dark' });
  assert.ok(entry(map));
});

const NOT_OFFERED = {
  'no dhm block': { dhm: null },
  'dhm without ortho': { dhm: dhmBlock() },
  'dhm disabled': { dhm: Object.assign(dhmBlock(orthoBlock()), { enabled: false }) },
  'ortho null': { dhm: dhmBlock(null) },
  'absolute https url': { dhm: dhmBlock(orthoBlock({ url: 'https://api.dataforsyningen.dk/orto_foraar_webm_DAF?token=x&{z}{x}{y}' })) },
  'protocol-relative url': { dhm: dhmBlock(orthoBlock({ url: '//evil.example/{z}/{x}/{y}' })) },
  'backslash url': { dhm: dhmBlock(orthoBlock({ url: '/\\evil.example/{z}/{x}/{y}' })) },
  'url without {y}': { dhm: dhmBlock(orthoBlock({ url: '/api/map/dhm/ortho/{z}/{x}' })) },
  'url not a string': { dhm: dhmBlock(orthoBlock({ url: 7 })) },
  'minZoom above maxZoom': { dhm: dhmBlock(orthoBlock({ minZoom: 21, maxZoom: 20 })) },
  'fractional zoom': { dhm: dhmBlock(orthoBlock({ minZoom: 7.5 })) },
  'no bounds': { dhm: dhmBlock(orthoBlock({ bounds: null })) },
  'empty credit': { dhm: dhmBlock(orthoBlock({ attribution: '' })) },
};
for (const [label, opts] of Object.entries(NOT_OFFERED)) {
  test('A1 not offered: ' + label, () => {
    const { map } = setup(opts);
    assert.ok(!Object.keys(lastCtl(map).base).includes(ORTHO_ID), 'offered with ' + label);
  });
}

test('A1 the shared same-origin check refuses "/\\host" for the hillshade too', () => {
  const ctx = makeSandbox({ dhm: Object.assign(dhmBlock(), { url: '/\\evil.example/wms' }) });
  assert.strictEqual(ctx.window.MC_getTerrainConfig(), null);
  assert.ok(makeSandbox({ dhm: dhmBlock() }).window.MC_getTerrainConfig());
});

test('A1 MC_getOrthoConfig returns the block only when it is valid', () => {
  assert.strictEqual(setup().ctx.window.MC_getOrthoConfig().url, ORTHO_URL);
  assert.strictEqual(setup({ dhm: dhmBlock() }).ctx.window.MC_getOrthoConfig(), null);
});

console.log('\n── #728 A2: the layer, with and without Esri ──');

test('A2 Esri off: the ortho tile layer alone, same-origin, the block\'s zooms and bounds', () => {
  const { map } = setup();
  const layer = entry(map);
  assert.strictEqual(layer.kind, 'tile');
  assert.strictEqual(layer._url, ORTHO_URL);
  assert.ok(layer._url.charAt(0) === '/' && layer._url.charAt(1) !== '/', 'same-origin path');
  assert.strictEqual(layer.options.minZoom, 7);
  assert.strictEqual(layer.options.maxZoom, 20);
  assert.deepStrictEqual(layer.options.bounds._bounds, orthoBlock().bounds);
});

test('A2 Esri off: no Esri URL or credit in the entry', () => {
  const { map } = setup();
  const layer = entry(map);
  assert.ok(!/arcgisonline|Esri/.test(layer._url + ' ' + layer.options.attribution));
});

test('A2 the url and zooms follow the block, not a copy in JS', () => {
  const { map } = setup({ dhm: dhmBlock(orthoBlock({ url: '/x/ortho/{z}/{x}/{y}', minZoom: 9, maxZoom: 18 })) });
  const layer = entry(map);
  assert.strictEqual(layer._url, '/x/ortho/{z}/{x}/{y}');
  assert.strictEqual(layer.options.minZoom, 9);
  assert.strictEqual(layer.options.maxZoom, 18);
});

test('A2 Esri on: Esri World Imagery under the ortho layer, in one group', () => {
  const { map } = setup({ mapCfg: ESRI_ON });
  const group = entry(map);
  assert.strictEqual(group.kind, 'layerGroup');
  const members = Array.from(group.members);
  assert.strictEqual(members.length, 2);
  const esri = members.find(l => l._url === ESRI_URL);
  const ortho = orthoTiles(group);
  assert.ok(esri && ortho, 'members ' + members.map(l => l._url));
  assert.ok(!esri.options.pane, 'Esri draws in the tile pane');
  assert.strictEqual(ortho.options.pane, 'mcOrthoPane');
  assert.strictEqual(esri.options.attribution, ESRI_CREDIT);
  assert.strictEqual(esri.options.maxZoom, 19);
  assert.ok(!esri.options.bounds, 'Esri is the fallback everywhere, so it has no bounds');
  assert.deepStrictEqual(ortho.options.bounds._bounds, orthoBlock().bounds);
});

test('A2 Esri on: the Esri member uses the M1 style\'s URL and credit', () => {
  const { ctx, map } = setup({ mapCfg: ESRI_ON });
  const style = ctx.window.MC_TILE_PROVIDERS['esri-imagery'];
  const esri = Array.from(entry(map).members).find(l => l !== orthoTiles(entry(map)));
  assert.strictEqual(esri._url, style.url());
  assert.strictEqual(esri.options.attribution, style.attribution);
});

test('A2 the ortho layer has its own pane between the base tiles and the hillshade', () => {
  for (const mapCfg of [undefined, ESRI_ON]) {
    const { map } = setup({ mapCfg });
    assert.strictEqual(orthoTiles(entry(map)).options.pane, 'mcOrthoPane');
    const pane = map.getPane('mcOrthoPane');
    assert.ok(pane, 'pane not created');
    const z = Number(pane.style.zIndex);
    assert.ok(z > map.getPane('tilePane').zIndex && z < 250, 'pane z ' + z);
    assert.strictEqual(pane.style.pointerEvents, 'none');
    assert.ok(pane.classes.includes('mc-ortho-pane'));
  }
});

console.log('\n── #728 A3: credit ──');

test('A3 the credit links the dataset and the licence, from the block', () => {
  const { map } = setup();
  assert.strictEqual(entry(map).options.attribution,
    'Indeholder data fra GeoDanmark, <a href="https://dataforsyningen.dk/data/981" target="_blank" rel="noopener noreferrer">Ortofoto forår</a>, ' +
    '<a href="https://creativecommons.org/licenses/by/4.0/" target="_blank" rel="noopener noreferrer license">CC BY 4.0</a>');
});

test('A3 the credit is the shared walker\'s, built from whatever the block says', () => {
  const block = orthoBlock({ attribution: 'Data: Owner X, Set Y, Lic Z', datasetName: 'Set Y', datasetUrl: 'https://example.org/y', licenseName: 'Lic Z', licenseUrl: 'https://example.org/z' });
  const { ctx, map } = setup({ dhm: dhmBlock(block) });
  const want = ctx.window.MC_buildCredit(block.attribution, ctx.window.MC_creditLinks(block));
  assert.strictEqual(entry(map).options.attribution, want);
  assert.ok(want.includes('<a href="https://example.org/y"') && want.includes('<a href="https://example.org/z"'), want);
});

test('A3 a block without the link parts keeps the plain credit; markup in it is text', () => {
  const plain = setup({ dhm: dhmBlock(orthoBlock({ datasetUrl: undefined, licenseUrl: 'javascript:alert(1)' })) });
  assert.strictEqual(entry(plain.map).options.attribution, 'Indeholder data fra GeoDanmark, Ortofoto forår, CC BY 4.0');
  const hostile = setup({ dhm: dhmBlock(orthoBlock({ attribution: '<img src=x onerror=alert(1)> GeoDanmark', datasetName: 'none', licenseName: 'none' })) });
  assert.strictEqual(entry(hostile.map).options.attribution, '<img src=x onerror=alert(1)> GeoDanmark',
    'with nothing linked the plain text goes to Leaflet as before (#733 contract)');
});

console.log('\n── #728 A4: never inverted ──');

for (const theme of ['light', 'dark']) {
  test('A4 ' + theme + ' theme: picking it clears the tile pane filter and locks the pane', () => {
    const { map } = setup({ theme });
    const pane = map.getPane('tilePane');
    pane.style.filter = 'invert(1) hue-rotate(180deg) brightness(0.9) contrast(1.05)';
    pick(map, ORTHO_ID);
    assert.strictEqual(pane.style.filter, '');
    assert.strictEqual(pane.getAttribute('data-explicit-layer'), 'true');
  });
}

test('A4 with Esri on, the group clears the filter too', () => {
  const { map } = setup({ theme: 'dark', mapCfg: ESRI_ON });
  const pane = map.getPane('tilePane');
  pane.style.filter = 'invert(1)';
  pick(map, ORTHO_ID);
  assert.strictEqual(pane.style.filter, '');
});

console.log('\n── #728 A5: the foreign mask ──');

// 4×2 window at z12 from (2190, 1270); marked: (2192..2193, 1270) and (2192, 1271).
const MASK = mask(12, 2190, 1270, 4, 2, [[2, 0], [3, 0], [2, 1]]);

function maskedLayer(m) {
  const { map } = setup({ dhm: dhmBlock(orthoBlock({ foreignMask: m })) });
  const layer = entry(map);
  return (z, x, y) => layer._isValidTile({ z, x, y });
}

test('A5 at the mask zoom: marked tiles are not requested, others are', () => {
  const valid = maskedLayer(MASK);
  assert.strictEqual(valid(12, 2192, 1270), false);
  assert.strictEqual(valid(12, 2193, 1270), false);
  assert.strictEqual(valid(12, 2192, 1271), false);
  assert.strictEqual(valid(12, 2190, 1270), true);
  assert.strictEqual(valid(12, 2193, 1271), true);
});

test('A5 above it: a tile inside a marked z12 tile is not requested', () => {
  const valid = maskedLayer(MASK);
  assert.strictEqual(valid(13, 2192 * 2 + 1, 1270 * 2 + 1), false);
  assert.strictEqual(valid(20, 2193 * 256 + 255, 1270 * 256), false);
  assert.strictEqual(valid(20, 2193 * 256, 1271 * 256 + 3), true);
});

test('A5 below it: skipped only when every z12 tile under it is marked or outside the window', () => {
  const valid = maskedLayer(MASK);
  // z11 (1096, 635) holds z12 x 2192..2193, y 1270..1271: (2193, 1271) is unmarked.
  assert.strictEqual(valid(11, 1096, 635), true);
  const all = mask(12, 2190, 1270, 4, 2, [[2, 0], [3, 0], [2, 1], [3, 1]]);
  assert.strictEqual(maskedLayer(all)(11, 1096, 635), false);
  // z10 (548, 317) holds z12 x 2192..2195, y 1268..1271: only marked tiles inside the window.
  assert.strictEqual(maskedLayer(all)(10, 548, 317), false);
  // A tile with no z12 tile in the window is left to `bounds`.
  assert.strictEqual(maskedLayer(all)(11, 1000, 600), true);
});

test('A5 tiles outside the window are left to `bounds`', () => {
  const valid = maskedLayer(MASK);
  assert.strictEqual(valid(12, 2189, 1270), true);
  assert.strictEqual(valid(12, 2194, 1270), true);
  assert.strictEqual(valid(12, 2192, 1272), true);
});

test('A5 the base _isValidTile (Leaflet\'s bounds test) still applies', () => {
  const ctx = makeSandbox({ dhm: dhmBlock(orthoBlock({ foreignMask: MASK })) });
  const seen = [];
  const tileLayer = ctx.L.tileLayer;
  // Stand-in for GridLayer._isValidTile: x = 1 is out of bounds.
  ctx.L.tileLayer = Object.assign((u, o) => Object.assign(tileLayer(u, o), { _isValidTile(c) { seen.push(c); return c.x !== 1; } }), tileLayer);
  const { map } = buildControl(ctx, true);
  const layer = entry(map);
  assert.strictEqual(layer._isValidTile({ z: 12, x: 1, y: 1270 }), false, 'out of bounds');
  assert.strictEqual(layer._isValidTile({ z: 12, x: 2190, y: 1270 }), true);
  assert.strictEqual(layer._isValidTile({ z: 12, x: 2192, y: 1270 }), false, 'marked');
  assert.strictEqual(seen.length, 3, 'the bounds test ran for every tile');
});

const BAD_MASKS = {
  'not an object': 'x',
  'bits not base64': Object.assign({}, MASK, { bits: '***' }),
  'bits too short': Object.assign({}, MASK, { bits: '' }),
  'bits too long': Object.assign({}, MASK, { bits: Buffer.alloc(5).toString('base64') }),
  // The declared window does not match the bits: read anyway, the first
  // byte would mark tiles the mask never meant.
  'rows larger than the bits': Object.assign({}, MASK, { rows: 20 }),
  'negative cols': Object.assign({}, MASK, { cols: -4 }),
  'fractional x0': Object.assign({}, MASK, { x0: 2190.5 }),
  'zoom out of range': Object.assign({}, MASK, { z: 40 }),
};
for (const [label, m] of Object.entries(BAD_MASKS)) {
  test('A5 a malformed mask is ignored (' + label + '): every tile is left to `bounds`', () => {
    const valid = maskedLayer(m);
    assert.strictEqual(valid(12, 2192, 1270), true);
    assert.strictEqual(valid(7, 68, 39), true);
  });
}

test('A5 no mask: every tile is left to `bounds`', () => {
  const valid = maskedLayer(undefined);
  assert.strictEqual(valid(12, 2192, 1270), true);
});

// Perf: Leaflet runs _isValidTile for every tile in range on each move.
// Worst case is a low zoom tile over marked tiles (1024 z12 tiles under
// one z7 tile). The window below is the server mask's size (88×69).
test('A5 perf: every z7–z20 tile of a full 88×69 marked window checks in well under the budget', () => {
  const marked = [];
  for (let r = 0; r < 69; r++) for (let c = 0; c < 88; c++) marked.push([c, r]);
  const valid = maskedLayer(mask(12, 2150, 1260, 88, 69, marked));
  let checks = 0;
  const t0 = process.hrtime.bigint();
  for (let z = 7; z <= 11; z++) {
    const s = 2 ** (12 - z);
    for (let y = Math.floor(1260 / s); y <= Math.floor((1260 + 68) / s); y++)
      for (let x = Math.floor(2150 / s); x <= Math.floor((2150 + 87) / s); x++) { valid(z, x, y); checks++; }
  }
  // A 1100×720 map at z13–z20 shows about 24 tiles; check 1000 per zoom.
  for (let z = 13; z <= 20; z++) for (let i = 0; i < 1000; i++) { valid(z, (2150 << (z - 12)) + i, 1260 << (z - 12)); checks++; }
  const ms = Number(process.hrtime.bigint() - t0) / 1e6;
  console.log('       ' + checks + ' tile checks in ' + ms.toFixed(1) + ' ms');
  assert.ok(ms < 100, checks + ' checks took ' + ms.toFixed(1) + ' ms');
});

console.log('\n── #728 A6: per viewer, and in the link ──');

test('A6 picking it stores the choice and sets ?ortho=1', () => {
  const { ctx, map } = setup();
  pick(map, ORTHO_ID);
  assert.strictEqual(ctx.localStorage.getItem('mc-ortho-base'), '1');
  assert.ok(/[?&]ortho=1(&|$)/.test(ctx.location.hash), ctx.location.hash);
  assert.ok(map.hasLayer(entry(map)));
});

test('A6 picking Auto or another style afterwards clears both', () => {
  for (const other of ['__auto__', 'osm-standard']) {
    const { ctx, map } = setup();
    pick(map, ORTHO_ID);
    pick(map, other);
    assert.strictEqual(ctx.localStorage.getItem('mc-ortho-base'), '0', other);
    assert.ok(!/ortho=/.test(ctx.location.hash), other + ': ' + ctx.location.hash);
    assert.ok(!map.hasLayer(entry(map)), other + ': the ortho layer is still on');
  }
});

test('A6 a stored choice is restored at load (the entry is on, Auto off)', () => {
  const { map } = setup({ storage: { 'mc-ortho-base': '1' } });
  assert.ok(map.hasLayer(entry(map)));
  assert.strictEqual(map.getPane('tilePane').getAttribute('data-explicit-layer'), 'true');
});

test('A6 the hash wins over storage, both ways, and is saved', () => {
  const on = setup({ hash: '#/map?ortho=1', storage: { 'mc-ortho-base': '0' } });
  assert.ok(on.map.hasLayer(entry(on.map)));
  assert.strictEqual(on.ctx.localStorage.getItem('mc-ortho-base'), '1');
  const off = setup({ hash: '#/map?ortho=0', storage: { 'mc-ortho-base': '1' } });
  assert.ok(!off.map.hasLayer(entry(off.map)));
  assert.strictEqual(off.ctx.localStorage.getItem('mc-ortho-base'), '0');
});

test('A6 a stored choice while ortho is not offered keeps Auto and the stored value', () => {
  const { ctx, map } = setup({ dhm: dhmBlock(), storage: { 'mc-ortho-base': '1' } });
  assert.ok(!lastCtl(map).base[ORTHO_ID]);
  assert.strictEqual(ctx.localStorage.getItem('mc-ortho-base'), '1');
});

test('A6 config that arrives after the map is built still restores the choice', () => {
  const { ctx, map } = setup({ dhm: null, storage: { 'mc-ortho-base': '1' } });
  assert.ok(!lastCtl(map).base[ORTHO_ID]);
  ctx.window.MC_DHM_CFG = dhmBlock(orthoBlock());
  ctx.window.MC_initTileRegistry(true);
  assert.ok(map.hasLayer(entry(map)), 'restored after the async config');
});

test('A6 a config rebuild keeps the viewer\'s pick, and drops it if ortho goes away', () => {
  const { ctx, map } = setup();
  pick(map, ORTHO_ID);
  ctx.window.MC_initTileRegistry(true);
  assert.ok(map.hasLayer(entry(map)), 'kept across a rebuild');
  assert.strictEqual(Array.from(map.layers).filter(l => l._url === ORTHO_URL).length, 1, 'one ortho layer on the map');
  ctx.window.MC_DHM_CFG = dhmBlock();
  ctx.window.MC_initTileRegistry(true);
  assert.ok(!Array.from(map.layers).some(l => l._url === ORTHO_URL), 'ortho layer left on the map');
  assert.ok(map.hasLayer(lastCtl(map).base.__auto__), 'Auto is back');
});

console.log('\n── #728 A7: off unless chosen ──');

test('A7 by default the entry is offered but not on the map', () => {
  for (const mapCfg of [undefined, ESRI_ON]) {
    const { map } = setup({ mapCfg });
    assert.ok(entry(map));
    assert.ok(!Array.from(map.layers).some(l => l._url === ORTHO_URL || l._url === ESRI_URL), 'ortho or Esri on the map');
  }
});

console.log('\n── #728 A8: Live credit ──');

const ORTHO_CREDIT_TEXT = 'Indeholder data fra GeoDanmark, Ortofoto forår, CC BY 4.0';
function liveCreditTexts(map) {
  return liveCredits(map).map(c => c.replace(/<[^>]+>/g, '')).sort();
}

test('A8 Live, ortho picked: its credit replaces the base credit; Auto brings the base credit back', () => {
  const { map, auto } = setup({ live: true, hash: '#/live' });
  assert.deepStrictEqual(liveCreditTexts(map), ['© OpenStreetMap contributors']);
  pick(map, ORTHO_ID);
  map.removeLayer(auto); // Live's Auto group, which holds this layer, leaves the map with it
  assert.deepStrictEqual(liveCreditTexts(map), [ORTHO_CREDIT_TEXT]);
  pick(map, '__auto__');
  map.addLayer(auto);
  assert.deepStrictEqual(liveCreditTexts(map), ['© OpenStreetMap contributors']);
});

test('A8 Live, Esri on: both credits while picked, none after', () => {
  const { map, auto } = setup({ live: true, hash: '#/live', mapCfg: ESRI_ON });
  pick(map, ORTHO_ID);
  map.removeLayer(auto);
  assert.deepStrictEqual(liveCredits(map).sort(), [ESRI_CREDIT, orthoTiles(entry(map)).options.attribution].sort());
  pick(map, '__auto__');
  assert.deepStrictEqual(liveCredits(map), []);
});

console.log(`\n#728 ortho base: ${passed} passed, ${failed} failed`);
if (failed) process.exit(1);
