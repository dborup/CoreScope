/**
 * E2E (#728 M1): Esri World Imagery base map and the Esri opt-in gate.
 *
 * The CI server has no map.tiles config, so /api/config/client is
 * intercepted and given the map.tiles block an operator would set. Every
 * third-party tile is stubbed (nothing reaches Esri or OSM); the DHM
 * terrain overlay is switched on the same way #680's suite does it, with a
 * stubbed /api/map/dhm/wms.
 *
 * Asserts, on Map (and Live where it has the control):
 *   1. Esri off (no esri block): no Esri entry in the layer control, no
 *      Esri option in the customizer, zero arcgisonline requests.
 *   2. Esri off, but darkDefault / lightDefault and the viewer's stored
 *      choice name Esri styles: the map falls back to OSM tiles, still zero
 *      arcgisonline requests, a console warning names the switch, no page
 *      errors.
 *   3. Esri on: World Imagery, its dark twin and Dark Gray are offered;
 *      zero World_Imagery requests until it is selected (incl. pan/zoom);
 *      selecting it loads {z}/{y}/{x} tiles at z<=19, shows the exact
 *      credit led by "Powered by Esri" with one Esri link to esri.com,
 *      and the tile pane carries no filter; back to Auto, no more
 *      World_Imagery requests.
 *   4. Dark theme: the imagery is never inverted (osm-dark is, as a check
 *      that the probe sees filters at all); Dark Gray shows "Powered by
 *      Esri" too.
 *   5. Per viewer: the customizer's light provider persists the choice;
 *      after a reload Map and Live load World_Imagery tiles by default.
 *   6. With the DHM hillshade on: both layers load, terrain above the
 *      imagery, both credits shown.
 *
 * Usage:
 *   BASE_URL=http://localhost:13728 node test-issue-728-esri-imagery-e2e.js
 * Optional: SCREENSHOT_DIR=<dir> writes small screenshots (stub tiles only).
 */
'use strict';
const { chromium } = require('playwright');
const zlib = require('zlib');
const path = require('path');

const BASE = process.env.BASE_URL || 'http://localhost:13800';
const SHOT_DIR = process.env.SCREENSHOT_DIR || '';
// The attribution control's textContent; "Esri" in "Powered by Esri" is a
// link to https://www.esri.com (checked through esriLinks).
const POWERED_BY = 'Powered by Esri | ';
const CREDIT = POWERED_BY + 'Tiles © Esri — Source: Esri, Vantor, Earthstar Geographics, and the GIS User Community';
const ESRI_HOME = 'https://www.esri.com';
const IMAGERY_RE = /^https:\/\/server\.arcgisonline\.com\/ArcGIS\/rest\/services\/World_Imagery\/MapServer\/tile\/(\d+)\/(\d+)\/(\d+)$/;
const DHM_PATH = '/api/map/dhm/wms';
const DHM_BLOCK = {
  enabled: true, url: DHM_PATH, layer: 'dhm_terraen_skyggekort',
  minZoom: 7, maxZoom: 19, bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
  attribution: 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0',
};
const ESRI_IDS = ['esri-darkgray-labels', 'esri-imagery', 'esri-imagery-dark'];
const DK_VIEW = { lat: 56.15, lon: 10.2, zoom: 12 };

// ── Stub tiles ──────────────────────────────────────────────────────────────
const CRC_TABLE = (() => {
  const t = new Int32Array(256);
  for (let n = 0; n < 256; n++) { let c = n; for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1; t[n] = c; }
  return t;
})();
function crc32(buf) { let c = -1; for (const b of buf) c = CRC_TABLE[(c ^ b) & 0xff] ^ (c >>> 8); return (c ^ -1) >>> 0; }
function chunk(type, data) {
  const len = Buffer.alloc(4); len.writeUInt32BE(data.length);
  const td = Buffer.concat([Buffer.from(type), data]);
  const crc = Buffer.alloc(4); crc.writeUInt32BE(crc32(td));
  return Buffer.concat([len, td, crc]);
}
// 256x256 PNG; fn(i, j) -> [r, g, b] (colour) or grey number.
function png(fn, colour) {
  const W = 256, H = 256, bpp = colour ? 3 : 1, raw = Buffer.alloc((W * bpp + 1) * H);
  for (let j = 0; j < H; j++) {
    raw[j * (W * bpp + 1)] = 0;
    for (let i = 0; i < W; i++) {
      const v = fn(i, j), o = j * (W * bpp + 1) + 1 + i * bpp;
      if (colour) { raw[o] = v[0]; raw[o + 1] = v[1]; raw[o + 2] = v[2]; } else raw[o] = v;
    }
  }
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(W, 0); ihdr.writeUInt32BE(H, 4); ihdr[8] = 8; ihdr[9] = colour ? 2 : 0;
  return Buffer.concat([Buffer.from('\x89PNG\r\n\x1a\n', 'binary'), chunk('IHDR', ihdr), chunk('IDAT', zlib.deflateSync(raw)), chunk('IEND', Buffer.alloc(0))]);
}
// "Fields": a patchwork of greens and browns, so screenshots read as a photo stand-in.
const IMAGERY_STUB = png((i, j) => {
  const f = ((i >> 5) * 7 + (j >> 5) * 13) % 5;
  const n = ((i * 31 + j * 17) % 11) - 5;
  return [[86, 110, 60], [120, 132, 74], [150, 128, 90], [70, 96, 52], [104, 118, 68]][f].map(c => c + n);
}, true);
const OTHER_STUB = png(() => 200, false);
const TERRAIN_STUB = png((i, j) => 150 + Math.round(70 * Math.sin(i / 19) * Math.cos(j / 23)), false);

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #728 Esri World Imagery against ${BASE} ===\n`);

  // tiles: the map.tiles block to serve; dhm: add the terrain block.
  async function newCtx({ tiles, dhm = false, theme = 'light', storage = {}, viewport = { width: 1100, height: 720 } }) {
    const ctx = await browser.newContext({ viewport });
    const rec = { esri: [], imagery: [], osm: [], terrain: [], warnings: [], errors: [] };
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      delete config.tiles;
      delete config.dhm;
      config.map = Object.assign({}, config.map, { tiles: tiles || {} });
      if (dhm) config.dhm = DHM_BLOCK;
      await route.fulfill({ response, json: config });
    });
    await ctx.route(url => new URL(url).pathname === DHM_PATH, async (route) => {
      rec.terrain.push(route.request().url());
      await route.fulfill({ status: 200, contentType: 'image/png', body: TERRAIN_STUB });
    });
    await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, async (route) => {
      const url = route.request().url();
      if (route.request().resourceType() !== 'image') { await route.continue(); return; }
      if (/arcgisonline\.com/.test(url)) rec.esri.push(url);
      if (/World_Imagery/.test(url)) rec.imagery.push(url);
      if (/tile\.openstreetmap\.org/.test(url)) rec.osm.push(url);
      await route.fulfill({ status: 200, contentType: 'image/png', body: /World_Imagery/.test(url) ? IMAGERY_STUB : OTHER_STUB });
    });
    await ctx.addInitScript(({ theme: t, storage: st }) => {
      try {
        // Only on the first load, so a reload keeps what the page stored.
        if (!sessionStorage.getItem('__e2e728')) {
          sessionStorage.setItem('__e2e728', '1');
          localStorage.setItem('meshcore-theme', t);
          for (const k of Object.keys(st)) localStorage.setItem(k, st[k]);
        }
      } catch (_) {}
      const apply = () => document.documentElement && document.documentElement.setAttribute('data-theme', t);
      apply();
      document.addEventListener('DOMContentLoaded', apply);
    }, { theme, storage });
    const page = await ctx.newPage();
    page.on('console', m => {
      if (m.type() === 'warning') rec.warnings.push(m.text());
      if (m.type() === 'error') rec.errors.push(m.text());
    });
    page.on('pageerror', e => rec.errors.push(String(e)));
    return { ctx, page, rec };
  }

  async function gotoMap(page, hash, view = DK_VIEW) {
    await page.goto(`${BASE}/${hash}?lat=${view.lat}&lon=${view.lon}&zoom=${view.zoom}`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('.leaflet-control-layers', { timeout: 20000 });
    await page.waitForFunction(() => window.MeshConfigReady !== undefined, null, { timeout: 10000 });
    await page.evaluate(() => window.MeshConfigReady).catch(() => {});
    await page.waitForTimeout(900);
  }
  async function offeredIds(page) {
    return page.$$eval('.leaflet-control-layers-base label[data-tile-id]', ls => ls.map(l => l.getAttribute('data-tile-id')));
  }
  async function selectBase(page, id) {
    await page.evaluate((tid) => {
      const input = document.querySelector('.leaflet-control-layers-base label[data-tile-id="' + tid + '"] input');
      if (!input) throw new Error('no base layer ' + tid);
      input.click();
    }, id);
    await page.waitForTimeout(900);
  }
  async function state(page) {
    return page.evaluate(() => {
      const pane = document.querySelector('.leaflet-tile-pane');
      const attr = document.querySelector('.leaflet-control-attribution');
      const imgs = pane ? Array.from(pane.querySelectorAll('img.leaflet-tile')) : [];
      const z = (sel) => { const el = document.querySelector(sel); return el ? Number(getComputedStyle(el).zIndex) : null; };
      const tpane = document.querySelector('.mc-terrain-pane');
      return {
        filter: pane ? getComputedStyle(pane).filter : null,
        imageryTiles: imgs.filter(i => /World_Imagery/.test(i.src) && i.complete && i.naturalWidth === 256).length,
        terrainTiles: tpane ? Array.from(tpane.querySelectorAll('img.leaflet-tile')).filter(i => i.complete && i.naturalWidth === 256).length : 0,
        attribution: attr ? attr.textContent : '',
        esriLinks: attr ? Array.from(attr.querySelectorAll('a')).filter(a => a.getAttribute('href') === 'https://www.esri.com').map(a => a.textContent) : [],
        attributionVisible: (() => {
          if (!attr) return false;
          const r = attr.getBoundingClientRect();
          if (!r.width || !r.height) return false;
          const hit = document.elementFromPoint(r.x + r.width / 2, r.y + r.height / 2);
          return !!hit && attr.contains(hit);
        })(),
        zTile: z('.leaflet-tile-pane'), zTerrain: z('.mc-terrain-pane'),
      };
    });
  }
  async function pan(page, dx, dy) {
    const b = await page.$eval('.leaflet-container', el => { const r = el.getBoundingClientRect(); return { x: r.x + r.width / 2, y: r.y + r.height / 2 }; });
    await page.mouse.move(b.x, b.y);
    await page.mouse.down();
    await page.mouse.move(b.x + dx, b.y + dy, { steps: 8 });
    await page.mouse.up();
    await page.waitForTimeout(700);
  }
  async function zoomIn(page) { await page.click('.leaflet-control-zoom-in'); await page.waitForTimeout(900); }
  async function shot(page, name) {
    if (!SHOT_DIR) return;
    await page.screenshot({ path: path.join(SHOT_DIR, name), type: 'jpeg', quality: 55 });
  }
  function checkImageryUrls(urls) {
    for (const u of urls) {
      const m = IMAGERY_RE.exec(u);
      assert(m, 'unexpected imagery URL shape: ' + u);
      const z = +m[1], y = +m[2], x = +m[3];
      assert(z <= 19 && y < 2 ** z && x < 2 ** z, 'tile out of range: ' + u);
    }
  }

  // 1 — Esri off: not offered anywhere, no requests
  for (const p of [{ name: 'Map', hash: '#/map' }, { name: 'Live', hash: '#/live' }]) {
    await step(`${p.name}: Esri off → no Esri entry and no arcgisonline request`, async () => {
      const { ctx, page, rec } = await newCtx({ tiles: { providers: { opentopomap: { enabled: true } } } });
      try {
        await gotoMap(page, p.hash);
        const ids = await offeredIds(page);
        assert(ids.includes('opentopomap'), 'control not populated from config: ' + ids.join(','));
        for (const id of ESRI_IDS) assert(!ids.includes(id), id + ' offered with Esri off');
        await pan(page, 250, 0);
        await zoomIn(page);
        assert(rec.esri.length === 0, 'arcgisonline requests: ' + rec.esri.length);
        assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
      } finally { await ctx.close(); }
    });
  }

  await step('Customizer: Esri off → no Esri option in either provider select', async () => {
    const { ctx, page } = await newCtx({ tiles: {} });
    try {
      await gotoMap(page, '#/map');
      await page.waitForFunction(() => window._customizerV2 && window._customizerV2.initDone, null, { timeout: 15000 });
      await page.click('#customizeToggle');
      await page.waitForSelector('.cust-overlay:not(.hidden)', { timeout: 5000 });
      await page.click('.cust-tab[data-tab="display"]');
      await page.waitForSelector('#cv2-light-tile-provider', { timeout: 5000 });
      const opts = await page.$$eval('#cv2-light-tile-provider option, #cv2-dark-tile-provider option', os => os.map(o => o.value));
      assert(opts.length > 0, 'no provider options');
      for (const id of ESRI_IDS) assert(!opts.includes(id), id + ' offered in the customizer');
    } finally { await ctx.close(); }
  });

  // 2 — Esri off, but configured/stored as default: graceful fallback
  for (const theme of ['light', 'dark']) {
    await step(`Map (${theme}): Esri defaults + stored choice while Esri is off → OSM fallback, warning, no Esri request`, async () => {
      const { ctx, page, rec } = await newCtx({
        theme,
        tiles: { darkDefault: 'esri-darkgray-labels', lightDefault: 'esri-imagery', providers: { esri: { enabled: false } } },
        storage: { 'mc-light-tile-provider': 'esri-imagery', 'mc-dark-tile-provider': 'esri-imagery-dark' },
      });
      try {
        await gotoMap(page, '#/map');
        await pan(page, 200, 0);
        assert(rec.esri.length === 0, 'arcgisonline requests: ' + rec.esri.slice(0, 3).join(', '));
        assert(rec.osm.length > 0, 'the fallback OSM base map did not load');
        const s = await state(page);
        assert(/OpenStreetMap/.test(s.attribution), 'fallback credit missing: ' + s.attribution);
        const want = theme === 'dark' ? 'darkDefault "esri-darkgray-labels"' : 'lightDefault "esri-imagery"';
        assert(rec.warnings.some(w => w.includes(want) && w.includes('map.tiles.providers.esri.enabled: true')),
          'no console warning naming the switch: ' + JSON.stringify(rec.warnings));
        assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
      } finally { await ctx.close(); }
    });
  }

  // 3 — Esri on: offered, lazy, loads on selection
  await step('Map: Esri on → imagery offered, no request until selected, loads with the exact credit', async () => {
    const { ctx, page, rec } = await newCtx({ tiles: { providers: { esri: { enabled: true } } } });
    try {
      await gotoMap(page, '#/map');
      const ids = await offeredIds(page);
      for (const id of ESRI_IDS) assert(ids.includes(id), id + ' not offered: ' + ids.join(','));
      const label = await page.$eval('.leaflet-control-layers-base label[data-tile-id="esri-imagery"]', l => l.textContent.trim());
      assert(label === 'Esri World Imagery', 'label: ' + label);
      await pan(page, 250, 0);
      await zoomIn(page);
      assert(rec.imagery.length === 0, 'World_Imagery requested before selection: ' + rec.imagery.length);

      await selectBase(page, 'esri-imagery');
      assert(rec.imagery.length > 0, 'no World_Imagery request after selecting it');
      checkImageryUrls(rec.imagery);
      const s = await state(page);
      assert(s.imageryTiles > 0, 'no imagery tile rendered');
      assert(s.attribution.includes(CREDIT), 'credit missing or altered: ' + s.attribution);
      assert(s.esriLinks.join() === 'Esri', 'want one "Esri" link to ' + ESRI_HOME + ', got ' + JSON.stringify(s.esriLinks));
      assert(s.attributionVisible, 'credit is covered');
      assert(s.filter === 'none', 'tile pane filter on imagery: ' + s.filter);
      await shot(page, 'esri-imagery-light.jpg');

      await selectBase(page, '__auto__');
      const n = rec.imagery.length;
      await pan(page, -300, 100);
      await zoomIn(page);
      assert(rec.imagery.length === n, 'World_Imagery requests after switching back to Auto');
      assert(!(await state(page)).attribution.includes('Vantor'), 'imagery credit left after switching away');
      assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
    } finally { await ctx.close(); }
  });

  await step('Map: the imagery zooms to z19 and no further', async () => {
    const { ctx, page, rec } = await newCtx({ tiles: { providers: { esri: { enabled: true } } } });
    try {
      await gotoMap(page, '#/map', { lat: 55.676, lon: 12.568, zoom: 18 });
      await selectBase(page, 'esri-imagery');
      const capped = await page.$eval('.leaflet-control-zoom-in', el => el.classList.contains('leaflet-disabled'));
      assert(!capped, 'zoom-in disabled at z18: the imagery stops below z19');
      await zoomIn(page);
      checkImageryUrls(rec.imagery);
      assert(rec.imagery.some(u => /\/tile\/19\//.test(u)), 'no z19 imagery request');
      // With only the imagery on the map, its maxZoom is the map's limit.
      const disabled = await page.$eval('.leaflet-control-zoom-in', el => el.classList.contains('leaflet-disabled'));
      assert(disabled, 'zoom-in still enabled at z19 with the imagery selected');
    } finally { await ctx.close(); }
  });

  // 4 — dark theme: never inverted
  await step('Map (dark): esri-imagery-dark carries no filter; osm-dark does; Dark Gray credits Esri', async () => {
    const { ctx, page, rec } = await newCtx({ theme: 'dark', tiles: { providers: { esri: { enabled: true } } } });
    try {
      await gotoMap(page, '#/map');
      await selectBase(page, 'osm-dark');
      const inv = await state(page);
      assert(/invert/.test(inv.filter), 'probe sees no filter on osm-dark: ' + inv.filter);
      await selectBase(page, 'esri-imagery-dark');
      const s = await state(page);
      assert(s.filter === 'none', 'imagery is filtered in dark mode: ' + s.filter);
      assert(rec.imagery.length > 0 && s.imageryTiles > 0, 'imagery did not load in dark mode');
      assert(s.attribution.includes(CREDIT), 'credit missing in dark mode');
      assert(s.esriLinks.join() === 'Esri', 'Esri link in dark mode: ' + JSON.stringify(s.esriLinks));
      await shot(page, 'esri-imagery-dark.jpg');
      await selectBase(page, 'esri-darkgray-labels');
      const g = await state(page);
      assert(g.attribution.includes(POWERED_BY + 'Tiles © Esri') && !g.attribution.includes('Vantor'), 'Dark Gray credit: ' + g.attribution);
      assert(g.esriLinks.join() === 'Esri', 'Dark Gray Esri link: ' + JSON.stringify(g.esriLinks));
    } finally { await ctx.close(); }
  });

  // 5 — per-viewer choice through the customizer survives a reload, Map and Live
  await step('Customizer choice persists per viewer: reload → Map and Live load the imagery', async () => {
    const { ctx, page, rec } = await newCtx({ tiles: { providers: { esri: { enabled: true } } } });
    try {
      await gotoMap(page, '#/map');
      assert(rec.imagery.length === 0, 'imagery before choosing it');
      await page.waitForFunction(() => window._customizerV2 && window._customizerV2.initDone, null, { timeout: 15000 });
      await page.click('#customizeToggle');
      await page.waitForSelector('.cust-overlay:not(.hidden)', { timeout: 5000 });
      await page.click('.cust-tab[data-tab="display"]');
      await page.waitForSelector('#cv2-light-tile-provider option[value="esri-imagery"]', { state: 'attached', timeout: 5000 });
      await page.selectOption('#cv2-light-tile-provider', 'esri-imagery');
      await page.waitForTimeout(500);
      const stored = await page.evaluate(() => localStorage.getItem('mc-light-tile-provider'));
      assert(stored === 'esri-imagery', 'not stored: ' + stored);

      rec.imagery.length = 0;
      await page.reload({ waitUntil: 'domcontentloaded' });
      await page.waitForSelector('.leaflet-control-layers', { timeout: 20000 });
      await page.waitForTimeout(1500);
      assert(rec.imagery.length > 0, 'Map did not load the imagery after a reload');
      const s = await state(page);
      assert(s.attribution.includes(CREDIT), 'credit missing after reload: ' + s.attribution);

      rec.imagery.length = 0;
      await gotoMap(page, '#/live');
      await page.waitForTimeout(800);
      assert(rec.imagery.length > 0, 'Live did not load the imagery');
      checkImageryUrls(rec.imagery);
    } finally { await ctx.close(); }
  });

  // 6 — DHM hillshade on top of the imagery
  for (const p of [{ name: 'Map', hash: '#/map' }, { name: 'Live', hash: '#/live' }]) {
    await step(`${p.name}: DHM hillshade on top of the imagery — both load, terrain above, both credits`, async () => {
      const { ctx, page, rec } = await newCtx({
        dhm: true,
        tiles: { providers: { esri: { enabled: true } } },
        storage: { 'mc-light-tile-provider': 'esri-imagery', 'mc-terrain-dhm': '1' },
      });
      try {
        await gotoMap(page, p.hash);
        await page.waitForTimeout(800);
        const s = await state(page);
        assert(rec.imagery.length > 0 && s.imageryTiles > 0, 'imagery did not load under the terrain');
        assert(rec.terrain.length > 0 && s.terrainTiles > 0, 'terrain did not load over the imagery');
        assert(s.zTerrain > s.zTile, `terrain pane (${s.zTerrain}) not above the base tiles (${s.zTile})`);
        assert(s.attribution.includes(CREDIT), 'Esri credit missing with terrain on: ' + s.attribution);
        assert(s.esriLinks.join() === 'Esri', 'Esri link with terrain on: ' + JSON.stringify(s.esriLinks));
        assert(s.attribution.includes('Danmarks Højdemodel'), 'DHM credit missing: ' + s.attribution);
        assert(s.attributionVisible, 'credits are covered');
        assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
        if (p.name === 'Map') await shot(page, 'esri-imagery-terrain.jpg');
      } finally { await ctx.close(); }
    });
  }

  await browser.close();
  console.log(`\n#728 Esri imagery E2E: ${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
