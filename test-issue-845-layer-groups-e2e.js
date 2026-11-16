/**
 * E2E (#845): the Map layer control's "Denmark" group and theme filter, and
 * CARTO off by default.
 *
 * Every scenario is a fresh browser context (cold start, empty storage),
 * driven by real taps (touch emulation) on phones and real clicks at 1440.
 * Nothing leaves the machine: /api/config/client is intercepted and given
 * staging-like blocks (Danish base maps with a dummy token, DHM overlays
 * with Aerial, monuments, antenna sites), and every third-party image is a
 * 1x1 PNG.
 *
 * Asserts, at 375x560, 360x640, 640x360 and 1440x900:
 *   1. every row of the open control (the Denmark button included) can be
 *      brought into view inside the list and takes a tap there; no row says
 *      "(Denmark)" or "(dark)"; the contour zoom hint is apart, at the
 *      row's right;
 *   2. the Denmark group starts open over Denmark; a tap on its button
 *      collapses it (aria-expanded="false", its rows hidden) and another
 *      opens it; the state survives a reload;
 *   3. one base map stays selected across the two groups: a Danish map,
 *      then an international one, then Aerial;
 *   4. the theme switch (the app's own toggle, tapped) lists only the new
 *      theme's variants and moves the pick to its counterpart, and back;
 *      the pick survives a reload of the same theme.
 * And:
 *   5. started over Berlin with no saved state, the group is collapsed;
 *   6. with no Danish base map and no overlay offered, there is no group;
 *      with overlays only, there is no "Base maps" heading;
 *   7. CARTO is not listed by default, and a saved "carto-dark" pick (Esri
 *      on) shows Esri Dark Gray Canvas with no request to cartocdn.com;
 *   8. no console errors anywhere.
 *
 * Usage:
 *   BASE_URL=http://localhost:13845 node test-issue-845-layer-groups-e2e.js
 *   (E2E_845_ONLY=<regex> runs only the cases whose name matches.)
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13845';
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

let passed = 0, failed = 0;
// E2E_845_ONLY=<regex> runs the matching cases only (for mutation runs).
const ONLY = process.env.E2E_845_ONLY ? new RegExp(process.env.E2E_845_ONLY) : null;
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
  console.log(`\n=== #845 layer control: Denmark group against ${BASE} ===\n`);

  async function newCtx(o) {
    o = o || {};
    const v = o.view || VIEWS[0];
    const ctx = await browser.newContext({
      viewport: { width: v.width, height: v.height }, hasTouch: v.touch, isMobile: v.touch, colorScheme: 'light',
    });
    const rec = { errors: [], carto: 0 };
    await ctx.route(BASE + '/api/config/client', async (route) => {
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
    await ctx.addInitScript((st) => {
      try {
        ['row-swipe', 'tab-swipe', 'edge-drawer', 'pull-refresh']
          .forEach((k) => localStorage.setItem('meshcore-gesture-hints-' + k, 'seen'));
        if (!sessionStorage.getItem('e2e-845-seeded')) {
          sessionStorage.setItem('e2e-845-seeded', '1');
          Object.keys(st || {}).forEach((k) => localStorage.setItem(k, st[k]));
        }
      } catch (_) {}
    }, o.storage || {});
    const page = await ctx.newPage();
    page.on('console', (m) => { if (m.type() === 'error') rec.errors.push(m.text()); });
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

  for (const view of VIEWS) {
    // ── 1. every row in reach ──────────────────────────────────────────────
    await step(`${view.name}: every row of the open control can be reached and takes a tap; labels clean`, async () => {
      const { ctx, page, rec } = await newCtx({ view });
      try {
        await gotoMap(page);
        await openControl(page, view);
        const ids = await page.evaluate(() => {
          const els = Array.from(document.querySelectorAll('.leaflet-control-layers-list label, .mc-layers-dk-toggle'));
          els.forEach((el, i) => el.setAttribute('data-e2e-row', String(i)));
          return els.length;
        });
        assert(ids >= 14, 'only ' + ids + ' rows');
        for (let i = 0; i < ids; i++) {
          const sel = `[data-e2e-row="${i}"]`;
          await bringIntoList(page, view, sel);
          const hit = await hitsItself(page, sel);
          assert(hit.ok, 'row ' + i + ' is under ' + hit.on);
        }
        const text = await page.evaluate((s) => document.querySelector(s).textContent, LIST);
        assert(!/\(Denmark\)|\(dark\)|\(DHM/.test(text), 'a label still says (Denmark)/(dark)/(DHM): ' + text);
        const hint = await page.evaluate(() => {
          const l = document.querySelector('label[data-overlay-id="dhm-contours"]');
          const h = l && l.querySelector('.mc-layer-hint');
          if (!h) return null;
          return { text: h.textContent, gap: Math.round(l.getBoundingClientRect().right - h.getBoundingClientRect().right), name: l.querySelector('.mc-layer-name').textContent.trim() };
        });
        assert(hint && hint.text === 'zoom 14+' && hint.name === 'Contour lines', 'contour row: ' + JSON.stringify(hint));
        assert(hint.gap <= 12, 'the zoom hint is not at the row\'s right: ' + hint.gap + ' px short');
        assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
      } finally { await ctx.close(); }
    });

    // ── 2. the group toggles, and remembers ────────────────────────────────
    await step(`${view.name}: the Denmark group starts open over Denmark, toggles by tap, and a reload keeps it`, async () => {
      const { ctx, page, rec } = await newCtx({ view });
      try {
        await gotoMap(page);
        await openControl(page, view);
        let s = await dkState(page);
        assert(s && s.expanded === 'true' && !s.bodyHidden && s.bodyVisible, 'not open over Denmark: ' + JSON.stringify(s));
        const tag = await page.evaluate((b) => document.querySelector(b).tagName, DK_BTN);
        assert(tag === 'BUTTON', 'the toggle is a ' + tag);
        await bringIntoList(page, view, DK_BTN);
        await tap(page, view, DK_BTN);
        s = await dkState(page);
        assert(s.expanded === 'false' && s.bodyHidden && !s.bodyVisible, 'did not collapse: ' + JSON.stringify(s));
        assert(await page.$('.leaflet-control-layers-expanded'), 'the tap closed the whole control');
        await page.reload({ waitUntil: 'domcontentloaded' });
        await page.waitForSelector('.leaflet-control-layers-toggle', { timeout: 20000 });
        await page.waitForTimeout(1200);
        await openControl(page, view);
        s = await dkState(page);
        assert(s.expanded === 'false' && s.bodyHidden, 'the reload reopened it: ' + JSON.stringify(s));
        await bringIntoList(page, view, DK_BTN);
        await tap(page, view, DK_BTN);
        s = await dkState(page);
        assert(s.expanded === 'true' && s.bodyVisible, 'did not open again: ' + JSON.stringify(s));
        assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
      } finally { await ctx.close(); }
    });

    // ── 3. one base map selected across both groups ────────────────────────
    await step(`${view.name}: exactly one base map stays selected across both groups`, async () => {
      const { ctx, page, rec } = await newCtx({ view });
      try {
        await gotoMap(page);
        await openControl(page, view);
        for (const id of ['dataforsyningen-skaermkort-graa', 'esri-imagery', 'dhm-ortho', 'opentopomap']) {
          await pickBase(page, view, id);
          const rows = await baseRows(page);
          const on = rows.filter((r) => r.checked).map((r) => r.id);
          assert(on.length === 1 && on[0] === id, 'after ' + id + ' checked: ' + on.join(','));
        }
        const rows = await baseRows(page);
        assert(rows.find((r) => r.id === 'dhm-ortho').dk && rows.find((r) => r.id === 'dataforsyningen-skaermkort-graa').dk, 'Danish rows outside the group');
        assert(!rows.find((r) => r.id === 'esri-imagery').dk && !rows.find((r) => r.id === '__auto__').dk, 'international rows inside the group');
        assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
      } finally { await ctx.close(); }
    });

    // ── 4. the theme switch ────────────────────────────────────────────────
    await step(`${view.name}: the theme switch lists the new theme's variants and keeps the pick (counterpart), across a reload`, async () => {
      const { ctx, page, rec } = await newCtx({ view });
      try {
        await gotoMap(page);
        await openControl(page, view);
        await pickBase(page, view, 'dataforsyningen-skaermkort-graa');
        let rows = await baseRows(page);
        assert(rows.some((r) => r.id === 'opentopomap') && !rows.some((r) => r.id === 'osm-dark'), 'light list: ' + rows.map((r) => r.id));
        await toggleTheme(page, view);
        assert(await theme(page) === 'dark', 'the theme did not switch');
        await openControl(page, view);
        rows = await baseRows(page);
        const ids = rows.map((r) => r.id);
        assert(ids.includes('osm-dark') && ids.includes('esri-darkgray-labels') && !ids.includes('osm-standard') && !ids.includes('opentopomap'), 'dark list: ' + ids);
        const on = rows.filter((r) => r.checked).map((r) => r.id);
        assert(on.length === 1 && on[0] === 'dataforsyningen-skaermkort-graa-dark', 'dark pick: ' + on);
        const filter = await page.evaluate(() => document.querySelector('.leaflet-tile-pane').style.filter);
        assert(/invert/.test(filter), 'the dark variant is not inverted: ' + filter);
        await toggleTheme(page, view);
        assert(await theme(page) === 'light', 'the theme did not switch back');
        await openControl(page, view);
        rows = await baseRows(page);
        const back = rows.filter((r) => r.checked).map((r) => r.id);
        assert(back.length === 1 && back[0] === 'dataforsyningen-skaermkort-graa', 'light pick: ' + back);
        assert(!rows.some((r) => r.id === 'osm-dark'), 'dark ids in the light list');
        assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
      } finally { await ctx.close(); }
    });
  }

  // ── 2b. reload restores an Aerial pick and the theme's list ──────────────
  await step('375x560: a reload restores the Aerial pick (in the open Denmark group) and the dark list', async () => {
    const view = VIEWS[0];
    const { ctx, page, rec } = await newCtx({ view });
    try {
      await gotoMap(page);
      await openControl(page, view);
      await pickBase(page, view, 'dhm-ortho');
      await toggleTheme(page, view);
      await page.reload({ waitUntil: 'domcontentloaded' });
      await page.waitForSelector('.leaflet-control-layers-toggle', { timeout: 20000 });
      await page.waitForTimeout(1200);
      assert(await theme(page) === 'dark', 'theme lost');
      await openControl(page, view);
      const rows = await baseRows(page);
      const on = rows.filter((r) => r.checked).map((r) => r.id);
      assert(on.length === 1 && on[0] === 'dhm-ortho', 'after reload: ' + on);
      assert(!rows.some((r) => r.id === 'osm-standard'), 'light ids in the dark list');
      assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
    } finally { await ctx.close(); }
  });

  // ── 5. outside Denmark ───────────────────────────────────────────────────
  await step('375x560 over Berlin, nothing saved: the Denmark group starts collapsed', async () => {
    const view = VIEWS[0];
    const { ctx, page } = await newCtx({ view });
    try {
      await gotoMap(page, BERLIN);
      await openControl(page, view);
      const s = await dkState(page);
      assert(s && s.expanded === 'false' && s.bodyHidden, JSON.stringify(s));
    } finally { await ctx.close(); }
  });

  // ── 6. gates ─────────────────────────────────────────────────────────────
  await step('1440: no Danish base map and no overlay offered — no Denmark group at all', async () => {
    const view = VIEWS[3];
    const { ctx, page, rec } = await newCtx({ view, providers: { esri: { enabled: true } }, noDhm: true, noOverlays: true });
    try {
      await gotoMap(page);
      await openControl(page, view);
      const g = await page.evaluate((s) => { const el = document.querySelector(s); return el ? { hidden: el.hidden, h: el.getBoundingClientRect().height } : null; }, DK);
      assert(!g || (g.hidden && g.h === 0), 'an empty Denmark group is shown: ' + JSON.stringify(g));
      const rows = await baseRows(page);
      assert(!rows.some((r) => /^dataforsyningen|dhm-ortho/.test(r.id)), 'gated rows listed');
      assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
    } finally { await ctx.close(); }
  });

  await step('1440: overlays only — the group shows its layers, without a "Base maps" heading', async () => {
    const view = VIEWS[3];
    const { ctx, page } = await newCtx({ view, providers: { esri: { enabled: true } }, noDhm: true });
    try {
      await gotoMap(page);
      await openControl(page, view);
      const heads = await page.evaluate(() => Array.from(document.querySelectorAll('.mc-layers-dk-head'))
        .filter((h) => h.getBoundingClientRect().height > 0).map((h) => h.textContent));
      assert(heads.join('|') === 'Layers', 'headings: ' + heads.join('|'));
      assert(await page.$('.mc-layers-dk-overlays label[data-overlay-id="heritage"], .mc-layers-dk-overlays label[data-overlay-id="masts-sites"]'), 'no overlay row in the group');
    } finally { await ctx.close(); }
  });

  // ── 7. CARTO ─────────────────────────────────────────────────────────────
  await step('1440: CARTO is not listed by default; a saved carto-dark pick shows Esri Dark Gray Canvas, no cartocdn request', async () => {
    const view = VIEWS[3];
    const { ctx, page, rec } = await newCtx({ view, storage: { 'meshcore-theme': 'dark', 'mc-dark-tile-provider': 'carto-dark' } });
    try {
      await gotoMap(page);
      assert(await theme(page) === 'dark', 'not dark');
      await openControl(page, view);
      const rows = await baseRows(page);
      assert(!rows.some((r) => /carto|positron/.test(r.id) || /Carto/.test(r.text)), 'CARTO listed: ' + rows.map((r) => r.id));
      const dark = await page.evaluate(() => window.MC_getDarkTileProvider());
      assert(dark === 'esri-darkgray-labels', 'resolved to ' + dark);
      const tiles = await page.evaluate(() => Array.from(document.querySelectorAll('.leaflet-tile-pane img')).map((i) => i.src).slice(0, 3));
      assert(tiles.length && tiles.every((u) => /arcgisonline/.test(u)), 'tiles: ' + tiles.join(' '));
      assert(rec.carto === 0, rec.carto + ' request(s) to cartocdn.com');
      assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
    } finally { await ctx.close(); }
  });

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
