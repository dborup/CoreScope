/**
 * E2E (#737 "Later (customizer)", AGENTS rule 8): the oblique photo
 * (Skråfoto) viewer's defaults in the customizer, and what the viewer does
 * with them.
 *
 * Nothing leaves the machine: /api/config/client is intercepted and given
 * the `skraafoto` block (and `dhm` with profile: true) in the shapes
 * cmd/server sends; /api/skraafoto/search, the previews and the ?zoom=1
 * crop, and /api/terrain/profile are stubs in the M1/M3a/M3b shapes (the
 * same as test-issue-737-skraafoto-viewer-e2e.js); every other host is
 * refused. The fixture's node used here is moved to Aarhus in the stubbed
 * /api/nodes answers.
 *
 * Asserts:
 *   1. a phone at 375x560, cold start (no storage, no deep link, the
 *      gesture hints dismissed by taps), real taps: More, Customize,
 *      Display; the "Oblique Photos" section; the first photo "west", the
 *      first view "zoomed" and the line of sight off, each control at least
 *      48 px, inside the screen and hit by itself; the section is stored;
 *      nothing overflows sideways;
 *   2. the same phone, by taps: Nodes, the node, "Oblique photo": the
 *      viewer opens looking west, on the crop (no overview fetched first);
 *      a tap on Zoom out shows the overview;
 *   3. back in the customizer by taps, the first photo "auto": from a Path
 *      profile (A, tapped) the viewer shows the side view of A–B, asks the
 *      search for no line and draws none;
 *   4. a deep-linked direction wins over the customizer;
 *   5. the export carries the section and no server key; Reset All
 *      brings M3c back (north, overview, the line asked for);
 *   6. without the server's skraafoto block there is no section;
 *   7. no page errors.
 *
 * SHOTS_DIR=dir also writes screenshots for the PR.
 *
 * Usage:
 *   BASE_URL=http://localhost:13581 node test-issue-737-customizer-skraafoto-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SHOTS = process.env.SHOTS_DIR || '';
const STORE = 'cs-theme-overrides';
const PROFILE_PATH = '/api/terrain/profile';
const SEARCH_PATH = '/api/skraafoto/search';
const PREVIEW_PREFIX = '/api/skraafoto/preview/';
const CREDIT = {
  attribution: 'Klimadatastyrelsen, Skråfotos, CC BY 4.0', owner: 'Klimadatastyrelsen',
  datasetName: 'Skråfotos', datasetUrl: 'https://www.klimadatastyrelsen.dk/kortlaegning/skraafotos',
  licenseName: 'CC BY 4.0', licenseUrl: 'https://creativecommons.org/licenses/by/4.0/',
};
const SF_BLOCK = {
  enabled: true, searchUrl: SEARCH_PATH, previewUrl: '/api/skraafoto/preview/{collection}/{item}',
  maxPx: 1024, note: 'Public aerial photos from Klimadatastyrelsen, shown at reduced size.',
  credit: CREDIT, bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
};
const DHM_BLOCK = {
  enabled: true, url: '/api/map/dhm/wms', layer: 'dhm_terraen_skyggekort',
  minZoom: 7, maxZoom: 19, profile: true, bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
  attribution: 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0',
};
const AARHUS = { lat: 56.15674, lon: 10.21076 };
const PNG_1X1 = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGM4ceIEAAS0AlkWLoFAAAAAAElFTkSuQmCC', 'base64');
const DIRS = ['north', 'east', 'south', 'west', 'nadir'];
const FRAME = { width: 480, height: 320 };
const PT = { x: 130.5, y: 210.2 };
const LINE = [{ x: 20, y: 300 }, { x: 130.5, y: 210.2 }, { x: 470, y: 30 }];
// A north–south profile: its side view looks east.
const P_NS = '56.10000,10.20000,56.20000,10.20000';
const N = 101, D = 8400;

const ACTION = '[data-testid="node-skraafoto"]';
const DIALOG = '[data-testid="sf-dialog"]';
const CLOSE = '[data-testid="sf-close"]';
const IMG = '[data-testid="sf-img"]';
const ZOOM = '[data-testid="sf-zoom"]';
const LOS = '[data-testid="sf-los"]';
const DIR = (d) => `[data-testid="sf-dir-${d}"]`;
const PANEL = '[data-testid="tp-panel"]';
const SF_A = '[data-testid="tp-sf-a"]';
const SEL = (k) => `[data-cv2-skraafoto="${k}"]`;

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); return true; }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); return false; }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

function profileFor(q) {
  const end = (side) => {
    const v = q.get(side) || '';
    const ll = v.split(',').map(Number);
    return { lat: ll[0], lon: ll[1], kind: 'point', groundM: 30, antennaM: 10, antennaSource: 'point-default', assumed: true };
  };
  const samples = [];
  for (let i = 0; i < N; i++) samples.push({ d: D * i / (N - 1), dtm: 30 + 10 * Math.sin(i / 9), dsm: 34 + 10 * Math.sin(i / 9) });
  return {
    a: end('a'), b: end('b'), distanceM: D, stepM: 84, sampleCount: N, model: 'both',
    coverage: { state: 'full', pct: 100, firstGapM: null }, samples,
    physics: { freqMHz: 869.618, freqSource: 'firmware', lambdaM: 0.34474, k: 1.3333333333333333, kSource: 'default', f1MaxM: 13.4 },
    verdict: { state: 'clear', model: 'dtm', criterion: 0.6, worstClearanceM: 12.1, worstAtM: 4200, worstF1Fraction: 1.2 },
    attribution: DHM_BLOCK.attribution,
  };
}

function searchAnswer(q) {
  const year = q.get('year') ? Number(q.get('year')) : 2025;
  const c = 'skraafotos' + year;
  const photos = DIRS.map((d, i) => {
    const id = year + '_83_36_' + (i + 1) + '_0020_0000' + (3045 + i);
    const ph = {
      id, collection: c, direction: d, datetime: year + '-05-07T10:21:33Z',
      viewerUrl: 'https://skraafoto.dataforsyningen.dk/?item=' + id,
      previewUrl: PREVIEW_PREFIX + c + '/' + id, frame: FRAME, point: PT, inFrame: true, approx: false,
    };
    if (q.get('path')) ph.path = LINE; // M3a: only when a path is asked for
    return ph;
  });
  return { collection: c, year, years: [2025, 2023], photos, credit: CREDIT };
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #737 customizer: Skråfoto viewer defaults against ${BASE} ===\n`);

  const JPEG = await (async () => {
    const page = await browser.newPage();
    const out = await page.evaluate((dirs) => {
      const res = {};
      dirs.concat(['crop']).forEach((d, i) => {
        const c = document.createElement('canvas');
        c.width = 480; c.height = 320;
        const g = c.getContext('2d');
        g.fillStyle = 'hsl(' + (i * 70) + ',45%,55%)';
        g.fillRect(0, 0, 480, 320);
        res[d] = c.toDataURL('image/jpeg', 0.8).split(',')[1];
      });
      return res;
    }, DIRS);
    await page.close();
    const buf = {};
    Object.keys(out).forEach(k => { buf[k] = Buffer.from(out[k], 'base64'); });
    return buf;
  })();

  let MOVED_PK = null;
  /* o.sf false: no skraafoto block. o.phone: 375x560 touch. o.storage:
   * localStorage seeded once. */
  async function newCtx(o) {
    o = o || {};
    const ctx = await browser.newContext(o.phone
      ? { viewport: { width: 375, height: 560 }, isMobile: true, hasTouch: true, deviceScaleFactor: 2 }
      : { viewport: { width: 1280, height: 860 } });
    const rec = { search: [], preview: [], zoom: [], external: [], errors: [] };
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      delete config.skraafoto;
      if (o.sf !== false) config.skraafoto = Object.assign({}, SF_BLOCK);
      config.dhm = Object.assign({}, DHM_BLOCK);
      await route.fulfill({ response, json: config });
    });
    await ctx.route(url => new URL(url).pathname === SEARCH_PATH, async (route) => {
      const u = new URL(route.request().url());
      rec.search.push(u);
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(searchAnswer(u.searchParams)) });
    });
    await ctx.route(url => new URL(url).pathname.startsWith(PREVIEW_PREFIX), async (route) => {
      const pu = new URL(route.request().url());
      if (pu.search) {
        rec.zoom.push(pu);
        await route.fulfill({ status: 200, contentType: 'image/jpeg', body: JPEG.crop,
          headers: { 'X-CoreScope-Skraafoto-Marker': '240,160', 'X-CoreScope-Skraafoto-Crop': '70.5,170.2,190.5,250.2' } });
        return;
      }
      rec.preview.push(pu.pathname);
      const dir = DIRS[Number(pu.pathname.split('_')[3]) - 1];
      await route.fulfill({ status: 200, contentType: 'image/jpeg', body: JPEG[dir] });
    });
    await ctx.route(url => new URL(url).pathname === PROFILE_PATH, (route) =>
      route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(profileFor(new URL(route.request().url()).searchParams)) }));
    await ctx.route(url => /^\/api\/nodes(\/|$)/.test(new URL(url).pathname), async (route) => {
      if (!MOVED_PK) { await route.fallback(); return; }
      const response = await route.fetch();
      let body;
      try { body = await response.json(); } catch (_) { await route.fulfill({ response }); return; }
      (function walk(v) {
        if (!v || typeof v !== 'object') return;
        if (Array.isArray(v)) { v.forEach(walk); return; }
        if (typeof v.public_key === 'string' && v.public_key.toLowerCase() === MOVED_PK) { v.lat = AARHUS.lat; v.lon = AARHUS.lon; }
        Object.keys(v).forEach(k => walk(v[k]));
      })(body);
      await route.fulfill({ response, json: body });
    });
    await ctx.route(url => new URL(url).pathname === '/api/map/dhm/wms', (route) =>
      route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 }));
    await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, async (route) => {
      rec.external.push(route.request().url());
      if (route.request().resourceType() === 'image') await route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 });
      else await route.abort();
    });
    await ctx.addInitScript((st) => {
      window.__sfThemeSettled = false;
      window.addEventListener('theme-refresh', () => {
        if (window._customizerV2 && window._customizerV2.initDone) window.__sfThemeSettled = true;
      });
      try {
        if (st && !sessionStorage.getItem('__seeded')) {
          for (const k of Object.keys(st)) localStorage.setItem(k, st[k]);
          sessionStorage.setItem('__seeded', '1');
        }
      } catch (_) {}
    }, o.storage || null);
    const page = await ctx.newPage();
    page.on('pageerror', e => rec.errors.push(String(e)));
    return { ctx, page, rec };
  }

  // A node with a fix and a name, moved to Aarhus in every answer.
  const node = await (async () => {
    const { ctx, page } = await newCtx({ sf: false });
    await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
    const list = await page.evaluate(async () => {
      const r = await fetch('/api/nodes?limit=2000');
      const d = await r.json();
      return (d.nodes || d).map(n => ({ pk: n.public_key, name: n.name, lat: n.lat, lon: n.lon }));
    });
    await ctx.close();
    return list.find(n => /^[0-9a-f]{64}$/i.test(n.pk) && n.name && n.lat != null && n.lon != null && !(n.lat === 0 && n.lon === 0));
  })();
  assert(node, 'the fixture needs a node with a fix');
  const PK = node.pk.toLowerCase();
  MOVED_PK = PK;

  const themeSettled = (page) => page.waitForFunction(() => window.__sfThemeSettled, null, { timeout: 15000 });
  const stored = (page) => page.evaluate((k) => {
    try { return JSON.parse(localStorage.getItem(k) || '{}').skraafoto || null; } catch (_) { return 'unparsable'; }
  }, STORE);
  async function waitReady(page) {
    await page.waitForFunction((sel) => { const d = document.querySelector(sel); return d && d.getAttribute('data-state') === 'ready'; }, DIALOG, { timeout: 10000 });
    await page.waitForFunction((sel) => { const i = document.querySelector(sel); return i && !i.hidden && i.complete && i.naturalWidth > 0; }, IMG, { timeout: 10000 });
  }
  /* The first visit's gesture hints: "Got it" on each, by taps. */
  async function dismissHints(page) {
    const up = await page.waitForSelector('.gesture-hint:not(.gesture-hint-fading)', { timeout: 4000 }).catch(() => null);
    if (!up) return;
    const ids = await page.$$eval('.gesture-hint', els => els.map(e => e.getAttribute('data-gesture-hint')));
    for (const h of ids.reverse()) await page.tap(`[data-gesture-hint="${h}"] [data-gesture-hint-dismiss]`);
    await page.waitForFunction(() => !document.querySelector('.gesture-hint'), null, { timeout: 3000 });
  }
  /* A customizer control: scrolled into view, at least 48 px tall, inside
   * the screen and hit by itself at its centre; then tapped there. */
  async function tapControl(page, sel) {
    await page.locator(sel).scrollIntoViewIfNeeded();
    await page.waitForTimeout(200);
    const vp = page.viewportSize();
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
  const sectionShown = (page) => page.evaluate(() =>
    Array.from(document.querySelectorAll('.cust-section-title')).some(p => p.textContent === 'Oblique Photos'));
  /* A select on a phone: a tap opens the native picker, which cannot be
   * tapped, so the option is then chosen as the picker would. */
  async function pick(page, key, value) {
    await tapControl(page, SEL(key));
    await page.selectOption(SEL(key), value);
    await page.waitForTimeout(500);
  }
  async function shot(page, name) {
    if (SHOTS) await page.screenshot({ path: require('path').join(SHOTS, name + '.png') });
  }
  const pressed = (page, d) => page.getAttribute(DIR(d), 'aria-pressed');

  // ── 1–3. Phone, cold start, real taps ────────────────────────────────────
  {
    const { ctx, page, rec } = await newCtx({ phone: true });
    try {
      const set = await step('375x560 cold start: the Oblique Photos defaults set by taps in the customizer', async () => {
        await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
        await themeSettled(page);
        await dismissHints(page);
        assert(await page.evaluate((k) => localStorage.getItem(k), STORE) === null, 'not a cold start');
        await openDisplayByTaps(page);
        assert(await sectionShown(page), 'no Oblique Photos section');
        assert(await page.inputValue(SEL('direction')) === 'auto' && await page.inputValue(SEL('zoom')) === 'overview', 'built-ins');
        assert(await page.isChecked(SEL('lineOfSight')), 'line of sight off by default');
        await pick(page, 'direction', 'west');
        await pick(page, 'zoom', 'zoomed');
        await tapControl(page, SEL('lineOfSight'));
        await page.waitForTimeout(500);
        assert(!await page.isChecked(SEL('lineOfSight')), 'the line box still checked after a tap');
        const s = await stored(page);
        assert(s && s.direction === 'west' && s.zoom === 'zoomed' && s.lineOfSight === false && Object.keys(s).length === 3, 'stored ' + JSON.stringify(s));
        const over = await page.evaluate(() => {
          const o = document.querySelector('.cust-overlay');
          return { page: document.documentElement.scrollWidth - document.documentElement.clientWidth, overlay: o ? o.scrollWidth - o.clientWidth : 0 };
        });
        assert(over.page <= 0 && over.overlay <= 0, 'horizontal overflow ' + JSON.stringify(over));
        await page.locator('.cust-panel[data-panel="display"] .cust-section-title:text-is("Oblique Photos")').scrollIntoViewIfNeeded();
        await shot(page, '737-customizer-section-375');
        await page.tap('.cust-close');
        await page.waitForTimeout(400);
      });

      if (set) await step('375x560: by taps to the node; the viewer opens looking west, on the crop', async () => {
        await page.tap('[data-bottom-nav-tab="more"]');
        await page.tap('[data-bottom-nav-more-route="nodes"]');
        const cell = page.locator(`tr[data-key="${node.pk}"] td:visible`).first();
        await cell.waitFor({ timeout: 30000 });
        await dismissHints(page);
        await cell.tap();
        await page.waitForSelector(ACTION, { timeout: 30000 });
        await page.locator(ACTION).scrollIntoViewIfNeeded();
        await page.tap(ACTION);
        await waitReady(page);
        assert(await pressed(page, 'west') === 'true', 'not west');
        assert(rec.zoom.length === 1 && rec.preview.length === 0, 'zoom ' + rec.zoom.length + ', previews ' + rec.preview.length);
        assert(/_4_0020_/.test(rec.zoom[0].pathname), 'the crop of ' + rec.zoom[0].pathname);
        assert(await page.getAttribute(ZOOM, 'aria-pressed') === 'true', 'zoom not pressed');
        await shot(page, '737-customizer-viewer-west-zoomed-375');
        await page.locator(ZOOM).scrollIntoViewIfNeeded();
        await page.tap(ZOOM);
        await waitReady(page);
        assert(await page.getAttribute(ZOOM, 'aria-pressed') === 'false' && rec.preview.length === 1, 'zoom out: previews ' + rec.preview.length);
        await page.tap(CLOSE);
        await page.waitForSelector(DIALOG, { state: 'detached', timeout: 3000 });
      });

      if (set) await step('375x560: first photo auto again by taps; from a profile, the side view and no line', async () => {
        await openDisplayByTaps(page);
        await pick(page, 'direction', 'auto');
        const s = await stored(page);
        assert(s && !('direction' in s) && s.lineOfSight === false, 'auto is the default, not stored: ' + JSON.stringify(s));
        await page.tap('.cust-close');
        await page.waitForTimeout(400);
        await page.goto(`${BASE}/#/map?profile=${P_NS}`, { waitUntil: 'domcontentloaded' });
        await themeSettled(page);
        await page.waitForSelector(PANEL + '.is-open', { timeout: 30000 });
        await page.waitForSelector(SF_A, { timeout: 15000 });
        await dismissHints(page);
        const before = rec.search.length;
        await page.locator(SF_A).scrollIntoViewIfNeeded();
        await page.tap(SF_A);
        await waitReady(page);
        const q = rec.search[before].searchParams;
        assert(!q.has('path') && !q.has('ha') && !q.has('hb'), 'the line was asked for: ' + q.toString());
        assert(await pressed(page, 'east') === 'true', 'not the side view of a north–south line');
        assert(await page.$(LOS) === null, 'a line is drawn');
        await shot(page, '737-customizer-profile-no-line-375');
        const label = await page.getAttribute('[data-testid="sf-marks"]', 'aria-label');
        assert(label && !/Line of sight/.test(label), 'label ' + label);
        assert(rec.errors.length === 0, rec.errors.join(' | '));
        assert(rec.external.every(u => !/dataforsyningen|klimadatastyrelsen/.test(u)), 'went upstream: ' + rec.external.join(' '));
      });
    } finally { await ctx.close(); }
  }

  // ── 4–5. A link wins; export; Reset All ──────────────────────────────────
  {
    const storage = { [STORE]: JSON.stringify({ skraafoto: { direction: 'west', zoom: 'zoomed', lineOfSight: false } }) };
    const { ctx, page, rec } = await newCtx({ storage });
    try {
      await step('a deep-linked direction wins over the customizer', async () => {
        await page.goto(`${BASE}/#/nodes/${PK}?skraafoto=${AARHUS.lat.toFixed(5)},${AARHUS.lon.toFixed(5)},south`, { waitUntil: 'domcontentloaded' });
        await themeSettled(page);
        await waitReady(page);
        assert(await pressed(page, 'south') === 'true', 'not south');
        assert(rec.zoom.length >= 1 && rec.zoom.every(u => /_3_0020_/.test(u.pathname)) && rec.preview.length === 0, 'south, zoomed: ' + rec.zoom.map(u => u.pathname).join(' ') + ' / ' + rec.preview.join(' '));
        await page.click(CLOSE);
        await page.waitForSelector(DIALOG, { state: 'detached', timeout: 3000 });
      });
      await step('the export carries the section and no server key; Reset All brings M3c back', async () => {
        await page.click('#customizeToggle');
        await page.waitForSelector('.cust-overlay:not(.hidden)', { timeout: 5000 });
        await page.click('.cust-tab[data-tab="export"]');
        await page.waitForSelector('.cust-panel[data-panel="export"].active', { timeout: 4000 });
        const json = JSON.parse(await page.inputValue('#cv2ExportJson'));
        assert(JSON.stringify(json.skraafoto) === JSON.stringify({ direction: 'west', zoom: 'zoomed', lineOfSight: false }), 'export ' + JSON.stringify(json.skraafoto));
        for (const k of ['searchUrl', 'previewUrl', 'token', 'maxPx', 'enabled']) assert(!(k in json.skraafoto), k + ' in the export');
        page.once('dialog', (d) => d.accept());
        await page.click('#cv2ResetAll');
        await page.waitForTimeout(500);
        assert(await stored(page) === null, 'kept after reset');
        await page.click('.cust-close');
        await page.waitForTimeout(300);
        const from = { s: rec.search.length, z: rec.zoom.length };
        await page.goto(`${BASE}/#/map?profile=${P_NS}`, { waitUntil: 'domcontentloaded' });
        await page.waitForSelector(SF_A, { timeout: 30000 });
        await page.click(SF_A);
        await waitReady(page);
        assert(rec.search[from.s].searchParams.has('path'), 'the line not asked for after reset');
        assert(await pressed(page, 'east') === 'true', 'not the side view after reset');
        assert(rec.zoom.length === from.z, 'zoomed after reset');
        assert(await page.$(LOS) !== null, 'no line after reset');
        assert(rec.errors.length === 0, rec.errors.join(' | '));
      });
    } finally { await ctx.close(); }
  }

  // ── 6. The server gate ───────────────────────────────────────────────────
  {
    const { ctx, page, rec } = await newCtx({ sf: false });
    try {
      await step('without the server\'s skraafoto block there is no section', async () => {
        await page.goto(BASE + '/#/nodes', { waitUntil: 'domcontentloaded' });
        await themeSettled(page);
        await page.click('#customizeToggle');
        await page.waitForSelector('.cust-overlay:not(.hidden)', { timeout: 5000 });
        await page.click('.cust-tab[data-tab="display"]');
        await page.waitForSelector('.cust-panel[data-panel="display"].active', { timeout: 4000 });
        assert(!await sectionShown(page), 'the section is shown with the gate off');
        assert(await page.$(SEL('direction')) === null, 'a control is shown with the gate off');
        assert(rec.search.length === 0 && rec.errors.length === 0, 'asked ' + rec.search.length + '; ' + rec.errors.join(' | '));
      });
    } finally { await ctx.close(); }
  }

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed\n`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
