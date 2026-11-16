/**
 * E2E (#839, CS-Macmini rc19 findings 1, 2, 7, 8, 9): the map on a phone.
 *
 * Every scenario is a fresh browser context (cold start, empty storage),
 * driven by real taps (touch emulation) on phones and real clicks on the
 * desktop. Nothing leaves the machine: /api/config/client is intercepted
 * and given the blocks the staging server sends (base maps, DHM overlays
 * with Aerial, monuments, antenna sites; the token is a dummy), /api/masts
 * answers one rich antenna site at Jelling, the monument lookup is a stub
 * and every third-party image is a 1x1 PNG.
 *
 * Asserts:
 *   1. finding 1 — at 375x560 (light and dark), 360x640 and 640x360, with
 *      three overlay credits on, the open layer list ends above the
 *      attribution and the bottom nav; a swipe brings its last row,
 *      "Nearby transmitters (interference)", into view, a tap there is not
 *      taken by the credit and switches it on; at zoom 12 the antenna
 *      "Zoom in to 13+" line under it can be seen too;
 *   2. finding 2 — at the same phone sizes, a tap on the antenna site opens
 *      a popup that overlaps no map button and stops above the credit; its
 *      source link can be swiped to and seen; a tap on its ✕ closes it. At
 *      1440x900 the same popup shows whole, unscrolled, clear of the Map
 *      Controls panel (no regression);
 *   3. finding 7 — at 375x560 and 640x360 with monuments on, the tap that
 *      closes the open layer control asks for no monument info and opens no
 *      popup; the next tap on the map still does;
 *   4. finding 8 — at 1440x900, the surface hillshade and the monuments
 *      switched on in the layer control are off again after Reset All and a
 *      reload of the same page: their remembered values and link
 *      parameters are gone;
 *   5. finding 9 — opening the customizer and each of its tabs logs no
 *      "does not conform to the required format" warning (light, dark,
 *      phone);
 *   6. no console errors anywhere.
 *
 * Usage:
 *   BASE_URL=http://localhost:13839 node test-issue-839-map-phone-e2e.js
 */
'use strict';
const { chromium } = require('playwright');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const BASE = process.env.BASE_URL || 'http://localhost:13839';
const PNG_1X1 = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg==', 'base64');

const DHM_ATTR = 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0';
const LICENSE_URL = 'https://creativecommons.org/licenses/by/4.0/';
const DK_BOUNDS = [[54.4265, 7.99125], [57.778, 15.5995]];
const MASK = JSON.parse(fs.readFileSync(path.join(__dirname, 'test-fixtures', 'dhm-ortho-foreign-mask.json'), 'utf8'));
const DHM_BLOCK = {
  enabled: true, url: '/api/map/dhm/wms', layer: 'dhm_terraen_skyggekort', minZoom: 7, maxZoom: 19, profile: true,
  bounds: DK_BOUNDS, attribution: DHM_ATTR,
  datasetName: 'Danmarks Højdemodel', datasetUrl: 'https://dataforsyningen.dk/data/930',
  licenseName: 'CC BY 4.0', licenseUrl: LICENSE_URL, attributionDerived: DHM_ATTR + ' — data bearbejdet',
  overlays: [
    { id: 'terrain', minZoom: 7, maxZoom: 19, datasetName: 'Danmarks Højdemodel', datasetUrl: 'https://dataforsyningen.dk/data/930' },
    { id: 'surface', minZoom: 7, maxZoom: 19, datasetName: 'Danmarks Højdemodel', datasetUrl: 'https://dataforsyningen.dk/data/928' },
    { id: 'contours', minZoom: 14, maxZoom: 19, datasetName: 'Danmarks Højdemodel', datasetUrl: 'https://dataforsyningen.dk/data/926' },
  ],
  ortho: {
    url: '/api/map/dhm/ortho/{z}/{x}/{y}', minZoom: 7, maxZoom: 20, bounds: DK_BOUNDS,
    attribution: 'Indeholder data fra GeoDanmark, Ortofoto forår, CC BY 4.0',
    datasetName: 'Ortofoto forår', datasetUrl: 'https://dataforsyningen.dk/data/981',
    licenseName: 'CC BY 4.0', licenseUrl: LICENSE_URL, foreignMask: MASK,
  },
};
const HERITAGE_BLOCK = {
  enabled: true, url: 'https://www.kulturarv.dk/ffgeoserver/public/wms',
  layers: 'fundogfortidsminder_areal_beskyttelse,fundogfortidsminder_punkt_fredet',
  minZoom: 12, maxZoom: 19, bounds: [[54.4646, 8.0899], [57.7289, 15.5689]],
  attribution: '© Slots- og Kulturstyrelsen',
};
const MASTS_BLOCK = {
  enabled: true, url: '/api/masts', minZoom: 13, cellSizeM: 2000,
  attribution: 'Antenna positions: Mastedatabasen, Digitaliseringsstyrelsen (CC0)',
  sourceUrl: 'https://mastedatabasen.dk/', cellExtent: [219, 3017, 462, 3209],
  hintDistanceM: 500, hintStrongM: 100,
};
// The staging server's base maps; the Dataforsyningen token is a dummy.
const TILES_BLOCK = {
  darkDefault: 'esri-darkgray-labels', lightDefault: 'opentopomap',
  providers: { dataforsyningen: { enabled: true, token: 'test-token' }, esri: { enabled: true }, opentopomap: { enabled: true } },
};

const JELLING = { lat: 55.7565, lon: 9.4195 };
// The antenna site of finding 2: nine stations, nine band rows, the
// interference note — taller than a phone's map.
const SITE = {
  lat: 55.7572, lon: 9.4188, street: 'Bredager', houseNumber: '31', postcode: '7300', city: 'Jelling',
  stations: ['S1', 'S2', 'S3', 'S4', 'S5', 'S6', 'S7', 'S8', 'S9'],
  entries: [
    ['GSM', 900, true], ['LTE', 800, true], ['LTE', 1800, false], ['LTE', 2600, false], ['5G-NR', 700, true],
    ['5G-NR', 3500, false], ['UMTS', 2100, false], ['LTE', 700, true], ['5G-NR', 2100, false],
  ].map(([technology, bandMHz, interference], i) => ({
    technology, bandMHz, service: 'Mobiltelefoni', commissioned: '2014-03-' + String(10 + i), interference,
  })),
  interference: true, likelyMast: false,
};
const ML = (() => {
  const ctx = { window: {}, Math, Map, Set, Promise, Object, Array, Number, String, isFinite };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'public', 'masts-layer.js'), 'utf8'), ctx);
  return ctx.window.MastsLayer;
})();
const SITE_CELL = (() => { const p = ML.toUTM32(SITE.lat, SITE.lon); return Math.floor(p.e / 2000) + ',' + Math.floor(p.n / 2000); })();

const PHONES = [
  { name: '375x560', width: 375, height: 560 },
  { name: '360x640', width: 360, height: 640 },
  { name: '640x360', width: 640, height: 360 },
];
const LIST = '.leaflet-control-layers-list';
const INT_LABEL = '.leaflet-control-layers-overlays label[data-overlay-id="masts-interference"]';

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
  console.log(`\n=== #839 map on a phone against ${BASE} ===\n`);

  async function newCtx(o) {
    o = o || {};
    const touch = o.touch !== false;
    const ctx = await browser.newContext({
      viewport: o.viewport, hasTouch: touch, isMobile: touch, colorScheme: o.theme || 'light',
    });
    const rec = { errors: [], warnings: [], heritage: 0 };
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      config.map = Object.assign({}, config.map, { tiles: TILES_BLOCK });
      config.dhm = DHM_BLOCK;
      config.heritage = HERITAGE_BLOCK;
      config.masts = MASTS_BLOCK;
      await route.fulfill({ response, json: config });
    });
    await ctx.route((u) => new URL(u).pathname === MASTS_BLOCK.url, async (route) => {
      const cell = new URL(route.request().url()).searchParams.get('cell') || '';
      const [e, n] = cell.split(',').map(Number);
      const sites = o.noSites || cell !== SITE_CELL ? [] : [SITE];
      await route.fulfill({ status: 200, json: { cell: { e, n }, truncated: false, sites } });
    });
    await ctx.route((u) => new URL(u).pathname === '/api/terrain/heritage', async (route) => {
      rec.heritage++;
      await route.fulfill({ status: 200, json: { radiusM: 100, count: 0, truncated: false, attribution: HERITAGE_BLOCK.attribution } });
    });
    await ctx.route((u) => /^\/api\/map\/dhm\//.test(new URL(u).pathname), (route) =>
      route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 }));
    await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, async (route) => {
      if (route.request().resourceType() === 'image') {
        await route.fulfill({ status: 200, contentType: 'image/png', headers: { 'Access-Control-Allow-Origin': '*' }, body: PNG_1X1 });
        return;
      }
      await route.abort();
    });
    await ctx.addInitScript(() => {
      // The cold-start gesture tips are their own E2E (#1065); here they
      // would sit over the map the way a viewer dismisses them.
      try {
        ['row-swipe', 'tab-swipe', 'edge-drawer', 'pull-refresh']
          .forEach((k) => localStorage.setItem('meshcore-gesture-hints-' + k, 'seen'));
      } catch (_) {}
      // Record every Leaflet map, so the test can find the site on screen.
      let _L;
      Object.defineProperty(window, 'L', {
        configurable: true,
        get() { return _L; },
        set(v) {
          _L = v;
          try { v.Map.addInitHook(function () { (window.__mcMaps = window.__mcMaps || []).push(this); }); } catch (_) {}
        },
      });
    });
    const page = await ctx.newPage();
    page.on('console', (m) => {
      if (m.type() === 'error') rec.errors.push(m.text());
      if (/does not conform to the required format/.test(m.text())) rec.warnings.push(m.text());
    });
    page.on('pageerror', (e) => rec.errors.push(String(e)));
    page.on('dialog', (d) => d.accept());
    return { ctx, page, rec, touch };
  }

  async function gotoMap(page, extra, zoom) {
    await page.goto(`${BASE}/#/map?lat=${JELLING.lat}&lon=${JELLING.lon}&zoom=${zoom || 15}${extra || ''}`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('.leaflet-control-layers-toggle', { timeout: 20000 });
    await page.evaluate(() => window.MeshConfigReady).catch(() => {});
    await page.waitForSelector('.leaflet-control-layers-overlays label[data-overlay-id="masts-interference"]', { state: 'attached', timeout: 10000 });
    await page.waitForTimeout(1200);
  }
  async function center(page, sel) {
    const b = await page.locator(sel).first().boundingBox();
    assert(b, 'not on the page: ' + sel);
    return { x: b.x + b.width / 2, y: b.y + b.height / 2 };
  }
  async function tapAt(page, p) { await page.touchscreen.tap(Math.round(p.x), Math.round(p.y)); }
  // A real touch swipe (Chrome's synthetic gesture), upwards by `dy` px.
  async function swipeUp(page, at, dy) {
    const cdp = await page.context().newCDPSession(page);
    await cdp.send('Input.synthesizeScrollGesture', {
      x: Math.round(at.x), y: Math.round(at.y), yDistance: -dy, speed: 2000, gestureSourceType: 'touch',
    });
    await cdp.detach();
    await page.waitForTimeout(300);
  }
  // What a tap at the middle of `sel` would land on: inside `sel` or not.
  async function hitsItself(page, sel) {
    return page.evaluate((s) => {
      const el = document.querySelector(s);
      const b = el.getBoundingClientRect();
      const top = document.elementFromPoint(b.left + b.width / 2, b.top + b.height / 2);
      return { ok: !!top && (top === el || el.contains(top)), on: top ? (top.id || top.className || top.tagName) : null };
    }, sel);
  }
  // The highest edge of what sits at the map's bottom: the credit and the nav.
  async function bottomChromeTop(page) {
    return page.evaluate(() => Math.min.apply(null, ['.leaflet-control-attribution', '.bottom-nav']
      .map((s) => document.querySelector(s)).filter((el) => el && el.getBoundingClientRect().height)
      .map((el) => el.getBoundingClientRect().top)));
  }

  // ── 1. finding 1: every layer in reach ───────────────────────────────────
  const reachRuns = PHONES.map((v) => Object.assign({ theme: 'light' }, v));
  reachRuns.splice(1, 0, Object.assign({ theme: 'dark' }, PHONES[0]));
  for (const v of reachRuns) {
    await step(`finding 1, ${v.name} ${v.theme}: the last layer row is above the credit and takes a tap`, async () => {
      const { ctx, page, rec } = await newCtx({ viewport: { width: v.width, height: v.height }, theme: v.theme });
      try {
        await gotoMap(page, '&terrain=surface&contours=1&heritage=1');
        await page.tap('.leaflet-control-layers-toggle');
        await page.waitForSelector('.leaflet-control-layers-expanded', { timeout: 3000 });
        const listBottom = await page.evaluate((s) => document.querySelector(s).closest('.leaflet-control-layers').getBoundingClientRect().bottom, LIST);
        const floor = await bottomChromeTop(page);
        assert(listBottom <= floor, `the open list ends at y=${Math.round(listBottom)}, under the credit/nav at y=${Math.round(floor)}`);
        const sh = await page.evaluate((s) => document.querySelector(s).scrollHeight, LIST);
        await swipeUp(page, await center(page, LIST), sh + 200);
        const atEnd = await page.evaluate((s) => { const l = document.querySelector(s); return l.scrollTop + l.clientHeight >= l.scrollHeight - 2; }, LIST);
        assert(atEnd, 'the swipe did not scroll the list to its end');
        const hit = await hitsItself(page, INT_LABEL);
        assert(hit.ok, 'a tap on "Nearby transmitters (interference)" lands on ' + hit.on);
        await tapAt(page, await center(page, INT_LABEL));
        await page.waitForTimeout(600);
        const on = await page.evaluate((s) => document.querySelector(s + ' input').checked, INT_LABEL);
        assert(on, 'the tap did not switch the interference layer on');
        assert(/mastsInt=1/.test(await page.evaluate(() => location.hash)), 'the link does not say mastsInt=1');
        assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
      } finally { await ctx.close(); }
    });
  }

  await step('finding 1, 375x560 zoom 12: the antenna "Zoom in to 13+" line under the last row can be seen', async () => {
    const { ctx, page, rec } = await newCtx({ viewport: { width: 375, height: 560 } });
    try {
      await gotoMap(page, '&terrain=surface&contours=1&heritage=1', 12);
      await page.tap('.leaflet-control-layers-toggle');
      await page.waitForSelector('.leaflet-control-layers-expanded', { timeout: 3000 });
      const sh = await page.evaluate((s) => document.querySelector(s).scrollHeight, LIST);
      await swipeUp(page, await center(page, LIST), sh + 200);
      const hint = await page.evaluate(() => {
        const els = Array.from(document.querySelectorAll('.leaflet-control-layers-overlays *'))
          .filter((e) => !e.children.length && /^Zoom in to 13\+/.test(e.textContent.trim()) && !e.hidden);
        if (!els.length) return null;
        const b = els[0].getBoundingClientRect();
        const top = document.elementFromPoint(b.left + Math.min(20, b.width / 2), b.top + b.height / 2);
        return { ok: !!top && (top === els[0] || els[0].contains(top)), on: top ? top.className : null };
      });
      assert(hint, 'no "Zoom in to 13+" line in the control');
      assert(hint.ok, 'the "Zoom in to 13+" line is under ' + hint.on);
      assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
    } finally { await ctx.close(); }
  });

  // ── 2. finding 2: the antenna-site popup ─────────────────────────────────
  async function openSite(page, touch) {
    await page.waitForFunction(() => (window.__mcMaps || []).some((m) => m._mcMasts && m._mcMasts.stats && !m._mcMasts.stats().pending), null, { timeout: 10000 }).catch(() => {});
    await page.waitForTimeout(800);
    const p = await page.evaluate(({ lat, lon }) => {
      const m = (window.__mcMaps || []).filter((x) => x._mcMasts).pop();
      const pt = m.latLngToContainerPoint([lat, lon]);
      const r = m.getContainer().getBoundingClientRect();
      return { x: r.left + pt.x, y: r.top + pt.y };
    }, SITE);
    if (touch) await tapAt(page, p); else await page.mouse.click(p.x, p.y);
    await page.waitForSelector('.mc-masts-popup', { timeout: 4000 });
    await page.waitForTimeout(700); // the auto-pan
  }
  // Rectangles of the map's buttons and panels that the popup must not cover.
  async function popupGeometry(page) {
    return page.evaluate(() => {
      const rect = (el) => { const b = el.getBoundingClientRect(); return { l: b.left, t: b.top, r: b.right, b: b.bottom, w: b.width, h: b.height }; };
      const wrap = rect(document.querySelector('.mc-masts-popup .leaflet-popup-content-wrapper'));
      const chrome = Array.from(document.querySelectorAll('.leaflet-control:not(.leaflet-control-attribution), #mapControlsToggle, .map-controls'))
        .map((el) => ({ el: el.id || el.className.split(' ').slice(0, 2).join('.'), r: rect(el) })).filter((c) => c.r.w && c.r.h);
      const covered = chrome.filter((c) => c.r.l < wrap.r && wrap.l < c.r.r && c.r.t < wrap.b && wrap.t < c.r.b).map((c) => c.el);
      const map = rect(document.querySelector('.leaflet-container'));
      const scrolled = document.querySelector('.mc-masts-popup .leaflet-popup-content').classList.contains('leaflet-popup-scrolled');
      return { wrap, covered, map, scrolled };
    });
  }
  for (const v of PHONES) {
    await step(`finding 2, ${v.name}: the antenna popup fits, its source link can be reached and its ✕ closes it`, async () => {
      const { ctx, page, rec } = await newCtx({ viewport: { width: v.width, height: v.height } });
      try {
        await gotoMap(page, '');
        await openSite(page, true);
        const g = await popupGeometry(page);
        assert(g.covered.length === 0, 'the popup is under ' + g.covered.join(', ') + ': ' + JSON.stringify(g.wrap));
        assert(g.wrap.t >= g.map.t, 'the popup starts above the map');
        const floor = await bottomChromeTop(page);
        assert(g.wrap.b <= floor, `the popup ends at y=${Math.round(g.wrap.b)}, under the credit/nav at y=${Math.round(floor)}`);
        const close = await hitsItself(page, '.mc-masts-popup .leaflet-popup-close-button');
        assert(close.ok, 'a tap on the ✕ lands on ' + close.on);
        if (g.scrolled) await swipeUp(page, await center(page, '.mc-masts-popup .leaflet-popup-content'), 2000);
        const link = await hitsItself(page, '.mc-masts-popup a[href="https://mastedatabasen.dk/"]');
        assert(link.ok, 'the source link cannot be reached: a tap lands on ' + link.on);
        await tapAt(page, await center(page, '.mc-masts-popup .leaflet-popup-close-button'));
        await page.waitForSelector('.mc-masts-popup', { state: 'detached', timeout: 3000 });
        assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
      } finally { await ctx.close(); }
    });
  }
  await step('finding 2, 1440x900: the popup shows whole, unscrolled, clear of the Map Controls panel', async () => {
    const { ctx, page, rec } = await newCtx({ viewport: { width: 1440, height: 900 }, touch: false });
    try {
      await gotoMap(page, '');
      await openSite(page, false);
      const g = await popupGeometry(page);
      assert(!g.scrolled, 'the desktop popup scrolls');
      assert(g.covered.length === 0, 'the popup is under ' + g.covered.join(', '));
      assert(g.wrap.b <= await bottomChromeTop(page), 'the popup runs under the credit');
      await page.click('.mc-masts-popup .leaflet-popup-close-button');
      await page.waitForSelector('.mc-masts-popup', { state: 'detached', timeout: 3000 });
      assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
    } finally { await ctx.close(); }
  });

  // ── 3. finding 7: the tap that closes the control is no monument lookup ──
  for (const v of [PHONES[0], PHONES[2]]) {
    await step(`finding 7, ${v.name}: closing the layer control with a map tap asks for no monument info`, async () => {
      const { ctx, page, rec } = await newCtx({ viewport: { width: v.width, height: v.height }, noSites: true });
      try {
        await gotoMap(page, '&heritage=1');
        await page.tap('.leaflet-control-layers-toggle');
        await page.waitForSelector('.leaflet-control-layers-expanded', { timeout: 3000 });
        // A spot of bare map to the right of the open control.
        const spot = await page.evaluate(() => {
          const c = document.querySelector('.leaflet-control-layers').getBoundingClientRect();
          const m = document.querySelector('.leaflet-container').getBoundingClientRect();
          return { x: Math.round((c.right + m.right) / 2), y: Math.round(m.top + m.height * 0.55) };
        });
        await tapAt(page, spot);
        await page.waitForTimeout(800);
        assert(!(await page.$('.leaflet-control-layers-expanded')), 'the tap did not close the control');
        assert(rec.heritage === 0, 'the closing tap asked for monument info ' + rec.heritage + ' time(s)');
        assert(!(await page.$('.mc-heritage-popup')), 'the closing tap opened a monument popup');
        await tapAt(page, spot);
        await page.waitForSelector('.mc-heritage-popup', { timeout: 4000 });
        assert(rec.heritage === 1, 'the next tap did not ask for monument info: ' + rec.heritage);
        assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
      } finally { await ctx.close(); }
    });
  }

  // ── 4. finding 8: Reset All forgets the layer control's choices ──────────
  await step('finding 8, 1440x900: hillshade and monuments picked in the layer control are off after Reset All and a reload', async () => {
    const { ctx, page, rec } = await newCtx({ viewport: { width: 1440, height: 900 }, touch: false });
    try {
      await gotoMap(page, '');
      const tg = await center(page, '.leaflet-control-layers-toggle');
      await page.mouse.move(tg.x, tg.y);
      await page.waitForSelector('.leaflet-control-layers-expanded', { timeout: 3000 });
      await page.click('.leaflet-control-layers-overlays label[data-overlay-id="dhm-surface"]');
      await page.click('.leaflet-control-layers-overlays label[data-overlay-id="heritage-monuments"]');
      await page.waitForTimeout(500);
      const before = await page.evaluate(() => ({ s: localStorage.getItem('mc-terrain-dhm'), h: localStorage.getItem('mc-heritage'), hash: location.hash }));
      assert(before.s === 'surface' && /terrain=surface/.test(before.hash) && /heritage=1/.test(before.hash), 'the picks were not made: ' + JSON.stringify(before));
      await page.mouse.move(700, 500);
      await page.click('#customizeToggle');
      await page.waitForSelector('.cust-overlay:not(.hidden)', { timeout: 5000 });
      // A customizer change too, as in the finding: Reset All is offered
      // only when there is one.
      await page.click('.cust-tab[data-tab="display"]');
      await page.selectOption('#cv2-light-tile-provider', 'dataforsyningen-skaermkort-daempet');
      await page.waitForTimeout(400);
      await page.click('.cust-tab[data-tab="export"]');
      await page.click('#cv2ResetAll');
      await page.waitForTimeout(600);
      await page.reload({ waitUntil: 'domcontentloaded' });
      await page.waitForSelector('.leaflet-control-layers-overlays label[data-overlay-id="dhm-surface"]', { state: 'attached', timeout: 20000 });
      await page.waitForTimeout(1200);
      const after = await page.evaluate(() => {
        const m = (window.__mcMaps || []).filter((x) => x._mcSurfaceLayer).pop();
        return {
          surfaceOn: !!(m && m.hasLayer(m._mcSurfaceLayer)), heritageOn: !!(m && m._mcHeritageLayer && m.hasLayer(m._mcHeritageLayer)),
          s: localStorage.getItem('mc-terrain-dhm'), h: localStorage.getItem('mc-heritage'), hash: location.hash,
          light: localStorage.getItem('mc-light-tile-provider'),
        };
      });
      assert(!after.surfaceOn && after.s === null, 'the surface hillshade survived Reset All: ' + JSON.stringify(after));
      assert(!after.heritageOn && after.h === null, 'the monuments survived Reset All: ' + JSON.stringify(after));
      assert(!/terrain=|heritage=/.test(after.hash), 'the link still carries the picks: ' + after.hash);
      assert(/lat=/.test(after.hash), 'Reset All dropped the rest of the link: ' + after.hash);
      assert(after.light === null, 'the customizer pick survived Reset All');
      assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
    } finally { await ctx.close(); }
  });

  // ── 5. finding 9: no colour-input warnings ───────────────────────────────
  for (const v of [
    { name: '1440x900 light', viewport: { width: 1440, height: 900 }, theme: 'light', touch: false },
    { name: '1440x900 dark', viewport: { width: 1440, height: 900 }, theme: 'dark', touch: false },
    { name: '375x560 light', viewport: { width: 375, height: 560 }, theme: 'light', touch: true },
  ]) {
    await step(`finding 9, ${v.name}: the customizer and every tab open without colour-format warnings`, async () => {
      const { ctx, page, rec } = await newCtx(v);
      try {
        await gotoMap(page, '');
        if (v.touch) {
          await page.tap('[data-bottom-nav-tab="more"]');
          await page.waitForSelector('[data-mpa-mirror="customizeToggle"]', { state: 'visible', timeout: 5000 });
          await page.tap('[data-mpa-mirror="customizeToggle"]');
        } else {
          await page.click('#customizeToggle');
        }
        await page.waitForSelector('.cust-overlay:not(.hidden)', { timeout: 5000 });
        const tabs = await page.$$eval('.cust-tab[data-tab]', (ts) => ts.map((t) => t.getAttribute('data-tab')));
        assert(tabs.length >= 5, 'tabs: ' + tabs.join(','));
        for (const t of tabs) {
          const sel = `.cust-tab[data-tab="${t}"]`;
          if (v.touch) { await page.locator(sel).scrollIntoViewIfNeeded(); await page.tap(sel); } else await page.click(sel);
          await page.waitForTimeout(150);
        }
        const n = await page.$$eval('input[type="color"]', (is) => is.length);
        assert(n > 10, 'only ' + n + ' colour inputs rendered');
        assert(rec.warnings.length === 0, rec.warnings.length + ' warning(s): ' + rec.warnings.slice(0, 3).join(' | '));
        assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
      } finally { await ctx.close(); }
    });
  }

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
