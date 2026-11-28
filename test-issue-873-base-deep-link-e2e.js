/**
 * E2E (#873): the base map deep link, #/map?base=<style id> and #/live?base=.
 *
 * Every scenario is a fresh browser context (cold start, empty storage),
 * driven by real taps (touch emulation) at 375x560 and real clicks at 1440.
 * Nothing leaves the machine: /api/config/client is intercepted and given
 * a staging-like block, and every third-party image is a 1x1 PNG.
 *
 *   1. cold open on #/map?base=esri-imagery: that base map is the selected
 *      one and the link keeps it;
 *   2. a gated link (Dataforsyningen with no token, a style that does not
 *      exist) opens on Auto, with base= removed from the address bar;
 *   3. a real tap on a base map writes base=<id> into the address bar with
 *      no new history entry; a tap on Auto takes it out again;
 *   4. the link wins over a stored Aerial pick;
 *   5. #/live?base=<id> opens the Live map on that base map;
 *   6. the client config answering late (400 ms and 1500 ms, on Map and on
 *      Live): the link still wins and stays in the address bar;
 *   7. a rejected link (unknown, gated) logs exactly one console warning
 *      naming it, and a valid link logs none; a late config does not
 *      change that.
 *
 * Usage:  BASE_URL=http://localhost:13873 node test-issue-873-base-deep-link-e2e.js
 *         (E2E_873_ONLY=<regex> runs only the cases whose name matches.)
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13873';
const PNG_1X1 = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg==', 'base64');

const DHM_ATTR = 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0';
const LICENSE_URL = 'https://creativecommons.org/licenses/by/4.0/';
const DK_BOUNDS = [[54.4265, 7.99125], [57.778, 15.5995]];
const DHM_BLOCK = {
  enabled: true, url: '/api/map/dhm/wms', layer: 'dhm_terraen_skyggekort', minZoom: 7, maxZoom: 19,
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
    licenseName: 'CC BY 4.0', licenseUrl: LICENSE_URL,
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
const PROVIDERS = { dataforsyningen: { enabled: true, token: 'test-token' }, esri: { enabled: true }, opentopomap: { enabled: true } };

const JELLING = { lat: 55.7565, lon: 9.4195 };
const BERLIN = { lat: 52.52, lon: 13.4 };
const VIEWS = [
  { name: '375x560', width: 375, height: 560, touch: true },
  { name: '360x640', width: 360, height: 640, touch: true },
  { name: '640x360', width: 640, height: 360, touch: true },
  { name: '1440x900', width: 1440, height: 900, touch: false },
];
const LIST = '.leaflet-control-layers-list';
const DK = '.mc-layers-dk';
const DK_BTN = '.mc-layers-dk-toggle';
const row = (id) => `.leaflet-control-layers-base label[data-tile-id="${id}"]`;
// #859 — the smallest row height on a coarse pointer (the Denmark toggle's
// own min-height in public/style.css).
const TOUCH_ROW_MIN = 32;

let passed = 0, failed = 0;
// E2E_873_ONLY=<regex> runs the matching cases only (for mutation runs).
const ONLY = process.env.E2E_873_ONLY ? new RegExp(process.env.E2E_873_ONLY) : null;
async function step(name, fn) {
  if (ONLY && !ONLY.test(name)) return;
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
  console.log(`\n=== #873 base map deep link against ${BASE} ===\n`);

  async function newCtx(o) {
    o = o || {};
    const v = o.view || VIEWS[0];
    const ctx = await browser.newContext({
      viewport: { width: v.width, height: v.height }, hasTouch: v.touch, isMobile: v.touch, colorScheme: 'light',
    });
    const rec = { errors: [], warnings: [], carto: 0 };
    await ctx.route(BASE + '/api/config/client', async (route) => {
      if (o.configDelayMs) await new Promise((r) => setTimeout(r, o.configDelayMs));
      const response = await route.fetch();
      const config = await response.json();
      const providers = o.providers || PROVIDERS;
      config.map = Object.assign({}, config.map, { tiles: { providers } });
      if (!o.noDhm) config.dhm = DHM_BLOCK;
      if (!o.noOverlays) { config.heritage = HERITAGE_BLOCK; config.masts = MASTS_BLOCK; }
      await route.fulfill({ response, json: config });
    });
    await ctx.route((u) => new URL(u).pathname === MASTS_BLOCK.url, (route) =>
      route.fulfill({ status: 200, json: { cell: { e: 0, n: 0 }, truncated: false, sites: [] } }));
    await ctx.route((u) => /^\/api\/map\/dhm\//.test(new URL(u).pathname), (route) =>
      route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 }));
    await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, async (route) => {
      if (/cartocdn\.com/.test(route.request().url())) rec.carto++;
      if (route.request().resourceType() === 'image') {
        await route.fulfill({ status: 200, contentType: 'image/png', headers: { 'Access-Control-Allow-Origin': '*' }, body: PNG_1X1 });
        return;
      }
      await route.abort();
    });
    // #859 — keeps every Leaflet map the page makes, so a case can make the
    // layer control re-render the way Leaflet does (window.L is set once,
    // by the vendored leaflet.js).
    if (o.captureMaps) {
      await ctx.addInitScript(() => {
        let real;
        Object.defineProperty(window, 'L', {
          configurable: true,
          get() { return real; },
          set(v) {
            real = v;
            try { v.Map.addInitHook(function () { (window.__e2eMaps = window.__e2eMaps || []).push(this); }); } catch (_) {}
          },
        });
      });
    }
    await ctx.addInitScript((st) => {
      try {
        ['row-swipe', 'tab-swipe', 'edge-drawer', 'pull-refresh']
          .forEach((k) => localStorage.setItem('meshcore-gesture-hints-' + k, 'seen'));
        if (!sessionStorage.getItem('e2e-873-seeded')) {
          sessionStorage.setItem('e2e-873-seeded', '1');
          Object.keys(st || {}).forEach((k) => localStorage.setItem(k, st[k]));
        }
      } catch (_) {}
    }, o.storage || {});
    const page = await ctx.newPage();
    page.on('console', (m) => {
      if (m.type() === 'error') rec.errors.push(m.text());
      if (m.type() === 'warning' && /\[map tiles\] \?base=/.test(m.text())) rec.warnings.push(m.text());
    });
    page.on('pageerror', (e) => rec.errors.push(String(e)));
    page.on('dialog', (d) => d.accept());
    return { ctx, page, rec, view: v };
  }

  async function gotoMap(page, at, extra) {
    at = at || JELLING;
    await page.goto(`${BASE}/#/map?lat=${at.lat}&lon=${at.lon}&zoom=15${extra || ''}`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('.leaflet-control-layers-toggle', { timeout: 20000 });
    await page.evaluate(() => window.MeshConfigReady).catch(() => {});
    await page.waitForTimeout(1200);
  }
  async function tap(page, view, sel) {
    const b = await page.locator(sel).first().boundingBox();
    assert(b, 'not on the page: ' + sel);
    const x = Math.round(b.x + Math.min(b.width / 2, 40)), y = Math.round(b.y + b.height / 2);
    if (view.touch) await page.touchscreen.tap(x, y); else await page.mouse.click(x, y);
    await page.waitForTimeout(300);
  }
  async function openControl(page, view) {
    if (await page.$('.leaflet-control-layers-expanded')) return;
    if (view.touch) await page.tap('.leaflet-control-layers-toggle');
    else {
      // A real pointer onto the toggle (Leaflet opens it on mouseenter;
      // page.hover would wait for the opened list to stop covering it).
      const b = await page.locator('.leaflet-control-layers-toggle').boundingBox();
      await page.mouse.move(Math.round(b.x + b.width / 2), Math.round(b.y + b.height / 2));
    }
    await page.waitForSelector('.leaflet-control-layers-expanded', { timeout: 3000 });
    await page.waitForTimeout(200);
  }
  // Inside the list's visible box, and how far below or above it.
  async function placement(page, target) {
    return page.evaluate(({ b, t }) => {
      const c = document.querySelector(b).getBoundingClientRect();
      const r = document.querySelector(t).getBoundingClientRect();
      return { inside: r.top >= c.top - 1 && r.bottom <= c.bottom + 1, below: Math.round(r.bottom - c.bottom) };
    }, { b: LIST, t: target });
  }
  /* Brings `target` into view inside the list as a viewer would: real touch
   * swipes on phones (stepped CDP touches, as #839's suite), the wheel on
   * the desktop. Nothing scrolls the list for it. */
  async function bringIntoList(page, view, target) {
    let p = await placement(page, target);
    if (p.inside) return;
    const cdp = view.touch ? await page.context().newCDPSession(page) : null;
    try {
      for (let n = 0; n < 40 && !p.inside; n++) {
        const b = await page.locator(LIST).boundingBox();
        const x = Math.round(b.x + b.width / 2), dist = Math.max(30, Math.round(b.height / 3)), dir = p.below > 0 ? -1 : 1;
        if (cdp) {
          const y0 = Math.round(b.y + b.height / 2 + dist / 2);
          await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x, y: y0 }] });
          for (let k = 1; k <= 10; k++) {
            await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x, y: y0 + Math.round(dir * dist * k / 10) }] });
          }
          await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
        } else {
          await page.mouse.move(x, Math.round(b.y + b.height / 2));
          await page.mouse.wheel(0, -dir * dist);
        }
        await page.waitForTimeout(150);
        p = await placement(page, target);
      }
    } finally { if (cdp) await cdp.detach(); }
    assert(p.inside, 'could not bring ' + target + ' into view inside the list: ' + JSON.stringify(p));
  }
  async function hitsItself(page, sel) {
    return page.evaluate((s) => {
      const el = document.querySelector(s);
      const b = el.getBoundingClientRect();
      const top = document.elementFromPoint(b.left + Math.min(b.width / 2, 40), b.top + b.height / 2);
      return { ok: !!top && (top === el || el.contains(top)), on: top ? (top.id || top.className || top.tagName) : null };
    }, sel);
  }
  // The base-map rows: id, checked, and whether they sit in the Denmark group.
  async function baseRows(page) {
    return page.evaluate(() => Array.from(document.querySelectorAll('.leaflet-control-layers-base label[data-tile-id]')).map((l) => ({
      id: l.getAttribute('data-tile-id'), checked: l.querySelector('input').checked,
      dk: !!l.closest('.mc-layers-dk'), text: l.textContent.trim(),
    })));
  }
  async function pickBase(page, view, id) {
    await bringIntoList(page, view, row(id));
    await tap(page, view, row(id));
    await page.waitForTimeout(300);
  }
  async function toggleTheme(page, view) {
    if (view.touch) {
      // The page's own control, as a viewer reaches it on a phone.
      await page.tap('[data-bottom-nav-tab="more"]');
      await page.waitForSelector('[data-bottom-nav-dark-toggle]', { state: 'visible', timeout: 3000 });
      await page.tap('[data-bottom-nav-dark-toggle]');
    } else {
      await page.click('#darkModeToggle');
    }
    await page.waitForTimeout(600);
  }
  const theme = (page) => page.evaluate(() => document.documentElement.getAttribute('data-theme'));
  const dkState = (page) => page.evaluate((s) => {
    const b = document.querySelector(s);
    const body = b && document.getElementById(b.getAttribute('aria-controls'));
    return b ? { expanded: b.getAttribute('aria-expanded'), bodyHidden: !!body && body.hidden, bodyVisible: !!body && body.getBoundingClientRect().height > 0 } : null;
  }, DK_BTN);

  const hashOf = (page) => page.evaluate(() => location.hash);
  const checkedId = async (page) => (await baseRows(page)).filter((r) => r.checked).map((r) => r.id);
  const NO_DK = { esri: { enabled: true }, opentopomap: { enabled: true } };

  for (const view of [VIEWS[0], VIEWS[3]]) {
    await step(`${view.name}: cold open on ?base=esri-imagery selects it and keeps the link`, async () => {
      const { ctx, page, rec } = await newCtx({ view });
      try {
        await gotoMap(page, JELLING, '&base=esri-imagery');
        await openControl(page, view);
        const on = await checkedId(page);
        assert(on.length === 1 && on[0] === 'esri-imagery', 'checked: ' + on);
        assert(/[?&]base=esri-imagery(&|$)/.test(await hashOf(page)), 'hash: ' + await hashOf(page));
        assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
      } finally { await ctx.close(); }
    });

    await step(`${view.name}: gated and unknown links open on Auto and leave a clean address`, async () => {
      for (const id of ['dataforsyningen-skaermkort-graa', 'no-such-style']) {
        const { ctx, page, rec } = await newCtx({ view, providers: NO_DK });
        try {
          await gotoMap(page, JELLING, '&base=' + id);
          await openControl(page, view);
          const on = await checkedId(page);
          assert(on.length === 1 && on[0] === '__auto__', id + ' checked: ' + on);
          assert(!/base=/.test(await hashOf(page)), id + ' hash: ' + await hashOf(page));
          assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
        } finally { await ctx.close(); }
      }
    });

    await step(`${view.name}: a tap on a base map writes base= with no history entry; Auto removes it`, async () => {
      const { ctx, page } = await newCtx({ view });
      try {
        await gotoMap(page);
        await openControl(page, view);
        const len = await page.evaluate(() => history.length);
        await pickBase(page, view, 'esri-imagery');
        assert(/[?&]base=esri-imagery(&|$)/.test(await hashOf(page)), 'after pick: ' + await hashOf(page));
        assert(/lat=/.test(await hashOf(page)), 'the other parameters were lost: ' + await hashOf(page));
        await pickBase(page, view, 'opentopomap');
        assert(/[?&]base=opentopomap(&|$)/.test(await hashOf(page)), 'after second pick: ' + await hashOf(page));
        await pickBase(page, view, '__auto__');
        assert(!/base=/.test(await hashOf(page)), 'after Auto: ' + await hashOf(page));
        assert((await page.evaluate(() => history.length)) === len, 'history entries were added');
      } finally { await ctx.close(); }
    });

    await step(`${view.name}: the link wins over a stored Aerial pick`, async () => {
      const { ctx, page } = await newCtx({ view, storage: { 'mc-ortho-base': '1' } });
      try {
        await gotoMap(page, JELLING, '&base=esri-imagery');
        await openControl(page, view);
        const on = await checkedId(page);
        assert(on.length === 1 && on[0] === 'esri-imagery', 'checked: ' + on);
      } finally { await ctx.close(); }
    });

    await step(`${view.name}: #/live?base=esri-imagery opens the Live map on it`, async () => {
      const { ctx, page } = await newCtx({ view });
      try {
        await page.goto(`${BASE}/#/live?base=esri-imagery`, { waitUntil: 'domcontentloaded' });
        await page.waitForSelector('.leaflet-control-layers-toggle', { timeout: 20000 });
        await page.waitForTimeout(1200);
        await openControl(page, view);
        const on = await checkedId(page);
        assert(on.length === 1 && on[0] === 'esri-imagery', 'checked: ' + on);
      } finally { await ctx.close(); }
    });
  }

  // F1: /api/config/client answers after the control is built. Both a phone
  // from a cold start (real taps) and a desktop, on Map and on Live.
  for (const view of [VIEWS[0], VIEWS[3]]) {
    for (const delay of [400, 1500]) {
      for (const page_ of ['map', 'live']) {
        await step(`${view.name}: config ${delay} ms late, #/${page_}?base=esri-imagery still wins and stays in the address`, async () => {
          const { ctx, page, rec } = await newCtx({ view, configDelayMs: delay });
          try {
            if (page_ === 'map') await gotoMap(page, JELLING, '&base=esri-imagery');
            else {
              await page.goto(`${BASE}/#/live?base=esri-imagery`, { waitUntil: 'domcontentloaded' });
              await page.waitForSelector('.leaflet-control-layers-toggle', { timeout: 20000 });
              await page.evaluate(() => window.MeshConfigReady).catch(() => {});
              await page.waitForTimeout(1200);
            }
            await openControl(page, view);
            const on = await checkedId(page);
            assert(on.length === 1 && on[0] === 'esri-imagery', 'checked: ' + on);
            assert(/[?&]base=esri-imagery(&|$)/.test(await hashOf(page)), 'hash: ' + await hashOf(page));
            if (page_ === 'map') assert(/lat=/.test(await hashOf(page)), 'the other parameters were lost: ' + await hashOf(page));
            assert(rec.warnings.length === 0, 'warned about a valid link: ' + rec.warnings.join(' | '));
            assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
          } finally { await ctx.close(); }
        });
      }
    }
  }

  await step('375x560: config 1500 ms late, the Aerial link (base=dhm-ortho) is honoured too', async () => {
    const { ctx, page } = await newCtx({ view: VIEWS[0], configDelayMs: 1500 });
    try {
      await gotoMap(page, JELLING, '&base=dhm-ortho');
      await openControl(page, VIEWS[0]);
      const on = await checkedId(page);
      assert(on.length === 1 && on[0] === 'dhm-ortho', 'checked: ' + on);
    } finally { await ctx.close(); }
  });

  await step('375x560: config 1500 ms late, a pick made before it arrives is not undone by the link', async () => {
    const { ctx, page } = await newCtx({ view: VIEWS[0], configDelayMs: 1500 });
    try {
      await page.goto(`${BASE}/#/map?lat=${JELLING.lat}&lon=${JELLING.lon}&zoom=15&base=esri-imagery`, { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('.leaflet-control-layers-toggle', { timeout: 20000 });
      await openControl(page, VIEWS[0]);
      await pickBase(page, VIEWS[0], 'osm-standard');
      await page.evaluate(() => window.MeshConfigReady).catch(() => {});
      await page.waitForTimeout(1200);
      await openControl(page, VIEWS[0]);
      const on = await checkedId(page);
      assert(on.length === 1 && on[0] === 'osm-standard', 'checked: ' + on);
      assert(/[?&]base=osm-standard(&|$)/.test(await hashOf(page)), 'hash: ' + await hashOf(page));
    } finally { await ctx.close(); }
  });

  // Review finding 4: a link that cannot be shown is Auto for that open; a
  // stored Aerial pick is neither restored nor deleted. A phone, cold start.
  for (const delay of [0, 1500]) {
    await step(`375x560: a stored Aerial pick plus an unusable link opens on Auto and keeps the pick (config ${delay} ms)`, async () => {
      for (const id of ['no-such-style', 'dataforsyningen-skaermkort-graa']) {
        const { ctx, page } = await newCtx({ view: VIEWS[0], providers: NO_DK, configDelayMs: delay, storage: { 'mc-ortho-base': '1' } });
        try {
          await gotoMap(page, JELLING, '&base=' + id);
          await openControl(page, VIEWS[0]);
          const on = await checkedId(page);
          assert(on.length === 1 && on[0] === '__auto__', id + ' checked: ' + on);
          assert(!/base=/.test(await hashOf(page)), id + ' hash: ' + await hashOf(page));
          assert(await page.evaluate(() => localStorage.getItem('mc-ortho-base')) === '1', id + ': the stored pick was deleted');
          // The next open without a link brings the stored pick back.
          await page.goto('about:blank');
          await gotoMap(page, JELLING, '');
          await openControl(page, VIEWS[0]);
          const again = await checkedId(page);
          assert(again.length === 1 && again[0] === 'dhm-ortho', id + ' next open checked: ' + again);
        } finally { await ctx.close(); }
      }
    });
  }
  // N3: an empty base= is dropped without a warning.
  await step('375x560: an empty base= is removed from the address without a warning', async () => {
    const { ctx, page, rec } = await newCtx({ view: VIEWS[0] });
    try {
      await gotoMap(page, JELLING, '&base=');
      assert(!/base=/.test(await hashOf(page)), 'hash: ' + await hashOf(page));
      assert(rec.warnings.length === 0, JSON.stringify(rec.warnings));
      await openControl(page, VIEWS[0]);
      const on = await checkedId(page);
      assert(on.length === 1 && on[0] === '__auto__', 'checked: ' + on);
    } finally { await ctx.close(); }
  });

  // F2: one console warning per rejected id.
  for (const delay of [0, 1500]) {
    await step(`375x560: a rejected link logs one console warning naming it (config ${delay} ms)`, async () => {
      for (const id of ['no-such-style', 'dataforsyningen-skaermkort-graa']) {
        const { ctx, page, rec } = await newCtx({ view: VIEWS[0], providers: NO_DK, configDelayMs: delay });
        try {
          await gotoMap(page, JELLING, '&base=' + id);
          assert(rec.warnings.length === 1 && rec.warnings[0].indexOf(id) >= 0, id + ' warnings: ' + JSON.stringify(rec.warnings));
          assert(!/base=/.test(await hashOf(page)), id + ' hash: ' + await hashOf(page));
        } finally { await ctx.close(); }
      }
    });
  }
  await step('375x560: a valid link logs no warning', async () => {
    const { ctx, page, rec } = await newCtx({ view: VIEWS[0] });
    try {
      await gotoMap(page, JELLING, '&base=esri-imagery');
      assert(rec.warnings.length === 0, JSON.stringify(rec.warnings));
    } finally { await ctx.close(); }
  });

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
