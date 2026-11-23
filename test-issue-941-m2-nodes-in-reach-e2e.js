/**
 * E2E (#941 M2): nodes in reach and the model choice in the Predicted
 * reach panel, in a real browser.
 *
 * As in test-issue-941-predicted-reach-e2e.js, the reach is switched on by
 * intercepting /api/config/client (the `coverage` block cmd/server sends,
 * now with `model` and `models`), and /api/terrain/coverage and its raster
 * are a stub in the server's shape (CoverageResponse), here with `reach`:
 * five repeaters and rooms with their path loss, one of them named with
 * markup, and one without terrain. Hidden nodes are in no count (the
 * server leaves them out). Nothing here reaches a real network.
 *
 * Asserts, light and dark:
 *   1. desktop: the class chips count the nodes at the operator's budget
 *      (good 2, marginal 1, weak 1, all 4), the note says one is below
 *      0 dB and one has no terrain, and says nothing of hidden nodes;
 *      the Good chip opens the list (rlist=good in the address bar) with
 *      node-page links, the marked-up name as text; All lists every node
 *      in reach in loss order;
 *   2. moving the power slider recounts and recolours the chips and the
 *      list with no new request of any kind to /api/;
 *   3. the model select asks again once, with model=delta, puts
 *      rmodel=delta in the address bar and names the model; a deep link
 *      with rmodel and rlist restores both;
 *   4. 375×560 with touch, from a cold #/map: real taps on the toggle and
 *      the map open the panel, a tap on a chip opens the list, the list
 *      does not scroll on its own (a swipe that starts on it scrolls the
 *      sheet), a budget change recounts it with no request, there is no
 *      horizontal scroll, the chip and the first link are on screen and
 *      on top, and a tap on the link opens the node's page;
 *   5. no console error, and no request leaves localhost.
 *
 * SHOTS_DIR, when set, receives small JPEG screenshots.
 *
 * Usage:
 *   BASE_URL=http://localhost:13581 node test-issue-941-m2-nodes-in-reach-e2e.js
 */
'use strict';
const path = require('path');
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SHOTS = process.env.SHOTS_DIR || '';
const COVERAGE_PATH = '/api/terrain/coverage';
const RASTER_PATH = '/api/terrain/coverage/raster';

const RADIO = { txPowerDbm: 22, antennaGainDbi: 3, rxGainDbi: 3, lossDb: 0, sensitivityDbm: -130 };
const COVERAGE_BLOCK = {
  enabled: true, maxRadiusKm: 40, radiusKm: 40, resolutionM: 30, rxHeightM: 2,
  pointAntenna: { m: 2, source: 'point-default' }, radio: RADIO, dhm: false,
  model: 'bullington', models: ['bullington', 'delta'],
};
const AARHUS = { lat: 56.1629, lon: 10.2039 };
const XSS_NAME = '<img src=x onerror="window.__xss=1">Roof';
// Lowest loss first, as cmd/server sorts them. At 158 dB: margins 38, 18,
// 8, 2, −3 → good, good, marginal, weak, none.
const NODES = [
  { pubkey: '1'.repeat(64), name: 'Near mast', role: 'repeater', distanceKm: 2.1, heightM: 10, heightSource: 'role-default', lossDb: 120 },
  { pubkey: 'ab'.repeat(32), name: XSS_NAME, role: 'room', distanceKm: 5.4, heightM: 5, heightSource: 'role-default', lossDb: 140 },
  { pubkey: '3'.repeat(64), name: 'Hill top', role: 'repeater', distanceKm: 9.8, heightM: 31.5, heightSource: 'owner', lossDb: 150 },
  { pubkey: '4'.repeat(64), name: 'Far valley', role: 'repeater', distanceKm: 17.2, heightM: 10, heightSource: 'role-default', lossDb: 156 },
  { pubkey: '5'.repeat(64), name: 'Behind the ridge', role: 'repeater', distanceKm: 23, heightM: 10, heightSource: 'role-default', lossDb: 161 },
];
const REACH = { roles: ['repeater', 'room'], budgetDb: 158, counts: { good: 2, marginal: 1, weak: 1, none: 1 }, nodes: NODES, omitted: 0, noTerrain: 1 };

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + (e && e.stack || e)); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

function mercY(lat) { return Math.log(Math.tan(Math.PI / 4 + lat * Math.PI / 360)); }
function latOfMercY(y) { return (2 * Math.atan(Math.exp(y)) - Math.PI / 2) * 180 / Math.PI; }

/* A small loss raster: free-space-like growth with distance. */
function rasterFor(c, radiusKm) {
  const W = 200, H = 200;
  const dLat = radiusKm / 111.2 * 1.05, dLon = dLat / Math.cos(c.lat * Math.PI / 180);
  const s = c.lat - dLat, n = c.lat + dLat, w = c.lon - dLon, e = c.lon + dLon;
  const y0 = mercY(n), y1 = mercY(s);
  const bytes = Buffer.alloc(W * H);
  for (let row = 0; row < H; row++) {
    const lat = latOfMercY(y0 + (y1 - y0) * (row + 0.5) / H);
    for (let col = 0; col < W; col++) {
      const lon = w + (e - w) * (col + 0.5) / W;
      const d = Math.hypot((lat - c.lat) * 111.2, (lon - c.lon) * 111.2 * Math.cos(c.lat * Math.PI / 180));
      bytes[row * W + col] = d <= radiusKm ? Math.max(0, Math.min(254, Math.round((116 + 20 * Math.log10(Math.max(d, 0.05)) - 60) / 0.5))) : 255;
    }
  }
  return { bytes, W, H, bounds: [[s, w], [n, e]] };
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #941 M2 nodes in reach against ${BASE} ===\n`);

  async function newCtx(o) {
    o = o || {};
    const ctx = await browser.newContext({
      viewport: o.viewport || { width: 1280, height: 900 },
      hasTouch: !!o.touch, isMobile: !!o.touch,
    });
    const rec = { consoleErrors: [], all: [], api: [], coverage: [], raster: 0 };
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      config.coverage = COVERAGE_BLOCK;
      await route.fulfill({ response, json: config });
    });
    const rasters = new Map();
    await ctx.route(url => new URL(url).pathname === COVERAGE_PATH, async (route) => {
      const u = new URL(route.request().url());
      rec.coverage.push(u.search);
      const at = u.searchParams.get('at');
      const c = { lat: Number(at.split(',')[0]), lon: Number(at.split(',')[1]) };
      const r = Number(u.searchParams.get('r') || 40);
      const model = u.searchParams.get('model') === 'delta' ? 'delta-bullington-p526-v1' : 'bullington-p526-v1';
      const key = (u.search.length.toString(16) + 'b'.repeat(32)).slice(0, 32);
      const ras = rasterFor(c, r);
      rasters.set(key, ras.bytes);
      const body = {
        key, center: { lat: c.lat, lon: c.lon, kind: 'point', groundM: 34.2 },
        params: { radiusKm: r, resolutionM: 30, txHeightM: 2, txHeightSource: 'point-default', txAssumed: true,
          rxHeightM: 2, rxHeightSource: 'default', freqMHz: 869.618, freqSource: 'firmware', k: 4 / 3, kSource: 'default', model },
        raster: { url: RASTER_PATH + '?key=' + key, width: ras.W, height: ras.H, bounds: ras.bounds, encoding: 'loss-u8',
          lossBaseDb: 60, lossStepDb: 0.5, noData: 255, coveredPct: 100, bytes: ras.bytes.length },
        sources: [{ id: 'dhm-dtm', resolutionM: 30, model: 'dtm' }],
        reach: REACH,
      };
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) });
    });
    await ctx.route(url => new URL(url).pathname === RASTER_PATH, async (route) => {
      rec.raster++;
      const bytes = rasters.get(new URL(route.request().url()).searchParams.get('key'));
      if (!bytes) { await route.fulfill({ status: 404, contentType: 'application/json', body: '{"error":"not found"}' }); return; }
      await route.fulfill({ status: 200, contentType: 'application/octet-stream', body: bytes });
    });
    await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, async (route) => {
      await route.fulfill({ status: 200, contentType: 'image/png', body: Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGM4ceIEAAS0AlkWLoFAAAAAAElFTkSuQmCC', 'base64') });
    });
    await ctx.addInitScript(({ theme }) => {
      try {
        localStorage.setItem('meshcore-theme', theme);
        localStorage.setItem('meshcore-map-clustering', 'false');
        ['row-swipe', 'tab-swipe', 'edge-drawer', 'pull-refresh']
          .forEach(k => localStorage.setItem('meshcore-gesture-hints-' + k, 'seen'));
      } catch (_) {}
      const apply = () => document.documentElement && document.documentElement.setAttribute('data-theme', theme);
      apply();
      document.addEventListener('DOMContentLoaded', apply);
    }, { theme: o.theme || 'light' });
    const page = await ctx.newPage();
    page.on('console', m => { if (m.type() === 'error') rec.consoleErrors.push(m.text()); });
    page.on('pageerror', e => rec.consoleErrors.push(String(e)));
    page.on('request', r => {
      rec.all.push(r.url());
      if (/^https?:\/\/(localhost|127\.0\.0\.1)[:/][^/]*\/api\//i.test(r.url())) rec.api.push(r.url());
    });
    return { ctx, page, rec };
  }

  async function waitPanel(page) {
    await page.waitForSelector('[data-testid="reach-panel"]:not([hidden])', { timeout: 20000 });
    await page.waitForFunction(() => {
      const c = document.querySelector('canvas.reach-layer');
      const nir = document.querySelector('[data-testid="reach-nir"]');
      return c && c.width > 0 && nir && !nir.hidden && !document.querySelector('[data-testid="reach-panel"].is-busy');
    }, null, { timeout: 15000 });
    await page.waitForTimeout(150);
  }

  function chips(page) {
    return page.evaluate(() => {
      const out = {};
      ['good', 'marginal', 'weak', 'all'].forEach(c => {
        const b = document.querySelector('[data-testid="reach-nir-' + c + '"]');
        out[c] = b && { n: b.querySelector('.reach-nir-count').textContent, pressed: b.getAttribute('aria-pressed'), hidden: b.hidden };
      });
      return out;
    });
  }

  function listed(page) {
    return page.$$eval('[data-testid="reach-nir-item"]', lis => lis.filter(li => !li.hidden && li.offsetParent !== null).map(li => {
      const a = li.querySelector('a');
      return { cls: li.getAttribute('data-class'), name: a.textContent, href: a.getAttribute('href'),
        margin: li.querySelector('.reach-nir-margin').textContent, swatch: li.querySelector('.reach-swatch').className };
    }));
  }

  /* setPower moves the power slider and waits past the panel's request
   * debounce (500 ms), so a request it wrongly started would be seen. */
  async function setPower(page, v) {
    await page.evaluate((val) => {
      const s = document.querySelector('[data-testid="reach-power"]');
      s.value = String(val);
      s.dispatchEvent(new Event('input', { bubbles: true }));
    }, v);
    await page.waitForTimeout(800);
  }

  function noNetwork(rec) {
    const bad = rec.all.filter(u => !/^https?:\/\/(localhost|127\.0\.0\.1)[:/]/i.test(u) && !u.startsWith('data:') && !u.startsWith('blob:'));
    assert(bad.filter(u => /dataforsyningen|amazonaws|copernicus/i.test(u)).length === 0, 'a real upstream was reached');
    const errs = rec.consoleErrors.filter(e => !/Failed to load resource/.test(e));
    assert(errs.length === 0, 'console errors: ' + errs.join(' | '));
  }

  async function shot(page, name, el) {
    if (!SHOTS) return;
    const target = el ? await page.$(el) : page;
    await target.screenshot({ path: path.join(SHOTS, name), type: 'jpeg', quality: 60 });
  }

  // ── 1–3: desktop ───────────────────────────────────────────────────────
  for (const theme of ['light', 'dark']) {
    await step(`${theme}: counts, the Good list, All, a budget change with no request, the model`, async () => {
      const { ctx, page, rec } = await newCtx({ theme });
      try {
        await page.goto(`${BASE}/#/map?reach=${AARHUS.lat},${AARHUS.lon}&rkm=25`, { waitUntil: 'domcontentloaded' });
        await waitPanel(page);
        let c = await chips(page);
        assert(c.good.n === '2' && c.marginal.n === '1' && c.weak.n === '1' && c.all.n === '4', 'chips ' + JSON.stringify(c));
        assert(['good', 'marginal', 'weak', 'all'].every(k => c[k].pressed === 'false'), 'a chip pressed before a click');
        const note = await page.textContent('[data-testid="reach-nir-note"]');
        assert(/4 of 5 repeaters and rooms/.test(note) && /1 below 0 dB/.test(note) && /1 without terrain data/.test(note) && !/hidden/.test(note), 'note ' + note);
        assert(await page.isHidden('[data-testid="reach-nir-list"]'), 'the list is open before a chip is clicked');

        await page.click('[data-testid="reach-nir-good"]');
        let l = await listed(page);
        assert(l.length === 2 && l[0].name === 'Near mast' && l[1].name === XSS_NAME, 'good list ' + JSON.stringify(l));
        assert(l[0].href === '#/nodes/' + '1'.repeat(64) && l[1].href === '#/nodes/' + 'ab'.repeat(32), 'hrefs');
        assert(l[0].margin === '+38 dB' && l[0].swatch === 'reach-swatch reach-swatch-good', 'item ' + JSON.stringify(l[0]));
        assert(await page.evaluate(() => !document.querySelector('[data-testid="reach-nir-list"] img') && !window.__xss), 'markup in a name became DOM');
        assert((await chips(page)).good.pressed === 'true', 'Good not pressed');
        await page.waitForFunction(() => /[?&]rlist=good/.test(location.hash), null, { timeout: 3000 });
        await page.click('[data-testid="reach-nir-all"]');
        l = await listed(page);
        assert(l.map(x => x.cls).join() === 'good,good,marginal,weak', 'all list ' + JSON.stringify(l.map(x => x.cls)));
        await shot(page, `941-m2-nodes-desktop-${theme}.jpg`);
        await shot(page, `941-m2-panel-${theme}.jpg`, '[data-testid="reach-panel"]');

        // 2: the budget recounts and recolours in place, no request.
        const before = { api: rec.api.length, coverage: rec.coverage.length, raster: rec.raster };
        await setPower(page, 10); // −12 dB: margins 26, 6, −4, −10, −15
        c = await chips(page);
        assert(c.good.n === '1' && c.marginal.n === '1' && c.weak.n === '0' && c.all.n === '2', 'chips at 10 dBm ' + JSON.stringify(c));
        l = await listed(page);
        assert(l.map(x => x.name + ':' + x.cls + ':' + x.margin).join('|') === `Near mast:good:+26 dB|${XSS_NAME}:marginal:+6 dB`, 'list at 10 dBm ' + JSON.stringify(l));
        await setPower(page, 30); // +8 dB: 46, 26, 16, 10, 5
        c = await chips(page);
        assert(c.good.n === '4' && c.weak.n === '0' && c.all.n === '5', 'chips at 30 dBm ' + JSON.stringify(c));
        assert(rec.api.length === before.api && rec.coverage.length === before.coverage && rec.raster === before.raster,
          `a budget change asked the server: ${rec.api.slice(before.api).join(', ')}`);
        assert(/5 of 5/.test(await page.textContent('[data-testid="reach-nir-note"]')), 'note after the budget change');
        await shot(page, `941-m2-nodes-30dbm-${theme}.jpg`, '[data-testid="reach-panel"]');

        // 3: the model asks again, once, and is in the link.
        await page.selectOption('[data-testid="reach-model-select"]', 'delta');
        for (let i = 0; i < 60 && rec.coverage.length === before.coverage; i++) await page.waitForTimeout(50);
        await page.waitForFunction(() => /delta-Bullington/.test(document.querySelector('[data-testid="reach-model"]').textContent), null, { timeout: 5000 });
        assert(rec.coverage.length === before.coverage + 1 && new URLSearchParams(rec.coverage[rec.coverage.length - 1]).get('model') === 'delta',
          'model request ' + rec.coverage.slice(before.coverage).join(' '));
        assert(!/model|rpwr|rlist/.test(rec.coverage[rec.coverage.length - 1].replace('model=delta', '')), 'the request carries the budget or the list');
        await page.waitForFunction(() => /[?&]rmodel=delta/.test(location.hash), null, { timeout: 3000 });
        assert((await chips(page)).all.pressed === 'true', 'the list filter was lost on the new model');
        noNetwork(rec);
      } finally { await ctx.close(); }
    });
  }

  await step('a link with rmodel and rlist restores the model and the open list', async () => {
    const { ctx, page, rec } = await newCtx({});
    try {
      await page.goto(`${BASE}/#/map?reach=${AARHUS.lat},${AARHUS.lon}&rkm=25&rmodel=delta&rlist=marginal`, { waitUntil: 'domcontentloaded' });
      await waitPanel(page);
      assert(rec.coverage.length === 1 && new URLSearchParams(rec.coverage[0]).get('model') === 'delta', 'request ' + rec.coverage);
      assert(await page.inputValue('[data-testid="reach-model-select"]') === 'delta', 'select');
      const l = await listed(page);
      assert(l.length === 1 && l[0].name === 'Hill top' && l[0].cls === 'marginal', 'list ' + JSON.stringify(l));
      noNetwork(rec);
    } finally { await ctx.close(); }
  });

  // ── 4: phone, touch, from a cold #/map ──────────────────────────────────
  for (const theme of ['light', 'dark']) {
    await step(`${theme}: 375×560 touch from a cold #/map: taps open the list, the budget recounts it, a link opens the node`, async () => {
      const { ctx, page, rec } = await newCtx({ theme, viewport: { width: 375, height: 560 }, touch: true });
      try {
        await page.goto(`${BASE}/#/map?lat=${AARHUS.lat}&lon=${AARHUS.lon}&zoom=10`, { waitUntil: 'domcontentloaded' });
        await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
        await page.waitForSelector('[data-testid="map-reach-toggle"]', { timeout: 15000 });
        await page.tap('[data-testid="map-reach-toggle"]');
        const box = await page.locator('#leaflet-map').boundingBox();
        await page.touchscreen.tap(box.x + box.width / 2, box.y + box.height / 3);
        await waitPanel(page);
        assert(rec.coverage.length === 1, 'requests ' + rec.coverage.length);
        await page.locator('[data-testid="reach-nir-good"]').scrollIntoViewIfNeeded();
        const onTop = await page.evaluate(() => {
          const b = document.querySelector('[data-testid="reach-nir-good"]');
          const r = b.getBoundingClientRect();
          const hit = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
          return { top: r.top, bottom: r.bottom, h: r.height, ok: !!hit && (hit === b || b.contains(hit)) };
        });
        assert(onTop.ok && onTop.top >= 0 && onTop.bottom <= 560 && onTop.h >= 44, 'Good chip ' + JSON.stringify(onTop));
        await page.tap('[data-testid="reach-nir-good"]');
        let l = await listed(page);
        assert(l.length === 2, 'phone list ' + JSON.stringify(l));
        // The list is part of the sheet, not a scroller inside it: a swipe
        // that starts on the list scrolls the sheet.
        const nested = await page.evaluate(() => {
          const cs = getComputedStyle(document.querySelector('[data-testid="reach-nir-list"]'));
          return { overflowY: cs.overflowY, maxHeight: cs.maxHeight };
        });
        assert(nested.overflowY === 'visible' && nested.maxHeight === 'none', 'the list scrolls inside the sheet ' + JSON.stringify(nested));
        const item = page.locator('[data-testid="reach-nir-item"]:not([hidden])').first();
        await item.scrollIntoViewIfNeeded();
        const sheetTop = () => page.evaluate(() => document.querySelector('[data-testid="reach-panel"]').scrollTop);
        const fromTop = await sheetTop();
        const ib = await item.boundingBox();
        const cdp = await ctx.newCDPSession(page);
        const at = { x: ib.x + ib.width / 2, y: ib.y + ib.height / 2 };
        await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [at] });
        for (let k = 1; k <= 8; k++) {
          await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x: at.x, y: at.y - 10 * k }] });
          await page.waitForTimeout(16);
        }
        await page.waitForTimeout(200); // held still: no fling
        await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
        await page.waitForTimeout(300);
        const swiped = await sheetTop();
        assert(swiped > fromTop + 30, `a swipe on the list did not scroll the sheet: ${fromTop} -> ${swiped}`);
        await shot(page, `941-m2-nodes-phone-sheet-${theme}-375x560.jpg`);
        const before = rec.api.length;
        await setPower(page, 10);
        l = await listed(page);
        assert(l.length === 1 && l[0].name === 'Near mast', 'phone list at 10 dBm ' + JSON.stringify(l));
        assert(rec.api.length === before, 'a budget change asked the server: ' + rec.api.slice(before).join(', '));
        const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
        assert(overflow <= 0, 'horizontal scroll ' + overflow);
        const link = page.locator('[data-testid="reach-nir-item"]:not([hidden]) a').first();
        await link.scrollIntoViewIfNeeded();
        const lk = await link.evaluate(a => {
          const r = a.getBoundingClientRect();
          const hit = document.elementFromPoint(r.left + Math.min(r.width / 2, 20), r.top + r.height / 2);
          return { top: r.top, bottom: r.bottom, ok: !!hit && (hit === a || a.contains(hit)) };
        });
        assert(lk.ok && lk.top >= 0 && lk.bottom <= 560, 'link ' + JSON.stringify(lk));
        await shot(page, `941-m2-nodes-phone-${theme}-375x560.jpg`);
        await link.tap();
        await page.waitForFunction(() => location.hash.indexOf('#/nodes/' + '1'.repeat(64)) === 0, null, { timeout: 5000 });
        noNetwork(rec);
      } finally { await ctx.close(); }
    });
  }

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed\n`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
