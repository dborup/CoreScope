/**
 * E2E (#734 "Later (customizer)", AGENTS rule 8): the DHM overlay defaults
 * in the customizer and their effect on the Map page.
 *
 * Nothing leaves the machine: /api/config/client is intercepted and given
 * the `dhm` block with dhm.overlays (cmd/server/dhm_proxy.go), every DHM
 * tile is a synthetic 256x256 PNG, and third-party base tiles are stubs.
 *
 * Asserts:
 *   1. a phone at 375x560, cold start (no storage, no deep link), real taps:
 *      dismiss the tips, open the customizer from the bottom nav's More
 *      sheet, tap Display, pick "Surface" as the default hillshade (a tap on
 *      the select, then the option: a native picker cannot be tapped), tap
 *      "Show contour lines by default", tap the min-zoom field and type 15;
 *      each control is at least 48 px, inside the screen and hit by itself;
 *      the section is stored; the open map's contour label says "zoom 15+"
 *      at once; nothing overflows sideways;
 *   2. the next map (a reload) starts with surface and contours on, asks no
 *      contour tile at z14 (its checkbox disabled) and does after one tap
 *      on + (z15); the link carries neither default; tapping surface off
 *      writes terrain=0 (a default-on overlay's off rides the link);
 *   3. URL > customizer: ?terrain=1&contours=0 gives terrain on and
 *      contours off over the stored defaults;
 *   4. round trip: the export JSON carries the section; without a dhm block
 *      the section is absent;
 *   5. no console errors;
 *   6. changing the hillshade or contours default forgets the viewer's
 *      remembered layer-control choice for that overlay, so the next map
 *      starts with the new default (v0.2.9 review nit, #826 F1).
 *
 * Usage:
 *   BASE_URL=http://localhost:13700 node test-issue-734-customizer-overlays-e2e.js
 */
'use strict';
const { chromium } = require('playwright');
const zlib = require('zlib');

const BASE = process.env.BASE_URL || 'http://localhost:13700';
const DHM_PATH = '/api/map/dhm/wms';
const STORE = 'cs-theme-overrides';
const ATTRIBUTION = 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0';
const DHM_BLOCK = {
  enabled: true, url: DHM_PATH, layer: 'dhm_terraen_skyggekort',
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
const LABEL = { terrain: 'dhm-terrain', surface: 'dhm-surface', contours: 'dhm-contours' };
const AARHUS = { lat: 56.16, lon: 10.2 };

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
  console.log(`\n=== #734 customizer: DHM overlay defaults against ${BASE} ===\n`);

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
      rec.tiles.push(new URL(route.request().url()));
      await route.fulfill({ status: 200, contentType: 'image/png', body: DHM_TILE });
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

  const count = (rec, id, from) => rec.tiles.slice(from || 0).filter(u => u.searchParams.get('layers') === id).length;
  const storedSection = (page) => page.evaluate((k) => {
    try { return JSON.parse(localStorage.getItem(k) || '{}').dhmOverlays || null; } catch (_) { return 'unparsable'; }
  }, STORE);
  async function state(page) {
    return page.evaluate((LABEL) => {
      const out = { checked: {}, disabled: {}, labels: {}, hash: location.hash };
      for (const [id, dataId] of Object.entries(LABEL)) {
        const label = document.querySelector(`.leaflet-control-layers-overlays label[data-overlay-id="${dataId}"]`);
        const input = label && label.querySelector('input');
        out.checked[id] = input ? input.checked : null;
        out.disabled[id] = input ? input.disabled : null;
        out.labels[id] = label ? label.textContent.trim() : null;
      }
      return out;
    }, LABEL);
  }
  async function waitMap(page) {
    await page.waitForSelector('.leaflet-control-layers-overlays label[data-overlay-id="dhm-contours"]', { state: 'attached', timeout: 20000 });
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
  // A control in the customizer: scrolled into view, at least 48 px tall,
  // inside the screen and hit by itself at its centre; then tapped there.
  async function tapControl(page, sel, vp) {
    await page.locator(sel).scrollIntoViewIfNeeded();
    await page.waitForTimeout(200);
    const hit = await page.evaluate((sel) => {
      const el = document.querySelector(sel);
      const target = el.closest('label') || el;
      const r = target.getBoundingClientRect();
      const x = r.x + Math.min(r.width / 2, 40), y = r.y + r.height / 2;
      const h = document.elementFromPoint(x, y);
      return { x, y, h: r.height, top: r.top, bottom: r.bottom, left: r.left, right: r.right, ok: !!h && (target === h || target.contains(h)) };
    }, sel);
    assert(hit.h >= 48, sel + ' is ' + hit.h + 'px tall, below the 48px touch target');
    assert(hit.top >= 0 && hit.bottom <= vp.height + 1 && hit.left >= 0 && hit.right <= vp.width + 1, sel + ' off screen ' + JSON.stringify(hit));
    assert(hit.ok, sel + ' is covered at the tap point');
    await page.touchscreen.tap(hit.x, hit.y);
    await page.waitForTimeout(300);
  }
  async function openDisplayByTaps(page) {
    await page.tap('[data-bottom-nav-tab="more"]');
    await page.waitForSelector('[data-mpa-mirror="customizeToggle"]', { state: 'visible', timeout: 5000 });
    await page.tap('[data-mpa-mirror="customizeToggle"]');
    await page.waitForSelector('.cust-overlay:not(.hidden)', { timeout: 5000 });
    await page.tap('.cust-tab[data-tab="display"]');
    await page.waitForSelector('.cust-panel[data-panel="display"].active', { timeout: 4000 });
  }
  async function tapMapLabel(page, id) {
    const t = await page.$('.leaflet-control-layers-toggle');
    if (t && await t.isVisible()) { await t.tap(); await page.waitForTimeout(600); }
    const sel = `.leaflet-control-layers-overlays label[data-overlay-id="${LABEL[id]}"]`;
    await page.locator(sel).scrollIntoViewIfNeeded();
    const hit = await page.evaluate((sel) => {
      const l = document.querySelector(sel), r = l.getBoundingClientRect();
      const x = r.x + Math.min(r.width / 2, 60), y = r.y + r.height / 2;
      const h = document.elementFromPoint(x, y);
      return { ok: !!h && l.contains(h), x, y };
    }, sel);
    assert(hit.ok, id + ': its label is covered at the tap point');
    await page.touchscreen.tap(hit.x, hit.y);
    await page.waitForTimeout(900);
  }

  // ── 1 + 2. Phone, cold start, real taps ─────────────────────────────────
  {
    const vp = { width: 375, height: 560 };
    const { ctx, page, rec } = await newCtx({ phone: true, viewport: vp });
    try {
      await step('375x560 cold start: set the defaults in the customizer by taps', async () => {
        await page.goto(`${BASE}/#/map`, { waitUntil: 'domcontentloaded' });
        await page.waitForSelector('.leaflet-control-layers', { timeout: 20000 });
        await page.waitForTimeout(1200);
        await dismissTips(page);
        // Denmark, once through the address bar (place search is not this
        // feature); from here on only taps.
        await page.goto(`${BASE}/#/map?lat=${AARHUS.lat}&lon=${AARHUS.lon}&zoom=14`, { waitUntil: 'domcontentloaded' });
        await waitMap(page);
        await dismissTips(page);
        let s = await state(page);
        assert(!s.checked.terrain && !s.checked.surface && !s.checked.contours, 'built-in is not all off: ' + JSON.stringify(s.checked));
        assert(s.labels.contours === 'Contour lines (DHM, zoom 14+)', s.labels.contours);

        await openDisplayByTaps(page);
        const title = await page.evaluate(() => Array.from(document.querySelectorAll('.cust-section-title')).some(p => p.textContent === 'DHM Map Overlays'));
        assert(title, 'no DHM Map Overlays section');
        // The select: a tap, then the option (a native picker cannot be tapped).
        await tapControl(page, '[data-cv2-overlay="hillshade"]', vp);
        await page.selectOption('[data-cv2-overlay="hillshade"]', 'surface');
        await page.waitForTimeout(600);
        await tapControl(page, '[data-cv2-overlay="contours"]', vp);
        await page.waitForTimeout(600);
        assert(await page.isChecked('[data-cv2-overlay="contours"]'), 'contours box not checked after a tap');
        await tapControl(page, '[data-cv2-overlay="contourMinZoom"]', vp);
        // Clear the field the way a phone keyboard does, then type.
        await page.keyboard.press('End');
        for (let i = 0; i < 3; i++) await page.keyboard.press('Backspace');
        await page.keyboard.type('15');
        assert(await page.inputValue('[data-cv2-overlay="contourMinZoom"]') === '15', 'typed value');
        // Tapping the section title blurs the field, which fires change.
        await page.tap('.cust-panel[data-panel="display"] .cust-section-title');
        await page.waitForTimeout(700);
        const sec = await storedSection(page);
        assert(sec && sec.hillshade === 'surface' && sec.contours === true && sec.contourMinZoom === 15, 'stored ' + JSON.stringify(sec));
        const over = await page.evaluate(() => {
          const o = document.querySelector('.cust-overlay');
          return { page: document.documentElement.scrollWidth - document.documentElement.clientWidth, overlay: o ? o.scrollWidth - o.clientWidth : 0 };
        });
        assert(over.page <= 0 && over.overlay <= 0, 'horizontal overflow ' + JSON.stringify(over));
        // The open map's control follows the new min-zoom at once.
        s = await state(page);
        assert(s.labels.contours === 'Contour lines (DHM, zoom 15+)', 'label not live: ' + s.labels.contours);
        assert(!s.checked.surface && !s.checked.contours, 'a default changed the open map: ' + JSON.stringify(s.checked));
        await page.tap('.cust-close');
        await page.waitForTimeout(400);
      });

      await step('375x560: the next map starts with the defaults; contours from z15; the link carries none', async () => {
        const from = rec.tiles.length;
        await page.reload({ waitUntil: 'domcontentloaded' });
        await waitMap(page);
        await dismissTips(page);
        let s = await state(page);
        assert(s.checked.surface && !s.checked.terrain && s.checked.contours, 'start ' + JSON.stringify(s.checked));
        assert(s.disabled.contours, 'contours enabled below its min-zoom');
        assert(!/[?&](terrain|contours)=/.test(s.hash), 'a default rode the link: ' + s.hash);
        assert(count(rec, 'surface', from) > 0, 'no surface tiles');
        assert(count(rec, 'contours', from) === 0, count(rec, 'contours', from) + ' contour tiles below z15');
        assert(count(rec, 'terrain', from) === 0, 'terrain tiles');
        await page.tap('.leaflet-control-zoom-in');
        await page.waitForTimeout(1500);
        s = await state(page);
        assert(!s.disabled.contours, 'contours still disabled at z15');
        assert(count(rec, 'contours', from) > 0, 'no contour tiles at z15');
        // Off while the default is on rides the link as =0.
        await tapMapLabel(page, 'surface');
        s = await state(page);
        assert(!s.checked.surface, 'surface still on after a tap');
        assert(/[?&]terrain=0\b/.test(s.hash), 'hash ' + s.hash);
        const pageScrollX = await page.evaluate(() => document.documentElement.scrollWidth - innerWidth);
        assert(pageScrollX <= 0, 'horizontal page scroll ' + pageScrollX);
        assert(rec.consoleErrors.length === 0, rec.consoleErrors.join(' | '));
      });
    } finally { await ctx.close(); }
  }

  // ── 3. URL > customizer ─────────────────────────────────────────────────
  {
    const storage = { [STORE]: JSON.stringify({ dhmOverlays: { hillshade: 'surface', contours: true } }) };
    const { ctx, page, rec } = await newCtx({ storage });
    try {
      await step('a link wins over the customizer defaults', async () => {
        await page.goto(`${BASE}/#/map?lat=${AARHUS.lat}&lon=${AARHUS.lon}&zoom=15&terrain=1&contours=0`, { waitUntil: 'domcontentloaded' });
        await waitMap(page);
        const s = await state(page);
        assert(s.checked.terrain && !s.checked.surface && !s.checked.contours, JSON.stringify(s.checked));
        assert(count(rec, 'surface') === 0 && count(rec, 'contours') === 0, 'asked for an overlay the link turned off');
        assert(rec.consoleErrors.length === 0, rec.consoleErrors.join(' | '));
      });
      // ── 4. Export round trip ──
      await step('the export JSON carries the section', async () => {
        await page.click('#customizeToggle');
        await page.waitForSelector('.cust-overlay:not(.hidden)', { timeout: 5000 });
        await page.click('.cust-tab[data-tab="export"]');
        await page.waitForSelector('.cust-panel[data-panel="export"].active', { timeout: 4000 });
        const text = await page.evaluate(() => {
          const t = document.querySelector('.cust-panel[data-panel="export"] textarea, .cust-panel[data-panel="export"] pre');
          return t ? (t.value || t.textContent) : '';
        });
        const json = JSON.parse(text);
        assert(json.dhmOverlays && json.dhmOverlays.hillshade === 'surface' && json.dhmOverlays.contours === true, text);
      });
    } finally { await ctx.close(); }
  }

  // ── 6. A new default forgets the remembered layer-control choice ──
  {
    const storage = { 'mc-terrain-dhm': '1', 'mc-contours-dhm': '1' };
    const { ctx, page, rec } = await newCtx({ storage });
    try {
      await step('changing a default forgets the remembered choice for that overlay', async () => {
        await page.goto(`${BASE}/#/map?lat=${AARHUS.lat}&lon=${AARHUS.lon}&zoom=15`, { waitUntil: 'domcontentloaded' });
        await waitMap(page);
        let s = await state(page);
        assert(s.checked.terrain && s.checked.contours, 'the remembered choice did not apply: ' + JSON.stringify(s.checked));
        await page.click('#customizeToggle');
        await page.waitForSelector('.cust-overlay:not(.hidden)', { timeout: 5000 });
        await page.click('.cust-tab[data-tab="display"]');
        await page.waitForSelector('.cust-panel[data-panel="display"].active', { timeout: 4000 });
        await page.selectOption('[data-cv2-overlay="hillshade"]', 'surface');
        await page.waitForTimeout(400);
        await page.click('[data-cv2-overlay="contours"]');
        await page.waitForTimeout(400);
        const left = await page.evaluate(() => [localStorage.getItem('mc-terrain-dhm'), localStorage.getItem('mc-contours-dhm')]);
        assert(left[0] === null && left[1] === null, 'remembered choices left: ' + JSON.stringify(left));
        await page.goto(`${BASE}/#/map?lat=${AARHUS.lat}&lon=${AARHUS.lon}&zoom=15`, { waitUntil: 'domcontentloaded' });
        await page.reload({ waitUntil: 'domcontentloaded' });
        await waitMap(page);
        s = await state(page);
        assert(s.checked.surface && !s.checked.terrain && s.checked.contours, 'next map: ' + JSON.stringify(s.checked));
        assert(rec.consoleErrors.length === 0, rec.consoleErrors.join(' | '));
      });
    } finally { await ctx.close(); }
  }

  {
    const { ctx, page } = await newCtx({ dhm: null });
    try {
      await step('without a dhm block the section is absent', async () => {
        await page.goto(`${BASE}/#/map`, { waitUntil: 'domcontentloaded' });
        await page.waitForSelector('.leaflet-control-layers', { timeout: 20000 });
        await page.waitForFunction(() => window._customizerV2 && window._customizerV2.initDone, null, { timeout: 15000 });
        await page.click('#customizeToggle');
        await page.waitForSelector('.cust-overlay:not(.hidden)', { timeout: 5000 });
        await page.click('.cust-tab[data-tab="display"]');
        await page.waitForSelector('.cust-panel[data-panel="display"].active', { timeout: 4000 });
        assert(await page.locator('[data-cv2-overlay]').count() === 0, 'section shown without DHM');
      });
    } finally { await ctx.close(); }
  }

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch(e => { console.error(e); process.exit(1); });
