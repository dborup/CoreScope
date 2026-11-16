/**
 * E2E (#839): the Path profile findings of the rc19 test pass, from a cold
 * start with real taps (touch) and clicks (desktop).
 *
 * Switched on the way test-issue-790-profile-mobile-pick-e2e.js does it: the
 * client config gets the `dhm` block with profile: true (and a `skraafoto`
 * block with no bounds, so the photo row shows anywhere), and
 * /api/terrain/profile is answered by a stub, so no token and no real
 * service is involved.
 *
 * At 375x560 (phone, touch), 640x360 (landscape, touch) and 1440x900
 * (desktop, mouse):
 *   §1  after the first pick, an A marker sits where the map was tapped,
 *       while B is still to be picked;
 *   §3  an A picked low on the map (where the panel then opens) and a B
 *       picked high are both in view once the profile is drawn: on the
 *       map, not under the panel and not under the zoom buttons;
 *   §2  the same holds for the chart cursor's dot on the map (a tap on
 *       the chart on a touch screen, hover on desktop), and after the
 *       panel is folded and unfolded;
 *   §8  the chart's distance axis reads "8.14 km", not "8141.4 m";
 *   §12 while no chart point is picked, the photo row says in visible
 *       text how to pick one; a tap on the chart hides it and enables
 *       Chart point;
 *   §6  a path the server refuses as too long says so, with the limit in
 *       km from the refusal, not the bare "path too long";
 *   §7  a malformed /p link says it is invalid, and the junk leaves the
 *       address bar;
 * no console errors.
 *
 * Usage:
 *   BASE_URL=http://localhost:13800 node test-issue-839-profile-ux-e2e.js
 * Optional: SCREENSHOT_DIR=<dir> writes the PR screenshots.
 */
'use strict';
const { chromium } = require('playwright');
const path = require('path');

const BASE = process.env.BASE_URL || 'http://localhost:13700';
const SHOT_DIR = process.env.SCREENSHOT_DIR || '';
const PROFILE_PATH = '/api/terrain/profile';
const ATTRIBUTION = 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0';
const DHM_BLOCK = {
  enabled: true, url: '/api/map/dhm/wms', layer: 'dhm_terraen_skyggekort',
  minZoom: 7, maxZoom: 19, profile: true,
  bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
  attribution: ATTRIBUTION,
};
// No bounds: an older server's block, offered everywhere — the stub's ends
// are off Denmark.
const SF_BLOCK = {
  enabled: true, searchUrl: '/api/skraafoto/search', previewUrl: '/api/skraafoto/preview/{collection}/{item}',
  maxPx: 1024, note: 'Public aerial photos.',
  credit: { attribution: 'Klimadatastyrelsen, Skråfotos, CC BY 4.0', owner: 'Klimadatastyrelsen' },
};
const PNG_1X1 = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGM4ceIEAAS0AlkWLoFAAAAAAElFTkSuQmCC',
  'base64');
const D = 8141.4;
const MAX_KM = 80;

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

function profileFor(q) {
  const end = (h) => {
    const asked = q.get(h);
    return { lat: 37.6, lon: -122.7, kind: 'point', groundM: 40, antennaM: asked !== null ? Number(asked) : 2,
      antennaSource: asked !== null ? 'request' : 'point-default', assumed: asked === null };
  };
  const samples = [];
  for (let i = 0; i < 60; i++) {
    const d = Math.round(D * i / 59 * 10) / 10;
    samples.push({ d, dtm: 30 + 18 * Math.sin(d / 900), dsm: 42 + 18 * Math.sin(d / 900) });
  }
  return {
    a: end('ha'), b: end('hb'),
    distanceM: D, stepM: 11.7, sampleCount: 60, model: q.get('model') === 'dtm' ? 'dtm' : 'both',
    coverage: { state: 'full', pct: 100, firstGapM: null }, samples,
    physics: { freqMHz: 869.618, freqSource: 'firmware', lambdaM: 0.34474, k: 1.3333333333333333, kSource: 'default', f1MaxM: 32.14 },
    verdict: { state: 'clear', model: 'dtm', criterion: 0.6, worstClearanceM: 41.2, worstAtM: 5000, worstF1Fraction: 1.4 },
    attribution: ATTRIBUTION,
  };
}

const TOGGLE = '[data-testid="map-profile-toggle"]';
const HINT = '[data-testid="map-profile-hint"]';
const CHART = '[data-testid="tp-chart"] .tp-plot svg';
const DOT = '.leaflet-overlay-pane .tp-cursor-dot';
// Open sea west of the fixture's Bay Area nodes: no markers to snap to.
const VIEW = { lat: 37.5, lon: -122.85, zoom: 10 };

const VIEWPORTS = [
  { name: '375x560', viewport: { width: 375, height: 560 }, touch: true, lowA: 0.55 },
  { name: '640x360', viewport: { width: 640, height: 360 }, touch: true, lowA: 0.4 },
  { name: '1440x900', viewport: { width: 1440, height: 900 }, touch: false, lowA: 0.75 },
];

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #839 Path profile UX against ${BASE} ===\n`);

  /* o.refuse: answer every profile request with the too-long refusal. */
  async function newCtx(o) {
    const ctx = await browser.newContext({ viewport: o.viewport, hasTouch: o.touch, isMobile: o.touch });
    const rec = { profile: [], consoleErrors: [] };
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      config.dhm = DHM_BLOCK;
      config.skraafoto = SF_BLOCK;
      await route.fulfill({ response, json: config });
    });
    await ctx.route(url => new URL(url).pathname === PROFILE_PATH, async (route) => {
      const u = new URL(route.request().url());
      rec.profile.push(u);
      if (o.refuse) {
        await route.fulfill({ status: 400, contentType: 'application/json', body: JSON.stringify({ error: 'path too long', maxPathKm: MAX_KM }) });
        return;
      }
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(profileFor(u.searchParams)) });
    });
    await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, route => route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 }));
    await ctx.route(url => new URL(url).pathname === '/api/map/dhm/wms', route => route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 }));
    await ctx.addInitScript((view) => {
      try {
        // A cold start without a view in the URL (/p) opens the stored one.
        if (view) localStorage.setItem('map-view', JSON.stringify(view));
        localStorage.setItem('meshcore-map-clustering', 'false');
        ['row-swipe', 'tab-swipe', 'edge-drawer', 'pull-refresh']
          .forEach(k => localStorage.setItem('meshcore-gesture-hints-' + k, 'seen'));
      } catch (_) {}
    }, o.storeView ? { lat: VIEW.lat, lng: VIEW.lon, zoom: VIEW.zoom } : null);
    const page = await ctx.newPage();
    page.on('console', m => {
      // The refusal stub's own 400 is the case under test.
      if (m.type() === 'error' && !(o.refuse && /status of 400/.test(m.text()))) rec.consoleErrors.push(m.text());
    });
    page.on('pageerror', e => rec.consoleErrors.push(String(e)));
    return { ctx, page, rec };
  }

  async function gotoMap(page) {
    await page.goto(`${BASE}/#/map?lat=${VIEW.lat}&lon=${VIEW.lon}&zoom=${VIEW.zoom}`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
    await page.waitForSelector(TOGGLE, { timeout: 15000 });
    await page.waitForTimeout(500);
  }

  /* A spot on the visible map — not under the panel, a control, the hint
   * or a marker — at or below minFrac of the map's height, away from
   * `avoid`. */
  async function freeSpot(page, o) {
    o = o || {};
    return page.evaluate(({ minFrac, avoid }) => {
      const box = document.getElementById('leaflet-map').getBoundingClientRect();
      const marks = Array.from(document.querySelectorAll('.leaflet-marker-icon')).map(el => el.getBoundingClientRect());
      const BLOCK = '.leaflet-control, .map-controls, .map-controls-toggle, .map-profile-toggle, .map-profile-hint, .tp-panel, .leaflet-marker-icon';
      const y0 = box.y + Math.max(40, box.height * (minFrac || 0));
      for (let y = y0; y < box.bottom - 20; y += 10) {
        for (let x = box.x + 80; x < box.right - 60; x += 14) {
          const px = Math.round(x), py = Math.round(y);
          if (avoid && Math.hypot(px - avoid.x, py - avoid.y) < 80) continue;
          if (!marks.every(r => px < r.x - 32 || px > r.right + 32 || py < r.y - 32 || py > r.bottom + 32)) continue;
          const ok = [[0, 0], [-20, 0], [20, 0], [0, -20], [0, 20]].every(([dx, dy]) => {
            const hit = document.elementFromPoint(px + dx, py + dy);
            return hit && hit.closest('.leaflet-container') && !hit.closest(BLOCK);
          });
          if (ok) return { x: px, y: py };
        }
      }
      return null;
    }, { minFrac: o.minFrac || 0, avoid: o.avoid || null });
  }

  /* seen reports whether the centre of each element matching a selector
   * can be seen: on the map, and with no panel, control or hint on top of
   * it. A non-interactive layer lets elementFromPoint fall through to the
   * map, which still counts as seen; the panel or a control does not. */
  async function seen(page, sels) {
    return page.evaluate((sels) => {
      const map = document.getElementById('leaflet-map').getBoundingClientRect();
      const COVER = '.tp-panel, .leaflet-control, .map-controls, .map-profile-toggle, .map-profile-hint';
      return sels.map((sel) => {
        const el = document.querySelector(sel);
        if (!el) return { sel, ok: false, why: 'missing' };
        const r = el.getBoundingClientRect();
        const x = Math.round(r.left + r.width / 2), y = Math.round(r.top + r.height / 2);
        if (!(x >= map.left && x <= map.right && y >= map.top && y <= map.bottom)) return { sel, ok: false, why: 'off the map', x, y };
        const hit = document.elementFromPoint(x, y);
        const cover = hit && hit.closest(COVER);
        return { sel, ok: !!hit && !cover, why: cover ? 'under ' + cover.className : '', x, y };
      });
    }, sels);
  }

  async function pick(page, vp, spot) {
    if (vp.touch) await page.touchscreen.tap(spot.x, spot.y);
    else await page.mouse.click(spot.x, spot.y);
    await page.waitForTimeout(700);
  }

  async function press(page, vp, sel) {
    if (vp.touch) await page.tap(sel); else await page.click(sel);
  }

  /* Waits for n profile requests and the chart they draw. */
  async function waitForChart(page, rec, n) {
    const t0 = Date.now();
    while (rec.profile.length < n && Date.now() - t0 < 5000) await page.waitForTimeout(100);
    await page.waitForSelector('[data-testid="tp-chart"] svg.tp-svg', { timeout: 5000 });
    await page.waitForTimeout(900); // a pan to keep both ends in view settles
  }

  const shot = (page, name) => SHOT_DIR ? page.screenshot({ path: path.join(SHOT_DIR, '839-' + name + '.png') }) : null;

  for (const vp of VIEWPORTS) {
    const { ctx, page, rec } = await newCtx(vp);
    await gotoMap(page);
    let spotA = null;

    await step(vp.name + ': §1 after the first pick an A marker sits where the map was ' + (vp.touch ? 'tapped' : 'clicked'), async () => {
      await press(page, vp, TOGGLE);
      await page.waitForTimeout(200);
      spotA = await freeSpot(page, { minFrac: vp.lowA });
      assert(spotA, 'no free spot for A');
      await pick(page, vp, spotA);
      const first = await page.evaluate(() => {
        const el = document.querySelector('[data-testid="tp-end-first"]');
        if (!el) return null;
        const r = el.getBoundingClientRect();
        return { x: r.left + r.width / 2, y: r.top + r.height / 2, text: el.textContent.trim() };
      });
      assert(first, 'no A marker on the map after the first pick');
      assert(Math.hypot(first.x - spotA.x, first.y - spotA.y) <= 4, 'the A marker is not where the map was picked: ' + JSON.stringify({ first, spotA }));
      assert(first.text === 'A', 'the marker reads ' + JSON.stringify(first.text));
      const [v] = await seen(page, ['[data-testid="tp-end-first"]']);
      assert(v.ok, 'the A marker cannot be seen: ' + JSON.stringify(v));
      assert(rec.profile.length === 0, 'A alone issued a request');
      await shot(page, 'first-end-' + vp.name);
    });

    await step(vp.name + ': §3 an A picked low and a B picked high are both in view once the profile is drawn', async () => {
      const spotB = await freeSpot(page, { minFrac: 0.05, avoid: spotA });
      assert(spotB, 'no free spot for B');
      await pick(page, vp, spotB);
      await waitForChart(page, rec, 1);
      assert(rec.profile.length === 1, 'B issued ' + rec.profile.length + ' requests');
      assert(await page.locator('[data-testid="tp-end-first"]').count() === 0, 'the first-end marker stayed beside the A/B ends');
      const v = await seen(page, ['[data-testid="tp-end-marker-a"]', '[data-testid="tp-end-marker-b"]']);
      assert(v.every(e => e.ok), 'an end is hidden: ' + JSON.stringify(v));
      await shot(page, 'ready-' + vp.name);
    });

    await step(vp.name + ': §8 the distance axis reads 8.14 km', async () => {
      const x1 = await page.locator('[data-testid="tp-chart"] .tp-ax-x1').textContent();
      assert(x1 === '8.14 km', 'axis end ' + JSON.stringify(x1));
      const title = await page.evaluate(() => document.querySelector('[data-testid="tp-chart"] svg title').textContent);
      assert(/, 8\.14 km\./.test(title) && /at 5\.00 km along the path/.test(title), 'chart title ' + JSON.stringify(title));
    });

    await step(vp.name + ': §12 with no chart point picked, the photo row says how to pick one', async () => {
      const s = await page.evaluate(() => {
        const hint = document.querySelector('[data-testid="tp-sf-hint"]');
        const btn = document.querySelector('[data-testid="tp-sf-point"]');
        return { hint: hint && hint.getClientRects().length ? hint.textContent : null, disabled: !!(btn && btn.disabled) };
      });
      assert(s.disabled, 'Chart point is enabled before a point is picked');
      assert(s.hint && /tap the chart/i.test(s.hint), 'no visible hint: ' + JSON.stringify(s));
    });

    await step(vp.name + ': §2 the chart cursor\'s dot on the map can be seen (' + (vp.touch ? 'tap' : 'hover') + ')', async () => {
      await page.locator(CHART).scrollIntoViewIfNeeded();
      const svg = await page.locator(CHART).boundingBox();
      assert(svg, 'no chart');
      const at = { x: svg.x + svg.width * 0.4, y: svg.y + svg.height / 2 };
      if (vp.touch) await page.touchscreen.tap(at.x, at.y);
      else await page.mouse.move(at.x, at.y);
      await page.waitForTimeout(250);
      const [v] = await seen(page, [DOT]);
      assert(v.ok, 'the cursor dot cannot be seen: ' + JSON.stringify(v));
      await shot(page, 'cursor-' + vp.name);
    });

    if (vp.touch) {
      await step(vp.name + ': §12 a tap on the chart hides the hint and enables Chart point', async () => {
        const s = await page.evaluate(() => {
          const hint = document.querySelector('[data-testid="tp-sf-hint"]');
          const btn = document.querySelector('[data-testid="tp-sf-point"]');
          return { hint: !!(hint && hint.getClientRects().length), disabled: !!(btn && btn.disabled) };
        });
        assert(!s.disabled && !s.hint, 'after a chart tap: ' + JSON.stringify(s));
      });
    }

    await step(vp.name + ': §2 folded and unfolded, both ends stay in view', async () => {
      if (!vp.touch) await page.mouse.move(5, 5);
      await page.locator('[data-testid="tp-fold"]').scrollIntoViewIfNeeded();
      await press(page, vp, '[data-testid="tp-fold"]');
      await page.waitForTimeout(800);
      let v = await seen(page, ['[data-testid="tp-end-marker-a"]', '[data-testid="tp-end-marker-b"]']);
      assert(v.every(e => e.ok), 'folded, an end is hidden: ' + JSON.stringify(v));
      await press(page, vp, '[data-testid="tp-fold"]');
      await page.waitForTimeout(800);
      v = await seen(page, ['[data-testid="tp-end-marker-a"]', '[data-testid="tp-end-marker-b"]']);
      assert(v.every(e => e.ok), 'unfolded, an end is hidden: ' + JSON.stringify(v));
      assert(rec.profile.length === 1, 'fold/unfold issued requests: ' + rec.profile.length);
      assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
    });
    await ctx.close();

    {
      const { ctx, page, rec } = await newCtx(Object.assign({}, vp, { refuse: true }));
      await gotoMap(page);
      await step(vp.name + ': §6 a path refused as too long says so, with the limit in km', async () => {
        await press(page, vp, TOGGLE);
        await page.waitForTimeout(200);
        const a = await freeSpot(page, { minFrac: 0.3 });
        await pick(page, vp, a);
        const b = await freeSpot(page, { minFrac: 0.05, avoid: a });
        await pick(page, vp, b);
        await page.waitForSelector('[data-testid="tp-status"].tp-error', { timeout: 5000 });
        const s = await page.evaluate(() => ({
          status: document.querySelector('[data-testid="tp-status"]').textContent,
          chip: document.querySelector('[data-testid="tp-verdict"]').textContent,
          detail: document.querySelector('[data-testid="tp-detail"]').textContent,
        }));
        assert(/too long/i.test(s.status) && s.status.indexOf('up to ' + MAX_KM + ' km') >= 0, 'status ' + JSON.stringify(s));
        assert(s.status !== 'path too long', 'the bare server text is shown');
        assert(s.chip === 'Too long', 'chip ' + JSON.stringify(s.chip));
        assert(!/Could not load/.test(s.detail), 'the body still says the profile could not load: ' + JSON.stringify(s.detail));
        assert(rec.profile.length === 1, 'requests: ' + rec.profile.length);
        assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
        await shot(page, 'too-long-' + vp.name);
      });
      await ctx.close();
    }

    {
      const { ctx, page, rec } = await newCtx(Object.assign({}, vp, { storeView: true }));
      await step(vp.name + ': §7 a malformed /p link says it is invalid, and the junk leaves the URL', async () => {
        await page.goto(`${BASE}/p?profile=garbage`, { waitUntil: 'domcontentloaded' });
        await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
        await page.waitForSelector(TOGGLE, { timeout: 15000 });
        await page.waitForTimeout(500);
        const s = await page.evaluate(() => {
          const h = document.querySelector('[data-testid="map-profile-hint"]');
          const panel = document.querySelector('[data-testid="tp-panel"]');
          return { hint: h && !h.hidden && h.getClientRects().length ? h.textContent : null, role: h && h.getAttribute('role'),
            hash: location.hash, panel: !!(panel && panel.classList.contains('is-open')) };
        });
        assert(s.hint && /invalid/i.test(s.hint), 'no visible notice: ' + JSON.stringify(s));
        assert(s.role === 'status', 'the notice is not announced: ' + s.role);
        assert(!/profile=/.test(s.hash) && /^#\/map/.test(s.hash), 'the junk stayed in the URL: ' + s.hash);
        assert(!s.panel && rec.profile.length === 0, 'a panel opened or a request went out');
        await shot(page, 'bad-link-' + vp.name);
        // The notice goes with the next tap on the map.
        const spot = await freeSpot(page, { minFrac: 0.4 });
        await pick(page, vp, spot);
        assert(await page.locator(HINT).isHidden(), 'the notice stayed after a tap on the map');
        assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
      });
      await ctx.close();
    }
  }

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed\n`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
