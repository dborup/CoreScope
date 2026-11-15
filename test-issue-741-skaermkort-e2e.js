/**
 * E2E (#741 M1b): the Dataforsyningen Skærmkort base maps through WMS in
 * EPSG:3857, behind map.tiles.providers.dataforsyningen.
 *
 * The CI server has no map.tiles config, so /api/config/client is
 * intercepted and given the block an operator would set, with a dummy
 * token. Every request to api.dataforsyningen.dk is answered by a stub in
 * the browser (a 512 px PNG per layer) and recorded: nothing reaches
 * Dataforsyningen, and no real token exists anywhere in the run. Every
 * other third-party tile is stubbed too.
 *
 * Asserts:
 *   1. Off by default, and for every near miss (enabled without a token,
 *      enabled as a string): no Skærmkort entry in the layer control or
 *      the customizer, and zero Dataforsyningen requests, on Map and Live.
 *      A default naming a gated-off style falls back to OSM with a console
 *      warning naming both switches.
 *   2. On: the four styles are offered on Map and Live, and nothing is
 *      requested until one is picked (pan + zoom).
 *   3. Picked: every request is a GetMap with exactly the allow-listed
 *      parameters, 512x512, EPSG:3857, the style's layer, the dummy token
 *      exactly once, and a bbox that spans 512 px at the map's zoom (the
 *      512 px tile is not a magnified 256 px one); the tiles render; the
 *      credit is the exact CC BY 4.0 text with the dataset and licence
 *      links; no filter in light. Back to Auto stops the requests.
 *   4. Dark: the Auto map with the grey dark style requests
 *      dtk_skaermkort_graa and carries the invert filter, on Map and Live.
 *   5. The DHM hillshade stays above it, with both credits.
 *   6. Auto follows a theme switch between a WMS style and a tile style
 *      both ways on Map and Live: the old layer's tiles go, its requests
 *      stop, and the credit follows.
 *   7. The customizer's choice applies in place and persists across a
 *      reload, on Map and Live (the reload persistence is the customizer's).
 *   8. A real phone flow at 375x560: tap the layer control, tap a Skærmkort
 *      style; the pick survives a theme switch from the More sheet, a
 *      rebuild of the control and SPA navigation through the bottom nav;
 *      a reload starts from Auto again (no new persistence).
 *   9. Perf: a full 1920x1080 view is at most 20 GetMap requests (512 px
 *      tiles; 256 px tiles would need up to 54).
 *  10. Outside the layers' bounds (Berlin) nothing is requested.
 *  11. Below the layers' minZoom 6 the entries are disabled (Leaflet) and
 *      nothing is requested; at 6 a pick loads zoom-6 tiles, and the map
 *      then cannot be zoomed out below 6.
 *
 * Usage:
 *   BASE_URL=http://localhost:13741 node test-issue-741-skaermkort-e2e.js
 * Optional: SCREENSHOT_DIR=<dir> writes small screenshots (stub tiles only).
 */
'use strict';
const { chromium } = require('playwright');
const zlib = require('zlib');
const path = require('path');

const BASE = process.env.BASE_URL || 'http://localhost:13741';
const SHOT_DIR = process.env.SCREENSHOT_DIR || '';
const TOKEN = 'e2e-741-dummy-token';
const WMS_BASE = 'https://api.dataforsyningen.dk/topo_skaermkort_DAF';
const CREDIT = 'Indeholder data fra Klimadatastyrelsen og GeoDanmark, Skærmkort, CC BY 4.0';
const DATASET_URL = 'https://dataforsyningen.dk/data/962';
const LICENSE_URL = 'https://creativecommons.org/licenses/by/4.0/';
const OSM_CREDIT = '© OpenStreetMap contributors';
const LIGHT_IDS = ['dataforsyningen-skaermkort', 'dataforsyningen-skaermkort-graa', 'dataforsyningen-skaermkort-daempet'];
const DARK_IDS = ['dataforsyningen-skaermkort-graa-dark'];
const ALL_IDS = LIGHT_IDS.concat(DARK_IDS);
const INVERT = 'invert(1) hue-rotate(180deg) brightness(0.9) contrast(1.05)';
const ON = { enabled: true, token: TOKEN };
const DK_VIEW = { lat: 56.15, lon: 10.2, zoom: 12 };
// Leaflet's EPSG:3857 ground resolution at zoom 0, metres per pixel.
const RES0 = 2 * Math.PI * 6378137 / 256;
const DHM_PATH = '/api/map/dhm/wms';
const DHM_BLOCK = {
  enabled: true, url: DHM_PATH, layer: 'dhm_terraen_skyggekort',
  minZoom: 7, maxZoom: 19, bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
  attribution: 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0',
};

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
// size x size RGB PNG; fn(i, j) -> [r, g, b].
function png(size, fn) {
  const raw = Buffer.alloc((size * 3 + 1) * size);
  for (let j = 0; j < size; j++) {
    raw[j * (size * 3 + 1)] = 0;
    for (let i = 0; i < size; i++) {
      const v = fn(i, j), o = j * (size * 3 + 1) + 1 + i * 3;
      raw[o] = v[0]; raw[o + 1] = v[1]; raw[o + 2] = v[2];
    }
  }
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(size, 0); ihdr.writeUInt32BE(size, 4); ihdr[8] = 8; ihdr[9] = 2;
  return Buffer.concat([Buffer.from('\x89PNG\r\n\x1a\n', 'binary'), chunk('IHDR', ihdr), chunk('IDAT', zlib.deflateSync(raw)), chunk('IEND', Buffer.alloc(0))]);
}
// A street-map stand-in per layer, with a frame so the 512 px tiling shows.
const grid = (bg, line) => (i, j) => (i % 128 < 3 || j % 128 < 3) ? line : bg;
const WMS_STUBS = {
  dtk_skaermkort: png(512, grid([244, 236, 214], [214, 120, 60])),
  dtk_skaermkort_graa: png(512, grid([232, 232, 232], [140, 140, 140])),
  dtk_skaermkort_daempet: png(512, grid([236, 233, 224], [168, 150, 120])),
};
const OTHER_STUB = png(256, () => [200, 200, 200]);
const TERRAIN_STUB = png(256, (i, j) => { const v = 150 + Math.round(70 * Math.sin(i / 19) * Math.cos(j / 23)); return [v, v, v]; });

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

// The GetMap parameters Leaflet sends for these layers, and nothing else.
const PARAMS = ['token', 'service', 'request', 'layers', 'styles', 'format', 'transparent', 'version', 'width', 'height', 'crs', 'bbox'];
function checkGetMap(url, layer) {
  const u = new URL(url);
  assert(u.origin + u.pathname === WMS_BASE, 'wrong service: ' + url);
  const keys = Array.from(u.searchParams.keys());
  assert(keys.slice().sort().join() === PARAMS.slice().sort().join(), 'parameters ' + keys.join(',') + ' in ' + url);
  assert(u.searchParams.getAll('token').length === 1 && u.searchParams.get('token') === TOKEN, 'token: ' + url);
  const want = { service: 'WMS', request: 'GetMap', styles: '', format: 'image/png', transparent: 'false', version: '1.3.0', width: '512', height: '512', crs: 'EPSG:3857' };
  for (const k of Object.keys(want)) assert(u.searchParams.get(k) === want[k], k + '=' + u.searchParams.get(k) + ' in ' + url);
  if (layer) assert(u.searchParams.get('layers') === layer, 'layers=' + u.searchParams.get('layers') + ', want ' + layer);
  const b = u.searchParams.get('bbox').split(',').map(Number);
  assert(b.length === 4 && b.every(Number.isFinite), 'bbox: ' + url);
  const w = b[2] - b[0], h = b[3] - b[1];
  assert(Math.abs(w - h) < 1e-6 * w, 'bbox not square: ' + url);
  // A 512 px tile at zoom z spans 512 px of zoom z: w = 512 * RES0 / 2^z.
  const z = Math.log2(512 * RES0 / w);
  assert(Math.abs(z - Math.round(z)) < 1e-9, 'bbox ' + w + ' m is not 512 px at any zoom: ' + url);
  return { z: Math.round(z), layer: u.searchParams.get('layers') };
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #741 M1b Skærmkort against ${BASE} ===\n`);

  // tiles: the map.tiles block to serve; dhm: add the hillshade block.
  async function newCtx({ tiles, dhm = false, theme = 'light', storage = {}, viewport = { width: 1100, height: 720 }, phone = false }) {
    const ctx = await browser.newContext(phone
      ? { viewport, hasTouch: true, isMobile: true, deviceScaleFactor: 2 }
      : { viewport });
    const rec = { wms: [], osm: [], terrain: [], tokens: [], warnings: [], errors: [] };
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
      if (/[?&]token=/i.test(url)) rec.tokens.push(url);
      if (url.indexOf('https://api.dataforsyningen.dk/') === 0) {
        rec.wms.push(url);
        const layer = new URL(url).searchParams.get('layers');
        await route.fulfill({ status: 200, contentType: 'image/png', body: WMS_STUBS[layer] || OTHER_STUB });
        return;
      }
      // Nothing else third-party leaves the browser: non-image requests are refused.
      if (route.request().resourceType() !== 'image') { await route.abort(); return; }
      if (/tile\.openstreetmap\.org/.test(url)) rec.osm.push(url);
      await route.fulfill({ status: 200, contentType: 'image/png', body: OTHER_STUB });
    });
    await ctx.addInitScript(({ theme: t, storage: st }) => {
      try {
        // Only on the first load, so a reload keeps what the page stored.
        if (!sessionStorage.getItem('__e2e741')) {
          sessionStorage.setItem('__e2e741', '1');
          localStorage.setItem('meshcore-theme', t);
          for (const k of Object.keys(st)) localStorage.setItem(k, st[k]);
        }
      } catch (_) {}
    }, { theme, storage });
    const page = await ctx.newPage();
    page.on('console', m => {
      if (m.type() === 'warning') rec.warnings.push(m.text());
      if (m.type() === 'error') rec.errors.push(m.text());
    });
    page.on('pageerror', e => rec.errors.push(String(e)));
    return { ctx, page, rec };
  }

  async function settle(page) {
    await page.waitForSelector('.leaflet-control-layers', { timeout: 20000 });
    await page.waitForFunction(() => window.MeshConfigReady !== undefined, null, { timeout: 10000 });
    await page.evaluate(() => window.MeshConfigReady).catch(() => {});
    await page.waitForTimeout(900);
  }
  async function gotoMap(page, hash, view = DK_VIEW) {
    await page.goto(`${BASE}/${hash}?lat=${view.lat}&lon=${view.lon}&zoom=${view.zoom}`, { waitUntil: 'domcontentloaded' });
    await settle(page);
  }
  async function offeredIds(page) {
    return page.$$eval('.leaflet-control-layers-base label[data-tile-id]', ls => ls.map(l => l.getAttribute('data-tile-id')));
  }
  async function checkedId(page) {
    return page.evaluate(() => {
      const l = Array.from(document.querySelectorAll('.leaflet-control-layers-base label[data-tile-id]')).find(x => x.querySelector('input').checked);
      return l ? l.getAttribute('data-tile-id') : null;
    });
  }
  async function selectBase(page, id) {
    await page.evaluate((tid) => {
      const input = document.querySelector('.leaflet-control-layers-base label[data-tile-id="' + tid + '"] input');
      if (!input) throw new Error('no base layer ' + tid);
      input.click();
    }, id);
    await page.waitForTimeout(900);
  }
  async function setTheme(page, theme) {
    await page.evaluate((t) => document.documentElement.setAttribute('data-theme', t), theme);
    await page.waitForTimeout(900);
  }
  async function state(page) {
    return page.evaluate((base) => {
      const pane = document.querySelector('.leaflet-tile-pane');
      const attr = document.querySelector('.leaflet-control-attribution');
      const imgs = pane ? Array.from(pane.querySelectorAll('img.leaflet-tile')) : [];
      const z = (sel) => { const el = document.querySelector(sel); return el ? Number(getComputedStyle(el).zIndex) : null; };
      const tpane = document.querySelector('.mc-terrain-pane');
      return {
        filter: pane ? getComputedStyle(pane).filter : null,
        wmsTiles: imgs.filter(i => i.src.indexOf(base) === 0 && i.complete && i.naturalWidth === 512).length,
        wmsImgs: imgs.filter(i => i.src.indexOf(base) === 0).length,
        // The GetMap URLs of the tiles in the pane (a repeat view in the same
        // document is served from the in-memory image cache, with no request).
        wmsSrcs: imgs.filter(i => i.src.indexOf(base) === 0).map(i => i.src),
        osmImgs: imgs.filter(i => /tile\.openstreetmap\.org/.test(i.src)).length,
        terrainTiles: tpane ? Array.from(tpane.querySelectorAll('img.leaflet-tile')).filter(i => i.complete && i.naturalWidth === 256).length : 0,
        attribution: attr ? attr.textContent : '',
        links: attr ? Array.from(attr.querySelectorAll('a')).map(a => ({ href: a.getAttribute('href'), text: a.textContent, rel: a.getAttribute('rel'), target: a.getAttribute('target') })) : [],
        attributionVisible: (() => {
          if (!attr) return false;
          const r = attr.getBoundingClientRect();
          if (!r.width || !r.height) return false;
          if (r.left < 0 || r.top < 0 || r.right > innerWidth || r.bottom > innerHeight) return false;
          const hit = document.elementFromPoint(r.x + r.width / 2, r.y + r.height / 2);
          return !!hit && attr.contains(hit);
        })(),
        zTile: z('.leaflet-tile-pane'), zTerrain: z('.mc-terrain-pane'),
      };
    }, WMS_BASE);
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
    await page.screenshot({ path: path.join(SHOT_DIR, name), type: 'jpeg', quality: 60 });
  }
  function checkCredit(s, label) {
    assert(s.attribution.includes(CREDIT), label + ': credit missing: ' + JSON.stringify(s.attribution));
    const ds = s.links.filter(l => l.href === DATASET_URL), li = s.links.filter(l => l.href === LICENSE_URL);
    assert(ds.length === 1 && ds[0].text === 'Skærmkort', label + ': dataset link: ' + JSON.stringify(s.links));
    assert(li.length === 1 && li[0].text === 'CC BY 4.0', label + ': licence link: ' + JSON.stringify(s.links));
    for (const l of ds.concat(li)) {
      assert(l.target === '_blank' && /noopener/.test(l.rel), label + ': link attributes: ' + JSON.stringify(l));
    }
    assert(/license/.test(li[0].rel), label + ': licence rel: ' + li[0].rel);
  }
  function checkTokens(rec, label) {
    for (const u of rec.tokens) {
      assert(u.indexOf(WMS_BASE + '?token=' + TOKEN + '&') === 0, label + ': a token went somewhere else: ' + u);
    }
  }

  // 1 — off by default and for every near miss
  const OFF_CONFIGS = [
    ['no config', {}],
    ['enabled without a token', { providers: { dataforsyningen: { enabled: true } } }],
    ['enabled "true" (a string) with a token', { providers: { dataforsyningen: { enabled: 'true', token: TOKEN } } }],
  ];
  for (const [name, tiles] of OFF_CONFIGS) {
    for (const p of [{ name: 'Map', hash: '#/map' }, { name: 'Live', hash: '#/live' }]) {
      await step(`${p.name}, ${name}: no Skærmkort entry and no Dataforsyningen request`, async () => {
        const { ctx, page, rec } = await newCtx({ tiles });
        try {
          await gotoMap(page, p.hash);
          const ids = await offeredIds(page);
          assert(ids.length > 0, 'control not populated');
          for (const id of ALL_IDS) assert(!ids.includes(id), id + ' offered');
          await pan(page, 250, 0);
          await zoomIn(page);
          assert(rec.wms.length === 0, 'Dataforsyningen requests: ' + rec.wms.length);
          assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
        } finally { await ctx.close(); }
      });
    }
  }

  await step('Customizer, enabled without a token: no Skærmkort option in either provider select', async () => {
    const { ctx, page } = await newCtx({ tiles: { providers: { dataforsyningen: { enabled: true, token: '  ' } } } });
    try {
      await gotoMap(page, '#/map');
      await page.waitForFunction(() => window._customizerV2 && window._customizerV2.initDone, null, { timeout: 15000 });
      await page.click('#customizeToggle');
      await page.waitForSelector('.cust-overlay:not(.hidden)', { timeout: 5000 });
      await page.click('.cust-tab[data-tab="display"]');
      await page.waitForSelector('#cv2-light-tile-provider', { timeout: 5000 });
      const opts = await page.$$eval('#cv2-light-tile-provider option, #cv2-dark-tile-provider option', os => os.map(o => o.value));
      assert(opts.length > 0, 'no provider options');
      for (const id of ALL_IDS) assert(!opts.includes(id), id + ' offered in the customizer');
    } finally { await ctx.close(); }
  });

  await step('Map: Skærmkort defaults and a stored choice while it is off → OSM, a warning naming both switches, no request', async () => {
    const { ctx, page, rec } = await newCtx({
      tiles: { lightDefault: 'dataforsyningen-skaermkort', providers: { dataforsyningen: { enabled: true } } },
      storage: { 'mc-light-tile-provider': 'dataforsyningen-skaermkort-graa' },
    });
    try {
      await gotoMap(page, '#/map');
      await pan(page, 200, 0);
      assert(rec.wms.length === 0, 'Dataforsyningen requests: ' + rec.wms.length);
      assert(rec.osm.length > 0, 'the fallback OSM base map did not load');
      const s = await state(page);
      assert(s.attribution.includes(OSM_CREDIT), 'fallback credit: ' + s.attribution);
      const w = rec.warnings.filter(m => /dataforsyningen-skaermkort/.test(m));
      assert(w.length >= 1, 'no console warning: ' + rec.warnings.join(' | '));
      assert(/providers\.dataforsyningen\.enabled/.test(w[0]) && /providers\.dataforsyningen\.token/.test(w[0]), 'warning does not name both switches: ' + w[0]);
      assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
    } finally { await ctx.close(); }
  });

  // 2 — on: offered, nothing requested until picked
  for (const p of [{ name: 'Map', hash: '#/map' }, { name: 'Live', hash: '#/live' }]) {
    await step(`${p.name}: on → the four styles are offered, and nothing is requested until one is picked`, async () => {
      const { ctx, page, rec } = await newCtx({ tiles: { providers: { dataforsyningen: ON } } });
      try {
        await gotoMap(page, p.hash);
        const ids = await offeredIds(page);
        for (const id of ALL_IDS) assert(ids.includes(id), id + ' not offered: ' + ids.join(','));
        const labels = await page.$$eval('.leaflet-control-layers-base label[data-tile-id^="dataforsyningen-"]', ls => ls.map(l => l.textContent.trim()));
        assert(labels.join('|') === 'Skærmkort (Denmark)|Skærmkort grå (Denmark)|Skærmkort dæmpet (Denmark)|Skærmkort grå (Denmark)', 'labels: ' + labels.join('|'));
        await pan(page, 250, 0);
        await zoomIn(page);
        assert(rec.wms.length === 0, 'requests before a pick: ' + rec.wms.length);
      } finally { await ctx.close(); }
    });
  }

  // 3 — picked: the request shape, the tiles, the credit
  for (const p of [{ name: 'Map', hash: '#/map' }, { name: 'Live', hash: '#/live' }]) {
    await step(`${p.name}: picking Skærmkort loads 512 px EPSG:3857 GetMap tiles with the exact credit; Auto stops them`, async () => {
      const { ctx, page, rec } = await newCtx({ tiles: { providers: { dataforsyningen: ON } } });
      try {
        await gotoMap(page, p.hash);
        await selectBase(page, 'dataforsyningen-skaermkort');
        await page.waitForTimeout(600);
        assert(rec.wms.length > 0, 'no GetMap request');
        const s = await state(page);
        for (const u of rec.wms) {
          const g = checkGetMap(u, 'dtk_skaermkort');
          assert(g.z === DK_VIEW.zoom, 'bbox at zoom ' + g.z + ', map at ' + DK_VIEW.zoom);
        }
        assert(s.wmsTiles > 0 && s.wmsTiles === s.wmsImgs, 'tiles rendered: ' + s.wmsTiles + '/' + s.wmsImgs);
        assert(s.filter === 'none', 'filter in light: ' + s.filter);
        checkCredit(s, p.name);
        assert(s.attributionVisible, 'credit not visible');
        await shot(page, `741-${p.name.toLowerCase()}-skaermkort.jpg`);

        await zoomIn(page);
        await pan(page, -300, 120);
        for (const u of rec.wms) checkGetMap(u, 'dtk_skaermkort');
        checkTokens(rec, p.name);

        await selectBase(page, '__auto__');
        const n = rec.wms.length;
        await pan(page, 300, 0);
        assert(rec.wms.length === n, 'requests after Auto: ' + (rec.wms.length - n));
        const s2 = await state(page);
        assert(!s2.attribution.includes(CREDIT), 'Skærmkort credit left behind: ' + s2.attribution);
        assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
      } finally { await ctx.close(); }
    });
  }

  await step('Map: grå and dæmpet request their own layers', async () => {
    const { ctx, page, rec } = await newCtx({ tiles: { providers: { dataforsyningen: ON } } });
    try {
      await gotoMap(page, '#/map');
      for (const [id, layer] of [['dataforsyningen-skaermkort-graa', 'dtk_skaermkort_graa'], ['dataforsyningen-skaermkort-daempet', 'dtk_skaermkort_daempet']]) {
        rec.wms.length = 0;
        await selectBase(page, id);
        assert(rec.wms.length > 0, id + ': no request');
        for (const u of rec.wms) checkGetMap(u, layer);
        checkCredit(await state(page), id);
      }
    } finally { await ctx.close(); }
  });

  // 4 — dark: grey under the invert filter, through Auto
  for (const p of [{ name: 'Map', hash: '#/map' }, { name: 'Live', hash: '#/live' }]) {
    await step(`${p.name} (dark): the Auto map with the grey dark default requests dtk_skaermkort_graa under the invert filter`, async () => {
      const { ctx, page, rec } = await newCtx({ theme: 'dark', tiles: { darkDefault: 'dataforsyningen-skaermkort-graa-dark', providers: { dataforsyningen: ON } } });
      try {
        await gotoMap(page, p.hash);
        assert(await checkedId(page) === '__auto__', 'Auto is the selection');
        assert(rec.wms.length > 0, 'no GetMap request');
        for (const u of rec.wms) checkGetMap(u, 'dtk_skaermkort_graa');
        assert(rec.osm.length === 0, 'OSM requested under a Skærmkort default: ' + rec.osm.length);
        const s = await state(page);
        assert(s.filter === INVERT, 'filter: ' + s.filter);
        assert(s.wmsTiles > 0, 'no tile rendered');
        checkCredit(s, p.name);
        await shot(page, `741-${p.name.toLowerCase()}-graa-dark.jpg`);
        assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
      } finally { await ctx.close(); }
    });
  }

  // 5 — the hillshade stays above
  await step('Map: the DHM hillshade loads above Skærmkort, both credits shown', async () => {
    const { ctx, page, rec } = await newCtx({
      dhm: true,
      tiles: { lightDefault: 'dataforsyningen-skaermkort-daempet', providers: { dataforsyningen: ON } },
      storage: { 'mc-terrain-dhm': '1' },
    });
    try {
      await gotoMap(page, '#/map');
      await page.waitForTimeout(600);
      const s = await state(page);
      assert(rec.wms.length > 0 && s.wmsTiles > 0, 'no Skærmkort tiles');
      assert(rec.terrain.length > 0 && s.terrainTiles > 0, 'no terrain tiles');
      assert(s.zTerrain > s.zTile, 'terrain pane z ' + s.zTerrain + ' not above tile pane z ' + s.zTile);
      checkCredit(s, 'with hillshade');
      assert(s.attribution.includes(DHM_BLOCK.attribution), 'DHM credit missing: ' + s.attribution);
    } finally { await ctx.close(); }
  });

  // 6 — Auto across a theme switch, WMS ↔ tile, both ways
  for (const p of [{ name: 'Map', hash: '#/map' }, { name: 'Live', hash: '#/live' }]) {
    await step(`${p.name}: Auto swaps Skærmkort (light) and OSM (dark) on a theme switch, both ways, with the credit`, async () => {
      const { ctx, page, rec } = await newCtx({ tiles: { lightDefault: 'dataforsyningen-skaermkort-daempet', providers: { dataforsyningen: ON } } });
      try {
        await gotoMap(page, p.hash);
        let s = await state(page);
        assert(s.wmsTiles > 0, 'light: no Skærmkort tiles');
        checkCredit(s, 'light');

        rec.osm.length = 0;
        await setTheme(page, 'dark');
        s = await state(page);
        assert(rec.osm.length > 0 && s.osmImgs > 0, 'dark: OSM not loaded');
        assert(s.wmsImgs === 0, 'dark: Skærmkort tiles left in the pane: ' + s.wmsImgs);
        assert(!s.attribution.includes(CREDIT), 'dark: Skærmkort credit left behind: ' + s.attribution);
        assert(s.attribution.includes(OSM_CREDIT), 'dark: OSM credit missing: ' + s.attribution);
        assert(s.filter === INVERT, 'dark: osm-dark filter: ' + s.filter);
        const n = rec.wms.length;
        await pan(page, 250, 0);
        assert(rec.wms.length === n, 'dark: Skærmkort still requested');

        await setTheme(page, 'light');
        s = await state(page);
        assert(rec.wms.length > n && s.wmsTiles > 0, 'light again: Skærmkort not back');
        assert(s.osmImgs === 0, 'light again: OSM tiles left: ' + s.osmImgs);
        assert(s.filter === 'none', 'light again: filter ' + s.filter);
        checkCredit(s, 'light again');
        assert(!s.attribution.includes(OSM_CREDIT), 'light again: OSM credit left behind: ' + s.attribution);
        for (const u of rec.wms) checkGetMap(u, 'dtk_skaermkort_daempet');
        assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
      } finally { await ctx.close(); }
    });
  }

  // 7 — the customizer: in place, then across a reload
  await step('Customizer: picking Skærmkort grå applies in place, and after a reload Map and Live load it', async () => {
    const { ctx, page, rec } = await newCtx({ tiles: { providers: { dataforsyningen: ON } } });
    try {
      await gotoMap(page, '#/map');
      assert(rec.wms.length === 0, 'requests before choosing it');
      await page.waitForFunction(() => window._customizerV2 && window._customizerV2.initDone, null, { timeout: 15000 });
      await page.click('#customizeToggle');
      await page.waitForSelector('.cust-overlay:not(.hidden)', { timeout: 5000 });
      await page.click('.cust-tab[data-tab="display"]');
      await page.waitForSelector('#cv2-light-tile-provider option[value="dataforsyningen-skaermkort-graa"]', { state: 'attached', timeout: 5000 });
      const darkOpts = await page.$$eval('#cv2-dark-tile-provider option', os => os.map(o => o.value));
      assert(darkOpts.includes('dataforsyningen-skaermkort-graa-dark'), 'dark option missing: ' + darkOpts.join(','));
      await page.selectOption('#cv2-light-tile-provider', 'dataforsyningen-skaermkort-graa');
      await page.waitForTimeout(1200);
      assert(await page.evaluate(() => localStorage.getItem('mc-light-tile-provider')) === 'dataforsyningen-skaermkort-graa', 'not stored');
      assert(rec.wms.length > 0, 'the Map did not switch in place');
      for (const u of rec.wms) checkGetMap(u, 'dtk_skaermkort_graa');
      await page.keyboard.press('Escape');

      rec.wms.length = 0;
      await page.reload({ waitUntil: 'domcontentloaded' });
      await settle(page);
      assert(rec.wms.length > 0, 'Map did not load Skærmkort after a reload');
      checkCredit(await state(page), 'after reload');

      rec.wms.length = 0;
      await gotoMap(page, '#/live');
      assert(rec.wms.length > 0, 'Live did not load Skærmkort');
      for (const u of rec.wms) checkGetMap(u, 'dtk_skaermkort_graa');
    } finally { await ctx.close(); }
  });

  // 8 — the phone flow
  await step('Phone 375x560: pick Skærmkort in the layer control; it survives a theme switch, a rebuild and SPA navigation; a reload is Auto', async () => {
    const { ctx, page, rec } = await newCtx({ tiles: { providers: { dataforsyningen: ON } }, viewport: { width: 375, height: 560 }, phone: true });
    try {
      await gotoMap(page, '#/map');
      for (const b of await page.$$('text=Got it')) { try { await b.tap(); } catch (_) {} }
      await page.waitForTimeout(300);
      // The control is collapsed on a phone: tap it open, tap the style.
      await page.tap('.leaflet-control-layers-toggle');
      await page.waitForSelector('.leaflet-control-layers-expanded', { timeout: 5000 });
      const label = page.locator('.leaflet-control-layers-base label[data-tile-id="dataforsyningen-skaermkort-daempet"]');
      await label.scrollIntoViewIfNeeded();
      const box = await label.boundingBox();
      assert(box && box.x >= 0 && box.x + box.width <= 375 && box.height > 0, 'label not on screen: ' + JSON.stringify(box));
      await label.tap();
      await page.waitForTimeout(1200);
      assert(await checkedId(page) === 'dataforsyningen-skaermkort-daempet', 'not selected: ' + await checkedId(page));
      assert(rec.wms.length > 0, 'no GetMap request after the tap');
      for (const u of rec.wms) checkGetMap(u, 'dtk_skaermkort_daempet');
      // Close the control by tapping the map, as a user would.
      await page.tap('.leaflet-container', { position: { x: 300, y: 300 } });
      await page.waitForTimeout(500);
      let s = await state(page);
      assert(s.wmsTiles > 0, 'no Skærmkort tile rendered');
      checkCredit(s, 'phone');
      assert(s.attributionVisible, 'credit not visible on the phone');
      await shot(page, '741-phone-picked.jpg');

      // Theme switch through the More sheet's dark-mode button.
      await page.tap('.bottom-nav >> text=More');
      await page.waitForSelector('.bottom-nav-sheet-darkbtn', { state: 'visible', timeout: 5000 });
      await page.tap('.bottom-nav-sheet-darkbtn');
      await page.waitForTimeout(900);
      assert(await page.evaluate(() => document.documentElement.getAttribute('data-theme')) === 'dark', 'theme did not switch');
      await page.keyboard.press('Escape');
      await page.waitForTimeout(400);
      if (await page.locator('.bottom-nav-sheet-darkbtn').isVisible()) {
        await page.tap('.leaflet-container', { position: { x: 300, y: 250 } });
        await page.waitForTimeout(400);
      }
      assert(await checkedId(page) === 'dataforsyningen-skaermkort-daempet', 'theme switch: ' + await checkedId(page));
      s = await state(page);
      assert(s.wmsTiles > 0 && s.filter === 'none', 'theme switch: tiles ' + s.wmsTiles + ', filter ' + s.filter);

      // A rebuild of the control, as when the config lands late.
      await page.evaluate(() => window.MC_initTileRegistry(true));
      await page.waitForTimeout(900);
      assert(await checkedId(page) === 'dataforsyningen-skaermkort-daempet', 'rebuild: ' + await checkedId(page));
      s = await state(page);
      assert(s.wmsTiles > 0, 'rebuild: no tiles');

      // SPA navigation through the bottom nav and back. Zoom in first, as a
      // user looking at the map would: the Map page restores the last view
      // it saved (map-view), else its default centre, which is outside
      // Denmark (step 10 covers that).
      await page.tap('.leaflet-control-zoom-in');
      await page.waitForTimeout(900);
      await page.tap('.bottom-nav a[href="#/packets"]');
      await page.waitForFunction(() => location.hash.indexOf('#/packets') === 0, null, { timeout: 5000 });
      await page.waitForTimeout(800);
      rec.wms.length = 0;
      await page.tap('.bottom-nav a[href="#/map"]');
      await settle(page);
      assert(await checkedId(page) === 'dataforsyningen-skaermkort-daempet', 'SPA navigation: ' + await checkedId(page));
      s = await state(page);
      assert(s.wmsTiles > 0 && s.wmsTiles === s.wmsImgs, 'SPA navigation: tiles ' + s.wmsTiles + '/' + s.wmsImgs);
      for (const u of s.wmsSrcs.concat(rec.wms)) checkGetMap(u, 'dtk_skaermkort_daempet');
      checkCredit(s, 'SPA navigation');
      await shot(page, '741-phone-after-nav.jpg');

      // A reload: the layer control's pick is not persisted.
      rec.wms.length = 0;
      await page.reload({ waitUntil: 'domcontentloaded' });
      await settle(page);
      assert(await checkedId(page) === '__auto__', 'reload: ' + await checkedId(page));
      assert(rec.wms.length === 0, 'reload: Skærmkort requested ' + rec.wms.length + ' times');
      checkTokens(rec, 'phone');
      assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
    } finally { await ctx.close(); }
  });

  // 10 — outside the coverage nothing is requested
  await step('Map: with Skærmkort picked, a view outside its bounds (Berlin) requests nothing', async () => {
    const { ctx, page, rec } = await newCtx({ tiles: { providers: { dataforsyningen: ON } } });
    try {
      await gotoMap(page, '#/map', { lat: 52.52, lon: 13.4, zoom: 12 });
      await selectBase(page, 'dataforsyningen-skaermkort');
      await pan(page, 200, 100);
      assert(rec.wms.length === 0, 'requests outside the bounds: ' + rec.wms.slice(0, 2).join(' '));
      assert(await checkedId(page) === 'dataforsyningen-skaermkort', 'still the selection');
      assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
    } finally { await ctx.close(); }
  });

  // 11 — the zoom floor: L.Control.Layers disables a base layer whose
  // minZoom is above the map's zoom, so below 6 the styles cannot be
  // picked; once picked, the map cannot be zoomed out below 6.
  await step('Map: below zoom 6 the Skærmkort entries are disabled; at 6 a pick loads, and the map stays at 6 or above', async () => {
    const { ctx, page, rec } = await newCtx({ tiles: { providers: { dataforsyningen: ON } } });
    const disabled = () => page.$$eval('.leaflet-control-layers-base label[data-tile-id^="dataforsyningen-"] input', is => is.map(i => i.disabled));
    try {
      await gotoMap(page, '#/map', { lat: 56.0, lon: 10.5, zoom: 5 });
      assert((await disabled()).join() === 'true,true,true,true', 'at zoom 5: ' + (await disabled()).join());
      await zoomIn(page);
      assert((await disabled()).join() === 'false,false,false,false', 'at zoom 6: ' + (await disabled()).join());
      await selectBase(page, 'dataforsyningen-skaermkort');
      assert(rec.wms.length > 0, 'nothing loaded at zoom 6');
      for (const u of rec.wms) assert(checkGetMap(u, 'dtk_skaermkort').z === 6, 'not a zoom-6 tile: ' + u);
      checkCredit(await state(page), 'zoom 6');
      // The layer is the map's only tile layer, so Leaflet takes the map's
      // zoom range from it: zoom-out is disabled at 6.
      const out = await page.$eval('.leaflet-control-zoom-out', a => a.classList.contains('leaflet-disabled'));
      assert(out, 'zoom-out still enabled at the floor');
      await page.mouse.move(500, 360);
      await page.mouse.wheel(0, 600);
      await page.waitForTimeout(900);
      const z = await page.evaluate(() => JSON.parse(localStorage.getItem('map-view') || '{}').zoom);
      assert(z === 6, 'zoomed below the floor: ' + z);
      for (const u of rec.wms) assert(checkGetMap(u, 'dtk_skaermkort').z >= 6, 'tile below zoom 6: ' + u);
      assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
    } finally { await ctx.close(); }
  });

  // 9 — perf: requests per full view
  await step('Perf: a full 1920x1080 view of Skærmkort is at most 20 GetMap requests (256 px tiles: up to 54)', async () => {
    const { ctx, page, rec } = await newCtx({ tiles: { lightDefault: 'dataforsyningen-skaermkort', providers: { dataforsyningen: ON } }, viewport: { width: 1920, height: 1080 } });
    try {
      await gotoMap(page, '#/map');
      await page.waitForTimeout(600);
      const first = rec.wms.length;
      const view = await page.$eval('.leaflet-container', el => ({ w: el.clientWidth, h: el.clientHeight }));
      const max512 = (Math.ceil(view.w / 512) + 1) * (Math.ceil(view.h / 512) + 1);
      const max256 = (Math.ceil(view.w / 256) + 1) * (Math.ceil(view.h / 256) + 1);
      console.log(`    map ${view.w}x${view.h}: ${first} GetMap requests for the first view (bound ${max512}; 256 px tiles: up to ${max256})`);
      assert(first > 0 && first <= max512 && first <= 20, 'requests for one view: ' + first);
      for (const u of rec.wms) checkGetMap(u, 'dtk_skaermkort');
    } finally { await ctx.close(); }
  });

  await browser.close();
  console.log(`\n=== #741 M1b Skærmkort: ${passed} passed, ${failed} failed ===`);
  process.exit(failed ? 1 : 0);
})().catch(e => { console.error(e); process.exit(1); });
