/**
 * E2E (#792): the colour-by-elevation DHM overlay on the Map page.
 *
 * Nothing leaves the machine: /api/config/client is intercepted and given
 * the `dhm` block with dhm.overlays as cmd/server/dhm_proxy.go sends it
 * since #792, every elevation tile is test-fixtures/dhm-elevation-792.png
 * (a tile the Go proxy rendered from a fake WCS raster; cmd/server's
 * TestDHM792_E2ETileFixtureIsARenderedTile keeps it so), the other DHM
 * tiles are a synthetic 256x256 PNG and third-party base tiles are stubs.
 *
 * Asserts:
 *   1. a phone at 375x560, cold start (no storage), real taps: the toggle
 *      is listed under the contours; its legend is hidden while it is off;
 *      one tap turns it on, asks for layers=elevation tiles at the view's
 *      zoom, writes ?elevation=1 and remembers it; the legend then sits
 *      under the toggle with its bar drawn from the CSS variables and its
 *      metres, inside the screen; the credit
 *      says "data bearbejdet"; nothing overflows sideways;
 *   2. two taps on + (z12 → z14): no elevation tile above z13 is asked
 *      for, and the z13 tiles stay on screen (upscaled);
 *   3. a reload keeps it on; a tap turns it off, hides the legend and
 *      drops the parameter;
 *   3b. #831 F1 — 375x560, cold start (no storage), taps only, zoomed to
 *      z14 with +: "Contour lines" is inside the layer list's visible box
 *      without scrolling it, and one tap turns it on, both with elevation
 *      off and with it on (its legend showing);
 *   4. the customizer's opacity reaches the layer; ?elevation=0 wins over
 *      the stored choice;
 *   5. no elevation toggle without the entry (a server before #792);
 *   6. no console errors.
 *
 * Usage:
 *   BASE_URL=http://localhost:13700 node test-issue-792-elevation-overlay-e2e.js
 */
'use strict';
const fs = require('fs');
const path = require('path');
const zlib = require('zlib');
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13700';
const DHM_PATH = '/api/map/dhm/wms';
const STORE = 'cs-theme-overrides';
const ATTRIBUTION = 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0';
const OVERLAYS_734 = [
  { id: 'terrain', minZoom: 7, maxZoom: 19, datasetName: 'Danmarks Højdemodel', datasetUrl: 'https://dataforsyningen.dk/data/930' },
  { id: 'surface', minZoom: 7, maxZoom: 19, datasetName: 'Danmarks Højdemodel', datasetUrl: 'https://dataforsyningen.dk/data/928' },
  { id: 'contours', minZoom: 14, maxZoom: 19, datasetName: 'Danmarks Højdemodel', datasetUrl: 'https://dataforsyningen.dk/data/926' },
];
const ELEVATION = {
  id: 'elevation', minZoom: 9, maxZoom: 13, datasetName: 'Danmarks Højdemodel', datasetUrl: 'https://dataforsyningen.dk/data/930',
  // #860: the stops tuned to Danish land and the ramp id (dhm.ElevationStyle).
  attribution: ATTRIBUTION + ' — data bearbejdet', legendM: [0, 2, 5, 10, 15, 20, 25, 30, 40, 50, 65, 80, 100, 130, 175],
  style: 'ramp-471e5e7c',
};
function dhmBlock(overlays) {
  return {
    enabled: true, url: DHM_PATH, layer: 'dhm_terraen_skyggekort',
    minZoom: 7, maxZoom: 19, bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
    attribution: ATTRIBUTION,
    datasetName: 'Danmarks Højdemodel', datasetUrl: 'https://dataforsyningen.dk/data/930',
    licenseName: 'CC BY 4.0', licenseUrl: 'https://creativecommons.org/licenses/by/4.0/',
    attributionDerived: ATTRIBUTION + ' — data bearbejdet',
    overlays,
  };
}
const DHM_BLOCK = dhmBlock(OVERLAYS_734.concat([ELEVATION]));
const AARHUS = { lat: 56.16, lon: 10.2 };
const ELEVATION_TILE = fs.readFileSync(path.join(__dirname, 'test-fixtures', 'dhm-elevation-792.png'));

// ── A synthetic 256x256 PNG (a real tile size, so no placeholder retry) ─────
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
function rgbaPNG(W, H, rgba) {
  const row = Buffer.alloc(W * 4 + 1);
  for (let i = 0; i < W; i++) row.set(rgba, 1 + i * 4);
  const raw = Buffer.concat(Array.from({ length: H }, () => row));
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(W, 0); ihdr.writeUInt32BE(H, 4); ihdr[8] = 8; ihdr[9] = 6;
  return Buffer.concat([Buffer.from('\x89PNG\r\n\x1a\n', 'binary'), chunk('IHDR', ihdr), chunk('IDAT', zlib.deflateSync(raw)), chunk('IEND', Buffer.alloc(0))]);
}
const DHM_TILE = rgbaPNG(256, 256, [128, 128, 128, 90]);
const BASE_TILE = rgbaPNG(1, 1, [242, 239, 233, 255]);

// The zoom of a tile request, from its EPSG:3857 bbox width.
function bboxZoom(u) {
  const b = (u.searchParams.get('bbox') || '').split(',').map(Number);
  return Math.round(Math.log2(2 * 20037508.342789244 / (b[2] - b[0])));
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
  console.log(`\n=== #792 colour-by-elevation overlay against ${BASE} ===\n`);

  async function newCtx({ phone = false, viewport = { width: 1280, height: 800 }, dhm = DHM_BLOCK, storage = null } = {}) {
    const ctx = await browser.newContext(phone
      ? { viewport, isMobile: true, hasTouch: true, deviceScaleFactor: 2 }
      : { viewport });
    const rec = { tiles: [], consoleErrors: [] };
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      if (dhm) config.dhm = dhm; else delete config.dhm;
      await route.fulfill({ response, json: config });
    });
    await ctx.route(url => new URL(url).pathname === DHM_PATH, async (route) => {
      const u = new URL(route.request().url());
      rec.tiles.push(u);
      const body = u.searchParams.get('layers') === 'elevation' ? ELEVATION_TILE : DHM_TILE;
      await route.fulfill({ status: 200, contentType: 'image/png', body });
    });
    await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, async (route) => {
      if (route.request().resourceType() !== 'image') { await route.continue(); return; }
      await route.fulfill({ status: 200, contentType: 'image/png', body: BASE_TILE });
    });
    if (storage) {
      await ctx.addInitScript((st) => {
        try { if (!sessionStorage.getItem('__seeded')) { for (const k of Object.keys(st)) localStorage.setItem(k, st[k]); sessionStorage.setItem('__seeded', '1'); } } catch (_) {}
      }, storage);
    }
    const page = await ctx.newPage();
    page.on('console', m => { if (m.type() === 'error') rec.consoleErrors.push(m.text()); });
    page.on('pageerror', e => rec.consoleErrors.push(String(e)));
    return { ctx, page, rec };
  }

  const elevationTiles = (rec, from) => rec.tiles.slice(from || 0).filter(u => u.searchParams.get('layers') === 'elevation');
  const SEL = '.leaflet-control-layers-overlays label[data-overlay-id="dhm-elevation"]';
  async function state(page) {
    return page.evaluate((SEL) => {
      const ids = Array.from(document.querySelectorAll('.leaflet-control-layers-overlays label[data-overlay-id]')).map(l => l.getAttribute('data-overlay-id'));
      const label = document.querySelector(SEL);
      const input = label && label.querySelector('input');
      const pane = document.querySelector('.leaflet-pane.mc-elevation-pane');
      const imgs = pane ? Array.from(pane.querySelectorAll('img.leaflet-tile-loaded')) : [];
      const credit = document.querySelector('.leaflet-control-attribution');
      return {
        // #845: the row's name and its zoom hint, shown apart at the right.
        ids, label: label ? (label.querySelector('.mc-layer-name') || label).textContent.trim() +
          (label.querySelector('.mc-layer-hint') ? ' | ' + label.querySelector('.mc-layer-hint').textContent.trim() : '') : null,
        checked: input ? input.checked : null, disabled: input ? input.disabled : null,
        hash: location.hash, stored: localStorage.getItem('mc-elevation-dhm'),
        paneZ: pane ? getComputedStyle(pane).zIndex : null,
        tilesShown: imgs.filter(i => { const r = i.getBoundingClientRect(); return r.right > 0 && r.bottom > 0 && r.left < innerWidth && r.top < innerHeight; }).length,
        layerOpacity: (() => { const c = pane && pane.querySelector('.leaflet-layer'); return c ? getComputedStyle(c).opacity : null; })(),
        credit: credit ? credit.textContent : '',
        // map.js saves the view on every move (map-view).
        zoom: (() => { try { return JSON.parse(localStorage.getItem('map-view') || '{}').zoom; } catch (_) { return null; } })(),
      };
    }, SEL);
  }
  // Whether the legend under the toggle is hidden (null: no legend).
  async function legendHidden(page) {
    return page.evaluate((SEL) => {
      const label = document.querySelector(SEL);
      const box = label && label.nextElementSibling;
      if (!box || !box.classList.contains('mc-elev-legend')) return null;
      return box.hidden && getComputedStyle(box).display === 'none';
    }, SEL);
  }
  async function waitMap(page) {
    await page.waitForSelector('.leaflet-control-layers-overlays label[data-overlay-id="dhm-terrain"]', { state: 'attached', timeout: 20000 });
    await page.waitForFunction(() => window._customizerV2 && window._customizerV2.initDone, null, { timeout: 15000 });
    await page.waitForTimeout(900);
  }
  async function dismissTips(page) {
    for (let i = 0; i < 4; i++) {
      const b = await page.waitForSelector('.gesture-hint-dismiss', { state: 'visible', timeout: 3000 }).catch(() => null);
      if (!b) break;
      await b.tap();
      await page.waitForTimeout(400);
    }
  }
  async function openLayers(page) {
    const t = await page.$('.leaflet-control-layers-toggle');
    if (t && await t.isVisible()) { await t.tap(); await page.waitForTimeout(600); }
  }
  // Taps the elevation toggle where a finger would: on its label, which
  // must be on screen and not covered.
  async function tapToggle(page, vp) {
    await openLayers(page);
    await page.locator(SEL).scrollIntoViewIfNeeded();
    const hit = await page.evaluate((SEL) => {
      const l = document.querySelector(SEL), r = l.getBoundingClientRect();
      const x = r.x + Math.min(r.width / 2, 60), y = r.y + r.height / 2;
      const h = document.elementFromPoint(x, y);
      return { ok: !!h && l.contains(h), x, y, r: { top: r.top, bottom: r.bottom, left: r.left, right: r.right } };
    }, SEL);
    assert(hit.ok, 'the elevation label is covered at the tap point');
    if (vp) assert(hit.r.top >= 0 && hit.r.bottom <= vp.height + 1 && hit.r.left >= 0 && hit.r.right <= vp.width + 1, 'toggle off screen ' + JSON.stringify(hit.r));
    await page.touchscreen.tap(hit.x, hit.y);
    await page.waitForTimeout(1200);
  }

  // ── 1-3. Phone, cold start, real taps ───────────────────────────────────
  {
    const vp = { width: 375, height: 560 };
    const { ctx, page, rec } = await newCtx({ phone: true, viewport: vp });
    try {
      await step('375x560 cold start: one tap turns it on; legend, credit, link and storage', async () => {
        await page.goto(`${BASE}/#/map`, { waitUntil: 'domcontentloaded' });
        await page.waitForSelector('.leaflet-control-layers', { timeout: 20000 });
        await page.waitForTimeout(1200);
        await dismissTips(page);
        // Denmark, once through the address bar (place search is not this
        // feature); from here on only taps.
        await page.goto(`${BASE}/#/map?lat=${AARHUS.lat}&lon=${AARHUS.lon}&zoom=12`, { waitUntil: 'domcontentloaded' });
        await waitMap(page);
        await dismissTips(page);
        let s = await state(page);
        assert(s.ids.join(',') === 'dhm-terrain,dhm-surface,dhm-contours,dhm-elevation', 'order ' + s.ids.join(','));
        assert(await legendHidden(page) === true, 'the legend shows while the overlay is off');
        assert(s.label === 'Colour by elevation | zoom 9+', s.label);
        assert(s.checked === false, 'on before any tap');
        assert(elevationTiles(rec).length === 0, 'asked for elevation tiles while off');

        await tapToggle(page, vp);
        s = await state(page);
        assert(s.checked === true, 'not on after a tap');
        const tiles = elevationTiles(rec);
        assert(tiles.length > 0, 'no elevation tiles');
        assert(tiles.every(u => bboxZoom(u) === 12), 'zooms ' + tiles.map(bboxZoom).join(','));
        for (const k of ['service', 'request', 'version', 'styles', 'format', 'transparent', 'width', 'height', 'crs', 'layers', 'bbox']) {
          assert(tiles[0].searchParams.has(k), 'request lacks ' + k);
        }
        assert(Array.from(tiles[0].searchParams.keys()).length === 11, 'extra request keys: ' + tiles[0].search);
        // #860: the ramp id is the tile's styles, so a new ramp is a new URL.
        assert(tiles.every(u => u.searchParams.get('styles') === ELEVATION.style), 'styles ' + tiles[0].searchParams.get('styles'));
        assert(/[?&]elevation=1\b/.test(s.hash), 'hash ' + s.hash);
        assert(s.stored === '1', 'stored ' + s.stored);
        assert(s.paneZ === '245', 'pane z ' + s.paneZ);
        assert(s.tilesShown > 0, 'no elevation tile on screen');
        assert(/data bearbejdet/.test(s.credit), 'credit ' + s.credit);

        const legend = await page.evaluate((SEL) => {
          const label = document.querySelector(SEL);
          const box = label && label.nextElementSibling;
          if (!box || !box.classList.contains('mc-elev-legend')) return null;
          const bar = box.querySelector('.mc-elev-legend-bar');
          const r = box.getBoundingClientRect();
          const root = getComputedStyle(document.documentElement);
          const rect = (e) => { const q = e.getBoundingClientRect(); return { left: q.left, right: q.right, top: q.top, bottom: q.bottom }; };
          return {
            text: Array.from(box.querySelectorAll('.mc-elev-legend-scale span')).map(s => s.textContent),
            // #860: where each label is drawn, and what is drawn on top of it.
            spans: Array.from(box.querySelectorAll('.mc-elev-legend-scale span')).map(s => {
              const q = rect(s);
              const top = document.elementFromPoint((q.left + q.right) / 2, (q.top + q.bottom) / 2);
              return Object.assign(q, { visible: !!top && (top === s || s.contains(top)) });
            }),
            barR: rect(bar),
            image: getComputedStyle(bar).backgroundImage,
            first: root.getPropertyValue('--dhm-elevation-0').trim(),
            last: root.getPropertyValue('--dhm-elevation-175').trim(),
            barH: bar.getBoundingClientRect().height,
            r: { top: r.top, bottom: r.bottom, left: r.left, right: r.right },
            aria: box.getAttribute('aria-label'),
          };
        }, SEL);
        assert(legend, 'no legend under the toggle');
        assert(await legendHidden(page) === false, 'the legend is hidden while the overlay is on');
        assert(legend.text.join('|') === '0|5|15|25|40|65|175 m', 'legend ' + legend.text.join('|'));
        assert(legend.first === '#007974' && legend.last === '#642560', 'variables ' + legend.first + ' ' + legend.last);
        // The bar is the variables resolved, one equal step per stop (#860):
        // rgb(0, 121, 116) 0%, rgb(31, 155, 130) 7.14…%, … rgb(100, 37, 96) 100%.
        assert(/^linear-gradient\(to right, rgb\(0, 121, 116\) 0%, rgb\(31, 155, 130\) 7\.14\d*%,/.test(legend.image) &&
          /rgb\(100, 37, 96\) 100%\)$/.test(legend.image), legend.image);
        // #860 on the phone: every label is on screen, inside the bar's width,
        // clear of its neighbours and not covered.
        legend.spans.forEach((r, i) => {
          assert(r.left >= legend.barR.left - 0.5 && r.right <= legend.barR.right + 0.5, 'label ' + legend.text[i] + ' outside the bar ' + JSON.stringify(r));
          assert(r.left >= 0 && r.right <= vp.width && r.bottom <= vp.height, 'label ' + legend.text[i] + ' off screen ' + JSON.stringify(r));
          assert(r.visible, 'label ' + legend.text[i] + ' is covered');
          if (i) assert(r.left >= legend.spans[i - 1].right + 2, 'labels ' + legend.text[i - 1] + ' and ' + legend.text[i] + ' touch');
        });
        assert(legend.barH >= 8, 'bar ' + legend.barH + 'px');
        assert(legend.r.left >= 0 && legend.r.right <= vp.width + 1 && legend.r.bottom <= vp.height + 1, 'legend off screen ' + JSON.stringify(legend.r));
        assert(legend.aria === 'Elevation colours from 0 to 175 m, stops at 0, 2, 5, 10, 15, 20, 25, 30, 40, 50, 65, 80, 100, 130 and 175 m', legend.aria);
        const over = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
        assert(over <= 0, 'horizontal overflow ' + over);
      });

      await step('375x560: two taps on + reach z14; no tile above z13, the z13 tiles stay', async () => {
        const from = rec.tiles.length;
        // Close the layer list so + is not covered: a tap on the map beside
        // the open list (#859: its taller touch rows reach the centre, where
        // a tap would land on the Denmark toggle instead of the map).
        const list = await page.locator('.leaflet-control-layers').boundingBox();
        await page.touchscreen.tap(Math.round((list.x + list.width + vp.width) / 2), vp.height / 2);
        await page.waitForTimeout(400);
        for (let i = 0; i < 2; i++) {
          await page.tap('.leaflet-control-zoom-in');
          await page.waitForTimeout(1500);
        }
        const s = await state(page);
        assert(s.zoom === 14, 'not at z14: ' + s.zoom);
        const zs = elevationTiles(rec, from).map(bboxZoom);
        assert(zs.length > 0, 'no z13 tiles asked');
        assert(zs.every(z => z <= 13), 'tile zooms ' + zs.join(','));
        assert(s.tilesShown > 0, 'nothing shown at z14');
        assert(s.disabled === false, 'the toggle is disabled at z14');
      });

      await step('375x560: a reload keeps it; a tap turns it off and drops the parameter', async () => {
        const from = rec.tiles.length;
        await page.reload({ waitUntil: 'domcontentloaded' });
        await waitMap(page);
        await dismissTips(page);
        let s = await state(page);
        assert(s.checked === true, 'off after a reload');
        assert(elevationTiles(rec, from).length > 0, 'no tiles after a reload');
        await tapToggle(page, vp);
        s = await state(page);
        assert(s.checked === false, 'still on after a tap');
        assert(!/elevation=/.test(s.hash), 'hash ' + s.hash);
        assert(s.stored === '0', 'stored ' + s.stored);
        assert(await legendHidden(page) === true, 'the legend stays after the overlay went off');
        assert(rec.consoleErrors.length === 0, rec.consoleErrors.join(' | '));
      });
    } finally { await ctx.close(); }
  }

  // ── 3b. #831 F1: "Contour lines" stays tappable on a 375x560 phone ──────
  {
    const vp = { width: 375, height: 560 };
    const { ctx, page, rec } = await newCtx({ phone: true, viewport: vp });
    const CONTOURS = '.leaflet-control-layers-overlays label[data-overlay-id="dhm-contours"]';
    // A tap on the contours toggle where a finger would, WITHOUT scrolling
    // the layer list: the label must be inside the list's visible box (so
    // not under its scrolled-off bottom edge), inside the screen and the
    // element at the tap point.
    async function tapContoursUnscrolled() {
      await openLayers(page);
      const hit = await page.evaluate((sel) => {
        const l = document.querySelector(sel);
        const list = document.querySelector('.leaflet-control-layers-list');
        if (!l || !list) return null;
        const r = l.getBoundingClientRect(), b = list.getBoundingClientRect();
        const x = r.x + Math.min(r.width / 2, 60), y = r.y + r.height / 2;
        const h = document.elementFromPoint(x, y);
        return {
          x, y, scrollTop: list.scrollTop, hit: !!h && l.contains(h),
          r: { top: r.top, bottom: r.bottom }, list: { top: b.top, bottom: b.bottom },
        };
      }, CONTOURS);
      assert(hit, 'no contours toggle or layer list');
      assert(hit.scrollTop === 0, 'the list was scrolled: ' + hit.scrollTop);
      assert(hit.r.top >= hit.list.top && hit.r.bottom <= hit.list.bottom + 0.5,
        'contours outside the visible list: ' + JSON.stringify(hit.r) + ' in ' + JSON.stringify(hit.list));
      assert(hit.r.bottom <= vp.height, 'contours below the screen: ' + JSON.stringify(hit.r));
      assert(hit.hit, 'the tap point is not on the contours toggle');
      await page.touchscreen.tap(hit.x, hit.y);
      await page.waitForTimeout(900);
    }
    const contoursChecked = () => page.$eval(CONTOURS + ' input', i => i.checked);
    const isExpanded = () => page.$eval('.leaflet-control-layers', el => el.classList.contains('leaflet-control-layers-expanded'));
    try {
      await step('375x560 cold start at z14: contours tappable without scrolling, elevation off and on (#831 F1)', async () => {
        await page.goto(`${BASE}/#/map`, { waitUntil: 'domcontentloaded' });
        await page.waitForSelector('.leaflet-control-layers', { timeout: 20000 });
        await page.waitForTimeout(1200);
        await dismissTips(page);
        await page.goto(`${BASE}/#/map?lat=${AARHUS.lat}&lon=${AARHUS.lon}&zoom=11`, { waitUntil: 'domcontentloaded' });
        await waitMap(page);
        await dismissTips(page);
        for (let i = 0; i < 3; i++) { await page.tap('.leaflet-control-zoom-in'); await page.waitForTimeout(800); }
        let s = await state(page);
        assert(s.zoom === 14, 'zoom after three taps: ' + s.zoom);

        // Elevation off: one tap on contours turns it on.
        await tapContoursUnscrolled();
        assert(await contoursChecked() === true, 'contours not on after a tap (elevation off)');
        s = await state(page);
        assert(s.checked === false, 'elevation on');
        // Contours off again, then elevation on (its legend showing).
        if (!(await isExpanded())) await openLayers(page);
        await tapContoursUnscrolled();
        assert(await contoursChecked() === false, 'contours still on after a second tap');
        if (!(await isExpanded())) await openLayers(page);
        await tapToggle(page, vp);
        s = await state(page);
        assert(s.checked === true, 'elevation not on after a tap');
        assert(await legendHidden(page) === false, 'no legend with elevation on');
        // Then contours from the list as it now stands (elevation's tap
        // may have scrolled it; its row is the last of the DHM ones).
        if (!(await isExpanded())) await openLayers(page);
        await page.evaluate(() => { const l = document.querySelector('.leaflet-control-layers-list'); return l && l.scrollTop; });
        await tapContoursUnscrolled();
        assert(await contoursChecked() === true, 'contours not on after a tap (elevation on)');
        s = await state(page);
        assert(s.checked === true, 'the contours tap turned elevation off');
        assert(rec.consoleErrors.length === 0, rec.consoleErrors.join(' | '));
      });
    } finally { await ctx.close(); }
  }

  // ── 4. Customizer opacity; the link over storage ─────────────────────────
  {
    const storage = { [STORE]: JSON.stringify({ dhmOverlays: { elevationOpacity: 30 } }), 'mc-elevation-dhm': '1' };
    const { ctx, page, rec } = await newCtx({ storage });
    try {
      await step('the customizer opacity reaches the layer, live', async () => {
        await page.goto(`${BASE}/#/map?lat=${AARHUS.lat}&lon=${AARHUS.lon}&zoom=11`, { waitUntil: 'domcontentloaded' });
        await waitMap(page);
        let s = await state(page);
        assert(s.checked === true, 'stored choice not applied');
        assert(s.layerOpacity === '0.3', 'opacity ' + s.layerOpacity);
        await page.click('#customizeToggle');
        await page.waitForSelector('.cust-overlay:not(.hidden)', { timeout: 5000 });
        await page.click('.cust-tab[data-tab="display"]');
        await page.waitForSelector('.cust-panel[data-panel="display"].active', { timeout: 4000 });
        const inp = page.locator('[data-cv2-overlay="elevationOpacity"]');
        assert(await inp.inputValue() === '30', 'input ' + await inp.inputValue());
        const h = await inp.evaluate(el => el.getBoundingClientRect().height);
        assert(h >= 44, 'opacity input ' + h + 'px');
        await inp.fill('75');
        await inp.dispatchEvent('change');
        await page.waitForTimeout(800);
        s = await state(page);
        assert(s.layerOpacity === '0.75', 'opacity after change ' + s.layerOpacity);
        assert(s.checked === true, 'the change turned the layer off');
        const sec = await page.evaluate((k) => JSON.parse(localStorage.getItem(k) || '{}').dhmOverlays, STORE);
        assert(sec && sec.elevationOpacity === 75, 'stored ' + JSON.stringify(sec));
        assert(rec.consoleErrors.length === 0, rec.consoleErrors.join(' | '));
      });
      await step('?elevation=0 wins over the stored choice', async () => {
        const from = rec.tiles.length;
        await page.goto(`${BASE}/#/map?lat=${AARHUS.lat}&lon=${AARHUS.lon}&zoom=11&elevation=0`, { waitUntil: 'domcontentloaded' });
        await page.reload({ waitUntil: 'domcontentloaded' });
        await waitMap(page);
        const s = await state(page);
        assert(s.checked === false, 'on despite ?elevation=0');
        assert(elevationTiles(rec, from).length === 0, 'asked for elevation tiles');
      });
    } finally { await ctx.close(); }
  }

  // ── 5. A server before #792 ─────────────────────────────────────────────
  {
    const { ctx, page, rec } = await newCtx({ dhm: dhmBlock(OVERLAYS_734), storage: { 'mc-elevation-dhm': '1' } });
    try {
      await step('no elevation toggle, tile or legend without the entry', async () => {
        await page.goto(`${BASE}/#/map?lat=${AARHUS.lat}&lon=${AARHUS.lon}&zoom=11&elevation=1`, { waitUntil: 'domcontentloaded' });
        await waitMap(page);
        const s = await state(page);
        assert(s.ids.join(',') === 'dhm-terrain,dhm-surface,dhm-contours', 'ids ' + s.ids.join(','));
        assert(elevationTiles(rec).length === 0, 'elevation tiles asked');
        assert(await page.locator('.mc-elev-legend').count() === 0, 'a legend');
        assert(rec.consoleErrors.length === 0, rec.consoleErrors.join(' | '));
      });
    } finally { await ctx.close(); }
  }

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch(e => { console.error(e); process.exit(1); });
