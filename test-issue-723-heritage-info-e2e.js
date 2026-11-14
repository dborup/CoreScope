/**
 * E2E (#723 M2): click-for-info on the protected ancient monuments overlay,
 * Map and Live.
 *
 * /api/config/client is intercepted and given the `heritage` block the Go
 * server sends while map.heritage.enabled is true; GET /api/terrain/heritage
 * is answered by a stub and recorded; the agency's WMS is answered with a
 * 1x1 PNG. Neither the agency's WMS nor its WFS is ever contacted.
 *
 * Asserts:
 *   1. overlay off: a map click makes no lookup and opens no popup;
 *   2. overlay on at zoom 11: no lookup;
 *   3. overlay on at zoom 13: one lookup with lat and lon only, and the
 *      popup shows the stubbed monument; HTML in the dataset text is shown
 *      as text and never runs; turning the overlay off closes the popup;
 *   4. Map: a click on a node marker opens the node's popup, not a lookup;
 *   5. Map: while the Path profile picks A and B, clicks make profile
 *      requests and no lookup; once both ends are set, a click looks up;
 *   6. 360x740 touch: a tap opens the popup, its close button is 44x44 px
 *      and closes it;
 *   6b. closing the popup while its lookup is still pending: the late
 *      answer does not reopen it (#762 review finding 5);
 *   7. light and dark: the popup's colours are the theme's variables;
 *   8. no console errors.
 *
 * Usage:
 *   BASE_URL=http://localhost:13723 node test-issue-723-heritage-info-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13723';
const INFO_PATH = '/api/terrain/heritage';
const HERITAGE_BLOCK = {
  enabled: true, url: 'https://www.kulturarv.dk/ffgeoserver/public/wms',
  layers: 'fundogfortidsminder_areal_beskyttelse,fundogfortidsminder_punkt_fredet',
  minZoom: 12, maxZoom: 19, bounds: [[54.4646, 8.0899], [57.7289, 15.5689]],
  attribution: '© Slots- og Kulturstyrelsen',
};
const DHM_BLOCK = {
  enabled: true, url: '/api/map/dhm/wms', layer: 'dhm_terraen_skyggekort', minZoom: 7, maxZoom: 19, profile: true,
  bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
  attribution: 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0',
};
const LABEL = '.leaflet-control-layers-overlays label[data-overlay-id="heritage-monuments"]';
const DK = { lat: 55.756, lon: 9.42, zoom: 13 };
const PNG_1X1 = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg==', 'base64');
const XSS = '<img src=x onerror="window.__heritageXss=1">';
const MONUMENT = {
  radiusM: 100, count: 1, truncated: false, attribution: '© Slots- og Kulturstyrelsen',
  nearest: { type: 'Rundhøj', dating: 'Oldtid', status: 'Fredet', protectionNumber: '3624:15', systemNumber: '190001', distanceM: 0, insideZone: true },
};

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

function profileFor(q) {
  const end = (p) => ({ lat: 55.75, lon: 9.42, kind: 'point', groundM: 40, antennaM: 2, antennaSource: 'point-default', assumed: true });
  const samples = [];
  for (let i = 0; i < 20; i++) samples.push({ d: i * 50, dtm: 40, dsm: 45 });
  return {
    a: end(q.get('a')), b: end(q.get('b')), distanceM: 950, stepM: 10, sampleCount: 96, model: 'both',
    coverage: { state: 'full', pct: 100, firstGapM: null }, samples,
    physics: { freqMHz: 869.618, freqSource: 'firmware', lambdaM: 0.34474, k: 1.3333333333333333, kSource: 'default', f1MaxM: 9.1 },
    verdict: { state: 'clear', model: 'dtm', criterion: 0.6, worstClearanceM: 20, worstAtM: 400, worstF1Fraction: 2 },
    attribution: DHM_BLOCK.attribution,
  };
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #723 M2 heritage click-for-info against ${BASE} ===\n`);

  async function newCtx(o) {
    o = o || {};
    const ctx = await browser.newContext({ viewport: o.viewport || { width: 1280, height: 800 }, hasTouch: !!o.touch, isMobile: !!o.touch });
    const rec = { info: [], profile: [], agency: 0, consoleErrors: [], held: [], infoFailed: 0 };
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      config.heritage = Object.assign({}, HERITAGE_BLOCK, o.bounds ? { bounds: o.bounds } : {});
      if (o.dhm) config.dhm = DHM_BLOCK; else delete config.dhm;
      await route.fulfill({ response, json: config });
    });
    await ctx.route(url => new URL(url).pathname === INFO_PATH, async (route) => {
      const u = new URL(route.request().url());
      rec.info.push(u);
      // o.hold: the answer waits until the test releases it.
      if (o.hold) await new Promise(r => rec.held.push(r));
      // A request the page aborted can no longer be fulfilled.
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(o.answer || MONUMENT) }).catch(() => {});
    });
    await ctx.route(url => new URL(url).pathname === '/api/terrain/profile', async (route) => {
      const u = new URL(route.request().url());
      rec.profile.push(u);
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(profileFor(u.searchParams)) });
    });
    await ctx.route(url => new URL(url).pathname === DHM_BLOCK.url, route =>
      route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 }));
    await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, async (route) => {
      if (route.request().resourceType() === 'image') { await route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 }); return; }
      await route.continue();
    });
    // Registered last, so it runs first: the agency's host never sees a request.
    await ctx.route(/^https?:\/\/www\.kulturarv\.dk\//i, async (route) => {
      rec.agency++;
      await route.fulfill({ status: 200, contentType: 'image/png', headers: { 'Access-Control-Allow-Origin': '*' }, body: PNG_1X1 });
    });
    await ctx.addInitScript(({ theme }) => {
      try { localStorage.setItem('meshcore-theme', theme); } catch (_) {}
      const apply = () => document.documentElement && document.documentElement.setAttribute('data-theme', theme);
      apply();
      document.addEventListener('DOMContentLoaded', apply);
    }, { theme: o.theme || 'light' });
    const page = await ctx.newPage();
    page.on('console', m => { if (m.type() === 'error') rec.consoleErrors.push(m.text()); });
    page.on('pageerror', e => rec.consoleErrors.push(String(e)));
    page.on('requestfailed', r => { if (new URL(r.url()).pathname === INFO_PATH) rec.infoFailed++; });
    return { ctx, page, rec };
  }

  async function gotoMap(page, hash, view) {
    const v = view || DK;
    const sep = hash.includes('?') ? '&' : '?';
    await page.goto(`${BASE}/${hash}${sep}lat=${v.lat}&lon=${v.lon}&zoom=${v.zoom}`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('.leaflet-control-layers', { timeout: 20000 });
    await page.waitForFunction(() => window.MeshConfigReady !== undefined, null, { timeout: 10000 });
    await page.evaluate(() => window.MeshConfigReady).catch(() => {});
    await page.waitForTimeout(900);
  }
  async function setOverlay(page, on) {
    await page.evaluate(({ sel, want }) => {
      const input = document.querySelector(sel + ' input[type="checkbox"]');
      if (!input) throw new Error('heritage overlay not offered');
      if (input.checked !== want) input.click();
    }, { sel: LABEL, want: on });
    await page.waitForTimeout(500);
  }
  // A point on bare map: the container centre shifted by (dx, dy), checked
  // to hit the map itself and nothing on it.
  async function mapPoint(page, dx, dy) {
    const p = await page.$eval('.leaflet-container', (el, d) => {
      const r = el.getBoundingClientRect();
      return { x: r.x + r.width / 2 + d.dx, y: r.y + r.height / 2 + d.dy };
    }, { dx: dx || 0, dy: dy || 0 });
    const hit = await page.evaluate(({ x, y }) => {
      const t = document.elementFromPoint(x, y);
      return t ? { inMap: !!t.closest('.leaflet-container'), owned: !!t.closest('.leaflet-interactive, .leaflet-control, .leaflet-popup') } : null;
    }, p);
    assert(hit && hit.inMap && !hit.owned, 'point is not bare map: ' + JSON.stringify(hit));
    return p;
  }
  async function clickMap(page, dx, dy, tap) {
    const p = await mapPoint(page, dx, dy);
    if (tap) await page.touchscreen.tap(p.x, p.y); else await page.mouse.click(p.x, p.y);
    await page.waitForTimeout(500);
  }
  const popupText = (page) => page.evaluate(() => {
    const p = document.querySelector('.mc-heritage-popup');
    return p ? p.textContent : null;
  });

  const PAGES = [{ name: 'Map', hash: '#/map' }, { name: 'Live', hash: '#/live' }];

  for (const p of PAGES) {
    await step(`${p.name}: overlay off — a click makes no lookup and opens no popup`, async () => {
      const { ctx, page, rec } = await newCtx({});
      try {
        await gotoMap(page, p.hash);
        await clickMap(page, 40, 30);
        assert(rec.info.length === 0, 'lookups with the overlay off: ' + rec.info.length);
        assert((await popupText(page)) === null, 'popup opened');
        assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
      } finally { await ctx.close(); }
    });

    await step(`${p.name}: overlay on at zoom 11 — no lookup`, async () => {
      const { ctx, page, rec } = await newCtx({});
      try {
        await gotoMap(page, p.hash + '?heritage=1', { lat: DK.lat, lon: DK.lon, zoom: 11 });
        await clickMap(page, 40, 30);
        assert(rec.info.length === 0, 'lookups at zoom 11: ' + rec.info.length);
        assert((await popupText(page)) === null, 'popup opened at zoom 11');
      } finally { await ctx.close(); }
    });

    await step(`${p.name}: overlay on at zoom 13 — one lookup, the monument popup, off closes it`, async () => {
      const { ctx, page, rec } = await newCtx({});
      try {
        await gotoMap(page, p.hash + '?heritage=1');
        await clickMap(page, 40, 30);
        assert(rec.info.length === 1, 'lookups: ' + rec.info.length);
        const keys = Array.from(rec.info[0].searchParams.keys()).sort().join(',');
        assert(keys === 'lat,lon', 'params ' + keys);
        const lat = Number(rec.info[0].searchParams.get('lat')), lon = Number(rec.info[0].searchParams.get('lon'));
        assert(Math.abs(lat - DK.lat) < 0.02 && Math.abs(lon - DK.lon) < 0.03, `point ${lat},${lon}`);
        await page.waitForFunction(() => /Rundhøj/.test((document.querySelector('.mc-heritage-popup') || {}).textContent || ''), null, { timeout: 5000 });
        const t = await popupText(page);
        for (const s of ['Oldtid', 'Fredet', '3624:15', '190001', 'Inside protection zone', 'Yes', '© Slots- og Kulturstyrelsen']) assert(t.includes(s), 'missing ' + s);
        assert(!(await page.$('.mc-heritage-info a')), 'a link in the popup content');
        await setOverlay(page, false);
        assert((await popupText(page)) === null, 'popup survived the overlay going off');
        assert(rec.agency >= 0 && rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
      } finally { await ctx.close(); }
    });
  }

  await step('Map: dataset HTML is shown as text and never runs', async () => {
    const evil = { radiusM: 100, count: 1, truncated: true, attribution: XSS,
      nearest: { type: XSS, dating: XSS, status: XSS, protectionNumber: XSS, systemNumber: XSS, distanceM: 12, insideZone: false } };
    const { ctx, page } = await newCtx({ answer: evil });
    try {
      await gotoMap(page, '#/map?heritage=1');
      await clickMap(page, 40, 30);
      await page.waitForFunction(() => /onerror/.test((document.querySelector('.mc-heritage-popup') || {}).textContent || ''), null, { timeout: 5000 });
      const r = await page.evaluate(() => ({
        imgs: document.querySelectorAll('.mc-heritage-popup img').length,
        ran: window.__heritageXss === 1,
        text: document.querySelector('.mc-heritage-popup').textContent,
      }));
      assert(r.imgs === 0 && !r.ran, 'payload became markup: ' + JSON.stringify(r));
      assert(r.text.includes('12 m') && /may be incomplete/.test(r.text), r.text);
    } finally { await ctx.close(); }
  });

  await step('Map: a node marker click wins over the lookup', async () => {
    // The fixture's nodes are in California; the block's bounds are widened
    // so a lookup there would be allowed, and only precedence prevents it.
    const { ctx, page, rec } = await newCtx({ bounds: [[-85, -180], [85, 180]] });
    try {
      await gotoMap(page, '#/map?heritage=1', { lat: 37.888, lon: -122.268, zoom: 15 });
      const marker = await page.evaluateHandle(() => {
        const c = document.querySelector('.leaflet-container').getBoundingClientRect();
        return Array.from(document.querySelectorAll('.leaflet-marker-pane .leaflet-marker-icon')).find(el => {
          const r = el.getBoundingClientRect(), x = r.x + r.width / 2, y = r.y + r.height / 2;
          if (!(r.width > 0 && x > c.x + 60 && x < c.right - 60 && y > c.y + 60 && y < c.bottom - 60)) return false;
          const hit = document.elementFromPoint(x, y);
          return !!hit && el.contains(hit);
        }) || null;
      });
      assert(await marker.evaluate(el => !!el), 'no node marker drawn at the fixture node');
      await marker.click();
      await page.waitForTimeout(600);
      assert(rec.info.length === 0, 'lookup for a marker click: ' + rec.info.length);
      assert(!(await page.$('.mc-heritage-popup')), 'heritage popup for a marker click');
      await page.keyboard.press('Escape');
      await page.evaluate(() => { const b = document.querySelector('.leaflet-popup-close-button'); if (b) b.click(); });
      await clickMap(page, -300, 200);
      assert(rec.info.length === 1, 'bare-map click made no lookup: ' + rec.info.length);
    } finally { await ctx.close(); }
  });

  await step('Map: Path profile picking wins; after it, a click looks up', async () => {
    const { ctx, page, rec } = await newCtx({ dhm: true });
    try {
      await gotoMap(page, '#/map?heritage=1');
      await page.click('[data-testid="map-profile-toggle"]');
      await page.waitForTimeout(300);
      await clickMap(page, -150, 0);
      await clickMap(page, 150, 60);
      await page.waitForTimeout(800);
      assert(rec.profile.length >= 1, 'no profile request');
      assert(rec.info.length === 0, 'lookups while picking: ' + rec.info.length);
      assert(!(await page.$('.mc-heritage-popup')), 'heritage popup while picking');
      await clickMap(page, 0, -150);
      assert(rec.info.length === 1, 'no lookup once the picker is done: ' + rec.info.length);
      assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
    } finally { await ctx.close(); }
  });

  for (const p of PAGES) {
    await step(`${p.name} 360x740 touch: a tap opens the popup; the close button is 44x44 and closes it`, async () => {
      const { ctx, page, rec } = await newCtx({ viewport: { width: 360, height: 740 }, touch: true });
      try {
        await gotoMap(page, p.hash + '?heritage=1');
        await clickMap(page, 0, 60, true);
        assert(rec.info.length === 1, 'lookups: ' + rec.info.length);
        await page.waitForFunction(() => /Rundhøj/.test((document.querySelector('.mc-heritage-popup') || {}).textContent || ''), null, { timeout: 5000 });
        const box = await page.$eval('.mc-heritage-popup .leaflet-popup-close-button', el => { const r = el.getBoundingClientRect(); return { w: r.width, h: r.height, x: r.x + r.width / 2, y: r.y + r.height / 2 }; });
        assert(box.w >= 44 && box.h >= 44, 'close button ' + JSON.stringify(box));
        await page.touchscreen.tap(box.x, box.y);
        await page.waitForTimeout(400);
        assert(!(await page.$('.mc-heritage-popup')), 'popup still open after tapping close');
        const overflow = await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth);
        assert(!overflow, 'horizontal page scroll');
      } finally { await ctx.close(); }
    });
  }

  for (const p of PAGES) {
    await step(`${p.name}: closing the popup while the lookup is pending — the late answer does not reopen it`, async () => {
      const { ctx, page, rec } = await newCtx({ hold: true });
      try {
        await gotoMap(page, p.hash + '?heritage=1');
        await clickMap(page, 40, 30);
        assert(rec.info.length === 1, 'lookups: ' + rec.info.length);
        await page.waitForFunction(() => /Looking up/.test((document.querySelector('.mc-heritage-popup') || {}).textContent || ''), null, { timeout: 5000 });
        await page.click('.mc-heritage-popup .leaflet-popup-close-button');
        await page.waitForTimeout(300);
        assert(!(await page.$('.mc-heritage-popup')), 'popup still open after its close button');
        rec.held.splice(0).forEach(r => r());
        await page.waitForTimeout(800);
        assert(!(await page.$('.mc-heritage-popup')), 'the late answer reopened the closed popup');
        assert(rec.infoFailed === 1, 'the pending lookup was not aborted: ' + rec.infoFailed);
        assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
      } finally { await ctx.close(); }
    });
  }

  for (const theme of ['light', 'dark']) {
    await step(`Map ${theme}: the popup uses the theme's variables`, async () => {
      const { ctx, page } = await newCtx({ theme });
      try {
        await gotoMap(page, '#/map?heritage=1');
        await clickMap(page, 40, 30);
        await page.waitForSelector('.mc-heritage-popup .mc-heritage-fields dt', { timeout: 5000 });
        const c = await page.evaluate(() => {
          const probe = document.createElement('div');
          probe.style.cssText = 'color: var(--text-muted); background: var(--card-bg)';
          document.body.appendChild(probe);
          const want = getComputedStyle(probe);
          const out = {
            wantMuted: want.color, wantBg: want.backgroundColor,
            dt: getComputedStyle(document.querySelector('.mc-heritage-popup dt')).color,
            bg: getComputedStyle(document.querySelector('.mc-heritage-popup .leaflet-popup-content-wrapper')).backgroundColor,
          };
          probe.remove();
          return out;
        });
        assert(c.dt === c.wantMuted && c.bg === c.wantBg, JSON.stringify(c));
      } finally { await ctx.close(); }
    });
  }

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch(e => { console.error(e); process.exit(1); });
