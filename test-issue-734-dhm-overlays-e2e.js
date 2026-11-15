/**
 * E2E (#734 M2): the DHM hillshade pair (terrain / surface, either-or) and
 * the contour-line toggle, with their credits, on Map and Live.
 *
 * Nothing leaves the machine: /api/config/client is intercepted and given
 * the `dhm` block the Go server sends while the proxy is active (with
 * dhm.overlays, cmd/server/dhm_proxy.go), /api/map/dhm/wms is answered with
 * a synthetic tile per overlay id, and every third-party tile is a stub.
 * The real proxy (allow-list, style, zoom range, budget) is covered by
 * cmd/server/dhm_overlay_734_test.go and internal/dhm against fakes.
 *
 * Asserts:
 *   1. Map + Live, light + dark: the three entries; checking surface while
 *      terrain is on unchecks terrain (real Leaflet click), requests switch
 *      from layers=terrain to layers=surface, the hash says
 *      terrain=surface, and it survives a reload; contours is its own
 *      toggle; every request carries exactly the allow-listed keys;
 *   2. contours: no request below z14 even with ?contours=1 (its checkbox
 *      is disabled there), requests from z14, layers=contours;
 *   3. credits: each overlay's credit links its dataset (930/928/926) and
 *      the licence, visible and uncovered, gone with the overlay;
 *   4. a real phone flow at 375x560 (touch, cold start, no deep link):
 *      dismiss the first-visit tips, open the layer control by tapping,
 *      tap the surface hillshade, tap terrain (surface unchecks), zoom in
 *      by tapping + to z14, tap Contour lines; each tapped label is hit by
 *      itself and inside the viewport; then the same at 560x375 landscape.
 *
 * Usage:
 *   BASE_URL=http://localhost:13910 node test-issue-734-dhm-overlays-e2e.js
 * Optional: SCREENSHOT_DIR=<dir> writes light/dark screenshots at z15 with
 * the stubbed tiles, and STYLE_SHOTS=1 adds one per contour colour
 * candidate (white, yellow, red: the hvid/gul/roed styles as stubs).
 */
'use strict';
const { chromium } = require('playwright');
const zlib = require('zlib');
const path = require('path');

const BASE = process.env.BASE_URL || 'http://localhost:13910';
const SHOT_DIR = process.env.SCREENSHOT_DIR || '';
const DHM_PATH = '/api/map/dhm/wms';
const ATTRIBUTION = 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0';
const DATASET = {
  terrain: 'https://dataforsyningen.dk/data/930',
  surface: 'https://dataforsyningen.dk/data/928',
  contours: 'https://dataforsyningen.dk/data/926',
};
const LICENSE_URL = 'https://creativecommons.org/licenses/by/4.0/';
const DHM_BLOCK = {
  enabled: true, url: DHM_PATH, layer: 'dhm_terraen_skyggekort',
  minZoom: 7, maxZoom: 19, bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
  attribution: ATTRIBUTION,
  datasetName: 'Danmarks Højdemodel', datasetUrl: DATASET.terrain,
  licenseName: 'CC BY 4.0', licenseUrl: LICENSE_URL,
  attributionDerived: ATTRIBUTION + ' — data bearbejdet',
  overlays: [
    { id: 'terrain', minZoom: 7, maxZoom: 19, datasetName: 'Danmarks Højdemodel', datasetUrl: DATASET.terrain },
    { id: 'surface', minZoom: 7, maxZoom: 19, datasetName: 'Danmarks Højdemodel', datasetUrl: DATASET.surface },
    { id: 'contours', minZoom: 14, maxZoom: 19, datasetName: 'Danmarks Højdemodel', datasetUrl: DATASET.contours },
  ],
};
// The browser's tile shape (internal/dhm/wms.go tileParams), layers = an id.
const ALLOWED = {
  service: 'WMS', request: 'GetMap', version: '1.3.0',
  styles: '', format: 'image/png', transparent: 'true', width: '256', height: '256', crs: 'EPSG:3857',
};
const LABEL = { terrain: 'dhm-terrain', surface: 'dhm-surface', contours: 'dhm-contours' };

// ── Synthetic tiles (not real DHM data) ─────────────────────────────────────
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
function rgbaPNG(W, H, px) {
  const raw = Buffer.alloc((W * 4 + 1) * H);
  for (let j = 0; j < H; j++) {
    raw[j * (W * 4 + 1)] = 0;
    for (let i = 0; i < W; i++) {
      const [r, g, b, a] = px(i, j);
      const o = j * (W * 4 + 1) + 1 + i * 4;
      raw[o] = r; raw[o + 1] = g; raw[o + 2] = b; raw[o + 3] = a;
    }
  }
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(W, 0); ihdr.writeUInt32BE(H, 4); ihdr[8] = 8; ihdr[9] = 6;
  return Buffer.concat([Buffer.from('\x89PNG\r\n\x1a\n', 'binary'), chunk('IHDR', ihdr), chunk('IDAT', zlib.deflateSync(raw)), chunk('IEND', Buffer.alloc(0))]);
}
// Heights in EPSG:3857 metres, so neighbouring tiles join up.
const height = (x, y) => 40 * Math.sin(x / 900) * Math.cos(y / 700) + 20 * Math.sin((x + y) / 310);
function tilePNG(id, bbox, rgb) {
  const [minx, miny, maxx, maxy] = bbox.split(',').map(Number);
  const sx = (maxx - minx) / 256, sy = (maxy - miny) / 256;
  const at = (i, j) => [minx + (i + 0.5) * sx, maxy - (j + 0.5) * sy];
  if (id === 'contours') {
    // A line where the height crosses a multiple of 2.5 m.
    return rgbaPNG(256, 256, (i, j) => {
      const [x, y] = at(i, j), h = height(x, y) / 2.5;
      const d = Math.abs(h - Math.round(h)), grad = Math.abs(height(x + sx, y) - height(x, y)) / 2.5 + Math.abs(height(x, y + sy) - height(x, y)) / 2.5;
      return d < Math.max(grad, 0.02) * 0.9 ? rgb.concat(255) : [0, 0, 0, 0];
    });
  }
  return rgbaPNG(256, 256, (i, j) => {
    const [x, y] = at(i, j), d = Math.max(sx, 2);
    const dx = (height(x + d, y) - height(x - d, y)) / (2 * d), dy = (height(x, y + d) - height(x, y - d)) / (2 * d);
    let v = Math.max(40, Math.min(250, Math.round((0.78 + 3 * (-dx + dy)) * 255)));
    // The surface stub adds "buildings": a grid of dark blocks.
    if (id === 'surface' && ((Math.floor(x / 60) + Math.floor(y / 60)) % 5 === 0)) v = Math.round(v * 0.55);
    return [v, v, v, 255];
  });
}
const BASE_LIGHT = rgbaPNG(1, 1, () => [242, 239, 233, 255]);
const BASE_DARK = rgbaPNG(1, 1, () => [38, 38, 38, 255]);
const STYLE_RGB = { hvid: [255, 255, 255], gul: [255, 230, 0], roed: [220, 0, 0] };

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
  console.log(`\n=== #734 M2 DHM overlay toggles against ${BASE} ===\n`);

  async function newCtx({ theme = 'light', storage = {}, viewport = { width: 1280, height: 800 }, phone = false, contourRGB = STYLE_RGB.roed }) {
    const ctx = await browser.newContext(phone
      ? { viewport, isMobile: true, hasTouch: true, deviceScaleFactor: 2 }
      : { viewport });
    const rec = { tiles: [], consoleErrors: [], failed: [], all: [] };
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      config.dhm = DHM_BLOCK;
      await route.fulfill({ response, json: config });
    });
    await ctx.route(url => new URL(url).pathname === DHM_PATH, async (route) => {
      const u = new URL(route.request().url());
      rec.tiles.push(u);
      const id = u.searchParams.get('layers');
      await route.fulfill({ status: 200, contentType: 'image/png', body: tilePNG(id, u.searchParams.get('bbox') || '0,0,1,1', contourRGB) });
    });
    // Third-party base tiles never leave the machine: a flat stand-in in a
    // base map's land colour, dark for the dark styles (the light ones are
    // inverted by the page's own filter in the dark theme).
    await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, async (route) => {
      if (route.request().resourceType() !== 'image') { await route.continue(); return; }
      await route.fulfill({ status: 200, contentType: 'image/png', body: /dark_all/.test(route.request().url()) ? BASE_DARK : BASE_LIGHT });
    });
    if (!phone) {
      await ctx.addInitScript(({ theme: t, storage: st }) => {
        try {
          localStorage.setItem('meshcore-theme', t);
          for (const k of Object.keys(st)) localStorage.setItem(k, st[k]);
        } catch (_) {}
        const apply = () => document.documentElement && document.documentElement.setAttribute('data-theme', t);
        apply();
        document.addEventListener('DOMContentLoaded', apply);
      }, { theme, storage });
    }
    const page = await ctx.newPage();
    page.on('console', m => { if (m.type() === 'error') rec.consoleErrors.push(m.text()); });
    page.on('pageerror', e => rec.consoleErrors.push(String(e)));
    page.on('request', r => rec.all.push(r.url()));
    page.on('response', r => { if (r.status() >= 400 && new URL(r.url()).pathname === DHM_PATH) rec.failed.push(r.url()); });
    return { ctx, page, rec };
  }

  const PAGES = [{ name: 'Map', hash: '#/map' }, { name: 'Live', hash: '#/live' }];
  const AARHUS = { lat: 56.16, lon: 10.2 };

  async function gotoMap(page, hash, zoom) {
    const sep = hash.includes('?') ? '&' : '?';
    await page.goto(`${BASE}/${hash}${sep}lat=${AARHUS.lat}&lon=${AARHUS.lon}&zoom=${zoom}`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('.leaflet-control-layers-overlays label[data-overlay-id="dhm-contours"]', { state: 'attached', timeout: 20000 });
    await page.waitForTimeout(900);
  }
  const ids = (rec, from) => rec.tiles.slice(from || 0).map(u => u.searchParams.get('layers'));
  const count = (rec, id, from) => ids(rec, from).filter(x => x === id).length;

  async function state(page) {
    return page.evaluate((LABEL) => {
      const out = { checked: {}, disabled: {}, hash: location.hash, stored: localStorage.getItem('mc-terrain-dhm'), storedContours: localStorage.getItem('mc-contours-dhm') };
      for (const [id, dataId] of Object.entries(LABEL)) {
        const input = document.querySelector(`.leaflet-control-layers-overlays label[data-overlay-id="${dataId}"] input`);
        out.checked[id] = input ? input.checked : null;
        out.disabled[id] = input ? input.disabled : null;
      }
      const tiles = (cls) => Array.from(document.querySelectorAll(`img.leaflet-tile.${cls}, .${cls} img.leaflet-tile`)).filter(t => t.complete && t.naturalWidth === 256).length;
      out.tiles = { terrain: tiles('mc-terrain-tiles'), surface: tiles('mc-surface-tiles'), contours: tiles('mc-contour-tiles') };
      const attr = document.querySelector('.leaflet-control-attribution');
      out.credit = attr ? attr.textContent : '';
      out.links = attr ? Array.from(attr.querySelectorAll('a')).map(a => ({ href: a.getAttribute('href'), rel: a.getAttribute('rel'), target: a.getAttribute('target'), text: a.textContent })) : [];
      out.creditVisible = (() => {
        if (!attr) return false;
        const r = attr.getBoundingClientRect();
        const hit = document.elementFromPoint(r.x + r.width / 2, r.y + r.height / 2);
        return r.width > 0 && r.bottom <= innerHeight && !!hit && attr.contains(hit);
      })();
      const z = (sel) => { const el = document.querySelector(sel); return el ? Number(getComputedStyle(el).zIndex) : null; };
      out.z = { hillshade: z('.mc-terrain-pane'), contours: z('.mc-contour-pane'), tiles: z('.leaflet-tile-pane'), overlay: z('.leaflet-overlay-pane') };
      return out;
    }, LABEL);
  }
  async function click(page, id) {
    await page.evaluate((dataId) => {
      const input = document.querySelector(`.leaflet-control-layers-overlays label[data-overlay-id="${dataId}"] input`);
      if (!input) throw new Error('no input for ' + dataId);
      input.click();
    }, LABEL[id]);
    await page.waitForTimeout(900);
  }
  function checkShape(urls) {
    for (const u of urls) {
      const keys = Array.from(u.searchParams.keys()).sort();
      const want = Object.keys(ALLOWED).concat('bbox', 'layers').sort();
      assert(JSON.stringify(keys) === JSON.stringify(want), 'keys ' + keys.join(','));
      for (const [k, v] of Object.entries(ALLOWED)) assert(u.searchParams.get(k) === v, `${k}=${u.searchParams.get(k)}`);
      assert(['terrain', 'surface', 'contours'].includes(u.searchParams.get('layers')), 'layers=' + u.searchParams.get('layers'));
      assert(!/token/i.test(u.href), 'token in ' + u.href);
      assert(u.origin === new URL(BASE).origin, 'left the origin: ' + u.origin);
    }
  }
  function creditLinks(s, wantIds) {
    const datasets = s.links.filter(l => l.text === 'Danmarks Højdemodel').map(l => l.href).sort();
    assert(JSON.stringify(datasets) === JSON.stringify(wantIds.map(id => DATASET[id]).sort()), 'dataset links ' + JSON.stringify(datasets));
    const licences = s.links.filter(l => l.text === 'CC BY 4.0');
    assert(licences.length === wantIds.length && licences.every(l => l.href === LICENSE_URL && /\blicense\b/.test(l.rel)), 'licence links ' + JSON.stringify(licences));
    for (const l of s.links.filter(l => l.text === 'Danmarks Højdemodel' || l.text === 'CC BY 4.0')) {
      assert(l.target === '_blank' && /noopener/.test(l.rel), 'link attrs ' + JSON.stringify(l));
    }
    assert(!/bearbejdet/.test(s.credit), 'derived note in the overlay credit');
    if (wantIds.length) assert(s.creditVisible, 'credit covered or off-screen');
  }

  // 1 + 3 — the pair, contours, credits; Map and Live, light and dark
  for (const theme of ['light', 'dark']) {
    for (const p of PAGES) {
      await step(`${p.name} ${theme}: terrain → surface is either-or, contours independent, credits per overlay`, async () => {
        const { ctx, page, rec } = await newCtx({ theme });
        try {
          await gotoMap(page, p.hash, 15);
          let s = await state(page);
          assert(Object.values(s.checked).every(c => c === false), 'not all off by default: ' + JSON.stringify(s.checked));
          assert(rec.tiles.length === 0, 'tile requests while all off: ' + rec.tiles.length);
          creditLinks(s, []);

          await click(page, 'terrain');
          s = await state(page);
          assert(s.checked.terrain && !s.checked.surface && s.tiles.terrain > 0, 'terrain on: ' + JSON.stringify(s));
          assert(/[?&]terrain=1\b/.test(s.hash) && s.stored === '1', 'terrain hash/stored: ' + s.hash + ' ' + s.stored);
          creditLinks(s, ['terrain']);
          const n0 = rec.tiles.length;
          assert(count(rec, 'terrain') === n0 && n0 > 0, 'terrain requests ' + ids(rec).join(','));

          await click(page, 'surface');
          s = await state(page);
          assert(s.checked.surface && s.checked.terrain === false, 'checking surface left terrain checked: ' + JSON.stringify(s.checked));
          assert(s.tiles.surface > 0 && s.tiles.terrain === 0, 'tiles after the switch: ' + JSON.stringify(s.tiles));
          assert(count(rec, 'surface', n0) > 0 && count(rec, 'terrain', n0) === 0, 'requests after the switch: ' + ids(rec, n0).join(','));
          assert(/[?&]terrain=surface\b/.test(s.hash) && s.stored === 'surface', 'surface hash/stored: ' + s.hash + ' ' + s.stored);
          creditLinks(s, ['surface']);

          await click(page, 'contours');
          s = await state(page);
          assert(s.checked.surface && s.checked.contours && s.tiles.contours > 0, 'contours on: ' + JSON.stringify(s));
          assert(/[?&]contours=1\b/.test(s.hash) && s.storedContours === '1', 'contours hash: ' + s.hash);
          assert(s.z.contours > s.z.hillshade && s.z.contours < s.z.overlay && s.z.hillshade > s.z.tiles, 'z-order ' + JSON.stringify(s.z));
          creditLinks(s, ['surface', 'contours']);
          checkShape(rec.tiles);

          await page.reload({ waitUntil: 'domcontentloaded' });
          await page.waitForSelector('.leaflet-control-layers-overlays label[data-overlay-id="dhm-contours"]', { state: 'attached', timeout: 20000 });
          await page.waitForTimeout(900);
          s = await state(page);
          assert(s.checked.surface && !s.checked.terrain && s.checked.contours, 'not restored after reload: ' + JSON.stringify(s.checked));

          await click(page, 'surface');
          await click(page, 'contours');
          s = await state(page);
          assert(Object.values(s.checked).every(c => c === false), 'off: ' + JSON.stringify(s.checked));
          assert(!/[?&](terrain|contours)=/.test(s.hash), 'hash after off: ' + s.hash);
          assert(s.stored === '0' && s.storedContours === '0', 'stored after off');
          creditLinks(s, []);
          const errs = rec.consoleErrors.filter(e => !/favicon/i.test(e));
          assert(errs.length === 0, 'console errors: ' + errs.join(' | '));
          assert(rec.failed.length === 0, 'failed: ' + rec.failed.join(', '));
        } finally { await ctx.close(); }
      });
    }
  }

  // 2 — contours: nothing below z14
  await step('Map: ?contours=1 at z12 asks for no contour tile; zooming to z14 does', async () => {
    const { ctx, page, rec } = await newCtx({});
    try {
      await gotoMap(page, '#/map?contours=1&terrain=1', 12);
      let s = await state(page);
      assert(s.checked.contours && s.disabled.contours, 'contours at z12: ' + JSON.stringify({ c: s.checked, d: s.disabled }));
      assert(count(rec, 'contours') === 0 && count(rec, 'terrain') > 0, 'requests at z12: ' + ids(rec).join(','));
      await page.click('.leaflet-control-zoom-in'); await page.waitForTimeout(900);
      assert(count(rec, 'contours') === 0, 'contour requests at z13');
      await page.click('.leaflet-control-zoom-in'); await page.waitForTimeout(1200);
      s = await state(page);
      assert(!s.disabled.contours && s.tiles.contours > 0 && count(rec, 'contours') > 0, 'no contours at z14: ' + JSON.stringify(s.tiles));
      checkShape(rec.tiles);
    } finally { await ctx.close(); }
  });

  await step('Live: ?terrain=surface&contours=1 deep link, both credits, terrain=1 still means terrain', async () => {
    const { ctx, page, rec } = await newCtx({});
    try {
      await gotoMap(page, '#/live?terrain=surface&contours=1', 15);
      let s = await state(page);
      assert(s.checked.surface && s.checked.contours && !s.checked.terrain, JSON.stringify(s.checked));
      creditLinks(s, ['surface', 'contours']);
      await gotoMap(page, '#/live?terrain=1', 15);
      s = await state(page);
      assert(s.checked.terrain && !s.checked.surface && s.stored === '1', '?terrain=1: ' + JSON.stringify(s.checked));
      assert(rec.tiles.length > 0);
    } finally { await ctx.close(); }
  });

  // 4 — real phone flow
  async function tapLabel(page, id) {
    const sel = `.leaflet-control-layers-overlays label[data-overlay-id="${LABEL[id]}"]`;
    const el = await page.$(sel);
    assert(el, 'no label ' + id);
    await el.scrollIntoViewIfNeeded();
    const hit = await page.evaluate((sel) => {
      const l = document.querySelector(sel), r = l.getBoundingClientRect();
      const x = r.x + Math.min(r.width / 2, 60), y = r.y + r.height / 2;
      const h = document.elementFromPoint(x, y);
      return { ok: !!h && l.contains(h), inView: r.top >= 0 && r.bottom <= innerHeight && r.left >= 0 && r.right <= innerWidth, x, y, h: r.height };
    }, sel);
    assert(hit.ok, id + ': its label is covered at the tap point');
    assert(hit.inView, id + ': label outside the viewport');
    await page.touchscreen.tap(hit.x, hit.y);
    await page.waitForTimeout(900);
    return hit;
  }
  // The first-visit gesture tips (public/gesture-hints.js) appear ~800 ms
  // after the page settles and cover the map controls: a person taps
  // "Got it" first.
  async function dismissTips(page) {
    for (let i = 0; i < 4; i++) {
      const b = await page.waitForSelector('.gesture-hint-dismiss', { state: 'visible', timeout: 3000 }).catch(() => null);
      if (!b) break;
      await b.tap();
      await page.waitForTimeout(400);
    }
  }
  // A tap on the map itself, clear of every control (an expanded layer
  // control covers most of a phone's width).
  async function tapMapBlank(page) {
    const pt = await page.evaluate(() => {
      const r = document.querySelector('.leaflet-container').getBoundingClientRect();
      for (let fy = 0.85; fy > 0.15; fy -= 0.05) {
        for (let fx = 0.95; fx > 0.05; fx -= 0.05) {
          const x = r.x + r.width * fx, y = r.y + r.height * fy;
          const h = document.elementFromPoint(x, y);
          if (h && h.closest('.leaflet-container') && !h.closest('.leaflet-control, .leaflet-marker-icon, .leaflet-interactive')) return { x, y };
        }
      }
      return null;
    });
    assert(pt, 'no blank map point to tap');
    await page.touchscreen.tap(pt.x, pt.y);
    await page.waitForTimeout(400);
  }
  async function openControl(page) {
    const t = await page.$('.leaflet-control-layers-toggle');
    if (t && await t.isVisible()) await t.tap();
    await page.waitForTimeout(600);
    assert(await page.$eval('.leaflet-control-layers', el => el.classList.contains('leaflet-control-layers-expanded')), 'layer control did not open on tap');
  }

  for (const viewport of [{ width: 375, height: 560 }, { width: 560, height: 375 }]) {
    await step(`phone ${viewport.width}x${viewport.height}: cold start, tap surface, tap terrain, zoom to 14, tap contours`, async () => {
      const { ctx, page, rec } = await newCtx({ viewport, phone: true });
      try {
        await page.goto(`${BASE}/#/map`, { waitUntil: 'domcontentloaded' });
        await page.waitForSelector('.leaflet-control-layers', { timeout: 20000 });
        await page.waitForTimeout(1200);
        await dismissTips(page);
        // Pan to Denmark the way a person does: the search for a place is
        // not part of this feature, so set the view once through the
        // address bar, then use only taps.
        await page.goto(`${BASE}/#/map?lat=${AARHUS.lat}&lon=${AARHUS.lon}&zoom=11`, { waitUntil: 'domcontentloaded' });
        await page.waitForSelector('.leaflet-control-layers-overlays label[data-overlay-id="dhm-contours"]', { state: 'attached', timeout: 20000 });
        await page.waitForTimeout(900);
        await dismissTips(page);

        await openControl(page);
        await tapLabel(page, 'surface');
        let s = await state(page);
        assert(s.checked.surface && !s.checked.terrain, 'tap surface: ' + JSON.stringify(s.checked));
        if (!(await page.$eval('.leaflet-control-layers', el => el.classList.contains('leaflet-control-layers-expanded')))) await openControl(page);
        await tapLabel(page, 'terrain');
        s = await state(page);
        assert(s.checked.terrain && !s.checked.surface, 'tap terrain: ' + JSON.stringify(s.checked));
        assert(/[?&]terrain=1\b/.test(s.hash), 'hash ' + s.hash);
        assert(s.disabled.contours, 'contours enabled below z14');

        // Close the control by tapping the map, then zoom in with the + button.
        await tapMapBlank(page);
        await page.waitForTimeout(400);
        for (let i = 0; i < 3; i++) { await page.tap('.leaflet-control-zoom-in'); await page.waitForTimeout(700); }
        // The Map page saves its view on moveend (public/map.js map-view).
        const zoom = await page.evaluate(() => { try { return JSON.parse(localStorage.getItem('map-view')).zoom; } catch (_) { return null; } });
        assert(zoom === 14, 'zoom after three taps: ' + zoom);
        await openControl(page);
        await tapLabel(page, 'contours');
        s = await state(page);
        assert(s.checked.terrain && s.checked.contours && !s.checked.surface, 'tap contours: ' + JSON.stringify(s.checked));
        await page.waitForTimeout(800);
        s = await state(page);
        assert(s.tiles.contours > 0 && count(rec, 'contours') > 0, 'no contour tiles at z14');
        // The credit: both, uncovered, inside the viewport.
        await tapMapBlank(page);
        await page.waitForTimeout(500);
        s = await state(page);
        creditLinks(s, ['terrain', 'contours']);
        const pageScrollX = await page.evaluate(() => document.documentElement.scrollWidth - innerWidth);
        assert(pageScrollX <= 0, 'horizontal page scroll ' + pageScrollX);
        checkShape(rec.tiles);
        if (SHOT_DIR) {
          const file = path.join(SHOT_DIR, `734-phone-${viewport.width}x${viewport.height}.png`);
          await page.screenshot({ path: file });
          await openControl(page);
          await page.screenshot({ path: file.replace('.png', '-control.png') });
          console.log('    wrote ' + file);
        }
      } finally { await ctx.close(); }
    });
  }

  if (SHOT_DIR) {
    const styles = process.env.STYLE_SHOTS ? Object.keys(STYLE_RGB) : ['roed'];
    for (const theme of ['light', 'dark']) {
      for (const style of styles) {
        await step(`screenshot Map ${theme}, terrain + contours (${style} stub) at z15`, async () => {
          const { ctx, page } = await newCtx({ theme, contourRGB: STYLE_RGB[style], storage: { 'mc-terrain-dhm': '1', 'mc-contours-dhm': '1' } });
          try {
            await gotoMap(page, '#/map', 15);
            await page.waitForTimeout(1200);
            const file = path.join(SHOT_DIR, `734-map-${theme}-contours-${style}.png`);
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
