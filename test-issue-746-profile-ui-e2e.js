/**
 * E2E (#746): Path profile UI fixes on the Map page.
 *
 * Like the other DHM suites, the CI server has no map.dhm config and no
 * token, so /api/config/client is intercepted and given the `dhm` block
 * the Go server sends when the profile is served, and /api/terrain/profile
 * is answered with a synthetic profile that echoes the request. No real
 * DHM service and no third-party tile server is ever contacted.
 *
 * Asserts:
 *   1. §1 — switching from one profile link to another on the Map page, and
 *      going back in history, opens the destination profile: the old
 *      page's teardown no longer rewrites the new hash.
 *   2. §4 — a gap in the surface model (DSM) over complete terrain is drawn
 *      as two surface lines and two clutter bands, not one bridging it.
 *   3. §5 — with a customizer point height (30 m) unlike the server's
 *      (2 m), a link of two free points and a Move A onto a free point each
 *      cost exactly one request, which carries the 30 m.
 *   4. N2 — the accessibility tree reads the terrain and surface chips as two
 *      sentences ("Terrain: Clear. Buildings/trees: Blocked"), while the
 *      separator takes no visible room.
 *   5. Map fit (owner decision) — starting from a stored view elsewhere, a
 *      deep link (desktop) and a /p handoff (360x740 phone) put both ends
 *      inside the map and above the panel; a pan or a Move afterwards is
 *      not undone by a second fit; without a profile the stored view stays.
 *
 * Usage:
 *   BASE_URL=http://localhost:13581 node test-issue-746-profile-ui-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const PROFILE_PATH = '/api/terrain/profile';
const ATTRIBUTION = 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0';
const DHM_BLOCK = {
  enabled: true, url: '/api/map/dhm/wms', layer: 'dhm_terraen_skyggekort',
  minZoom: 7, maxZoom: 19, profile: true,
  bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
  attribution: ATTRIBUTION,
};
const PNG_1X1 = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGM4ceIEAAS0AlkWLoFAAAAAAElFTkSuQmCC',
  'base64');
const PANEL = '[data-testid="tp-panel"]';

// Two free-point profiles in Aarhus, far from the fixture's own nodes.
const P1 = '56.15467,10.21370,56.15265,10.21471';
const P2 = '56.16290,10.20390,56.15000,10.18000';

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

/* The stub answers the shape of a real GET /api/terrain/profile, with the
 * endpoints at the requested points and the heights the request asked for.
 * A request with freq=868 gets a DSM gap over samples 18-20 (§4); one with
 * k=1.5 gets a building through the line of sight that the terrain clears,
 * so the surface chip is shown (N2). */
function profileFor(q) {
  const D = 1200;
  const end = (side) => {
    const ref = q.get(side) || '';
    const node = /^[0-9a-f]{64}$/.test(ref);
    const ll = node ? [56.15, 10.2] : ref.split(',').map(Number);
    const asked = q.get('h' + side);
    return {
      lat: ll[0], lon: ll[1], kind: node ? 'node' : 'point', role: node ? 'repeater' : undefined,
      groundM: 40, antennaM: asked !== null ? Number(asked) : (node ? 10 : 2),
      antennaSource: asked !== null ? 'request' : (node ? 'role-default' : 'point-default'),
      assumed: asked === null,
    };
  };
  const samples = [];
  for (let i = 0; i < 40; i++) {
    const d = Math.round(D * i / 39);
    const dsmGap = q.get('freq') === '868' && i >= 18 && i <= 20;
    samples.push({ d, dtm: 30 + 8 * Math.sin(d / 200), dsm: dsmGap ? null : 36 + 8 * Math.sin(d / 200) });
  }
  return {
    a: end('a'), b: end('b'),
    distanceM: D, stepM: 1.2, sampleCount: 1001, model: q.get('model') === 'dtm' ? 'dtm' : 'both',
    coverage: { state: 'full', pct: 100, firstGapM: null }, samples,
    physics: { freqMHz: 869.618, freqSource: 'firmware', lambdaM: 0.34474, k: 1.3333333333333333, kSource: 'default', f1MaxM: 10.2 },
    verdict: { state: 'clear', model: 'dtm', criterion: 0.6, worstClearanceM: 21.2, worstAtM: 600, worstF1Fraction: 2.1,
      surface: q.get('k') === '1.5'
        ? { state: 'blocked', worstClearanceM: -17.5, worstAtM: 600, worstF1Fraction: -2.6 }
        : { state: 'clear', worstClearanceM: 15.1, worstAtM: 600, worstF1Fraction: 1.5 } },
    attribution: ATTRIBUTION,
  };
}

function hashParams(hash) {
  return new URLSearchParams(String(hash).split('?')[1] || '');
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #746 Path profile UI fixes against ${BASE} ===\n`);

  async function newCtx(o) {
    o = o || {};
    const ctx = await browser.newContext({
      viewport: o.viewport || { width: 1440, height: 900 },
      hasTouch: !!o.touch, isMobile: !!o.touch,
    });
    const rec = { profile: [], consoleErrors: [] };
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      config.dhm = Object.assign({}, DHM_BLOCK, o.dhm || {});
      await route.fulfill({ response, json: config });
    });
    await ctx.route(url => new URL(url).pathname === PROFILE_PATH, async (route) => {
      const u = new URL(route.request().url());
      rec.profile.push(u);
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(profileFor(u.searchParams)) });
    });
    await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, async (route) => {
      await route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 });
    });
    await ctx.route(url => new URL(url).pathname === '/api/map/dhm/wms', async (route) => {
      await route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 });
    });
    await ctx.addInitScript((init) => {
      try {
        localStorage.setItem('meshcore-theme', 'light');
        localStorage.setItem('meshcore-map-clustering', 'false');
        ['row-swipe', 'tab-swipe', 'edge-drawer', 'pull-refresh']
          .forEach(k => localStorage.setItem('meshcore-gesture-hints-' + k, 'seen'));
        Object.keys(init || {}).forEach(k => localStorage.setItem(k, init[k]));
      } catch (_) {}
    }, o.storage || {});
    const page = await ctx.newPage();
    page.on('console', m => { if (m.type() === 'error') rec.consoleErrors.push(m.text()); });
    page.on('pageerror', e => rec.consoleErrors.push(String(e)));
    return { ctx, page, rec };
  }

  /* waitForRequestFor waits until the panel is open and a profile request
   * for path `pair` (A's lat,lon,B's lat,lon) has been answered. */
  async function waitForRequestFor(page, rec, pair) {
    await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
    await page.waitForFunction((sel) => {
      const p = document.querySelector(sel);
      return !!(p && p.classList.contains('is-open'));
    }, PANEL, { timeout: 15000 });
    const want = pair.split(',');
    const t0 = Date.now();
    const hit = () => rec.profile.some(u => u.searchParams.get('a') === want[0] + ',' + want[1]
      && u.searchParams.get('b') === want[2] + ',' + want[3]);
    while (!hit() && Date.now() - t0 < 10000) await page.waitForTimeout(100);
    assert(hit(), 'no profile request for ' + pair + '; asked for ' + rec.profile.map(u => u.search).join(' | '));
    await page.waitForFunction(() => {
      const c = document.querySelector('[data-testid="tp-verdict"]');
      return !!(c && c.getAttribute('data-state') && c.getAttribute('data-state') !== 'pending');
    }, null, { timeout: 10000 });
  }

  const lastAsked = (rec) => rec.profile[rec.profile.length - 1].searchParams;

  // ── 1. §1: teardown leaves the destination hash alone ─────────────────
  {
    const { ctx, page, rec } = await newCtx();
    await step('§1: a second profile link on the Map page opens that profile', async () => {
      await page.goto(`${BASE}/#/map?profile=${P1}&ha=19&hb=19`, { waitUntil: 'domcontentloaded' });
      await waitForRequestFor(page, rec, P1);
      const next = `#/map?profile=${P2}&model=dtm&ha=12`;
      await page.evaluate((h) => { location.hash = h; }, next);
      await waitForRequestFor(page, rec, P2);
      const hp = hashParams(await page.evaluate(() => location.hash));
      assert(hp.get('profile') === P2, 'the destination profile left the hash: ' + hp.toString());
      assert(hp.get('model') === 'dtm' && hp.get('ha') === '12', 'the destination controls left the hash: ' + hp.toString());
      const q = lastAsked(rec);
      assert(q.get('model') === 'dtm' && q.get('ha') === '12', 'measured with ' + q.toString());
    });
    await step('§1: Back returns to the first profile, still open', async () => {
      await page.goBack({ waitUntil: 'domcontentloaded' });
      const before = rec.profile.length;
      const t0 = Date.now();
      while (rec.profile.length === before && Date.now() - t0 < 10000) await page.waitForTimeout(100);
      await waitForRequestFor(page, rec, P1);
      const hp = hashParams(await page.evaluate(() => location.hash));
      assert(hp.get('profile') === P1 && hp.get('ha') === '19', 'Back lost the first profile: ' + hp.toString());
      const q = lastAsked(rec);
      assert(q.get('a') + ',' + q.get('b') === P1 && q.get('ha') === '19', 'Back measured ' + q.toString());
    });
    await step('§1: the Close button still clears the profile from the hash', async () => {
      await page.click('[data-testid="tp-close"]');
      const hp = hashParams(await page.evaluate(() => location.hash));
      assert(hp.get('profile') === null && hp.get('ha') === null, 'Close left ' + hp.toString());
      assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
    });
    await ctx.close();
  }

  // ── 2. §4: the surface line never bridges a surface gap ───────────────
  {
    const { ctx, page, rec } = await newCtx();
    await step('§4: a DSM gap splits the surface line and the clutter band', async () => {
      await page.goto(`${BASE}/#/map?profile=${P1}&freq=868`, { waitUntil: 'domcontentloaded' });
      await waitForRequestFor(page, rec, P1);
      const got = await page.evaluate(() => {
        const svg = document.querySelector('[data-testid="tp-chart"] svg');
        return {
          lines: svg.querySelectorAll('polyline[stroke="var(--terrain-clutter)"]').length,
          bands: svg.querySelectorAll('polygon[fill="var(--terrain-clutter)"]').length,
        };
      });
      assert(got.lines === 2 && got.bands === 2, 'surface drawn as ' + JSON.stringify(got));
      assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
    });
    await ctx.close();
  }

  // ── 3. §5: one request per drop with a customizer point height ────────
  {
    const { ctx, page, rec } = await newCtx({
      dhm: { pointAntenna: { m: 2, source: 'point-default' } },
      storage: {
        'cs-theme-overrides': JSON.stringify({ terrain: { heightPoint: 30 } }),
        'map-view': JSON.stringify({ lat: 56.154, lng: 10.214, zoom: 15 }),
      },
    });
    await step('§5: a link of two free points is measured once, with the customizer height', async () => {
      await page.goto(`${BASE}/#/map?profile=${P1}`, { waitUntil: 'domcontentloaded' });
      await waitForRequestFor(page, rec, P1);
      await page.waitForTimeout(800); // a second request would show up here
      assert(rec.profile.length === 1, 'requests: ' + rec.profile.map(u => u.search).join(' | '));
      const q = lastAsked(rec);
      assert(q.get('ha') === '30' && q.get('hb') === '30', 'measured with ' + q.toString());
      assert(hashParams(await page.evaluate(() => location.hash)).get('ha') === null, 'a customizer default reached the link');
    });
    await step('§5: Move A onto a free point is one request, with the customizer height', async () => {
      const before = rec.profile.length;
      await page.click('[data-testid="tp-move-a"]');
      const box = await page.locator('#leaflet-map').boundingBox();
      await page.mouse.click(box.x + 320, box.y + 140);
      const t0 = Date.now();
      while (rec.profile.length === before && Date.now() - t0 < 5000) await page.waitForTimeout(100);
      await page.waitForTimeout(800);
      const after = rec.profile.slice(before);
      assert(after.length === 1, 'requests after the drop: ' + after.map(u => u.search).join(' | '));
      const q = after[0].searchParams;
      assert(q.get('a') !== P1.split(',').slice(0, 2).join(','), 'A did not move: ' + q.toString());
      assert(/^-?\d+\.\d{5},-?\d+\.\d{5}$/.test(q.get('a')), 'A is not a free point: ' + q.get('a'));
      assert(q.get('ha') === '30', 'measured with ' + q.toString());
      assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
    });
    await ctx.close();
  }

  // ── 4. N2: the two chips are two sentences to a screen reader ──────────
  {
    const { ctx, page, rec } = await newCtx();
    await step('N2: the accessibility tree separates the terrain and surface chips', async () => {
      await page.goto(`${BASE}/#/map?profile=${P1}&k=1.5`, { waitUntil: 'domcontentloaded' });
      await waitForRequestFor(page, rec, P1);
      await page.waitForSelector('[data-testid="tp-surface-chip"]:not([hidden])', { timeout: 5000 });
      const snap = await page.locator('.tp-head').ariaSnapshot();
      // The snapshot puts a space between inline elements; what matters is
      // the full stop between the two statements.
      assert(/Terrain: Clear\s*\.\s*Buildings\/trees: Blocked/.test(snap), 'aria snapshot: ' + snap);
      const sep = await page.evaluate(() => {
        const chip = document.querySelector('[data-testid="tp-verdict"]');
        const el = chip.nextElementSibling;
        const r = el.getBoundingClientRect();
        return { cls: el.className, text: el.textContent, w: r.width, h: r.height,
          next: el.nextElementSibling && el.nextElementSibling.getAttribute('data-testid') };
      });
      assert(sep.cls === 'sr-only' && sep.text === '. ' && sep.next === 'tp-surface-chip', 'separator ' + JSON.stringify(sep));
      assert(sep.w <= 1 && sep.h <= 1, 'the separator takes visible room: ' + JSON.stringify(sep));
      assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
    });
    await ctx.close();
  }

  // ── 5. Map fit: a linked path is in view above the panel ──────────────
  // The owner's case: the stored view was near 56.08, 9.62, about 37 km
  // from the path, so the profile opened somewhere off screen.
  const ELSEWHERE = { 'map-view': JSON.stringify({ lat: 56.08, lng: 9.62, zoom: 12 }) };

  /* endsInView reports where the A/B markers sit against the map and the
   * panel, in page pixels. */
  async function endsInView(page) {
    return page.evaluate(() => {
      const box = (sel) => { const e = document.querySelector(sel); return e ? e.getBoundingClientRect() : null; };
      const map = box('#leaflet-map'), panel = box('[data-testid="tp-panel"]');
      const mid = (r) => r && { x: Math.round(r.left + r.width / 2), y: Math.round(r.top + r.height / 2) };
      const a = mid(box('[data-testid="tp-end-marker-a"]')), b = mid(box('[data-testid="tp-end-marker-b"]'));
      const inside = (p) => !!p && p.x >= map.left && p.x <= map.right && p.y >= map.top && p.y < panel.top;
      return { a, b, ok: inside(a) && inside(b),
        map: { left: Math.round(map.left), right: Math.round(map.right), top: Math.round(map.top) }, panelTop: Math.round(panel.top) };
    });
  }

  {
    const { ctx, page, rec } = await newCtx({ storage: ELSEWHERE });
    await step('fit: a deep link puts both ends above the panel (desktop)', async () => {
      await page.goto(`${BASE}/#/map?profile=${P1}&ha=19&hb=19`, { waitUntil: 'domcontentloaded' });
      await waitForRequestFor(page, rec, P1);
      await page.waitForTimeout(400);
      const v = await endsInView(page);
      assert(v.ok, 'an end is off the map or under the panel: ' + JSON.stringify(v));
    });
    await step('fit: a pan afterwards is not undone', async () => {
      const before = await endsInView(page);
      const box = await page.locator('#leaflet-map').boundingBox();
      await page.mouse.move(box.x + 300, box.y + 120);
      await page.mouse.down();
      await page.mouse.move(box.x + 420, box.y + 170, { steps: 8 });
      await page.mouse.up();
      await page.waitForTimeout(1000); // drag inertia settles
      const panned = await endsInView(page);
      await page.waitForTimeout(1500);
      const after = await endsInView(page);
      assert(Math.hypot(panned.a.x - before.a.x, panned.a.y - before.a.y) > 60, 'setup: the pan did not move the map');
      assert(Math.abs(after.a.x - panned.a.x) <= 1 && Math.abs(after.a.y - panned.a.y) <= 1,
        'the map moved after the pan: ' + JSON.stringify({ before: before.a, panned: panned.a, after: after.a }));
    });
    await step('fit: a Move afterwards is not followed by a second fit', async () => {
      const before = await endsInView(page);
      const n = rec.profile.length;
      await page.click('[data-testid="tp-move-a"]');
      const box = await page.locator('#leaflet-map').boundingBox();
      await page.mouse.click(box.x + 700, box.y + 140);
      const t0 = Date.now();
      while (rec.profile.length === n && Date.now() - t0 < 5000) await page.waitForTimeout(100);
      await page.waitForTimeout(800);
      const after = await endsInView(page);
      assert(rec.profile.length === n + 1, 'the move was not measured');
      assert(Math.abs(after.b.x - before.b.x) <= 1 && Math.abs(after.b.y - before.b.y) <= 1,
        'the map moved after Move A: ' + JSON.stringify({ before: before.b, after: after.b }));
      assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
    });
    await ctx.close();
  }

  {
    const { ctx, page } = await newCtx({ storage: ELSEWHERE });
    await step('fit: without a profile in the URL the stored view is kept', async () => {
      await page.goto(`${BASE}/#/map`, { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
      await page.waitForTimeout(1500);
      const v = JSON.parse(await page.evaluate(() => localStorage.getItem('map-view')));
      assert(Math.abs(v.lat - 56.08) < 1e-6 && Math.abs(v.lng - 9.62) < 1e-6 && v.zoom === 12, 'the stored view moved: ' + JSON.stringify(v));
    });
    await ctx.close();
  }

  {
    const { ctx, page, rec } = await newCtx({ storage: ELSEWHERE, viewport: { width: 360, height: 740 }, touch: true });
    await step('fit: a /p link on a phone puts both ends above the panel', async () => {
      await page.goto(`${BASE}/p?profile=${encodeURIComponent(P1)}&ha=19&hb=19`, { waitUntil: 'domcontentloaded' });
      await page.waitForFunction(() => location.pathname === '/' && location.hash.indexOf('#/map?') === 0, null, { timeout: 15000 });
      await waitForRequestFor(page, rec, P1);
      await page.waitForTimeout(400);
      const v = await endsInView(page);
      assert(v.ok, 'an end is off the map or under the panel: ' + JSON.stringify(v));
      assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
    });
    await ctx.close();
  }

  await browser.close();
  console.log(`\n#746 E2E: ${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
