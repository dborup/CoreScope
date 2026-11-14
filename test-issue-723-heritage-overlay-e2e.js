/**
 * E2E (#723 M1): protected ancient monuments overlay on Map and Live.
 *
 * The CI server has no map.heritage config, so /api/config/client is
 * intercepted and given the `heritage` block the Go server sends while
 * map.heritage.enabled is true (cmd/server/heritage_overlay.go). The
 * agency's WMS is never contacted: every request to it is answered here
 * with a synthetic transparent PNG and recorded.
 *
 * Asserts:
 *   1. not offered without the block, and no request to the WMS at all;
 *   2. offered, off by default, no requests while off; toggling on/off on
 *      Map and Live: tiles, opacity, pane z-order (above the hillshade
 *      pane, below overlays/markers), pointer-events, credit, hash,
 *      storage — and on Live the credit comes back on a second "on";
 *   3. every request carries exactly the pinned WMS parameter set
 *      (service, request, version 1.3.0, both layers, styles, PNG,
 *      transparent, 256x256, EPSG:3857, bbox) against the agency's URL;
 *   4. zoom 11 makes no requests and draws nothing; zoom 12 does;
 *   5. ?heritage=1 deep link, reload and the next page keep it on;
 *      ?heritage=0 wins over a stored "on";
 *   6. dark theme: the overlay pane is not inverted with the base tiles;
 *   7. next to the DHM hillshade both overlays are listed and independent;
 *   8. outside Denmark: no requests, no console errors.
 *
 * Usage:
 *   BASE_URL=http://localhost:13723 node test-issue-723-heritage-overlay-e2e.js
 * Optional: SCREENSHOT_DIR=<dir> writes Map/Live light/dark screenshots
 * with real base-map tiles (the overlay stays the synthetic stub).
 */
'use strict';
const { chromium } = require('playwright');
const zlib = require('zlib');
const path = require('path');

const BASE = process.env.BASE_URL || 'http://localhost:13723';
const SHOT_DIR = process.env.SCREENSHOT_DIR || '';
const WMS_URL = 'https://www.kulturarv.dk/ffgeoserver/public/wms';
const HERITAGE_BLOCK = {
  enabled: true, url: WMS_URL,
  layers: 'fundogfortidsminder_areal_beskyttelse,fundogfortidsminder_punkt_fredet',
  minZoom: 12, maxZoom: 19, bounds: [[54.4646, 8.0899], [57.7289, 15.5689]],
  attribution: '© Slots- og Kulturstyrelsen',
};
const DHM_BLOCK = {
  enabled: true, url: '/api/map/dhm/wms', layer: 'dhm_terraen_skyggekort',
  minZoom: 7, maxZoom: 19, bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
  attribution: 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0',
};
// The exact GetMap parameter set the overlay sends (plus bbox).
const PINNED = {
  service: 'WMS', request: 'GetMap', version: '1.3.0',
  layers: 'fundogfortidsminder_areal_beskyttelse,fundogfortidsminder_punkt_fredet',
  styles: '', format: 'image/png', transparent: 'true', width: '256', height: '256', crs: 'EPSG:3857',
};
const LABEL = '.leaflet-control-layers-overlays label[data-overlay-id="heritage-monuments"]';
// Jelling, a known monument field; zoom 13 is inside the overlay's range.
const DK = { lat: 55.756, lon: 9.42, zoom: 13 };
const PNG_1X1 = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg==',
  'base64');

// ── Synthetic monument tile: transparent, one pink zone with a red dot ─────
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
const MONUMENT_PNG = (() => {
  const W = 256, H = 256, raw = Buffer.alloc((W * 4 + 1) * H);
  for (let y = 0; y < H; y++) {
    raw[y * (W * 4 + 1)] = 0;
    for (let x = 0; x < W; x++) {
      const d = Math.hypot(x - 128, y - 128), o = y * (W * 4 + 1) + 1 + x * 4;
      let px = [0, 0, 0, 0];
      if (d < 6) px = [255, 0, 0, 255];
      else if (d < 38) px = [255, 200, 210, 160];
      else if (d < 40) px = [0, 0, 0, 255];
      raw[o] = px[0]; raw[o + 1] = px[1]; raw[o + 2] = px[2]; raw[o + 3] = px[3];
    }
  }
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(W, 0); ihdr.writeUInt32BE(H, 4); ihdr[8] = 8; ihdr[9] = 6;
  return Buffer.concat([Buffer.from('\x89PNG\r\n\x1a\n', 'binary'), chunk('IHDR', ihdr), chunk('IDAT', zlib.deflateSync(raw)), chunk('IEND', Buffer.alloc(0))]);
})();

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
  console.log(`\n=== #723 protected ancient monuments overlay against ${BASE} ===\n`);

  async function newCtx({ heritage = true, dhm = false, theme = 'light', storage = {}, viewport = { width: 1280, height: 800 }, realTiles = false }) {
    const ctx = await browser.newContext({ viewport });
    const rec = { heritage: [], consoleErrors: [], failed: [] };
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      delete config.heritage;
      delete config.dhm;
      if (heritage) config.heritage = HERITAGE_BLOCK;
      if (dhm) config.dhm = DHM_BLOCK;
      await route.fulfill({ response, json: config });
    });
    await ctx.route(url => new URL(url).pathname === DHM_BLOCK.url, route =>
      route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 }));
    // Base-map tiles and anything else off-site: a 1x1 PNG unless the
    // optional screenshots need the real base map.
    await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, async (route) => {
      if (route.request().resourceType() !== 'image' || realTiles) { await route.continue(); return; }
      await route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 });
    });
    // Registered last, so it runs first: the agency's host never sees a
    // request from this suite.
    await ctx.route(/^https?:\/\/www\.kulturarv\.dk\//i, async (route) => {
      rec.heritage.push(new URL(route.request().url()));
      await route.fulfill({ status: 200, contentType: 'image/png', headers: { 'Access-Control-Allow-Origin': '*' }, body: MONUMENT_PNG });
    });
    await ctx.addInitScript(({ theme: t, storage: st }) => {
      try {
        localStorage.setItem('meshcore-theme', t);
        for (const k of Object.keys(st)) localStorage.setItem(k, st[k]);
      } catch (_) {}
      const apply = () => document.documentElement && document.documentElement.setAttribute('data-theme', t);
      apply();
      document.addEventListener('DOMContentLoaded', apply);
    }, { theme, storage });
    const page = await ctx.newPage();
    page.on('console', m => { if (m.type() === 'error') rec.consoleErrors.push(m.text()); });
    page.on('pageerror', e => rec.consoleErrors.push(String(e)));
    page.on('requestfailed', r => rec.failed.push(r.url()));
    return { ctx, page, rec };
  }

  const PAGES = [
    { name: 'Map', hash: '#/map' },
    { name: 'Live', hash: '#/live' },
  ];

  async function gotoMap(page, hash, view) {
    const sep = hash.includes('?') ? '&' : '?';
    const v = view || DK;
    await page.goto(`${BASE}/${hash}${sep}lat=${v.lat}&lon=${v.lon}&zoom=${v.zoom}`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('.leaflet-control-layers', { timeout: 20000 });
    await page.waitForFunction(() => window.MeshConfigReady !== undefined, null, { timeout: 10000 });
    await page.evaluate(() => window.MeshConfigReady).catch(() => {});
    await page.waitForTimeout(800);
  }
  async function setOverlay(page, on, sel) {
    await page.evaluate(({ sel: s, on: want }) => {
      const input = document.querySelector(s + ' input[type="checkbox"]');
      if (!input) throw new Error('no checkbox for ' + s);
      if (input.checked !== want) input.click();
    }, { sel: sel || LABEL, on });
    await page.waitForTimeout(900);
  }
  async function state(page) {
    return page.evaluate((sel) => {
      const pane = document.querySelector('.mc-heritage-pane');
      const tiles = pane ? Array.from(pane.querySelectorAll('img.leaflet-tile')) : [];
      const layer = pane ? pane.querySelector('.leaflet-layer') : null;
      const z = (s) => { const el = document.querySelector(s); return el ? Number(getComputedStyle(el).zIndex) : null; };
      const attr = document.querySelector('.leaflet-control-attribution');
      const input = document.querySelector(sel + ' input');
      const label = document.querySelector(sel);
      return {
        offered: !!label,
        label: label ? label.textContent.trim() : null,
        checked: input ? input.checked : null,
        tiles: tiles.length,
        loadedTiles: tiles.filter(t => t.complete && t.naturalWidth === 256).length,
        opacity: layer ? Number(getComputedStyle(layer).opacity) : null,
        zHeritage: z('.mc-heritage-pane'),
        zTerrain: z('.mc-terrain-pane'),
        zTile: z('.leaflet-tile-pane'),
        zOverlay: z('.leaflet-overlay-pane'),
        zMarker: z('.leaflet-marker-pane'),
        pointerEvents: pane ? getComputedStyle(pane).pointerEvents : null,
        paneFilter: pane ? getComputedStyle(pane).filter : null,
        paneBlend: pane ? getComputedStyle(pane).mixBlendMode : null,
        insideTilePane: pane ? !!pane.closest('.leaflet-tile-pane') : null,
        tilePaneFilter: (() => { const t = document.querySelector('.leaflet-tile-pane'); return t ? getComputedStyle(t).filter : null; })(),
        attribution: attr ? attr.textContent : '',
        attributionVisible: (() => {
          if (!attr) return false;
          const r = attr.getBoundingClientRect();
          const hit = document.elementFromPoint(r.x + r.width / 2, r.y + r.height / 2);
          return !!hit && attr.contains(hit);
        })(),
        hash: location.hash,
        search: location.search,
        stored: localStorage.getItem('mc-heritage'),
        storedTerrain: localStorage.getItem('mc-terrain-dhm'),
      };
    }, LABEL);
  }
  function checkShape(urls) {
    assert(urls.length > 0, 'no heritage requests to check');
    for (const u of urls) {
      assert(u.origin + u.pathname === WMS_URL, 'wrong service: ' + u.origin + u.pathname);
      const keys = Array.from(u.searchParams.keys()).sort();
      const expectKeys = Object.keys(PINNED).concat('bbox').sort();
      assert(JSON.stringify(keys) === JSON.stringify(expectKeys), 'param set ' + keys.join(','));
      for (const [k, v] of Object.entries(PINNED)) assert(u.searchParams.get(k) === v, `${k}=${u.searchParams.get(k)}`);
      const bbox = u.searchParams.get('bbox').split(',').map(Number);
      assert(bbox.length === 4 && bbox.every(Number.isFinite) && bbox[2] > bbox[0] && bbox[3] > bbox[1], 'bbox ' + bbox);
    }
  }
  async function pan(page, dx, dy) {
    const box = await page.$eval('.leaflet-container', el => { const r = el.getBoundingClientRect(); return { x: r.x + r.width / 2, y: r.y + r.height / 2 }; });
    await page.mouse.move(box.x, box.y);
    await page.mouse.down();
    await page.mouse.move(box.x + dx, box.y + dy, { steps: 8 });
    await page.mouse.up();
    await page.waitForTimeout(700);
  }
  async function zoomIn(page) {
    await page.click('.leaflet-control-zoom-in');
    await page.waitForTimeout(900);
  }

  // 1 — not offered without the block
  for (const p of PAGES) {
    await step(`${p.name}: not offered without a heritage block, no WMS requests`, async () => {
      const { ctx, page, rec } = await newCtx({ heritage: false, storage: { 'mc-heritage': '1' } });
      try {
        await gotoMap(page, p.hash + '?heritage=1');
        assert(!(await page.$(LABEL)), 'offered without config');
        await pan(page, 200, 0);
        assert(rec.heritage.length === 0, 'WMS requests without config: ' + rec.heritage.length);
      } finally { await ctx.close(); }
    });
  }

  // 2/3 — offered, off by default, toggle on/off/on
  for (const p of PAGES) {
    await step(`${p.name}: off by default; on/off/on — tiles, pane, credit, hash, storage, pinned request`, async () => {
      const { ctx, page, rec } = await newCtx({});
      try {
        await gotoMap(page, p.hash);
        let s = await state(page);
        assert(s.offered, 'not offered');
        assert(/zoom 12\+/.test(s.label), 'label does not say where it starts: ' + s.label);
        assert(s.checked === false && s.tiles === 0, 'not off by default: ' + JSON.stringify(s));
        await pan(page, 200, 0);
        assert(rec.heritage.length === 0, 'requests while off: ' + rec.heritage.length);

        await setOverlay(page, true);
        s = await state(page);
        assert(s.checked === true, 'checkbox not checked');
        assert(s.loadedTiles > 0, 'no loaded tiles: ' + JSON.stringify(s));
        assert(s.opacity >= 0.7 && s.opacity <= 0.95, 'opacity ' + s.opacity);
        assert(s.zHeritage > 250 && s.zHeritage < s.zOverlay && s.zHeritage < s.zMarker && s.zHeritage > s.zTile,
          `z-order tile=${s.zTile} heritage=${s.zHeritage} overlay=${s.zOverlay} marker=${s.zMarker}`);
        assert(s.pointerEvents === 'none', 'pane captures pointer events');
        assert(s.attribution.includes('© Slots- og Kulturstyrelsen'), 'credit missing: ' + s.attribution);
        assert(s.attributionVisible, 'credit covered by other UI');
        assert(/[?&]heritage=1\b/.test(s.hash), 'hash not updated: ' + s.hash);
        assert(s.search === '', 'parameter leaked into the query string: ' + s.search);
        assert(s.stored === '1', 'not stored: ' + s.stored);
        checkShape(rec.heritage);
        const first = rec.heritage.length;
        await pan(page, -300, 0);
        await zoomIn(page);
        console.log(`    ${p.name} WMS requests: initial ${first}, after a pan + zoom ${rec.heritage.length}`);
        assert(first <= 80 && rec.heritage.length - first <= 120, 'unexpectedly many requests');
        checkShape(rec.heritage);

        await setOverlay(page, false);
        s = await state(page);
        assert(s.checked === false && s.tiles === 0, 'tiles left after off: ' + JSON.stringify(s));
        assert(!s.attribution.includes('Slots- og Kulturstyrelsen'), 'credit left after off');
        assert(!/[?&]heritage=/.test(s.hash), 'hash still carries heritage: ' + s.hash);
        assert(s.stored === '0', 'off not stored');
        const offCount = rec.heritage.length;
        await pan(page, 200, 100);
        assert(rec.heritage.length === offCount, 'requests after off');

        await setOverlay(page, true);
        s = await state(page);
        assert(s.attribution.includes('© Slots- og Kulturstyrelsen') && s.attributionVisible,
          'credit not back on the second "on": ' + JSON.stringify(s.attribution));
        assert(s.loadedTiles > 0, 'no tiles on the second "on"');
        const errs = rec.consoleErrors.filter(e => !/favicon/i.test(e));
        assert(errs.length === 0, 'console errors: ' + errs.join(' | '));
      } finally { await ctx.close(); }
    });
  }

  // 4 — zoom 11 draws nothing, 12 does
  for (const p of PAGES) {
    await step(`${p.name}: zoom 11 makes no requests and draws nothing; zoom 12 does`, async () => {
      const { ctx, page, rec } = await newCtx({ storage: { 'mc-heritage': '1' } });
      try {
        await gotoMap(page, p.hash, { lat: DK.lat, lon: DK.lon, zoom: 11 });
        let s = await state(page);
        assert(s.checked === true, 'stored on not applied');
        await pan(page, 150, 0);
        s = await state(page);
        assert(rec.heritage.length === 0, 'requests at zoom 11: ' + rec.heritage.length);
        assert(s.tiles === 0, 'tiles at zoom 11: ' + s.tiles);
        await zoomIn(page);
        s = await state(page);
        assert(rec.heritage.length > 0 && s.loadedTiles > 0, 'nothing at zoom 12: ' + JSON.stringify(s));
        checkShape(rec.heritage);
      } finally { await ctx.close(); }
    });
  }

  // 5 — deep link / reload / next page
  await step('?heritage=1 deep link turns it on, survives a reload and carries to Live', async () => {
    const { ctx, page } = await newCtx({});
    try {
      await gotoMap(page, '#/map?heritage=1');
      let s = await state(page);
      assert(s.checked === true && s.stored === '1' && s.loadedTiles > 0, 'deep link not applied: ' + JSON.stringify(s));
      await page.reload({ waitUntil: 'domcontentloaded' });
      await page.waitForSelector(LABEL, { state: 'attached', timeout: 20000 });
      await page.waitForTimeout(900);
      s = await state(page);
      assert(s.checked === true, 'not restored after reload');
      await gotoMap(page, '#/live');
      s = await state(page);
      assert(s.checked === true && s.stored === '1', 'stored choice not applied on Live: ' + JSON.stringify(s));
    } finally { await ctx.close(); }
  });

  await step('?heritage=0 wins over a stored "on"', async () => {
    const { ctx, page, rec } = await newCtx({ storage: { 'mc-heritage': '1' } });
    try {
      await gotoMap(page, '#/live?heritage=0');
      const s = await state(page);
      assert(s.checked === false && s.stored === '0', JSON.stringify(s));
      assert(rec.heritage.length === 0, 'requests although ?heritage=0');
    } finally { await ctx.close(); }
  });

  // 6 — dark theme: not inverted with the base tiles
  for (const p of PAGES) {
    await step(`${p.name} dark theme: the overlay pane is not inverted or blended`, async () => {
      const { ctx, page } = await newCtx({ theme: 'dark', storage: { 'mc-heritage': '1' } });
      try {
        await gotoMap(page, p.hash);
        const s = await state(page);
        assert(s.loadedTiles > 0, 'no tiles');
        assert(s.insideTilePane === false, 'pane sits inside the tile pane');
        assert(s.paneFilter === 'none', 'pane filter ' + s.paneFilter);
        assert(s.paneBlend === 'normal', 'pane blend ' + s.paneBlend);
        console.log(`    ${p.name} dark: tile pane filter "${s.tilePaneFilter}", heritage pane filter "${s.paneFilter}"`);
      } finally { await ctx.close(); }
    });
  }

  // 7 — next to the hillshade
  await step('Map: with the DHM hillshade both are listed and toggle independently', async () => {
    const { ctx, page } = await newCtx({ dhm: true });
    try {
      await gotoMap(page, '#/map');
      const ids = await page.$$eval('.leaflet-control-layers-overlays label', ls => ls.map(l => l.getAttribute('data-overlay-id')));
      assert(JSON.stringify(ids) === JSON.stringify(['dhm-terrain', 'heritage-monuments']), 'overlays ' + ids);
      await setOverlay(page, true, '.leaflet-control-layers-overlays label[data-overlay-id="dhm-terrain"]');
      await setOverlay(page, true);
      let s = await state(page);
      assert(s.zHeritage > s.zTerrain, `heritage ${s.zHeritage} not above terrain ${s.zTerrain}`);
      assert(/terrain=1/.test(s.hash) && /heritage=1/.test(s.hash), 'hash ' + s.hash);
      await setOverlay(page, false);
      s = await state(page);
      assert(/terrain=1/.test(s.hash) && !/heritage=/.test(s.hash), 'hash ' + s.hash);
      assert(s.storedTerrain === '1' && s.stored === '0', JSON.stringify(s));
      assert(s.attribution.includes('Klimadatastyrelsen') && !s.attribution.includes('Slots- og'), 'credits ' + s.attribution);
    } finally { await ctx.close(); }
  });

  // 8 — outside Denmark
  for (const p of PAGES) {
    await step(`${p.name}: outside Denmark — no requests, no console errors`, async () => {
      const { ctx, page, rec } = await newCtx({ storage: { 'mc-heritage': '1' } });
      try {
        await gotoMap(page, p.hash, { lat: 52.52, lon: 13.4, zoom: 13 });   // Berlin
        await pan(page, 300, 200);
        await gotoMap(page, p.hash, { lat: 37.6, lon: -122.2, zoom: 12 });  // fixture area
        await pan(page, -200, 0);
        const s = await state(page);
        assert(s.checked === true, 'overlay should still be on');
        assert(rec.heritage.length === 0, 'requests outside Denmark: ' + rec.heritage.length);
        const errs = rec.consoleErrors.filter(e => !/favicon/i.test(e));
        assert(errs.length === 0, 'console errors: ' + errs.join(' | '));
      } finally { await ctx.close(); }
    });
  }

  if (SHOT_DIR) {
    for (const theme of ['light', 'dark']) {
      for (const p of PAGES) {
        await step(`screenshot ${p.name} ${theme} with the overlay on`, async () => {
          const { ctx, page } = await newCtx({ theme, storage: { 'mc-heritage': '1' }, realTiles: true });
          try {
            await gotoMap(page, p.hash);
            await page.waitForTimeout(1500);
            const file = path.join(SHOT_DIR, `723-${p.name.toLowerCase()}-${theme}.png`);
            await page.screenshot({ path: file });
            console.log('    wrote ' + file);
          } finally { await ctx.close(); }
        });
      }
    }
  }

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch(e => { console.error(e); process.exit(1); });
