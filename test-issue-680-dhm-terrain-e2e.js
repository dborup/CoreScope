/**
 * E2E (#680): optional DHM terrain hillshade overlay on Map and Live.
 *
 * The CI server has no map.dhm config, so the overlay is switched on the
 * way an operator's config would: /api/config/client is intercepted and
 * given the `dhm` block the Go server sends when the proxy is active, and
 * /api/map/dhm/wms is answered with a synthetic hillshade PNG. The real
 * proxy (validation, token, cache, upstream) is covered by
 * cmd/server/dhm_proxy_test.go against a fake upstream.
 *
 * Asserts:
 *   1. not offered without an enabled dhm block (Map + Live), and no
 *      terrain requests at all;
 *   2. offered when enabled, off by default, zero terrain requests while
 *      off — also across pan and zoom;
 *   3. toggling on/off on Map and Live: tiles, attribution (visible, not
 *      under Live's VCR bar), hash, storage;
 *   4. opacity 0.6–0.75, own pane above base tiles and below the overlay
 *      and marker panes, and markers still take the click;
 *   5. hash / stored-preference round-trip across reloads and pages;
 *   6. every browser request to the overlay carries exactly the
 *      allow-listed WMS shape and no token;
 *   7. panning outside Denmark makes no terrain requests and logs no
 *      console errors;
 *   8. request counts on pan / zoom (printed, and bounded).
 *
 * Usage:
 *   BASE_URL=http://localhost:13700 node test-issue-680-dhm-terrain-e2e.js
 * Optional: SCREENSHOT_DIR=<dir> writes light/dark screenshots with the
 * overlay on (those load the real base-map tiles; the overlay stays the
 * synthetic stub).
 */
'use strict';
const { chromium } = require('playwright');
const zlib = require('zlib');
const path = require('path');

const BASE = process.env.BASE_URL || 'http://localhost:13700';
const SHOT_DIR = process.env.SCREENSHOT_DIR || '';
const DHM_PATH = '/api/map/dhm/wms';
const DHM_BLOCK = {
  enabled: true, url: DHM_PATH, layer: 'dhm_terraen_skyggekort',
  minZoom: 7, maxZoom: 19, bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
  attribution: 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0',
};
// Must match tileParams + bbox in internal/dhm/wms.go.
const ALLOWED = {
  service: 'WMS', request: 'GetMap', version: '1.3.0', layers: 'dhm_terraen_skyggekort',
  styles: '', format: 'image/png', transparent: 'true', width: '256', height: '256', crs: 'EPSG:3857',
};
const PNG_1X1 = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg==',
  'base64');

// ── Synthetic hillshade stub ────────────────────────────────────────────────
// Shaded sine "terrain" evaluated in EPSG:3857 metres, so neighbouring tiles
// join up and screenshots look like relief. Not real DHM data.
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
function hillshadePNG(bbox) {
  const [minx, miny, maxx, maxy] = bbox.split(',').map(Number);
  const W = 256, H = 256, raw = Buffer.alloc((W + 1) * H);
  const h = (x, y) => 120 * Math.sin(x / 9000) * Math.cos(y / 7000) + 60 * Math.sin((x + y) / 3100) + 25 * Math.cos(x / 1300 - y / 1700);
  const step = (maxx - minx) / W;
  for (let j = 0; j < H; j++) {
    raw[j * (W + 1)] = 0;
    const y = maxy - (j + 0.5) * (maxy - miny) / H;
    for (let i = 0; i < W; i++) {
      const x = minx + (i + 0.5) * step;
      const d = Math.max(step, 50);
      const dx = (h(x + d, y) - h(x - d, y)) / (2 * d), dy = (h(x, y + d) - h(x, y - d)) / (2 * d);
      const shade = 0.78 + 4 * (-dx * 0.7 + dy * 0.7);
      raw[j * (W + 1) + 1 + i] = Math.max(30, Math.min(250, Math.round(shade * 255)));
    }
  }
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(W, 0); ihdr.writeUInt32BE(H, 4); ihdr[8] = 8; ihdr[9] = 0; ihdr[10] = 0; ihdr[11] = 0; ihdr[12] = 0;
  return Buffer.concat([Buffer.from('\x89PNG\r\n\x1a\n', 'binary'), chunk('IHDR', ihdr), chunk('IDAT', zlib.deflateSync(raw)), chunk('IEND', Buffer.alloc(0))]);
}

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
  console.log(`\n=== #680 DHM terrain overlay against ${BASE} ===\n`);

  // realTiles lets base-map tiles through for the optional screenshots only.
  async function newCtx({ dhm, theme = 'light', storage = {}, viewport = { width: 1280, height: 800 }, realTiles = false }) {
    const ctx = await browser.newContext({ viewport });
    const rec = { terrain: [], consoleErrors: [], failed: [], all: [] };
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      delete config.dhm;
      if (dhm === 'disabled') config.dhm = Object.assign({}, DHM_BLOCK, { enabled: false });
      else if (dhm) config.dhm = DHM_BLOCK;
      await route.fulfill({ response, json: config });
    });
    await ctx.route(url => new URL(url).pathname === DHM_PATH, async (route) => {
      const u = new URL(route.request().url());
      rec.terrain.push(u);
      const bbox = u.searchParams.get('bbox') || '0,0,1,1';
      await route.fulfill({ status: 200, contentType: 'image/png', body: hillshadePNG(bbox) });
    });
    await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, async (route) => {
      if (route.request().resourceType() !== 'image' || realTiles) { await route.continue(); return; }
      await route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 });
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
    page.on('request', r => rec.all.push(r.url()));
    page.on('requestfailed', r => rec.failed.push(r.url()));
    page.on('response', r => { if (r.status() >= 400 && new URL(r.url()).pathname === DHM_PATH) rec.failed.push(r.url()); });
    return { ctx, page, rec };
  }

  const PAGES = [
    { name: 'Map', hash: '#/map' },
    { name: 'Live', hash: '#/live' },
  ];

  async function gotoMap(page, hash, view) {
    // Every map page restores lat/lon/zoom from the hash (#1709).
    const sep = hash.includes('?') ? '&' : '?';
    const v = view || { lat: 56.15, lon: 10.2, zoom: 9 };
    await page.goto(`${BASE}/${hash}${sep}lat=${v.lat}&lon=${v.lon}&zoom=${v.zoom}`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('.leaflet-control-layers', { timeout: 20000 });
    await page.waitForFunction(() => window.MeshConfigReady !== undefined, null, { timeout: 10000 });
    await page.evaluate(() => window.MeshConfigReady).catch(() => {});
    await page.waitForTimeout(700);
  }

  const terrainLabel = '.leaflet-control-layers-overlays label[data-overlay-id="dhm-terrain"]';

  async function terrainCheckbox(page) {
    return page.$(terrainLabel + ' input[type="checkbox"]');
  }
  async function setTerrain(page, on) {
    await page.evaluate(({ sel, on: want }) => {
      const input = document.querySelector(sel + ' input[type="checkbox"]');
      if (!input) throw new Error('no terrain checkbox');
      if (input.checked !== want) input.click();
    }, { sel: terrainLabel, on });
    await page.waitForTimeout(800);
  }
  async function state(page) {
    return page.evaluate(() => {
      const pane = document.querySelector('.mc-terrain-pane');
      const tiles = pane ? Array.from(pane.querySelectorAll('img.leaflet-tile')) : [];
      const layer = pane ? pane.querySelector('.leaflet-layer') : null;
      const z = (sel) => { const el = document.querySelector(sel); return el ? Number(getComputedStyle(el).zIndex) : null; };
      const attr = document.querySelector('.leaflet-control-attribution');
      const input = document.querySelector('.leaflet-control-layers-overlays label[data-overlay-id="dhm-terrain"] input');
      return {
        checked: input ? input.checked : null,
        tiles: tiles.length,
        loadedTiles: tiles.filter(t => t.complete && t.naturalWidth === 256).length,
        opacity: layer ? Number(getComputedStyle(layer).opacity) : null,
        zTerrain: z('.mc-terrain-pane'),
        zTile: z('.leaflet-tile-pane'),
        zOverlay: z('.leaflet-overlay-pane'),
        zMarker: z('.leaflet-marker-pane'),
        pointerEvents: pane ? getComputedStyle(pane).pointerEvents : null,
        attribution: attr ? attr.textContent : '',
        // Live's VCR bar must not cover the credit.
        attributionVisible: (() => {
          if (!attr) return false;
          const r = attr.getBoundingClientRect();
          const hit = document.elementFromPoint(r.x + r.width / 2, r.y + r.height / 2);
          return !!hit && attr.contains(hit);
        })(),
        hash: location.hash,
        stored: localStorage.getItem('mc-terrain-dhm'),
      };
    });
  }

  function checkShape(urls) {
    for (const u of urls) {
      const keys = Array.from(u.searchParams.keys());
      const expectKeys = Object.keys(ALLOWED).concat('bbox');
      assert(keys.length === expectKeys.length, 'param count ' + keys.length + ' in ' + u.search);
      for (const k of keys) assert(expectKeys.includes(k), 'unexpected param ' + k);
      for (const [k, v] of Object.entries(ALLOWED)) assert(u.searchParams.get(k) === v, `${k}=${u.searchParams.get(k)}`);
      assert(!/token/i.test(u.href), 'token in terrain request ' + u.href);
      assert(u.origin === new URL(BASE).origin, 'terrain request left the origin: ' + u.origin);
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

  // 1 — not offered without an enabled dhm block
  for (const [p, dhm] of PAGES.flatMap(pg => [[pg, false], [pg, 'disabled']])) {
    await step(`${p.name}: not offered when the server does not enable it (${dhm ? 'enabled:false' : 'no dhm block'})`, async () => {
      const { ctx, page, rec } = await newCtx({ dhm });
      try {
        await gotoMap(page, p.hash);
        assert(!(await page.$(terrainLabel)), 'terrain overlay offered without dhm config');
        await pan(page, 200, 0);
        assert(rec.terrain.length === 0, 'terrain requests without dhm config: ' + rec.terrain.length);
      } finally { await ctx.close(); }
    });
  }

  // 2/3/4/6/8 — enabled: offered, off by default, toggle on/off
  for (const p of PAGES) {
    await step(`${p.name}: offered, off by default, no requests while off; toggle on/off`, async () => {
      const { ctx, page, rec } = await newCtx({ dhm: true });
      try {
        await gotoMap(page, p.hash);
        assert(await terrainCheckbox(page), 'terrain overlay not offered');
        let s = await state(page);
        assert(s.checked === false && s.tiles === 0, 'not off by default: ' + JSON.stringify(s));
        await pan(page, 250, 0);
        await zoomIn(page);
        assert(rec.terrain.length === 0, `terrain requests while off: ${rec.terrain.length}`);

        await setTerrain(page, true);
        s = await state(page);
        assert(s.checked === true, 'checkbox not checked');
        assert(rec.terrain.length > 0, 'no terrain requests after toggling on');
        assert(s.loadedTiles > 0, 'no loaded terrain tiles: ' + JSON.stringify(s));
        assert(s.opacity >= 0.6 && s.opacity <= 0.75, 'opacity ' + s.opacity);
        assert(s.zTerrain > s.zTile && s.zTerrain < s.zOverlay && s.zTerrain < s.zMarker,
          `z-order tile=${s.zTile} terrain=${s.zTerrain} overlay=${s.zOverlay} marker=${s.zMarker}`);
        assert(s.pointerEvents === 'none', 'terrain pane captures pointer events');
        assert(s.attribution.includes('Klimadatastyrelsen, Danmarks Højdemodel'), 'attribution missing: ' + s.attribution);
        assert(s.attributionVisible, 'attribution is covered by other UI');
        assert(/[?&]terrain=1\b/.test(s.hash), 'hash not updated: ' + s.hash);
        assert(s.stored === '1', 'not stored: ' + s.stored);
        checkShape(rec.terrain);

        const beforePan = rec.terrain.length;
        await pan(page, -300, 0);
        const afterPan = rec.terrain.length;
        await zoomIn(page);
        const afterZoom = rec.terrain.length;
        console.log(`    ${p.name} terrain requests: initial ${beforePan}, +${afterPan - beforePan} on a 300px pan, +${afterZoom - afterPan} on one zoom step`);
        // A 1280x800 viewport is at most 6x5 tiles (+ Leaflet's 2-tile keepBuffer ring).
        assert(beforePan <= 80 && afterPan - beforePan <= 40 && afterZoom - afterPan <= 80, 'unexpectedly many terrain requests');
        checkShape(rec.terrain);

        await setTerrain(page, false);
        s = await state(page);
        assert(s.checked === false && s.tiles === 0, 'tiles left after toggling off: ' + JSON.stringify(s));
        assert(!s.attribution.includes('Klimadatastyrelsen'), 'attribution left after off');
        assert(!/[?&]terrain=/.test(s.hash), 'hash still carries terrain: ' + s.hash);
        assert(s.stored === '0', 'off not stored');
        const offCount = rec.terrain.length;
        await pan(page, 300, 100);
        await zoomIn(page);
        assert(rec.terrain.length === offCount, 'terrain requests after toggling off');
        assert(rec.failed.length === 0, 'failed requests: ' + rec.failed.join(', '));
        if (process.env.DUMP_BBOXES) {
          for (const u of rec.terrain.slice(0, 40)) console.log('BBOX ' + u.searchParams.get('bbox'));
        }
      } finally { await ctx.close(); }
    });
  }

  // 4 — markers stay on top of the overlay (Map page has node markers)
  await step('Map: node markers render above the overlay and still take the click', async () => {
    const { ctx, page } = await newCtx({ dhm: true, storage: { 'mc-terrain-dhm': '1' } });
    try {
      await gotoMap(page, '#/map', { lat: 37.6, lon: -122.2, zoom: 9 });
      // Fixture nodes are in the Bay Area. The overlay is on (stored), so
      // its pane exists; markers must still win every hit-test.
      const r = await page.evaluate(() => {
        const vw = innerWidth, vh = innerHeight;
        const markers = Array.from(document.querySelectorAll('.leaflet-marker-pane .leaflet-marker-icon, .leaflet-overlay-pane path.leaflet-interactive'));
        let inView = 0, onMarker = 0, onTerrain = 0;
        for (const m of markers) {
          const b = m.getBoundingClientRect();
          const cx = b.x + b.width / 2, cy = b.y + b.height / 2;
          if (cx < 0 || cy < 0 || cx >= vw || cy >= vh) continue;
          const hit = document.elementFromPoint(cx, cy);
          if (!hit || hit.closest('.leaflet-control')) continue;
          inView++;
          if (hit.closest('.mc-terrain-pane')) onTerrain++;
          if (hit.closest('.leaflet-marker-pane, .leaflet-overlay-pane')) onMarker++;
        }
        return { none: inView === 0, inView, onMarker, onTerrain, hasPane: !!document.querySelector('.mc-terrain-pane') };
      });
      assert(!r.none, 'no marker found on the map page');
      assert(r.hasPane, 'terrain pane missing although stored on');
      assert(r.onTerrain === 0 && r.onMarker === r.inView, 'marker hit-tests: ' + JSON.stringify(r));
    } finally { await ctx.close(); }
  });

  // 5 — hash and stored-preference round-trip
  await step('?terrain=1 deep link turns it on and is remembered on the next page', async () => {
    const { ctx, page } = await newCtx({ dhm: true });
    try {
      await gotoMap(page, '#/map?terrain=1');
      let s = await state(page);
      assert(s.checked === true && s.stored === '1', 'deep link not applied: ' + JSON.stringify(s));
      await gotoMap(page, '#/live');
      s = await state(page);
      assert(s.checked === true, 'stored choice not applied on Live');
      // Leaving a page tears its map down; that must not save "off".
      assert(s.stored === '1', 'teardown saved off');
      await page.reload({ waitUntil: 'domcontentloaded' });
      await page.waitForSelector(terrainLabel, { state: 'attached', timeout: 20000 });
      await page.waitForTimeout(700);
      s = await state(page);
      assert(s.checked === true, 'not restored after reload');
    } finally { await ctx.close(); }
  });

  await step('?terrain=0 wins over a stored "on"', async () => {
    const { ctx, page, rec } = await newCtx({ dhm: true, storage: { 'mc-terrain-dhm': '1' } });
    try {
      await gotoMap(page, '#/live?terrain=0');
      const s = await state(page);
      assert(s.checked === false && s.stored === '0', JSON.stringify(s));
      assert(rec.terrain.length === 0, 'terrain requests although ?terrain=0');
    } finally { await ctx.close(); }
  });

  // 7 — outside Denmark: no requests, no console errors
  for (const p of PAGES) {
    await step(`${p.name}: outside Denmark — no terrain requests, no console errors`, async () => {
      const { ctx, page, rec } = await newCtx({ dhm: true, storage: { 'mc-terrain-dhm': '1' } });
      try {
        await gotoMap(page, p.hash, { lat: 48.85, lon: 2.35, zoom: 10 });   // Paris
        await pan(page, 300, 200);
        await zoomIn(page);
        await gotoMap(page, p.hash, { lat: 37.6, lon: -122.2, zoom: 9 });   // fixture area
        await pan(page, -200, 0);
        const s = await state(page);
        assert(s.checked === true, 'overlay should still be on');
        assert(rec.terrain.length === 0, 'terrain requests outside Denmark: ' + rec.terrain.length);
        const errs = rec.consoleErrors.filter(e => !/favicon/i.test(e));
        assert(errs.length === 0, 'console errors: ' + errs.join(' | '));
        assert(rec.failed.length === 0, 'failed requests: ' + rec.failed.join(', '));
      } finally { await ctx.close(); }
    });
  }

  // 6 — no token anywhere the browser can see
  await step('no token in any request, the client config or the page', async () => {
    const { ctx, page, rec } = await newCtx({ dhm: true, storage: { 'mc-terrain-dhm': '1' } });
    try {
      await gotoMap(page, '#/map');
      await gotoMap(page, '#/live');
      const leaked = rec.all.filter(u => /[?&]token=/i.test(u));
      assert(leaked.length === 0, 'token parameter in browser requests: ' + leaked.join(', '));
      assert(rec.terrain.length > 0, 'no terrain traffic to check');
      const cfg = await page.evaluate(() => JSON.stringify(window.MC_DHM_CFG));
      assert(!/token/i.test(cfg), 'client dhm config carries a token field: ' + cfg);
      const raw = await page.evaluate(async () => (await fetch('/api/config/client')).text());
      assert(!/"token"/.test(raw), 'client config has a token key');
    } finally { await ctx.close(); }
  });

  if (SHOT_DIR) {
    for (const theme of ['light', 'dark']) {
      for (const p of PAGES) {
        await step(`screenshot ${p.name} ${theme} with the overlay on`, async () => {
          const { ctx, page } = await newCtx({ dhm: true, theme, storage: { 'mc-terrain-dhm': '1' }, realTiles: true });
          try {
            await gotoMap(page, p.hash, { lat: 56.0, lon: 10.0, zoom: 9 });
            await page.waitForTimeout(1200);
            const file = path.join(SHOT_DIR, `680-${p.name.toLowerCase()}-${theme}.png`);
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
